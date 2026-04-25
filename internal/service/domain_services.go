package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"text/template"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ============================================
// ChapterService 章节服务
// ============================================

type ChapterService struct {
	chapterRepo   *repository.ChapterRepository
	novelRepo     *repository.NovelRepository
	aiService     *AIService
	contextSvc    *GenerationContextService
	narrativeSvc  *NarrativeMemoryService // 层次化记忆 + 摘要 + 标题 + 精修
}

func NewChapterService(
	chapterRepo *repository.ChapterRepository,
	novelRepo *repository.NovelRepository,
	aiService *AIService,
	contextSvc *GenerationContextService,
) *ChapterService {
	return &ChapterService{
		chapterRepo: chapterRepo,
		novelRepo:   novelRepo,
		aiService:   aiService,
		contextSvc:  contextSvc,
	}
}

// WithNarrativeMemory 注入层次化记忆服务（可选）
func (s *ChapterService) WithNarrativeMemory(svc *NarrativeMemoryService) *ChapterService {
	s.narrativeSvc = svc
	return s
}

// GetDefaultProviderName 返回默认 AI provider 名称
func (s *ChapterService) GetDefaultProviderName() string {
	return s.aiService.GetDefaultProviderName()
}

func (s *ChapterService) CreateChapter(novelID uint, req *model.CreateChapterRequest) (*model.Chapter, error) {
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		NovelID:   novelID,
		ChapterNo: req.ChapterNo,
		Title:     req.Title,
		Content:   req.Content,
		WordCount: countChineseChars(req.Content),
		Status:    "completed",
	}
	return chapter, s.chapterRepo.Create(chapter)
}

func (s *ChapterService) GetChapter(id uint) (*model.Chapter, error) {
	return s.chapterRepo.GetByID(id)
}

func (s *ChapterService) ListChapters(novelID uint) ([]*model.Chapter, error) {
	return s.chapterRepo.ListByNovel(novelID)
}

func (s *ChapterService) UpdateChapter(id uint, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		chapter.Title = req.Title
	}
	if req.Content != "" {
		chapter.Content = req.Content
		chapter.WordCount = countChineseChars(req.Content)
	}
	return chapter, s.chapterRepo.Update(chapter)
}

func (s *ChapterService) DeleteChapter(id uint) error {
	return s.chapterRepo.Delete(id)
}

func (s *ChapterService) GetChapterByNo(novelID uint, chapterNo int) (*model.Chapter, error) {
	return s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
}

func (s *ChapterService) UpdateChapterByNo(novelID uint, chapterNo int, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		chapter.Title = req.Title
	}
	if req.Content != "" {
		chapter.Content = req.Content
		chapter.WordCount = countChineseChars(req.Content)
	}
	return chapter, s.chapterRepo.Update(chapter)
}

func (s *ChapterService) DeleteChapterByNo(novelID uint, chapterNo int) error {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return err
	}
	return s.chapterRepo.Delete(chapter.ID)
}

// GenerateChapter 专业级章节生成流水线：
//
//  Step 1  构建层次化上下文（近章详摘 + 弧摘要 + 全局概述）
//  Step 2  生成场景大纲（3-5 个场景，含节拍、情绪、钩子）
//  Step 3  按场景大纲生成完整章节内容
//  Step 4  存储章节（包含场景大纲、叙事元数据）
//  Step 5  异步后处理：摘要生成、标题生成、精修、角色状态提取、弧摘要触发
func (s *ChapterService) GenerateChapter(tenantID uint, novelID uint, req *model.GenerateChapterRequest) (*model.Chapter, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}

	// ── Step 1: 层次化上下文 ──────────────────────────────
	globalCtx := s.buildGlobalContext(novelID, req.ChapterNo, novel)

	// 从小说大纲获取本章元数据（张力值、幕次、情感基调等）
	chapterMeta := s.extractChapterMeta(novelID, req.ChapterNo)

	// ── Step 2: 生成场景大纲 ──────────────────────────────
	sceneOutlineJSON, suggestedTitle := s.generateSceneOutline(
		tenantID, novelID, req, novel, globalCtx, chapterMeta,
	)

	// ── Step 3: 按场景大纲生成章节内容 ───────────────────
	content, err := s.generateFromSceneOutline(
		tenantID, novelID, req, novel, sceneOutlineJSON, globalCtx, chapterMeta,
	)
	if err != nil {
		return nil, err
	}

	// ── Step 4: 存储章节 ──────────────────────────────────
	title := suggestedTitle
	if title == "" {
		title = fmt.Sprintf("第%d章", req.ChapterNo)
	}
	chapter := &model.Chapter{
		UUID:          uuid.New().String(),
		NovelID:       novelID,
		ChapterNo:     req.ChapterNo,
		Title:         title,
		Content:       content,
		WordCount:     countChineseChars(content),
		SceneOutline:  sceneOutlineJSON,
		TensionLevel:  chapterMeta.tensionLevel,
		ActNo:         chapterMeta.actNo,
		EmotionalTone: chapterMeta.emotionalTone,
		HookType:      chapterMeta.hookType,
		Status:        "completed",
	}
	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}

	// ── Step 5: 异步后处理 ───────────────────────────────
	go s.postProcessChapter(tenantID, chapter, novel)

	return chapter, nil
}

// chapterOutlineMeta 从小说大纲中提取的章节叙事元数据
type chapterOutlineMeta struct {
	tensionLevel  int
	actNo         int
	emotionalTone string
	hookType      string
	summary       string // 大纲中的章节概述
}

