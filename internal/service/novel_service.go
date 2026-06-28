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
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/redis/go-redis/v9"
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
	memberRepo          *repository.NovelMemberRepository // 可选，用于协作成员访问校验
	// 广场社交
	novelLikeRepo    *repository.NovelLikeRepository
	novelCommentRepo *repository.NovelCommentRepository
	novelViewDedup   sync.Map     // fallback in-process dedup when Redis unavailable
	cache            *redis.Client // optional: cross-instance view dedup
	stopCh           chan struct{} // closed by Shutdown() to stop background goroutines
	onDeleteHook     func(novelID uint) // fired after a novel is deleted
}

// OnDeleteNovel registers a callback fired after a novel is successfully deleted.
func (s *NovelService) OnDeleteNovel(fn func(novelID uint)) {
	s.onDeleteHook = fn
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

// WithRedis enables cross-instance novel view deduplication via Redis SETNX.
func (s *NovelService) WithRedis(c *redis.Client) *NovelService {
	s.cache = c
	return s
}

// WithMemberRepo 注入协作成员仓库（用于跨租户访问校验）
func (s *NovelService) WithMemberRepo(repo *repository.NovelMemberRepository) *NovelService {
	s.memberRepo = repo
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
	ChapterMode     string `json:"chapter_mode"`
	TenantID        uint
	UserID          uint
}

// Create 创建小说
func (s *NovelService) Create(req *CreateNovelRequest) (*model.Novel, error) {
	tenantID := req.TenantID
	if tenantID == 0 {
		return nil, fmt.Errorf("tenant_id is required")
	}
	chapterMode := req.ChapterMode
	if chapterMode == "" {
		chapterMode = "sequential"
	}
	novel := &model.Novel{
		UUID:        uuid.New().String(),
		TenantID:    tenantID,
		CreatedBy:   req.UserID,
		Title:       req.Title,
		Status:      "planning",
		WorldviewID: req.WorldviewID,
		Meta: model.NovelMeta{
			Description:     req.Description,
			Genre:           req.Genre,
			CoverImage:      req.CoverImage,
			Channel:         req.Channel,
			TargetWordCount: req.TargetWordCount,
			TargetChapters:  req.TargetChapters,
		},
		AIConfig: model.NovelAIConfig{
			ChapterMode: chapterMode,
		},
	}

	if err := s.novelRepo.Create(novel); err != nil {
		metrics.NovelCreationTotal.WithLabelValues("error").Inc()
		return nil, err
	}

	metrics.NovelCreationTotal.WithLabelValues("success").Inc()
	return novel, nil
}

// GetNovel 获取小说（含租户校验）
// GetNovel 获取小说，验证所有权或协作成员资格。
// userIDs 可选：若提供且不为 0，当 tenant_id 不匹配时回退检查 ink_novel_member 表。
func (s *NovelService) GetNovel(id, tenantID uint, userIDs ...uint) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if novel.TenantID == tenantID {
		return novel, nil
	}
	// 回退：检查是否为协作成员
	if len(userIDs) > 0 && userIDs[0] > 0 && s.memberRepo != nil {
		m, err := s.memberRepo.GetByNovelAndUser(id, userIDs[0])
		if err == nil && m.Status == "active" {
			return novel, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

// GetRoleForUser 返回用户对指定小说的角色。
// 同租户用户 → "owner"；跨租户协作成员 → 成员角色；无权限 → ""。
func (s *NovelService) GetRoleForUser(novelID, tenantID, userID uint) string {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return ""
	}
	// 同租户 或 是创建者 → owner
	if novel.TenantID == tenantID || (userID > 0 && novel.CreatedBy == userID) {
		return "owner"
	}
	if s.memberRepo != nil && userID > 0 {
		m, err := s.memberRepo.GetByNovelAndUser(novelID, userID)
		if err == nil && m.Status == "active" {
			return m.Role
		}
	}
	return ""
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
	// 清理视图去重缓存（进程内），避免已删除小说的去重记录残留。
	suffix := fmt.Sprintf(":%d", id)
	s.novelViewDedup.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && strings.HasSuffix(key, suffix) {
			s.novelViewDedup.Delete(k)
		}
		return true
	})
	if err := s.novelRepo.DeleteWithCascade(id); err != nil {
		return err
	}
	if s.onDeleteHook != nil {
		s.onDeleteHook(id)
	}
	return nil
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
		ChapterMode:     req.ChapterMode,
		TenantID:        req.TenantID,
		UserID:          req.UserID,
	})
}

