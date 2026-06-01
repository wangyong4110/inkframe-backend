package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)


// ============================================
// ChapterService 章节服务
// ============================================

type ChapterService struct {
	chapterRepo    *repository.ChapterRepository
	novelRepo      *repository.NovelRepository
	characterRepo  *repository.CharacterRepository                       // 注入角色数据到生成 prompt
	snapshotRepo   *repository.CharacterStateSnapshotRepository          // 角色状态快照（注入连贯状态）
	versionRepo    *repository.ChapterVersionRepository                  // 章节版本（手动编辑自动存档）
	aiService      *AIService
	contextSvc     *GenerationContextService
	narrativeSvc   *NarrativeMemoryService // 层次化记忆 + 摘要 + 标题 + 精修
	continuitySvc  *ContinuityService      // 可选：章节连贯性检查
	hookSvc        *HookChainService
	spSvc          *SatisfactionPointService
	arcSvc         *ConflictArcService
	plotPointRepo  *repository.PlotPointRepository // 未解决剧情点注入
	mcpService     *McpService                     // 可选：用于联网搜索 MCP 工具
	notifSvc       *NotificationService            // 可选：用于章节生成完成通知
	skillRepo      *repository.SkillRepository     // 可选：用于将技能体系注入生成上下文
	qualitySvc     *QualityControlService          // 可选：用于生成后质量评分与触发精修
	knowledgeSvc   *KnowledgeService               // 可选：用于异步提取并存储剧情点
	timelineSvc    *TimelineService                // 可选：时间线约束注入生成 prompt

	// genLocks 防止同一章节并发生成（key: "novelID-chapterNo"）。
	// DB 层的 AtomicSetGenerating 负责已存在占位章节的乐观锁保护；
	// 此 sync.Map 负责进程内还未写入 DB 的并发请求保护。
	genLocks sync.Map
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

// WithVersionRepo 注入章节版本仓库（可选，注入后手动编辑内容时自动存版本）
func (s *ChapterService) WithVersionRepo(repo *repository.ChapterVersionRepository) *ChapterService {
	s.versionRepo = repo
	return s
}

// WithContinuityService 注入连贯性检查服务（可选，注入后章节生成完成后自动异步检查）
func (s *ChapterService) WithContinuityService(svc *ContinuityService) *ChapterService {
	s.continuitySvc = svc
	return s
}

// WithNarrativeMemory 注入层次化记忆服务（可选）
func (s *ChapterService) WithNarrativeMemory(svc *NarrativeMemoryService) *ChapterService {
	s.narrativeSvc = svc
	return s
}

// WithCharacterRepo 注入角色仓库（可选），用于将 DB 中的角色信息注入生成 prompt
func (s *ChapterService) WithCharacterRepo(repo *repository.CharacterRepository) *ChapterService {
	s.characterRepo = repo
	return s
}

// WithSnapshotRepo 注入角色状态快照仓库（可选），用于将最新角色状态注入生成 prompt
func (s *ChapterService) WithSnapshotRepo(repo *repository.CharacterStateSnapshotRepository) *ChapterService {
	s.snapshotRepo = repo
	return s
}

// WithPlotPointRepo 注入剧情点仓库（可选），用于将未解决的伏笔/冲突注入生成 prompt
func (s *ChapterService) WithPlotPointRepo(repo *repository.PlotPointRepository) *ChapterService {
	s.plotPointRepo = repo
	return s
}

// WithMcpService 注入 MCP 服务（可选），用于联网搜索工具调用
func (s *ChapterService) WithMcpService(mcp *McpService) *ChapterService {
	s.mcpService = mcp
	return s
}

// WithNotificationService 注入通知服务（可选），用于章节生成完成后发送站内通知
func (s *ChapterService) WithNotificationService(svc *NotificationService) *ChapterService {
	s.notifSvc = svc
	return s
}

// WithSkillRepo 注入技能仓库（可选），用于将技能体系注入生成上下文
func (s *ChapterService) WithSkillRepo(repo *repository.SkillRepository) *ChapterService {
	s.skillRepo = repo
	return s
}

// WithQualityService 注入质量控制服务（可选），生成后自动评分并触发精修
func (s *ChapterService) WithQualityService(svc *QualityControlService) *ChapterService {
	s.qualitySvc = svc
	return s
}

// WithKnowledgeService 注入知识库服务（可选），章节生成后异步提取并存储剧情点
func (s *ChapterService) WithKnowledgeService(svc *KnowledgeService) *ChapterService {
	s.knowledgeSvc = svc
	return s
}

// WithTimelineService 注入时间线服务（可选），用于将时间线约束注入生成 prompt
func (s *ChapterService) WithTimelineService(svc *TimelineService) *ChapterService {
	s.timelineSvc = svc
	return s
}

// WithDramaticServices 注入戏剧张力服务（可选）
func (s *ChapterService) WithDramaticServices(hookSvc *HookChainService, spSvc *SatisfactionPointService, arcSvc *ConflictArcService) *ChapterService {
	s.hookSvc = hookSvc
	s.spSvc = spSvc
	s.arcSvc = arcSvc
	return s
}

// GetDefaultProviderName 返回默认 AI provider 名称
func (s *ChapterService) GetDefaultProviderName() string {
	return s.aiService.GetDefaultProviderName()
}

// syncNovelStats refreshes chapter_count and total_words on the novel (best-effort).
func (s *ChapterService) syncNovelStats(novelID uint) {
	if err := s.novelRepo.SyncStats(novelID); err != nil {
		logger.Printf("syncNovelStats: novelID=%d: %v", novelID, err)
	}
}

func (s *ChapterService) CreateChapter(novelID uint, req *model.CreateChapterRequest) (*model.Chapter, error) {
	var tenantID uint
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		tenantID = novel.TenantID
	}
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		NovelID:   novelID,
		TenantID:  tenantID,
		ChapterNo: req.ChapterNo,
		Title:     req.Title,
		Content:   req.Content,
		WordCount: countChineseChars(req.Content),
		Status:    "completed",
	}
	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}
	s.syncNovelStats(novelID)
	return chapter, nil
}

func (s *ChapterService) GetChapter(id, tenantID uint) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if chapter.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}
	return chapter, nil
}

func (s *ChapterService) ListChapters(novelID uint) ([]*model.Chapter, error) {
	return s.chapterRepo.ListByNovel(novelID)
}

// ListChaptersPaged returns a page of chapter metadata for a novel.
func (s *ChapterService) ListChaptersPaged(novelID uint, page, pageSize int) ([]*model.Chapter, int64, error) {
	return s.chapterRepo.ListByNovelPaged(novelID, page, pageSize)
}

// applyChapterUpdate patches non-zero request fields onto a chapter in place.
func applyChapterUpdate(chapter *model.Chapter, req *model.UpdateChapterRequest) {
	if req.Title != "" {
		chapter.Title = req.Title
	}
	if req.Content != "" {
		chapter.Content = req.Content
		chapter.WordCount = countChineseChars(req.Content)
	}
	if req.ChapterHook != "" {
		chapter.ChapterHook = req.ChapterHook
	}
	if req.Outline != "" {
		chapter.Outline = req.Outline
	}
}

func (s *ChapterService) UpdateChapter(id, tenantID uint, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if chapter.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}
	// Snapshot current content before overwriting (best-effort, ignore errors).
	if req.Content != "" && chapter.Content != "" && s.versionRepo != nil {
		latest, _ := s.versionRepo.GetLatest(chapter.ID)
		nextNo := 1
		if latest != nil {
			nextNo = latest.VersionNo + 1
		}
		if err := s.versionRepo.Create(&model.ChapterVersion{
			ChapterID:  chapter.ID,
			VersionNo:  nextNo,
			Content:    chapter.Content,
			ChangeType: "manual_edit",
		}); err != nil {
			logger.Printf("[ChapterService] create version failed: %v", err)
		}
	}
	applyChapterUpdate(chapter, req)
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, err
	}
	if req.Content != "" {
		s.syncNovelStats(chapter.NovelID)
	}
	return chapter, nil
}

func (s *ChapterService) DeleteChapter(id, tenantID uint) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("not found")
	}
	if chapter.TenantID != tenantID {
		return fmt.Errorf("not found")
	}
	if err := s.chapterRepo.DeleteAndRenumber(id, chapter.NovelID); err != nil {
		return err
	}
	// Clean up character state snapshots that reference this chapter.
	if s.snapshotRepo != nil {
		if delErr := s.snapshotRepo.DeleteByChapterID(id); delErr != nil {
			logger.Printf("[ChapterService] DeleteChapter: delete snapshots for chapter %d: %v", id, delErr)
		}
	}
	s.syncNovelStats(chapter.NovelID)
	return nil
}