func (s *ChapterService) extractChapterMeta(novelID uint, chapterNo int) chapterOutlineMeta {
	// 尝试从小说 Outline 字段中解析章节元数据
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return chapterOutlineMeta{}
	}
	if novel.StylePrompt == "" {
		return chapterOutlineMeta{}
	}
	// 解析存储在 StylePrompt 中的大纲 JSON（大纲生成后存储在此）
	var outline struct {
		Chapters []struct {
			ChapterNo     int    `json:"chapter_no"`
			TensionLevel  int    `json:"tension_level"`
			Act           int    `json:"act"`
			EmotionalTone string `json:"emotional_tone"`
			HookType      string `json:"hook_type"`
			Summary       string `json:"summary"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(novel.StylePrompt), &outline); err != nil {
		return chapterOutlineMeta{}
	}
	for _, ch := range outline.Chapters {
		if ch.ChapterNo == chapterNo {
			return chapterOutlineMeta{
				tensionLevel:  ch.TensionLevel,
				actNo:         ch.Act,
				emotionalTone: ch.EmotionalTone,
				hookType:      ch.HookType,
				summary:       ch.Summary,
			}
		}
	}
	return chapterOutlineMeta{}
}

// buildGlobalContext 构建层次化全局上下文（优先使用 NarrativeMemoryService）
func (s *ChapterService) buildGlobalContext(novelID uint, chapterNo int, novel *model.Novel) string {
	// 优先使用层次化记忆
	if s.narrativeSvc != nil {
		ctx, err := s.narrativeSvc.BuildHierarchicalContext(novelID, chapterNo)
		if err == nil && ctx != "" {
			return ctx
		}
		log.Printf("GenerateChapter: hierarchical context failed: %v — fallback", err)
	}
	// 降级到原 GenerationContextService
	if s.contextSvc != nil {
		ctx, err := s.contextSvc.BuildGenerationPrompt(novelID, chapterNo, "", "", 8000)
		if err == nil {
			return ctx
		}
	}
	// 最终降级
	return fmt.Sprintf("【故事概要】\n%s", novel.Description)
}

// generateSceneOutline 调用 AI 生成场景级大纲，返回 JSON 字符串和建议标题
func (s *ChapterService) generateSceneOutline(
	tenantID, novelID uint,
	req *model.GenerateChapterRequest,
	novel *model.Novel,
	globalCtx string,
	meta chapterOutlineMeta,
) (sceneOutlineJSON, suggestedTitle string) {

	// 获取角色状态
	charStateStr := s.buildCharacterStateString(novelID)

	// 构建伏笔提示
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	// 获取上一章结尾（供连贯性参考）
	prevEnding := s.getPreviousChapterEnding(novelID, req.ChapterNo)

	// 获取角色列表
	characters := s.getCharactersForPrompt(novelID)

	hookType := meta.hookType
	if hookType == "" {
		hookType = "cliffhanger"
	}
	emotionalTone := meta.emotionalTone
	if emotionalTone == "" {
		emotionalTone = "紧张"
	}
	tensionLevel := meta.tensionLevel
	if tensionLevel == 0 {
		tensionLevel = 6
	}
	actNo := meta.actNo
	if actNo == 0 {
		actNo = 1
	}
	chapterSummary := meta.summary
	if chapterSummary == "" && req.Prompt != "" {
		chapterSummary = req.Prompt
	}

	tmplStr := loadPromptTemplate("chapter_scene_outline.tmpl")
	tmpl, err := template.New("scene_outline").Parse(tmplStr)
	if err != nil {
		log.Printf("GenerateChapter: parse scene_outline.tmpl: %v", err)
		return "", ""
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":            novel.Title,
		"ChapterNo":             req.ChapterNo,
		"GlobalContext":         globalCtx,
		"ChapterSummary":        chapterSummary,
		"TensionLevel":          tensionLevel,
		"ActNo":                 actNo,
		"EmotionalTone":         emotionalTone,
		"HookType":              hookType,
		"PreviousChapterEnding": prevEnding,
		"Characters":            characters,
		"ForeshadowHints":       foreshadowHints,
		"CharacterStates":       charStateStr,
	}); err != nil {
		log.Printf("GenerateChapter: execute scene_outline.tmpl: %v", err)
		return "", ""
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, novelID, "scene_outline", buf.String(), req.ModelOverride)
	if err != nil {
		log.Printf("GenerateChapter: scene outline AI call failed: %v", err)
		return "", ""
	}

	resp = extractJSON(strings.TrimSpace(resp))

	// 提取建议标题
	var outlineResult struct {
		ChapterTitle string `json:"chapter_title"`
	}
	if err := json.Unmarshal([]byte(resp), &outlineResult); err == nil {
		suggestedTitle = outlineResult.ChapterTitle
	}

	return resp, suggestedTitle
}

// generateFromSceneOutline 根据场景大纲生成章节正文
func (s *ChapterService) generateFromSceneOutline(
	tenantID, novelID uint,
	req *model.GenerateChapterRequest,
	novel *model.Novel,
	sceneOutlineJSON string,
	globalCtx string,
	meta chapterOutlineMeta,
) (string, error) {

	// MaxTokens 约等于字数（中文约1token/字）；未设置时默认3000字
	wordCount := req.MaxTokens
	if wordCount <= 0 {
		wordCount = 3000
	}

	// 解析场景大纲以注入模板
	var outlineData struct {
		ChapterTitle string `json:"chapter_title"`
		HookSetup    string `json:"hook_setup"`
		ChapterArc   string `json:"chapter_arc"`
		Scenes       []struct {
			SceneNo       int      `json:"scene_no"`
			Location      string   `json:"location"`
			TimeOfDay     string   `json:"time_of_day"`
			Characters    []string `json:"characters"`
			Goal          string   `json:"goal"`
			OpeningBeat   string   `json:"opening_beat"`
			KeyBeats      []string `json:"key_beats"`
			ClosingBeat   string   `json:"closing_beat"`
			EmotionalShift string  `json:"emotional_shift"`
			POVCharacter  string   `json:"pov_character"`
			Tension       int      `json:"tension"`
		} `json:"scenes"`
	}
	_ = json.Unmarshal([]byte(sceneOutlineJSON), &outlineData)

	// 获取角色对话风格
	characterVoices := s.getCharacterVoices(novelID)

	// 峰值张力
	peakTension := 0
	for _, sc := range outlineData.Scenes {
		if sc.Tension > peakTension {
			peakTension = sc.Tension
		}
	}

	chapterTitle := outlineData.ChapterTitle
	if chapterTitle == "" {
		chapterTitle = fmt.Sprintf("第%d章", req.ChapterNo)
	}

	// 如果没有场景大纲，降级到简单 prompt
	if sceneOutlineJSON == "" || len(outlineData.Scenes) == 0 {
		return s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
	}

	tmplStr := loadPromptTemplate("chapter_from_outline.tmpl")
	tmpl, err := template.New("chapter_from_outline").Parse(tmplStr)
	if err != nil {
		return s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novel.Title,
		"ChapterNo":     req.ChapterNo,
		"ChapterTitle":  chapterTitle,
		"WordCount":     wordCount,
		"GlobalContext": globalCtx,
		"Scenes":        outlineData.Scenes,
		"HookSetup":     outlineData.HookSetup,
		"PeakTension":   peakTension,
		"Characters":    characterVoices,
		"UserPrompt":    req.Prompt,
	}); err != nil {
		return s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
	}

	return s.aiService.GenerateWithProvider(tenantID, novelID, "chapter", buf.String(), req.ModelOverride)
}

// generateFallbackChapter 场景大纲失败时的降级生成
func (s *ChapterService) generateFallbackChapter(tenantID, novelID uint, req *model.GenerateChapterRequest, novel *model.Novel, globalCtx string) (string, error) {
	log.Printf("GenerateChapter: using fallback (no scene outline) for novel %d ch %d", novelID, req.ChapterNo)
	wc := req.MaxTokens
	if wc <= 0 {
		wc = 3000
	}
	prompt := globalCtx + fmt.Sprintf("\n\n请为小说《%s》生成第%d章内容，字数约%d字。", novel.Title, req.ChapterNo, wc)
	if req.Prompt != "" {
		prompt += "\n\n创作要求：" + req.Prompt
	}
	return s.aiService.GenerateWithProvider(tenantID, novelID, "chapter", prompt, req.ModelOverride)
}

// postProcessChapter 异步后处理：生成摘要→生成标题→精修→提取角色状态→触发弧摘要
func (s *ChapterService) postProcessChapter(tenantID uint, chapter *model.Chapter, novel *model.Novel) {
	// 1. 生成摘要（最重要：供后续章节的上下文使用）
	if s.narrativeSvc != nil && chapter.Summary == "" {
		if summary, err := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); err == nil {
			chapter.Summary = summary
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				log.Printf("postProcessChapter: update chapter %d [摘要]: %v", chapter.ID, updateErr)
			}
		} else {
			log.Printf("postProcess: summary ch%d: %v", chapter.ChapterNo, err)
		}
	}

	// 2. 如果标题仍是"第N章"，生成创意标题
	defaultTitle := fmt.Sprintf("第%d章", chapter.ChapterNo)
	if s.narrativeSvc != nil && chapter.Title == defaultTitle && chapter.Summary != "" {
		if title, err := s.narrativeSvc.GenerateChapterTitle(tenantID, chapter, novel.Genre, chapter.EmotionalTone); err == nil && title != "" {
			chapter.Title = fmt.Sprintf("第%d章 %s", chapter.ChapterNo, title)
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				log.Printf("postProcessChapter: update chapter %d [标题]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 3. 精修（检测并修复重复词、AI惯用句等）
	if s.narrativeSvc != nil {
		if refined, err := s.narrativeSvc.RefineChapterContent(tenantID, chapter, novel.Title); err == nil && refined != chapter.Content {
			chapter.Content = refined
			chapter.WordCount = countChineseChars(refined)
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				log.Printf("postProcessChapter: update chapter %d [精修]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 4. 提取角色状态快照（原有逻辑）
	novelSvc := &NovelService{novelRepo: s.novelRepo, chapterRepo: s.chapterRepo, aiService: s.aiService}
	novelSvc.writeCharacterSnapshots(chapter)

	// 5. 触发弧摘要（每 arcSize 章触发一次）
	if s.narrativeSvc != nil {
		s.narrativeSvc.TriggerArcSummaryIfNeeded(tenantID, novel.ID, chapter.ChapterNo)
	}
}

// ──────────────────────────────────────────────
// Context helpers for GenerateChapter
// ──────────────────────────────────────────────

type characterForPrompt struct {
	Name         string
	Role         string
	CurrentState string
	DialogueStyle string
}

func (s *ChapterService) getCharactersForPrompt(novelID uint) []characterForPrompt {
	// ChapterService 没有直接访问 charRepo，通过 novelSvc 的快照机制获取
	// 这里返回空列表，实际角色信息通过 globalCtx 已包含
	return nil
}

func (s *ChapterService) getCharacterVoices(novelID uint) []characterForPrompt {
	return nil
}

func (s *ChapterService) buildCharacterStateString(novelID uint) string {
	return ""
}

func (s *ChapterService) buildForeshadowHints(novelID uint, chapterNo int) string {
	if s.contextSvc == nil || s.contextSvc.foreshadowSvc == nil {
		return ""
	}
	foreshadows, err := s.contextSvc.foreshadowSvc.CheckForeshadowStatus(novelID, chapterNo)
	if err != nil {
		return ""
	}
	var hints strings.Builder
	count := 0
	for _, fs := range foreshadows {
		if !fs.IsFulfilled && chapterNo-fs.ChapterNo >= 3 {
			hints.WriteString(fmt.Sprintf("- 请考虑回收伏笔：「%s」（第%d章埋设）\n", fs.Description, fs.ChapterNo))
			count++
			if count >= 3 {
				break
			}
		}
	}
	return hints.String()
}

func (s *ChapterService) getPreviousChapterEnding(novelID uint, chapterNo int) string {
	if chapterNo <= 1 {
		return "（本章为开篇）"
	}
	prev, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo-1)
	if err != nil || prev == nil {
		return ""
	}
	if prev.Summary != "" {
		return prev.Summary
	}
	// 从内容末尾截取约200字
	content := []rune(prev.Content)
	if len(content) > 200 {
		content = content[len(content)-200:]
	}
	return string(content)
}


func (s *ChapterService) RegenerateChapter(tenantID uint, id uint, prompt string) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	content, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter", prompt, "")
	if err != nil {
		return nil, err
	}
	chapter.Content = content
	chapter.WordCount = countChineseChars(content)
	return chapter, s.chapterRepo.Update(chapter)
}

func (s *ChapterService) ApproveChapter(id uint, comment string) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return err
	}
	chapter.Status = "approved"
	return s.chapterRepo.Update(chapter)
}

func (s *ChapterService) RejectChapter(id uint, reason string) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return err
	}
	chapter.Status = "rejected"
	return s.chapterRepo.Update(chapter)
}

// ============================================
// ChapterVersionService 章节版本服务
// ============================================

type ChapterVersionService struct {
	versionRepo *repository.ChapterVersionRepository
	chapterRepo *repository.ChapterRepository
}

func NewChapterVersionService(
	versionRepo *repository.ChapterVersionRepository,
	chapterRepo *repository.ChapterRepository,
) *ChapterVersionService {
	return &ChapterVersionService{
		versionRepo: versionRepo,
		chapterRepo: chapterRepo,
	}
}

func (s *ChapterVersionService) GetVersions(chapterID uint) ([]*model.ChapterVersion, error) {
	return s.versionRepo.List(chapterID)
}

func (s *ChapterVersionService) RestoreVersion(chapterID uint, versionNo int) (*model.Chapter, error) {
	version, err := s.versionRepo.GetVersion(chapterID, versionNo)
	if err != nil {
		return nil, err
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, err
	}
	chapter.Content = version.Content
	return chapter, s.chapterRepo.Update(chapter)
}

// ============================================
// CharacterService 角色服务
// ============================================

// EffectiveCharacter 有效角色（合并项目级与章节级覆盖）
type EffectiveCharacter struct {
	model.Character
	ChapterOverride      *model.ChapterCharacter `json:"chapter_override,omitempty"`
	EffectiveAppearance  string                  `json:"effective_appearance"`
	EffectivePersonality string                  `json:"effective_personality"`
	EffectiveStatus      string                  `json:"effective_status"`
	EffectiveLocation    string                  `json:"effective_location"`
}

type CharacterService struct {
	characterRepo        *repository.CharacterRepository
	chapterCharacterRepo *repository.ChapterCharacterRepository
	aiService            *AIService
}

func NewCharacterService(
	characterRepo *repository.CharacterRepository,
	aiService *AIService,
) *CharacterService {
	return &CharacterService{
		characterRepo: characterRepo,
		aiService:     aiService,
	}
}

// WithChapterCharacterRepo 注入章节角色覆盖仓库（可选）
func (s *CharacterService) WithChapterCharacterRepo(r *repository.ChapterCharacterRepository) *CharacterService {
	s.chapterCharacterRepo = r
	return s
}

func (s *CharacterService) CreateCharacter(novelID uint, req *model.CreateCharacterRequest) (*model.Character, error) {
	character := &model.Character{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		Name:        req.Name,
		Role:        req.Role,
		Archetype:   req.Archetype,
		Background:  req.Background,
		Appearance:  req.Appearance,
		Personality: req.Personality,
		Status:      "active",
	}
	return character, s.characterRepo.Create(character)
}

func (s *CharacterService) GetCharacter(id uint) (*model.Character, error) {
	return s.characterRepo.GetByID(id)
}

func (s *CharacterService) ListCharacters(novelID uint) ([]*model.Character, error) {
	return s.characterRepo.ListByNovel(novelID)
}

func (s *CharacterService) UpdateCharacter(id uint, req *model.UpdateCharacterRequest) (*model.Character, error) {
	character, err := s.characterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		character.Name = req.Name
	}
	if req.Role != "" {
		character.Role = req.Role
	}
	if req.Archetype != "" {
		character.Archetype = req.Archetype
	}
	if req.Background != "" {
		character.Background = req.Background
	}
	if req.Appearance != "" {
		character.Appearance = req.Appearance
	}
	if req.Personality != "" {
		character.Personality = req.Personality
	}
	if req.CharacterArc != "" {
		character.CharacterArc = req.CharacterArc
	}
	// PersonalityTags: nil = absent (skip), non-nil (including empty) = update
	if req.PersonalityTags != nil {
		if b, err := json.Marshal(req.PersonalityTags); err == nil {
			character.PersonalityTags = string(b)
		}
	}
	// Abilities: nil = absent (skip), non-nil = update
	if req.Abilities != nil {
		if b, err := json.Marshal(req.Abilities); err == nil {
			character.Abilities = string(b)
		}
	}
	if req.ThreeViewFront != "" {
		character.ThreeViewFront = req.ThreeViewFront
	}
	if req.ThreeViewSide != "" {
		character.ThreeViewSide = req.ThreeViewSide
	}
	if req.ThreeViewBack != "" {
		character.ThreeViewBack = req.ThreeViewBack
	}
	if req.Portrait != "" {
		character.Portrait = req.Portrait
	}
	if req.CoverImage != "" {
		character.CoverImage = req.CoverImage
	}
	return character, s.characterRepo.Update(character)
}

func (s *CharacterService) DeleteCharacter(id uint) error {
	return s.characterRepo.Delete(id)
}

// ListEffectiveCharacters 获取章节的有效角色列表（章节级覆盖优先）
func (s *CharacterService) ListEffectiveCharacters(novelID, chapterID uint) ([]*EffectiveCharacter, error) {
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	overrideMap := make(map[uint]*model.ChapterCharacter)
	if s.chapterCharacterRepo != nil {
		overrides, _ := s.chapterCharacterRepo.ListByChapter(chapterID)
		for _, o := range overrides {
			overrideMap[o.CharacterID] = o
		}
	}
	result := make([]*EffectiveCharacter, 0, len(chars))
	for _, ch := range chars {
		ec := &EffectiveCharacter{Character: *ch}
		if o, ok := overrideMap[ch.ID]; ok {
			ec.ChapterOverride = o
			if o.Appearance != "" {
				ec.EffectiveAppearance = o.Appearance
			} else {
				ec.EffectiveAppearance = ch.Appearance
			}
			if o.Personality != "" {
				ec.EffectivePersonality = o.Personality
			} else {
				ec.EffectivePersonality = ch.Personality
			}
			if o.Status != "" {
				ec.EffectiveStatus = o.Status
			} else {
				ec.EffectiveStatus = ch.Status
			}
			ec.EffectiveLocation = o.Location
		} else {
			ec.EffectiveAppearance = ch.Appearance
			ec.EffectivePersonality = ch.Personality
			ec.EffectiveStatus = ch.Status
		}
		result = append(result, ec)
	}
	return result, nil
}

// UpsertChapterCharacter 创建或更新章节级角色覆盖
func (s *CharacterService) UpsertChapterCharacter(novelID, chapterID, characterID uint, req *model.UpsertChapterCharacterRequest) (*model.ChapterCharacter, error) {
	if s.chapterCharacterRepo == nil {
		return nil, fmt.Errorf("chapter character repository not configured")
	}
	cc := &model.ChapterCharacter{
		CharacterID: characterID,
		ChapterID:   chapterID,
		NovelID:     novelID,
		Appearance:  req.Appearance,
		Personality: req.Personality,
		Status:      req.Status,
		Location:    req.Location,
		Notes:       req.Notes,
	}
	if err := s.chapterCharacterRepo.Upsert(cc); err != nil {
		return nil, err
	}
	saved, err := s.chapterCharacterRepo.GetByChapterAndCharacter(chapterID, characterID)
	if err != nil {
		return cc, nil
	}
	return saved, nil
}

// DeleteChapterCharacter 删除章节级角色覆盖（回退到项目级）
func (s *CharacterService) DeleteChapterCharacter(chapterID, characterID uint) error {
	if s.chapterCharacterRepo == nil {
		return fmt.Errorf("chapter character repository not configured")
	}
	return s.chapterCharacterRepo.Delete(chapterID, characterID)
}

func (s *CharacterService) GenerateProfile(tenantID uint, novelID uint, description string) (*model.Character, error) {
	prompt := fmt.Sprintf("根据以下描述生成小说角色档案：%s\n以JSON格式返回：{\"name\":\"角色名\",\"role\":\"protagonist/antagonist/supporting\",\"background\":\"背景故事\",\"appearance\":\"外貌描述\",\"personality\":\"性格特点\"}", description)
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "character_profile", prompt, "")
	if err != nil {
		return nil, err
	}

	var profile struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Background  string `json:"background"`
		Appearance  string `json:"appearance"`
		Personality string `json:"personality"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &profile); err != nil {
		return &model.Character{
			UUID:       uuid.New().String(),
			NovelID:    novelID,
			Name:       "AI生成角色",
			Role:       "supporting",
			Background: result,
			Status:     "active",
		}, nil
	}
	return &model.Character{
		UUID:       uuid.New().String(),
		NovelID:    novelID,
		Name:       profile.Name,
		Role:       profile.Role,
		Background: profile.Background,
		Appearance: profile.Appearance,
		Status:     "active",
	}, nil
}

