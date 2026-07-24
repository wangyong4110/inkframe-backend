package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
)

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
		video.RenderConfig.QualityTier = qualityTierOverride
	}

	// Resolve effective provider and aspect ratio from novel config
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.RenderConfig.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if aspectRatio == "" && novel.VideoConf().VideoAspectRatio != "" {
				aspectRatio = novel.VideoConf().VideoAspectRatio
			}
		}
	}

	mode := video.Mode
	if mode == "" {
		mode = "video"
	}
	logger.Printf("BatchGenerateShots: videoID=%d total=%d mode=%s provider=%s aspectRatio=%s", videoID, len(shotIDs), mode, effectiveProvider, aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShots, batchErr := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErr != nil {
		return nil, batchErr
	}
	shotMap := make(map[uint]*model.StoryboardShot, len(allShots))
	for _, sh := range allShots {
		shotMap[sh.ID] = sh
	}

	// 图片解说模式：无论是否配置了视频 provider，都应该走"图片+Ken Burns"而非 AI 视频生成——
	// mode 才是唯一的开关，不能像视频动画模式那样由 provider 是否配置来决定分支。
	isSlideshow := mode == "slideshow"

	// 确定是否有视频提供商（对整批分镜一致；slideshow 模式下无意义，不参与分支判断）
	hasProvider := s.hasVideoProvider(s.videoTenantID(video))
	logger.Printf("BatchGenerateShots: hasVideoProvider=%v effectiveProvider=%q", hasProvider, effectiveProvider)

	// 并发数和队列键均从 DB 模型配置中统一获取
	tenantID := s.videoTenantID(video)
	providerType := "image"
	if hasProvider && !isSlideshow {
		providerType = "video"
	}
	concurrency := 1
	if s.aiService != nil {
		concurrency = s.aiService.GetProviderConcurrency(tenantID, providerType)
	}
	queueKey := fmt.Sprintf("%d:%s-gen", tenantID, providerType)

	var taskQueue *ModelTaskQueue
	if s.aiService != nil {
		taskQueue = s.aiService.ImageQueue
	} else {
		taskQueue = newModelTaskQueue()
	}

	total := len(shotIDs)
	var done atomic.Int32
	advanceProgress := func() {
		n := int(done.Add(1))
		if progressFn != nil && total > 0 {
			progressFn(n * 99 / total)
		}
	}

	// ── 第一阶段：过滤 + 更新状态 + 提交任务到队列 ─────────────────────────
	// 所有分镜任务提交后立即返回；Worker 严格按并发限制执行，超出部分在 channel 中排队，
	// 不会因为并发限制触发 API 429。
	type shotFuturePair struct {
		shot   *model.StoryboardShot
		future *TaskFuture
	}
	var pairs []shotFuturePair
	var queued []*model.StoryboardShot
	bgCtx := context.Background()
	const maxRetries = 3

	for _, sid := range shotIDs {
		shot, ok := shotMap[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		shot.Status = "generating"
		s.refreshShotUserEditableFields(shot)
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Errorf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", shot.ShotNo, err)
		}
		queued = append(queued, shot)

		sh := shot
		ar := aspectRatio
		ep := effectiveProvider

		var future *TaskFuture
		if isSlideshow {
			// 图片解说模式：GenerateSlideshowShotVideo（出图 + Ken Burns 编码，一步到位，
			// 写入 shot.VideoURL），不受 hasProvider 影响。
			future = taskQueue.Submit(queueKey, concurrency, bgCtx, func(ctx context.Context) (string, error) {
				var genErr error
				for attempt := 1; attempt <= maxRetries; attempt++ {
					genErr = s.GenerateSlideshowShotVideo(sh, ar)
					if genErr == nil {
						break
					}
					if isContentSafetyError(genErr) {
						logger.Warnf("BatchGenerateShots: shot %d safety rejection, skipping retries", sh.ShotNo)
						break
					}
					logger.Errorf("BatchGenerateShots: shot %d slideshow attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr != nil {
					logger.Errorf("BatchGenerateShots: shot %d slideshow failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
					if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "failed"}); e != nil {
						logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", sh.ID, e)
					}
				} else {
					logger.Printf("BatchGenerateShots: shot %d slideshow ready", sh.ShotNo)
				}
				return "", genErr
			})
		} else if !hasProvider {
			// 图片模式：generateShotImageOnly → DB 更新（全在 Worker goroutine 中完成）
			future = taskQueue.Submit(queueKey, concurrency, bgCtx, func(ctx context.Context) (string, error) {
				var genErr error
				for attempt := 1; attempt <= maxRetries; attempt++ {
					_, _, genErr = s.generateShotImageOnly(sh, ar)
					if genErr == nil {
						break
					}
					if isContentSafetyError(genErr) {
						logger.Warnf("BatchGenerateShots: shot %d safety rejection, skipping retries", sh.ShotNo)
						break
					}
					logger.Errorf("BatchGenerateShots: shot %d image attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr == nil {
					if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "completed"}); e != nil {
						logger.Errorf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", sh.ShotNo, e)
					}
					logger.Printf("BatchGenerateShots: shot %d image ready", sh.ShotNo)
				} else {
					logger.Errorf("BatchGenerateShots: shot %d image failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
					if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "failed"}); e != nil {
						logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", sh.ID, e)
					}
				}
				return "", genErr
			})
		} else {
			// 视频模式：GenerateShotVideo（提交给 provider，内部轮询直到完成）
			future = taskQueue.Submit(queueKey, concurrency, bgCtx, func(ctx context.Context) (string, error) {
				var genErr error
				for attempt := 1; attempt <= maxRetries; attempt++ {
					genErr = s.GenerateShotVideo(sh, ar, ep)
					if genErr == nil {
						break
					}
					logger.Errorf("BatchGenerateShots: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr != nil {
					logger.Errorf("BatchGenerateShots: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
					if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "failed"}); e != nil {
						logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", sh.ID, e)
					}
				} else {
					logger.Printf("BatchGenerateShots: shot %d submitted successfully (taskID=%s)", sh.ShotNo, sh.TaskMeta.ShotTaskID)
				}
				return "", genErr
			})
		}
		pairs = append(pairs, shotFuturePair{shot: sh, future: future})
	}

	// ── 第二阶段：并发 Await，推进进度 ────────────────────────────────────
	// 每个 future 起一个轻量 goroutine（只阻塞在 channel receive，不做 I/O），
	// 等待 Worker 写入结果后推进进度计数器。
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Add(1)
		go func(sf shotFuturePair) {
			defer func() {
				wg.Done()
				advanceProgress()
				logger.Printf("BatchGenerateShots: shot %d done", sf.shot.ShotNo)
			}()
			sf.future.Await(bgCtx) //nolint:errcheck // 错误已在 worker 内部处理并写入 DB
		}(p)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShots: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// BatchGenerateShotImages 批量为分镜生成参考图片（幂等：已有 ImageURL 的分镜自动跳过）。
