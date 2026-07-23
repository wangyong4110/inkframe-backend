package handler

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

func (h *VideoHandler) GenerateSingleShot(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var req struct {
		Provider string `json:"provider"`
	}
	c.ShouldBindJSON(&req) //nolint:errcheck — optional body

	tenantID := getTenantID(c)
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeAssetGen 的
	// 执行函数在 cmd/server/task_resume.go，source="single_shot" 分支反序列化下面存的字段
	// 调用同一套 GenerateSingleShot + PollSingleShotUntilDone）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("镜头 #%d 素材生成", shotID), "shot", uint(shotID), map[string]interface{}{
			"source":   "single_shot",
			"video_id": uint(videoID),
			"shot_id":  uint(shotID),
			"provider": req.Provider,
		})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "素材生成任务已提交")
}

// BatchGenerateShots 批量生成分镜素材（异步任务模式，立即返回 task_id）
// POST /api/v1/videos/:id/shots/batch-generate
func (h *VideoHandler) BatchGenerateShots(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	var req model.BatchGenerateShotsRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeAssetGen 的
	// 执行函数在 cmd/server/task_resume.go，source="batch_shots" 分支反序列化下面存的整个
	// req 结构体，按 VoiceFirst/Sequential/默认三种模式调用同一套 service 方法 +
	// PollAndStitchVideo 后续轮询）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("批量生成 %d 个镜头素材", len(req.ShotIDs)), "video", uint(videoID), map[string]interface{}{
			"source": "batch_shots",
			"req":    req,
		})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "批量素材生成任务已提交")
}

// BatchGenerateShotImages POST /videos/:id/shots/batch-images
// 批量为分镜生成参考图片（阶段一）。已有图片的分镜自动跳过（幂等）。
func (h *VideoHandler) BatchGenerateShotImages(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	var req model.BatchGenerateShotsRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeAssetGen 的
	// 执行函数在 cmd/server/task_resume.go，source="batch_images" 分支反序列化下面存的整个
	// req 结构体，调用同一个 h.videoService.BatchGenerateShotImages，包括 req.Force）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("批量生成 %d 个镜头图片", len(req.ShotIDs)), "video", uint(videoID), map[string]interface{}{
			"source": "batch_images",
			"req":    req,
		})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "批量图片生成任务已提交")
}

// BatchGenerateShotClips POST /videos/:id/shots/batch-clips
// 批量为已有图片的分镜生成 Ken Burns 动效视频（阶段二）。已有视频的分镜自动跳过（幂等）。
func (h *VideoHandler) BatchGenerateShotClips(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	var req model.BatchGenerateShotsRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeAssetGen 的
	// 执行函数在 cmd/server/task_resume.go，source="batch_clips" 分支反序列化下面存的整个
	// req 结构体，调用同一个 h.videoService.BatchGenerateShotClips）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("批量生成 %d 个镜头视频", len(req.ShotIDs)), "video", uint(videoID), map[string]interface{}{
			"source": "batch_clips",
			"req":    req,
		})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "批量视频生成任务已提交")
}

// RefineShotImage POST /videos/:id/shots/:shot_id/refine-image
// 基于用户修改建议重新生成分镜图片（同步，直接返回新图片 URL）。
func (h *VideoHandler) RefineShotImage(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var req struct {
		Suggestion string `json:"suggestion"`
	}
	if !bindJSON(c, &req) {
		return
	}

	newURL, err := h.videoService.RefineShotImage(uint(shotID), req.Suggestion)
	if err != nil {
		reqLogger(c).Errorf("[VideoHandler] RefineShotImage shot %d failed: %v", shotID, err)
		respondErr(c, http.StatusInternalServerError, "图片重新生成失败，请重试")
		return
	}
	respondOK(c, gin.H{"image_url": newURL})
}

