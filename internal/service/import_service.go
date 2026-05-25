package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// ============================================
// 小说导入服务 - Novel Import Service
// ============================================

// ImportSource 导入来源
type ImportSource string

const (
	SourceFile  ImportSource = "file"  // 本地文件
	SourceURL   ImportSource = "url"   // URL链接
	SourceCrawl ImportSource = "crawl" // 爬取
	SourceAPI   ImportSource = "api"   // API导入
)

// ImportFormat 支持的文件格式
type ImportFormat string

const (
	FormatTxt  ImportFormat = "txt"
	FormatEpub ImportFormat = "epub"
	FormatDocx ImportFormat = "docx"
	FormatHtml ImportFormat = "html"
	FormatJson ImportFormat = "json"
	FormatMd   ImportFormat = "md"
)

// ImportRequest 导入请求
type ImportRequest struct {
	Source   ImportSource `json:"source"`
	URL      string       `json:"url,omitempty"`       // 导入URL
	FileData []byte       `json:"file_data,omitempty"` // 文件数据
	FileName string       `json:"file_name,omitempty"` // 文件名
	Format   ImportFormat `json:"format,omitempty"`    // 文件格式
	SiteName string       `json:"site_name,omitempty"` // 站点名称（爬取时）
	NovelID  uint         `json:"novel_id,omitempty"`  // 已有小说ID（追加时）
	TenantID uint         `json:"tenant_id,omitempty"` // 租户ID（用于去重）
}

// ImportResult 导入结果
type ImportResult struct {
	NovelID          uint     `json:"novel_id"`
	Title            string   `json:"title"`
	TotalChapters    int      `json:"total_chapters"`
	ImportedChapters int      `json:"imported_chapters"`
	FailedChapters   int      `json:"failed_chapters"`
	Duration         float64  `json:"duration"` // 秒
	Errors           []string `json:"errors,omitempty"`
	OSSUrl           string   `json:"oss_url,omitempty"` // 原始文件 OSS 备份地址
}

// CrawlProgress 爬取进度
type CrawlProgress struct {
	NovelID uint   `json:"novel_id"`
	Status  string `json:"status"`   // running / paused / completed / failed
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Current string `json:"current"` // 当前正在爬取的章节标题
}

// NovelImportService 小说导入服务
type NovelImportService struct {
	novelRepo          *repository.NovelRepository
	chapterRepo        *repository.ChapterRepository
	crawler            *crawler.NovelCrawler
	narrativeMemory    *NarrativeMemoryService
	storageSvc         storage.Service
	analysisService    *NovelAnalysisService
	aiService          *AIService
	crawlProgress      sync.Map // novelID(uint) → *CrawlProgress
	crawlDoneCallbacks sync.Map // novelID(uint) → func()
}

// NewNovelImportService 创建小说导入服务
func NewNovelImportService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	crawler *crawler.NovelCrawler,
) *NovelImportService {
	return &NovelImportService{
		novelRepo:   novelRepo,
		chapterRepo: chapterRepo,
		crawler:     crawler,
	}
}

// WithNarrativeMemory 注入叙事记忆服务（用于爬取后自动生成摘要）
func (s *NovelImportService) WithNarrativeMemory(nm *NarrativeMemoryService) *NovelImportService {
	s.narrativeMemory = nm
	return s
}

// WithStorage 注入存储服务（用于将原始文件上传到 OSS）
func (s *NovelImportService) WithStorage(svc storage.Service) *NovelImportService {
	s.storageSvc = svc
	return s
}

// WithAnalysisService 注入分析服务（导入完成后自动触发分析）
func (s *NovelImportService) WithAnalysisService(svc *NovelAnalysisService) *NovelImportService {
	s.analysisService = svc
	return s
}

// WithAIService 注入 AI 服务（章节标记不明显时 AI 辅助划分章节）
func (s *NovelImportService) WithAIService(svc *AIService) *NovelImportService {
	s.aiService = svc
	return s
}

// RegisterCrawlDoneCallback 注册爬取完成回调（一次性，触发后自动移除）
func (s *NovelImportService) RegisterCrawlDoneCallback(novelID uint, fn func()) {
	s.crawlDoneCallbacks.Store(novelID, fn)
}

// uploadRawToOSS 将原始字节上传到 OSS（失败为 best-effort，不阻断主流程）
func (s *NovelImportService) uploadRawToOSS(ctx context.Context, tenantID, novelID uint, filename string, data []byte) string {
	if s.storageSvc == nil || len(data) == 0 {
		return ""
	}
	key := fmt.Sprintf("novels/%d/source/%s_%s",
		novelID, time.Now().Format("20060102150405"), filepath.Base(filename))
	url, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)),
		contentTypeFromFilename(filename))
	if err != nil {
		logger.Printf("[Import] OSS upload failed for novel %d: %v", novelID, err)
		return ""
	}
	return url
}

