package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// AnalysisTask 分析任务状态（线程安全，仅作 goroutine 内部追踪）
type AnalysisTask struct {
	NovelID        uint               `json:"novel_id"`
	CreateOutlines bool               `json:"-"`
	cancel         context.CancelFunc `json:"-"`
	taskSvc        *TaskService       `json:"-"` // 统一任务服务（可为 nil，降级为纯内存模式）
	externalTaskID string             `json:"-"` // TaskService 分配的 task_id

	mu       sync.RWMutex `json:"-"`
	Status   string       `json:"status"`
	Progress int          `json:"progress"`
	Step     string       `json:"step"`
	Error    string       `json:"error,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
}

func (t *AnalysisTask) setStatus(s string) { t.mu.Lock(); t.Status = s; t.mu.Unlock() }
func (t *AnalysisTask) setError(e string)  { t.mu.Lock(); t.Error = e; t.mu.Unlock() }
func (t *AnalysisTask) addWarning(w string) {
	t.mu.Lock(); t.Warnings = append(t.Warnings, w); t.mu.Unlock()
}

func (t *AnalysisTask) setProgress(p int) {
	t.mu.Lock(); t.Progress = p; t.mu.Unlock()
	if t.taskSvc != nil && t.externalTaskID != "" {
		t.taskSvc.UpdateProgress(t.externalTaskID, p) //nolint:errcheck
	}
}

func (t *AnalysisTask) setStep(s string) {
	t.mu.Lock(); t.Step = s; t.mu.Unlock()
	if t.taskSvc != nil && t.externalTaskID != "" {
		t.taskSvc.SetMeta(t.externalTaskID, map[string]interface{}{"step": s}) //nolint:errcheck
	}
}

// NovelAnalysisService 小说分析服务（异步 Pipeline）
type NovelAnalysisService struct {
	novelRepo          *repository.NovelRepository
	chapterRepo        *repository.ChapterRepository
	characterRepo      *repository.CharacterRepository
	worldviewRepo      *repository.WorldviewRepository
	itemRepo           *repository.ItemRepository
	itemService        *ItemService
	novelService       *NovelService
	aiService          *AIService
	plotPointService   *PlotPointService
	sceneAnchorService *SceneAnchorService
	foreshadowSvc      *ForeshadowCRUDService
	taskSvc            *TaskService
	modelRepo          *repository.AIModelRepository // optional, for voice auto-suggestion
	lookRepo           *repository.CharacterLookRepository // optional, auto-create default look
	cleanupStop        chan struct{} // closed by Shutdown() to stop background goroutines
}

func NewNovelAnalysisService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	worldviewRepo *repository.WorldviewRepository,
	novelService *NovelService,
	aiService *AIService,
) *NovelAnalysisService {
	return &NovelAnalysisService{
		novelRepo:     novelRepo,
		chapterRepo:   chapterRepo,
		characterRepo: characterRepo,
		worldviewRepo: worldviewRepo,
		novelService:  novelService,
		aiService:     aiService,
		cleanupStop:   make(chan struct{}),
	}
}

// Shutdown stops all background goroutines (call on server exit).
func (s *NovelAnalysisService) Shutdown() {
	select {
	case <-s.cleanupStop:
		// already closed
	default:
		close(s.cleanupStop)
	}
}

// WithTaskService 注入统一任务服务（可选，注入后任务状态持久化到 DB）
func (s *NovelAnalysisService) WithTaskService(svc *TaskService) *NovelAnalysisService {
	s.taskSvc = svc
	return s
}

// WithLookRepo 注入形象仓库（可选，AI 分析角色后自动创建默认形象）
func (s *NovelAnalysisService) WithLookRepo(r *repository.CharacterLookRepository) *NovelAnalysisService {
	s.lookRepo = r
	return s
}

// WithItemRepo 注入物品仓库（可选，支持物品提取步骤）
func (s *NovelAnalysisService) WithItemRepo(itemRepo *repository.ItemRepository) *NovelAnalysisService {
	s.itemRepo = itemRepo
	return s
}

// WithItemService 注入物品服务（可选，启用逐章并发提取）
func (s *NovelAnalysisService) WithItemService(svc *ItemService) *NovelAnalysisService {
	s.itemService = svc
	return s
}

// WithPlotPointService 注入剧情点服务（可选，支持剧情点提取步骤）
func (s *NovelAnalysisService) WithPlotPointService(svc *PlotPointService) *NovelAnalysisService {
	s.plotPointService = svc
	return s
}

// WithSceneAnchorService 注入场景锚点服务（可选，支持场景锚点提取步骤）
func (s *NovelAnalysisService) WithSceneAnchorService(svc *SceneAnchorService) *NovelAnalysisService {
	s.sceneAnchorService = svc
	return s
}

// WithForeshadowService 注入伏笔服务（可选，支持伏笔提取步骤）
func (s *NovelAnalysisService) WithForeshadowService(svc *ForeshadowCRUDService) *NovelAnalysisService {
	s.foreshadowSvc = svc
	return s
}

// WithModelRepo 注入模型仓库（可选，启用角色音色自动推荐）
func (s *NovelAnalysisService) WithModelRepo(r *repository.AIModelRepository) *NovelAnalysisService {
	s.modelRepo = r
	return s
}

// StartAnalysis 启动分析任务，返回 task_id（统一 TaskService ID）
func (s *NovelAnalysisService) StartAnalysis(tenantID, novelID uint, createOutlines bool) (string, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return "", fmt.Errorf("novel not found: %w", err)
	}

	// 优先使用 TaskService（持久化）；若未注入则降级为随机 UUID
	externalTaskID := uuid.New().String()
	if s.taskSvc != nil {
		if dbTask, err := s.taskSvc.Create(tenantID, TaskTypeNovelAnalysis, "小说分析", "novel", novelID); err == nil {
			externalTaskID = dbTask.TaskID
			_ = s.taskSvc.SetParams(externalTaskID, map[string]interface{}{
				"create_outlines": createOutlines,
			})
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := &AnalysisTask{
		NovelID:        novelID,
		Status:         "pending",
		Progress:       0,
		Step:           "准备中",
		CreateOutlines: createOutlines,
		cancel:         cancel,
		taskSvc:        s.taskSvc,
		externalTaskID: externalTaskID,
	}

	logger.Printf("[NovelAnalysis] StartAnalysis: novelID=%d taskID=%s", novel.ID, externalTaskID)
	go s.runPipeline(ctx, task, tenantID, novel)
	return externalTaskID, nil
}

// ResumeAnalysis resumes an orphaned novel_analysis task reusing its existing task ID.
func (s *NovelAnalysisService) ResumeAnalysis(t *model.AsyncTask, createOutlines bool) {
	novel, err := s.novelRepo.GetByID(t.EntityID)
	if err != nil {
		if s.taskSvc != nil {
			_ = s.taskSvc.Fail(t.TaskID, "novel not found: "+err.Error())
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if s.taskSvc != nil {
		s.taskSvc.RegisterCancel(t.TaskID, cancel)
	}
	task := &AnalysisTask{
		NovelID:        t.EntityID,
		Status:         "pending",
		Progress:       0,
		Step:           "恢复中...",
		CreateOutlines: createOutlines,
		cancel:         cancel,
		taskSvc:        s.taskSvc,
		externalTaskID: t.TaskID,
	}
	logger.Printf("[NovelAnalysis] ResumeAnalysis: novelID=%d taskID=%s", t.EntityID, t.TaskID)
	go s.runPipeline(ctx, task, t.TenantID, novel)
}

// ──────────────────────────────────────────────
// Pipeline 内部实现
// ──────────────────────────────────────────────

func (s *NovelAnalysisService) runPipeline(ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel) {
	logger.Printf("[NovelAnalysis] runPipeline start: novelID=%d", novel.ID)
	defer func() {
		if task.cancel != nil {
			task.cancel()
		}
	}()
	// 顶层 panic 捕获：任何未预期的 panic 都调 fail() 而不是让进程崩溃
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("internal panic: %v", r)
			logger.Errorf("[NovelAnalysis] PANIC in runPipeline novelID=%d: %v", novel.ID, r)
			s.fail(task, msg)
		}
	}()
	task.setStatus("running")
	if task.taskSvc != nil && task.externalTaskID != "" {
		task.taskSvc.SetRunning(task.externalTaskID) //nolint:errcheck
	}

	// 预检：确认 AI 提供商可用，否则立即报错，避免全流程静默空跑
	if err := s.aiService.CheckAvailability(tenantID); err != nil {
		s.fail(task, "AI 提供商未配置或不可用，请在「模型管理」页面为至少一个文本生成提供商添加 API Key（错误："+err.Error()+"）")
		return
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(novel.ID)
	if err != nil {
		s.fail(task, "获取章节列表失败: "+err.Error())
		return
	}

	// ── Phase 1: 章节摘要 (0→20)，最多 3 分钟 ──────────────────────────────
	if len(chapters) > 0 {
		task.setStep("正在生成章节摘要...")
		phase1Ctx, phase1Cancel := context.WithTimeout(ctx, 3*time.Minute)
		if err := s.stepSummarizeChapters(phase1Ctx, task, tenantID, novel, chapters); err != nil {
			logger.Errorf("NovelAnalysis[%d]: stepSummarizeChapters warn: %v", novel.ID, err)
			task.addWarning("章节摘要生成失败: " + err.Error())
		}
		phase1Cancel()
		// 刷新章节（含更新后的摘要和内容）
		if refreshed, err := s.chapterRepo.ListByNovelWithContent(novel.ID); err == nil {
			chapters = refreshed
		}
	}
	task.setProgress(20)

	// ── Phase 2: 并发提取 角色/物品/世界观/剧情点/场景锚点/伏笔 (20→70)，最多 6 分钟 ──
	task.setStep("正在同步提取角色、物品、世界观、剧情点、场景锚点、伏笔...")
	{
		phase2Ctx, phase2Cancel := context.WithTimeout(ctx, 6*time.Minute)
		defer phase2Cancel()

		type phaseTask struct {
			name string
			fn   func() error
		}
		phaseTasks := []phaseTask{
			{"角色", func() error {
				return s.stepExtractCharacters(phase2Ctx, task, tenantID, novel, chapters)
			}},
			{"物品", func() error {
				if s.itemRepo == nil {
					return nil
				}
				return s.stepExtractItems(phase2Ctx, task, tenantID, novel, chapters)
			}},
			{"世界观", func() error {
				return s.stepExtractWorldview(phase2Ctx, task, tenantID, novel, chapters)
			}},
			{"剧情点", func() error {
				return s.stepExtractPlotPoints(phase2Ctx, task, tenantID, novel, chapters)
			}},
			{"场景锚点", func() error {
				return s.stepExtractSceneAnchors(phase2Ctx, task, tenantID, novel, chapters)
			}},
			{"伏笔", func() error {
				return s.stepExtractForeshadows(phase2Ctx, task, tenantID, novel, chapters)
			}},
			}

		var phWg sync.WaitGroup
		var doneCount atomic.Int32
		total := len(phaseTasks)
		for _, pt := range phaseTasks {
			pt := pt
			phWg.Add(1)
			go func() {
				defer phWg.Done()
				defer func() {
					if r := recover(); r != nil {
						msg := fmt.Sprintf("%s提取 panic: %v", pt.name, r)
						logger.Errorf("NovelAnalysis[%d]: step%s PANIC: %v", novel.ID, pt.name, r)
						task.addWarning(msg)
					}
				}()
				if err := retryStepCtx(phase2Ctx, 3, pt.fn); err != nil {
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						logger.Warnf("NovelAnalysis[%d]: step%s skipped: phase2 timeout/cancelled (%v)", novel.ID, pt.name, err)
						task.addWarning(pt.name + "提取因超时跳过")
					} else {
						msg := fmt.Sprintf("%s提取失败（已重试3次）: %v", pt.name, err)
						logger.Errorf("NovelAnalysis[%d]: step%s error after retries: %v", novel.ID, pt.name, err)
						task.addWarning(msg)
					}
				}
				n := int(doneCount.Add(1))
				task.setProgress(20 + n*50/total)
			}()
		}
		phWg.Wait()
	}
	task.setProgress(70)

	// ── Phase 3: 大纲 + 设置 并行 (70→88)，大纲最多 5 分钟 ─────────────────
	task.setStep("正在生成大纲与设置...")
	var outline *OutlineResult
	{
		var phWg sync.WaitGroup
		phWg.Add(2)
		go func() {
			defer phWg.Done()
			outlineCtx, outlineCancel := context.WithTimeout(ctx, 5*time.Minute)
			defer outlineCancel()
			var oErr error
			outline, oErr = s.stepGenerateOutline(outlineCtx, task, tenantID, novel)
			if oErr != nil {
				logger.Errorf("NovelAnalysis[%d]: stepGenerateOutline warn: %v", novel.ID, oErr)
			}
		}()
		go func() {
			defer phWg.Done()
			settingsCtx, settingsCancel := context.WithTimeout(ctx, 2*time.Minute)
			defer settingsCancel()
			if err := s.stepUpdateNovelSettings(settingsCtx, task, tenantID, novel, chapters); err != nil {
				logger.Errorf("NovelAnalysis[%d]: stepUpdateNovelSettings warn: %v", novel.ID, err)
			}
		}()
		phWg.Wait()
	}
	task.setProgress(88)

	// ── Phase 4: 章节大纲 (88→95) ─────────────────────────────────────────────
	phase4Created := false
	if outline != nil && len(outline.Chapters) > 0 {
		logger.Printf("NovelAnalysis[%d]: Phase4 creating chapter outlines: %d chapters", novel.ID, len(outline.Chapters))
		task.setStep("正在创建章节大纲...")
		if err := s.stepCreateChapterOutlines(ctx, task, novel, outline); err != nil {
			logger.Errorf("NovelAnalysis[%d]: stepCreateChapterOutlines warn: %v", novel.ID, err)
		} else {
			phase4Created = true
		}
	} else {
		if outline == nil {
			logger.Errorf("NovelAnalysis[%d]: Phase4 skipped: outline is nil (GenerateOutline failed)", novel.ID)
		} else {
			logger.Printf("NovelAnalysis[%d]: Phase4 skipped: outline has 0 chapters", novel.ID)
		}
	}

	// ── Phase 4.5: 补跑物品/场景/设置提取（仅当 Phase2 时无章节内容，Phase4 后有摘要可用）──
	if phase4Created {
		// 补跑小说设置（genre/description/style_prompt 等）
		freshNovel, _ := s.novelRepo.GetByID(novel.ID)
		needSettings := freshNovel != nil && (freshNovel.Genre == "" || freshNovel.Genre == "unknown" ||
			freshNovel.Description == "" || freshNovel.StylePrompt == "")
		if needSettings {
			logger.Printf("NovelAnalysis[%d]: Phase4.5 re-running novel settings update", novel.ID)
			task.setStep("正在补充生成设置...")
			// 用 outline.Summary 填充临时 description，让 settings 有素材可用
			novelForSettings := *freshNovel
			if novelForSettings.Description == "" && outline != nil && outline.Summary != "" {
				novelForSettings.Description = outline.Summary
			}
			freshChaptersForSettings, _ := s.chapterRepo.ListByNovelWithContent(novel.ID)
			if err := s.stepUpdateNovelSettings(ctx, task, tenantID, &novelForSettings, freshChaptersForSettings); err != nil {
				logger.Errorf("NovelAnalysis[%d]: Phase4.5 settings update warn: %v", novel.ID, err)
			}
		}
		// 补跑物品提取
		if s.itemRepo != nil && s.itemService != nil {
			existingItems, _ := s.itemRepo.ListByNovel(novel.ID)
			if len(existingItems) == 0 {
				logger.Printf("NovelAnalysis[%d]: Phase4.5 re-running item extraction (chapters now have summaries)", novel.ID)
				task.setStep("正在补充提取物品...")
				if items, err := s.itemService.AIExtractAllFromNovel(ctx, tenantID, novel.ID); err != nil {
					logger.Errorf("NovelAnalysis[%d]: Phase4.5 item extraction warn: %v", novel.ID, err)
				} else {
					logger.Printf("NovelAnalysis[%d]: Phase4.5 item extraction done: %d items", novel.ID, len(items))
				}
			}
		}
		// 补跑场景锚点提取（用章节 summary 代替 content）
		if s.sceneAnchorService != nil {
			existingAnchors, _ := s.sceneAnchorService.ListByNovel(novel.ID)
			if len(existingAnchors) == 0 {
				logger.Printf("NovelAnalysis[%d]: Phase4.5 re-running scene anchor extraction (using chapter summaries)", novel.ID)
				task.setStep("正在补充提取场景...")
				// 重新加载章节（Phase4 创建的带 summary 的 draft 章节）
				if freshChapters, err := s.chapterRepo.ListByNovelWithContent(novel.ID); err == nil {
					const maxConcurrent = 3
					sem := make(chan struct{}, maxConcurrent)
					var wg sync.WaitGroup
					count := 0
					for _, ch := range freshChapters {
						text := ch.Content
						if text == "" {
							text = ch.Summary
						}
						if text == "" || count >= 10 {
							continue
						}
						count++
						ch := ch
						capturedText := text
						sem <- struct{}{}
						wg.Add(1)
						go func() {
							defer func() { <-sem; wg.Done() }()
							anchors, err := s.sceneAnchorService.ExtractFromChapter(ctx, tenantID, novel.ID, novel.Title, capturedText)
							if err != nil {
								logger.Errorf("NovelAnalysis[%d]: Phase4.5 scene anchor ch%d warn: %v", novel.ID, ch.ChapterNo, err)
							} else {
								logger.Printf("NovelAnalysis[%d]: Phase4.5 scene anchor ch%d: %d anchors", novel.ID, ch.ChapterNo, len(anchors))
							}
						}()
					}
					wg.Wait()
				}
			}
		}
		// 补跑角色信息丰富（Phase2 时无章节内容，Description 可能为空）
		{
			existingChars, _ := s.characterRepo.ListByNovel(novel.ID)
			var emptyChars []*model.Character
			for _, c := range existingChars {
				if c.Description == "" {
					emptyChars = append(emptyChars, c)
				}
			}
			if len(emptyChars) > 0 {
				logger.Printf("NovelAnalysis[%d]: Phase4.5 enriching %d characters with empty fields", novel.ID, len(emptyChars))
				task.setStep("正在补充角色信息...")
				novelForChar := freshNovel
				if novelForChar == nil {
					novelForChar, _ = s.novelRepo.GetByID(novel.ID)
				}
				if novelForChar != nil {
					freshChaptersForChar, _ := s.chapterRepo.ListByNovelWithContent(novel.ID)
					summariesText := buildChapterSummariesText(freshChaptersForChar, 15, 8000)
					if summariesText != "" {
						extractPrompt, pErr := renderPrompt("extract_characters", map[string]interface{}{
							"NovelTitle":     novelForChar.Title,
							"Genre":          novelForChar.Genre,
							"Summaries":      summariesText,
							"PromptLanguage": novelForChar.PromptLanguage,
						})
						if pErr == nil {
							result, aErr := s.aiService.GenerateWithProvider(tenantID, novel.ID, "extract_characters", extractPrompt, "",
								StoryboardOverrides{})
							if aErr == nil {
								chars, pErr2 := parseCharacterJSONResult(result)
								if pErr2 == nil {
									// 建立 name→emptyChar 映射，只更新空字段
									emptyByName := make(map[string]*model.Character, len(emptyChars))
									for _, c := range emptyChars {
										emptyByName[strings.ToLower(c.Name)] = c
									}
									for _, c := range chars {
										ec, ok := emptyByName[strings.ToLower(c.Name)]
										if !ok {
											continue
										}
										descEnriched := false
										if ec.Description == "" {
											// 优先新格式的统一 description，兼容旧格式分离字段
											desc := c.Description
											if desc == "" {
												var parts []string
												if c.Appearance != "" { parts = append(parts, "外貌："+c.Appearance) }
												if c.Personality != "" { parts = append(parts, "性格："+c.Personality) }
												if c.Background != "" { parts = append(parts, "背景："+c.Background) }
												if c.CharacterArc != "" { parts = append(parts, "弧光："+c.CharacterArc) }
												if c.Archetype != "" { parts = append(parts, "原型："+c.Archetype) }
												if c.DialogueStyle.SpeechHabits != "" {
													parts = append(parts, "口头禅："+c.DialogueStyle.SpeechHabits)
												} else if len(c.DialogueStyle.Patterns) > 0 {
													parts = append(parts, "说话风格："+strings.Join(c.DialogueStyle.Patterns, "；"))
												}
												desc = strings.Join(parts, "\n")
											}
											if desc != "" {
												ec.Description = desc
												descEnriched = true
											}
										}
										if descEnriched {
											if err := s.characterRepo.Update(ec); err != nil {
												logger.Errorf("NovelAnalysis[%d]: Phase4.5 update char %q: %v", novel.ID, ec.Name, err)
											} else {
												logger.Printf("NovelAnalysis[%d]: Phase4.5 enriched char %q", novel.ID, ec.Name)
											}
										}
										// 确保有默认形象，并同步 VisualPrompt
										if s.lookRepo != nil {
											if ec.DefaultLookID != 0 {
												if c.VisualPrompt != "" {
													if look, e := s.lookRepo.GetByID(ec.DefaultLookID); e == nil && look.VisualPrompt == "" {
														look.VisualPrompt = c.VisualPrompt
														s.lookRepo.Update(look) //nolint:errcheck
													}
												}
											} else {
												newLook := &model.CharacterLook{
													CharacterID:  ec.ID,
													NovelID:      ec.NovelID,
													Label:        "默认形象",
													ChapterFrom:  1,
													VisualPrompt: c.VisualPrompt,
												}
												if s.lookRepo.Create(newLook) == nil { //nolint:errcheck
													s.characterRepo.UpdateDefaultLookID(ec.ID, newLook.ID) //nolint:errcheck
												}
											}
										}
									}
								}
							} else {
								logger.Errorf("NovelAnalysis[%d]: Phase4.5 character enrichment AI error: %v", novel.ID, aErr)
							}
						}
					}
				}
			}
		}
	}
	task.setProgress(95)

	// ── Phase 5: 收尾 (95→100) ────────────────────────────────────────────────
	task.setStep("收尾中...")
	if err := s.stepFinalize(task, novel); err != nil {
		logger.Errorf("NovelAnalysis[%d]: stepFinalize warn: %v", novel.ID, err)
	}

	task.setProgress(100)
	task.setStep("分析完成")
	task.setStatus("completed")
	if task.taskSvc != nil && task.externalTaskID != "" {
		task.mu.RLock()
		ws := make([]string, len(task.Warnings))
		copy(ws, task.Warnings)
		task.mu.RUnlock()
		task.taskSvc.Complete(task.externalTaskID, map[string]interface{}{ //nolint:errcheck
			"novel_id": novel.ID,
			"warnings": ws,
		})
	}
	logger.Printf("NovelAnalysis[%d]: pipeline completed", novel.ID)
	logger.Printf("[NovelAnalysis] runPipeline done: novelID=%d", novel.ID)
}

// ──────────────────────────────────────────────
// 共享 JSON 结构体（pipeline 各步骤复用）
// ──────────────────────────────────────────────

type analysisAbilityJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type analysisDialogueStyleJSON struct {
	VocabularyLevel string   `json:"vocabulary_level"`
	Patterns        []string `json:"patterns"`
	SpeechHabits    string   `json:"speech_habits"`
}

type analysisCharJSON struct {
	Name            string                    `json:"name"`
	Role            string                    `json:"role"`
	Gender          string                    `json:"gender"`          // male/female/neutral
	Age             string                    `json:"age"`             // 如 "16" / "约25岁"
	Description     string                    `json:"description"`     // 统一中文描述（新格式）
	Archetype       string                    `json:"archetype"`       // 旧格式兼容
	Appearance      string                    `json:"appearance"`      // 旧格式兼容
	Personality     string                    `json:"personality"`     // 旧格式兼容
	PersonalityTags []string                  `json:"personality_tags"` // AI 生成的性格标签
	Background      string                    `json:"background"`      // 旧格式兼容
	CharacterArc    string                    `json:"character_arc"`   // 旧格式兼容
	DialogueStyle   analysisDialogueStyleJSON `json:"dialogue_style"`  // 旧格式兼容
	VisualPrompt    string                    `json:"visual_prompt"`
}

type extractMinorCharsResponse struct {
	NewCharacters       []analysisCharJSON `json:"new_characters"`
	AppearingCharacters []string           `json:"appearing_characters"`
}

type analysisItemJSON struct {
	Name         string `json:"name"`
	Category     string `json:"category"`
	Appearance   string `json:"appearance"`
	Description  string `json:"description"`
	Location     string `json:"location"`
	Owner        string `json:"owner"`
	VisualPrompt string `json:"visual_prompt"`
}

// buildChapterSummariesText 从章节列表构建摘要文本，最多取 maxChapters 章，截断至 maxLen。
// 回退顺序：Summary → Content 前500字 → Outline（用户大纲）→ 跳过该章。
func buildChapterSummariesText(chapters []*model.Chapter, maxChapters, maxLen int) string {
	n := len(chapters)
	if n > maxChapters {
		n = maxChapters
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		ch := chapters[i]
		summary := ch.Summary
		if summary == "" {
			summary = truncateForPrompt(ch.Content, 500)
		}
		if summary == "" {
			summary = truncateForPrompt(ch.Outline, 200) // 章节大纲兜底（无正文时也能提取信息）
		}
		if summary != "" {
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, summary))
		}
	}
	return truncateForPrompt(sb.String(), maxLen)
}

// stepSummarizeChapters 为每章生成摘要（复用 chapter_summary.tmpl）。
// Pipeline 关键路径只处理前 maxForPipeline 章（并发），其余章节后台异步补全。
func (s *NovelAnalysisService) stepSummarizeChapters(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepSummarizeChapters: novelID=%d chapters=%d", novel.ID, len(chapters))

	// 关键路径：只并发处理前 N 章（Phase 2 各提取步骤也上限 10 章，保持一致）
	const maxForPipeline = 10
	const maxConcurrent = 3

	pipeline := chapters
	if len(pipeline) > maxForPipeline {
		pipeline = chapters[:maxForPipeline]
	}

	total := len(pipeline)
	if total == 0 {
		return nil
	}

	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for _, ch := range pipeline {
		ch := ch
		if ch.Summary != "" || ch.Content == "" {
			mu.Lock()
			done++
			task.setProgress(done * 20 / total)
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			prompt, pErr := renderPrompt("chapter_summary", map[string]interface{}{
				"NovelTitle":   novel.Title,
				"ChapterNo":    ch.ChapterNo,
				"ChapterTitle": ch.Title,
				"Content":      truncateForPrompt(ch.Content, 6000),
			})
			if pErr != nil {
				logger.Errorf("NovelAnalysis: chapter %d render prompt: %v", ch.ChapterNo, pErr)
			} else if summary, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "chapter_summary", prompt, ""); err != nil {
				logger.Errorf("NovelAnalysis: chapter %d summary AI error: %v", ch.ChapterNo, err)
			} else {
				ch.Summary = strings.TrimSpace(summary)
				if err := s.chapterRepo.Update(ch); err != nil {
					logger.Errorf("NovelAnalysis: chapter %d save summary error: %v", ch.ChapterNo, err)
				}
			}
			mu.Lock()
			done++
			task.setProgress(done * 20 / total)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 剩余章节后台异步补全摘要，不阻塞 pipeline
	if len(chapters) > maxForPipeline {
		go s.summarizeChaptersBackground(ctx, tenantID, novel, chapters[maxForPipeline:])
	}
	return nil
}

// summarizeChaptersBackground 后台低优先级补全剩余章节摘要（不影响 pipeline 进度）
func (s *NovelAnalysisService) summarizeChaptersBackground(
	ctx context.Context, tenantID uint, novel *model.Novel,
	chapters []*model.Chapter,
) {
	logger.Printf("[NovelAnalysis] summarizeChaptersBackground: novelID=%d chapters=%d", novel.ID, len(chapters))
	const maxConcurrent = 2
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, ch := range chapters {
		if ch.Summary != "" || ch.Content == "" {
			continue
		}
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
				if r := recover(); r != nil {
					logger.Errorf("NovelAnalysis[bg][%d]: chapter %d panic: %v", novel.ID, ch.ChapterNo, r)
				}
			}()
			prompt, err := renderPrompt("chapter_summary", map[string]interface{}{
				"NovelTitle":   novel.Title,
				"ChapterNo":    ch.ChapterNo,
				"ChapterTitle": ch.Title,
				"Content":      truncateForPrompt(ch.Content, 6000),
			})
			if err != nil {
				return
			}
			summary, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "chapter_summary", prompt, "")
			if err != nil {
				logger.Errorf("NovelAnalysis[bg][%d]: chapter %d summary error: %v", novel.ID, ch.ChapterNo, err)
				return
			}
			ch.Summary = strings.TrimSpace(summary)
			if err := s.chapterRepo.Update(ch); err != nil {
				logger.Errorf("NovelAnalysis[bg][%d]: chapter %d save error: %v", novel.ID, ch.ChapterNo, err)
			}
		}()
	}
	wg.Wait()
	logger.Printf("NovelAnalysis[bg][%d]: background summarization complete (%d chapters)", novel.ID, len(chapters))
}

// stepExtractCharacters 从章节摘要提取角色；无章节时基于小说描述生成角色
func (s *NovelAnalysisService) stepExtractCharacters(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepExtractCharacters: novelID=%d", novel.ID)
	var summariesText string
	if len(chapters) > 0 {
		summariesText = buildChapterSummariesText(chapters, 15, 8000)
	} else {
		summariesText = novel.Description
	}
	if summariesText == "" {
		summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计主要角色。", novel.Genre, novel.Title)
	}

	extractCharsPrompt, err := renderPrompt("extract_characters", map[string]interface{}{
		"NovelTitle":     novel.Title,
		"Genre":          novel.Genre,
		"Summaries":      summariesText,
		"PromptLanguage": novel.PromptLanguage,
	})
	if err != nil {
		return fmt.Errorf("render extract_characters: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "extract_characters", extractCharsPrompt, "",
		StoryboardOverrides{})
	if err != nil {
		return fmt.Errorf("AI extract_characters: %w", err)
	}

	chars, err := parseCharacterJSONResult(result)
	if err != nil {
		return fmt.Errorf("parse characters JSON: %w", err)
	}

	// 获取已有角色，用于去重
	existingChars, _ := s.characterRepo.ListByNovel(novel.ID)
	existingNames := make(map[string]bool, len(existingChars))
	for _, ec := range existingChars {
		existingNames[strings.ToLower(ec.Name)] = true
	}

	// 加载可用音色模型（用于自动推荐，可选）
	var voiceModels []*model.AIModel
	if s.modelRepo != nil {
		voiceModels, _ = s.modelRepo.GetAvailableByTaskType("voice_gen", tenantID)
	}

	const maxMainCharacters = 20
	var createdChars []*model.Character
	for _, c := range chars {
		if len(createdChars)+len(existingChars) >= maxMainCharacters {
			break
		}
		if c.Name == "" {
			continue
		}
		if existingNames[strings.ToLower(c.Name)] {
			continue // 去重
		}
		// 项目级只保留主要角色
		if c.Role == "minor" {
			continue
		}
		role := c.Role
		if role != "protagonist" && role != "antagonist" && role != "supporting" {
			role = "supporting"
		}
		// 优先使用新格式的统一 description；兼容旧格式的分离字段
		finalDesc := c.Description
		if finalDesc == "" {
			var descParts []string
			if c.Appearance != "" { descParts = append(descParts, "外貌："+c.Appearance) }
			if c.Personality != "" { descParts = append(descParts, "性格："+c.Personality) }
			if c.Background != "" { descParts = append(descParts, "背景："+c.Background) }
			if c.CharacterArc != "" { descParts = append(descParts, "弧光："+c.CharacterArc) }
			if c.Archetype != "" { descParts = append(descParts, "原型："+c.Archetype) }
			if c.DialogueStyle.SpeechHabits != "" {
				descParts = append(descParts, "口头禅/语癖："+c.DialogueStyle.SpeechHabits)
			} else if len(c.DialogueStyle.Patterns) > 0 {
				descParts = append(descParts, "说话风格："+strings.Join(c.DialogueStyle.Patterns, "；"))
			} else if c.DialogueStyle.VocabularyLevel != "" {
				descParts = append(descParts, "说话风格："+c.DialogueStyle.VocabularyLevel)
			}
			finalDesc = strings.Join(descParts, "\n")
		}
		// 自动推荐配音设置
		suggestedVoice := suggestVoiceForCharacter(finalDesc, c.Gender, c.PersonalityTags, role, voiceModels)
		suggestedStyle := suggestVoiceStyle(c.Gender, c.Age, role, c.PersonalityTags, finalDesc)
		suggestedLang := suggestVoiceLanguage(novel.PromptLanguage)
		char := &model.Character{
			NovelID:       novel.ID,
			TenantID:      tenantID,
			UUID:          uuid.New().String(),
			Name:          c.Name,
			Role:          role,
			Gender:        c.Gender,
			Age:           c.Age,
			Description:   finalDesc,
			VoiceID:       suggestedVoice,
			VoiceStyle:    suggestedStyle,
			VoiceLanguage: suggestedLang,
			Status:        "active",
		}
		if err := s.characterRepo.Create(char); err != nil {
			logger.Errorf("NovelAnalysis: create character %q: %v", c.Name, err)
			continue
		}
		if s.lookRepo != nil {
			defaultLook := &model.CharacterLook{
				CharacterID:  char.ID,
				NovelID:      char.NovelID,
				Label:        "默认形象",
				ChapterFrom:  1,
				VisualPrompt: c.VisualPrompt,
			}
			if err := s.lookRepo.Create(defaultLook); err != nil {
				logger.Errorf("NovelAnalysis: create default look for %q: %v", char.Name, err)
			} else {
				_ = s.characterRepo.UpdateDefaultLookID(char.ID, defaultLook.ID)
			}
		}
		existingNames[strings.ToLower(c.Name)] = true
		createdChars = append(createdChars, char)
	}

	// 角色图片由用户手动触发，分析阶段不自动生成
	logger.Printf("[NovelAnalysis] stepExtractCharacters done: novelID=%d characters created", novel.ID)
	return nil
}

// stepExtractWorldview 从章节摘要提取世界观；无章节时基于小说描述生成世界观
func (s *NovelAnalysisService) stepExtractWorldview(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepExtractWorldview: novelID=%d", novel.ID)
	var summariesText string
	if len(chapters) > 0 {
		var sb strings.Builder
		for _, ch := range chapters {
			if ch.Summary != "" {
				sb.WriteString(fmt.Sprintf("第%d章：%s\n", ch.ChapterNo, ch.Summary))
			}
		}
		summariesText = truncateForPrompt(sb.String(), 2000)
	}
	if summariesText == "" {
		if novel.Description != "" {
			summariesText = novel.Description
		} else {
			summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计世界观体系。", novel.Genre, novel.Title)
		}
	}

	worldviewPrompt, err := renderPrompt("extract_worldview", map[string]interface{}{
		"NovelTitle": novel.Title,
		"Genre":      novel.Genre,
		"Summaries":  summariesText,
	})
	if err != nil {
		return fmt.Errorf("render extract_worldview: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "extract_worldview", worldviewPrompt, "")
	if err != nil {
		return fmt.Errorf("AI extract_worldview: %w", err)
	}
	type wvRaw struct {
		Name        json.RawMessage `json:"name"`
		Description json.RawMessage `json:"description"`
		MagicSystem json.RawMessage `json:"magic_system"`
		Geography   json.RawMessage `json:"geography"`
		History     json.RawMessage `json:"history"`
		Culture     json.RawMessage `json:"culture"`
		Technology  json.RawMessage `json:"technology"`
		Rules       json.RawMessage `json:"rules"`
		CheatSystem json.RawMessage `json:"cheat_system"`
	}
	var raw wvRaw
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &raw); err != nil {
		return fmt.Errorf("parse worldview JSON: %w", err)
	}
	parseField := func(data json.RawMessage) string {
		if len(data) == 0 {
			return ""
		}
		var s string
		if err := json.Unmarshal(data, &s); err == nil {
			return s
		}
		var arr []string
		if err := json.Unmarshal(data, &arr); err == nil {
			return strings.Join(arr, "；")
		}
		return strings.Trim(string(data), `"`)
	}
	type wvParsed struct {
		Name, Description, MagicSystem, Geography, History, Culture, Technology, Rules, CheatSystem string
	}
	wv := wvParsed{
		Name:        parseField(raw.Name),
		Description: parseField(raw.Description),
		MagicSystem: parseField(raw.MagicSystem),
		Geography:   parseField(raw.Geography),
		History:     parseField(raw.History),
		Culture:     parseField(raw.Culture),
		Technology:  parseField(raw.Technology),
		Rules:       parseField(raw.Rules),
		CheatSystem: parseField(raw.CheatSystem),
	}

	if wv.Name == "" {
		wv.Name = novel.Title + " 世界观"
	}

	// 若小说已关联世界观，直接更新（复用），不重复创建
	if novel.WorldviewID != nil {
		existing, err := s.worldviewRepo.GetByID(*novel.WorldviewID)
		if err == nil {
			existing.Name = wv.Name
			existing.Description = wv.Description
			existing.Genre = novel.Genre
			existing.MagicSystem = wv.MagicSystem
			existing.Geography = wv.Geography
			existing.History = wv.History
			existing.Culture = wv.Culture
			existing.Technology = wv.Technology
			existing.Rules = wv.Rules
			existing.CheatSystem = wv.CheatSystem
			logger.Printf("NovelAnalysis[%d]: updating existing worldview id=%d %q", novel.ID, existing.ID, existing.Name)
			if err := s.worldviewRepo.Update(existing); err != nil {
				logger.Errorf("NovelAnalysis: update worldview %d: %v", existing.ID, err)
			}
			return nil
		}
		// existing worldview missing in DB — fall through to create
	}

	worldview := &model.Worldview{
		UUID:        uuid.New().String(),
		Name:        wv.Name,
		Description: wv.Description,
		Genre:       novel.Genre,
		MagicSystem: wv.MagicSystem,
		Geography:   wv.Geography,
		History:     wv.History,
		Culture:     wv.Culture,
		Technology:  wv.Technology,
		Rules:       wv.Rules,
		CheatSystem: wv.CheatSystem,
	}
	logger.Printf("NovelAnalysis[%d]: creating worldview %q", novel.ID, worldview.Name)
	if err := s.worldviewRepo.Create(worldview); err != nil {
		return fmt.Errorf("save worldview: %w", err)
	}

	// 关联到小说
	if err := s.novelRepo.UpdateFields(novel.ID, map[string]interface{}{
		"worldview_id": worldview.ID,
	}); err != nil {
		logger.Errorf("NovelAnalysis: link worldview to novel %d: %v", novel.ID, err)
	} else {
		novel.WorldviewID = &worldview.ID
	}
	return nil
}