// 只执行阶段一（AI 图片生成），不启动 Ken Burns 编码。
func (s *VideoService) BatchGenerateShotImages(videoID uint, shotIDs []uint, force bool, progressFn func(int)) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	aspectRatio := video.RenderConfig.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil && novel.VideoConf().VideoAspectRatio != "" {
			aspectRatio = novel.VideoConf().VideoAspectRatio
		}
	}

	logger.Printf("BatchGenerateShotImages: videoID=%d total=%d aspectRatio=%s", videoID, len(shotIDs), aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShotsImg, batchErrImg := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErrImg != nil {
		return nil, batchErrImg
	}
	shotMapImg := make(map[uint]*model.StoryboardShot, len(allShotsImg))
	for _, sh := range allShotsImg {
		shotMapImg[sh.ID] = sh
	}

	// 按 ShotNo 升序处理：确保同一场景中编号最小的分镜最先生成并锁定场景锚点，
	// 后续分镜在 imageSem 等待期间能借助已锁定的锚点参考图提升一致性。
	sort.Slice(shotIDs, func(i, j int) bool {
		si, oki := shotMapImg[shotIDs[i]]
		sj, okj := shotMapImg[shotIDs[j]]
		if !oki || !okj {
			return oki
		}
		return si.ShotNo < sj.ShotNo
	})

	// 并发数统一从图片提供商的 AIModel.Concurrency（DB 配置）读取
	tenantIDImg := s.videoTenantID(video)
	concurrency := 1
	if s.aiService != nil {
		concurrency = s.aiService.GetProviderConcurrency(tenantIDImg, "image")
	}
	queueKey := fmt.Sprintf("%d:image-gen", tenantIDImg)
	var imageQueue *ModelTaskQueue
	if s.aiService != nil {
		imageQueue = s.aiService.ImageQueue
	} else {
		imageQueue = newModelTaskQueue()
	}

	total := len(shotIDs)
	var done atomic.Int32
	advanceProgress := func() {
		n := int(done.Add(1))
		if progressFn != nil && total > 0 {
			progressFn(n * 99 / total)
		}
	}

	// ── 第一阶段：过滤 + 提交任务到队列 ──────────────────────────────────
	type shotFuturePair struct {
		shot   *model.StoryboardShot
		future *TaskFuture
	}
	var pairs []shotFuturePair
	bgCtx := context.Background() // 使用后台 ctx：即使 HTTP 请求断开，已提交的任务仍会执行

	for _, sid := range shotIDs {
		shot, ok := shotMapImg[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		if shot.Status == "generating" && !force {
			advanceProgress()
			continue
		}
		if shot.ImageURL != "" && shot.Status != "failed" && !force {
			advanceProgress()
			continue
		}
		sh := shot
		ar := aspectRatio
		future := imageQueue.Submit(queueKey, concurrency, bgCtx, func(ctx context.Context) (string, error) {
			// Worker 内部：重试逻辑 + DB 更新，完全隔离在 Worker goroutine 中执行
			metrics.ShotImageGenerationInFlight.Inc()
			defer metrics.ShotImageGenerationInFlight.Dec()

			const maxRetries = 3
			var localImage string
			var genErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				localImage, _, genErr = s.generateShotImageOnly(sh, ar)
				if genErr == nil {
					break
				}
				// 内容安全拦截是确定性失败（50511），重试相同输入没有意义；
				// generateShotReferenceImage 内部已降级为纯文生图，此处仍失败则直接放弃。
				if isContentSafetyError(genErr) {
					logger.Warnf("BatchGenerateShotImages: shot %d safety rejection, skipping retries", sh.ShotNo)
					break
				}
				logger.Errorf("BatchGenerateShotImages: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt*2) * time.Second)
				}
			}
			if localImage != "" {
				os.Remove(localImage) //nolint:errcheck
			}
			if genErr == nil {
				metrics.ShotImageGenerationTotal.WithLabelValues("success").Inc()
				if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "completed"}); e != nil {
					logger.Errorf("[VideoService] BatchGenerateShotImages: update shot %d status: %v", sh.ShotNo, e)
				}
				logger.Printf("BatchGenerateShotImages: shot %d image ready", sh.ShotNo)
			} else {
				metrics.ShotImageGenerationTotal.WithLabelValues("error").Inc()
				logger.Errorf("BatchGenerateShotImages: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "failed"}); e != nil {
					logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", sh.ID, e)
				}
			}
			return "", genErr // URL 已写入 DB，此处仅返回 err 供进度统计
		})
		pairs = append(pairs, shotFuturePair{shot: sh, future: future})
	}

	// ── 第二阶段：并发 Await，收集结果并推进进度 ──────────────────────────
	// 每个 future 起一个轻量 goroutine 等待结果（这些 goroutine 只阻塞在 channel receive，
	// 不做任何 I/O，内存占用远低于原来阻塞在信号量上的 goroutine）。
	var wg sync.WaitGroup
	var queued []*model.StoryboardShot
	for _, p := range pairs {
		queued = append(queued, p.shot)
		wg.Add(1)
		go func(sf shotFuturePair) {
			defer func() {
				wg.Done()
				advanceProgress()
				logger.Printf("BatchGenerateShotImages: shot %d done", sf.shot.ShotNo)
			}()
			sf.future.Await(bgCtx) //nolint:errcheck // 错误已在 worker 内部处理并写入 DB
		}(p)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotImages: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// GetStatus 获取视频生成状态（从 provider 同步最新进度）

// generateShotReferenceImage 为分镜生成参考帧图像，返回图片URL和错误。
// ─── 参考图合成辅助函数 ─────────────────────────────────────────────────────

const maxCompositeImages = 4 // 最多合成张数（角色最多3张 + 场景1张）

// getCharDefaultLook 返回角色当前使用的形象：优先取 Character.DefaultLookID 指向的形象；
// 最终兜底取第一个含三视图的形象（如老数据未设置 DefaultLookID）。
func (s *VideoService) getCharDefaultLook(char *model.Character) *model.CharacterLook {
	if s.lookRepo == nil {
		return nil
	}
	if char.DefaultLookID != 0 {
		if defaultLook, err := s.lookRepo.GetByID(char.DefaultLookID); err == nil && defaultLook != nil {
			return defaultLook
		}
	}
	// 兜底：角色有形象但 DefaultLookID 未设置（如老数据），取第一个含三视图的形象
	if looks, err := s.lookRepo.ListByCharacter(char.ID); err == nil {
		for _, l := range looks {
			if l.ThreeViewSheet != "" {
				logger.Printf("[getCharDefaultLook] charID=%d: DefaultLookID unset, fallback to first look with ThreeViewSheet id=%d", char.ID, l.ID)
				return l
			}
		}
	}
	return nil
}

// charLookRefImage 返回角色形象的参考图 URL（三视图合图，含正面/侧面/背面/面部特写）。
func (s *VideoService) charLookRefImage(look *model.CharacterLook) string {
	if look == nil {
		return ""
	}
	return normalizeMediaURL(look.ThreeViewSheet)
}

// ─── 分镜参考图生成 ──────────────────────────────────────────────────────────

func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) (string, error) {
	if s.aiService == nil {
		return "", fmt.Errorf("AI service not initialized")
	}

	// 章节序号：仅用于 OSS 存储路径提示（ImageStorageHint），与角色形象选取无关
	var chapterNo int
	if shot.ChapterID != nil && s.chapterRepo != nil {
		if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
			chapterNo = chapter.ChapterNo
		}
	}

	// 精准匹配：批量加载 shot.CharacterIDs 中的所有角色三视图（ThreeViewSheet），最多 maxCharRefs 张
	const maxCharRefs = maxCompositeImages - 1
	var characterPortraits []string
	var refSources []string
	// portraitOwners 与 characterPortraits 严格并行：记录有参考图角色的名字和视觉描述。
	// 用于单参考图提供商降级策略：主角用参考图，次要角色用文字描述。
	type portraitOwner struct{ name, vp string }
	var portraitOwners []portraitOwner
	// noPortraitVPs：无参考图角色的视觉描述，所有提供商都只能靠文字约束。
	var noPortraitVPs []string
	if len(shot.CharacterIDs) > 0 {
		ids := []uint(shot.CharacterIDs)
		batchChars, batchErr := s.characterRepo.ListByIDs(ids)
		if batchErr != nil {
			logger.Errorf("[CharRef] shot#%d ListByIDs(%v) failed: %v", shot.ShotNo, ids, batchErr)
		} else if len(batchChars) == 0 {
			logger.Errorf("[CharRef] shot#%d ListByIDs(%v) returned empty — characters may have been deleted", shot.ShotNo, ids)
		} else {
			// 按 shot.CharacterIDs 顺序处理，确保主角色（首位）的 Portrait 作为 DreamO 主参考图。
			// ListByIDs 使用 WHERE IN 不保证顺序，必须手动按 CharacterIDs 重排。
			charMap := make(map[uint]*model.Character, len(batchChars))
			for _, c := range batchChars {
				charMap[c.ID] = c
			}
			for _, cid := range shot.CharacterIDs {
				char, ok := charMap[cid]
				if !ok {
					continue
				}
				activeLook := s.getCharDefaultLook(char)
				var refImage, vprompt string
				if activeLook != nil {
					refImage = s.charLookRefImage(activeLook)
					vprompt = activeLook.VisualPrompt
				}
				urlType := "empty"
				if strings.HasPrefix(refImage, "https://") || strings.HasPrefix(refImage, "http://") {
					urlType = "absolute-url"
				} else if strings.HasPrefix(refImage, "/") {
					urlType = "relative-path"
				} else if refImage != "" {
					urlType = "other"
				}
				logger.Printf("[CharRef] shot#%d charID=%d name=%q activeLook=%v ref=%q urlType=%s",
					shot.ShotNo, char.ID, char.Name, activeLook != nil, refImage, urlType)
				charVP := vprompt
				if charVP == "" {
					charVP = buildCharTextAnchor(char)
				}
				if refImage != "" && len(characterPortraits) < maxCharRefs {
					characterPortraits = append(characterPortraits, refImage)
					refSources = append(refSources, fmt.Sprintf("charID=%d ThreeViewSheet", char.ID))
					portraitOwners = append(portraitOwners, portraitOwner{name: char.Name, vp: charVP})
				} else {
					noPortraitVPs = append(noPortraitVPs, charVP)
				}
			}
		}
	}

	logger.Printf("generateShotReferenceImage: shot %d charIDs=%v sources=%v portraits=%d",
		shot.ShotNo, shot.CharacterIDs, refSources, len(characterPortraits))
	if len(shot.CharacterIDs) > 0 && len(characterPortraits) == 0 {
		logger.Errorf("[WARN] generateShotReferenceImage: shot %d has CharacterIDs=%v but no portrait/ThreeViewSheet found — characters may not have images generated yet", shot.ShotNo, shot.CharacterIDs)
	}

	promptText := shot.Description

	// 场景锚点：注入锁定词，并收集场景参考图。
	// !! 必须在角色注入之前 prepend，这样角色信息最终排在场景描述前面。
	// 对 Seedream 等非 IP-Adapter 模型，prompt 靠前的 token 权重更高；
	// 若场景描述排在第一位，模型优先渲染场景而忽略角色。
	var sceneRefImage string
	var sceneAnchorName string
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, refURL, anchorName, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil {
			if fragment != "" {
				promptText = fragment + ", " + promptText
			}
			sceneRefImage = refURL
			sceneAnchorName = anchorName
		}
	}

	// 角色外观描述注入（prepend，排在场景锚点前）：
	// DreamO 模式（有参考图）：IP-Adapter 通过参考图保证角色外貌，注入冗长 VP 会：
	//   ① 将场景描述（LLM image_prompt）推到 600 字截断线后被丢弃
	//   ② 与参考图的外貌信号产生矛盾干扰
	//   因此 DreamO 模式只注入无参考图角色的 VP（这些角色外貌无其他约束），有参考图角色跳过。
	// Text2ImgV3 模式（无参考图）：文字是唯一外貌约束，注入全部 VP。
	if len(noPortraitVPs) > 0 || (len(portraitOwners) > 0 && len(characterPortraits) == 0) {
		if len(characterPortraits) > 0 {
			// DreamO 模式：只注入无参考图角色的 VP
			if len(noPortraitVPs) > 0 {
				promptText = strings.Join(noPortraitVPs, ", ") + ", " + promptText
			}
		} else {
			// Text2ImgV3 模式（无参考图）：文字是唯一外貌约束，注入全部 VP
			var allVPs []string
			for _, po := range portraitOwners {
				allVPs = append(allVPs, po.vp)
			}
			allVPs = append(allVPs, noPortraitVPs...)
			if len(allVPs) > 0 {
				promptText = strings.Join(allVPs, ", ") + ", " + promptText
			}
		}
	}

	// 角色名 + 动作/姿态（最后 prepend → 最终排在 prompt 最前面）：
	// 角色名排在 prompt 最前使 Seedream 将其识别为画面主体。
	// DreamO 模式（有参考图）和 Text2ImgV3 模式（无参考图）均注入，确保模型知道角色在做什么。
	hasAnyShotChars := len(characterPortraits) > 0 || len(noPortraitVPs) > 0 || len(portraitOwners) > 0
	if hasAnyShotChars {
		var presenceTokens []string // 人物存在性，从 portraitOwners 加载的角色名
		for _, po := range portraitOwners {
			if po.name != "" {
				presenceTokens = append(presenceTokens, po.name)
			}
		}
		if len(presenceTokens) > 0 {
			promptText = strings.Join(presenceTokens, "; ") + ", " + promptText
		}
	}

	// 道具参考图：使用分镜显式绑定的道具（shot.ItemIDs，通过"绑定道具"设置），
	// 优先取其生成完成的道具图（ImageURL —— 与角色 ThreeViewSheet 同源语义：AI 生成/确认后的最终形象图，
	// ItemsTab"批量生成图片"产出的就是这张），ImageURL 为空时退化为用户上传的原始参考图（ReferenceImageURL）。
	// 有角色时（DreamO 模式）：道具图不加入参考图列表（防止污染 IP embedding），仅通过 prompt 文字传达。
	// 无角色时（Text2ImgV3 模式）：可加入道具图作为视觉参考。
	var itemRefImages []string
	var itemRefNames []string // 与 itemRefImages 严格并行，用于参考图编号替换
	if s.itemRepo != nil && len(shot.ItemIDs) > 0 {
		items, err := s.itemRepo.ListByIDs([]uint(shot.ItemIDs))
		if err != nil {
			logger.Errorf("[ItemRef] shot#%d ListByIDs(%v) failed: %v", shot.ShotNo, shot.ItemIDs, err)
		} else {
			itemMap := make(map[uint]*model.Item, len(items))
			for _, it := range items {
				itemMap[it.ID] = it
			}
			for _, iid := range shot.ItemIDs {
				item, ok := itemMap[iid]
				if !ok {
					continue
				}
				refImage := item.ImageURL
				if refImage == "" {
					refImage = item.ReferenceImageURL
				}
				if refImage == "" {
					continue
				}
				itemRefImages = append(itemRefImages, normalizeMediaURL(refImage))
				itemRefNames = append(itemRefNames, item.Name)
				logger.Printf("[ItemRef] shot#%d item=%q refURL=%q", shot.ShotNo, item.Name, refImage)
			}
		}
	}

	ctx := context.Background()

	// 获取视频的 ArtStyle、TenantID、质量档位和宽高比
	// （提前到角色参考图截断逻辑之前，因为截断与否需要按 tenantID 判断实际生效的图片模型）
	artStyle := ""
	var tenantID uint
	qualityTier := "production" // 默认质量档位（preview=768px 对视频参考帧质量不够）
	var imageAspectRatio string
	if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
		artStyle = video.RenderConfig.ArtStyle
		tenantID = s.videoTenantID(video)
		imageAspectRatio = video.RenderConfig.AspectRatio
		if video.RenderConfig.QualityTier != "" {
			qualityTier = video.RenderConfig.QualityTier
		}
		if video.NovelID > 0 && s.novelRepo != nil {
			if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
				if tenantID == 0 {
					tenantID = novel.TenantID
				}
				vc := novel.VideoConf()
				// 项目设置的画面风格优先于视频级别的默认值
				if novel.AIConfig.ImageStyle != "" {
					artStyle = novel.AIConfig.ImageStyle
				}
				if imageAspectRatio == "" && vc.VideoAspectRatio != "" {
					imageAspectRatio = vc.VideoAspectRatio
				}
				// 注入 OSS 路径提示（项目名+章节序号）
				if novel.Title != "" {
					ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title, ChapterNo: chapterNo})
				}
			}
		}
	}

	// DreamO（seed3l_single_ip）是单 IP 模型，只支持一个角色的参考图。
	// 传入多个不同角色的参考图时，模型会将它们误认为同一角色的多视角，
	// 导致画面中同一角色出现多次（重复角色 bug）。仅在租户实际生效的图片模型为 DreamO 时
	// 才限制为 1 张参考图（主角色走 IP-Adapter，其余角色转为文字 VP 描述注入 prompt）；
	// 其余多图 API（jimeng4.0/4.6、doubao-seedream 等）原生支持多角色参考图，不做截断。
	cappedPortraits := characterPortraits
	if len(cappedPortraits) > 1 && s.aiService.activeImageModelIsSingleIP(tenantID) {
		cappedPortraits = characterPortraits[:1]
		// 超出部分的角色转为无参考图模式：VP 注入 noPortraitVPs，由文字约束外貌
		for _, po := range portraitOwners[1:] {
			noPortraitVPs = append(noPortraitVPs, po.vp)
		}
		logger.Printf("[CharRef] shot#%d capped references: %d→1 (moved %d extra char VPs to text injection)",
			shot.ShotNo, len(characterPortraits), len(characterPortraits)-1)
	}
	logger.Printf("[CharRef] shot#%d using %d character portrait(s) as reference", shot.ShotNo, len(cappedPortraits))

	// allRefImages 组装：角色图 + 道具图 + 场景锚定图。
	// 各 provider 按自身能力取用：
	//   - 多图 API（jimeng4.0/4.6、doubao-seedream 等）可全部使用；
	//   - 单图 API（Wanx、kling-image 等）由 provider 自身实现只取第一张（角色图优先）。
	var allRefImages []string
	allRefImages = append(allRefImages, cappedPortraits...)
	allRefImages = append(allRefImages, itemRefImages...)
	if sceneRefImage != "" {
		allRefImages = append(allRefImages, sceneRefImage)
	}
	logger.Printf("generateShotReferenceImage: shot %d allRefImages=%d (charPortraits=%d itemRefs=%d sceneRef=%v)",
		shot.ShotNo, len(allRefImages), len(characterPortraits), len(itemRefImages), sceneRefImage != "")

	// allRefImages 直接传给 API，无需合图（所有图生图 API 均支持多张参考图）
	logger.Printf("generateShotReferenceImage: shot %d qualityTier=%s aspectRatio=%s", shot.ShotNo, qualityTier, imageAspectRatio)

	// 构建负向提示词：基础解剖/物理规律排除词 + 分镜 LLM 生成的镜头专项排除词
	// 图像生成必须有负向提示词，否则极易出现变形肢体、违反物理规律、比例失调等问题
	// 纯环境镜头（无角色参考图时）：强制加入无人物排除词，防止 Text2ImgV3 随机生成人物
	imgNegBase := "worst quality, low quality, jpeg artifacts, noise, blurry, " +
		"deformed, ugly, bad anatomy, extra limbs, missing limbs, floating limbs, disconnected limbs, " +
		"malformed hands, missing fingers, fused fingers, extra fingers, poorly drawn hands, extra arms, extra legs, " +
		"bad proportions, gross proportions, long neck, cloned face, " +
		"out of frame, cropped head, poorly drawn face, poorly drawn eyes, asymmetric eyes, " +
		"text, watermark, logo, signature, " +
		"impossible physics, floating objects, gravity defying, " +
		"oversaturated, overexposed, underexposed"
	// 无角色参考图且分镜中确实没有任何角色时，加无人物排除词（纯环境镜头）。
	// 若分镜有角色（即使是没有参考图的路人），不加此约束，让模型根据 image_prompt 自行生成角色形象。
	shotHasAnyCharacter := len(characterPortraits) > 0 || len(shot.CharacterIDs) > 0
	noPersonNeg := "person, people, human, man, woman, figure, silhouette, character, face, body, limbs, hands, clothing, portrait"
	if !shotHasAnyCharacter {
		imgNegBase = noPersonNeg + ", " + imgNegBase
	}
	// 有角色时追加面部模糊专项负向词 + 重复角色专项负向词
	// 重复角色负向词：防止 DreamO（seed3l_single_ip）将参考图的多视角误判为多个角色实例
	faceNeg := "blurry face, out of focus face, soft focus face, unfocused face, " +
		"pixelated face, low res face, motion blur on face, smeared face, smudged face, " +
		"faceless, featureless face, undefined face, indistinct face"
	dupCharNeg := "duplicate character, cloned character, multiple copies of same person, " +
		"same character appearing twice, character repeated, split character, mirrored figure"
	if shotHasAnyCharacter {
		imgNegBase = imgNegBase + ", " + faceNeg + ", " + dupCharNeg
	}
	negPrompt := imgNegBase

	// Prompt 前缀策略：
	// shot.Description 已包含画风/画质词/镜头参数（见 storyboard_generate.j2 的结构化要求），
	// 只补充项目级调色和风格词，避免重复注入镜头参数（如 35mm vs 85mm）产生冲突，导致画面比例/构图异常。

	// 将风格 ID 解析为英文风格描述词（与 GenerateThreeViewSheet 保持一致）。
	// 使用 resolveStyleIllustrationDesc（英文）而非 resolveStyleDesc（中文），
	// 因为扩散模型对中文 token 信号弱且可能被忽略。
	// 无条件注入：LLM 生成的分镜描述可能残留旧风格词，以项目当前设置覆盖为准。
	styleDesc := ""
	if artStyle != "" {
		styleDesc = resolveStyleIllustrationDesc(artStyle)
	}

	// description 若已带了这段风格词（LLM 有时会把 ImageStyleHint 原样写进开头），
	// 此处再无条件 prepend 一次就会导致同一段风格词在 prompt 里出现两次，
	// 白白占用 800 字符上限的空间——见 promptText 已包含 styleDesc 时跳过注入。
	var prefix string
	if styleDesc != "" && !strings.Contains(promptText, styleDesc) {
		prefix += styleDesc + ", "
	}
	if prefix != "" {
		promptText = prefix + promptText
	}

	// 画质词强制注入：先移除与当前风格冲突的 realistic 质量词（防止旧分镜或 LLM 示例遗留的
	// "photorealistic, cinematic lighting" 污染动漫/水彩/国画等风格），再追加风格匹配的质量词。
	promptText = removeConflictingQualityTokens(promptText, artStyle)
	if !strings.Contains(strings.ToLower(promptText), "masterpiece") {
		promptText += ", " + resolveStyleQualityTokens(artStyle)
	}

	// 有角色时追加面部锐化词——无论是否有参考图。
	// 原仅在 cappedPortraits>0 时触发，导致无参考图的角色（如 Seedream 3.0 不支持 image 字段的情况）
	// 缺少面部正向约束，生成面部模糊。
	if shotHasAnyCharacter {
		promptText += ", sharp face, detailed face, crisp facial features, high facial detail, perfect face"
	}

	// DreamO 模式（有角色参考图）：IP-Adapter 已保证角色外貌，过长的 prompt 会分散注意力。
	// 截断至 600 字符，优先保留前段（场景/构图/动作），最多保留到最近一个逗号边界。
	// DreamO 模式截断：去掉角色 VP 注入后，prompt 的大部分是 style+presenceTokens+LLM场景描述+质量词。
	// 放宽至 1200 字符，保留完整 LLM 场景描述；超出才截断，优先截在逗号边界。
	if len(cappedPortraits) > 0 && len(promptText) > 1200 {
		truncated := promptText[:1200]
		if idx := strings.LastIndex(truncated, ","); idx > 600 {
			truncated = truncated[:idx]
		}
		promptText = truncated
	}

	// 参考图说明：在 prompt 最前面追加"参考图N对应角色/道具/场景名"的说明，
	// 使模型能将参考图位置与名称对应，避免误判为同一对象的多视角导致角色重复。
	// 不改写 prompt 正文中的原始名称，仅前置说明文本。
	// promptForFallback 保留追加说明前的版本，用于无参考图降级（此时参考图序号说明无意义）。
	promptForFallback := promptText
	if len(allRefImages) > 0 {
		nameToRefIdx := make(map[string]int)
		for i, po := range portraitOwners {
			if i < len(cappedPortraits) && po.name != "" {
				nameToRefIdx[po.name] = i + 1
			}
		}
		offset := len(cappedPortraits)
		for i, name := range itemRefNames {
			if name != "" {
				nameToRefIdx[name] = offset + i + 1
			}
		}
		if sceneRefImage != "" && sceneAnchorName != "" {
			nameToRefIdx[sceneAnchorName] = offset + len(itemRefImages) + 1
		}
		if refAnnotation := buildRefAnnotation(nameToRefIdx); refAnnotation != "" {
			promptText = refAnnotation + " " + promptText
			logger.Printf("[RefIdx] shot#%d refMap=%v annotation=%q", shot.ShotNo, nameToRefIdx, refAnnotation)
		}
	}

	// 场景锚点图片不加入 allRefImages：
	// 见上方"参考图列表"注释。二次读取也同样跳过场景图，防止并发批次中后加进来。

	logger.Printf("generateShotReferenceImage: shot %d prompt=%q negPrompt=%q", shot.ShotNo, promptText[:min(len(promptText), 120)], negPrompt[:min(len(negPrompt), 80)])
	var sceneSeed int64
	if shot.SceneAnchorID != nil {
		sceneSeed = int64(*shot.SceneAnchorID) * 31337
	}
	imageURL, err := s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, "", promptText, allRefImages, artStyle, negPrompt, "", sceneSeed)
	if err != nil && isContentSafetyError(err) && len(allRefImages) > 0 {
		// 参考图被内容安全系统拦截（50511 Post Img Risk Not Pass）：
		// 此类错误是确定性失败，重试相同参考图无意义。
		// 降级为纯文生图（无参考图），但需补注入参考图角色的 VisualPrompt（原 DreamO 模式下这些 VP 被跳过，
		// 因为参考图承担了外貌约束；纯文生图时文字是唯一外貌依据，必须补回）。
		logger.Warnf("generateShotReferenceImage: shot %d ref image blocked by safety filter, falling back to text-only with injected char VPs", shot.ShotNo)
		textOnlyPrompt := promptForFallback // 使用替换前版本，[图N] 在无参考图时无意义
		if len(cappedPortraits) > 0 {
			var fallbackVPs []string
			for i := range cappedPortraits {
				if i < len(portraitOwners) && portraitOwners[i].vp != "" {
					fallbackVPs = append(fallbackVPs, portraitOwners[i].vp)
				}
			}
			if len(fallbackVPs) > 0 {
				textOnlyPrompt = strings.Join(fallbackVPs, ", ") + ", " + textOnlyPrompt
			}
		}
		imageURL, err = s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, "", textOnlyPrompt, nil, artStyle, negPrompt, "", sceneSeed)
	}
	if err != nil {
		logger.Errorf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return "", err
	}
	if imageURL == "" {
		logger.Printf("generateShotReferenceImage: image gen returned empty URL for shot %d", shot.ShotNo)
		return "", fmt.Errorf("image provider returned empty URL")
	}

	return imageURL, nil
}

