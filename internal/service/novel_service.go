package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ErrPermissionDenied is returned when a user tries to modify a resource they don't own.
var ErrPermissionDenied = errors.New("permission denied")

type NovelService struct {
	novelRepo           *repository.NovelRepository
	chapterRepo         *repository.ChapterRepository
	aiService           *AIService
	characterRepo       *repository.CharacterRepository
	snapshotRepo        *repository.CharacterStateSnapshotRepository
	plotPointService    *PlotPointService
	notifSvc            *NotificationService // 可选，用于章节生成完成通知
	outlineVersionRepo  *repository.NovelOutlineVersionRepository // 可选，大纲历史版本快照
	// 广场社交
	novelLikeRepo    *repository.NovelLikeRepository
	novelCommentRepo *repository.NovelCommentRepository
	novelViewDedup   sync.Map     // key "ip:id" → expiry time.Time
	stopCh           chan struct{} // closed by Shutdown() to stop background goroutines
}

func NewNovelService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	aiService *AIService,
) *NovelService {
	return &NovelService{
		novelRepo:   novelRepo,
		chapterRepo: chapterRepo,
		aiService:   aiService,
		stopCh:      make(chan struct{}),
	}
}

// Shutdown 停止所有后台 goroutine（优雅关闭时调用）。防止重复关闭 panic。
func (s *NovelService) Shutdown() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

// WithCharacterRepos 设置角色相关仓库（用于快照写入）
func (s *NovelService) WithCharacterRepos(characterRepo *repository.CharacterRepository, snapshotRepo *repository.CharacterStateSnapshotRepository) *NovelService {
	s.characterRepo = characterRepo
	s.snapshotRepo = snapshotRepo
	return s
}

// WithPlotPointService 注入剧情点服务（用于AI提取后保存）
func (s *NovelService) WithPlotPointService(svc *PlotPointService) *NovelService {
	s.plotPointService = svc
	return s
}

// WithNotificationService 注入通知服务（可选，用于章节生成完成后发送站内通知）
func (s *NovelService) WithNotificationService(svc *NotificationService) *NovelService {
	s.notifSvc = svc
	return s
}

// WithOutlineVersionRepo 注入大纲历史版本仓库（可选，用于大纲重新生成前自动快照）
func (s *NovelService) WithOutlineVersionRepo(repo *repository.NovelOutlineVersionRepository) *NovelService {
	s.outlineVersionRepo = repo
	return s
}

// GetAIService 返回 AIService（供 handler 查询默认 provider 名称）
func (s *NovelService) GetAIService() *AIService {
	return s.aiService
}

// CreateNovelRequest 创建小说请求
type CreateNovelRequest struct {
	Title           string `json:"title" binding:"required"`
	Description     string `json:"description"`
	Genre           string `json:"genre" binding:"required"`
	WorldviewID     *uint  `json:"worldview_id"`
	CoverImage      string `json:"cover_image"`
	Channel         string `json:"channel"`
	TargetWordCount int    `json:"target_word_count"`
	TargetChapters  int    `json:"target_chapters"`
	TenantID        uint
}

// Create 创建小说
func (s *NovelService) Create(req *CreateNovelRequest) (*model.Novel, error) {
	tenantID := req.TenantID
	if tenantID == 0 {
		return nil, fmt.Errorf("tenant_id is required")
	}
	novel := &model.Novel{
		UUID:            uuid.New().String(),
		TenantID:        tenantID,
		Title:           req.Title,
		Description:     req.Description,
		Genre:           req.Genre,
		Status:          "planning",
		WorldviewID:     req.WorldviewID,
		CoverImage:      req.CoverImage,
		Channel:         req.Channel,
		TargetWordCount: req.TargetWordCount,
		TargetChapters:  req.TargetChapters,
	}

	if err := s.novelRepo.Create(novel); err != nil {
		return nil, err
	}

	return novel, nil
}

// GetNovel 获取小说（含租户校验）
func (s *NovelService) GetNovel(id, tenantID uint) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if novel.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}
	return novel, nil
}

// ListNovelsFiltered 获取小说列表（带过滤器）
func (s *NovelService) ListNovelsFiltered(page, pageSize int, filters map[string]interface{}) ([]*model.Novel, int64, error) {
	return s.novelRepo.List(page, pageSize, filters)
}

// ListNovels 获取小说列表
func (s *NovelService) ListNovels(page, pageSize int) ([]*model.Novel, int, error) {
	novels, total, err := s.novelRepo.List(page, pageSize, nil)
	return novels, int(total), err
}

// UpdateNovelEntity 更新小说实体
func (s *NovelService) UpdateNovelEntity(novel *model.Novel) error {
	return s.novelRepo.Update(novel)
}

// DeleteNovel 删除小说及其全部关联数据（含租户校验）
func (s *NovelService) DeleteNovel(id, tenantID uint) error {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("not found")
	}
	if novel.TenantID != tenantID {
		return fmt.Errorf("not found")
	}
	return s.novelRepo.DeleteWithCascade(id)
}

// CreateNovel handler-compatible wrapper
func (s *NovelService) CreateNovel(req *model.CreateNovelRequest) (*model.Novel, error) {
	return s.Create(&CreateNovelRequest{
		Title:           req.Title,
		Description:     req.Description,
		Genre:           req.Genre,
		WorldviewID:     req.WorldviewID,
		CoverImage:      req.CoverImage,
		Channel:         req.Channel,
		TargetWordCount: req.TargetWordCount,
		TargetChapters:  req.TargetChapters,
		TenantID:        req.TenantID,
	})
}

