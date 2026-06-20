package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
	"github.com/inkframe/inkframe-backend/internal/model"
	"golang.org/x/net/html"
	"gorm.io/gorm"
)

// siteDomains maps site key to allowed colly domain
var siteDomains = map[string]string{
	"qidian":   "www.qidian.com",
	"jjwxc":    "www.jjwxc.net",
	"zongheng": "book.zongheng.com",
	"qimao":    "www.qimao.com",
}

// ---------------------------------------------------------------------------
// CrawlConfig — per-crawl HTTP/parsing behavior
// ---------------------------------------------------------------------------

// CrawlConfig configures per-crawl HTTP/parsing behavior
type CrawlConfig struct {
	Concurrency   int    `json:"concurrency"`     // max parallel chapter fetches (1–5, default 1)
	DelayMs       int    `json:"delay_ms"`        // base request delay in ms (default 2000)
	RandomDelayMs int    `json:"random_delay_ms"` // extra random jitter in ms (default 3000)
	UARotation    bool   `json:"ua_rotation"`     // rotate browser User-Agents (default true)
	DetectCharset bool   `json:"detect_charset"`  // auto-detect GBK/GB2312 (default true)
	CacheEnabled  bool   `json:"cache_enabled"`   // disk-cache responses (default false)
	CacheDir      string `json:"cache_dir"`       // disk cache path, default "./.crawl_cache"
	MaxRetries    int    `json:"max_retries"`     // retry attempts on error (default 3)
	ProxyURL      string `json:"proxy_url"`       // optional HTTP/SOCKS5 proxy URL
	SkipVIPChaps  bool   `json:"skip_vip_chaps"`  // skip paid chapters
}

func DefaultCrawlConfig() CrawlConfig {
	return CrawlConfig{
		Concurrency:   1,
		DelayMs:       2000,
		RandomDelayMs: 3000,
		UARotation:    true,
		DetectCharset: true,
		MaxRetries:    3,
	}
}

// ---------------------------------------------------------------------------
// CrawlStats — download metrics across all collectors for one crawl session
// ---------------------------------------------------------------------------

// CrawlStats tracks download metrics across all collectors for one crawl session
type CrawlStats struct {
	mu              sync.Mutex
	PagesVisited    int       `json:"pages_visited"`
	BytesDownloaded int64     `json:"bytes_downloaded"`
	SuccessCount    int       `json:"success_count"`
	FailCount       int       `json:"fail_count"`
	StartedAt       time.Time `json:"started_at"`
}

func (s *CrawlStats) recordResponse(bodyLen int64) {
	s.mu.Lock()
	s.PagesVisited++
	s.BytesDownloaded += bodyLen
	s.mu.Unlock()
}
func (s *CrawlStats) recordSuccess() { s.mu.Lock(); s.SuccessCount++; s.mu.Unlock() }
func (s *CrawlStats) recordFail()    { s.mu.Lock(); s.FailCount++; s.mu.Unlock() }
func (s *CrawlStats) ElapsedSeconds() float64 { return time.Since(s.StartedAt).Seconds() }
func (s *CrawlStats) Snapshot() CrawlStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CrawlStats{
		PagesVisited:    s.PagesVisited,
		BytesDownloaded: s.BytesDownloaded,
		SuccessCount:    s.SuccessCount,
		FailCount:       s.FailCount,
		StartedAt:       s.StartedAt,
	}
}

// ---------------------------------------------------------------------------
// ChapterFetchResult — one result from FetchChapterStream
// ---------------------------------------------------------------------------

// ChapterFetchResult is one result from FetchChapterStream
type ChapterFetchResult struct {
	Index   int // position in the input slice
	Chapter *ChapterInfo
	Content *ChapterContent
	Err     error
}

// ---------------------------------------------------------------------------
// NovelCrawler 小说爬虫 (colly-based)
// ---------------------------------------------------------------------------

// NovelCrawler 小说爬虫 (colly-based)
type NovelCrawler struct {
	db      *gorm.DB
	parsers map[string]NovelParser
	jar     *cookiejar.Jar // shared cookie jar across all collectors
	config  CrawlConfig
	stats   CrawlStats
}

// NovelParser site-specific HTML parser.
// root is the goquery.Selection of the <html> element from the page.
type NovelParser interface {
	ParseNovelList(root *goquery.Selection) ([]*NovelInfo, error)
	ParseNovelDetail(root *goquery.Selection, bookURL string) (*NovelDetail, error)
	ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error)
	ParseChapter(root *goquery.Selection) (*ChapterContent, error)
	GetSiteName() string
}

// ChapterListFetcher optional extension: parser fetches chapter list via its own HTTP request
// (e.g., Ajax API). Preferred over ParseChapterList; falls back to HTML parse on error.
type ChapterListFetcher interface {
	FetchChapterList(ctx context.Context, jar http.CookieJar, bookURL string) ([]*ChapterInfo, error)
}

// NovelInfo 小说信息
type NovelInfo struct {
	Title    string `json:"title"`
	Author   string `json:"author"`
	URL      string `json:"url"`
	Genre    string `json:"genre"`
	CoverURL string `json:"cover_url"`
}

// NovelDetail 小说详情
type NovelDetail struct {
	Title         string   `json:"title"`
	Author        string   `json:"author"`
	Description   string   `json:"description"`
	Genre         string   `json:"genre"`
	Tags          []string `json:"tags"`
	Status        string   `json:"status"` // ongoing, completed
	TotalChapters int      `json:"total_chapters"`
	TotalWords    int      `json:"total_words"`
	CoverURL      string   `json:"cover_url"`
}

// ChapterInfo 章节信息
type ChapterInfo struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	ChapterNo int    `json:"chapter_no"`
	IsVip     bool   `json:"is_vip"` // 付费章节（内容可能无法爬取）
}

// ChapterContent 章节内容
type ChapterContent struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	PrevURL string `json:"prev_url"`
	NextURL string `json:"next_url"`
}

// NewNovelCrawler 创建爬虫
func NewNovelCrawler(db *gorm.DB) *NovelCrawler {
	jar, _ := cookiejar.New(nil)
	c := &NovelCrawler{
		db:      db,
		parsers: make(map[string]NovelParser),
		jar:     jar,
		config:  DefaultCrawlConfig(),
	}
	c.parsers["qidian"] = NewQidianParser()
	c.parsers["jjwxc"] = NewJjwxcParser()
	c.parsers["zongheng"] = NewZonghengParser()
	c.parsers["qimao"] = NewQimaoParser()
	return c
}

// SetConfig replaces the current crawl config.
func (nc *NovelCrawler) SetConfig(cfg CrawlConfig) {
	nc.config = cfg
}

// GetStats returns a snapshot of current crawl stats.
func (nc *NovelCrawler) GetStats() CrawlStats {
	return nc.stats.Snapshot()
}