func (s *CharacterService) AnalyzeConsistency(id uint, images []string) (interface{}, error) {
	return map[string]interface{}{
		"character_id":      id,
		"consistency_score": 0.9,
		"images_analyzed":   len(images),
	}, nil
}

// ============================================
// ImageGenerationService 图像生成服务
// ============================================

type ImageGenerationService struct {
	aiService *AIService
}

func NewImageGenerationService(aiService *AIService) *ImageGenerationService {
	return &ImageGenerationService{aiService: aiService}
}

type GeneratedCharacterImage struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

func (s *ImageGenerationService) GenerateCharacterImage(req *model.GenerateImageRequest) (*GeneratedCharacterImage, error) {
	options := &ImageGenerationOptions{
		Prompt:  fmt.Sprintf("%s, %s, %s style", req.Subject, req.Description, req.Style),
		Size:    "512x512",
		Steps:   50,
		CFGScale: 7.5,
	}
	image, err := s.aiService.GenerateImage(options.Prompt, options)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: image.URL, Description: req.Description}, nil
}

// ============================================
// StoryboardService 分镜服务（handler-facing）
// ============================================

type StoryboardService struct {
	videoService *VideoService
	aiService    *AIService
}

func NewStoryboardService(videoService *VideoService, aiService *AIService) *StoryboardService {
	return &StoryboardService{videoService: videoService, aiService: aiService}
}

