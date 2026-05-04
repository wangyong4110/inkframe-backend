package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// NovelService 小说服务
type NovelService struct {
	novelRepo        *repository.NovelRepository
	chapterRepo      *repository.ChapterRepository
	aiService        *AIService
	characterRepo    *repository.CharacterRepository
	snapshotRepo     *repository.CharacterStateSnapshotRepository
	plotPointService *PlotPointService
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
	if req.VideoType != "" {
		novel.VideoType = req.VideoType
	}
	if req.VideoResolution != "" {
		novel.VideoResolution = req.VideoResolution
	}
	if req.VideoFPS != nil {
		novel.VideoFPS = *req.VideoFPS
	}
	if req.VideoAspectRatio != "" {
		novel.VideoAspectRatio = req.VideoAspectRatio
	}
	if req.CharConsistencyWeight != nil {
		novel.CharConsistencyWeight = *req.CharConsistencyWeight
	}
	if req.AssetExportPath != "" {
		novel.AssetExportPath = req.AssetExportPath
	}
	if req.NarrationVoice != "" {
		novel.NarrationVoice = req.NarrationVoice
	}
	// 字幕配置（可清空）
	if req.SubtitleEnabled != nil {
		novel.SubtitleEnabled = *req.SubtitleEnabled
	}
	if req.SubtitlePosition != "" {
		novel.SubtitlePosition = req.SubtitlePosition
	}
	if req.SubtitleFontSize != nil {
		novel.SubtitleFontSize = *req.SubtitleFontSize
	}
	if req.SubtitleColor != "" {
		novel.SubtitleColor = req.SubtitleColor
	}
	if req.SubtitleBgStyle != "" {
		novel.SubtitleBgStyle = req.SubtitleBgStyle
	}
	if err := s.novelRepo.Update(novel); err != nil {
		return nil, err
	}
	return novel, nil
}

// GenerateOutlineRequest 生成大纲请求
type GenerateOutlineRequest struct {
	NovelID    uint     `json:"novel_id" binding:"required"`
	Prompt     string   `json:"prompt"`
	ChapterNum int      `json:"chapter_num" binding:"required"`
	Keywords   []string `json:"keywords"`
}

// GenerateOutline 生成大纲
func (s *NovelService) GenerateOutline(tenantID uint, req *GenerateOutlineRequest) (*OutlineResult, error) {
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, err
	}

	// 构建提示词
	prompt := s.buildOutlinePrompt(novel, req)

	// 调用AI生成（使用租户提供商）
	result, err := s.aiService.GenerateWithProvider(tenantID, req.NovelID, "outline", prompt, "")
	if err != nil {
		return nil, err
	}

	// 解析结果
	outline := &OutlineResult{}
	cleaned := extractJSON(result)
	if err := json.Unmarshal([]byte(cleaned), outline); err != nil {
		log.Printf("GenerateOutline: failed to parse AI response for novel %d: %v", req.NovelID, err)
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

	// 获取上一章
	if len(recentChapters) > 0 {
		prev := recentChapters[0]
		chapter.PreviousChapterID = &prev.ID
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
		log.Printf("writeCharacterSnapshots: AI extraction failed for chapter %d: %v", chapter.ID, err)
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
		log.Printf("writeCharacterSnapshots: parse failed: %v", err)
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
			log.Printf("writeCharacterSnapshots: create snapshot failed for char %d: %v", char.ID, err)
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

	if reusePrevious {
		// 复用上章快照：复制到本章
		for _, char := range chars {
			var prevSnap *model.CharacterStateSnapshot
			if prevChapter != nil {
				prevSnap, _ = s.snapshotRepo.GetByChapterAndCharacter(prevChapter.ID, char.ID)
			}
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
				log.Printf("SyncCharacterSnapshots: copy snapshot char %d: %v", char.ID, e)
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
		// 构建上章角色状态上下文
		var prevCtx string
		if prevChapter != nil {
			if ps, _ := s.snapshotRepo.GetByChapterAndCharacter(prevChapter.ID, char.ID); ps != nil {
				prevCtx = fmt.Sprintf(
					"上章末状态：情绪=%s, 位置=%s, 动机=%s, 战力=%d, 健康=%s",
					ps.Mood, ps.Location, ps.Motivation, ps.PowerLevel, ps.Health,
				)
			}
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
			log.Printf("SyncCharacterSnapshots: AI failed for char %d: %v", char.ID, err)
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
			log.Printf("SyncCharacterSnapshots: parse failed char %d: %v", char.ID, e)
			continue
		}

		// 沿用上章快照中的静态字段（身高体重等）
		var baseAbilities, baseEquipment string
		var age, height, weight float64
		if prevChapter != nil {
			if ps, _ := s.snapshotRepo.GetByChapterAndCharacter(prevChapter.ID, char.ID); ps != nil {
				age, height, weight = ps.Age, ps.Height, ps.Weight
				baseEquipment = ps.Equipment
			}
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
			log.Printf("SyncCharacterSnapshots: create snapshot char %d: %v", char.ID, e)
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

// countChineseChars 统计中文字符数
func countChineseChars(text string) int {
	count := 0
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fa5 {
			count++
		}
	}
	return count
}

// updateNovelStats 更新小说统计
func (s *NovelService) updateNovelStats(novelID uint) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		log.Printf("updateNovelStats: list chapters for novel %d: %v", novelID, err)
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
		log.Printf("updateNovelStats: update novel %d: %v", novelID, err)
	}
}

// extractPlotPoints 提取剧情点并保存到数据库
func (s *NovelService) extractPlotPoints(chapter *model.Chapter) {
	if s.plotPointService == nil {
		return
	}
	if _, err := s.plotPointService.ExtractFromChapter(0, chapter); err != nil {
		log.Printf("extractPlotPoints chapter %d: %v", chapter.ID, err)
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
type AIService struct {
	modelRepo     *repository.AIModelRepository
	taskRepo      *repository.TaskModelConfigRepository
	aiManager     *ai.ModelManager
	providerRepo  *repository.ModelProviderRepository
	novelRepo     *repository.NovelRepository
	storageSvc    storage.Service
	taskRouting   TaskRouting
	providerCache sync.Map     // key: "tenantID:providerName" → providerCacheEntry
	imageSem      chan struct{} // nil = unlimited; set via WithImageConcurrency
	semMu         sync.RWMutex // protects imageSem replacement
}

func NewAIService(
	modelRepo *repository.AIModelRepository,
	taskRepo *repository.TaskModelConfigRepository,
	aiManager *ai.ModelManager,
	providerRepo ...*repository.ModelProviderRepository,
) *AIService {
	svc := &AIService{
		modelRepo: modelRepo,
		taskRepo:  taskRepo,
		aiManager: aiManager,
	}
	if len(providerRepo) > 0 {
		svc.providerRepo = providerRepo[0]
	}
	return svc
}

// WithNovelRepo 注入小说仓库，用于在生成时读取小说级 AI 配置
func (s *AIService) WithNovelRepo(repo *repository.NovelRepository) *AIService {
	s.novelRepo = repo
	return s
}

// WithStorage 注入媒体存储服务，供图片生成后持久化使用
func (s *AIService) WithStorage(svc storage.Service) *AIService {
	s.storageSvc = svc
	return s
}

// WithTaskRouting 设置各任务类型优先使用的 provider（来自 config.yaml ai.tasks）
func (s *AIService) WithTaskRouting(tr TaskRouting) *AIService {
	s.taskRouting = tr
	return s
}

// WithImageConcurrency 设置图像生成的最大并发数。
// n ≤ 0 时不限制并发（仅受 AI 提供商限速约束）。
func (s *AIService) WithImageConcurrency(n int) *AIService {
	if n > 0 {
		s.imageSem = make(chan struct{}, n)
	}
	return s
}

// SetImageConcurrency 运行时动态调整图像并发度（线程安全）。
func (s *AIService) SetImageConcurrency(n int) {
	s.semMu.Lock()
	defer s.semMu.Unlock()
	if n > 0 {
		s.imageSem = make(chan struct{}, n)
	} else {
		s.imageSem = nil
	}
}

// Generate 生成内容（使用系统级提供商，tenantID=0）
func (s *AIService) Generate(novelID uint, taskType string, prompt string) (string, error) {
	return s.GenerateWithProvider(0, novelID, taskType, prompt, "")
}

// GetDefaultProviderName 返回当前默认 provider 名称
func (s *AIService) GetDefaultProviderName() string {
	if s.aiManager == nil {
		return "unknown"
	}
	p, err := s.aiManager.GetProvider("")
	if err != nil {
		return "unknown"
	}
	return p.GetName()
}

// getTenantProvider 按租户加载提供商（带缓存，TTL 5 分钟）
func (s *AIService) getTenantProvider(tenantID uint, providerName string) (ai.AIProvider, error) {
	if s.providerRepo == nil {
		return s.aiManager.GetProvider(providerName)
	}

	cacheKey := fmt.Sprintf("%d:%s", tenantID, providerName)

	// 检查缓存
	if v, ok := s.providerCache.Load(cacheKey); ok {
		entry, assertOK := v.(providerCacheEntry)
		if !assertOK {
			s.providerCache.Delete(cacheKey)
		} else if time.Now().Before(entry.expiresAt) {
			return entry.provider, nil
		} else {
			s.providerCache.Delete(cacheKey)
		}
	}

	// 从 DB 加载（租户私有 + 系统级）
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return s.aiManager.GetProvider(providerName)
	}

	// 优先租户私有，其次系统级（优先选择有 credentials 的 provider）
	var tenantMatch, systemMatch *model.ModelProvider
	for _, p := range providers {
		// 跳过已禁用的提供商
		if !p.IsActive {
			continue
		}
		// 当未指定具体提供商时，跳过图像/视频/语音/嵌入类型（这些不做文本生成）
		if providerName == "" {
			t := strings.ToLower(p.Type)
			if t == "image" || t == "video" || t == "voice" || t == "embedding" {
				continue
			}
		}
		if providerName == "" || p.Name == providerName {
			if p.TenantID == tenantID && tenantID != 0 {
				if tenantMatch == nil || (!providerHasCredentials(tenantMatch) && providerHasCredentials(p)) {
					tenantMatch = p
				}
				if providerHasCredentials(tenantMatch) {
					break
				}
			}
			if p.TenantID == 0 {
				// 优先选有凭证的系统级 provider，已有有凭证的则不覆盖
				if systemMatch == nil {
					systemMatch = p
				} else if !providerHasCredentials(systemMatch) && providerHasCredentials(p) {
					systemMatch = p
				}
			}
		}
	}
	matched := tenantMatch
	if matched == nil {
		matched = systemMatch
	}

	if matched == nil {
		// DB 中无配置，降级到内存 aiManager
		return s.aiManager.GetProvider(providerName)
	}

	// Validate credentials before constructing the provider.
	if !providerHasCredentials(matched) {
		log.Printf("getTenantProvider: DB provider %q missing credentials, falling back to in-memory manager", matched.Name)
		return s.aiManager.GetProvider(providerName)
	}

	// Instantiate the provider.
	// 名称优先匹配已知 key；对自定义名称（如"豆包图片"）则根据 endpoint 推断构造器。
	timeout := ai.ResolveTimeout(matched.Timeout)
	var provider ai.AIProvider
	switch matched.Name {
	case ai.ProviderNameVolcengineVisual:
		provider = ai.NewVolcengineVisualProvider(matched.APIKey, matched.APISecretKey)
	case "openai":
		provider = ai.NewOpenAIProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "anthropic":
		provider = ai.NewAnthropicProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "google":
		provider = ai.NewGoogleProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "doubao":
		provider = ai.NewDoubaoProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "doubao-speech":
		// APIKey = X-Api-Key, APIVersion = resourceID（如 "seed-tts-2.0"）
		provider = ai.NewDoubaoSpeechProvider(matched.APIKey, matched.APIVersion)
	case "doubao-speech-v1":
		// APIKey = appID, APISecretKey = access_token, APIVersion = cluster（默认 volcano_tts）
		provider = ai.NewDoubaoSpeechV1Provider(matched.APIKey, matched.APISecretKey, matched.APIVersion)
	case "deepseek":
		provider = ai.NewDeepSeekProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "qianwen":
		provider = ai.NewQianwenProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "azure":
		provider = ai.NewAzureProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, "", timeout)
	default:
		// 自定义名称：按 endpoint 推断底层 API 格式
		ep := strings.ToLower(matched.APIEndpoint)
		switch {
		case strings.Contains(ep, "volces.com") || strings.Contains(ep, "volcengine"):
			// 火山方舟 / 豆包系列（OpenAI 兼容格式）
			log.Printf("getTenantProvider: provider %q mapped to doubao constructor via endpoint", matched.Name)
			provider = ai.NewDoubaoProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
		case strings.Contains(ep, "azure.com") || strings.Contains(ep, "openai.azure"):
			log.Printf("getTenantProvider: provider %q mapped to azure constructor via endpoint", matched.Name)
			provider = ai.NewAzureProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, "", timeout)
		case strings.Contains(ep, "anthropic.com"):
			log.Printf("getTenantProvider: provider %q mapped to anthropic constructor via endpoint", matched.Name)
			provider = ai.NewAnthropicProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
		case matched.APIEndpoint != "":
			// 有自定义 endpoint → 按 OpenAI 兼容格式通用处理
			log.Printf("getTenantProvider: provider %q using OpenAI-compatible constructor for endpoint %s", matched.Name, matched.APIEndpoint)
			provider = ai.NewOpenAIProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
		default:
			log.Printf("getTenantProvider: unrecognized provider %q with no endpoint — falling back to static aiManager", matched.Name)
			return s.aiManager.GetProvider(providerName)
		}
	}

	// 包装重试
	provider = ai.NewRetryProvider(provider, 3, 500*time.Millisecond)

	// 写入缓存
	s.providerCache.Store(cacheKey, providerCacheEntry{
		provider:  provider,
		expiresAt: time.Now().Add(5 * time.Minute),
	})

	return provider, nil
}

// CheckAvailability 检查指定租户是否有可用的 LLM 提供商（用于 pipeline 预检）
func (s *AIService) CheckAvailability(tenantID uint) error {
	_, err := s.getTenantProvider(tenantID, "")
	return err
}

// InvalidateProviderCache 删除指定提供商在所有租户下的缓存，供 DeleteProvider/UpdateProvider 调用。
func (s *AIService) InvalidateProviderCache(providerName string) {
	s.providerCache.Range(func(k, _ any) bool {
		key, ok := k.(string)
		// key format: "tenantID:providerName"
		if ok && strings.HasSuffix(key, ":"+providerName) {
			s.providerCache.Delete(k)
		}
		return true
	})
}

// GenerateWithProvider 使用指定 Provider 生成内容（providerName 为空则使用默认）
// 参数优先级：小说项目配置 > 任务配置 > 内置默认值
func (s *AIService) GenerateWithProvider(tenantID uint, novelID uint, taskType string, prompt string, providerName string) (string, error) {
	// 获取任务配置（基础层）
	base, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		base = &model.TaskModelConfig{
			Temperature: 0.7,
			MaxTokens:   16384, // 默认值提高至 16384，避免提取类任务（角色/物品/世界观）JSON 被截断
		}
	}
	// 复制一份避免污染缓存
	config := *base

	// 应用小说项目级 AI 配置（覆盖任务默认值）
	var resolvedModel string
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			if novel.Temperature > 0 {
				config.Temperature = novel.Temperature
			}
			if novel.TopP > 0 {
				config.TopP = novel.TopP
			}
			if novel.TopK > 0 {
				config.TopK = novel.TopK
			}
			if novel.MaxTokens > 0 {
				config.MaxTokens = novel.MaxTokens
			}
			// 若小说配置了特定 AI 模型且调用方未指定 provider，
			// 通过模型名反查对应 provider 并将模型名透传给 API
			if providerName == "" && novel.AIModel != "" {
				resolvedModel = novel.AIModel
				if pName := s.resolveProviderFromModel(tenantID, novel.AIModel); pName != "" {
					providerName = pName
				}
			}
		}
	}

	// 分镜/角色/世界观等 JSON 输出量大的任务，不允许被 novel.MaxTokens（章节字数目标）压低
	// novel.MaxTokens 存的是章节字数目标 ×2，不代表模型 output token 上限
	switch taskType {
	case "storyboard", "character", "worldview", "character_state", "scene_anchor_extract":
		if config.MaxTokens < 16384 {
			config.MaxTokens = 16384
		}
		// JSON 结构化输出任务降温：高温度会产生格式漂移（多余 markdown / 说明文字），
		// 触发 retry 反而更慢；0.1 足以保证输出格式稳定。
		if config.Temperature > 0.2 {
			config.Temperature = 0.1
		}
	}

	// 调用真实AI API
	result, err := s.callAIWithProvider(tenantID, prompt, &config, providerName, resolvedModel)
	if err != nil {
		return "", fmt.Errorf("AI generation failed: %w", err)
	}

	// 记录使用
	s.logUsage(&config, prompt, result)

	return result, nil
}