// UpdateNovel handler-compatible wrapper（含租户校验）
func (s *NovelService) UpdateNovel(id, tenantID uint, req *model.UpdateNovelRequest) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if novel.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}
	if req.Title != "" {
		novel.Title = req.Title
	}
	if req.Description != "" {
		novel.Description = req.Description
	}
	if req.Genre != "" {
		novel.Genre = req.Genre
	}
	if req.Status != "" {
		novel.Status = req.Status
	}
	if req.WorldviewID != nil {
		novel.WorldviewID = req.WorldviewID
	}
	if req.CoverImage != "" {
		novel.CoverImage = req.CoverImage
	}
	if req.AIModel != "" {
		novel.AIModel = req.AIModel
	}
	if req.ImageModel != "" {
		novel.ImageModel = req.ImageModel
	}
	if req.VideoModel != "" {
		novel.VideoModel = req.VideoModel
	}
	if req.TTSModel != "" {
		novel.TTSModel = req.TTSModel
	}
	if req.Temperature != nil {
		novel.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		novel.TopP = *req.TopP
	}
	if req.TopK != nil {
		novel.TopK = *req.TopK
	}
	if req.MaxTokens != nil {
		novel.MaxTokens = *req.MaxTokens
	}
	if req.StylePrompt != "" {
		novel.StylePrompt = req.StylePrompt
	}
	if req.ImageStyle != "" {
		novel.ImageStyle = req.ImageStyle
	}
	if req.ReferenceStyle != "" {
		novel.ReferenceStyle = req.ReferenceStyle
	}
	if req.PromptLanguage != "" {
		novel.PromptLanguage = req.PromptLanguage
	}
	if req.CoreTheme != "" {
		novel.CoreTheme = req.CoreTheme
	}
	if req.AutoReviewRounds != nil {
		rounds := *req.AutoReviewRounds
		if rounds < 0 {
			rounds = 0
		}
		if rounds > 5 {
			rounds = 5
		}
		novel.AutoReviewRounds = rounds
	}
	if req.AutoReviewMinScore != nil {
		score := *req.AutoReviewMinScore
		if score < 0 {
			score = 0
		}
		if score > 100 {
			score = 100
		}
		novel.AutoReviewMinScore = score
	}
	if req.TargetWordCount != nil {
		novel.TargetWordCount = *req.TargetWordCount
	}
	if req.TargetChapters != nil {
		novel.TargetChapters = *req.TargetChapters
	}
	// 视频/字幕配置写入 VideoConfig（通过 EnsureVideoConfig 懒初始化）
	vc := novel.EnsureVideoConfig()
	if req.VideoType != "" {
		vc.VideoType = req.VideoType
	}
	if req.VideoResolution != "" {
		vc.VideoResolution = req.VideoResolution
	}
	if req.VideoFPS != nil {
		vc.VideoFPS = *req.VideoFPS
	}
	if req.VideoAspectRatio != "" {
		vc.VideoAspectRatio = req.VideoAspectRatio
	}
	if req.CharConsistencyWeight != nil {
		vc.CharConsistencyWeight = *req.CharConsistencyWeight
	}
	if req.AssetExportPath != "" {
		vc.AssetExportPath = req.AssetExportPath
	}
	if req.NarrationVoice != "" {
		vc.NarrationVoice = req.NarrationVoice
	}
	// 字幕配置（可清空）
	if req.SubtitleEnabled != nil {
		vc.SubtitleEnabled = *req.SubtitleEnabled
	}
	if req.SubtitlePosition != "" {
		vc.SubtitlePosition = req.SubtitlePosition
	}
	if req.SubtitleFontSize != nil {
		vc.SubtitleFontSize = *req.SubtitleFontSize
	}
	if req.SubtitleColor != "" {
		vc.SubtitleColor = req.SubtitleColor
	}
	if req.SubtitleBgStyle != "" {
		vc.SubtitleBgStyle = req.SubtitleBgStyle
	}
	if req.SubtitleFont != "" {
		vc.SubtitleFont = req.SubtitleFont
	}
	// 超时
	if req.TimeoutSeconds != nil {
		novel.TimeoutSeconds = *req.TimeoutSeconds
	}
	// 色彩调色
	if req.ColorGrade != "" {
		vc.ColorGrade = req.ColorGrade
	}
	if req.ContrastLevel != nil {
		vc.ContrastLevel = *req.ContrastLevel
	}
	if req.Saturation != nil {
		vc.Saturation = *req.Saturation
	}
	// 镜头特效（bool 用指针，false 也要写入）
	if req.FilmGrain != nil {
		vc.FilmGrain = *req.FilmGrain
	}
	if req.Vignette != nil {
		vc.Vignette = *req.Vignette
	}
	if req.ChromaticAberration != nil {
		vc.ChromaticAberration = *req.ChromaticAberration
	}
	if req.KlingProForAction != nil {
		vc.KlingProForAction = *req.KlingProForAction
	}
	if err := s.novelRepo.Update(novel); err != nil {
		return nil, err
	}
	return novel, nil
}