// BatchDeleteChapters deletes multiple chapters by ID, verifying novelID and tenantID ownership.
// Chapters are deleted one by one to ensure renumbering consistency after each deletion.
func (s *ChapterService) BatchDeleteChapters(ctx context.Context, novelID, tenantID uint, ids []uint) error {
	for _, id := range ids {
		chapter, err := s.chapterRepo.GetByID(id)
		if err != nil {
			// Skip chapters that don't exist
			continue
		}
		// Verify the chapter belongs to the requested novel and tenant
		if chapter.NovelID != novelID || chapter.TenantID != tenantID {
			continue
		}
		if err := s.chapterRepo.DeleteAndRenumber(id, chapter.NovelID); err != nil {
			return fmt.Errorf("failed to delete chapter %d: %w", id, err)
		}
	}
	s.syncNovelStats(novelID)
	return nil
}

func (s *ChapterService) GetChapterByNo(novelID uint, chapterNo int) (*model.Chapter, error) {
	return s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
}

func (s *ChapterService) UpdateChapterByNo(novelID uint, chapterNo int, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}
	applyChapterUpdate(chapter, req)
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, err
	}
	if req.Content != "" {
		s.syncNovelStats(novelID)
	}
	return chapter, nil
}

// GenerateChapterOutline 用 AI 为指定章节生成大纲（概述性文字，非场景 JSON）
func (s *ChapterService) GenerateChapterOutline(tenantID, novelID uint, chapterNo int, extraPrompt string) (*model.Chapter, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}

	// 构建上下文：近期章节摘要
	var recentCtx string
	if s.narrativeSvc != nil {
		if ctx, err := s.narrativeSvc.BuildHierarchicalContext(novelID, chapterNo); err == nil {
			recentCtx = ctx
		}
	}

	recentCtxSection := ""
	if recentCtx != "" {
		recentCtxSection = "叙事上下文：\n" + recentCtx
	}
	extraPromptSection := ""
	if extraPrompt != "" {
		extraPromptSection = "补充要求：" + extraPrompt
	}

	prompt := fmt.Sprintf(`请为小说《%s》第%d章生成详细的章节大纲（100～300字）。

小说简介：%s
章节标题：%s
%s
%s

要求：
- 详细描述本章的核心情节脉络与关键转折
- 点明主要人物的行动、目标与心理变化
- 交代场景背景与氛围
- 说明本章在整体故事中的作用（推进、铺垫或高潮）
- 字数不少于100字，不超过300字
- 直接输出大纲文本，不要加前缀或说明`,
		novel.Title, chapterNo, novel.Description, chapter.Title,
		recentCtxSection, extraPromptSection,
	)

	// 从项目配置读取参数默认值
	chOutlineOverrides := StoryboardOverrides{
		MaxTokens:      novel.MaxTokens,
		Temperature:    novel.Temperature,
		TimeoutSeconds: novel.TimeoutSeconds,
	}
	outline, err := s.aiService.GenerateWithProvider(tenantID, novelID, "chapter_outline", prompt, "", chOutlineOverrides)
	if err != nil {
		return nil, err
	}

	chapter.Outline = strings.TrimSpace(outline)
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, err
	}
	return chapter, nil
}

func (s *ChapterService) DeleteChapterByNo(novelID uint, chapterNo int) error {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return err
	}
	if err := s.chapterRepo.DeleteAndRenumber(chapter.ID, novelID); err != nil {
		return err
	}
	s.syncNovelStats(novelID)
	return nil
}

// PublishChapter 将章节标记为已发布（不改变内容 status）
func (s *ChapterService) PublishChapter(novelID uint, chapterNo int) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}
	if err := s.chapterRepo.UpdateIsPublished(chapter.ID, novelID, true); err != nil {
		return nil, err
	}
	chapter.IsPublished = true
	if err := s.novelRepo.SyncPublishedCount(novelID); err != nil {
		logger.Printf("[ChapterService] SyncPublishedCount failed: %v", err)
	}
	return chapter, nil
}

// UnpublishChapter 取消章节发布（不改变内容 status）
func (s *ChapterService) UnpublishChapter(novelID uint, chapterNo int) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}
	if err := s.chapterRepo.UpdateIsPublished(chapter.ID, novelID, false); err != nil {
		return nil, err
	}
	chapter.IsPublished = false
	if err := s.novelRepo.SyncPublishedCount(novelID); err != nil {
		logger.Printf("[ChapterService] SyncPublishedCount failed: %v", err)
	}
	return chapter, nil
}

// BatchPublishChapters 批量发布小说所有章节到广场
func (s *ChapterService) BatchPublishChapters(novelID uint) (int64, error) {
	return s.chapterRepo.BatchUpdateIsPublished(novelID, true)
}

// ListPublishedChapters 获取小说已公开发布的章节列表
func (s *ChapterService) ListPublishedChapters(novelID uint) ([]*model.Chapter, error) {
	return s.chapterRepo.ListPublishedByNovel(novelID)
}