// isContentSafetyError 判断错误是否由 Volcengine 内容安全系统触发。
// code=50511 (Post Img Risk Not Pass) 表示提交的参考图被拦截，属于确定性失败，
// 重试相同输入不会改变结果，应立即降级而非重试。
func isContentSafetyError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "50511") || strings.Contains(s, "Risk Not Pass") || strings.Contains(s, "Img Risk")
}

// chineseNumerals 中文数字序列，用于参考图编号替换（[图一]、[图二]…）
var chineseNumerals = []string{
	"一", "二", "三", "四", "五", "六", "七", "八", "九", "十",
	"十一", "十二", "十三", "十四", "十五",
}

// buildRefAnnotation 根据"参考图序号→角色/道具/场景名"的映射，生成前置于 prompt 正文的
// 参考图说明文本；中文提示词用 [图N]（如"参考图说明：[图一]李白，..."），英文提示词用 [Image-N]。
// 不改写 prompt 正文中的原始名称，只生成前缀；图片生成和视频生成共用此方法。
func buildRefAnnotation(nameToRefIdx map[string]int) string {
	if len(nameToRefIdx) == 0 {
		return ""
	}
	type refEntry struct {
		idx  int
		name string
	}
	entries := make([]refEntry, 0, len(nameToRefIdx))
	for name, idx := range nameToRefIdx {
		entries = append(entries, refEntry{idx, name})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].idx < entries[j].idx })

	var mappings []string
	for _, e := range entries {
		cn := fmt.Sprintf("%d", e.idx)
		if e.idx >= 1 && e.idx <= len(chineseNumerals) {
			cn = chineseNumerals[e.idx-1]
		}
		tag := fmt.Sprintf("[图%s]", cn)
		mappings = append(mappings, tag+"为"+e.name)
	}
	return "参考图说明：" + strings.Join(mappings, "，") +
		"。每张参考图各对应不同的独立角色/道具/场景，每个角色只出现一次，不得重复。"
}

// buildCharTextAnchor 从角色基本信息构建文本锚点，用于无 VisualPrompt 时的最低限度外貌约束。
// 优先使用 AppearancePromptEN（AI 生成的时代准确形象提示词），兜底才用截断描述。
func buildCharTextAnchor(char *model.Character) string {
	if char.Meta.AppearancePrompt != "" {
		return char.Meta.AppearancePrompt
	}
	anchor := char.Name
	if char.Description != "" {
		desc := char.Description
		if runes := []rune(desc); len(runes) > 50 {
			desc = string(runes[:50])
		}
		anchor += ", " + desc
	}
	return anchor
}

// 成功后自动更新 DB 中的 ImageURL 并返回新 URL。
func (s *VideoService) RefineShotImage(shotID uint, suggestion string) (string, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return "", fmt.Errorf("shot %d not found: %w", shotID, err)
	}

	// 构建含修改建议的提示词（操作副本，不改 DB 原始字段）
	shotCopy := *shot
	if suggestion != "" {
		shotCopy.Description = shot.Description + ". Modification: " + suggestion
	}

	newURL, err := s.generateShotReferenceImage(&shotCopy)
	if err != nil {
		return "", fmt.Errorf("refine image for shot %d: %w", shotID, err)
	}

	// 持久化新图片 URL
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"image_url": newURL}); err != nil {
		logger.Errorf("[VideoService] RefineShot: failed to update shot %d image URL: %v", shotID, err)
	}
	return newURL, nil
}

// resolveArtStyle 返回视频的画面风格。
// 优先级：novel.AIConfig.ImageStyle（项目设置） > video.ArtStyle（视频级覆盖）。
// novel.AIConfig.ImageStyle 代表用户在"项目设置-画面风格"中的明确意图，应始终优先；
// video.ArtStyle 仅在 novel 未配置时作为降级。
func (s *VideoService) resolveArtStyle(videoID uint) string {
	if s.videoRepo == nil {
		return ""
	}
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return ""
	}
	// 优先使用小说级画面风格（项目设置优先）
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil && novel.AIConfig.ImageStyle != "" {
			return novel.AIConfig.ImageStyle
		}
	}
	// 小说未设置时降级使用视频自带风格
	return video.RenderConfig.ArtStyle
}