// resolveProviderFromModel 通过模型名（如 "deepseek-chat"）在 DB 中查找对应的 provider name（如 "deepseek"）
// 若查找失败则静默返回空字符串（由 getTenantProvider 兜底选择第一个可用 provider）
func (s *AIService) resolveProviderFromModel(tenantID uint, modelName string) string {
	if s.modelRepo == nil {
		return ""
	}
	m, err := s.modelRepo.GetByName(modelName)
	if err != nil || m == nil || m.Provider == nil {
		return ""
	}
	providerName := m.Provider.Name
	// 确认该 provider 对当前租户可用（有凭证）
	if s.providerRepo != nil {
		providers, err := s.providerRepo.ListByTenant(tenantID)
		if err == nil {
			for _, p := range providers {
				if p.Name == providerName && p.IsActive && providerHasCredentials(p) {
					return providerName
				}
			}
		}
		return "" // provider 无凭证，让 getTenantProvider 自动选择
	}
	return providerName
}

// callAI 调用AI接口（使用系统级 provider）
func (s *AIService) callAI(prompt string, config *model.TaskModelConfig) (string, error) {
	return s.callAIWithProvider(0, prompt, config, "")
}

// GenerateWithVision 使用 Vision AI 分析图像内容
// 优先使用 anthropic（claude-3），其次 openai（gpt-4o），都失败则用默认 provider
func (s *AIService) GenerateWithVision(prompt string, imageURLs []string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider
	var err error
	for _, name := range []string{"anthropic", "openai"} {
		provider, err = s.aiManager.GetProvider(name)
		if err == nil {
			break
		}
	}
	if err != nil {
		provider, err = s.aiManager.GetProvider("")
		if err != nil {
			return "", fmt.Errorf("failed to get AI provider for vision: %w", err)
		}
	}

	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{
			{
				Role:      "user",
				Content:   prompt,
				ImageURLs: imageURLs,
			},
		},
		MaxTokens:   512,
		Temperature: 0.1,
	}

	resp, err := provider.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	return resp.Content, nil
}

// callAIWithProvider 调用指定 Provider 的 AI 接口
// modelOverride 可选，非空时会覆盖 provider 的默认模型（用于小说项目级 ai_model 配置）
func (s *AIService) callAIWithProvider(tenantID uint, prompt string, config *model.TaskModelConfig, providerName string, modelOverride ...string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		log.Printf("callAIWithProvider: getTenantProvider failed (tenant=%d, provider=%q): %v", tenantID, providerName, err)
		return "", fmt.Errorf("failed to get AI provider: %w", err)
	}

	req := &ai.GenerateRequest{
		Messages:    []ai.ChatMessage{{Role: "user", Content: prompt}},
		MaxTokens:   config.MaxTokens,
		Temperature: config.Temperature,
		TopP:        config.TopP,
	}
	if len(modelOverride) > 0 && modelOverride[0] != "" {
		req.Model = modelOverride[0]
	}
	// Claude 不支持 top_k，仅在非 Anthropic provider 时传入
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}
	// Stop sequences 仅 OpenAI 支持，其他 provider 忽略
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
	}

	resp, err := provider.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}

	return resp.Content, nil
}

// generateJSONForTenant 带 tenantID 的 JSON 生成重试（最多重试 maxRetries 次）
func (s *AIService) generateJSONForTenant(tenantID, novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			log.Printf("generateJSONForTenant: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.GenerateWithProvider(tenantID, novelID, taskType, p, "")
		if err != nil {
			lastErr = err
			continue
		}
		cleaned := extractJSON(result)
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		log.Printf("generateJSONForTenant: %v", lastErr)
	}
	return "", fmt.Errorf("generateJSONForTenant failed after %d attempts: %w", maxRetries+1, lastErr)
}

// generateWithRetry 带容错重试的 JSON 生成（最多重试 2 次）
func (s *AIService) generateWithRetry(novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			log.Printf("generateWithRetry: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.Generate(novelID, taskType, p)
		if err != nil {
			lastErr = err
			continue
		}
		// 尝试提取 JSON
		cleaned := extractJSON(result)
		// 验证是否为有效 JSON
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		log.Printf("generateWithRetry: %v", lastErr)
	}
	return "", fmt.Errorf("generateWithRetry failed after %d attempts: %w", maxRetries+1, lastErr)
}

// extractJSON 从 AI 输出中提取纯 JSON 字符串
func extractJSON(content string) string {
	content = strings.TrimSpace(content)
	if idx := strings.Index(content, "```json"); idx != -1 {
		content = content[idx+7:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	} else if idx := strings.Index(content, "```"); idx != -1 {
		content = content[idx+3:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	}
	content = strings.TrimSpace(content)
	start := strings.IndexAny(content, "{[")
	if start == -1 {
		return content
	}
	openChar := content[start]
	closeChar := byte('}')
	if openChar == '[' {
		closeChar = ']'
	}
	depth := 0
	for i := start; i < len(content); i++ {
		if content[i] == openChar {
			depth++
		} else if content[i] == closeChar {
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	return content[start:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// logUsage 记录使用
func (s *AIService) logUsage(config *model.TaskModelConfig, prompt, result string) {
	inputTokens := countChineseChars(prompt)
	outputTokens := countChineseChars(result)

	log := &model.ModelUsageLog{
		ModelID:      config.PrimaryModelID,
		TaskType:     "generation",
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         float64(inputTokens+outputTokens) / 1000 * 0.01,
		Latency:      1.5,
		Success:      true,
	}

	s.modelRepo.LogUsage(log)
}

// GenerateImage 调用AI生成图像
func (s *AIService) GenerateImage(prompt string, options *ImageGenerationOptions) (*GeneratedImage, error) {
	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.aiManager.GetProvider("")
	if err != nil {
		return nil, fmt.Errorf("get AI provider failed: %w", err)
	}
	req := &ai.ImageGenerateRequest{
		Prompt:         prompt,
		NegativePrompt: options.NegativePrompt,
		Size:           options.Size,
		Steps:          options.Steps,
		CFGScale:       options.CFGScale,
	}
	resp, err := provider.ImageGenerate(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return &GeneratedImage{
		URL:    resp.URL,
		Width:  resp.Width,
		Height: resp.Height,
	}, nil
}

// knownImageCapableProviders 已知支持图像生成的提供者及其默认模型，用于 DB 动态加载的回退路径。
var knownImageCapableProviders = []ai.ImageProviderEntry{
	{ProviderName: "doubao", Model: "seedream-3-0-t2i-250415", Size: "1024x1024"},
	{ProviderName: "qianwen", Model: "wanx2.1-t2i-turbo", Size: "1024x1024"},
	{ProviderName: "openai", Model: "dall-e-3", Size: "1024x1024"},
	{ProviderName: "volcengine-visual", Model: ai.VolcModelText2ImgV3, Size: "1328x1328"},
}

// loadDBImageProviderEntries 从 DB 加载 IMAGE 类型的提供者列表，使用实际配置的模型名称（APIVersion）。
// 避免 knownImageCapableProviders 硬编码模型与用户实际配置不匹配的问题。
// volcengine-visual 排在末尾：它需要服务端下载参考图，私有 OSS URL 会导致 403 失败。
func (s *AIService) loadDBImageProviderEntries(tenantID uint) []ai.ImageProviderEntry {
	if s.providerRepo == nil {
		return nil
	}
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil
	}
	defaultSizeMap := map[string]string{
		"doubao":                          "1024x1024",
		"qianwen":                         "1024x1024",
		"openai":                          "1024x1024",
		ai.ProviderNameVolcengineVisual:   "1328x1328",
	}
	var primary, volcengine []ai.ImageProviderEntry
	seen := map[string]bool{}
	for _, p := range providers {
		if !p.IsActive {
			log.Printf("loadDBImageProviderEntries: skip provider %q (inactive)", p.Name)
			continue
		}
		if !strings.EqualFold(p.Type, "image") {
			continue // non-image providers are expected, no need to log
		}
		if !providerHasCredentials(p) {
			log.Printf("loadDBImageProviderEntries: skip IMAGE provider %q (missing credentials)", p.Name)
			continue
		}
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		size := defaultSizeMap[p.Name]
		if size == "" {
			size = "1024x1024"
		}
		entry := ai.ImageProviderEntry{ProviderName: p.Name, Model: p.APIVersion, Size: size}
		log.Printf("loadDBImageProviderEntries: adding IMAGE provider %q model=%q size=%s (tenantID=%d)", p.Name, p.APIVersion, size, tenantID)
		// volcengine-visual 依赖服务端下载参考图，排到最后作为兜底
		if p.Name == ai.ProviderNameVolcengineVisual {
			volcengine = append(volcengine, entry)
		} else {
			primary = append(primary, entry)
		}
	}
	result := append(primary, volcengine...)
	if len(result) == 0 {
		log.Printf("loadDBImageProviderEntries: no IMAGE providers found for tenantID=%d (total providers checked: %d)", tenantID, len(providers))
	}
	return result
}

// selectImageModel returns the model to use for the given entry.
// For volcengine-visual: referenceImage → DreamO; style=="realistic" → PortraitPhoto.
// selectImageModel 根据提供者、参考图、风格和一致性权重选择合适的图像生成模型。
// consistencyWeight: 0-1，≥0.7 使用 DreamO（角色特征保持），<0.7 使用 SeedEditV3（指令编辑）
func selectImageModel(entry ai.ImageProviderEntry, referenceImage, style string, consistencyWeight ...float64) string {
	if entry.ProviderName == ai.ProviderNameVolcengineVisual {
		// volcengine-visual 始终用内置 req_key，不依赖用户填写的 APIVersion
		if referenceImage != "" {
			// 写实风格：即使有参考图也使用 PortraitPhoto，保证生成真实感肖像
			if style == "realistic" {
				return ai.VolcModelPortraitPhoto
			}
			weight := 1.0
			if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
				weight = consistencyWeight[0]
			}
			if weight >= 0.7 {
				return ai.VolcModelDreamO
			}
			return ai.VolcModelSeedEditV3
		}
		// 无参考图时：PortraitPhoto 是 I2I 模型，必须有 image_input 才能正常工作；
		// 无论什么风格都使用 Text2ImgV3（文生图），这样 prompt/negative_prompt 才能完整生效。
		return ai.VolcModelText2ImgV3
	}
	return entry.Model
}

// GenerateCharacterThreeView 使用图像生成 API 生成角色/场景视图图像。
// style: 图片风格（"realistic"/"anime"/"ink_painting" 等），影响 Volcengine 模型选择。
// 空字符串表示使用提供者默认模型。
// consistencyWeight（可选）: 0-1，角色一致性强度；默认 1.0（严格）。
//   ≥0.7 → DreamO（角色特征保持），<0.7 → SeedEditV3（指令编辑，scale 线性映射 1-10）
func (s *AIService) GenerateCharacterThreeView(ctx context.Context, tenantID uint, providerName, prompt, referenceImage, style, negativePrompt string, consistencyWeight ...float64) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	// 并发限流：若配置了 image_concurrency，则在此处等待令牌
	s.semMu.RLock()
	sem := s.imageSem
	s.semMu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	weight := 1.0
	if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
		weight = consistencyWeight[0]
	}
	// SeedEditV3 的 scale 参数范围 1-10；以 weight 线性映射
	cfgScale := 1.0 + weight*9.0

	// 指定提供者时：直接加载并调用，不走遍历逻辑
	if providerName != "" {
		// 找到对应的 entry（model/size）
		var entry *ai.ImageProviderEntry
		for _, e := range knownImageCapableProviders {
			if e.ProviderName == providerName {
				entry = &e
				break
			}
		}
		// 也在静态注册列表里找
		if entry == nil {
			for _, e := range s.aiManager.GetImageProviders() {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			return "", fmt.Errorf("unknown image provider: %s", providerName)
		}
		provider, err := s.aiManager.GetProvider(providerName)
		if err != nil {
			// 静态 manager 无此 provider，尝试 DB
			provider, err = s.getTenantProvider(tenantID, providerName)
			if err != nil {
				return "", fmt.Errorf("image provider %q not available: %w", providerName, err)
			}
		}
		resp, err := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
			Model:             selectImageModel(*entry, referenceImage, style, weight),
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              entry.Size,
			ReferenceImage:    referenceImage,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
		})
		if err != nil {
			return "", err
		}
		if resp.Error != "" {
			return "", fmt.Errorf("image generation failed: %s", resp.Error)
		}
		return resp.URL, nil
	}

	// DB 模式（providerRepo 存在）时：DB 是唯一权威来源，完全忽略静态 aiManager 的图像提供者。
	// 这样删除/更改 DB 中的提供者可以立即生效，不受 env/config.yaml 中通用 AI key 的干扰。
	// 纯静态模式（无 DB）：读 aiManager 静态列表，为空时回退硬编码表。
	var entries []ai.ImageProviderEntry
	useDB := s.providerRepo != nil
	if useDB {
		entries = s.loadDBImageProviderEntries(tenantID)
	} else {
		entries = s.aiManager.GetImageProviders()
		if len(entries) == 0 {
			entries = knownImageCapableProviders
		}
	}

	if len(entries) == 0 {
		return "", fmt.Errorf("no image providers configured")
	}

	var lastErr error
	for _, e := range entries {
		var provider ai.AIProvider
		var err error
		if useDB {
			// 从 DB 动态加载提供者（带租户感知和缓存）
			provider, err = s.getTenantProvider(tenantID, e.ProviderName)
		} else {
			provider, err = s.aiManager.GetProvider(e.ProviderName)
		}
		if err != nil {
			lastErr = err
			continue
		}
		model := selectImageModel(e, referenceImage, style, weight)
		log.Printf("GenerateCharacterThreeView: trying provider=%s model=%s refImage=%v", e.ProviderName, model, referenceImage != "")
		resp, err := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
			Model:             model,
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              e.Size,
			ReferenceImage:    referenceImage,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
		})
		if err != nil {
			log.Printf("GenerateCharacterThreeView: provider=%s failed: %v", e.ProviderName, err)
			lastErr = err
			continue
		}
		if resp.Error != "" {
			log.Printf("GenerateCharacterThreeView: provider=%s error: %s", e.ProviderName, resp.Error)
			lastErr = fmt.Errorf("image generation failed: %s", resp.Error)
			continue
		}
		return resp.URL, nil
	}
	return "", fmt.Errorf("no image provider available: %w", lastErr)
}

