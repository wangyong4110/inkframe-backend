package handler

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// importTaskStatus 导入任务状态
type importTaskStatus struct {
	TaskID    string  `json:"task_id"`
	Status    string  `json:"status"`   // pending, running, completed, failed
	Progress  int     `json:"progress"` // 0-100
	Message   string  `json:"message,omitempty"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

// importTaskStore 全局任务状态存储
var importTaskStore sync.Map

func setImportTask(id string, status *importTaskStatus) {
	status.UpdatedAt = time.Now().Unix()
	importTaskStore.Store(id, status)
}

func getImportTask(id string) (*importTaskStatus, bool) {
	v, ok := importTaskStore.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*importTaskStatus), true
}

// ImportHandler 导入处理器
type ImportHandler struct {
	importService       *service.NovelImportService
	novelToVideoService *service.NovelToVideoService
	analysisService     *service.NovelAnalysisService
}

func NewImportHandler(
	importService *service.NovelImportService,
	novelToVideoService *service.NovelToVideoService,
) *ImportHandler {
	return &ImportHandler{
		importService:       importService,
		novelToVideoService: novelToVideoService,
	}
}

// SetAnalysisService 注入分析服务
func (h *ImportHandler) SetAnalysisService(svc *service.NovelAnalysisService) {
	h.analysisService = svc
}

// ImportNovel 导入小说
// POST /api/v1/import/novel
func (h *ImportHandler) ImportNovel(c *gin.Context) {
	var req service.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	taskID := fmt.Sprintf("import-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	setImportTask(taskID, &importTaskStatus{
		TaskID:    taskID,
		Status:    "running",
		Progress:  0,
		Message:   "导入中",
		CreatedAt: now,
	})

	go func(r service.ImportRequest) {
		result, err := h.importService.Import(&r)
		if err != nil {
			setImportTask(taskID, &importTaskStatus{
				TaskID:    taskID,
				Status:    "failed",
				Progress:  0,
				Message:   err.Error(),
				CreatedAt: now,
			})
			return
		}
		setImportTask(taskID, &importTaskStatus{
			TaskID:    taskID,
			Status:    "completed",
			Progress:  100,
			Message:   fmt.Sprintf("导入完成，共 %d 章", result.ImportedChapters),
			CreatedAt: now,
		})
	}(req)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "import started",
		"data":    gin.H{"task_id": taskID},
	})
}

// ImportFromFile 上传文件导入小说
// POST /api/v1/import/novel/file
func (h *ImportHandler) ImportFromFile(c *gin.Context) {
	// 获取上传的文件
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		respondBadRequest(c, "no file uploaded")
		return
	}
	defer file.Close()

	// 限制文件大小防止 OOM（最大 50MB）
	const maxFileSize = 50 * 1024 * 1024
	if header.Size > maxFileSize {
		respondBadRequest(c, "file too large (max 50MB)")
		return
	}

	// ReadAll 保证读取完整内容（file.Read 可能返回部分数据）
	data, err := io.ReadAll(file)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "read file failed")
		return
	}

	// 获取其他参数
	format := c.PostForm("format")
	if format == "" {
		format = detectFormatFromFilename(header.Filename)
	}

	req := &service.ImportRequest{
		Source:   service.SourceFile,
		FileData: data,
		FileName: header.Filename,
		Format:   service.ImportFormat(format),
		TenantID: getTenantID(c),
	}

	// 追加模式：前端可传 novel_id 将章节追加到已有小说
	if novelIDStr := c.PostForm("novel_id"); novelIDStr != "" {
		if novelID, err := strconv.ParseUint(novelIDStr, 10, 32); err == nil {
			req.NovelID = uint(novelID)
		}
	}

	result, err := h.importService.Import(req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// ImportFromURL URL导入小说
// POST /api/v1/import/novel/url
func (h *ImportHandler) ImportFromURL(c *gin.Context) {
	var req struct {
		URL      string `json:"url" binding:"required"`
		SiteName string `json:"site_name,omitempty"`
		NovelID  uint   `json:"novel_id,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	importReq := &service.ImportRequest{
		Source:   service.SourceURL,
		URL:      req.URL,
		SiteName: req.SiteName,
		NovelID:  req.NovelID,
		TenantID: getTenantID(c),
	}

	result, err := h.importService.Import(importReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// ImportFromCrawl 爬取导入小说
// POST /api/v1/import/novel/crawl
func (h *ImportHandler) ImportFromCrawl(c *gin.Context) {
	var req struct {
		URL      string `json:"url" binding:"required"`
		SiteName string `json:"site_name,omitempty"`
		NovelID  uint   `json:"novel_id,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	importReq := &service.ImportRequest{
		Source:   service.SourceCrawl,
		URL:      req.URL,
		SiteName: req.SiteName,
		NovelID:  req.NovelID,
		TenantID: getTenantID(c),
	}

	result, err := h.importService.Import(importReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// ImportAndGenerate 导入小说并生成视频
// POST /api/v1/import/novel/video
func (h *ImportHandler) ImportAndGenerate(c *gin.Context) {
	var req struct {
		// 导入参数
		Source   string `json:"source" binding:"required"`
		URL      string `json:"url,omitempty"`
		FileData []byte `json:"file_data,omitempty"`
		FileName string `json:"file_name,omitempty"`
		Format   string `json:"format,omitempty"`
		SiteName string `json:"site_name,omitempty"`

		// 视频参数
		StartChapter int    `json:"start_chapter,omitempty"`
		EndChapter  int    `json:"end_chapter,omitempty"`
		Resolution  string `json:"resolution,omitempty"`
		FrameRate   int    `json:"frame_rate,omitempty"`
		AspectRatio string `json:"aspect_ratio,omitempty"`
		ArtStyle    string `json:"art_style,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	importReq := &service.ImportRequest{
		Source:   service.ImportSource(req.Source),
		URL:      req.URL,
		FileData: req.FileData,
		FileName: req.FileName,
		Format:   service.ImportFormat(req.Format),
		SiteName: req.SiteName,
	}

	videoReq := &service.NovelToVideoRequest{
		StartChapter: req.StartChapter,
		EndChapter:  req.EndChapter,
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		AspectRatio: req.AspectRatio,
		ArtStyle:   req.ArtStyle,
	}

	result, err := h.novelToVideoService.ImportAndGenerate(importReq, videoReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// GenerateVideoFromNovel 从已有小说生成视频
// POST /api/v1/novels/:id/generate-video
func (h *ImportHandler) GenerateVideoFromNovel(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	var req service.NovelToVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	req.NovelID = uint(novelId)

	result, err := h.novelToVideoService.GenerateVideo(&req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// GetImportStatus 获取导入状态
// GET /api/v1/import/status/:task_id
func (h *ImportHandler) GetImportStatus(c *gin.Context) {
	taskID := c.Param("task_id")

	task, ok := getImportTask(taskID)
	if !ok {
		respondErr(c, http.StatusNotFound, "task not found")
		return
	}

	respondOK(c, task)
}

// StartAnalysis 触发小说分析 Pipeline
// POST /api/v1/novels/:id/analyze
func (h *ImportHandler) StartAnalysis(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	if h.analysisService == nil {
		respondErr(c, http.StatusInternalServerError, "analysis service not available")
		return
	}
	var body struct {
		CreateChapterOutlines bool `json:"create_chapter_outlines"`
	}
	c.ShouldBindJSON(&body)

	tenantID := getTenantID(c)
	taskID, err := h.analysisService.StartAnalysis(tenantID, uint(novelID), body.CreateChapterOutlines)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "analysis started",
		"data":    gin.H{"task_id": taskID},
	})
}

// GetCrawlStatus 查询爬取进度
// GET /api/v1/novels/:id/crawl/status
func (h *ImportHandler) GetCrawlStatus(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	progress, err := h.importService.GetCrawlProgress(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if progress == nil {
		respondErr(c, http.StatusNotFound, "no crawl task found")
		return
	}
	respondOK(c, progress)
}

// ResumeCrawl 从断点继续爬取
// POST /api/v1/novels/:id/crawl/resume
func (h *ImportHandler) ResumeCrawl(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	if err := h.importService.ResumeCrawl(uint(novelID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "crawl resumed"})
}

// GetAnalysisStatus 查询分析任务状态
// GET /api/v1/novels/:id/analyze/status?task_id=xxx
func (h *ImportHandler) GetAnalysisStatus(c *gin.Context) {
	taskID := c.Query("task_id")
	if taskID == "" {
		respondBadRequest(c, "task_id required")
		return
	}
	if h.analysisService == nil {
		respondErr(c, http.StatusInternalServerError, "analysis service not available")
		return
	}
	task, err := h.analysisService.GetStatus(taskID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "task not found")
		return
	}
	respondOK(c, task)
}

// 检测文件格式
func detectFormatFromFilename(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".txt":
		return "txt"
	case ".md", ".markdown":
		return "md"
	case ".json":
		return "json"
	case ".html", ".htm":
		return "html"
	case ".epub":
		return "epub"
	case ".docx":
		return "docx"
	default:
		return "txt"
	}
}
