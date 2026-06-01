package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// BilibiliPublisher 哔哩哔哩开放平台发布器
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
		url.QueryEscape(p.AppKey), url.QueryEscape(redirectURI), url.QueryEscape(state),
	)
}

func (p *BilibiliPublisher) ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error) {
	if p.AppKey == "" {
		return nil, fmt.Errorf("bilibili: AppKey not configured — set BILIBILI_APP_KEY env var")
	}

	form := url.Values{
		"client_id":     {p.AppKey},
		"client_secret": {p.AppSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://passport.bilibili.com/x/passport-oauth2/access_token",
		bytes.NewBufferString(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bilibili token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			TokenInfo struct {
				Mid          int64  `json:"mid"`
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int64  `json:"expires_in"`
			} `json:"token_info"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("bilibili token decode: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("bilibili token error %d: %s", result.Code, result.Message)
	}

	expiresAt := time.Now().Add(time.Duration(result.Data.TokenInfo.ExpiresIn) * time.Second)
	return &model.PlatformAccount{
		Platform:     "bilibili",
		UID:          fmt.Sprintf("%d", result.Data.TokenInfo.Mid),
		AccessToken:  result.Data.TokenInfo.AccessToken,
		RefreshToken: result.Data.TokenInfo.RefreshToken,
		ExpiresAt:    &expiresAt,
		Status:       "active",
	}, nil
}

func (p *BilibiliPublisher) RefreshToken(ctx context.Context, account *model.PlatformAccount) error {
	if p.AppKey == "" {
		return fmt.Errorf("bilibili: AppKey not configured")
	}

	form := url.Values{
		"client_id":     {p.AppKey},
		"client_secret": {p.AppSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {account.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://passport.bilibili.com/x/passport-oauth2/access_token",
		bytes.NewBufferString(form.Encode()),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("bilibili refresh: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			TokenInfo struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int64  `json:"expires_in"`
			} `json:"token_info"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("bilibili refresh decode: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("bilibili refresh error %d: %s", result.Code, result.Message)
	}

	expiresAt := time.Now().Add(time.Duration(result.Data.TokenInfo.ExpiresIn) * time.Second)
	account.AccessToken = result.Data.TokenInfo.AccessToken
	account.RefreshToken = result.Data.TokenInfo.RefreshToken
	account.ExpiresAt = &expiresAt
	account.Status = "active"
	return nil
}

// PublishVideo 上传视频到哔哩哔哩（视频URL直传投稿接口）
func (p *BilibiliPublisher) PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error) {
	if account.AccessToken == "" {
		return "", "", fmt.Errorf("bilibili: account not authorized")
	}

	// 使用B站客户端上传接口（URL投稿）
	body := map[string]interface{}{
		"copyright": 2, // 转载
		"source":    videoURL,
		"title":     opts.Title,
		"desc":      opts.Description,
		"tag":       joinTags(opts.Tags),
		"tid":       17, // 分类：生活其他
		"no_reprint": 0,
		"open":      map[string]interface{}{
			"is_open": 1,
		},
		"videos": []map[string]interface{}{
			{"filename": "video", "title": opts.Title, "desc": ""},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://member.bilibili.com/x/vu/client/add",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("bilibili publish: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			Aid  int64  `json:"aid"`
			BVid string `json:"bvid"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("bilibili publish decode: %w", err)
	}
	if result.Code != 0 {
		return "", "", fmt.Errorf("bilibili publish error %d: %s", result.Code, result.Message)
	}

	bvid := result.Data.BVid
	return bvid, fmt.Sprintf("https://www.bilibili.com/video/%s", bvid), nil
}

func (p *BilibiliPublisher) CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://member.bilibili.com/x/vu/client/card?bvid=%s", url.QueryEscape(externalID)),
		nil,
	)
	if err != nil {
		return "unknown", err
	}
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unknown", fmt.Errorf("bilibili status check: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			Archive struct {
				State int `json:"state"` // 0=审核中, 1=已通过, -1=未通过
			} `json:"archive"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "unknown", nil
	}
	if result.Code != 0 {
		return "unknown", nil
	}
	switch result.Data.Archive.State {
	case 1:
		return "published", nil
	case -1:
		return "failed", nil
	default:
		return "processing", nil
	}
}

func joinTags(tags []string) string {
	result := ""
	for i, t := range tags {
		if i > 0 {
			result += ","
		}
		result += t
	}
	return result
}

// bilibiliDownloadToReader streams a video URL as a reader (used for chunked upload if needed).
func bilibiliDownloadToReader(ctx context.Context, videoURL string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}
