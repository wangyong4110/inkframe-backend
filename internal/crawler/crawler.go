package crawler

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// NovelCrawler 小说爬虫
type NovelCrawler struct {
	db         *gorm.DB
	httpClient *HTTPClient
	parsers    map[string]NovelParser
	rateLimit  time.Duration
}

type HTTPClient struct {
	client  *http.Client
	headers map[string]string
}

type NovelParser interface {
	// ParseNovelList 解析小说列表页
	ParseNovelList(doc *goquery.Document) ([]*NovelInfo, error)

	// ParseNovelDetail 解析小说详情页
	ParseNovelDetail(doc *goquery.Document, url string) (*NovelDetail, error)

	// ParseChapterList 解析章节列表
	ParseChapterList(doc *goquery.Document) ([]*ChapterInfo, error)

	// ParseChapter 解析章节内容
	ParseChapter(doc *goquery.Document) (*ChapterContent, error)

	// GetSiteName 获取站点名称
	GetSiteName() string
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
	Title         string `json:"title"`
	Author        string `json:"author"`
	Description   string `json:"description"`
	Genre         string `json:"genre"`
	Tags          []string `json:"tags"`
	Status        string `json:"status"` // ongoing, completed
	TotalChapters int    `json:"total_chapters"`
	TotalWords    int    `json:"total_words"`
	CoverURL      string `json:"cover_url"`
}

// ChapterInfo 章节信息
type ChapterInfo struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	ChapterNo int    `json:"chapter_no"`
}

// ChapterContent 章节内容
type ChapterContent struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	PrevURL  string `json:"prev_url"`
	NextURL  string `json:"next_url"`
}

// NewNovelCrawler 创建爬虫
func NewNovelCrawler(db *gorm.DB) *NovelCrawler {
	crawler := &NovelCrawler{
		db:        db,
		httpClient: NewHTTPClient(),
		parsers:   make(map[string]NovelParser),
		rateLimit: 2 * time.Second,
	}

	// 注册解析器
	crawler.parsers["qidian"] = NewQidianParser()
	crawler.parsers["jjwxc"] = NewJjwxcParser()
	crawler.parsers["zongheng"] = NewZonghengParser()

	return crawler
}

// CrawlNovel 爬取小说
func (c *NovelCrawler) CrawlNovel(ctx context.Context, url string) (*model.ReferenceNovel, error) {
	// 识别站点
	site := c.identifySite(url)
	parser, ok := c.parsers[site]
	if !ok {
		return nil, fmt.Errorf("unsupported site: %s", site)
	}

	// 获取页面
	html, err := c.httpClient.Get(ctx, url)
	if err != nil {
		return nil, err
	}

	// 解析详情
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	detail, err := parser.ParseNovelDetail(doc, url)
	if err != nil {
		return nil, err
	}

	// 创建数据库记录
	novel := &model.ReferenceNovel{
		Title:      detail.Title,
		Author:     detail.Author,
		SourceURL:  url,
		SourceSite: site,
		Genre:      detail.Genre,
		Status:     "crawling",
	}

	if err := c.db.Create(novel).Error; err != nil {
		return nil, err
	}

	// 异步爬取章节
	go c.crawlChaptersAsync(context.Background(), novel.ID, site, parser, url)

	return novel, nil
}

// crawlChaptersAsync 异步爬取章节
func (c *NovelCrawler) crawlChaptersAsync(ctx context.Context, novelID uint, site string, parser NovelParser, baseURL string) {
	// 获取章节列表页
	html, err := c.httpClient.Get(ctx, baseURL)
	if err != nil {
		c.updateNovelStatus(novelID, "failed")
		return
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		c.updateNovelStatus(novelID, "failed")
		return
	}

	chapters, err := parser.ParseChapterList(doc)
	if err != nil {
		c.updateNovelStatus(novelID, "failed")
		return
	}

	// 爬取每一章
	for i, ch := range chapters {
		select {
		case <-ctx.Done():
			return
		default:
		}

		content, err := c.crawlChapter(ctx, ch.URL, parser)
		if err != nil {
			continue
		}

		// 保存章节
		refChapter := &model.ReferenceChapter{
			NovelID:   novelID,
			ChapterNo: i + 1,
			Title:     content.Title,
			Content:   content.Content,
		}
		c.db.Create(refChapter)

		// 限流
		time.Sleep(c.rateLimit)
	}

	// 更新状态
	c.updateNovelStatus(novelID, "completed")

	// 触发分析
	c.analyzeNovel(novelID)
}