func (s *StoryboardService) GenerateStoryboard(videoID, chapterID uint, characters []string, style string) (interface{}, error) {
	var chapterIDPtr *uint
	if chapterID != 0 {
		chapterIDPtr = &chapterID
	}
	shots, err := s.videoService.GenerateStoryboard(videoID, chapterIDPtr)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"video_id":   videoID,
		"chapter_id": chapterID,
		"shots":      shots,
		"total":      len(shots),
	}, nil
}

func (s *StoryboardService) AnalyzeEmotions(content string) (interface{}, error) {
	prompt := fmt.Sprintf("请分析以下内容的情感曲线：\n%s", content)
	result, err := s.aiService.Generate(0, "analysis", prompt)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"analysis": result,
		"content":  content[:min(100, len(content))],
	}, nil
}

// ============================================
// QualityControlService adapter methods
// ============================================

// CheckChapter handler-compatible wrapper — delegates to the real AI+rule-based check.
func (s *QualityControlService) CheckChapter(id uint) (*QualityReport, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("chapter %d not found: %w", id, err)
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, fmt.Errorf("novel %d not found: %w", chapter.NovelID, err)
	}
	return s.CheckChapterQuality(context.Background(), chapter, novel)
}

// ============================================
// VideoEnhancementService adapter methods
// ============================================