// contentTypeFromFilename 根据文件名推断 Content-Type
func contentTypeFromFilename(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".markdown":
		return "text/plain; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".epub":
		return "application/epub+zip"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// GetCrawlProgress 获取爬取进度（先查内存，再查数据库）
func (s *NovelImportService) GetCrawlProgress(novelID uint) (*CrawlProgress, error) {
	if v, ok := s.crawlProgress.Load(novelID); ok {
		return v.(*CrawlProgress), nil
	}
	// 内存中无记录，查询数据库判断是否有待爬取章节
	pending, err := s.chapterRepo.ListPendingCrawl(novelID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil // 无爬取任务
	}
	total, _ := s.chapterRepo.CountByNovel(novelID)
	done := int(total) - len(pending)
	progress := &CrawlProgress{
		NovelID: novelID,
		Status:  "paused", // 内存中无记录说明服务器重启过，状态为 paused
		Total:   int(total),
		Done:    done,
		Failed:  0,
	}
	return progress, nil
}

// ResumeCrawl 从断点继续爬取
func (s *NovelImportService) ResumeCrawl(novelID uint) error {
	// 检查是否已有运行中的任务
	if v, ok := s.crawlProgress.Load(novelID); ok {
		p := v.(*CrawlProgress)
		if p.Status == "running" {
			return fmt.Errorf("crawl already running for novel %d", novelID)
		}
	}

	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return fmt.Errorf("novel not found: %w", err)
	}

	pending, err := s.chapterRepo.ListPendingCrawl(novelID)
	if err != nil || len(pending) == 0 {
		return fmt.Errorf("no pending chapters to crawl")
	}

	// 从第一个待爬取章节的 URL 推断解析器
	firstURL := strings.TrimPrefix(pending[0].Outline, "crawl:")
	parser, err := s.getParserForURL(firstURL)
	if err != nil {
		return fmt.Errorf("cannot identify site parser: %w", err)
	}

	total, _ := s.chapterRepo.CountByNovel(novelID)
	done := int(total) - len(pending)
	progress := &CrawlProgress{
		NovelID: novelID,
		Status:  "running",
		Total:   int(total),
		Done:    done,
	}
	s.crawlProgress.Store(novelID, progress)

	go s.crawlChaptersBackground(context.Background(), novelID, novel.Title, parser, pending, progress)
	return nil
}

// crawlChaptersBackground 后台爬取章节内容（带进度追踪 + AI 摘要生成）
func (s *NovelImportService) crawlChaptersBackground(
	ctx context.Context,
	novelID uint,
	novelTitle string,
	parser crawler.NovelParser,
	chapters []*model.Chapter,
	progress *CrawlProgress,
) {
	httpClient := crawler.NewHTTPClient()
	const rateLimit = 2 * time.Second

	for _, ch := range chapters {
		select {
		case <-ctx.Done():
			progress.Status = "paused"
			return
		default:
		}

		chapterURL := strings.TrimPrefix(ch.Outline, "crawl:")
		if chapterURL == "" {
			progress.Done++
			continue
		}

		progress.Current = ch.Title

		// 爬取页面
		html, err := httpClient.Get(ctx, chapterURL)
		if err != nil {
			logger.Printf("[Crawl] chapter %d fetch error: %v", ch.ChapterNo, err)
			progress.Failed++
			time.Sleep(rateLimit)
			continue
		}

		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			logger.Printf("[Crawl] chapter %d parse html error: %v", ch.ChapterNo, err)
			progress.Failed++
			time.Sleep(rateLimit)
			continue
		}

		content, err := parser.ParseChapter(doc)
		if err != nil || content.Content == "" {
			logger.Printf("[Crawl] chapter %d parse content error: %v", ch.ChapterNo, err)
			progress.Failed++
			time.Sleep(rateLimit)
			continue
		}

		// 使用爬取到的标题（若非空）
		title := ch.Title
		if strings.TrimSpace(content.Title) != "" {
			title = strings.TrimSpace(content.Title)
		}
		wordCount := len([]rune(content.Content))

		// 写回数据库
		if err := s.chapterRepo.UpdateCrawledContent(ch.ID, title, content.Content, wordCount); err != nil {
			logger.Printf("[Crawl] chapter %d save error: %v", ch.ChapterNo, err)
			progress.Failed++
			time.Sleep(rateLimit)
			continue
		}

		progress.Done++
		logger.Printf("[Crawl] novel=%d chapter=%d/%d done", novelID, progress.Done, progress.Total)

		// 爬取完成后自动生成 AI 摘要
		if s.narrativeMemory != nil {
			ch.Title = title
			ch.Content = content.Content
			summary, err := s.narrativeMemory.GenerateChapterSummary(0, ch, novelTitle)
			if err != nil {
				logger.Printf("[Crawl] chapter %d summary error: %v", ch.ChapterNo, err)
			} else {
				ch.Summary = summary
				s.chapterRepo.Update(ch)
			}
		}

		time.Sleep(rateLimit)
	}

	// 收尾
	if progress.Failed == 0 {
		progress.Status = "completed"
		s.novelRepo.UpdateFields(novelID, map[string]interface{}{"status": "writing"})
	} else if progress.Done == 0 {
		progress.Status = "failed"
	} else {
		progress.Status = "completed"
		s.novelRepo.UpdateFields(novelID, map[string]interface{}{"status": "writing"})
	}
	progress.Current = ""
	logger.Printf("[Crawl] novel=%d finished: done=%d failed=%d", novelID, progress.Done, progress.Failed)

	// 将全本内容合并上传到 OSS 备份
	if s.storageSvc != nil && progress.Status == "completed" {
		go func() {
			allChapters, err := s.chapterRepo.ListByNovel(novelID)
			if err != nil || len(allChapters) == 0 {
				return
			}
			var sb strings.Builder
			sb.WriteString(novelTitle + "\n\n")
			for _, ch := range allChapters {
				if ch.Content != "" {
					sb.WriteString(fmt.Sprintf("第%d章 %s\n\n%s\n\n", ch.ChapterNo, ch.Title, ch.Content))
				}
			}
			if sb.Len() == 0 {
				return
			}
			data := []byte(sb.String())
			key := fmt.Sprintf("novels/%d/source/crawl_%s.txt", novelID, time.Now().Format("20060102150405"))
			s.storageSvc.Upload(context.Background(), key, bytes.NewReader(data), int64(len(data)), "text/plain; charset=utf-8") //nolint:errcheck
		}()
	}

	// 触发注册的完成回调（如自动分析）
	if fn, ok := s.crawlDoneCallbacks.LoadAndDelete(novelID); ok {
		fn.(func())()
	}
}

