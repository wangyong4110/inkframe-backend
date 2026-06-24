package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

func (h *VideoHandler) GenerateStoryboard(c *gin.Context) {
	videoId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoId)); !ok {
		return
	}

	var req struct {
		ChapterID      uint     `json:"chapter_id"`
		Characters     []string `json:"characters"`
		Style          string   `json:"style,omitempty"`
		Provider       string   `json:"provider,omitempty"`        // 指定 LLM 提供者，可为空
		UserPrompt     string   `json:"user_prompt,omitempty"`     // 用户自定义提示词
		Pacing         string   `json:"pacing,omitempty"`          // slow/normal/fast
		TargetDuration int      `json:"target_duration,omitempty"` // 0=自动估算
		MaxTokens      int      `json:"max_tokens,omitempty"`      // 0=使用系统默认
		Temperature    float64  `json:"temperature,omitempty"`     // 0=使用系统默认
		TimeoutSeconds int      `json:"timeout_seconds,omitempty"` // 0=使用系统默认(180s)
		VoiceMode      string   `json:"voice_mode,omitempty"`      // ""/"auto"/"both"=自动, "narration"=仅旁白, "dialogue"=仅对白, "narration_primary"=旁白为主, "dialogue_primary"=对白为主
	}
	// 所有字段均可选，body 为空时忽略 EOF
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}

	// 若请求携带节奏/时长配置，持久化到 Video 记录，后续 GenerateStoryboard 读取
	if req.Pacing != "" || req.TargetDuration != 0 {
		if err := h.videoService.UpdatePacingConfig(uint(videoId), req.Pacing, req.TargetDuration); err != nil {
			logger.Errorf("[VideoHandler] UpdatePacingConfig failed (non-fatal): %v", err)
		}
	}

	tenantID := getTenantID(c)
	// 取消正在运行的旧 goroutine（通知其 context 取消），再标记旧任务为 cancelled。
	h.videoService.CancelStoryboardGeneration(uint(videoId))
	h.taskSvc.CancelActiveByEntity("video", uint(videoId), service.TaskTypeStoryboardGen)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardGen, "分镜脚本生成", "video", uint(videoId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"chapter_id":      req.ChapterID,
		"characters":      req.Characters,
		"style":           req.Style,
		"provider":        req.Provider,
		"user_prompt":     req.UserPrompt,
		"max_tokens":      req.MaxTokens,
		"temperature":     req.Temperature,
		"timeout_seconds": req.TimeoutSeconds,
		"voice_mode":      req.VoiceMode,
	})

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[VideoHandler] GenerateStoryboard task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck

		overrides := service.StoryboardOverrides{
			MaxTokens:      req.MaxTokens,
			Temperature:    req.Temperature,
			TimeoutSeconds: req.TimeoutSeconds,
			VoiceMode:      req.VoiceMode,
		}
		result, err := h.storyboardService.GenerateStoryboard(uint(videoId), req.ChapterID, req.Characters, req.Style, req.Provider, req.UserPrompt, progressFn, overrides)
		if err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			logger.Errorf("[VideoHandler] GenerateStoryboard task %s failed: %v", taskID, err)
			return
		}
		// 只存 shot_count，不把完整分镜数组写入 result 列（JSON 可能超出 TEXT 65KB 限制导致 Update 失败，任务永远卡在 99%）
		var shotCount int
		if shots, ok := result.([]*model.StoryboardShot); ok {
			shotCount = len(shots)
		}
		h.taskSvc.Complete(taskID, gin.H{"shot_count": shotCount}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// shotWithAudio 在分镜基础上增加可直接播放的 audio_url 字段
type shotWithAudio struct {
	*model.StoryboardShot
	AudioURL string `json:"audio_url"`
}

// resolveAudioURL returns the serve endpoint for a shot's voice audio.
// The endpoint delegates to the first VoiceSegment with audio.
func resolveAudioURL(videoID uint, shot *model.StoryboardShot) string {
	return fmt.Sprintf("/api/v1/videos/%d/storyboard/%d/audio", videoID, shot.ID)
}

// ReviewStoryboard 对分镜脚本进行 AI 专业审查（异步任务）
// POST /api/v1/videos/:id/storyboard/review
// 立即返回 202 + task_id，轮询 GET /:id/storyboard/review/:task_id 获取结果
func (h *VideoHandler) ReviewStoryboard(c *gin.Context) {
	videoId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoId)); !ok {
		return
	}

	var req struct {
		Provider      string  `json:"provider"`
		PreviousScore float64 `json:"previous_score"` // 上次审查分数，用于稳定相对评分
	}
	_ = c.ShouldBindJSON(&req) // 可选 body

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardReview, "分镜 AI 审查", "video", uint(videoId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider":       req.Provider,
		"previous_score": req.PreviousScore,
	})

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[VideoHandler] ReviewStoryboard task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		review, recordID, reviewErr := h.storyboardService.ReviewStoryboard(tenantID, uint(videoId), req.Provider, req.PreviousScore)
		if reviewErr != nil {
			logger.Errorf("[VideoHandler] ReviewStoryboard task %s failed: videoID=%d err=%v", taskID, videoId, reviewErr)
			h.taskSvc.Fail(taskID, reviewErr.Error()) //nolint:errcheck
			return
		}
		// 将 record_id 附在 task data 中，前端可用于关联后续 apply/rollback
		type reviewResult struct {
			*model.StoryboardReview
			RecordID uint `json:"record_id,omitempty"`
		}
		h.taskSvc.UpdateProgress(taskID, 90)                                                      //nolint:errcheck
		h.taskSvc.Complete(taskID, &reviewResult{StoryboardReview: review, RecordID: recordID}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜审查任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GetStoryboard 获取分镜列表
// GET /api/v1/videos/:id/storyboard
func (h *VideoHandler) GetStoryboard(c *gin.Context) {
	videoId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoId)); !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(videoId))
	if err != nil {
		logger.Errorf("[VideoHandler] GetStoryboard: videoID=%d err=%v", videoId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	result := make([]shotWithAudio, len(shots))
	for i, s := range shots {
		result[i] = shotWithAudio{
			StoryboardShot: s,
			AudioURL:       resolveAudioURL(uint(videoId), s),
		}
	}
	respondOK(c, result)
}

// ServeAudio 供前端播放配音文件
// GET /api/v1/videos/:id/storyboard/:shot_id/audio
func (h *VideoHandler) ServeAudio(c *gin.Context) {
	videoId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoId)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	shot, err := h.videoService.GetShot(uint(shotID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "shot not found")
		return
	}

	// Load first voice segment with audio
	segs, _ := h.videoService.ListVoiceSegments(shot.ID)
	var audioPath string
	for _, seg := range segs {
		if seg.AudioPath != "" {
			audioPath = seg.AudioPath
			break
		}
	}
	if audioPath == "" {
		respondErr(c, http.StatusNotFound, "no audio for this shot")
		return
	}
	if strings.HasPrefix(audioPath, "http://") || strings.HasPrefix(audioPath, "https://") {
		c.Redirect(http.StatusFound, audioPath)
		return
	}
	if strings.HasPrefix(audioPath, "file://") {
		filePath := strings.TrimPrefix(audioPath, "file://")
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.File(filePath)
		return
	}
	c.Redirect(http.StatusFound, audioPath)
}

