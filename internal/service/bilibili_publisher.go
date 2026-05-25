package service

import (
	"context"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// BilibiliPublisher 哔哩哔哩开放平台发布器
// NOTE: 完整实现需配置 AppKey/Secret（通过环境变量注入），当前为框架实现。
type BilibiliPublisher struct {
	AppKey    string
	AppSecret string
}

func (p *BilibiliPublisher) Platform() string { return "bilibili" }

func (p *BilibiliPublisher) GetAuthURL(redirectURI, state string) string {
	if p.AppKey == "" {
		return ""
	}
	return fmt.Sprintf(
		"https://passport.bilibili.com/oauth2/authorize?response_type=code&client_id=%s&redirect_uri=%s&state=%s",
		p.AppKey, redirectURI, state,
	)
}

func (p *BilibiliPublisher) ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error) {
	if p.AppKey == "" {
		return nil, fmt.Errorf("bilibili: AppKey not configured — set BILIBILI_APP_KEY env var")
	}
	// TODO: 调用 https://passport.bilibili.com/oauth2/token 换取 access_token
	return nil, fmt.Errorf("bilibili: ExchangeToken not implemented, configure BILIBILI_APP_KEY/SECRET")
}

func (p *BilibiliPublisher) RefreshToken(ctx context.Context, account *model.PlatformAccount) error {
	if p.AppKey == "" {
		return fmt.Errorf("bilibili: AppKey not configured")
	}
	// TODO: 调用 token 刷新接口
	return fmt.Errorf("bilibili: RefreshToken not implemented")
}

// PublishVideo 上传视频到哔哩哔哩（preupload → 分片上传 → 投稿 add 接口）
func (p *BilibiliPublisher) PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error) {
	if p.AppKey == "" {
		return "", "", fmt.Errorf("bilibili: AppKey not configured — set BILIBILI_APP_KEY env var")
	}
	// TODO: 实现完整上传流程
	// 1. GET /x/upload/web/preupload → 获取分片上传 endpoint
	// 2. POST 分片至 upos endpoint
	// 3. POST /x/vu/client/add 投稿
	return "", "", fmt.Errorf("bilibili: PublishVideo not implemented")
}

func (p *BilibiliPublisher) CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (string, error) {
	// TODO: 调用 /x/space/arc/mine 查询投稿状态
	return "", fmt.Errorf("bilibili: CheckPublishStatus not yet implemented")
}