// GenerateChapter 专业级章节生成流水线：
//
//  Step 1  构建层次化上下文（近章详摘 + 弧摘要 + 全局概述）
//  Step 2  生成场景大纲（3-5 个场景，含节拍、情绪、钩子）
//  Step 3  按场景大纲生成完整章节内容
//  Step 4  存储章节（包含场景大纲、叙事元数据）
//  Step 5  异步后处理：摘要生成、标题生成、精修、角色状态提取、弧摘要触发
func (s *ChapterService) GenerateChapter(tenantID uint, novelID uint, req *model.GenerateChapterRequest) (*model.Chapter, error) {
	logger.Printf("[ChapterService] GenerateChapter: tenantID=%d novelID=%d chapterNo=%d", tenantID, novelID, req.ChapterNo)
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}

	// ── 并发保护：防止同一章节被同时触发两次生成 ────────────────────────────────────────
	// 1. 进程内 sync.Map 锁：对于尚未写入 DB 的并发请求，在内存层面排斥。
	genLockKey := fmt.Sprintf("%d-%d", novelID, req.ChapterNo)
	if _, loaded := s.genLocks.LoadOrStore(genLockKey, struct{}{}); loaded {
		return nil, fmt.Errorf("chapter %d of novel %d is already being generated", req.ChapterNo, novelID)
	}
	defer s.genLocks.Delete(genLockKey)

	// 2. DB 层乐观锁：如果已有占位章节，仅当其 status 不是 generating/completed 时才允许开始。
	if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil {
		ok, atomicErr := s.chapterRepo.AtomicSetGenerating(existing.ID, novelID)
		if atomicErr != nil {
			return nil, fmt.Errorf("chapter %d: check generating status: %w", req.ChapterNo, atomicErr)
		}
		if !ok {
			return nil, fmt.Errorf("chapter %d of novel %d is already being generated or completed", req.ChapterNo, novelID)
		}
	}

	// ── Step 0: 确保角色数据存在（防止主角漂移的关键前置步骤）───────────────────────────
	// 若 DB 中无角色记录，自动从小说简介中提取并写入，确保后续每章都有固定主角锚点。
	s.ensureProtagonistExtracted(tenantID, novel)

	// ── Step 0b: 等待弧摘要完成（防止上一弧摘要还在异步生成时就开始新章节）─────────────
	// If the previous chapter was the last chapter of an arc, wait for its arc summary to complete
	// so that BuildHierarchicalContext can include it.
	const arcSizeConst = 10
	if req.ChapterNo > 1 && s.narrativeSvc != nil {
		prevNo := req.ChapterNo - 1
		if prevNo%arcSizeConst == 0 {
			arcNo := prevNo / arcSizeConst
			s.narrativeSvc.WaitForArcSummary(novelID, arcNo, 30*time.Second)
		}
	}

	// ── Step 1: 层次化上下文 ──────────────────────────────
	globalCtx := s.buildGlobalContext(novelID, req.ChapterNo, novel)

	// 自动检测最终章：当前章节号达到小说目标章节数时，确保故事完整收尾
	// 用户也可通过 is_standalone=true 显式触发（如临时想提前收尾）
	if !req.IsStandalone && novel.TargetChapters > 0 && req.ChapterNo >= novel.TargetChapters {
		req.IsStandalone = true
	}

	// 从小说大纲获取本章元数据（张力值、幕次、情感基调等）
	chapterMeta := s.extractChapterMeta(novelID, req.ChapterNo)

	// ── Step 1b: 联网参考搜索（可选）─────────────────────
	var refStories string
	if req.WebSearch && s.mcpService != nil {
		query := buildStorySearchQuery(novel.Genre, chapterMeta.summary)
		webCtx, webCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer webCancel()
		out, searchErr := s.mcpService.InvokeTool(webCtx, tenantID, "web_search", map[string]interface{}{
			"query":       query,
			"max_results": 3,
		})
		if searchErr == nil {
			refStories = parseWebSearchOutput(out)
			logger.Printf("[WebSearch] chapter %d: query=%q results=%d", req.ChapterNo, query, countWebSearchResults(out))
		} else {
			logger.Printf("[WebSearch] chapter %d: skipped: %v", req.ChapterNo, searchErr)
		}
	}

	// ── Step 1c: 百科知识查询（可选）─────────────────────
	var wikiContext string
	if req.WikiSearch && s.mcpService != nil {
		query := buildWikiSearchQuery(novel.Genre, chapterMeta.summary)
		wikiCtx, wikiCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer wikiCancel()
		out, searchErr := s.mcpService.InvokeTool(wikiCtx, tenantID, "wiki_search", map[string]interface{}{
			"query":       query,
			"max_results": 3,
		})
		if searchErr == nil {
			wikiContext = parseWikiOutput(out)
			logger.Printf("[WikiSearch] chapter %d: query=%q", req.ChapterNo, query)
		} else {
			logger.Printf("[WikiSearch] chapter %d: skipped: %v", req.ChapterNo, searchErr)
		}
	}

	// ── Step 1d: 情节模板查询（可选）─────────────────────
	var storyPatternRef string
	if req.UseStoryPattern && s.mcpService != nil {
		patternCtx, patternCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer patternCancel()
		out, searchErr := s.mcpService.InvokeTool(patternCtx, tenantID, "story_pattern", map[string]interface{}{
			"genre":       novel.Genre,
			"archetype":   chapterMeta.emotionalTone,
			"max_results": 2,
		})
		if searchErr == nil {
			storyPatternRef = parseStoryPatternOutput(out)
			logger.Printf("[StoryPattern] chapter %d: genre=%q", req.ChapterNo, novel.Genre)
		} else {
			logger.Printf("[StoryPattern] chapter %d: skipped: %v", req.ChapterNo, searchErr)
		}
	}

	// ── Step 2: 生成场景大纲 ──────────────────────────────
	// prevEnding 在此处统一计算，避免后续两步各查一次 DB
	prevEnding := s.getPreviousChapterEnding(novelID, req.ChapterNo)
	sceneOutlineJSON, suggestedTitle, outlineErr := s.generateSceneOutline(
		tenantID, novelID, req, novel, globalCtx, chapterMeta, refStories, wikiContext, storyPatternRef, prevEnding,
	)
	if outlineErr != nil {
		// Fix 1+2: 将预置占位章节（如存在）标记为 failed，避免状态卡在 "generating"
		if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil && existing.Status == "generating" {
			existing.Status = "failed"
			if updateErr := s.chapterRepo.Update(existing); updateErr != nil {
				logger.Printf("[ChapterService] failed to set chapter %d status=failed: %v", existing.ID, updateErr)
			}
		}
		return nil, fmt.Errorf("generate scene outline failed: %w", outlineErr)
	}

	// ── Step 3: 按场景大纲生成章节内容 ───────────────────
	content, chapterHook, err := s.generateFromSceneOutline(
		tenantID, novelID, req, novel, sceneOutlineJSON, globalCtx, chapterMeta, refStories, wikiContext, prevEnding,
	)
	if err != nil {
		// Fix 1: 将预置占位章节（如存在）标记为 failed，避免状态卡在 "generating"
		if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil && existing.Status == "generating" {
			existing.Status = "failed"
			if updateErr := s.chapterRepo.Update(existing); updateErr != nil {
				logger.Printf("[ChapterService] failed to set chapter %d status=failed: %v", existing.ID, updateErr)
			}
		}
		return nil, err
	}

	// ── Content length validation ──────────────────────────────────────────────
	if len([]rune(content)) < 100 {
		if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil && existing.Status == "generating" {
			existing.Status = "failed"
			_ = s.chapterRepo.Update(existing)
		}
		return nil, fmt.Errorf("generated content too short (%d chars), expected at least 100 chars", len([]rune(content)))
	}

	// ── Step 4: 存储章节 (upsert: update if placeholder exists) ──────────────
	title := suggestedTitle
	if title == "" {
		title = chapterMeta.chapterTitle // 大纲中的预设标题
	}
	if title == "" {
		title = fmt.Sprintf("第%d章", req.ChapterNo)
	}
	var chapter *model.Chapter
	if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil {
		existing.Title = title
		existing.Content = content
		existing.WordCount = countChineseChars(content)
		existing.SceneOutline = sceneOutlineJSON
		existing.TensionLevel = chapterMeta.tensionLevel
		existing.ActNo = chapterMeta.actNo
		existing.EmotionalTone = chapterMeta.emotionalTone
		existing.HookType = chapterMeta.hookType
		existing.ChapterHook = chapterHook
		existing.Status = "generating"
		if err := s.chapterRepo.Update(existing); err != nil {
			return nil, err
		}
		chapter = existing
	} else {
		chapter = &model.Chapter{
			UUID:          uuid.New().String(),
			NovelID:       novelID,
			TenantID:      novel.TenantID,
			ChapterNo:     req.ChapterNo,
			Title:         title,
			Content:       content,
			WordCount:     countChineseChars(content),
			SceneOutline:  sceneOutlineJSON,
			TensionLevel:  chapterMeta.tensionLevel,
			ActNo:         chapterMeta.actNo,
			EmotionalTone: chapterMeta.emotionalTone,
			HookType:      chapterMeta.hookType,
			ChapterHook:   chapterHook,
			Status:        "generating",
		}
		if err := s.chapterRepo.Create(chapter); err != nil {
			return nil, err
		}
	}

	s.syncNovelStats(novelID)

	// ── Step 5: 同步生成摘要（必须在返回前完成，供下一章上下文使用）───────────────────────────────
	if s.narrativeSvc != nil && chapter.Summary == "" {
		if summary, err := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); err == nil {
			chapter.Summary = summary
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				logger.Printf("[ChapterService] GenerateChapter: update chapter %d [摘要]: %v", chapter.ID, updateErr)
			}
		} else {
			logger.Printf("[ChapterService] GenerateChapter: summary ch%d: %v", chapter.ChapterNo, err)
		}
	}

	// ── Step 5b: 同步提取角色快照（必须在返回前完成，下一章生成时依赖主角当前状态）──────────────
	// 注意：使用正确的 tenantID（非 0），确保租户自定义 AI 提供商可以被选中。
	novelSvcForSnapshot := &NovelService{novelRepo: s.novelRepo, chapterRepo: s.chapterRepo, aiService: s.aiService}
	if s.characterRepo != nil {
		novelSvcForSnapshot.characterRepo = s.characterRepo
	}
	if s.snapshotRepo != nil {
		novelSvcForSnapshot.snapshotRepo = s.snapshotRepo
	}
	novelSvcForSnapshot.writeCharacterSnapshots(tenantID, chapter)

	// Mark chapter as completed now that all synchronous steps are done.
	chapter.Status = "completed"
	if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
		logger.Printf("[ChapterService] GenerateChapter: update chapter %d [status=completed]: %v", chapter.ID, updateErr)
	}

	// ── Step 6: 异步后处理（标题/精修/弧摘要，不再包含角色快照）────────────────────────────────
	go s.postProcessChapter(tenantID, chapter, novel)

	// 站内通知：章节生成完成
	if s.notifSvc != nil {
		_ = s.notifSvc.Send(
			novel.TenantID, 0,
			"chapter_done",
			"章节生成完成",
			fmt.Sprintf("《%s》第%d章已生成完毕", novel.Title, chapter.ChapterNo),
			"chapter", chapter.ID,
			fmt.Sprintf("/novel/%d", novel.ID),
		)
	}

	logger.Printf("[ChapterService] GenerateChapter done: chapterID=%d wordCount=%d", chapter.ID, chapter.WordCount)
	return chapter, nil
}

// chapterOutlineMeta 从小说大纲中提取的章节叙事元数据
type chapterOutlineMeta struct {
	tensionLevel  int
	actNo         int
	emotionalTone string
	hookType      string
	summary       string   // 大纲中的章节概述
	chapterTitle  string   // 大纲中的章节标题建议
	plotPoints    []string // 大纲中的章节剧情点
}