// UpdateStoryboardShot 更新分镜（支持部分字段更新）
// PUT /api/v1/videos/:id/storyboard/:shot_id
func (h *VideoHandler) UpdateStoryboardShot(c *gin.Context) {
	shotId, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var fields map[string]interface{}
	if !bindJSON(c, &fields) {
		return
	}

	shot, err := h.videoService.UpdateShotPartial(uint(shotId), fields)
	if err != nil {
		logger.Errorf("[VideoHandler] UpdateStoryboardShot: shotID=%d err=%v", shotId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, shot)
}

// SetShotCharacters 手动绑定分镜角色
// PUT /api/v1/videos/:id/shots/:shot_id/characters
func (h *VideoHandler) SetShotCharacters(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	var body struct {
		CharacterIDs []uint `json:"character_ids"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if err := h.videoService.SetShotCharacters(uint(shotID), body.CharacterIDs); err != nil {
		logger.Errorf("[VideoHandler] SetShotCharacters: shotID=%d err=%v", shotID, err)
		respondErr(c, http.StatusInternalServerError, "failed to set shot characters")
		return
	}
	respondOK(c, nil)
}

// OptimizeStoryboardFromReview 根据 AI 审查报告一键优化分镜（异步任务）
// POST /api/v1/videos/:id/storyboard/optimize-from-review
// Body: StoryboardReview JSON（由 review 任务结果直接透传）+ 可选 provider
func (h *VideoHandler) OptimizeStoryboardFromReview(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	var req struct {
		model.StoryboardReview
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "request body must contain a valid StoryboardReview: "+err.Error())
		return
	}
	if len(req.GlobalSuggestions) == 0 && len(req.ShotFeedback) == 0 {
		respondBadRequest(c, "审查报告中无改进建议，无需优化")
		return
	}

	tenantID := getTenantID(c)
	h.taskSvc.CancelActiveByEntity("video", uint(videoID), service.TaskTypeStoryboardOptimize)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardOptimize, "分镜一键优化", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"review":   req.StoryboardReview,
		"provider": req.Provider,
	})

	review := req.StoryboardReview
	provider := req.Provider
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[VideoHandler] OptimizeStoryboardFromReview task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		count, optErr := h.storyboardService.OptimizeStoryboardFromReview(tenantID, uint(videoID), &review, provider)
		if optErr != nil {
			logger.Errorf("[VideoHandler] OptimizeStoryboardFromReview task %s failed: %v", taskID, optErr)
			h.taskSvc.Fail(taskID, optErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90)                              //nolint:errcheck
		h.taskSvc.Complete(taskID, gin.H{"updated_shots": count}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜优化任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// ApplyStoryboardDiffs 将用户选中的差异直接写入 DB（同步，无 AI 调用）。
// POST /api/v1/videos/:id/storyboard/optimize/apply
func (h *VideoHandler) ApplyStoryboardDiffs(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	var req struct {
		Diffs    []service.ShotApplyDiff `json:"diffs" binding:"required"`
		RecordID uint                    `json:"record_id"` // 可选，关联审查记录以记录回滚快照
	}
	if !bindJSON(c, &req) {
		return
	}
	if len(req.Diffs) == 0 {
		respondBadRequest(c, "diffs 列表不能为空")
		return
	}

	count, err := h.videoService.ApplyStoryboardDiffs(uint(videoID), req.Diffs, req.RecordID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"updated_shots": count})
}

// ListReviewRecords 获取某视频的审查历史列表
// GET /api/v1/videos/:id/storyboard/reviews
func (h *VideoHandler) ListReviewRecords(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	records, err := h.storyboardService.ListReviewRecords(uint(videoID))
	if err != nil {
		logger.Errorf("[VideoHandler] ListReviewRecords videoID=%d err=%v", videoID, err)
		// 表可能尚未迁移（服务首次启动），返回空列表而不是 500
		respondOK(c, []struct{}{})
		return
	}

	// 将 ReviewJSON 反序列化后附在响应中
	type recordResp struct {
		ID           uint                 `json:"id"`
		CreatedAt    string               `json:"created_at"`
		OverallScore float64              `json:"overall_score"`
		Status       string               `json:"status"`
		AppliedAt    *string              `json:"applied_at,omitempty"`
		Review       *model.StoryboardReview `json:"review,omitempty"`
	}
	resp := make([]recordResp, 0, len(records))
	for _, rec := range records {
		r := recordResp{
			ID:           rec.ID,
			CreatedAt:    rec.CreatedAt.Format("2006-01-02 15:04:05"),
			OverallScore: rec.OverallScore,
			Status:       rec.Status,
		}
		if rec.AppliedAt != nil {
			s := rec.AppliedAt.Format("2006-01-02 15:04:05")
			r.AppliedAt = &s
		}
		if rec.ReviewJSON != "" {
			var rv model.StoryboardReview
			if err := json.Unmarshal([]byte(rec.ReviewJSON), &rv); err == nil {
				r.Review = &rv
			}
		}
		resp = append(resp, r)
	}
	respondOK(c, resp)
}

// RollbackReview 将分镜内容回滚到某次审查应用之前的状态
// POST /api/v1/videos/:id/storyboard/reviews/:record_id/rollback
func (h *VideoHandler) RollbackReview(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	recordID, ok := parseID(c, "record_id")
	if !ok {
		return
	}

	tenantID := getTenantID(c)
	restored, err := h.storyboardService.RollbackReview(tenantID, uint(videoID), uint(recordID))
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"restored_shots": restored})
}

// IgnoreSuggestion 永久忽略某条审查建议
// POST /api/v1/videos/:id/storyboard/ignored-suggestions
func (h *VideoHandler) IgnoreSuggestion(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	var req struct {
		ShotNo    int    `json:"shot_no" binding:"required"`
		IssueText string `json:"issue_text" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	tenantID := getTenantID(c)
	item, err := h.videoService.IgnoreSuggestion(tenantID, uint(videoID), req.ShotNo, req.IssueText)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, ignoredIssueToShotDTO(item))
}

// ListIgnoredSuggestions 列出已忽略的建议
// GET /api/v1/videos/:id/storyboard/ignored-suggestions
func (h *VideoHandler) ListIgnoredSuggestions(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	items, err := h.videoService.ListIgnoredSuggestions(uint(videoID))
	if err != nil {
		respondOK(c, []struct{}{})
		return
	}
	dtos := make([]ignoredShotDTO, 0, len(items))
	for _, it := range items {
		dtos = append(dtos, ignoredIssueToShotDTO(it))
	}
	respondOK(c, dtos)
}

// UnignoreSuggestion 取消忽略
// DELETE /api/v1/videos/:id/storyboard/ignored-suggestions/:suggestion_id
func (h *VideoHandler) UnignoreSuggestion(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	suggestionID, ok := parseID(c, "suggestion_id")
	if !ok {
		return
	}
	if err := h.videoService.UnignoreSuggestion(uint(videoID), uint(suggestionID)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, nil)
}

// ApplyReviewInserts 应用 AI 审查建议的插入分镜
// POST /api/v1/videos/:id/storyboard/review/apply-inserts
func (h *VideoHandler) ApplyReviewInserts(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	var req struct {
		Inserts []model.ShotInsertSuggestion `json:"inserts" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if len(req.Inserts) == 0 {
		respondBadRequest(c, "inserts 列表不能为空")
		return
	}
	count, err := h.storyboardService.ApplyReviewInserts(uint(videoID), req.Inserts)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"inserted_shots": count})
}

// ApplyReviewDeletes 应用 AI 审查建议的删除分镜
// POST /api/v1/videos/:id/storyboard/review/apply-deletes
func (h *VideoHandler) ApplyReviewDeletes(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	var req struct {
		ShotNos []int `json:"shot_nos" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if len(req.ShotNos) == 0 {
		respondBadRequest(c, "shot_nos 列表不能为空")
		return
	}
	count, err := h.storyboardService.ApplyReviewDeletes(uint(videoID), req.ShotNos)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"deleted_shots": count})
}

