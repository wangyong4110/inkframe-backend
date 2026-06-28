package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// WebSearchResult represents a single search result item.
type WebSearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Date    string  `json:"date,omitempty"`    // 内容发布时间（ISO8601 或 YYYY-MM-DD HH:MM:SS）
	Score   float64 `json:"score,omitempty"`   // 相关性得分 0~1（tencent-wsa）
	Site    string  `json:"site,omitempty"`    // 网站名称
	Favicon string  `json:"favicon,omitempty"` // 网站图标 URL
}

// WebSearcher is the interface for web search providers.
type WebSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error)
	Name() string
}

// TencentWSAOptions 腾讯 WSA 搜索高级选项（SearchPro 完整参数）
type TencentWSAOptions struct {
	Mode     int    // 0=自然检索（默认）1=多模态VR 2=混合
	Site     string // 站内搜索，如 zhihu.com
	FromTime int64  // 起始时间（Unix 秒）
	ToTime   int64  // 结束时间（Unix 秒）
	Cnt      int    // 结果数量，最大 50
	Industry string // gov/news/acad/finance（尊享版）
}

// NewWebSearcher creates the appropriate WebSearcher based on provider name.
// provider: "bing" | "searxng" | "tavily" | "tencent-wsa" | "" (noop)
// secretID / secretKey are only used for "tencent-wsa".
func NewWebSearcher(provider, apiKey, endpoint, secretID, secretKey string) WebSearcher {
	switch strings.ToLower(provider) {
	case "bing":
		return &BingSearcher{apiKey: apiKey}
	case "searxng":
		return &SearXNGSearcher{endpoint: endpoint}
	case "tavily":
		return &TavilySearcher{apiKey: apiKey}
	case "tencent-wsa":
		return NewTencentWSASearcher(secretID, secretKey)
	default:
		return &NoopSearcher{}
	}
}

// ──────────────────────────────────────────────────────────────
// NoopSearcher — placeholder when no provider is configured
// ──────────────────────────────────────────────────────────────

type NoopSearcher struct{}

func (n *NoopSearcher) Name() string { return "noop" }
func (n *NoopSearcher) Search(_ context.Context, _ string, _ int) ([]WebSearchResult, error) {
	return nil, nil
}

// ──────────────────────────────────────────────────────────────
// BingSearcher — Azure Bing Search v7 (1000 free calls/month)
// Header: Ocp-Apim-Subscription-Key
// Endpoint: https://api.bing.microsoft.com/v7.0/search
// ──────────────────────────────────────────────────────────────

type BingSearcher struct {
	apiKey string
}

func (b *BingSearcher) Name() string { return "bing" }

