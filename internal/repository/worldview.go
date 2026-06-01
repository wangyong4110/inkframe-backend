package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

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

// GetByID 根据ID获取世界观（内部使用，不含租户校验）
func (r *WorldviewRepository) GetByID(id uint) (*model.Worldview, error) {
	var worldview model.Worldview
	if err := r.db.First(&worldview, id).Error; err != nil {
		return nil, err
	}
	return &worldview, nil
}

// GetByIDAndTenant 根据ID和租户获取世界观（对外接口使用）
func (r *WorldviewRepository) GetByIDAndTenant(id, tenantID uint) (*model.Worldview, error) {
	var worldview model.Worldview
	if err := r.db.Where("id = ? AND tenant_id = ?", id, tenantID).First(&worldview).Error; err != nil {
		return nil, err
	}
	return &worldview, nil
}

// List 获取世界观列表（仅返回属于指定租户的世界观）
func (r *WorldviewRepository) List(tenantID uint, page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	var worldviews []*model.Worldview
	var total int64

	query := r.db.Model(&model.Worldview{}).Where("tenant_id = ?", tenantID)
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

// ListWithEntities 获取世界观列表并同时加载所有实体（单次 Preload，避免 N+1 查询）。
func (r *WorldviewRepository) ListWithEntities(tenantID uint, page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	var worldviews []*model.Worldview
	var total int64

	query := r.db.Model(&model.Worldview{}).Where("tenant_id = ?", tenantID)
	if genre != "" {
		query = query.Where("genre = ?", genre)
	}

	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := query.Preload("Entities").
		Order("used_count DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&worldviews).Error; err != nil {
		return nil, 0, err
	}

	return worldviews, total, nil
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

// DeleteEntitiesByWorldview 删除某世界观下的所有实体
func (r *WorldviewRepository) DeleteEntitiesByWorldview(worldviewID uint) error {
	return r.db.Where("worldview_id = ?", worldviewID).Delete(&model.WorldviewEntity{}).Error
}

// ItemRepository 物品仓库
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

// ListByNovelPaged returns a paginated slice and total count.
func (r *ItemRepository) ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.Item, int64, error) {
	var items []*model.Item
	var total int64
	q := r.db.Model(&model.Item{}).Where("novel_id = ?", novelID)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	if err := q.Order("created_at ASC").Limit(pageSize).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (r *ItemRepository) Update(item *model.Item) error {
	return r.db.Save(item).Error
}

func (r *ItemRepository) Delete(id uint) error {
	return r.db.Delete(&model.Item{}, id).Error
}

// DeleteChapterItemsByItem 删除物品的所有章节覆盖记录
func (r *ItemRepository) DeleteChapterItemsByItem(itemID uint) error {
	return r.db.Where("item_id = ?", itemID).Delete(&model.ChapterItem{}).Error
}

// BatchDeleteByNovel 批量删除属于指定小说的物品（WHERE novel_id = ? AND id IN (?)）
func (r *ItemRepository) BatchDeleteByNovel(novelID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Where("novel_id = ? AND id IN ?", novelID, ids).Delete(&model.Item{}).Error
}

// GetByTitleAndNovel 按名称和小说 ID 查找物品（用于去重检查）
func (r *ItemRepository) GetByTitleAndNovel(title string, novelID uint) (*model.Item, error) {
	var item model.Item
	if err := r.db.Where("name = ? AND novel_id = ?", title, novelID).First(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// SkillRepository 技能仓库
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

func (r *SkillRepository) List(novelID uint) ([]*model.Skill, error) {
	var skills []*model.Skill
	if err := r.db.Where("novel_id = ?", novelID).Order("created_at ASC").Find(&skills).Error; err != nil {
		return nil, err
	}
	return skills, nil
}

// ListPaged returns a paginated slice and total count.
func (r *SkillRepository) ListPaged(novelID uint, page, pageSize int) ([]*model.Skill, int64, error) {
	var skills []*model.Skill
	var total int64
	q := r.db.Model(&model.Skill{}).Where("novel_id = ?", novelID)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	if err := q.Order("created_at ASC").Limit(pageSize).Offset(offset).Find(&skills).Error; err != nil {
		return nil, 0, err
	}
	return skills, total, nil
}

func (r *SkillRepository) Update(skill *model.Skill) error {
	return r.db.Save(skill).Error
}

func (r *SkillRepository) Delete(id uint) error {
	return r.db.Delete(&model.Skill{}, id).Error
}

// BatchDeleteByNovel 批量删除属于指定小说的技能（WHERE novel_id = ? AND id IN (?)）
func (r *SkillRepository) BatchDeleteByNovel(novelID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Where("novel_id = ? AND id IN ?", novelID, ids).Delete(&model.Skill{}).Error
}