// crawlChapter 爬取单章
func (c *NovelCrawler) crawlChapter(ctx context.Context, url string, parser NovelParser) (*ChapterContent, error) {
	html, err := c.httpClient.Get(ctx, url)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	return parser.ParseChapter(doc)
}

// updateNovelStatus 更新小说状态
func (c *NovelCrawler) updateNovelStatus(novelID uint, status string) {
	c.db.Model(&model.ReferenceNovel{}).Where("id = ?", novelID).Update("status", status)
}

// analyzeNovel 分析小说
func (c *NovelCrawler) analyzeNovel(novelID uint) {
	// 触发分析服务（实际应该在 service 层实现）
}

// identifySite 识别站点
func (c *NovelCrawler) identifySite(url string) string {
	if strings.Contains(url, "qidian.com") {
		return "qidian"
	}
	if strings.Contains(url, "jjwxc.net") {
		return "jjwxc"
	}
	if strings.Contains(url, "zongheng.com") {
		return "zongheng"
	}
	return "unknown"
}

// NewHTTPClient 创建 HTTP 客户端
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		headers: map[string]string{
			"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
		},
	}
}

func (c *HTTPClient) Get(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(buf), nil
}

// 导入需要的包
import (
	"io"
	"net/http"
)

// QidianParser 起点中文网解析器
type QidianParser struct{}

func NewQidianParser() *QidianParser {
	return &QidianParser{}
}

func (p *QidianParser) GetSiteName() string {
	return "起点中文网"
}

func (p *QidianParser) ParseNovelList(doc *goquery.Document) ([]*NovelInfo, error) {
	var novels []*NovelInfo

	doc.Find(".book-list li, .work-list li").Each(func(i int, s *goquery.Selection) {
		title := s.Find(".book-name, .title").Text()
		author := s.Find(".author").Text()
		url, _ := s.Find("a").Attr("href")

		novels = append(novels, &NovelInfo{
			Title:  strings.TrimSpace(title),
			Author: strings.TrimSpace(author),
			URL:    url,
		})
	})

	return novels, nil
}

func (p *QidianParser) ParseNovelDetail(doc *goquery.Document, url string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	detail.Title = doc.Find(".book-title, .book-name").First().Text()
	detail.Author = doc.Find(".author-name").First().Text()
	detail.Description = doc.Find(".book-intro, .desc").First().Text()

	// 提取章节数
	chapterText := doc.Find(".chapter-count, .total").First().Text()
	detail.TotalChapters = extractNumber(chapterText)

	return detail, nil
}

func (p *QidianParser) ParseChapterList(doc *goquery.Document) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo

	doc.Find(".chapter-list li a, .volume-chapter a").Each(func(i int, s *goquery.Selection) {
		title := s.Text()
		url, _ := s.Attr("href")

		chapters = append(chapters, &ChapterInfo{
			Title:    strings.TrimSpace(title),
			URL:      url,
			ChapterNo: i + 1,
		})
	})

	return chapters, nil
}

func (p *QidianParser) ParseChapter(doc *goquery.Document) (*ChapterContent, error) {
	content := &ChapterContent{}

	content.Title = doc.Find(".chapter-title, .j_chapterName").First().Text()
	content.Content = doc.Find("#j_chapterBox, .chapter-content, .read-content").First().Text()

	// 清理内容
	content.Content = cleanText(content.Content)

	return content, nil
}

// JjwxcParser 晋江文学城解析器
type JjwxcParser struct{}

