package repository

import (
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

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
	if err := r.db.Where("novel_id = ? AND deleted_at IS NULL", novelID).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

// ListByNovelPaged 分页获取小说的知识条目，返回当页数据和总数
func (r *KnowledgeBaseRepository) ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	q := r.db.Model(&model.KnowledgeBase{}).Where("novel_id = ? AND deleted_at IS NULL", novelID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	var results []*model.KnowledgeBase
	if err := q.Order("id DESC").Offset(offset).Limit(pageSize).Find(&results).Error; err != nil {
		return nil, 0, err
	}
	return results, total, nil
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

// Delete 删除单条知识条目（软删除）
func (r *KnowledgeBaseRepository) Delete(id uint) error {
	return r.db.Delete(&model.KnowledgeBase{}, id).Error
}

// IncrementUsageCount 增加使用次数
func (r *KnowledgeBaseRepository) IncrementUsageCount(id uint) error {
	return r.db.Model(&model.KnowledgeBase{}).Where("id = ?", id).
		UpdateColumn("usage_count", gorm.Expr("usage_count + 1")).Error
}

// ListBySourceChapter 列出某章节提取的所有知识条目（用于删除前获取 ID 列表）
func (r *KnowledgeBaseRepository) ListBySourceChapter(novelID, chapterID uint) ([]*model.KnowledgeBase, error) {
	var results []*model.KnowledgeBase
	if err := r.db.Where("novel_id = ? AND source_chapter_id = ?", novelID, chapterID).
		Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

// DeleteBySourceChapter 删除某章节提取的所有知识条目（用于重新提取时去重）
func (r *KnowledgeBaseRepository) DeleteBySourceChapter(novelID, chapterID uint) error {
	return r.db.Where("novel_id = ? AND source_chapter_id = ?", novelID, chapterID).
		Delete(&model.KnowledgeBase{}).Error
}
