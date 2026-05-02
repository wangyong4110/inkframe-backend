package handler

import (
	"fmt"
	"io"
	"net/http"
	"os"
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
	TaskID         string `json:"task_id"`
	Status         string `json:"status"`   // pending/uploading/parsing/analyzing/completed/failed
	Progress       int    `json:"progress"` // 0-100
	Step           string `json:"step,omitempty"`
	Message        string `json:"message,omitempty"`
	NovelID        uint   `json:"novel_id,omitempty"`
	OSSUrl         string `json:"oss_url,omitempty"`
	AnalysisTaskID string `json:"analysis_task_id,omitempty"`
	// 爬取进度（crawl 任务）
	CrawlDone    int    `json:"crawl_done,omitempty"`
	CrawlTotal   int    `json:"crawl_total,omitempty"`
	CrawlCurrent string `json:"crawl_current,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// importTaskStore 全局任务状态存储（in-memory，进程重启后丢失，可接受）
var importTaskStore sync.Map

// chunkSession 分片上传会话
type chunkSession struct {
	UploadID    string
	TotalChunks int
	TenantID    uint
	FileName    string
	Format      string
	NovelID     uint
	TmpDir      string
	mu          sync.Mutex
	received    map[int]bool
}

// chunkStore 全局分片会话存储
var chunkStore sync.Map

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

// runImportAndAnalyze 通用导入+分析流程（在 goroutine 中调用）
func (h *ImportHandler) runImportAndAnalyze(taskID string, createdAt int64, req *service.ImportRequest, tenantID uint) {
	setImportTask(taskID, &importTaskStatus{
		TaskID:    taskID,
		Status:    "parsing",
		Step:      "解析导入中...",
		Progress:  20,
		CreatedAt: createdAt,
	})

	result, err := h.importService.Import(req)
	if err != nil {
		setImportTask(taskID, &importTaskStatus{
			TaskID:    taskID,
			Status:    "failed",
			Message:   err.Error(),
			CreatedAt: createdAt,
		})
		return
	}

	// 自动触发分析 pipeline
	analysisTaskID := ""
	if h.analysisService != nil {
		if id, aErr := h.analysisService.StartAnalysis(tenantID, result.NovelID, false); aErr == nil {
			analysisTaskID = id
		}
	}

	setImportTask(taskID, &importTaskStatus{
		TaskID:         taskID,
		Status:         "completed",
		Progress:       100,
		Message:        fmt.Sprintf("导入完成，共 %d 章", result.ImportedChapters),
		NovelID:        result.NovelID,
		OSSUrl:         result.OSSUrl,
		AnalysisTaskID: analysisTaskID,
		CreatedAt:      createdAt,
	})
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

	tenantID := getTenantID(c)

	// 异步执行：OSS 上传 → 解析 → 保存 → 触发分析
	taskID := fmt.Sprintf("import-file-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	setImportTask(taskID, &importTaskStatus{
		TaskID:    taskID,
		Status:    "uploading",
		Step:      "上传中...",
		Progress:  0,
		CreatedAt: now,
	})

	go h.runImportAndAnalyze(taskID, now, req, tenantID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "import started",
		"data":    gin.H{"task_id": taskID},
	})
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

// ImportFromCrawl 爬取导入小说（异步）
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
	tenantID := getTenantID(c)

	taskID := fmt.Sprintf("import-crawl-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	setImportTask(taskID, &importTaskStatus{
		TaskID:    taskID,
		Status:    "crawling",
		Step:      "获取章节目录...",
		Progress:  0,
		CreatedAt: now,
	})

	go func(r *service.ImportRequest) {
		// 1. 创建章节存根，启动后台爬取
		result, err := h.importService.Import(r)
		if err != nil {
			setImportTask(taskID, &importTaskStatus{
				TaskID:    taskID,
				Status:    "failed",
				Message:   err.Error(),
				CreatedAt: now,
			})
			return
		}
		novelID := result.NovelID
		setImportTask(taskID, &importTaskStatus{
			TaskID:     taskID,
			Status:     "crawling",
			Step:       "爬取章节内容中...",
			Progress:   5,
			NovelID:    novelID,
			CrawlTotal: result.TotalChapters,
			CreatedAt:  now,
		})

		// 2. 注册爬取完成回调（触发分析）
		analysisDone := make(chan string, 1)
		h.importService.RegisterCrawlDoneCallback(novelID, func() {
			id := ""
			if h.analysisService != nil {
				if aid, aErr := h.analysisService.StartAnalysis(tenantID, novelID, false); aErr == nil {
					id = aid
				}
			}
			analysisDone <- id
		})

		// 3. 轮询爬取进度
		for {
			progress, _ := h.importService.GetCrawlProgress(novelID)
			if progress == nil {
				break // 无待爬取章节，视为完成
			}
			if progress.Status == "completed" || progress.Status == "failed" || progress.Status == "paused" {
				break
			}
			pct := 5
			if progress.Total > 0 {
				pct = 5 + int(float64(progress.Done)/float64(progress.Total)*55)
			}
			setImportTask(taskID, &importTaskStatus{
				TaskID:       taskID,
				Status:       "crawling",
				Step:         "爬取章节内容中...",
				Progress:     pct,
				NovelID:      novelID,
				CrawlDone:    progress.Done,
				CrawlTotal:   progress.Total,
				CrawlCurrent: progress.Current,
				CreatedAt:    now,
			})
			time.Sleep(2 * time.Second)
		}

		// 4. 等待分析任务 ID（带超时）
		analysisTaskID := ""
		select {
		case id := <-analysisDone:
			analysisTaskID = id
		case <-time.After(10 * time.Second):
			// 回调可能已在爬取完成前触发，直接继续
		}

		setImportTask(taskID, &importTaskStatus{
			TaskID:         taskID,
			Status:         "completed",
			Progress:       100,
			Message:        fmt.Sprintf("爬取完成，共 %d 章", result.ImportedChapters),
			NovelID:        novelID,
			AnalysisTaskID: analysisTaskID,
			CreatedAt:      now,
		})
	}(importReq)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "crawl started",
		"data":    gin.H{"task_id": taskID},
	})
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

// InitChunkedUpload 初始化分片上传会话
// POST /api/v1/import/novel/file/init
func (h *ImportHandler) InitChunkedUpload(c *gin.Context) {
	var body struct {
		Filename    string `json:"filename" binding:"required"`
		TotalChunks int    `json:"total_chunks" binding:"required,min=1"`
		NovelID     uint   `json:"novel_id,omitempty"`
		Format      string `json:"format,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	uploadID := fmt.Sprintf("chunk-%d", time.Now().UnixNano())
	tmpDir := filepath.Join(os.TempDir(), "inkframe_chunks", uploadID)
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create temp dir")
		return
	}

	sess := &chunkSession{
		UploadID:    uploadID,
		TotalChunks: body.TotalChunks,
		TenantID:    getTenantID(c),
		FileName:    body.Filename,
		Format:      body.Format,
		NovelID:     body.NovelID,
		TmpDir:      tmpDir,
		received:    make(map[int]bool),
	}
	chunkStore.Store(uploadID, sess)

	respondOK(c, gin.H{"upload_id": uploadID})
}

