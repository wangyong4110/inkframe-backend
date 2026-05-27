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
	"unicode"
)

// WebSearchResult represents a single search result item.
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// WebSearcher is the interface for web search providers.
type WebSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error)
	Name() string
}

// NewWebSearcher creates the appropriate WebSearcher based on provider name.
// provider: "bing" | "searxng" | "tavily" | "" (noop)
func NewWebSearcher(provider, apiKey, endpoint string) WebSearcher {
	switch strings.ToLower(provider) {
	case "bing":
		return &BingSearcher{apiKey: apiKey}
	case "searxng":
		return &SearXNGSearcher{endpoint: endpoint}
	case "tavily":
		return &TavilySearcher{apiKey: apiKey}
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