// UpdateNovel handler-compatible wrapper（含租户校验）
//
// Uses atomic UpdateFields for all ink_novel columns so concurrent partial
// updates (e.g. worldview binding racing against a settings save) cannot
// overwrite each other's changes via the classic read-modify-write pattern.
func (s *NovelService) UpdateNovel(id, tenantID uint, req *model.UpdateNovelRequest) (*model.Novel, error) {
	// Always read from DB (skip Redis) for the auth check so we don't trust a
	// stale cache for ownership validation.
	novel, err := s.novelRepo.GetByIDFromDB(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if novel.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}

	// ── Step 1: build the set of ink_novel columns to update atomically ──────
	fields := map[string]interface{}{}
	if req.Title != ""        { fields["title"] = req.Title }
	if req.Description != ""  { fields["description"] = req.Description }
	if req.Genre != ""        { fields["genre"] = req.Genre }
	if req.Status != ""       { fields["status"] = req.Status }
	if req.WorldviewID != nil {
		if *req.WorldviewID == 0 {
			fields["worldview_id"] = nil
		} else {
			fields["worldview_id"] = *req.WorldviewID
		}
	}
	if req.CoverImage != ""     { fields["cover_image"] = req.CoverImage }
	if req.AIModel != ""        { fields["ai_model"] = req.AIModel }
	if req.Temperature != nil   { fields["temperature"] = *req.Temperature }
	if req.TopP != nil          { fields["top_p"] = *req.TopP }
	if req.MaxTokens != nil     { fields["max_tokens"] = *req.MaxTokens }
	if req.StylePrompt != ""    { fields["style_prompt"] = req.StylePrompt }
	if req.ImageStyle != ""     { fields["image_style"] = req.ImageStyle }
	if req.PromptLanguage != "" { fields["prompt_language"] = req.PromptLanguage }
	if req.ChapterMode != ""    { fields["chapter_mode"] = req.ChapterMode }
	if req.CoreTheme != ""      { fields["core_theme"] = req.CoreTheme }
	if req.AutoReviewRounds != nil {
		rounds := *req.AutoReviewRounds
		if rounds < 0 { rounds = 0 }
		if rounds > 5 { rounds = 5 }
		fields["auto_review_rounds"] = rounds
	}
	if req.AutoReviewMinScore != nil {
		score := *req.AutoReviewMinScore
		if score < 0 { score = 0 }
		if score > 100 { score = 100 }
		fields["auto_review_min_score"] = score
	}
	if req.TargetWordCount != nil { fields["target_word_count"] = *req.TargetWordCount }
	if req.TargetChapters != nil  { fields["target_chapters"] = *req.TargetChapters }
	if req.TimeoutSeconds != nil  { fields["timeout_seconds"] = *req.TimeoutSeconds }

	if len(fields) > 0 {
		if err := s.novelRepo.UpdateFields(id, fields); err != nil {
			return nil, err
		}
	}

	// ── Step 2: VideoConfig lives in a separate table; read-modify-write is
	// acceptable here because video settings races are non-critical. ──────────
	hasVideoFields := req.VideoType != "" || req.VideoResolution != "" || req.VideoFPS != nil ||
		req.VideoAspectRatio != "" || req.CharConsistencyWeight != nil ||
		req.NarrationVoice != "" || req.SubtitleEnabled != nil || req.SubtitlePosition != "" ||
		req.SubtitleFontSize != nil || req.SubtitleColor != "" || req.SubtitleBgStyle != "" ||
		req.SubtitleFont != "" || req.ColorGrade != "" || req.ContrastLevel != nil ||
		req.Saturation != nil || req.FilmGrain != nil || req.Vignette != nil ||
		req.ChromaticAberration != nil || req.KlingProForAction != nil
	if hasVideoFields {
		fresh, err := s.novelRepo.GetByIDFromDB(id)
		if err != nil {
			return nil, err
		}
		vc := fresh.EnsureVideoConfig()
		vc.NovelID = id
		if req.VideoType != ""             { vc.Config.VideoType = req.VideoType }
		if req.VideoResolution != ""       { vc.Config.VideoResolution = req.VideoResolution }
		if req.VideoFPS != nil             { vc.Config.VideoFPS = *req.VideoFPS }
		if req.VideoAspectRatio != ""      { vc.Config.VideoAspectRatio = req.VideoAspectRatio }
		if req.CharConsistencyWeight != nil { vc.Config.CharConsistencyWeight = *req.CharConsistencyWeight }
		if req.NarrationVoice != ""        { vc.Config.NarrationVoice = req.NarrationVoice }
		if req.SubtitleEnabled != nil      { vc.Config.SubtitleEnabled = *req.SubtitleEnabled }
		if req.SubtitlePosition != ""      { vc.Config.SubtitlePosition = req.SubtitlePosition }
		if req.SubtitleFontSize != nil     { vc.Config.SubtitleFontSize = *req.SubtitleFontSize }
		if req.SubtitleColor != ""         { vc.Config.SubtitleColor = req.SubtitleColor }
		if req.SubtitleBgStyle != ""       { vc.Config.SubtitleBgStyle = req.SubtitleBgStyle }
		if req.SubtitleFont != ""          { vc.Config.SubtitleFont = req.SubtitleFont }
		if req.ColorGrade != ""            { vc.Config.ColorGrade = req.ColorGrade }
		if req.ContrastLevel != nil        { vc.Config.ContrastLevel = *req.ContrastLevel }
		if req.Saturation != nil           { vc.Config.Saturation = *req.Saturation }
		if req.FilmGrain != nil            { vc.Config.FilmGrain = *req.FilmGrain }
		if req.Vignette != nil             { vc.Config.Vignette = *req.Vignette }
		if req.ChromaticAberration != nil  { vc.Config.ChromaticAberration = *req.ChromaticAberration }
		if req.KlingProForAction != nil    { vc.Config.KlingProForAction = *req.KlingProForAction }
		if err := s.novelRepo.SaveVideoConfig(vc); err != nil {
			logger.Errorf("[NovelService] UpdateNovel SaveVideoConfig: %v", err)
			return nil, fmt.Errorf("save video config: %w", err)
		}
	}

	// ── Step 3: return fresh state from DB ───────────────────────────────────
	return s.novelRepo.GetByIDFromDB(id)
}