// GenerateCoverImage 使用 AI 为小说生成封面图，并将 URL 写回 cover_image 字段
func (s *NovelService) GenerateCoverImage(ctx context.Context, tenantID, novelID uint, suggestion string) (string, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return "", err
	}
	if novel.TenantID != tenantID {
		return "", fmt.Errorf("not found")
	}

	genreMap := map[string]string{
		"fantasy":    "fantasy xianxia cultivation magic world",
		"xianxia":    "xianxia immortal cultivation Chinese fantasy",
		"urban":      "modern Chinese urban city",
		"romance":    "romance love story elegant",
		"historical": "ancient Chinese historical palace dynasty",
		"scifi":      "science fiction futuristic space technology",
		"mystery":    "mystery thriller suspense dark",
		"wuxia":      "Chinese martial arts wuxia sword hero",
		"horror":     "horror supernatural dark eerie",
		"apocalypse": "post-apocalyptic wasteland survival",
		"rebirth":    "time travel rebirth second chance",
	}
	genreDesc := genreMap[novel.Genre]
	if genreDesc == "" {
		genreDesc = novel.Genre
	}

	desc := ""
	if novel.Description != "" && len(novel.Description) <= 150 {
		desc = " Theme: " + novel.Description + "."
	}

	negativePrompt := "text, words, letters, watermark, signature, blurry, low quality, ugly, distorted, nsfw"

	// 判断是否有可用的旧封面作为参考图（图生图模式）
	existingCover := ""
	if suggestion != "" && novel.CoverImage != "" &&
		(strings.HasPrefix(novel.CoverImage, "http://") || strings.HasPrefix(novel.CoverImage, "https://")) {
		existingCover = novel.CoverImage
	}

	var prompt string
	if suggestion != "" && existingCover != "" {
		// 图生图：以旧封面为参考，按用户指令编辑
		prompt = suggestion
	} else if suggestion != "" {
		// 无旧封面，文生图但加入用户建议
		prompt = fmt.Sprintf(
			"Professional book cover illustration for a Chinese novel titled \"%s\". Genre: %s. "+
				"%s Style: cinematic, atmospheric, high-quality digital art, "+
				"dramatic lighting, vibrant colors, detailed, no text, no letters, no watermarks.",
			novel.Title, genreDesc, suggestion,
		)
	} else {
		prompt = fmt.Sprintf(
			"Professional book cover illustration for a Chinese novel titled \"%s\".%s "+
				"Genre: %s. Style: cinematic, atmospheric, high-quality digital art, dramatic lighting, "+
				"vibrant colors, detailed, no text, no letters, no watermarks.",
			novel.Title, desc, genreDesc,
		)
	}

	ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title})
	// existingCover != "" 时走 SeedEditV3（图生图指令编辑），否则走文生图
	imageURL, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, "", prompt, existingCover, novel.ImageStyle, negativePrompt, "")
	if err != nil {
		return "", fmt.Errorf("generate cover image: %w", err)
	}

	novel.CoverImage = imageURL
	if err := s.novelRepo.Update(novel); err != nil {
		return imageURL, fmt.Errorf("persist cover image: %w", err)
	}
	return imageURL, nil
}

// PublishNovel 发布小说到广场（含租户校验）
func (s *NovelService) PublishNovel(id, tenantID uint, visibility string) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if novel.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}
	if visibility == "" {
		visibility = "public"
	}
	now := time.Now()
	if novel.ReviewStatus == "approved" {
		// 已审核通过，直接发布
		novel.IsPublished = true
		novel.PublishedAt = &now
		novel.Visibility = visibility
		if err := s.novelRepo.UpdateFields(id, map[string]interface{}{
			"is_published": true, "published_at": &now, "visibility": visibility,
		}); err != nil {
			return nil, err
		}
	} else {
		// 提交审核，不直接发布
		novel.ReviewStatus = "pending_review"
		novel.Visibility = visibility
		if err := s.novelRepo.UpdateFields(id, map[string]interface{}{
			"review_status": "pending_review", "visibility": visibility,
		}); err != nil {
			return nil, err
		}
	}
	return novel, nil
}

// ReviewNovelRequest 审核小说请求
type ReviewNovelRequest struct {
	Approved   bool   `json:"approved"`
	ReviewNote string `json:"review_note"`
}

// ReviewNovel 审核小说（管理员操作）
func (s *NovelService) ReviewNovel(id, reviewerID, tenantID uint, req ReviewNovelRequest) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if novel.TenantID != tenantID {
		return nil, fmt.Errorf("novel not found")
	}
	if novel.ReviewStatus != "pending_review" {
		return nil, fmt.Errorf("novel is not pending review")
	}
	now := time.Now()
	fields := map[string]interface{}{
		"reviewed_at": &now,
		"reviewed_by": reviewerID,
	}
	if req.Approved {
		novel.ReviewStatus = "approved"
		novel.IsPublished = true
		novel.PublishedAt = &now
		novel.ReviewedAt = &now
		novel.ReviewedBy = reviewerID
		fields["review_status"] = "approved"
		fields["is_published"] = true
		fields["published_at"] = &now
	} else {
		novel.ReviewStatus = "rejected"
		novel.ReviewNote = req.ReviewNote
		novel.ReviewedAt = &now
		novel.ReviewedBy = reviewerID
		fields["review_status"] = "rejected"
		fields["review_note"] = req.ReviewNote
	}
	if err := s.novelRepo.UpdateFields(id, fields); err != nil {
		return nil, err
	}
	return novel, nil
}

// UnpublishNovel 取消发布小说（含租户校验）
func (s *NovelService) UnpublishNovel(id, tenantID uint) error {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("not found")
	}
	if novel.TenantID != tenantID {
		return fmt.Errorf("not found")
	}
	return s.novelRepo.UpdateFields(id, map[string]interface{}{
		"is_published": false, "visibility": "private",
	})
}