// BatchGenerateSFX POST /videos/:id/shots/sfx
// 为视频所有分镜批量自动生成音效（异步任务）。
// 已有音效条目的分镜自动跳过（幂等，通过 ink_shot_sfx_item 检查）。
func (h *VideoHandler) BatchGenerateSFX(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	tenantID := getTenantID(c)

	shots, err := h.videoService.GetStoryboard(uint(videoID), 0)
	if err != nil || len(shots) == 0 {
		respondErr(c, http.StatusNotFound, "storyboard not found or empty")
		return
	}

	var sfxReq struct {
		UserContext string `json:"user_context"`
		Provider    string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&sfxReq)

	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeSFXGen 的执行
	// 函数在 cmd/server/task_resume.go，entity_type=="video" 分支反序列化下面存的字段，
	// force=false 表示跳过已有标签的镜头，与 AnalyzeSFXTags 共用同一分支但 force=true）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeSFXGen, "自动音效生成", "video", uint(videoID), map[string]interface{}{
		"user_context": sfxReq.UserContext,
		"provider":     sfxReq.Provider,
		"force":        false,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}
	respondAccepted(c, task.TaskID, "音效生成任务已提交")
}

// AnalyzeSFXTags POST /videos/:id/shots/sfx-tags
// 用 AI 批量分析分镜脚本，为每个镜头生成精准的自然语言音效搜索词，写入 sfx_tags 字段。
// 仅更新标签，不搜索/生成实际音频文件。
func (h *VideoHandler) AnalyzeSFXTags(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	tenantID := getTenantID(c)

	shots, err := h.videoService.GetStoryboard(uint(videoID), 0)
	if err != nil || len(shots) == 0 {
		respondErr(c, http.StatusNotFound, "storyboard not found or empty")
		return
	}

	var sfxTagsReq struct {
		UserContext string `json:"user_context"`
		Provider    string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&sfxTagsReq)

	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeSFXGen 的执行
	// 函数在 cmd/server/task_resume.go，entity_type=="video" 分支，force=true 强制重新
	// 分析所有镜头标签，与 BatchGenerateSFX 共用同一分支但 force=false）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeSFXGen, "AI 音效标签分析", "video", uint(videoID), map[string]interface{}{
		"user_context": sfxTagsReq.UserContext,
		"provider":     sfxTagsReq.Provider,
		"force":        true,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}
	respondAccepted(c, task.TaskID, "AI 音效分析任务已提交")
}

// GenerateShotSFX POST /videos/:id/shots/:shot_id/sfx
// 为单个分镜生成音效（异步任务）。
func (h *VideoHandler) GenerateShotSFX(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, err := strconv.Atoi(c.Param("shot_id"))
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	tenantID := getTenantID(c)

	if _, err := h.videoService.GetShotByID(uint(videoID), uint(shotID)); err != nil {
		respondErr(c, http.StatusNotFound, "shot not found")
		return
	}

	var shotSFXReq struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&shotSFXReq)

	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeSFXGen 的执行
	// 函数在 cmd/server/task_resume.go，entity_type=="shot" 分支用 t.EntityID/video_id 重新
	// 查分镜，反序列化 provider 调用同一个 h.sfxSvc.AutoGenerateSFX）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeSFXGen, "单镜头音效生成", "shot", uint(shotID), map[string]interface{}{
		"shot_id":  uint(shotID),
		"video_id": uint(videoID),
		"provider": shotSFXReq.Provider,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}
	respondAccepted(c, task.TaskID, "音效生成任务已提交")
}

// UpdateShotSFXTags PUT /api/v1/videos/:id/shots/:shot_id/sfx-tags
// 手动更新单个分镜的 sfx_tags（插入/修改/删除标签），无需重新 AI 分析。
// Body: {"tags": [{"tag":"...","type":"action|ambient|emotion","prompt":"..."}]}
func (h *VideoHandler) UpdateShotSFXTags(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	type tagInput struct {
		Tag    string `json:"tag"`
		Type   string `json:"type"`
		Prompt string `json:"prompt,omitempty"`
	}
	var req struct {
		Tags []tagInput `json:"tags"`
	}
	if !bindJSON(c, &req) {
		return
	}

	// 转换为 sfxTagItem（包内类型）并序列化
	tags := make([]service.SFXTagItemPublic, 0, len(req.Tags))
	for _, t := range req.Tags {
		if t.Tag == "" {
			continue
		}
		sfxType := t.Type
		if sfxType == "" {
			sfxType = "action"
		}
		tags = append(tags, service.SFXTagItemPublic{Tag: t.Tag, SFXType: sfxType, Prompt: t.Prompt})
	}

	if err := h.sfxSvc.UpdateShotSFXTagsPublic(uint(shotID), tags); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"shot_id": shotID, "count": len(tags)})
}

