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

// ImageRefResult is a single image reference result.
type ImageRefResult struct {
	URL     string `json:"url"`
	ThumbURL string `json:"thumb_url"`
	Tags    string `json:"tags"`
	PageURL string `json:"page_url"`
}

// ImageRefSearcher searches for visual reference images.
type ImageRefSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]ImageRefResult, error)
	Name() string
}

// PixabayImageSearcher queries Pixabay (free tier: ~1800/day, no credit required for download links).
type PixabayImageSearcher struct {
	apiKey string
	client *http.Client
}

// NewPixabayImageSearcher creates a Pixabay-backed image searcher.
func NewPixabayImageSearcher(apiKey string) *PixabayImageSearcher {
	return &PixabayImageSearcher{
		apiKey: apiKey,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *PixabayImageSearcher) Name() string { return "pixabay" }

func (p *PixabayImageSearcher) Search(ctx context.Context, query string, maxResults int) ([]ImageRefResult, error) {
	if maxResults <= 0 {
		maxResults = 3
	}

	params := url.Values{
		"key":        {p.apiKey},
		"q":          {url.QueryEscape(query)},
		"image_type": {"photo"},
		"per_page":   {fmt.Sprintf("%d", maxResults)},
		"safesearch": {"true"},
		"lang":       {"en"},
		"min_width":  {"1280"},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://pixabay.com/api/?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pixabay request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Hits []struct {
			WebformatURL string `json:"webformatURL"`
			PreviewURL   string `json:"previewURL"`
			Tags         string `json:"tags"`
			PageURL      string `json:"pageURL"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("pixabay decode failed: %w", err)
	}

	results := make([]ImageRefResult, 0, len(result.Hits))
	for _, h := range result.Hits {
		results = append(results, ImageRefResult{
			URL:     h.WebformatURL,
			ThumbURL: h.PreviewURL,
			Tags:    h.Tags,
			PageURL: h.PageURL,
		})
	}
	return results, nil
}

// UnsplashImageSearcher queries Unsplash (requires free developer account).
type UnsplashImageSearcher struct {
	accessKey string
	client    *http.Client
}

// NewUnsplashImageSearcher creates an Unsplash-backed image searcher.
func NewUnsplashImageSearcher(accessKey string) *UnsplashImageSearcher {
	return &UnsplashImageSearcher{
		accessKey: accessKey,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (u *UnsplashImageSearcher) Name() string { return "unsplash" }

func (u *UnsplashImageSearcher) Search(ctx context.Context, query string, maxResults int) ([]ImageRefResult, error) {
	if maxResults <= 0 {
		maxResults = 3
	}

	params := url.Values{
		"query":    {query},
		"per_page": {fmt.Sprintf("%d", maxResults)},
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.unsplash.com/search/photos?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Client-ID "+u.accessKey)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unsplash request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Results []struct {
			URLs struct {
				Regular string `json:"regular"`
				Thumb   string `json:"thumb"`
			} `json:"urls"`
			Links struct {
				HTML string `json:"html"`
			} `json:"links"`
			AltDescription string `json:"alt_description"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("unsplash decode failed: %w", err)
	}

	results := make([]ImageRefResult, 0, len(result.Results))
	for _, r := range result.Results {
		results = append(results, ImageRefResult{
			URL:     r.URLs.Regular,
			ThumbURL: r.URLs.Thumb,
			Tags:    r.AltDescription,
			PageURL: r.Links.HTML,
		})
	}
	return results, nil
}

// NoopImageRefSearcher is a no-op when no API key is configured.
type NoopImageRefSearcher struct{}

func (n *NoopImageRefSearcher) Name() string { return "noop" }
func (n *NoopImageRefSearcher) Search(_ context.Context, _ string, _ int) ([]ImageRefResult, error) {
	return nil, nil
}

// NewImageRefSearcher creates an image reference searcher based on provider name and key.
// provider: "pixabay" | "unsplash" | "" (noop)
func NewImageRefSearcher(provider, apiKey string) ImageRefSearcher {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "pixabay":
		if apiKey != "" {
			return NewPixabayImageSearcher(apiKey)
		}
	case "unsplash":
		if apiKey != "" {
			return NewUnsplashImageSearcher(apiKey)
		}
	}
	return &NoopImageRefSearcher{}
}

// formatImageRefResults formats image results as readable prompt hints (URL list).
func formatImageRefResults(results []ImageRefResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range results {
		if i >= 3 {
			break
		}
		sb.WriteString(fmt.Sprintf("- %s", r.URL))
		if r.Tags != "" {
			sb.WriteString(fmt.Sprintf("（tags: %s）", r.Tags))
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}