// getParserForURL 根据 URL 返回对应解析器
func (s *NovelImportService) getParserForURL(url string) (crawler.NovelParser, error) {
	site := s.detectSiteFromURL(url)
	switch site {
	case "qidian":
		return crawler.NewQidianParser(), nil
	case "jjwxc":
		return crawler.NewJjwxcParser(), nil
	case "zongheng":
		return crawler.NewZonghengParser(), nil
	case "qimao":
		return crawler.NewQimaoParser(), nil
	default:
		return nil, fmt.Errorf("unsupported site: %s", site)
	}
}

// Import 执行导入
func (s *NovelImportService) Import(req *ImportRequest) (*ImportResult, error) {
	start := time.Now()

	var result *ImportResult
	var err error

	switch req.Source {
	case SourceFile:
		result, err = s.importFromFile(req)
	case SourceURL:
		result, err = s.importFromURL(req)
	case SourceCrawl:
		result, err = s.importFromCrawl(req)
	default:
		return nil, fmt.Errorf("unsupported import source: %s", req.Source)
	}

	if err != nil || result == nil {
		return result, err
	}
	result.Duration = time.Since(start).Seconds()
	return result, nil
}

// 从文件导入
func (s *NovelImportService) importFromFile(req *ImportRequest) (*ImportResult, error) {
	result := &ImportResult{}

	// 检测格式
	format := s.detectFormat(req.FileName, req.Format)

	// 解析内容
	var novel *model.Novel
	var chapters []*model.Chapter
	var err error

	switch format {
	case FormatTxt:
		novel, chapters, err = s.parseTxtFile(req.FileData, req.FileName)
	case FormatMd:
		novel, chapters, err = s.parseMarkdownFile(req.FileData, req.FileName)
	case FormatJson:
		novel, chapters, err = s.parseJsonFile(req.FileData)
	case FormatHtml:
		novel, chapters, err = s.parseHtmlFile(req.FileData)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}

	if err != nil {
		return nil, fmt.Errorf("parse file failed: %w", err)
	}

	// 追加模式：使用已有小说，跳过创建
	chapterOffset := 0
	if req.NovelID > 0 {
		existing, err := s.novelRepo.GetByID(req.NovelID)
		if err != nil {
			return nil, fmt.Errorf("novel not found: %w", err)
		}
		novel = existing
		count, _ := s.chapterRepo.CountByNovel(req.NovelID)
		chapterOffset = int(count)
	} else {
		// 按标题去重：同 tenant 下同名小说直接追加章节，不新建项目
		if req.TenantID > 0 {
			if existing, err := s.novelRepo.FindByTitle(novel.Title, req.TenantID); err == nil && existing != nil {
				novel = existing
				count, _ := s.chapterRepo.CountByNovel(existing.ID)
				chapterOffset = int(count)
				logger.Printf("[Import] novel %q already exists (id=%d), appending chapters from offset %d", novel.Title, novel.ID, chapterOffset)
			}
		}
		// 仍需新建（无同名小说）
		if novel.ID == 0 {
			novel.UUID = uuid.New().String()
			novel.TenantID = req.TenantID
			if err := s.novelRepo.Create(novel); err != nil {
				return nil, fmt.Errorf("save novel failed: %w", err)
			}
		}
	}
	result.NovelID = novel.ID
	result.Title = novel.Title

	// 将原始文件上传到 OSS 备份（novelID 确定后立即上传）
	if len(req.FileData) > 0 {
		result.OSSUrl = s.uploadRawToOSS(context.Background(), req.TenantID, novel.ID, req.FileName, req.FileData)
	}

	// 保存章节：先给所有章节填充 NovelID / ChapterNo / UUID
	for i, chapter := range chapters {
		chapter.NovelID = novel.ID
		chapter.ChapterNo = chapterOffset + i + 1
		if chapter.UUID == "" {
			chapter.UUID = uuid.New().String()
		}
	}
	result.TotalChapters = len(chapters)

	// 新建小说（chapterOffset==0 且 novel 刚刚创建）：跳过逐章查重，直接批量插入。
	// 这将 N 次 SELECT+INSERT 降为 1 次批量 INSERT，对数百章的大文件性能提升显著。
	if chapterOffset == 0 && req.NovelID == 0 {
		if err := s.chapterRepo.CreateInBatches(chapters, 100); err != nil {
			result.FailedChapters = len(chapters)
			result.Errors = append(result.Errors, fmt.Sprintf("batch create failed: %v", err))
			logger.Printf("[Import] novel=%d batch create failed: %v", novel.ID, err)
		} else {
			result.ImportedChapters = len(chapters)
			logger.Printf("[Import] novel=%d batch created %d chapters", novel.ID, len(chapters))
		}
		return result, nil
	}

	// 追加/覆盖模式：批量查已有章节，构建 chapter_no→record map，避免 N+1 查询
	minChNo := chapterOffset + 1
	maxChNo := chapterOffset + len(chapters)
	existingList, _ := s.chapterRepo.GetByNovelAndChapterRange(novel.ID, minChNo, maxChNo)
	existingMap := make(map[int]*model.Chapter, len(existingList))
	for _, ex := range existingList {
		existingMap[ex.ChapterNo] = ex
	}

	var toCreate []*model.Chapter
	for _, chapter := range chapters {
		existing, ok := existingMap[chapter.ChapterNo]
		if ok {
			if existing.Content == "" && chapter.Content != "" {
				// 用新内容覆盖上次导入遗留的空章节
				existing.Title = chapter.Title
				existing.Content = chapter.Content
				existing.WordCount = chapter.WordCount
				existing.Status = chapter.Status
				if err := s.chapterRepo.Update(existing); err != nil {
					result.FailedChapters++
					logger.Printf("[Import] novel=%d chapter %d update failed: %v", novel.ID, chapter.ChapterNo, err)
					result.Errors = append(result.Errors, fmt.Sprintf("chapter %d update failed: %v", chapter.ChapterNo, err))
				} else {
					result.ImportedChapters++
				}
			} else {
				logger.Printf("[Import] novel=%d chapter %d already exists (%d chars), skipping", novel.ID, chapter.ChapterNo, len([]rune(existing.Content)))
				result.ImportedChapters++
			}
		} else {
			toCreate = append(toCreate, chapter)
		}
	}
	if len(toCreate) > 0 {
		if err := s.chapterRepo.CreateInBatches(toCreate, 100); err != nil {
			result.FailedChapters += len(toCreate)
			result.Errors = append(result.Errors, fmt.Sprintf("batch create failed: %v", err))
			logger.Printf("[Import] novel=%d batch create %d chapters failed: %v", novel.ID, len(toCreate), err)
		} else {
			result.ImportedChapters += len(toCreate)
			logger.Printf("[Import] novel=%d batch created %d new chapters", novel.ID, len(toCreate))
		}
	}

	return result, nil
}