// coverCharacterRoleRank 主角优先排序权重
func coverCharacterRoleRank(role string) int {
	switch role {
	case "protagonist":
		return 0
	case "antagonist":
		return 1
	case "supporting":
		return 2
	default:
		return 3
	}
}

// generateCoverBrief 调用 LLM 根据小说实际内容生成封面视觉简报（英文 image prompt）。
// 始终能产生输出（哪怕只有标题+简介），失败时返回空字符串。
func (s *NovelService) generateCoverBrief(novel *model.Novel) string {
	var sb strings.Builder
	sb.WriteString("You are a professional book cover art director. Based on the novel information below, write a 90–120 word English image generation prompt.\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- Focus on: protagonist visual appearance (hair color/style, clothing, expression, posture) + key scene or iconic prop\n")
	sb.WriteString("- Express the emotional tone through lighting, color palette, and composition\n")
	sb.WriteString("- Use concrete nouns and adjectives; avoid vague words like 'epic', 'amazing', 'magical'\n")
	sb.WriteString("- Do NOT mention book title or text in the image\n")
	sb.WriteString("- End with the art style suffix that matches the novel's visual style\n")
	sb.WriteString("- Output the English prompt ONLY — no explanation, no prefix, no quotes\n\n")

	sb.WriteString(fmt.Sprintf("Novel title: %s\n", novel.Title))
	sb.WriteString(fmt.Sprintf("Genre: %s\n", novel.Meta.Genre))

	// 简介（最多400字）
	if novel.Meta.Description != "" {
		desc := novel.Meta.Description
		if len([]rune(desc)) > 400 {
			desc = string([]rune(desc)[:400])
		}
		sb.WriteString(fmt.Sprintf("Synopsis: %s\n", desc))
	}

	// 风格设定（style_prompt，如有）
	if novel.AIConfig.StylePrompt != "" {
		style := novel.AIConfig.StylePrompt
		if len([]rune(style)) > 150 {
			style = string([]rune(style)[:150])
		}
		sb.WriteString(fmt.Sprintf("Visual style: %s\n", style))
	}
	if novel.AIConfig.ImageStyle != "" {
		sb.WriteString(fmt.Sprintf("Art style preset: %s\n", novel.AIConfig.ImageStyle))
	}

	// 角色（最多3个，主角优先，Description截前200字）
	if s.characterRepo != nil {
		chars, _ := s.characterRepo.ListByNovel(novel.ID)
		for i := 0; i < len(chars); i++ {
			for j := i + 1; j < len(chars); j++ {
				if coverCharacterRoleRank(chars[i].Role) > coverCharacterRoleRank(chars[j].Role) {
					chars[i], chars[j] = chars[j], chars[i]
				}
			}
		}
		added := 0
		for _, c := range chars {
			if added >= 3 {
				break
			}
			if c.Description == "" {
				continue
			}
			desc := c.Description
			if len([]rune(desc)) > 200 {
				desc = string([]rune(desc)[:200])
			}
			if added == 0 {
				sb.WriteString("Key characters:\n")
			}
			sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", c.Name, c.Role, desc))
			added++
		}
	}

	// 世界观（描述+地理，截前300字）
	if novel.Worldview != nil {
		wv := novel.Worldview
		var wvParts []string
		if wv.Description != "" {
			wvParts = append(wvParts, wv.Description)
		}
		if wv.Geography != "" {
			wvParts = append(wvParts, wv.Geography)
		}
		if len(wvParts) > 0 {
			wvText := strings.Join(wvParts, " ")
			if len([]rune(wvText)) > 300 {
				wvText = string([]rune(wvText)[:300])
			}
			sb.WriteString(fmt.Sprintf("World setting: %s\n", wvText))
		}
	}

	llmInput := sb.String()
	logger.Infof("[coverBrief] novelID=%d inputLen=%d hasDesc=%v hasWorldview=%v",
		novel.ID, len(llmInput), novel.Meta.Description != "", novel.Worldview != nil)

	brief, err := s.aiService.Generate(novel.ID, "cover_brief", llmInput)
	if err != nil {
		logger.Warnf("[coverBrief] LLM failed novelID=%d: %v", novel.ID, err)
		return ""
	}
	brief = strings.TrimSpace(brief)
	if brief == "" {
		logger.Warnf("[coverBrief] LLM returned empty novelID=%d", novel.ID)
		return ""
	}
	// 清除 LLM 可能附加的前缀（如 "Sure! Here's the prompt:"）
	if idx := strings.Index(brief, "\n\n"); idx > 0 && idx < 80 {
		brief = strings.TrimSpace(brief[idx:])
	}
	logger.Infof("[coverBrief] novelID=%d briefLen=%d brief=%.120s", novel.ID, len(brief), brief)
	return brief
}

