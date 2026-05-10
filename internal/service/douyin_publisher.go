package service

import (
	"context"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// DouyinPublisher 抖音开放平台发布器（stub）
type DouyinPublisher struct {
	AppKey    string
	AppSecret string
}

func (p *DouyinPublisher) Platform() string { return "douyin" }

func (p *DouyinPublisher) GetAuthURL(redirectURI, state string) string {
	if p.AppKey == "" {
		return ""
	}
	return fmt.Sprintf(
		"https://open.douyin.com/platform/oauth/connect/?client_key=%s&redirect_uri=%s&state=%s&response_type=code&scope=video.upload",
		p.AppKey, redirectURI, state,
	)
}

func (p *DouyinPublisher) ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error) {
	return nil, fmt.Errorf("douyin: 请配置 AppKey 后使用（set DOUYIN_APP_KEY env var）")
}

func (p *DouyinPublisher) RefreshToken(ctx context.Context, account *model.PlatformAccount) error {
	return fmt.Errorf("douyin: 请配置 AppKey 后使用")
}

func (p *DouyinPublisher) PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error) {
	return "", "", fmt.Errorf("douyin: 请配置 AppKey 后使用（set DOUYIN_APP_KEY env var）")
}

func (p *DouyinPublisher) CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (string, error) {
	return "unknown", nil
}