// newCollector creates a colly.Collector configured with the shared cookie jar,
// standard browser headers, 30 s timeout, and rate limiting based on config.
// If domain is empty, no AllowedDomains restriction is applied (for generic/unknown sites).
func (nc *NovelCrawler) newCollector(domain string) *colly.Collector {
	opts := []colly.CollectorOption{
		colly.AllowURLRevisit(),
	}
	if domain != "" {
		opts = append(opts, colly.AllowedDomains(domain))
	}
	if nc.config.DetectCharset {
		opts = append(opts, colly.DetectCharset())
	}
	if nc.config.CacheEnabled {
		cacheDir := nc.config.CacheDir
		if cacheDir == "" {
			cacheDir = "./.crawl_cache"
		}
		opts = append(opts, colly.CacheDir(cacheDir))
	}

	col := colly.NewCollector(opts...)
	col.SetCookieJar(nc.jar)
	col.SetRequestTimeout(30 * time.Second)

	if nc.config.UARotation {
		extensions.RandomUserAgent(col)
	} else {
		col.OnRequest(func(r *colly.Request) {
			r.Headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		})
	}

	col.OnRequest(func(r *colly.Request) {
		r.Headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		r.Headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	})

	if nc.config.ProxyURL != "" {
		if err := col.SetProxy(nc.config.ProxyURL); err == nil {
			// proxy set successfully
			_ = err
		}
	}

	col.OnResponse(func(r *colly.Response) {
		nc.stats.recordResponse(int64(len(r.Body)))
	})

	// Enforce minimum safe crawl rate
	if nc.config.DelayMs < 1000 {
		nc.config.DelayMs = 1000
	}
	if nc.config.Concurrency > 2 {
		nc.config.Concurrency = 2
	}
	_ = col.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Delay:       time.Duration(nc.config.DelayMs) * time.Millisecond,
		RandomDelay: time.Duration(nc.config.RandomDelayMs) * time.Millisecond,
	})
	return col
}

// newAsyncCollector creates an async colly.Collector with parallelism support.
// If domain is empty, no AllowedDomains restriction is applied (for generic/unknown sites).
func (nc *NovelCrawler) newAsyncCollector(domain string) *colly.Collector {
	opts := []colly.CollectorOption{
		colly.AllowURLRevisit(),
		colly.Async(true),
	}
	if domain != "" {
		opts = append(opts, colly.AllowedDomains(domain))
	}
	if nc.config.DetectCharset {
		opts = append(opts, colly.DetectCharset())
	}
	if nc.config.CacheEnabled {
		cacheDir := nc.config.CacheDir
		if cacheDir == "" {
			cacheDir = "./.crawl_cache"
		}
		opts = append(opts, colly.CacheDir(cacheDir))
	}

	col := colly.NewCollector(opts...)
	col.SetCookieJar(nc.jar)
	col.SetRequestTimeout(30 * time.Second)

	if nc.config.UARotation {
		extensions.RandomUserAgent(col)
	} else {
		col.OnRequest(func(r *colly.Request) {
			r.Headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		})
	}

	col.OnRequest(func(r *colly.Request) {
		r.Headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		r.Headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	})

	if nc.config.ProxyURL != "" {
		_ = col.SetProxy(nc.config.ProxyURL)
	}

	col.OnResponse(func(r *colly.Response) {
		nc.stats.recordResponse(int64(len(r.Body)))
	})

	// Enforce minimum safe crawl rate
	if nc.config.DelayMs < 1000 {
		nc.config.DelayMs = 1000
	}
	if nc.config.Concurrency > 2 {
		nc.config.Concurrency = 2
	}
	parallelism := nc.config.Concurrency
	if parallelism < 1 {
		parallelism = 1
	}
	_ = col.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Delay:       time.Duration(nc.config.DelayMs) * time.Millisecond,
		RandomDelay: time.Duration(nc.config.RandomDelayMs) * time.Millisecond,
		Parallelism: parallelism,
	})
	return col
}

// chaptersDomain returns the host from chapters[0].URL.
func chaptersDomain(chapters []*ChapterInfo) string {
	if len(chapters) == 0 {
		return ""
	}
	if u, err := url.Parse(chapters[0].URL); err == nil {
		return u.Host
	}
	return ""
}

// FetchChapterStream fetches all chapters concurrently using an async collector.
// Results are delivered in completion order (not necessarily input order).
// The returned channel is closed after all chapters have been processed.
func (nc *NovelCrawler) FetchChapterStream(ctx context.Context, chapters []*ChapterInfo, parser NovelParser) <-chan ChapterFetchResult {
	bufSize := nc.config.Concurrency * 4
	if bufSize < 4 {
		bufSize = 4
	}
	resultCh := make(chan ChapterFetchResult, bufSize)

	go func() {
		defer close(resultCh)

		domain := chaptersDomain(chapters)
		// GenericParser 不限制域名：未知站点可能重定向到 CDN 或不同域名
		if _, isGeneric := parser.(*GenericParser); isGeneric {
			domain = ""
		}
		col := nc.newAsyncCollector(domain)

		// 为 GenericParser 添加 Referer（减少反爬拦截）
		if gp, isGeneric := parser.(*GenericParser); isGeneric && gp.bookURL != "" {
			referer := gp.bookURL
			col.OnRequest(func(r *colly.Request) {
				if r.Headers.Get("Referer") == "" {
					r.Headers.Set("Referer", referer)
				}
			})
		}

		col.OnHTML("html", func(e *colly.HTMLElement) {
			idxRaw := e.Request.Ctx.Get("chapter_idx")
			var idx int
			fmt.Sscanf(idxRaw, "%d", &idx)

			content, err := parser.ParseChapter(e.DOM)
			if err != nil {
				nc.stats.recordFail()
				select {
				case resultCh <- ChapterFetchResult{Index: idx, Chapter: chapters[idx], Err: err}:
				case <-ctx.Done():
				}
				return
			}
			if content == nil || content.Content == "" {
				// 页面已加载但正文为空：记录页面大小帮助诊断（JS 渲染 / 反爬 / 选择器不匹配）
				log.Printf("[Crawler] WARN idx=%d url=%s pageBytes=%d: content empty after parse (possible JS-rendered or anti-bot page)",
					idx, e.Request.URL.String(), len(e.Response.Body))
			}
			nc.stats.recordSuccess()
			select {
			case resultCh <- ChapterFetchResult{Index: idx, Chapter: chapters[idx], Content: content}:
			case <-ctx.Done():
			}
		})

		col.OnError(func(r *colly.Response, err error) {
			idxRaw := r.Request.Ctx.Get("chapter_idx")
			var idx int
			fmt.Sscanf(idxRaw, "%d", &idx)
			nc.stats.recordFail()
			select {
			case resultCh <- ChapterFetchResult{Index: idx, Chapter: chapters[idx], Err: err}:
			case <-ctx.Done():
			}
		})

		for i, ch := range chapters {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if ch.IsVip && nc.config.SkipVIPChaps {
				select {
				case resultCh <- ChapterFetchResult{Index: i, Chapter: ch, Err: fmt.Errorf("skipped VIP chapter: %s", ch.Title)}:
				case <-ctx.Done():
					return
				}
				continue
			}

			rCtx := colly.NewContext()
			rCtx.Put("chapter_idx", fmt.Sprintf("%d", i))
			if err := col.Request("GET", ch.URL, nil, rCtx, nil); err != nil {
				nc.stats.recordFail()
				select {
				case resultCh <- ChapterFetchResult{Index: i, Chapter: ch, Err: err}:
				case <-ctx.Done():
					return
				}
			}
		}

		col.Wait()
	}()

	return resultCh
}