// GenerateOutlineRequest 生成大纲请求
type GenerateOutlineRequest struct {
	NovelID        uint     `json:"novel_id" binding:"required"`
	Prompt         string   `json:"prompt"`
	ChapterNum     int      `json:"chapter_num"` // 0 = AI 自决章节数
	Keywords       []string `json:"keywords"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Temperature    float64  `json:"temperature,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// GenerateOutline 生成大纲
func (s *NovelService) GenerateOutline(tenantID uint, req *GenerateOutlineRequest) (*OutlineResult, error) {
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, err
	}
	if novel.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}

	// 构建提示词
	prompt := s.buildOutlinePrompt(novel, req)

	// 构建 AI 参数覆盖：优先请求参数，其次项目配置
	outlineOverrides := StoryboardOverrides{
		MaxTokens:      req.MaxTokens,
		Temperature:    req.Temperature,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	if outlineOverrides.MaxTokens == 0 {
		outlineOverrides.MaxTokens = novel.MaxTokens
	}
	if outlineOverrides.Temperature == 0 {
		outlineOverrides.Temperature = novel.Temperature
	}
	if outlineOverrides.TimeoutSeconds == 0 {
		outlineOverrides.TimeoutSeconds = novel.TimeoutSeconds
	}

	// 调用AI生成（使用租户提供商）
	result, err := s.aiService.GenerateWithProvider(tenantID, req.NovelID, "outline", prompt, "", outlineOverrides)
	if err != nil {
		return nil, err
	}

	// 解析结果：兼容两种 AI 输出格式
	//   格式 A（预期）：{"title":"...","chapters":[...]}  → 用 extractJSONObject 保留完整对象
	//   格式 B（AI 偶发）：[{"chapter_no":1,...},...]      → 直接输出数组，用 extractJSON 作降级
	//   格式 C（token 截断）：上述两种格式但 JSON 不完整 → repairTruncatedJSON 修复后重试
	tryParse := func(s string) (*OutlineResult, error) {
		out := &OutlineResult{}
		if e := json.Unmarshal([]byte(s), out); e == nil && len(out.Chapters) > 0 {
			return out, nil
		}
		arr := extractJSON(s)
		var chapters []ChapterOutline
		if e2 := json.Unmarshal([]byte(arr), &chapters); e2 == nil && len(chapters) > 0 {
			return &OutlineResult{Title: novel.Title, Chapters: chapters}, nil
		}
		return nil, fmt.Errorf("cannot parse outline JSON")
	}

	outline := &OutlineResult{}
	cleaned := extractJSONObject(result)
	if err := json.Unmarshal([]byte(cleaned), outline); err != nil || len(outline.Chapters) == 0 {
		// 降级：尝试格式 B（纯 chapters 数组）
		cleanedArr := extractJSON(result)
		var chapters []ChapterOutline
		if err2 := json.Unmarshal([]byte(cleanedArr), &chapters); err2 == nil && len(chapters) > 0 {
			logger.Printf("GenerateOutline: novel %d returned array format, wrapping into OutlineResult", req.NovelID)
			outline = &OutlineResult{
				Title:    novel.Title,
				Chapters: chapters,
			}
		} else {
			// 格式 C-1：修复缺失冒号（DeepSeek 偶发 "key" value 格式）
			fixedObj := repairMissingColons(cleaned)
			if out, fixErr := tryParse(fixedObj); fixErr == nil {
				logger.Printf("GenerateOutline: novel %d repaired missing colons → %d chapters", req.NovelID, len(out.Chapters))
				outline = out
			} else if out, fixErr2 := tryParse(repairMissingColons(cleanedArr)); fixErr2 == nil {
				logger.Printf("GenerateOutline: novel %d repaired missing colons (array) → %d chapters", req.NovelID, len(out.Chapters))
				outline = out
			} else {
				// 格式 C-2：修复截断（stopReason=length 时常见）
				repaired := repairTruncatedJSON(fixedObj)
				if out, repErr := tryParse(repaired); repErr == nil {
					logger.Printf("GenerateOutline: novel %d JSON truncated+colons repaired → %d chapters from %d chars",
						req.NovelID, len(out.Chapters), len(cleaned))
					outline = out
				} else {
					repairedArr := repairTruncatedJSON(repairMissingColons(cleanedArr))
					if out, repErr2 := tryParse(repairedArr); repErr2 == nil {
						logger.Printf("GenerateOutline: novel %d array JSON truncated+colons repaired → %d chapters",
							req.NovelID, len(out.Chapters))
						outline = out
					} else {
						parseErr := err
						if parseErr == nil {
							parseErr = fmt.Errorf("chapters array empty after parse")
						}
						preview := cleaned
						if len(preview) > 500 {
							preview = preview[:500]
						}
						logger.Printf("GenerateOutline: failed to parse AI response for novel %d: %v (object len=%d, array len=%d, preview=%q)",
							req.NovelID, parseErr, len(cleaned), len(cleanedArr), preview)
						return nil, fmt.Errorf("outline parse failed: %w", parseErr)
					}
				}
			}
		}
	}

	// 大纲版本快照：在用新大纲覆盖之前，将当前大纲存为历史版本
	if novel.Outline != "" && s.outlineVersionRepo != nil {
		if maxV, vErr := s.outlineVersionRepo.MaxVersion(novel.ID); vErr == nil {
			_ = s.outlineVersionRepo.Create(&model.NovelOutlineVersion{
				TenantID: novel.TenantID,
				NovelID:  novel.ID,
				Version:  maxV + 1,
				Outline:  novel.Outline,
				Prompt:   req.Prompt,
			})
		}
	}

	// 将新生成的大纲 JSON 持久化回 novel.outline
	if outlineJSON, marshalErr := json.Marshal(outline); marshalErr == nil {
		novel.Outline = string(outlineJSON)
		_ = s.novelRepo.UpdateFields(novel.ID, map[string]interface{}{"outline": novel.Outline})
	}

	// 大纲生成成功后，自动创建占位章节（跳过已存在的）
	for _, chap := range outline.Chapters {
		if _, err := s.chapterRepo.GetByNovelAndChapterNo(novel.ID, chap.ChapterNo); err == nil {
			continue // 已存在，跳过
		}
		placeholder := &model.Chapter{
			UUID:          uuid.New().String(),
			NovelID:       novel.ID,
			TenantID:      novel.TenantID,
			ChapterNo:     chap.ChapterNo,
			Title:         chap.Title,
			Summary:       chap.Summary,
			TensionLevel:  chap.TensionLevel,
			ActNo:         chap.Act,
			EmotionalTone: chap.EmotionalTone,
			HookType:      chap.HookType,
			Status:        "draft",
		}
		if err := s.chapterRepo.Create(placeholder); err != nil {
			logger.Printf("GenerateOutline: create placeholder chapter %d: %v", chap.ChapterNo, err)
		}
	}

	return outline, nil
}

// ListOutlineVersions 列出小说大纲历史版本（供前端查看/回滚）
func (s *NovelService) ListOutlineVersions(novelID uint) ([]*model.NovelOutlineVersion, error) {
	if s.outlineVersionRepo == nil {
		return []*model.NovelOutlineVersion{}, nil
	}
	return s.outlineVersionRepo.ListByNovel(novelID)
}

