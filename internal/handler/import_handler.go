package handler

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/redis/go-redis/v9"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// chunkSession 分片上传会话（进程内，Redis 不可用时使用）
type chunkSession struct {
	UploadID    string
	TotalChunks int
	TotalSize   int64 // declared total file size in bytes (0 = not provided)
	TenantID    uint
	FileName    string
	Format      string
	NovelID     uint
	TmpDir      string
	CreatedAt   time.Time
	mu          sync.Mutex
	received    map[int]bool
}

// chunkSessionMeta is the Redis-serializable view of a chunk upload session.
type chunkSessionMeta struct {
	UploadID    string    `json:"upload_id"`
	TotalChunks int       `json:"total_chunks"`
	TotalSize   int64     `json:"total_size"`
	TenantID    uint      `json:"tenant_id"`
	FileName    string    `json:"file_name"`
	Format      string    `json:"format"`
	NovelID     uint      `json:"novel_id"`
	CreatedAt   time.Time `json:"created_at"`
}

const chunkRedisTTL = 2 * time.Hour

func chunkSessionKey(uploadID string) string  { return "chunk:session:" + uploadID }
func chunkReceivedKey(uploadID string) string { return "chunk:received:" + uploadID }
func chunkDataKey(uploadID string, no int) string {
	return fmt.Sprintf("chunk:data:%s:%05d", uploadID, no)
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

// sanitizeFilename strips path components and removes unsafe characters from an upload filename.
func sanitizeFilename(name string) string {
	// Extract just the base filename (no path components)
	name = filepath.Base(name)
	// Remove any null bytes
	name = strings.ReplaceAll(name, "\x00", "")
	// Only allow safe characters
	safeRe := regexp.MustCompile(`[^a-zA-Z0-9\-_\. ]`)
	name = safeRe.ReplaceAllString(name, "_")
	// Trim to reasonable length
	if len(name) > 255 {
		ext := filepath.Ext(name)
		name = name[:255-len(ext)] + ext
	}
	if name == "" || name == "." {
		name = "upload"
	}
	return name
}

// ImportHandler 导入处理器
type ImportHandler struct {
	importService       *service.NovelImportService
	novelToVideoService *service.NovelToVideoService
	analysisService     *service.NovelAnalysisService
	taskSvc             *service.TaskService
	novelSvc            *service.NovelService
	cache               *redis.Client // optional: cross-instance chunked-upload session storage
	auditSvc            *service.AuditService
}

func (h *ImportHandler) WithAuditService(svc *service.AuditService) *ImportHandler {
	h.auditSvc = svc
	return h
}

// WithRedis injects a Redis client so chunked-upload sessions survive cross-instance routing.
func (h *ImportHandler) WithRedis(c *redis.Client) *ImportHandler {
	h.cache = c
	return h
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
	if _, err := h.novelSvc.GetNovel(novelID, getTenantID(c), getUserID(c)); err != nil {
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
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{"req": req})

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c),
			Action: "novel.import", ResourceType: "novel",
			Details: map[string]any{"source": "text"}, IP: c.ClientIP(),
		})
	}

	respondAccepted(c, task.TaskID, "import started")
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

	// Use LimitReader to prevent reading beyond limit even if Content-Length is spoofed
	limitedReader := io.LimitReader(file, maxFileSize+1)
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to read file")
		return
	}
	if int64(len(data)) > maxFileSize {
		respondBadRequest(c, "file content exceeds 50MB limit")
		return
	}

	// 获取其他参数
	format := c.PostForm("format")
	if format == "" {
		format = detectFormatFromFilename(header.Filename)
	}

	tenantID := getTenantID(c)
	req := service.ImportRequest{
		Source:   service.SourceFile,
		FileName: header.Filename,
		Format:   service.ImportFormat(format),
		TenantID: tenantID,
	}

	// 追加模式：前端可传 novel_id 将章节追加到已有小说
	if novelIDStr := c.PostForm("novel_id"); novelIDStr != "" {
		if novelID, err := strconv.ParseUint(novelIDStr, 10, 32); err == nil {
			req.NovelID = uint(novelID)
		}
	}

	// 文件字节不适合塞进任务表的 params 列（mediumtext，且不该承载大 blob），
	// 先落地到 OSS/本地存储，只把 URL 存进 params；引擎调度执行时再下载回来。
	fileURL, err := h.importService.StageFileForImport(c.Request.Context(), tenantID, header.Filename, data)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to stage uploaded file: "+err.Error())
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "文件导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{"req": req, "file_url": fileURL})
	h.taskSvc.SetMeta(task.TaskID, map[string]interface{}{"step": "上传中..."}) //nolint:errcheck

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c),
			Action: "novel.import", ResourceType: "novel",
			Details: map[string]any{"source": "file", "filename": header.Filename}, IP: c.ClientIP(),
		})
	}

	respondAccepted(c, task.TaskID, "import started")
}