// buildFallbackCoverPrompt 当 LLM brief 不可用时，用小说自身数据构建兜底 prompt
func buildFallbackCoverPrompt(novel *model.Novel, userSuggestion string) string {
	genreStyleMap := map[string]string{
		"fantasy":    "fantasy xianxia cultivation, mystical glowing energy, ancient Chinese architecture, misty mountains",
		"xianxia":    "xianxia immortal cultivation, celestial robes, floating swords, golden spiritual aura, mountain peaks",
		"urban":      "modern Chinese city at night, neon lights, contemporary fashion",
		"romance":    "romantic atmosphere, soft warm lighting, cherry blossoms or elegant interiors",
		"historical": "ancient Chinese dynasty setting, imperial palace, silk robes, lanterns",
		"scifi":      "science fiction, futuristic technology, holographic displays, space or cyberpunk city",
		"mystery":    "dark mysterious atmosphere, shadows and dim light, suspenseful mood",
		"wuxia":      "Chinese martial arts, warriors in traditional robes, dynamic action pose, mountain landscape",
		"horror":     "horror atmosphere, dark eerie environment, moonlight, supernatural elements",
		"apocalypse": "post-apocalyptic wasteland, ruins, dramatic sky",
		"rebirth":    "rebirth time travel, dual-era contrast, glowing light of second chance",
	}
	genreDesc := genreStyleMap[novel.Meta.Genre]
	if genreDesc == "" {
		genreDesc = novel.Meta.Genre
	}

	// 尽量加入简介内容
	synopsisPart := ""
	if novel.Meta.Description != "" {
		desc := novel.Meta.Description
		if len([]rune(desc)) > 200 {
			desc = string([]rune(desc)[:200])
		}
		synopsisPart = fmt.Sprintf(" Story theme: %s.", desc)
	}

	suggestionPart := ""
	if userSuggestion != "" {
		suggestionPart = fmt.Sprintf(" Style direction: %s.", userSuggestion)
	}

	return fmt.Sprintf(
		"Professional Chinese novel book cover illustration, %s.%s%s "+
			"Cinematic composition, dramatic lighting, vibrant colors, highly detailed digital art.",
		genreDesc, synopsisPart, suggestionPart,
	)
}