// repairTruncatedJSON 尝试修复因 token 截断而不完整的 JSON 字符串。
// 策略：找到最后一个完整的 '}'，在此截断，然后补全所有未关闭的 '[' 和 '{' 。
// 适用于大纲/数组等大 JSON 因 maxTokens 限制被截断的场景。
func repairTruncatedJSON(s string) string {
	// 找最后一个 '}'
	last := strings.LastIndex(s, "}")
	if last < 0 {
		return s
	}
	s = s[:last+1]

	// 逐字符统计未关闭的括号层数（跳过字符串内容）
	var arrDepth, objDepth int
	inStr, escape := false, false
	for _, ch := range s {
		if escape {
			escape = false
			continue
		}
		if ch == '\\' && inStr {
			escape = true
			continue
		}
		if ch == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch ch {
		case '[':
			arrDepth++
		case ']':
			if arrDepth > 0 {
				arrDepth--
			}
		case '{':
			objDepth++
		case '}':
			if objDepth > 0 {
				objDepth--
			}
		}
	}

	var buf strings.Builder
	buf.WriteString(s)
	for i := 0; i < arrDepth; i++ {
		buf.WriteByte(']')
	}
	for i := 0; i < objDepth; i++ {
		buf.WriteByte('}')
	}
	return buf.String()
}

// OutlineStructure 三幕结构信息
type OutlineStructure struct {
	Act1EndChapter   int `json:"act1_end_chapter"`
	Act2StartChapter int `json:"act2_start_chapter"`
	ClimaxChapter    int `json:"climax_chapter"`
	Act3StartChapter int `json:"act3_start_chapter"`
}

// ForeshadowMapItem 伏笔映射条目
type ForeshadowMapItem struct {
	ID            int    `json:"id"`
	Type          string `json:"type"`
	Description   string `json:"description"`
	PlantChapter  int    `json:"plant_chapter"`
	PayoffChapter int    `json:"payoff_chapter"`
}

// OutlineResult 大纲结果
type OutlineResult struct {
	Title         string              `json:"title"`
	Genre         string              `json:"genre,omitempty"`
	Theme         string              `json:"theme,omitempty"`
	Summary       string              `json:"summary,omitempty"`
	Structure     *OutlineStructure   `json:"structure,omitempty"`
	ForeshadowMap []ForeshadowMapItem `json:"foreshadow_map,omitempty"`
	Chapters      []ChapterOutline    `json:"chapters"`
}

// ChapterOutline 章节大纲
type ChapterOutline struct {
	ChapterNo     int      `json:"chapter_no"`
	Title         string   `json:"title"`
	Summary       string   `json:"summary"`
	WordCount     int      `json:"word_count"`
	PlotPoints    []string `json:"plot_points"`
	EmotionalTone string   `json:"emotional_tone,omitempty"`
	TensionLevel  int      `json:"tension_level,omitempty"`
	Hook          string   `json:"hook,omitempty"`
	HookType      string   `json:"hook_type,omitempty"`
	ConflictType  string   `json:"conflict_type,omitempty"`
	Act           int      `json:"act,omitempty"`
}

