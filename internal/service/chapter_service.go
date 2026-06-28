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
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/redis/go-redis/v9"
)


// flexString 兼容 AI 偶发将字段返回为数字的情况（如 key_beat: 25 而非 "25%"）
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	// 先尝试字符串
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	// 降级：数字转字符串
	var n json.Number
	if err := json.Unmarshal(data, &n); err == nil {
		*f = flexString(n.String())
		return nil
	}
	return fmt.Errorf("flexString: cannot unmarshal %s", data)
}

// plotCoverageEntry 场景大纲的剧情点覆盖条目（KeyBeat 兼容字符串/数字）
type plotCoverageEntry struct {
	PlotPoint string     `json:"plot_point"`
	SceneNo   int        `json:"scene_no"`
	KeyBeat   flexString `json:"key_beat"`
}

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
	foreshadowRepo       *repository.ForeshadowRepository // 可选：伏笔生命周期注入生成 prompt
	chapterCharacterRepo *repository.ChapterCharacterRepository // 可选：章节角色级联清理
	chapterItemRepo      *repository.ChapterItemRepository      // 可选：章节道具级联清理

	cache    *redis.Client // optional: cross-instance chapter generation lock

	// genLocks 进程内去重（无 Redis 或 Redis 出错时的兜底）
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

// WithRedis enables cross-instance chapter generation deduplication via Redis SETNX.
func (s *ChapterService) WithRedis(c *redis.Client) *ChapterService {
	s.cache = c
	return s
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

// WithForeshadowRepo 注入伏笔仓库（可选），用于伏笔生命周期注入生成 prompt
func (s *ChapterService) WithForeshadowRepo(repo *repository.ForeshadowRepository) *ChapterService {
	s.foreshadowRepo = repo
	return s
}

func (s *ChapterService) WithChapterCharacterRepo(repo *repository.ChapterCharacterRepository) *ChapterService {
	s.chapterCharacterRepo = repo
	return s
}

func (s *ChapterService) WithChapterItemRepo(repo *repository.ChapterItemRepository) *ChapterService {
	s.chapterItemRepo = repo
	return s
}

// WithDramaticServices 注入戏剧张力服务（可选）
func (s *ChapterService) WithDramaticServices(hookSvc *HookChainService, spSvc *SatisfactionPointService, arcSvc *ConflictArcService) *ChapterService {
	s.hookSvc = hookSvc
	s.spSvc = spSvc
	s.arcSvc = arcSvc
	return s
}

// chapterBelongsToTenant verifies chapter ownership via novel.TenantID (novel-based ownership).
func (s *ChapterService) chapterBelongsToTenant(chapter *model.Chapter, tenantID uint) bool {
	if s.novelRepo == nil {
		return true
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return false
	}
	return novel.TenantID == 0 || novel.TenantID == tenantID
}

// GetDefaultProviderName 返回默认 AI provider 名称
func (s *ChapterService) GetDefaultProviderName() string {
	return s.aiService.GetDefaultProviderName()
}

// syncNovelStats refreshes chapter_count and total_words on the novel (best-effort).
func (s *ChapterService) syncNovelStats(novelID uint) {
	if err := s.novelRepo.SyncStats(novelID); err != nil {
		logger.Errorf("syncNovelStats: novelID=%d: %v", novelID, err)
	}
}

func (s *ChapterService) CreateChapter(novelID uint, req *model.CreateChapterRequest) (*model.Chapter, error) {
	const maxChapterContentBytes = 512 * 1024 // 512KB
	if len(req.Content) > maxChapterContentBytes {
		return nil, fmt.Errorf("chapter content too large (%d bytes, max 512KB)", len(req.Content))
	}
	var tenantID uint
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		tenantID = novel.TenantID
	}
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		TenantID:  tenantID,
		NovelID:   novelID,
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
	if !s.chapterBelongsToTenant(chapter, tenantID) {
		return nil, fmt.Errorf("not found")
	}
	return chapter, nil
}

func (s *ChapterService) ListChapters(novelID uint) ([]*model.Chapter, error) {
	return s.chapterRepo.ListByNovel(novelID)
}

// ListChaptersForExport 获取全部章节含正文，专用于导出。
func (s *ChapterService) ListChaptersForExport(novelID uint) ([]*model.Chapter, error) {
	return s.chapterRepo.ListByNovelWithContentUnlimited(novelID)
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
		chapter.ContentVersion++ // increment on every content write
	}
	if req.ChapterHook != "" {
		chapter.NarrativeMeta.ChapterHook = req.ChapterHook
	}
	if req.Outline != "" {
		chapter.NarrativeMeta.Outline = req.Outline
	}
}

func (s *ChapterService) UpdateChapter(id, tenantID uint, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if !s.chapterBelongsToTenant(chapter, tenantID) {
		return nil, fmt.Errorf("not found")
	}
	// Snapshot current content before overwriting (best-effort, ignore errors).
	if req.Content != "" && chapter.Content != "" && s.versionRepo != nil {
		if err := s.versionRepo.CreateAtomic(&model.ChapterVersion{
			NovelID:    chapter.NovelID,
			ChapterID:  chapter.ID,
			Content:    chapter.Content,
			ChangeType: "manual_edit",
		}); err != nil {
			logger.Errorf("[ChapterService] create version failed: %v", err)
		} else {
			_ = s.versionRepo.DeleteExcessVersions(chapter.ID, 20)
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
	if !s.chapterBelongsToTenant(chapter, tenantID) {
		return fmt.Errorf("not found")
	}
	if err := s.chapterRepo.DeleteAndRenumber(id, chapter.NovelID); err != nil {
		return err
	}
	// Clean up character state snapshots that reference this chapter.
	if s.snapshotRepo != nil {
		if delErr := s.snapshotRepo.DeleteByChapterID(id); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapter: delete snapshots for chapter %d: %v", id, delErr)
		}
	}
	// Clean up chapter versions.
	if s.versionRepo != nil {
		if delErr := s.versionRepo.DeleteByChapter(id); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapter: delete versions for chapter %d: %v", id, delErr)
		}
	}
	// Clean up chapter-level character and item overrides.
	if s.chapterCharacterRepo != nil {
		if delErr := s.chapterCharacterRepo.DeleteByChapter(id); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapter: delete chapter_characters for chapter %d: %v", id, delErr)
		}
	}
	if s.chapterItemRepo != nil {
		if delErr := s.chapterItemRepo.DeleteByChapter(id); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapter: delete chapter_items for chapter %d: %v", id, delErr)
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
		if chapter.NovelID != novelID || !s.chapterBelongsToTenant(chapter, tenantID) {
			continue
		}
		if err := s.chapterRepo.DeleteAndRenumber(id, chapter.NovelID); err != nil {
			return fmt.Errorf("failed to delete chapter %d: %w", id, err)
		}
		if s.versionRepo != nil {
			if delErr := s.versionRepo.DeleteByChapter(id); delErr != nil {
				logger.Errorf("[ChapterService] BatchDeleteChapters: delete versions for chapter %d: %v", id, delErr)
			}
		}
		if s.chapterCharacterRepo != nil {
			if delErr := s.chapterCharacterRepo.DeleteByChapter(id); delErr != nil {
				logger.Errorf("[ChapterService] BatchDeleteChapters: delete chapter_characters for chapter %d: %v", id, delErr)
			}
		}
		if s.chapterItemRepo != nil {
			if delErr := s.chapterItemRepo.DeleteByChapter(id); delErr != nil {
				logger.Errorf("[ChapterService] BatchDeleteChapters: delete chapter_items for chapter %d: %v", id, delErr)
			}
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

	extraPromptSection := ""
	if extraPrompt != "" {
		extraPromptSection = "【用户要求】\n" + extraPrompt
	}

	var prompt string
	if novel.AIConfig.ChapterMode == "independent" {
		// 独立成篇：每章是自完结的独立故事，不引用其他章节
		prompt = fmt.Sprintf(`请为小说集《%s》第%d章生成详细的章节大纲（200～500字）。

小说集简介：%s
本章标题：%s

%s

⚠️ 章节模式：独立成篇——本章是一个完整独立的故事，不依赖任何其他章节。

要求：
- 必须包含完整的故事弧光：①开场（人物处境/矛盾引入）②冲突升级（矛盾激化过程）③高潮（关键转折或对决）④结局（冲突如何解决，情感如何落地）
- 核心冲突必须在本章内完全解决，不留悬念、不依赖后续章节
- 点明主要人物的行动、目标与心理变化
- 交代场景背景与氛围
- 禁止：章末钩子、"待续"式结尾、跨章伏笔、引用其他章节的人物或事件
- 字数不少于200字，不超过500字
- 直接输出大纲文本，不要加前缀或说明`,
			novel.Title, chapterNo, novel.Meta.Description, chapter.Title,
			extraPromptSection,
		)
	} else {
		// 连贯剧情：构建近期章节上下文供情节衔接
		var recentCtx string
		if s.narrativeSvc != nil {
			if ctx, err := s.narrativeSvc.BuildHierarchicalContext(novelID, chapterNo); err == nil {
				recentCtx = ctx
			}
		}

		var prevOutlineSection string
		if allChapters, err := s.chapterRepo.ListByNovel(novelID); err == nil {
			var prevChapters []*model.Chapter
			for _, ch := range allChapters {
				if ch.ChapterNo < chapterNo && ch.NarrativeMeta.Outline != "" {
					prevChapters = append(prevChapters, ch)
				}
			}
			if len(prevChapters) > 5 {
				prevChapters = prevChapters[len(prevChapters)-5:]
			}
			if len(prevChapters) > 0 {
				var sb strings.Builder
				sb.WriteString("【前续章节大纲（供情节衔接参考）】\n")
				for _, ch := range prevChapters {
					title := ch.Title
					if title == "" {
						title = fmt.Sprintf("第%d章", ch.ChapterNo)
					}
					sb.WriteString(fmt.Sprintf("第%d章《%s》：%s\n\n", ch.ChapterNo, title, ch.NarrativeMeta.Outline))
				}
				prevOutlineSection = sb.String()
			}
		}

		recentCtxSection := ""
		if recentCtx != "" {
			recentCtxSection = "【叙事上下文】\n" + recentCtx
		}

		prompt = fmt.Sprintf(`请为小说《%s》第%d章生成详细的章节大纲（200～500字）。

小说简介：%s
本章标题：%s

%s
%s
%s

要求：
- 情节须与前续章节大纲自然衔接，避免重复或矛盾
- 详细描述本章的核心情节脉络与关键转折
- 点明主要人物的行动、目标与心理变化
- 交代场景背景与氛围
- 说明本章在整体故事中的作用（推进、铺垫或高潮）
- 字数不少于200字，不超过500字
- 直接输出大纲文本，不要加前缀或说明`,
			novel.Title, chapterNo, novel.Meta.Description, chapter.Title,
			prevOutlineSection, recentCtxSection, extraPromptSection,
		)
	}

	// 从项目配置读取参数默认值
	chOutlineOverrides := StoryboardOverrides{
		MaxTokens:      novel.AIConfig.MaxTokens,
		Temperature:    novel.AIConfig.Temperature,
		TimeoutSeconds: novel.AIConfig.TimeoutSeconds,
	}

	const minOutlineLen = 200
	var outline string
	for attempt := 0; attempt < 2; attempt++ {
		raw, genErr := s.aiService.GenerateWithProvider(tenantID, novelID, "chapter_outline", prompt, "", chOutlineOverrides)
		if genErr != nil {
			return nil, genErr
		}
		outline = strings.TrimSpace(raw)
		if len([]rune(outline)) >= minOutlineLen {
			break
		}
		// AI returned too short — ask it to expand on the second attempt
		prompt += fmt.Sprintf("\n\n【重要】上次输出仅%d字，不满足最低200字要求，请重新生成更详细的大纲。", len([]rune(outline)))
	}

	chapter.NarrativeMeta.Outline = outline
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
	if s.versionRepo != nil {
		if delErr := s.versionRepo.DeleteByChapter(chapter.ID); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapterByNo: delete versions for chapter %d: %v", chapter.ID, delErr)
		}
	}
	if s.chapterCharacterRepo != nil {
		if delErr := s.chapterCharacterRepo.DeleteByChapter(chapter.ID); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapterByNo: delete chapter_characters for chapter %d: %v", chapter.ID, delErr)
		}
	}
	if s.chapterItemRepo != nil {
		if delErr := s.chapterItemRepo.DeleteByChapter(chapter.ID); delErr != nil {
			logger.Errorf("[ChapterService] DeleteChapterByNo: delete chapter_items for chapter %d: %v", chapter.ID, delErr)
		}
	}
	s.syncNovelStats(novelID)
	return nil
}

// ReorderChapters 按新顺序批量更新章节号。orders 中每项为 {ChapterID, ChapterNo}，
// 必须覆盖该小说的全部章节，chapter_no 必须从 1 开始连续。
func (s *ChapterService) ReorderChapters(novelID uint, orders []repository.ChapterOrder) error {
	if len(orders) == 0 {
		return fmt.Errorf("orders is empty")
	}
	// 简单校验：所有 chapter_no 必须唯一且 > 0
	seen := make(map[int]bool, len(orders))
	for _, o := range orders {
		if o.ChapterNo <= 0 || seen[o.ChapterNo] {
			return fmt.Errorf("invalid or duplicate chapter_no: %d", o.ChapterNo)
		}
		seen[o.ChapterNo] = true
	}
	if err := s.chapterRepo.BatchReorderChapters(novelID, orders); err != nil {
		return err
	}
	s.syncNovelStats(novelID)
	return nil
}

