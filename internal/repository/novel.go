package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// novelCall 用于 singleflight：持有一次 DB 查询的结果和同步通道
type novelCall struct {
	wg    sync.WaitGroup
	novel *model.Novel
	err   error
}

// NovelRepository 小说仓库
type NovelRepository struct {
	db    *gorm.DB
	cache *redis.Client
	// inflight 用于 singleflight：对同一 novel_id 的并发 cache miss 只发起一次 DB 查询
	inflight sync.Map // key: uint novel_id → *novelCall
}

const novelCacheTTL = 30 * time.Minute

func NewNovelRepository(db *gorm.DB, cache *redis.Client) *NovelRepository {
	return &NovelRepository{db: db, cache: cache}
}

// Create 创建小说
func (r *NovelRepository) Create(novel *model.Novel) error {
	err := r.db.Create(novel).Error
	if err != nil && (isSchemaMissing(err)) {
		// 广场社交列尚未迁移到 DB，降级：剔除这些列（它们均有 DB default，不影响业务）
		logger.Errorf("NovelRepository.Create: social columns missing, retrying without them: %v", err)
		err = r.db.Omit("hot_score",
			"is_published", "published_at", "visibility", "plaza_tags").Create(novel).Error
	}
	if err != nil {
		return err
	}
	r.invalidateCache(novel.ID)
	return nil
}

// GetByID 根据ID获取小说（Redis 缓存 + singleflight 防 DB 击穿）
func (r *NovelRepository) GetByID(id uint) (*model.Novel, error) {
	// 1. 尝试 Redis 缓存
	if r.cache != nil {
		cacheKey := fmt.Sprintf("novel:%d", id)
		if cached, err := r.cache.Get(context.Background(), cacheKey).Result(); err == nil {
			var novel model.Novel
			if json.Unmarshal([]byte(cached), &novel) == nil {
				return &novel, nil
			}
		}
	}

	// 2. Singleflight：同一进程内对相同 novel_id 的并发 cache miss 只发起一次 DB 查询。
	//    LoadOrStore 返回已有 call → 等待它完成并共享结果；否则自己做 DB 查询。
	call := &novelCall{}
	call.wg.Add(1)
	if actual, loaded := r.inflight.LoadOrStore(id, call); loaded {
		// 等待先到的 goroutine 完成
		existing := actual.(*novelCall)
		existing.wg.Wait()
		return existing.novel, existing.err
	}
	// 自己执行 DB 查询，完成后广播结果
	defer func() {
		call.wg.Done()
		r.inflight.Delete(id)
	}()

	var novel model.Novel
	if err := r.db.Preload("Worldview").Preload("VideoConfig").First(&novel, id).Error; err != nil {
		call.err = err
		return nil, err
	}
	call.novel = &novel

	// 3. 写入缓存
	if r.cache != nil {
		if data, err := json.Marshal(novel); err == nil {
			r.cache.Set(context.Background(), fmt.Sprintf("novel:%d", id), data, novelCacheTTL)
		}
	}
	return &novel, nil
}

// GetByIDFromDB 直接从数据库读取小说（跳过 Redis 缓存），用于需要最新配置的场景。
func (r *NovelRepository) GetByIDFromDB(id uint) (*model.Novel, error) {
	var novel model.Novel
	if err := r.db.Preload("Worldview").Preload("VideoConfig").First(&novel, id).Error; err != nil {
		return nil, err
	}
	return &novel, nil
}

// GetByUUID 根据UUID获取小说
func (r *NovelRepository) GetByUUID(uuid string) (*model.Novel, error) {
	var novel model.Novel
	if err := r.db.Preload("Worldview").Preload("VideoConfig").Where("uuid = ?", uuid).First(&novel).Error; err != nil {
		return nil, err
	}
	return &novel, nil
}

// FindByTitle 按标题和 tenantID 查找小说（用于导入去重）
func (r *NovelRepository) FindByTitle(title string, tenantID uint) (*model.Novel, error) {
	var novel model.Novel
	err := r.db.Where("title = ? AND tenant_id = ? AND deleted_at IS NULL", title, tenantID).First(&novel).Error
	if err != nil {
		return nil, err
	}
	return &novel, nil
}

