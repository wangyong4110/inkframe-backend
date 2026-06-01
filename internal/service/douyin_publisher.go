package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// DouyinPublisher 抖音开放平台发布器
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
		"https://open.douyin.com/platform/oauth/connect/?client_key=%s&redirect_uri=%s&state=%s&response_type=code&scope=video.upload,video.create",
		url.QueryEscape(p.AppKey), url.QueryEscape(redirectURI), url.QueryEscape(state),
	)
}

func (p *DouyinPublisher) ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error) {
	if p.AppKey == "" {
		return nil, fmt.Errorf("douyin: AppKey not configured — set DOUYIN_APP_KEY env var")
	}

	payload := map[string]string{
		"client_key":    p.AppKey,
		"client_secret": p.AppSecret,
		"code":          code,
		"grant_type":    "authorization_code",
	}
	bodyBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.douyin.com/oauth/access_token/",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("douyin token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			OpenID       string `json:"open_id"`
			ExpiresIn    int64  `json:"expires_in"`
			Scope        string `json:"scope"`
			ErrorCode    int    `json:"error_code"`
			Description  string `json:"description"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("douyin token decode: %w", err)
	}
	if result.Data.ErrorCode != 0 {
		return nil, fmt.Errorf("douyin token error %d: %s", result.Data.ErrorCode, result.Data.Description)
	}

	expiresAt := time.Now().Add(time.Duration(result.Data.ExpiresIn) * time.Second)
	return &model.PlatformAccount{
		Platform:     "douyin",
		UID:          result.Data.OpenID,
		AccessToken:  result.Data.AccessToken,
		RefreshToken: result.Data.RefreshToken,
		ExpiresAt:    &expiresAt,
		Status:       "active",
	}, nil
}

func (p *DouyinPublisher) RefreshToken(ctx context.Context, account *model.PlatformAccount) error {
	if p.AppKey == "" {
		return fmt.Errorf("douyin: AppKey not configured")
	}

	payload := map[string]string{
		"client_key":    p.AppKey,
		"client_secret": p.AppSecret,
		"refresh_token": account.RefreshToken,
		"grant_type":    "refresh_token",
	}
	bodyBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.douyin.com/oauth/refresh_token/",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("douyin refresh: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
			ErrorCode    int    `json:"error_code"`
			Description  string `json:"description"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("douyin refresh decode: %w", err)
	}
	if result.Data.ErrorCode != 0 {
		return fmt.Errorf("douyin refresh error %d: %s", result.Data.ErrorCode, result.Data.Description)
	}

	expiresAt := time.Now().Add(time.Duration(result.Data.ExpiresIn) * time.Second)
	account.AccessToken = result.Data.AccessToken
	account.RefreshToken = result.Data.RefreshToken
	account.ExpiresAt = &expiresAt
	account.Status = "active"
	return nil
}

// PublishVideo 上传视频到抖音（URL方式投稿）
func (p *DouyinPublisher) PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error) {
	if account.AccessToken == "" {
		return "", "", fmt.Errorf("douyin: account not authorized")
	}

	// Step 1: 初始化上传任务
	initPayload := map[string]interface{}{
		"source_info": map[string]interface{}{
			"source":       "PULL_FROM_URL",
			"video_url":    videoURL,
			"cover_url":    "",
			"video_size":   0,
			"video_format": "mp4",
		},
		"post_info": map[string]interface{}{
			"title":       opts.Title,
			"description": opts.Description,
			"privacy_level": func() string {
				if opts.IsPublic {
					return "PUBLIC_TO_EVERYONE"
				}
				return "SELF_ONLY"
			}(),
		},
	}
	bodyBytes, _ := json.Marshal(initPayload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.douyin.com/api/douyin/v1/video/create_video/",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("access-token", account.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("douyin publish: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			ItemID      string `json:"item_id"`
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
		} `json:"data"`
		Extra struct {
			ErrorCode int    `json:"error_code"`
			LogID     string `json:"log_id"`
		} `json:"extra"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("douyin publish decode: %w", err)
	}
	if result.Data.ErrorCode != 0 {
		return "", "", fmt.Errorf("douyin publish error %d: %s", result.Data.ErrorCode, result.Data.Description)
	}

	itemID := result.Data.ItemID
	return itemID, fmt.Sprintf("https://www.douyin.com/video/%s", itemID), nil
}

func (p *DouyinPublisher) CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (string, error) {
	payload := map[string]interface{}{
		"item_ids": []string{externalID},
	}
	bodyBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.douyin.com/api/douyin/v1/video/query_video/",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return "unknown", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("access-token", account.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unknown", fmt.Errorf("douyin status check: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			List []struct {
				ItemID string `json:"item_id"`
				Status string `json:"status"` // posted|not_allowed|...
			} `json:"list"`
			ErrorCode int `json:"error_code"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "unknown", nil
	}
	if result.Data.ErrorCode != 0 || len(result.Data.List) == 0 {
		return "processing", nil
	}
	switch result.Data.List[0].Status {
	case "posted":
		return "published", nil
	case "not_allowed", "deleted":
		return "failed", nil
	default:
		return "processing", nil
	}
}