func (s *ChapterService) extractChapterMeta(novelID uint, chapterNo int) chapterOutlineMeta {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return chapterOutlineMeta{}
	}

	meta := chapterOutlineMeta{}

	// 优先从 novel.Outline JSON 中解析完整元数据（含剧情点、钩子、张力值等）
	outlineJSON := novel.Outline
	if outlineJSON == "" {
		outlineJSON = novel.StylePrompt // 向后兼容
	}
	if outlineJSON != "" {
		var outline struct {
			Chapters []struct {
				ChapterNo     int      `json:"chapter_no"`
				Title         string   `json:"title"`
				TensionLevel  int      `json:"tension_level"`
				Act           int      `json:"act"`
				EmotionalTone string   `json:"emotional_tone"`
				HookType      string   `json:"hook_type"`
				Hook          string   `json:"hook"`
				Summary       string   `json:"summary"`
				PlotPoints    []string `json:"plot_points"`
			} `json:"chapters"`
		}
		if err := json.Unmarshal([]byte(outlineJSON), &outline); err == nil {
			for _, ch := range outline.Chapters {
				if ch.ChapterNo == chapterNo {
					meta.tensionLevel  = ch.TensionLevel
					meta.actNo         = ch.Act
					meta.emotionalTone = ch.EmotionalTone
					meta.hookType      = ch.HookType
					if meta.hookType == "" {
						meta.hookType = ch.Hook
					}
					meta.summary       = ch.Summary
					meta.chapterTitle  = ch.Title
					meta.plotPoints    = ch.PlotPoints
					break
				}
			}
		}
	}

	// 降级：从已有章节占位记录中读取 Title 和 Summary（由 AI 分析阶段写入）
	if meta.summary == "" || meta.chapterTitle == "" {
		if existing, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo); err == nil && existing != nil {
			if meta.summary == "" {
				meta.summary = existing.Summary
			}
			if meta.chapterTitle == "" {
				meta.chapterTitle = existing.Title
			}
		}
	}

	return meta
}

// ensureProtagonistExtracted 确保 DB 中至少有一个角色（含主角）。
// 若角色表为空，通过 AI 从小说简介中提取主角信息并写入 DB，防止主角每章漂移。
func (s *ChapterService) ensureProtagonistExtracted(tenantID uint, novel *model.Novel) {
	if s.characterRepo == nil {
		return
	}
	chars, err := s.characterRepo.ListByNovel(novel.ID)
	if err != nil || len(chars) > 0 {
		return
	}

	logger.Printf("[ChapterService] ensureProtagonistExtracted: no characters for novel %d, auto-extracting…", novel.ID)

	desc := novel.Description
	if desc == "" {
		desc = novel.Title + "（" + novel.Genre + "类型小说）"
	}

	prompt := fmt.Sprintf(`从以下小说简介中提取主要角色（必须包含主角），最多3个，以JSON数组格式返回。
《%s》（%s类型）
%s

只返回JSON数组，格式：
[{"name":"角色名","role":"protagonist","description":"角色核心特征一句话描述"}]
role只能是：protagonist / antagonist / supporting
注意：必须有且仅有一个protagonist`,
		novel.Title, novel.Genre, truncateForPrompt(desc, 800))

	result, aiErr := s.aiService.GenerateWithProvider(tenantID, novel.ID, "character_extract_mini", prompt, "")
	if aiErr != nil {
		logger.Printf("[ChapterService] ensureProtagonistExtracted: AI error: %v", aiErr)
		return
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var extracted []struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
	}
	if jsonErr := json.Unmarshal([]byte(cleaned), &extracted); jsonErr != nil {
		logger.Printf("[ChapterService] ensureProtagonistExtracted: parse error: %v (raw: %.200s)", jsonErr, cleaned)
		return
	}

	for _, c := range extracted {
		role := c.Role
		if role != "protagonist" && role != "antagonist" && role != "supporting" {
			role = "supporting"
		}
		char := &model.Character{
			UUID:        uuid.New().String(),
			NovelID:     novel.ID,
			TenantID:    novel.TenantID,
			Name:        c.Name,
			Role:        role,
			Description: c.Description,
			Status:      "active",
		}
		if createErr := s.characterRepo.Create(char); createErr != nil {
			logger.Printf("[ChapterService] ensureProtagonistExtracted: create %s: %v", c.Name, createErr)
		}
	}
	logger.Printf("[ChapterService] ensureProtagonistExtracted: created %d characters for novel %d", len(extracted), novel.ID)
}

// buildGlobalContext 构建层次化全局上下文（优先使用 NarrativeMemoryService）
func (s *ChapterService) buildGlobalContext(novelID uint, chapterNo int, novel *model.Novel) string {
	// 优先使用层次化记忆
	if s.narrativeSvc != nil {
		ctx, err := s.narrativeSvc.BuildHierarchicalContext(novelID, chapterNo)
		if err == nil && ctx != "" {
			return ctx
		}
		logger.Printf("GenerateChapter: hierarchical context failed: %v — fallback", err)
	}
	// 降级到原 GenerationContextService
	if s.contextSvc != nil {
		ctx, err := s.contextSvc.BuildGenerationPrompt(novelID, chapterNo, "", "", 8000)
		if err == nil {
			return ctx
		}
	}
	// 最终降级：直接从 repo 拼装基础上下文，确保主角和近章信息不丢失
	return s.buildMinimalContext(novelID, chapterNo, novel)
}

// buildMinimalContext 在所有上下文服务均失败时，直接从 repo 拼装最小可用上下文。
// 保证主角信息和近3章摘要不因服务失败而丢失。
func (s *ChapterService) buildMinimalContext(novelID uint, chapterNo int, novel *model.Novel) string {
	var sb strings.Builder
	sb.WriteString("【故事概要】\n")
	sb.WriteString(novel.Description)

	if s.characterRepo != nil {
		if chars, err := s.characterRepo.ListByNovel(novelID); err == nil && len(chars) > 0 {
			sb.WriteString("\n\n【主要角色】\n")
			for _, c := range chars {
				prefix := "- "
				if isProtagonistRole(c.Role) {
					prefix = "⚠️【主角】"
				}
				sb.WriteString(fmt.Sprintf("%s%s（%s）：%s\n", prefix, c.Name, c.Role, c.Description))
				if isProtagonistRole(c.Role) && s.snapshotRepo != nil {
					if snap, err := s.snapshotRepo.GetLatestForCharacter(c.ID); err == nil && snap != nil {
						if state := formatCharacterState(snap); state != "" {
							sb.WriteString("  → 当前状态：" + state + "\n")
						}
					}
				}
			}
		}
	}

	if s.chapterRepo != nil && chapterNo > 1 {
		if recent, err := s.chapterRepo.GetRecent(novelID, chapterNo, 3); err == nil && len(recent) > 0 {
			sb.WriteString("\n【近期章节】\n")
			for i := len(recent) - 1; i >= 0; i-- {
				ch := recent[i]
				sum := ch.Summary
				if sum == "" && ch.Content != "" {
					runes := []rune(ch.Content)
					start := len(runes) - 200
					if start < 0 {
						start = 0
					}
					sum = "（章末）…" + string(runes[start:])
				}
				sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, sum))
			}
		}
	}

	// 注入技能体系
	if s.skillRepo != nil {
		if skills, err := s.skillRepo.List(novelID); err == nil && len(skills) > 0 {
			sb.WriteString("\n\n## 技能体系\n")
			for _, sk := range skills {
				sb.WriteString(fmt.Sprintf("【%s】(%s Lv.%d) %s\n", sk.Name, sk.SkillType, sk.Level, sk.Description))
			}
		}
	}

	return sb.String()
}