// EnhanceVideo handler-compatible wrapper (accepts model.EnhancementConfig)
func (s *VideoEnhancementService) EnhanceVideo(videoURL string, enhancements []model.EnhancementConfig) (interface{}, error) {
	configs := make([]EnhancementConfig, 0, len(enhancements))
	for _, e := range enhancements {
		configs = append(configs, EnhancementConfig{
			Type:      EnhancementType(e.Type),
			Enabled:   e.Enabled,
			Intensity: e.Intensity,
		})
	}
	return s.EnhanceVideoWithConfigs(videoURL, configs)
}

// GetRecommendations handler-compatible wrapper
func (s *VideoEnhancementService) GetRecommendations(fps int, resolution string, duration int, style string) (interface{}, error) {
	return map[string]interface{}{
		"fps":        fps,
		"resolution": resolution,
		"duration":   duration,
		"style":      style,
		"recommendations": []map[string]interface{}{
			{"type": "frame_interpolation", "priority": "high", "reason": "提升流畅度"},
			{"type": "super_resolution", "priority": "medium", "reason": "提升画质"},
		},
	}, nil
}

// ============================================
// CharacterArcService adapter methods
// ============================================

func (s *CharacterArcService) GetAllArcs(novelID uint) (interface{}, error) {
	characters, err := s.charRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	arcs := make([]interface{}, 0, len(characters))
	for _, c := range characters {
		arc, _ := s.GetCharacterArc(novelID, c.ID)
		arcs = append(arcs, arc)
	}
	return arcs, nil
}