// ImportFromURL URL导入小说（异步）
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

	tenantID := getTenantID(c)
	importReq := service.ImportRequest{
		Source:   service.SourceURL,
		URL:      req.URL,
		SiteName: req.SiteName,
		NovelID:  req.NovelID,
		TenantID: tenantID,
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "URL导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{"req": importReq})

	respondAccepted(c, task.TaskID, "import started")
}

// ImportFromCrawl 爬取导入小说（异步）
// POST /api/v1/import/novel/crawl
func (h *ImportHandler) ImportFromCrawl(c *gin.Context) {
	var req struct {
		URL      string               `json:"url" binding:"required"`
		SiteName string               `json:"site_name,omitempty"`
		NovelID  uint                 `json:"novel_id,omitempty"`
		Config   *crawler.CrawlConfig `json:"config,omitempty"`
	}
	if !bindJSON(c, &req) {
		return
	}

	callerUID, _ := c.Get("user_id")
	callerUserID, _ := callerUID.(uint)
	tenantID := getTenantID(c)
	importReq := service.ImportRequest{
		Source:      service.SourceCrawl,
		URL:         req.URL,
		SiteName:    req.SiteName,
		NovelID:     req.NovelID,
		TenantID:    tenantID,
		UserID:      callerUserID,
		CrawlConfig: req.Config,
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "爬取导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{"req": importReq})
	h.taskSvc.SetMeta(task.TaskID, map[string]interface{}{"step": "获取章节目录..."}) //nolint:errcheck
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c),
			Action: "novel.import", ResourceType: "novel",
			Details: map[string]any{"source": "crawl", "url": req.URL}, IP: c.ClientIP(),
		})
	}

	respondAccepted(c, task.TaskID, "crawl started")
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
		TotalSize   int64  `json:"total_size,omitempty"` // optional: declared total file size in bytes
		NovelID     uint   `json:"novel_id,omitempty"`
		Format      string `json:"format,omitempty"`
	}
	if !bindJSON(c, &body) {
		return
	}

	const maxTotalSize = 500 * 1024 * 1024 // 500 MB
	if body.TotalSize > 0 && body.TotalSize > maxTotalSize {
		respondErr(c, http.StatusRequestEntityTooLarge, "declared file size exceeds 500MB limit")
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
		TotalSize:   body.TotalSize,
		TenantID:    getTenantID(c),
		FileName:    sanitizeFilename(body.Filename),
		Format:      body.Format,
		NovelID:     body.NovelID,
		TmpDir:      tmpDir,
		CreatedAt:   time.Now(),
		received:    make(map[int]bool),
	}
	chunkStore.Store(uploadID, sess)

	// Persist to Redis so other instances can serve subsequent chunk requests.
	if h.cache != nil {
		meta := chunkSessionMeta{
			UploadID: uploadID, TotalChunks: body.TotalChunks, TotalSize: body.TotalSize,
			TenantID: sess.TenantID, FileName: sess.FileName, Format: body.Format,
			NovelID: body.NovelID, CreatedAt: sess.CreatedAt,
		}
		if b, err := json.Marshal(meta); err == nil {
			h.cache.Set(context.Background(), chunkSessionKey(uploadID), b, chunkRedisTTL) //nolint:errcheck
		}
	}

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

	// Load session: local first (same instance), then Redis fallback (other instance).
	var sess *chunkSession
	var redisMeta *chunkSessionMeta
	if v, ok := chunkStore.Load(uploadID); ok {
		sess = v.(*chunkSession)
	} else if h.cache != nil {
		raw, err := h.cache.Get(context.Background(), chunkSessionKey(uploadID)).Bytes()
		if err != nil {
			respondErr(c, http.StatusNotFound, "upload session not found")
			return
		}
		var meta chunkSessionMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			respondErr(c, http.StatusInternalServerError, "corrupt upload session")
			return
		}
		redisMeta = &meta
	} else {
		respondErr(c, http.StatusNotFound, "upload session not found")
		return
	}

	tenantID := getTenantID(c)
	totalChunks := 0
	if sess != nil {
		if sess.TenantID != tenantID {
			respondErr(c, http.StatusForbidden, "forbidden")
			return
		}
		totalChunks = sess.TotalChunks
	} else {
		if redisMeta.TenantID != tenantID {
			respondErr(c, http.StatusForbidden, "forbidden")
			return
		}
		totalChunks = redisMeta.TotalChunks
	}

	if chunkNo > totalChunks {
		respondBadRequest(c, fmt.Sprintf("chunk_no %d exceeds total_chunks %d", chunkNo, totalChunks))
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

	chunkBytes, err := io.ReadAll(io.LimitReader(f, maxChunkSize+1))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to read chunk")
		return
	}

	h2 := md5.New()
	h2.Write(chunkBytes) //nolint:errcheck
	if expectedMD5 := c.PostForm("chunk_md5"); expectedMD5 != "" {
		actualMD5 := hex.EncodeToString(h2.Sum(nil))
		if !strings.EqualFold(actualMD5, expectedMD5) {
			respondBadRequest(c, fmt.Sprintf("chunk %d MD5 mismatch: expected %s got %s", chunkNo, expectedMD5, actualMD5))
			return
		}
	}

	var received int
	if sess != nil {
		// Local session: write to temp file.
		chunkPath := filepath.Join(sess.TmpDir, fmt.Sprintf("chunk_%05d", chunkNo))
		if err := os.WriteFile(chunkPath, chunkBytes, 0600); err != nil {
			respondErr(c, http.StatusInternalServerError, "failed to save chunk")
			return
		}
		sess.mu.Lock()
		sess.received[chunkNo] = true
		received = len(sess.received)
		sess.mu.Unlock()
	}

	// Store chunk in Redis (required when redisMeta != nil; also mirrors local chunks for redundancy).
	if h.cache != nil {
		pipe := h.cache.Pipeline()
		pipe.Set(context.Background(), chunkDataKey(uploadID, chunkNo), chunkBytes, chunkRedisTTL)
		pipe.SAdd(context.Background(), chunkReceivedKey(uploadID), strconv.Itoa(chunkNo))
		pipe.Expire(context.Background(), chunkReceivedKey(uploadID), chunkRedisTTL)
		if _, err := pipe.Exec(context.Background()); err != nil {
			reqLogger(c).Errorf("[UploadChunk] Redis pipeline failed for %s chunk %d: %v", uploadID, chunkNo, err)
		}
		// Get cross-instance received count from Redis Set.
		n, _ := h.cache.SCard(context.Background(), chunkReceivedKey(uploadID)).Result()
		received = int(n)
	}

	respondOK(c, gin.H{"received": received, "total": totalChunks})
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

	uploadID := body.UploadID

	// Load session: local sync.Map first, then Redis.
	var localSess *chunkSession
	var redisMeta *chunkSessionMeta
	if v, ok := chunkStore.Load(uploadID); ok {
		localSess = v.(*chunkSession)
	} else if h.cache != nil {
		raw, err := h.cache.Get(context.Background(), chunkSessionKey(uploadID)).Bytes()
		if err != nil {
			respondErr(c, http.StatusNotFound, "upload session not found")
			return
		}
		var meta chunkSessionMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			respondErr(c, http.StatusInternalServerError, "corrupt upload session")
			return
		}
		redisMeta = &meta
	} else {
		respondErr(c, http.StatusNotFound, "upload session not found")
		return
	}

	tenantID := getTenantID(c)
	var fileName, format string
	var novelID uint
	var totalChunks int

	if localSess != nil {
		if localSess.TenantID != tenantID {
			respondErr(c, http.StatusForbidden, "forbidden")
			return
		}
		localSess.mu.Lock()
		missing := localSess.TotalChunks - len(localSess.received)
		localSess.mu.Unlock()
		if missing > 0 {
			respondBadRequest(c, fmt.Sprintf("%d chunks not yet received", missing))
			return
		}
		fileName, format, novelID, totalChunks = localSess.FileName, localSess.Format, localSess.NovelID, localSess.TotalChunks
	} else {
		if redisMeta.TenantID != tenantID {
			respondErr(c, http.StatusForbidden, "forbidden")
			return
		}
		n, _ := h.cache.SCard(context.Background(), chunkReceivedKey(uploadID)).Result()
		if int(n) < redisMeta.TotalChunks {
			respondBadRequest(c, fmt.Sprintf("%d chunks not yet received", redisMeta.TotalChunks-int(n)))
			return
		}
		fileName, format, novelID, totalChunks = redisMeta.FileName, redisMeta.Format, redisMeta.NovelID, redisMeta.TotalChunks
	}

	// 按序拼装分片
	var assembled []byte
	if localSess != nil && redisMeta == nil {
		// Read from local temp files (same instance).
		for i := 1; i <= totalChunks; i++ {
			chunkPath := filepath.Join(localSess.TmpDir, fmt.Sprintf("chunk_%05d", i))
			data, err := os.ReadFile(chunkPath)
			if err != nil {
				// Fallback: try Redis if chunk file is missing.
				if h.cache != nil {
					data, err = h.cache.Get(context.Background(), chunkDataKey(uploadID, i)).Bytes()
				}
				if err != nil {
					respondErr(c, http.StatusInternalServerError, fmt.Sprintf("chunk %d missing", i))
					return
				}
			}
			assembled = append(assembled, data...)
		}
	} else {
		// Read all chunks from Redis.
		for i := 1; i <= totalChunks; i++ {
			data, err := h.cache.Get(context.Background(), chunkDataKey(uploadID, i)).Bytes()
			if err != nil {
				respondErr(c, http.StatusInternalServerError, fmt.Sprintf("chunk %d missing from Redis", i))
				return
			}
			assembled = append(assembled, data...)
		}
	}

	const maxAssembledSize = 500 * 1024 * 1024 // 500MB max assembled file
	if len(assembled) > maxAssembledSize {
		respondErr(c, http.StatusRequestEntityTooLarge, "assembled file exceeds 500MB limit")
		return
	}

	// 清理
	if localSess != nil {
		chunkStore.Delete(uploadID)
		os.RemoveAll(localSess.TmpDir) //nolint:errcheck
	}
	if h.cache != nil {
		pipe := h.cache.Pipeline()
		pipe.Del(context.Background(), chunkSessionKey(uploadID))
		pipe.Del(context.Background(), chunkReceivedKey(uploadID))
		for i := 1; i <= totalChunks; i++ {
			pipe.Del(context.Background(), chunkDataKey(uploadID, i))
		}
		pipe.Exec(context.Background()) //nolint:errcheck
	}

	req := service.ImportRequest{
		Source:   service.SourceFile,
		FileName: fileName,
		Format:   service.ImportFormat(format),
		TenantID: tenantID,
		NovelID:  novelID,
	}
	if req.Format == "" {
		req.Format = service.ImportFormat(detectFormatFromFilename(fileName))
	}

	// 同 ImportFromFile：组装后的字节先落地到 OSS/本地存储，只把 URL 存进任务 params。
	fileURL, err := h.importService.StageFileForImport(c.Request.Context(), tenantID, fileName, assembled)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to stage uploaded file: "+err.Error())
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImport, "分片文件导入", "novel", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{"req": req, "file_url": fileURL})
	h.taskSvc.UpdateProgress(task.TaskID, 5)                                   //nolint:errcheck
	h.taskSvc.SetMeta(task.TaskID, map[string]interface{}{"step": "解析导入中..."}) //nolint:errcheck

	reqLogger(c).Printf("[async] task created: task_id=%s", task.TaskID)
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
	// body 是可选的，空 body 时 ShouldBindJSON 会报 EOF，忽略该错误
	if err := c.ShouldBindJSON(&body); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, "invalid request body: "+err.Error())
		return
	}

	tenantID := getTenantID(c)
	taskID, err := h.analysisService.StartAnalysis(tenantID, uint(novelID), body.CreateChapterOutlines)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondAccepted(c, taskID, "analysis started")
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

	respondAccepted(c, task.TaskID, "crawl resumed")
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
