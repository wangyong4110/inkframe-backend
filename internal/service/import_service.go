package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"gorm.io/gorm"

	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/inkframe/inkframe-backend/internal/metrics"
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
	Source      ImportSource        `json:"source"`
	URL         string              `json:"url,omitempty"`       // 导入URL
	FileData    []byte              `json:"file_data,omitempty"` // 文件数据
	FileName    string              `json:"file_name,omitempty"` // 文件名
	Format      ImportFormat        `json:"format,omitempty"`    // 文件格式
	SiteName    string              `json:"site_name,omitempty"` // 站点名称（爬取时）
	NovelID     uint                `json:"novel_id,omitempty"`  // 已有小说ID（追加时）
	TenantID    uint                `json:"tenant_id,omitempty"` // 租户ID（用于去重）
	UserID      uint                `json:"user_id,omitempty"`   // 发起导入的用户ID（用于站内通知）
	CrawlConfig *crawler.CrawlConfig `json:"-"`                  // optional per-crawl settings
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
	Duplicate        bool     `json:"duplicate,omitempty"` // 是否为重复导入（内容哈希匹配）
	Message          string   `json:"message,omitempty"`   // 附加说明（如重复导入提示）
}

// CrawlProgress 爬取进度
type CrawlProgress struct {
	mu      sync.RWMutex
	NovelID uint   `json:"novel_id"`
	Status  string `json:"status"`   // running / paused / completed / failed
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Current string `json:"current"` // 当前正在爬取的章节标题
	// Populated from crawler stats
	BytesDownloaded int64   `json:"bytes_downloaded"`
	PagesVisited    int     `json:"pages_visited"`
	ElapsedSecs     float64 `json:"elapsed_secs"`
	SpeedCPS        float64 `json:"speed_cps"` // chars crawled per second
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
	notifSvc           *NotificationService // 可选，用于爬取完成通知
	crawlJobRepo       *repository.NovelCrawlJobRepository
	cache              *redis.Client // optional: cross-instance crawl progress sharing
	crawlProgress      sync.Map // novelID(uint) → *CrawlProgress
	crawlDoneCallbacks sync.Map // novelID(uint) → func()
	db                 *gorm.DB // optional: used for transactional novel+chapter creation
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

// WithNotificationService 注入通知服务（可选，用于爬取完成后发送站内通知）
func (s *NovelImportService) WithNotificationService(svc *NotificationService) *NovelImportService {
	s.notifSvc = svc
	return s
}

// WithCrawlJobRepo 注入爬取任务仓库（可选，用于持久化爬取进度）
func (s *NovelImportService) WithCrawlJobRepo(repo *repository.NovelCrawlJobRepository) *NovelImportService {
	s.crawlJobRepo = repo
	return s
}

// WithDB 注入 DB（可选，用于小说+章节事务性创建）
func (s *NovelImportService) WithDB(db *gorm.DB) *NovelImportService {
	s.db = db
	return s
}

// WithRedis injects a Redis client for cross-instance crawl progress sharing.
func (s *NovelImportService) WithRedis(c *redis.Client) *NovelImportService {
	s.cache = c
	return s
}

// crawlProgressRedisKey returns the Redis key for crawl progress of a novel.
func crawlProgressRedisKey(novelID uint) string {
	return fmt.Sprintf("crawl:progress:%d", novelID)
}

// storeCrawlProgressToRedis writes the current progress snapshot to Redis (best-effort).
func (s *NovelImportService) storeCrawlProgressToRedis(p *CrawlProgress) {
	if s.cache == nil {
		return
	}
	p.mu.RLock()
	b, err := json.Marshal(p)
	p.mu.RUnlock()
	if err != nil {
		return
	}
	s.cache.Set(context.Background(), crawlProgressRedisKey(p.NovelID), b, 2*time.Hour) //nolint:errcheck
}

const crawlDonePubSubChannel = "inkframe:crawl:done"

// StartCrawlDoneSubscriber 启动 Redis Pub/Sub 订阅，接收其他实例发出的爬取完成事件并触发本地回调。
// 应在 WithRedis 注入之后调用（Redis 不可用时为空操作）。
func (s *NovelImportService) StartCrawlDoneSubscriber(ctx context.Context) {
	if s.cache == nil {
		return
	}
	go func() {
		sub := s.cache.Subscribe(ctx, crawlDonePubSubChannel)
		defer sub.Close()
		for msg := range sub.Channel() {
			var novelID uint
			if _, err := fmt.Sscan(msg.Payload, &novelID); err != nil {
				continue
			}
			if fn, ok := s.crawlDoneCallbacks.LoadAndDelete(novelID); ok {
				fn.(func())()
			}
		}
	}()
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
		logger.Errorf("[Import] OSS upload failed for novel %d: %v", novelID, err)
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

// GetCrawlProgress 获取爬取进度（先查本地内存，再查 Redis，最后查数据库）
func (s *NovelImportService) GetCrawlProgress(novelID uint) (*CrawlProgress, error) {
	if v, ok := s.crawlProgress.Load(novelID); ok {
		p := v.(*CrawlProgress)
		p.mu.RLock()
		cp := &CrawlProgress{
			NovelID:         p.NovelID,
			Status:          p.Status,
			Total:           p.Total,
			Done:            p.Done,
			Failed:          p.Failed,
			Current:         p.Current,
			BytesDownloaded: p.BytesDownloaded,
			PagesVisited:    p.PagesVisited,
			ElapsedSecs:     p.ElapsedSecs,
			SpeedCPS:        p.SpeedCPS,
		}
		p.mu.RUnlock()
		return cp, nil
	}
	// 本地无记录，尝试从 Redis 读取（另一实例正在爬取）
	if s.cache != nil {
		if raw, err := s.cache.Get(context.Background(), crawlProgressRedisKey(novelID)).Bytes(); err == nil {
			var cp CrawlProgress
			if json.Unmarshal(raw, &cp) == nil {
				return &cp, nil
			}
		}
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
	parser, err := s.getParserForURL(firstURL, "")
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

	crawlCtx, crawlCancel := context.WithTimeout(context.Background(), 24*time.Hour)
	go func() {
		defer crawlCancel()
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[CrawlChapters] panic recovered novelID=%d: %v", novelID, r)
				// CrawlJobsInFlight.Dec() fires via defer in crawlChaptersBackground before panic propagates.
				// Only record the failed job count here.
				metrics.CrawlJobsTotal.WithLabelValues("failed").Inc()
			}
		}()
		s.crawlChaptersBackground(crawlCtx, novelID, novel.Title, parser, pending, progress, novel.TenantID, 0)
	}()
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
	tenantID uint,
	userID uint,
) {
	metrics.CrawlJobsInFlight.Inc()
	defer metrics.CrawlJobsInFlight.Dec()

	// summarySem：AI 摘要异步化——最多 3 并发，主循环不阻塞（fire-and-forget）
	summarySem := make(chan struct{}, 3)

	// 创建 DB 爬取任务记录，获取 jobID 用于后续进度持久化
	var jobID uint
	if s.crawlJobRepo != nil {
		job := &model.NovelCrawlJob{
			NovelID:    novelID,
			Status:     "running",
			Progress:   0,
			TotalChaps: progress.Total,
		}
		if err := s.crawlJobRepo.Create(job); err != nil {
			logger.Errorf("[Crawl] create novel crawl job failed: %v", err)
		} else {
			jobID = job.ID
		}
	}
	// Build fetch list from chapter stubs (those with a "crawl:" URL in Outline)
	var toFetch []*crawler.ChapterInfo
	var modelChapters []*model.Chapter
	for _, ch := range chapters {
		chURL := strings.TrimPrefix(ch.Outline, "crawl:")
		if chURL == "" {
			progress.mu.Lock()
			progress.Done++
			progress.mu.Unlock()
			continue
		}
		toFetch = append(toFetch, &crawler.ChapterInfo{
			Title: ch.Title, URL: chURL, ChapterNo: ch.ChapterNo,
		})
		modelChapters = append(modelChapters, ch)
	}

	for r := range s.crawler.FetchChapterStream(ctx, toFetch, parser) {
		ch := modelChapters[r.Index]

		progress.mu.Lock()
		progress.Current = ch.Title
		progress.mu.Unlock()

		if r.Err != nil || r.Content == nil || r.Content.Content == "" {
			chURL := strings.TrimPrefix(ch.Outline, "crawl:")
			if r.Err != nil {
				logger.Errorf("[Crawl] chapter %d fetch error: %v (url=%s)", ch.ChapterNo, r.Err, chURL)
			} else {
				logger.Errorf("[Crawl] chapter %d content empty after parse (url=%s) — likely JS-rendered page or parser mismatch", ch.ChapterNo, chURL)
			}
			metrics.CrawlChaptersTotal.WithLabelValues("error").Inc()
			progress.mu.Lock()
			progress.Failed++
			progress.mu.Unlock()
			continue
		}

		title := ch.Title
		if strings.TrimSpace(r.Content.Title) != "" {
			title = strings.TrimSpace(r.Content.Title)
		}
		wordCount := len([]rune(r.Content.Content))

		if err := s.chapterRepo.UpdateCrawledContent(ch.ID, title, r.Content.Content, wordCount); err != nil {
			logger.Errorf("[Crawl] chapter %d save error: %v", ch.ChapterNo, err)
			metrics.CrawlChaptersTotal.WithLabelValues("error").Inc()
			progress.mu.Lock()
			progress.Failed++
			progress.mu.Unlock()
			continue
		}

		metrics.CrawlChaptersTotal.WithLabelValues("success").Inc()

		progress.mu.Lock()
		progress.Done++
		doneSoFar := progress.Done
		totalSoFar := progress.Total
		failedSoFar := progress.Failed
		progress.mu.Unlock()
		logger.Printf("[Crawl] novel=%d chapter=%d/%d done", novelID, doneSoFar, totalSoFar)

		// Persist progress after every chapter so any instance can serve GetCrawlProgress.
		if jobID > 0 && s.crawlJobRepo != nil {
			_ = s.crawlJobRepo.UpdateProgress(jobID, doneSoFar, totalSoFar, failedSoFar)
		}
		stats := s.crawler.GetStats()
		elapsed := stats.ElapsedSeconds()
		var speedCPS float64
		if elapsed > 0 {
			speedCPS = float64(stats.BytesDownloaded) / elapsed
		}
		progress.mu.Lock()
		progress.BytesDownloaded = stats.BytesDownloaded
		progress.PagesVisited = stats.PagesVisited
		progress.ElapsedSecs = elapsed
		progress.SpeedCPS = speedCPS
		progress.mu.Unlock()
		// Sync progress to Redis so other instances can observe it.
		s.storeCrawlProgressToRedis(progress)

		// AI 摘要：异步 fire-and-forget，不阻塞主爬取循环
		// 主循环只负责拉 channel，不等 AI；摘要后台写入 DB（最多 3 并发）
		if s.narrativeMemory != nil {
			chCopy := *ch
			chCopy.Title = title
			chCopy.Content = r.Content.Content
			nm := s.narrativeMemory
			repo := s.chapterRepo
			nTitle := novelTitle
			go func() {
				summarySem <- struct{}{}
				defer func() { <-summarySem }()
				summary, summaryErr := nm.GenerateChapterSummary(0, &chCopy, nTitle)
				if summaryErr != nil {
					time.Sleep(2 * time.Second)
					summary, summaryErr = nm.GenerateChapterSummary(0, &chCopy, nTitle)
					if summaryErr != nil {
						logger.Errorf("[Crawl] chapter %d summary failed (skipped): %v", chCopy.ChapterNo, summaryErr)
						return
					}
				}
				if summary != "" {
					chCopy.Summary = summary
					if updateErr := repo.Update(&chCopy); updateErr != nil {
						logger.Errorf("[Crawl] chapter %d summary save failed: %v", chCopy.ChapterNo, updateErr)
					}
				}
			}()
		}
	}

	// 收尾
	progress.mu.Lock()
	if progress.Failed == 0 {
		progress.Status = "completed"
		s.novelRepo.UpdateFields(novelID, map[string]interface{}{"status": "writing"}) //nolint:errcheck
	} else if progress.Done == 0 {
		progress.Status = "failed"
	} else {
		progress.Status = "completed"
		s.novelRepo.UpdateFields(novelID, map[string]interface{}{"status": "writing"}) //nolint:errcheck
	}
	progress.Current = ""
	finalDone := progress.Done
	finalFailed := progress.Failed
	finalStatus := progress.Status
	progress.mu.Unlock()
	logger.Printf("[Crawl] novel=%d finished: done=%d failed=%d status=%s", novelID, finalDone, finalFailed, finalStatus)
	// Persist final status to Redis (shorter TTL — result only needs to be readable briefly).
	s.storeCrawlProgressToRedis(progress)

	jobMetricStatus := finalStatus
	if finalStatus == "completed" && finalFailed > 0 {
		jobMetricStatus = "partial"
	}
	metrics.CrawlJobsTotal.WithLabelValues(jobMetricStatus).Inc()

	// 持久化最终状态到 DB
	if jobID > 0 && s.crawlJobRepo != nil {
		dbStatus := finalStatus
		if finalStatus == "completed" && finalFailed > 0 {
			dbStatus = "partial"
		}
		_ = s.crawlJobRepo.Finalize(jobID, dbStatus, finalDone, finalDone+finalFailed, finalFailed)
	}

	// 将全本内容合并上传到 OSS 备份
	if s.storageSvc != nil && finalStatus == "completed" {
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
	// 先触发本实例的本地回调（即时无延迟）
	if fn, ok := s.crawlDoneCallbacks.LoadAndDelete(novelID); ok {
		fn.(func())()
	}
	// 同时广播到其他实例（跨实例场景：回调注册在不同实例时触发）
	if s.cache != nil {
		s.cache.Publish(context.Background(), crawlDonePubSubChannel, fmt.Sprintf("%d", novelID)) //nolint:errcheck
	}

}

// getParserForURL 根据 URL（或 siteName 提示）返回对应解析器
func (s *NovelImportService) getParserForURL(url string, siteName string) (crawler.NovelParser, error) {
	site := s.detectSiteFromURL(url)
	if site == "default" && siteName != "" {
		// 用户显式指定了站点：校验 URL 域名与站点是否匹配，防止解析器错用。
		sn := strings.ToLower(siteName)
		if expected, ok := siteExpectedDomain[sn]; ok {
			if !strings.Contains(strings.ToLower(url), expected) {
				return nil, fmt.Errorf("URL 域名与所选站点不匹配：所选站点 %q 期望域名含 %q，但 URL 为 %s", sn, expected, url)
			}
		}
		site = sn
	}
	switch site {
	case "qidian":
		return crawler.NewQidianParser(), nil
	case "jjwxc":
		return crawler.NewJjwxcParser(), nil
	case "zongheng":
		return crawler.NewZonghengParser(), nil
	case "qimao":
		return crawler.NewQimaoParser(), nil
	case "hongxiu":
		return crawler.NewHongxiuParser(), nil
	default:
		// 未知站点：降级到通用解析器（传入书目 URL 用于链接前缀过滤）
		return crawler.NewGenericParserWithURL(url), nil
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

	// 文件内容哈希去重：同 tenant 下同一文件内容不重复导入
	// TODO: wrap in transaction for atomicity (the check-then-create is a TOCTOU race under concurrent imports)
	var fileHashHex string
	if len(req.FileData) > 0 && req.TenantID > 0 && req.NovelID == 0 && s.db != nil {
		hash := sha256.Sum256(req.FileData)
		fileHashHex = fmt.Sprintf("%x", hash)
		var existingNovel model.Novel
		if err := s.db.Where("tenant_id = ? AND source_file_hash = ?", req.TenantID, fileHashHex).First(&existingNovel).Error; err == nil {
			logger.Printf("[Import] duplicate file hash detected for tenant %d: novel %d (%s)", req.TenantID, existingNovel.ID, existingNovel.Title)
			return &ImportResult{
				NovelID:          existingNovel.ID,
				Title:            existingNovel.Title,
				ImportedChapters: existingNovel.ChapterCount,
				TotalChapters:    existingNovel.ChapterCount,
				Duplicate:        true,
				Message:          "file already imported as novel: " + existingNovel.Title,
			}, nil
		}
	}

	// 解析内容
	var novel *model.Novel
	var chapters []*model.Chapter
	var err error

	switch format {
	case FormatTxt:
		novel, chapters, err = s.parseTxtFile(req.FileData, req.FileName, req.TenantID)
	case FormatMd:
		novel, chapters, err = s.parseMarkdownFile(req.FileData, req.FileName, req.TenantID)
	case FormatJson:
		novel, chapters, err = s.parseJsonFile(req.FileData, req.TenantID)
	case FormatHtml:
		novel, chapters, err = s.parseHtmlFile(req.FileData, req.TenantID)
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
				// 重复上传检测：若文件的章节数 ≤ 已有章节数，说明是同一文件重复上传，
				// 直接返回已有小说，避免每次上传都追加重复章节。
				if chapterOffset >= len(chapters) {
					result.NovelID = novel.ID
					result.Title = novel.Title
					result.ImportedChapters = int(count)
					result.TotalChapters = int(count)
					logger.Printf("[Import] novel %q (id=%d) re-upload detected (%d chapters already present, file has %d), skipping", novel.Title, novel.ID, chapterOffset, len(chapters))
					_ = s.novelRepo.SyncStats(novel.ID)
					return result, nil
				}
				logger.Printf("[Import] novel %q already exists (id=%d), appending chapters from offset %d", novel.Title, novel.ID, chapterOffset)
			}
		}
		// 仍需新建（无同名小说）
		if novel.ID == 0 {
			novel.UUID = uuid.New().String()
			novel.TenantID = req.TenantID
			if fileHashHex != "" {
				novel.SourceFileHash = fileHashHex
			}
			// 准备章节数据（需要 novel.ID，所以必须先创建 novel）。
			// 如果有 DB 连接，使用事务确保 novel + chapters 原子性写入，
			// 避免 novel 创建成功但 chapters 批量插入失败时出现孤立 novel 记录。
			if s.db != nil {
				txErr := s.db.Transaction(func(tx *gorm.DB) error {
					if err := tx.Create(novel).Error; err != nil {
						return fmt.Errorf("save novel failed: %w", err)
					}
					for i, chapter := range chapters {
						chapter.NovelID = novel.ID
						chapter.ChapterNo = i + 1
						if chapter.UUID == "" {
							chapter.UUID = uuid.New().String()
						}
					}
					if err := tx.CreateInBatches(chapters, 100).Error; err != nil {
						return fmt.Errorf("batch create chapters failed: %w", err)
					}
					return nil
				})
				if txErr != nil {
					return nil, txErr
				}
				result.NovelID = novel.ID
				result.Title = novel.Title
				result.TotalChapters = len(chapters)
				result.ImportedChapters = len(chapters)
				logger.Printf("[Import] novel=%d batch created %d chapters (tx)", novel.ID, len(chapters))
				_ = s.novelRepo.SyncStats(novel.ID)
				return result, nil
			}
			// No DB injected — fall back to original non-transactional path
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
			logger.Errorf("[Import] novel=%d batch create failed: %v", novel.ID, err)
		} else {
			result.ImportedChapters = len(chapters)
			logger.Printf("[Import] novel=%d batch created %d chapters", novel.ID, len(chapters))
		}
		_ = s.novelRepo.SyncStats(novel.ID)
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
					logger.Errorf("[Import] novel=%d chapter %d update failed: %v", novel.ID, chapter.ChapterNo, err)
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
			logger.Errorf("[Import] novel=%d batch create %d chapters failed: %v", novel.ID, len(toCreate), err)
		} else {
			result.ImportedChapters += len(toCreate)
			logger.Printf("[Import] novel=%d batch created %d new chapters", novel.ID, len(toCreate))
		}
	}

	_ = s.novelRepo.SyncStats(novel.ID)
	return result, nil
}