// fetchHTML visits rawURL with colly and returns the <html> root as a goquery.Selection.
// Retries up to MaxRetries times with exponential back-off on transient errors.
func (nc *NovelCrawler) fetchHTML(rawURL, domain string) (*goquery.Selection, error) {
	maxAttempts := nc.config.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
		var (
			root     *goquery.Selection
			fetchErr error
		)
		col := nc.newCollector(domain)
		col.OnHTML("html", func(e *colly.HTMLElement) {
			root = e.DOM
		})
		col.OnError(func(_ *colly.Response, err error) {
			fetchErr = err
		})
		if err := col.Visit(rawURL); err != nil {
			lastErr = err
			continue
		}
		if fetchErr != nil {
			lastErr = fetchErr
			continue
		}
		if root != nil {
			return root, nil
		}
		lastErr = fmt.Errorf("no HTML content from %s", rawURL)
	}
	return nil, lastErr
}

// FetchPageHTML fetches rawURL and returns the <html> root as a goquery.Selection.
// Known sites use their fixed domain; unknown sites use no domain restriction
// because they may redirect to CDN or different domains.
func (nc *NovelCrawler) FetchPageHTML(rawURL string) (*goquery.Selection, error) {
	site := nc.identifySite(rawURL)
	domain := siteDomains[site] // empty string for unknown sites → no restriction
	return nc.fetchHTML(rawURL, domain)
}

// Jar returns the shared cookie jar used by all collectors.
func (nc *NovelCrawler) Jar() http.CookieJar { return nc.jar }

// CrawlNovel 爬取小说详情并异步爬取全部章节
func (nc *NovelCrawler) CrawlNovel(ctx context.Context, rawURL string) (*model.ReferenceNovel, error) {
	site := nc.identifySite(rawURL)
	parser, ok := nc.parsers[site]
	if !ok {
		return nil, fmt.Errorf("unsupported site: %s", site)
	}
	domain := siteDomains[site]

	root, err := nc.fetchHTML(rawURL, domain)
	if err != nil {
		return nil, err
	}

	detail, err := parser.ParseNovelDetail(root, rawURL)
	if err != nil {
		return nil, err
	}

	novel := &model.ReferenceNovel{
		Title:      detail.Title,
		Author:     detail.Author,
		SourceURL:  rawURL,
		SourceSite: site,
		Genre:      detail.Genre,
		Status:     "crawling",
	}
	if err := nc.db.Create(novel).Error; err != nil {
		return nil, err
	}

	go nc.crawlChaptersAsync(context.Background(), novel.ID, site, parser, rawURL)
	return novel, nil
}

// crawlChaptersAsync 异步爬取所有章节，使用 FetchChapterStream 并发下载
func (nc *NovelCrawler) crawlChaptersAsync(ctx context.Context, novelID uint, site string, parser NovelParser, baseURL string) {
	nc.stats = CrawlStats{StartedAt: time.Now()}

	domain := siteDomains[site]

	// Fetch chapter list — prefer Ajax API if available, fall back to HTML parse
	chapters, err := nc.fetchChapterList(ctx, parser, domain, baseURL)
	if err != nil || len(chapters) == 0 {
		nc.updateNovelStatus(novelID, "failed")
		return
	}

	// Filter VIP chapters if configured
	if nc.config.SkipVIPChaps {
		var filtered []*ChapterInfo
		for _, ch := range chapters {
			if !ch.IsVip {
				filtered = append(filtered, ch)
			}
		}
		chapters = filtered
	}

	chaptersSaved := 0
	for result := range nc.FetchChapterStream(ctx, chapters, parser) {
		if result.Err != nil {
			continue
		}
		if result.Content == nil || result.Content.Content == "" {
			continue
		}

		refChapter := &model.ReferenceChapter{
			NovelID:   novelID,
			ChapterNo: result.Index + 1,
			Title:     result.Content.Title,
			Content:   result.Content.Content,
		}
		if createErr := nc.db.Create(refChapter).Error; createErr == nil {
			chaptersSaved++
			nc.db.Model(&model.ReferenceNovel{}).Where("id = ?", novelID).
				Updates(map[string]interface{}{"total_chapters": chaptersSaved})
		}
	}

	nc.updateNovelStatus(novelID, "completed")
	nc.analyzeNovel(novelID)
}

// fetchChapterList tries the ChapterListFetcher Ajax API first, then falls back to HTML parsing.
func (nc *NovelCrawler) fetchChapterList(ctx context.Context, parser NovelParser, domain, baseURL string) ([]*ChapterInfo, error) {
	if fetcher, ok := parser.(ChapterListFetcher); ok {
		chapters, err := fetcher.FetchChapterList(ctx, nc.jar, baseURL)
		if err == nil && len(chapters) > 0 {
			return chapters, nil
		}
	}
	root, err := nc.fetchHTML(baseURL, domain)
	if err != nil {
		return nil, err
	}
	return parser.ParseChapterList(root)
}

// updateNovelStatus 更新小说状态
func (nc *NovelCrawler) updateNovelStatus(novelID uint, status string) {
	nc.db.Model(&model.ReferenceNovel{}).Where("id = ?", novelID).Update("status", status)
}

// analyzeNovel 触发小说分析（实际在 service 层完成）
func (nc *NovelCrawler) analyzeNovel(_ uint) {}