// GenerateShotVoice 为单个分镜异步生成配音
// POST /api/v1/videos/:id/storyboard/:shot_id/voice
// 立即返回 202 + task_id，轮询 GET /api/v1/tasks/:task_id 获取结果
func (h *VideoHandler) GenerateShotVoice(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	shot, err := h.videoService.GetShotByID(uint(videoID), uint(shotID))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	if shot.Narration == "" && shot.GenMeta.Dialogue == "" && shot.Description == "" {
		respondBadRequest(c, "shot has no text content")
		return
	}

	var req struct {
		NarrationVoice  string `json:"narration_voice"`
		SubtitleEnabled bool   `json:"subtitle_enabled"`
		// SubtitleConfig 字幕样式参数（当前已解析，暂未持久化至 SRT；规划中实现 ASS 样式输出）
		SubtitleConfig struct {
			Position string `json:"position"`
			FontSize int    `json:"font_size"`
			Color    string `json:"color"`
			BgStyle  string `json:"bg_style"`
		} `json:"subtitle_config"`
	}
	_ = c.ShouldBindJSON(&req)

	// 若前端未传 narration_voice，从视频配置中读取（与批量配音接口行为一致）
	narrationVoice := req.NarrationVoice
	if narrationVoice == "" {
		if video, err := h.videoService.GetVideo(uint(videoID)); err == nil {
			if vc := h.videoService.GetNovelVideoConfig(video.NovelID); vc != nil {
				narrationVoice = vc.Config.NarrationVoice
			}
		}
	}

	tenantID := getTenantID(c)
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeVoiceGen 的
	// 执行函数在 cmd/server/task_resume.go，entity_type=="shot" 分支用 t.EntityID/video_id
	// 重新查分镜，反序列化下面存的字段调用同一套 GenerateShotAudio 重试 + 字幕生成逻辑）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeVoiceGen,
		fmt.Sprintf("镜头 #%d 配音生成", shot.ShotNo), "shot", uint(shotID), map[string]interface{}{
			"narration_voice":  narrationVoice,
			"subtitle_enabled": req.SubtitleEnabled,
			"video_id":         uint(videoID),
		})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "配音生成任务已提交")
}

// GetDefaultConsistencyConfig 获取默认一致性配置
// GET /api/v1/consistency/default
func (h *VideoHandler) GetDefaultConsistencyConfig(c *gin.Context) {
	if h.consistencyService == nil {
		respondErr(c, http.StatusServiceUnavailable, "consistency service unavailable")
		return
	}
	level := h.consistencyService.GetDefaultConsistencyLevel()
	respondOK(c, level)
}