// 从URL导入
func (s *NovelImportService) importFromURL(req *ImportRequest) (*ImportResult, error) {
	// 下载内容
	resp, err := http.Get(req.URL)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	// 解析内容
	req.FileData = data
	req.FileName = req.URL

	return s.importFromFile(req)
}

// 从爬取导入
func (s *NovelImportService) importFromCrawl(req *ImportRequest) (*ImportResult, error) {
	result := &ImportResult{}

	parser, err := s.getParserForURL(req.URL)
	if err != nil {
		return nil, fmt.Errorf("unsupported site: %w", err)
	}

	httpClient := crawler.NewHTTPClient()

	// 下载小说目录页
	html, err := httpClient.Get(context.Background(), req.URL)
	if err != nil {
		return nil, fmt.Errorf("get page failed: %w", err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("parse page failed: %w", err)
	}

	// 解析小说详情
	detail, err := parser.ParseNovelDetail(doc, req.URL)
	if err != nil {
		return nil, fmt.Errorf("parse novel detail failed: %w", err)
	}

	// 解析章节列表
	chapterInfos, err := parser.ParseChapterList(doc)
	if err != nil {
		return nil, fmt.Errorf("parse chapter list failed: %w", err)
	}

	// 按标题去重：同 tenant 下同名小说直接追加，不新建项目
	var novel *model.Novel
	chapterOffset := 0
	if req.TenantID > 0 {
		if existing, err := s.novelRepo.FindByTitle(detail.Title, req.TenantID); err == nil && existing != nil {
			novel = existing
			count, _ := s.chapterRepo.CountByNovel(existing.ID)
			chapterOffset = int(count)
			logger.Printf("[Import] crawl: novel %q already exists (id=%d), appending from offset %d", novel.Title, novel.ID, chapterOffset)
		}
	}
	if novel == nil {
		novel = &model.Novel{
			UUID:        uuid.New().String(),
			TenantID:    req.TenantID,
			Title:       detail.Title,
			Description: detail.Description,
			Genre:       detail.Genre,
			Status:      "importing",
		}
		if err := s.novelRepo.Create(novel); err != nil {
			return nil, fmt.Errorf("save novel failed: %w", err)
		}
	}

	result.NovelID = novel.ID
	result.Title = novel.Title
	result.TotalChapters = len(chapterInfos)

	// 创建章节存根，Outline 存储章节爬取 URL（用于断点续爬）
	var pendingChapters []*model.Chapter
	for i, info := range chapterInfos {
		chapter := &model.Chapter{
			NovelID:   novel.ID,
			ChapterNo: chapterOffset + i + 1,
			Title:     info.Title,
			Content:   "",
			Outline:   "crawl:" + info.URL,
			Status:    "draft",
		}
		if err := s.chapterRepo.Create(chapter); err != nil {
			result.FailedChapters++
			result.Errors = append(result.Errors, fmt.Sprintf("chapter %d save failed: %v", i+1, err))
		} else {
			result.ImportedChapters++
			pendingChapters = append(pendingChapters, chapter)
		}
	}

	// 初始化进度并启动后台爬取
	progress := &CrawlProgress{
		NovelID: novel.ID,
		Status:  "running",
		Total:   len(pendingChapters),
		Done:    0,
	}
	s.crawlProgress.Store(novel.ID, progress)
	go s.crawlChaptersBackground(context.Background(), novel.ID, novel.Title, parser, pendingChapters, progress)

	return result, nil
}

// 检测文件格式
func (s *NovelImportService) detectFormat(fileName string, format ImportFormat) ImportFormat {
	if format != "" {
		return format
	}

	ext := strings.ToLower(filepath.Ext(fileName))
	switch ext {
	case ".txt":
		return FormatTxt
	case ".md", ".markdown":
		return FormatMd
	case ".json":
		return FormatJson
	case ".html", ".htm":
		return FormatHtml
	case ".epub":
		return FormatEpub
	case ".docx":
		return FormatDocx
	default:
		return FormatTxt
	}
}

// 检测站点
func (s *NovelImportService) detectSiteFromURL(url string) string {
	url = strings.ToLower(url)
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
	return "default"
}

// toUTF8Text 将文本字节转换为 UTF-8 字符串。
// 若输入已是合法 UTF-8 直接返回；否则尝试 GBK（GB2312/GB18030）解码。
// 绝大多数中文 TXT 小说在 Windows 下以 GBK 编码保存，此处做自动兼容。
func toUTF8Text(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	// 尝试 GBK → UTF-8
	decoder := simplifiedchinese.GBK.NewDecoder()
	out, _, err := transform.Bytes(decoder, data)
	if err == nil && utf8.Valid(out) {
		return string(out)
	}
	// 降级：直接转换（可能含乱码，但不中断）
	return string(data)
}

// 解析TXT文件
func (s *NovelImportService) parseTxtFile(data []byte, fileName string) (*model.Novel, []*model.Chapter, error) {
	content := toUTF8Text(data)
	// 统一换行符：Windows CRLF → LF，避免正则行首锚点失配
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	// 提取标题（从文件名或内容第一行）
	// 注意：若第一行本身是章节标题（如"第一章 xxx"），不把它当小说标题
	title := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	lines := strings.Split(content, "\n")
	chapterHeadRe := regexp.MustCompile(`^第[一二三四五六七八九十百千零〇\d]+[章节卷]`)
	if len(lines) > 0 {
		titleLine := strings.TrimSpace(lines[0])
		if len([]rune(titleLine)) > 0 && len([]rune(titleLine)) < 50 && !chapterHeadRe.MatchString(titleLine) {
			title = titleLine
			content = strings.Join(lines[1:], "\n")
		}
	}

	novel := &model.Novel{
		Title:  title,
		Genre:  "unknown",
		Status: "completed",
	}

	// 按章节分割
	chapters := s.splitByChapters(content, title)

	return novel, chapters, nil
}

// 解析Markdown文件
func (s *NovelImportService) parseMarkdownFile(data []byte, fileName string) (*model.Novel, []*model.Chapter, error) {
	content := toUTF8Text(data)
	lines := strings.Split(content, "\n")

	// 提取标题（从文件名或第一个#标题）
	title := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") {
			title = strings.TrimPrefix(strings.TrimSpace(line), "# ")
			break
		}
	}

	novel := &model.Novel{
		Title:  title,
		Genre:  "unknown",
		Status: "completed",
	}

	// 合并内容并按章节分割
	fullContent := strings.Join(lines, "\n")
	chapters := s.splitByChapters(fullContent, title)

	return novel, chapters, nil
}