// identifySite 从 URL 识别站点
func (nc *NovelCrawler) identifySite(rawURL string) string {
	switch {
	case strings.Contains(rawURL, "qidian.com"):
		return "qidian"
	case strings.Contains(rawURL, "jjwxc.net"):
		return "jjwxc"
	case strings.Contains(rawURL, "zongheng.com"):
		return "zongheng"
	case strings.Contains(rawURL, "qimao.com"):
		return "qimao"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// QidianParser 起点中文网解析器
// ---------------------------------------------------------------------------

type QidianParser struct{}

func NewQidianParser() *QidianParser { return &QidianParser{} }

func (p *QidianParser) GetSiteName() string { return "起点中文网" }

func (p *QidianParser) extractBookID(bookURL string) string {
	re := regexp.MustCompile(`/book/(\d+)`)
	if m := re.FindStringSubmatch(bookURL); len(m) > 1 {
		return m[1]
	}
	return ""
}

func (p *QidianParser) ParseNovelList(root *goquery.Selection) ([]*NovelInfo, error) {
	var novels []*NovelInfo
	root.Find(".book-list li, .work-list li").Each(func(i int, s *goquery.Selection) {
		title := s.Find(".book-name, .title").Text()
		author := s.Find(".author").Text()
		href, _ := s.Find("a").Attr("href")
		novels = append(novels, &NovelInfo{
			Title:  strings.TrimSpace(title),
			Author: strings.TrimSpace(author),
			URL:    href,
		})
	})
	return novels, nil
}

// ParseNovelDetail 解析书籍详情页（内嵌 JSON 优先，HTML 选择器回退）
func (p *QidianParser) ParseNovelDetail(root *goquery.Selection, bookURL string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	root.Find("script").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		src := s.Text()
		if !strings.Contains(src, "bookName") && !strings.Contains(src, "bookInfo") {
			return true
		}
		if m := regexp.MustCompile(`"bookName"\s*:\s*"([^"]+)"`).FindStringSubmatch(src); len(m) > 1 {
			detail.Title = m[1]
		}
		if m := regexp.MustCompile(`"authorName"\s*:\s*"([^"]+)"`).FindStringSubmatch(src); len(m) > 1 {
			detail.Author = m[1]
		}
		if m := regexp.MustCompile(`"description"\s*:\s*"([^"]+)"`).FindStringSubmatch(src); len(m) > 1 {
			detail.Description = strings.ReplaceAll(m[1], `\n`, "\n")
		}
		if m := regexp.MustCompile(`"categoryName"\s*:\s*"([^"]+)"`).FindStringSubmatch(src); len(m) > 1 {
			detail.Genre = m[1]
		}
		return detail.Title == ""
	})

	if detail.Title == "" {
		for _, sel := range []string{"h1.book-title", "h1.book-info-title", ".book-info h1", "h1"} {
			if t := strings.TrimSpace(root.Find(sel).First().Text()); t != "" {
				detail.Title = t
				break
			}
		}
	}
	if detail.Author == "" {
		for _, sel := range []string{"a.writer-name", ".writer-info .name", ".author-name", ".book-author a"} {
			if a := strings.TrimSpace(root.Find(sel).First().Text()); a != "" {
				detail.Author = a
				break
			}
		}
	}
	if detail.Description == "" {
		for _, sel := range []string{"p.intro", ".book-intro p", ".book-intro", ".intro"} {
			if d := strings.TrimSpace(root.Find(sel).First().Text()); d != "" {
				detail.Description = d
				break
			}
		}
	}
	if detail.Genre == "" {
		detail.Genre = strings.TrimSpace(root.Find(".book-cat a, .tag-list a, .book-label a").First().Text())
	}
	detail.CoverURL, _ = root.Find(".book-img img, .book-cover img, .cover img").First().Attr("src")
	if s := strings.ToLower(root.Find(".book-label, .book-status, .status").First().Text()); strings.Contains(s, "完") {
		detail.Status = "completed"
	} else {
		detail.Status = "ongoing"
	}
	return detail, nil
}

// ParseChapterList HTML 回退（FetchChapterList 失败时使用）
func (p *QidianParser) ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo
	root.Find(".chapter-list li a, .volume-chapter a, .chapter-wrap a").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		href, _ := s.Attr("href")
		if title == "" || href == "" {
			return
		}
		if strings.HasPrefix(href, "//") {
			href = "https:" + href
		} else if strings.HasPrefix(href, "/") {
			href = "https://www.qidian.com" + href
		}
		chapters = append(chapters, &ChapterInfo{Title: title, URL: href, ChapterNo: i + 1})
	})
	return chapters, nil
}

// ParseChapter 解析起点章节正文
func (p *QidianParser) ParseChapter(root *goquery.Selection) (*ChapterContent, error) {
	content := &ChapterContent{}
	for _, sel := range []string{".chapter-name", ".j_chapterName", "h1.chapter-name", "h1"} {
		if t := strings.TrimSpace(root.Find(sel).First().Text()); t != "" {
			content.Title = t
			break
		}
	}
	var paragraphs []string
	contentSel := root.Find("#j_readContent p, .read-content p, .chapter-content p, #j_chapterBox p")
	if contentSel.Length() > 0 {
		contentSel.Each(func(_ int, s *goquery.Selection) {
			if t := strings.TrimSpace(s.Text()); t != "" {
				paragraphs = append(paragraphs, t)
			}
		})
		content.Content = strings.Join(paragraphs, "\n\n")
	} else {
		content.Content = cleanText(root.Find("#j_readContent, .read-content, .chapter-content, #j_chapterBox").First().Text())
	}
	return content, nil
}

// qidianCategoryResp Ajax 章节目录响应结构
type qidianCategoryResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Vs []struct {
			VName string `json:"vName"`
			Cs    []struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				IsVip int    `json:"isVip"`
			} `json:"cs"`
		} `json:"vs"`
	} `json:"data"`
}

// FetchChapterList 通过起点 Ajax API 获取完整章节列表（实现 ChapterListFetcher 接口）
// Cookies（含 _csrfToken）由共享 jar 自动携带。
func (p *QidianParser) FetchChapterList(_ context.Context, jar http.CookieJar, bookURL string) ([]*ChapterInfo, error) {
	bookID := p.extractBookID(bookURL)
	if bookID == "" {
		return nil, fmt.Errorf("cannot extract book ID from URL: %s", bookURL)
	}

	// 从 jar 读取 _csrfToken（访问详情页后自动存入）
	var csrfToken string
	if u, err := url.Parse("https://www.qidian.com"); err == nil {
		for _, c := range jar.Cookies(u) {
			if c.Name == "_csrfToken" {
				csrfToken = c.Value
				break
			}
		}
	}

	apiURL := fmt.Sprintf("https://www.qidian.com/ajax/book/category?bookId=%s&_csrfToken=%s", bookID, csrfToken)

	// 用独立的 colly collector 发送 XHR 请求；共享 jar 自动携带 cookie
	col := colly.NewCollector(colly.AllowedDomains("www.qidian.com"))
	col.SetCookieJar(jar)
	col.SetRequestTimeout(30 * time.Second)
	col.OnRequest(func(r *colly.Request) {
		r.Headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		r.Headers.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		r.Headers.Set("X-Requested-With", "XMLHttpRequest")
		r.Headers.Set("Referer", bookURL)
	})

	var chapters []*ChapterInfo
	var fetchErr error

	col.OnResponse(func(r *colly.Response) {
		var resp qidianCategoryResp
		if err := json.Unmarshal(r.Body, &resp); err != nil {
			fetchErr = fmt.Errorf("parse chapter list JSON failed: %w (body: %.200s)", err, string(r.Body))
			return
		}
		if resp.Code != 0 {
			fetchErr = fmt.Errorf("chapter list API error code=%d msg=%s", resp.Code, resp.Msg)
			return
		}
		seq := 1
		for _, vol := range resp.Data.Vs {
			for _, ch := range vol.Cs {
				chapters = append(chapters, &ChapterInfo{
					Title:     ch.Name,
					URL:       fmt.Sprintf("https://www.qidian.com/chapter/%s/%d/", bookID, ch.ID),
					ChapterNo: seq,
					IsVip:     ch.IsVip != 0,
				})
				seq++
			}
		}
	})
	col.OnError(func(_ *colly.Response, err error) {
		fetchErr = fmt.Errorf("chapter list API request failed: %w", err)
	})

	if err := col.Visit(apiURL); err != nil {
		return nil, fmt.Errorf("chapter list API request failed: %w", err)
	}
	if fetchErr != nil {
		return nil, fetchErr
	}
	if len(chapters) == 0 {
		return nil, fmt.Errorf("chapter list API returned 0 chapters (bookId=%s)", bookID)
	}
	return chapters, nil
}