// GenerateCoverImage 使用 AI 为小说生成封面图，并将 URL 写回 cover_image 字段
func (s *NovelService) GenerateCoverImage(ctx context.Context, tenantID, novelID uint, suggestion string) (string, error) {
	novel, err := s.novelRepo.GetByIDFromDB(novelID) // 跳过缓存，确保读到最新角色/世界观数据
	if err != nil {
		return "", err
	}
	if novel.TenantID != tenantID {
		return "", fmt.Errorf("not found")
	}

	negativePrompt := "text, watermark, signature, blurry, low quality, ugly, distorted, nsfw, letters, words, title"

	// 判断是否有可用的旧封面作为参考图（图生图模式）
	existingCover := ""
	if suggestion != "" && novel.Meta.CoverImage != "" &&
		(strings.HasPrefix(novel.Meta.CoverImage, "http://") || strings.HasPrefix(novel.Meta.CoverImage, "https://")) {
		existingCover = novel.Meta.CoverImage
	}

	var prompt, promptSource string
	if suggestion != "" && existingCover != "" {
		// 图生图：以旧封面为参考，按用户指令编辑
		prompt = suggestion
		promptSource = "img2img"
	} else {
		// 文生图：用 LLM brief，兜底用结构化模板
		brief := s.generateCoverBrief(novel)
		if brief != "" {
			promptSource = "llm_brief"
			if suggestion != "" {
				prompt = fmt.Sprintf("%s Style direction: %s.", brief, suggestion)
			} else {
				prompt = brief
			}
		} else {
			promptSource = "fallback_template"
			prompt = buildFallbackCoverPrompt(novel, suggestion)
		}
	}

	logger.Infof("[GenerateCoverImage] novelID=%d imageStyle=%q source=%s promptLen=%d prompt=%.200s",
		novel.ID, novel.AIConfig.ImageStyle, promptSource, len(prompt), prompt)

	ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title})
	sizeOverride := ""
	if novel.VideoConfig != nil && novel.VideoConfig.Config.VideoAspectRatio != "" {
		sizeOverride = novel.VideoConfig.Config.VideoAspectRatio // e.g. "16:9", "9:16", "1:1"
	}
	imageURL, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, "", prompt, existingCover, novel.AIConfig.ImageStyle, negativePrompt, sizeOverride)
	if err != nil {
		return "", fmt.Errorf("generate cover image: %w", err)
	}

	novel.Meta.CoverImage = imageURL
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
	if novel.ReviewMeta.ReviewStatus == "approved" {
		// 已审核通过，直接发布
		novel.IsPublished = true
		novel.Meta.PublishedAt = &now
		novel.Meta.Visibility = visibility
		if err := s.novelRepo.UpdateFields(id, map[string]interface{}{
			"is_published": true, "published_at": &now, "visibility": visibility,
		}); err != nil {
			return nil, err
		}
	} else {
		// 提交审核，不直接发布
		novel.ReviewMeta.ReviewStatus = "pending_review"
		novel.Meta.Visibility = visibility
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
	if novel.ReviewMeta.ReviewStatus != "pending_review" {
		return nil, fmt.Errorf("novel is not pending review")
	}
	now := time.Now()
	fields := map[string]interface{}{
		"reviewed_at": &now,
		"reviewed_by": reviewerID,
	}
	if req.Approved {
		novel.ReviewMeta.ReviewStatus = "approved"
		novel.IsPublished = true
		novel.Meta.PublishedAt = &now
		novel.ReviewMeta.ReviewedAt = &now
		novel.ReviewMeta.ReviewedBy = reviewerID
		fields["review_status"] = "approved"
		fields["is_published"] = true
		fields["published_at"] = &now
	} else {
		novel.ReviewMeta.ReviewStatus = "rejected"
		novel.ReviewMeta.ReviewNote = req.ReviewNote
		novel.ReviewMeta.ReviewedAt = &now
		novel.ReviewMeta.ReviewedBy = reviewerID
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
	outlineStart := time.Now()
	recordOutline := func(status string) {
		metrics.OutlineGenerationTotal.WithLabelValues(status).Inc()
		metrics.OutlineGenerationDuration.Observe(time.Since(outlineStart).Seconds())
	}

	// 写操作必须绕过 Redis 缓存直读 DB，避免缓存过期数据导致后续章节外键约束失败
	novel, err := s.novelRepo.GetByIDFromDB(req.NovelID)
	if err != nil {
		recordOutline("error")
		return nil, err
	}
	if novel.TenantID != tenantID {
		recordOutline("error")
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
		outlineOverrides.MaxTokens = novel.AIConfig.MaxTokens
	}
	if outlineOverrides.Temperature == 0 {
		outlineOverrides.Temperature = novel.AIConfig.Temperature
	}
	if outlineOverrides.TimeoutSeconds == 0 {
		outlineOverrides.TimeoutSeconds = novel.AIConfig.TimeoutSeconds
	}

	// 调用AI生成（使用租户提供商）
	result, err := s.aiService.GenerateWithProvider(tenantID, req.NovelID, "outline", prompt, "", outlineOverrides)
	if err != nil {
		recordOutline("error")
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
	// Pre-repair: fix the ""ChineseTitle" corruption before any other parse attempt.
	result = repairCorruptedTitleKey(result)
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
						logger.Errorf("GenerateOutline: failed to parse AI response for novel %d: %v (object len=%d, array len=%d, preview=%q)",
							req.NovelID, parseErr, len(cleaned), len(cleanedArr), preview)
						recordOutline("error")
						return nil, fmt.Errorf("outline parse failed: %w", parseErr)
					}
				}
			}
		}
	}

	// 大纲版本快照：在用新大纲覆盖之前，将当前大纲存为历史版本
	if novel.Outline != "" && s.outlineVersionRepo != nil {
		_ = s.outlineVersionRepo.CreateVersionAtomic(&model.NovelOutlineVersion{
			NovelID: novel.ID,
			Outline: novel.Outline,
			Prompt:  req.Prompt,
		})
	}

	// 将新生成的大纲 JSON 持久化回 novel.outline
	if outlineJSON, marshalErr := json.Marshal(outline); marshalErr == nil {
		novel.Outline = string(outlineJSON)
		_ = s.novelRepo.UpdateFields(novel.ID, map[string]interface{}{"outline": novel.Outline})
	}

	// 大纲生成成功后，更新/创建章节占位记录
	// - 已存在且未完成（draft/outline）的章节：用新大纲更新 summary/title/meta
	// - 已存在且已完成（has content）的章节：仅在独立成篇模式下更新 summary（不动内容/状态）
	// - 不存在的章节：创建新占位记录
	for _, chap := range outline.Chapters {
		existing, err := s.chapterRepo.GetByNovelAndChapterNo(novel.ID, chap.ChapterNo)
		if err == nil && existing != nil {
			// 已完成（有正文）的章节在连贯模式下不覆盖
			if existing.Content != "" && novel.AIConfig.ChapterMode != "independent" {
				continue
			}
			existing.Summary = chap.Summary
			if chap.Title != "" {
				existing.Title = chap.Title
			}
			existing.NarrativeMeta.TensionLevel = chap.TensionLevel
			existing.NarrativeMeta.ActNo = chap.Act
			existing.NarrativeMeta.EmotionalTone = chap.EmotionalTone
			existing.NarrativeMeta.HookType = chap.HookType
			if err := s.chapterRepo.Update(existing); err != nil {
				logger.Errorf("GenerateOutline: update chapter %d: %v", chap.ChapterNo, err)
			}
			continue
		}
		placeholder := &model.Chapter{
			UUID:      uuid.New().String(),
			NovelID:   novel.ID,
			ChapterNo: chap.ChapterNo,
			Title:     chap.Title,
			Summary:   chap.Summary,
			NarrativeMeta: model.ChapterNarrativeMeta{
				TensionLevel:  chap.TensionLevel,
				ActNo:         chap.Act,
				EmotionalTone: chap.EmotionalTone,
				HookType:      chap.HookType,
			},
			Status: "draft",
		}
		if err := s.chapterRepo.Create(placeholder); err != nil {
			logger.Errorf("GenerateOutline: create placeholder chapter %d: %v", chap.ChapterNo, err)
			recordOutline("error")
			return nil, fmt.Errorf("create placeholder chapter %d: %w", chap.ChapterNo, err)
		}
	}

	recordOutline("success")
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
	independent := novel.AIConfig.ChapterMode == "independent"
	var sb strings.Builder

	if independent {
		sb.WriteString(fmt.Sprintf("请为小说集《%s》生成一个详细的章节大纲。\n\n", novel.Title))
		sb.WriteString("⚠️ 章节模式：**独立成篇**——每一章都是一个完整独立的故事，有自己的起承转合与结局，章节之间无剧情关联、无悬念延续、无跨章伏笔。\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("请为小说《%s》生成一个详细的大纲。\n\n", novel.Title))
	}

	if novel.Meta.Description != "" {
		sb.WriteString(fmt.Sprintf("故事简介：%s\n\n", novel.Meta.Description))
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

	if independent {
		if req.ChapterNum > 0 {
			sb.WriteString(fmt.Sprintf("请生成 %d 个独立故事的大纲，每个故事（即每章）必须包含：标题、完整的故事概述（不少于150字，含开场/冲突/高潮/结局）、预计字数（2000-3000字）、主要剧情点。\n", req.ChapterNum))
		} else {
			sb.WriteString("请根据题材自行决定合适的故事数量（通常10-50个），每个故事（即每章）必须包含：标题、完整的故事概述（不少于150字，含开场/冲突/高潮/结局）、预计字数（2000-3000字）、主要剧情点。\n")
		}
		sb.WriteString(`
## 每章（每个独立故事）的创作要求
- **完整性**：必须包含清晰的开场（建立人物/处境）、核心冲突（矛盾激化）、高潮（关键对决/转折）、结局（冲突解决，情感落地）
- **独立性**：不引用其他章节的人物、事件或伏笔；读者无需阅读其他章节即可完全理解本章
- **禁止**：章末悬念钩子、"待续"式结尾、跨章伏笔、依赖外部背景的开头
- **summary 必须描述完整的故事弧光**：从开场到结局，让读者知道这个故事讲了什么、如何结尾

`)
	} else {
		if req.ChapterNum > 0 {
			sb.WriteString(fmt.Sprintf("请生成%d章的大纲，每章包括：标题、详细剧情概述（不少于150字）、预计字数（2000-3000字）、主要剧情点。\n", req.ChapterNum))
		} else {
			sb.WriteString("请根据故事规模自行决定合适的章节数（通常30-200章之间），每章包括：标题、详细剧情概述（不少于150字）、预计字数（2000-3000字）、主要剧情点。\n")
		}

		// 注入未解决剧情点（仅连贯模式需要跨章推进）
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
	}

	if independent {
		sb.WriteString(`
## 输出格式（严格遵守）
仅输出如下 JSON 对象，禁止任何说明文字、markdown 代码块、注释或 schema 以外的额外字段：
{
  "title": "小说集标题",
  "chapters": [
    {
      "chapter_no": 1,
      "title": "故事标题",
      "summary": "完整故事概述，不少于150字。必须涵盖：①开场（人物处境/矛盾引入）②核心冲突（矛盾激化过程）③高潮（关键转折或对决）④结局（冲突如何解决，情感如何落地）。禁止以悬念或"待续"结尾。",
      "word_count": 2500,
      "plot_points": ["剧情点1（含人物+动作+结果，20字内）", "剧情点2", "剧情点3"],
      "emotional_tone": "紧张",
      "tension_level": 7,
      "hook": "",
      "hook_type": "",
      "conflict_type": "人与人",
      "act": 1
    }
  ]
}
字段类型说明：chapter_no/word_count/tension_level/act 必须是整数，其余为字符串或字符串数组。
hook 和 hook_type 在独立成篇模式下必须为空字符串。
最外层必须是 {} 对象，chapters 是其中的数组字段，禁止直接返回 [] 数组。
重要：每章 summary 字段不得少于150字，且必须描述完整的故事弧光（含结局），这是硬性要求。`)
	} else {
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
	}

	return sb.String()
}


// writeCharacterSnapshots 从章节内容中提取角色状态并写入快照
func (s *NovelService) writeCharacterSnapshots(tenantID uint, chapter *model.Chapter) {
	if s.characterRepo == nil || s.snapshotRepo == nil {
		return
	}
	// 分布式心跳锁：30 s base TTL（每10s续期），实例崩溃后最多30s自动释放。
	if s.cache != nil {
		lockKey := fmt.Sprintf("lock:char:snap:%d", chapter.ID)
		lock, ok, lockErr := acquireDistLock(s.cache, lockKey, 30*time.Second)
		if lockErr != nil {
			logger.Errorf("writeCharacterSnapshots: Redis lock error ch%d: %v, continuing without lock", chapter.ID, lockErr)
		} else if !ok {
			logger.Printf("writeCharacterSnapshots: chapter %d already processing by another instance, skip", chapter.ID)
			metrics.CharacterSnapshotExtractionTotal.WithLabelValues("skipped").Inc()
			return
		} else {
			defer lock.release()
		}
	}
	characters, err := s.characterRepo.ListByNovel(chapter.NovelID)
	if err != nil || len(characters) == 0 {
		metrics.CharacterSnapshotExtractionTotal.WithLabelValues("skipped").Inc()
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
		logger.Errorf("writeCharacterSnapshots: AI extraction failed for chapter %d: %v", chapter.ID, err)
		metrics.CharacterSnapshotExtractionTotal.WithLabelValues("ai_error").Inc()
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
		logger.Errorf("writeCharacterSnapshots: parse failed: %v", err)
		metrics.CharacterSnapshotExtractionTotal.WithLabelValues("parse_error").Inc()
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
			NovelID:     chapter.NovelID,
			CharacterID: char.ID,
			ChapterID:   chapter.ID,
			Mood:        state.Mood,
			Location:    state.Location,
			Motivation:  state.Motivation,
			PowerLevel:  state.PowerLevel,
		}
		if err := s.snapshotRepo.Upsert(snapshot); err != nil {
			logger.Errorf("writeCharacterSnapshots: create snapshot failed for char %d: %v", char.ID, err)
			metrics.CharacterSnapshotTotal.WithLabelValues("error").Inc()
		} else {
			metrics.CharacterSnapshotTotal.WithLabelValues("success").Inc()
		}
	}
	metrics.CharacterSnapshotExtractionTotal.WithLabelValues("success").Inc()
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
				NovelID:     chapter.NovelID,
				CharacterID: char.ID,
				ChapterID:   chapter.ID,
				Health:      prevSnap.Health,
				PowerLevel:  prevSnap.PowerLevel,
				Mood:        prevSnap.Mood,
				Motivation:  prevSnap.Motivation,
				Location:    prevSnap.Location,
			}
			if e := s.snapshotRepo.Upsert(snap); e != nil {
				logger.Errorf("SyncCharacterSnapshots: copy snapshot char %d: %v", char.ID, e)
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
			logger.Errorf("SyncCharacterSnapshots: AI failed for char %d: %v", char.ID, err)
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
			logger.Errorf("SyncCharacterSnapshots: parse failed char %d: %v", char.ID, e)
			continue
		}

		health := state.Health
		if health == "" {
			health = "healthy"
		}

		snap := &model.CharacterStateSnapshot{
			NovelID:     chapter.NovelID,
			CharacterID: char.ID,
			ChapterID:   chapter.ID,
			Health:      health,
			PowerLevel:  state.PowerLevel,
			Mood:        state.Mood,
			Motivation:  state.Motivation,
			Location:    state.Location,
		}
		if e := s.snapshotRepo.Upsert(snap); e != nil {
			logger.Errorf("SyncCharacterSnapshots: create snapshot char %d: %v", char.ID, e)
		}
	}
	return nil
}