func (b *BingSearcher) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if b.apiKey == "" {
		return nil, fmt.Errorf("bing: api_key is empty")
	}
	u := "https://api.bing.microsoft.com/v7.0/search"
	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", maxResults))
	params.Set("mkt", "zh-CN")
	params.Set("responseFilter", "Webpages")
	fullURL := u + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", b.apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bing: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		WebPages struct {
			Value []struct {
				Name    string `json:"name"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("bing: parse response: %w", err)
	}

	out := make([]WebSearchResult, 0, len(result.WebPages.Value))
	for _, v := range result.WebPages.Value {
		out = append(out, WebSearchResult{
			Title:   v.Name,
			URL:     v.URL,
			Content: v.Snippet,
		})
	}
	return out, nil
}

// ──────────────────────────────────────────────────────────────
// SearXNGSearcher — self-hosted SearXNG (free, supports Chinese)
// GET {endpoint}/search?q={query}&format=json
// ──────────────────────────────────────────────────────────────

type SearXNGSearcher struct {
	endpoint string
}

func (s *SearXNGSearcher) Name() string { return "searxng" }

func (s *SearXNGSearcher) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if s.endpoint == "" {
		return nil, fmt.Errorf("searxng: endpoint is empty")
	}
	base := strings.TrimRight(s.endpoint, "/")
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("language", "zh-CN")
	fullURL := base + "/search?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("searxng: parse response: %w", err)
	}

	limit := maxResults
	if limit > len(result.Results) {
		limit = len(result.Results)
	}
	out := make([]WebSearchResult, 0, limit)
	for i := 0; i < limit; i++ {
		v := result.Results[i]
		out = append(out, WebSearchResult{
			Title:   v.Title,
			URL:     v.URL,
			Content: v.Content,
		})
	}
	return out, nil
}

// ──────────────────────────────────────────────────────────────
// TavilySearcher — Tavily AI Search API
// POST https://api.tavily.com/search
// ──────────────────────────────────────────────────────────────

type TavilySearcher struct {
	apiKey string
}

func (t *TavilySearcher) Name() string { return "tavily" }

func (t *TavilySearcher) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if t.apiKey == "" {
		return nil, fmt.Errorf("tavily: api_key is empty")
	}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"api_key":     t.apiKey,
		"query":       query,
		"max_results": maxResults,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("tavily: parse response: %w", err)
	}

	out := make([]WebSearchResult, 0, len(result.Results))
	for _, v := range result.Results {
		out = append(out, WebSearchResult{
			Title:   v.Title,
			URL:     v.URL,
			Content: v.Content,
		})
	}
	return out, nil
}

// ──────────────────────────────────────────────────────────────
// Helper functions
// ──────────────────────────────────────────────────────────────

// buildStorySearchQuery builds a search query from novel genre and chapter summary.
func buildStorySearchQuery(genre, summary string) string {
	// Extract key Chinese words from the summary (first 50 chars, skip common stop words)
	keywords := extractKeywords(summary, 3)
	if genre != "" {
		return genre + " 小说 " + keywords + " 经典情节写法"
	}
	return "小说 " + keywords + " 经典情节写法"
}

// extractKeywords extracts up to n keywords from a Chinese text.
func extractKeywords(text string, n int) string {
	// Simple approach: take the first meaningful words
	// Strip punctuation, take runes, return first 20 chars of meaningful content
	var sb strings.Builder
	count := 0
	for _, r := range text {
		if count >= 20 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
			count++
		} else if sb.Len() > 0 {
			sb.WriteRune(' ')
		}
	}
	return strings.TrimSpace(sb.String())
}

// ──────────────────────────────────────────────────────────────
// TencentWSASearcher — 腾讯云联网搜索 API（WSA SearchPro）
// Host:    wsa.tencentcloudapi.com
// Action:  SearchPro
// Version: 2025-05-08
// Auth:    TC3-HMAC-SHA256（腾讯云 API 3.0 标准签名）
// Docs:    https://cloud.tencent.com/document/product/1806
// ──────────────────────────────────────────────────────────────

const (
	wsaHost    = "wsa.tencentcloudapi.com"
	wsaAction  = "SearchPro"
	wsaVersion = "2025-05-08"
	wsaService = "wsa"
)

type TencentWSASearcher struct {
	secretID  string
	secretKey string
	client    *http.Client
}

func NewTencentWSASearcher(secretID, secretKey string) *TencentWSASearcher {
	return &TencentWSASearcher{
		secretID:  secretID,
		secretKey: secretKey,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *TencentWSASearcher) Name() string { return "tencent-wsa" }

// Search 实现 WebSearcher 接口，默认 Mode=2（混合结果）
func (s *TencentWSASearcher) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	return s.SearchPro(ctx, query, TencentWSAOptions{Mode: 2, Cnt: maxResults})
}

// SearchPro 支持腾讯 WSA SearchPro 的完整参数
func (s *TencentWSASearcher) SearchPro(ctx context.Context, query string, opts TencentWSAOptions) ([]WebSearchResult, error) {
	if s.secretID == "" || s.secretKey == "" {
		return nil, fmt.Errorf("tencent-wsa: secret_id or secret_key is empty")
	}
	maxResults := opts.Cnt
	if maxResults <= 0 {
		maxResults = 10
	}

	params := map[string]interface{}{
		"Query": query,
		"Cnt":   maxResults,
		"Mode":  opts.Mode,
	}
	if opts.Site != "" {
		params["Site"] = opts.Site
	}
	if opts.FromTime > 0 {
		params["FromTime"] = opts.FromTime
	}
	if opts.ToTime > 0 {
		params["ToTime"] = opts.ToTime
	}
	if opts.Industry != "" {
		params["Industry"] = opts.Industry
	}
	reqBody, _ := json.Marshal(params)

	timestamp := time.Now().Unix()
	auth, err := s.buildAuthHeader(timestamp, reqBody)
	if err != nil {
		return nil, fmt.Errorf("tencent-wsa: build auth: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+wsaHost, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Host", wsaHost)
	httpReq.Header.Set("X-TC-Action", wsaAction)
	httpReq.Header.Set("X-TC-Version", wsaVersion)
	httpReq.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	httpReq.Header.Set("Authorization", auth)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tencent-wsa: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tencent-wsa: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tencent-wsa: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析外层结构
	var outer struct {
		Response struct {
			Pages     []string `json:"Pages"`
			Query     string   `json:"Query"`
			Msg       string   `json:"Msg"`
			RequestID string   `json:"RequestId"`
			Error     *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(respBody, &outer); err != nil {
		return nil, fmt.Errorf("tencent-wsa: parse response: %w", err)
	}
	if outer.Response.Error != nil {
		return nil, fmt.Errorf("tencent-wsa: API error %s: %s", outer.Response.Error.Code, outer.Response.Error.Message)
	}

	// 每条 Page 是一个 JSON 字符串，解析后提取字段
	type pageItem struct {
		Title   string  `json:"title"`
		Date    string  `json:"date"`
		URL     string  `json:"url"`
		Passage string  `json:"passage"`
		Content string  `json:"content"` // 尊享版专属，比 passage 更长
		Site    string  `json:"site"`
		Score   float64 `json:"score"`
		Favicon string  `json:"favicon"`
	}

	out := make([]WebSearchResult, 0, len(outer.Response.Pages))
	for _, raw := range outer.Response.Pages {
		var item pageItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			continue
		}
		// 优先用 content（尊享版），否则用 passage
		text := item.Content
		if text == "" {
			text = item.Passage
		}
		out = append(out, WebSearchResult{
			Title:   item.Title,
			URL:     item.URL,
			Content: text,
			Date:    item.Date,
			Score:   item.Score,
			Site:    item.Site,
			Favicon: item.Favicon,
		})
	}
	return out, nil
}

// buildAuthHeader 生成 TC3-HMAC-SHA256 Authorization 头
func (s *TencentWSASearcher) buildAuthHeader(timestamp int64, payload []byte) (string, error) {
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")

	canonicalHeaders := "content-type:application/json\nhost:" + wsaHost + "\n"
	signedHeaders := "content-type;host"
	hashedPayload := wsaSHA256Hex(payload)
	canonicalReq := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + hashedPayload

	credentialScope := date + "/" + wsaService + "/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		credentialScope + "\n" +
		wsaSHA256Hex([]byte(canonicalReq))

	secretDate := wsaHMACSHA256([]byte("TC3"+s.secretKey), []byte(date))
	secretService := wsaHMACSHA256(secretDate, []byte(wsaService))
	secretSigning := wsaHMACSHA256(secretService, []byte("tc3_request"))
	signature := hex.EncodeToString(wsaHMACSHA256(secretSigning, []byte(stringToSign)))

	auth := fmt.Sprintf(
		"TC3-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.secretID, credentialScope, signedHeaders, signature,
	)
	return auth, nil
}

func wsaSHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func wsaHMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// ──────────────────────────────────────────────────────────────

// formatRefStories formats search results as a readable prompt section.
// Returns at most 3 results, each truncated to 200 characters.
func formatRefStories(results []WebSearchResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	limit := 3
	if len(results) < limit {
		limit = len(results)
	}
	for i := 0; i < limit; i++ {
		r := results[i]
		title := r.Title
		content := r.Content
		// Truncate content to 200 chars
		runes := []rune(content)
		if len(runes) > 200 {
			content = string(runes[:200]) + "……"
		}
		if title != "" {
			sb.WriteString(fmt.Sprintf("**%s**\n", title))
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}
