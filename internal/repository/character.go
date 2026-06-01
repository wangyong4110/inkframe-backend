package repository

import (
	"context"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

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
	if err := r.db.Where("novel_id = ? AND deleted_at IS NULL", novelID).Find(&characters).Error; err != nil {
		return nil, err
	}
	return characters, nil
}

// ListByNovelFiltered 获取小说的角色列表，可按 role 过滤（空字符串 = 不过滤）
func (r *CharacterRepository) ListByNovelFiltered(novelID uint, role string) ([]*model.Character, error) {
	return r.ListByNovelFilteredCtx(context.Background(), novelID, role)
}

// ListByNovelFilteredCtx 获取小说的角色列表，可按 role 过滤，支持 context 传递（用于超时/取消传播）
func (r *CharacterRepository) ListByNovelFilteredCtx(ctx context.Context, novelID uint, role string) ([]*model.Character, error) {
	var characters []*model.Character
	q := r.db.WithContext(ctx).Where("novel_id = ? AND deleted_at IS NULL", novelID)
	if role != "" {
		q = q.Where("role = ?", role)
	}
	if err := q.Find(&characters).Error; err != nil {
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

// BatchDeleteByNovel 批量删除属于指定小说的角色（WHERE novel_id = ? AND id IN (?)）
func (r *CharacterRepository) BatchDeleteByNovel(novelID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Where("novel_id = ? AND id IN ?", novelID, ids).Delete(&model.Character{}).Error
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

// DeleteByCharacter 删除指定角色的所有状态快照（级联清理用）
func (r *CharacterStateSnapshotRepository) DeleteByCharacter(characterID uint) error {
	return r.db.Where("character_id = ?", characterID).Delete(&model.CharacterStateSnapshot{}).Error
}

// DeleteByChapterID 删除指定章节关联的所有状态快照（章节删除时级联清理用）
func (r *CharacterStateSnapshotRepository) DeleteByChapterID(chapterID uint) error {
	return r.db.Where("chapter_id = ?", chapterID).Delete(&model.CharacterStateSnapshot{}).Error
}