// stepGenerateOutline 生成大纲并写入小说描述，返回大纲结果供后续步骤使用
func (s *NovelAnalysisService) stepGenerateOutline(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel,
) (*OutlineResult, error) {
	chapterCount, _ := s.chapterRepo.CountByNovel(novel.ID)

	// AI 创建模式：无章节时使用 TargetChapters；若为 0 则由 AI 自行决定章节数
	chapterNum := int(chapterCount)
	if chapterNum == 0 {
		chapterNum = novel.TargetChapters // 0 表示让 AI 自决
	}

	req := &GenerateOutlineRequest{
		NovelID:    novel.ID,
		ChapterNum: chapterNum,
	}
	outline, err := s.novelService.GenerateOutline(tenantID, req)
	if err != nil {
		return nil, fmt.Errorf("GenerateOutline: %w", err)
	}

	// 保存完整大纲 JSON 到 novel.Outline，供章节生成时使用
	updateFields := map[string]interface{}{}
	if outlineJSON, err := json.Marshal(outline); err == nil {
		updateFields["outline"] = string(outlineJSON)
	}
	if novel.Description == "" && outline.Summary != "" {
		updateFields["description"] = outline.Summary
	}
	// 若用户未设置目标章节数，用 AI 生成的实际章节数回填
	if novel.TargetChapters == 0 && len(outline.Chapters) > 0 {
		updateFields["target_chapters"] = len(outline.Chapters)
	}
	if len(updateFields) > 0 {
		if err := s.novelRepo.UpdateFields(novel.ID, updateFields); err != nil {
			logger.Errorf("NovelAnalysis: update novel outline/description: %v", err)
		}
	}
	return outline, nil
}

