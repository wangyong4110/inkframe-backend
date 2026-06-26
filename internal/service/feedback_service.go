package service

import (
	"encoding/json"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

type FeedbackService struct {
	repo *repository.FeedbackRepository
}

func NewFeedbackService(repo *repository.FeedbackRepository) *FeedbackService {
	return &FeedbackService{repo: repo}
}

func (s *FeedbackService) Create(req *model.CreateFeedbackRequest, userID, tenantID uint) (*model.UserFeedback, error) {
	screenshotsJSON := ""
	if len(req.Screenshots) > 0 {
		b, err := json.Marshal(req.Screenshots)
		if err == nil {
			screenshotsJSON = string(b)
		}
	}
	f := &model.UserFeedback{
		TenantID:     tenantID,
		UserID:       userID,
		Type:         req.Type,
		Title:        req.Title,
		Content:      req.Content,
		Rating:       req.Rating,
		PageURL:      req.PageURL,
		UserAgent:    req.UserAgent,
		Screenshots:  screenshotsJSON,
		ContactEmail: req.ContactEmail,
		Status:       "pending",
		Priority:     "medium",
	}
	if err := s.repo.Create(f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *FeedbackService) ListForAdmin(page, size int, status, typ, priority string) ([]*model.UserFeedback, int64, error) {
	return s.repo.List(page, size, status, typ, priority)
}

func (s *FeedbackService) ListForUser(userID uint, page, size int) ([]*model.UserFeedback, int64, error) {
	return s.repo.ListByUser(userID, page, size)
}

func (s *FeedbackService) GetByID(id uint) (*model.UserFeedback, error) {
	return s.repo.GetByID(id)
}

func (s *FeedbackService) UpdateStatus(id uint, req *model.UpdateFeedbackRequest) (*model.UserFeedback, error) {
	f, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Status != "" {
		f.Status = req.Status
		if req.Status == "resolved" && f.ResolvedAt == nil {
			now := time.Now()
			f.ResolvedAt = &now
		}
	}
	if req.Priority != "" {
		f.Priority = req.Priority
	}
	if req.AdminNote != "" {
		f.AdminNote = req.AdminNote
	}
	if err := s.repo.Update(f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *FeedbackService) Reply(id uint, req *model.ReplyFeedbackRequest) (*model.UserFeedback, error) {
	f, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	f.ReplyContent = req.Content
	f.RepliedAt = &now
	if err := s.repo.Update(f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *FeedbackService) GetStats() (map[string]interface{}, error) {
	return s.repo.GetStats()
}