// AudioGenerate 调用默认 AI provider 生成 TTS 音频，返回本地文件路径（file:// URL）
func (s *AIService) AudioGenerate(ctx context.Context, text, voice string) (string, error) {
	return s.AudioGenerateWithOptions(ctx, 0, text, voice, 1.0, "")
}

// AudioGenerateWithOptions 支持语速和风格的 TTS 生成。
// Provider 选取顺序（DB 模式优先，与图像生成保持一致）：
//  1. DB 中租户配置的 voice/tts 类型 provider
//  2. config.yaml ai.tasks.tts 指定的 provider（静态模式兜底）
//  3. env-var 注册的默认 provider（静态模式兜底）
func (s *AIService) AudioGenerateWithOptions(ctx context.Context, tenantID uint, text, voice string, speed float64, style string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider

	// 1. DB 模式：扫描 voice/tts 类型的 provider（与图像生成逻辑对称）
	if s.providerRepo != nil {
		if p, err := s.loadDBVoiceProvider(tenantID); err == nil && p != nil {
			provider = p
		}
	}

	// 2. 静态模式：config.yaml task routing
	if provider == nil && s.taskRouting.TTS != "" {
		if p, err := s.aiManager.GetProvider(s.taskRouting.TTS); err == nil {
			provider = p
		}
	}
	// 注意：不再兜底到默认 LLM provider，LLM 提供商通常不支持 /audio/speech 接口，
	// 兜底只会产生 404 错误，不如直接给用户明确的配置提示。

	if provider == nil {
		return "", fmt.Errorf("未配置语音合成提供商，请在「模型管理」中添加一个类型为 voice 或 tts 的 AI 提供商（如豆包语音、OpenAI TTS 等）并填写 API Key")
	}

	if voice == "" {
		voice = "alloy"
	}
	if speed <= 0 {
		speed = 1.0
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:    text,
		Voice:   voice,
		Speed:   speed,
		Emotion: style,
	})
	if err != nil {
		return "", err
	}
	return resp.URL, nil
}

// loadDBVoiceProvider 从 DB 中取第一个有效的 voice/tts 类型提供商。
func (s *AIService) loadDBVoiceProvider(tenantID uint) (ai.AIProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		t := strings.ToLower(p.Type)
		if t != "voice" && t != "tts" {
			continue
		}
		if !providerHasCredentials(p) {
			log.Printf("loadDBVoiceProvider: skip voice provider %q (missing credentials)", p.Name)
			continue
		}
		provider, err := s.getTenantProvider(tenantID, p.Name)
		if err != nil {
			log.Printf("loadDBVoiceProvider: failed to instantiate provider %q: %v", p.Name, err)
			continue
		}
		log.Printf("loadDBVoiceProvider: using voice provider %q model=%q", p.Name, p.APIVersion)
		return provider, nil
	}
	return nil, fmt.Errorf("no voice providers configured in DB")
}

// QualityService 质量服务
type QualityService struct {
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
	characterRepo *repository.CharacterRepository
	aiService     *AIService
}

func NewQualityService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	aiService *AIService,
) *QualityService {
	return &QualityService{
		novelRepo:     novelRepo,
		chapterRepo:   chapterRepo,
		characterRepo: characterRepo,
		aiService:     aiService,
	}
}

// CheckChapterQuality 检查章节质量
func (s *QualityService) CheckChapterQuality(chapterID uint) (*QualityReport, error) {
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, err
	}

	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, err
	}

	report := &QualityReport{
		Issues:      []QualityIssue{},
		Suggestions: []string{},
	}

	// 1. 检查角色一致性
	charIssues := s.checkCharacterConsistency(chapter, novel)
	report.Issues = append(report.Issues, charIssues...)

	// 2. 检查世界观一致性
	worldIssues := s.checkWorldviewConsistency(chapter, novel)
	report.Issues = append(report.Issues, worldIssues...)

	// 3. 检查重复性
	repetitionIssues := s.checkRepetition(chapter)
	report.Issues = append(report.Issues, repetitionIssues...)

	// 4. 计算整体评分（基于问题权重）
	highCount, mediumCount := 0, 0
	for _, issue := range report.Issues {
		switch issue.Severity {
		case "high":
			highCount++
		case "medium":
			mediumCount++
		}
	}
	report.OverallScore = 1.0 - float64(highCount)*0.15 - float64(mediumCount)*0.05
	if report.OverallScore < 0 {
		report.OverallScore = 0
	}
	if report.OverallScore > 1.0 {
		report.OverallScore = 1.0
	}

	// 5. 生成建议
	report.Suggestions = s.generateSuggestions(report.Issues)

	return report, nil
}

// checkCharacterConsistency 检查角色一致性（基于文本规则）
func (s *QualityService) checkCharacterConsistency(chapter *model.Chapter, novel *model.Novel) []QualityIssue {
	issues := []QualityIssue{}

	characters, _ := s.characterRepo.ListByNovel(novel.ID)
	if len(characters) == 0 {
		return issues
	}

	// 检查主要角色在章节中的出现次数
	for _, char := range characters {
		nameCount := strings.Count(chapter.Content, char.Name)
		if nameCount == 0 && len(chapter.Content) > 1000 {
			issues = append(issues, QualityIssue{
				Type:        "character_consistency",
				Severity:    "low",
				Description: fmt.Sprintf("主要角色「%s」在本章未出现，可能影响剧情连贯", char.Name),
				Location:    "全文",
				Suggestion:  fmt.Sprintf("确认角色「%s」是否应在本章出场", char.Name),
			})
		}
	}

	return issues
}

// checkWorldviewConsistency 检查世界观一致性（基于文本规则）
func (s *QualityService) checkWorldviewConsistency(chapter *model.Chapter, novel *model.Novel) []QualityIssue {
	issues := []QualityIssue{}

	if novel.Worldview == nil {
		return issues
	}

	// 检查世界观关键词是否与内容一致
	if novel.Worldview.MagicSystem != "" {
		keywords := strings.Fields(novel.Worldview.MagicSystem)
		foundAny := false
		for _, kw := range keywords {
			if len(kw) >= 2 && strings.Contains(chapter.Content, kw) {
				foundAny = true
				break
			}
		}
		if !foundAny && len(chapter.Content) > 2000 {
			issues = append(issues, QualityIssue{
				Type:        "worldview_consistency",
				Severity:    "low",
				Description: "本章未提及修炼/魔法体系相关内容，可能导致世界观缺失感",
				Location:    "全文",
				Suggestion:  "建议在适当位置融入世界观设定元素",
			})
		}
	}

	return issues
}

// checkRepetition 检查重复性
func (s *QualityService) checkRepetition(chapter *model.Chapter) []QualityIssue {
	issues := []QualityIssue{}

	// 检查重复词汇
	words := []string{"突然", "然后", "接着"}
	for _, word := range words {
		count := strings.Count(chapter.Content, word)
		if count > 5 {
			issues = append(issues, QualityIssue{
				Type:        "repetition",
				Severity:    "low",
				Description: fmt.Sprintf("「%s」一词出现了%d次", word, count),
				Location:    "全文",
				Suggestion:  "建议使用同义词替换以增加表达多样性",
			})
		}
	}

	return issues
}

// generateSuggestions 生成建议
func (s *QualityService) generateSuggestions(issues []QualityIssue) []string {
	suggestions := []string{}

	highCount := 0
	for _, issue := range issues {
		if issue.Severity == "high" {
			highCount++
		}
	}

	if highCount > 0 {
		suggestions = append(suggestions, fmt.Sprintf("有%d个高优先级问题需要修复", highCount))
	}

	if len(issues) > 10 {
		suggestions = append(suggestions, "章节存在较多问题，建议整体重写或大幅修改")
	}

	if len(suggestions) == 0 {
		suggestions = append(suggestions, "章节质量良好，无需特别修改")
	}

	return suggestions
}

// VideoService 视频服务
type VideoService struct {
	videoRepo          *repository.VideoRepository
	storyboardRepo     *repository.StoryboardRepository
	chapterRepo        *repository.ChapterRepository
	characterRepo      *repository.CharacterRepository
	novelRepo          *repository.NovelRepository
	tenantRepo         *repository.TenantRepository
	aiService          *AIService
	videoProviders     map[string]ai.VideoProvider
	consistencyService *CharacterConsistencyService
	bgmService         *BGMService
	sfxService         *SFXService
	storageSvc         storage.Service
	sceneAnchorSvc     *SceneAnchorService
	plotPointRepo      *repository.PlotPointRepository
	systemSettingRepo  *repository.SystemSettingRepository
	videoSem           chan struct{} // nil = unlimited; set via WithVideoConcurrency
	videoSemMu         sync.RWMutex // protects videoSem replacement
}

// GetNovelByID 通过 novelRepo 加载小说（供 handler 传递给 CapCutService 等下游服务）
func (s *VideoService) GetNovelByID(id uint) (*model.Novel, error) {
	return s.novelRepo.GetByID(id)
}

// ffmpegBin 返回 FFmpeg 可执行文件路径（优先读系统配置，fallback 到 PATH 中的 ffmpeg）
func (s *VideoService) ffmpegBin() string {
	if s.systemSettingRepo != nil {
		if p, err := s.systemSettingRepo.Get("ffmpeg_path"); err == nil && p != "" {
			return p
		}
	}
	return "ffmpeg"
}

func (s *VideoService) WithSystemSettingRepo(r *repository.SystemSettingRepository) *VideoService {
	s.systemSettingRepo = r
	return s
}

// WithVideoConcurrency 设置视频生成的最大并发数（启动时调用）。
func (s *VideoService) WithVideoConcurrency(n int) *VideoService {
	if n > 0 {
		s.videoSem = make(chan struct{}, n)
	}
	return s
}

// SetVideoConcurrency 运行时动态调整视频并发度（线程安全）。
func (s *VideoService) SetVideoConcurrency(n int) {
	s.videoSemMu.Lock()
	defer s.videoSemMu.Unlock()
	if n > 0 {
		s.videoSem = make(chan struct{}, n)
	} else {
		s.videoSem = nil
	}
}

func (s *VideoService) WithSceneAnchorService(svc *SceneAnchorService) *VideoService {
	s.sceneAnchorSvc = svc
	return s
}

func (s *VideoService) WithPlotPointRepo(repo *repository.PlotPointRepository) *VideoService {
	s.plotPointRepo = repo
	return s
}

func NewVideoService(
	videoRepo *repository.VideoRepository,
	storyboardRepo *repository.StoryboardRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	novelRepo *repository.NovelRepository,
	tenantRepo *repository.TenantRepository,
	aiService *AIService,
	videoProviders map[string]ai.VideoProvider,
) *VideoService {
	return &VideoService{
		videoRepo:      videoRepo,
		storyboardRepo: storyboardRepo,
		chapterRepo:    chapterRepo,
		characterRepo:  characterRepo,
		novelRepo:      novelRepo,
		tenantRepo:     tenantRepo,
		aiService:      aiService,
		videoProviders: videoProviders,
	}
}

// WithConsistencyService 设置一致性服务（选填）
func (s *VideoService) WithConsistencyService(cs *CharacterConsistencyService) *VideoService {
	s.consistencyService = cs
	return s
}

// WithBGMService 设置BGM服务（选填）
func (s *VideoService) WithBGMService(bgm *BGMService) *VideoService {
	s.bgmService = bgm
	return s
}

// WithSFXService 设置音效服务（选填）
func (s *VideoService) WithSFXService(sfx *SFXService) *VideoService {
	s.sfxService = sfx
	return s
}

// WithStorage 注入媒体存储服务（选填；配置 OSS 时上传至 OSS，否则存 DB）
func (s *VideoService) WithStorage(svc storage.Service) *VideoService {
	s.storageSvc = svc
	return s
}

// CreateVideoFromChapter 从章节创建视频
func (s *VideoService) CreateVideoFromChapter(novelID uint, chapterID *uint) (*model.Video, error) {
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   chapterID,
		Title:       "新视频",
		Status:      "planning",
		FrameRate:   24,
		Resolution:  "1080p",
		AspectRatio: "16:9",
	}
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		video.TenantID = novel.TenantID
	}

	if err := s.videoRepo.Create(video); err != nil {
		return nil, err
	}

	return video, nil
}

// CreateVideo 创建视频（接受请求对象）
func (s *VideoService) CreateVideo(novelID uint, req *model.CreateVideoRequest) (*model.Video, error) {
	return s.CreateVideoFromReq(novelID, req)
}