// stepCreateChapterOutlines 根据大纲为每个章节创建占位记录
func (s *NovelAnalysisService) stepCreateChapterOutlines(
	ctx context.Context, task *AnalysisTask, novel *model.Novel, outline *OutlineResult,
) error {
	logger.Printf("NovelAnalysis[%d]: stepCreateChapterOutlines start: novelID=%d totalChapters=%d tenantID=%d",
		novel.ID, novel.ID, len(outline.Chapters), novel.TenantID)
	created := 0
	skipped := 0
	for _, co := range outline.Chapters {
		if co.ChapterNo <= 0 {
			continue
		}
		// 检查是否已存在同 NovelID+ChapterNo 的记录
		existing, err := s.chapterRepo.GetByNovelAndChapterNo(novel.ID, co.ChapterNo)
		if err == nil && existing != nil {
			skipped++
			continue // 已存在，跳过
		}
		ch := &model.Chapter{
			UUID:      uuid.New().String(),
			NovelID:   novel.ID,
			TenantID:  novel.TenantID,
			ChapterNo: co.ChapterNo,
			Title:     co.Title,
			Summary:   co.Summary,
			Status:    "draft",
			Content:   "",
		}
		if err := s.chapterRepo.Create(ch); err != nil {
			logger.Errorf("NovelAnalysis: create chapter placeholder %d: %v", co.ChapterNo, err)
		} else {
			created++
		}
	}
	logger.Printf("NovelAnalysis[%d]: stepCreateChapterOutlines done: created=%d skipped=%d", novel.ID, created, skipped)
	if created > 0 {
		_ = s.novelRepo.SyncStats(novel.ID)
	}
	return nil
}