// generateSceneOutline 调用 AI 生成场景级大纲，返回 JSON 字符串和建议标题
func (s *ChapterService) generateSceneOutline(
	tenantID, novelID uint,
	req *model.GenerateChapterRequest,
	novel *model.Novel,
	globalCtx string,
	meta chapterOutlineMeta,
	refStories string,
	wikiContext string,
	storyPatternRef string,
	prevEnding string,
) (sceneOutlineJSON, suggestedTitle string, outlineErr error) {

	// 构建伏笔提示
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	// 获取角色列表（含快照状态 + 内在动机）
	characters := s.getCharactersForPrompt(novelID)

	// 计算章节叙事预算（结构位置约束）
	budget := computeChapterBudget(req.ChapterNo, novel.TargetChapters, meta.actNo)
	budgetText := formatBudgetForPrompt(budget)

	// 构建角色注册表（防命名混淆）
	characterRegistry := s.buildCharacterRegistry(novelID)

	// 获取剧情张力状态（供场景大纲决策参考）
	plotTensionState := ""
	if s.narrativeSvc != nil {
		plotTensionState = s.narrativeSvc.BuildPlotTensionStateText(novelID, req.ChapterNo)
	}
	// 注入戏剧上下文（钩子链、爽点、冲突弧）
	if s.hookSvc != nil {
		if ctx := s.hookSvc.GetInjectionContext(novelID, req.ChapterNo); ctx != "" {
			if plotTensionState != "" {
				plotTensionState += "\n\n"
			}
			plotTensionState += ctx
		}
	}
	if s.spSvc != nil {
		if ctx := s.spSvc.GetInjectionContext(novelID, req.ChapterNo); ctx != "" {
			if plotTensionState != "" {
				plotTensionState += "\n\n"
			}
			plotTensionState += ctx
		}
	}
	if s.arcSvc != nil {
		if ctx := s.arcSvc.GetInjectionContext(novelID, req.ChapterNo); ctx != "" {
			if plotTensionState != "" {
				plotTensionState += "\n\n"
			}
			plotTensionState += ctx
		}
	}

	hookType := meta.hookType
	if hookType == "" {
		if req.IsStandalone {
			hookType = "大结局" // 独立故事：圆满/震撼收尾，不留悬念
		} else {
			hookType = "cliffhanger"
		}
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

	// 将大纲剧情点格式化为文本注入 prompt
	plotPointsText := ""
	if len(meta.plotPoints) > 0 {
		var sb strings.Builder
		for _, pp := range meta.plotPoints {
			sb.WriteString("- ")
			sb.WriteString(pp)
			sb.WriteString("\n")
		}
		plotPointsText = sb.String()
	}

	// 获取时间线约束（仅注入与当前章节相近的事件）
	var timelineContext string
	if s.timelineSvc != nil {
		if timeline, tlErr := s.timelineSvc.GetTimeline(novelID); tlErr == nil && timeline != nil {
			timelineContext = s.timelineSvc.FormatTimelineForPrompt(timeline, req.ChapterNo)
		}
	}

	outlinePrompt, err := renderPrompt("chapter_scene_outline", map[string]interface{}{
		"NovelTitle":            novel.Title,
		"ChapterNo":             req.ChapterNo,
		"ChapterTitle":          meta.chapterTitle,
		"GlobalContext":         globalCtx,
		"ChapterSummary":        chapterSummary,
		"PlotPoints":            plotPointsText,
		"TensionLevel":          tensionLevel,
		"ActNo":                 actNo,
		"EmotionalTone":         emotionalTone,
		"HookType":              hookType,
		"IsStandalone":          req.IsStandalone,
		"PreviousChapterEnding": prevEnding,
		"Characters":            characters,
		"ForeshadowHints":       foreshadowHints,
		"PlotTensionState":      plotTensionState,
		"RefStories":            refStories,
		"WikiContext":           wikiContext,
		"StoryPatternRef":       storyPatternRef,
		"ChapterBudget":         budgetText,
		"CharacterRegistry":     characterRegistry,
		"TimelineContext":        timelineContext,
	})
	if err != nil {
		logger.Printf("GenerateChapter: render chapter_scene_outline: %v", err)
		return "", "", fmt.Errorf("generateSceneOutline: render template: %w", err)
	}

	outlineCtx, outlineCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	resp, err := s.aiService.GenerateWithProviderCtx(outlineCtx, tenantID, novelID, "scene_outline", outlinePrompt, req.ModelOverride, buildChapterOverrides(req, novel))
	outlineCancel()
	if err != nil {
		logger.Printf("GenerateChapter: scene outline AI call failed: %v", err)
		return "", "", fmt.Errorf("generateSceneOutline: AI call failed: %w", err)
	}

	resp = extractJSON(strings.TrimSpace(resp))

	// 提取建议标题，并校验场景数量
	var outlineResult struct {
		ChapterTitle string            `json:"chapter_title"`
		Scenes       []json.RawMessage `json:"scenes"`
	}
	if err := json.Unmarshal([]byte(resp), &outlineResult); err == nil {
		suggestedTitle = outlineResult.ChapterTitle
	}

	sceneCount := len(outlineResult.Scenes)
	logger.Printf("[ChapterService] generateSceneOutline: chapterNo=%d scenes=%d", req.ChapterNo, sceneCount)

	// 场景数量越界检查：AI 返回 0 场景说明结构出错，返回错误触发降级；
	// 超过 7 个场景时截断到 5，防止正文过长或 token 超限。
	if sceneCount < 1 {
		return "", "", fmt.Errorf("generateSceneOutline: AI returned 0 scenes, expected 3-5 (chapterNo=%d)", req.ChapterNo)
	}
	if sceneCount > 7 {
		logger.Printf("[ChapterService] generateSceneOutline: chapterNo=%d scenes=%d >7, truncating to 5", req.ChapterNo, sceneCount)
		outlineResult.Scenes = outlineResult.Scenes[:5]
		// 重新序列化截断后的 JSON，确保后续步骤使用一致的数据
		if truncated, marshalErr := json.Marshal(outlineResult); marshalErr == nil {
			resp = string(truncated)
		}
	}

	return resp, suggestedTitle, nil
}

// generateFromSceneOutline 根据场景大纲生成章节正文
// 返回 (正文内容, 章末钩子, error)
// AI 输出中「【章末钩子】」标记后的内容会被提取为独立钩子字段
func (s *ChapterService) generateFromSceneOutline(
	tenantID, novelID uint,
	req *model.GenerateChapterRequest,
	novel *model.Novel,
	sceneOutlineJSON string,
	globalCtx string,
	meta chapterOutlineMeta,
	refStories string,
	wikiContext string,
	prevEnding string,
) (string, string, error) {

	// 章节目标字数：优先用显式 WordCount，其次从小说 TargetWordCount 推算，最后默认 3000
	// 注意：MaxTokens 是 LLM 上下文限制，与章节字数目标无关，不再用于此处
	wordCount := req.WordCount
	if wordCount <= 0 && novel.TargetWordCount > 0 {
		// novel.TargetWordCount 单位是"字"（原始字数），TargetChapters 是总章节数
		chapters := novel.TargetChapters
		if chapters <= 0 {
			chapters = 100
		}
		wordCount = novel.TargetWordCount / chapters
	}
	if wordCount <= 0 {
		wordCount = 3000
	}
	// 合理范围限制：单章 500-8000 字
	if wordCount < 500 {
		wordCount = 500
	}
	if wordCount > 8000 {
		wordCount = 8000
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
	if err := json.Unmarshal([]byte(sceneOutlineJSON), &outlineData); err != nil {
		logger.Printf("[ChapterService] generateFromSceneOutline: failed to parse scene outline JSON: %v", err)
	}
	logger.Printf("[ChapterService] generateFromSceneOutline: chapterNo=%d scenes=%d", req.ChapterNo, len(outlineData.Scenes))

	// 获取角色对话风格（同时包含状态快照 + 内在动机）
	characterVoices := s.getCharacterVoices(novelID)

	// 未解决剧情线（伏笔/冲突）
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	// 章节叙事预算（防信息过载、防过早化解矛盾）
	budget := computeChapterBudget(req.ChapterNo, novel.TargetChapters, meta.actNo)
	budgetText := formatBudgetForPrompt(budget)

	// 角色注册表（防命名混淆）
	characterRegistry := s.buildCharacterRegistry(novelID)

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
		content, err := s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
		return content, "", err
	}

	// 获取时间线约束（注入正文生成，与场景大纲保持一致）
	var timelineCtx string
	if s.timelineSvc != nil {
		if tl, tlErr := s.timelineSvc.GetTimeline(novelID); tlErr == nil && tl != nil {
			timelineCtx = s.timelineSvc.FormatTimelineForPrompt(tl, req.ChapterNo)
		}
	}

	chapterPrompt, err := renderPrompt("chapter_from_outline", map[string]interface{}{
		"NovelTitle":            novel.Title,
		"ChapterNo":             req.ChapterNo,
		"ChapterTitle":          chapterTitle,
		"WordCount":             wordCount,
		"GlobalContext":         globalCtx,
		"Scenes":                outlineData.Scenes,
		"HookSetup":             outlineData.HookSetup,
		"PeakTension":           peakTension,
		"Characters":            characterVoices,
		"ForeshadowHints":       foreshadowHints,
		"PreviousChapterEnding": prevEnding,
		"UserPrompt":            req.Prompt,
		"IsStandalone":          req.IsStandalone,
		"RefStories":            refStories,
		"WikiContext":           wikiContext,
		"ChapterBudget":         budgetText,
		"CharacterRegistry":     characterRegistry,
		"TimelineContext":        timelineCtx,
	})
	if err != nil {
		content, err := s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
		return content, "", err
	}

	var raw string
	var genErr error
	for attempt := 0; attempt < 3; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		raw, genErr = s.aiService.GenerateWithProviderCtx(attemptCtx, tenantID, novelID, "chapter", chapterPrompt, req.ModelOverride, buildChapterOverrides(req, novel))
		attemptCancel()
		if genErr == nil {
			break
		}
		logger.Printf("[ChapterService] generateFromSceneOutline: attempt %d failed: %v", attempt+1, genErr)
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
	}
	if genErr != nil {
		return "", "", genErr
	}
	raw = cleanChapterOutput(raw)
	content, hook := extractChapterHook(raw)
	logger.Printf("[ChapterService] generateFromSceneOutline done: chapterNo=%d contentLen=%d", req.ChapterNo, len(content))
	return content, hook, nil
}