// extractLastFrame 使用 FFmpeg 提取视频最后一帧，返回本地 jpeg 路径
func (s *VideoService) extractLastFrame(clipPath string) (string, error) {
	// 处理 file:// 前缀
	localPath := strings.TrimPrefix(clipPath, "file://")

	tmpJpeg := fmt.Sprintf("%s/inkframe-lastframe-%d.jpg", inkframeTempDir(), time.Now().UnixNano())
	if _, err := runFFmpegCtx(context.Background(), "-y",
		"-sseof", "-0.1",
		"-i", localPath,
		"-vframes", "1",
		"-f", "image2",
		tmpJpeg,
	); err != nil {
		return "", fmt.Errorf("extractLastFrame failed: %w", err)
	}
	return tmpJpeg, nil
}

// uploadFrameToStorage 将本地 JPEG 帧图片上传到持久存储（OSS），返回持久 URL。
// 复用 uploadClipToStorage 的 OSS key 规则，以 /frames/ 子路径区分。
func (s *VideoService) uploadFrameToStorage(shot *model.StoryboardShot, localPath string) string {
	if s.storageSvc == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	f, err := os.Open(localPath)
	if err != nil {
		logger.Errorf("uploadFrameToStorage: open %s: %v", localPath, err)
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		logger.Errorf("uploadFrameToStorage: stat %s: %v", localPath, err)
		return ""
	}

	filename := uuid.New().String() + ".jpg"
	key := fmt.Sprintf("frames/%s", filename)

	ossURL, err := s.storageSvc.Upload(ctx, key, f, fi.Size(), "image/jpeg")
	if err != nil {
		logger.Errorf("uploadFrameToStorage: upload failed for shot %d: %v", shot.ShotNo, err)
		return ""
	}
	return ossURL
}

// chainLastFrameToNextShot 在分镜视频生成完成后提取最后一帧，写入下一个分镜的 reference_image_url。
// 非阻塞：调用方应在 goroutine 中调用此函数。
func (s *VideoService) chainLastFrameToNextShot(shot *model.StoryboardShot) {
	// 1. 找下一个分镜
	nextShot, err := s.storyboardRepo.GetByVideoAndShotNo(shot.VideoID, shot.ShotNo+1)
	if err != nil || nextShot == nil {
		return // 已是最后一镜或查询失败，无需链接
	}
	if nextShot.GenMeta.ReferenceImageURL != "" {
		return // 已有末帧，跳过（避免重复提取）
	}

	// 快捷路径：Seedance return_last_frame 已返回末帧 URL，无需下载视频
	if shot.TaskMeta.LastFrameURL != "" {
		nextShot.GenMeta.ReferenceImageURL = shot.TaskMeta.LastFrameURL
		s.refreshShotUserEditableFields(nextShot)
		if dbErr := s.storyboardRepo.Update(nextShot); dbErr != nil {
			logger.Errorf("chainLastFrameToNextShot: shot %d → nextShot %d Update failed: %v", shot.ShotNo, nextShot.ShotNo, dbErr)
			return
		}
		logger.Printf("chainLastFrameToNextShot: shot %d → nextShot %d last_frame_url (API直返) = %s", shot.ShotNo, nextShot.ShotNo, shot.TaskMeta.LastFrameURL)
		return
	}

	// 2. 确定视频本地路径（优先 file:// 本地文件，其次从远程 URL 下载）
	clipLocalPath := ""
	if strings.HasPrefix(shot.TaskMeta.ClipPath, "file://") {
		clipLocalPath = strings.TrimPrefix(shot.TaskMeta.ClipPath, "file://")
	} else {
		videoURL := shot.VideoURL
		if shot.TaskMeta.ClipPath != "" && !strings.HasPrefix(shot.TaskMeta.ClipPath, "file://") {
			videoURL = shot.TaskMeta.ClipPath
		}
		if videoURL == "" {
			logger.Errorf("chainLastFrameToNextShot: shot %d has no video URL/path", shot.ShotNo)
			return
		}
		tmp, dlErr := downloadToTemp(videoURL, "inkframe-chain-", ".mp4")
		if dlErr != nil {
			logger.Errorf("chainLastFrameToNextShot: shot %d download failed: %v", shot.ShotNo, dlErr)
			return
		}
		defer os.Remove(tmp)
		clipLocalPath = tmp
	}

	// 3. 提取最后一帧
	lastFramePath, err := s.extractLastFrame(clipLocalPath)
	if err != nil {
		logger.Errorf("chainLastFrameToNextShot: shot %d extractLastFrame failed: %v", shot.ShotNo, err)
		return
	}
	defer os.Remove(lastFramePath)

	// 4. 上传到 OSS（或保留本地路径）
	frameURL := s.uploadFrameToStorage(shot, lastFramePath)
	if frameURL == "" {
		// OSS 未配置或上传失败：直接复制到持久临时文件
		persistPath := fmt.Sprintf("%s/inkframe-lastframe-persist-%d.jpg", inkframeTempDir(), shot.ID)
		if copyErr := copyFile(lastFramePath, persistPath); copyErr != nil {
			logger.Errorf("chainLastFrameToNextShot: shot %d persist fallback failed: %v", shot.ShotNo, copyErr)
			return
		}
		frameURL = "file://" + persistPath
	}

	// 5. 写入下一分镜的 reference_image_url（GenMeta JSON 字段，需整体 Update）
	nextShot.GenMeta.ReferenceImageURL = frameURL
	s.refreshShotUserEditableFields(nextShot)
	if dbErr := s.storyboardRepo.Update(nextShot); dbErr != nil {
		logger.Errorf("chainLastFrameToNextShot: shot %d → nextShot %d Update failed: %v", shot.ShotNo, nextShot.ShotNo, dbErr)
		return
	}
	logger.Printf("chainLastFrameToNextShot: shot %d → nextShot %d reference_image_url=%s", shot.ShotNo, nextShot.ShotNo, frameURL)
}

// copyFile 将 src 文件复制到 dst（供 chainLastFrameToNextShot 在无 OSS 时持久化末帧）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// cameraTypeToKlingParams 根据摄像机类型映射最优的 Kling 生成参数。
// 升降/环绕等大范围运镜使用高 CFG + 10 秒展现全貌；其余镜头默认 5 秒防止内容填充。
func cameraTypeToKlingParams(cameraType string) (mode string, cfgScale float64, duration float64) {
	ct := strings.ToLower(cameraType)

	switch ct {
	case "crane_up", "crane_down", "crane":
		return "std", 0.8, 10
	default:
		// 默认 CFG=0.65：偏高忠实度，视频贴近参考帧，减少偏离场景的随机发挥
		return "std", 0.65, 5
	}
}

// shotVideoRenderConfig 汇总从 Video/Novel 配置中解析出的视频渲染参数。
type shotVideoRenderConfig struct {
	klingMode          string
	klingModelOverride string
	hdEnabled          bool
	threeDEnabled      bool
	threeDStyle        string
	resolution         string
	generateAudio      *bool
	priority           int
	webSearchEnabled   bool
	safetyID           string
}

// GenerateShotVideo 为单个分镜提交视频生成任务
func (s *VideoService) GenerateShotVideo(shot *model.StoryboardShot, videoAspectRatio string, providerOverride ...string) error {
	preferredProvider := ""
	if len(providerOverride) > 0 {
		preferredProvider = providerOverride[0]
	}
	// Determine tenantID from associated video for DB provider lookup
	var tenantID uint
	if video, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil {
		tenantID = s.videoTenantID(video)
	}
	provider, providerName, provErr := s.resolveVideoProvider(tenantID, preferredProvider)
	if provErr != nil {
		logger.Errorf("GenerateShotVideo: shot %d 找不到视频提供商 preferred=%s tenantID=%d: %v", shot.ShotNo, preferredProvider, tenantID, provErr)
		return fmt.Errorf("no video provider configured")
	}

	if s.aiService != nil {
		release, err := s.aiService.acquireProviderSlot(context.Background(), tenantID, providerName)
		if err != nil {
			return fmt.Errorf("GenerateShotVideo: acquire slot for %s: %w", providerName, err)
		}
		defer release()
	}

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}
	logger.Printf("GenerateShotVideo: shot %d provider=%s(%s) aspect=%s duration=%.2fs", shot.ShotNo, providerName, provider.GetName(), videoAspectRatio, shot.Duration)

	// 参考图策略（优先级从高到低）：
	//   ① shot.GenMeta.ReferenceImageURL 非空（上一镜最后一帧）→ I2V 时序链接，最高优先级
	//   ② shot.GenMeta.ReferenceImageURL 空 + shot.ImageURL 已生成 → 复用场景图
	//   ③ 两者均空 + 角色三视图/场景锚点存在 → 直接用这些作参考图
	//   ④ 无任何参考图 → 先生成场景图
	referenceImage, refLabel, err := s.resolveShotReferenceImage(shot)
	if err != nil {
		return err
	}

	// 画面风格：video.ArtStyle 优先，降级到 novel.AIConfig.ImageStyle
	videoArtStyle := s.resolveArtStyle(shot.VideoID)
	videoPrompt := s.buildShotVideoPrompt(shot, videoArtStyle)

	videoTraits := ai.VideoEngineTraitsFor(providerName)

	// 动态 Kling 参数（根据摄像机类型选择最优配置）
	klingMode, klingCFG, klingDefaultDur := cameraTypeToKlingParams(shot.CamDir.CameraType)
	shotDuration := s.resolveShotDuration(shot, klingDefaultDur, videoTraits)

	// 检查项目配置：KlingProForAction、HD、3D、Seedance 分辨率/音频
	renderCfg := s.resolveVideoRenderConfig(shot, tenantID, providerName, videoTraits, klingMode)

	videoPromptFinal, negativePrompt := buildShotCinematicPrompt(shot, videoPrompt, videoArtStyle, renderCfg)

	// Seedance / Kling / HappyHorse 均支持多张参考图：在主参考图之外追加角色三视图和场景锚点图
	extraRefImages, extraRefLabels := s.collectExtraReferenceImages(shot, referenceImage, videoTraits)

	// 外部 API 不能访问相对路径，将 /api/v1/media/* 补全为绝对 URL
	absRef := s.resolveAbsURL(referenceImage)
	var absExtras []string
	for _, u := range extraRefImages {
		if resolved := s.resolveAbsURL(u); resolved != "" {
			absExtras = append(absExtras, resolved)
		}
	}

	videoPromptFinal = applyRefIndexAnnotation(shot, videoPromptFinal, absRef, refLabel, extraRefLabels)
	videoPromptFinal = applyPerImageAnnotation(videoTraits, videoPromptFinal, absRef, refLabel, extraRefLabels, absExtras)

	// HappyHorse 分辨率：HD 模式用 1080P，否则 720P；Seedance/Doubao：使用 vidResolution 设置
	videoResolution := ""
	if videoTraits.DefaultResolution != nil {
		videoResolution = videoTraits.DefaultResolution(renderCfg.hdEnabled, renderCfg.resolution)
	}

	// Seedance 多模态时序链接：查找前一分镜的完成视频 URL 作为运动参考
	prevVideoURLs := s.resolvePrevVideoURLs(shot, videoTraits)

	req := buildVideoGenerateRequest(shot, videoPromptFinal, negativePrompt, shotDuration, videoAspectRatio, videoResolution, absRef, absExtras, prevVideoURLs, klingCFG, renderCfg, videoTraits)

	logger.Printf("GenerateShotVideo: shot %d submitting to %s(%s) (hasRef=%v extraRefs=%d mode=%s cfg=%.2f prompt=%q)", shot.ShotNo, providerName, provider.GetName(), referenceImage != "", len(extraRefImages), renderCfg.klingMode, klingCFG, videoPromptFinal)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		metrics.ShotVideoSubmissionTotal.WithLabelValues(providerName, "error").Inc()
		logger.Errorf("GenerateShotVideo: shot %d submit failed via %s(%s): %v", shot.ShotNo, providerName, provider.GetName(), err)
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	metrics.ShotVideoSubmissionTotal.WithLabelValues(providerName, "success").Inc()
	logger.Printf("GenerateShotVideo: shot %d submitted taskID=%s", shot.ShotNo, task.TaskID)
	shot.TaskMeta.ShotTaskID = task.TaskID
	shot.TaskMeta.ShotProviderName = providerName
	shot.Status = "processing"
	s.refreshShotUserEditableFields(shot)
	return s.storyboardRepo.Update(shot)
}

