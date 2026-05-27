package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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
	Title     string `json:"title"`
	URL       string `json:"url"`
	ChapterNo int    `json:"chapter_no"`
	IsVip     bool   `json:"is_vip"` // 付费章节（内容可能无法爬取）
}

// ChapterListFetcher 可选扩展接口：解析器通过独立 HTTP 请求（如 Ajax API）获取章节列表
// 优先于 ParseChapterList 使用；若失败则回退到 HTML 解析
type ChapterListFetcher interface {
	FetchChapterList(ctx context.Context, client *HTTPClient, bookURL string) ([]*ChapterInfo, error)
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
	crawler.parsers["qimao"] = NewQimaoParser()

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
	if strings.Contains(url, "qimao.com") {
		return "qimao"
	}
	return "unknown"
}

// NewHTTPClient 创建 HTTP 客户端（带 cookie jar，自动保持会话）
func NewHTTPClient() *HTTPClient {
	jar, _ := cookiejar.New(nil)
	return &HTTPClient{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
		},
		headers: map[string]string{
			"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
			"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		},
	}
}

// CookieValue 返回指定域名下 cookie 的值（需先请求过该域名）
func (c *HTTPClient) CookieValue(rawURL, name string) string {
	if c.client.Jar == nil {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, cookie := range c.client.Jar.Cookies(u) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func (c *HTTPClient) Get(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
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

// GetJSON 发送带 XHR 标记的 GET 请求，用于调用 Ajax API
func (c *HTTPClient) GetJSON(ctx context.Context, rawURL string, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if referer != "" {
		req.Header.Set("Referer", referer)
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

// QidianParser 起点中文网解析器
type QidianParser struct{}

func NewQidianParser() *QidianParser {
	return &QidianParser{}
}

func (p *QidianParser) GetSiteName() string {
	return "起点中文网"
}

// extractBookID 从 URL 提取书籍 ID（如 https://www.qidian.com/book/1048727942/）
func (p *QidianParser) extractBookID(bookURL string) string {
	re := regexp.MustCompile(`/book/(\d+)`)
	if m := re.FindStringSubmatch(bookURL); len(m) > 1 {
		return m[1]
	}
	return ""
}

func (p *QidianParser) ParseNovelList(doc *goquery.Document) ([]*NovelInfo, error) {
	var novels []*NovelInfo
	doc.Find(".book-list li, .work-list li").Each(func(i int, s *goquery.Selection) {
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

// ParseNovelDetail 解析书籍详情页（多选择器回退 + 内嵌 JSON 提取）
func (p *QidianParser) ParseNovelDetail(doc *goquery.Document, bookURL string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	// 尝试从页面内嵌 JSON 提取（起点通常注入 window.g_data 或 __INITIAL_STATE__）
	doc.Find("script").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		src := s.Text()
		if !strings.Contains(src, "bookName") && !strings.Contains(src, "bookInfo") {
			return true
		}
		// 匹配 "bookName":"xxx"
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
		return detail.Title == "" // 找到标题就停止遍历
	})

	// HTML 选择器回退（多组，优先级从高到低）
	if detail.Title == "" {
		for _, sel := range []string{
			"h1.book-title", "h1.book-info-title", ".book-info h1", "h1",
		} {
			if t := strings.TrimSpace(doc.Find(sel).First().Text()); t != "" {
				detail.Title = t
				break
			}
		}
	}
	if detail.Author == "" {
		for _, sel := range []string{
			"a.writer-name", ".writer-info .name", ".author-name", ".book-author a",
		} {
			if a := strings.TrimSpace(doc.Find(sel).First().Text()); a != "" {
				detail.Author = a
				break
			}
		}
	}
	if detail.Description == "" {
		for _, sel := range []string{
			"p.intro", ".book-intro p", ".book-intro", ".intro",
		} {
			if d := strings.TrimSpace(doc.Find(sel).First().Text()); d != "" {
				detail.Description = d
				break
			}
		}
	}
	if detail.Genre == "" {
		detail.Genre = strings.TrimSpace(doc.Find(".book-cat a, .tag-list a, .book-label a").First().Text())
	}

	detail.CoverURL, _ = doc.Find(".book-img img, .book-cover img, .cover img").First().Attr("src")

	statusText := strings.ToLower(doc.Find(".book-label, .book-status, .status").First().Text())
	if strings.Contains(statusText, "完") {
		detail.Status = "completed"
	} else {
		detail.Status = "ongoing"
	}

	return detail, nil
}

// ParseChapterList HTML 回退方案（FetchChapterList 失败时使用）
func (p *QidianParser) ParseChapterList(doc *goquery.Document) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo
	doc.Find(".chapter-list li a, .volume-chapter a, .chapter-wrap a").Each(func(i int, s *goquery.Selection) {
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
		chapters = append(chapters, &ChapterInfo{
			Title:     title,
			URL:       href,
			ChapterNo: i + 1,
		})
	})
	return chapters, nil
}

// qidianCategoryResp 起点章节目录 Ajax API 响应结构
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
// API: GET /ajax/book/category?bookId={id}&_csrfToken={token}
// 访问书籍页面后 cookie jar 中会自动携带 _csrfToken
func (p *QidianParser) FetchChapterList(ctx context.Context, client *HTTPClient, bookURL string) ([]*ChapterInfo, error) {
	bookID := p.extractBookID(bookURL)
	if bookID == "" {
		return nil, fmt.Errorf("cannot extract book ID from URL: %s", bookURL)
	}

	csrfToken := client.CookieValue("https://www.qidian.com", "_csrfToken")
	apiURL := fmt.Sprintf("https://www.qidian.com/ajax/book/category?bookId=%s&_csrfToken=%s", bookID, csrfToken)

	body, err := client.GetJSON(ctx, apiURL, bookURL)
	if err != nil {
		return nil, fmt.Errorf("chapter list API request failed: %w", err)
	}

	var resp qidianCategoryResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("parse chapter list JSON failed: %w (body prefix: %.200s)", err, body)
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("chapter list API error code=%d msg=%s", resp.Code, resp.Msg)
	}

	var chapters []*ChapterInfo
	seq := 1
	for _, vol := range resp.Data.Vs {
		for _, ch := range vol.Cs {
			chapterURL := fmt.Sprintf("https://www.qidian.com/chapter/%s/%d/", bookID, ch.ID)
			chapters = append(chapters, &ChapterInfo{
				Title:     ch.Name,
				URL:       chapterURL,
				ChapterNo: seq,
				IsVip:     ch.IsVip != 0,
			})
			seq++
		}
	}

	if len(chapters) == 0 {
		return nil, fmt.Errorf("chapter list API returned 0 chapters (bookId=%s)", bookID)
	}
	return chapters, nil
}

// ParseChapter 解析起点章节正文
func (p *QidianParser) ParseChapter(doc *goquery.Document) (*ChapterContent, error) {
	content := &ChapterContent{}

	// 标题：多选择器回退
	for _, sel := range []string{
		".chapter-name", ".j_chapterName", "h1.chapter-name", "h1",
	} {
		if t := strings.TrimSpace(doc.Find(sel).First().Text()); t != "" {
			content.Title = t
			break
		}
	}

	// 正文：起点免费章节内容在 #j_readContent 或 .read-content 下的 <p> 标签
	var paragraphs []string
	contentSel := doc.Find("#j_readContent p, .read-content p, .chapter-content p, #j_chapterBox p")
	if contentSel.Length() > 0 {
		contentSel.Each(func(_ int, s *goquery.Selection) {
			if t := strings.TrimSpace(s.Text()); t != "" {
				paragraphs = append(paragraphs, t)
			}
		})
		content.Content = strings.Join(paragraphs, "\n\n")
	} else {
		// 回退到整块文本
		content.Content = cleanText(doc.Find("#j_readContent, .read-content, .chapter-content, #j_chapterBox").First().Text())
	}

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

// QimaoParser 七猫小说解析器
// 书籍详情页: https://www.qimao.com/shuku/{id}/
// 章节阅读页: https://www.qimao.com/shuku/{id}/{chapter_id}.html
type QimaoParser struct{}

func NewQimaoParser() *QimaoParser {
	return &QimaoParser{}
}

func (p *QimaoParser) GetSiteName() string {
	return "七猫小说"
}

func (p *QimaoParser) ParseNovelList(doc *goquery.Document) ([]*NovelInfo, error) {
	var novels []*NovelInfo

	doc.Find(".book-item, .novel-item, .book-list li").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Find(".book-name, .title, h3").First().Text())
		author := strings.TrimSpace(s.Find(".author, .writer").First().Text())
		url, _ := s.Find("a").First().Attr("href")
		cover, _ := s.Find("img").First().Attr("src")

		if title != "" {
			novels = append(novels, &NovelInfo{
				Title:    title,
				Author:   author,
				URL:      url,
				CoverURL: cover,
			})
		}
	})

	return novels, nil
}

func (p *QimaoParser) ParseNovelDetail(doc *goquery.Document, url string) (*NovelDetail, error) {
	detail := &NovelDetail{}

	// 书名: <h1 class="book-title"> 或 <h1 class="name">
	detail.Title = strings.TrimSpace(doc.Find("h1.book-title, h1.name, .detail-title h1").First().Text())
	if detail.Title == "" {
		detail.Title = strings.TrimSpace(doc.Find("h1").First().Text())
	}

	// 作者
	detail.Author = strings.TrimSpace(doc.Find(".author-name, .author a, .writer").First().Text())

	// 简介
	detail.Description = strings.TrimSpace(doc.Find(".book-intro, .desc, .intro, .summary").First().Text())

	// 封面
	detail.CoverURL, _ = doc.Find(".book-cover img, .cover img").First().Attr("src")

	// 类型/标签
	doc.Find(".tag-item, .book-tag, .label").Each(func(i int, s *goquery.Selection) {
		tag := strings.TrimSpace(s.Text())
		if tag != "" {
			detail.Tags = append(detail.Tags, tag)
		}
	})
	if len(detail.Tags) > 0 {
		detail.Genre = detail.Tags[0]
	}

	// 状态
	statusText := strings.TrimSpace(doc.Find(".status, .book-status").First().Text())
	if strings.Contains(statusText, "完") || strings.Contains(statusText, "完结") {
		detail.Status = "completed"
	} else {
		detail.Status = "ongoing"
	}

	// 章节数
	chapterText := doc.Find(".chapter-count, .total-chapter, .chapter-num").First().Text()
	detail.TotalChapters = extractNumber(chapterText)

	return detail, nil
}

func (p *QimaoParser) ParseChapterList(doc *goquery.Document) ([]*ChapterInfo, error) {
	var chapters []*ChapterInfo

	// 七猫章节列表常见选择器
	doc.Find(".chapter-list a, .catalog-list a, .chapter-item a, ul.list a").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		chapterURL, _ := s.Attr("href")

		if title == "" || chapterURL == "" {
			return
		}

		// 补全相对路径
		if strings.HasPrefix(chapterURL, "/") {
			chapterURL = "https://www.qimao.com" + chapterURL
		}

		chapters = append(chapters, &ChapterInfo{
			Title:     title,
			URL:       chapterURL,
			ChapterNo: i + 1,
		})
	})

	return chapters, nil
}

func (p *QimaoParser) ParseChapter(doc *goquery.Document) (*ChapterContent, error) {
	content := &ChapterContent{}

	// 章节标题
	content.Title = strings.TrimSpace(doc.Find(".chapter-title, .read-title, h1.title").First().Text())

	// 正文内容: 七猫正文一般在 #chapter-content 或 .chapter-content
	contentSel := doc.Find("#chapter-content, .chapter-content, .read-content, .content")
	if contentSel.Length() == 0 {
		contentSel = doc.Find("article")
	}
	content.Content = cleanText(contentSel.First().Text())

	// 上一章 / 下一章链接
	content.PrevURL, _ = doc.Find(".prev-chapter a, .btn-prev, a.prev").First().Attr("href")
	content.NextURL, _ = doc.Find(".next-chapter a, .btn-next, a.next").First().Attr("href")

	if strings.HasPrefix(content.PrevURL, "/") {
		content.PrevURL = "https://www.qimao.com" + content.PrevURL
	}
	if strings.HasPrefix(content.NextURL, "/") {
		content.NextURL = "https://www.qimao.com" + content.NextURL
	}

	return content, nil
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