// generateFallbackChapter 场景大纲失败时的降级生成
func (s *ChapterService) generateFallbackChapter(tenantID, novelID uint, req *model.GenerateChapterRequest, novel *model.Novel, globalCtx string) (string, error) {
	logger.Printf("GenerateChapter: using fallback (no scene outline) for novel %d ch %d", novelID, req.ChapterNo)
	wc := req.WordCount
	if wc <= 0 && novel.TargetWordCount > 0 {
		chapters := novel.TargetChapters
		if chapters <= 0 {
			chapters = 100
		}
		wc = novel.TargetWordCount / chapters
	}
	if wc <= 0 {
		wc = 3000
	}
	if wc < 500 {
		wc = 500
	}
	if wc > 8000 {
		wc = 8000
	}
	prompt := globalCtx + fmt.Sprintf("\n\n请为小说《%s》生成第%d章内容，字数约%d字。", novel.Title, req.ChapterNo, wc)
	if req.Prompt != "" {
		prompt += "\n\n创作要求：" + req.Prompt
	}
	raw, err := s.aiService.GenerateWithProvider(tenantID, novelID, "chapter", prompt, req.ModelOverride, buildChapterOverrides(req, novel))
	if err != nil {
		return "", err
	}
	return cleanChapterOutput(raw), nil
}