// resolveShotReferenceImage 按优先级解析分镜视频的参考图：
// ①上一镜末帧 I2V 链接 ②已生成的场景图（含旧本地路径迁移） ③④无场景图时先生成分镜首帧图。
func (s *VideoService) resolveShotReferenceImage(shot *model.StoryboardShot) (string, string, error) {
	var refLabel string // HappyHorse r2v: label for referenceImage ("角色名" or "")

	if shot.GenMeta.ReferenceImageURL != "" {
		// ① 上一镜最后一帧（I2V 链接）：作为主参考图，保证时序连贯
		referenceImage := shot.GenMeta.ReferenceImageURL
		logger.Printf("GenerateShotVideo: shot %d using last-frame I2V reference: %s", shot.ShotNo, referenceImage)
		// ImageURL（静态场景图）降级为附加参考图，维持外观一致性（在 collectExtraReferenceImages 中追加）
		return referenceImage, refLabel, nil
	}

	if shot.ImageURL != "" {
		// ② 已有正式镜头图，直接复用，无需再次生成
		referenceImage := shot.ImageURL
		logger.Printf("GenerateShotVideo: shot %d reusing existing ImageURL as reference: %s", shot.ShotNo, shot.ImageURL)
		// 迁移旧的本地 DB 路径到 OSS，同时永久更新 DB（只做一次）
		if migrated := s.migrateLocalImageToPublic(shot.ImageURL); migrated != shot.ImageURL {
			logger.Printf("GenerateShotVideo: shot %d migrated ImageURL %s → %s", shot.ShotNo, shot.ImageURL, migrated)
			referenceImage = migrated
			shot.ImageURL = migrated
			if err := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{
				"image_url": migrated,
			}); err != nil {
				logger.Errorf("GenerateShotVideo: shot %d persist migrated URL: %v", shot.ShotNo, err)
			}
		}
		return referenceImage, refLabel, nil
	}

	// ③④ 无正式场景图：统一先生成分镜首帧图，再用于 I2V
	// generateShotReferenceImage 内部已处理角色三视图/场景锚点参考，
	// 确保 shot.ImageURL（分镜图）与视频首帧严格一致。
	logger.Printf("GenerateShotVideo: shot %d ImageURL empty, generating storyboard first frame before video", shot.ShotNo)
	frameURL, frameErr := s.generateShotReferenceImage(shot)
	if frameErr != nil {
		logger.Errorf("GenerateShotVideo: shot %d image generation failed: %v", shot.ShotNo, frameErr)
	}
	if frameURL == "" {
		errMsg := "image generation failed: empty URL returned"
		if frameErr != nil {
			errMsg = "image generation failed: " + frameErr.Error()
		}
		if e := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"status": "failed", "error_message": errMsg}); e != nil {
			logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", shot.ID, e)
		}
		if frameErr != nil {
			return "", refLabel, frameErr
		}
		return "", refLabel, fmt.Errorf("shot %d: %s", shot.ShotNo, errMsg)
	}
	shot.ImageURL = frameURL
	s.refreshShotUserEditableFields(shot)
	if updateErr := s.storyboardRepo.Update(shot); updateErr != nil {
		logger.Errorf("GenerateShotVideo: shot %d failed to persist ImageURL: %v", shot.ShotNo, updateErr)
	}
	return frameURL, refLabel, nil
}

// buildContinuityPrefix 计算衔接语义前缀：截取上一镜头 Description 作粗粒度衔接引导。
// 仅在无 I2V 末帧时调用（有末帧时视频模型已能自动感知运动延续，文字引导可能干扰）。
func (s *VideoService) buildContinuityPrefix(shot *model.StoryboardShot) string {
	if shot.ShotNo <= 1 || shot.GenMeta.ReferenceImageURL != "" || s.storyboardRepo == nil {
		return ""
	}
	prev, prevErr := s.storyboardRepo.GetByVideoAndShotNo(shot.VideoID, shot.ShotNo-1)
	if prevErr != nil || prev == nil {
		return ""
	}
	desc := prev.Description
	if len([]rune(desc)) > 80 {
		desc = string([]rune(desc)[:80]) + "..."
	}
	if desc == "" {
		return ""
	}
	return "continuing from previous shot (" + desc + ")"
}

// buildShotVideoPrompt 组装视频生成 prompt：衔接语义 → 场景锚点锁定词 → 画面风格前缀 → 角色动作 → 台词/音效。
func (s *VideoService) buildShotVideoPrompt(shot *model.StoryboardShot, videoArtStyle string) string {
	// buildMotionPrompt 把 camera_type 映射为具体的运镜速度/方式词汇，并结合 description
	// 与昼夜氛围合成基础运动描述——description 本身是静态构图/光线描述，不含运镜信息。
	videoPrompt := buildMotionPrompt(shot)
	if continuityPrefix := s.buildContinuityPrefix(shot); continuityPrefix != "" {
		videoPrompt = continuityPrefix + ", " + videoPrompt
	}
	// 场景锚点：将锁定词注入视频生成 prompt
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, _, _, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && fragment != "" {
			videoPrompt = fragment + ", " + videoPrompt
		}
	}
	if videoArtStyle != "" {
		videoPrompt = resolveVideoStylePrefix(videoArtStyle) + videoPrompt
	}
	videoPrompt = appendNarrationDialogueSFX(videoPrompt, shot)
	return videoPrompt
}

// appendNarrationDialogueSFX 将旁白、角色台词、音效标签注入 prompt，帮助模型理解画面动作和声音氛围。
func appendNarrationDialogueSFX(videoPrompt string, shot *model.StoryboardShot) string {
	var extras []string
	if n := shot.Narration(); n != "" {
		if len([]rune(n)) > 50 {
			n = string([]rune(n)[:50]) + "…"
		}
		extras = append(extras, "narration: "+n)
	}
	if d := shot.Dialogue(); d != "" {
		if len([]rune(d)) > 60 {
			d = string([]rune(d)[:60]) + "…"
		}
		extras = append(extras, "dialogue: "+d)
	}
	if shot.GenMeta.SFXTags != "" {
		if sfxItems := parseSFXTags(shot.GenMeta.SFXTags); len(sfxItems) > 0 {
			tags := make([]string, 0, len(sfxItems))
			for _, item := range sfxItems {
				if item.Tag != "" {
					tags = append(tags, item.Tag)
				}
			}
			if len(tags) > 0 {
				extras = append(extras, "sound effects: "+strings.Join(tags, " / "))
			}
		}
	}
	if len(extras) == 0 {
		return videoPrompt
	}
	return videoPrompt + ", " + strings.Join(extras, ", ")
}

// resolveShotDuration 计算最终视频时长：情绪化默认时长 → TTS 配音总时长兜底 → 供应商固定档位对齐。
func (s *VideoService) resolveShotDuration(shot *model.StoryboardShot, klingDefaultDur float64, videoTraits ai.VideoEngineTraits) float64 {
	shotDuration := shot.Duration
	if shotDuration <= 0 {
		shotDuration = klingDefaultDur
	}
	// 用配音总时长覆盖 shotDuration：确保 Kling 选到能容纳完整配音的档位（5s 或 10s）
	if s.segmentRepo != nil {
		if segs, segErr := s.segmentRepo.ListByShotID(shot.ID); segErr == nil {
			var totalAudio float64
			for _, seg := range segs {
				totalAudio += seg.DurationSecs
			}
			if totalAudio > shotDuration {
				shotDuration = totalAudio
			}
		}
	}
	// Seedance / Doubao：duration 只接受 5 或 10（整数秒）；其他值一律 snap 到最近档位。
	// Kling 由 emotionToKlingParams 保证返回 5/10，无需额外处理。
	if videoTraits.SnapsFixedDuration {
		if shotDuration < 7.5 {
			shotDuration = 5
		} else {
			shotDuration = 10
		}
	}
	return shotDuration
}

// resolveVideoRenderConfig 解析 Video/Novel 中的渲染配置（KlingProForAction、HD、3D、Seedance 分辨率/音频），
// 并据此调整 Kling 模式与模型。
func (s *VideoService) resolveVideoRenderConfig(shot *model.StoryboardShot, tenantID uint, providerName string, videoTraits ai.VideoEngineTraits, klingMode string) shotVideoRenderConfig {
	cfg := shotVideoRenderConfig{klingMode: klingMode}

	if vid, vidErr := s.videoRepo.GetByID(shot.VideoID); vidErr == nil {
		cfg.resolution = vid.RenderConfig.Resolution
		cfg.generateAudio = vid.RenderConfig.GenerateAudio
		cfg.priority = vid.RenderConfig.Priority
		cfg.webSearchEnabled = vid.RenderConfig.WebSearchEnabled
		if vid.NovelID > 0 && s.novelRepo != nil {
			if novel, novelErr := s.novelRepo.GetByID(vid.NovelID); novelErr == nil {
				vc := novel.VideoConf()
				if cfg.klingMode == "pro" && !vc.KlingProForAction {
					cfg.klingMode = "std"
				}
				cfg.hdEnabled = strings.Contains(vid.RenderConfig.VisualMode, "hd")
				cfg.threeDEnabled = vc.ThreeDEnabled || strings.Contains(vid.RenderConfig.VisualMode, "3d")
				cfg.threeDStyle = vid.RenderConfig.ThreeDStyle
				cfg.klingModelOverride = vc.KlingModel
				if novel.TenantID > 0 {
					cfg.safetyID = fmt.Sprintf("tenant-%d", novel.TenantID)
				}
			}
		}
	}
	if cfg.threeDStyle == "" {
		cfg.threeDStyle = "cg"
	}
	// HD 模式：升级为更高清的模型并强制 pro
	if cfg.hdEnabled {
		if cfg.klingModelOverride == "" || cfg.klingModelOverride == "kling-v1" {
			cfg.klingModelOverride = "kling-v1-6"
		}
		cfg.klingMode = "pro"
	}

	// Doubao/Seedance：Model 字段必须是 Endpoint ID，不是 Kling 专用的 klingModelOverride。
	// 当 klingModelOverride 为空（或被 HD 逻辑跳过），从 DB 中查询该 provider 的活跃模型名称。
	if videoTraits.ResolvesModelFromDB && s.aiService != nil {
		if dbModel, dbErr := s.aiService.GetActiveVideoModelName(tenantID, providerName); dbErr == nil && dbModel != "" {
			cfg.klingModelOverride = dbModel
		}
	}
	return cfg
}

// buildShotCinematicPrompt 组装电影级动态前缀（运镜词+3D风格）与负向提示词。
func buildShotCinematicPrompt(shot *model.StoryboardShot, videoPrompt, videoArtStyle string, cfg shotVideoRenderConfig) (string, string) {
	// 电影级动态前缀——注入运镜词，风格自适应前缀（赛博朋克等特殊风格替换 film 词汇）
	cinematicPrefix := buildCinematicPrefix(shot.CamDir.CameraType, videoArtStyle)
	if cfg.threeDEnabled {
		cinematicPrefix = resolve3DStylePrefix(cfg.threeDStyle) + ", " + cinematicPrefix
	}
	// 视频生成专属负向词：补充 static/still/frozen/slideshow 防止模型生成静止画面
	negativeBase := "blurry, low quality, watermark, text overlay, deformed, ugly, " +
		"bad anatomy, duplicate, morbid, mutilated, out of frame, extra limbs, " +
		"gross proportions, malformed limbs, " +
		"static image, still frame, frozen, no motion, slideshow, photo, " +
		"flickering, temporal inconsistency, abrupt scene change, jump cut"

	videoPromptFinal := cinematicPrefix + videoPrompt
	return videoPromptFinal, negativeBase
}

// collectExtraReferenceImages 收集主参考图之外的额外参考图（I2V 场景图、角色三视图、场景锚点图），
// 角色按 CharacterIDs 顺序严格排列，保持和分镜角色的一一对应关系。
func (s *VideoService) collectExtraReferenceImages(shot *model.StoryboardShot, referenceImage string, videoTraits ai.VideoEngineTraits) ([]string, []string) {
	var extraRefImages []string
	var extraRefLabels []string // HappyHorse r2v：与 extraRefImages 并行的标签（角色名 / "场景背景"）

	// I2V 模式：shot.ImageURL（静态场景图）追加为第一额外参考图，维持外观一致性
	if shot.GenMeta.ReferenceImageURL != "" && shot.ImageURL != "" && videoTraits.SupportsMultiImageReference {
		if absImg := s.resolveAbsURL(shot.ImageURL); absImg != "" && absImg != s.resolveAbsURL(referenceImage) {
			extraRefImages = append(extraRefImages, absImg)
			extraRefLabels = append(extraRefLabels, "场景参考")
		}
	}
	if videoTraits.SupportsMultiImageReference && s.characterRepo != nil && len(shot.CharacterIDs) > 0 {
		chars, charErr := s.characterRepo.ListByIDs([]uint(shot.CharacterIDs))
		if charErr == nil && len(chars) > 0 {
			charMap := make(map[uint]*model.Character, len(chars))
			for _, c := range chars {
				charMap[c.ID] = c
			}
			// 按 CharacterIDs 顺序遍历，严格对应角色顺序
			for _, cid := range shot.CharacterIDs {
				c, ok := charMap[cid]
				if !ok {
					continue
				}
				look := s.getCharDefaultLook(c)
				img := s.charLookRefImage(look)
				if img != "" && img != referenceImage {
					extraRefImages = append(extraRefImages, img)
					extraRefLabels = append(extraRefLabels, c.Name)
				}
			}
		}
	}
	if videoTraits.SupportsMultiImageReference && s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if _, anchorRefURL, anchorLabel, anchorErr := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); anchorErr == nil && anchorRefURL != "" && anchorRefURL != referenceImage {
			extraRefImages = append(extraRefImages, anchorRefURL)
			if anchorLabel == "" {
				anchorLabel = "场景背景"
			}
			extraRefLabels = append(extraRefLabels, anchorLabel)
		}
	}
	return extraRefImages, extraRefLabels
}

