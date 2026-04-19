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

// NovelRepository 小说仓库
type NovelRepository struct {
	db    *gorm.DB
	cache *redis.Client
}

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
	// 尝试从缓存获取
	cacheKey := fmt.Sprintf("novel:%d", id)
	if r.cache != nil {
		cached, err := r.cache.Get(context.Background(), cacheKey).Result()
		if err == nil {
			var novel model.Novel
			if json.Unmarshal([]byte(cached), &novel) == nil {
				return &novel, nil
			}
		}
	}

	var novel model.Novel
	if err := r.db.Preload("Worldview").First(&novel, id).Error; err != nil {
		return nil, err
	}

	// 写入缓存
	if r.cache != nil {
		if data, err := json.Marshal(novel); err == nil {
			r.cache.Set(context.Background(), cacheKey, data, 30*time.Minute)
		}
	}

	return &novel, nil
}

// GetByUUID 根据UUID获取小说
func (r *NovelRepository) GetByUUID(uuid string) (*model.Novel, error) {
	var novel model.Novel
	if err := r.db.Preload("Worldview").Where("uuid = ?", uuid).First(&novel).Error; err != nil {
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
	if status, ok := filters["status"]; ok {
		query = query.Where("status = ?", status)
	}
	if genre, ok := filters["genre"]; ok {
		query = query.Where("genre = ?", genre)
	}

	// 统计总数
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 分页查询
	offset := (page - 1) * pageSize
	if err := query.Preload("Worldview").
		Order("updated_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&novels).Error; err != nil {
		return nil, 0, err
	}

	return novels, total, nil
}

// Update 更新小说
func (r *NovelRepository) Update(novel *model.Novel) error {
	if err := r.db.Save(novel).Error; err != nil {
		return err
	}
	r.invalidateCache(novel.ID)
	return nil
}

// Delete 删除小说
func (r *NovelRepository) Delete(id uint) error {
	if err := r.db.Delete(&model.Novel{}, id).Error; err != nil {
		return err
	}
	r.invalidateCache(id)
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

func NewChapterRepository(db *gorm.DB, cache *redis.Client) *ChapterRepository {
	return &ChapterRepository{db: db, cache: cache}
}

// Create 创建章节
func (r *ChapterRepository) Create(chapter *model.Chapter) error {
	return r.db.Create(chapter).Error
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

// ListByNovel 获取小说的所有章节
func (r *ChapterRepository) ListByNovel(novelID uint) ([]*model.Chapter, error) {
	var chapters []*model.Chapter
	if err := r.db.Where("novel_id = ?", novelID).Order("chapter_no ASC").Find(&chapters).Error; err != nil {
		return nil, err
	}
	return chapters, nil
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
	return r.db.Save(chapter).Error
}

// Delete 删除章节
func (r *ChapterRepository) Delete(id uint) error {
	return r.db.Delete(&model.Chapter{}, id).Error
}

// CountByNovel 统计小说章节数
func (r *ChapterRepository) CountByNovel(novelID uint) (int64, error) {
	var count int64
	if err := r.db.Model(&model.Chapter{}).Where("novel_id = ?", novelID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
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

// GetAvailableByTaskType 获取任务可用的模型
func (r *AIModelRepository) GetAvailableByTaskType(taskType string) ([]*model.AIModel, error) {
	var models []*model.AIModel
	if err := r.db.Preload("Provider").
		Where("is_active = ? AND is_available = ?", true, true).
		Find(&models).Error; err != nil {
		return nil, err
	}

	// 过滤适合该任务的模型
	var suitableModels []*model.AIModel
	for _, m := range models {
		var tasks []string
		if json.Unmarshal([]byte(m.SuitableTasks), &tasks) == nil {
			for _, t := range tasks {
				if t == taskType {
					suitableModels = append(suitableModels, m)
					break
				}
			}
		}
	}

	return suitableModels, nil
}

// GetByID 根据ID获取模型
func (r *AIModelRepository) GetByID(id uint) (*model.AIModel, error) {
	var model model.AIModel
	if err := r.db.Preload("Provider").First(&model, id).Error; err != nil {
		return nil, err
	}
	return &model, nil
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

// UpdateHealthStatus 更新健康状态
func (r *AIModelRepository) UpdateHealthStatus(providerID uint, status string) error {
	return r.db.Model(&model.ModelProvider{}).Where("id = ?", providerID).
		Updates(map[string]interface{}{
			"health_check": status,
			"last_checked": time.Now(),
		}).Error
}

// LogUsage 记录使用
func (r *AIModelRepository) LogUsage(log *model.ModelUsageLog) error {
	return r.db.Create(log).Error
}

// GetUsageStats 获取使用统计
func (r *AIModelRepository) GetUsageStats(modelID uint, startTime, endTime time.Time) (*UsageStats, error) {
	var stats UsageStats

	// 查询使用记录
	var logs []model.ModelUsageLog
	if err := r.db.Where("model_id = ? AND created_at BETWEEN ? AND ?", modelID, startTime, endTime).Find(&logs).Error; err != nil {
		return nil, err
	}

	// 统计
	stats.TotalRequests = len(logs)
	for _, log := range logs {
		stats.TotalTokens += log.TotalTokens
		stats.TotalCost += log.Cost
		stats.TotalLatency += log.Latency
		if log.Success {
			stats.SuccessCount++
		}
	}

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
	if err := r.db.Where("title LIKE ? OR content LIKE ?", "%"+keyword+"%", "%"+keyword+"%").
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

// List 获取视频列表
func (r *VideoRepository) List(novelID *uint, page, pageSize int) ([]*model.Video, int64, error) {
	var videos []*model.Video
	var total int64

	query := r.db.Model(&model.Video{})
	if novelID != nil {
		query = query.Where("novel_id = ?", *novelID)
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

// Update 更新分镜
func (r *StoryboardRepository) Update(shot *model.StoryboardShot) error {
	return r.db.Save(shot).Error
}

// ReviewTaskRepository 审核任务仓库
type ReviewTaskRepository struct {
	db *gorm.DB
}