// validateImportURL 验证导入 URL 防止 SSRF 攻击
func validateImportURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https URLs are allowed, got %q", u.Scheme)
	}
	host := u.Hostname()
	// 拒绝私有/内网地址
	ip := net.ParseIP(host)
	if ip != nil {
		private := []string{"10.", "172.16.", "172.17.", "172.18.", "172.19.", "172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.", "172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "127.", "169.254.", "::1", "fc", "fd"}
		ipStr := ip.String()
		for _, prefix := range private {
			if strings.HasPrefix(ipStr, prefix) {
				return fmt.Errorf("requests to private/internal IP addresses are not allowed")
			}
		}
	}
	if host == "localhost" || host == "0.0.0.0" {
		return fmt.Errorf("requests to localhost are not allowed")
	}
	return nil
}

// 从URL导入
func (s *NovelImportService) importFromURL(req *ImportRequest) (*ImportResult, error) {
	if err := validateImportURL(req.URL); err != nil {
		return nil, fmt.Errorf("URL validation failed: %w", err)
	}
	// 下载内容
	resp, err := http.Get(req.URL) //nolint:gosec // URL already validated above
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	const maxImportSize = 50 * 1024 * 1024 // 50MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImportSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxImportSize {
		return nil, fmt.Errorf("response too large (max 50MB)")
	}
	data := body

	// 解析内容
	req.FileData = data
	req.FileName = req.URL

	return s.importFromFile(req)
}