// buildOutlinePrompt 构建大纲提示词
func (s *NovelService) buildOutlinePrompt(novel *model.Novel, req *GenerateOutlineRequest) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("请为小说《%s》生成一个详细的大纲。\n\n", novel.Title))

	if novel.Description != "" {
		sb.WriteString(fmt.Sprintf("故事简介：%s\n\n", novel.Description))
	}

	if len(req.Keywords) > 0 {
		keywords := req.Keywords
		if len(keywords) > 10 {
			logger.Printf("[NovelService] buildOutlinePrompt: truncating keywords from %d to 10", len(keywords))
			keywords = keywords[:10]
		}
		sb.WriteString(fmt.Sprintf("关键词：%s\n\n", strings.Join(keywords, ", ")))
	}

	if req.Prompt != "" {
		sb.WriteString(fmt.Sprintf("创作要求：%s\n\n", req.Prompt))
	}

	if req.ChapterNum > 0 {
		sb.WriteString(fmt.Sprintf("请生成%d章的大纲，每章包括：标题、详细剧情概述（不少于150字）、预计字数（2000-3000字）、主要剧情点。\n", req.ChapterNum))
	} else {
		sb.WriteString("请根据故事规模自行决定合适的章节数（通常30-200章之间），每章包括：标题、详细剧情概述（不少于150字）、预计字数（2000-3000字）、主要剧情点。\n")
	}

	// 注入未解决剧情点（引导大纲在后续章节中推进解决）
	if s.plotPointService != nil {
		pps, _ := s.plotPointService.ListByNovel(novel.ID, "", true)
		if len(pps) > 0 {
			sb.WriteString("\n【未解决的剧情线（大纲需在后续章节中推进解决）】\n")
			max := 8
			if len(pps) < max {
				max = len(pps)
			}
			for i := 0; i < max; i++ {
				sb.WriteString(fmt.Sprintf("- [%s] %s\n", pps[i].Type, pps[i].Description))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(`
## 输出格式（严格遵守）
仅输出如下 JSON 对象，禁止任何说明文字、markdown 代码块、注释或 schema 以外的额外字段：
{
  "title": "小说标题",
  "chapters": [
    {
      "chapter_no": 1,
      "title": "章节标题",
      "summary": "章节剧情详细概述，不少于150字。必须涵盖：①场景与开场氛围（人物在哪、在做什么）②本章核心事件的起因、经过、结果③主角的关键决策或行动及其动机④与其他角色的重要互动或冲突⑤本章结尾的情绪落点与对下一章的引导。禁止用空泛语言敷衍，每章概述须有实质内容。",
      "word_count": 2500,
      "plot_points": ["剧情点1（含人物+动作+结果，20字内）", "剧情点2", "剧情点3"],
      "emotional_tone": "紧张",
      "tension_level": 7,
      "hook": "章末悬念钩子（具体描述悬念内容，20字内）",
      "hook_type": "cliffhanger",
      "conflict_type": "人与人",
      "act": 1
    }
  ]
}
字段类型说明：chapter_no/word_count/tension_level/act 必须是整数，其余为字符串或字符串数组。
最外层必须是 {} 对象，chapters 是其中的数组字段，禁止直接返回 [] 数组。
重要：每章 summary 字段不得少于150字，这是硬性要求，AI 不得缩减。`)


	return sb.String()
}


// writeCharacterSnapshots 从章节内容中提取角色状态并写入快照
func (s *NovelService) writeCharacterSnapshots(tenantID uint, chapter *model.Chapter) {
	if s.characterRepo == nil || s.snapshotRepo == nil {
		return
	}
	characters, err := s.characterRepo.ListByNovel(chapter.NovelID)
	if err != nil || len(characters) == 0 {
		return
	}

	// 构建角色列表字符串
	charNames := make([]string, 0, len(characters))
	for _, c := range characters {
		charNames = append(charNames, c.Name)
	}

	// 取章末 3000 字（rune-safe），章末比章头更能反映角色的当前状态
	runes := []rune(chapter.Content)
	start := 0
	if len(runes) > 3000 {
		start = len(runes) - 3000
	}
	contentPreview := string(runes[start:])

	prompt := fmt.Sprintf("从以下章节内容中提取主要角色的当前状态，以JSON格式返回：\n角色列表：%s\n章节内容（章末节选）：\n%s\n\n【严格要求】\n- 必须返回且只返回一个 JSON 对象，根键为 \"characters\"，值为数组\n- 禁止直接返回裸数组（即禁止以 [ 开头）\n- 禁止在 JSON 前后添加任何说明文字或代码块标记\n- 只包含章节中实际出现的角色\n\n正确格式示例：\n{\"characters\":[{\"name\":\"角色名\",\"mood\":\"情绪状态\",\"location\":\"当前位置\",\"motivation\":\"当前动机\",\"power_level\":5}]}",
		strings.Join(charNames, "、"), contentPreview)

	result, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "character_state", prompt, "")
	if err != nil {
		logger.Printf("writeCharacterSnapshots: AI extraction failed for chapter %d: %v", chapter.ID, err)
		return
	}

	cleaned := extractJSON(result)
	// Normalise: AI sometimes returns a bare array instead of {"characters":[...]}
	if strings.HasPrefix(strings.TrimSpace(cleaned), "[") {
		cleaned = `{"characters":` + cleaned + `}`
	}
	var extraction struct {
		Characters []struct {
			Name       string `json:"name"`
			Mood       string `json:"mood"`
			Location   string `json:"location"`
			Motivation string `json:"motivation"`
			PowerLevel int    `json:"power_level"`
		} `json:"characters"`
	}

	if err := json.Unmarshal([]byte(cleaned), &extraction); err != nil {
		logger.Printf("writeCharacterSnapshots: parse failed: %v", err)
		return
	}

	// 建立名称到ID的映射
	nameToChar := make(map[string]*model.Character)
	for _, c := range characters {
		nameToChar[c.Name] = c
	}

	for _, state := range extraction.Characters {
		char, ok := nameToChar[state.Name]
		if !ok {
			continue
		}
		snapshot := &model.CharacterStateSnapshot{
			CharacterID:  char.ID,
			ChapterID:    chapter.ID,
			Mood:         state.Mood,
			Location:     state.Location,
			Motivation:   state.Motivation,
			PowerLevel:   state.PowerLevel,
			SnapshotTime: chapter.CreatedAt,
		}
		if err := s.snapshotRepo.Create(snapshot); err != nil {
			logger.Printf("writeCharacterSnapshots: create snapshot failed for char %d: %v", char.ID, err)
		}
	}
}

// SyncCharacterSnapshots 为章节同步角色状态快照
// characterIDs: 要处理的角色 ID 列表（空表示全部角色）
// reusePrevious: true=复用上章快照, false=基于本章内容 AI 重新生成
func (s *NovelService) SyncCharacterSnapshots(
	tenantID uint,
	chapter *model.Chapter,
	characterIDs []uint,
	reusePrevious bool,
) error {
	if s.characterRepo == nil || s.snapshotRepo == nil {
		return fmt.Errorf("character repos not wired")
	}

	// 获取目标角色列表
	var chars []*model.Character
	var err error
	if len(characterIDs) == 0 {
		chars, err = s.characterRepo.ListByNovel(chapter.NovelID)
	} else {
		all, e := s.characterRepo.ListByNovel(chapter.NovelID)
		if e != nil {
			return fmt.Errorf("list characters: %w", e)
		}
		idSet := make(map[uint]bool, len(characterIDs))
		for _, id := range characterIDs {
			idSet[id] = true
		}
		for _, c := range all {
			if idSet[c.ID] {
				chars = append(chars, c)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("list characters: %w", err)
	}
	if len(chars) == 0 {
		return nil
	}

	// 查找上一章节记录（chapter_no - 1）
	var prevChapter *model.Chapter
	if chapter.ChapterNo > 1 {
		prevChapter, _ = s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, chapter.ChapterNo-1)
	}

	// 批量预取上章所有角色快照，构建 characterID → snapshot 的 map，避免 N+1
	prevSnapMap := make(map[uint]*model.CharacterStateSnapshot)
	if prevChapter != nil {
		if prevSnaps, err2 := s.snapshotRepo.ListByChapterID(prevChapter.ID); err2 == nil {
			for _, ps := range prevSnaps {
				prevSnapMap[ps.CharacterID] = ps
			}
		}
	}

	if reusePrevious {
		// 复用上章快照：复制到本章
		for _, char := range chars {
			prevSnap := prevSnapMap[char.ID]
			if prevSnap == nil {
				// 没有上章快照就跳过
				continue
			}
			snap := &model.CharacterStateSnapshot{
				CharacterID:    char.ID,
				ChapterID:      chapter.ID,
				Age:            prevSnap.Age,
				Height:         prevSnap.Height,
				Weight:         prevSnap.Weight,
				Health:         prevSnap.Health,
				Injuries:       prevSnap.Injuries,
				PowerLevel:     prevSnap.PowerLevel,
				Abilities:      prevSnap.Abilities,
				Equipment:      prevSnap.Equipment,
				Mood:           prevSnap.Mood,
				Motivation:     prevSnap.Motivation,
				Goals:          prevSnap.Goals,
				Fears:          prevSnap.Fears,
				Location:       prevSnap.Location,
				KnownLocations: prevSnap.KnownLocations,
				Relations:      prevSnap.Relations,
				SnapshotTime:   chapter.CreatedAt,
			}
			if e := s.snapshotRepo.Create(snap); e != nil {
				logger.Printf("SyncCharacterSnapshots: copy snapshot char %d: %v", char.ID, e)
			}
		}
		return nil
	}

	// 重新生成：结合上章快照 + 本章内容，调用 AI
	contentPreview := chapter.Content
	if runes := []rune(contentPreview); len(runes) > 3000 {
		contentPreview = string(runes[:3000]) + "..."
	}

	for _, char := range chars {
		// 构建上章角色状态上下文（直接从预取 map 查找，无额外 DB 查询）
		var prevCtx string
		if ps := prevSnapMap[char.ID]; ps != nil {
			prevCtx = fmt.Sprintf(
				"上章末状态：情绪=%s, 位置=%s, 动机=%s, 战力=%d, 健康=%s",
				ps.Mood, ps.Location, ps.Motivation, ps.PowerLevel, ps.Health,
			)
		}
		if prevCtx == "" {
			if ls, _ := s.snapshotRepo.GetLatestForCharacter(char.ID); ls != nil {
				prevCtx = fmt.Sprintf(
					"最近状态：情绪=%s, 位置=%s, 动机=%s, 战力=%d, 健康=%s",
					ls.Mood, ls.Location, ls.Motivation, ls.PowerLevel, ls.Health,
				)
			}
		}
		if prevCtx == "" {
			prevCtx = fmt.Sprintf("角色信息：%s", char.Description)
		}

		prompt := fmt.Sprintf(
			`根据角色「%s」的背景信息和本章内容，提取该角色在本章末尾的状态，以JSON格式返回。

角色信息：
%s

本章内容（节选）：
%s

请返回以下JSON格式：
{"mood":"情绪状态","location":"当前位置","motivation":"当前动机","power_level":5,"health":"healthy|injured|critical","abilities":"能力描述（若有变化）"}`,
			char.Name, prevCtx, contentPreview,
		)

		result, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "character_state", prompt, "")
		if err != nil {
			logger.Printf("SyncCharacterSnapshots: AI failed for char %d: %v", char.ID, err)
			continue
		}

		var state struct {
			Mood       string `json:"mood"`
			Location   string `json:"location"`
			Motivation string `json:"motivation"`
			PowerLevel int    `json:"power_level"`
			Health     string `json:"health"`
			Abilities  string `json:"abilities"`
		}
		cleaned := extractJSON(strings.TrimSpace(result))
		if e := json.Unmarshal([]byte(cleaned), &state); e != nil {
			logger.Printf("SyncCharacterSnapshots: parse failed char %d: %v", char.ID, e)
			continue
		}

		// 沿用上章快照中的静态字段（身高体重等，直接从预取 map 查找）
		var baseAbilities, baseEquipment string
		var age, height, weight float64
		if ps := prevSnapMap[char.ID]; ps != nil {
			age, height, weight = ps.Age, ps.Height, ps.Weight
			baseEquipment = ps.Equipment
		}
		abilities := state.Abilities
		if abilities == "" {
			abilities = baseAbilities
		}
		health := state.Health
		if health == "" {
			health = "healthy"
		}

		snap := &model.CharacterStateSnapshot{
			CharacterID:  char.ID,
			ChapterID:    chapter.ID,
			Age:          age,
			Height:       height,
			Weight:       weight,
			Health:       health,
			PowerLevel:   state.PowerLevel,
			Abilities:    abilities,
			Equipment:    baseEquipment,
			Mood:         state.Mood,
			Motivation:   state.Motivation,
			Location:     state.Location,
			SnapshotTime: chapter.CreatedAt,
		}
		if e := s.snapshotRepo.Create(snap); e != nil {
			logger.Printf("SyncCharacterSnapshots: create snapshot char %d: %v", char.ID, e)
		}
	}
	return nil
}

// updateNovelStats 更新小说统计（使用 DB 聚合避免并发竞态）
func (s *NovelService) updateNovelStats(novelID uint) {
	if err := s.novelRepo.SyncStats(novelID); err != nil {
		logger.Printf("updateNovelStats: sync novel %d: %v", novelID, err)
	}
}

// extractPlotPoints 提取剧情点并保存到数据库
func (s *NovelService) extractPlotPoints(chapter *model.Chapter) {
	if s.plotPointService == nil {
		return
	}
	if _, err := s.plotPointService.ExtractFromChapter(0, chapter); err != nil {
		logger.Printf("extractPlotPoints chapter %d: %v", chapter.ID, err)
	}
}

// providerCacheEntry 提供商缓存条目
type providerCacheEntry struct {
	provider  ai.AIProvider
	expiresAt time.Time
}

// TaskRouting specifies which provider to prefer for each task type.
// Provider names match registered names: "openai", "anthropic", "doubao", etc.
// Empty string means use the system default or DB-configured provider.
type TaskRouting struct {
	ChapterGen   string
	QualityCheck string
	TTS          string
	ImageGen     string
	VideoGen     string
	Embedding    string
}

// AIService AI服务

// ─── 小说广场 ─────────────────────────────────────────────────────────────────

// WithNovelSocial 注入广场社交仓库
func (s *NovelService) WithNovelSocial(likeRepo *repository.NovelLikeRepository, commentRepo *repository.NovelCommentRepository) *NovelService {
	s.novelLikeRepo = likeRepo
	s.novelCommentRepo = commentRepo
	go s.cleanupViewDedup()
	return s
}

// cleanupViewDedup 每小时扫描并清除已过期的去重条目，防止 sync.Map 无限增长。
func (s *NovelService) cleanupViewDedup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			s.novelViewDedup.Range(func(k, v any) bool {
				if expiry, ok := v.(time.Time); ok && now.After(expiry) {
					s.novelViewDedup.Delete(k)
				}
				return true
			})
		case <-s.stopCh:
			return
		}
	}
}

