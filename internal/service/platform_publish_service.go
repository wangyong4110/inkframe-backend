package service

import (
	"context"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// PublishOptions 发布选项
type PublishOptions struct {
	Title       string
	Description string
	Tags        []string
	IsPublic    bool
}

// PlatformPublisher 外部平台发布器接口
type PlatformPublisher interface {
	Platform() string
	GetAuthURL(redirectURI, state string) string
	ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error)
	RefreshToken(ctx context.Context, account *model.PlatformAccount) error
	PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error)
	CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (status string, err error)
}

// PlatformPublishService 外部平台发布服务
type PlatformPublishService struct {
	accountRepo *repository.PlatformAccountRepository
	recordRepo  *repository.VideoPublishRecordRepository
	publishers  map[string]PlatformPublisher
	taskSvc     *TaskService
}

// NewPlatformPublishService 创建平台发布服务
func NewPlatformPublishService(
	accountRepo *repository.PlatformAccountRepository,
	recordRepo *repository.VideoPublishRecordRepository,
	taskSvc *TaskService,
) *PlatformPublishService {
	svc := &PlatformPublishService{
		accountRepo: accountRepo,
		recordRepo:  recordRepo,
		taskSvc:     taskSvc,
		publishers:  make(map[string]PlatformPublisher),
	}
	// 注册内置平台发布器
	publishers := []PlatformPublisher{
		&BilibiliPublisher{},
		&DouyinPublisher{},
		&YoutubePublisher{},
	}
	for _, p := range publishers {
		svc.publishers[p.Platform()] = p
	}
	return svc
}

// ConnectAccount OAuth 换取 token，保存账号
func (s *PlatformPublishService) ConnectAccount(ctx context.Context, platform, code, redirectURI string, tenantID uint) (*model.PlatformAccount, error) {
	p, ok := s.publishers[platform]
	if !ok {
		return nil, fmt.Errorf("unsupported platform: %s", platform)
	}
	account, err := p.ExchangeToken(ctx, code, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("exchange token: %w", err)
	}
	account.TenantID = tenantID
	account.Platform = platform
	if err := s.accountRepo.Create(account); err != nil {
		return nil, fmt.Errorf("save account: %w", err)
	}
	return account, nil
}

// GetAuthURL 获取OAuth授权URL
func (s *PlatformPublishService) GetAuthURL(platform, redirectURI, state string) (string, error) {
	p, ok := s.publishers[platform]
	if !ok {
		return "", fmt.Errorf("unsupported platform: %s", platform)
	}
	return p.GetAuthURL(redirectURI, state), nil
}

// ListAccounts 列出租户的平台账号
func (s *PlatformPublishService) ListAccounts(tenantID uint) ([]*model.PlatformAccount, error) {
	return s.accountRepo.ListByTenant(tenantID)
}

// DisconnectAccount 删除平台账号
func (s *PlatformPublishService) DisconnectAccount(id uint) error {
	return s.accountRepo.Delete(id)
}

// PublishToExternal 向外部平台发布视频（异步）
func (s *PlatformPublishService) PublishToExternal(ctx context.Context, video *model.Video, accountIDs []uint, opts PublishOptions, tenantID uint) (string, error) {
	if video.FinalVideoURL == "" {
		return "", fmt.Errorf("video not synthesized yet: final_video_url is empty")
	}

	var taskID string
	if s.taskSvc != nil {
		task, err := s.taskSvc.Create(tenantID, "platform_publish", "外部平台发布", "video", video.ID)
		if err != nil {
			return "", fmt.Errorf("create task: %w", err)
		}
		taskID = task.TaskID
	}

	go func() {
		bgCtx := context.Background()
		if s.taskSvc != nil {
			_ = s.taskSvc.SetRunning(taskID)
		}

		for i, accountID := range accountIDs {
			account, err := s.accountRepo.GetByID(accountID)
			if err != nil {
				logger.Errorf("[PlatformPublish] video=%d account=%d: load account: %v", video.ID, accountID, err)
				continue
			}
			p, ok := s.publishers[account.Platform]
			if !ok {
				logger.Printf("[PlatformPublish] video=%d account=%d: unsupported platform %q", video.ID, accountID, account.Platform)
				continue
			}

			// Fix 1: Auto-refresh token if expired or expiring within 5 minutes
			if account.ExpiresAt != nil && account.ExpiresAt.Before(time.Now().Add(5*time.Minute)) {
				if refreshErr := p.RefreshToken(bgCtx, account); refreshErr != nil {
					logger.Errorf("[PlatformPublish] token refresh failed for account %d: %v", account.ID, refreshErr)
					// Continue anyway — let publish attempt reveal the real error
				} else {
					// Persist refreshed token
					if updateErr := s.accountRepo.UpdateTokens(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt); updateErr != nil {
						logger.Errorf("[PlatformPublish] failed to persist refreshed token for account %d: %v", account.ID, updateErr)
					}
				}
			}

			// 创建发布记录
			rec := &model.VideoPublishRecord{
				VideoID:   video.ID,
				Platform:  account.Platform,
				AccountID: accountID,
				Status:    "uploading",
			}
			if err := s.recordRepo.Create(rec); err != nil {
				logger.Errorf("[PlatformPublish] video=%d account=%d: create record: %v", video.ID, accountID, err)
				continue
			}

			// Fix 3: Retry publish up to 3 attempts with exponential backoff
			var externalID, externalURL string
			var pubErr error
			for attempt := 0; attempt < 3; attempt++ {
				externalID, externalURL, pubErr = p.PublishVideo(bgCtx, account, video.FinalVideoURL, opts)
				if pubErr == nil {
					break
				}
				if attempt < 2 {
					logger.Errorf("[PlatformPublish] attempt %d failed for record %d: %v, retrying...", attempt+1, rec.ID, pubErr)
					time.Sleep(time.Duration(1<<uint(attempt)) * time.Second) // 1s, 2s
				}
			}
			if pubErr != nil {
				logger.Errorf("[PlatformPublish] video=%d platform=%s: publish failed: %v", video.ID, account.Platform, pubErr)
				if err := s.recordRepo.UpdateStatus(rec.ID, "failed", pubErr.Error(), "", ""); err != nil {
					logger.Errorf("[PlatformPublish] record=%d: update failed status: %v", rec.ID, err)
				}
			} else {
				logger.Printf("[PlatformPublish] video=%d platform=%s: published externalID=%s", video.ID, account.Platform, externalID)
				if err := s.recordRepo.UpdateStatus(rec.ID, "published", "", externalID, externalURL); err != nil {
					logger.Errorf("[PlatformPublish] record=%d: update published status: %v", rec.ID, err)
				}
			}

			// 更新进度
			if s.taskSvc != nil {
				progress := (i + 1) * 100 / len(accountIDs)
				if err := s.taskSvc.UpdateProgress(taskID, progress); err != nil {
					logger.Errorf("[PlatformPublish] task=%s: update progress: %v", taskID, err)
				}
			}
		}

		if s.taskSvc != nil {
			_ = s.taskSvc.Complete(taskID, map[string]string{"status": "done"})
		}
	}()

	return taskID, nil
}

// ListPublishRecords 返回视频的所有发布记录
func (s *PlatformPublishService) ListPublishRecords(videoID uint) ([]*model.VideoPublishRecord, error) {
	return s.recordRepo.ListByVideo(videoID)
}