// 从爬取导入
func (s *NovelImportService) importFromCrawl(req *ImportRequest) (*ImportResult, error) {
	if err := validateImportURL(req.URL); err != nil {
		return nil, fmt.Errorf("invalid import URL: %w", err)
	}

	result := &ImportResult{}

	parser, err := s.getParserForURL(req.URL, req.SiteName)
	if err != nil {
		return nil, err
	}

	// Apply per-request crawl config if provided
	if req.CrawlConfig != nil {
		s.crawler.SetConfig(*req.CrawlConfig)
	} else {
		s.crawler.SetConfig(crawler.DefaultCrawlConfig())
	}

	// 下载小说目录页
	root, err := s.crawler.FetchPageHTML(req.URL)
	if err != nil {
		return nil, fmt.Errorf("get page failed: %w", err)
	}

	// 解析小说详情
	detail, err := parser.ParseNovelDetail(root, req.URL)
	if err != nil {
		return nil, fmt.Errorf("parse novel detail failed: %w", err)
	}

	// 解析章节列表：优先使用 Ajax API（如解析器实现了 ChapterListFetcher），失败则回退 HTML 解析
	var chapterInfos []*crawler.ChapterInfo
	if fetcher, ok := parser.(crawler.ChapterListFetcher); ok {
		chapterInfos, err = fetcher.FetchChapterList(context.Background(), s.crawler.Jar(), req.URL)
		if err != nil {
			logger.Errorf("[Import] Ajax chapter list failed (%v), falling back to HTML parse", err)
			chapterInfos, err = parser.ParseChapterList(root)
		}
	} else {
		chapterInfos, err = parser.ParseChapterList(root)
	}
	if err != nil {
		return nil, fmt.Errorf("parse chapter list failed: %w", err)
	}

	const maxChaptersPerImport = 10000
	if len(chapterInfos) > maxChaptersPerImport {
		return nil, fmt.Errorf("chapter list too large: %d chapters (max %d)", len(chapterInfos), maxChaptersPerImport)
	}

	// 爬取不按标题自动匹配已有小说，避免同名 AI 小说被意外追加爬取内容。
	// 仅在调用方明确传入 novel_id 时才追加到已有项目。
	var novel *model.Novel
	chapterOffset := 0
	if req.NovelID > 0 {
		existing, err := s.novelRepo.GetByID(req.NovelID)
		if err != nil {
			return nil, fmt.Errorf("novel not found: %w", err)
		}
		if req.TenantID > 0 && existing.TenantID != req.TenantID {
			return nil, fmt.Errorf("novel does not belong to this tenant")
		}
		novel = existing
		count, _ := s.chapterRepo.CountByNovel(existing.ID)
		chapterOffset = int(count)
		logger.Printf("[Import] crawl: appending to novel %q (id=%d) from offset %d", novel.Title, novel.ID, chapterOffset)
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
	stubs := make([]*model.Chapter, 0, len(chapterInfos))
	for i, info := range chapterInfos {
		stubs = append(stubs, &model.Chapter{
			UUID:      uuid.New().String(),
			NovelID:   novel.ID,
			ChapterNo: chapterOffset + i + 1,
			Title:     info.Title,
			Content:   "",
			Outline:   "crawl:" + info.URL,
			Status:    "draft",
		})
	}
	var pendingChapters []*model.Chapter
	if err := s.chapterRepo.CreateInBatches(stubs, 100); err != nil {
		result.FailedChapters = len(stubs)
		result.Errors = append(result.Errors, fmt.Sprintf("batch create chapter stubs failed: %v", err))
	} else {
		result.ImportedChapters = len(stubs)
		pendingChapters = stubs
	}

	// 初始化进度并启动后台爬取
	progress := &CrawlProgress{
		NovelID: novel.ID,
		Status:  "running",
		Total:   len(pendingChapters),
		Done:    0,
	}
	s.crawlProgress.Store(novel.ID, progress)
	crawlCtx, crawlCancel := context.WithTimeout(context.Background(), 24*time.Hour)
	go func() {
		defer crawlCancel()
		s.crawlChaptersBackground(crawlCtx, novel.ID, novel.Title, parser, pendingChapters, progress, req.TenantID, req.UserID)
	}()

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
	if strings.Contains(url, "hongxiu.com") {
		return "hongxiu"
	}
	return "default"
}

// siteExpectedDomain 返回站点对应的域名关键字，用于 URL 与 site_name 一致性校验。
var siteExpectedDomain = map[string]string{
	"qidian":   "qidian.com",
	"jjwxc":    "jjwxc.net",
	"zongheng": "zongheng.com",
	"qimao":    "qimao.com",
	"hongxiu":  "hongxiu.com",
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
func (s *NovelImportService) parseTxtFile(data []byte, fileName string, tenantID uint) (*model.Novel, []*model.Chapter, error) {
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
	chapters := s.splitByChapters(content, title, tenantID)

	return novel, chapters, nil
}

// 解析Markdown文件
func (s *NovelImportService) parseMarkdownFile(data []byte, fileName string, tenantID uint) (*model.Novel, []*model.Chapter, error) {
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
	chapters := s.splitByChapters(fullContent, title, tenantID)

	return novel, chapters, nil
}

// 解析JSON文件
func (s *NovelImportService) parseJsonFile(data []byte, tenantID uint) (*model.Novel, []*model.Chapter, error) {
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
		return s.parseTxtFile(data, "imported.json", tenantID)
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
				Status:    "completed",
			})
		}
	} else if structured.Content != "" {
		// 扁平结构，按章节分割
		chapters = s.splitByChapters(structured.Content, novel.Title, tenantID)
	}

	return novel, chapters, nil
}

