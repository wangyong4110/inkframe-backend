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

func (r *ItemRepository) Update(item *model.Item) error {
	return r.db.Save(item).Error
}

func (r *ItemRepository) Delete(id uint) error {
	return r.db.Delete(&model.Item{}, id).Error
}

// ListSkillsOpts 技能查询选项
type ListSkillsOpts struct {
	CharacterID *uint
	Category    string
	Status      string
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