// updateNovelStats 更新小说统计（使用 DB 聚合避免并发竞态）
func (s *NovelService) updateNovelStats(novelID uint) {
	if err := s.novelRepo.SyncStats(novelID); err != nil {
		logger.Errorf("updateNovelStats: sync novel %d: %v", novelID, err)
	}
}

// extractPlotPoints 提取剧情点并保存到数据库
func (s *NovelService) extractPlotPoints(chapter *model.Chapter) {
	if s.plotPointService == nil {
		return
	}
	if _, err := s.plotPointService.ExtractFromChapter(context.Background(), 0, chapter); err != nil {
		logger.Errorf("extractPlotPoints chapter %d: %v", chapter.ID, err)
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
// Redis 可用时跨实例去重；否则降级为进程内去重。
func (s *NovelService) RecordNovelViewDeduped(id uint, clientIP string) error {
	if s.cache != nil {
		redisKey := fmt.Sprintf("view:novel:%d:%s", id, clientIP)
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", time.Hour).Result()
		if err != nil {
			logger.Errorf("[NovelService] Redis view dedup error: %v, fallback to local", err)
			// fall through to local dedup
		} else if !ok {
			return nil // 已由某实例记录
		} else {
			return s.novelRepo.IncrNovelViewCount(id)
		}
	}
	// 进程内兜底
	localKey := fmt.Sprintf("novel:%s:%d", clientIP, id)
	expiry := time.Now().Add(time.Hour)
	old, _ := s.novelViewDedup.Swap(localKey, expiry)
	if old != nil {
		if t, ok := old.(time.Time); ok && time.Now().Before(t) {
			s.novelViewDedup.Store(localKey, t)
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
func (s *NovelService) AddNovelComment(novelID, userID uint, content string, parentID *uint) (*model.NovelComment, error) {
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
		Content:  content,
		ParentID: parentID,
	}
	if err := s.novelCommentRepo.Create(c); err != nil {
		return nil, err
	}
	if err := s.novelRepo.IncrNovelCommentCount(novelID, 1); err != nil {
		logger.Errorf("AddNovelComment: IncrNovelCommentCount novel=%d: %v", novelID, err)
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
	rows, err := s.novelRepo.ListPublicNovelsForHotCalc()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, n := range rows {
		ageDays := 0.0
		if n.PublishedAt != nil {
			ageDays = now.Sub(*n.PublishedAt).Hours() / 24
		}
		base := float64(n.ViewCount)*0.5 + float64(n.LikeCount)*0.3 + float64(n.CommentCount)*0.2
		score := base / (1 + ageDays*0.1)
		if err := s.novelRepo.UpdateNovelHotScore(n.ID, score); err != nil {
			logger.Errorf("RecalcNovelHotScores: failed to update novel %d: %v", n.ID, err)
		}
	}
	return nil
}
