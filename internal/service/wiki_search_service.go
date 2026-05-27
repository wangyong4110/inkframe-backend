package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WikiSearchResult is a single encyclopedic search result.
type WikiSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Extract string `json:"extract"`
}

// WikiSearcher searches encyclopedic knowledge (Wikipedia / Baidu Baike).
type WikiSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]WikiSearchResult, error)
	Name() string
}

// WikipediaSearcher queries Chinese Wikipedia via the OpenSearch API.
type WikipediaSearcher struct {
	client *http.Client
	lang   string // "zh" | "en"
}

// NewWikiSearcher returns a WikipediaSearcher for the Chinese edition.
// Wikipedia is freely accessible without an API key.
func NewWikiSearcher() WikiSearcher {
	return &WikipediaSearcher{
		client: &http.Client{Timeout: 10 * time.Second},
		lang:   "zh",
	}
}

func (w *WikipediaSearcher) Name() string { return "wikipedia_" + w.lang }

func (w *WikipediaSearcher) Search(ctx context.Context, query string, maxResults int) ([]WikiSearchResult, error) {
	if maxResults <= 0 {
		maxResults = 3
	}

	apiBase := fmt.Sprintf("https://%s.wikipedia.org/w/api.php", w.lang)
	params := url.Values{
		"action": {"opensearch"},
		"search": {query},
		"limit":  {fmt.Sprintf("%d", maxResults)},
		"format": {"json"},
		"utf8":   {""},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiBase+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "InkFrame/1.0 (+https://github.com/inkframe)")

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wikipedia opensearch failed: %w", err)
	}
	defer resp.Body.Close()

	// OpenSearch format: [query, [titles], [descriptions], [urls]]
	var raw []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("wikipedia decode failed: %w", err)
	}
	if len(raw) < 4 {
		return nil, nil
	}

	titles, _ := raw[1].([]interface{})
	descs, _ := raw[2].([]interface{})
	urls, _ := raw[3].([]interface{})

	var results []WikiSearchResult
	for i := 0; i < len(titles) && i < maxResults; i++ {
		title, _ := titles[i].(string)
		desc, _ := descs[i].(string)
		pageURL, _ := urls[i].(string)
		if title == "" {
			continue
		}
		// Trim overly long extracts to save tokens
		runes := []rune(desc)
		if len(runes) > 300 {
			desc = string(runes[:300]) + "..."
		}
		results = append(results, WikiSearchResult{
			Title:   title,
			URL:     pageURL,
			Extract: desc,
		})
	}
	return results, nil
}

// NoopWikiSearcher is a no-op placeholder.
type NoopWikiSearcher struct{}

func (n *NoopWikiSearcher) Name() string { return "noop" }
func (n *NoopWikiSearcher) Search(_ context.Context, _ string, _ int) ([]WikiSearchResult, error) {
	return nil, nil
}

// buildWikiSearchQuery builds a focused search query from the novel genre and chapter summary.
func buildWikiSearchQuery(genre, summary string) string {
	keywords := extractKeywords(summary, 3) // reuse helper from websearch_service.go; returns string
	parts := make([]string, 0, 4)
	if genre != "" {
		parts = append(parts, genre)
	}
	if keywords != "" {
		parts = append(parts, keywords)
	}
	return strings.Join(parts, " ")
}

// formatWikiResults formats results as a readable prompt section.
func formatWikiResults(results []WikiSearchResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range results {
		if i >= 3 {
			break
		}
		sb.WriteString(fmt.Sprintf("【%s】\n", r.Title))
		if r.Extract != "" {
			sb.WriteString(r.Extract)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// parseWikiOutput parses the output map from McpService.InvokeTool("wiki_search", …).
func parseWikiOutput(output map[string]interface{}) string {
	if output == nil {
		return ""
	}
	raw, ok := output["results"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	var results []WikiSearchResult
	if err := json.Unmarshal(b, &results); err != nil {
		return ""
	}
	return formatWikiResults(results)
}