// stepFinalize 收尾：更新小说状态
func (s *NovelAnalysisService) stepFinalize(task *AnalysisTask, novel *model.Novel) error {
	fields := map[string]interface{}{
		"status": "writing",
	}
	if err := s.novelRepo.UpdateFields(novel.ID, fields); err != nil {
		return fmt.Errorf("update novel status: %w", err)
	}
	return nil
}

func (s *NovelAnalysisService) fail(task *AnalysisTask, msg string) {
	task.setStatus("failed")
	task.setError(msg)
	task.setStep("失败")
	if task.taskSvc != nil && task.externalTaskID != "" {
		task.taskSvc.Fail(task.externalTaskID, msg) //nolint:errcheck
	}
	logger.Errorf("[NovelAnalysis] fail: %s", msg)
}

// generateThreeViewsAsync 为角色异步生成三视图（正/侧/背面），失败仅记录日志不影响流程
func (s *NovelAnalysisService) generateThreeViewsAsync(ctx context.Context, tenantID uint, imageStyle string, chars []*model.Character) {
	logger.Printf("[NovelAnalysis] generateThreeViewsAsync: characters=%d", len(chars))
	// 优先为主角和反派生成，配角次之
	sorted := make([]*model.Character, 0, len(chars))
	for _, c := range chars {
		if c.Role == "protagonist" || c.Role == "antagonist" {
			sorted = append(sorted, c)
		}
	}
	for _, c := range chars {
		if c.Role == "supporting" {
			sorted = append(sorted, c)
		}
	}

	for _, char := range sorted {
		// 优先从默认形象获取 VisualPrompt，降级使用 Description
		visualPrompt := ""
		if s.lookRepo != nil && char.DefaultLookID != 0 {
			if look, err := s.lookRepo.GetByID(char.DefaultLookID); err == nil {
				visualPrompt = look.VisualPrompt
			}
		}
		basePrompt := ""
		if visualPrompt != "" {
			basePrompt = fmt.Sprintf("character named %s, %s, high quality illustration", char.Name, visualPrompt)
		} else if char.Description != "" {
			basePrompt = fmt.Sprintf("character named %s, %s, high quality illustration", char.Name, char.Description)
		} else {
			basePrompt = fmt.Sprintf("character named %s, full body, high quality illustration", char.Name)
		}

		// 生成三视图合图（combined turnaround sheet），结果存入默认形象
		sheetPrompt := basePrompt + ", character turnaround sheet, front and side and back views side by side, three-view character design sheet, same character multiple angles"
		url, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, "", sheetPrompt, "", imageStyle, "", "")
		if err != nil {
			logger.Errorf("NovelAnalysis: three-view sheet for char %d: %v", char.ID, err)
			continue
		}
		if url == "" {
			continue
		}
		if s.lookRepo != nil {
			if char.DefaultLookID != 0 {
				if look, e := s.lookRepo.GetByID(char.DefaultLookID); e == nil {
					look.ThreeViewSheet = url
					if err := s.lookRepo.Update(look); err != nil {
						logger.Errorf("NovelAnalysis: save three-view to look for char %d: %v", char.ID, err)
					}
				}
			} else {
				newLook := &model.CharacterLook{
					CharacterID:    char.ID,
					NovelID:        char.NovelID,
					Label:          "默认形象",
					ChapterFrom:    1,
					ThreeViewSheet: url,
				}
				if s.lookRepo.Create(newLook) == nil {
					_ = s.characterRepo.UpdateDefaultLookID(char.ID, newLook.ID)
				}
			}
		}
	}
}