// SearchByTitle 按标题模糊搜索小说（限当前租户，防止跨租户数据泄露）
func (r *NovelRepository) SearchByTitle(title string, tenantID uint, limit int) ([]*model.Novel, error) {
	var novels []*model.Novel
	if limit <= 0 {
		limit = 20
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(title)
	pattern := "%" + escaped + "%"
	err := r.db.Where("title LIKE ? AND tenant_id = ? AND deleted_at IS NULL", pattern, tenantID).
		Order("updated_at DESC").
		Limit(limit).
		Find(&novels).Error
	return novels, err
}

// List 获取小说列表
func (r *NovelRepository) List(page, pageSize int, filters map[string]interface{}) ([]*model.Novel, int64, error) {
	var novels []*model.Novel
	var total int64

	query := r.db.Model(&model.Novel{})

	// 应用过滤：owner 视图 + 协作成员视图（取并集）
	if tenantID, ok := filters["tenant_id"]; ok {
		if userID, ok2 := filters["user_id"]; ok2 {
			query = query.Where(
				"tenant_id = ? OR id IN (SELECT novel_id FROM ink_novel_member WHERE user_id = ? AND status = 'active')",
				tenantID, userID,
			)
		} else {
			query = query.Where("tenant_id = ?", tenantID)
		}
	}
	if status, ok := filters["status"]; ok {
		query = query.Where("status = ?", status)
	}
	if genre, ok := filters["genre"]; ok {
		query = query.Where("genre = ?", genre)
	}

	// 统计总数 (clone to avoid state contamination)
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 排序：支持从 filters 传入 sort 和 order，均经过白名单校验
	allowedSortFields := map[string]bool{
		"created_at": true, "updated_at": true, "title": true, "status": true,
	}
	sortField := "updated_at"
	if v, ok := filters["sort"]; ok {
		if s, ok := v.(string); ok && allowedSortFields[s] {
			sortField = s
		}
	}
	sortOrder := "DESC"
	if v, ok := filters["order"]; ok {
		if o, ok := v.(string); ok && (o == "asc" || o == "ASC") {
			sortOrder = "ASC"
		}
	}

	// 分页查询（列表视图不需要 Worldview 完整数据，novel.WorldviewID 字段已足够）
	offset := (page - 1) * pageSize
	if err := query.
		Order(sortField + " " + sortOrder).
		Offset(offset).
		Limit(pageSize).
		Find(&novels).Error; err != nil {
		return nil, 0, err
	}

	return novels, total, nil
}

// Update 更新小说（同时 upsert VideoConfig）
func (r *NovelRepository) Update(novel *model.Novel) error {
	if err := r.db.Save(novel).Error; err != nil {
		return err
	}
	if novel.VideoConfig != nil {
		novel.VideoConfig.NovelID = novel.ID
		if err := r.db.Select("*").Save(novel.VideoConfig).Error; err != nil {
			logger.Errorf("[NovelRepository] Save VideoConfig: %v", err)
		}
	}
	r.invalidateCache(novel.ID)
	return nil
}

// SaveVideoConfig upserts the video config for a novel (select all columns to
// allow zero-value writes) and invalidates the novel cache.
func (r *NovelRepository) SaveVideoConfig(vc *model.NovelVideoConfig) error {
	if err := r.db.Select("*").Save(vc).Error; err != nil {
		return err
	}
	r.invalidateCache(vc.NovelID)
	return nil
}

// UpdateFields 更新小说指定字段（避免 Save 写零值导致数据丢失）
func (r *NovelRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	if err := r.db.Model(&model.Novel{}).Where("id = ?", id).Updates(fields).Error; err != nil {
		return err
	}
	r.invalidateCache(id)
	return nil
}

// Delete 软删除小说（不删关联数据）
func (r *NovelRepository) Delete(id uint) error {
	if err := r.db.Delete(&model.Novel{}, id).Error; err != nil {
		return err
	}
	r.invalidateCache(id)
	return nil
}

// DeleteWithCascade 物理删除小说及其全部关联数据（在事务中按依赖顺序执行）
// 对"列/表不存在"类错误（schema 尚未迁移）采用 skip 策略，不中断事务。
func (r *NovelRepository) DeleteWithCascade(id uint) error {
	err := r.db.Transaction(func(tx *gorm.DB) error {
		// tryExec 执行 SQL；若因 schema 缺失（列/表不存在）失败则记录日志并跳过
		tryExec := func(sql string, args ...interface{}) error {
			if e := tx.Exec(sql, args...).Error; e != nil {
				if isSchemaMissing(e) {
					logger.Errorf("[DeleteNovel] skip (schema not ready): %v", e)
					return nil
				}
				return e
			}
			return nil
		}

		// ── 1. 间接关联：场景一致性日志（通过 anchor / storyboard_shot）
		if e := tryExec(`DELETE FROM ink_scene_consistency_log WHERE anchor_id IN (SELECT id FROM ink_scene_anchor WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_scene_consistency_log WHERE shot_id IN (SELECT id FROM ink_storyboard_shot WHERE video_id IN (SELECT id FROM ink_video WHERE novel_id = ?))`, id); e != nil {
			return e
		}

		// ── 2. 分镜（通过 video.novel_id）
		if e := tryExec(`DELETE FROM ink_storyboard_shot WHERE video_id IN (SELECT id FROM ink_video WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 3. 章节版本（通过 chapter.chapter_id）
		if e := tryExec(`DELETE FROM ink_chapter_version WHERE chapter_id IN (SELECT id FROM ink_chapter WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 4. 角色间接数据（通过 character.novel_id）
		if e := tryExec(`DELETE FROM ink_character_visual_design WHERE character_id IN (SELECT id FROM ink_character WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_character_state_snapshot WHERE character_id IN (SELECT id FROM ink_character WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		// ── 5. 章节物品关联（通过 item.novel_id）
		if e := tryExec(`DELETE FROM ink_chapter_item WHERE item_id IN (SELECT id FROM ink_item WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 5b. 分镜子表（通过 shot → video → novel）
		if e := tryExec(`DELETE FROM ink_shot_voice_segment WHERE shot_id IN (SELECT id FROM ink_storyboard_shot WHERE video_id IN (SELECT id FROM ink_video WHERE novel_id = ?))`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_shot_sfx_item WHERE shot_id IN (SELECT id FROM ink_storyboard_shot WHERE video_id IN (SELECT id FROM ink_video WHERE novel_id = ?))`, id); e != nil {
			return e
		}

		// ── 5c. 视频 BGM 分段（通过 video → novel）
		if e := tryExec(`DELETE FROM ink_video_bgm_segment WHERE video_id IN (SELECT id FROM ink_video WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 5d. 改写项目子表（通过 rewrite_project → novel）
		if e := tryExec(`DELETE FROM ink_chapter_rewrite_task WHERE project_id IN (SELECT id FROM ink_rewrite_project WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_literary_analysis WHERE project_id IN (SELECT id FROM ink_rewrite_project WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_rewrite_bible WHERE project_id IN (SELECT id FROM ink_rewrite_project WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_rewrite_continuity_index WHERE project_id IN (SELECT id FROM ink_rewrite_project WHERE novel_id = ?)`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_rewrite_chapter_summary WHERE project_id IN (SELECT id FROM ink_rewrite_project WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 5e. 审查记录（通过 chapter → novel 或 video → novel）
		if e := tryExec(`DELETE FROM ink_review_record WHERE (entity_type = 'chapter' AND entity_id IN (SELECT id FROM ink_chapter WHERE novel_id = ?)) OR (entity_type = 'storyboard' AND entity_id IN (SELECT id FROM ink_video WHERE novel_id = ?))`, id, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_ignored_review_issue WHERE (entity_type = 'chapter' AND entity_id IN (SELECT id FROM ink_chapter WHERE novel_id = ?)) OR (entity_type = 'storyboard' AND entity_id IN (SELECT id FROM ink_video WHERE novel_id = ?))`, id, id); e != nil {
			return e
		}

		// ── 5f. 社交数据（统一表 ink_entity_like / ink_entity_comment）
		if e := tryExec(`DELETE FROM ink_entity_like WHERE (entity_type = 'novel' AND entity_id = ?) OR (entity_type = 'chapter' AND novel_id = ?)`, id, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_entity_comment WHERE novel_id = ?`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_reading_progress WHERE novel_id = ?`, id); e != nil {
			return e
		}
		if e := tryExec(`DELETE FROM ink_chapter_read_record WHERE novel_id = ?`, id); e != nil {
			return e
		}

		// ── 6. 扩展表（novel_id 直接关联；部分表可能尚未迁移，tryExec 会跳过）
		extStmts := []string{
			`DELETE FROM ink_rewrite_project WHERE novel_id = ?`,
			`DELETE FROM ink_video WHERE novel_id = ?`,
			`DELETE FROM ink_scene_anchor WHERE novel_id = ?`,
			`DELETE FROM ink_arc_summary WHERE novel_id = ?`,
			`DELETE FROM ink_review_task WHERE novel_id = ?`,
			`DELETE FROM ink_feedback_record WHERE novel_id = ?`,
			`DELETE FROM ink_plot_point WHERE novel_id = ?`,
			`DELETE FROM ink_model_usage_log WHERE novel_id = ?`,
			`DELETE FROM ink_async_task WHERE entity_id = ? AND entity_type = 'novel'`,
			`DELETE FROM ink_hook_chain WHERE novel_id = ?`,
			`DELETE FROM ink_satisfaction_point WHERE novel_id = ?`,
			`DELETE FROM ink_conflict_arc WHERE novel_id = ?`,
			`DELETE FROM ink_knowledge_base WHERE novel_id = ?`,
			`DELETE FROM ink_foreshadow WHERE novel_id = ?`,
			`DELETE FROM ink_novel_member WHERE novel_id = ?`,
			`DELETE FROM ink_media_asset WHERE novel_id = ? AND CONCAT('/api/v1/media/', id) NOT IN (SELECT storage_url FROM ink_asset WHERE status != 'trash' AND deleted_at IS NULL)`,
			`DELETE FROM ink_chapter_character WHERE novel_id = ?`,
		}
		for _, stmt := range extStmts {
			if e := tryExec(stmt, id); e != nil {
				return e
			}
		}

		// ── 7. 核心表（必须成功）
		coreStmts := []string{
			`DELETE FROM ink_item WHERE novel_id = ?`,
			`DELETE FROM ink_character WHERE novel_id = ?`,
			`DELETE FROM ink_chapter WHERE novel_id = ?`,
			`DELETE FROM ink_novel WHERE id = ?`,
		}
		for _, stmt := range coreStmts {
			if e := tx.Exec(stmt, id).Error; e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.invalidateCache(id)
	return nil
}

// SyncStats recalculates chapter_count and total_words from the chapters table.
func (r *NovelRepository) SyncStats(novelID uint) error {
	err := r.db.Transaction(func(tx *gorm.DB) error {
		var result struct {
			Count int
			Words int
		}
		if err := tx.Model(&model.Chapter{}).
			Select("COUNT(*) AS count, COALESCE(SUM(word_count),0) AS words").
			Where("novel_id = ? AND deleted_at IS NULL", novelID).
			Scan(&result).Error; err != nil {
			return err
		}
		return tx.Model(&model.Novel{}).Where("id = ?", novelID).Updates(map[string]interface{}{
			"chapter_count": result.Count,
			"total_words":   result.Words,
		}).Error
	})
	if err != nil {
		return err
	}
	r.invalidateCache(novelID)
	return nil
}

// SyncPublishedCount 重新计算并更新已发布章节数（幂等，最终一致）。
func (r *NovelRepository) SyncPublishedCount(novelID uint) error {
	if err := r.db.Exec(
		"UPDATE ink_novel SET published_count = (SELECT COUNT(*) FROM ink_chapter WHERE novel_id = ? AND is_published = TRUE AND deleted_at IS NULL) WHERE id = ?",
		novelID, novelID,
	).Error; err != nil {
		return err
	}
	r.invalidateCache(novelID)
	return nil
}

// SyncAllStats 在单个事务中原子地重新计算并更新小说的章节数、总字数和已发布章节数。
func (r *NovelRepository) SyncAllStats(novelID uint) error {
	err := r.db.Transaction(func(tx *gorm.DB) error {
		var result struct {
			Count int
			Words int
		}
		if err := tx.Model(&model.Chapter{}).
			Select("COUNT(*) AS count, COALESCE(SUM(word_count),0) AS words").
			Where("novel_id = ? AND deleted_at IS NULL", novelID).
			Scan(&result).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Novel{}).Where("id = ?", novelID).Updates(map[string]interface{}{
			"chapter_count": result.Count,
			"total_words":   result.Words,
		}).Error; err != nil {
			return err
		}
		return tx.Exec(
			"UPDATE ink_novel SET published_count = (SELECT COUNT(*) FROM ink_chapter WHERE novel_id = ? AND is_published = TRUE AND deleted_at IS NULL) WHERE id = ?",
			novelID, novelID,
		).Error
	})
	if err != nil {
		return err
	}
	r.invalidateCache(novelID)
	return nil
}

// invalidateCache 清除缓存
func (r *NovelRepository) invalidateCache(id uint) {
	if r.cache != nil {
		cacheKey := fmt.Sprintf("novel:%d", id)
		r.cache.Del(context.Background(), cacheKey)
	}
}

// ─── 小说广场 — NovelRepository 扩展 ──────────────────────────────────────────

// GetPublicByID 获取单条公开小说（无需 tenantID）
func (r *NovelRepository) GetPublicByID(id uint) (*model.Novel, error) {
	var n model.Novel
	err := r.db.Where("id = ? AND is_published = ? AND visibility = ?", id, true, "public").
		First(&n).Error
	return &n, err
}

// NovelPublicFilter 公开小说列表筛选参数
type NovelPublicFilter struct {
	Sort        string // hot|latest|words|favorites
	Q           string
	Channel     string // female|male|publish|""=全部
	Genre       string // exact match, ""=全部
	WordMin     int    // 0=不限
	WordMax     int    // 0=不限
	UpdatedDays int    // 0=不限，N=最近N天内更新
	IsCompleted string // ""=全部 "1"=completed "0"=ongoing
	Page        int
	PageSize    int
}

// ListPublicSorted 列出公开小说（支持精细筛选和多种排序）
func (r *NovelRepository) ListPublicSorted(f NovelPublicFilter) ([]*model.Novel, int64, error) {
	var novels []*model.Novel
	var total int64
	base := r.db.Model(&model.Novel{}).Where("is_published = ? AND visibility = ?", true, "public")
	if f.Q != "" {
		base = base.Where("title LIKE ? OR description LIKE ?", "%"+f.Q+"%", "%"+f.Q+"%")
	}
	if f.Channel != "" {
		base = base.Where("channel = ?", f.Channel)
	}
	if f.Genre != "" {
		base = base.Where("genre = ?", f.Genre)
	}
	if f.WordMin > 0 {
		base = base.Where("total_words >= ?", f.WordMin)
	}
	if f.WordMax > 0 {
		base = base.Where("total_words <= ?", f.WordMax)
	}
	if f.UpdatedDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -f.UpdatedDays)
		base = base.Where("updated_at >= ?", cutoff)
	}
	if f.IsCompleted == "1" {
		base = base.Where("status = ?", "completed")
	} else if f.IsCompleted == "0" {
		base = base.Where("status IN ?", []string{"planning", "writing", "paused"})
	}
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := "hot_score DESC, published_at DESC"
	switch f.Sort {
	case "latest":
		order = "published_at DESC, created_at DESC"
	case "words":
		order = "total_words DESC, published_at DESC"
	case "favorites":
		order = "hot_score DESC, published_at DESC"
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize < 1 {
		pageSize = 12
	}
	offset := (page - 1) * pageSize
	err := base.Order(order).Offset(offset).Limit(pageSize).Find(&novels).Error
	return novels, total, err
}

// GetPublicRanking 获取公开小说排行榜
// rankType: hot|new|completed|favorites|updated  gender: male|female|""=全部
func (r *NovelRepository) GetPublicRanking(rankType, gender string, limit int) ([]*model.Novel, error) {
	if limit <= 0 {
		limit = 30
	}
	base := r.db.Model(&model.Novel{}).Where("is_published = ? AND visibility = ?", true, "public")
	if gender == "male" || gender == "female" {
		base = base.Where("channel = ?", gender)
	}
	switch rankType {
	case "new":
		cutoff := time.Now().AddDate(0, -1, 0)
		base = base.Where("published_at >= ?", cutoff).Order("published_at DESC")
	case "completed":
		base = base.Where("status = ?", "completed").Order("hot_score DESC")
	case "favorites":
		base = base.Order("hot_score DESC, published_at DESC")
	case "updated":
		base = base.Order("updated_at DESC")
	default: // hot
		base = base.Order("hot_score DESC, published_at DESC")
	}
	var novels []*model.Novel
	err := base.Limit(limit).Find(&novels).Error
	return novels, err
}

// IncrNovelViewCount 浏览量+1（写入 ink_content_stats）
func (r *NovelRepository) IncrNovelViewCount(id uint) error {
	r.db.Exec(`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
		VALUES ('novel', ?, 1, 0, 0, NOW())
		ON DUPLICATE KEY UPDATE view_count = view_count + 1, updated_at = NOW()`, id)
	return nil
}

// IncrNovelLikeCount 点赞数 delta（+1 或 -1，写入 ink_content_stats）
func (r *NovelRepository) IncrNovelLikeCount(id uint, delta int) error {
	r.db.Exec(`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
		VALUES ('novel', ?, 0, ?, 0, NOW())
		ON DUPLICATE KEY UPDATE like_count = GREATEST(0, like_count + ?), updated_at = NOW()`, id, delta, delta)
	return nil
}

// IncrNovelCommentCount 评论数 delta（写入 ink_content_stats）
func (r *NovelRepository) IncrNovelCommentCount(id uint, delta int) error {
	r.db.Exec(`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
		VALUES ('novel', ?, 0, 0, ?, NOW())
		ON DUPLICATE KEY UPDATE comment_count = GREATEST(0, comment_count + ?), updated_at = NOW()`, id, delta, delta)
	return nil
}

// UpdateNovelHotScore 更新热度分
func (r *NovelRepository) UpdateNovelHotScore(id uint, score float64) error {
	if err := r.db.Model(&model.Novel{}).Where("id = ?", id).Update("hot_score", score).Error; err != nil {
		return err
	}
	r.invalidateCache(id)
	return nil
}

// NovelHotCalcRow 热度分计算辅助行（JOIN content_stats 结果）
type NovelHotCalcRow struct {
	ID           uint
	PublishedAt  *time.Time
	ViewCount    int
	LikeCount    int
	CommentCount int
}

// ListPublicNovelsForHotCalc 批量拉取公开小说用于热度分计算（JOIN ink_content_stats）
func (r *NovelRepository) ListPublicNovelsForHotCalc() ([]NovelHotCalcRow, error) {
	var rows []NovelHotCalcRow
	err := r.db.Raw(`SELECT n.id, n.published_at,
		COALESCE(cs.view_count, 0) AS view_count,
		COALESCE(cs.like_count, 0) AS like_count,
		COALESCE(cs.comment_count, 0) AS comment_count
		FROM ink_novel n
		LEFT JOIN ink_content_stats cs ON cs.entity_type = 'novel' AND cs.entity_id = n.id
		WHERE n.is_published = 1 AND n.visibility = 'public' AND n.deleted_at IS NULL`).
		Scan(&rows).Error
	return rows, err
}

// HydrateNovelStats 批量填充小说统计字段（ViewCount/LikeCount/CommentCount）
func (r *NovelRepository) HydrateNovelStats(novels []*model.Novel) {
	if len(novels) == 0 {
		return
	}
	ids := make([]uint, 0, len(novels))
	for _, n := range novels {
		ids = append(ids, n.ID)
	}
	var rows []struct {
		EntityID     uint
		ViewCount    int
		LikeCount    int
		CommentCount int
	}
	r.db.Raw(`SELECT entity_id, view_count, like_count, comment_count
		FROM ink_content_stats WHERE entity_type = 'novel' AND entity_id IN ?`, ids).Scan(&rows)
	statsMap := make(map[uint]struct{ v, l, c int }, len(rows))
	for _, row := range rows {
		statsMap[row.EntityID] = struct{ v, l, c int }{row.ViewCount, row.LikeCount, row.CommentCount}
	}
	for _, n := range novels {
		if s, ok := statsMap[n.ID]; ok {
			n.ViewCount = s.v
			n.LikeCount = s.l
			n.CommentCount = s.c
		}
	}
}

// ─── NovelLikeRepository ────────────────────────────────────────────────────

type NovelLikeRepository struct{ db *gorm.DB }

func NewNovelLikeRepository(db *gorm.DB) *NovelLikeRepository {
	return &NovelLikeRepository{db: db}
}

// Toggle 点赞/取消，返回最终状态（true=已点赞）。
func (r *NovelLikeRepository) Toggle(novelID, userID uint) (liked bool, err error) {
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var like model.EntityLike
		result := tx.Where("entity_type = 'novel' AND entity_id = ? AND user_id = ?", novelID, userID).First(&like)
		if result.Error != nil {
			if err2 := tx.Create(&model.EntityLike{EntityType: "novel", EntityID: novelID, UserID: userID, NovelID: novelID}).Error; err2 != nil {
				return err2
			}
			liked = true
			return tx.Exec(`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
				VALUES ('novel', ?, 0, 1, 0, NOW())
				ON DUPLICATE KEY UPDATE like_count = like_count + 1, updated_at = NOW()`, novelID).Error
		}
		if err2 := tx.Delete(&like).Error; err2 != nil {
			return err2
		}
		liked = false
		return tx.Exec(`UPDATE ink_content_stats SET like_count = GREATEST(0, like_count - 1), updated_at = NOW()
			WHERE entity_type = 'novel' AND entity_id = ?`, novelID).Error
	})
	return
}

// Exists 是否已点赞
func (r *NovelLikeRepository) Exists(novelID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.EntityLike{}).
		Where("entity_type = 'novel' AND entity_id = ? AND user_id = ?", novelID, userID).Count(&count).Error
	return count > 0, err
}

// ─── NovelCommentRepository ─────────────────────────────────────────────────

type NovelCommentRepository struct{ db *gorm.DB }

func NewNovelCommentRepository(db *gorm.DB) *NovelCommentRepository {
	return &NovelCommentRepository{db: db}
}

func (r *NovelCommentRepository) Create(c *model.NovelComment) error {
	ec := &model.EntityComment{
		EntityType: "novel", EntityID: c.NovelID, NovelID: c.NovelID,
		UserID: c.UserID, Content: c.Content, ParentID: c.ParentID,
	}
	if err := r.db.Create(ec).Error; err != nil {
		return err
	}
	c.ID = ec.ID
	c.CreatedAt = ec.CreatedAt
	c.UpdatedAt = ec.UpdatedAt
	return nil
}

func (r *NovelCommentRepository) ListByNovel(novelID uint, page, size int) ([]*model.NovelComment, int64, error) {
	var list []*model.EntityComment
	var total int64
	base := r.db.Model(&model.EntityComment{}).Where("entity_type = 'novel' AND entity_id = ?", novelID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	if err := base.Order("created_at DESC").Offset(offset).Limit(size).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*model.NovelComment, len(list))
	for i, ec := range list {
		out[i] = &model.NovelComment{ID: ec.ID, NovelID: ec.EntityID, UserID: ec.UserID, Content: ec.Content, ParentID: ec.ParentID, CreatedAt: ec.CreatedAt, UpdatedAt: ec.UpdatedAt}
	}
	return out, total, nil
}

func (r *NovelCommentRepository) GetByID(id uint) (*model.NovelComment, error) {
	var ec model.EntityComment
	if err := r.db.Where("entity_type = 'novel' AND id = ?", id).First(&ec).Error; err != nil {
		return nil, err
	}
	return &model.NovelComment{ID: ec.ID, NovelID: ec.EntityID, UserID: ec.UserID, Content: ec.Content, ParentID: ec.ParentID, CreatedAt: ec.CreatedAt, UpdatedAt: ec.UpdatedAt}, nil
}

func (r *NovelCommentRepository) Delete(id uint) error {
	return r.db.Where("entity_type = 'novel' AND id = ?", id).Delete(&model.EntityComment{}).Error
}

// DeleteWithReplies deletes a comment and all its direct replies atomically.
func (r *NovelCommentRepository) DeleteWithReplies(id uint) (int64, error) {
	var replyCount int64
	err := r.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Where("entity_type = 'novel' AND parent_id = ?", id).Delete(&model.EntityComment{})
		if result.Error != nil {
			return result.Error
		}
		replyCount = result.RowsAffected
		return tx.Where("entity_type = 'novel' AND id = ?", id).Delete(&model.EntityComment{}).Error
	})
	if err != nil {
		return 0, err
	}
	return replyCount + 1, nil
}

// ─── NovelCrawlJobRepository ─────────────────────────────────────────────────

type NovelCrawlJobRepository struct{ db *gorm.DB }

func NewNovelCrawlJobRepository(db *gorm.DB) *NovelCrawlJobRepository {
	return &NovelCrawlJobRepository{db: db}
}

// Create 创建爬取任务记录
func (r *NovelCrawlJobRepository) Create(job *model.NovelCrawlJob) error {
	return r.db.Create(job).Error
}

// GetLatestByNovelID 获取小说最新的爬取任务
func (r *NovelCrawlJobRepository) GetLatestByNovelID(novelID uint) (*model.NovelCrawlJob, error) {
	var job model.NovelCrawlJob
	err := r.db.Where("novel_id = ?", novelID).Order("id DESC").First(&job).Error
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// UpdateProgress 更新爬取进度
func (r *NovelCrawlJobRepository) UpdateProgress(id uint, done, total, failed int) error {
	return r.db.Model(&model.NovelCrawlJob{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"progress":     done,
			"total_chaps":  total,
			"failed_count": failed,
		}).Error
}

// ListRunning 返回所有处于 running 状态的爬取任务（用于启动时恢复）
func (r *NovelCrawlJobRepository) ListRunning() ([]*model.NovelCrawlJob, error) {
	var jobs []*model.NovelCrawlJob
	err := r.db.Where("status = ?", "running").Order("id ASC").Find(&jobs).Error
	return jobs, err
}

// HasRunningJob 检查指定小说是否已有处于 running 状态的爬取任务（防并发重复爬取）
func (r *NovelCrawlJobRepository) HasRunningJob(novelID uint) (bool, error) {
	var cnt int64
	err := r.db.Model(&model.NovelCrawlJob{}).
		Where("novel_id = ? AND status = 'running'", novelID).
		Count(&cnt).Error
	return cnt > 0, err
}

// Finalize 完成爬取任务（更新最终状态）
func (r *NovelCrawlJobRepository) Finalize(id uint, status string, done, total, failed int) error {
	now := time.Now()
	return r.db.Model(&model.NovelCrawlJob{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":       status,
			"progress":     done,
			"total_chaps":  total,
			"failed_count": failed,
			"completed_at": &now,
		}).Error
}

// SetError 记录任务失败原因（status 已由 Finalize 更新，此方法仅补写 error_message）
func (r *NovelCrawlJobRepository) SetError(id uint, errMsg string) error {
	return r.db.Model(&model.NovelCrawlJob{}).Where("id = ?", id).
		Update("error_message", errMsg).Error
}

// ──────────────────────────────────────────────
// NovelOutlineVersionRepository 小说大纲历史版本仓库
// ──────────────────────────────────────────────

// NovelOutlineVersionRepository 管理小说大纲历史版本
type NovelOutlineVersionRepository struct {
	db *gorm.DB
}

// NewNovelOutlineVersionRepository 构造函数
func NewNovelOutlineVersionRepository(db *gorm.DB) *NovelOutlineVersionRepository {
	return &NovelOutlineVersionRepository{db: db}
}

// Create 创建版本记录
func (r *NovelOutlineVersionRepository) Create(v *model.NovelOutlineVersion) error {
	return r.db.Create(v).Error
}

// ListByNovel 列出小说的所有版本（降序）
func (r *NovelOutlineVersionRepository) ListByNovel(novelID uint) ([]*model.NovelOutlineVersion, error) {
	var list []*model.NovelOutlineVersion
	return list, r.db.Where("novel_id = ?", novelID).Order("version DESC").Find(&list).Error
}

// MaxVersion 查询当前最大版本号（无记录时返回 0）
func (r *NovelOutlineVersionRepository) MaxVersion(novelID uint) (int, error) {
	var v int
	err := r.db.Model(&model.NovelOutlineVersion{}).Where("novel_id = ?", novelID).
		Select("COALESCE(MAX(version),0)").Scan(&v).Error
	return v, err
}

// CreateVersionAtomic assigns a monotonic version number and persists the record in a single
// transaction, eliminating the read-then-write race when multiple instances run concurrently.
func (r *NovelOutlineVersionRepository) CreateVersionAtomic(v *model.NovelOutlineVersion) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var maxNo struct{ V *int }
		if err := tx.Raw(
			"SELECT COALESCE(MAX(version), 0) AS v FROM ink_novel_outline_version WHERE novel_id = ? FOR UPDATE",
			v.NovelID,
		).Scan(&maxNo).Error; err != nil {
			return err
		}
		if maxNo.V == nil {
			v.Version = 1
		} else {
			v.Version = *maxNo.V + 1
		}
		return tx.Create(v).Error
	})
}