// InsertChapterAfter 在 afterChapterNo 后插入新章节：先将后续章节 +1，再创建新章节。
// afterChapterNo=0 表示插入到第一章前面。
func (s *ChapterService) InsertChapterAfter(novelID uint, afterChapterNo int) (*model.Chapter, error) {
	newNo := afterChapterNo + 1
	if err := s.chapterRepo.ShiftUp(novelID, newNo); err != nil {
		return nil, err
	}
	var tenantID uint
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		tenantID = novel.TenantID
	}
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		TenantID:  tenantID,
		NovelID:   novelID,
		ChapterNo: newNo,
		Title:     "",
		Status:    "draft",
	}
	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}
	s.syncNovelStats(novelID)
	return chapter, nil
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
		logger.Errorf("[ChapterService] SyncPublishedCount failed: %v", err)
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
		logger.Errorf("[ChapterService] SyncPublishedCount failed: %v", err)
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

	metrics.ChapterGenerationInFlight.Inc()
	defer metrics.ChapterGenerationInFlight.Dec()
	genStart := time.Now()

	recordChapterGen := func(status string) {
		metrics.ChapterGenerationTotal.WithLabelValues(status).Inc()
		if status != "conflict" {
			metrics.ChapterGenerationDuration.Observe(time.Since(genStart).Seconds())
		}
	}

	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		recordChapterGen("error")
		return nil, err
	}

	// ── 并发保护：防止同一章节被同时触发两次生成（跨实例） ──────────────────────────────
	genLockKey := fmt.Sprintf("%d-%d", novelID, req.ChapterNo)
	if s.cache != nil {
		redisKey := "lock:chgen:" + genLockKey
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", 30*time.Minute).Result()
		if err != nil {
			logger.Errorf("[ChapterService] Redis SETNX for gen lock: %v, falling back to local lock", err)
			if _, loaded := s.genLocks.LoadOrStore(genLockKey, struct{}{}); loaded {
				recordChapterGen("conflict")
				return nil, fmt.Errorf("chapter %d of novel %d is already being generated", req.ChapterNo, novelID)
			}
			defer s.genLocks.Delete(genLockKey)
		} else if !ok {
			recordChapterGen("conflict")
			return nil, fmt.Errorf("chapter %d of novel %d is already being generated", req.ChapterNo, novelID)
		} else {
			s.genLocks.Store(genLockKey, struct{}{})
			defer func() {
				s.cache.Del(context.Background(), redisKey)
				s.genLocks.Delete(genLockKey)
			}()
		}
	} else {
		if _, loaded := s.genLocks.LoadOrStore(genLockKey, struct{}{}); loaded {
			recordChapterGen("conflict")
			return nil, fmt.Errorf("chapter %d of novel %d is already being generated", req.ChapterNo, novelID)
		}
		defer s.genLocks.Delete(genLockKey)
	}

	// 2. DB 层乐观锁：如果已有占位章节，仅当其 status 为 generating 时阻断（并发冲突）。
	// completed 章节允许重新生成，AtomicSetGenerating 会直接将其切换回 generating。
	if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil {
		ok, atomicErr := s.chapterRepo.AtomicSetGenerating(existing.ID, novelID)
		if atomicErr != nil {
			recordChapterGen("error")
			return nil, fmt.Errorf("chapter %d: check generating status: %w", req.ChapterNo, atomicErr)
		}
		if !ok {
			recordChapterGen("conflict")
			return nil, fmt.Errorf("chapter %d of novel %d is already being generated", req.ChapterNo, novelID)
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

	// 独立成篇模式：每章都必须是完整故事
	if novel.AIConfig.ChapterMode == "independent" {
		req.IsStandalone = true
	}
	// 自动检测最终章：当前章节号达到小说目标章节数时，确保故事完整收尾
	// 用户也可通过 is_standalone=true 显式触发（如临时想提前收尾）
	if !req.IsStandalone && novel.Meta.TargetChapters > 0 && req.ChapterNo >= novel.Meta.TargetChapters {
		req.IsStandalone = true
	}
	// 兜底检测：未设置目标章节数时，检查当前章是否为大纲中最后一章
	if !req.IsStandalone {
		if maxNo, err := s.chapterRepo.MaxChapterNo(novelID); err == nil && maxNo > 0 && req.ChapterNo >= maxNo {
			req.IsStandalone = true
		}
	}

	// 从小说大纲获取本章元数据（张力值、幕次、情感基调等）
	chapterMeta := s.extractChapterMeta(novelID, req.ChapterNo)

	// ── Step 1b: 联网参考搜索（可选）─────────────────────
	var refStories string
	if req.WebSearch && s.mcpService != nil {
		query := buildStorySearchQuery(novel.Meta.Genre, chapterMeta.summary)
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
			logger.Errorf("[WebSearch] chapter %d: skipped: %v", req.ChapterNo, searchErr)
		}
	}

	// ── Step 1c: 百科知识查询（可选）─────────────────────
	var wikiContext string
	if req.WikiSearch && s.mcpService != nil {
		query := buildWikiSearchQuery(novel.Meta.Genre, chapterMeta.summary)
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
			logger.Errorf("[WikiSearch] chapter %d: skipped: %v", req.ChapterNo, searchErr)
		}
	}

	// ── Step 1d: 情节模板查询（可选）─────────────────────
	var storyPatternRef string
	if req.UseStoryPattern && s.mcpService != nil {
		patternCtx, patternCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer patternCancel()
		out, searchErr := s.mcpService.InvokeTool(patternCtx, tenantID, "story_pattern", map[string]interface{}{
			"genre":       novel.Meta.Genre,
			"archetype":   chapterMeta.emotionalTone,
			"max_results": 2,
		})
		if searchErr == nil {
			storyPatternRef = parseStoryPatternOutput(out)
			logger.Printf("[StoryPattern] chapter %d: genre=%q", req.ChapterNo, novel.Meta.Genre)
		} else {
			logger.Errorf("[StoryPattern] chapter %d: skipped: %v", req.ChapterNo, searchErr)
		}
	}

	// ── Step 1e: 知识库语义搜索（可选）──────────────────────
	if s.mcpService != nil && s.knowledgeSvc != nil {
		kCtx, kCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer kCancel()
		kOut, kErr := s.mcpService.InvokeTool(kCtx, tenantID, "knowledge_search", map[string]interface{}{
			"novel_id": novel.ID,
			"query":    chapterMeta.summary,
			"limit":    3,
		})
		if kErr == nil {
			if kText := parseKnowledgeSearchOutput(kOut); kText != "" {
				if wikiContext != "" {
					wikiContext += "\n\n" + kText
				} else {
					wikiContext = kText
				}
				logger.Printf("[KnowledgeSearch] chapter %d: appended knowledge context", req.ChapterNo)
			}
		} else {
			logger.Errorf("[KnowledgeSearch] chapter %d: skipped: %v", req.ChapterNo, kErr)
		}
	}

	// ── Step 1f: 角色档案查询（可选，仅日志增强，实际角色数据由 getCharactersForPrompt 提供）──
	if s.mcpService != nil && s.characterRepo != nil {
		characters, charListErr := s.characterRepo.ListByNovel(novelID)
		if charListErr == nil {
			limit := min(3, len(characters))
			for i := 0; i < limit; i++ {
				charName := characters[i].Name
				cCtx, cCancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cCancel()
				_, cErr := s.mcpService.InvokeTool(cCtx, tenantID, "character_lookup", map[string]interface{}{
					"novel_id":       novel.ID,
					"character_name": charName,
				})
				if cErr != nil {
					logger.Errorf("[CharacterLookup] chapter %d char %q: skipped: %v", req.ChapterNo, charName, cErr)
				}
			}
		}
	}

	// ── Step 2: 生成场景大纲 ──────────────────────────────
	// prevEnding 在此处统一计算，避免后续两步各查一次 DB
	prevEnding := s.getPreviousChapterEnding(tenantID, novel, req.ChapterNo)

	// 最终章：提前收集全部未关闭悬线，供场景大纲和正文生成两步共用
	// 独立成篇模式不需要：每章自成一体，不收束全局悬线
	finalChapterCtx := ""
	if req.IsStandalone && novel.AIConfig.ChapterMode != "independent" {
		finalChapterCtx = s.buildFinalChapterContext(novelID, novel)
		logger.Printf("[GenerateChapter] ch%d: IsStandalone=true, finalChapterCtx len=%d", req.ChapterNo, len(finalChapterCtx))
	}

	sceneOutlineJSON, suggestedTitle, outlineErr := s.generateSceneOutline(
		tenantID, novelID, req, novel, globalCtx, chapterMeta, refStories, wikiContext, storyPatternRef, prevEnding, finalChapterCtx,
	)
	if outlineErr != nil {
		// Fix 1+2: 将预置占位章节（如存在）标记为 failed，避免状态卡在 "generating"
		if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil && existing.Status == "generating" {
			existing.Status = "failed"
			if updateErr := s.chapterRepo.Update(existing); updateErr != nil {
				logger.Errorf("[ChapterService] failed to set chapter %d status=failed: %v", existing.ID, updateErr)
			}
		}
		recordChapterGen("error")
		return nil, fmt.Errorf("generate scene outline failed: %w", outlineErr)
	}

	// ── Step 3: 按场景大纲生成章节内容 ───────────────────
	content, chapterHook, err := s.generateFromSceneOutline(
		tenantID, novelID, req, novel, sceneOutlineJSON, globalCtx, chapterMeta, refStories, wikiContext, prevEnding, finalChapterCtx,
	)
	if err != nil {
		// Fix 1: 将预置占位章节（如存在）标记为 failed，避免状态卡在 "generating"
		if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil && existing.Status == "generating" {
			existing.Status = "failed"
			if updateErr := s.chapterRepo.Update(existing); updateErr != nil {
				logger.Errorf("[ChapterService] failed to set chapter %d status=failed: %v", existing.ID, updateErr)
			}
		}
		recordChapterGen("error")
		return nil, err
	}

	// ── Content length validation ──────────────────────────────────────────────
	if len([]rune(content)) < 100 {
		if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil && existing.Status == "generating" {
			existing.Status = "failed"
			_ = s.chapterRepo.Update(existing)
		}
		recordChapterGen("error")
		return nil, fmt.Errorf("generated content too short (%d chars), expected at least 100 chars", len([]rune(content)))
	}

	// ── Step 4: 存储章节 (upsert: update if placeholder exists) ──────────────
	// 预规划大纲中的标题优先，AI 建议标题仅在无预规划时兜底，避免两者不一致。
	title := chapterMeta.chapterTitle
	if title == "" {
		title = suggestedTitle
	}
	if title == "" {
		title = fmt.Sprintf("第%d章", req.ChapterNo)
	}
	var chapter *model.Chapter
	if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil {
		existing.Title = title
		existing.Content = content
		existing.WordCount = countChineseChars(content)
		existing.NarrativeMeta.SceneOutline = sceneOutlineJSON
		existing.NarrativeMeta.TensionLevel = chapterMeta.tensionLevel
		existing.NarrativeMeta.ActNo = chapterMeta.actNo
		existing.NarrativeMeta.EmotionalTone = chapterMeta.emotionalTone
		existing.NarrativeMeta.HookType = chapterMeta.hookType
		existing.NarrativeMeta.ChapterHook = chapterHook
		existing.Status = "generating"
		if err := s.chapterRepo.Update(existing); err != nil {
			recordChapterGen("error")
			return nil, err
		}
		chapter = existing
	} else {
		chapter = &model.Chapter{
			UUID:      uuid.New().String(),
			TenantID:  tenantID,
			NovelID:   novelID,
			ChapterNo: req.ChapterNo,
			Title:     title,
			Content:   content,
			WordCount: countChineseChars(content),
			NarrativeMeta: model.ChapterNarrativeMeta{
				SceneOutline:  sceneOutlineJSON,
				TensionLevel:  chapterMeta.tensionLevel,
				ActNo:         chapterMeta.actNo,
				EmotionalTone: chapterMeta.emotionalTone,
				HookType:      chapterMeta.hookType,
				ChapterHook:   chapterHook,
			},
			Status: "generating",
		}
		if err := s.chapterRepo.Create(chapter); err != nil {
			recordChapterGen("error")
			return nil, err
		}
	}

	s.syncNovelStats(novelID)

	// ── Step 5: 同步生成摘要（必须在返回前完成，供下一章上下文使用）───────────────────────────────
	if s.narrativeSvc != nil && chapter.Summary == "" {
		if summary, err := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); err == nil {
			chapter.Summary = summary
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				logger.Errorf("[ChapterService] GenerateChapter: update chapter %d [摘要]: %v", chapter.ID, updateErr)
			}
		} else {
			logger.Errorf("[ChapterService] GenerateChapter: summary ch%d: %v", chapter.ChapterNo, err)
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

	// ── Step 5c: 同步生成章末精确状态快照（连贯性核心锚点，必须在返回前完成）────────────────────
	// postProcessChapter 是异步的——若下一章生成早于它完成，ChapterEndState 就是空的，
	// 导致 getPreviousChapterEnding 退化为 300 字原文兜底。此处同步生成，确保下一章能读到它。
	if chapter.NarrativeMeta.ChapterEndState == "" && chapter.Content != "" {
		if endState := s.generateChapterEndState(tenantID, chapter, novel); endState != "" {
			chapter.NarrativeMeta.ChapterEndState = endState
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				logger.Errorf("[ChapterService] GenerateChapter: update chapter %d [end_state]: %v", chapter.ID, updateErr)
			} else {
				logger.Printf("[ChapterService] chapter_end_state generated sync for ch%d", chapter.ChapterNo)
			}
		}
	}

	// Mark chapter as completed now that all synchronous steps are done.
	chapter.Status = "completed"
	if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
		logger.Errorf("[ChapterService] GenerateChapter: update chapter %d [status=completed]: %v", chapter.ID, updateErr)
	}

	// ── Step 6: 异步后处理（标题/精修/弧摘要，不再包含角色快照）────────────────────────────────
	go s.postProcessChapter(tenantID, chapter, novel)

	recordChapterGen("success")
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
		outlineJSON = novel.AIConfig.StylePrompt // 向后兼容
	}

	logger.Printf("[extractChapterMeta] novelID=%d chapterNo=%d outlineLen=%d", novelID, chapterNo, len(outlineJSON))

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
		if parseErr := json.Unmarshal([]byte(outlineJSON), &outline); parseErr != nil {
			logger.Errorf("[extractChapterMeta] JSON parse error: %v (preview=%q)", parseErr, truncateForPrompt(outlineJSON, 200))
		} else {
			logger.Printf("[extractChapterMeta] outline parsed: %d chapters total", len(outline.Chapters))
			found := false
			for _, ch := range outline.Chapters {
				if ch.ChapterNo == chapterNo {
					found = true
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
					logger.Printf("[extractChapterMeta] ch%d found: title=%q summaryLen=%d plotPoints=%d",
						chapterNo, meta.chapterTitle, len(meta.summary), len(meta.plotPoints))
					break
				}
			}
			if !found {
				logger.Printf("[extractChapterMeta] ch%d NOT found in outline (outline has ch_nos: %v)",
					chapterNo, func() []int {
						nos := make([]int, 0, len(outline.Chapters))
						for _, c := range outline.Chapters { nos = append(nos, c.ChapterNo) }
						return nos
					}())
			}
		}
	} else {
		logger.Printf("[extractChapterMeta] novel.Outline is EMPTY — no outline data available for ch%d", chapterNo)
	}

	// 读取章节记录（chapter.NarrativeMeta.Outline 是用户可见/可编辑的大纲，优先级最高）
	if existing, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo); err == nil && existing != nil {
		if meta.chapterTitle == "" {
			meta.chapterTitle = existing.Title
		}
		// chapter.NarrativeMeta.Outline（用户可见字段）优先于 novel.Outline JSON 的 summary
		// 因为用户在 UI 上看到并编辑的是 chapter.NarrativeMeta.Outline，这才是他们期望 AI 遵循的大纲
		if existing.NarrativeMeta.Outline != "" {
			if existing.NarrativeMeta.Outline != meta.summary {
				logger.Printf("[extractChapterMeta] ch%d: chapter.NarrativeMeta.Outline overrides novel.Outline summary (chOutlineLen=%d, novelSummaryLen=%d)",
					chapterNo, len(existing.NarrativeMeta.Outline), len(meta.summary))
			}
			meta.summary = existing.NarrativeMeta.Outline
		} else if meta.summary == "" {
			meta.summary = existing.Summary
			logger.Printf("[extractChapterMeta] ch%d: using chapter.Summary fallback (len=%d)", chapterNo, len(meta.summary))
		}
	}

	// 若大纲 JSON 中无剧情点，尝试从文本摘要中提取——保证 plot coverage 机制始终有效
	if len(meta.plotPoints) == 0 && meta.summary != "" {
		meta.plotPoints = extractPlotPointsFromText(meta.summary)
		if len(meta.plotPoints) > 0 {
			logger.Printf("[extractChapterMeta] ch%d: extracted %d plot points from text summary", chapterNo, len(meta.plotPoints))
		}
	}

	logger.Printf("[extractChapterMeta] ch%d final: summaryLen=%d plotPoints=%d title=%q",
		chapterNo, len(meta.summary), len(meta.plotPoints), meta.chapterTitle)

	return meta
}