func (s *CharacterArcService) UpdateArc(novelID, characterID uint, currentStage int, note string) (interface{}, error) {
	arc, err := s.GetCharacterArc(novelID, characterID)
	if err != nil {
		return nil, err
	}
	arc.CurrentStage = currentStage
	return arc, nil
}

// ============================================
// GenerationContextService adapter methods
// ============================================

func (s *GenerationContextService) BuildGenerationPrompt(novelID uint, chapterNo int, style, extraPrompt string, maxContextLen int) (string, error) {
	ctx, err := s.GetContext(novelID, chapterNo)
	if err != nil {
		return "", err
	}
	var sc *StyleConfig
	if style != "" {
		sc = &StyleConfig{NarrativeVoice: style}
	}
	prompt := s.buildGenerationPrompt(ctx, chapterNo, sc, extraPrompt)
	return prompt, nil
}

func (s *GenerationContextService) GetContextPreview(novelID uint) (interface{}, error) {
	ctx, err := s.GetContext(novelID, 0)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"novel_id":      novelID,
		"total_context": fmt.Sprintf("%d chars", len(ctx.GlobalSummary)),
		"summary":       ctx.GlobalSummary,
	}, nil
}

// ============================================
// StyleService adapter methods
// ============================================

func (s *StyleService) GetDefaultStyle() (*StyleConfig, error) {
	return s.getDefaultStyleConfig(), nil
}

func (s *StyleService) BuildStylePrompt(req interface{}) (string, error) {
	cfg := s.getDefaultStyleConfig()
	// Use encoding/json to copy compatible fields
	if data, err := json.Marshal(req); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	return s.buildStylePromptInternal(cfg), nil
}

func (s *StyleService) GetStylePresets() interface{} {
	return []map[string]interface{}{
		{"name": "literary", "description": "文学风格"},
		{"name": "commercial", "description": "商业小说风格"},
		{"name": "young_adult", "description": "青春小说风格"},
	}
}

func (s *StyleService) ApplyPreset(name string) (interface{}, error) {
	presets := map[string]*StyleConfig{
		"literary": {
			NarrativeVoice:     "third_limited",
			EmotionalTone:      "cold",
			SentenceComplexity: "complex",
			DescriptionDensity: "rich",
		},
		"commercial": {
			NarrativeVoice:     "third_omniscient",
			EmotionalTone:      "warm",
			SentenceComplexity: "simple",
			DescriptionDensity: "moderate",
		},
		"young_adult": {
			NarrativeVoice:     "first_person",
			EmotionalTone:      "warm",
			SentenceComplexity: "simple",
			DescriptionDensity: "minimal",
		},
	}
	style, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("preset %s not found", name)
	}
	return style, nil
}

// ============================================
// ModelService adapter methods
// ============================================

func (s *ModelService) ListProviders(tenantID uint) (interface{}, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	return providers, err
}

func (s *ModelService) GetProvider(id uint, tenantID uint) (*model.ModelProvider, error) {
	return s.providerRepo.GetByIDAndTenant(id, tenantID)
}

func (s *ModelService) CreateProvider(req *model.CreateModelProviderRequest, tenantID uint) (*model.ModelProvider, error) {
	provider := &model.ModelProvider{
		TenantID:    tenantID,
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Type:        req.Type,
		APIEndpoint: req.APIEndpoint,
		APIKey:      req.APIKey,
		APIVersion:  req.APIVersion,
		IsActive:    req.IsActive,
	}
	return provider, s.providerRepo.Create(provider)
}

