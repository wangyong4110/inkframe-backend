package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

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

// CreateInBatches 批量创建章节（单次事务，避免 N 次独立 INSERT 的性能开销）
func (r *ChapterRepository) CreateInBatches(chapters []*model.Chapter, batchSize int) error {
	if len(chapters) == 0 {
		return nil
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	if err := r.db.CreateInBatches(chapters, batchSize).Error; err != nil {
		return err
	}
	if len(chapters) > 0 {
		r.invalidateListCache(chapters[0].NovelID)
	}
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
	if err := r.db.Where("novel_id = ? AND chapter_no = ? AND deleted_at IS NULL", novelID, chapterNo).First(&chapter).Error; err != nil {
		return nil, err
	}
	return &chapter, nil
}

// chapterListColumns 章节列表元数据字段。排除 content/scene_outline/plot_points/chapter_hook 等超大文本列。
// outline/summary 较短（百字级），保留用于列表摘要展示。
const chapterListColumns = "id, novel_id, tenant_id, uuid, chapter_no, title, status, word_count, " +
	"tension_level, act_no, emotional_tone, hook_type, " +
	"outline, summary, continuity_blocked, " +
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
// 用于 AI 提取任务（角色/物品/技能等），不走缓存。最多返回 300 章，避免超大小说导致 OOM。
func (r *ChapterRepository) ListByNovelWithContent(novelID uint) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	return chapters, r.db.Where("novel_id = ? AND deleted_at IS NULL", novelID).
		Order("chapter_no ASC").Limit(300).Find(&chapters).Error
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

// UpdateStatus 更新章节状态（novelID 用于缓存失效）
func (r *ChapterRepository) UpdateStatus(id, novelID uint, status string) error {
	if err := r.db.Model(&model.Chapter{}).Where("id = ? AND novel_id = ?", id, novelID).
		Update("status", status).Error; err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// AtomicSetGenerating 使用条件更新（乐观锁）将章节状态从非 generating/completed 状态原子性地切换到
// "generating"。返回 true 表示本次调用成功抢占到生成权，false 表示该章节已在生成中或已完成。
func (r *ChapterRepository) AtomicSetGenerating(id, novelID uint) (bool, error) {
	result := r.db.Model(&model.Chapter{}).
		Where("id = ? AND novel_id = ? AND status NOT IN ?", id, novelID, []string{"generating", "completed"}).
		Update("status", "generating")
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	r.invalidateListCache(novelID)
	return true, nil
}

// ListPublishedByNovel 获取小说已发布章节（按章节号升序）
func (r *ChapterRepository) ListPublishedByNovel(novelID uint) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	err := r.db.Select("id, novel_id, tenant_id, uuid, chapter_no, title, summary, status, is_published, published_at, word_count, created_at, updated_at").
		Where("novel_id = ? AND is_published = ?", novelID, true).
		Order("chapter_no ASC").Find(&chapters).Error
	return chapters, err
}

// UpdateIsPublished 更新章节广场发布状态（不修改内容 status）
func (r *ChapterRepository) UpdateIsPublished(id, novelID uint, isPublished bool) error {
	updates := map[string]interface{}{"is_published": isPublished}
	if isPublished {
		now := time.Now()
		updates["published_at"] = &now
	}
	if err := r.db.Model(&model.Chapter{}).Where("id = ? AND novel_id = ?", id, novelID).Updates(updates).Error; err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// UpdateContinuityBlocked 原子性更新连贯性阻塞标记（仅改单列，避免与并发 Update 产生写入竞争）。
// 与 Update(chapter) 不同，本方法不触及其他字段，适合在独立 goroutine 中安全调用。
func (r *ChapterRepository) UpdateContinuityBlocked(id, novelID uint, blocked bool) error {
	if err := r.db.Model(&model.Chapter{}).Where("id = ? AND novel_id = ?", id, novelID).
		Update("continuity_blocked", blocked).Error; err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// UpdateSummary 只更新摘要字段，不触碰其他字段（防止并发写入覆盖 continuity_blocked 等字段）
func (r *ChapterRepository) UpdateSummary(id, novelID uint, summary string) error {
	if err := r.db.Model(&model.Chapter{}).Where("id = ? AND novel_id = ?", id, novelID).
		Update("summary", summary).Error; err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// BatchUpdateIsPublished 批量更新小说下所有章节的发布状态
func (r *ChapterRepository) BatchUpdateIsPublished(novelID uint, isPublished bool) (int64, error) {
	updates := map[string]interface{}{"is_published": isPublished}
	if isPublished {
		now := time.Now()
		updates["published_at"] = &now
	}
	result := r.db.Model(&model.Chapter{}).Where("novel_id = ?", novelID).Updates(updates)
	if result.Error != nil {
		return 0, result.Error
	}
	r.invalidateListCache(novelID)
	return result.RowsAffected, nil
}

// Delete 删除章节（novelID 用于缓存失效）
func (r *ChapterRepository) Delete(id, novelID uint) error {
	if err := r.db.Delete(&model.Chapter{}, id).Error; err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// DeleteAndRenumber 在事务中软删除章节，并将其后续章节的 chapter_no 减一，消除序号空洞。
func (r *ChapterRepository) DeleteAndRenumber(id, novelID uint) error {
	// First fetch the chapter_no before deleting
	var chapter model.Chapter
	if err := r.db.Select("id, novel_id, chapter_no").First(&chapter, id).Error; err != nil {
		// If not found, fall back to plain delete
		return r.Delete(id, novelID)
	}
	deletedNo := chapter.ChapterNo

	err := r.db.Transaction(func(tx *gorm.DB) error {
		// Soft-delete the chapter
		if err := tx.Delete(&model.Chapter{}, id).Error; err != nil {
			return err
		}
		// Decrement chapter_no for all subsequent chapters in this novel
		return tx.Exec(
			"UPDATE ink_chapter SET chapter_no = chapter_no - 1 WHERE novel_id = ? AND chapter_no > ? AND deleted_at IS NULL",
			novelID, deletedNo,
		).Error
	})
	if err != nil {
		return err
	}
	r.invalidateListCache(novelID)
	return nil
}

// CountByNovel 统计小说章节数
func (r *ChapterRepository) MaxChapterNo(novelID uint) (int, error) {
	var max int
	err := r.db.Model(&model.Chapter{}).Where("novel_id = ?", novelID).
		Select("COALESCE(MAX(chapter_no), 0)").Scan(&max).Error
	return max, err
}

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

// UpdateContent 将指定章节的正文内容直接写回数据库（供改写完成后同步使用）。
func (r *ChapterRepository) UpdateContent(id uint, content string) error {
	return r.db.Model(&model.Chapter{}).Where("id = ?", id).
		UpdateColumn("content", content).Error
}

// UpdateCrawledContent 将爬取完成的内容写回章节（状态置为 completed，发布状态独立管理）
func (r *ChapterRepository) UpdateCrawledContent(id uint, title, content string, wordCount int) error {
	return r.db.Model(&model.Chapter{}).Where("id = ?", id).Updates(map[string]interface{}{
		"title":      title,
		"content":    content,
		"outline":    "",
		"word_count": wordCount,
		"status":     "completed",
	}).Error
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

// DeleteOlderThan 删除早于 cutoff 时间的历史版本，返回删除条数
func (r *ChapterVersionRepository) DeleteOlderThan(cutoff time.Time) (int64, error) {
	result := r.db.Where("created_at < ?", cutoff).Delete(&model.ChapterVersion{})
	return result.RowsAffected, result.Error
}

// DeleteExcessVersions 保留指定章节的最新 keepN 个版本，删除其余旧版本
func (r *ChapterVersionRepository) DeleteExcessVersions(chapterID uint, keepN int) error {
	if keepN <= 0 {
		keepN = 20
	}
	// MySQL 不允许在 DELETE 的 WHERE 子查询中直接引用被删除的表，
	// 需要通过派生表（double-subquery）绕过该限制。
	return r.db.Exec(`
		DELETE FROM ink_chapter_version
		WHERE chapter_id = ? AND version_no < (
			SELECT min_no FROM (
				SELECT MIN(version_no) AS min_no
				FROM (
					SELECT version_no FROM ink_chapter_version
					WHERE chapter_id = ?
					ORDER BY version_no DESC
					LIMIT ?
				) AS top_n
			) AS sub
		)
	`, chapterID, chapterID, keepN).Error
}

// ChapterItemRepository 章节物品覆盖仓库
type ChapterItemRepository struct {
	db *gorm.DB
}

func NewChapterItemRepository(db *gorm.DB) *ChapterItemRepository {
	return &ChapterItemRepository{db: db}
}

func (r *ChapterItemRepository) Upsert(ci *model.ChapterItem) error {
	var existing model.ChapterItem
	result := r.db.Where("chapter_id = ? AND item_id = ?", ci.ChapterID, ci.ItemID).
		Attrs(model.ChapterItem{ChapterID: ci.ChapterID, ItemID: ci.ItemID}).
		FirstOrCreate(&existing)
	if result.Error != nil {
		return result.Error
	}
	existing.Location = ci.Location
	existing.Owner = ci.Owner
	existing.Condition = ci.Condition
	existing.Notes = ci.Notes
	return r.db.Save(&existing).Error
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

// ChapterCharacterRepository 章节角色覆盖仓库
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

// DeleteByCharacter 删除指定角色的所有章节覆盖记录（级联清理用）
func (r *ChapterCharacterRepository) DeleteByCharacter(characterID uint) error {
	return r.db.Where("character_id = ?", characterID).Delete(&model.ChapterCharacter{}).Error
}