// UploadChunk 上传单个分片
// PUT /api/v1/import/novel/file/chunk
func (h *ImportHandler) UploadChunk(c *gin.Context) {
	uploadID := c.PostForm("upload_id")
	chunkNoStr := c.PostForm("chunk_no")
	if uploadID == "" || chunkNoStr == "" {
		respondBadRequest(c, "upload_id and chunk_no required")
		return
	}
	chunkNo, err := strconv.Atoi(chunkNoStr)
	if err != nil || chunkNo < 1 {
		respondBadRequest(c, "invalid chunk_no")
		return
	}

	v, ok := chunkStore.Load(uploadID)
	if !ok {
		respondErr(c, http.StatusNotFound, "upload session not found")
		return
	}
	sess := v.(*chunkSession)

	f, _, err := c.Request.FormFile("chunk")
	if err != nil {
		respondBadRequest(c, "chunk file required")
		return
	}
	defer f.Close()

	chunkPath := filepath.Join(sess.TmpDir, fmt.Sprintf("chunk_%05d", chunkNo))
	out, err := os.Create(chunkPath)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save chunk")
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		respondErr(c, http.StatusInternalServerError, "failed to write chunk")
		return
	}
	out.Close()

	sess.mu.Lock()
	sess.received[chunkNo] = true
	received := len(sess.received)
	sess.mu.Unlock()

	respondOK(c, gin.H{"received": received, "total": sess.TotalChunks})
}

// CompleteChunkedUpload 完成分片上传，组装文件并触发导入
// POST /api/v1/import/novel/file/complete
func (h *ImportHandler) CompleteChunkedUpload(c *gin.Context) {
	var body struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	v, ok := chunkStore.Load(body.UploadID)
	if !ok {
		respondErr(c, http.StatusNotFound, "upload session not found")
		return
	}
	sess := v.(*chunkSession)

	sess.mu.Lock()
	missing := sess.TotalChunks - len(sess.received)
	sess.mu.Unlock()
	if missing > 0 {
		respondBadRequest(c, fmt.Sprintf("%d chunks not yet received", missing))
		return
	}

	// 按序拼装分片
	var assembled []byte
	for i := 1; i <= sess.TotalChunks; i++ {
		chunkPath := filepath.Join(sess.TmpDir, fmt.Sprintf("chunk_%05d", i))
		data, err := os.ReadFile(chunkPath)
		if err != nil {
			respondErr(c, http.StatusInternalServerError, fmt.Sprintf("chunk %d missing", i))
			return
		}
		assembled = append(assembled, data...)
	}

	// 清理临时目录
	chunkStore.Delete(body.UploadID)
	os.RemoveAll(sess.TmpDir) //nolint:errcheck

	req := &service.ImportRequest{
		Source:   service.SourceFile,
		FileData: assembled,
		FileName: sess.FileName,
		Format:   service.ImportFormat(sess.Format),
		TenantID: sess.TenantID,
		NovelID:  sess.NovelID,
	}
	if req.Format == "" {
		req.Format = service.ImportFormat(detectFormatFromFilename(sess.FileName))
	}

	tenantID := sess.TenantID
	taskID := fmt.Sprintf("import-file-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	setImportTask(taskID, &importTaskStatus{
		TaskID:    taskID,
		Status:    "parsing",
		Step:      "解析导入中...",
		Progress:  5,
		CreatedAt: now,
	})

	go h.runImportAndAnalyze(taskID, now, req, tenantID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "import started",
		"data": gin.H{
			"task_id":       taskID,
			"assembled_size": len(assembled),
		},
	})
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
