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

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// chunkSession 分片上传会话
type chunkSession struct {
	UploadID    string
	TotalChunks int
	TenantID    uint
	FileName    string
	Format      string
	NovelID     uint
	TmpDir      string
	CreatedAt   time.Time
	mu          sync.Mutex
	received    map[int]bool
}

// CleanupChunkStore 清理超过 2 小时未完成的分片上传会话（防内存泄漏）。
// 应在 main.go 启动后台 goroutine 定期调用。
func CleanupChunkStore() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-2 * time.Hour)
		chunkStore.Range(func(k, v any) bool {
			sess := v.(*chunkSession)
			if sess.CreatedAt.Before(cutoff) {
				chunkStore.Delete(k)
				os.RemoveAll(sess.TmpDir) //nolint:errcheck
			}
			return true
		})
	}
}

// chunkStore 全局分片会话存储
var chunkStore sync.Map

// ImportHandler 导入处理器
type ImportHandler struct {
	importService       *service.NovelImportService
	novelToVideoService *service.NovelToVideoService
	analysisService     *service.NovelAnalysisService
	taskSvc             *service.TaskService
	novelSvc            *service.NovelService
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

// WithNovelService 注入小说服务（用于校验小说归属租户）
func (h *ImportHandler) WithNovelService(svc *service.NovelService) *ImportHandler {
	h.novelSvc = svc
	return h
}

// checkNovelTenant 校验小说归属当前租户。返回 false 时已写入错误响应。
func (h *ImportHandler) checkNovelTenant(c *gin.Context, novelID uint) bool {
	if h.novelSvc == nil {
		return true
	}
	if _, err := h.novelSvc.GetNovel(novelID, getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// SetAnalysisService 注入分析服务
func (h *ImportHandler) SetAnalysisService(svc *service.NovelAnalysisService) *ImportHandler {
	h.analysisService = svc
	return h
}

// WithTaskService 注入统一任务服务
func (h *ImportHandler) WithTaskService(svc *service.TaskService) *ImportHandler {
	h.taskSvc = svc
	return h
}

// runImportAndAnalyze 通用导入+分析流程（在 goroutine 中调用）
func (h *ImportHandler) runImportAndAnalyze(taskID string, req *service.ImportRequest, tenantID uint) {
	h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
	h.taskSvc.UpdateProgress(taskID, 20)                                  //nolint:errcheck
	h.taskSvc.SetMeta(taskID, map[string]interface{}{"step": "解析导入中..."}) //nolint:errcheck

	result, err := h.importService.Import(req)
	if err != nil {
		logger.Printf("[ImportHandler] runImportAndAnalyze task %s failed: %v", taskID, err)
		h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		return
	}

	// 自动触发分析 pipeline
	analysisTaskID := ""
	if h.analysisService != nil {
		if id, aErr := h.analysisService.StartAnalysis(tenantID, result.NovelID, false); aErr == nil {
			analysisTaskID = id
		}
	}

	h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
		"novel_id":          result.NovelID,
		"imported_chapters": result.ImportedChapters,
		"oss_url":           result.OSSUrl,
		"analysis_task_id":  analysisTaskID,
		"message":           fmt.Sprintf("导入完成，共 %d 章", result.ImportedChapters),
	})
}

// ImportNovel 导入小说
// POST /api/v1/import/novel
func (h *ImportHandler) ImportNovel(c *gin.Context) {
	var req service.ImportRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "小说导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string, r service.ImportRequest) {
		defer func() {
			if rc := recover(); rc != nil {
				logger.Printf("[ImportHandler] ImportNovel task %s panic: %v", taskID, rc)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		result, err := h.importService.Import(&r)
		if err != nil {
			logger.Printf("[ImportHandler] ImportNovel task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"novel_id":          result.NovelID,
			"imported_chapters": result.ImportedChapters,
			"message":           fmt.Sprintf("导入完成，共 %d 章", result.ImportedChapters),
		})
	}(task.TaskID, req)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "import started",
		"data":    gin.H{"task_id": task.TaskID},
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
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "文件导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	h.taskSvc.SetMeta(task.TaskID, map[string]interface{}{"step": "上传中..."}) //nolint:errcheck

	go h.runImportAndAnalyze(task.TaskID, req, tenantID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "import started",
		"data":    gin.H{"task_id": task.TaskID},
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
	if !bindJSON(c, &req) {
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
	if !bindJSON(c, &req) {
		return
	}

	callerUID, _ := c.Get("user_id")
	callerUserID, _ := callerUID.(uint)
	importReq := &service.ImportRequest{
		Source:   service.SourceCrawl,
		URL:      req.URL,
		SiteName: req.SiteName,
		NovelID:  req.NovelID,
		TenantID: getTenantID(c),
		UserID:   callerUserID,
	}
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "爬取导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	h.taskSvc.SetMeta(task.TaskID, map[string]interface{}{"step": "获取章节目录..."}) //nolint:errcheck

	go func(taskID string, r *service.ImportRequest) {
		defer func() {
			if rc := recover(); rc != nil {
				logger.Printf("[ImportHandler] ImportFromCrawl task %s panic: %v", taskID, rc)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		// 1. 创建章节存根，启动后台爬取
		result, err := h.importService.Import(r)
		if err != nil {
			logger.Printf("[ImportHandler] Crawl import task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		novelID := result.NovelID
		h.taskSvc.UpdateProgress(taskID, 5)               //nolint:errcheck
		h.taskSvc.SetMeta(taskID, map[string]interface{}{ //nolint:errcheck
			"step":        "爬取章节内容中...",
			"novel_id":    novelID,
			"crawl_total": result.TotalChapters,
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
		noProgressCount := 0
		const maxNoProgress = 20 // 最多等 40 秒无变化
		for {
			progress, _ := h.importService.GetCrawlProgress(novelID)
			if progress == nil {
				noProgressCount++
				if noProgressCount >= maxNoProgress {
					logger.Printf("[ImportHandler] crawl progress lost for novel %d, aborting poll", novelID)
					break
				}
			} else {
				noProgressCount = 0
				if progress.Status == "completed" || progress.Status == "failed" || progress.Status == "paused" {
					break
				}
				pct := 5
				if progress.Total > 0 {
					pct = 5 + int(float64(progress.Done)/float64(progress.Total)*55)
				}
				h.taskSvc.UpdateProgress(taskID, pct)             //nolint:errcheck
				h.taskSvc.SetMeta(taskID, map[string]interface{}{ //nolint:errcheck
					"step":          "爬取章节内容中...",
					"novel_id":      novelID,
					"crawl_done":    progress.Done,
					"crawl_total":   progress.Total,
					"crawl_current": progress.Current,
				})
			}
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

		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"novel_id":          novelID,
			"imported_chapters": result.ImportedChapters,
			"analysis_task_id":  analysisTaskID,
			"message":           fmt.Sprintf("爬取完成，共 %d 章", result.ImportedChapters),
		})
	}(task.TaskID, importReq)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "crawl started",
		"data":    gin.H{"task_id": task.TaskID},
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
		EndChapter   int    `json:"end_chapter,omitempty"`
		Resolution   string `json:"resolution,omitempty"`
		FrameRate    int    `json:"frame_rate,omitempty"`
		AspectRatio  string `json:"aspect_ratio,omitempty"`
		ArtStyle     string `json:"art_style,omitempty"`
	}
	if !bindJSON(c, &req) {
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
		EndChapter:   req.EndChapter,
		Resolution:   req.Resolution,
		FrameRate:    req.FrameRate,
		AspectRatio:  req.AspectRatio,
		ArtStyle:     req.ArtStyle,
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
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req service.NovelToVideoRequest
	if !bindJSON(c, &req) {
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
	if !bindJSON(c, &body) {
		return
	}

	uploadID := "chunk-" + uuid.New().String()
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
		CreatedAt:   time.Now(),
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

	if sess.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	if chunkNo > sess.TotalChunks {
		respondBadRequest(c, fmt.Sprintf("chunk_no %d exceeds total_chunks %d", chunkNo, sess.TotalChunks))
		return
	}

	f, header, err := c.Request.FormFile("chunk")
	if err != nil {
		respondBadRequest(c, "chunk file required")
		return
	}
	defer f.Close()

	const maxChunkSize = 10 * 1024 * 1024 // 10 MB per chunk
	if header.Size > maxChunkSize {
		respondBadRequest(c, fmt.Sprintf("chunk too large: max %d bytes", maxChunkSize))
		return
	}

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
	if !bindJSON(c, &body) {
		return
	}

	v, ok := chunkStore.Load(body.UploadID)
	if !ok {
		respondErr(c, http.StatusNotFound, "upload session not found")
		return
	}
	sess := v.(*chunkSession)

	if sess.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

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

	const maxAssembledSize = 500 * 1024 * 1024 // 500MB max assembled file
	if len(assembled) > maxAssembledSize {
		respondErr(c, http.StatusRequestEntityTooLarge, "assembled file exceeds 500MB limit")
		return
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
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "分片文件导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	h.taskSvc.UpdateProgress(task.TaskID, 5)                                   //nolint:errcheck
	h.taskSvc.SetMeta(task.TaskID, map[string]interface{}{"step": "解析导入中..."}) //nolint:errcheck

	go h.runImportAndAnalyze(task.TaskID, req, tenantID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "import started",
		"data": gin.H{
			"task_id":        task.TaskID,
			"assembled_size": len(assembled),
		},
	})
}

// StartAnalysis 触发小说分析 Pipeline
// POST /api/v1/novels/:id/analyze
func (h *ImportHandler) StartAnalysis(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if h.analysisService == nil {
		respondErr(c, http.StatusInternalServerError, "analysis service not available")
		return
	}
	var body struct {
		CreateChapterOutlines bool `json:"create_chapter_outlines"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "invalid request body: "+err.Error())
		return
	}

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
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkNovelTenant(c, uint(novelID)) {
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

// ResumeCrawl 从断点继续爬取（异步，返回 202+task_id）
// POST /api/v1/novels/:id/crawl/resume
func (h *ImportHandler) ResumeCrawl(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkNovelTenant(c, uint(novelID)) {
		return
	}
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "续爬导入", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	if err := h.importService.ResumeCrawl(uint(novelID)); err != nil {
		h.taskSvc.Fail(task.TaskID, err.Error()) //nolint:errcheck
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}

	go func(taskID string, nid uint) {
		defer func() {
			if rc := recover(); rc != nil {
				logger.Printf("[ImportHandler] ResumeCrawl task %s panic: %v", taskID, rc)
				h.taskSvc.Fail(taskID, "内部错误") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		for {
			progress, _ := h.importService.GetCrawlProgress(nid)
			if progress == nil {
				// Job record gone unexpectedly (e.g. novel deleted mid-crawl)
				h.taskSvc.Fail(taskID, "crawl job not found") //nolint:errcheck
				return
			}
			if progress.Status == "completed" || progress.Status == "failed" || progress.Status == "paused" {
				break
			}
			pct := 0
			if progress.Total > 0 {
				pct = int(float64(progress.Done) / float64(progress.Total) * 100)
			}
			h.taskSvc.UpdateProgress(taskID, pct)             //nolint:errcheck
			h.taskSvc.SetMeta(taskID, map[string]interface{}{ //nolint:errcheck
				"novel_id":      nid,
				"crawl_done":    progress.Done,
				"crawl_total":   progress.Total,
				"crawl_current": progress.Current,
			})
			time.Sleep(2 * time.Second)
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"novel_id": nid, "message": "续爬完成"}) //nolint:errcheck
	}(task.TaskID, uint(novelID))

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "crawl resumed",
		"data":    gin.H{"task_id": task.TaskID},
	})
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
