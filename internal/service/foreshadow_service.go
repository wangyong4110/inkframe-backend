package service

import (
	"context"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ForeshadowCRUDService 专用伏笔表的 CRUD 服务（ink_foreshadow 表）
// 与旧的 ForeshadowService（基于 KnowledgeBase tag）并存，新 API 优先使用此服务。
type ForeshadowCRUDService struct {
	repo *repository.ForeshadowRepository
}

func NewForeshadowCRUDService(repo *repository.ForeshadowRepository) *ForeshadowCRUDService {
	return &ForeshadowCRUDService{repo: repo}
}

func (s *ForeshadowCRUDService) Create(ctx context.Context, f *model.Foreshadow) error {
	return s.repo.Create(f)
}

func (s *ForeshadowCRUDService) ListByNovel(ctx context.Context, novelID uint, tenantID uint) ([]*model.Foreshadow, error) {
	return s.repo.ListByNovel(novelID, tenantID)
}

func (s *ForeshadowCRUDService) ListUnfulfilled(ctx context.Context, novelID uint, tenantID uint) ([]*model.Foreshadow, error) {
	return s.repo.ListUnfulfilled(novelID, tenantID)
}

func (s *ForeshadowCRUDService) Update(ctx context.Context, id uint, updates map[string]interface{}) (*model.Foreshadow, error) {
	f, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if v, ok := updates["title"].(string); ok && v != "" {
		f.Title = v
	}
	if v, ok := updates["description"].(string); ok && v != "" {
		f.Description = v
	}
	if v, ok := updates["status"].(string); ok && v != "" {
		f.Status = v
	}
	if v, ok := updates["tags"].(string); ok {
		f.Tags = v
	}
	return f, s.repo.Update(f)
}

func (s *ForeshadowCRUDService) Delete(ctx context.Context, id uint) error {
	return s.repo.Delete(id)
}