// 解析HTML文件
func (s *NovelImportService) parseHtmlFile(data []byte, tenantID uint) (*model.Novel, []*model.Chapter, error) {
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

	chapters := s.splitByChapters(cleanContent, title, tenantID)

	return novel, chapters, nil
}

// 去除HTML标签（含事件处理器、javascript: URL 等潜在 XSS 载体）
func (s *NovelImportService) stripHtmlTags(html string) string {
	// Remove script tags and their content
	scriptRe := regexp.MustCompile(`(?si)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")

	// Remove style tags
	styleRe := regexp.MustCompile(`(?si)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	// Remove ALL event handler attributes (on*)
	eventRe := regexp.MustCompile(`(?i)\s+on\w+\s*=\s*["'][^"']*["']`)
	html = eventRe.ReplaceAllString(html, "")

	// Remove javascript: URLs
	jsUrlRe := regexp.MustCompile(`(?i)(href|src|action)\s*=\s*["']\s*javascript:[^"']*["']`)
	html = jsUrlRe.ReplaceAllString(html, `$1="#"`)

	// Remove remaining HTML tags (keep text content)
	tagRe := regexp.MustCompile(`<[^>]*>`)
	content := tagRe.ReplaceAllString(html, "\n")

	// Decode HTML entities
	content = strings.ReplaceAll(content, "&amp;", "&")
	content = strings.ReplaceAll(content, "&lt;", "<")
	content = strings.ReplaceAll(content, "&gt;", ">")
	content = strings.ReplaceAll(content, "&quot;", `"`)
	content = strings.ReplaceAll(content, "&#39;", "'")
	content = strings.ReplaceAll(content, "&nbsp;", " ")

	// 清理多余空白
	lines := strings.Split(content, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 10 {
			cleaned = append(cleaned, line)
		}
	}

	return strings.TrimSpace(strings.Join(cleaned, "\n\n"))
}

// 按章节分割
func (s *NovelImportService) splitByChapters(content, novelTitle string, tenantID uint) []*model.Chapter {
	contentRunes := len([]rune(content))
	logger.Printf("[Import] splitByChapters: novelTitle=%q contentRunes=%d", novelTitle, contentRunes)
	if contentRunes == 0 {
		logger.Printf("[Import] splitByChapters: empty content, returning no chapters")
		return nil
	}
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
		logger.Printf("[Import] splitByChapters: regex found %d splits, building chapters", len(splits))
		return s.buildChaptersFromSplits(content, splits, chapterTitles)
	}

	// 正则仅命中 1 处：说明有章节标记但只有一章，直接作为单章返回
	if len(splits) == 1 {
		logger.Printf("[Import] splitByChapters: single chapter header found, treating as 1 chapter")
		return s.buildChaptersFromSplits(content, splits, chapterTitles)
	}

	// 正则未命中，无章节标记 → 如果文本超过 5000 字，按固定字数分割；否则整体作为一章
	const defaultChapterSize = 5000
	runes := []rune(content)
	if len(runes) > defaultChapterSize {
		logger.Printf("[Import] splitByChapters: no chapter markers found, splitting by %d chars (%d total runes)", defaultChapterSize, len(runes))
		var chapters []*model.Chapter
		chapterNo := 1
		for i := 0; i < len(runes); i += defaultChapterSize {
			end := i + defaultChapterSize
			if end > len(runes) {
				end = len(runes)
			}
			chunkContent := strings.TrimSpace(string(runes[i:end]))
			if len([]rune(chunkContent)) == 0 {
				continue
			}
			chapters = append(chapters, &model.Chapter{
				UUID:      uuid.New().String(),
				ChapterNo: chapterNo,
				Title:     fmt.Sprintf("第%d章", chapterNo),
				Content:   chunkContent,
				WordCount: len([]rune(chunkContent)),
				Status:    "completed",
			})
			chapterNo++
		}
		return chapters
	}
	logger.Printf("[Import] splitByChapters: no chapter markers found, treating entire content as 1 chapter")
	return []*model.Chapter{
		{
			UUID:      uuid.New().String(),
			ChapterNo: 1,
			Title:     novelTitle,
			Content:   strings.TrimSpace(content),
			WordCount: contentRunes,
			Status:    "completed",
		},
	}
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
			Status:    "completed",
		})
	}
	return chapters
}

// splitByChaptersWithAI 让 AI 从文本中识别章节标题，用于正则无法匹配的非标准格式
func (s *NovelImportService) splitByChaptersWithAI(content string, tenantID uint) []*model.Chapter {
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

	resp, err := s.aiService.GenerateWithProvider(tenantID, 0, "chapter", prompt, "", StoryboardOverrides{TimeoutSeconds: 30})
	if err != nil || strings.TrimSpace(resp) == "" {
		logger.Errorf("[Import] AI chapter split error: %v", err)
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
		logger.Errorf("[Import] AI chapter split JSON parse error: %v, raw=%q", err, raw)
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
			Status:    "completed",
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
					logger.Errorf("save storyboard shot failed (chapter %d, shot %d): %v", chapter.ChapterNo, idx+1, err)
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