// extractPlotPointsFromText 将纯文本的章节概述/大纲转换为剧情点列表。
// 当 novel.Outline JSON 中没有结构化 plot_points 时使用（如用户手动编辑的大纲文本）。
func extractPlotPointsFromText(text string) []string {
	if text == "" {
		return nil
	}
	var points []string

	// 按换行分割，识别列表项（带编号或符号前缀）
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len([]rune(line)) < 10 {
			continue
		}
		extracted := ""
		// 符号前缀: - · • *
		for _, prefix := range []string{"- ", "· ", "• ", "* "} {
			if strings.HasPrefix(line, prefix) {
				extracted = strings.TrimSpace(line[len(prefix):])
				break
			}
		}
		// 数字编号: "1. " "2. " ... "10. "
		if extracted == "" {
			r := []rune(line)
			for i, ch := range r {
				if ch == '.' || ch == '、' || ch == '）' {
					prefix := string(r[:i])
					// 判断前缀全是数字或序号字符
					isNum := true
					for _, c := range prefix {
						if c < '0' || c > '9' {
							isNum = false
							break
						}
					}
					if isNum && i > 0 && i < 4 && len(r) > i+2 {
						extracted = strings.TrimSpace(string(r[i+1:]))
					}
					break
				}
			}
		}
		if extracted != "" && len([]rune(extracted)) >= 8 {
			points = append(points, extracted)
		}
	}

	if len(points) >= 2 {
		if len(points) > 5 {
			points = points[:5]
		}
		return points
	}

	// 兜底：按句号切分，取有意义的句子作为剧情点
	sentences := strings.Split(text, "。")
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if len([]rune(s)) >= 15 {
			points = append(points, s)
		}
	}
	if len(points) > 5 {
		points = points[:5]
	}
	return points
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

	desc := novel.Meta.Description
	if desc == "" {
		desc = novel.Title + "（" + novel.Meta.Genre + "类型小说）"
	}

	prompt := fmt.Sprintf(`从以下小说简介中提取主要角色（必须包含主角），最多3个，以JSON数组格式返回。
《%s》（%s类型）
%s

只返回JSON数组，格式：
[{"name":"角色名","role":"protagonist","description":"角色核心特征一句话描述"}]
role只能是：protagonist / antagonist / supporting
注意：必须有且仅有一个protagonist`,
		novel.Title, novel.Meta.Genre, truncateForPrompt(desc, 800))

	result, aiErr := s.aiService.GenerateWithProvider(tenantID, novel.ID, "character_extract_mini", prompt, "")
	if aiErr != nil {
		logger.Errorf("[ChapterService] ensureProtagonistExtracted: AI error: %v", aiErr)
		return
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var extracted []struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
	}
	if jsonErr := json.Unmarshal([]byte(cleaned), &extracted); jsonErr != nil {
		logger.Errorf("[ChapterService] ensureProtagonistExtracted: parse error: %v (raw: %.200s)", jsonErr, cleaned)
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
			Name:        c.Name,
			Role:        role,
			Description: c.Description,
			Status:      "active",
		}
		if createErr := s.characterRepo.Create(char); createErr != nil {
			logger.Errorf("[ChapterService] ensureProtagonistExtracted: create %s: %v", c.Name, createErr)
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
		logger.Errorf("GenerateChapter: hierarchical context failed: %v — fallback", err)
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
	sb.WriteString(novel.Meta.Description)

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
	finalChapterCtx string,
) (sceneOutlineJSON, suggestedTitle string, outlineErr error) {

	// 构建伏笔提示
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	worldviewRules := ""
	if novel.Worldview != nil {
		worldviewRules = novel.Worldview.Rules
	}

	// 获取角色列表（含快照状态 + 内在动机）
	characters := s.getCharactersForPrompt(novelID)

	// 计算章节叙事预算（结构位置约束）
	budget := computeChapterBudget(req.ChapterNo, novel.Meta.TargetChapters, meta.actNo)
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

	logger.Printf("[generateSceneOutline] ch%d: summaryLen=%d plotPoints=%d chapterSummaryEmpty=%v",
		req.ChapterNo, len(meta.summary), len(meta.plotPoints), chapterSummary == "")

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
		if timeline, tlErr := s.timelineSvc.BuildTimeline(novelID); tlErr == nil && timeline != nil {
			timelineContext = s.timelineSvc.FormatTimelineForPrompt(timeline, req.ChapterNo)
		}
	}

	// 读者期待（来自上一章）
	prevReaderExpectations := s.buildPreviousReaderExpectations(novelID, req.ChapterNo)

	// 角色弧光进度（从角色的 ArcDesign + CurrentArcStage 构建）
	characterArcContext := s.buildCharacterArcContext(novelID, req.ChapterNo)

	// 张力预算（近几章若全为高张力，提醒插入缓冲章）
	tensionBudget := s.buildTensionBudget(novelID, req.ChapterNo, tensionLevel)

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
		"ChapterType":           computeChapterType(tensionLevel, hookType, actNo),
		"IsStandalone":          req.IsStandalone,
		"ChapterMode":           novel.AIConfig.ChapterMode,
		"FinalChapterContext":   finalChapterCtx, // 最终章专用：全部未关闭悬线收尾清单
		"PreviousChapterEnding": prevEnding,
		"Characters":            characters,
		"CharacterStates":       formatCharacterStates(characters),
		"ForeshadowHints":       foreshadowHints,
		"ReviewHints":           buildReviewHintsText(req.ReviewHints),
		"PlotTensionState":      plotTensionState,
		"RefStories":            refStories,
		"WikiContext":           wikiContext,
		"StoryPatternRef":       storyPatternRef,
		"ChapterBudget":         budgetText,
		"CharacterRegistry":     characterRegistry,
		"TimelineContext":        timelineContext,
		"CoreTheme":             novel.Meta.CoreTheme,
		"ReaderExpectations":    prevReaderExpectations,
		"CharacterArcContext":   characterArcContext,
		"TensionBudget":         tensionBudget,
		"WorldRules":            worldviewRules,
		"GenreHints":            genreSceneHints(novel.Genre),
		"UserPrompt":            req.Prompt,
	})
	if err != nil {
		logger.Errorf("GenerateChapter: render chapter_scene_outline: %v", err)
		return "", "", fmt.Errorf("generateSceneOutline: render template: %w", err)
	}

	resp, err := s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, novelID, "scene_outline", outlinePrompt, req.ModelOverride, buildChapterOverrides(req, novel))
	if err != nil {
		logger.Errorf("GenerateChapter: scene outline AI call failed: %v", err)
		return "", "", fmt.Errorf("generateSceneOutline: AI call failed: %w", err)
	}

	resp = extractJSONObject(strings.TrimSpace(resp))

	// 提取建议标题，并校验场景数量
	var outlineResult struct {
		ChapterTitle string              `json:"chapter_title"`
		Scenes       []json.RawMessage   `json:"scenes"`
		PlotCoverage []plotCoverageEntry `json:"plot_coverage"`
	}
	if err := json.Unmarshal([]byte(resp), &outlineResult); err != nil {
		return "", "", fmt.Errorf("generateSceneOutline: parse outline JSON: %w (raw=%q)", err, truncateForPrompt(resp, 200))
	}
	suggestedTitle = outlineResult.ChapterTitle

	sceneCount := len(outlineResult.Scenes)
	logger.Printf("[ChapterService] generateSceneOutline: chapterNo=%d scenes=%d", req.ChapterNo, sceneCount)

	// 场景数量越界检查
	if sceneCount < 1 {
		return "", "", fmt.Errorf("generateSceneOutline: AI returned 0 scenes, expected 3-5 (chapterNo=%d)", req.ChapterNo)
	}
	if sceneCount > 7 {
		logger.Printf("[ChapterService] generateSceneOutline: chapterNo=%d scenes=%d >7, truncating to 5", req.ChapterNo, sceneCount)
		outlineResult.Scenes = outlineResult.Scenes[:5]
		if truncated, marshalErr := json.Marshal(outlineResult); marshalErr == nil {
			resp = string(truncated)
		}
	}

	// ── 剧情点覆盖率验证 ─────────────────────────────────────
	// 检查 meta.plotPoints 是否全部出现在 plot_coverage 中；
	// 若有缺失，注入 MissingPlotPoints 后重试一次（不降级生成）。
	if len(meta.plotPoints) > 0 {
		missingPoints := findMissingPlotPoints(meta.plotPoints, outlineResult.PlotCoverage)
		if len(missingPoints) > 0 {
			logger.Printf("[generateSceneOutline] ch%d: %d/%d plot points missing from coverage, retrying",
				req.ChapterNo, len(missingPoints), len(meta.plotPoints))

			// 构建缺失剧情点文本
			var missingText strings.Builder
			for _, mp := range missingPoints {
				missingText.WriteString("- " + mp + "\n")
			}
			// 重新渲染 prompt，加入 MissingPlotPoints 警告
			retryPrompt, renderErr := renderPrompt("chapter_scene_outline", map[string]interface{}{
				"NovelTitle":            novel.Title,
				"ChapterNo":             req.ChapterNo,
				"ChapterTitle":          meta.chapterTitle,
				"GlobalContext":         globalCtx,
				"ChapterSummary":        chapterSummary,
				"PlotPoints":            plotPointsText,
				"MissingPlotPoints":     missingText.String(), // ← 缺失剧情点
				"TensionLevel":          tensionLevel,
				"ActNo":                 actNo,
				"EmotionalTone":         emotionalTone,
				"HookType":              hookType,
				"ChapterType":           computeChapterType(tensionLevel, hookType, actNo),
				"IsStandalone":          req.IsStandalone,
				"ChapterMode":           novel.AIConfig.ChapterMode,
				"FinalChapterContext":   finalChapterCtx, // ← 重试也需要最终章约束
				"PreviousChapterEnding": prevEnding,
				"Characters":            characters,
				"CharacterStates":       formatCharacterStates(characters),
				"ForeshadowHints":       foreshadowHints,
				"PlotTensionState":      plotTensionState,
				"RefStories":            refStories,
				"WikiContext":           wikiContext,
				"StoryPatternRef":       storyPatternRef,
				"ChapterBudget":         budgetText,
				"CharacterRegistry":     characterRegistry,
				"TimelineContext":       timelineContext,
				"CoreTheme":             novel.Meta.CoreTheme,
				"ReaderExpectations":    prevReaderExpectations,
				"CharacterArcContext":   characterArcContext,
				"TensionBudget":         tensionBudget,
				"WorldRules":            worldviewRules,
				"GenreHints":            genreSceneHints(novel.Genre),
				"UserPrompt":            req.Prompt,
			})
			if renderErr == nil {
				retryResp, retryErr := s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, novelID, "scene_outline", retryPrompt, req.ModelOverride, buildChapterOverrides(req, novel))
				if retryErr == nil {
					retryResp = extractJSONObject(strings.TrimSpace(retryResp))
					var retryResult struct {
						ChapterTitle string            `json:"chapter_title"`
						Scenes       []json.RawMessage `json:"scenes"`
					}
					if jsonErr := json.Unmarshal([]byte(retryResp), &retryResult); jsonErr == nil && len(retryResult.Scenes) > 0 {
						resp = retryResp
						suggestedTitle = retryResult.ChapterTitle
						logger.Printf("[generateSceneOutline] ch%d: retry succeeded with %d scenes", req.ChapterNo, len(retryResult.Scenes))
					}
				} else {
					logger.Errorf("[generateSceneOutline] ch%d: retry failed: %v (using original)", req.ChapterNo, retryErr)
				}
			}
		} else {
			logger.Printf("[generateSceneOutline] ch%d: all %d plot points covered", req.ChapterNo, len(meta.plotPoints))
		}
	}

	return resp, suggestedTitle, nil
}

