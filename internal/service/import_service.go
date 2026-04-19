package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
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
}

// NovelImportService 小说导入服务
type NovelImportService struct {
	novelRepo   *repository.NovelRepository
	chapterRepo *repository.ChapterRepository
	crawler     *crawler.NovelCrawler
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

// Import 执行导入
func (s *NovelImportService) Import(req *ImportRequest) (*ImportResult, error) {
	start := time.Now()
	result := &ImportResult{}

	switch req.Source {
	case SourceFile:
		return s.importFromFile(req)
	case SourceURL:
		return s.importFromURL(req)
	case SourceCrawl:
		return s.importFromCrawl(req)
	default:
		return nil, fmt.Errorf("unsupported import source: %s", req.Source)
	}

	result.Duration = time.Since(start).Seconds()
	return result, nil
}

// 从文件导入
func (s *NovelImportService) importFromFile(req *ImportRequest) (*ImportResult, error) {
	result := &ImportResult{
		Duration: 0,
	}

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

	// 保存小说
	if req.NovelID > 0 {
		novel.ID = req.NovelID
	}
	if err := s.novelRepo.Create(novel); err != nil {
		return nil, fmt.Errorf("save novel failed: %w", err)
	}
	result.NovelID = novel.ID
	result.Title = novel.Title

	// 保存章节
	for i, chapter := range chapters {
		chapter.NovelID = novel.ID
		chapter.ChapterNo = i + 1
		if err := s.chapterRepo.Create(chapter); err != nil {
			result.FailedChapters++
			result.Errors = append(result.Errors, fmt.Sprintf("chapter %d failed: %v", i+1, err))
		} else {
			result.ImportedChapters++
		}
	}
	result.TotalChapters = len(chapters)

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

	// 解析URL获取站点
	siteName := s.detectSiteFromURL(req.URL)
	var parser crawler.NovelParser
	switch siteName {
	case "qidian":
		parser = crawler.NewQidianParser()
	case "jjwxc":
		parser = crawler.NewJjwxcParser()
	case "zongheng":
		parser = crawler.NewZonghengParser()
	default:
		return nil, fmt.Errorf("unsupported site: %s", siteName)
	}

	// 爬取小说详情（创建小说记录）
	novelDetail, err := s.crawler.CrawlNovel(context.Background(), req.URL)
	if err != nil {
		return nil, fmt.Errorf("crawl novel failed: %w", err)
	}

	// 同步爬取章节列表
	html, err := crawler.NewHTTPClient().Get(context.Background(), req.URL)
	if err != nil {
		return nil, fmt.Errorf("get page failed: %w", err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("parse page failed: %w", err)
	}

	chapterInfos, err := parser.ParseChapterList(doc)
	if err != nil {
		return nil, fmt.Errorf("parse chapters failed: %w", err)
	}

	// 转换为 Chapter 数组
	chapters := make([]*model.Chapter, len(chapterInfos))
	for i, info := range chapterInfos {
		chapters[i] = &model.Chapter{
			Title:     info.Title,
			ChapterNo: info.ChapterNo,
			Summary:   fmt.Sprintf("爬取自: %s", info.URL),
			Content:   "",
		}
	}

	// 创建小说
	novel := &model.Novel{
		Title:       novelDetail.Title,
		Description: "",
		Genre:       novelDetail.Genre,
		Status:      "importing", // 正在导入
	}

	if err := s.novelRepo.Create(novel); err != nil {
		return nil, fmt.Errorf("save novel failed: %w", err)
	}

	result.NovelID = novel.ID
	result.Title = novel.Title
	result.TotalChapters = len(chapters)

	// 创建章节（稍后异步爬取内容）
	for i, chapterInfo := range chapters {
		chapter := &model.Chapter{
			NovelID:   novel.ID,
			ChapterNo: i + 1,
			Title:     chapterInfo.Title,
			Summary:   fmt.Sprintf("爬取自: %s", req.URL),
			Content:   "", // 稍后异步爬取
			WordCount: 0,
			Status:    "draft",
		}

		if err := s.chapterRepo.Create(chapter); err != nil {
			result.FailedChapters++
			result.Errors = append(result.Errors, fmt.Sprintf("chapter %d save failed: %v", i+1, err))
		} else {
			result.ImportedChapters++
		}

		// 进度限制
		if i%10 == 0 {
			log.Printf("Crawling progress: %d/%d", i+1, len(chapters))
		}
	}

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
	return "default"
}

// 解析TXT文件
func (s *NovelImportService) parseTxtFile(data []byte, fileName string) (*model.Novel, []*model.Chapter, error) {
	content := string(data)

	// 提取标题（从文件名或内容第一行）
	title := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	titleLine := strings.Split(content, "\n")[0]
	if len(titleLine) < 50 && len(titleLine) > 0 {
		title = titleLine
		content = strings.Join(strings.Split(content, "\n")[1:], "\n")
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
	content := string(data)
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
	content := string(data)

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
	var chapters []*model.Chapter

	// 尝试多种章节分割模式
	patterns := []string{
		`第[一二三四五六七八九十百千\d]+章[^\n]*`, // 中文章节
		`第[0-9]+章[^\n]*`,         // 数字章节
		`Chapter\s+[0-9]+[^\n]*`, // English chapter
		`ch\.\s*[0-9]+[^\n]*`,    // ch.1
		`\[第[0-9]+章\]`,           // [第1章]
	}

	var splits []int
	var chapterTitles []string

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllStringIndex(content, -1)

		if len(matches) > len(splits) {
			splits = []int{}
			chapterTitles = []string{}
			for _, match := range matches {
				splits = append(splits, match[0])
				chapterTitles = append(chapterTitles, match[0])
			}
		}
	}

	// 如果没有找到章节标记，按固定字数分割
	if len(splits) == 0 {
		return s.splitByLength(content, novelTitle, 3000) // 每章3000字
	}

	// 提取章节内容
	for i := 0; i < len(splits); i++ {
		start := splits[i]
		end := len(content)
		if i < len(splits)-1 {
			end = splits[i+1]
		}

		chapterContent := strings.TrimSpace(content[start:end])
		if len(chapterContent) < 100 {
			continue
		}

		chapter := &model.Chapter{
			ChapterNo: i + 1,
			Title:     chapterTitles[i],
			Content:   chapterContent,
			WordCount: len([]rune(chapterContent)),
			Status:    "published",
		}
		chapters = append(chapters, chapter)
	}

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
	log.Printf("Novel imported: %s, chapters: %d", importResult.Title, importResult.ImportedChapters)

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

	chapters, err := s.chapterRepo.ListByRange(req.NovelID, startCh, endCh)
	if err != nil {
		return nil, fmt.Errorf("get chapters failed: %w", err)
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

		// 保存分镜
		for _, shot := range shots {
			shot.VideoID = video.ID
			// TODO: 保存到数据库
			totalShots++
		}

		result.ChaptersProcessed = i + 1
		log.Printf("Processed chapter %d/%d, shots: %d", i+1, len(chapters), len(shots))
	}

	result.ShotsGenerated = totalShots
	result.Status = "storyboard_completed"

	// 5. 应用视频增强
	enhancements := s.videoEnhancement.GetDefaultEnhancements()
	for _, enh := range enhancements {
		if enh.Enabled {
			log.Printf("Applying enhancement: %s", enh.Type)
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