// stepExtractItems 逐章并发提取物品信息
func (s *NovelAnalysisService) stepExtractItems(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepExtractItems: novelID=%d", novel.ID)
	// 若已有物品则跳过
	existing, _ := s.itemRepo.ListByNovel(novel.ID)
	if len(existing) > 0 {
		logger.Printf("NovelAnalysis[%d]: items already exist (%d), skip", novel.ID, len(existing))
		return nil
	}

	items, err := s.itemService.AIExtractAllFromNovel(ctx, tenantID, novel.ID)
	if err != nil {
		return fmt.Errorf("AIExtractAllFromNovel items: %w", err)
	}
	logger.Printf("NovelAnalysis[%d]: extracted %d items", novel.ID, len(items))
	return nil
}

// stepExtractPlotPoints 从章节中提取剧情点（伏笔/冲突/转折等）
func (s *NovelAnalysisService) stepExtractPlotPoints(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepExtractPlotPoints: novelID=%d", novel.ID)
	if s.plotPointService == nil {
		return nil
	}
	// 若已有剧情点则跳过
	existing, _ := s.plotPointService.ListByNovel(novel.ID, "", false)
	if len(existing) > 0 {
		logger.Printf("NovelAnalysis[%d]: plot points already exist (%d), skip", novel.ID, len(existing))
		return nil
	}
	pps, err := s.plotPointService.AIExtractFromNovel(ctx, tenantID, novel.ID)
	if err != nil {
		return fmt.Errorf("AIExtractPlotPoints: %w", err)
	}
	logger.Printf("NovelAnalysis[%d]: extracted %d plot points", novel.ID, len(pps))
	return nil
}

