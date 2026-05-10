package service

import (
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
	novelRepo        *repository.NovelRepository
	chapterRepo      *repository.ChapterRepository
	aiService        *AIService
	characterRepo    *repository.CharacterRepository
	snapshotRepo     *repository.CharacterStateSnapshotRepository
	plotPointService *PlotPointService
	// 广场社交
	novelLikeRepo    *repository.NovelLikeRepository
	novelCommentRepo *repository.NovelCommentRepository
	novelViewDedup   sync.Map // key "ip:id" → expiry time.Time
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
		tenantID = 1
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

// GetNovel 获取小说
func (s *NovelService) GetNovel(id uint) (*model.Novel, error) {
	return s.novelRepo.GetByID(id)
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

// DeleteNovel 删除小说及其全部关联数据
func (s *NovelService) DeleteNovel(id uint) error {
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

// UpdateNovel handler-compatible wrapper
func (s *NovelService) UpdateNovel(id uint, req *model.UpdateNovelRequest) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, err
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
	if err := s.novelRepo.Update(novel); err != nil {
		return nil, err
	}
	return novel, nil
}

// PublishNovel 发布小说到广场
func (s *NovelService) PublishNovel(id uint, visibility string) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	novel.IsPublished = true
	novel.PublishedAt = &now
	novel.Visibility = visibility
	if err := s.novelRepo.UpdateFields(id, map[string]interface{}{
		"is_published": true, "published_at": &now, "visibility": visibility,
	}); err != nil {
		return nil, err
	}
	return novel, nil
}

// UnpublishNovel 取消发布小说
func (s *NovelService) UnpublishNovel(id uint) error {
	return s.novelRepo.UpdateFields(id, map[string]interface{}{
		"is_published": false, "visibility": "private",
	})
}

// GenerateOutlineRequest 生成大纲请求
type GenerateOutlineRequest struct {
	NovelID        uint     `json:"novel_id" binding:"required"`
	Prompt         string   `json:"prompt"`
	ChapterNum     int      `json:"chapter_num" binding:"required"`
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

	// 解析结果
	outline := &OutlineResult{}
	cleaned := extractJSON(result)
	if err := json.Unmarshal([]byte(cleaned), outline); err != nil {
		logger.Printf("GenerateOutline: failed to parse AI response for novel %d: %v", req.NovelID, err)
		outline = &OutlineResult{
			Title:    novel.Title,
			Chapters: []ChapterOutline{},
		}
	}

	return outline, nil
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
	ChapterNo    int      `json:"chapter_no"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	WordCount    int      `json:"word_count"`
	PlotPoints   []string `json:"plot_points"`
	TensionLevel int      `json:"tension_level,omitempty"`
	Hook         string   `json:"hook,omitempty"`
	HookType     string   `json:"hook_type,omitempty"`
	ConflictType string   `json:"conflict_type,omitempty"`
	Act          int      `json:"act,omitempty"`
}

// buildOutlinePrompt 构建大纲提示词
func (s *NovelService) buildOutlinePrompt(novel *model.Novel, req *GenerateOutlineRequest) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("请为小说《%s》生成一个详细的大纲。\n\n", novel.Title))

	if novel.Description != "" {
		sb.WriteString(fmt.Sprintf("故事简介：%s\n\n", novel.Description))
	}

	if len(req.Keywords) > 0 {
		sb.WriteString(fmt.Sprintf("关键词：%s\n\n", strings.Join(req.Keywords, ", ")))
	}

	if req.Prompt != "" {
		sb.WriteString(fmt.Sprintf("创作要求：%s\n\n", req.Prompt))
	}

	sb.WriteString(fmt.Sprintf("请生成%d章的大纲，每章包括：标题、简要概述、预计字数（2000-3000字）、主要剧情点。\n", req.ChapterNum))

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

	sb.WriteString("\n请以JSON格式返回，格式如下：\n")
	sb.WriteString(`{"title":"小说标题","chapters":[{"chapter_no":1,"title":"章节标题","summary":"章节概述","word_count":2500,"plot_points":["剧情点1","剧情点2"]}]}`)

	return sb.String()
}

// GenerateChapterRequest 生成章节请求
type GenerateChapterRequest struct {
	NovelID   uint   `json:"novel_id" binding:"required"`
	ChapterNo int    `json:"chapter_no" binding:"required"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
}

// GenerateChapter 生成章节
func (s *NovelService) GenerateChapter(req *GenerateChapterRequest) (*model.Chapter, error) {
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, err
	}

	// 获取前几章作为上下文
	recentChapters, err := s.chapterRepo.GetRecent(req.NovelID, req.ChapterNo, 3)
	if err != nil {
		return nil, err
	}

	// 构建提示词
	prompt := s.buildChapterPrompt(novel, req, recentChapters)

	// 调用AI生成
	content, err := s.aiService.Generate(req.NovelID, "chapter", prompt)
	if err != nil {
		return nil, err
	}

	// 创建章节
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		NovelID:   req.NovelID,
		ChapterNo: req.ChapterNo,
		Title:     fmt.Sprintf("第%d章", req.ChapterNo),
		Content:   content,
		WordCount: countChineseChars(content),
		Status:    "completed",
	}

	// 生成摘要
	summary, _ := s.aiService.Generate(req.NovelID, "summary", fmt.Sprintf("请简要概括以下内容，不超过100字：\n%s", content))
	chapter.Summary = summary

	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}

	// 更新小说统计
	s.updateNovelStats(req.NovelID)

	// 提取剧情点
	s.extractPlotPoints(chapter)

	// 写入角色状态快照（非阻塞）
	if s.characterRepo != nil && s.snapshotRepo != nil {
		go s.writeCharacterSnapshots(chapter)
	}

	return chapter, nil
}