// ---------------------------------------------------------------------------
// JjwxcParser 晋江文学城解析器
// ---------------------------------------------------------------------------

type JjwxcParser struct{}

func NewJjwxcParser() *JjwxcParser { return &JjwxcParser{} }

func (p *JjwxcParser) GetSiteName() string { return "晋江文学城" }

func (p *JjwxcParser) ParseNovelList(_ *goquery.Selection) ([]*NovelInfo, error) {
	return nil, nil
}

func (p *JjwxcParser) ParseNovelDetail(root *goquery.Selection, bookURL string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	// Title: prefer itemprop span, fallback to og:title meta
	if t := strings.TrimSpace(root.Find(`h2 span[itemprop="name"]`).First().Text()); t != "" {
		detail.Title = t
	} else if t, exists := root.Find(`meta[property="og:title"]`).First().Attr("content"); exists {
		detail.Title = strings.TrimSpace(t)
	}

	// Author
	if a := strings.TrimSpace(root.Find(".author_hiddenbio a").First().Text()); a != "" {
		detail.Author = a
	} else if a := strings.TrimSpace(root.Find(`[itemprop="author"]`).First().Text()); a != "" {
		detail.Author = a
	}

	// Description
	detail.Description = strings.TrimSpace(root.Find("#novelintro").First().Text())

	// Cover
	detail.CoverURL, _ = root.Find(".bookimage img").First().Attr("src")

	if detail.Title == "" {
		return nil, fmt.Errorf("jjwxc: could not parse novel title from %s", bookURL)
	}
	return detail, nil
}

func (p *JjwxcParser) ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo
	root.Find("table.css_info td.a2 a").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		href, _ := s.Attr("href")
		if title == "" || href == "" {
			return
		}
		if strings.HasPrefix(href, "/") {
			href = "https://www.jjwxc.net" + href
		} else if !strings.HasPrefix(href, "http") {
			href = "https://www.jjwxc.net/" + href
		}
		chapters = append(chapters, &ChapterInfo{Title: title, URL: href, ChapterNo: i + 1})
	})
	return chapters, nil
}

func (p *JjwxcParser) ParseChapter(root *goquery.Selection) (*ChapterContent, error) {
	content := &ChapterContent{}

	// Title: h2 inside .readContent or h2.chapter-title
	if t := strings.TrimSpace(root.Find(".readContent h2").First().Text()); t != "" {
		content.Title = t
	} else if t := strings.TrimSpace(root.Find("h2.chapter-title").First().Text()); t != "" {
		content.Title = t
	}

	// Content: div#novelcontent paragraphs
	var paragraphs []string
	root.Find("div#novelcontent p").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			paragraphs = append(paragraphs, t)
		}
	})
	content.Content = strings.Join(paragraphs, "\n\n")

	return content, nil
}

// ---------------------------------------------------------------------------
// ZonghengParser 纵横中文网解析器
// ---------------------------------------------------------------------------

type ZonghengParser struct{}

func NewZonghengParser() *ZonghengParser { return &ZonghengParser{} }

func (p *ZonghengParser) GetSiteName() string { return "纵横中文网" }

func (p *ZonghengParser) ParseNovelList(_ *goquery.Selection) ([]*NovelInfo, error) {
	return nil, nil
}

func (p *ZonghengParser) ParseNovelDetail(root *goquery.Selection, _ string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	// Title
	detail.Title = strings.TrimSpace(root.Find("h1.bookname, h1.book-name, .booknav h1").First().Text())

	// Author
	detail.Author = strings.TrimSpace(root.Find(`.bookinfo a[href*="user"]`).First().Text())

	// Description
	detail.Description = strings.TrimSpace(root.Find("#intro-all p, .intro p").First().Text())

	// Cover
	detail.CoverURL, _ = root.Find(".bookcover img").First().Attr("src")

	return detail, nil
}

func (p *ZonghengParser) ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo
	root.Find(".chapter-list li a, .chapterlist li a").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		href, _ := s.Attr("href")
		if title == "" || href == "" {
			return
		}
		if strings.HasPrefix(href, "/") {
			href = "https://book.zongheng.com" + href
		}
		chapters = append(chapters, &ChapterInfo{Title: title, URL: href, ChapterNo: i + 1})
	})
	return chapters, nil
}

func (p *ZonghengParser) ParseChapter(root *goquery.Selection) (*ChapterContent, error) {
	content := &ChapterContent{}

	// Title
	content.Title = strings.TrimSpace(root.Find("h1.ctitle, .chapter-title h1").First().Text())

	// Content
	var paragraphs []string
	root.Find(".readerList p, #content p").Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			paragraphs = append(paragraphs, t)
		}
	})
	content.Content = strings.Join(paragraphs, "\n\n")

	return content, nil
}

// FetchChapterList fetches the chapter list via zongheng's chapter page.
// Implements ChapterListFetcher.
func (p *ZonghengParser) FetchChapterList(_ context.Context, jar http.CookieJar, bookURL string) ([]*ChapterInfo, error) {
	// Extract book ID from URL: /book/(\d+)\.html
	re := regexp.MustCompile(`/book/(\d+)\.html`)
	m := re.FindStringSubmatch(bookURL)
	if len(m) < 2 {
		return nil, fmt.Errorf("zongheng: cannot extract book ID from URL: %s", bookURL)
	}
	bookID := m[1]

	chapterListURL := fmt.Sprintf("https://book.zongheng.com/showchapter/%s.html", bookID)

	col := colly.NewCollector(colly.AllowedDomains("book.zongheng.com"))
	col.SetCookieJar(jar)
	col.SetRequestTimeout(30 * time.Second)
	col.OnRequest(func(r *colly.Request) {
		r.Headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		r.Headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		r.Headers.Set("Referer", bookURL)
	})

	var chapters []*ChapterInfo
	var fetchErr error

	col.OnHTML("html", func(e *colly.HTMLElement) {
		e.DOM.Find(".chapter-list li a, .chapterlist li a").Each(func(i int, s *goquery.Selection) {
			title := strings.TrimSpace(s.Text())
			href, _ := s.Attr("href")
			if title == "" || href == "" {
				return
			}
			if strings.HasPrefix(href, "/") {
				href = "https://book.zongheng.com" + href
			}
			chapters = append(chapters, &ChapterInfo{Title: title, URL: href, ChapterNo: i + 1})
		})
	})

	col.OnError(func(_ *colly.Response, err error) {
		fetchErr = fmt.Errorf("zongheng: chapter list fetch failed: %w", err)
	})

	if err := col.Visit(chapterListURL); err != nil {
		return nil, fmt.Errorf("zongheng: chapter list visit failed: %w", err)
	}
	if fetchErr != nil {
		return nil, fetchErr
	}
	return chapters, nil
}

// ---------------------------------------------------------------------------
// QimaoParser 七猫小说解析器
// ---------------------------------------------------------------------------

type QimaoParser struct{}

func NewQimaoParser() *QimaoParser { return &QimaoParser{} }

func (p *QimaoParser) GetSiteName() string { return "七猫小说" }