// stepExtractSceneAnchors 从前几章提取场景锚点（并发提取，最多 10 章）
func (s *NovelAnalysisService) stepExtractSceneAnchors(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepExtractSceneAnchors: novelID=%d chapters=%d", novel.ID, len(chapters))
	if s.sceneAnchorService == nil {
		return nil
	}
	// 若已有锚点则跳过
	existing, _ := s.sceneAnchorService.ListByNovel(novel.ID)
	if len(existing) > 0 {
		logger.Printf("NovelAnalysis[%d]: scene anchors already exist (%d), skip", novel.ID, len(existing))
		return nil
	}
	// 从前 10 章提取，覆盖更多场景
	maxCh := 10
	if len(chapters) < maxCh {
		maxCh = len(chapters)
	}

	// 过滤出有内容的章节
	candidates := make([]*model.Chapter, 0, maxCh)
	for i := 0; i < maxCh; i++ {
		if chapters[i].Content != "" {
			candidates = append(candidates, chapters[i])
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// 并发提取，最多 3 个并发
	const maxConcurrent = 3
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, ch := range candidates {
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			anchors, err := s.sceneAnchorService.ExtractFromChapter(ctx, tenantID, novel.ID, novel.Title, ch.Content)
			if err != nil {
				logger.Errorf("NovelAnalysis[%d]: ExtractSceneAnchors ch%d warn: %v", novel.ID, ch.ChapterNo, err)
				return
			}
			logger.Printf("NovelAnalysis[%d]: extracted %d scene anchors from ch%d", novel.ID, len(anchors), ch.ChapterNo)
		}()
	}
	wg.Wait()
	return nil
}

// stepExtractForeshadows 从章节摘要中提取伏笔线索
func (s *NovelAnalysisService) stepExtractForeshadows(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	logger.Printf("[NovelAnalysis] stepExtractForeshadows: novelID=%d", novel.ID)
	if s.foreshadowSvc == nil {
		return nil
	}
	// 若已有伏笔则跳过
	existing, _ := s.foreshadowSvc.ListByNovel(ctx, novel.ID, tenantID)
	if len(existing) > 0 {
		logger.Printf("NovelAnalysis[%d]: foreshadows already exist (%d), skip", novel.ID, len(existing))
		return nil
	}
	created, err := s.foreshadowSvc.AIExtractFromNovel(ctx, tenantID, novel.ID)
	if err != nil {
		if strings.Contains(err.Error(), "no chapter content available") {
			logger.Printf("NovelAnalysis[%d]: no chapter content for foreshadow extraction, skipping", novel.ID)
			return nil
		}
		return fmt.Errorf("AIExtractForeshadows: %w", err)
	}
	logger.Printf("NovelAnalysis[%d]: extracted %d foreshadows", novel.ID, len(created))
	return nil
}

// ──────────────────────────────────────────────
// Analysis status query
// ──────────────────────────────────────────────

// AnalysisStatus 小说分析状态（供 API 查询）
type AnalysisStatus struct {
	NovelID     uint      `json:"novel_id"`
	Status      string    `json:"status"` // "not_started", "pending", "running", "completed", "failed", "cancelled"
	Progress    int       `json:"progress"` // 0-100
	CurrentStep string    `json:"current_step"`
	Error       string    `json:"error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GetAnalysisStatus 查询小说的最新分析任务状态。
// 若从未触发过分析，返回 status="not_started"。
func (s *NovelAnalysisService) GetAnalysisStatus(novelID uint) (*AnalysisStatus, error) {
	if s.taskSvc == nil {
		return &AnalysisStatus{NovelID: novelID, Status: "not_started"}, nil
	}
	task, err := s.taskSvc.GetLatestAnalysisTask(novelID)
	if err != nil {
		// No task found → not started yet
		return &AnalysisStatus{NovelID: novelID, Status: "not_started"}, nil
	}
	step := ""
	if task.ResultJSON != "" {
		var meta struct {
			Step string `json:"step"`
		}
		if jsonErr := json.Unmarshal([]byte(task.ResultJSON), &meta); jsonErr == nil && meta.Step != "" {
			step = meta.Step
		}
	}
	return &AnalysisStatus{
		NovelID:     novelID,
		Status:      task.Status,
		Progress:    task.Progress,
		CurrentStep: step,
		Error:       task.Error,
		UpdatedAt:   task.UpdatedAt,
	}, nil
}

// retryStep 对分析步骤执行最多 maxRetries 次重试，每次重试前等待递增时长。
// 若所有重试均失败，返回最后一次错误。
func retryStep(maxRetries int, fn func() error) error {
	return retryStepCtx(context.Background(), maxRetries, fn)
}

// retryStepCtx 与 retryStep 相同，但会在重试间隔检查 ctx，context 取消时立即停止重试。
// 注意：若 fn() 内部未感知 ctx，正在执行中的 fn() 不会被中断，ctx 取消仅阻止启动下一次重试。
func retryStepCtx(ctx context.Context, maxRetries int, fn func() error) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			logger.Warnf("[retryStepCtx] context cancelled before attempt %d: %v", i+1, ctx.Err())
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		default:
		}
		if lastErr = fn(); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			logger.Warnf("[retryStepCtx] context cancelled after attempt %d (err=%v): %v", i+1, lastErr, ctx.Err())
			return lastErr
		case <-time.After(time.Duration(i+1) * time.Second):
		}
	}
	return lastErr
}

// stepUpdateNovelSettings 使用 AI 自动填写小说设置（类型、写作风格、图片风格、简介、目标字数/章节数）
// 每个子字段独立判断是否需要填充，互不阻断。
func (s *NovelAnalysisService) stepUpdateNovelSettings(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	needStylePrompt := novel.StylePrompt == ""
	needDescription := novel.Description == ""
	needGenre := novel.Genre == "" || novel.Genre == "unknown"
	needImageStyle := novel.ImageStyle == ""
	needTargets := novel.TargetChapters == 0 || novel.TargetWordCount == 0

	if !needStylePrompt && !needDescription && !needGenre && !needImageStyle && !needTargets {
		return nil // 所有字段已填，无需更新
	}

	sampleContent := ""
	for _, ch := range chapters {
		if ch.Content != "" {
			sampleContent = truncateForPrompt(ch.Content, 2000)
			break
		}
	}
	// 降级：章节摘要（大纲分析时 draft 章节只有 summary）
	if sampleContent == "" {
		for _, ch := range chapters {
			if ch.Summary != "" {
				sampleContent = truncateForPrompt(ch.Summary, 2000)
				break
			}
		}
	}
	if sampleContent == "" {
		sampleContent = novel.Description
	}
	if sampleContent == "" {
		// 最后兜底：只用小说标题让 AI 推断基本设置
		sampleContent = fmt.Sprintf("小说标题：《%s》，类型待确认。", novel.Title)
	}

	updates := map[string]interface{}{}

	validGenres := []string{
		"现代言情", "古代言情", "幻想言情", "历史", "军事", "科幻",
		"游戏", "游戏竞技", "玄幻奇幻", "都市", "奇闻异事", "武侠仙侠",
		"体育", "N次元", "文学艺术",
	}

	// ── 1. 合并 AI 调用：一次返回 genre/style_prompt/description ─────────────
	// 只请求实际缺失的字段，最多 1 次 AI 往返（原来最多 3 次）
	if needGenre || needStylePrompt || needDescription {
		type settingsJSON struct {
			Genre       string `json:"genre"`
			StylePrompt string `json:"style_prompt"`
			Description string `json:"description"`
		}
		var fieldDescs []string
		if needGenre {
			fieldDescs = append(fieldDescs, fmt.Sprintf(
				`  "genre": "从以下类型选一个最符合的：%s"`, strings.Join(validGenres, "、")))
		}
		if needStylePrompt {
			fieldDescs = append(fieldDescs, `  "style_prompt": "100字以内：叙事视角、语言风格、情感基调、节奏特点（AI续写风格参考）"`)
		}
		if needDescription {
			fieldDescs = append(fieldDescs, `  "description": "150字以内作品简介：故事背景、主人公、核心冲突，不剧透结局，语言生动"`)
		}
		combinedPrompt := fmt.Sprintf("小说《%s》部分章节内容：\n%s\n\n请用JSON格式返回以下字段：\n{\n%s\n}\n只输出JSON对象，不要任何其他内容。",
			novel.Title, sampleContent, strings.Join(fieldDescs, ",\n"))
		if resp, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "novel_settings", combinedPrompt, ""); err == nil {
			var res settingsJSON
			if cleaned := extractJSON(strings.TrimSpace(resp)); json.Unmarshal([]byte(cleaned), &res) == nil {
				if needGenre && res.Genre != "" {
					for _, vg := range validGenres {
						if strings.Contains(res.Genre, vg) {
							novel.Genre = vg
							updates["genre"] = vg
							break
						}
					}
				}
				if needStylePrompt {
					if sp := strings.TrimSpace(res.StylePrompt); sp != "" {
						updates["style_prompt"] = sp
					}
				}
				if needDescription {
					if desc := strings.TrimSpace(res.Description); desc != "" {
						updates["description"] = desc
					}
				}
			}
		}
	}

	// ── 2. 根据类型自动推断图片风格（无需 AI，纯映射）────────────────────────
	if needImageStyle {
		genreStyleMap := map[string]string{
			"武侠仙侠": "ink_painting", "古代言情": "ink_painting", "历史": "ink_painting",
			"玄幻奇幻": "anime", "幻想言情": "anime", "游戏": "anime", "游戏竞技": "anime", "N次元": "anime",
			"科幻": "cyberpunk", "军事": "realistic",
			"都市": "realistic", "现代言情": "realistic", "体育": "realistic",
		}
		if style, ok := genreStyleMap[novel.Genre]; ok {
			updates["image_style"] = style
		}
	}

	// ── 4. 目标章节数 / 目标字数（从已有章节推断）──────────────────────────
	if needTargets && len(chapters) > 0 {
		if novel.TargetChapters == 0 {
			updates["target_chapters"] = len(chapters)
		}
		if novel.TargetWordCount == 0 {
			totalWords := 0
			for _, ch := range chapters {
				totalWords += ch.WordCount
			}
			if totalWords == 0 {
				// WordCount 未计算时按 2000字/章 估算
				totalWords = len(chapters) * 2000
			}
			updates["target_word_count"] = totalWords
		}
	}

	if len(updates) == 0 {
		return nil
	}
	return s.novelRepo.UpdateFields(novel.ID, updates)
}