// applyRefIndexAnnotation 在 prompt 最前追加"参考图N对应角色/道具/场景名"的说明，
// 使模型能将参考图位置与名称对应，不改写 prompt 正文中的原始名称。
// 参考图顺序：absRef(index 1) → absExtras[0](index 2) → absExtras[1](index 3) …
// "场景参考"（前一镜末帧）无对应名称，跳过；角色名和场景锚点名参与说明。
func applyRefIndexAnnotation(shot *model.StoryboardShot, videoPromptFinal, absRef, refLabel string, extraRefLabels []string) string {
	nameToRefIdx := make(map[string]int)
	baseIdx := 1
	if absRef != "" {
		if refLabel != "" {
			nameToRefIdx[refLabel] = baseIdx
		}
		baseIdx++
	}
	for i, label := range extraRefLabels {
		if label != "" && label != "场景参考" {
			nameToRefIdx[label] = baseIdx + i
		}
	}
	refAnnotation := buildRefAnnotation(nameToRefIdx)
	if refAnnotation == "" {
		return videoPromptFinal
	}
	logger.Printf("[RefIdx] video shot#%d refMap=%v annotation=%q", shot.ShotNo, nameToRefIdx, refAnnotation)
	return refAnnotation + " " + videoPromptFinal
}

// applyPerImageAnnotation 为 HappyHorse r2v 在 prompt 前缀注入 [Image N] 角色引用，帮助模型区分多张参考图中的人物。
// 官方文档：prompt 中使用 "[Image N]中的{名字}" 引用 media 数组第 N 张图（1-based）。
func applyPerImageAnnotation(videoTraits ai.VideoEngineTraits, videoPromptFinal, absRef, refLabel string, extraRefLabels []string, absExtras []string) string {
	if !videoTraits.NeedsPerImageAnnotation || (absRef == "" && len(absExtras) == 0) {
		return videoPromptFinal
	}
	totalImages := 0
	if absRef != "" {
		totalImages++
	}
	totalImages += len(absExtras)
	if totalImages < 2 {
		return videoPromptFinal
	}
	allLabels := make([]string, 0, 1+len(extraRefLabels))
	allLabels = append(allLabels, refLabel) // label for absRef ("角色名" or "")
	allLabels = append(allLabels, extraRefLabels...)
	var annotations []string
	for i, label := range allLabels {
		if i < totalImages && label != "" {
			annotations = append(annotations, fmt.Sprintf("[Image %d]为%s", i+1, label))
		}
	}
	if len(annotations) == 0 {
		return videoPromptFinal
	}
	return strings.Join(annotations, "，") + "。" + videoPromptFinal
}

// resolvePrevVideoURLs 为 Seedance 多模态时序链接查找前一分镜的完成视频 URL 作为运动参考。
func (s *VideoService) resolvePrevVideoURLs(shot *model.StoryboardShot, videoTraits ai.VideoEngineTraits) []string {
	if !videoTraits.SupportsTemporalLinking || shot.ShotNo <= 1 || s.storyboardRepo == nil {
		return nil
	}
	prev, prevErr := s.storyboardRepo.GetByVideoAndShotNo(shot.VideoID, shot.ShotNo-1)
	if prevErr != nil || prev == nil || prev.VideoURL == "" || !strings.HasPrefix(prev.VideoURL, "http") {
		return nil
	}
	logger.Printf("GenerateShotVideo: shot %d Seedance/Doubao video-chain: %s", shot.ShotNo, prev.VideoURL)
	return []string{prev.VideoURL}
}

// buildVideoGenerateRequest 组装视频生成请求，并附加 Seedance/豆包 专属参数。
func buildVideoGenerateRequest(shot *model.StoryboardShot, videoPromptFinal, negativePrompt string, shotDuration float64, videoAspectRatio, videoResolution, absRef string, absExtras, prevVideoURLs []string, klingCFG float64, cfg shotVideoRenderConfig, videoTraits ai.VideoEngineTraits) *ai.VideoGenerateRequest {
	req := &ai.VideoGenerateRequest{
		Prompt:         videoPromptFinal,
		NegativePrompt: negativePrompt,
		Duration:       shotDuration,
		AspectRatio:    videoAspectRatio,
		Resolution:     videoResolution,
		ImageURL:       absRef,        // 主参考图（生成的场景图），image-to-video；空时退化为 text-to-video
		ImageURLs:      absExtras,     // 额外参考图（Seedance 多图支持）
		VideoURLs:      prevVideoURLs, // 前一分镜视频（Seedance 多模态时序链接）
		CFGScale:       klingCFG,
		Mode:           cfg.klingMode,
		Model:          cfg.klingModelOverride,
	}
	// Seedance/豆包 专属参数
	if videoTraits.SupportsExtendedVideoParams {
		req.ReturnLastFrame = true // 让 API 直接返回末帧 URL，避免下载+ffprobe
		// generate_audio：必须显式传 true，API 默认值为 false（无声）。
		// 用户明确设为 false 时（静音模式）才跳过。
		if cfg.generateAudio != nil && !*cfg.generateAudio {
			// 用户显式关闭音频
			falseVal := false
			req.GenerateAudio = &falseVal
			shot.TaskMeta.HasEmbeddedAudio = false
		} else {
			// nil（未配置）或 true → 显式开启，确保 API 生成音效
			trueVal := true
			req.GenerateAudio = &trueVal
			shot.TaskMeta.HasEmbeddedAudio = true
		}
		// Seedance 2.0 新增参数
		req.Priority = cfg.priority
		req.WebSearchEnabled = cfg.webSearchEnabled
		if cfg.safetyID != "" {
			req.SafetyIdentifier = cfg.safetyID
		}
	}
	return req
}

// buildCinematicPrefix 根据摄像机类型生成动态电影级 prompt 前缀。
// 刻意移除了 "film still"（静帧含义），改用 "cinematic sequence" 强化动态感。
func buildCinematicPrefix(cameraType, artStyle string) string {
	motion := cameraMotionToken(cameraType)

	var base string
	switch artStyle {
	case "cyberpunk":
		base = "cyberpunk cinematic sequence, neon-lit rainy cityscape, holographic display glow, synthetic digital atmosphere, high-contrast dark shadows"
	case "steampunk":
		base = "steampunk cinematic sequence, brass machinery atmosphere, Victorian industrial fog, warm amber gaslight"
	case "gothic_dark":
		base = "gothic dark cinematic sequence, dramatic chiaroscuro lighting, ominous supernatural atmosphere, deep shadow volumes"
	case "anime", "chinese_animation":
		base = "anime cinematic sequence, dynamic visual style, vibrant color energy, expressive motion"
	case "ink_painting":
		base = "Chinese ink wash cinematic sequence, flowing brush atmosphere, monochromatic elegance, misty negative space"
	case "xianxia_style":
		base = "xianxia fantasy cinematic sequence, ethereal spiritual mist, celestial energy visualization, flowing robes"
	case "pixel_art":
		base = "pixel art cinematic sequence, crisp retro aesthetic, limited palette, 16-bit visual style"
	case "ukiyo_e":
		base = "ukiyo-e cinematic sequence, flat bold color blocks, strong contour lines, traditional Japanese aesthetic"
	default:
		// 未命中上面手写的旧版风格分支（如风格库新增/管理员自定义的预设）：
		// 回退到风格库配置的提示词，而非无视项目实际风格套用通用电影感前缀。
		if artStyle != "" {
			if desc := resolveStyleIllustrationDesc(artStyle); desc != "" {
				base = desc + " cinematic sequence"
				break
			}
		}
		base = "cinematic sequence, professional cinematography, anamorphic lens, natural film grain, high dynamic range"
	}

	if motion != "" {
		base = motion + ", " + base
	}
	return base + ", "
}

// resolveVideoStylePrefix 返回视频 prompt 专用的风格描述前缀（带末尾逗号+空格）。
// 比纯粹的 "cyberpunk style, " 更精确，为视频模型提供具体的视觉场景基调。
func resolveVideoStylePrefix(style string) string {
	switch style {
	case "cyberpunk":
		return "cyberpunk neon-lit city, rain-soaked reflective streets, holographic advertisements, synthetic digital glow, dark near-future dystopia, "
	case "steampunk":
		return "steampunk industrial scene, brass gears and steam pipes, Victorian mechanical aesthetic, amber gaslight, "
	case "gothic_dark":
		return "gothic dark fantasy scene, dramatic shadows, macabre atmosphere, deep jewel tones, "
	case "anime", "chinese_animation":
		return "anime visual style, vibrant cel-shaded colors, clean dynamic linework, "
	case "ink_painting":
		return "Chinese ink wash painting style, flowing brush strokes, monochrome ink atmosphere, "
	case "xianxia_style":
		return "Chinese xianxia fantasy style, ethereal mist and spiritual energy, flowing silk robes, "
	case "oil_painting":
		return "oil painting visual style, rich painterly brushwork, impasto texture, "
	case "watercolor":
		return "watercolor visual style, soft translucent washes, wet-on-wet color blending, "
	case "pixel_art":
		return "pixel art style, crisp retro 16-bit aesthetic, limited color palette, "
	case "ukiyo_e":
		return "ukiyo-e woodblock print style, flat bold color areas, strong black outlines, traditional Japanese Edo period aesthetic, "
	case "game_concept":
		return "game concept art style, professional fantasy character design, detailed rendering, "
	case "sketch":
		return "pencil sketch style, graphite linework, monochrome drawing aesthetic, "
	case "realistic", "real_person":
		return "" // 写实风格：视频模型默认即为写实，无需额外前缀
	default:
		// 未命中上面手写的旧版风格分支（如风格库新增/管理员自定义的预设）：
		// 回退到风格库配置的提示词（同图像生成路径），而非把 style_id 原样拼成无意义的英文词组。
		if style == "" {
			return ""
		}
		if desc := resolveStyleIllustrationDesc(style); desc != "" {
			return desc + ", "
		}
		return ""
	}
}

// cameraMotionToken 把 CameraType 映射为视频 prompt 运镜描述词。
func cameraMotionToken(cameraType string) string {
	switch strings.ToLower(cameraType) {
	case "pan":
		return "smooth camera pan"
	case "tilt":
		return "camera tilt movement"
	case "zoom":
		return "cinematic zoom"
	case "dolly":
		return "dolly shot, camera pushing forward"
	case "tracking", "track":
		return "smooth tracking shot following subject"
	case "crane", "crane_up":
		return "crane shot, camera rising dramatically"
	case "crane_down":
		return "crane shot, camera descending"
	case "arc":
		return "arc shot, camera orbiting subject"
	case "handheld":
		return "handheld camera, subtle natural shake"
	case "whip_pan":
		return "whip pan transition, fast swipe"
	default: // "static" or unknown — no motion token
		return ""
	}
}

// resolve3DStylePrefix 返回对应 3D 风格的提示词前缀。
func resolve3DStylePrefix(style string) string {
	switch style {
	case "pixar":
		return "Pixar-style 3D animation, stylized characters, warm appealing lighting, Disney Pixar quality render"
	case "anime3d":
		return "3D anime style, cel-shaded 3D, vibrant colors, smooth 3D animation, Japanese anime 3D render"
	case "realistic3d":
		return "ultra-realistic 3D render, Unreal Engine 5, ray tracing global illumination, cinematic 3D, 8K 3D rendering"
	default: // "cg"
		return "3D CGI animation, ray tracing, volumetric lighting, subsurface scattering, photorealistic 3D render, high-fidelity 3D"
	}
}

// PollShotStatus 轮询单个分镜视频生成状态