func (s *ModelService) UpdateProvider(id uint, tenantID uint, req *model.UpdateModelProviderRequest) (*model.ModelProvider, error) {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		provider.Name = req.Name
	}
	if req.DisplayName != "" {
		provider.DisplayName = req.DisplayName
	}
	if req.APIEndpoint != "" {
		provider.APIEndpoint = req.APIEndpoint
	}
	if req.APIKey != "" {
		provider.APIKey = req.APIKey
	}
	if req.APIVersion != "" {
		provider.APIVersion = req.APIVersion
	}
	if req.IsActive != nil {
		provider.IsActive = *req.IsActive
	}
	return provider, s.providerRepo.Update(provider)
}

func (s *ModelService) DeleteProvider(id uint, tenantID uint) error {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return err
	}
	// 系统级 provider（tenant_id=0）不允许租户删除
	if provider.TenantID != tenantID {
		return fmt.Errorf("cannot delete system-level provider")
	}
	return s.providerRepo.Delete(id)
}

func (s *ModelService) TestProvider(id uint, tenantID uint) (interface{}, error) {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return nil, err
	}
	if s.aiService != nil {
		if _, loadErr := s.aiService.getTenantProvider(tenantID, provider.Name); loadErr != nil {
			return map[string]interface{}{"status": "error", "error": loadErr.Error(), "provider_id": id}, nil
		}
	}
	return map[string]interface{}{"status": "ok", "provider_id": id}, nil
}

func (s *ModelService) ListModels(providerID *uint) (interface{}, error) {
	models, err := s.modelRepo.List(providerID)
	return models, err
}

func (s *ModelService) CreateModel(req *model.CreateAIModelRequest) (*model.AIModel, error) {
	m := &model.AIModel{
		ProviderID:    req.ProviderID,
		Name:          req.Name,
		SuitableTasks: req.TaskTypes,
		MaxTokens:     req.MaxTokens,
		CostPer1K:     req.CostPer1K,
		IsActive:      true,
	}
	return m, s.modelRepo.Create(m)
}

func (s *ModelService) UpdateModel(id uint, req *model.UpdateAIModelRequest) (*model.AIModel, error) {
	m, err := s.modelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		m.Name = req.Name
	}
	if req.MaxTokens != 0 {
		m.MaxTokens = req.MaxTokens
	}
	if req.CostPer1K != 0 {
		m.CostPer1K = req.CostPer1K
	}
	return m, s.modelRepo.Update(m)
}

func (s *ModelService) DeleteModel(id uint) error {
	return s.modelRepo.Delete(id)
}


func (s *ModelService) TestModel(id uint) (interface{}, error) {
	return map[string]interface{}{"status": "ok", "model_id": id}, nil
}

func (s *ModelService) GetTaskConfig(taskType string) (interface{}, error) {
	cfg, err := s.taskRepo.GetByTaskType(taskType)
	return cfg, err
}

func (s *ModelService) UpdateTaskConfig(taskType string, req *model.UpdateTaskConfigRequest) (interface{}, error) {
	cfg, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		return nil, err
	}
	if req.PrimaryModelID != 0 {
		cfg.PrimaryModelID = req.PrimaryModelID
	}
	if req.MaxTokens != 0 {
		cfg.MaxTokens = req.MaxTokens
	}
	if req.Temperature != 0 {
		cfg.Temperature = req.Temperature
	}
	if req.TopP != 0 {
		cfg.TopP = req.TopP
	}
	return cfg, s.taskRepo.Update(cfg)
}

func (s *ModelService) ListExperiments() (interface{}, error) {
	experiments, err := s.experimentRepo.List(100)
	return experiments, err
}

func (s *ModelService) CreateExperiment(req *model.CreateModelComparisonRequest) (interface{}, error) {
	experiment := &model.ModelComparisonExperiment{
		Name:     req.Name,
		TaskType: req.TaskType,
		Status:   "pending",
	}
	return experiment, s.experimentRepo.Create(experiment)
}

func (s *ModelService) GetExperiment(id uint) (interface{}, error) {
	return s.experimentRepo.GetByID(id)
}

func (s *ModelService) StartExperiment(id uint) error {
	return nil
}

func (s *ModelService) GetAvailableModels(taskType string) ([]*model.AIModel, error) {
	return s.modelRepo.GetAvailableByTaskType(taskType)
}

func (s *ModelService) SelectModel(taskType, strategy string) (*model.AIModel, error) {
	models, err := s.modelRepo.GetAvailableByTaskType(taskType)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no available models for task type: %s", taskType)
	}
	switch strategy {
	case "quality":
		return selectByQuality(models), nil
	case "cost":
		return selectByCost(models), nil
	default:
		return selectBalanced(models), nil
	}
}

// ============================================
// ForeshadowService adapter methods
// ============================================

func (s *ForeshadowService) GetForeshadows(novelID uint, chapterNo int) ([]*ForeshadowItem, error) {
	return s.CheckForeshadowStatus(novelID, chapterNo)
}

func (s *ForeshadowService) MarkFulfilledByID(novelID, foreshadowID, chapterID uint) error {
	chapter := &model.Chapter{ID: chapterID}
	return s.MarkFulfilled(novelID, foreshadowID, chapter)
}

// ============================================
// TimelineService adapter methods
// ============================================

func (s *TimelineService) GetTimeline(novelID uint) (*Timeline, error) {
	return s.BuildTimeline(novelID)
}

// ============================================
// WorldviewService
// ============================================

type WorldviewService struct {
	worldviewRepo *repository.WorldviewRepository
	aiService     *AIService
}

func NewWorldviewService(worldviewRepo *repository.WorldviewRepository, aiService *AIService) *WorldviewService {
	return &WorldviewService{worldviewRepo: worldviewRepo, aiService: aiService}
}

func (s *WorldviewService) CreateWorldview(worldview *model.Worldview) error {
	return s.worldviewRepo.Create(worldview)
}