// GetPublicNovel 获取公开小说详情（无需 tenantID）
func (s *NovelService) GetPublicNovel(id uint) (*model.Novel, error) {
	return s.novelRepo.GetPublicByID(id)
}

// ListPublicNovels 列出公开小说（支持精细筛选）
func (s *NovelService) ListPublicNovels(f repository.NovelPublicFilter) ([]*model.Novel, int64, error) {
	if f.Sort == "" {
		f.Sort = "hot"
	}
	return s.novelRepo.ListPublicSorted(f)
}

// GetNovelRanking 获取公开小说排行榜
func (s *NovelService) GetNovelRanking(rankType, gender string) ([]*model.Novel, error) {
	return s.novelRepo.GetPublicRanking(rankType, gender, 30)
}

// RecordNovelViewDeduped 防刷浏览量（同 IP 对同一小说 1 小时内只计一次）
func (s *NovelService) RecordNovelViewDeduped(id uint, clientIP string) error {
	key := fmt.Sprintf("novel:%s:%d", clientIP, id)
	expiry := time.Now().Add(time.Hour)
	// Use Swap to atomically replace and check the old value in one operation.
	old, _ := s.novelViewDedup.Swap(key, expiry)
	if old != nil {
		// Key already existed: skip if not yet expired.
		if t, ok := old.(time.Time); ok && time.Now().Before(t) {
			// Restore the original expiry so we don't accidentally shorten it.
			s.novelViewDedup.Store(key, t)
			return nil
		}
	}
	return s.novelRepo.IncrNovelViewCount(id)
}