// generateKenBurnsClip 使用 FFmpeg zoompan 滤镜将静图制作成 Ken Burns 动效短片
// generateStillFrameClip 用 FFmpeg 将静态图片编码为固定时长的视频（无动效，Ken Burns 降级方案）。
func (s *VideoService) generateStillFrameClip(imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = defaultShotDurationSecs
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
	outPath := fmt.Sprintf("%s/inkframe-still-%s.mp4", inkframeTempDir(), uuid.New().String()[:8])
	logger.Printf("generateStillFrameClip: start image=%s duration=%.1fs res=%s → %s", imagePath, duration, resolution, outPath)
	encStart := time.Now()
	// 使用 goroutine 超时而非 context 超时：wazero 在密集计算时不响应 ctx 取消。
	// -preset ultrafast -tune stillimage 大幅降低 WASM x264 编码时间（静止帧全为 P 帧）。
	out, err := runFFmpegWithGoroutineTimeout(90*time.Second,
		"-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "stillimage",
		"-pix_fmt", "yuv420p",
		"-r", "24",
		outPath,
	)
	if err != nil {
		logger.Errorf("generateStillFrameClip: failed after %.1fs: %v\noutput: %s", time.Since(encStart).Seconds(), err, string(out))
		return "", fmt.Errorf("ffmpeg still frame: %w", err)
	}
	logger.Printf("generateStillFrameClip: done in %.1fs → %s", time.Since(encStart).Seconds(), outPath)
	return outPath, nil
}

func (s *VideoService) generateKenBurnsClip(shot *model.StoryboardShot, imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	fps := 24 // P1-4: match synthesis output fps to eliminate concat stuttering
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
	switch shot.CamDir.CameraType {
	case "zoom", "push":
		// 推镜/变焦：明显放大，模拟向前推进
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.002,1.5)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	case "pull":
		// 拉镜：缩小，模拟向后拉远（从1.4缩到1.0）
		zoompan = fmt.Sprintf("zoompan=z='max(1.4-t*0.4/%.1f,1.0)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", duration, totalFrames)
	case "pan", "track":
		// 摇镜/移镜：水平平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f))':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	case "crane_up":
		// 升镜：向上平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='iw/2-(iw/zoom/2)':y='trunc(ih-(ih/zoom)-t*((ih-(ih/zoom))/%.1f))'", totalFrames, duration)
	case "crane_down":
		// 降镜：向下平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='iw/2-(iw/zoom/2)':y='trunc(t*((ih-(ih/zoom))/%.1f))'", totalFrames, duration)
	case "whip_pan":
		// 甩镜：快速水平扫过
		zoompan = fmt.Sprintf("zoompan=z=1.2:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f)*2)':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	default:
		// static / follow / arc / tilt / 旧值：默认轻微放大（Ken Burns 经典效果）
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.0008,1.2)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	}

	outPath := fmt.Sprintf("%s/inkframe-slideshow-%d-%s.mp4", inkframeTempDir(), shot.ID, uuid.New().String()[:8])
	// pre-scale 到恰好等于输出分辨率：zoompan 的 zoom≤1.2 只需输入≥输出即可，更大对效果无益
	// 但会让 WASM 每帧计算量成倍增加（3840 vs 1920 = 4x 像素量）。
	// 1920:-1 for 16:9, 1080:-1 for 9:16/1:1 — 均与最终输出宽度对齐。
	preScale := "1920:-1"
	if aspectRatio == "9:16" || aspectRatio == "1:1" {
		preScale = "1080:-1"
	}
	vf := fmt.Sprintf("scale=%s,%s,scale=%s,setsar=1", preScale, zoompan, resolution)

	// P0-2: WASM cannot be interrupted via context.WithTimeout; use goroutine-level timeout.
	// 30s covers typical zoompan runs (10-25s on a single CPU); on timeout falls back to still frame.
	if _, err := runFFmpegWithGoroutineTimeout(30*time.Second, "-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%d", fps),
		"-threads", "1",
		outPath,
	); err != nil {
		return "", fmt.Errorf("ffmpeg ken burns: %w", err)
	}
	return outPath, nil
}

// generateShotImageOnly 执行图片解说模式的第一阶段：生成图片 + 下载到本地临时文件。
// 返回本地文件路径和实际视频时长；调用方负责在使用完毕后删除该文件。
// shot.Status 会在此函数内被设置为 "generating"；完成后调用方应更新为 "completed"。
func (s *VideoService) generateShotImageOnly(shot *model.StoryboardShot, aspectRatio string) (localImage string, duration float64, err error) {
	duration = shot.Duration
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	shot.GenMeta.GenerationMode = "static"
	shot.Status = "generating"
	s.refreshShotUserEditableFields(shot)
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Errorf("[VideoService] generateShotImageOnly: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		shot.Status = "failed"
		shot.TaskMeta.ErrorMessage = errMsg
		s.refreshShotUserEditableFields(shot)
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Errorf("[VideoService] generateShotImageOnly: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return "", 0, fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return "", 0, fmt.Errorf("image generation failed for shot %d (empty URL)", shot.ShotNo)
	}
	var tenantID uint
	if v, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil {
		tenantID = s.videoTenantID(v)
	}
	s.snapshotShotAsset(shot, "image", shot.ImageURL, tenantID)
	shot.ImageURL = imageURL
	s.refreshShotUserEditableFields(shot)
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Errorf("[VideoService] generateShotImageOnly: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// Async scene consistency scoring: compare generated image vs scene anchor reference image.
	if s.sceneConsistencySvc != nil && s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		go func(sh *model.StoryboardShot, imgURL string) {
			tenantID := uint(0)
			novelID := uint(0)
			if v, err := s.videoRepo.GetByID(sh.VideoID); err == nil {
				tenantID = s.videoTenantID(v)
				novelID = v.NovelID
			}
			anchor, err := s.sceneAnchorSvc.GetByID(*sh.SceneAnchorID)
			if err == nil {
				if report, err := s.sceneConsistencySvc.ScoreScene(sh, anchor, imgURL, 1, tenantID, novelID); err != nil {
					logger.Errorf("[VideoService] ScoreScene shot %d: %v", sh.ShotNo, err)
				} else {
					logger.Printf("[VideoService] ScoreScene shot %d: overall=%.2f passed=%v", sh.ShotNo, report.OverallScore, report.Passed)
				}
			}
		}(shot, imageURL)
	}

	// 只对绝对 URL（CDN/OSS）执行下载。相对路径（/api/v1/media/...，本地 DB 存储）
	// 无法被独立 http.Client 访问；而两个调用方（BatchGenerateShots/BatchGenerateShotImages）
	// 拿到 localImage 后立即 os.Remove——ImageURL 已存 DB，本地文件实际上无需下载。
	if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		localImage, err = downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
		if err != nil {
			return "", 0, fmt.Errorf("download image for shot %d: %w", shot.ShotNo, err)
		}
	}
	return localImage, duration, nil
}

// generateClipAndUploadWithRetry 在后台 goroutine 中执行 Ken Burns 编码 + OSS 上传，
// 支持最多 maxClipRetries 次自动重试（指数退避）。
// 无论成功与否，最终均将 progress 更新为 100，并清理本地临时文件。
const maxClipRetries = 3

func (s *VideoService) generateClipAndUploadWithRetry(ctx context.Context, shotID uint, localImage string, duration float64, aspectRatio string) {
	defer os.Remove(localImage)

	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		logger.Errorf("generateClipAndUploadWithRetry: shot %d not found: %v", shotID, err)
		return
	}
	// 用配音时长覆盖 duration，确保 Ken Burns 视频与配音等长
	if audioDur := s.shotTotalAudioDuration(shot); audioDur > duration {
		duration = audioDur
	}

	var clipPath string
	var lastErr error

	for attempt := 1; attempt <= maxClipRetries; attempt++ {
		// 优先纯 Go Ken Burns；失败时降级为静止画面
		clipPath, lastErr = s.generateKenBurnsPureGo(ctx, shot, localImage, duration, aspectRatio)
		if lastErr != nil {
			logger.Errorf("generateClipAndUploadWithRetry: shot %d ken burns attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
			clipPath, lastErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
		}
		if lastErr == nil {
			break
		}
		logger.Errorf("generateClipAndUploadWithRetry: shot %d still frame attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
		if attempt < maxClipRetries {
			select {
			case <-time.After(time.Duration(attempt*5) * time.Second):
			case <-ctx.Done():
				logger.Printf("[VideoService] generateClipAndUploadWithRetry: context cancelled for shot %d, stopping retries", shotID)
				return
			}
		}
	}

	fields := map[string]interface{}{}
	if lastErr != nil {
		logger.Errorf("generateClipAndUploadWithRetry: shot %d clip failed after %d attempts, keeping image-only: %v", shot.ShotNo, maxClipRetries, lastErr)
	} else if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		var tenantID uint
		if v, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil {
			tenantID = s.videoTenantID(v)
		}
		s.snapshotShotAsset(shot, "video", shot.VideoURL, tenantID)
		fields["video_url"] = ossURL
		fields["clip_path"] = ""
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip → %s", shot.ShotNo, ossURL)
	} else {
		fields["clip_path"] = "file://" + clipPath
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip done (local only)", shot.ShotNo)
	}
	if err := s.storyboardRepo.UpdateFields(shotID, fields); err != nil {
		logger.Errorf("[VideoService] generateClipAndUploadWithRetry: failed to update shot %d fields: %v", shotID, err)
	}
}

// GenerateSlideshowShotVideo 为单个分镜生成图片并应用 Ken Burns 动效（图片解说模式）
// 此函数保持同步语义，供 runSlideshowPipeline 的顺序流水线使用。
// BatchGenerateShots 中的批量生成改用 generateShotImageOnly + generateClipAndUploadWithRetry 两阶段异步模式。
func (s *VideoService) GenerateSlideshowShotVideo(shot *model.StoryboardShot, aspectRatio string) error {
	duration := shot.Duration
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}

	logger.Printf("GenerateSlideshowShotVideo: shot %d aspect=%s duration=%.1fs", shot.ShotNo, aspectRatio, duration)

	var tenantID uint
	if v, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil {
		tenantID = s.videoTenantID(v)
	}

	shot.GenMeta.GenerationMode = "static"
	shot.Status = "generating"
	s.refreshShotUserEditableFields(shot)
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Errorf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	// 1. 生成图片
	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		logger.Errorf("GenerateSlideshowShotVideo: image gen failed for shot %d: %s", shot.ShotNo, errMsg)
		shot.Status = "failed"
		shot.TaskMeta.ErrorMessage = errMsg
		s.refreshShotUserEditableFields(shot)
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Errorf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return fmt.Errorf("image generation failed for shot %d (empty URL returned)", shot.ShotNo)
	}
	s.snapshotShotAsset(shot, "image", shot.ImageURL, tenantID)
	shot.ImageURL = imageURL
	logger.Printf("GenerateSlideshowShotVideo: shot %d storing image_url=%q (len=%d)", shot.ShotNo, imageURL, len(imageURL))
	// 保存图片 URL（后续步骤失败时图片仍可用）
	s.refreshShotUserEditableFields(shot)
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Errorf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// 2. 生成 Ken Burns 动效视频片段
	logger.Printf("GenerateSlideshowShotVideo: shot %d starting Ken Burns encode", shot.ShotNo)
	localImage, dlErr := s.resolveImageURLToLocalFile(shot.ImageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID))
	if dlErr != nil {
		logger.Errorf("GenerateSlideshowShotVideo: shot %d resolve image failed: %v — skipping Ken Burns", shot.ShotNo, dlErr)
		shot.Status = "completed"
		shot.TaskMeta.Progress = 100
		s.refreshShotUserEditableFields(shot)
		return s.storyboardRepo.Update(shot)
	}
	defer os.Remove(localImage)

	var clipPath string
	var clipErr error
	for attempt := 1; attempt <= maxClipRetries; attempt++ {
		clipPath, clipErr = s.generateKenBurnsPureGo(context.Background(), shot, localImage, duration, aspectRatio)
		if clipErr != nil {
			clipPath, clipErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
		}
		if clipErr == nil {
			break
		}
		logger.Errorf("GenerateSlideshowShotVideo: shot %d Ken Burns attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, clipErr)
		if attempt < maxClipRetries {
			time.Sleep(time.Duration(attempt*5) * time.Second)
		}
	}
	if clipErr != nil {
		logger.Errorf("GenerateSlideshowShotVideo: shot %d Ken Burns failed: %v", shot.ShotNo, clipErr)
	} else if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		s.snapshotShotAsset(shot, "video", shot.VideoURL, tenantID)
		shot.VideoURL = ossURL
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("GenerateSlideshowShotVideo: shot %d video → %s", shot.ShotNo, ossURL)
	} else {
		s.snapshotShotAsset(shot, "video", shot.VideoURL, tenantID)
		shot.VideoURL = "file://" + clipPath
		logger.Printf("GenerateSlideshowShotVideo: shot %d video local → %s", shot.ShotNo, clipPath)
	}

	shot.Status = "completed"
	shot.TaskMeta.Progress = 100
	s.refreshShotUserEditableFields(shot)
	return s.storyboardRepo.Update(shot)
}