func NewJjwxcParser() *JjwxcParser {
	return &JjwxcParser{}
}

func (p *JjwxcParser) GetSiteName() string {
	return "晋江文学城"
}

func (p *JjwxcParser) ParseNovelList(doc *goquery.Document) ([]*NovelInfo, error) {
	var novels []*NovelInfo

	doc.Find(".novel-item").Each(func(i int, s *goquery.Selection) {
		title := s.Find(".novel-title").Text()
		author := s.Find(".author").Text()

		novels = append(novels, &NovelInfo{
			Title:  strings.TrimSpace(title),
			Author: strings.TrimSpace(author),
		})
	})

	return novels, nil
}

func (p *JjwxcParser) ParseNovelDetail(doc *goquery.Document, url string) (*NovelDetail, error) {
	return &NovelDetail{}, nil
}

func (p *JjwxcParser) ParseChapterList(doc *goquery.Document) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo

	doc.Find(".chapter-list a").Each(func(i int, s *goquery.Selection) {
		title := s.Text()
		url, _ := s.Attr("href")

		chapters = append(chapters, &ChapterInfo{
			Title:    strings.TrimSpace(title),
			URL:      url,
			ChapterNo: i + 1,
		})
	})

	return chapters, nil
}

func (p *JjwxcParser) ParseChapter(doc *goquery.Document) (*ChapterContent, error) {
	content := &ChapterContent{}

	content.Title = doc.Find(".chapter-title").First().Text()
	content.Content = doc.Find("#content").First().Text()
	content.Content = cleanText(content.Content)

	return content, nil
}

// ZonghengParser 纵横中文网解析器
type ZonghengParser struct{}

func NewZonghengParser() *ZonghengParser {
	return &ZonghengParser{}
}

func (p *ZonghengParser) GetSiteName() string {
	return "纵横中文网"
}

func (p *ZonghengParser) ParseNovelList(doc *goquery.Document) ([]*NovelInfo, error) {
	return []*NovelInfo{}, nil
}

func (p *ZonghengParser) ParseNovelDetail(doc *goquery.Document, url string) (*NovelDetail, error) {
	return &NovelDetail{}, nil
}

func (p *ZonghengParser) ParseChapterList(doc *goquery.Document) ([]*ChapterInfo, error) {
	return []*ChapterInfo{}, nil
}

func (p *ZonghengParser) ParseChapter(doc *goquery.Document) (*ChapterContent, error) {
	return &ChapterContent{}, nil
}

// Helper Functions

func extractNumber(text string) int {
	re := regexp.MustCompile(`\d+`)
	matches := re.FindAllString(text, -1)
	if len(matches) > 0 {
		var num int
		fmt.Sscanf(matches[len(matches)-1], "%d", &num)
		return num
	}
	return 0
}

func cleanText(text string) string {
	// 移除多余空白
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, "\n")
	// 移除特殊字符
	text = strings.TrimSpace(text)
	return text
}

// BatchCrawler 批量爬虫
type BatchCrawler struct {
	crawler  *NovelCrawler
	workers  int
	queue    chan string
	results  chan *CrawlResult
	wg       sync.WaitGroup
}

type CrawlResult struct {
	URL    string
	Novel  *model.ReferenceNovel
	Error  error
}

func NewBatchCrawler(db *gorm.DB, workers int) *BatchCrawler {
	return &BatchCrawler{
		crawler: NewNovelCrawler(db),
		workers:  workers,
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

	for url := range b.queue {
		novel, err := b.crawler.CrawlNovel(context.Background(), url)
		b.results <- &CrawlResult{
			URL:   url,
			Novel: novel,
			Error: err,
		}
	}
}

func (b *BatchCrawler) Add(url string) {
	b.queue <- url
}

func (b *BatchCrawler) Wait() {
	close(b.queue)
	b.wg.Wait()
	close(b.results)
}

func (b *BatchCrawler) Results() <-chan *CrawlResult {
	return b.results
}
