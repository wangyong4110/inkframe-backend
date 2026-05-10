package service

import (
	"context"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// YoutubePublisher YouTube Data API v3 发布器（stub）
type YoutubePublisher struct {
	ClientID     string
	ClientSecret string
}

func (p *YoutubePublisher) Platform() string { return "youtube" }

func (p *YoutubePublisher) GetAuthURL(redirectURI, state string) string {
	if p.ClientID == "" {
		return ""
	}
	return fmt.Sprintf(
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&state=%s&response_type=code&scope=https://www.googleapis.com/auth/youtube.upload&access_type=offline",
		p.ClientID, redirectURI, state,
	)
}

func (p *YoutubePublisher) ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error) {
	return nil, fmt.Errorf("youtube: 请配置 ClientID 后使用（set YOUTUBE_CLIENT_ID env var）")
}

func (p *YoutubePublisher) RefreshToken(ctx context.Context, account *model.PlatformAccount) error {
	return fmt.Errorf("youtube: 请配置 ClientID 后使用")
}

func (p *YoutubePublisher) PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error) {
	return "", "", fmt.Errorf("youtube: 请配置 ClientID 后使用（set YOUTUBE_CLIENT_ID env var）")
}

func (p *YoutubePublisher) CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (string, error) {
	return "unknown", nil
}