// findMissingPlotPoints checks which plot points from the plan are absent from the AI-generated
// plot_coverage field. Uses per-entry fuzzy matching to avoid false positives from similar prefixes.
func findMissingPlotPoints(plotPoints []string, coverage []plotCoverageEntry) []string {
	if len(coverage) == 0 {
		// No coverage field at all — treat all as missing
		return plotPoints
	}

	var missing []string
	for _, pp := range plotPoints {
		ppRunes := []rune(pp)
		keyLen := 10
		if len(ppRunes) < keyLen {
			keyLen = len(ppRunes)
		}
		keyPhrase := string(ppRunes[:keyLen])

		// Per-entry matching: each coverage item is checked independently.
		// Avoids false positives when two plot points share the same 10-char prefix
		// (e.g. "主角与反派正面交锋" vs "主角与反派秘密交涉" would both match a blob containing either).
		found := false
		for _, c := range coverage {
			if strings.Contains(c.PlotPoint, keyPhrase) || strings.Contains(string(c.KeyBeat), keyPhrase) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, pp)
		}
	}
	return missing
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
	finalChapterCtx string,
) (string, string, error) {

	// 章节目标字数：优先用显式 WordCount，其次从小说 TargetWordCount 推算，最后默认 3000
	wordCount := req.WordCount
	if wordCount <= 0 && novel.Meta.TargetWordCount > 0 {
		chapters := novel.Meta.TargetChapters
		if chapters <= 0 {
			chapters = 100
		}
		wordCount = novel.Meta.TargetWordCount / chapters
	}
	if wordCount <= 0 {
		wordCount = 3000
	}
	// 字数下限
	if wordCount < 500 {
		wordCount = 500
	}
	// 字数上限：由 MaxTokens 反推可行上限（中文约 1.3 token/字）；
	// 若未设置 MaxTokens，硬上限 8000（避免超出模型默认输出窗口）
	{
		effectiveMaxTokens := req.MaxTokens
		if effectiveMaxTokens == 0 {
			effectiveMaxTokens = novel.AIConfig.MaxTokens
		}
		var maxWords int
		if effectiveMaxTokens > 0 {
			// token → 字：保留 20% buffer 给 prompt overhead
			maxWords = int(float64(effectiveMaxTokens) / 1.3 * 0.8)
			if maxWords < 500 {
				maxWords = 500
			}
		} else {
			maxWords = 8000 // 未配置 MaxTokens 时的安全上限
		}
		if wordCount > maxWords {
			logger.Printf("[generateFromSceneOutline] ch%d: wordCount %d → capped to %d (effectiveMaxTokens=%d)",
				req.ChapterNo, wordCount, maxWords, effectiveMaxTokens)
			wordCount = maxWords
		}
	}
	// 当目标字数 > 默认 8000 且未显式限制 MaxTokens，自动扩大以确保模型能输出足够内容
	// 中文 ~1.5 token/字 + 25% buffer
	if wordCount > 3000 && req.MaxTokens == 0 && novel.AIConfig.MaxTokens == 0 {
		req.MaxTokens = int(float64(wordCount) * 1.5 * 1.25)
		logger.Printf("[generateFromSceneOutline] ch%d: auto-scaled MaxTokens=%d for wordCount=%d",
			req.ChapterNo, req.MaxTokens, wordCount)
	}

	// 解析场景大纲以注入模板
	var outlineData struct {
		ChapterTitle string `json:"chapter_title"`
		HookSetup    string `json:"hook_setup"`
		ChapterArc   string `json:"chapter_arc"`
		Scenes       []struct {
			SceneNo          int      `json:"scene_no"`
			Location         string   `json:"location"`
			TimeOfDay        string   `json:"time_of_day"`
			Characters       []string `json:"characters"`
			Goal             string   `json:"goal"`
			NarrativePurpose string   `json:"narrative_purpose"`
			OpeningBeat      string   `json:"opening_beat"`
			KeyBeats         []string `json:"key_beats"`
			ClosingBeat      string   `json:"closing_beat"`
			EmotionalShift   string   `json:"emotional_shift"`
			DialogueSubtext  string   `json:"dialogue_subtext"`
			DialogueMode     string   `json:"dialogue_mode"`
			MicroPacing      string   `json:"micro_pacing"`
			SceneWeight      string   `json:"scene_weight"`      // 核心场景/过渡场景/衔接场景
			ThemeEcho        string   `json:"theme_echo"`        // 本场景如何呼应核心主题
			TensionDirection string   `json:"tension_direction"` // rising/peak/falling/reversal
			WordBudget       int      `json:"-"` // computed in Go, not from JSON
			RequiredEvent    string   `json:"-"` // plot point that MUST happen in this scene (from plot_coverage cross-ref)
			POVCharacter     string   `json:"pov_character"`
			Tension          int      `json:"tension"`
		} `json:"scenes"`
		PlotCoverage []struct {
			PlotPoint string `json:"plot_point"`
			SceneNo   int    `json:"scene_no"`
		} `json:"plot_coverage"`
	}
	if err := json.Unmarshal([]byte(sceneOutlineJSON), &outlineData); err != nil {
		logger.Errorf("[ChapterService] generateFromSceneOutline: failed to parse scene outline JSON: %v", err)
	}

	// 将 plot_coverage 中的剧情点映射回对应场景，设置 RequiredEvent
	// 这样在正文生成模板中，每个场景会得到一个"本场景必须发生的核心剧情"标注
	if len(outlineData.PlotCoverage) > 0 {
		for _, pc := range outlineData.PlotCoverage {
			for i := range outlineData.Scenes {
				if outlineData.Scenes[i].SceneNo == pc.SceneNo {
					if outlineData.Scenes[i].RequiredEvent == "" {
						outlineData.Scenes[i].RequiredEvent = pc.PlotPoint
					} else {
						outlineData.Scenes[i].RequiredEvent += " / " + pc.PlotPoint
					}
					break
				}
			}
		}
	} else if len(meta.plotPoints) > 0 && len(outlineData.Scenes) > 0 {
		// AI 未输出 plot_coverage 字段时的兜底：将剧情点均匀分配给各场景。
		// 核心场景（scene_weight="核心场景"）优先接收剧情点，其余场景按顺序分配。
		nScenes := len(outlineData.Scenes)
		nPoints := len(meta.plotPoints)
		// 找出核心场景的序号
		coreIdxs := make([]int, 0, nScenes)
		otherIdxs := make([]int, 0, nScenes)
		for i, sc := range outlineData.Scenes {
			if sc.SceneWeight == "核心场景" {
				coreIdxs = append(coreIdxs, i)
			} else {
				otherIdxs = append(otherIdxs, i)
			}
		}
		priority := append(coreIdxs, otherIdxs...) // 核心场景先分配
		for pIdx, ppText := range meta.plotPoints {
			if pIdx >= nScenes {
				// 多余的剧情点叠加到最后一个核心场景（确保不丢失）
				lastCore := nScenes - 1
				if len(coreIdxs) > 0 {
					lastCore = coreIdxs[len(coreIdxs)-1]
				}
				if outlineData.Scenes[lastCore].RequiredEvent == "" {
					outlineData.Scenes[lastCore].RequiredEvent = ppText
				} else {
					outlineData.Scenes[lastCore].RequiredEvent += " / " + ppText
				}
				continue
			}
			scIdx := priority[pIdx%nScenes] // 循环分配（nPoints > nScenes 时）
			if nPoints <= nScenes {
				scIdx = priority[pIdx]
			}
			if outlineData.Scenes[scIdx].RequiredEvent == "" {
				outlineData.Scenes[scIdx].RequiredEvent = ppText
			} else {
				outlineData.Scenes[scIdx].RequiredEvent += " / " + ppText
			}
		}
		logger.Printf("[generateFromSceneOutline] ch%d: plot_coverage absent — distributed %d plot points to %d scenes",
			req.ChapterNo, nPoints, nScenes)
	}
	logger.Printf("[ChapterService] generateFromSceneOutline: chapterNo=%d scenes=%d", req.ChapterNo, len(outlineData.Scenes))

	// 获取角色对话风格（同时包含状态快照 + 内在动机）
	characterVoices := s.getCharacterVoices(novelID)

	// 未解决剧情线（伏笔/冲突）
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	worldviewRulesFromOutline := ""
	if novel.Worldview != nil {
		worldviewRulesFromOutline = novel.Worldview.Rules
	}

	// 章节叙事预算（防信息过载、防过早化解矛盾）
	budget := computeChapterBudget(req.ChapterNo, novel.Meta.TargetChapters, meta.actNo)
	budgetText := formatBudgetForPrompt(budget)

	// 角色注册表（防命名混淆）
	characterRegistry := s.buildCharacterRegistry(novelID)

	// 峰值张力 + 按场景权重分配字数预算
	peakTension := 0
	coreCount, transCount := 0, 0
	for _, sc := range outlineData.Scenes {
		if sc.Tension > peakTension {
			peakTension = sc.Tension
		}
		switch sc.SceneWeight {
		case "核心场景":
			coreCount++
		case "过渡场景":
			transCount++
		}
	}
	// 按场景权重+张力值分配字数
	// 基础份额：核心6分/过渡3分/衔接1分
	// 张力加成：tension>=8 额外+3分，tension_direction=="peak" 额外+2分
	// 确保高潮场景获得足够字数，避免叙事头重脚轻
	totalScenes := len(outlineData.Scenes)
	sceneWeights := make([]int, totalScenes)
	for i, sc := range outlineData.Scenes {
		base := 1
		switch sc.SceneWeight {
		case "核心场景":
			base = 6
		case "过渡场景":
			base = 3
		}
		// 张力加成
		if sc.Tension >= 8 {
			base += 3
		} else if sc.Tension >= 6 {
			base += 1
		}
		if sc.TensionDirection == "peak" {
			base += 2
		}
		sceneWeights[i] = base
	}
	totalWeight := 0
	for _, w := range sceneWeights {
		totalWeight += w
	}
	if totalWeight == 0 {
		totalWeight = totalScenes
	}
	for i := range outlineData.Scenes {
		budget := wordCount * sceneWeights[i] / totalWeight
		// 最低字数保障
		switch outlineData.Scenes[i].SceneWeight {
		case "核心场景":
			if budget < 200 {
				budget = 200
			}
		case "过渡场景":
			if budget < 150 {
				budget = 150
			}
		default:
			if budget < 100 {
				budget = 100
			}
		}
		outlineData.Scenes[i].WordBudget = budget
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
		if tl, tlErr := s.timelineSvc.BuildTimeline(novelID); tlErr == nil && tl != nil {
			timelineCtx = s.timelineSvc.FormatTimelineForPrompt(tl, req.ChapterNo)
		}
	}

	// 格式化大纲剧情点（用于第二步正文生成的双重约束）
	outlinePlotPointsText := ""
	if len(meta.plotPoints) > 0 {
		var sb strings.Builder
		for _, pp := range meta.plotPoints {
			sb.WriteString("- ")
			sb.WriteString(pp)
			sb.WriteString("\n")
		}
		outlinePlotPointsText = sb.String()
	}

	// 读者期待（来自上一章，供正文生成保证回应上章悬念）
	prevReaderExpectations := s.buildPreviousReaderExpectations(novelID, req.ChapterNo)

	// ── 主路径：逐场景生成 ──
	// P2-1: chapterPrompt（chapter_from_outline.j2）仅在逐场景路径失败时才渲染（懒加载）。
	// 主路径使用 scene_write.j2 逐场景调用，chapterPrompt 只作为 ≥1/3 场景失败时的降级兜底。
	// 每个场景单独调用 AI，使用精简的 scene_write.j2 提示词（~55 行）。
	// 相比一次性生成整章的 200+ 行提示词，此方式的优点：
	//   1. 剧情点履约率更高：每次调用只需关注 1 个 RequiredEvent，AI 不会遗忘
	//   2. 连贯性更强：每个场景获得前一场景的原文末尾作为"零距离接续"约束
	if len(outlineData.Scenes) > 0 {
		var sceneParts []string
		var lastHook string
		prevSceneEnding := "" // 前一场景原文末尾 300 字，供下一场景零距离接续

		sceneOk := true
		for scIdx, sc := range outlineData.Scenes {
			isLastScene := scIdx == len(outlineData.Scenes)-1

			// 按场景字数预算计算 MaxTokens（中文约 1.5 token/字 + 25% overhead）
			sceneMaxTokens := int(float64(sc.WordBudget)*1.5*1.25) + 256
			// 最后一个场景额外保障 40% token：章末钩子、收尾段落往往比预算更长
			if isLastScene {
				sceneMaxTokens = int(float64(sceneMaxTokens) * 1.4)
			}
			// 不超过章节级 MaxTokens 限制
			if chMax := req.MaxTokens; chMax > 0 && sceneMaxTokens > chMax {
				sceneMaxTokens = chMax
			} else if nm := novel.AIConfig.MaxTokens; nm > 0 && req.MaxTokens == 0 && sceneMaxTokens > nm {
				sceneMaxTokens = nm
			}
			sceneOverrides := StoryboardOverrides{
				MaxTokens:   sceneMaxTokens,
				Temperature: req.Temperature,
			}
			if sceneOverrides.Temperature == 0 {
				sceneOverrides.Temperature = novel.AIConfig.Temperature
			}
			if ts := req.TimeoutSeconds; ts > 0 {
				sceneOverrides.TimeoutSeconds = ts
			} else if ts := novel.AIConfig.TimeoutSeconds; ts > 0 {
				sceneOverrides.TimeoutSeconds = ts
			}

			minWordsScene := sc.WordBudget * 7 / 10
			maxWordsScene := sc.WordBudget * 14 / 10
			if minWordsScene < 200 {
				minWordsScene = 200
			}

			// 场景 1：接上一章结尾；后续场景：接上一场景原文末尾
			scPrevChapterEnding := ""
			scPrevSceneEnding := ""
			if scIdx == 0 {
				scPrevChapterEnding = prevEnding
			} else {
				scPrevSceneEnding = prevSceneEnding
			}

			// 只传入本场景出场角色的描述（过滤无关角色减少 token 浪费，提高 AI 聚焦度）
			sceneCharSet := make(map[string]bool, len(sc.Characters))
			for _, cn := range sc.Characters {
				sceneCharSet[strings.TrimSpace(cn)] = true
			}
			filteredVoices := make([]characterForPrompt, 0, len(sc.Characters)+1)
			for _, cv := range characterVoices {
				if sceneCharSet[strings.TrimSpace(cv.Name)] || cv.IsProtagonist {
					filteredVoices = append(filteredVoices, cv)
				}
			}
			if len(filteredVoices) == 0 {
				filteredVoices = characterVoices // 过滤结果为空时兜底（名字不匹配）
			}

			scenePrompt, promptErr := renderPrompt("scene_write", map[string]interface{}{
				"NovelTitle":            novel.Title,
				"ChapterNo":             req.ChapterNo,
				"ChapterTitle":          chapterTitle,
				"SceneNo":               sc.SceneNo,
				"TotalScenes":           len(outlineData.Scenes),
				"RequiredEvent":         sc.RequiredEvent,
				"ChapterOutlineSummary": meta.summary,
				"ChapterType":           computeChapterType(meta.tensionLevel, meta.hookType, meta.actNo),
				"PreviousSceneEnding":   scPrevSceneEnding,
				"PreviousChapterEnding": scPrevChapterEnding,
				"Location":              sc.Location,
				"TimeOfDay":             sc.TimeOfDay,
				"Characters":            strings.Join(sc.Characters, "、"),
				"POVCharacter":          sc.POVCharacter,
				"Goal":                  sc.Goal,
				"NarrativePurpose":      sc.NarrativePurpose,
				"EmotionalShift":        sc.EmotionalShift,
				"MicroPacing":           sc.MicroPacing,
				"DialogueSubtext":       sc.DialogueSubtext,
				"DialogueMode":          sc.DialogueMode,    // P1-1: 对话模式（之前被丢弃）
				"KeyBeats":              sc.KeyBeats,
				"OpeningBeat":           sc.OpeningBeat,
				"ClosingBeat":           sc.ClosingBeat,
				"WordBudget":            sc.WordBudget,
				"MinWords":              minWordsScene,
				"MaxWords":              maxWordsScene,
				"CharacterVoices":       filteredVoices,
				"IsLastScene":           isLastScene,
				"IsStandalone":          req.IsStandalone,   // P0-2: 最终章标记
				"ChapterMode":           novel.AIConfig.ChapterMode,
				"HookType":              meta.hookType,
				// 场景大纲生成的重要字段
				"TensionDirection":    sc.TensionDirection,  // rising/peak/falling/reversal
				"SceneWeight":         sc.SceneWeight,       // 核心场景/过渡场景/衔接场景
				"ThemeEcho":           sc.ThemeEcho,         // 本场景如何呼应核心主题
				"CoreTheme":           novel.Meta.CoreTheme,      // 全书核心主题
				"FinalChapterContext": finalChapterCtx,      // P0-2: 最终章未关闭悬线清单
				"WorldRules":          worldviewRulesFromOutline,
				"GenreHints":          genreWritingHints(novel.Genre),
				"UserPrompt":          req.Prompt,
			})
			if promptErr != nil {
				logger.Errorf("[generateFromSceneOutline] ch%d scene%d: render scene_write failed: %v; falling back to one-shot",
					req.ChapterNo, sc.SceneNo, promptErr)
				sceneOk = false
				break
			}

			var sceneRaw string
			var sceneErr error
			// P1-3: currentOverrides 声明在循环外，确保温度提升在 attempt=1 时真正生效。
			// 原实现的 retryOverrides 是块级局部变量，continue 后下一轮仍用原始 sceneOverrides。
			currentOverrides := sceneOverrides
			for attempt := 0; attempt < 2; attempt++ {
				sceneRaw, sceneErr = s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, novelID, "chapter", scenePrompt, req.ModelOverride, currentOverrides)
				if sceneErr == nil {
					// 字数不足检测：生成成功但内容过短时，提高温度重试（最多1次）
					if attempt == 0 && len([]rune(cleanChapterOutput(sceneRaw))) < minWordsScene {
						logger.Printf("[generateFromSceneOutline] ch%d scene%d: content too short (%d < %d), retrying with higher temperature",
							req.ChapterNo, sc.SceneNo, len([]rune(cleanChapterOutput(sceneRaw))), minWordsScene)
						currentOverrides.Temperature = 0.85 // 直接修改循环级变量，下次迭代即生效
						continue                            // attempt++ → attempt=1 再试一次
					}
					break
				}
				logger.Errorf("[generateFromSceneOutline] ch%d scene%d attempt%d: %v", req.ChapterNo, sc.SceneNo, attempt+1, sceneErr)
				if attempt < 1 {
					time.Sleep(3 * time.Second)
				}
			}
			if sceneErr != nil {
				// 场景失败：不立即放弃整章。记录失败并用占位符继续，
				// 保留已生成的场景，最终判断是否有足够内容。
				logger.Errorf("[generateFromSceneOutline] ch%d scene%d: all attempts failed (%v); using placeholder, continuing",
					req.ChapterNo, sc.SceneNo, sceneErr)
				sceneParts = append(sceneParts, "") // 空占位符，后续过滤
				sceneOk = false
				// prevSceneEnding 保持上一个成功场景的末尾，继续为下一场景提供接续锚
				continue
			}

			sceneRaw = cleanChapterOutput(sceneRaw)
			if isLastScene {
				sceneContent, sceneHook := extractChapterHook(sceneRaw)
				sceneParts = append(sceneParts, sceneContent)
				lastHook = sceneHook
			} else {
				sceneParts = append(sceneParts, sceneRaw)
			}

			// 更新"前一场景末尾"（供下一场景第一句零距离接续）
			sceneRunes := []rune(sceneRaw)
			n := 500 // P1-6: 300→500 字，复杂场景中段的关键状态变化不再被截断
			if len(sceneRunes) < n {
				n = len(sceneRunes)
			}
			prevSceneEnding = string(sceneRunes[len(sceneRunes)-n:])
			logger.Printf("[generateFromSceneOutline] ch%d scene%d done: len=%d", req.ChapterNo, sc.SceneNo, len(sceneRaw))
		}

		// 统计成功场景数（非空）
		successCount := 0
		for _, p := range sceneParts {
			if p != "" {
				successCount++
			}
		}
		// 成功场景 >= 2/3 时直接使用，过滤掉空占位符后拼接
		minSuccessRatio := (len(outlineData.Scenes)*2 + 2) / 3 // ceil(2/3)
		if successCount >= minSuccessRatio {
			var nonEmpty []string
			for _, p := range sceneParts {
				if p != "" {
					nonEmpty = append(nonEmpty, p)
				}
			}
			totalContent := strings.Join(nonEmpty, "\n\n")
			if successCount < len(outlineData.Scenes) {
				logger.Printf("[generateFromSceneOutline] ch%d: partial success (%d/%d scenes), using available content (len=%d)",
					req.ChapterNo, successCount, len(outlineData.Scenes), len(totalContent))
			} else {
				logger.Printf("[ChapterService] generateFromSceneOutline done (scene-by-scene): chapterNo=%d contentLen=%d scenes=%d",
					req.ChapterNo, len(totalContent), len(sceneParts))
			}
			// P1-4: 最后场景失败时 lastHook 为空，尝试从合并内容提取钩子标记（零成本兜底）
			if lastHook == "" {
				_, lastHook = extractChapterHook(totalContent)
			}
			return totalContent, lastHook, nil
		}
		if !sceneOk || successCount < minSuccessRatio {
			logger.Errorf("[generateFromSceneOutline] ch%d: scene-by-scene failed (success=%d/%d); using one-shot fallback",
				req.ChapterNo, successCount, len(outlineData.Scenes))
		}
	}

	// ── 降级兜底：一次性生成整章 ──
	// P2-1: 仅在逐场景路径失败时才渲染 chapter_from_outline.j2（懒加载，避免主路径浪费渲染）。
	chapterPrompt, renderErr := renderPrompt("chapter_from_outline", map[string]interface{}{
		"NovelTitle":            novel.Title,
		"ChapterNo":             req.ChapterNo,
		"ChapterTitle":          chapterTitle,
		"WordCount":             wordCount,
		"GlobalContext":         globalCtx,
		"Scenes":                outlineData.Scenes,
		"HookSetup":             outlineData.HookSetup,
		"PeakTension":           peakTension,
		"Characters":            characterVoices,
		"CharacterStates":       formatCharacterStates(characterVoices),
		"ForeshadowHints":       foreshadowHints,
		"PreviousChapterEnding": prevEnding,
		"UserPrompt":            req.Prompt,
		"ReviewHints":           buildReviewHintsText(req.ReviewHints),
		"IsStandalone":          req.IsStandalone,
		"ChapterMode":           novel.AIConfig.ChapterMode,
		"RefStories":            refStories,
		"WikiContext":           wikiContext,
		"ChapterBudget":         budgetText,
		"CharacterRegistry":     characterRegistry,
		"TimelineContext":       timelineCtx,
		"ChapterOutlineSummary": meta.summary,
		"OutlinePlotPoints":     outlinePlotPointsText,
		"CoreTheme":             novel.Meta.CoreTheme,
		"ReaderExpectations":    prevReaderExpectations,
		"FinalChapterContext":   finalChapterCtx,
		"WorldRules":            worldviewRulesFromOutline,
		"GenreHints":            genreWritingHints(novel.Genre),
	})
	if renderErr != nil {
		logger.Errorf("[generateFromSceneOutline] ch%d: one-shot render failed: %v; using simple fallback", req.ChapterNo, renderErr)
		content, err := s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
		return content, "", err
	}
	var raw string
	var genErr error
	for attempt := 0; attempt < 3; attempt++ {
		raw, genErr = s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, novelID, "chapter", chapterPrompt, req.ModelOverride, buildChapterOverrides(req, novel))
		if genErr == nil {
			break
		}
		logger.Errorf("[ChapterService] generateFromSceneOutline: attempt %d failed: %v", attempt+1, genErr)
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
	}
	if genErr != nil {
		return "", "", genErr
	}
	raw = cleanChapterOutput(raw)
	content, hook := extractChapterHook(raw)
	logger.Printf("[ChapterService] generateFromSceneOutline done (one-shot): chapterNo=%d contentLen=%d", req.ChapterNo, len(content))
	return content, hook, nil
}