// resolveImageURLToLocalFile 将图片 URL 解析为本地临时文件路径，支持三种来源：
//  1. https:// 或 http:// — 直接下载
//  2. /api/v1/media/{id}  — 从 DB storage 读取（storageSvc.Get）
//  3. file:///path        — 直接返回本地路径（不复制）
func (s *VideoService) resolveImageURLToLocalFile(imageURL, prefix string) (string, error) {
	if strings.HasPrefix(imageURL, "file://") {
		return strings.TrimPrefix(imageURL, "file://"), nil
	}
	if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		return downloadToTemp(imageURL, prefix, ".jpg")
	}
	// DB / local storage 相对路径：通过 storageSvc.Get 读取二进制数据
	if s.storageSvc == nil {
		return "", fmt.Errorf("no storage service available to resolve %q", imageURL)
	}
	data, err := s.storageSvc.Get(context.Background(), imageURL)
	if err != nil {
		return "", fmt.Errorf("resolveImageURLToLocalFile %q: %w", imageURL, err)
	}
	f, err := os.CreateTemp(inkframeTempDir(), prefix+"*.jpg")
	if err != nil {
		return "", fmt.Errorf("resolveImageURLToLocalFile create temp: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name()) //nolint:errcheck
		return "", fmt.Errorf("resolveImageURLToLocalFile write temp: %w", err)
	}
	return f.Name(), nil
}

// uploadClipToStorage 将本地 MP4 文件上传到持久存储（OSS），返回持久 URL。
// storageSvc 为 nil 或上传失败时返回 ""（调用方保留 file:// 本地路径）。
// OSS key 格式：novels/{title}/chapters/{no}/videos/{uuid}.mp4
//
//	章节 ID 未知时降级：videos/{uuid}.mp4

// runSlideshowPipeline 异步处理图片解说模式的所有分镜，完成后自动拼接
func (s *VideoService) runSlideshowPipeline(ctx context.Context, videoID uint) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		logger.Errorf("runSlideshowPipeline: get video %d failed: %v", videoID, err)
		return
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil || len(shots) == 0 {
		logger.Printf("runSlideshowPipeline: no pending shots for video %d", videoID)
		return
	}

	// 从小说视频配置读取旁白音色
	narrationVoice := ""
	if vc := s.GetNovelVideoConfig(video.NovelID); vc != nil {
		narrationVoice = vc.Config.NarrationVoice
	}

	var audioWg sync.WaitGroup
	for _, shot := range shots {
		if err := s.GenerateSlideshowShotVideo(shot, video.RenderConfig.AspectRatio); err != nil {
			logger.Errorf("runSlideshowPipeline: shot %d failed: %v", shot.ShotNo, err)
		}
		audioWg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer audioWg.Done()
			if err := s.GenerateShotAudio(ctx, sh, s.videoTenantID(video), narrationVoice); err != nil {
				logger.Errorf("runSlideshowPipeline: audio gen failed for shot %d: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	audioWg.Wait()
	// 图片生成完成后不自动拼接；拼接由独立步骤（先生成 Ken Burns 片段，再 StitchVideo）触发
}

// GenerateAllShotVideos 提交所有待生成分镜的视频任务
func (s *VideoService) GenerateAllShotVideos(ctx context.Context, videoID uint) error {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return err
	}

	tenantID := s.videoTenantID(video)
	hasProvider := s.hasVideoProvider(tenantID)
	logger.Printf("GenerateAllShotVideos: videoID=%d mode=%q tenantID=%d hasVideoProvider=%v", videoID, video.Mode, tenantID, hasProvider)

	// 无视频提供商：降级为图片解说 + Ken Burns
	if !hasProvider {
		shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if err != nil || len(shots) == 0 {
			return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
		}
		video.Status = "generating"
		video.TaskMeta.ErrorMessage = ""
		if err := s.videoRepo.Update(video); err != nil {
			logger.Errorf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
		}
		logger.Printf("GenerateAllShotVideos: videoID=%d → slideshow fallback (no video provider)", videoID)
		// 同步执行以确保调用方（handler goroutine）等待完成后再标记任务结束
		s.runSlideshowPipeline(ctx, videoID)
		// 拼接所有 completed 分镜
		if _, stitchErr := s.StitchVideoCtx(ctx, videoID); stitchErr != nil {
			logger.Errorf("GenerateAllShotVideos: slideshow stitch failed videoID=%d: %v", videoID, stitchErr)
		}
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
	video.TaskMeta.ErrorMessage = ""
	if err := s.videoRepo.Update(video); err != nil {
		logger.Errorf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
	}

	// 从小说视频配置读取旁白音色
	narrationVoice := ""
	if vc := s.GetNovelVideoConfig(video.NovelID); vc != nil {
		narrationVoice = vc.Config.NarrationVoice
	}

	for _, shot := range shots {
		if err := s.GenerateShotVideo(shot, video.RenderConfig.AspectRatio); err != nil {
			logger.Errorf("GenerateAllShotVideos: shot %d failed: %v", shot.ShotNo, err)
			continue
		}
		// TTS audio in parallel
		go s.GenerateShotAudio(ctx, shot, s.videoTenantID(video), narrationVoice) //nolint:errcheck
	}
	return nil
}

// containsChinese 检查字符串是否包含中文字符（CJK Unified Ideographs 基本区）
func containsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// normalizeMediaURL 修复 DB 存储时写入的畸形 /api/v1/media/ 路径：
//   - "/ap1/media/N"   → "/api/v1/media/N"  (ap1 typo)
//   - "/api//media/N"  → "/api/v1/media/N"  (missing v1, double slash)
//   - "/v1/media/N"    → "/api/v1/media/N"  (missing api/ prefix)
func normalizeMediaURL(u string) string {
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	for _, bad := range []string{"/ap1/media/", "/api//media/", "/v1/media/"} {
		if strings.HasPrefix(u, bad) {
			return "/api/v1/media/" + u[len(bad):]
		}
	}
	return u
}

// ─── Sequential Generation ────────────────────────────────────────────────────

// SequentialGenerateShots 顺序生成分镜（高质量衔接模式）：
// 每个分镜提交后内联等待完成，再同步提取最后一帧写入下一分镜的 ReferenceImageURL，
// 保证所有分镜均基于前一镜头真实最后一帧做 I2V，从根本上消除割裂感。
// 代价：无并发，速度约为并发模式的 1/N，适合对连贯性要求极高的最终输出。
func (s *VideoService) SequentialGenerateShots(videoID uint, shotIDs []uint, qualityTierOverride string, progressFn func(int), provider ...string) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	if qualityTierOverride != "" {
		video.RenderConfig.QualityTier = qualityTierOverride
	}
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.RenderConfig.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if aspectRatio == "" && novel.VideoConf().VideoAspectRatio != "" {
				aspectRatio = novel.VideoConf().VideoAspectRatio
			}
		}
	}

	allShots, batchErr := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErr != nil {
		return nil, batchErr
	}
	shotMap := make(map[uint]*model.StoryboardShot, len(allShots))
	for _, sh := range allShots {
		shotMap[sh.ID] = sh
	}
	var ordered []*model.StoryboardShot
	for _, sid := range shotIDs {
		if sh, ok := shotMap[sid]; ok && sh.VideoID == videoID {
			ordered = append(ordered, sh)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ShotNo < ordered[j].ShotNo })

	total := len(ordered)
	var completed []*model.StoryboardShot
	logger.Printf("SequentialGenerateShots: videoID=%d total=%d provider=%s", videoID, total, effectiveProvider)

	for idx, shot := range ordered {
		shot.Status = "generating"
		s.refreshShotUserEditableFields(shot)
		if e := s.storyboardRepo.Update(shot); e != nil {
			logger.Errorf("SequentialGenerateShots: shot %d status update: %v", shot.ShotNo, e)
		}

		const maxRetries = 3
		var genErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			genErr = s.GenerateShotVideo(shot, aspectRatio, effectiveProvider)
			if genErr == nil {
				break
			}
			logger.Errorf("SequentialGenerateShots: shot %d attempt %d/%d: %v", shot.ShotNo, attempt, maxRetries, genErr)
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt*2) * time.Second)
			}
		}
		if genErr != nil {
			logger.Errorf("SequentialGenerateShots: shot %d failed after %d attempts: %v", shot.ShotNo, maxRetries, genErr)
			if e := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"status": "failed"}); e != nil {
				logger.Errorf("SequentialGenerateShots: UpdateFields shot %d: %v", shot.ID, e)
			}
			if progressFn != nil {
				progressFn((idx + 1) * 99 / total)
			}
			continue
		}
		logger.Printf("SequentialGenerateShots: shot %d submitted, waiting for completion...", shot.ShotNo)

		// 同步等待完成（最长 10 分钟/镜头）
		// waitForShotCompletion 内部会调用 chainLastFrameToNextShot，
		// 确保下一镜头的 reference_image_url 在提交前已写入 DB。
		finishedShot, waitErr := s.waitForShotCompletion(shot, 10*time.Minute)
		if waitErr != nil {
			logger.Errorf("SequentialGenerateShots: shot %d wait: %v", shot.ShotNo, waitErr)
		} else {
			completed = append(completed, finishedShot)
			logger.Printf("SequentialGenerateShots: shot %d completed, chained to next", shot.ShotNo)
		}
		if progressFn != nil {
			progressFn((idx + 1) * 99 / total)
		}
	}
	logger.Printf("SequentialGenerateShots: videoID=%d done %d/%d shots", videoID, len(completed), total)
	return completed, nil
}

// VoiceFirstGenerateShots 配音优先模式：
//
//	阶段1 - 并发为所有分镜生成 TTS，测量实际配音时长
//	阶段2 - 将各分镜 Duration 更新为配音时长（保证视频不短于配音）
//	阶段3 - 调用 BatchGenerateShots 正常生成视频
//
// 这样视频生成时已知精确目标时长，从根本上消除配音溢出问题。
func (s *VideoService) VoiceFirstGenerateShots(videoID uint, shotIDs []uint, qualityTierOverride string, progressFn func(int), provider ...string) ([]*model.StoryboardShot, error) {
	logger.Printf("[VoiceFirst] videoID=%d shots=%d: Phase1 TTS start", videoID, len(shotIDs))

	// ── Phase 1: 并发 TTS ────────────────────────────────────────────────────
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	allShots, err := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if err != nil {
		return nil, err
	}

	// 确定旁白音色（复用 BatchGenerateShotAudio 的默认逻辑）
	narrationVoice := ""
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, ne := s.novelRepo.GetByID(video.NovelID); ne == nil {
			narrationVoice = novel.VideoConf().NarrationVoice
		}
	}

	var wg sync.WaitGroup
	const ttsConc = 4
	ttssSem := make(chan struct{}, ttsConc)
	for _, shot := range allShots {
		if shot.VideoID != videoID {
			continue
		}
		sh := shot
		wg.Add(1)
		ttssSem <- struct{}{}
		go func() {
			defer func() { <-ttssSem; wg.Done() }()
			if genErr := s.GenerateShotAudio(context.Background(), sh, s.videoTenantID(video), narrationVoice); genErr != nil {
				logger.Errorf("[VoiceFirst] shot %d TTS failed: %v", sh.ShotNo, genErr)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[VoiceFirst] videoID=%d: Phase1 TTS done", videoID)

	// ── Phase 2: 用配音时长更新 shot.Duration ────────────────────────────────
	for _, shot := range allShots {
		if shot.VideoID != videoID || s.segmentRepo == nil {
			continue
		}
		segs, e := s.segmentRepo.ListByShotID(shot.ID)
		if e != nil || len(segs) == 0 {
			continue
		}
		var totalVoice float64
		for _, seg := range segs {
			totalVoice += seg.DurationSecs
		}
		if totalVoice <= 0 {
			continue
		}
		const buffer = 0.3
		target := totalVoice + buffer
		if target > shot.Duration {
			if ue := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"duration": target}); ue != nil {
				logger.Errorf("[VoiceFirst] update shot %d duration: %v", shot.ShotNo, ue)
			} else {
				logger.Printf("[VoiceFirst] shot %d duration %.1fs→%.1fs (voice=%.1fs)", shot.ShotNo, shot.Duration, target, totalVoice)
			}
		}
	}

	// ── Phase 3: 正常批量生成视频 ─────────────────────────────────────────────
	logger.Printf("[VoiceFirst] videoID=%d: Phase3 video generation start", videoID)
	if progressFn != nil {
		progressFn(10) // TTS阶段已完成，标记10%进度
	}
	return s.BatchGenerateShots(videoID, shotIDs, qualityTierOverride, progressFn, provider...)
}