// GenerateStoryboard 生成分镜
// userPrompt: 用户自定义提示词（可选），将追加到系统 prompt 之后
// progressFn: 可选的进度回调（0-99），供调用方更新任务进度（传 nil 则跳过）
func (s *VideoService) GenerateStoryboard(videoID uint, provider, userPrompt string, progressFn func(int), chapterIDOverride ...*uint) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}

	// 允许调用方覆盖 chapterID（解决 StoryboardService 忽略 chapterID 的问题）
	chapterID := video.ChapterID
	if len(chapterIDOverride) > 0 && chapterIDOverride[0] != nil {
		chapterID = chapterIDOverride[0]
		// 同步更新 video 记录，保持一致性
		video.ChapterID = chapterID
		s.videoRepo.Update(video) //nolint:errcheck
	}

	var content string
	if chapterID != nil {
		chapter, _ := s.chapterRepo.GetByID(*chapterID)
		if chapter != nil {
			content = chapter.Content
		}
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("章节内容为空，请先在「写作」页面编写章节内容再生成分镜脚本")
	}

	// 获取租户 ID（供 getTenantProvider 查租户私有配置）
	var tenantID uint
	if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
		tenantID = novel.TenantID
	}

	// 预取所有段落共用的静态数据，避免 N×3 次重复 DB 查询
	var characters []*model.Character
	if video.NovelID > 0 {
		characters, _ = s.characterRepo.ListByNovel(video.NovelID)
	}
	var anchors []*model.SceneAnchor
	if s.sceneAnchorSvc != nil && video.NovelID > 0 {
		anchors, _ = s.sceneAnchorSvc.ListByNovel(video.NovelID)
	}
	var plotPoints []*model.PlotPoint
	if s.plotPointRepo != nil {
		if chapterID != nil {
			plotPoints, _ = s.plotPointRepo.ListByChapter(*chapterID)
		}
		if len(plotPoints) == 0 && video.NovelID > 0 {
			plotPoints, _ = s.plotPointRepo.ListByNovel(video.NovelID, "", true)
		}
	}

	// 分段生成：长章节按 2000 字切割，每段独立调用 AI，合并后顺序重编号。
	// 短章节（≤2000 字）等同于原单段调用路径，行为完全一致。
	const segmentMaxRunes = 2000
	segments := splitContentSegments(content, segmentMaxRunes)

	totalRunes := len([]rune(content))
	totalShots := calcTotalShots(totalRunes, video.TargetDuration, video.Pacing)

	// 顺序处理各段落，传递上段末尾分镜作为情节连贯上下文。
	// 顺序处理是确保跨段连贯的必要条件：每段的 prompt 依赖上段的生成结果。
	var allShots []*model.StoryboardShot
	var prevTailShots []*model.StoryboardShot // 上一段末尾 N 个分镜，用于连贯上下文
	shotCounter := 0
	for segIdx, seg := range segments {
		segRunes := len([]rune(seg))
		segShotCount := totalShots * segRunes / max(totalRunes, 1)
		if segShotCount < 3 {
			segShotCount = 3
		}
		prompt := s.buildStoryboardPrompt(video, seg, userPrompt, segIdx+1, len(segments), segShotCount, characters, anchors, plotPoints, prevTailShots)
		var result string
		var aiErr error
		// 最多重试 2 次：仅针对 JSON 格式错误重试，超时直接 fail-fast。
		for attempt := 0; attempt < 3; attempt++ {
			p := prompt
			if attempt > 0 {
				p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON 数组，不要包含任何 markdown 代码块（```）或说明文字。"
				log.Printf("GenerateStoryboard: segment %d/%d retry attempt %d", segIdx+1, len(segments), attempt)
			}
			result, aiErr = s.aiService.GenerateWithProvider(tenantID, video.NovelID, "storyboard", p, provider)
			if aiErr == nil && strings.TrimSpace(result) != "" {
				break
			}
			if aiErr != nil {
				log.Printf("GenerateStoryboard: segment %d/%d attempt %d AI error: %v", segIdx+1, len(segments), attempt, aiErr)
				if ai.IsTimeoutError(aiErr) {
					break
				}
			}
		}
		if aiErr != nil {
			if segIdx == 0 {
				return nil, aiErr
			}
			log.Printf("GenerateStoryboard: segment %d/%d failed (non-fatal): %v", segIdx+1, len(segments), aiErr)
			continue
		}
		segShots, parseErr := s.parseStoryboardResult(videoID, chapterID, result)
		if parseErr != nil {
			if segIdx == 0 {
				return nil, fmt.Errorf("解析AI分镜结果失败: %w", parseErr)
			}
			log.Printf("GenerateStoryboard: segment %d/%d parse failed (non-fatal): %v", segIdx+1, len(segments), parseErr)
			continue
		}
		// 全局顺序重编号，保证跨段 shot_no 连续且唯一
		for _, shot := range segShots {
			shotCounter++
			shot.ShotNo = shotCounter
		}
		allShots = append(allShots, segShots...)
		// 保存本段末尾 3 个分镜，供下一段的连贯上下文使用
		tailCount := 3
		if len(segShots) < tailCount {
			tailCount = len(segShots)
		}
		prevTailShots = segShots[len(segShots)-tailCount:]
		// 进度回调：每段完成后按比例上报（最多到 90%，Complete 时才到 100%）
		if progressFn != nil {
			pct := (segIdx + 1) * 90 / len(segments)
			progressFn(pct)
		}
	}

	if len(allShots) == 0 {
		return nil, fmt.Errorf("所有段落均未能生成分镜，请检查章节内容或重试")
	}
	shots := allShots

	// 场景锚点自动匹配：按 shot.Location 名称匹配已注册的场景锚点
	if s.sceneAnchorSvc != nil {
		s.autoMatchShotAnchors(shots, anchors)
	}
	// 角色自动关联：按 shot.Characters JSON 中的名称匹配小说角色
	s.autoMatchShotCharacters(shots, characters)

	// 删除旧分镜，再批量插入新分镜（单次 SQL，避免 N 次往返）
	if err := s.storyboardRepo.DeleteByVideoID(videoID); err != nil {
		return nil, err
	}
	if err := s.storyboardRepo.BatchCreate(shots); err != nil {
		return nil, fmt.Errorf("保存分镜失败: %w", err)
	}

	// 更新视频状态
	video.TotalShots = len(shots)
	video.Status = "storyboard"
	s.videoRepo.Update(video)

	return shots, nil
}

// autoMatchShotAnchors 按场景名称自动将分镜绑定到场景锚点（模糊匹配 scene.location）
// 这样无需前端手动调用 SetShotAnchor，锚点注入即可在视频生成时自动生效。
// anchors 为调用方预取的数据（避免重复查 DB）。
func (s *VideoService) autoMatchShotAnchors(shots []*model.StoryboardShot, anchors []*model.SceneAnchor) {
	if len(anchors) == 0 {
		return
	}
	// 构建名称→ID映射（小写，方便模糊匹配）
	anchorMap := make(map[string]uint, len(anchors))
	for _, a := range anchors {
		anchorMap[strings.ToLower(a.Name)] = a.ID
	}
	for _, shot := range shots {
		if shot.SceneAnchorID != nil {
			continue // 已手动绑定，不覆盖
		}
		// shot.Scene 是 JSON: {"location":"...","time_of_day":"..."}
		loc := extractLocationFromScene(shot.Scene)
		if loc == "" {
			// 降级：从 Description 中做关键词匹配
			loc = shot.Description
		}
		loc = strings.ToLower(loc)
		if loc == "" {
			continue
		}
		// 精确匹配优先，其次包含匹配
		if id, ok := anchorMap[loc]; ok {
			id := id
			shot.SceneAnchorID = &id
			continue
		}
		for name, id := range anchorMap {
			if strings.Contains(loc, name) || strings.Contains(name, loc) {
				id := id
				shot.SceneAnchorID = &id
				break
			}
		}
	}
}

// autoMatchShotCharacters 按 shot.Characters JSON 中的名称匹配小说角色，写入 CharacterIDs
// 已有 CharacterIDs 时不覆盖（保留手动绑定结果）。
// chars 为调用方预取的数据（避免重复查 DB）。
func (s *VideoService) autoMatchShotCharacters(shots []*model.StoryboardShot, chars []*model.Character) {
	if len(chars) == 0 {
		return
	}
	// 构建 小写名→ID map
	nameMap := make(map[string]uint, len(chars))
	for _, c := range chars {
		nameMap[strings.ToLower(c.Name)] = c.ID
	}
	for _, shot := range shots {
		if len(shot.CharacterIDs) > 0 {
			continue // 已手动绑定，不覆盖
		}
		// shot.Characters = JSON array: [{"name":"...","expression":"...","pose":"..."}]
		var shotChars []struct {
			Name string `json:"name"`
		}
		if shot.Characters == "" {
			continue
		}
		if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err != nil {
			continue
		}
		var matched model.JSONUintSlice
		seen := make(map[uint]bool)
		for _, sc := range shotChars {
			nameLower := strings.ToLower(sc.Name)
			if id, ok := nameMap[nameLower]; ok {
				if !seen[id] {
					matched = append(matched, id)
					seen[id] = true
				}
				continue
			}
			// 模糊匹配：子串包含 + 汉字字符级重叠（阈值 50%，适配"萧炎"vs"炎少"等写法）
			const runeOverlapThreshold = 0.5
			for name, id := range nameMap {
				if strings.Contains(nameLower, name) || strings.Contains(name, nameLower) ||
					charRuneOverlap(nameLower, name) >= runeOverlapThreshold {
					if !seen[id] {
						matched = append(matched, id)
						seen[id] = true
					}
					break
				}
			}
		}
		if len(matched) > 0 {
			shot.CharacterIDs = matched
		}
	}
}

// charRuneOverlap 返回两个字符串的汉字级重叠比例（以较短串为分母）。
// 用于模糊角色名匹配，如"萧炎"vs"炎少"（"炎"重叠 → 0.5，超过阈值即视为同一角色）。
func charRuneOverlap(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	shorter, longer := ra, rb
	if len(ra) > len(rb) {
		shorter, longer = rb, ra
	}
	longerSet := make(map[rune]struct{}, len(longer))
	for _, r := range longer {
		longerSet[r] = struct{}{}
	}
	overlap := 0
	for _, r := range shorter {
		if _, ok := longerSet[r]; ok {
			overlap++
		}
	}
	return float64(overlap) / float64(len(shorter))
}

// calcTotalShots 按目标时长+节奏计算全章期望分镜总数。
// targetDuration=0 时降级为字数密度估算（向后兼容）。
func calcTotalShots(totalRunes, targetDuration int, pacing string) int {
	if targetDuration > 0 {
		avg := map[string]int{"slow": 8, "normal": 5, "fast": 3}
		s, ok := avg[pacing]
		if !ok {
			s = 5
		}
		n := targetDuration / s
		if n < 3 {
			n = 3
		}
		return n
	}
	// 降级：字数估算
	n := totalRunes / 280
	if n < 5 {
		n = 5
	}
	if n > 60 {
		n = 60
	}
	return n
}

// splitContentSegments 按段落边界切割长文本，每段最多 maxRunes 个字符。
// 若内容不超过 maxRunes，直接返回单段切片。
// 切割优先在双换行（段落）处断开，其次在单换行处断开，保证分镜上下文完整。
func splitContentSegments(content string, maxRunes int) []string {
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return []string{content}
	}
	var segments []string
	start := 0
	for start < len(runes) {
		end := start + maxRunes
		if end >= len(runes) {
			segments = append(segments, string(runes[start:]))
			break
		}
		// 在最后 20% 区间内找段落边界：优先双换行，次选单换行
		boundary := -1
		searchFrom := end - maxRunes/5
		for i := end; i >= searchFrom; i-- {
			if runes[i] == '\n' {
				if i > 0 && runes[i-1] == '\n' {
					boundary = i + 1 // 双换行后断开
					break
				}
				if boundary < 0 {
					boundary = i + 1 // 先记录单换行，继续找双换行
				}
			}
		}
		if boundary > start {
			end = boundary
		}
		segments = append(segments, string(runes[start:end]))
		start = end
	}
	return segments
}

// extractLocationFromScene 从 shot.Scene JSON 中提取 location 字符串
func extractLocationFromScene(sceneJSON string) string {
	if sceneJSON == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(sceneJSON), &m); err != nil {
		return ""
	}
	return m["location"]
}

// buildStoryboardPrompt 构建分镜提示词（含截断保护、角色信息、段落上下文）
// segNo/totalSegs 为分段编号（从 1 开始），单段调用时传 1, 1。
// expectedShots 为本段期望分镜数，由调用方通过 calcTotalShots 计算后传入。
// characters/anchors/plotPoints 为调用方预取的数据（避免每段重复查 DB）。
// buildStoryboardPrompt 构建分镜生成 Prompt。
// prevShots: 上一段落末尾的 N 个分镜，用于跨段落情节连贯（顺序处理时传入，并发时为 nil）。
func (s *VideoService) buildStoryboardPrompt(
	video *model.Video, content, userPrompt string,
	segNo, totalSegs, expectedShots int,
	characters []*model.Character, anchors []*model.SceneAnchor, plotPoints []*model.PlotPoint,
	prevShots []*model.StoryboardShot,
) string {
	var sb strings.Builder

	segLabel := ""
	if totalSegs > 1 {
		segLabel = fmt.Sprintf("（第%d段，共%d段）", segNo, totalSegs)
	}
	sb.WriteString(fmt.Sprintf("你是专业影视分镜师，同时负责撰写旁白文案。请将以下章节内容%s转化为约%d个分镜，返回JSON数组。\n\n", segLabel, expectedShots))

	// ── 字段规范（核心约束，AI 必须严格遵守）──────────────────────────
	sb.WriteString(`【输出字段规范——严格遵守，违反规范将导致输出无法使用】
▸ description（英文画面描述）
  - 仅用于 AI 图片生成的英文视觉提示词
  - 描述：主体（人物/物体）、场景环境、构图、光线氛围
  - 禁止出现中文、叙事句子、心理描写

▸ narration（中文旁白文案）——每镜必填，不得为空
  - 观众"听到"的旁白内容，第三人称叙事视角，语言生动凝练
  - 严禁出现以下词汇：镜头、画面、特写、推进、切换、转场、画外音、摄影机、定格
  - ✅ 正确示例："凌云握紧长剑，眼中燃起不灭的复仇之火。"
  - ❌ 错误示例："镜头推近，画面聚焦在凌云手握长剑的特写上。"
  - ❌ 错误示例："画面展示凌云愤怒的表情，镜头缓缓拉远。"

▸ dialogue（角色台词）——仅当角色实际开口说话时填写
  - 格式固定："角色名：台词内容"（如"凌云：你敢再说一遍！"）
  - 无对话时必须为空字符串 ""，禁止填入旁白或镜头描述

`)

	// ── 连续性上下文（跨段落情节连贯）──────────────────────────────
	if len(prevShots) > 0 {
		sb.WriteString("【上一段末尾分镜——本段首镜须与其情节自然衔接】\n")
		for _, ps := range prevShots {
			narr := ps.Narration
			if narr == "" {
				narr = ps.Description
			}
			sb.WriteString(fmt.Sprintf("  第%d镜 旁白：%s", ps.ShotNo, narr))
			if ps.Dialogue != "" {
				sb.WriteString(fmt.Sprintf(" ／ 台词：%s", ps.Dialogue))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// ── 角色外貌信息（仅注入当前段落提及的角色）──────────────────
	if len(characters) > 0 {
		contentLower := strings.ToLower(content)
		var matched []*model.Character
		for _, c := range characters {
			if strings.Contains(contentLower, strings.ToLower(c.Name)) {
				matched = append(matched, c)
			}
		}
		if len(matched) == 0 {
			for _, c := range characters {
				if c.Role == "protagonist" {
					matched = append(matched, c)
				}
			}
		}
		if len(matched) > 0 {
			sb.WriteString("【角色外貌（description 中须保持一致）】\n")
			for _, c := range matched {
				sb.WriteString(fmt.Sprintf("- %s（%s）", c.Name, c.Role))
				if c.Appearance != "" {
					sb.WriteString(fmt.Sprintf("：%s", c.Appearance))
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	// ── 场景锚点（仅注入当前段落提及的锚点）──────────────────────
	if len(anchors) > 0 {
		contentLower := strings.ToLower(content)
		var matchedAnchors []*model.SceneAnchor
		for _, a := range anchors {
			if strings.Contains(contentLower, strings.ToLower(a.Name)) {
				matchedAnchors = append(matchedAnchors, a)
			}
		}
		if len(matchedAnchors) == 0 && len(anchors) > 0 {
			limit := 5
			if len(anchors) < limit {
				limit = len(anchors)
			}
			matchedAnchors = anchors[:limit]
		}
		if len(matchedAnchors) > 8 {
			matchedAnchors = matchedAnchors[:8]
		}
		if len(matchedAnchors) > 0 {
			sb.WriteString("【已命名场景（location 字段请从以下名称中选择）】\n")
			for _, a := range matchedAnchors {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", a.Name, a.Description))
			}
			sb.WriteString("\n")
		}
	}

	// ── 剧情线（提示叙事张力）──────────────────────────────────
	if len(plotPoints) > 0 {
		sb.WriteString("【本章剧情线（旁白须体现叙事张力）】\n")
		maxPP := 5
		if len(plotPoints) < maxPP {
			maxPP = len(plotPoints)
		}
		for i := 0; i < maxPP; i++ {
			sb.WriteString(fmt.Sprintf("- [%s] %s\n", plotPoints[i].Type, plotPoints[i].Description))
		}
		sb.WriteString("\n")
	}

	// ── 章节内容 ──────────────────────────────────────────────
	if content != "" {
		sb.WriteString("【章节内容】\n")
		if len(content) > 10000 {
			content = content[:10000] + "…（已截断）"
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// ── 用户附加要求 ──────────────────────────────────────────
	if userPrompt != "" {
		sb.WriteString("【用户要求】\n")
		sb.WriteString(userPrompt)
		sb.WriteString("\n\n")
	}

	// ── 输出格式 ──────────────────────────────────────────────
	sb.WriteString(fmt.Sprintf(`请将章节内容分解为约%d个分镜（每个重大动作、对话轮次、场景切换须单独成镜）。
只返回JSON数组，不要任何额外说明或markdown代码块：
[
  {
    "shot_no": 1,
    "description": "English visual prompt for image generation only, no Chinese",
    "narration": "中文旁白（必填，严禁镜头语言）",
    "dialogue": "角色名：台词（无对话则为空字符串）",
    "camera_type": "static|pan|zoom|tracking|dolly|crane",
    "camera_angle": "eye_level|high|low|dutch|overhead|POV",
    "shot_size": "extreme_wide|wide|full|medium|close_up|extreme_close_up",
    "duration": 5.0,
    "location": "场景地点",
    "time_of_day": "dawn|morning|afternoon|evening|night",
    "weather": "clear|cloudy|rainy|snowy|foggy",
    "lighting": "natural|dramatic|soft|backlit",
    "characters": [{"name":"角色名","expression":"表情","pose":"姿势动作"}],
    "transition": "cut|fade|dissolve|wipe"
  }
]`, expectedShots))

	return sb.String()
}

// parseStoryboardResult 解析AI分镜响应。解析失败时返回 error（不生成空占位）。
func (s *VideoService) parseStoryboardResult(videoID uint, chapterID *uint, result string) ([]*model.StoryboardShot, error) {
	// 提取 JSON 数组
	cleaned := extractJSON(result)

	var rawShots []struct {
		ShotNo      int     `json:"shot_no"`
		Description string  `json:"description"` // 英文画面描述（供图片/视频生成）
		Narration   string  `json:"narration"`   // 中文旁白文案（供TTS和字幕）
		Dialogue    string  `json:"dialogue"`    // 角色台词（格式："角色名：台词"）
		CameraType  string  `json:"camera_type"`
		CameraAngle string  `json:"camera_angle"`
		ShotSize    string  `json:"shot_size"`
		Duration    float64 `json:"duration"`
		Location    string  `json:"location"`
		TimeOfDay   string  `json:"time_of_day"`
		Weather     string  `json:"weather"`
		Lighting    string  `json:"lighting"`
		Characters  []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
			Pose       string `json:"pose"`
		} `json:"characters"`
		Transition string `json:"transition"`
	}

	if err := json.Unmarshal([]byte(cleaned), &rawShots); err != nil || len(rawShots) == 0 {
		log.Printf("[VideoService] parseStoryboardResult: JSON parse failed (%v); raw AI response (first 500 chars): %.500s", err, result)
		if err != nil {
			return nil, fmt.Errorf("分镜JSON解析失败: %w; AI原始响应(前200字符): %.200s", err, result)
		}
		return nil, fmt.Errorf("AI返回了空的分镜列表，请检查章节内容或重试")
	}

	shots := make([]*model.StoryboardShot, 0, len(rawShots))
	for i, r := range rawShots {
		shotNo := r.ShotNo
		if shotNo == 0 {
			shotNo = i + 1
		}
		duration := r.Duration
		if duration <= 0 {
			duration = 5.0
		}

		// 将角色信息序列化为 JSON 存储
		var charsJSON string
		if len(r.Characters) > 0 {
			if b, err := json.Marshal(r.Characters); err == nil {
				charsJSON = string(b)
			}
		}

		// 将场景配置序列化
		scene := map[string]string{
			"location":    r.Location,
			"time_of_day": r.TimeOfDay,
			"weather":     r.Weather,
			"lighting":    r.Lighting,
		}
		var sceneJSON string
		if b, err := json.Marshal(scene); err == nil {
			sceneJSON = string(b)
		}

		// Prompt 用 Description 填充（供视频生成接口使用）
		// 附加摄像机和场景信息以丰富 prompt
		prompt := r.Description
		if r.CameraAngle != "" || r.ShotSize != "" {
			prompt = fmt.Sprintf("%s, %s shot, %s angle", r.Description, r.ShotSize, r.CameraAngle)
		}
		if r.Lighting != "" {
			prompt += ", " + r.Lighting + " lighting"
		}

		shot := &model.StoryboardShot{
			UUID:        uuid.New().String(),
			VideoID:     videoID,
			ChapterID:   chapterID,
			ShotNo:      shotNo,
			Description: r.Description,
			Narration:   r.Narration,
			Prompt:      prompt,
			Dialogue:    r.Dialogue,
			CameraType:  validCameraType(r.CameraType),
			CameraAngle: validCameraAngle(r.CameraAngle),
			ShotSize:    validShotSize(r.ShotSize),
			Duration:    duration,
			Characters:  charsJSON,
			Scene:       sceneJSON,
			Status:      "pending",
		}
		shots = append(shots, shot)
	}
	return shots, nil
}

// validCameraType 验证摄像机类型，无效时返回默认值
func validCameraType(t string) string {
	valid := map[string]bool{"static": true, "pan": true, "zoom": true, "tracking": true, "dolly": true, "crane": true}
	if valid[t] {
		return t
	}
	return "static"
}

// validCameraAngle 验证摄像机角度
func validCameraAngle(a string) string {
	valid := map[string]bool{"eye_level": true, "high": true, "low": true, "dutch": true, "overhead": true, "POV": true}
	if valid[a] {
		return a
	}
	return "eye_level"
}

// validShotSize 验证镜头尺寸
func validShotSize(s string) string {
	valid := map[string]bool{"extreme_wide": true, "wide": true, "full": true, "medium": true, "close_up": true, "extreme_close_up": true}
	if valid[s] {
		return s
	}
	return "medium"
}

// CreateVideo handler-compatible wrapper
func (s *VideoService) CreateVideoFromReq(novelID uint, req *model.CreateVideoRequest) (*model.Video, error) {
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   req.ChapterID,
		Title:       req.Title,
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		AspectRatio: req.AspectRatio,
		ArtStyle:    req.ArtStyle,
		QualityTier: req.QualityTier,
		Mode:        req.Mode,
		Status:      "planning",
	}
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		video.TenantID = novel.TenantID
		// 默认画面风格：继承项目设置中的画面风格
		if video.ArtStyle == "" && novel.ImageStyle != "" {
			video.ArtStyle = novel.ImageStyle
		}
	}
	if video.FrameRate == 0 {
		video.FrameRate = 24
	}
	if video.Resolution == "" {
		video.Resolution = "1080p"
	}
	if video.AspectRatio == "" {
		video.AspectRatio = "16:9"
	}
	if video.QualityTier == "" {
		video.QualityTier = "preview"
	}
	if video.Mode == "" {
		video.Mode = "slideshow"
	}
	return video, s.videoRepo.Create(video)
}

// GetVideo 获取视频（内部调用，无租户隔离）
func (s *VideoService) GetVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetByID(id)
}

// GetVideoByTenant 获取视频（带租户隔离，防止越权访问）
func (s *VideoService) GetVideoByTenant(id, tenantID uint) (*model.Video, error) {
	return s.videoRepo.GetByIDAndTenant(id, tenantID)
}

// ListVideos 获取视频列表
func (s *VideoService) ListVideos(novelId *uint, chapterID *uint, status string, page, pageSize int) ([]*model.Video, int, error) {
	videos, total, err := s.videoRepo.List(novelId, chapterID, page, pageSize)
	return videos, int(total), err
}

// UpdateVideo 更新视频
func (s *VideoService) UpdateVideo(id uint, req *model.UpdateVideoRequest) (*model.Video, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		video.Title = req.Title
	}
	if req.Resolution != "" {
		video.Resolution = req.Resolution
	}
	if req.FrameRate != 0 {
		video.FrameRate = req.FrameRate
	}
	if req.AspectRatio != "" {
		video.AspectRatio = req.AspectRatio
	}
	if req.ArtStyle != "" {
		video.ArtStyle = req.ArtStyle
	}
	if req.ScriptStatus != "" {
		video.ScriptStatus = req.ScriptStatus
	}
	if req.Mode != "" {
		video.Mode = req.Mode
	}
	return video, s.videoRepo.Update(video)
}

// UpdatePacingConfig 更新视频的节奏和目标时长配置（供分镜生成前调用）
func (s *VideoService) UpdatePacingConfig(id uint, pacing string, targetDuration int) error {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return err
	}
	if pacing != "" {
		video.Pacing = pacing
	}
	video.TargetDuration = targetDuration
	return s.videoRepo.Update(video)
}

// DeleteVideo 删除视频
func (s *VideoService) DeleteVideo(id uint) error {
	return s.videoRepo.DeleteByID(id)
}

// StartGeneration 开始生成视频（调用真实视频 Provider）
func (s *VideoService) StartGeneration(id uint) (string, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return "", err
	}

	// 租户状态校验
	if err := s.checkTenantAccess(video.NovelID); err != nil {
		video.Status = "failed"
		video.ErrorMessage = err.Error()
		s.videoRepo.Update(video)
		return "", err
	}

	// 选择 provider：优先 kling，其次 seedance，均无则返回错误
	providerName := "kling"
	provider, ok := s.videoProviders[providerName]
	if !ok {
		providerName = "seedance"
		provider, ok = s.videoProviders[providerName]
	}
	if !ok {
		// 无可用 provider：标记失败并返回错误
		video.Status = "failed"
		video.ErrorMessage = "no video provider configured (set KLING_API_KEY or SEEDANCE_API_KEY)"
		s.videoRepo.Update(video)
		return "", fmt.Errorf("no video provider configured")
	}

	// 构建生成请求
	req := &ai.VideoGenerateRequest{
		Prompt:      fmt.Sprintf("%s — cinematic, high quality", video.Title),
		AspectRatio: video.AspectRatio,
		Duration:    5.0,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		video.Status = "failed"
		video.ErrorMessage = err.Error()
		video.RetryCount++
		s.videoRepo.Update(video)
		return "", fmt.Errorf("video generation start failed: %w", err)
	}

	// 持久化任务 ID 和 provider 信息
	video.Status = "generating"
	video.ProviderName = providerName
	video.TaskID = task.TaskID
	video.ErrorMessage = ""
	s.videoRepo.Update(video)

	return task.TaskID, nil
}

// VideoProvider is a minimal video provider descriptor used by the listing endpoint.
type VideoProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// ListVideoProviders returns configured video providers with display names.
func (s *VideoService) ListVideoProviders() []VideoProvider {
	result := make([]VideoProvider, 0, len(s.videoProviders))
	for name := range s.videoProviders {
		result = append(result, VideoProvider{Name: name, DisplayName: capableProviderDisplayName(name, "")})
	}
	return result
}

// GetStoryboard 获取分镜列表
func (s *VideoService) GetStoryboard(videoID uint) ([]*model.StoryboardShot, error) {
	return s.storyboardRepo.ListByVideo(videoID)
}

// ReviewStoryboard 调用 AI 对分镜脚本进行专业审查，返回结构化报告。
func (s *VideoService) ReviewStoryboard(videoID uint, provider string) (*model.StoryboardReview, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return nil, fmt.Errorf("获取分镜失败: %w", err)
	}
	if len(shots) == 0 {
		return nil, fmt.Errorf("该视频暂无分镜，请先生成分镜脚本")
	}

	prompt := buildStoryboardReviewPrompt(shots)

	result, err := s.aiService.Generate(0, provider, prompt)
	if err != nil {
		return nil, fmt.Errorf("AI审查失败: %w", err)
	}

	return parseStoryboardReview(result)
}

// buildStoryboardReviewPrompt 构建分镜审查提示词
func buildStoryboardReviewPrompt(shots []*model.StoryboardShot) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("你是资深影视分镜审查师，请从专业角度对以下%d个分镜脚本进行全面审查，返回JSON格式的审查报告。\n\n", len(shots)))

	sb.WriteString(`【审查标准】
1. 叙事连贯性（30分）：故事逻辑、场景衔接、情节推进是否自然流畅
2. 视觉多样性（20分）：景别、镜头类型、角度是否合理变化，避免长时间单调重复
3. 节奏控制（20分）：单镜时长分配是否匹配场景强度，快慢张弛有度
4. 旁白质量（20分）：旁白是否生动感人、避免镜头语言、符合叙事视角、无重复
5. 整体专业度（10分）：构图合理性、转场设计、角色调度

【分镜数据】
`)

	for _, shot := range shots {
		narr := shot.Narration
		if narr == "" {
			narr = shot.Description
		}
		// 截断过长的旁白
		if len([]rune(narr)) > 80 {
			runes := []rune(narr)
			narr = string(runes[:80]) + "…"
		}
		sb.WriteString(fmt.Sprintf("[镜%d] 景别:%s 时长:%.0fs 镜头:%s/%s",
			shot.ShotNo, shot.ShotSize, shot.Duration, shot.CameraType, shot.CameraAngle))
		if narr != "" {
			sb.WriteString(fmt.Sprintf(" 旁白:\"%s\"", narr))
		}
		if shot.Dialogue != "" {
			d := shot.Dialogue
			if len([]rune(d)) > 40 {
				d = string([]rune(d)[:40]) + "…"
			}
			sb.WriteString(fmt.Sprintf(" 台词:\"%s\"", d))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(`
【输出格式】
返回JSON对象，不要包含markdown代码块或任何额外说明：
{
  "overall_score": 7.5,
  "narrative_score": 8.0,
  "visual_score": 6.5,
  "pacing_score": 7.0,
  "narration_score": 8.0,
  "summary": "总体评价（100字以内）",
  "strengths": ["亮点描述1", "亮点描述2"],
  "weaknesses": ["主要问题描述1", "主要问题描述2"],
  "global_suggestions": ["整体改进建议1", "整体改进建议2", "整体改进建议3"],
  "shot_feedback": [
    {
      "shot_no": 3,
      "issues": ["旁白包含镜头语言：'镜头推近'"],
      "suggestion": "改为：'凌云眼神逐渐坚定，握紧了手中的剑'",
      "severity": "warning"
    }
  ]
}
注意：shot_feedback 只需列出有问题的镜头（建议不超过10个最典型的），无问题镜头无需列出。
severity 取值：error（严重，直接影响质量）、warning（中等，建议修改）、info（轻微，可选优化）`)

	return sb.String()
}

// parseStoryboardReview 解析 AI 审查结果为结构化报告
func parseStoryboardReview(result string) (*model.StoryboardReview, error) {
	cleaned := extractJSON(result)

	var review model.StoryboardReview
	if err := json.Unmarshal([]byte(cleaned), &review); err != nil {
		return nil, fmt.Errorf("审查报告解析失败: %w; AI原始响应(前300字符): %.300s", err, result)
	}
	return &review, nil
}

// GetShot 根据 ID 获取单个分镜
func (s *VideoService) GetShot(id uint) (*model.StoryboardShot, error) {
	return s.storyboardRepo.GetByID(id)
}

// UpdateShot 更新分镜
func (s *VideoService) UpdateShot(id uint, req *model.StoryboardShot) (*model.StoryboardShot, error) {
	shot, err := s.storyboardRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.CameraType != "" {
		shot.CameraType = req.CameraType
	}
	if req.CameraAngle != "" {
		shot.CameraAngle = req.CameraAngle
	}
	if req.ShotSize != "" {
		shot.ShotSize = req.ShotSize
	}
	if req.Duration > 0 {
		shot.Duration = req.Duration
	}
	if req.Status != "" {
		shot.Status = req.Status
	}
	if req.GenerationMode != "" {
		shot.GenerationMode = req.GenerationMode
	}
	return shot, s.storyboardRepo.Update(shot)
}

// SetShotCharacters 手动设置分镜的角色绑定
func (s *VideoService) SetShotCharacters(shotID uint, ids []uint) error {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return err
	}
	shot.CharacterIDs = model.JSONUintSlice(ids)
	return s.storyboardRepo.Update(shot)
}

// GenerateSingleShot 触发单个分镜生成（异步）
func (s *VideoService) GetShotByID(videoID, shotID uint) (*model.StoryboardShot, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, err
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("shot %d does not belong to video %d", shotID, videoID)
	}
	return shot, nil
}

func (s *VideoService) GenerateSingleShot(videoID, shotID uint, provider ...string) (*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, err
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("shot %d does not belong to video %d", shotID, videoID)
	}

	// Resolve provider and aspect ratio from novel project config (caller override wins)
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if effectiveProvider == "" && novel.VideoModel != "" {
				effectiveProvider = novel.VideoModel
			}
			if aspectRatio == "" && novel.VideoAspectRatio != "" {
				aspectRatio = novel.VideoAspectRatio
			}
		}
	}

	shot.Status = "generating"
	s.storyboardRepo.Update(shot) //nolint:errcheck
	if video.Mode == "slideshow" {
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	// AI 视频模式：若没有可用的视频提供商，自动降级为图片解说模式
	if len(s.videoProviders) == 0 {
		log.Printf("GenerateSingleShot: no video provider available, falling back to slideshow for shot %d (video %d)", shotID, videoID)
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	return shot, s.GenerateShotVideo(shot, aspectRatio, effectiveProvider)
}

// maxConcurrentShots 限制同时提交给视频提供商的并发数，防止触发 API 429
const maxConcurrentShots = 3

// downloadHTTPClient 用于下载生成的图片/视频文件。
// 设置 5 分钟超时，防止 CDN 接受连接后挂起导致 goroutine 永久阻塞（批量生成卡在 99% 的根本原因）。
var downloadHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// BatchGenerateShots 批量触发指定分镜生成（同步等待所有完成，支持并发限制）
// 图片解说模式(Mode=="slideshow")只生成图片，不生成 Ken Burns 短片。
func (s *VideoService) BatchGenerateShots(videoID uint, shotIDs []uint, qualityTierOverride string, progressFn func(int), provider ...string) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	if qualityTierOverride != "" {
		video.QualityTier = qualityTierOverride
	}

	// Resolve effective provider and aspect ratio from novel config
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if effectiveProvider == "" && novel.VideoModel != "" {
				effectiveProvider = novel.VideoModel
			}
			if aspectRatio == "" && novel.VideoAspectRatio != "" {
				aspectRatio = novel.VideoAspectRatio
			}
		}
	}

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32
	for _, sid := range shotIDs {
		shot, err := s.storyboardRepo.GetByID(sid)
		if err != nil || shot.VideoID != videoID {
			if progressFn != nil && total > 0 {
				pct := int(done.Add(1)) * 99 / total
				progressFn(pct)
			}
			continue
		}
		shot.Status = "generating"
		s.storyboardRepo.Update(shot) //nolint:errcheck
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				if progressFn != nil && total > 0 {
					pct := int(done.Add(1)) * 99 / total
					progressFn(pct)
				}
			}()
			const maxRetries = 3
			var genErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				if video.Mode == "slideshow" || len(s.videoProviders) == 0 {
					genErr = s.GenerateSlideshowShotVideo(sh, aspectRatio)
				} else {
					genErr = s.GenerateShotVideo(sh, aspectRatio, effectiveProvider)
				}
				if genErr == nil {
					break
				}
				log.Printf("BatchGenerateShots: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt*2) * time.Second)
				}
			}
			if genErr != nil {
				log.Printf("BatchGenerateShots: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
			}
		}(shot)
	}
	wg.Wait()
	return queued, nil
}

// GetStatus 获取视频生成状态（从 provider 同步最新进度）
func (s *VideoService) GetStatus(id uint) (interface{}, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	// 超时检查：生成中超过 30 分钟
	if video.Status == "generating" && time.Since(video.UpdatedAt) > 30*time.Minute {
		video.Status = "failed"
		video.ErrorMessage = "generation timed out (>30min)"
		s.videoRepo.Update(video)
	}

	// 自动重试：失败且重试次数 < 3
	if video.Status == "failed" && video.RetryCount < 3 && video.TaskID != "" {
		video.RetryCount++
		video.Status = "generating"
		video.ErrorMessage = ""
		s.videoRepo.Update(video)
		go func() { s.StartGeneration(id) }() //nolint:errcheck
	}

	// 如果有外部任务 ID 且状态为 generating，则同步 provider 状态
	if video.TaskID != "" && video.Status == "generating" && video.ProviderName != "" {
		if provider, ok := s.videoProviders[video.ProviderName]; ok {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			taskStatus, err := provider.GetVideoStatus(ctx, video.TaskID)
			if err == nil {
				video.Progress = taskStatus.Progress
				switch taskStatus.Status {
				case "completed":
					// 获取视频 URL
					if videoURL, urlErr := provider.GetVideoURL(ctx, video.TaskID); urlErr == nil {
						video.VideoPath = videoURL
						video.Status = "completed"
					}
				case "failed":
					video.Status = "failed"
					video.ErrorMessage = taskStatus.Error
				}
				s.videoRepo.Update(video)
			}
		}
	}

	return map[string]interface{}{
		"status":        video.Status,
		"progress":      video.Progress,
		"task_id":       video.TaskID,
		"provider":      video.ProviderName,
		"error_message": video.ErrorMessage,
		"video_url":     video.VideoPath,
	}, nil
}

// checkTenantAccess 校验 novel 关联租户状态
func (s *VideoService) checkTenantAccess(novelID uint) error {
	if s.tenantRepo == nil || s.novelRepo == nil {
		return nil
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil // 不阻塞，让其他逻辑处理
	}
	tenant, err := s.tenantRepo.GetByID(novel.TenantID)
	if err != nil {
		return nil
	}
	if tenant.Status != "active" {
		return fmt.Errorf("tenant account is %s", tenant.Status)
	}
	if tenant.ExpiresAt != nil && !tenant.ExpiresAt.IsZero() && time.Now().After(*tenant.ExpiresAt) {
		return fmt.Errorf("tenant account has expired")
	}
	return nil
}

// generateShotReferenceImage 为分镜生成参考帧图像，返回图片URL和错误。
func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) (string, error) {
	if s.aiService == nil {
		return "", fmt.Errorf("AI service not initialized")
	}

	// 精准匹配：从 shot.CharacterIDs 查找角色参考图
	// 优先级：ThreeViewFront（专为一致性生成）> Portrait（仅脸部，一致性较差）
	// 多角色镜头：扫描全部角色，一旦找到三视图即停止；Portrait 仅作保底（不提前 break）
	var characterPortrait string
	for _, id := range shot.CharacterIDs {
		char, err := s.characterRepo.GetByID(id)
		if err != nil {
			continue
		}
		if char.ThreeViewFront != "" {
			characterPortrait = char.ThreeViewFront
			break // 三视图是最优选择，找到即停
		}
		if char.Portrait != "" && characterPortrait == "" {
			characterPortrait = char.Portrait
			// 不 break：继续查找是否有三视图
		}
	}
	// 降级：通过 ChapterID 找到 NovelID，取第一个有肖像的角色
	if characterPortrait == "" && shot.ChapterID != nil {
		chapter, err := s.chapterRepo.GetByID(*shot.ChapterID)
		if err == nil && chapter != nil {
			chars, err := s.characterRepo.ListByNovel(chapter.NovelID)
			if err == nil {
				for _, c := range chars {
					if c.Portrait != "" {
						characterPortrait = c.Portrait
						break
					}
				}
			}
		}
	}

	promptText := shot.Prompt
	if promptText == "" {
		promptText = shot.Description
	}

	// 场景锚点：注入锁定词，使用锚点参考图替代角色图（布景优先于人物）
	var sceneRefImage string
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, refURL, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil {
			if fragment != "" {
				promptText = fragment + ", " + promptText
			}
			sceneRefImage = refURL
		}
	}
	// 角色三视图/肖像优先；场景锚点参考图仅在无角色参考图时作保底
	// 场景上下文已通过 fragment 注入 promptText，无需用参考图再次覆盖
	refImage := characterPortrait
	if refImage == "" {
		refImage = sceneRefImage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 获取视频的 ArtStyle、TenantID 和角色一致性权重
	artStyle := ""
	var tenantID uint
	charConsistencyWeight := 1.0 // 默认严格一致
	if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
		artStyle = video.ArtStyle
		tenantID = video.TenantID
		if video.NovelID > 0 && s.novelRepo != nil {
			if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
				if tenantID == 0 {
					tenantID = novel.TenantID
				}
				if novel.CharConsistencyWeight > 0 {
					charConsistencyWeight = novel.CharConsistencyWeight
				}
				// 降级：视频未设置画面风格时使用项目设置中的画面风格
				if artStyle == "" && novel.ImageStyle != "" {
					artStyle = novel.ImageStyle
				}
			}
		}
	}

	imageURL, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, "", promptText, refImage, artStyle, "", charConsistencyWeight)
	if err != nil {
		log.Printf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return "", err
	}
	if imageURL == "" {
		log.Printf("generateShotReferenceImage: image gen returned empty URL for shot %d", shot.ShotNo)
		return "", fmt.Errorf("image provider returned empty URL")
	}

	// 首图锁定：场景锚点无参考图时，将本次生成结果存为参考图
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		s.sceneAnchorSvc.AutoSetRefImage(*shot.SceneAnchorID, imageURL) //nolint:errcheck
	}

	return imageURL, nil
}

// resolveArtStyle 返回视频的画面风格：优先用 video.ArtStyle，降级到 novel.ImageStyle
func (s *VideoService) resolveArtStyle(videoID uint) string {
	if s.videoRepo == nil {
		return ""
	}
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return ""
	}
	if video.ArtStyle != "" {
		return video.ArtStyle
	}
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
			return novel.ImageStyle
		}
	}
	return ""
}

// extractLastFrame 使用 FFmpeg 提取视频最后一帧，返回本地 jpeg 路径
func (s *VideoService) extractLastFrame(clipPath string) (string, error) {
	// 处理 file:// 前缀
	localPath := strings.TrimPrefix(clipPath, "file://")

	tmpJpeg := fmt.Sprintf("%s/inkframe-lastframe-%d.jpg", inkframeTempDir(), time.Now().UnixNano())
	cmd := exec.Command(s.ffmpegBin(), "-y",
		"-sseof", "-0.1",
		"-i", localPath,
		"-vframes", "1",
		"-f", "image2",
		tmpJpeg,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extractLastFrame failed: %w\noutput: %s", err, string(out))
	}
	return tmpJpeg, nil
}

// GenerateShotVideo 为单个分镜提交视频生成任务
func (s *VideoService) GenerateShotVideo(shot *model.StoryboardShot, videoAspectRatio string, providerOverride ...string) error {
	// 并发限流：若配置了 video_concurrency，则在此处等待令牌
	s.videoSemMu.RLock()
	vsem := s.videoSem
	s.videoSemMu.RUnlock()
	if vsem != nil {
		vsem <- struct{}{}
		defer func() { <-vsem }()
	}

	providerName := "kling"
	if len(providerOverride) > 0 && providerOverride[0] != "" {
		providerName = providerOverride[0]
	}
	provider, ok := s.videoProviders[providerName]
	if !ok {
		// fallback to any available
		for name, p := range s.videoProviders {
			providerName = name
			provider = p
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("no video provider configured")
	}

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}

	// 始终生成角色一致性参考帧（携带角色三视图），确保批量重新生成时角色参考图不被跳过。
	// 生成成功则覆盖 ReferenceImageURL；失败时降级使用已有的 ReferenceImageURL（非致命）。
	referenceImage := shot.ReferenceImageURL
	frameURL, frameErr := s.generateShotReferenceImage(shot)
	if frameErr != nil {
		log.Printf("GenerateVideoShot: reference image gen failed for shot %d (non-fatal): %v", shot.ShotNo, frameErr)
	}
	if frameURL != "" {
		shot.FrameImageURL = frameURL
		referenceImage = frameURL
	}

	// 场景锚点：将锁定词注入视频生成 prompt
	videoPrompt := shot.Prompt
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, _, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && fragment != "" {
			videoPrompt = fragment + ", " + videoPrompt
		}
	}

	// 画面风格：注入视频 prompt（video.ArtStyle 优先，降级到 novel.ImageStyle）
	if videoArtStyle := s.resolveArtStyle(shot.VideoID); videoArtStyle != "" {
		videoPrompt = videoArtStyle + " style, " + videoPrompt
	}

	shotDuration := shot.Duration
	if shotDuration <= 0 {
		shotDuration = 5
	}
	req := &ai.VideoGenerateRequest{
		Prompt:         videoPrompt,
		NegativePrompt: shot.NegativePrompt,
		Duration:       shotDuration,
		AspectRatio:    videoAspectRatio,
		ImageURL:       referenceImage, // image-to-video（空时退化为 text-to-video）
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	shot.ShotTaskID = task.TaskID
	shot.ShotProviderName = providerName
	shot.Status = "processing"
	return s.storyboardRepo.Update(shot)
}

// PollShotStatus 轮询单个分镜视频生成状态
func (s *VideoService) PollShotStatus(shot *model.StoryboardShot) error {
	// 超时检查
	if shot.Status == "processing" && time.Since(shot.UpdatedAt) > 30*time.Minute {
		shot.Status = "failed"
		shot.RetryCount++
		s.storyboardRepo.Update(shot) //nolint:errcheck
		return nil
	}

	provider, ok := s.videoProviders[shot.ShotProviderName]
	if !ok {
		return fmt.Errorf("provider %s not found", shot.ShotProviderName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	taskStatus, err := provider.GetVideoStatus(ctx, shot.ShotTaskID)
	if err != nil {
		return err
	}

	switch taskStatus.Status {
	case "completed", "succeed":
		videoURL, urlErr := provider.GetVideoURL(ctx, shot.ShotTaskID)
		if urlErr != nil {
			return urlErr
		}

		// 立即下载到本地，防止临时签名 URL 在拼接时过期
		localClip := fmt.Sprintf("/tmp/inkframe-shot-%d.mp4", shot.ID)
		if dlErr := downloadFile(videoURL, localClip); dlErr != nil {
			log.Printf("PollShotStatus: download shot %d clip failed (%v), storing URL as fallback", shot.ID, dlErr)
			shot.ClipPath = videoURL
		} else {
			shot.ClipPath = "file://" + localClip
		}
		shot.Status = "completed"

		// 一致性评分（可选）
		if s.consistencyService != nil && shot.ChapterID != nil {
			chapter, _ := s.chapterRepo.GetByID(*shot.ChapterID)
			if chapter != nil {
				chars, _ := s.characterRepo.ListByNovel(chapter.NovelID)
				for _, c := range chars {
					if c.Portrait != "" {
						score, scoreErr := s.consistencyService.CalculateConsistencyScore(c.Portrait, []string{videoURL})
						if scoreErr == nil {
							shot.ConsistencyScore = score.OverallScore
							// 一致性过低时自动重试
							if score.OverallScore < 0.5 && shot.RetryCount < 2 {
								shot.Status = "pending"
								shot.RetryCount++
								shot.ClipPath = ""
								shot.ConsistencyScore = 0
								shot.ShotTaskID = "" // 必须清除，否则重试不会重新提交
							}
						}
						break
					}
				}
			}
		}
		s.storyboardRepo.Update(shot) //nolint:errcheck

		// 时序连贯：提取本镜最后一帧，存入下一镜 ReferenceImageURL
		if shot.Status == "completed" && strings.HasPrefix(shot.ClipPath, "file://") {
			if lastFramePath, frameErr := s.extractLastFrame(shot.ClipPath); frameErr == nil {
				// 查找下一镜（同 VideoID，ShotNo+1）
				nextShots, listErr := s.storyboardRepo.ListByVideo(shot.VideoID)
				if listErr == nil {
					for _, ns := range nextShots {
						if ns.ShotNo == shot.ShotNo+1 && ns.ShotTaskID == "" {
							ns.ReferenceImageURL = "file://" + lastFramePath
							s.storyboardRepo.Update(ns) //nolint:errcheck
							break
						}
					}
				}
			} else {
				log.Printf("PollShotStatus: extractLastFrame for shot %d failed: %v", shot.ShotNo, frameErr)
			}
		}

	case "failed":
		shot.Status = "failed"
		shot.RetryCount++
		if shot.RetryCount < 2 {
			shot.Status = "pending"
			shot.ShotTaskID = ""
		}
		s.storyboardRepo.Update(shot) //nolint:errcheck
	}

	return nil
}

// GenerateShotAudio 为单个分镜生成 TTS 音频，优先使用角色声音设置。
// 朗读文本优先级：Dialogue（角色台词）> Narration（旁白文案）> Description（英文画面描述，兜底）。
// narrationVoice 为旁白音色 ID（空串则自动从项目配置加载，仍空则降级到 "alloy"）。
// 若注入了 storageSvc，将音频上传至 OSS 或 DB，并更新 shot.AudioPath 为持久 URL。
func (s *VideoService) GenerateShotAudio(shot *model.StoryboardShot, tenantID uint, narrationVoice string) error {
	text := shot.Dialogue
	if text == "" {
		text = shot.Narration // 旁白文案（中文，无镜头语言）
	}
	if text == "" {
		text = shot.Description // 兜底：英文画面描述（旧数据兼容）
	}
	if text == "" {
		return nil
	}

	// 若调用方未传入旁白音色，从项目配置自动加载
	if narrationVoice == "" && s.novelRepo != nil {
		if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
			if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
				narrationVoice = novel.NarrationVoice
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	voice, speed, style := s.resolveVoiceForShot(shot, narrationVoice)
	audioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, text, voice, speed, style)
	if err != nil {
		return fmt.Errorf("TTS generation failed: %w", err)
	}
	if audioURL == "" {
		return fmt.Errorf("TTS returned empty audio URL")
	}

	// 若配置了存储服务，将音频上传至持久存储
	if s.storageSvc != nil {
		persistURL, uploadErr := s.uploadAudioToStorage(ctx, shot, audioURL)
		if uploadErr != nil {
			log.Printf("GenerateShotAudio: storage upload failed (falling back to local): %v", uploadErr)
		} else {
			audioURL = persistURL
			// 删除 /tmp 临时文件（file:// 前缀）
			if strings.HasPrefix(shot.AudioPath, "file://") {
				os.Remove(strings.TrimPrefix(shot.AudioPath, "file://")) //nolint:errcheck
			}
		}
	}

	shot.AudioPath = audioURL
	s.storyboardRepo.Update(shot) //nolint:errcheck
	return nil
}

// uploadAudioToStorage 读取 TTS 输出（file:// 路径或 HTTP URL），上传并返回持久 URL。
func (s *VideoService) uploadAudioToStorage(ctx context.Context, shot *model.StoryboardShot, audioURL string) (string, error) {
	var data []byte
	var readErr error

	if strings.HasPrefix(audioURL, "file://") {
		data, readErr = os.ReadFile(strings.TrimPrefix(audioURL, "file://"))
	} else if strings.HasPrefix(audioURL, "http://") || strings.HasPrefix(audioURL, "https://") {
		resp, err := http.Get(audioURL) //nolint:gosec
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		data, readErr = io.ReadAll(resp.Body)
	} else {
		return "", fmt.Errorf("unsupported audio URL scheme: %s", audioURL)
	}
	if readErr != nil {
		return "", readErr
	}

	video, err := s.videoRepo.GetByID(shot.VideoID)
	if err != nil {
		return "", err
	}
	novelID := video.NovelID
	var chapterID uint
	if video.ChapterID != nil {
		chapterID = *video.ChapterID
	}

	filename := fmt.Sprintf("shot-%d.mp3", shot.ID)
	key := storage.BuildKey(novelID, chapterID, "audio", filename)
	return s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
}

// GenerateShotSRT 根据分镜的台词/旁白和时长生成单条 SRT 字幕内容。
// 时间码从 00:00:00,000 开始，结束时间 = shot.Duration。
// 文本优先级：Dialogue > Narration > Description（兜底兼容旧数据）。
func GenerateShotSRT(shot *model.StoryboardShot) string {
	text := shot.Dialogue
	if text == "" {
		text = shot.Narration
	}
	if text == "" {
		text = shot.Description
	}
	if text == "" {
		return ""
	}
	dur := shot.Duration
	if dur <= 0 {
		dur = 5.0
	}
	end := formatSRTTimecode(dur)
	return fmt.Sprintf("1\n00:00:00,000 --> %s\n%s\n", end, text)
}

// formatSRTTimecode 将秒数格式化为 SRT 时间码 HH:MM:SS,mmm
func formatSRTTimecode(secs float64) string {
	h := int(secs) / 3600
	m := (int(secs) % 3600) / 60
	s := int(secs) % 60
	ms := int((secs-float64(int(secs)))*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// resolveVoiceForShot 解析分镜对应角色的配音设置（voice, speed, style）。
// 优先级：① 对话文本「角色名：」前缀 → ② shot.CharacterIDs 第一个角色 → ③ narrationVoice → ④ alloy。
func (s *VideoService) resolveVoiceForShot(shot *model.StoryboardShot, narrationVoice string) (voice string, speed float64, style string) {
	voice = "alloy"
	if narrationVoice != "" {
		voice = narrationVoice
	}
	speed = 1.0

	// 步骤一：从对话中解析发言角色（格式：角色名：对话内容 或 角色名:对话内容）
	speakerName := ""
	for _, sep := range []string{"：", ":"} {
		if idx := strings.Index(shot.Dialogue, sep); idx > 0 && idx < 20 {
			speakerName = strings.TrimSpace(shot.Dialogue[:idx])
			break
		}
	}

	video, err := s.videoRepo.GetByID(shot.VideoID)
	if err != nil || video.NovelID == 0 {
		return
	}

	applyCharVoice := func(c *model.Character) {
		if c.VoiceID != "" {
			voice = c.VoiceID
		}
		if c.VoiceSpeed > 0 {
			speed = c.VoiceSpeed
		}
		style = c.VoiceStyle
	}

	if speakerName != "" {
		// 按名称匹配
		characters, err := s.characterRepo.ListByNovel(video.NovelID)
		if err != nil {
			return
		}
		for _, c := range characters {
			if strings.EqualFold(c.Name, speakerName) {
				applyCharVoice(c)
				return
			}
		}
	}

	// 步骤二：仅当分镜有角色台词（Dialogue 非空）时，才使用第一个绑定角色的音色。
	// 旁白/描述文本不应被角色音色覆盖，应沿用 narrationVoice。
	if len(shot.CharacterIDs) > 0 && shot.Dialogue != "" {
		char, err := s.characterRepo.GetByID(shot.CharacterIDs[0])
		if err == nil && char != nil {
			applyCharVoice(char)
		}
	}

	return
}

// downloadFile 下载 HTTP URL 到本地路径
func downloadFile(url, dest string) error {
	resp, err := downloadHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// StitchVideo 将所有 completed 分镜拼接为最终视频
func (s *VideoService) StitchVideo(videoID uint) (string, error) {
	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "completed")
	if err != nil {
		return "", err
	}
	if len(shots) == 0 {
		return "", fmt.Errorf("no completed shots to stitch")
	}

	tmpDir := fmt.Sprintf("%s/inkframe-%d", inkframeTempDir(), videoID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var localShotFiles []string // 记录 PollShotStatus 下载的本地文件，拼接后清理
	defer func() {
		for _, f := range localShotFiles {
			os.Remove(f) //nolint:errcheck
		}
	}()
	var concatLines []string
	for i, shot := range shots {
		// 跳过无视频片段的镜头（仅有图片，Ken Burns 未生成）
		if shot.ClipPath == "" {
			log.Printf("StitchVideo: shot %d has no clip, skipping", shot.ShotNo)
			continue
		}

		clipFile := fmt.Sprintf("%s/clip_%d.mp4", tmpDir, i)
		finalClip := clipFile

		// 如果已是本地文件（PollShotStatus 立即下载过），直接使用，无需再下载
		if strings.HasPrefix(shot.ClipPath, "file://") {
			clipFile = strings.TrimPrefix(shot.ClipPath, "file://")
			finalClip = clipFile
			localShotFiles = append(localShotFiles, clipFile)
		} else {
			// 仍是远程 URL（fallback），下载到 tmpDir
			if err := downloadFile(shot.ClipPath, clipFile); err != nil {
				// URL 可能已过期，尝试从 provider 重新获取
				if shot.ShotTaskID != "" && shot.ShotProviderName != "" {
					if p, ok := s.videoProviders[shot.ShotProviderName]; ok {
						rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
						freshURL, fErr := p.GetVideoURL(rCtx, shot.ShotTaskID)
						rCancel()
						if fErr == nil {
							if err2 := downloadFile(freshURL, clipFile); err2 != nil {
								return "", fmt.Errorf("download shot %d clip failed (fresh URL also failed): %w", shot.ShotNo, err2)
							}
						} else {
							return "", fmt.Errorf("download shot %d clip failed and refresh URL failed: %w", shot.ShotNo, err)
						}
					} else {
						return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
					}
				} else {
					return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
				}
			}
		}

		// Merge audio if present
		if shot.AudioPath != "" {
			audioPath := strings.TrimPrefix(shot.AudioPath, "file://")
			mergedFile := fmt.Sprintf("%s/clip_audio_%d.mp4", tmpDir, i)
			cmd := exec.Command(s.ffmpegBin(), "-y",
				"-i", clipFile,
				"-i", audioPath,
				"-c:v", "copy",
				"-c:a", "aac",
				"-shortest",
				mergedFile,
			)
			if err := cmd.Run(); err != nil {
				log.Printf("StitchVideo: merge audio for shot %d failed: %v, using clip without audio", shot.ShotNo, err)
			} else {
				finalClip = mergedFile
			}
		}

		concatLines = append(concatLines, fmt.Sprintf("file '%s'", finalClip))
	}

	listFile := fmt.Sprintf("%s/list.txt", tmpDir)
	if err := os.WriteFile(listFile, []byte(strings.Join(concatLines, "\n")), 0644); err != nil {
		return "", err
	}

	stitchedPath := fmt.Sprintf("%s/inkframe-%d-stitched.mp4", inkframeTempDir(), videoID)
	cmd := exec.Command(s.ffmpegBin(), "-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy",
		stitchedPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg stitch failed: %w\noutput: %s", err, string(out))
	}

	// BGM 混音（非致命：失败时使用无BGM版本）
	outputPath := fmt.Sprintf("%s/inkframe-%d-output.mp4", inkframeTempDir(), videoID)
	if s.bgmService != nil {
		bgmURL := s.bgmService.SelectBGM("")
		if bgmURL != "" {
			if mixErr := s.bgmService.MixBGM(stitchedPath, bgmURL, outputPath); mixErr != nil {
				log.Printf("StitchVideo: BGM mixing failed (video %d): %v, using stitched without BGM", videoID, mixErr)
				outputPath = stitchedPath
			}
		} else {
			outputPath = stitchedPath
		}
	} else {
		outputPath = stitchedPath
	}

	// Update video record
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return outputPath, nil
	}
	video.VideoPath = outputPath
	video.Status = "completed"
	s.videoRepo.Update(video) //nolint:errcheck

	return outputPath, nil
}

// inkframeTempDir 返回可写的临时目录（优先用工作目录下的 tmp/，fallback 到系统临时目录）
func inkframeTempDir() string {
	dir := "tmp"
	if err := os.MkdirAll(dir, 0755); err == nil {
		return dir
	}
	return os.TempDir()
}

// downloadToTemp 将 URL 下载到临时文件，返回本地路径
func downloadToTemp(url, prefix, ext string) (string, error) {
	resp, err := downloadHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmpFile, err := os.CreateTemp(inkframeTempDir(), prefix+"*"+ext)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()
	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}

// generateKenBurnsClip 使用 FFmpeg zoompan 滤镜将静图制作成 Ken Burns 动效短片
// generateStillFrameClip 用 FFmpeg 将静态图片编码为固定时长的视频（无动效，Ken Burns 降级方案）。
func (s *VideoService) generateStillFrameClip(imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = 5.0
	}
	resolution := "1920:1080"
	switch aspectRatio {
	case "9:16":
		resolution = "1080:1920"
	case "1:1":
		resolution = "1080:1080"
	case "4:3":
		resolution = "1440:1080"
	}
	parts := strings.SplitN(resolution, ":", 2)
	w, h := parts[0], parts[1]
	vf := fmt.Sprintf("scale=%s:%s:force_original_aspect_ratio=decrease,pad=%s:%s:(ow-iw)/2:(oh-ih)/2,setsar=1", w, h, w, h)
	outPath := fmt.Sprintf("%s/inkframe-still-%d.mp4", inkframeTempDir(), time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ffmpegBin(), "-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", "24",
		outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg still frame: %v — %s", err, stderr.String())
	}
	return outPath, nil
}

func (s *VideoService) generateKenBurnsClip(shot *model.StoryboardShot, imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = 5.0
	}
	fps := 30
	totalFrames := int(duration * float64(fps))

	resolution := "1920:1080"
	switch aspectRatio {
	case "9:16":
		resolution = "1080:1920"
	case "1:1":
		resolution = "1080:1080"
	case "4:3":
		resolution = "1440:1080"
	}

	// 根据摄像机类型选择 zoompan 动效
	var zoompan string
	switch shot.CameraType {
	case "zoom":
		// 明显放大
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.002,1.5)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	case "pan":
		// 水平平移（从左向右）
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f))':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	default:
		// 默认轻微放大（Ken Burns 经典效果）
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.0008,1.2)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	}

	outPath := fmt.Sprintf("%s/inkframe-slideshow-%d-%d.mp4", inkframeTempDir(), shot.ID, time.Now().UnixNano())
	// pre-scale to 2x output resolution — zoompan 只需输入略大于输出，8000 过大极慢
	preScale := "3840:-1"
	if aspectRatio == "9:16" {
		preScale = "2160:-1"
	}
	vf := fmt.Sprintf("scale=%s,%s,scale=%s,setsar=1", preScale, zoompan, resolution)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ffmpegBin(), "-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%d", fps),
		"-threads", "0",
		outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg ken burns: %v — %s", err, stderr.String())
	}
	return outPath, nil
}

// GenerateSlideshowShotVideo 为单个分镜生成图片并应用 Ken Burns 动效（图片解说模式）
func (s *VideoService) GenerateSlideshowShotVideo(shot *model.StoryboardShot, aspectRatio string) error {
	duration := shot.Duration
	if duration <= 0 {
		duration = 5.0
	}

	shot.GenerationMode = "static"
	shot.Status = "generating"
	s.storyboardRepo.Update(shot) //nolint:errcheck

	// 1. 生成图片
	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		log.Printf("GenerateSlideshowShotVideo: image gen failed for shot %d: %s", shot.ShotNo, errMsg)
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		s.storyboardRepo.Update(shot) //nolint:errcheck
		if imgErr != nil {
			return fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return fmt.Errorf("image generation failed for shot %d (empty URL returned)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	// 保存图片 URL（后续步骤失败时图片仍可用）
	s.storyboardRepo.Update(shot) //nolint:errcheck

	// 2. 下载图片到本地（Volcengine 返回的 URL 后缀为 .image，FFmpeg 无法识别格式，需重命名为 .jpg）
	localImage, err := downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
	if err != nil {
		log.Printf("GenerateSlideshowShotVideo: download image failed for shot %d, marking completed with image only: %v", shot.ShotNo, err)
		shot.Status = "completed"
		shot.Progress = 100
		shot.ErrorMessage = fmt.Sprintf("ken burns skipped: %v", err)
		return s.storyboardRepo.Update(shot)
	}
	defer os.Remove(localImage)

	// 3. Ken Burns 动效（缓慢推拉/平移，让静态图更生动）；失败时降级为静止画面
	clipPath, err := s.generateKenBurnsClip(shot, localImage, duration, aspectRatio)
	if err != nil {
		log.Printf("GenerateSlideshowShotVideo: ken burns failed for shot %d, falling back to still frame: %v", shot.ShotNo, err)
		// 降级：用静止画面代替 Ken Burns
		clipPath, err = s.generateStillFrameClip(localImage, duration, aspectRatio)
		if err != nil {
			log.Printf("GenerateSlideshowShotVideo: still frame fallback also failed for shot %d, completing with image only: %v", shot.ShotNo, err)
			shot.Status = "completed"
			shot.Progress = 100
			shot.ErrorMessage = fmt.Sprintf("ffmpeg unavailable: %v", err)
			return s.storyboardRepo.Update(shot)
		}
		shot.ErrorMessage = "ken burns skipped, used still frame"
	}

	shot.ClipPath = "file://" + clipPath
	if shot.ErrorMessage == "" {
		shot.ErrorMessage = ""
	}
	shot.Status = "completed"
	shot.Progress = 100
	return s.storyboardRepo.Update(shot)
}

// runSlideshowPipeline 异步处理图片解说模式的所有分镜，完成后自动拼接
func (s *VideoService) runSlideshowPipeline(videoID uint) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		log.Printf("runSlideshowPipeline: get video %d failed: %v", videoID, err)
		return
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil || len(shots) == 0 {
		log.Printf("runSlideshowPipeline: no pending shots for video %d", videoID)
		return
	}

	for _, shot := range shots {
		if err := s.GenerateSlideshowShotVideo(shot, video.AspectRatio); err != nil {
			log.Printf("runSlideshowPipeline: shot %d failed: %v", shot.ShotNo, err)
		}
		go s.GenerateShotAudio(shot, video.TenantID, "") //nolint:errcheck
	}

	// 拼接
	if _, err := s.StitchVideo(videoID); err != nil {
		log.Printf("runSlideshowPipeline: stitch video %d failed: %v", videoID, err)
		if v, _ := s.videoRepo.GetByID(videoID); v != nil {
			v.Status = "failed"
			v.ErrorMessage = err.Error()
			s.videoRepo.Update(v) //nolint:errcheck
		}
	}
}

// GenerateAllShotVideos 提交所有待生成分镜的视频任务
func (s *VideoService) GenerateAllShotVideos(videoID uint) error {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return err
	}

	// 图片解说模式：异步生成图片，完成后自动拼接
	if video.Mode == "slideshow" {
		shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if err != nil || len(shots) == 0 {
			return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
		}
		video.Status = "generating"
		video.ErrorMessage = ""
		s.videoRepo.Update(video) //nolint:errcheck
		go s.runSlideshowPipeline(videoID)
		return nil
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil {
		return err
	}
	if len(shots) == 0 {
		return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
	}

	// 更新状态，让用户可以通过 GetStatus 感知进度
	video.Status = "generating"
	video.ErrorMessage = ""
	s.videoRepo.Update(video) //nolint:errcheck

	for _, shot := range shots {
		if err := s.GenerateShotVideo(shot, video.AspectRatio); err != nil {
			log.Printf("GenerateAllShotVideos: shot %d failed: %v", shot.ShotNo, err)
			continue
		}
		// TTS audio in parallel
		go s.GenerateShotAudio(shot, video.TenantID, "") //nolint:errcheck
	}
	return nil
}

// PollAndStitchVideo 后台轮询所有分镜状态，完成后拼接
func (s *VideoService) PollAndStitchVideo(videoID uint) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Hour)
	noProgressCount := 0

	for {
		if time.Now().After(deadline) {
			log.Printf("PollAndStitchVideo: videoID %d timed out after 2h", videoID)
			video, _ := s.videoRepo.GetByID(videoID)
			if video != nil && video.Status != "completed" {
				video.Status = "failed"
				video.ErrorMessage = "stitch pipeline timed out (>2h)"
				s.videoRepo.Update(video) //nolint:errcheck
			}
			return
		}

		<-ticker.C

		// Retry pending shots (from consistency/failed retry)
		pending, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		for _, shot := range pending {
			if shot.ShotTaskID == "" {
				video, _ := s.videoRepo.GetByID(videoID)
				aspectRatio := "16:9"
				if video != nil {
					aspectRatio = video.AspectRatio
				}
				s.GenerateShotVideo(shot, aspectRatio) //nolint:errcheck
			}
		}

		// Poll processing shots
		processing, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		for _, shot := range processing {
			s.PollShotStatus(shot) //nolint:errcheck
		}

		// Check if all completed
		allShots, _ := s.storyboardRepo.ListByVideo(videoID)
		if len(allShots) == 0 {
			continue
		}
		completedCount := 0
		failedCount := 0
		for _, shot := range allShots {
			switch shot.Status {
			case "completed":
				completedCount++
			case "failed":
				failedCount++
			}
		}

		if completedCount+failedCount == len(allShots) {
			if completedCount > 0 {
				if _, err := s.StitchVideo(videoID); err != nil {
					log.Printf("PollAndStitchVideo: stitch failed: %v", err)
					video, _ := s.videoRepo.GetByID(videoID)
					if video != nil {
						video.Status = "failed"
						video.ErrorMessage = err.Error()
						s.videoRepo.Update(video) //nolint:errcheck
					}
				}
			} else {
				video, _ := s.videoRepo.GetByID(videoID)
				if video != nil {
					video.Status = "failed"
					video.ErrorMessage = "all shots failed"
					s.videoRepo.Update(video) //nolint:errcheck
				}
			}
			return
		}

		// Stall detection (no progress after 5 ticks): re-query to get fresh counts
		processingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		pendingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if len(processingNow) == 0 && len(pendingNow) == 0 {
			noProgressCount++
			if noProgressCount >= 5 {
				log.Printf("PollAndStitchVideo: videoID %d stalled, stopping", videoID)
				return
			}
		} else {
			noProgressCount = 0
		}
	}
}

// ModelService 模型服务
type ModelService struct {
	modelRepo      *repository.AIModelRepository
	providerRepo   *repository.ModelProviderRepository
	taskRepo       *repository.TaskModelConfigRepository
	experimentRepo *repository.ModelComparisonRepository
	aiService      *AIService
}

func NewModelService(
	modelRepo *repository.AIModelRepository,
	providerRepo *repository.ModelProviderRepository,
	taskRepo *repository.TaskModelConfigRepository,
	experimentRepo *repository.ModelComparisonRepository,
	aiService ...*AIService,
) *ModelService {
	svc := &ModelService{
		modelRepo:      modelRepo,
		providerRepo:   providerRepo,
		taskRepo:       taskRepo,
		experimentRepo: experimentRepo,
	}
	if len(aiService) > 0 {
		svc.aiService = aiService[0]
	}
	return svc
}

func selectByQuality(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestScore := 0.0

	for _, m := range models {
		score := m.Quality
		if score > bestScore {
			bestScore = score
			best = m
		}
	}

	return best
}

func selectByCost(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestCost := 999999.0

	for _, m := range models {
		if m.CostPer1K < bestCost {
			bestCost = m.CostPer1K
			best = m
		}
	}

	return best
}

func selectBalanced(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestScore := 0.0

	for _, m := range models {
		// 质量/成本比
		score := m.Quality / m.CostPer1K
		if score > bestScore {
			bestScore = score
			best = m
		}
	}

	return best
}
