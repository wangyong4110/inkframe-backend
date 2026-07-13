package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
			reqLogger(c).Errorf("[VideoHandler] UpdatePacingConfig failed (non-fatal): %v", err)
		}
	}

	tenantID := getTenantID(c)
	// 取代同一视频上正在运行的旧任务：不仅把 DB 行标记为 cancelled，还真正调用旧任务注册的
	// cancel 函数，让旧 goroutine 里的 AI 请求收到取消信号（而不是标记完就不管，继续跑到底）。
	h.taskSvc.CancelActiveByEntityAndInvoke("video", uint(videoId), service.TaskTypeStoryboardGen)

	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeStoryboardGen
	// 的执行函数在 cmd/server/task_resume.go，反序列化下面存的字段调用同一个
	// h.storyboardService.GenerateStoryboardCtx）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeStoryboardGen, "分镜脚本生成", "video", uint(videoId), map[string]interface{}{
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
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c),
			Action: "storyboard.generate", ResourceType: "video", ResourceID: uint(videoId),
			IP: c.ClientIP(),
		})
	}

	respondAccepted(c, task.TaskID, "分镜生成任务已提交")
}

// shotWithAudio 在分镜基础上增加可直接播放的 audio_url 字段
type shotWithAudio struct {
	*model.StoryboardShot
	AudioURL string `json:"audio_url"`
}

// MarshalJSON 必须显式定义，否则 *model.StoryboardShot.MarshalJSON() 通过方法提升成为
// shotWithAudio 的 MarshalJSON，导致 AudioURL 字段被完全忽略。
// 此处先调用嵌入 shot 的序列化，再将 audio_url 拼接进 JSON 对象。
func (s shotWithAudio) MarshalJSON() ([]byte, error) {
	shotJSON, err := s.StoryboardShot.MarshalJSON()
	if err != nil {
		return nil, err
	}
	if s.AudioURL == "" {
		return shotJSON, nil
	}
	// 将 audio_url 注入到 JSON 对象末尾（在最后一个 } 之前）
	audioVal, err := json.Marshal(s.AudioURL)
	if err != nil {
		return nil, err
	}
	if len(shotJSON) < 2 || shotJSON[len(shotJSON)-1] != '}' {
		return shotJSON, nil
	}
	result := make([]byte, 0, len(shotJSON)+len(audioVal)+14)
	result = append(result, shotJSON[:len(shotJSON)-1]...)
	result = append(result, []byte(`,"audio_url":`)...)
	result = append(result, audioVal...)
	result = append(result, '}')
	return result, nil
}

// ResolveAudioURL returns the serve endpoint for a shot's voice audio.
// The endpoint delegates to the first VoiceSegment with audio. Exported so
// cmd/server/task_resume.go can build the same URL when completing a voice_gen task.
func ResolveAudioURL(videoID uint, shot *model.StoryboardShot) string {
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
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeStoryboardReview
	// 的执行函数在 cmd/server/task_resume.go，反序列化下面存的字段调用同一个
	// h.storyboardService.ReviewStoryboard）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeStoryboardReview, "分镜 AI 审查", "video", uint(videoId), map[string]interface{}{
		"provider":       req.Provider,
		"previous_score": req.PreviousScore,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "分镜审查任务已提交")
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
		reqLogger(c).Errorf("[VideoHandler] GetStoryboard: videoID=%d err=%v", videoId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	shotIDs := make([]uint, len(shots))
	for i, s := range shots {
		shotIDs[i] = s.ID
	}
	audioMap := h.videoService.GetShotAudioMap(shotIDs)

	result := make([]shotWithAudio, len(shots))
	for i, s := range shots {
		audioURL := ""
		if _, hasAudio := audioMap[s.ID]; hasAudio {
			audioURL = ResolveAudioURL(uint(videoId), s)
		}
		result[i] = shotWithAudio{
			StoryboardShot: s,
			AudioURL:       audioURL,
		}
	}
	respondOK(c, result)
}

// ServeAudio 供前端播放配音文件
// GET /api/v1/videos/:id/storyboard/:shot_id/audio
// 该端点位于公开路由区域（无 JWT），用 shot.VideoID 校验归属关系。
func (h *VideoHandler) ServeAudio(c *gin.Context) {
	videoId, ok := parseID(c, "id")
	if !ok {
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
	if shot.VideoID != uint(videoId) {
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
		c.Header("Content-Type", "audio/mpeg")
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
		reqLogger(c).Errorf("[VideoHandler] UpdateStoryboardShot: shotID=%d err=%v", shotId, err)
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
		reqLogger(c).Errorf("[VideoHandler] SetShotCharacters: shotID=%d err=%v", shotID, err)
		respondErr(c, http.StatusInternalServerError, "failed to set shot characters")
		return
	}
	respondOK(c, nil)
}

// SetShotItems 手动绑定分镜物品
// PUT /api/v1/videos/:id/shots/:shot_id/items
func (h *VideoHandler) SetShotItems(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	var body struct {
		ItemIDs []uint `json:"item_ids"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if err := h.videoService.SetShotItems(uint(shotID), body.ItemIDs); err != nil {
		reqLogger(c).Errorf("[VideoHandler] SetShotItems: shotID=%d err=%v", shotID, err)
		respondErr(c, http.StatusInternalServerError, "failed to set shot items")
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
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeStoryboardOptimize
	// 的执行函数在 cmd/server/task_resume.go，反序列化下面存的 review/provider 调用同一个
	// h.storyboardService.OptimizeStoryboardFromReview）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeStoryboardOptimize, "分镜一键优化", "video", uint(videoID), map[string]interface{}{
		"review":   req.StoryboardReview,
		"provider": req.Provider,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "分镜优化任务已提交")
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
		reqLogger(c).Errorf("[VideoHandler] ListReviewRecords videoID=%d err=%v", videoID, err)
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
		reqLogger(c).Errorf("[VideoHandler] AnalyzeEmotions: err=%v", err)
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
		reqLogger(c).Errorf("[VideoHandler] StartVideoGeneration: videoID=%d err=%v", id, err)
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
		reqLogger(c).Errorf("[VideoHandler] GetVideoStatus: videoID=%d err=%v", id, err)
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
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeVideoGen 的
	// 执行函数在 cmd/server/task_resume.go，反序列化下面存的 video_id/mode 调用同一套
	// GenerateAllShotVideos + PollAndStitchVideo）。
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeVideoGen, "视频生成", "video", uint(id), map[string]interface{}{
		"video_id": uint(id),
		"mode":     video.Mode,
	})
	if err != nil {
		reqLogger(c).Errorf("[VideoHandler] GenerateShotVideos: create task videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondAccepted(c, task.TaskID, "视频生成任务已提交")
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
		reqLogger(c).Errorf("[VideoHandler] ListShots: videoID=%d err=%v", id, err)
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
		reqLogger(c).Errorf("[VideoHandler] StitchVideo: videoID=%d err=%v", id, err)
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
	outputPath := video.TaskMeta.VideoPath
	if outputPath == "" {
		var err error
		outputPath, err = h.videoService.StitchVideo(uint(id))
		if err != nil {
			reqLogger(c).Errorf("[VideoHandler] DownloadVideo stitch: videoID=%d err=%v", id, err)
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