// generateFallbackChapter 场景大纲失败时的降级生成
func (s *ChapterService) generateFallbackChapter(tenantID, novelID uint, req *model.GenerateChapterRequest, novel *model.Novel, globalCtx string) (string, error) {
	logger.Printf("GenerateChapter: using fallback (no scene outline) for novel %d ch %d", novelID, req.ChapterNo)
	wc := req.WordCount
	if wc <= 0 && novel.Meta.TargetWordCount > 0 {
		chapters := novel.Meta.TargetChapters
		if chapters <= 0 {
			chapters = 100
		}
		wc = novel.Meta.TargetWordCount / chapters
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
			logger.Errorf("[ChapterService] postProcessChapter panic recovered: %v\n%s", r, debug.Stack())
		}
	}()
	logger.Printf("[ChapterService] postProcessChapter start: chapterID=%d no=%d", chapter.ID, chapter.ChapterNo)

	// Fetch a fresh copy from DB to avoid mutating the caller's pointer concurrently.
	if fresh, err := s.chapterRepo.GetByID(chapter.ID); err != nil {
		logger.Errorf("[ChapterService] postProcessChapter: fetch fresh chapter %d failed: %v", chapter.ID, err)
		return
	} else {
		chapter = fresh
	}

	// Fetch a fresh novel directly from DB (bypass Redis cache) so that config changes
	// made between generation start and post-processing (e.g. AutoReviewRounds) are visible.
	if freshNovel, err := s.novelRepo.GetByIDFromDB(chapter.NovelID); err == nil {
		novel = freshNovel
	} else {
		logger.Errorf("[ChapterService] postProcessChapter: fetch fresh novel %d failed (non-fatal, using caller copy): %v", chapter.NovelID, err)
	}
	// 1. 精修（先于摘要执行：摘要必须基于最终内容，不能基于草稿）
	if s.narrativeSvc != nil {
		if refined, err := s.narrativeSvc.RefineChapterContent(tenantID, chapter, novel.Title); err == nil && refined != chapter.Content {
			chapter.Content = refined
			chapter.WordCount = countChineseChars(refined)
			if updateErr := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
				"content": chapter.Content, "word_count": chapter.WordCount,
			}); updateErr != nil {
				logger.Errorf("postProcessChapter: update chapter %d [精修]: %v", chapter.ID, updateErr)
			}
			// P0-1: 精修可能改变人物位置/状态/心情，Step 5b 是在精修前提取的快照，
			// 必须用精修后内容重新提取，确保下一章读到的角色状态与实际呈现给读者的内容一致。
			if s.characterRepo != nil && s.snapshotRepo != nil {
				snapshotSvc := &NovelService{novelRepo: s.novelRepo, chapterRepo: s.chapterRepo, aiService: s.aiService}
				snapshotSvc.characterRepo = s.characterRepo
				snapshotSvc.snapshotRepo = s.snapshotRepo
				snapshotSvc.writeCharacterSnapshots(tenantID, chapter)
				logger.Printf("[postProcessChapter] character snapshots refreshed after refinement for ch%d", chapter.ChapterNo)
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
					if updateErr := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
						"content": chapter.Content, "word_count": chapter.WordCount,
					}); updateErr != nil {
						logger.Errorf("postProcessChapter: update chapter %d [quality-refinement]: %v", chapter.ID, updateErr)
					}
					// P0-1: quality refinement changes content → refresh snapshots so the next chapter
					// reads correct character states, not the pre-quality-refinement draft.
					if s.characterRepo != nil && s.snapshotRepo != nil {
						snapSvc := &NovelService{novelRepo: s.novelRepo, chapterRepo: s.chapterRepo, aiService: s.aiService}
						snapSvc.characterRepo = s.characterRepo
						snapSvc.snapshotRepo = s.snapshotRepo
						snapSvc.writeCharacterSnapshots(tenantID, chapter)
						logger.Printf("[postProcessChapter] character snapshots refreshed after quality-refinement for ch%d", chapter.ChapterNo)
					}
					// Re-check quality after refinement
					if report2, qErr2 := s.qualitySvc.CheckChapterQuality(ctx, chapter, novel); qErr2 == nil && report2.IsAcceptable() {
						stillLow = false
					}
				} else if refErr != nil {
					logger.Errorf("postProcessChapter: quality-refinement ch%d: %v", chapter.ChapterNo, refErr)
				}
			}
			if stillLow {
				if updateErr := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
					"quality_status": "low",
				}); updateErr != nil {
					logger.Errorf("postProcessChapter: update chapter %d [quality-status]: %v", chapter.ID, updateErr)
				} else {
					chapter.QualityMeta.QualityStatus = "low"
					logger.Printf("[ChapterService] chapter %d saved with low quality status", chapter.ChapterNo)
				}
			}
		} else if qErr != nil {
			logger.Errorf("[ChapterService] postProcessChapter: quality check ch%d failed (non-fatal): %v", chapter.ChapterNo, qErr)
		}
	}

	// 2. 生成摘要（精修完成后执行，确保摘要基于最终正文而非草稿）
	// 无论 Summary 是否已有值，均重新生成以确保与精修后内容一致。
	// Retry up to 3 times with 1s/2s delays.
	if s.narrativeSvc != nil && chapter.Content != "" {
		var summaryText string
		for attempt := 0; attempt < 3; attempt++ {
			if generated, err := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); err == nil {
				summaryText = generated
				break
			} else if attempt < 2 {
				logger.Errorf("postProcess: summary ch%d attempt %d failed: %v, retrying", chapter.ChapterNo, attempt+1, err)
				time.Sleep(time.Duration(attempt+1) * time.Second)
			} else {
				logger.Errorf("postProcess: summary ch%d attempt %d failed: %v", chapter.ChapterNo, attempt+1, err)
			}
		}
		if summaryText != "" {
			chapter.Summary = summaryText
			if updateErr := s.chapterRepo.UpdateSummary(chapter.ID, chapter.NovelID, summaryText); updateErr != nil {
				logger.Errorf("postProcessChapter: update chapter %d [摘要]: %v", chapter.ID, updateErr)
			}
		} else {
			logger.Errorf("[ChapterService] WARNING: chapter %d has no summary after 3 attempts", chapter.ChapterNo)
		}
	}

	// 3. 如果标题仍是"第N章"，基于精修后内容生成创意标题
	defaultTitle := fmt.Sprintf("第%d章", chapter.ChapterNo)
	if s.narrativeSvc != nil && chapter.Title == defaultTitle && chapter.Summary != "" {
		if title, err := s.narrativeSvc.GenerateChapterTitle(tenantID, chapter, novel.Meta.Genre, chapter.NarrativeMeta.EmotionalTone); err == nil && title != "" {
			chapter.Title = fmt.Sprintf("第%d章 %s", chapter.ChapterNo, title)
			if updateErr := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
				"title": chapter.Title,
			}); updateErr != nil {
				logger.Errorf("postProcessChapter: update chapter %d [标题]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 4. 连贯性检查（不阻塞主流程；结果持久化到 continuity_report 表供 UI 查询）
	// 若发现 high/critical 级别问题，在章节上打标记 continuity_blocked=true，
	// 前端据此提示用户审查，但不阻断生成流程。
	if s.continuitySvc != nil && chapter.Content != "" {
		go func(ch *model.Chapter) {
			report, err := s.continuitySvc.ValidateChapter(novel.ID, ch.ID, tenantID, ch.ChapterNo, ch.Content)
			if err != nil {
				logger.Errorf("[ChapterService] continuity check ch%d: %v", ch.ChapterNo, err)
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
				// P0-2: 使用原子性单列更新，避免与 postProcessChapter 主 goroutine
				// 的并发写入（steps 4c/4d/4e）产生写入竞争，导致 continuity_blocked=true 被覆盖。
				if updateErr := s.chapterRepo.UpdateContinuityBlocked(ch.ID, novel.ID, true); updateErr != nil {
					logger.Errorf("[ChapterService] continuity_blocked update ch%d: %v", ch.ChapterNo, updateErr)
				} else {
					logger.Printf("[ChapterService] continuity_blocked=true marked for ch%d (novel %d)", ch.ChapterNo, novel.ID)
				}
			}
		}(chapter)
	}

	// 4b. 自动审查：章节生成后执行 N 轮 AI 深度审查 + 自动应用修改
	if s.qualitySvc != nil && novel.AIConfig.AutoReviewRounds > 0 {
		logger.Printf("[ChapterService] postProcessChapter: starting auto-review for ch%d (%d rounds, minScore=%.0f)",
			chapter.ChapterNo, novel.AIConfig.AutoReviewRounds, novel.AIConfig.AutoReviewMinScore)
		finalScore, totalApplied, reviewErr := s.qualitySvc.RunAutoReview(
			context.Background(), chapter.ID, tenantID,
			novel.AIConfig.AutoReviewRounds, novel.AIConfig.AutoReviewMinScore,
		)
		if reviewErr != nil {
			logger.Errorf("[ChapterService] postProcessChapter: auto-review ch%d error (non-fatal): %v", chapter.ChapterNo, reviewErr)
		} else {
			logger.Printf("[ChapterService] postProcessChapter: auto-review ch%d done: finalScore=%.1f totalApplied=%d",
				chapter.ChapterNo, finalScore, totalApplied)
		}

		// P0-1/P0-2: AutoReview 应用了 diff 修改，步骤 2 生成的摘要和步骤 1 刷新的角色快照均已失效。
		// 必须用最终内容重新生成，避免后续章节拿到基于"审查前草稿"的错误上下文。
		if totalApplied > 0 {
			if fresh, fetchErr := s.chapterRepo.GetByID(chapter.ID); fetchErr == nil {
				chapter = fresh
			}
			// P0-1: 重新生成摘要。
			// 使用 UpdateSummary（单列更新）而非 Update（全量），避免覆盖 continuity_blocked 等
			// 可能被连贯性检查 goroutine 并发写入的字段（P1-3 race fix）。
			if s.narrativeSvc != nil && chapter.Content != "" {
				if newSummary, sumErr := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); sumErr == nil && newSummary != "" {
					if updateErr := s.chapterRepo.UpdateSummary(chapter.ID, chapter.NovelID, newSummary); updateErr != nil {
						logger.Errorf("postProcessChapter: update ch%d [summary-post-review]: %v", chapter.ID, updateErr)
					} else {
						chapter.Summary = newSummary // sync in-memory
						logger.Printf("[ChapterService] summary regenerated after AutoReview for ch%d", chapter.ChapterNo)
					}
				}
			}
			// P0-2: 刷新角色快照（AutoReview 可能改写角色行为/位置/状态）
			if s.characterRepo != nil && s.snapshotRepo != nil {
				snapshotSvc := &NovelService{novelRepo: s.novelRepo, chapterRepo: s.chapterRepo, aiService: s.aiService}
				snapshotSvc.characterRepo = s.characterRepo
				snapshotSvc.snapshotRepo = s.snapshotRepo
				snapshotSvc.writeCharacterSnapshots(tenantID, chapter)
				logger.Printf("[ChapterService] character snapshots refreshed after AutoReview for ch%d", chapter.ChapterNo)
			}
		}
	}

	// 4c. 生成读者期待状态（章末读者最想知道的3件事，供下一章生成时约束）
	// 独立成篇模式：章章独立，无需下章接续，跳过生成。
	// 依赖摘要，所以在摘要生成之后执行；AI 调用失败最多重试3次，与摘要生成策略一致。
	if novel.AIConfig.ChapterMode != "independent" && chapter.Summary != "" && chapter.NarrativeMeta.ReaderExpectations == "" {
		var expectations string
		for attempt := 0; attempt < 3; attempt++ {
			if exp := s.generateReaderExpectations(tenantID, chapter, novel); exp != "" {
				expectations = exp
				break
			}
			if attempt < 2 {
				logger.Errorf("postProcessChapter: reader_expectations ch%d attempt %d failed, retrying", chapter.ChapterNo, attempt+1)
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
		}
		if expectations != "" {
			if updateErr := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
				"reader_expectations": expectations,
			}); updateErr != nil {
				logger.Errorf("postProcessChapter: update chapter %d [reader_expectations]: %v", chapter.ID, updateErr)
			} else {
				chapter.NarrativeMeta.ReaderExpectations = expectations
				logger.Printf("[ChapterService] reader_expectations generated for ch%d", chapter.ChapterNo)
			}
		} else {
			logger.Errorf("[ChapterService] WARNING: reader_expectations ch%d failed after 3 attempts", chapter.ChapterNo)
		}
	}

	// 4d. 精修后重新提取章末精确状态快照（重要：精修可能改变章末内容，必须覆盖 Step 5c 的旧快照）
	// Step 5c 在精修前同步提取，是为了让下一章"立即可用"。
	// 这里精修已完成，用最终内容覆盖，确保快照基于已精修的章末文本，而非草稿。
	// 独立成篇模式：章章独立，无需下章接续，跳过章末状态快照生成。
	if novel.AIConfig.ChapterMode != "independent" && chapter.Content != "" {
		if endState := s.generateChapterEndState(tenantID, chapter, novel); endState != "" {
			if updateErr := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
				"chapter_end_state": endState,
			}); updateErr != nil {
				logger.Errorf("postProcessChapter: update chapter %d [chapter_end_state_refined]: %v", chapter.ID, updateErr)
			} else {
				chapter.NarrativeMeta.ChapterEndState = endState
				logger.Printf("[ChapterService] chapter_end_state refreshed after refinement for ch%d", chapter.ChapterNo)
			}
		}
	}

	// 4e. 大纲剧情点覆盖率检查（解决章节内容与大纲相关性低的问题）
	// 提取本章大纲规定的剧情点，检查是否全部在正文中作为实际剧情展开；
	// 若有缺失，AI 自动补写对应段落并集成到章节内容中。
	if chapter.Content != "" {
		meta := s.extractChapterMeta(chapter.NovelID, chapter.ChapterNo)
		if len(meta.plotPoints) > 0 {
			logger.Printf("[ChapterService] postProcessChapter: ch%d checking plot point compliance (%d points)",
				chapter.ChapterNo, len(meta.plotPoints))
			if fresh, fetchErr := s.chapterRepo.GetByID(chapter.ID); fetchErr == nil {
				if patched := s.checkAndPatchMissingPlotPoints(tenantID, fresh, novel, meta.plotPoints); patched {
					if updateErr := s.chapterRepo.UpdateFields(fresh.ID, fresh.NovelID, map[string]interface{}{
						"content": fresh.Content, "word_count": fresh.WordCount,
					}); updateErr != nil {
						logger.Errorf("postProcessChapter: update chapter %d [plot_compliance_patch]: %v", chapter.ID, updateErr)
					} else {
						chapter = fresh
						logger.Printf("[ChapterService] plot compliance patch applied for ch%d (new wordCount=%d)",
							chapter.ChapterNo, chapter.WordCount)
						// P1-5: 补写段落是 AI 原始输出，未经精修；对全章再运行一次精修过滤套话/质量问题。
						// 精修后才重新生成 ChapterEndState，确保快照基于最终定稿内容。
						if s.narrativeSvc != nil {
							if refined2, refErr := s.narrativeSvc.RefineChapterContent(tenantID, chapter, novel.Title); refErr == nil && refined2 != chapter.Content {
								chapter.Content = refined2
								chapter.WordCount = countChineseChars(refined2)
								if updateErr3 := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
									"content": chapter.Content, "word_count": chapter.WordCount,
								}); updateErr3 == nil {
									logger.Printf("[ChapterService] patch content refined for ch%d", chapter.ChapterNo)
								}
							} else if refErr != nil {
								logger.Errorf("[ChapterService] patch refinement ch%d (non-fatal): %v", chapter.ChapterNo, refErr)
							}
						}
						// 补写（并精修）改变了章末内容，必须重新生成章末状态快照，
						// 否则下一章 getPreviousChapterEnding 会读到已过时的快照。
						if endState := s.generateChapterEndState(tenantID, chapter, novel); endState != "" {
							if updateErr2 := s.chapterRepo.UpdateFields(chapter.ID, chapter.NovelID, map[string]interface{}{
								"chapter_end_state": endState,
							}); updateErr2 == nil {
								chapter.NarrativeMeta.ChapterEndState = endState
								logger.Printf("[ChapterService] chapter_end_state refreshed after plot patch for ch%d", chapter.ChapterNo)
							}
						}
					}
				}
			}
		}
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
				logger.Errorf("[ChapterService] ExtractAndStorePlotPoints failed for ch%d: %v", chapter.ChapterNo, err)
			}
		}()
	}

	// 5. 自动检查并标记已解决的剧情点（伏笔/冲突）
	s.checkAndAutoResolvePlotPoints(tenantID, chapter)

	// 6. 异步更新角色声音档案（每5章更新一次主要角色的声音档案，供后续章节注入使用）
	// 至少积累5章内容后才有足够的对话样本，过早提取准确度低。
	if s.narrativeSvc != nil && s.characterRepo != nil && chapter.ChapterNo >= 5 && chapter.ChapterNo%5 == 0 {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[ChapterService] voice profile extraction panic: %v", r)
				}
			}()
			chars, charErr := s.characterRepo.ListByNovel(chapter.NovelID)
			if charErr != nil {
				return
			}
			for _, c := range chars {
				if c.Role != "protagonist" && c.Role != "antagonist" {
					continue // 只为主要角色提取声音档案
				}
				voiceJSON, vErr := s.narrativeSvc.ExtractCharacterVoice(tenantID, c, chapter.NovelID)
				if vErr != nil {
					logger.Errorf("[ChapterService] voice profile for %s: %v", c.Name, vErr)
					continue
				}
				voiceJSON = extractJSON(strings.TrimSpace(voiceJSON))
				if voiceJSON == "" {
					continue
				}
				c.VoiceConfig.VoiceProfile = voiceJSON
				if updateErr := s.characterRepo.Update(c); updateErr != nil {
					logger.Errorf("[ChapterService] save voice profile for %s: %v", c.Name, updateErr)
				} else {
					logger.Printf("[ChapterService] voice profile updated: character=%s novelID=%d ch=%d", c.Name, chapter.NovelID, chapter.ChapterNo)
				}
			}
		}()
	}

	// 7. 为下一章生成接续预览摘要，写入下一章的 Summary 字段（仅当该章尚无内容时）。
	// 使下一章在被生成前已有可读的摘要预览（UI 展示 + 为更后面章节提供"预期上下文"）。
	// P2-2: 值拷贝传入 goroutine，避免 goroutine 读取到后续对 chapter/novel 指针的并发修改。
	chSnapForPreview := *chapter
	novelSnapForPreview := *novel
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterService] updateNextChapterPreview panic: %v", r)
			}
		}()
		s.updateNextChapterPreview(tenantID, &chSnapForPreview, &novelSnapForPreview)
	}()

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

	// 构建精简 prompt（头+尾截取策略，覆盖全章，避免遗漏后半章解决的伏笔）
	var sb strings.Builder
	sb.WriteString("请分析以下章节内容摘录，判断哪些剧情线在本章中已明确解决（不再是悬念或未完结冲突）：\n\n")
	sb.WriteString("【章节内容摘录】\n")
	{
		const maxAutoResolveRunes = 6000
		runes := []rune(chapter.Content)
		var excerpt string
		if len(runes) <= maxAutoResolveRunes {
			excerpt = string(runes)
		} else {
			half := maxAutoResolveRunes / 2
			excerpt = string(runes[:half]) + "\n…（中间部分已省略）…\n" + string(runes[len(runes)-half:])
		}
		sb.WriteString(excerpt)
	}
	sb.WriteString("\n\n【待检查的剧情线】\n")
	for i, pp := range relevant {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, pp.Type, pp.Description))
	}
	sb.WriteString("\n只返回在本章中明确解决的序号，以JSON格式：{\"resolved_indices\":[1,3]}\n")
	sb.WriteString("若全部未解决则返回 {\"resolved_indices\":[]}")

	result, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "plot_resolution_check", sb.String(), "")
	if err != nil {
		logger.Errorf("checkAndAutoResolvePlotPoints[%d]: AI error: %v", chapter.NovelID, err)
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
			logger.Errorf("checkAndAutoResolvePlotPoints: update pp#%d: %v", pp.ID, err)
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
	VoiceProfile  string // 声音档案摘要：说话风格/口癖/禁忌用语（来自 character_voice.j2 提取）
	Archetype     string // 角色原型（dominant_ceo/reborn_villain/pure_heroine等）
}