func (s *WorldviewService) GetWorldview(id uint) (*model.Worldview, error) {
	return s.worldviewRepo.GetByID(id)
}

func (s *WorldviewService) ListWorldviews(page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	return s.worldviewRepo.List(page, pageSize, genre)
}

func (s *WorldviewService) UpdateWorldview(worldview *model.Worldview) error {
	return s.worldviewRepo.Update(worldview)
}

func (s *WorldviewService) DeleteWorldview(id uint) error {
	return s.worldviewRepo.Delete(id)
}

// Entity CRUD

func (s *WorldviewService) GetEntities(worldviewID uint) ([]*model.WorldviewEntity, error) {
	return s.worldviewRepo.GetEntities(worldviewID)
}

func (s *WorldviewService) GetEntity(id uint) (*model.WorldviewEntity, error) {
	var entity model.WorldviewEntity
	if err := s.worldviewRepo.DB().First(&entity, id).Error; err != nil {
		return nil, err
	}
	return &entity, nil
}

func (s *WorldviewService) CreateEntity(entity *model.WorldviewEntity) error {
	return s.worldviewRepo.CreateEntity(entity)
}

func (s *WorldviewService) UpdateEntity(entity *model.WorldviewEntity) error {
	return s.worldviewRepo.UpdateEntity(entity)
}

func (s *WorldviewService) DeleteEntity(id uint) error {
	return s.worldviewRepo.DeleteEntity(id)
}

// GenerateWorldview AI生成世界观
func (s *WorldviewService) GenerateWorldview(tenantID uint, genre string, hints []string) (*model.Worldview, error) {
	prompt := fmt.Sprintf("请为%s类型的小说生成一个完整的世界观设定。", genre)
	if len(hints) > 0 {
		prompt += fmt.Sprintf("参考提示：%s。", strings.Join(hints, "、"))
	}
	prompt += "\n以JSON格式返回：{\"magic_system\":\"修炼体系描述\",\"geography\":\"地理环境描述\",\"culture\":\"文化背景描述\"}"

	result, err := s.aiService.GenerateWithProvider(tenantID, 0, "worldview", prompt, "")
	if err != nil {
		return nil, err
	}

	var data struct {
		MagicSystem string `json:"magic_system"`
		Geography   string `json:"geography"`
		Culture     string `json:"culture"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &data); err != nil {
		log.Printf("GenerateWorldview: failed to parse AI response: %v", err)
	}

	return &model.Worldview{
		UUID:        uuid.New().String(),
		Name:        genre + "世界观",
		Genre:       genre,
		MagicSystem: data.MagicSystem,
		Geography:   data.Geography,
		Culture:     data.Culture,
	}, nil
}

// ============================================
// ReviewTaskService
// ============================================

type ReviewTaskService struct {
	reviewTaskRepo *repository.ReviewTaskRepository
}

func NewReviewTaskService(reviewTaskRepo *repository.ReviewTaskRepository) *ReviewTaskService {
	return &ReviewTaskService{reviewTaskRepo: reviewTaskRepo}
}

func (s *ReviewTaskService) CreateTask(task *model.ReviewTask) error {
	return s.reviewTaskRepo.Create(task)
}

func (s *ReviewTaskService) GetTask(id uint) (*model.ReviewTask, error) {
	return s.reviewTaskRepo.GetByID(id)
}

func (s *ReviewTaskService) ListPendingTasks(priority string, limit int) ([]*model.ReviewTask, error) {
	return s.reviewTaskRepo.ListPending(priority, limit)
}

func (s *ReviewTaskService) UpdateTaskStatus(id uint, status, note string) error {
	return s.reviewTaskRepo.UpdateStatus(id, status, note)
}

// ============================================
// TenantService
// ============================================

type TenantService struct {
	tenantRepo     *repository.TenantRepository
	tenantUserRepo *repository.TenantUserRepository
}

func NewTenantService(tenantRepo *repository.TenantRepository, tenantUserRepo *repository.TenantUserRepository) *TenantService {
	return &TenantService{tenantRepo: tenantRepo, tenantUserRepo: tenantUserRepo}
}

func (s *TenantService) ListTenants(page, pageSize int) ([]*model.Tenant, int64, error) {
	return s.tenantRepo.List(page, pageSize)
}

func (s *TenantService) GetTenant(id uint) (*model.Tenant, error) {
	return s.tenantRepo.GetByID(id)
}

func (s *TenantService) CreateTenant(tenant *model.Tenant) error {
	return s.tenantRepo.Create(tenant)
}

func (s *TenantService) UpdateTenant(tenant *model.Tenant) error {
	return s.tenantRepo.Update(tenant)
}

func (s *TenantService) DeleteTenant(id uint) error {
	return s.tenantRepo.Delete(id)
}

func (s *TenantService) GetQuota(id uint) (interface{}, error) {
	tenant, err := s.tenantRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	quota := tenant.GetQuota()
	return map[string]interface{}{
		"tenant_id":  tenant.ID,
		"max_users":  quota.MaxUsers,
		"used_users": tenant.UsedUsers,
	}, nil
}

func (s *TenantService) ListMembers(tenantID uint) ([]*model.TenantUser, error) {
	return s.tenantUserRepo.ListByTenant(tenantID)
}

func (s *TenantService) AddMember(tenantID, userID uint, role string) error {
	tu := &model.TenantUser{
		TenantID: tenantID,
		UserID:   userID,
		Role:     role,
		Status:   "active",
	}
	return s.tenantUserRepo.Create(tu)
}

func (s *TenantService) RemoveMember(tenantID, userID uint) error {
	return s.tenantUserRepo.DeleteByTenantAndUser(tenantID, userID)
}

func (s *TenantService) UpdateMemberRole(tenantID, userID uint, role string) error {
	return s.tenantUserRepo.UpdateRole(tenantID, userID, role)
}