// ToggleNovelLike 点赞/取消，返回最终状态
func (s *NovelService) ToggleNovelLike(novelID, userID uint) (bool, error) {
	if s.novelLikeRepo == nil {
		return false, fmt.Errorf("like feature not available")
	}
	liked, err := s.novelLikeRepo.Toggle(novelID, userID)
	if err != nil {
		return false, err
	}
	delta := 1
	if !liked {
		delta = -1
	}
	if err := s.novelRepo.IncrNovelLikeCount(novelID, delta); err != nil {
		logger.Printf("ToggleNovelLike: IncrNovelLikeCount novel=%d delta=%d: %v", novelID, delta, err)
	}
	return liked, nil
}

// IsNovelLiked 检查用户是否已点赞
func (s *NovelService) IsNovelLiked(novelID, userID uint) bool {
	if s.novelLikeRepo == nil {
		return false
	}
	exists, _ := s.novelLikeRepo.Exists(novelID, userID)
	return exists
}

// ListNovelComments 获取评论列表
func (s *NovelService) ListNovelComments(novelID uint, page, size int) ([]*model.NovelComment, int64, error) {
	if s.novelCommentRepo == nil {
		return []*model.NovelComment{}, 0, nil
	}
	return s.novelCommentRepo.ListByNovel(novelID, page, size)
}

// AddNovelComment 发表评论（最多支持1级回复，不允许无限嵌套）
func (s *NovelService) AddNovelComment(novelID, userID uint, nickname, content string, parentID *uint) (*model.NovelComment, error) {
	if s.novelCommentRepo == nil {
		return nil, fmt.Errorf("comment feature not available")
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("comment content cannot be empty")
	}
	if parentID != nil {
		parent, err := s.novelCommentRepo.GetByID(*parentID)
		if err != nil {
			return nil, fmt.Errorf("parent comment not found")
		}
		if parent.ParentID != nil {
			return nil, fmt.Errorf("cannot reply to a reply: only one level of nesting is allowed")
		}
	}
	c := &model.NovelComment{
		NovelID:  novelID,
		UserID:   userID,
		Nickname: nickname,
		Content:  content,
		ParentID: parentID,
	}
	if err := s.novelCommentRepo.Create(c); err != nil {
		return nil, err
	}
	if err := s.novelRepo.IncrNovelCommentCount(novelID, 1); err != nil {
		logger.Printf("AddNovelComment: IncrNovelCommentCount novel=%d: %v", novelID, err)
	}
	return c, nil
}

// DeleteNovelComment 删除评论及其子回复（仅作者本人）
func (s *NovelService) DeleteNovelComment(commentID, userID uint) error {
	if s.novelCommentRepo == nil {
		return fmt.Errorf("comment feature not available")
	}
	c, err := s.novelCommentRepo.GetByID(commentID)
	if err != nil {
		return err
	}
	if c.UserID != userID {
		return ErrPermissionDenied
	}
	deleted, err := s.novelCommentRepo.DeleteWithReplies(commentID)
	if err != nil {
		return err
	}
	_ = s.novelRepo.IncrNovelCommentCount(c.NovelID, -int(deleted))
	return nil
}

// RecalcNovelHotScores 批量重算热度分（由后台定时任务调用）
// hot_score = (view×0.5 + like×0.3 + comment×0.2) × (1 / (1 + ageDays×0.1))
func (s *NovelService) RecalcNovelHotScores() error {
	novels, err := s.novelRepo.ListPublicNovelsForHotCalc()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, n := range novels {
		ageDays := 0.0
		if n.PublishedAt != nil {
			ageDays = now.Sub(*n.PublishedAt).Hours() / 24
		}
		base := float64(n.ViewCount)*0.5 + float64(n.LikeCount)*0.3 + float64(n.CommentCount)*0.2
		score := base / (1 + ageDays*0.1)
		if err := s.novelRepo.UpdateNovelHotScore(n.ID, score); err != nil {
			logger.Printf("RecalcNovelHotScores: failed to update novel %d: %v", n.ID, err)
		}
	}
	return nil
}