func (s *ChapterService) getCharactersForPrompt(novelID uint) []characterForPrompt {
	if s.characterRepo == nil {
		logger.Errorf("[ChapterService] getCharactersForPrompt: characterRepo not wired, no character context injected")
		return nil
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		logger.Errorf("[ChapterService] getCharactersForPrompt: ListByNovel error: %v", err)
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
			InnerConflict: c.Meta.InnerConflict,
			CoreDesire:    c.Meta.CoreDesire,
			VoiceProfile:  formatVoiceProfile(c.VoiceConfig.VoiceProfile),
			Archetype:     c.Archetype,
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

// formatVoiceProfile 将 VoiceProfile JSON 压缩为注入 prompt 的单行摘要
func formatVoiceProfile(voiceJSON string) string {
	if voiceJSON == "" {
		return ""
	}
	var vp struct {
		VocabularyLevel     string   `json:"vocabulary_level"`
		SpeechHabits        []string `json:"speech_habits"`
		EmotionalExpression string   `json:"emotional_expression"`
		ForbiddenPhrases    []string `json:"forbidden_phrases"`
		SignatureExpressions []string `json:"signature_expressions"`
		OverallVoice        string   `json:"overall_voice"`
	}
	if err := json.Unmarshal([]byte(voiceJSON), &vp); err != nil {
		return ""
	}
	var parts []string
	if vp.OverallVoice != "" {
		parts = append(parts, "风格："+vp.OverallVoice)
	}
	if vp.VocabularyLevel != "" {
		parts = append(parts, "用词："+vp.VocabularyLevel)
	}
	if len(vp.SignatureExpressions) > 0 {
		parts = append(parts, "标志性表达：「"+strings.Join(vp.SignatureExpressions, "」「")+"」")
	}
	if len(vp.ForbiddenPhrases) > 0 && len(vp.ForbiddenPhrases) <= 3 {
		parts = append(parts, "⚠️禁止使用：「"+strings.Join(vp.ForbiddenPhrases, "」「")+"」")
	}
	if vp.EmotionalExpression != "" {
		parts = append(parts, "情绪表达："+vp.EmotionalExpression)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；")
}

func (s *ChapterService) getCharacterVoices(novelID uint) []characterForPrompt {
	// 同 getCharactersForPrompt，供 chapter_from_outline.j2 的 Characters 变量使用
	return s.getCharactersForPrompt(novelID)
}

// formatCharacterStates 将角色列表（含快照状态）格式化为可注入 prompt 的状态描述文本
func formatCharacterStates(chars []characterForPrompt) string {
	if len(chars) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range chars {
		if c.CurrentState != "" {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", c.Name, c.CurrentState))
		}
	}
	return sb.String()
}

// buildCharacterStateString 生成主角当前状态的结构化描述，优先注入场景大纲 prompt
// 这是防止主角漂移最重要的上下文

func (s *ChapterService) buildForeshadowHints(novelID uint, chapterNo int) string {
	var hints strings.Builder
	count := 0
	// P2-8: 跨来源去重集合，防止 ForeshadowRepo 与 PlotPointRepo 注入相同内容。
	// key 格式：来源前缀 + 标题/描述前12字符。
	seen := make(map[string]bool)

	// 来源1：专用伏笔表（带生命周期分类）
	if s.foreshadowRepo != nil {
		foreshadows, err := s.foreshadowRepo.ListUnfulfilled(novelID)
		if err == nil {
			// 按重要程度排序：critical > major > normal
			urgency := func(f *model.Foreshadow) int {
				switch f.Meta.Importance {
				case "critical":
					return 3
				case "major":
					return 2
				default:
					return 1
				}
			}
			// 分类显示
			for _, fs := range foreshadows {
				if count >= 5 {
					break
				}
				// 去重：优先以 Title 为键，无 Title 则取 Description 前12字符
				dedupKey := "fs1:" + fs.Title
				if fs.Title == "" {
					r := []rune(fs.Meta.Description)
					end := 12
					if len(r) < end {
						end = len(r)
					}
					dedupKey = "fs1:" + string(r[:end])
				}
				if seen[dedupKey] {
					continue
				}
				seen[dedupKey] = true
				// 判断生命周期状态
				lifecycle := "planted"
				if fs.PayoffChapterNo > 0 {
					remaining := fs.PayoffChapterNo - chapterNo
					if remaining <= 3 && remaining >= 0 {
						lifecycle = "ripening" // 临近回收时机
					} else if remaining < 0 {
						lifecycle = "overdue" // 已过预期回收时机
					}
				}
				var prefix string
				switch lifecycle {
				case "overdue":
					prefix = fmt.Sprintf("⚠️ 【逾期未回收】「%s」（第%d章种下，预计第%d章回收，已过期%d章）",
						fs.Title, fs.PlantedChapterNo, fs.PayoffChapterNo, chapterNo-fs.PayoffChapterNo)
				case "ripening":
					prefix = fmt.Sprintf("🔔 【即将回收】「%s」（第%d章种下，预计第%d章回收，还剩%d章）",
						fs.Title, fs.PlantedChapterNo, fs.PayoffChapterNo, fs.PayoffChapterNo-chapterNo)
				default:
					if urgency(fs) >= 2 {
						prefix = fmt.Sprintf("📌 【重要伏笔】「%s」（第%d章种下）",
							fs.Title, fs.PlantedChapterNo)
					} else {
						prefix = fmt.Sprintf("- 未回收伏笔：「%s」（第%d章种下）",
							fs.Title, fs.PlantedChapterNo)
					}
				}
				if fs.Meta.Description != "" {
					hints.WriteString(prefix + "：" + fs.Meta.Description + "\n")
				} else {
					hints.WriteString(prefix + "\n")
				}
				count++
			}
		}
	}

	// 来源2：旧伏笔系统（ForeshadowService）
	if s.contextSvc != nil && s.contextSvc.foreshadowSvc != nil && count < 3 {
		foreshadows, err := s.contextSvc.foreshadowSvc.CheckForeshadowStatus(novelID, chapterNo)
		if err == nil {
			for _, fs := range foreshadows {
				if count >= 5 {
					break
				}
				if !fs.IsFulfilled && chapterNo-fs.ChapterNo >= 3 {
					r := []rune(fs.Description)
					end := 12
					if len(r) < end {
						end = len(r)
					}
					dedupKey := "fs2:" + string(r[:end])
					if seen[dedupKey] {
						continue
					}
					seen[dedupKey] = true
					hints.WriteString(fmt.Sprintf("- 请考虑回收伏笔：「%s」（第%d章埋设）\n", fs.Description, fs.ChapterNo))
					count++
				}
			}
		}
	}

	// 来源3：PlotPoint 表中未解决的伏笔与冲突（最多补充至5条）
	if s.plotPointRepo != nil && count < 5 {
		pps, err := s.plotPointRepo.ListByNovel(novelID, "", true) // unresolved only
		if err == nil {
			for _, pp := range pps {
				if count >= 5 {
					break
				}
				r := []rune(pp.Description)
				end := 12
				if len(r) < end {
					end = len(r)
				}
				dedupKey := "pp3:" + pp.Type + ":" + string(r[:end])
				if seen[dedupKey] {
					continue
				}
				switch pp.Type {
				case "foreshadow":
					seen[dedupKey] = true
					hints.WriteString(fmt.Sprintf("- 未回收伏笔：「%s」\n", pp.Description))
					count++
				case "conflict":
					seen[dedupKey] = true
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

func (s *ChapterService) getPreviousChapterEnding(tenantID uint, novel *model.Novel, chapterNo int) string {
	if chapterNo <= 1 {
		return "（本章为开篇）"
	}
	novelID := novel.ID
	prev, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo-1)
	if err != nil || prev == nil {
		return ""
	}

	// 若上一章已有内容但尚无 ChapterEndState（在本功能上线前生成的章节），立即同步生成并持久化。
	// 这样旧章节只需生成一次，此后每次读取都有结构化锚点。
	if prev.NarrativeMeta.ChapterEndState == "" && prev.Content != "" && s.aiService != nil {
		logger.Printf("[getPreviousChapterEnding] ch%d: ChapterEndState missing, generating on-demand", chapterNo-1)
		if endState := s.generateChapterEndState(tenantID, prev, novel); endState != "" {
			prev.NarrativeMeta.ChapterEndState = endState
			_ = s.chapterRepo.Update(prev)
			logger.Printf("[getPreviousChapterEnding] ch%d: ChapterEndState generated and saved", chapterNo-1)
		}
	}

	var sb strings.Builder

	// 1. 优先使用结构化章末状态快照（最精确的连续性锚点）
	if prev.NarrativeMeta.ChapterEndState != "" {
		var endState struct {
			Characters []struct {
				Name       string `json:"name"`
				Location   string `json:"location"`
				State      string `json:"state"`
				LastAction string `json:"last_action"`
			} `json:"characters"`
			SceneEnd      string `json:"scene_end"`
			PendingAction string `json:"pending_action"`
			OpeningHint   string `json:"opening_hint"`
		}
		if parseErr := json.Unmarshal([]byte(prev.NarrativeMeta.ChapterEndState), &endState); parseErr == nil {
			// 强制接续锚——openingHint 优先放在最顶部，作为本章第一段的硬约束
			if endState.OpeningHint != "" {
				sb.WriteString("━━ 本章第一段强制开头（不可跳过，不可替换为背景交代或心理独白）━━\n")
				sb.WriteString("「" + endState.OpeningHint + "」\n")
				sb.WriteString("（可调整措辞，但必须保持这个具体动作/感知，且必须是本章正文的第一句）\n\n")
			}
			sb.WriteString("【章末精确状态——本章必须从以下状态直接接续，禁止重复任何已发生情节】\n")
			if endState.SceneEnd != "" {
				sb.WriteString("场景：" + endState.SceneEnd + "\n")
			}
			for _, c := range endState.Characters {
				sb.WriteString(fmt.Sprintf("  %s → 位置：%s | 状态：%s | 最后动作：%s\n",
					c.Name, c.Location, c.State, c.LastAction))
			}
			if endState.PendingAction != "" {
				sb.WriteString("⚡ 未完成动作/冲突（下章第一场景必须立即接续此处）：" + endState.PendingAction + "\n")
			}
		} else {
			// JSON 解析失败，直接用原始内容
			sb.WriteString("【章末状态】" + prev.NarrativeMeta.ChapterEndState)
		}
	} else if prev.NarrativeMeta.ChapterHook != "" {
		// 2. 次优：章末钩子（情感悬念点）
		sb.WriteString("【章末悬念】" + prev.NarrativeMeta.ChapterHook)
	} else if prev.Summary != "" {
		// 3. 降级：摘要
		sb.WriteString("【上章摘要】" + prev.Summary)
	} else {
		// 4. 最终降级：末尾 300 字（此分支已含原文，下方不再重复追加）
		content := []rune(prev.Content)
		if len(content) > 300 {
			content = content[len(content)-300:]
		}
		sb.WriteString("【上章结尾】…" + string(content))
	}

	// 仅在无结构化状态时，才附加主角快照（结构化状态已包含角色位置信息）
	if prev.NarrativeMeta.ChapterEndState == "" && s.characterRepo != nil && s.snapshotRepo != nil {
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

	// 总是追加上章正文末尾原文（400 字）作为直接接续锚。
	// 结构化 JSON 状态描述的是"情节坐标"，但 AI 需要读到真实的散文句子才能在
	// 语感和叙事节奏上无缝延续。此原文段落置于最后，确保 AI 最后读到的是可以
	// 直接在后面续写的真实文字，而不是抽象的状态描述。
	// 仅当 prev.Content 有内容时追加；最终降级分支已展示过 300 字时仍可追加更多以增强约束。
	if prev.Content != "" {
		runes := []rune(prev.Content)
		n := 400
		if len(runes) < n {
			n = len(runes)
		}
		rawEnding := string(runes[len(runes)-n:])
		sb.WriteString("\n\n【上章正文末尾原文 — 本章第一句必须在叙事上接在此处之后，不得重置场景】\n")
		sb.WriteString("……" + rawEnding + "\n")
		sb.WriteString("↑ 本章第一句必须直接延伸以上散文（人物动作/位置/情绪保持连续，不得另起炉灶）\n")
	}

	return sb.String()
}

// buildPreviousReaderExpectations fetches the reader_expectations field of the previous
// chapter (if any) and formats it as a numbered list for prompt injection.
func (s *ChapterService) buildPreviousReaderExpectations(novelID uint, chapterNo int) string {
	if chapterNo <= 1 {
		return ""
	}
	prev, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo-1)
	if err != nil || prev == nil || prev.NarrativeMeta.ReaderExpectations == "" {
		return ""
	}
	var expectations []string
	if err := json.Unmarshal([]byte(prev.NarrativeMeta.ReaderExpectations), &expectations); err != nil {
		return prev.NarrativeMeta.ReaderExpectations // fallback: return raw string
	}
	if len(expectations) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, exp := range expectations {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, exp))
	}
	return sb.String()
}

// buildCharacterArcContext formats the core desire and inner conflict of protagonist/antagonist
// characters into a compact prompt section.
func (s *ChapterService) buildCharacterArcContext(novelID uint, chapterNo int) string {
	if s.characterRepo == nil {
		return ""
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil || len(chars) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range chars {
		// Only inject for major characters (protagonist/antagonist)
		role := strings.ToLower(c.Role)
		if role != "protagonist" && role != "antagonist" && role != "主角" && role != "反派" {
			continue
		}
		if c.Meta.CoreDesire == "" && c.Meta.InnerConflict == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("**%s**（%s）\n", c.Name, c.Role))
		if c.Meta.CoreDesire != "" {
			sb.WriteString(fmt.Sprintf("  核心渴望：%s\n", c.Meta.CoreDesire))
		}
		if c.Meta.InnerConflict != "" {
			sb.WriteString(fmt.Sprintf("  内在矛盾：%s\n", c.Meta.InnerConflict))
		}
	}
	return sb.String()
}

// buildTensionBudget checks if recent chapters have been consecutively high-tension.
// If 3+ consecutive chapters have tension >= 7, warns to insert a recovery chapter.
func (s *ChapterService) buildTensionBudget(novelID uint, chapterNo, currentTension int) string {
	if chapterNo < 4 {
		return ""
	}
	// 一次批量查询前3章，避免 N+1（3次独立查询 → 1次范围查询）
	prevChapters, err := s.chapterRepo.GetByNovelAndChapterRange(novelID, chapterNo-3, chapterNo-1)
	if err != nil || len(prevChapters) == 0 {
		return ""
	}
	// 建立 chapterNo → chapter 映射，便于按顺序访问
	chMap := make(map[int]*model.Chapter, len(prevChapters))
	for _, ch := range prevChapters {
		chMap[ch.ChapterNo] = ch
	}
	highCount := 0
	for lookback := 1; lookback <= 3; lookback++ {
		ch := chMap[chapterNo-lookback]
		if ch == nil {
			break
		}
		if ch.NarrativeMeta.TensionLevel >= 7 {
			highCount++
		} else {
			break // stop at first non-high chapter
		}
	}
	if highCount >= 3 {
		return fmt.Sprintf("⚠️ 张力预算警告：前%d章持续高张力（≥7），本章建议插入缓冲场景（至少1个张力≤4的场景），让读者喘息并强化下一轮高张力的冲击力。", highCount)
	}
	if currentTension <= 4 && highCount >= 2 {
		return "✅ 缓冲章：本章张力适当降低，有利于节奏恢复。建议至少1个场景深化人物关系或揭示背景信息，为下一波高张力做情感铺垫。"
	}
	return ""
}

// buildFinalChapterContext 为最终章收集所有必须解决的悬线（伏笔、冲突、角色弧光）。
// 普通章节只注入≤5条提示；最终章必须关闭全部悬线——不能遗漏。
func (s *ChapterService) buildFinalChapterContext(novelID uint, novel *model.Novel) string {
	var sb strings.Builder
	sb.WriteString("━━ 最终章收尾清单 ━━\n")
	sb.WriteString("以下所有悬线必须在本章内完整解决，不得留下任何开放性悬念。\n\n")
	itemCount := 0

	// 1. 所有未回收的伏笔（全量，不限条数）
	if s.foreshadowRepo != nil {
		foreshadows, err := s.foreshadowRepo.ListUnfulfilled(novelID)
		if err == nil && len(foreshadows) > 0 {
			sb.WriteString("【必须回收的伏笔】\n")
			for _, f := range foreshadows {
				line := fmt.Sprintf("❌ 「%s」（第%d章种下）", f.Title, f.PlantedChapterNo)
				if f.Meta.Description != "" {
					line += "：" + f.Meta.Description
				}
				sb.WriteString(line + "\n")
				itemCount++
			}
			sb.WriteString("\n")
		}
	}

	// 2. 所有未解决的冲突/剧情点（全量）
	if s.plotPointRepo != nil {
		pps, err := s.plotPointRepo.ListByNovel(novelID, "", true)
		if err == nil && len(pps) > 0 {
			sb.WriteString("【必须解决的冲突/剧情点】\n")
			for _, pp := range pps {
				if pp.Type == "foreshadow" || pp.Type == "conflict" || pp.Type == "twist" {
					sb.WriteString(fmt.Sprintf("❌ [%s] %s\n", pp.Type, pp.Description))
					itemCount++
				}
			}
			sb.WriteString("\n")
		}
	}

	// 3. 主要角色内在成长收尾（主角/反派的核心渴望/内在矛盾必须得到回应）
	if s.characterRepo != nil {
		chars, err := s.characterRepo.ListByNovel(novelID)
		if err == nil {
			arcCount := 0
			var arcBuf strings.Builder
			for _, c := range chars {
				role := strings.ToLower(c.Role)
				if role != "protagonist" && role != "antagonist" && role != "主角" && role != "反派" && role != "男主" && role != "女主" {
					continue
				}
				if c.Meta.CoreDesire == "" && c.Meta.InnerConflict == "" {
					continue
				}
				arcBuf.WriteString(fmt.Sprintf("- **%s**（%s）", c.Name, c.Role))
				if c.Meta.CoreDesire != "" {
					arcBuf.WriteString(fmt.Sprintf("：核心渴望「%s」", c.Meta.CoreDesire))
				}
				if c.Meta.InnerConflict != "" {
					arcBuf.WriteString(fmt.Sprintf(" / 内在矛盾「%s」", c.Meta.InnerConflict))
				}
				arcBuf.WriteString("\n")
				arcCount++
				itemCount++
			}
			if arcCount > 0 {
				sb.WriteString("【角色成长必须完成】\n")
				sb.WriteString(arcBuf.String())
				sb.WriteString("\n")
			}
		}
	}

	// 4. 全书核心矛盾（来自 novel.Summary 或 novel.Meta.CoreTheme）
	if novel.Meta.CoreTheme != "" || novel.Meta.Description != "" {
		sb.WriteString("【全书核心矛盾的最终答案】\n")
		if novel.Meta.CoreTheme != "" {
			sb.WriteString("主题：" + novel.Meta.CoreTheme + "\n")
		}
		if novel.Meta.Description != "" {
			sb.WriteString("故事核心：" + truncateForPrompt(novel.Meta.Description, 200) + "\n")
		}
		sb.WriteString("→ 本章必须给出这一矛盾的最终答案（圆满/震撼/余韵均可，但不能回避）\n\n")
	}

	if itemCount == 0 && novel.Meta.CoreTheme == "" && novel.Meta.Description == "" {
		// 什么显式悬线都没有，给出通用收尾指导
		return "本章为最终章，必须完整收束全部故事线：核心矛盾解决，主角命运明确，情感落地，不留开放性悬念。\n"
	}

	sb.WriteString("━━ 收尾规则 ━━\n")
	sb.WriteString("- 上述每条 ❌ 标记的悬线必须在本章转变为 ✅（明确解决或揭示）\n")
	sb.WriteString("- 不允许用「也许」「或许」「留待将来」等模糊语言回避解决\n")
	sb.WriteString("- 每条悬线的解决必须通过具体场景/对话/事件展现，不能只用叙述性一笔带过\n")
	return sb.String()
}

// updateNextChapterPreview 在当前章节写完并后处理完成后，为下一章生成接续预览摘要。
//
// 目的：
//  1. 用户在 UI 中可即时看到下一章的预期内容（不必等到下一章真正生成）
//  2. 为下下章（N+2）的 BuildHierarchicalContext 提供"下一章（N+1）的预期内容"上下文
//     ——即使 N+1 尚未写完，BuildHierarchicalContext 可读到 N+1 的预览摘要，
//     使 N+2 的生成在叙事上更自然地承接 N+1 的预期走向。
//
// 条件：下一章必须已存在（大纲占位章节）且尚无正文；
//       下一章已有真实摘要时跳过（避免覆盖）。
func (s *ChapterService) updateNextChapterPreview(tenantID uint, chapter *model.Chapter, novel *model.Novel) {
	nextNo := chapter.ChapterNo + 1
	next, err := s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, nextNo)
	if err != nil || next == nil {
		return // 下一章不存在（当前章是最终章，或大纲尚未生成占位章）
	}
	if next.Content != "" {
		return // 下一章已有正文，真实摘要更有价值，不覆盖
	}
	if next.Summary != "" {
		return // 下一章已有摘要（可能是上次运行写入的预览或用户手写），不重复生成
	}

	// 构建"章末状态"上下文：结构化快照 > 章末钩子 > 章节摘要
	endingCtx := chapter.NarrativeMeta.ChapterEndState
	if endingCtx == "" {
		endingCtx = chapter.NarrativeMeta.ChapterHook
	}
	if endingCtx == "" {
		endingCtx = chapter.Summary
	}

	prompt, renderErr := renderPrompt("next_chapter_preview", map[string]interface{}{
		"NovelTitle":         novel.Title,
		"CurrentChapterNo":   chapter.ChapterNo,
		"CurrentSummary":     chapter.Summary,
		"CurrentEnding":      endingCtx,
		"NextChapterNo":      nextNo,
		"NextChapterTitle":   next.Title,
		"NextChapterOutline": next.NarrativeMeta.Outline,
	})
	if renderErr != nil {
		logger.Errorf("[ChapterService] updateNextChapterPreview: render failed: %v", renderErr)
		return
	}

	preview, aiErr := s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, chapter.NovelID,
		"next_chapter_preview", prompt, "", StoryboardOverrides{})
	if aiErr != nil {
		logger.Errorf("[ChapterService] updateNextChapterPreview ch%d→ch%d: %v", chapter.ChapterNo, nextNo, aiErr)
		return
	}
	preview = strings.TrimSpace(preview)
	if len([]rune(preview)) < 30 {
		return
	}

	// 写入前再次确认下一章仍无摘要（避免并发写入冲突）
	if latest, fetchErr := s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, nextNo); fetchErr == nil {
		if latest.Summary != "" || latest.Content != "" {
			return
		}
		latest.Summary = preview
		if updateErr := s.chapterRepo.Update(latest); updateErr != nil {
			logger.Errorf("[ChapterService] updateNextChapterPreview: save ch%d: %v", nextNo, updateErr)
		} else {
			logger.Printf("[ChapterService] updateNextChapterPreview: ch%d preview summary set (len=%d chars)", nextNo, len([]rune(preview)))
		}
	}
}

// generateReaderExpectations calls AI to extract what readers most want to know after this chapter.
func (s *ChapterService) generateReaderExpectations(tenantID uint, chapter *model.Chapter, novel *model.Novel) string {
	if chapter.Summary == "" {
		return ""
	}
	prompt, err := renderPrompt("reader_expectation", map[string]interface{}{
		"NovelTitle":   novel.Title,
		"Genre":        novel.Meta.Genre,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"ChapterHook":  chapter.NarrativeMeta.ChapterHook,
		"Summary":      chapter.Summary,
	})
	if err != nil {
		logger.Errorf("[ChapterService] generateReaderExpectations: render template: %v", err)
		return ""
	}
	result, err := s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, chapter.NovelID, "reader_expectation", prompt, "", StoryboardOverrides{})
	if err != nil {
		logger.Errorf("[ChapterService] generateReaderExpectations ch%d: AI error: %v", chapter.ChapterNo, err)
		return ""
	}
	// extractJSONObject (not extractJSON) — extractJSON unwraps {"k":[...]} → [...], losing the wrapper.
	result = strings.TrimSpace(extractJSONObject(result))
	var parsed struct {
		ReaderExpectations []string `json:"reader_expectations"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil || len(parsed.ReaderExpectations) == 0 {
		logger.Errorf("[ChapterService] generateReaderExpectations ch%d: unexpected format: %v (raw: %.200s)", chapter.ChapterNo, err, result)
		return ""
	}
	out, _ := json.Marshal(parsed.ReaderExpectations)
	return string(out)
}

// generateChapterEndState extracts a structured end-state snapshot from chapter content.
// This snapshot is the primary continuity anchor used by the next chapter's getPreviousChapterEnding.
// It records exact character positions/states/last-actions, scene, and any pending action.
func (s *ChapterService) generateChapterEndState(tenantID uint, chapter *model.Chapter, novel *model.Novel) string {
	if chapter.Content == "" {
		return ""
	}
	// 头尾双窗口：章首500字（记录开场状态）+ 章末1000字（记录结束状态）。
	// 纯末尾截取会遗漏章节中段的重要状态变化（受伤/换位/获得信息）。
	content := []rune(chapter.Content)
	var ending string
	const headRunes, tailRunes = 500, 1000
	if len(content) <= headRunes+tailRunes {
		ending = string(content)
	} else {
		ending = string(content[:headRunes]) +
			"\n…（中间段已省略）…\n" +
			string(content[len(content)-tailRunes:])
	}
	prompt, err := renderPrompt("chapter_end_state", map[string]interface{}{
		"NovelTitle":    novel.Title,
		"ChapterNo":     chapter.ChapterNo,
		"ChapterTitle":  chapter.Title,
		"ChapterEnding": ending,
	})
	if err != nil {
		logger.Errorf("[generateChapterEndState] render prompt error: %v", err)
		return ""
	}
	result, err := s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, chapter.NovelID, "chapter_end_state", prompt, "", StoryboardOverrides{})
	if err != nil {
		logger.Errorf("[generateChapterEndState] ch%d AI error: %v", chapter.ChapterNo, err)
		return ""
	}
	// extractJSONObject — extractJSON would unwrap {"characters":[...]} → [...], losing the whole structure.
	cleaned := strings.TrimSpace(extractJSONObject(result))
	var check struct {
		Characters    []map[string]string `json:"characters"`
		SceneEnd      string              `json:"scene_end"`
		PendingAction string              `json:"pending_action"`
		OpeningHint   string              `json:"opening_hint"`
	}
	if jsonErr := json.Unmarshal([]byte(cleaned), &check); jsonErr != nil {
		logger.Errorf("[generateChapterEndState] ch%d invalid JSON: %v (raw: %.200s)", chapter.ChapterNo, jsonErr, cleaned)
		return ""
	}
	return cleaned
}

// checkAndPatchMissingPlotPoints checks whether all outline plot points are covered in the
// generated chapter content. If any are missing, it calls AI to write targeted patches
// and integrates them into the chapter content. This directly solves 章节内容和章节大纲相关性低.
func (s *ChapterService) checkAndPatchMissingPlotPoints(tenantID uint, chapter *model.Chapter, novel *model.Novel, plotPoints []string) bool {
	if len(plotPoints) == 0 || chapter.Content == "" {
		return false
	}

	// 截断策略：取前4000字 + 后4000字，覆盖全章而不只看前段。
	// 只截前段会把后半章中已发生的剧情点误报为"缺失"，触发多余补写。
	content := chapter.Content
	contentRunes := []rune(content)
	const maxContentRunes = 8000
	if len(contentRunes) > maxContentRunes {
		half := maxContentRunes / 2
		content = string(contentRunes[:half]) + "\n…（中间部分已省略）…\n" + string(contentRunes[len(contentRunes)-half:])
	}

	plotPointsText := ""
	for _, pp := range plotPoints {
		plotPointsText += "- " + pp + "\n"
	}

	prompt, err := renderPrompt("chapter_plot_compliance", map[string]interface{}{
		"NovelTitle":     novel.Title,
		"ChapterNo":      chapter.ChapterNo,
		"PlotPoints":     plotPointsText,
		"ChapterContent": content,
	})
	if err != nil {
		logger.Errorf("[checkAndPatchMissingPlotPoints] render prompt error: %v", err)
		return false
	}

	result, err := s.aiService.GenerateWithProviderCtx(context.Background(), tenantID, chapter.NovelID, "chapter_plot_compliance", prompt, "", StoryboardOverrides{})
	if err != nil {
		logger.Errorf("[checkAndPatchMissingPlotPoints] ch%d AI error: %v", chapter.ChapterNo, err)
		return false
	}

	// extractJSONObject — extractJSON would strip {"coverage":[...],"patches":[...]} to the inner array.
	cleaned := strings.TrimSpace(extractJSONObject(result))
	var complianceResult struct {
		Coverage []struct {
			PlotPoint string `json:"plot_point"`
			Covered   bool   `json:"covered"`
			Evidence  string `json:"evidence"`
		} `json:"coverage"`
		Patches []struct {
			PlotPoint   string `json:"plot_point"`
			InsertAfter string `json:"insert_after"`
			Content     string `json:"content"`
		} `json:"patches"`
	}
	if jsonErr := json.Unmarshal([]byte(cleaned), &complianceResult); jsonErr != nil {
		logger.Errorf("[checkAndPatchMissingPlotPoints] ch%d parse error: %v", chapter.ChapterNo, jsonErr)
		return false
	}

	// Log coverage status
	uncovered := 0
	for _, cov := range complianceResult.Coverage {
		if !cov.Covered {
			uncovered++
			logger.Printf("[checkAndPatchMissingPlotPoints] ch%d MISSING plot point: %s", chapter.ChapterNo, truncateForPrompt(cov.PlotPoint, 50))
		}
	}
	if uncovered == 0 || len(complianceResult.Patches) == 0 {
		logger.Printf("[checkAndPatchMissingPlotPoints] ch%d all plot points covered (%d total)", chapter.ChapterNo, len(complianceResult.Coverage))
		return false
	}

	// Apply patches: integrate missing plot point content into the chapter.
	// 保护章末钩子：【章末钩子】标记之后的内容是悬念段，补写内容必须插在钩子之前，
	// 否则会破坏章末悬念效果（读者读到的最后一句应是钩子，而非补写的剧情段）。
	const hookMarker = "【章末钩子】"
	patched := chapter.Content
	hookIdx := strings.LastIndex(patched, hookMarker)
	for _, p := range complianceResult.Patches {
		if p.Content == "" {
			continue
		}
		if p.InsertAfter == "章节末尾" || p.InsertAfter == "" {
			if hookIdx >= 0 {
				// 有章末钩子：插在钩子之前，保护悬念位置
				patched = patched[:hookIdx] + p.Content + "\n\n" + patched[hookIdx:]
				hookIdx += len(p.Content) + 2 // 更新 hook 位置偏移
			} else {
				patched = patched + "\n\n" + p.Content
			}
		} else {
			// P1-5: 模糊锚点匹配 — AI 的 insert_after 是段落前15字，可能因精修/格式差异导致精确匹配失败。
			// 依次尝试原始锚点 → 前10字 → 前8字 → 前6字，首次命中即用。
			anchorRunes := []rune(p.InsertAfter)
			matchIdx := -1
			for _, tryLen := range []int{len(anchorRunes), 10, 8, 6} {
				if tryLen <= 0 || tryLen > len(anchorRunes) {
					continue
				}
				if idx := strings.Index(patched, string(anchorRunes[:tryLen])); idx >= 0 {
					matchIdx = idx
					break
				}
			}
			if matchIdx >= 0 {
				// 命中锚点：在锚点所在段落之后插入
				paraEnd := strings.Index(patched[matchIdx:], "\n\n")
				if paraEnd >= 0 {
					insertPos := matchIdx + paraEnd + 2
					patched = patched[:insertPos] + p.Content + "\n\n" + patched[insertPos:]
					if hookIdx >= insertPos {
						hookIdx += len(p.Content) + 2
					}
				} else {
					// 锚点段落后无双换行，退化为追加（仍保护钩子）
					if hookIdx >= 0 {
						patched = patched[:hookIdx] + p.Content + "\n\n" + patched[hookIdx:]
						hookIdx += len(p.Content) + 2
					} else {
						patched = patched + "\n\n" + p.Content
					}
				}
			} else {
				// 所有锚点长度均未命中，退化为追加（仍保护钩子）
				if hookIdx >= 0 {
					patched = patched[:hookIdx] + p.Content + "\n\n" + patched[hookIdx:]
					hookIdx += len(p.Content) + 2
				} else {
					patched = patched + "\n\n" + p.Content
				}
			}
		}
		logger.Printf("[checkAndPatchMissingPlotPoints] ch%d patched missing plot point: %s",
			chapter.ChapterNo, truncateForPrompt(p.PlotPoint, 50))
	}

	if patched != chapter.Content {
		chapter.Content = patched
		chapter.WordCount = countChineseChars(patched)
		return true
	}
	return false
}

// computeChapterType classifies the chapter as one of four narrative types based on
// tension level and hook type. The type drives type-specific scene design rules in
// chapter_scene_outline.j2.
func computeChapterType(tensionLevel int, hookType string, actNo int) string {
	hook := strings.ToLower(hookType)
	if tensionLevel >= 8 || hook == "cliffhanger" || hook == "revelation" || hook == "大结局" {
		return "高潮章"
	}
	if tensionLevel <= 4 && actNo > 1 {
		return "反思章"
	}
	if tensionLevel >= 6 {
		return "事件章"
	}
	return "关系章"
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

func (s *ChapterService) RegenerateChapter(tenantID uint, id uint, req *model.GenerateChapterRequest) (*model.Chapter, error) {
	// Load and validate ownership
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("chapter not found")
	}
	if !s.chapterBelongsToTenant(chapter, tenantID) {
		return nil, fmt.Errorf("not found")
	}

	// Save current content as a version before overwriting
	if s.versionRepo != nil && chapter.Content != "" {
		if err := s.versionRepo.CreateAtomic(&model.ChapterVersion{
			NovelID:           chapter.NovelID,
			ChapterID:         chapter.ID,
			Content:           chapter.Content,
			ChangeType:        "generation",
			ChangeDescription: "重新生成前自动存档",
		}); err == nil {
			_ = s.versionRepo.DeleteExcessVersions(chapter.ID, 20)
		}
	}

	// Fill in the novel/chapter identity from the existing record
	req.NovelID = chapter.NovelID
	req.ChapterNo = chapter.ChapterNo

	// 审查驱动重生成：将当前章节正文注入 ReviewHints，供 prompt 模板参考
	if req.ReviewHints != nil && chapter.Content != "" {
		req.ReviewHints.ExistingContent = chapter.Content
	}

	// Reset status to "draft" so AtomicSetGenerating in GenerateChapter can acquire the lock.
	// A completed/generating chapter would otherwise be blocked by the optimistic-lock guard.
	_ = s.chapterRepo.UpdateStatus(chapter.ID, chapter.NovelID, "draft")

	// Delegate to the full generation pipeline (scene outline → full chapter → refinement → post-processing)
	return s.GenerateChapter(tenantID, chapter.NovelID, req)
}

// ArchiveVersionBeforeRewrite saves the current chapter content as a version before rewriting.
func (s *ChapterService) ArchiveVersionBeforeRewrite(chapterID uint, instruction string) error {
	if s.versionRepo == nil {
		return nil
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil || chapter.Content == "" {
		return err
	}
	desc := "按指令修改前自动存档"
	if instruction != "" {
		desc = fmt.Sprintf("按指令修改前存档：%s", instruction)
		if len([]rune(desc)) > 100 {
			desc = string([]rune(desc)[:100]) + "..."
		}
	}
	if err := s.versionRepo.CreateAtomic(&model.ChapterVersion{
		NovelID:           chapter.NovelID,
		ChapterID:         chapter.ID,
		Content:           chapter.Content,
		ChangeType:        "rewrite",
		ChangeDescription: desc,
	}); err == nil {
		_ = s.versionRepo.DeleteExcessVersions(chapter.ID, 20)
	}
	return nil
}

// ApplyRewrittenContent updates a chapter's content with AI-rewritten text.
func (s *ChapterService) ApplyRewrittenContent(chapterID uint, newContent string) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}
	chapter.Content = newContent
	chapter.WordCount = len([]rune(newContent))
	chapter.Status = "completed"
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, fmt.Errorf("update chapter: %w", err)
	}
	return chapter, nil
}

func (s *ChapterService) ApproveChapter(id uint, comment string) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return err
	}
	chapter.Status = "approved"
	chapter.QualityMeta.QualityStatus = "ok"
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
				logger.Errorf("BatchGenerateSummaries: ch%d: %v", ch.ChapterNo, err)
			} else {
				ch.Summary = strings.TrimSpace(summary)
				if err := s.chapterRepo.Update(ch); err != nil {
					logger.Errorf("BatchGenerateSummaries: save ch%d: %v", ch.ChapterNo, err)
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

// parseKnowledgeSearchOutput parses the output map from McpService.InvokeTool("knowledge_search", …)
// into a human-readable prompt section that can be appended to WikiContext.
func parseKnowledgeSearchOutput(output map[string]interface{}) string {
	rawResults, ok := output["results"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(rawResults)
	if err != nil {
		return ""
	}
	var items []struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(b, &items); err != nil || len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【小说知识库参考】\n")
	for _, item := range items {
		sb.WriteString("• ")
		sb.WriteString(item.Title)
		sb.WriteString("：")
		// truncate long content
		content := item.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "…"
		}
		sb.WriteString(content)
		sb.WriteString("\n")
	}
	return sb.String()
}

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

// buildReviewHintsText 将审查反馈转为模板可注入的结构化文本
func buildReviewHintsText(hints *model.ReviewHintsPayload) string {
	if hints == nil {
		return ""
	}
	var sb strings.Builder

	// 现有章节正文（审查重生成的改写基础）
	if hints.ExistingContent != "" {
		sb.WriteString("【当前版本正文——在此基础上改进，保留已经写好的情节骨架和人物关系，针对性解决下列问题】\n")
		sb.WriteString("```\n")
		sb.WriteString(hints.ExistingContent)
		sb.WriteString("\n```\n\n")
	}

	if len(hints.Weaknesses) > 0 {
		sb.WriteString("【必须解决的整体不足】\n")
		for _, w := range hints.Weaknesses {
			sb.WriteString("• " + w + "\n")
		}
	}
	if len(hints.ParagraphIssues) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("【需重点改写的段落问题】\n")
		for _, p := range hints.ParagraphIssues {
			sb.WriteString("• " + p + "\n")
		}
	}
	return sb.String()
}

