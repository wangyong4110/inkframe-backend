package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// YoutubePublisher YouTube Data API v3 发布器
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
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&state=%s&response_type=code&scope=%s&access_type=offline&prompt=consent",
		url.QueryEscape(p.ClientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape(state),
		url.QueryEscape("https://www.googleapis.com/auth/youtube.upload https://www.googleapis.com/auth/youtube.readonly"),
	)
}

func (p *YoutubePublisher) ExchangeToken(ctx context.Context, code, redirectURI string) (*model.PlatformAccount, error) {
	if p.ClientID == "" {
		return nil, fmt.Errorf("youtube: ClientID not configured — set YOUTUBE_CLIENT_ID env var")
	}

	form := url.Values{
		"code":          {code},
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube token exchange: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("youtube token decode: %w", err)
	}
	if tokenResp.Error != "" {
		return nil, fmt.Errorf("youtube token error: %s — %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	// 获取频道信息（作为 UID）
	channelID, channelName, _ := p.fetchChannelInfo(ctx, tokenResp.AccessToken)

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return &model.PlatformAccount{
		Platform:     "youtube",
		UID:          channelID,
		AccountName:  channelName,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    &expiresAt,
		Status:       "active",
	}, nil
}

func (p *YoutubePublisher) RefreshToken(ctx context.Context, account *model.PlatformAccount) error {
	if p.ClientID == "" {
		return fmt.Errorf("youtube: ClientID not configured")
	}

	form := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"refresh_token": {account.RefreshToken},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("youtube refresh: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("youtube refresh decode: %w", err)
	}
	if tokenResp.Error != "" {
		account.Status = "expired"
		return fmt.Errorf("youtube refresh error: %s — %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	account.AccessToken = tokenResp.AccessToken
	account.ExpiresAt = &expiresAt
	account.Status = "active"
	return nil
}

// PublishVideo 上传视频到 YouTube（resumable upload）
func (p *YoutubePublisher) PublishVideo(ctx context.Context, account *model.PlatformAccount, videoURL string, opts PublishOptions) (externalID, externalURL string, err error) {
	if account.AccessToken == "" {
		return "", "", fmt.Errorf("youtube: account not authorized")
	}

	// Step 1: 初始化 resumable upload session，获取 upload URL
	privacyStatus := "public"
	if !opts.IsPublic {
		privacyStatus = "private"
	}

	metadata := map[string]interface{}{
		"snippet": map[string]interface{}{
			"title":       opts.Title,
			"description": opts.Description,
			"tags":        opts.Tags,
			"categoryId":  "24", // Entertainment
		},
		"status": map[string]interface{}{
			"privacyStatus": privacyStatus,
		},
	}
	metaBytes, _ := json.Marshal(metadata)

	initReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&part=snippet,status",
		bytes.NewReader(metaBytes),
	)
	if err != nil {
		return "", "", err
	}
	initReq.Header.Set("Authorization", "Bearer "+account.AccessToken)
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("X-Upload-Content-Type", "video/*")

	initResp, err := http.DefaultClient.Do(initReq)
	if err != nil {
		return "", "", fmt.Errorf("youtube upload init: %w", err)
	}
	defer initResp.Body.Close()

	if initResp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(initResp.Body, 512))
		return "", "", fmt.Errorf("youtube upload init HTTP %d: %s", initResp.StatusCode, body)
	}

	uploadURL := initResp.Header.Get("Location")
	if uploadURL == "" {
		return "", "", fmt.Errorf("youtube: no upload URL in response")
	}

	// Step 2: 下载视频源并流式上传至 YouTube
	dlResp, err := http.Get(videoURL) //nolint:noctx
	if err != nil {
		return "", "", fmt.Errorf("youtube: download source video: %w", err)
	}
	defer dlResp.Body.Close()

	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, dlResp.Body)
	if err != nil {
		return "", "", err
	}
	uploadReq.Header.Set("Authorization", "Bearer "+account.AccessToken)
	uploadReq.Header.Set("Content-Type", "video/*")
	if dlResp.ContentLength > 0 {
		uploadReq.ContentLength = dlResp.ContentLength
	}

	uploadResp, err := http.DefaultClient.Do(uploadReq)
	if err != nil {
		return "", "", fmt.Errorf("youtube upload: %w", err)
	}
	defer uploadResp.Body.Close()

	if uploadResp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(uploadResp.Body, 512))
		return "", "", fmt.Errorf("youtube upload HTTP %d: %s", uploadResp.StatusCode, body)
	}

	var videoResult struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(uploadResp.Body).Decode(&videoResult); err != nil {
		return "", "", fmt.Errorf("youtube upload decode: %w", err)
	}
	if videoResult.ID == "" {
		return "", "", fmt.Errorf("youtube: empty video ID in response")
	}

	return videoResult.ID, fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoResult.ID), nil
}

func (p *YoutubePublisher) CheckPublishStatus(ctx context.Context, account *model.PlatformAccount, externalID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://www.googleapis.com/youtube/v3/videos?id=%s&part=status", url.QueryEscape(externalID)),
		nil,
	)
	if err != nil {
		return "unknown", err
	}
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unknown", fmt.Errorf("youtube status check: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Items []struct {
			Status struct {
				UploadStatus  string `json:"uploadStatus"`
				PrivacyStatus string `json:"privacyStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Items) == 0 {
		return "processing", nil
	}

	switch result.Items[0].Status.UploadStatus {
	case "processed":
		return "published", nil
	case "failed", "deleted", "rejected":
		return "failed", nil
	default:
		return "processing", nil
	}
}

func (p *YoutubePublisher) fetchChannelInfo(ctx context.Context, accessToken string) (channelID, channelName string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.googleapis.com/youtube/v3/channels?part=snippet&mine=true",
		nil,
	)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var result struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Items) == 0 {
		return "", "", nil
	}
	return result.Items[0].ID, result.Items[0].Snippet.Title, nil
}