// writeCharacterSnapshots 从章节内容中提取角色状态并写入快照
func (s *NovelService) writeCharacterSnapshots(chapter *model.Chapter) {
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

	contentPreview := chapter.Content
	if len(contentPreview) > 2000 {
		contentPreview = contentPreview[:2000] + "..."
	}

	prompt := fmt.Sprintf(`从以下章节内容中提取主要角色的当前状态，以JSON格式返回：
角色列表：%s
章节内容：
%s

请返回以下JSON格式（只包含章节中出现的角色）：
{"characters":[{"name":"角色名","mood":"情绪状态","location":"当前位置","motivation":"当前动机","power_level":5}]}`,
		strings.Join(charNames, "、"), contentPreview)

	result, err := s.aiService.Generate(chapter.NovelID, "character_state", prompt)
	if err != nil {
		logger.Printf("writeCharacterSnapshots: AI extraction failed for chapter %d: %v", chapter.ID, err)
		return
	}

	cleaned := extractJSON(result)
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
	if len(contentPreview) > 3000 {
		contentPreview = contentPreview[:3000] + "..."
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
			prevCtx = fmt.Sprintf("角色背景：%s。性格：%s。", char.Background, char.Personality)
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

// buildChapterPrompt 构建章节提示词
func (s *NovelService) buildChapterPrompt(novel *model.Novel, req *GenerateChapterRequest, recentChapters []*model.Chapter) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("请为小说《%s》撰写第%d章。\n\n", novel.Title, req.ChapterNo))

	// 添加世界观信息
	if novel.Worldview != nil {
		sb.WriteString("【世界观设定】\n")
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString(fmt.Sprintf("修炼体系：%s\n", novel.Worldview.MagicSystem))
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString(fmt.Sprintf("地理环境：%s\n", novel.Worldview.Geography))
		}
		if novel.Worldview.Culture != "" {
			sb.WriteString(fmt.Sprintf("文化背景：%s\n", novel.Worldview.Culture))
		}
		if novel.Worldview.CheatSystem != "" {
			sb.WriteString(fmt.Sprintf("金手指/系统：%s\n", novel.Worldview.CheatSystem))
		}
		sb.WriteString("\n")
	}

	// 添加前几章内容作为上下文
	if len(recentChapters) > 0 {
		sb.WriteString("【前情提要】\n")
		for i := len(recentChapters) - 1; i >= 0; i-- {
			ch := recentChapters[i]
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, ch.Summary))
		}
		sb.WriteString("\n")
	}

	if req.Prompt != "" {
		sb.WriteString(fmt.Sprintf("【创作要求】%s\n\n", req.Prompt))
	}

	sb.WriteString(fmt.Sprintf("请撰写第%d章的完整内容，字数要求2000-3000字。\n", req.ChapterNo))
	sb.WriteString("请注意：\n")
	sb.WriteString("1. 保持与前文的剧情连贯性\n")
	sb.WriteString("2. 角色性格和对话风格保持一致\n")
	sb.WriteString("3. 遵循世界观设定\n")
	sb.WriteString("4. 适当埋下伏笔，为后续剧情做铺垫")

	return sb.String()
}

// updateNovelStats 更新小说统计
func (s *NovelService) updateNovelStats(novelID uint) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		logger.Printf("updateNovelStats: list chapters for novel %d: %v", novelID, err)
		return
	}

	var totalWords int
	for _, ch := range chapters {
		totalWords += ch.WordCount
	}

	fields := map[string]interface{}{
		"chapter_count": len(chapters),
		"total_words":   totalWords,
	}

	if len(chapters) > 0 {
		fields["status"] = "writing"
	}

	if err := s.novelRepo.UpdateFields(novelID, fields); err != nil {
		logger.Printf("updateNovelStats: update novel %d: %v", novelID, err)
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
	for range ticker.C {
		now := time.Now()
		s.novelViewDedup.Range(func(k, v any) bool {
			if expiry, ok := v.(time.Time); ok && now.After(expiry) {
				s.novelViewDedup.Delete(k)
			}
			return true
		})
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
	if v, ok := s.novelViewDedup.Load(key); ok {
		if expiry, ok2 := v.(time.Time); ok2 && time.Now().Before(expiry) {
			return nil
		}
	}
	s.novelViewDedup.Store(key, time.Now().Add(time.Hour))
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
	_ = s.novelRepo.IncrNovelLikeCount(novelID, delta)
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

// AddNovelComment 发表评论
func (s *NovelService) AddNovelComment(novelID, userID uint, nickname, content string, parentID *uint) (*model.NovelComment, error) {
	if s.novelCommentRepo == nil {
		return nil, fmt.Errorf("comment feature not available")
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
	_ = s.novelRepo.IncrNovelCommentCount(novelID, 1)
	return c, nil
}

// DeleteNovelComment 删除评论（仅作者本人）
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
	if err := s.novelCommentRepo.Delete(commentID); err != nil {
		return err
	}
	_ = s.novelRepo.IncrNovelCommentCount(c.NovelID, -1)
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
