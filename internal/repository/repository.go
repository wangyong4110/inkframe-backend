package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// NovelRepository 小说仓库
type NovelRepository struct {
	db    *gorm.DB
	cache *redis.Client
}

const novelCacheTTL = 30 * time.Minute

func NewNovelRepository(db *gorm.DB, cache *redis.Client) *NovelRepository {
	return &NovelRepository{db: db, cache: cache}
}

// Create 创建小说
func (r *NovelRepository) Create(novel *model.Novel) error {
	if err := r.db.Create(novel).Error; err != nil {
		return err
	}
	r.invalidateCache(novel.ID)
	return nil
}

// GetByID 根据ID获取小说
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

	// 2. 查 DB
	var novel model.Novel
	if err := r.db.Preload("Worldview").Preload("VideoConfig").First(&novel, id).Error; err != nil {
		return nil, err
	}

	// 3. 写入缓存
	if r.cache != nil {
		if data, err := json.Marshal(novel); err == nil {
			r.cache.Set(context.Background(), fmt.Sprintf("novel:%d", id), data, novelCacheTTL)
		}
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

// List 获取小说列表
func (r *NovelRepository) List(page, pageSize int, filters map[string]interface{}) ([]*model.Novel, int64, error) {
	var novels []*model.Novel
	var total int64

	query := r.db.Model(&model.Novel{})

	// 应用过滤
	if tenantID, ok := filters["tenant_id"]; ok {
		query = query.Where("tenant_id = ?", tenantID)
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

	// 分页查询（列表视图不需要 Worldview 完整数据，novel.WorldviewID 字段已足够）
	offset := (page - 1) * pageSize
	if err := query.
		Order("updated_at DESC").
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
		if err := r.db.Save(novel.VideoConfig).Error; err != nil {
			logger.Printf("[NovelRepository] Save VideoConfig: %v", err)
		}
	}
	r.invalidateCache(novel.ID)
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

// isSchemaMissing 判断是否为"列/表不存在"类错误（MySQL 1054/1146），遇到此类错误跳过而非中断
func isSchemaMissing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// MySQL: 1054 Unknown column / 1146 Table doesn't exist
	return strings.Contains(s, "1054") || strings.Contains(s, "1146") ||
		strings.Contains(s, "Unknown column") || strings.Contains(s, "doesn't exist") ||
		strings.Contains(s, "no such table") // SQLite
}

// DeleteWithCascade 物理删除小说及其全部关联数据（在事务中按依赖顺序执行）
// 对"列/表不存在"类错误（schema 尚未迁移）采用 skip 策略，不中断事务。
func (r *NovelRepository) DeleteWithCascade(id uint) error {
	err := r.db.Transaction(func(tx *gorm.DB) error {
		// tryExec 执行 SQL；若因 schema 缺失（列/表不存在）失败则记录日志并跳过
		tryExec := func(sql string, args ...interface{}) error {
			if e := tx.Exec(sql, args...).Error; e != nil {
				if isSchemaMissing(e) {
					logger.Printf("[DeleteNovel] skip (schema not ready): %v", e)
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
		if e := tryExec(`DELETE FROM ink_character_appearance WHERE character_id IN (SELECT id FROM ink_character WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 5. 章节物品关联（通过 item.novel_id）
		if e := tryExec(`DELETE FROM ink_chapter_item WHERE item_id IN (SELECT id FROM ink_item WHERE novel_id = ?)`, id); e != nil {
			return e
		}

		// ── 6. 扩展表（novel_id 直接关联；部分表可能尚未迁移，tryExec 会跳过）
		extStmts := []string{
			`DELETE FROM ink_video WHERE novel_id = ?`,
			`DELETE FROM ink_scene_anchor WHERE novel_id = ?`,
			`DELETE FROM ink_arc_summary WHERE novel_id = ?`,
			`DELETE FROM ink_quality_report WHERE novel_id = ?`,
			`DELETE FROM ink_review_task WHERE novel_id = ?`,
			`DELETE FROM ink_feedback_record WHERE novel_id = ?`,
			`DELETE FROM ink_plot_point WHERE novel_id = ?`,
			`DELETE FROM ink_model_usage_log WHERE novel_id = ?`,
			`DELETE FROM ink_async_task WHERE novel_id = ?`,
			`DELETE FROM ink_hook_chain WHERE novel_id = ?`,
			`DELETE FROM ink_satisfaction_point WHERE novel_id = ?`,
			`DELETE FROM ink_conflict_arc WHERE novel_id = ?`,
			`DELETE FROM ink_knowledge_base WHERE novel_id = ?`,
			`DELETE FROM ink_media_asset WHERE novel_id = ?`,
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
			`DELETE FROM ink_skill WHERE novel_id = ?`,
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
	var result struct {
		Count int
		Words int
	}
	r.db.Model(&model.Chapter{}).
		Select("COUNT(*) as count, COALESCE(SUM(word_count), 0) as words").
		Where("novel_id = ?", novelID).
		Scan(&result)
	if err := r.db.Model(&model.Novel{}).Where("id = ?", novelID).Updates(map[string]interface{}{
		"chapter_count": result.Count,
		"total_words":   result.Words,
	}).Error; err != nil {
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

// ChapterRepository 章节仓库
type ChapterRepository struct {
	db    *gorm.DB
	cache *redis.Client
}

const chapterListCacheTTL = 5 * time.Minute

func NewChapterRepository(db *gorm.DB, cache *redis.Client) *ChapterRepository {
	return &ChapterRepository{db: db, cache: cache}
}

// Create 创建章节
func (r *ChapterRepository) Create(chapter *model.Chapter) error {
	if err := r.db.Create(chapter).Error; err != nil {
		return err
	}
	r.invalidateListCache(chapter.NovelID)
	return nil
}

// GetByID 根据ID获取章节
func (r *ChapterRepository) GetByID(id uint) (*model.Chapter, error) {
	var chapter model.Chapter
	if err := r.db.First(&chapter, id).Error; err != nil {
		return nil, err
	}
	return &chapter, nil
}

// GetByNovelAndChapterNo 根据小说ID和章节号获取
func (r *ChapterRepository) GetByNovelAndChapterNo(novelID uint, chapterNo int) (*model.Chapter, error) {
	var chapter model.Chapter
	if err := r.db.Where("novel_id = ? AND chapter_no = ?", novelID, chapterNo).First(&chapter).Error; err != nil {
		return nil, err
	}
	return &chapter, nil
}

// chapterListColumns 章节列表只需元数据字段，排除 content/outline/scene_outline/plot_points/chapter_hook/summary 等大文本列。
// 100章 × ~3KB content = ~300KB 节省，减少 DB I/O 和网络传输。
const chapterListColumns = "id, novel_id, tenant_id, uuid, chapter_no, title, status, word_count, " +
	"tension_level, act_no, emotional_tone, hook_type, " +
	"created_at, updated_at, deleted_at"

func (r *ChapterRepository) chapterListCacheKey(novelID uint) string {
	return fmt.Sprintf("chapters:novel:%d", novelID)
}

func (r *ChapterRepository) invalidateListCache(novelID uint) {
	if r.cache != nil {
		r.cache.Del(context.Background(), r.chapterListCacheKey(novelID))
	}
}

// ListByNovel 获取小说的所有章节（列表元数据，不含正文/大纲等大字段）。
// 查询结果缓存 5 分钟，写操作（Create/Update/Delete）自动失效。
func (r *ChapterRepository) ListByNovel(novelID uint) ([]*model.Chapter, error) {
	// 1. 尝试 Redis 缓存
	if r.cache != nil {
		key := r.chapterListCacheKey(novelID)
		if cached, err := r.cache.Get(context.Background(), key).Result(); err == nil {
			var chapters []*model.Chapter
			if json.Unmarshal([]byte(cached), &chapters) == nil {
				return chapters, nil
			}
		}
	}

	// 2. 列投影：只查元数据列，跳过大文本字段
	var chapters []*model.Chapter
	if err := r.db.Select(chapterListColumns).
		Where("novel_id = ?", novelID).
		Order("chapter_no ASC").
		Find(&chapters).Error; err != nil {
		return nil, err
	}

	// 3. 写入缓存
	if r.cache != nil {
		if data, err := json.Marshal(chapters); err == nil {
			r.cache.Set(context.Background(), r.chapterListCacheKey(novelID), data, chapterListCacheTTL)
		}
	}
	return chapters, nil
}

// ListByNovelPaged 分页获取章节列表（列元数据，不含正文大字段，不走缓存）。
func (r *ChapterRepository) ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.Chapter, int64, error) {
	var chapters []*model.Chapter
	var total int64

	base := r.db.Model(&model.Chapter{}).Where("novel_id = ?", novelID)
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	if err := base.Select(chapterListColumns).Order("chapter_no ASC").Offset(offset).Limit(pageSize).Find(&chapters).Error; err != nil {
		return nil, 0, err
	}
	return chapters, total, nil
}

// ListByNovelWithContent 获取小说的所有章节，包含 content 和 summary 字段。
// 用于 AI 提取任务（角色/物品/技能等），不走缓存。
func (r *ChapterRepository) ListByNovelWithContent(novelID uint) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	return chapters, r.db.Where("novel_id = ?", novelID).
		Order("chapter_no ASC").Find(&chapters).Error
}

// GetByNovelAndChapterRange 批量获取章节范围（含首尾，一次 SQL 代替循环 GetByNovelAndChapterNo）
func (r *ChapterRepository) GetByNovelAndChapterRange(novelID uint, start, end int) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	return chapters, r.db.Where("novel_id = ? AND chapter_no BETWEEN ? AND ?", novelID, start, end).
		Order("chapter_no ASC").Find(&chapters).Error
}

// GetRecent 获取最近N章
func (r *ChapterRepository) GetRecent(novelID uint, currentChapterNo, count int) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	if err := r.db.Where("novel_id = ? AND chapter_no < ?", novelID, currentChapterNo).
		Order("chapter_no DESC").
		Limit(count).
		Find(&chapters).Error; err != nil {
		return nil, err
	}
	return chapters, nil
}

// Update 更新章节
func (r *ChapterRepository) Update(chapter *model.Chapter) error {
	if err := r.db.Save(chapter).Error; err != nil {
		return err
	}
	r.invalidateListCache(chapter.NovelID)
	return nil
}

// Delete 删除章节（novelID 用于缓存失效）
func (r *ChapterRepository) Delete(id, novelID uint) error {
	if err := r.db.Delete(&model.Chapter{}, id).Error; err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// CountByNovel 统计小说章节数
func (r *ChapterRepository) CountByNovel(novelID uint) (int64, error) {
	var count int64
	if err := r.db.Model(&model.Chapter{}).Where("novel_id = ?", novelID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// ListPendingCrawl 获取待爬取章节（outline 以 "crawl:" 开头且 content 为空）
func (r *ChapterRepository) ListPendingCrawl(novelID uint) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	err := r.db.Where("novel_id = ? AND outline LIKE 'crawl:%' AND (content = '' OR content IS NULL)", novelID).
		Order("chapter_no ASC").Find(&chapters).Error
	return chapters, err
}

// UpdateCrawledContent 将爬取完成的内容写回章节
func (r *ChapterRepository) UpdateCrawledContent(id uint, title, content string, wordCount int) error {
	return r.db.Model(&model.Chapter{}).Where("id = ?", id).Updates(map[string]interface{}{
		"title":      title,
		"content":    content,
		"outline":    "",
		"word_count": wordCount,
		"status":     "published",
	}).Error
}

// CharacterRepository 角色仓库
type CharacterRepository struct {
	db *gorm.DB
}

func NewCharacterRepository(db *gorm.DB) *CharacterRepository {
	return &CharacterRepository{db: db}
}

// Create 创建角色
func (r *CharacterRepository) Create(character *model.Character) error {
	return r.db.Create(character).Error
}

// GetByID 根据ID获取角色
func (r *CharacterRepository) GetByID(id uint) (*model.Character, error) {
	var character model.Character
	if err := r.db.First(&character, id).Error; err != nil {
		return nil, err
	}
	return &character, nil
}

// ListByNovel 获取小说的所有角色
func (r *CharacterRepository) ListByNovel(novelID uint) ([]*model.Character, error) {
	var characters []*model.Character
	if err := r.db.Where("novel_id = ?", novelID).Find(&characters).Error; err != nil {
		return nil, err
	}
	return characters, nil
}

// ListByIDs 批量获取指定ID的角色（单次 IN 查询，避免 N+1）
func (r *CharacterRepository) ListByIDs(ids []uint) ([]*model.Character, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var chars []*model.Character
	return chars, r.db.Where("id IN ?", ids).Find(&chars).Error
}

// Update 更新角色
func (r *CharacterRepository) Update(character *model.Character) error {
	return r.db.Save(character).Error
}

// Delete 删除角色
func (r *CharacterRepository) Delete(id uint) error {
	return r.db.Delete(&model.Character{}, id).Error
}

// GetActiveInChapter 获取章节中活跃的角色
func (r *CharacterRepository) GetActiveInChapter(chapterID uint) ([]*model.CharacterAppearance, error) {
	var appearances []*model.CharacterAppearance
	if err := r.db.Preload("Character").
		Where("chapter_id = ? AND role_in_chapter != ?", chapterID, "mentioned").
		Find(&appearances).Error; err != nil {
		return nil, err
	}
	return appearances, nil
}

// RecordAppearance 记录角色出场
func (r *CharacterRepository) RecordAppearance(appearance *model.CharacterAppearance) error {
	return r.db.Create(appearance).Error
}

// WorldviewRepository 世界观仓库
type WorldviewRepository struct {
	db *gorm.DB
}

func NewWorldviewRepository(db *gorm.DB) *WorldviewRepository {
	return &WorldviewRepository{db: db}
}

func (r *WorldviewRepository) DB() *gorm.DB { return r.db }

// Create 创建世界观
func (r *WorldviewRepository) Create(worldview *model.Worldview) error {
	return r.db.Create(worldview).Error
}

// GetByID 根据ID获取世界观
func (r *WorldviewRepository) GetByID(id uint) (*model.Worldview, error) {
	var worldview model.Worldview
	if err := r.db.First(&worldview, id).Error; err != nil {
		return nil, err
	}
	return &worldview, nil
}

// List 获取世界观列表
func (r *WorldviewRepository) List(page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	var worldviews []*model.Worldview
	var total int64

	query := r.db.Model(&model.Worldview{})
	if genre != "" {
		query = query.Where("genre = ?", genre)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := query.Order("used_count DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&worldviews).Error; err != nil {
		return nil, 0, err
	}

	return worldviews, total, nil
}

// Update 更新世界观
func (r *WorldviewRepository) Update(worldview *model.Worldview) error {
	return r.db.Save(worldview).Error
}

// Delete 删除世界观
func (r *WorldviewRepository) Delete(id uint) error {
	return r.db.Delete(&model.Worldview{}, id).Error
}

// IncrementUsageCount 增加使用次数
func (r *WorldviewRepository) IncrementUsageCount(id uint) error {
	return r.db.Model(&model.Worldview{}).Where("id = ?", id).
		UpdateColumn("used_count", gorm.Expr("used_count + 1")).Error
}

// GetEntities 获取世界观的所有实体
func (r *WorldviewRepository) GetEntities(worldviewID uint) ([]*model.WorldviewEntity, error) {
	var entities []*model.WorldviewEntity
	if err := r.db.Where("worldview_id = ?", worldviewID).Find(&entities).Error; err != nil {
		return nil, err
	}
	return entities, nil
}

// CreateEntity 创建世界观实体
func (r *WorldviewRepository) CreateEntity(entity *model.WorldviewEntity) error {
	return r.db.Create(entity).Error
}

// UpdateEntity 更新世界观实体
func (r *WorldviewRepository) UpdateEntity(entity *model.WorldviewEntity) error {
	return r.db.Save(entity).Error
}

// DeleteEntity 删除世界观实体
func (r *WorldviewRepository) DeleteEntity(id uint) error {
	return r.db.Delete(&model.WorldviewEntity{}, id).Error
}

// AIModelRepository AI模型仓库
type AIModelRepository struct {
	db *gorm.DB
}

func NewAIModelRepository(db *gorm.DB) *AIModelRepository {
	return &AIModelRepository{db: db}
}

// GetAvailableByTaskType 获取任务可用的模型。
// suitable_tasks 列存储 JSON 数组字符串（如 `["chapter","image"]`）；使用 LIKE 在 DB 层过滤，
// 兼容 MySQL 和 SQLite，无需全量加载后在内存中遍历。
func (r *AIModelRepository) GetAvailableByTaskType(taskType string) ([]*model.AIModel, error) {
	var models []*model.AIModel
	// LIKE pattern matches `"taskType"` as a JSON array element substring.
	pattern := `%"` + taskType + `"%`
	if err := r.db.Preload("Provider").
		Where("is_active = ? AND is_available = ? AND suitable_tasks LIKE ?", true, true, pattern).
		Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// GetByID 根据ID获取模型
func (r *AIModelRepository) GetByID(id uint) (*model.AIModel, error) {
	var model model.AIModel
	if err := r.db.Preload("Provider").First(&model, id).Error; err != nil {
		return nil, err
	}
	return &model, nil
}

// GetByName 按模型名称查找（如 "deepseek-chat"），返回第一个匹配的活跃模型及其提供商
func (r *AIModelRepository) GetByName(name string) (*model.AIModel, error) {
	var m model.AIModel
	if err := r.db.Preload("Provider").Where("name = ? AND is_active = ?", name, true).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// List 获取所有模型
func (r *AIModelRepository) List(providerID *uint) ([]*model.AIModel, error) {
	var models []*model.AIModel
	query := r.db.Preload("Provider")

	if providerID != nil {
		query = query.Where("provider_id = ?", *providerID)
	}

	if err := query.Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// Create 创建模型
func (r *AIModelRepository) Create(model *model.AIModel) error {
	return r.db.Create(model).Error
}

// Update 更新模型
func (r *AIModelRepository) Update(model *model.AIModel) error {
	return r.db.Save(model).Error
}

// Delete 删除AI模型
func (r *AIModelRepository) Delete(id uint) error {
	return r.db.Delete(&model.AIModel{}, id).Error
}

// UpdateHealthStatus 更新健康状态
func (r *AIModelRepository) UpdateHealthStatus(providerID uint, status string) error {
	return r.db.Model(&model.ModelProvider{}).Where("id = ?", providerID).
		Updates(map[string]interface{}{
			"health_check": status,
			"last_checked": time.Now(),
		}).Error
}

// LogUsage 记录使用（忽略外键约束错误，使用日志为非关键数据）
func (r *AIModelRepository) LogUsage(log *model.ModelUsageLog) error {
	err := r.db.Create(log).Error
	if err != nil && isForeignKeyError(err) {
		return nil // model_id 引用不存在时静默跳过，不影响主流程
	}
	return err
}

// isForeignKeyError 判断是否为 MySQL 外键约束错误（1452）
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1452") || strings.Contains(msg, "foreign key constraint")
}

// GetUsageStats 获取使用统计
func (r *AIModelRepository) GetUsageStats(modelID uint, startTime, endTime time.Time) (*UsageStats, error) {
	var stats UsageStats
	type aggRow struct {
		TotalRequests int
		SuccessCount  int
		TotalTokens   int
		TotalCost     float64
		TotalLatency  float64
	}
	var row aggRow
	err := r.db.Model(&model.ModelUsageLog{}).
		Select("COUNT(*) AS total_requests, SUM(CASE WHEN success THEN 1 ELSE 0 END) AS success_count, "+
			"COALESCE(SUM(total_tokens), 0) AS total_tokens, COALESCE(SUM(cost), 0) AS total_cost, "+
			"COALESCE(SUM(latency), 0) AS total_latency").
		Where("model_id = ? AND created_at BETWEEN ? AND ?", modelID, startTime, endTime).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	stats.TotalRequests = row.TotalRequests
	stats.SuccessCount = row.SuccessCount
	stats.TotalTokens = row.TotalTokens
	stats.TotalCost = row.TotalCost
	stats.TotalLatency = row.TotalLatency
	if stats.TotalRequests > 0 {
		stats.AverageLatency = stats.TotalLatency / float64(stats.TotalRequests)
		stats.SuccessRate = float64(stats.SuccessCount) / float64(stats.TotalRequests)
	}
	return &stats, nil
}

// UsageStats 使用统计
type UsageStats struct {
	TotalRequests int
	SuccessCount  int
	TotalTokens   int
	TotalCost     float64
	TotalLatency  float64
	AverageLatency float64
	SuccessRate   float64
}

// TaskModelConfigRepository 任务模型配置仓库
type TaskModelConfigRepository struct {
	db *gorm.DB
}

func NewTaskModelConfigRepository(db *gorm.DB) *TaskModelConfigRepository {
	return &TaskModelConfigRepository{db: db}
}

// GetByTaskType 获取任务配置
func (r *TaskModelConfigRepository) GetByTaskType(taskType string) (*model.TaskModelConfig, error) {
	var config model.TaskModelConfig
	if err := r.db.Preload("PrimaryModel").
		Where("task_type = ? AND is_active = ?", taskType, true).
		First(&config).Error; err != nil {
		return nil, err
	}
	return &config, nil
}

// Create 创建配置
func (r *TaskModelConfigRepository) Create(config *model.TaskModelConfig) error {
	return r.db.Create(config).Error
}

// Update 更新配置
func (r *TaskModelConfigRepository) Update(config *model.TaskModelConfig) error {
	return r.db.Save(config).Error
}

// KnowledgeBaseRepository 知识库仓库
type KnowledgeBaseRepository struct {
	db *gorm.DB
}

func NewKnowledgeBaseRepository(db *gorm.DB) *KnowledgeBaseRepository {
	return &KnowledgeBaseRepository{db: db}
}

// Create 创建知识
func (r *KnowledgeBaseRepository) Create(kb *model.KnowledgeBase) error {
	return r.db.Create(kb).Error
}

// Search 搜索知识
func (r *KnowledgeBaseRepository) Search(keyword string, limit int) ([]*model.KnowledgeBase, error) {
	var results []*model.KnowledgeBase
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(keyword)
	pattern := "%" + escaped + "%"
	if err := r.db.Where("title LIKE ? OR content LIKE ?", pattern, pattern).
		Limit(limit).
		Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

// GetByNovel 获取小说的所有知识
func (r *KnowledgeBaseRepository) GetByNovel(novelID uint) ([]*model.KnowledgeBase, error) {
	var results []*model.KnowledgeBase
	if err := r.db.Where("novel_id = ?", novelID).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

// GetByID 根据ID获取知识库条目
func (r *KnowledgeBaseRepository) GetByID(id uint) (*model.KnowledgeBase, error) {
	var kb model.KnowledgeBase
	if err := r.db.First(&kb, id).Error; err != nil {
		return nil, err
	}
	return &kb, nil
}

// Update 更新知识库
func (r *KnowledgeBaseRepository) Update(kb *model.KnowledgeBase) error {
	return r.db.Save(kb).Error
}

// IncrementUsageCount 增加使用次数
func (r *KnowledgeBaseRepository) IncrementUsageCount(id uint) error {
	return r.db.Model(&model.KnowledgeBase{}).Where("id = ?", id).
		UpdateColumn("usage_count", gorm.Expr("usage_count + 1")).Error
}

// VideoRepository 视频仓库
type VideoRepository struct {
	db *gorm.DB
}

func NewVideoRepository(db *gorm.DB) *VideoRepository {
	return &VideoRepository{db: db}
}

// Create 创建视频
func (r *VideoRepository) Create(video *model.Video) error {
	return r.db.Create(video).Error
}

// GetByID 根据ID获取视频
func (r *VideoRepository) GetByID(id uint) (*model.Video, error) {
	var video model.Video
	if err := r.db.First(&video, id).Error; err != nil {
		return nil, err
	}
	return &video, nil
}

// GetByIDAndTenant 根据ID和租户获取视频（防止越权访问）
// 优先使用 ink_videos.tenant_id 直接过滤（无需 JOIN）；
// tenant_id=0 的旧记录视为公共数据，任意租户均可访问（兼容迁移前数据）。
func (r *VideoRepository) GetByIDAndTenant(id, tenantID uint) (*model.Video, error) {
	var video model.Video
	err := r.db.
		Where("id = ? AND (tenant_id = ? OR tenant_id = 0)", id, tenantID).
		First(&video).Error
	if err != nil {
		return nil, err
	}
	return &video, nil
}

// List 获取视频列表
func (r *VideoRepository) List(novelID *uint, chapterID *uint, tenantID uint, page, pageSize int) ([]*model.Video, int64, error) {
	var videos []*model.Video
	var total int64

	query := r.db.Model(&model.Video{}).Session(&gorm.Session{})
	if tenantID > 0 {
		query = query.Where("tenant_id = ? OR tenant_id = 0", tenantID)
	}
	if novelID != nil {
		query = query.Where("novel_id = ?", *novelID)
	}
	if chapterID != nil {
		query = query.Where("chapter_id = ?", *chapterID)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := query.Order("created_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&videos).Error; err != nil {
		return nil, 0, err
	}

	return videos, total, nil
}

// Update 更新视频
func (r *VideoRepository) Update(video *model.Video) error {
	return r.db.Save(video).Error
}

// DeleteByID 删除视频
func (r *VideoRepository) DeleteByID(id uint) error {
	return r.db.Delete(&model.Video{}, id).Error
}

// UpdateFields 更新视频任意字段
func (r *VideoRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).Updates(fields).Error
}

// ListPublic 列出公开发布的视频（用于广场）
func (r *VideoRepository) ListPublic(page, pageSize int) ([]*model.Video, int64, error) {
	return r.ListPublicSorted("hot", "", page, pageSize)
}

// ListPublicSorted 排序列出公开视频（sort: latest|hot；q: 关键词搜索）
func (r *VideoRepository) ListPublicSorted(sort, q string, page, pageSize int) ([]*model.Video, int64, error) {
	var videos []*model.Video
	var total int64
	base := r.db.Model(&model.Video{}).Where("is_published = ? AND visibility = ?", true, "public")
	if q != "" {
		base = base.Where("title LIKE ? OR description LIKE ?", "%"+q+"%", "%"+q+"%")
	}
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := "hot_score DESC, published_at DESC"
	if sort == "latest" {
		order = "published_at DESC, created_at DESC"
	}
	offset := (page - 1) * pageSize
	if err := base.Order(order).Offset(offset).Limit(pageSize).Find(&videos).Error; err != nil {
		return nil, 0, err
	}
	return videos, total, nil
}

// GetPublicByID 获取单条公开视频（不需要 tenantID）
func (r *VideoRepository) GetPublicByID(id uint) (*model.Video, error) {
	var v model.Video
	err := r.db.Where("id = ? AND is_published = ? AND visibility = ?", id, true, "public").First(&v).Error
	return &v, err
}

// IncrViewCount 视频播放量+1
func (r *VideoRepository) IncrViewCount(id uint) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).UpdateColumn("view_count", gorm.Expr("view_count + 1")).Error
}

// IncrLikeCount 点赞数 delta（+1 或 -1）
func (r *VideoRepository) IncrLikeCount(id uint, delta int) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).UpdateColumn("like_count", gorm.Expr("like_count + ?", delta)).Error
}

// IncrCommentCount 评论数 delta（+1 或 -1）
func (r *VideoRepository) IncrCommentCount(id uint, delta int) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).UpdateColumn("comment_count", gorm.Expr("comment_count + ?", delta)).Error
}

// UpdateHotScore 更新热度分
func (r *VideoRepository) UpdateHotScore(id uint, score float64) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).Update("hot_score", score).Error
}

// ListPublicForHotCalc 列出所有公开视频用于热度分批量计算
func (r *VideoRepository) ListPublicForHotCalc() ([]*model.Video, error) {
	var videos []*model.Video
	err := r.db.Model(&model.Video{}).Where("is_published = ? AND visibility = ?", true, "public").
		Select("id, view_count, like_count, comment_count, published_at").Find(&videos).Error
	return videos, err
}

// ─── VideoLikeRepository ────────────────────────────────────────────────────

type VideoLikeRepository struct{ db *gorm.DB }

func NewVideoLikeRepository(db *gorm.DB) *VideoLikeRepository {
	return &VideoLikeRepository{db: db}
}

// Toggle 点赞/取消，返回最终状态
func (r *VideoLikeRepository) Toggle(videoID, userID uint) (liked bool, err error) {
	var like model.VideoLike
	result := r.db.Where("video_id = ? AND user_id = ?", videoID, userID).First(&like)
	if result.Error != nil {
		// 不存在 → 创建
		if err2 := r.db.Create(&model.VideoLike{VideoID: videoID, UserID: userID}).Error; err2 != nil {
			return false, err2
		}
		return true, nil
	}
	// 已存在 → 删除
	return false, r.db.Delete(&like).Error
}

// Exists 检查是否已点赞
func (r *VideoLikeRepository) Exists(videoID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.VideoLike{}).Where("video_id = ? AND user_id = ?", videoID, userID).Count(&count).Error
	return count > 0, err
}

// ─── VideoCommentRepository ──────────────────────────────────────────────────

type VideoCommentRepository struct{ db *gorm.DB }

func NewVideoCommentRepository(db *gorm.DB) *VideoCommentRepository {
	return &VideoCommentRepository{db: db}
}

func (r *VideoCommentRepository) Create(c *model.VideoComment) error {
	return r.db.Create(c).Error
}

func (r *VideoCommentRepository) ListByVideo(videoID uint, page, size int) ([]*model.VideoComment, int64, error) {
	var list []*model.VideoComment
	var total int64
	base := r.db.Model(&model.VideoComment{}).Where("video_id = ?", videoID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	if err := base.Order("created_at DESC").Offset(offset).Limit(size).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (r *VideoCommentRepository) GetByID(id uint) (*model.VideoComment, error) {
	var c model.VideoComment
	return &c, r.db.First(&c, id).Error
}

func (r *VideoCommentRepository) Delete(id uint) error {
	return r.db.Delete(&model.VideoComment{}, id).Error
}

// StoryboardRepository 分镜仓库
type StoryboardRepository struct {
	db *gorm.DB
}

func NewStoryboardRepository(db *gorm.DB) *StoryboardRepository {
	return &StoryboardRepository{db: db}
}

// Create 创建分镜
func (r *StoryboardRepository) Create(shot *model.StoryboardShot) error {
	return r.db.Create(shot).Error
}

// BatchCreate 批量插入分镜（单次 SQL，避免 N 次往返）
func (r *StoryboardRepository) BatchCreate(shots []*model.StoryboardShot) error {
	if len(shots) == 0 {
		return nil
	}
	return r.db.CreateInBatches(shots, 100).Error
}

// GetByID 根据ID获取分镜
func (r *StoryboardRepository) GetByID(id uint) (*model.StoryboardShot, error) {
	var shot model.StoryboardShot
	if err := r.db.First(&shot, id).Error; err != nil {
		return nil, err
	}
	return &shot, nil
}

// ListByVideo 获取视频的所有分镜
func (r *StoryboardRepository) ListByVideo(videoID uint) ([]*model.StoryboardShot, error) {
	var shots []*model.StoryboardShot
	if err := r.db.Where("video_id = ?", videoID).Order("shot_no ASC").Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// ListByVideoAndStatus 按视频ID和状态获取分镜
func (r *StoryboardRepository) ListByVideoAndStatus(videoID uint, status string) ([]*model.StoryboardShot, error) {
	var shots []*model.StoryboardShot
	if err := r.db.Where("video_id = ? AND status = ?", videoID, status).Order("shot_no ASC").Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// Update 更新分镜
func (r *StoryboardRepository) Update(shot *model.StoryboardShot) error {
	return r.db.Save(shot).Error
}

// UpdateFields 按 map 部分更新分镜字段（空字符串也会写入，支持清空字段）
func (r *StoryboardRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", id).Updates(fields).Error
}

// BatchGetByIDs 批量获取分镜（单次 IN 查询）
func (r *StoryboardRepository) BatchGetByIDs(ids []uint) ([]*model.StoryboardShot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var shots []*model.StoryboardShot
	if err := r.db.Where("id IN ?", ids).Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// UpdateSFXTags 仅更新分镜的 sfx_tags 字段，不修改 sfx_url 和 sfx_volume
func (r *StoryboardRepository) UpdateSFXTags(shotID uint, sfxTags string) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", shotID).Update("sfx_tags", sfxTags).Error
}

// UpdateSFX 更新单个分镜的音效字段（URL、标签、混音音量）
func (r *StoryboardRepository) UpdateSFX(shotID uint, sfxURL, sfxTags string, sfxVolume float64) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", shotID).Updates(map[string]interface{}{
		"sfx_url":    sfxURL,
		"sfx_tags":   sfxTags,
		"sfx_volume": sfxVolume,
	}).Error
}

// DeleteByVideoID 硬删除视频的所有分镜（重新生成时使用）
// 必须用 Unscoped() 物理删除，否则软删除的行仍触发 uk_video_shot 唯一键冲突。
func (r *StoryboardRepository) DeleteByVideoID(videoID uint) error {
	return r.db.Unscoped().Where("video_id = ?", videoID).Delete(&model.StoryboardShot{}).Error
}

// Delete 硬删除单个分镜
func (r *StoryboardRepository) Delete(shotID uint) error {
	return r.db.Unscoped().Delete(&model.StoryboardShot{}, shotID).Error
}

// MaxShotNo 返回视频中最大的 shot_no（无分镜时返回 0）
func (r *StoryboardRepository) MaxShotNo(videoID uint) (int, error) {
	var max int
	err := r.db.Model(&model.StoryboardShot{}).
		Where("video_id = ? AND deleted_at IS NULL", videoID).
		Select("COALESCE(MAX(shot_no), 0)").Scan(&max).Error
	return max, err
}

// ShiftShotNos 将 video_id 下所有 shot_no >= fromShotNo 的分镜的 shot_no 加 delta（delta 通常为 1）。
// 使用两阶段更新避免唯一键冲突：先整体偏移到无冲突区间，再还原到目标值。
func (r *StoryboardRepository) ShiftShotNos(videoID uint, fromShotNo, delta int) error {
	const tempOffset = 100000
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Phase 1: move into a temporary collision-free range
		if err := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			tempOffset, videoID, fromShotNo,
		).Error; err != nil {
			return err
		}
		// Phase 2: shift back to the intended final position
		return tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no - ? + ? WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			tempOffset, delta, videoID, fromShotNo+tempOffset,
		).Error
	})
}

// CompactShotNosAfter 将 video_id 下 shot_no > deletedShotNo 的分镜 shot_no 减 1（删除后紧凑化）。
// 同样使用两阶段更新。
func (r *StoryboardRepository) CompactShotNosAfter(videoID uint, deletedShotNo int) error {
	const tempOffset = 100000
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no > ? AND deleted_at IS NULL",
			tempOffset, videoID, deletedShotNo,
		).Error; err != nil {
			return err
		}
		return tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no - ? - 1 WHERE video_id = ? AND shot_no > ? AND deleted_at IS NULL",
			tempOffset, videoID, deletedShotNo+tempOffset,
		).Error
	})
}

// ShotVoiceSegmentRepository 分镜语音段落仓库
type ShotVoiceSegmentRepository struct {
	db *gorm.DB
}

func NewShotVoiceSegmentRepository(db *gorm.DB) *ShotVoiceSegmentRepository {
	return &ShotVoiceSegmentRepository{db: db}
}

// ListByShotID 获取分镜的所有语音段落，按 seq_no 升序
func (r *ShotVoiceSegmentRepository) ListByShotID(shotID uint) ([]*model.ShotVoiceSegment, error) {
	var segs []*model.ShotVoiceSegment
	err := r.db.Where("shot_id = ?", shotID).Order("seq_no ASC").Find(&segs).Error
	return segs, err
}

func (r *ShotVoiceSegmentRepository) GetByID(id uint) (*model.ShotVoiceSegment, error) {
	var seg model.ShotVoiceSegment
	if err := r.db.First(&seg, id).Error; err != nil {
		return nil, err
	}
	return &seg, nil
}

func (r *ShotVoiceSegmentRepository) Create(seg *model.ShotVoiceSegment) error {
	return r.db.Create(seg).Error
}

func (r *ShotVoiceSegmentRepository) Update(seg *model.ShotVoiceSegment) error {
	return r.db.Save(seg).Error
}

func (r *ShotVoiceSegmentRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.ShotVoiceSegment{}).Where("id = ?", id).Updates(fields).Error
}

func (r *ShotVoiceSegmentRepository) Delete(id uint) error {
	return r.db.Unscoped().Delete(&model.ShotVoiceSegment{}, id).Error
}

// MaxSeqNo 返回分镜中最大的 seq_no（无段落时返回 0）
func (r *ShotVoiceSegmentRepository) MaxSeqNo(shotID uint) (int, error) {
	var max int
	err := r.db.Model(&model.ShotVoiceSegment{}).
		Where("shot_id = ? AND deleted_at IS NULL", shotID).
		Select("COALESCE(MAX(seq_no), 0)").Scan(&max).Error
	return max, err
}

// ShiftSeqNos 将 shot_id 下所有 seq_no >= fromSeqNo 的段落的 seq_no 加 1（为插入腾出位置）
func (r *ShotVoiceSegmentRepository) ShiftSeqNos(shotID uint, fromSeqNo int) error {
	return r.db.Exec(
		"UPDATE ink_shot_voice_segment SET seq_no = seq_no + 1 WHERE shot_id = ? AND seq_no >= ? AND deleted_at IS NULL",
		shotID, fromSeqNo,
	).Error
}

// CompactSeqNosAfter 将 shot_id 下 seq_no > deletedSeqNo 的段落 seq_no 减 1（删除后紧凑化）
func (r *ShotVoiceSegmentRepository) CompactSeqNosAfter(shotID uint, deletedSeqNo int) error {
	return r.db.Exec(
		"UPDATE ink_shot_voice_segment SET seq_no = seq_no - 1 WHERE shot_id = ? AND seq_no > ? AND deleted_at IS NULL",
		shotID, deletedSeqNo,
	).Error
}

// CharacterStateSnapshotRepository 角色状态快照仓库
type CharacterStateSnapshotRepository struct {
	db *gorm.DB
}

func NewCharacterStateSnapshotRepository(db *gorm.DB) *CharacterStateSnapshotRepository {
	return &CharacterStateSnapshotRepository{db: db}
}

func (r *CharacterStateSnapshotRepository) Create(snapshot *model.CharacterStateSnapshot) error {
	return r.db.Create(snapshot).Error
}

func (r *CharacterStateSnapshotRepository) ListByCharacter(characterID uint) ([]*model.CharacterStateSnapshot, error) {
	var snapshots []*model.CharacterStateSnapshot
	err := r.db.Where("character_id = ?", characterID).Order("created_at DESC").Find(&snapshots).Error
	return snapshots, err
}

// GetByChapterAndCharacter 获取指定章节中特定角色的快照
func (r *CharacterStateSnapshotRepository) GetByChapterAndCharacter(chapterID, characterID uint) (*model.CharacterStateSnapshot, error) {
	var s model.CharacterStateSnapshot
	err := r.db.Where("chapter_id = ? AND character_id = ?", chapterID, characterID).First(&s).Error
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListByChapterID 批量获取指定章节的所有角色快照（一次查询，避免 N+1）
func (r *CharacterStateSnapshotRepository) ListByChapterID(chapterID uint) ([]*model.CharacterStateSnapshot, error) {
	var snapshots []*model.CharacterStateSnapshot
	if err := r.db.Where("chapter_id = ?", chapterID).Find(&snapshots).Error; err != nil {
		return nil, err
	}
	return snapshots, nil
}

// GetLatestForCharacter 获取某角色最新的快照（可选：只找 chapterID 之前创建的）
func (r *CharacterStateSnapshotRepository) GetLatestForCharacter(characterID uint) (*model.CharacterStateSnapshot, error) {
	var s model.CharacterStateSnapshot
	err := r.db.Where("character_id = ?", characterID).Order("created_at DESC").First(&s).Error
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ChapterVersionRepository 章节版本仓库
type ChapterVersionRepository struct {
	db *gorm.DB
}

func NewChapterVersionRepository(db *gorm.DB) *ChapterVersionRepository {
	return &ChapterVersionRepository{db: db}
}

// Create 创建版本
func (r *ChapterVersionRepository) Create(version *model.ChapterVersion) error {
	return r.db.Create(version).Error
}

// GetLatest 获取最新版本
func (r *ChapterVersionRepository) GetLatest(chapterID uint) (*model.ChapterVersion, error) {
	var version model.ChapterVersion
	if err := r.db.Where("chapter_id = ?", chapterID).
		Order("version_no DESC").
		First(&version).Error; err != nil {
		return nil, err
	}
	return &version, nil
}

// GetVersion 获取指定版本
func (r *ChapterVersionRepository) GetVersion(chapterID uint, versionNo int) (*model.ChapterVersion, error) {
	var version model.ChapterVersion
	if err := r.db.Where("chapter_id = ? AND version_no = ?", chapterID, versionNo).First(&version).Error; err != nil {
		return nil, err
	}
	return &version, nil
}

// List 获取章节所有版本
func (r *ChapterVersionRepository) List(chapterID uint) ([]*model.ChapterVersion, error) {
	var versions []*model.ChapterVersion
	if err := r.db.Where("chapter_id = ?", chapterID).
		Order("version_no DESC").
		Find(&versions).Error; err != nil {
		return nil, err
	}
	return versions, nil
}

// GetNextVersionNo 获取下一个版本号
func (r *ChapterVersionRepository) GetNextVersionNo(chapterID uint) (int, error) {
	var maxNo *int
	if err := r.db.Model(&model.ChapterVersion{}).
		Select("MAX(version_no)").
		Where("chapter_id = ?", chapterID).
		Scan(&maxNo).Error; err != nil {
		return 0, err
	}
	if maxNo == nil {
		return 1, nil
	}
	return *maxNo + 1, nil
}

// ============================================
// Model Repositories
// ============================================

// ModelProviderRepository 模型提供商仓库
type ModelProviderRepository struct {
	db *gorm.DB
}

func NewModelProviderRepository(db *gorm.DB) *ModelProviderRepository {
	return &ModelProviderRepository{db: db}
}

// List 获取提供商列表
func (r *ModelProviderRepository) List() ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// ListSystem 获取系统预置提供商列表（仅 tenant_id=0）
func (r *ModelProviderRepository) ListSystem() ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Where("tenant_id = 0").Order("id").Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// ListByTenant 获取租户提供商列表（含系统级 tenant_id=0）
func (r *ModelProviderRepository) ListByTenant(tenantID uint) ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Where("tenant_id = ? OR tenant_id = 0", tenantID).Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// GetByID 根据ID获取提供商
func (r *ModelProviderRepository) GetByID(id uint) (*model.ModelProvider, error) {
	var provider model.ModelProvider
	if err := r.db.First(&provider, id).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// GetByIDAndTenant 根据ID和租户获取提供商（仅租户自己的或系统级）
func (r *ModelProviderRepository) GetByIDAndTenant(id uint, tenantID uint) (*model.ModelProvider, error) {
	var provider model.ModelProvider
	if err := r.db.Where("id = ? AND (tenant_id = ? OR tenant_id = 0)", id, tenantID).First(&provider).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// GetSystemProvider 获取系统级提供商（tenant_id=0）
func (r *ModelProviderRepository) GetSystemProvider(name string) (*model.ModelProvider, error) {
	var provider model.ModelProvider
	if err := r.db.Where("name = ? AND tenant_id = 0", name).First(&provider).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// Create 创建提供商
func (r *ModelProviderRepository) Create(provider *model.ModelProvider) error {
	// 清理同 (tenant_id, name) 的历史软删除记录，避免唯一索引冲突。
	r.db.Unscoped().
		Where("tenant_id = ? AND name = ? AND deleted_at IS NOT NULL", provider.TenantID, provider.Name).
		Delete(&model.ModelProvider{}) //nolint:errcheck
	return r.db.Create(provider).Error
}

// Update 更新提供商
func (r *ModelProviderRepository) Update(provider *model.ModelProvider) error {
	return r.db.Save(provider).Error
}

// Delete 硬删除模型提供商（Unscoped，跳过软删除），确保再次创建同名提供商不会冲突。
func (r *ModelProviderRepository) Delete(id uint) error {
	return r.db.Unscoped().Delete(&model.ModelProvider{}, id).Error
}

// UpdateHealthStatus 更新健康状态
func (r *ModelProviderRepository) UpdateHealthStatus(id uint, status string) error {
	return r.db.Model(&model.ModelProvider{}).Where("id = ?", id).
		Update("health_check", status).Error
}

// ModelComparisonRepository 模型对比仓库
type ModelComparisonRepository struct {
	db *gorm.DB
}

func NewModelComparisonRepository(db *gorm.DB) *ModelComparisonRepository {
	return &ModelComparisonRepository{db: db}
}

// Create 创建对比实验
func (r *ModelComparisonRepository) Create(exp *model.ModelComparisonExperiment) error {
	return r.db.Create(exp).Error
}

// GetByID 获取实验
func (r *ModelComparisonRepository) GetByID(id uint) (*model.ModelComparisonExperiment, error) {
	var exp model.ModelComparisonExperiment
	if err := r.db.First(&exp, id).Error; err != nil {
		return nil, err
	}
	return &exp, nil
}

// Update 更新实验
func (r *ModelComparisonRepository) Update(exp *model.ModelComparisonExperiment) error {
	return r.db.Save(exp).Error
}

// List 获取实验列表
func (r *ModelComparisonRepository) List(limit int) ([]*model.ModelComparisonExperiment, error) {
	var experiments []*model.ModelComparisonExperiment
	if err := r.db.Order("created_at DESC").Limit(limit).Find(&experiments).Error; err != nil {
		return nil, err
	}
	return experiments, nil
}

// AddResult 添加实验结果
func (r *ModelComparisonRepository) AddResult(result *model.ExperimentResult) error {
	return r.db.Create(result).Error
}

// GetResults 获取实验结果
func (r *ModelComparisonRepository) GetResults(experimentID uint) ([]*model.ExperimentResult, error) {
	var results []*model.ExperimentResult
	if err := r.db.Preload("Model").Where("experiment_id = ?", experimentID).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}


// ============================================
// ItemRepository 物品仓库
// ============================================

type ItemRepository struct {
	db *gorm.DB
}

func NewItemRepository(db *gorm.DB) *ItemRepository {
	return &ItemRepository{db: db}
}

func (r *ItemRepository) Create(item *model.Item) error {
	return r.db.Create(item).Error
}

func (r *ItemRepository) GetByID(id uint) (*model.Item, error) {
	var item model.Item
	if err := r.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *ItemRepository) ListByNovel(novelID uint) ([]*model.Item, error) {
	var items []*model.Item
	if err := r.db.Where("novel_id = ?", novelID).Order("created_at ASC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *ItemRepository) Update(item *model.Item) error {
	return r.db.Save(item).Error
}

func (r *ItemRepository) Delete(id uint) error {
	return r.db.Delete(&model.Item{}, id).Error
}

// ============================================
// ChapterItemRepository 章节物品覆盖仓库
// ============================================

type ChapterItemRepository struct {
	db *gorm.DB
}

func NewChapterItemRepository(db *gorm.DB) *ChapterItemRepository {
	return &ChapterItemRepository{db: db}
}

func (r *ChapterItemRepository) Upsert(ci *model.ChapterItem) error {
	var existing model.ChapterItem
	err := r.db.Where("chapter_id = ? AND item_id = ?", ci.ChapterID, ci.ItemID).First(&existing).Error
	if err == nil {
		// update
		existing.Location = ci.Location
		existing.Owner = ci.Owner
		existing.Condition = ci.Condition
		existing.Notes = ci.Notes
		return r.db.Save(&existing).Error
	}
	return r.db.Create(ci).Error
}

func (r *ChapterItemRepository) GetByChapterAndItem(chapterID, itemID uint) (*model.ChapterItem, error) {
	var ci model.ChapterItem
	if err := r.db.Where("chapter_id = ? AND item_id = ?", chapterID, itemID).First(&ci).Error; err != nil {
		return nil, err
	}
	return &ci, nil
}

func (r *ChapterItemRepository) ListByChapter(chapterID uint) ([]*model.ChapterItem, error) {
	var items []*model.ChapterItem
	if err := r.db.Where("chapter_id = ?", chapterID).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *ChapterItemRepository) Delete(chapterID, itemID uint) error {
	return r.db.Where("chapter_id = ? AND item_id = ?", chapterID, itemID).Delete(&model.ChapterItem{}).Error
}

// ============================================
// ChapterCharacterRepository 章节角色覆盖仓库
// ============================================

type ChapterCharacterRepository struct {
	db *gorm.DB
}

func NewChapterCharacterRepository(db *gorm.DB) *ChapterCharacterRepository {
	return &ChapterCharacterRepository{db: db}
}

func (r *ChapterCharacterRepository) Upsert(cc *model.ChapterCharacter) error {
	var existing model.ChapterCharacter
	err := r.db.Where("chapter_id = ? AND character_id = ?", cc.ChapterID, cc.CharacterID).First(&existing).Error
	if err == nil {
		existing.Appearance = cc.Appearance
		existing.Personality = cc.Personality
		existing.Status = cc.Status
		existing.Location = cc.Location
		existing.Notes = cc.Notes
		return r.db.Save(&existing).Error
	}
	return r.db.Create(cc).Error
}

func (r *ChapterCharacterRepository) GetByChapterAndCharacter(chapterID, characterID uint) (*model.ChapterCharacter, error) {
	var cc model.ChapterCharacter
	if err := r.db.Where("chapter_id = ? AND character_id = ?", chapterID, characterID).First(&cc).Error; err != nil {
		return nil, err
	}
	return &cc, nil
}

func (r *ChapterCharacterRepository) ListByChapter(chapterID uint) ([]*model.ChapterCharacter, error) {
	var records []*model.ChapterCharacter
	if err := r.db.Where("chapter_id = ?", chapterID).Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

func (r *ChapterCharacterRepository) Delete(chapterID, characterID uint) error {
	return r.db.Where("chapter_id = ? AND character_id = ?", chapterID, characterID).Delete(&model.ChapterCharacter{}).Error
}

// ============================================
// SkillRepository 技能仓库
// ============================================

// ListSkillsOpts 技能查询选项
type ListSkillsOpts struct {
	CharacterID *uint
	Category    string
	Status      string
}

type SkillRepository struct {
	db *gorm.DB
}

func NewSkillRepository(db *gorm.DB) *SkillRepository {
	return &SkillRepository{db: db}
}

func (r *SkillRepository) Create(skill *model.Skill) error {
	return r.db.Create(skill).Error
}

func (r *SkillRepository) GetByID(id uint) (*model.Skill, error) {
	var skill model.Skill
	if err := r.db.First(&skill, id).Error; err != nil {
		return nil, err
	}
	return &skill, nil
}

func (r *SkillRepository) ListByNovel(novelID uint, opts ListSkillsOpts) ([]*model.Skill, error) {
	q := r.db.Where("novel_id = ?", novelID)
	if opts.CharacterID != nil {
		q = q.Where("character_id = ?", *opts.CharacterID)
	}
	if opts.Category != "" {
		q = q.Where("category = ?", opts.Category)
	}
	if opts.Status != "" {
		q = q.Where("status = ?", opts.Status)
	}
	var skills []*model.Skill
	err := q.Order("character_id, created_at ASC").Find(&skills).Error
	return skills, err
}

func (r *SkillRepository) ListByCharacter(characterID uint) ([]*model.Skill, error) {
	var skills []*model.Skill
	if err := r.db.Where("character_id = ?", characterID).Order("created_at ASC").Find(&skills).Error; err != nil {
		return nil, err
	}
	return skills, nil
}

func (r *SkillRepository) Update(skill *model.Skill) error {
	return r.db.Save(skill).Error
}

func (r *SkillRepository) Delete(id uint) error {
	return r.db.Delete(&model.Skill{}, id).Error
}

func (r *SkillRepository) BatchCreate(skills []*model.Skill) error {
	if len(skills) == 0 {
		return nil
	}
	return r.db.CreateInBatches(skills, 100).Error
}

// ─── 戏剧张力仓库 ──────────────────────────────────────────────────────────────

// HookChainRepository 钩子链仓库
type HookChainRepository struct{ db *gorm.DB }

func NewHookChainRepository(db *gorm.DB) *HookChainRepository {
	return &HookChainRepository{db: db}
}

func (r *HookChainRepository) Create(h *model.HookChain) error {
	return r.db.Create(h).Error
}

func (r *HookChainRepository) GetByID(id uint) (*model.HookChain, error) {
	var h model.HookChain
	if err := r.db.First(&h, id).Error; err != nil {
		return nil, err
	}
	return &h, nil
}

func (r *HookChainRepository) Update(h *model.HookChain) error {
	return r.db.Save(h).Error
}

func (r *HookChainRepository) Delete(id uint) error {
	return r.db.Delete(&model.HookChain{}, id).Error
}

func (r *HookChainRepository) ListByNovel(novelID uint) ([]*model.HookChain, error) {
	var items []*model.HookChain
	err := r.db.Where("novel_id = ?", novelID).Order("planted_at ASC").Find(&items).Error
	return items, err
}

// ListPending 返回未兑现的钩子
func (r *HookChainRepository) ListPending(novelID uint) ([]*model.HookChain, error) {
	var items []*model.HookChain
	err := r.db.Where("novel_id = ? AND is_fulfilled = false", novelID).Order("planted_at ASC").Find(&items).Error
	return items, err
}

// SatisfactionPointRepository 爽点仓库
type SatisfactionPointRepository struct{ db *gorm.DB }

func NewSatisfactionPointRepository(db *gorm.DB) *SatisfactionPointRepository {
	return &SatisfactionPointRepository{db: db}
}

func (r *SatisfactionPointRepository) Create(sp *model.SatisfactionPoint) error {
	return r.db.Create(sp).Error
}

func (r *SatisfactionPointRepository) GetByID(id uint) (*model.SatisfactionPoint, error) {
	var sp model.SatisfactionPoint
	if err := r.db.First(&sp, id).Error; err != nil {
		return nil, err
	}
	return &sp, nil
}

func (r *SatisfactionPointRepository) Update(sp *model.SatisfactionPoint) error {
	return r.db.Save(sp).Error
}

func (r *SatisfactionPointRepository) Delete(id uint) error {
	return r.db.Delete(&model.SatisfactionPoint{}, id).Error
}

func (r *SatisfactionPointRepository) ListByNovel(novelID uint) ([]*model.SatisfactionPoint, error) {
	var items []*model.SatisfactionPoint
	err := r.db.Where("novel_id = ?", novelID).Order("planned_chapter ASC").Find(&items).Error
	return items, err
}

// ListRecentFulfilled 返回最近N章内已发生的爽点（用于节奏健康检测）
func (r *SatisfactionPointRepository) ListRecentFulfilled(novelID uint, fromChapter int) ([]*model.SatisfactionPoint, error) {
	var items []*model.SatisfactionPoint
	err := r.db.Where("novel_id = ? AND is_planned = false AND planned_chapter >= ?", novelID, fromChapter).
		Find(&items).Error
	return items, err
}

// ConflictArcRepository 冲突弧仓库
type ConflictArcRepository struct{ db *gorm.DB }

func NewConflictArcRepository(db *gorm.DB) *ConflictArcRepository {
	return &ConflictArcRepository{db: db}
}

func (r *ConflictArcRepository) Create(arc *model.ConflictArc) error {
	return r.db.Create(arc).Error
}

func (r *ConflictArcRepository) GetByID(id uint) (*model.ConflictArc, error) {
	var arc model.ConflictArc
	if err := r.db.First(&arc, id).Error; err != nil {
		return nil, err
	}
	return &arc, nil
}

func (r *ConflictArcRepository) Update(arc *model.ConflictArc) error {
	return r.db.Save(arc).Error
}

func (r *ConflictArcRepository) Delete(id uint) error {
	return r.db.Delete(&model.ConflictArc{}, id).Error
}

func (r *ConflictArcRepository) ListByNovel(novelID uint) ([]*model.ConflictArc, error) {
	var items []*model.ConflictArc
	err := r.db.Where("novel_id = ?", novelID).Order("start_chapter ASC").Find(&items).Error
	return items, err
}

// ─── SceneAnchorRepository 场景锚点仓库 ──────────────────────────────────────

type SceneAnchorRepository struct{ db *gorm.DB }

func NewSceneAnchorRepository(db *gorm.DB) *SceneAnchorRepository {
	return &SceneAnchorRepository{db: db}
}

func (r *SceneAnchorRepository) Create(a *model.SceneAnchor) error {
	return r.db.Create(a).Error
}

func (r *SceneAnchorRepository) GetByID(id uint) (*model.SceneAnchor, error) {
	var a model.SceneAnchor
	if err := r.db.First(&a, id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *SceneAnchorRepository) Update(a *model.SceneAnchor) error {
	return r.db.Save(a).Error
}

func (r *SceneAnchorRepository) Delete(id uint) error {
	return r.db.Delete(&model.SceneAnchor{}, id).Error
}

func (r *SceneAnchorRepository) ListByNovel(novelID uint) ([]*model.SceneAnchor, error) {
	var items []*model.SceneAnchor
	err := r.db.Where("novel_id = ?", novelID).Order("created_at ASC").Find(&items).Error
	return items, err
}

// ─── SceneConsistencyLogRepository 场景一致性日志仓库 ────────────────────────

type SceneConsistencyLogRepository struct{ db *gorm.DB }

func NewSceneConsistencyLogRepository(db *gorm.DB) *SceneConsistencyLogRepository {
	return &SceneConsistencyLogRepository{db: db}
}

func (r *SceneConsistencyLogRepository) Create(log *model.SceneConsistencyLog) error {
	return r.db.Create(log).Error
}

func (r *SceneConsistencyLogRepository) ListByShotID(shotID uint) ([]*model.SceneConsistencyLog, error) {
	var items []*model.SceneConsistencyLog
	err := r.db.Where("shot_id = ?", shotID).Order("created_at DESC").Find(&items).Error
	return items, err
}

func (r *SceneConsistencyLogRepository) ListByAnchorID(anchorID uint) ([]*model.SceneConsistencyLog, error) {
	var items []*model.SceneConsistencyLog
	err := r.db.Where("anchor_id = ?", anchorID).Order("created_at DESC").Find(&items).Error
	return items, err
}

// ─── SystemSettingRepository ────────────────────────────────────────────────

type SystemSettingRepository struct{ db *gorm.DB }

func NewSystemSettingRepository(db *gorm.DB) *SystemSettingRepository {
	return &SystemSettingRepository{db: db}
}

func (r *SystemSettingRepository) Get(key string) (string, error) {
	var s model.SystemSetting
	if err := r.db.First(&s, "key = ?", key).Error; err != nil {
		return "", err
	}
	return s.Value, nil
}

func (r *SystemSettingRepository) Set(key, value, description string) error {
	return r.db.Save(&model.SystemSetting{Key: key, Value: value, Description: description}).Error
}

func (r *SystemSettingRepository) List() ([]model.SystemSetting, error) {
	var items []model.SystemSetting
	err := r.db.Order("key ASC").Find(&items).Error
	return items, err
}


// ShotSFXItemRepository 分镜音效条目仓库
type ShotSFXItemRepository struct {
	db *gorm.DB
}

func NewShotSFXItemRepository(db *gorm.DB) *ShotSFXItemRepository {
	return &ShotSFXItemRepository{db: db}
}

// ListByShotID 获取分镜的所有音效条目，按 seq_no 升序
func (r *ShotSFXItemRepository) ListByShotID(shotID uint) ([]*model.ShotSFXItem, error) {
	var items []*model.ShotSFXItem
	err := r.db.Where("shot_id = ?", shotID).Order("seq_no").Find(&items).Error
	return items, err
}

// CountByShotID 统计分镜已有音效数量（幂等检测）
func (r *ShotSFXItemRepository) CountByShotID(shotID uint) (int64, error) {
	var count int64
	err := r.db.Model(&model.ShotSFXItem{}).Where("shot_id = ?", shotID).Count(&count).Error
	return count, err
}

// BatchCreate 批量创建音效条目
func (r *ShotSFXItemRepository) BatchCreate(items []*model.ShotSFXItem) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.Create(&items).Error
}

// Update 更新音效条目（通常只更新 volume）
func (r *ShotSFXItemRepository) Update(item *model.ShotSFXItem) error {
	return r.db.Save(item).Error
}

func (r *ShotSFXItemRepository) UpdateDisabled(id uint, disabled bool) error {
	return r.db.Model(&model.ShotSFXItem{}).Where("id = ?", id).Update("disabled", disabled).Error
}

// Delete 物理删除单条音效条目
func (r *ShotSFXItemRepository) Delete(id uint) error {
	return r.db.Unscoped().Delete(&model.ShotSFXItem{}, id).Error
}

// DeleteByShotID 物理删除分镜的所有音效条目（重新生成时使用）
func (r *ShotSFXItemRepository) DeleteByShotID(shotID uint) error {
	return r.db.Unscoped().Where("shot_id = ?", shotID).Delete(&model.ShotSFXItem{}).Error
}

// ─── VideoBGMSegmentRepository ────────────────────────────────────────────────

// VideoBGMSegmentRepository 视频BGM分段仓库
type VideoBGMSegmentRepository struct {
	db *gorm.DB
}

func NewVideoBGMSegmentRepository(db *gorm.DB) *VideoBGMSegmentRepository {
	return &VideoBGMSegmentRepository{db: db}
}

// ListByVideoID 获取视频的所有BGM分段，按 seq_no 升序
func (r *VideoBGMSegmentRepository) ListByVideoID(videoID uint) ([]*model.VideoBGMSegment, error) {
	var segs []*model.VideoBGMSegment
	err := r.db.Where("video_id = ?", videoID).Order("seq_no").Find(&segs).Error
	return segs, err
}

// DeleteByVideoID 删除视频的所有BGM分段（重新分析时清空）
func (r *VideoBGMSegmentRepository) DeleteByVideoID(videoID uint) error {
	return r.db.Unscoped().Where("video_id = ?", videoID).Delete(&model.VideoBGMSegment{}).Error
}

// BatchCreate 批量创建BGM分段
func (r *VideoBGMSegmentRepository) BatchCreate(segs []*model.VideoBGMSegment) error {
	if len(segs) == 0 {
		return nil
	}
	return r.db.Create(&segs).Error
}

// Update 更新BGM分段（用于更新URL/Volume等）
func (r *VideoBGMSegmentRepository) GetByID(id uint) (*model.VideoBGMSegment, error) {
	var seg model.VideoBGMSegment
	if err := r.db.First(&seg, id).Error; err != nil {
		return nil, err
	}
	return &seg, nil
}

func (r *VideoBGMSegmentRepository) Update(seg *model.VideoBGMSegment) error {
	return r.db.Save(seg).Error
}

func (r *VideoBGMSegmentRepository) UpdateDisabled(id uint, disabled bool) error {
	return r.db.Model(&model.VideoBGMSegment{}).Where("id = ?", id).Update("disabled", disabled).Error
}

// UpdateTrack 更新BGM分段的曲目信息（手动选曲后调用）
func (r *VideoBGMSegmentRepository) UpdateTrack(id uint, url, name, artist, source string) error {
	return r.db.Model(&model.VideoBGMSegment{}).Where("id = ?", id).Updates(map[string]interface{}{
		"url":          url,
		"track_name":   name,
		"track_artist": artist,
		"source":       source,
	}).Error
}

// ReplaceForVideo 在单个事务内原子替换视频的所有 BGM 分段：先建新再删旧，避免数据丢失。
func (r *VideoBGMSegmentRepository) ReplaceForVideo(videoID uint, segs []*model.VideoBGMSegment) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if len(segs) > 0 {
			if err := tx.Create(&segs).Error; err != nil {
				return err
			}
		}
		return tx.Unscoped().Where("video_id = ? AND id NOT IN (?)",
			videoID, collectIDs(segs)).Delete(&model.VideoBGMSegment{}).Error
	})
}

// RewriteProjectRepository handles rewrite project data
type RewriteProjectRepository struct {
	db    *gorm.DB
	redis *redis.Client
}

func NewRewriteProjectRepository(db *gorm.DB, redis *redis.Client) *RewriteProjectRepository {
	return &RewriteProjectRepository{db: db, redis: redis}
}

func (r *RewriteProjectRepository) Create(p *model.RewriteProject) error {
	return r.db.Create(p).Error
}

func (r *RewriteProjectRepository) GetByID(id uint) (*model.RewriteProject, error) {
	var p model.RewriteProject
	if err := r.db.First(&p, id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *RewriteProjectRepository) ListByTenant(tenantID uint, page, pageSize int) ([]*model.RewriteProject, int64, error) {
	var projects []*model.RewriteProject
	var total int64
	offset := (page - 1) * pageSize
	r.db.Model(&model.RewriteProject{}).Where("tenant_id = ?", tenantID).Count(&total)
	err := r.db.Where("tenant_id = ?", tenantID).Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&projects).Error
	return projects, total, err
}

func (r *RewriteProjectRepository) UpdateStatus(id uint, status, errMsg string) error {
	return r.db.Model(&model.RewriteProject{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": status, "error_msg": errMsg,
	}).Error
}

func (r *RewriteProjectRepository) UpdateProgress(id uint, done, total int) error {
	progress := 0
	if total > 0 {
		progress = done * 100 / total
	}
	return r.db.Model(&model.RewriteProject{}).Where("id = ?", id).Updates(map[string]interface{}{
		"done_chapters": done, "progress": progress,
	}).Error
}

func (r *RewriteProjectRepository) UpdateTotalChapters(id uint, total int) error {
	return r.db.Model(&model.RewriteProject{}).Where("id = ?", id).Update("total_chapters", total).Error
}

func (r *RewriteProjectRepository) Delete(id uint) error {
	return r.db.Delete(&model.RewriteProject{}, id).Error
}

// LiteraryAnalysisRepository handles literary analysis data
type LiteraryAnalysisRepository struct {
	db *gorm.DB
}

func NewLiteraryAnalysisRepository(db *gorm.DB) *LiteraryAnalysisRepository {
	return &LiteraryAnalysisRepository{db: db}
}

func (r *LiteraryAnalysisRepository) Create(a *model.LiteraryAnalysis) error {
	return r.db.Create(a).Error
}

func (r *LiteraryAnalysisRepository) GetByProjectID(projectID uint) (*model.LiteraryAnalysis, error) {
	var a model.LiteraryAnalysis
	if err := r.db.Where("project_id = ?", projectID).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// RewriteBibleRepository handles rewrite bible data
type RewriteBibleRepository struct {
	db *gorm.DB
}

func NewRewriteBibleRepository(db *gorm.DB) *RewriteBibleRepository {
	return &RewriteBibleRepository{db: db}
}

func (r *RewriteBibleRepository) Create(b *model.RewriteBible) error {
	return r.db.Create(b).Error
}

func (r *RewriteBibleRepository) GetByProjectID(projectID uint) (*model.RewriteBible, error) {
	var b model.RewriteBible
	if err := r.db.Where("project_id = ?", projectID).First(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *RewriteBibleRepository) Update(b *model.RewriteBible) error {
	return r.db.Save(b).Error
}

// ChapterRewriteTaskRepository handles chapter rewrite task data
type ChapterRewriteTaskRepository struct {
	db *gorm.DB
}

func NewChapterRewriteTaskRepository(db *gorm.DB) *ChapterRewriteTaskRepository {
	return &ChapterRewriteTaskRepository{db: db}
}

func (r *ChapterRewriteTaskRepository) Create(t *model.ChapterRewriteTask) error {
	return r.db.Create(t).Error
}

func (r *ChapterRewriteTaskRepository) GetByID(id uint) (*model.ChapterRewriteTask, error) {
	var t model.ChapterRewriteTask
	if err := r.db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *ChapterRewriteTaskRepository) ListByProject(projectID uint) ([]*model.ChapterRewriteTask, error) {
	var tasks []*model.ChapterRewriteTask
	err := r.db.Where("project_id = ?", projectID).Order("chapter_no ASC").Find(&tasks).Error
	return tasks, err
}

func (r *ChapterRewriteTaskRepository) UpdateStatus(id uint, status, errMsg string) error {
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": status, "error_msg": errMsg,
	}).Error
}

func (r *ChapterRewriteTaskRepository) UpdateRewritten(id uint, content string, simScore float64, passed bool) error {
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"rewritten_content": content,
		"lexical_sim":       simScore,
		"similarity_score":  simScore,
		"passed":            passed,
		"status":            "completed",
	}).Error
}

// collectIDs 提取记录 ID 列表；若空则返回 []uint{0}（避免 NOT IN 空集合语法错误）
func collectIDs(segs []*model.VideoBGMSegment) []uint {
	if len(segs) == 0 {
		return []uint{0}
	}
	ids := make([]uint, len(segs))
	for i, s := range segs {
		ids[i] = s.ID
	}
	return ids
}

// ─── 小说广场 — NovelRepository 扩展 ──────────────────────────────────────────

// GetPublicByID 获取单条公开小说（无需 tenantID）
func (r *NovelRepository) GetPublicByID(id uint) (*model.Novel, error) {
	var n model.Novel
	err := r.db.Where("id = ? AND is_published = ? AND visibility = ?", id, true, "public").
		First(&n).Error
	return &n, err
}

// ListPublicSorted 列出公开小说（sort: latest|hot；q: 标题/描述关键词）
func (r *NovelRepository) ListPublicSorted(sort, q string, page, pageSize int) ([]*model.Novel, int64, error) {
	var novels []*model.Novel
	var total int64
	base := r.db.Model(&model.Novel{}).Where("is_published = ? AND visibility = ?", true, "public")
	if q != "" {
		base = base.Where("title LIKE ? OR description LIKE ?", "%"+q+"%", "%"+q+"%")
	}
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := "hot_score DESC, published_at DESC"
	if sort == "latest" {
		order = "published_at DESC, created_at DESC"
	}
	offset := (page - 1) * pageSize
	err := base.Order(order).Offset(offset).Limit(pageSize).Find(&novels).Error
	return novels, total, err
}

// IncrNovelViewCount 浏览量+1
func (r *NovelRepository) IncrNovelViewCount(id uint) error {
	return r.db.Model(&model.Novel{}).Where("id = ?", id).
		UpdateColumn("view_count", gorm.Expr("view_count + 1")).Error
}

// IncrNovelLikeCount 点赞数 delta（+1 或 -1）
func (r *NovelRepository) IncrNovelLikeCount(id uint, delta int) error {
	return r.db.Model(&model.Novel{}).Where("id = ?", id).
		UpdateColumn("like_count", gorm.Expr("like_count + ?", delta)).Error
}

// IncrNovelCommentCount 评论数 delta
func (r *NovelRepository) IncrNovelCommentCount(id uint, delta int) error {
	return r.db.Model(&model.Novel{}).Where("id = ?", id).
		UpdateColumn("comment_count", gorm.Expr("comment_count + ?", delta)).Error
}

// UpdateNovelHotScore 更新热度分
func (r *NovelRepository) UpdateNovelHotScore(id uint, score float64) error {
	return r.db.Model(&model.Novel{}).Where("id = ?", id).Update("hot_score", score).Error
}

// ListPublicNovelsForHotCalc 批量拉取公开小说用于热度分计算
func (r *NovelRepository) ListPublicNovelsForHotCalc() ([]*model.Novel, error) {
	var novels []*model.Novel
	err := r.db.Model(&model.Novel{}).
		Where("is_published = ? AND visibility = ?", true, "public").
		Select("id, view_count, like_count, comment_count, published_at").
		Find(&novels).Error
	return novels, err
}

// ─── NovelLikeRepository ────────────────────────────────────────────────────

type NovelLikeRepository struct{ db *gorm.DB }

func NewNovelLikeRepository(db *gorm.DB) *NovelLikeRepository {
	return &NovelLikeRepository{db: db}
}

// Toggle 点赞/取消，返回最终状态（true=已点赞）
func (r *NovelLikeRepository) Toggle(novelID, userID uint) (liked bool, err error) {
	var like model.NovelLike
	result := r.db.Where("novel_id = ? AND user_id = ?", novelID, userID).First(&like)
	if result.Error != nil {
		if err2 := r.db.Create(&model.NovelLike{NovelID: novelID, UserID: userID}).Error; err2 != nil {
			return false, err2
		}
		return true, nil
	}
	return false, r.db.Delete(&like).Error
}

// Exists 是否已点赞
func (r *NovelLikeRepository) Exists(novelID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.NovelLike{}).
		Where("novel_id = ? AND user_id = ?", novelID, userID).Count(&count).Error
	return count > 0, err
}

// ─── NovelCommentRepository ─────────────────────────────────────────────────

type NovelCommentRepository struct{ db *gorm.DB }

func NewNovelCommentRepository(db *gorm.DB) *NovelCommentRepository {
	return &NovelCommentRepository{db: db}
}

func (r *NovelCommentRepository) Create(c *model.NovelComment) error {
	return r.db.Create(c).Error
}

func (r *NovelCommentRepository) ListByNovel(novelID uint, page, size int) ([]*model.NovelComment, int64, error) {
	var list []*model.NovelComment
	var total int64
	base := r.db.Model(&model.NovelComment{}).Where("novel_id = ?", novelID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	err := base.Order("created_at DESC").Offset(offset).Limit(size).Find(&list).Error
	return list, total, err
}

func (r *NovelCommentRepository) GetByID(id uint) (*model.NovelComment, error) {
	var c model.NovelComment
	err := r.db.First(&c, id).Error
	return &c, err
}

func (r *NovelCommentRepository) Delete(id uint) error {
	return r.db.Delete(&model.NovelComment{}, id).Error
}