func (p *QimaoParser) ParseNovelList(root *goquery.Selection) ([]*NovelInfo, error) {
	var novels []*NovelInfo
	root.Find(".book-item, .novel-item, .book-list li").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Find(".book-name, .title, h3").First().Text())
		author := strings.TrimSpace(s.Find(".author, .writer").First().Text())
		u, _ := s.Find("a").First().Attr("href")
		cover, _ := s.Find("img").First().Attr("src")
		if title != "" {
			novels = append(novels, &NovelInfo{Title: title, Author: author, URL: u, CoverURL: cover})
		}
	})
	return novels, nil
}

func (p *QimaoParser) ParseNovelDetail(root *goquery.Selection, _ string) (*NovelDetail, error) {
	detail := &NovelDetail{}
	detail.Title = strings.TrimSpace(root.Find("h1.book-title, h1.name, .detail-title h1").First().Text())
	if detail.Title == "" {
		detail.Title = strings.TrimSpace(root.Find("h1").First().Text())
	}
	detail.Author = strings.TrimSpace(root.Find(".author-name, .author a, .writer").First().Text())
	detail.Description = strings.TrimSpace(root.Find(".book-intro, .desc, .intro, .summary").First().Text())
	detail.CoverURL, _ = root.Find(".book-cover img, .cover img").First().Attr("src")
	root.Find(".tag-item, .book-tag, .label").Each(func(i int, s *goquery.Selection) {
		if tag := strings.TrimSpace(s.Text()); tag != "" {
			detail.Tags = append(detail.Tags, tag)
		}
	})
	if len(detail.Tags) > 0 {
		detail.Genre = detail.Tags[0]
	}
	if s := strings.TrimSpace(root.Find(".status, .book-status").First().Text()); strings.Contains(s, "完") {
		detail.Status = "completed"
	} else {
		detail.Status = "ongoing"
	}
	detail.TotalChapters = extractNumber(root.Find(".chapter-count, .total-chapter, .chapter-num").First().Text())
	return detail, nil
}

func (p *QimaoParser) ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo
	root.Find(".chapter-list a, .catalog-list a, .chapter-item a, ul.list a").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		chURL, _ := s.Attr("href")
		if title == "" || chURL == "" {
			return
		}
		if strings.HasPrefix(chURL, "/") {
			chURL = "https://www.qimao.com" + chURL
		}
		chapters = append(chapters, &ChapterInfo{Title: title, URL: chURL, ChapterNo: i + 1})
	})
	return chapters, nil
}

func (p *QimaoParser) ParseChapter(root *goquery.Selection) (*ChapterContent, error) {
	content := &ChapterContent{}
	content.Title = strings.TrimSpace(root.Find(".chapter-title, .read-title, h1.title").First().Text())
	contentSel := root.Find("#chapter-content, .chapter-content, .read-content, .content")
	if contentSel.Length() == 0 {
		contentSel = root.Find("article")
	}
	content.Content = cleanText(contentSel.First().Text())
	content.PrevURL, _ = root.Find(".prev-chapter a, .btn-prev, a.prev").First().Attr("href")
	content.NextURL, _ = root.Find(".next-chapter a, .btn-next, a.next").First().Attr("href")
	if strings.HasPrefix(content.PrevURL, "/") {
		content.PrevURL = "https://www.qimao.com" + content.PrevURL
	}
	if strings.HasPrefix(content.NextURL, "/") {
		content.NextURL = "https://www.qimao.com" + content.NextURL
	}
	return content, nil
}

// ---------------------------------------------------------------------------
// HongxiuParser 红袖添香解析器（hongxiu.com）
// 书目 URL 格式：https://www.hongxiu.com/book/<bookID>
// ---------------------------------------------------------------------------

type HongxiuParser struct{}

func NewHongxiuParser() *HongxiuParser { return &HongxiuParser{} }

func (p *HongxiuParser) GetSiteName() string { return "红袖添香" }

func (p *HongxiuParser) extractBookID(bookURL string) string {
	re := regexp.MustCompile(`/book/(\d+)`)
	if m := re.FindStringSubmatch(bookURL); len(m) > 1 {
		return m[1]
	}
	return ""
}

func (p *HongxiuParser) ParseNovelList(root *goquery.Selection) ([]*NovelInfo, error) {
	var novels []*NovelInfo
	root.Find(".book-list li, .works-list li").Each(func(_ int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Find(".book-name, .title, h3").First().Text())
		author := strings.TrimSpace(s.Find(".author, .writer").First().Text())
		href, _ := s.Find("a").First().Attr("href")
		if title != "" {
			novels = append(novels, &NovelInfo{Title: title, Author: author, URL: href})
		}
	})
	return novels, nil
}

func (p *HongxiuParser) ParseNovelDetail(root *goquery.Selection, bookURL string) (*NovelDetail, error) {
	detail := &NovelDetail{}
	detail.Title = strings.TrimSpace(root.Find("h1.book-title, .book-name h1, .intro-name").First().Text())
	if detail.Title == "" {
		detail.Title = strings.TrimSpace(root.Find("h1").First().Text())
	}
	detail.Author = strings.TrimSpace(root.Find(".author-name, .book-author a").First().Text())
	detail.Description = strings.TrimSpace(root.Find(".book-intro, .intro-content, .desc").First().Text())
	detail.Genre = strings.TrimSpace(root.Find(".book-type a, .category a").First().Text())
	if detail.Title == "" {
		return nil, fmt.Errorf("hongxiu: 无法解析书名，请确认 URL 为书目页（/book/<ID>），不是搜索页")
	}
	return detail, nil
}

func (p *HongxiuParser) ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo
	root.Find(".chapter-list li a, .catalog-list li a, #chapterList li a").Each(func(_ int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		href, exists := s.Attr("href")
		if !exists || title == "" {
			return
		}
		if strings.HasPrefix(href, "/") {
			href = "https://www.hongxiu.com" + href
		}
		chapters = append(chapters, &ChapterInfo{Title: title, URL: href})
	})
	return chapters, nil
}

func (p *HongxiuParser) ParseChapter(root *goquery.Selection) (*ChapterContent, error) {
	content := &ChapterContent{}
	content.Title = strings.TrimSpace(root.Find(".chapter-title, .read-title, h1").First().Text())
	contentSel := root.Find("#readContent, .chapter-content, .read-content, .content-text")
	if contentSel.Length() == 0 {
		contentSel = root.Find("article")
	}
	content.Content = cleanText(contentSel.First().Text())
	content.PrevURL, _ = root.Find(".btn-prev a, .pre-chapter a, a.prev").First().Attr("href")
	content.NextURL, _ = root.Find(".btn-next a, .next-chapter a, a.next").First().Attr("href")
	if strings.HasPrefix(content.PrevURL, "/") {
		content.PrevURL = "https://www.hongxiu.com" + content.PrevURL
	}
	if strings.HasPrefix(content.NextURL, "/") {
		content.NextURL = "https://www.hongxiu.com" + content.NextURL
	}
	return content, nil
}