// postProcessChapter 异步后处理：生成摘要→生成标题→精修→提取角色状态→触发弧摘要
func (s *ChapterService) postProcessChapter(tenantID uint, chapter *model.Chapter, novel *model.Novel) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("[ChapterService] postProcessChapter panic recovered: %v\n%s", r, debug.Stack())
		}
	}()
	logger.Printf("[ChapterService] postProcessChapter start: chapterID=%d no=%d", chapter.ID, chapter.ChapterNo)

	// Fetch a fresh copy from DB to avoid mutating the caller's pointer concurrently.
	if fresh, err := s.chapterRepo.GetByID(chapter.ID); err != nil {
		logger.Printf("[ChapterService] postProcessChapter: fetch fresh chapter %d failed: %v", chapter.ID, err)
		return
	} else {
		chapter = fresh
	}
	// 1. 生成摘要（最重要：供后续章节的上下文使用）
	// Retry up to 3 times with 1s/2s delays to ensure summary is available for subsequent chapters.
	if s.narrativeSvc != nil && chapter.Summary == "" {
		var summaryText string
		for attempt := 0; attempt < 3; attempt++ {
			if generated, err := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); err == nil {
				summaryText = generated
				break
			} else if attempt < 2 {
				logger.Printf("postProcess: summary ch%d attempt %d failed: %v, retrying", chapter.ChapterNo, attempt+1, err)
				time.Sleep(time.Duration(attempt+1) * time.Second)
			} else {
				logger.Printf("postProcess: summary ch%d attempt %d failed: %v", chapter.ChapterNo, attempt+1, err)
			}
		}
		if summaryText != "" {
			chapter.Summary = summaryText
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				logger.Printf("postProcessChapter: update chapter %d [摘要]: %v", chapter.ID, updateErr)
			}
		} else {
			logger.Printf("[ChapterService] WARNING: chapter %d has no summary after 3 attempts", chapter.ChapterNo)
		}
	}

	// 2. 如果标题仍是"第N章"，生成创意标题
	defaultTitle := fmt.Sprintf("第%d章", chapter.ChapterNo)
	if s.narrativeSvc != nil && chapter.Title == defaultTitle && chapter.Summary != "" {
		if title, err := s.narrativeSvc.GenerateChapterTitle(tenantID, chapter, novel.Genre, chapter.EmotionalTone); err == nil && title != "" {
			chapter.Title = fmt.Sprintf("第%d章 %s", chapter.ChapterNo, title)
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				logger.Printf("postProcessChapter: update chapter %d [标题]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 3. 精修（检测并修复重复词、AI惯用句等）
	if s.narrativeSvc != nil {
		if refined, err := s.narrativeSvc.RefineChapterContent(tenantID, chapter, novel.Title); err == nil && refined != chapter.Content {
			chapter.Content = refined
			chapter.WordCount = countChineseChars(refined)
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				logger.Printf("postProcessChapter: update chapter %d [精修]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 3b. Quality score gating: if OverallScore < threshold, attempt one extra refinement pass.
	// If still below threshold after refinement, mark chapter as low quality.
	if s.qualitySvc != nil && chapter.Content != "" {
		ctx := context.Background()
		if report, qErr := s.qualitySvc.CheckChapterQuality(ctx, chapter, novel); qErr == nil && !report.IsAcceptable() {
			logger.Printf("[ChapterService] postProcessChapter: ch%d quality score %.2f < threshold %.2f, triggering extra refinement",
				chapter.ChapterNo, report.OverallScore, MinAcceptableQualityScore)
			stillLow := true
			if s.narrativeSvc != nil {
				if refined, refErr := s.narrativeSvc.RefineChapterContent(tenantID, chapter, novel.Title); refErr == nil && refined != chapter.Content {
					chapter.Content = refined
					chapter.WordCount = countChineseChars(refined)
					if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
						logger.Printf("postProcessChapter: update chapter %d [quality-refinement]: %v", chapter.ID, updateErr)
					}
					// Re-check quality after refinement
					if report2, qErr2 := s.qualitySvc.CheckChapterQuality(ctx, chapter, novel); qErr2 == nil && report2.IsAcceptable() {
						stillLow = false
					}
				} else if refErr != nil {
					logger.Printf("postProcessChapter: quality-refinement ch%d: %v", chapter.ChapterNo, refErr)
				}
			}
			if stillLow {
				// Fetch fresh copy to avoid overwriting concurrent field updates
				if fresh, fetchErr := s.chapterRepo.GetByID(chapter.ID); fetchErr == nil {
					fresh.QualityStatus = "low"
					fresh.QualityIssues = report.SummarizeIssues()
					if updateErr := s.chapterRepo.Update(fresh); updateErr != nil {
						logger.Printf("postProcessChapter: update chapter %d [quality-status]: %v", chapter.ID, updateErr)
					} else {
						logger.Printf("[ChapterService] chapter %d saved with low quality status", chapter.ChapterNo)
					}
					chapter = fresh
				}
			}
		} else if qErr != nil {
			logger.Printf("[ChapterService] postProcessChapter: quality check ch%d failed (non-fatal): %v", chapter.ChapterNo, qErr)
		}
	}

	// 4. 连贯性检查（不阻塞主流程；结果持久化到 continuity_report 表供 UI 查询）
	// 若发现 high/critical 级别问题，在章节上打标记 continuity_blocked=true，
	// 前端据此提示用户审查，但不阻断生成流程。
	if s.continuitySvc != nil && chapter.Content != "" {
		go func(ch *model.Chapter) {
			report, err := s.continuitySvc.ValidateChapter(novel.ID, ch.ID, tenantID, ch.ChapterNo, ch.Content)
			if err != nil {
				logger.Printf("[ChapterService] continuity check ch%d: %v", ch.ChapterNo, err)
				return
			}
			// 检测是否存在高危/严重问题
			blocked := false
			for _, issue := range report.CharacterIssues {
				if issue.Severity == "high" || issue.Severity == "critical" {
					blocked = true
					break
				}
			}
			if !blocked {
				for _, issue := range report.WorldviewIssues {
					if issue.Severity == "high" || issue.Severity == "critical" {
						blocked = true
						break
					}
				}
			}
			if !blocked {
				for _, issue := range report.PlotIssues {
					if issue.Severity == "high" || issue.Severity == "critical" {
						blocked = true
						break
					}
				}
			}
			if blocked {
				// 从 DB 拉最新版本，避免覆盖并发写入的其他字段
				if fresh, fetchErr := s.chapterRepo.GetByID(ch.ID); fetchErr == nil {
					fresh.ContinuityBlocked = true
					if updateErr := s.chapterRepo.Update(fresh); updateErr != nil {
						logger.Printf("[ChapterService] continuity_blocked update ch%d: %v", ch.ChapterNo, updateErr)
					} else {
						logger.Printf("[ChapterService] continuity_blocked=true marked for ch%d (novel %d)", ch.ChapterNo, novel.ID)
					}
				}
			}
		}(chapter)
	}

	// 5. 触发弧摘要（每 arcSize 章触发一次）
	// 注：角色快照已在 GenerateChapter Step 5b 同步提取，此处不再重复。
	if s.narrativeSvc != nil {
		s.narrativeSvc.TriggerArcSummaryIfNeeded(tenantID, novel.ID, chapter.ChapterNo)
	}

	// 5b. 异步提取并存储本章剧情点（知识库）
	if s.knowledgeSvc != nil {
		go func() {
			ctx := context.Background()
			if err := s.knowledgeSvc.ExtractAndStorePlotPoints(ctx, chapter, nil); err != nil {
				logger.Printf("[ChapterService] ExtractAndStorePlotPoints failed for ch%d: %v", chapter.ChapterNo, err)
			}
		}()
	}

	// 5. 自动检查并标记已解决的剧情点（伏笔/冲突）
	s.checkAndAutoResolvePlotPoints(tenantID, chapter)
	logger.Printf("[ChapterService] postProcessChapter done: chapterID=%d", chapter.ID)
}

// checkAndAutoResolvePlotPoints 用单次 AI 调用判断本章是否解决了悬而未决的剧情线，自动更新 is_resolved
func (s *ChapterService) checkAndAutoResolvePlotPoints(tenantID uint, chapter *model.Chapter) {
	if s.plotPointRepo == nil || chapter.Content == "" {
		return
	}
	pps, err := s.plotPointRepo.ListByNovel(chapter.NovelID, "", true) // unresolved only
	if err != nil || len(pps) == 0 {
		return
	}
	// 最多取前5条 foreshadow/conflict/twist 进行检查
	var relevant []*model.PlotPoint
	for _, pp := range pps {
		if pp.Type == "foreshadow" || pp.Type == "conflict" || pp.Type == "twist" {
			relevant = append(relevant, pp)
		}
		if len(relevant) >= 5 {
			break
		}
	}
	if len(relevant) == 0 {
		return
	}

	// 构建精简 prompt
	var sb strings.Builder
	sb.WriteString("请分析以下章节内容摘录，判断哪些剧情线在本章中已明确解决（不再是悬念或未完结冲突）：\n\n")
	sb.WriteString("【章节内容摘录】\n")
	excerpt := []rune(chapter.Content)
	if len(excerpt) > 2000 {
		excerpt = excerpt[:2000]
	}
	sb.WriteString(string(excerpt))
	sb.WriteString("\n\n【待检查的剧情线】\n")
	for i, pp := range relevant {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, pp.Type, pp.Description))
	}
	sb.WriteString("\n只返回在本章中明确解决的序号，以JSON格式：{\"resolved_indices\":[1,3]}\n")
	sb.WriteString("若全部未解决则返回 {\"resolved_indices\":[]}")

	result, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "plot_resolution_check", sb.String(), "")
	if err != nil {
		logger.Printf("checkAndAutoResolvePlotPoints[%d]: AI error: %v", chapter.NovelID, err)
		return
	}

	var resp struct {
		ResolvedIndices []int `json:"resolved_indices"`
	}
	if err := json.Unmarshal([]byte(extractJSON(strings.TrimSpace(result))), &resp); err != nil {
		return
	}
	for _, idx := range resp.ResolvedIndices {
		if idx < 1 || idx > len(relevant) {
			continue
		}
		pp := relevant[idx-1]
		pp.IsResolved = true
		pp.ResolvedIn = &chapter.ID
		if err := s.plotPointRepo.Update(pp); err != nil {
			logger.Printf("checkAndAutoResolvePlotPoints: update pp#%d: %v", pp.ID, err)
		} else {
			desc := pp.Description
			if len([]rune(desc)) > 40 {
				desc = string([]rune(desc)[:40]) + "…"
			}
			logger.Printf("postProcess[novel=%d ch=%d]: auto-resolved plot point #%d [%s]: %s",
				chapter.NovelID, chapter.ChapterNo, pp.ID, pp.Type, desc)
		}
	}
}

// ──────────────────────────────────────────────
// Context helpers for GenerateChapter
// ──────────────────────────────────────────────

type characterForPrompt struct {
	Name          string
	Role          string
	IsProtagonist bool
	CurrentState  string // 来自最新状态快照：位置、健康、心情等
	Description   string
	InnerConflict string // 人物内在矛盾（如：渴望自由却害怕失去家人）
	CoreDesire    string // 核心渴望（如：被认可、复仇、保护所爱之人）
}

func (s *ChapterService) getCharactersForPrompt(novelID uint) []characterForPrompt {
	if s.characterRepo == nil {
		logger.Printf("[ChapterService] getCharactersForPrompt: characterRepo not wired, no character context injected")
		return nil
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		logger.Printf("[ChapterService] getCharactersForPrompt: ListByNovel error: %v", err)
		return nil
	}
	if len(chars) == 0 {
		logger.Printf("[ChapterService] getCharactersForPrompt: no characters found for novel %d", novelID)
		return nil
	}

	result := make([]characterForPrompt, 0, len(chars))
	for _, c := range chars {
		cp := characterForPrompt{
			Name:          c.Name,
			Role:          c.Role,
			IsProtagonist: isProtagonistRole(c.Role),
			Description:   c.Description,
			InnerConflict: c.InnerConflict,
			CoreDesire:    c.CoreDesire,
		}
		// 加载最新状态快照，补充 CurrentState
		if s.snapshotRepo != nil {
			if snap, snapErr := s.snapshotRepo.GetLatestForCharacter(c.ID); snapErr == nil && snap != nil {
				cp.CurrentState = formatCharacterState(snap)
			}
		}
		result = append(result, cp)
	}

	// 将主角排在列表最前，确保 AI 优先关注
	for i, cp := range result {
		if cp.IsProtagonist && i != 0 {
			result[0], result[i] = result[i], result[0]
			break
		}
	}
	return result
}

// formatCharacterState 将快照字段格式化为简短可读的状态描述，注入到生成 prompt
func formatCharacterState(snap *model.CharacterStateSnapshot) string {
	var parts []string
	if snap.Location != "" {
		parts = append(parts, "位于「"+snap.Location+"」")
	}
	if snap.Health != "" && snap.Health != "healthy" {
		parts = append(parts, "健康:"+snap.Health)
	}
	if snap.Mood != "" {
		parts = append(parts, "心情:"+snap.Mood)
	}
	if snap.Motivation != "" {
		runes := []rune(snap.Motivation)
		if len(runes) > 30 {
			runes = runes[:30]
		}
		parts = append(parts, "动机:"+string(runes))
	}
	if snap.PowerLevel > 0 {
		parts = append(parts, fmt.Sprintf("实力等级:%d", snap.PowerLevel))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "，")
}

func (s *ChapterService) getCharacterVoices(novelID uint) []characterForPrompt {
	// 同 getCharactersForPrompt，供 chapter_from_outline.j2 的 Characters 变量使用
	return s.getCharactersForPrompt(novelID)
}

// buildCharacterStateString 生成主角当前状态的结构化描述，优先注入场景大纲 prompt
// 这是防止主角漂移最重要的上下文

func (s *ChapterService) buildForeshadowHints(novelID uint, chapterNo int) string {
	var hints strings.Builder
	count := 0

	// 来源1：旧伏笔系统（ForeshadowService）
	if s.contextSvc != nil && s.contextSvc.foreshadowSvc != nil {
		foreshadows, err := s.contextSvc.foreshadowSvc.CheckForeshadowStatus(novelID, chapterNo)
		if err == nil {
			for _, fs := range foreshadows {
				if !fs.IsFulfilled && chapterNo-fs.ChapterNo >= 3 {
					hints.WriteString(fmt.Sprintf("- 请考虑回收伏笔：「%s」（第%d章埋设）\n", fs.Description, fs.ChapterNo))
					count++
					if count >= 3 {
						break
					}
				}
			}
		}
	}

	// 来源2：PlotPoint 表中未解决的伏笔与冲突（最多补充至5条）
	if s.plotPointRepo != nil && count < 5 {
		pps, err := s.plotPointRepo.ListByNovel(novelID, "", true) // unresolved only
		if err == nil {
			for _, pp := range pps {
				if count >= 5 {
					break
				}
				switch pp.Type {
				case "foreshadow":
					hints.WriteString(fmt.Sprintf("- 未回收伏笔：「%s」\n", pp.Description))
					count++
				case "conflict":
					hints.WriteString(fmt.Sprintf("- 进行中的冲突：「%s」\n", pp.Description))
					count++
				}
			}
		}
	}

	return hints.String()
}

// buildCharacterRegistry 构建已注册角色名称列表，注入 prompt 以防止命名混淆与角色分裂。
// AI 生成新角色时必须避免与表中已有名称重复或混淆。
func (s *ChapterService) buildCharacterRegistry(novelID uint) string {
	if s.characterRepo == nil {
		return ""
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil || len(chars) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("已注册角色（全书已出现/规划的命名角色，**新角色禁止使用相同或相似名字**）：\n")
	for _, c := range chars {
		roleLabel := c.Role
		sb.WriteString(fmt.Sprintf("- %s（%s）\n", c.Name, roleLabel))
	}
	sb.WriteString("\n⚠️ 若本章需要引入新配角，其名字必须与上表中所有名字明显不同，不得同音、近音或一字之差。\n")
	return sb.String()
}

func (s *ChapterService) getPreviousChapterEnding(novelID uint, chapterNo int) string {
	if chapterNo <= 1 {
		return "（本章为开篇）"
	}
	prev, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo-1)
	if err != nil || prev == nil {
		return ""
	}

	var sb strings.Builder

	// 1. 优先使用独立保存的章末钩子（直接悬念点）
	if prev.ChapterHook != "" {
		sb.WriteString("【章末情节】" + prev.ChapterHook)
	} else if prev.Summary != "" {
		sb.WriteString("【上章摘要】" + prev.Summary)
	} else {
		// 从内容末尾截取约300字
		content := []rune(prev.Content)
		if len(content) > 300 {
			content = content[len(content)-300:]
		}
		sb.WriteString("【上章结尾】…" + string(content))
	}

	// 2. 附加主角当前快照状态（防止主角位置/状态漂移）
	if s.characterRepo != nil && s.snapshotRepo != nil {
		chars, charErr := s.characterRepo.ListByNovel(novelID)
		if charErr == nil {
			for _, c := range chars {
				if isProtagonistRole(c.Role) {
					if snap, snapErr := s.snapshotRepo.GetLatestForCharacter(c.ID); snapErr == nil && snap != nil {
						state := formatCharacterState(snap)
						if state != "" {
							sb.WriteString(fmt.Sprintf("\n【主角「%s」当前状态】%s", c.Name, state))
						}
					}
					break
				}
			}
		}
	}

	return sb.String()
}

// extractChapterHook 从 AI 生成的原始内容中分离正文与章末钩子。
// AI 应在正文后输出「【章末钩子】」标记，之后的内容即为钩子正文。
func extractChapterHook(raw string) (content, hook string) {
	const marker = "【章末钩子】"
	idx := strings.Index(raw, marker)
	if idx < 0 {
		return strings.TrimSpace(raw), ""
	}
	content = strings.TrimSpace(raw[:idx])
	hook = strings.TrimSpace(raw[idx+len(marker):])
	return
}

var chapterHeaderRe = regexp.MustCompile(`^第[零一二三四五六七八九十百千\d]+章`)

// isProtagonistRole 判断角色是否为主角，兼容多种表述方式
func isProtagonistRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "protagonist", "主角", "hero", "heroine", "main_character", "lead", "主人公", "男主", "女主", "主角色":
		return true
	}
	return false
}

