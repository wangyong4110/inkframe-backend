package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ForeshadowHandler 伏笔管理处理器
type ForeshadowHandler struct {
	foreshadowSvc *service.ForeshadowService
}

func NewForeshadowHandler(foreshadowSvc *service.ForeshadowService) *ForeshadowHandler {
	return &ForeshadowHandler{foreshadowSvc: foreshadowSvc}
}

// GetForeshadows 获取伏笔列表
// @Summary 获取小说的伏笔列表
// @Tags foreshadows
// @Param novel_id path int true "小说ID"
// @Success 200
// @Router /api/v1/novels/{novel_id}/foreshadows [get]
func (h *ForeshadowHandler) GetForeshadows(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	chapterNo := 0
	if cn := c.Query("chapter_no"); cn != "" {
		chapterNo, _ = strconv.Atoi(cn)
	}

	foreshadows, err := h.foreshadowSvc.CheckForeshadowStatus(uint(novelID), chapterNo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 分离已回收和未回收
	var pending, fulfilled []*service.ForeshadowItem
	for _, fs := range foreshadows {
		if fs.IsFulfilled {
			fulfilled = append(fulfilled, fs)
		} else {
			pending = append(pending, fs)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"pending":  pending,
		"fulfilled": fulfilled,
		"total":     len(foreshadows),
	})
}

// MarkFulfilled 标记伏笔已回收
// @Summary 标记伏笔已回收
// @Tags foreshadows
// @Param novel_id path int true "小说ID"
// @Param foreshadow_id path int true "伏笔ID"
// @Param request body map[string]interface{} true "回收信息"
// @Success 200
// @Router /api/v1/novels/{novel_id}/foreshadows/{foreshadow_id}/fulfill [post]
func (h *ForeshadowHandler) MarkFulfilled(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	foreshadowID, err := strconv.ParseUint(c.Param("foreshadow_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid foreshadow_id"})
		return
	}

	var req struct {
		ChapterID uint `json:"chapter_id"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 简化：不需要传入chapter对象
	err = h.foreshadowSvc.MarkFulfilled(uint(novelID), uint(foreshadowID), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "伏笔已标记为已回收"})
}

// ============================================
// Timeline Handler - 时间线处理器
// ============================================

type TimelineHandler struct {
	timelineSvc *service.TimelineService
}

func NewTimelineHandler(timelineSvc *service.TimelineService) *TimelineHandler {
	return &TimelineHandler{timelineSvc: timelineSvc}
}

// GetTimeline 获取时间线
// @Summary 获取小说时间线
// @Tags timeline
// @Param novel_id path int true "小说ID"
// @Success 200
// @Router /api/v1/novels/{novel_id}/timeline [get]
func (h *TimelineHandler) GetTimeline(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	timeline, err := h.timelineSvc.BuildTimeline(uint(novelID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, timeline)
}

// ============================================
// Character Arc Handler - 角色弧光处理器
// ============================================

type CharacterArcHandler struct {
	arcSvc *service.CharacterArcService
}

func NewCharacterArcHandler(arcSvc *service.CharacterArcService) *CharacterArcHandler {
	return &CharacterArcHandler{arcSvc: arcSvc}
}

// GetCharacterArc 获取角色弧光
// @Summary 获取角色弧光
// @Tags characters
// @Param character_id path int true "角色ID"
// @Success 200
// @Router /api/v1/characters/{character_id}/arc [get]
func (h *CharacterArcHandler) GetCharacterArc(c *gin.Context) {
	charID, err := strconv.ParseUint(c.Param("character_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid character_id"})
		return
	}

	// 需要传入novel_id来获取完整弧光
	novelID, err := strconv.ParseUint(c.Query("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "novel_id is required"})
		return
	}

	arc, err := h.arcSvc.GetCharacterArc(uint(novelID), uint(charID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, arc)
}

// GetAllCharacterArcs 获取所有角色弧光
// @Summary 获取小说的所有角色弧光
// @Tags characters
// @Param novel_id path int true "小说ID"
// @Success 200
// @Router /api/v1/novels/{novel_id}/character-arcs [get]
func (h *CharacterArcHandler) GetAllCharacterArcs(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	// TODO: 需要从角色服务获取所有角色
	// 然后为每个角色生成弧光
	arcs := make([]*service.CharacterArc, 0)

	c.JSON(http.StatusOK, gin.H{
		"arcs": ars,
	})
}

// ============================================
// Style Handler - 风格配置处理器
// ============================================

type StyleHandler struct {
	styleSvc *service.StyleService
}

func NewStyleHandler(styleSvc *service.StyleService) *StyleHandler {
	return &StyleHandler{styleSvc: styleSvc}
}

// GetDefaultStyle 获取默认风格
// @Summary 获取默认风格配置
// @Tags styles
// @Success 200
// @Router /api/v1/styles/default [get]
func (h *StyleHandler) GetDefaultStyle(c *gin.Context) {
	style := h.styleSvc.GetDefaultStyle()
	c.JSON(http.StatusOK, style)
}

// GetStylePrompt 获取风格提示词
// @Summary 获取风格提示词
// @Tags styles
// @Param request body service.StyleConfig true "风格配置"
// @Success 200
// @Router /api/v1/styles/prompt [post]
func (h *StyleHandler) GetStylePrompt(c *gin.Context) {
	var config service.StyleConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	prompt := h.styleSvc.BuildStylePrompt(&config)
	c.JSON(http.StatusOK, gin.H{"prompt": prompt})
}

// ============================================
// Generation Context Handler - 生成上下文处理器
// ============================================

type GenerationContextHandler struct {
	ctxSvc *service.GenerationContextService
}

func NewGenerationContextHandler(ctxSvc *service.GenerationContextService) *GenerationContextHandler {
	return &GenerationContextHandler{ctxSvc: ctxSvc}
}

// GetContext 获取生成上下文
// @Summary 获取生成所需的完整上下文
// @Tags generation
// @Param novel_id path int true "小说ID"
// @Param chapter_no query int true "章节号"
// @Success 200
// @Router /api/v1/novels/{novel_id}/context [get]
func (h *GenerationContextHandler) GetContext(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	chapterNo, err := strconv.Atoi(c.Query("chapter_no"))
	if err != nil || chapterNo <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chapter_no is required"})
		return
	}

	ctx, err := h.ctxSvc.GetContext(uint(novelID), chapterNo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, ctx)
}

// BuildPrompt 构建生成提示词
// @Summary 构建带上下文的生成提示词
// @Tags generation
// @Param novel_id path int true "小说ID"
// @Param request body BuildPromptRequest true "提示词构建请求"
// @Success 200
// @Router /api/v1/novels/{novel_id}/prompt [post]
func (h *GenerationContextHandler) BuildPrompt(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	var req BuildPromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 获取上下文
	ctx, err := h.ctxSvc.GetContext(uint(novelID), req.ChapterNo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 构建提示词
	var styleConfig *service.StyleConfig
	if req.Style != "" {
		var style service.StyleConfig
		json.Unmarshal([]byte(req.Style), &style)
		styleConfig = &style
	}

	prompt := h.ctxSvc.BuildGenerationPrompt(ctx, req.ChapterNo, styleConfig, req.ExtraPrompt)

	c.JSON(http.StatusOK, gin.H{
		"prompt": prompt,
		"context": ctx,
	})
}

// BuildPromptRequest 提示词构建请求
type BuildPromptRequest struct {
	ChapterNo   int    `json:"chapter_no" binding:"required"`
	Style      string `json:"style"`      // JSON格式的风格配置
	ExtraPrompt string `json:"extra_prompt"` // 额外提示
}

// ============================================
// Intelligent Storyboard Handler - 智能分镜处理器
// ============================================

type IntelligentStoryboardHandler struct {
	storyboardSvc *service.IntelligentStoryboardService
}

func NewIntelligentStoryboardHandler(storyboardSvc *service.IntelligentStoryboardService) *IntelligentStoryboardHandler {
	return &IntelligentStoryboardHandler{storyboardSvc: storyboardSvc}
}

// GenerateShots 生成分镜
// @Summary 智能生成分镜
// @Tags storyboard
// @Param request body GenerateShotsRequest true "分镜生成请求"
// @Success 200
// @Router /api/v1/storyboard/generate [post]
func (h *IntelligentStoryboardHandler) GenerateShots(c *gin.Context) {
	var req GenerateShotsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	shots, err := h.storyboardSvc.GenerateIntelligentShots(
		req.Content,
		req.Characters,
		req.Scene,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"shots":     shots,
		"total":     len(shots),
		"total_duration": calculateTotalDuration(shots),
	})
}

// GenerateShotsRequest 分镜生成请求
type GenerateShotsRequest struct {
	Content    string   `json:"content" binding:"required"`
	Characters []string `json:"characters"`
	Scene     string   `json:"scene"`
}

// calculateTotalDuration 计算总时长
func calculateTotalDuration(shots []*service.StoryboardShot) float64 {
	var total float64
	for _, shot := range shots {
		total += shot.Duration
	}
	return total
}

// AnalyzeEmotions 分析情感
// @Summary 分析章节情感
// @Tags storyboard
// @Param request body AnalyzeEmotionsRequest true "情感分析请求"
// @Success 200
// @Router /api/v1/storyboard/analyze-emotions [post]
func (h *IntelligentStoryboardHandler) AnalyzeEmotions(c *gin.Context) {
	var req AnalyzeEmotionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	analysis, err := h.storyboardSvc.AnalyzeEmotions(req.Content)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, analysis)
}

// AnalyzeEmotionsRequest 情感分析请求
type AnalyzeEmotionsRequest struct {
	Content string `json:"content" binding:"required"`
}

// ============================================
// Video Enhancement Handler - 视频增强处理器
// ============================================

type VideoEnhancementHandler struct {
	enhancementSvc *service.VideoEnhancementService
}

func NewVideoEnhancementHandler(enhancementSvc *service.VideoEnhancementService) *VideoEnhancementHandler {
	return &VideoEnhancementHandler{enhancementSvc: enhancementSvc}
}

// EnhanceVideo 增强视频
// @Summary 增强视频质量
// @Tags video
// @Param request body EnhanceVideoRequest true "增强请求"
// @Success 200
// @Router /api/v1/video/enhance [post]
func (h *VideoEnhancementHandler) EnhanceVideo(c *gin.Context) {
	var req EnhanceVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 转换配置
	configs := make([]service.EnhancementConfig, 0, len(req.Enhancements))
	for _, e := range req.Enhancements {
		configs = append(configs, service.EnhancementConfig{
			Type:             service.EnhancementType(e.Type),
			TargetFPS:        e.TargetFPS,
			ScaleFactor:      e.ScaleFactor,
			ColorGradePreset: e.ColorGradePreset,
			StylePreset:      e.StylePreset,
		})
	}

	jobs, err := h.enhancementSvc.EnhanceVideo(req.VideoURL, configs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"jobs": jobs,
	})
}

// EnhanceVideoRequest 增强请求
type EnhanceVideoRequest struct {
	VideoURL      string               `json:"video_url" binding:"required"`
	Enhancements []EnhancementRequest `json:"enhancements" binding:"required"`
}

// EnhancementRequest 增强配置请求
type EnhancementRequest struct {
	Type             string  `json:"type" binding:"required"` // frame_interpolation/super_resolution/color_grading/style_transfer
	TargetFPS       int     `json:"target_fps"`
	ScaleFactor     float64 `json:"scale_factor"`
	ColorGradePreset string `json:"color_grade_preset"`
	StylePreset     string `json:"style_preset"`
}

// GetRecommendations 获取增强建议
// @Summary 获取视频增强建议
// @Tags video
// @Param request body VideoInfoRequest true "视频信息"
// @Success 200
// @Router /api/v1/video/recommendations [post]
func (h *VideoEnhancementHandler) GetRecommendations(c *gin.Context) {
	var req VideoInfoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	recommendations := h.enhancementSvc.RecommendEnhancements(&struct {
		FPS        int
		Resolution string
		Duration   float64
		Style      string
	}{
		FPS:        req.FPS,
		Resolution: req.Resolution,
		Duration:   req.Duration,
		Style:      req.Style,
	})

	c.JSON(http.StatusOK, gin.H{"recommendations": recommendations})
}

// VideoInfoRequest 视频信息请求
type VideoInfoRequest struct {
	FPS        int     `json:"fps"`
	Resolution string  `json:"resolution"`
	Duration   float64 `json:"duration"`
	Style      string  `json:"style"`
}

// ============================================
// Consistency Handler - 一致性控制处理器
// ============================================

type ConsistencyHandler struct {
	consistencySvc *service.CharacterConsistencyService
}

func NewConsistencyHandler(consistencySvc *service.CharacterConsistencyService) *ConsistencyHandler {
	return &ConsistencyHandler{consistencySvc: consistencySvc}
}

// GetDefaultConsistency 获取默认一致性配置
// @Summary 获取默认一致性配置
// @Tags consistency
// @Success 200
// @Router /api/v1/consistency/default [get]
func (h *ConsistencyHandler) GetDefaultConsistency(c *gin.Context) {
	config := h.consistencySvc.GetDefaultConsistencyLevel()
	c.JSON(http.StatusOK, config)
}

// CalculateScore 计算一致性评分
// @Summary 计算图像一致性评分
// @Tags consistency
// @Param request body CalculateScoreRequest true "评分请求"
// @Success 200
// @Router /api/v1/consistency/score [post]
func (h *ConsistencyHandler) CalculateScore(c *gin.Context) {
	var req CalculateScoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	score, err := h.consistencySvc.CalculateConsistencyScore(
		req.ReferenceImage,
		req.GeneratedImages,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, score)
}

// CalculateScoreRequest 评分请求
type CalculateScoreRequest struct {
	ReferenceImage  string   `json:"reference_image" binding:"required"`
	GeneratedImages []string `json:"generated_images" binding:"required"`
}