// ---------------------------------------------------------------------------
// GenericParser 通用解析器
//
// 无需为每个站点单独实现选择器。
// - ParseNovelDetail  : meta/OG 标签 + h1 降级
// - ParseChapterList  : 基于 URL 前缀匹配 + 链接密度自动识别目录容器
// - ParseChapter      : 文本密度算法找正文节点，提取 <p> 段落
// ---------------------------------------------------------------------------

type GenericParser struct {
	bookURL string // 书目页 URL，用于 ParseChapterList 中计算路径前缀
}

func NewGenericParser() *GenericParser { return &GenericParser{} }

// NewGenericParserWithURL 创建带书目 URL 的通用解析器，ParseChapterList 用于过滤章节链接。
func NewGenericParserWithURL(bookURL string) *GenericParser { return &GenericParser{bookURL: bookURL} }

func (p *GenericParser) GetSiteName() string { return "通用" }

func (p *GenericParser) ParseNovelList(_ *goquery.Selection) ([]*NovelInfo, error) {
	return nil, nil
}

func (p *GenericParser) ParseNovelDetail(root *goquery.Selection, bookURL string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	// 标题：og:title > title 标签 > h1
	if t, ok := root.Find(`meta[property="og:title"]`).Attr("content"); ok {
		detail.Title = strings.TrimSpace(t)
	}
	if detail.Title == "" {
		detail.Title = strings.TrimSpace(root.Find("title").First().Text())
		// 去掉站点后缀（如 "书名 - 起点小说"）
		if idx := strings.LastIndex(detail.Title, " - "); idx > 0 {
			detail.Title = strings.TrimSpace(detail.Title[:idx])
		}
		if idx := strings.LastIndex(detail.Title, "_"); idx > 0 {
			detail.Title = strings.TrimSpace(detail.Title[:idx])
		}
	}
	if detail.Title == "" {
		detail.Title = strings.TrimSpace(root.Find("h1").First().Text())
	}

	// 简介：og:description > meta description
	if d, ok := root.Find(`meta[property="og:description"]`).Attr("content"); ok && d != "" {
		detail.Description = strings.TrimSpace(d)
	}
	if detail.Description == "" {
		if d, ok := root.Find(`meta[name="description"]`).Attr("content"); ok {
			detail.Description = strings.TrimSpace(d)
		}
	}

	// 作者：常见 class
	for _, sel := range []string{".author a", ".writer a", `[itemprop="author"]`, ".book-author"} {
		if a := strings.TrimSpace(root.Find(sel).First().Text()); a != "" {
			detail.Author = a
			break
		}
	}

	if detail.Title == "" {
		return nil, fmt.Errorf("generic: 无法从页面提取书名（URL: %s）", bookURL)
	}
	return detail, nil
}

// ParseChapterList 基于「URL 公共前缀 + 允许域名」自动识别章节目录链接。
func (p *GenericParser) ParseChapterList(root *goquery.Selection) ([]*ChapterInfo, error) {
	var baseHost, basePath string
	var bookParsed *url.URL
	if p.bookURL != "" {
		if u, err := url.Parse(p.bookURL); err == nil {
			bookParsed = u
			baseHost = u.Host
			basePath = strings.TrimRight(u.Path, "/")
		}
	}

	// 允许的域名集合：书目页域名 + 页面实际域名（重定向后 og:url / canonical 中的域）
	// 这样当书目页被重定向到 CDN 域（如 qindi.935666.xyz）时，该域的链接也能被识别
	allowedHosts := map[string]struct{}{}
	if baseHost != "" {
		allowedHosts[baseHost] = struct{}{}
	}
	for _, sel := range []string{`meta[property="og:url"]`, `link[rel="canonical"]`} {
		if val, ok := root.Find(sel).Attr("content"); !ok {
			if val, ok = root.Find(sel).Attr("href"); ok {
				if u, err := url.Parse(val); err == nil && u.Host != "" {
					allowedHosts[u.Host] = struct{}{}
					// 如果实际域名与书目域名不同，用实际路径更新 basePath
					if u.Host != baseHost && basePath == "" {
						basePath = strings.TrimRight(u.Path, "/")
					}
				}
			}
		} else {
			if u, err := url.Parse(val); err == nil && u.Host != "" {
				allowedHosts[u.Host] = struct{}{}
				if u.Host != baseHost && basePath == "" {
					basePath = strings.TrimRight(u.Path, "/")
				}
			}
		}
	}

	type linkEntry struct {
		title string
		href  string
	}

	// 收集候选链接：允许域 + 路径在书目页之下 + 标题长度 2-40
	navKeywords := []string{"prev", "next", "上一", "下一", "首页", "末页", "登录", "注册", "home", "login", "register"}
	var candidates []linkEntry

	root.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		title := strings.TrimSpace(s.Text())
		if href == "" || title == "" {
			return
		}
		titleRunes := []rune(title)
		if len(titleRunes) < 2 || len(titleRunes) > 40 {
			return
		}
		titleLower := strings.ToLower(title)
		for _, kw := range navKeywords {
			if strings.Contains(titleLower, kw) {
				return
			}
		}
		// 解析链接 URL
		linkURL, err := url.Parse(href)
		if err != nil {
			return
		}
		// 补全相对 URL：使用书目页 URL 作为基准正确解析（含相对路径如 1/ 或 ../chapter/1）
		if !linkURL.IsAbs() {
			if bookParsed == nil {
				return
			}
			linkURL = bookParsed.ResolveReference(linkURL)
		}
		// 域名检查：仅在允许集合非空时过滤（允许书目域 + 重定向后实际域）
		if len(allowedHosts) > 0 {
			if _, ok := allowedHosts[linkURL.Host]; !ok {
				return
			}
		}
		linkPath := strings.TrimRight(linkURL.Path, "/")
		// 链接路径必须比书目页更深
		if basePath != "" && !strings.HasPrefix(linkPath, basePath+"/") {
			return
		}
		// 排除 .js/.css 等资源
		for _, ext := range []string{".js", ".css", ".png", ".jpg", ".gif", ".ico"} {
			if strings.HasSuffix(linkPath, ext) {
				return
			}
		}
		candidates = append(candidates, linkEntry{title: title, href: linkURL.String()})
	})

	if len(candidates) == 0 {
		return nil, fmt.Errorf("generic: 未找到章节链接（bookURL=%s）", p.bookURL)
	}

	// 去重（保留首次出现的顺序）
	seen := make(map[string]struct{}, len(candidates))
	chapters := make([]*ChapterInfo, 0, len(candidates))
	for i, c := range candidates {
		if _, dup := seen[c.href]; dup {
			continue
		}
		seen[c.href] = struct{}{}
		chapters = append(chapters, &ChapterInfo{
			Title:     c.title,
			URL:       c.href,
			ChapterNo: i + 1,
		})
	}

	if len(chapters) == 0 {
		return nil, fmt.Errorf("generic: 章节列表为空（bookURL=%s）", p.bookURL)
	}
	// 重新按顺序编号
	for i := range chapters {
		chapters[i].ChapterNo = i + 1
	}
	return chapters, nil
}