// CalculateConsistencyScore 计算一致性评分
// POST /api/v1/consistency/score
func (h *VideoHandler) CalculateConsistencyScore(c *gin.Context) {
	if h.consistencyService == nil {
		respondErr(c, http.StatusServiceUnavailable, "consistency service unavailable")
		return
	}

	var req struct {
		ReferenceImage  string   `json:"reference_image"`
		GeneratedImages []string `json:"generated_images"`
	}
	if !bindJSON(c, &req) {
		return
	}

	score, err := h.consistencyService.CalculateConsistencyScore(req.ReferenceImage, req.GeneratedImages)
	if err != nil {
		reqLogger(c).Errorf("[VideoHandler] CalculateConsistencyScore: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, score)
}

// Export 多格式导出
// GET /api/v1/videos/:id/export/:format
// format: capcut | fcpxml | zip | shots | srt | vtt | edl | otio | csv | xlsx | broll
func (h *VideoHandler) Export(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	format := c.Param("format")

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(id), 0)
	if err != nil {
		reqLogger(c).Errorf("[VideoHandler] Export: videoID=%d get storyboard err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	var result *service.ExportResult
	switch format {
	case "fcpxml":
		result, err = h.capcutService.ExportFCPXML(video, shots)
	case "zip":
		var bgmSegs []*model.VideoBGMSegment
		if h.bgmRepo != nil {
			bgmSegs, _ = h.bgmRepo.ListByVideoID(uint(id))
		}
		result, err = h.capcutService.ExportResourceZip(video, shots, bgmSegs)
	case "shots":
		result, err = h.capcutService.ExportShotSlices(video, shots)
	case "srt":
		result, err = h.capcutService.ExportSRT(video, shots)
	case "vtt":
		result, err = h.capcutService.ExportVTT(video, shots)
	case "edl":
		result, err = h.capcutService.ExportEDL(video, shots)
	case "otio":
		result, err = h.capcutService.ExportOTIO(video, shots)
	case "csv":
		result, err = h.capcutService.ExportCSV(video, shots)
	case "xlsx":
		result, err = h.capcutService.ExportXLSX(video, shots)
	case "broll":
		novel, _ := h.videoService.GetNovelByID(video.NovelID)
		result, err = h.capcutService.ExportBRollDraft(video, shots, novel)
	default: // "capcut" 或其他
		novel, _ := h.videoService.GetNovelByID(video.NovelID)
		var bgmSegs []*model.VideoBGMSegment
		if h.bgmRepo != nil {
			bgmSegs, _ = h.bgmRepo.ListByVideoID(uint(id))
		}
		result, err = h.capcutService.ExportCapCutDraft(video, shots, novel, bgmSegs)
	}

	if err != nil {
		reqLogger(c).Errorf("[VideoHandler] Export: videoID=%d format=%s err=%v", id, format, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	reqLogger(c).Printf("[VideoHandler] Export: videoID=%d format=%s filename=%s size=%d", id, format, result.Filename, len(result.Data))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, result.Filename))
	c.Header("Content-Length", strconv.Itoa(len(result.Data)))
	c.Data(http.StatusOK, result.ContentType, result.Data)
}

// ─────────────────────────────────────────────────────────────────────────────
// 声画同步时间轴
// ─────────────────────────────────────────────────────────────────────────────

// ComputeTimeline POST /videos/:id/compute-timeline
// 计算所有分镜的绝对时间轴（timeline_start），写入 DB，并返回同步清单和 FFmpeg 脚本。
func (h *VideoHandler) ComputeTimeline(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	manifest, err := h.videoService.ComputeTimeManifest(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, manifest)
}

// GetSyncManifest GET /videos/:id/sync-manifest
// 获取最新同步清单（只读，不重新计算时间轴）。
func (h *VideoHandler) GetSyncManifest(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	manifest, err := h.videoService.ComputeTimeManifest(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, manifest)
}

// ─────────────────────────────────────────────────────────────────────────────
// 口型对齐 (LipSync)
// ─────────────────────────────────────────────────────────────────────────────

// GenerateLipSync POST /videos/:id/shots/:shot_id/lipsync
// 提交口型对齐任务：角色参考图 + TTS 音频 → 口型视频。
// 请求体（均可选）：{ "audio_url": "...", "image_url": "...", "model": "kling-v1-6" }
// 立即返回 task_id；前端可轮询 GET .../lipsync/status 或等待 shot.status 变为 done。
func (h *VideoHandler) GenerateLipSync(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var req service.LipSyncRequest
	_ = c.ShouldBindJSON(&req)

	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeLipSync 的执行
	// 函数在 cmd/server/task_resume.go，用 t.EntityID 作为 shotID，反序列化下面存的
	// video_id/req 调用同一套 GenerateLipSyncVideoWithReq + PollLipSyncUntilDone）。
	task, err := h.taskSvc.CreateWithParams(getTenantID(c), service.TaskTypeLipSync,
		fmt.Sprintf("口型对齐 shot #%d", shotID), "shot", uint(shotID), map[string]interface{}{
			"video_id": uint(videoID),
			"req":      req,
		})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "口型对齐任务已提交")
}

// GetLipSyncStatus GET /videos/:id/shots/:shot_id/lipsync/status
// 查询口型对齐任务状态（前端轮询）。
func (h *VideoHandler) GetLipSyncStatus(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	result, err := h.videoService.GetLipSyncStatus(uint(videoID), uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, result)
}

// ─────────────────────────────────────────────────────────────────────────────
// 分镜语音段落 (VoiceSegment) 处理器
// ─────────────────────────────────────────────────────────────────────────────

// ListVoiceSegments GET /videos/:id/shots/:shot_id/segments