// cleanChapterOutput strips AI meta-content (preambles, outlines, trailing disclaimers)
// from raw chapter output, keeping only actual novel prose.
func cleanChapterOutput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	lines := strings.Split(raw, "\n")

	// Step 1: Find where chapter prose starts.
	// Prefer the first "第X章" line in the first 80 lines
	// (accept lines like "### 第一章 标题" by stripping markdown markers first).
	startLine := -1
	lookAhead := len(lines)
	if lookAhead > 80 {
		lookAhead = 80
	}
	for i := 0; i < lookAhead; i++ {
		t := strings.TrimSpace(lines[i])
		// Strip leading markdown heading markers (# ## ###...)
		stripped := strings.TrimLeft(t, "#")
		stripped = strings.TrimSpace(stripped)
		// Also strip bold/italic markers
		stripped = strings.TrimLeft(stripped, "*_")
		stripped = strings.TrimSpace(stripped)
		if chapterHeaderRe.MatchString(stripped) {
			startLine = i
			break
		}
	}
	// Fallback: skip contiguous leading meta-lines from the top.
	if startLine < 0 {
		for i, line := range lines {
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			if chapterLeadingMeta(t) {
				startLine = i + 1 // tentatively skip this line
				continue
			}
			// First non-meta, non-empty line — prose starts here.
			if startLine < 0 {
				startLine = i
			}
			break
		}
	}
	if startLine > 0 && startLine < len(lines) {
		lines = lines[startLine:]
	} else if startLine < 0 {
		return raw // nothing recognisable — return as-is
	}

	// Step 2: Strip trailing meta lines.
	endLine := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if chapterTrailingMeta(t) {
			endLine = i
		} else {
			break
		}
	}
	lines = lines[:endLine]

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// chapterLeadingMeta returns true for lines that are clearly AI preamble / outline items.
func chapterLeadingMeta(s string) bool {
	prefixes := []string{
		"好的", "当然", "非常抱歉", "很抱歉",
		"以下是", "下面是", "以下为", "下面为",
		"根据您", "根据以上", "根据提供",
		"接下来", "让我", "我来", "这是第",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	keywords := []string{"由于篇幅", "内容篇幅", "篇幅限制", "字数限制"}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	// Markdown headings (#) that are not "第X章"
	if strings.HasPrefix(s, "#") && !chapterHeaderRe.MatchString(strings.TrimLeft(s, "# ")) {
		return true
	}
	// Bullet / numbered list items (outline)
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
		return true
	}
	if len(s) > 2 && s[0] >= '1' && s[0] <= '9' && s[1] == '.' && s[2] == ' ' {
		return true
	}
	return false
}

// chapterTrailingMeta returns true for lines that are clearly trailing AI commentary.
func chapterTrailingMeta(s string) bool {
	keywords := []string{
		"如需续写", "请告知", "未完待续", "待续",
		"字数统计", "字数约", "写作建议", "创作说明",
		"以上为", "以上是", "以上内容", "以上片段",
		"（片段）", "（未完）", "(片段)", "(未完)",
		"由于篇幅", "篇幅限制", "内容篇幅",
		"后续章节", "下一章",
		"如果需要继续", "如果需要补充", "可以继续", "可根据您",
		"正文约", "约3000字", "分批补充", "分批生成",
		"后续可继续", "后续内容可",
	}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	if s == "---" || s == "===" || (len(s) >= 3 && strings.Count(s, "-") == len(s)) {
		return true
	}
	return false
}

func (s *ChapterService) RegenerateChapter(tenantID uint, id uint, prompt string) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if chapter.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
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

// BatchGenerateSummaries 对所有无摘要章节逐章并发生成摘要（3 并发）
func (s *ChapterService) BatchGenerateSummaries(tenantID, novelID uint, progressFn func(int)) (int, error) {
	logger.Printf("[ChapterService] BatchGenerateSummaries: novelID=%d", novelID)
	if s.narrativeSvc == nil {
		return 0, fmt.Errorf("narrative service not configured")
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return 0, fmt.Errorf("load chapters: %w", err)
	}

	novelTitle := "本小说"
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
		}
	}

	// 过滤需要生成摘要的章节（有内容但无摘要）
	var needSummary []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" && ch.Summary == "" {
			needSummary = append(needSummary, ch)
		}
	}
	if len(needSummary) == 0 {
		return 0, nil
	}
	logger.Printf("[ChapterService] BatchGenerateSummaries: found %d chapters without summary", len(needSummary))

	total := len(needSummary)
	const concurrency = 3
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var count, done int32

	for _, ch := range needSummary {
		ch := ch
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			summary, err := s.narrativeSvc.GenerateChapterSummary(tenantID, ch, novelTitle)
			if err != nil {
				logger.Printf("BatchGenerateSummaries: ch%d: %v", ch.ChapterNo, err)
			} else {
				ch.Summary = strings.TrimSpace(summary)
				if err := s.chapterRepo.Update(ch); err != nil {
					logger.Printf("BatchGenerateSummaries: save ch%d: %v", ch.ChapterNo, err)
				} else {
					atomic.AddInt32(&count, 1)
				}
			}
			cur := int(atomic.AddInt32(&done, 1))
			if progressFn != nil {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[ChapterService] BatchGenerateSummaries done: novelID=%d generated=%d", novelID, atomic.LoadInt32(&count))
	return int(atomic.LoadInt32(&count)), nil
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

// GetChapterVersion returns a specific version by its ID, verifying it belongs to chapterID.
func (s *ChapterVersionService) GetChapterVersion(chapterID, versionID uint) (*model.ChapterVersion, error) {
	versions, err := s.versionRepo.List(chapterID)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.ID == versionID {
			return v, nil
		}
	}
	return nil, fmt.Errorf("version not found")
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

// ──────────────────────────────────────────────
// WebSearch helpers
// ──────────────────────────────────────────────

// parseWebSearchOutput parses the output map from McpService.InvokeTool("web_search", …)
// into a human-readable prompt section.
func parseWebSearchOutput(output map[string]interface{}) string {
	rawResults, ok := output["results"]
	if !ok {
		return ""
	}
	// Marshal then unmarshal to []WebSearchResult
	b, err := json.Marshal(rawResults)
	if err != nil {
		return ""
	}
	var results []WebSearchResult
	if err := json.Unmarshal(b, &results); err != nil {
		return ""
	}
	return formatRefStories(results)
}

// countWebSearchResults returns the number of results from an InvokeTool output map.
func countWebSearchResults(output map[string]interface{}) int {
	rawResults, ok := output["results"]
	if !ok {
		return 0
	}
	switch v := rawResults.(type) {
	case []interface{}:
		return len(v)
	}
	return 0
}