// AnalyzeEmotions 情感分析
// POST /api/v1/storyboard/analyze-emotions
func (h *VideoHandler) AnalyzeEmotions(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}

	result, err := h.storyboardService.AnalyzeEmotions(req.Content)
	if err != nil {
		logger.Errorf("[VideoHandler] AnalyzeEmotions: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// EnhanceVideo 增强视频
// POST /api/v1/video/enhance
func (h *VideoHandler) EnhanceVideo(c *gin.Context) {
	var req struct {
		VideoURL     string                    `json:"video_url" binding:"required"`
		Enhancements []model.EnhancementConfig `json:"enhancements"`
	}
	if !bindJSON(c, &req) {
		return
	}

	svcConfigs := make([]service.EnhancementConfig, 0, len(req.Enhancements))
	for _, ec := range req.Enhancements {
		svcConfigs = append(svcConfigs, service.EnhancementConfig{
			Type:      service.EnhancementType(ec.Type),
			Enabled:   ec.Enabled,
			Intensity: ec.Intensity,
		})
	}
	result, err := h.enhancementService.EnhanceVideoWithConfigs(req.VideoURL, svcConfigs)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// GetEnhancementRecommendations 获取增强建议
// POST /api/v1/video/recommendations
func (h *VideoHandler) GetEnhancementRecommendations(c *gin.Context) {
	var req struct {
		FPS        int    `json:"fps"`
		Resolution string `json:"resolution"`
		Duration   int    `json:"duration"`
		Style      string `json:"style"`
	}
	if !bindJSON(c, &req) {
		return
	}

	result, err := h.enhancementService.GetRecommendations(req.FPS, req.Resolution, req.Duration, req.Style)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, result)
}

// StartVideoGeneration 开始视频生成
// POST /api/v1/videos/:id/generate
func (h *VideoHandler) StartVideoGeneration(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	taskId, err := h.videoService.StartGeneration(uint(id))
	if err != nil {
		logger.Errorf("[VideoHandler] StartVideoGeneration: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"task_id": taskId,
	})
}

// GetVideoStatus 获取视频生成状态
// GET /api/v1/videos/:id/status
func (h *VideoHandler) GetVideoStatus(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	// 租户鉴权：确认该视频属于当前租户
	if _, ok := h.getVideoForTenant(c, uint(id)); !ok {
		return
	}

	status, err := h.videoService.GetStatus(uint(id))
	if err != nil {
		logger.Errorf("[VideoHandler] GetVideoStatus: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, status)
}

// GenerateShotVideos 提交所有分镜视频生成任务，并后台轮询拼接
// POST /api/v1/videos/:id/shots/generate
func (h *VideoHandler) GenerateShotVideos(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVideoGen, "视频生成", "video", uint(id))
	if err != nil {
		logger.Errorf("[VideoHandler] GenerateShotVideos: create task videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"video_id": uint(id),
		"mode":     video.Mode,
	})

	go func(taskID string, videoID uint, mode string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[VideoHandler] GenerateShotVideos task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 5)  //nolint:errcheck
		if err := h.videoService.GenerateAllShotVideos(videoID); err != nil {
			logger.Errorf("[VideoHandler] GenerateShotVideos task %s: submit failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
		// slideshow mode handles stitching internally; only poll for AI video mode
		if mode != "slideshow" {
			h.videoService.PollAndStitchVideo(videoID) // blocks until done or timeout
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"video_id": videoID}) //nolint:errcheck
	}(task.TaskID, uint(id), video.Mode)

	c.JSON(http.StatusAccepted, gin.H{
		"code": 0,
		"data": gin.H{"task_id": task.TaskID},
	})
}

// ListShots 获取所有分镜状态
// GET /api/v1/videos/:id/shots
func (h *VideoHandler) ListShots(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(id))
	if err != nil {
		logger.Errorf("[VideoHandler] ListShots: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, shots)
}

// StitchVideoHandler 手动触发视频拼接
// POST /api/v1/videos/:id/stitch
func (h *VideoHandler) StitchVideoHandler(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	outputPath, err := h.videoService.StitchVideo(uint(id))
	if err != nil {
		logger.Errorf("[VideoHandler] StitchVideo: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"output_path": outputPath,
	})
}

// DownloadVideo 下载完整 MP4（拼接所有分镜后直接发送文件）
// GET /api/v1/videos/:id/download
func (h *VideoHandler) DownloadVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	// 如果已经有拼接好的文件，直接下载；否则先触发拼接
	outputPath := video.VideoPath
	if outputPath == "" {
		var err error
		outputPath, err = h.videoService.StitchVideo(uint(id))
		if err != nil {
			logger.Errorf("[VideoHandler] DownloadVideo stitch: videoID=%d err=%v", id, err)
			respondErr(c, http.StatusInternalServerError, "视频拼接失败")
			return
		}
	}

	filename := fmt.Sprintf("inkframe-video-%d.mp4", id)
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Header("Content-Type", "video/mp4")
	c.File(outputPath)
}

// ─── Ignored suggestion DTO helpers ──────────────────────────────────────────

// ignoredShotDTO maps IgnoredReviewIssue to the API response expected by the frontend.
type ignoredShotDTO struct {
	ID        uint   `json:"id"`
	VideoID   uint   `json:"video_id"`
	ShotNo    int    `json:"shot_no"`
	IssueText string `json:"issue_text"`
	IssueHash string `json:"issue_hash"`
	CreatedAt string `json:"created_at"`
}

func ignoredIssueToShotDTO(item *model.IgnoredReviewIssue) ignoredShotDTO {
	var ctx struct {
		ShotNo int `json:"shot_no"`
	}
	_ = json.Unmarshal([]byte(item.ContextJSON), &ctx)
	return ignoredShotDTO{
		ID:        item.ID,
		VideoID:   item.EntityID,
		ShotNo:    ctx.ShotNo,
		IssueText: item.IssueText,
		IssueHash: item.IssueHash,
		CreatedAt: item.CreatedAt.Format("2006-01-02 15:04:05"),
	}
}

// GenerateSingleShot 生成单个分镜（异步任务模式，立即返回 task_id）
// POST /api/v1/videos/:id/shots/:shot_id/generate