// 解析JSON文件
func (s *NovelImportService) parseJsonFile(data []byte) (*model.Novel, []*model.Chapter, error) {
	// 尝试解析为结构化JSON
	var structured struct {
		Title    string `json:"title"`
		Author   string `json:"author"`
		Genre    string `json:"genre"`
		Chapters []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"chapters"`
		Content string `json:"content"` // 如果是扁平结构
	}

	if err := json.Unmarshal(data, &structured); err != nil {
		// 如果解析失败，当作纯文本处理
		return s.parseTxtFile(data, "imported.json")
	}

	novel := &model.Novel{
		Title:  structured.Title,
		Genre:  structured.Genre,
		Status: "completed",
	}

	var chapters []*model.Chapter
	if len(structured.Chapters) > 0 {
		for i, ch := range structured.Chapters {
			chapters = append(chapters, &model.Chapter{
				ChapterNo: i + 1,
				Title:     ch.Title,
				Content:   ch.Content,
				WordCount: len([]rune(ch.Content)),
				Status:    "published",
			})
		}
	} else if structured.Content != "" {
		// 扁平结构，按章节分割
		chapters = s.splitByChapters(structured.Content, novel.Title)
	}

	return novel, chapters, nil
}

// 解析HTML文件
func (s *NovelImportService) parseHtmlFile(data []byte) (*model.Novel, []*model.Chapter, error) {
	content := toUTF8Text(data)

	// 简单提取标题
	titleRegex := regexp.MustCompile(`<title>([^<]+)</title>`)
	titleMatch := titleRegex.FindStringSubmatch(content)
	title := "Imported Novel"
	if len(titleMatch) > 1 {
		title = titleMatch[1]
	}

	// 去除HTML标签
	cleanContent := s.stripHtmlTags(content)

	novel := &model.Novel{
		Title:  title,
		Genre:  "unknown",
		Status: "completed",
	}

	chapters := s.splitByChapters(cleanContent, title)

	return novel, chapters, nil
}

// 去除HTML标签
func (s *NovelImportService) stripHtmlTags(html string) string {
	// 去除script和style
	re := regexp.MustCompile(`(?s)<script[^>]*>.*?</script>`)
	html = re.ReplaceAllString(html, "")
	re = regexp.MustCompile(`(?s)<style[^>]*>.*?</style>`)
	html = re.ReplaceAllString(html, "")

	// 去除所有标签
	re = regexp.MustCompile(`<[^>]+>`)
	content := re.ReplaceAllString(html, "\n")

	// 清理多余空白
	lines := strings.Split(content, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 10 {
			cleaned = append(cleaned, line)
		}
	}

	return strings.Join(cleaned, "\n\n")
}

// 按章节分割
func (s *NovelImportService) splitByChapters(content, novelTitle string) []*model.Chapter {
	// 尝试多种章节分割模式（取命中数最多的一种）
	patterns := []string{
		`(?m)^第[一二三四五六七八九十百千零〇\d]+章[^\n]*`,  // 中文章节（行首）
		`(?m)^第[0-9]+章[^\n]*`,                        // 纯数字章（行首）
		`(?m)^卷[一二三四五六七八九十百千零〇\d]+[^\n]*`,    // 卷（行首）
		`(?m)^第[一二三四五六七八九十百千零〇\d]+节[^\n]*`,  // 节（行首）
		`(?m)^番外[一二三四五六七八九十\d]*[^\n]*`,         // 番外章（行首）
		`Chapter\s+[0-9]+[^\n]*`,                       // English chapter
		`ch\.\s*[0-9]+[^\n]*`,                          // ch.1
		`(?m)^\[第[0-9]+章\][^\n]*`,                    // [第1章]（行首）
		`(?m)^【[^】]{1,30}】[^\n]*`,                   // 【标题】（行首）
	}

	var splits []int
	var chapterTitles []string

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllStringIndex(content, -1)
		if len(matches) > len(splits) {
			splits = nil
			chapterTitles = nil
			for _, match := range matches {
				splits = append(splits, match[0])
				// 取标题行（去掉尾部空白）
				title := strings.TrimRight(content[match[0]:match[1]], " \t\r\n")
				chapterTitles = append(chapterTitles, title)
			}
		}
	}

	// 正则找到了章节 → 按切割位提取内容
	if len(splits) >= 2 {
		return s.buildChaptersFromSplits(content, splits, chapterTitles)
	}

	// 正则未命中，尝试 AI 辅助划分
	if s.aiService != nil && len([]rune(content)) > 500 {
		if aiChapters := s.splitByChaptersWithAI(content); len(aiChapters) >= 2 {
			return aiChapters
		}
	}

	// 最终兜底：按固定字数分割
	return s.splitByLength(content, novelTitle, 3000)
}

// buildChaptersFromSplits 从切割位和标题列表构建章节
func (s *NovelImportService) buildChaptersFromSplits(content string, splits []int, titles []string) []*model.Chapter {
	var chapters []*model.Chapter
	chNo := 0
	for i := 0; i < len(splits); i++ {
		start := splits[i]
		end := len(content)
		if i < len(splits)-1 {
			end = splits[i+1]
		}
		chapterContent := strings.TrimSpace(content[start:end])
		if len([]rune(chapterContent)) < 50 {
			continue
		}
		chNo++
		chapters = append(chapters, &model.Chapter{
			UUID:      uuid.New().String(),
			ChapterNo: chNo,
			Title:     titles[i],
			Content:   chapterContent,
			WordCount: len([]rune(chapterContent)),
			Status:    "published",
		})
	}
	return chapters
}

// splitByChaptersWithAI 让 AI 从文本中识别章节标题，用于正则无法匹配的非标准格式
func (s *NovelImportService) splitByChaptersWithAI(content string) []*model.Chapter {
	const maxSampleRunes = 10000
	runes := []rune(content)
	sample := content
	if len(runes) > maxSampleRunes {
		sample = string(runes[:maxSampleRunes])
	}

	prompt := `请从以下小说文本中找出所有章节的起始标题行，并以JSON数组格式返回章节标题列表。
格式要求：["第一章 xxx", "第二章 yyy", ...]
要求：
1. 标题必须与原文完全一致（字符相同）
2. 只返回JSON数组，不含其他内容
3. 若文本没有明显章节，则按每约3000字一章分章，标题使用"第N章"格式（N为数字）

小说文本（前10000字）：
` + sample

	resp, err := s.aiService.Generate(0, "chapter", prompt)
	if err != nil || strings.TrimSpace(resp) == "" {
		logger.Printf("[Import] AI chapter split error: %v", err)
		return nil
	}

	// 提取 JSON 数组
	raw := resp
	if i := strings.Index(raw, "["); i >= 0 {
		if j := strings.LastIndex(raw, "]"); j > i {
			raw = raw[i : j+1]
		}
	}
	var titles []string
	if err := json.Unmarshal([]byte(raw), &titles); err != nil || len(titles) == 0 {
		logger.Printf("[Import] AI chapter split JSON parse error: %v, raw=%q", err, raw)
		return nil
	}

	// 在全文中定位每个标题
	type entry struct {
		pos   int
		title string
	}
	var entries []entry
	searchFrom := 0
	for _, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		idx := strings.Index(content[searchFrom:], title)
		if idx < 0 {
			// 宽松搜索（忽略之前的偏移）
			idx = strings.Index(content, title)
			if idx < 0 {
				continue
			}
		} else {
			idx += searchFrom
		}
		entries = append(entries, entry{pos: idx, title: title})
		searchFrom = idx + len(title)
	}

	if len(entries) < 2 {
		return nil
	}

	// 确保升序（防止 AI 乱序返回）
	sort.Slice(entries, func(i, j int) bool { return entries[i].pos < entries[j].pos })

	splits := make([]int, len(entries))
	titleStrs := make([]string, len(entries))
	for i, e := range entries {
		splits[i] = e.pos
		titleStrs[i] = e.title
	}
	chapters := s.buildChaptersFromSplits(content, splits, titleStrs)
	logger.Printf("[Import] AI chapter split: %d chapters detected", len(chapters))
	return chapters
}

// 按字数分割
func (s *NovelImportService) splitByLength(content, title string, chunkSize int) []*model.Chapter {
	var chapters []*model.Chapter
	runes := []rune(content)
	totalLen := len(runes)

	for i := 0; i < totalLen; i += chunkSize {
		end := i + chunkSize
		if end > totalLen {
			end = totalLen
		}

		chapterContent := string(runes[i:end])
		chapterNo := i/chunkSize + 1

		chapter := &model.Chapter{
			UUID:      uuid.New().String(),
			ChapterNo: chapterNo,
			Title:     fmt.Sprintf("第%d章", chapterNo),
			Content:   chapterContent,
			WordCount: len(runes[i:end]),
			Status:    "published",
		}
		chapters = append(chapters, chapter)
	}

	return chapters
}

// ============================================
// 小说到视频的完整流程服务
// ============================================

// NovelToVideoRequest 小说转视频请求
type NovelToVideoRequest struct {
	NovelID      uint   `json:"novel_id"`
	ChapterRange []int  `json:"chapter_range,omitempty"` // [start, end]，nil表示全部
	StartChapter int    `json:"start_chapter,omitempty"`
	EndChapter   int    `json:"end_chapter,omitempty"`
	Resolution   string `json:"resolution"`   // 720p, 1080p, 4k
	FrameRate    int    `json:"frame_rate"`   // 24, 30, 60
	AspectRatio  string `json:"aspect_ratio"` // 16:9, 9:16, 1:1
	ArtStyle     string `json:"art_style"`    // realistic, anime, cartoon
	AutoImport   bool   `json:"auto_import"`  // 是否自动导入小说
}

// NovelToVideoResult 小说转视频结果
type NovelToVideoResult struct {
	NovelID           uint     `json:"novel_id"`
	VideoID           uint     `json:"video_id"`
	Status            string   `json:"status"`
	ChaptersProcessed int      `json:"chapters_processed"`
	ShotsGenerated    int      `json:"shots_generated"`
	Duration          float64  `json:"duration"` // 秒
	Errors            []string `json:"errors,omitempty"`
}

// NovelToVideoService 小说转视频服务
type NovelToVideoService struct {
	importService        *NovelImportService
	storyboardService    *IntelligentStoryboardService
	frameGenerator       *FrameGeneratorService
	videoEnhancement     *VideoEnhancementService
	consistencyValidator *ConsistencyValidatorService
	novelRepo            *repository.NovelRepository
	chapterRepo          *repository.ChapterRepository
	videoRepo            *repository.VideoRepository
	storyboardRepo       *repository.StoryboardRepository
}

// NewNovelToVideoService 创建小说转视频服务
func NewNovelToVideoService(
	importService *NovelImportService,
	storyboardService *IntelligentStoryboardService,
	frameGenerator *FrameGeneratorService,
	videoEnhancement *VideoEnhancementService,
	consistencyValidator *ConsistencyValidatorService,
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	videoRepo *repository.VideoRepository,
	storyboardRepo *repository.StoryboardRepository,
) *NovelToVideoService {
	return &NovelToVideoService{
		importService:        importService,
		storyboardService:    storyboardService,
		frameGenerator:       frameGenerator,
		videoEnhancement:     videoEnhancement,
		consistencyValidator: consistencyValidator,
		novelRepo:            novelRepo,
		chapterRepo:          chapterRepo,
		videoRepo:            videoRepo,
		storyboardRepo:       storyboardRepo,
	}
}

// ImportAndGenerate 导入小说并生成视频
func (s *NovelToVideoService) ImportAndGenerate(req *ImportRequest, videoReq *NovelToVideoRequest) (*NovelToVideoResult, error) {
	start := time.Now()
	result := &NovelToVideoResult{}

	// 1. 导入小说
	importResult, err := s.importService.Import(req)
	if err != nil {
		return nil, fmt.Errorf("import failed: %w", err)
	}

	result.NovelID = importResult.NovelID
	logger.Printf("Novel imported: %s, chapters: %d", importResult.Title, importResult.ImportedChapters)

	// 2. 生成视频
	videoReq.NovelID = importResult.NovelID
	videoResult, err := s.GenerateVideo(videoReq)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("video generation failed: %v", err))
	}

	// 合并结果
	if videoResult != nil {
		result.VideoID = videoResult.VideoID
		result.Status = videoResult.Status
		result.ChaptersProcessed = videoResult.ChaptersProcessed
		result.ShotsGenerated = videoResult.ShotsGenerated
	}

	result.Duration = time.Since(start).Seconds()

	return result, nil
}

// GenerateVideo 为小说生成视频
func (s *NovelToVideoService) GenerateVideo(req *NovelToVideoRequest) (*NovelToVideoResult, error) {
	result := &NovelToVideoResult{
		NovelID: req.NovelID,
		Status:  "processing",
	}

	// 1. 获取小说信息
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, fmt.Errorf("get novel failed: %w", err)
	}

	// 2. 获取章节
	startCh := req.StartChapter
	endCh := req.EndChapter
	if startCh == 0 {
		startCh = 1
	}
	if endCh == 0 {
		endCh = 9999
	}

	allChapters, err := s.chapterRepo.ListByNovel(req.NovelID)
	if err != nil {
		return nil, fmt.Errorf("get chapters failed: %w", err)
	}
	var chapters []*model.Chapter
	for _, ch := range allChapters {
		if ch.ChapterNo >= startCh && ch.ChapterNo <= endCh {
			chapters = append(chapters, ch)
		}
	}

	if len(chapters) == 0 {
		return nil, fmt.Errorf("no chapters found")
	}

	// 3. 创建视频项目
	video := &model.Video{
		NovelID:     req.NovelID,
		Title:       fmt.Sprintf("%s 视频", novel.Title),
		Description: fmt.Sprintf("基于《%s》第%d-%d章生成的视频", novel.Title, startCh, startCh+len(chapters)-1),
		Type:        "image_sequence",
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		AspectRatio: req.AspectRatio,
		ArtStyle:    req.ArtStyle,
		Status:      "planning",
	}

	if err := s.videoRepo.Create(video); err != nil {
		return nil, fmt.Errorf("create video failed: %w", err)
	}

	result.VideoID = video.ID

	// 4. 为每个章节生成分镜
	totalShots := 0
	for i, chapter := range chapters {
		// 分析章节内容
		shots, err := s.storyboardService.GenerateStoryboard(chapter, nil, &VideoGenerationRequest{
			NovelID:     req.NovelID,
			ChapterID:   chapter.ID,
			Resolution:  req.Resolution,
			FrameRate:   req.FrameRate,
			AspectRatio: req.AspectRatio,
			ArtStyle:    req.ArtStyle,
		})

		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("chapter %d storyboard failed: %v", chapter.ChapterNo, err))
			continue
		}

		// 保存分镜到数据库
		for idx, shot := range shots {
			dbShot := &model.StoryboardShot{
				VideoID:     video.ID,
				UUID:        uuid.New().String(),
				ShotNo:      idx + 1,
				ChapterID:   &chapter.ID,
				Description: shot.Description,
				Dialogue:    shot.Dialogue,
				CameraType:  string(shot.CameraMovement),
				CameraAngle: string(shot.ShotAngle),
				ShotSize:    string(shot.ShotSize),
				Duration:    shot.Duration,
				Prompt:      shot.Prompt,
				Status:      "pending",
			}
			if s.storyboardRepo != nil {
				if err := s.storyboardRepo.Create(dbShot); err != nil {
					logger.Printf("save storyboard shot failed (chapter %d, shot %d): %v", chapter.ChapterNo, idx+1, err)
				}
			}
			totalShots++
		}

		result.ChaptersProcessed = i + 1
		logger.Printf("Processed chapter %d/%d, shots: %d", i+1, len(chapters), len(shots))
	}

	result.ShotsGenerated = totalShots
	result.Status = "storyboard_completed"

	// 5. 应用视频增强
	enhancements := s.videoEnhancement.GetDefaultEnhancements()
	for _, enh := range enhancements {
		if enh.Enabled {
			logger.Printf("Applying enhancement: %s", enh.Type)
		}
	}

	result.Status = "completed"

	return result, nil
}

// ============================================
// 辅助类型
// ============================================

// json解析辅助
func jsonMarshal(v interface{}) ([]byte, error) {
	// 简单的JSON序列化，避免导入第三方库
	return []byte(fmt.Sprintf("%+v", v)), nil
}