// ParseChapter 使用 go-readability（Mozilla Readability 算法）提取正文。
// 直接传入 goquery DOM 节点，无需二次序列化。
func (p *GenericParser) ParseChapter(root *goquery.Selection) (*ChapterContent, error) {
	content := &ChapterContent{}

	// 构造一个章节 URL 供 readability 解析相对路径（用书目 URL 的 origin 即可）
	pageURL, _ := url.Parse(p.bookURL)
	if pageURL == nil {
		pageURL = &url.URL{Scheme: "https", Host: "unknown"}
	}

	// readability.FromDocument 需要 DocumentNode；colly OnHTML("html") 传入的是 ElementNode，
	// 取其 Parent 获得正确的 DocumentNode。
	htmlNode := root.Get(0)
	if htmlNode == nil {
		return content, nil
	}
	if htmlNode.Type == html.ElementNode && htmlNode.Parent != nil && htmlNode.Parent.Type == html.DocumentNode {
		htmlNode = htmlNode.Parent
	}
	article, err := readability.FromDocument(htmlNode, pageURL)
	if err == nil && len(strings.TrimSpace(article.TextContent)) > 50 {
		// 用 readability 结果
		if article.Title != "" {
			content.Title = article.Title
		}
		content.Content = cleanReadabilityText(article.TextContent)
		return content, nil
	}

	// readability 失败时降级：找 <h1> 标题 + 常见正文选择器提取 <p> 段落
	content.Title = strings.TrimSpace(root.Find("h1").First().Text())
	if content.Title == "" {
		if t, ok := root.Find(`meta[property="og:title"]`).Attr("content"); ok {
			content.Title = strings.TrimSpace(t)
		}
	}
	for _, sel := range []string{
		"article", "main", "#content", ".content",
		"#chapter-content", ".chapter-content", "#novelcontent",
		"#readContent", ".read-content", "#chapterContent",
	} {
		node := root.Find(sel).First()
		if node.Length() == 0 {
			continue
		}
		var paragraphs []string
		node.Find("p").Each(func(_ int, s *goquery.Selection) {
			if t := strings.TrimSpace(s.Text()); len([]rune(t)) >= 5 {
				paragraphs = append(paragraphs, t)
			}
		})
		if len(paragraphs) > 0 {
			content.Content = strings.Join(paragraphs, "\n\n")
			return content, nil
		}
		if t := strings.TrimSpace(node.Text()); len([]rune(t)) > 100 {
			content.Content = cleanText(t)
			return content, nil
		}
	}

	// 最后兜底：从 <body> 提取所有长段落（readability 和 CSS 选择器均失败时使用）
	var bodyParagraphs []string
	root.Find("body p").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		// 过滤极短段落（导航、提示、广告）
		if len([]rune(t)) >= 20 {
			bodyParagraphs = append(bodyParagraphs, t)
		}
	})
	if len(bodyParagraphs) >= 3 {
		content.Content = strings.Join(bodyParagraphs, "\n\n")
	}
	return content, nil
}

// cleanReadabilityText 清理 readability 返回的 TextContent（去多余空行和空白）。
func cleanReadabilityText(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n\n")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractNumber(text string) int {
	matches := regexp.MustCompile(`\d+`).FindAllString(text, -1)
	if len(matches) > 0 {
		var n int
		fmt.Sscanf(matches[len(matches)-1], "%d", &n)
		return n
	}
	return 0
}

func cleanText(text string) string {
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, "\n")
	return strings.TrimSpace(text)
}

// ---------------------------------------------------------------------------
// BatchCrawler 批量爬虫
// ---------------------------------------------------------------------------

type BatchCrawler struct {
	crawler *NovelCrawler
	workers int
	queue   chan string
	results chan *CrawlResult
	wg      sync.WaitGroup
}

type CrawlResult struct {
	URL   string
	Novel *model.ReferenceNovel
	Error error
}

func NewBatchCrawler(db *gorm.DB, workers int) *BatchCrawler {
	return &BatchCrawler{
		crawler: NewNovelCrawler(db),
		workers: workers,
		queue:   make(chan string, 1000),
		results: make(chan *CrawlResult, 100),
	}
}

func (b *BatchCrawler) Start() {
	for i := 0; i < b.workers; i++ {
		b.wg.Add(1)
		go b.worker()
	}
}

func (b *BatchCrawler) worker() {
	defer b.wg.Done()
	for rawURL := range b.queue {
		novel, err := b.crawler.CrawlNovel(context.Background(), rawURL)
		b.results <- &CrawlResult{URL: rawURL, Novel: novel, Error: err}
	}
}

func (b *BatchCrawler) Add(rawURL string) { b.queue <- rawURL }

func (b *BatchCrawler) Wait() {
	close(b.queue)
	b.wg.Wait()
	close(b.results)
}

func (b *BatchCrawler) Results() <-chan *CrawlResult { return b.results }

// ---------------------------------------------------------------------------
// Per-domain rate limiter (simple token bucket, no external deps)
// ---------------------------------------------------------------------------

// domainLimiter is a simple per-domain rate limiter using a last-request timestamp.
type domainLimiter struct {
	mu       sync.Mutex
	lastCall time.Time
	interval time.Duration // minimum interval between requests
}

// Wait blocks until a request to this domain is permitted.
func (l *domainLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	next := l.lastCall.Add(l.interval)
	if wait := next.Sub(now); wait > 0 {
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		l.mu.Lock()
	}
	l.lastCall = time.Now()
	l.mu.Unlock()
	return nil
}

var crawlLimiters sync.Map // domain -> *domainLimiter

// getCrawlLimiter returns (or creates) a per-domain rate limiter for HTTPClient.Get.
// Default: 1 request per second per domain.
func getCrawlLimiter(domain string) *domainLimiter {
	if v, ok := crawlLimiters.Load(domain); ok {
		return v.(*domainLimiter)
	}
	limiter := &domainLimiter{interval: time.Second}
	actual, _ := crawlLimiters.LoadOrStore(domain, limiter)
	return actual.(*domainLimiter)
}

// ---------------------------------------------------------------------------
// HTTPClient 简单 HTTP 客户端，供 import_service 等调用
// ---------------------------------------------------------------------------

// HTTPClient wraps a net/http.Client with a shared CookieJar and standard browser headers.
type HTTPClient struct {
	client *http.Client
}

// NewHTTPClient returns an HTTPClient with a 30-second timeout and shared cookie jar.
func NewHTTPClient() *HTTPClient {
	jar, _ := cookiejar.New(nil)
	return &HTTPClient{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
		},
	}
}

// Get fetches the given URL and returns the response body as a string.
// Applies per-domain rate limiting (1 req/s) and retries on 429/503.
func (h *HTTPClient) Get(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	// Rate limit per domain
	limiter := getCrawlLimiter(u.Host)
	if err := limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("crawl rate limit: %w", err)
	}

	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

		resp, err := h.client.Do(req)
		if err != nil {
			return "", err
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			waitSec := 5
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					waitSec = secs
				}
			}
			resp.Body.Close()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(waitSec) * time.Second):
			}
			continue
		}

		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
		}

		var sb strings.Builder
		buf := make([]byte, 32*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		resp.Body.Close()
		return sb.String(), nil
	}
	return "", fmt.Errorf("all %d attempts failed for %s", maxAttempts, rawURL)
}
