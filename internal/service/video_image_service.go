package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // PNG 解码支持（合成参考图时可能遇到 PNG 格式）
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
	"github.com/inkframe/inkframe-backend/internal/model"
)

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

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32
	for _, sid := range shotIDs {
		shot, ok := shotMap[sid]
		if !ok || shot.VideoID != videoID {
			if progressFn != nil && total > 0 {
				pct := int(done.Add(1)) * 99 / total
				progressFn(pct)
			}
			continue
		}
		shot.Status = "generating"
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", shot.ShotNo, err)
		}
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				n := int(done.Add(1))
				if progressFn != nil && total > 0 {
					pct := n * 99 / total
					progressFn(pct)
				}
				logger.Printf("BatchGenerateShots: shot %d done (%d/%d)", sh.ShotNo, n, total)
			}()
			logger.Printf("BatchGenerateShots: shot %d start (mode=%s)", sh.ShotNo, mode)
			const maxRetries = 3
			var genErr error
			if video.Mode == "slideshow" || !s.hasVideoProvider(video.TenantID) {
				// ── 两阶段异步模式 ──────────────────────────────────────────────────
				// 阶段一（同步，占用 sem）：AI 图片生成 → 下载到本地
				// 阶段二（异步，释放 sem 后后台执行）：Ken Burns 编码 → OSS 上传，支持自动重试
				// 只生成图片，不自动合成 MP4（Ken Burns 由独立的 batch-clips 步骤触发）
				for attempt := 1; attempt <= maxRetries; attempt++ {
					_, _, genErr = s.generateShotImageOnly(sh, aspectRatio)
					if genErr == nil {
						break
					}
					logger.Printf("BatchGenerateShots: shot %d image attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr == nil {
					if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
						"status": "completed", "progress": 100,
					}); err != nil {
						logger.Printf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", sh.ShotNo, err)
					}
					logger.Printf("BatchGenerateShots: shot %d image ready", sh.ShotNo)
				} else {
					logger.Printf("BatchGenerateShots: shot %d image failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
					_ = s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
						"status": "failed",
					})
				}
			} else {
				// ── AI 视频模式：原有同步逻辑（提交 → provider 轮询）──────────────
				for attempt := 1; attempt <= maxRetries; attempt++ {
					genErr = s.GenerateShotVideo(sh, aspectRatio, effectiveProvider)
					if genErr == nil {
						break
					}
					logger.Printf("BatchGenerateShots: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr != nil {
					logger.Printf("BatchGenerateShots: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
					_ = s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
						"status": "failed",
					})
				} else {
					logger.Printf("BatchGenerateShots: shot %d submitted successfully (taskID=%s)", sh.ShotNo, sh.ShotTaskID)
				}
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShots: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// BatchGenerateShotImages 批量为分镜生成参考图片（幂等：已有 ImageURL 的分镜自动跳过）。
// 只执行阶段一（AI 图片生成），不启动 Ken Burns 编码。
func (s *VideoService) BatchGenerateShotImages(videoID uint, shotIDs []uint, progressFn func(int)) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	aspectRatio := video.AspectRatio
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

	var queued []*model.StoryboardShot
	concurrency := maxConcurrentShots
	if s.aiService != nil {
		if c := s.aiService.ImageConcurrency(); c > 0 && c < concurrency {
			concurrency = c
		}
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32
	var goroutineIdx atomic.Int32

	advanceProgress := func() {
		n := int(done.Add(1))
		if progressFn != nil && total > 0 {
			progressFn(n * 99 / total)
		}
	}

	for _, sid := range shotIDs {
		shot, ok := shotMapImg[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		if shot.ImageURL != "" {
			// Already has image — skip (idempotent).
			advanceProgress()
			continue
		}
		queued = append(queued, shot)
		gIdx := goroutineIdx.Add(1) - 1
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot, idx int32) {
			// 前几个并发 goroutine 错开 800ms 启动，避免 API 侧同时收到多个请求导致质量下降
			if idx > 0 && idx < int32(concurrency) {
				time.Sleep(time.Duration(idx) * 800 * time.Millisecond)
			}
			defer func() {
				<-sem
				wg.Done()
				advanceProgress()
				logger.Printf("BatchGenerateShotImages: shot %d done", sh.ShotNo)
			}()
			const maxRetries = 3
			var localImage string
			var genErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				localImage, _, genErr = s.generateShotImageOnly(sh, aspectRatio)
				if genErr == nil {
					break
				}
				logger.Printf("BatchGenerateShotImages: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt*2) * time.Second)
				}
			}
			if localImage != "" {
				os.Remove(localImage) //nolint:errcheck  // temp file not needed; ImageURL is in DB
			}
			if genErr == nil {
				if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
					"status": "completed", "progress": 50,
				}); err != nil {
					logger.Printf("[VideoService] BatchGenerateShotImages: failed to update shot %d status: %v", sh.ShotNo, err)
				}
				logger.Printf("BatchGenerateShotImages: shot %d image ready", sh.ShotNo)
			} else {
				logger.Printf("BatchGenerateShotImages: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				_ = s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
					"status": "failed",
				})
			}
		}(shot, gIdx)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotImages: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// GetStatus 获取视频生成状态（从 provider 同步最新进度）

// generateShotReferenceImage 为分镜生成参考帧图像，返回图片URL和错误。
// ─── 参考图合成辅助函数 ─────────────────────────────────────────────────────

const (
	maxCompositeImages    = 4   // 最多合成张数（角色最多3张 + 场景1张）
	compositeTargetHeight = 512 // 等高缩放目标高度（px）
)

// compositeRefImages 将多张参考图等高缩放后横向拼接为一张，上传到 OSS（或降级为临时文件），返回 URL。
// 若只有一张图，直接返回原 URL 不做处理。
func (s *VideoService) compositeRefImages(ctx context.Context, imageURLs []string, tenantID uint) (string, error) {
	if len(imageURLs) == 0 {
		return "", fmt.Errorf("compositeRefImages: no images")
	}
	if len(imageURLs) == 1 {
		return imageURLs[0], nil
	}
	if len(imageURLs) > maxCompositeImages {
		imageURLs = imageURLs[:maxCompositeImages]
	}

	type imgEntry struct {
		img image.Image
		url string
	}
	var decoded []imgEntry
	for _, u := range imageURLs {
		localPath, dlErr := downloadToTemp(u, "inkframe-ref-", ".jpg")
		if dlErr != nil {
			logger.Printf("compositeRefImages: download failed (%s): %v", u, dlErr)
			continue
		}
		f, openErr := os.Open(localPath)
		if openErr != nil {
			os.Remove(localPath) //nolint:errcheck
			continue
		}
		img, _, decErr := image.Decode(f)
		f.Close()
		os.Remove(localPath) //nolint:errcheck
		if decErr != nil {
			logger.Printf("compositeRefImages: decode failed (%s): %v", u, decErr)
			continue
		}
		decoded = append(decoded, imgEntry{img: img, url: u})
	}

	if len(decoded) == 0 {
		return "", fmt.Errorf("compositeRefImages: all images failed to load")
	}
	if len(decoded) == 1 {
		return decoded[0].url, nil // 只有一张解码成功，直接复用原 URL
	}

	// 等高缩放到 compositeTargetHeight，按宽高比计算各图缩放后宽度
	const H = compositeTargetHeight
	totalW := 0
	widths := make([]int, len(decoded))
	for i, e := range decoded {
		b := e.img.Bounds()
		if b.Dy() > 0 && b.Dx() > 0 {
			widths[i] = b.Dx() * H / b.Dy()
		}
		if widths[i] < 1 {
			widths[i] = H
		}
		totalW += widths[i]
	}

	// 创建横向拼接画布（白色背景）
	canvas := image.NewRGBA(image.Rect(0, 0, totalW, H))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	x := 0
	for i, e := range decoded {
		dstRect := image.Rect(x, 0, x+widths[i], H)
		refCompositeDrawScaled(canvas, dstRect, e.img)
		x += widths[i]
	}

	// 编码为 JPEG
	var buf bytes.Buffer
	if encErr := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 88}); encErr != nil {
		return "", fmt.Errorf("compositeRefImages: encode: %w", encErr)
	}

	// 上传到 OSS（若配置了 storageSvc）
	if s.storageSvc != nil {
		key := fmt.Sprintf("composites/%d/ref-%d.jpg", tenantID, time.Now().UnixNano())
		ossURL, upErr := s.storageSvc.Upload(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "image/jpeg")
		if upErr == nil {
			return ossURL, nil
		}
		logger.Printf("compositeRefImages: OSS upload failed (falling back to temp file): %v", upErr)
	}

	// 降级：保存为临时文件，返回 file:// URL
	tmp, err := os.CreateTemp("", "inkframe-composite-*.jpg")
	if err != nil {
		return "", fmt.Errorf("compositeRefImages: create temp: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name()) //nolint:errcheck
		return "", fmt.Errorf("compositeRefImages: write temp: %w", err)
	}
	tmp.Close()
	return "file://" + tmp.Name(), nil
}

// refCompositeDrawScaled 最近邻缩放，将 src 绘制到 dst 的 dstRect 区域。
func refCompositeDrawScaled(dst draw.Image, dstRect image.Rectangle, src image.Image) {
	srcBounds := src.Bounds()
	srcW, srcH := srcBounds.Dx(), srcBounds.Dy()
	dstW, dstH := dstRect.Dx(), dstRect.Dy()
	if srcW <= 0 || srcH <= 0 || dstW <= 0 || dstH <= 0 {
		return
	}
	for dy := 0; dy < dstH; dy++ {
		sy := dy*srcH/dstH + srcBounds.Min.Y
		for dx := 0; dx < dstW; dx++ {
			sx := dx*srcW/dstW + srcBounds.Min.X
			dst.Set(dstRect.Min.X+dx, dstRect.Min.Y+dy, src.At(sx, sy))
		}
	}
}

// ─── 分镜参考图生成 ──────────────────────────────────────────────────────────

func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) (string, error) {
	if s.aiService == nil {
		return "", fmt.Errorf("AI service not initialized")
	}

	// 精准匹配：批量加载 shot.CharacterIDs 中的所有角色参考图（最多 maxCompositeImages-1 张，留槽给场景锚点）
	// 参考图优先级：FaceCloseup > Portrait > ThreeViewSheet
	// 理由：DreamO IP-Adapter 对高分辨率面部特写效果最佳（720×1280 face vs 三视图中每视角仅约 427px）。
	// 单角色时同时传 FaceCloseup + ThreeViewSheet，兼顾面部锁定和服装/体型一致性。
	const maxCharRefs = maxCompositeImages - 1
	var characterPortraits []string // 可能包含多个角色的图
	var characterVisualPrompts []string // 角色外观 token 串，用于注入 shot prompt（文本+图像双重约束）
	var refSources []string
	if len(shot.CharacterIDs) > 0 {
		ids := []uint(shot.CharacterIDs)
		batchChars, batchErr := s.characterRepo.ListByIDs(ids)
		if batchErr == nil {
			singleChar := len(batchChars) == 1
			for _, char := range batchChars {
				if len(characterPortraits) >= maxCharRefs {
					break
				}
				remaining := maxCharRefs - len(characterPortraits)
				// 收集外观 token 用于 prompt 注入
				if char.VisualPrompt != "" {
					characterVisualPrompts = append(characterVisualPrompts, char.VisualPrompt)
				}
				// FaceCloseup > Portrait（面部参考）; ThreeViewSheet（服装/体型参考）
				faceRef := char.FaceCloseup
				if faceRef == "" {
					faceRef = char.Portrait
				}
				bodyRef := char.ThreeViewSheet
				if singleChar && remaining >= 2 && faceRef != "" && bodyRef != "" {
					// 单角色且槽位充足：face + three-view 双引用，同时锁定面部特征和服装设计
					characterPortraits = append(characterPortraits, faceRef, bodyRef)
					refSources = append(refSources,
						fmt.Sprintf("charID=%d FaceCloseup/Portrait", char.ID),
						fmt.Sprintf("charID=%d ThreeViewSheet", char.ID))
				} else if faceRef != "" {
					characterPortraits = append(characterPortraits, faceRef)
					refSources = append(refSources, fmt.Sprintf("charID=%d FaceCloseup/Portrait", char.ID))
				} else if bodyRef != "" {
					characterPortraits = append(characterPortraits, bodyRef)
					refSources = append(refSources, fmt.Sprintf("charID=%d ThreeViewSheet(fallback)", char.ID))
				}
			}
		}
	}

	// cachedNovelChars 延迟加载：降级一名称匹配使用
	var cachedNovelChars []*model.Character

	// 降级一：若 CharacterIDs 未命中，从 shot.Characters JSON 内联名称匹配
	// （CharacterIDs 由 autoMatchShotCharacters 在分镜生成时设置，若名称有偏差则可能为空）
	if len(characterPortraits) == 0 && shot.Characters != "" {
		var shotChars []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err == nil && len(shotChars) > 0 {
			if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil && video.NovelID > 0 {
				if cachedNovelChars == nil {
					cachedNovelChars, _ = s.characterRepo.ListByNovel(video.NovelID)
				}
				if len(cachedNovelChars) > 0 {
					nameMap := make(map[string]*model.Character, len(cachedNovelChars))
					for _, c := range cachedNovelChars {
						nameMap[strings.ToLower(c.Name)] = c
					}
					for _, sc := range shotChars {
						if len(characterPortraits) >= maxCharRefs {
							break
						}
						nameLow := strings.ToLower(sc.Name)
						char, ok := nameMap[nameLow]
						if !ok {
							for n, c := range nameMap {
								// 最少2个字的子串匹配，防止单字误命中（如"云"匹配"凌云"导致角色混淆）
								nRunes := []rune(n)
								nmRunes := []rune(nameLow)
								if len(nRunes) >= 2 && len(nmRunes) >= 2 &&
									(strings.Contains(nameLow, n) || strings.Contains(n, nameLow)) {
									char = c
									ok = true
									break
								}
							}
						}
						if ok && char != nil {
							if char.VisualPrompt != "" {
								characterVisualPrompts = append(characterVisualPrompts, char.VisualPrompt)
							}
							inlineFaceRef := char.FaceCloseup
							if inlineFaceRef == "" {
								inlineFaceRef = char.Portrait
							}
							if inlineFaceRef != "" {
								characterPortraits = append(characterPortraits, inlineFaceRef)
								refSources = append(refSources, fmt.Sprintf("inline name=%q FaceCloseup/Portrait", sc.Name))
							} else if char.ThreeViewSheet != "" {
								characterPortraits = append(characterPortraits, char.ThreeViewSheet)
								refSources = append(refSources, fmt.Sprintf("inline name=%q ThreeViewSheet(fallback)", sc.Name))
							}
						}
					}
				}
			}
		}
	}

	// 获取章节序号用于 OSS 路径（不再降级注入第一个角色，避免把错误角色注入非角色镜头）
	var chapterNo int
	if shot.ChapterID != nil && s.chapterRepo != nil {
		if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
			chapterNo = chapter.ChapterNo
		}
	}
	logger.Printf("generateShotReferenceImage: shot %d charIDs=%v sources=%v portraits=%d",
		shot.ShotNo, shot.CharacterIDs, refSources, len(characterPortraits))
	if len(shot.CharacterIDs) > 0 && len(characterPortraits) == 0 {
		logger.Printf("[WARN] generateShotReferenceImage: shot %d has CharacterIDs=%v but no portrait/ThreeViewSheet found — characters may not have images generated yet", shot.ShotNo, shot.CharacterIDs)
	}

	promptText := shot.Prompt
	if promptText == "" {
		promptText = shot.Description
	}

	// 角色外观 token 前置注入：与参考图形成文本+图像双重约束，大幅提升角色一致性。
	// VisualPrompt 由 AI 生成，格式为 "1girl, long silver hair, blue eyes, white hanfu, ..."，
	// 置于 prompt 最前端能获得最高权重，精确锁定面部、发型、服装特征。
	// 无论是否找到参考图均注入：即使角色尚未生成图片，文字锚点也能约束 Text2ImgV3 维持外貌一致性；
	// 只要找到了匹配的角色记录（characterVisualPrompts 非空），都应注入。
	if len(characterVisualPrompts) > 0 {
		promptText = strings.Join(characterVisualPrompts, ", ") + ", " + promptText
	}

	// 场景锚点：注入锁定词，并收集场景参考图
	var sceneRefImage string
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, refURL, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil {
			if fragment != "" {
				promptText = fragment + ", " + promptText
			}
			sceneRefImage = refURL
		}
	}

	// 合并参考图 URL：角色图优先，场景锚点图仅在有角色图时追加。
	//
	// 关键约束：selectImageModel 依赖 firstRef（第一张图）决定是否启用 DreamO（角色特征保持）。
	// 若无角色参考图但场景锚点图非空，firstRef 将是场景背景图 → DreamO 错误地将背景作为"角色外观"
	// 进行特征保持，导致生成图角色面目全非。
	// 解决方案：无角色图时不把场景图加入 allRefImages，让模型回退到 Text2ImgV3（纯文生图）；
	// 场景锚点的文字描述已通过 promptText 注入，仍能保障画面主题一致性。
	allRefImages := make([]string, 0, len(characterPortraits)+1)
	allRefImages = append(allRefImages, characterPortraits...)
	if sceneRefImage != "" && len(characterPortraits) > 0 {
		// 仅在有角色图时追加场景图（作为 DreamO 的补充上下文，而非主参考）
		allRefImages = append(allRefImages, sceneRefImage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 获取视频的 ArtStyle、TenantID、质量档位、宽高比、角色一致性权重和色彩调色
	artStyle := ""
	var tenantID uint
	charConsistencyWeight := 1.0 // 默认严格一致
	qualityTier := "preview"     // 默认质量档位
	var imageAspectRatio, colorGrade string
	if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
		artStyle = video.ArtStyle
		tenantID = video.TenantID
		imageAspectRatio = video.AspectRatio
		if video.QualityTier != "" {
			qualityTier = video.QualityTier
		}
		if video.NovelID > 0 && s.novelRepo != nil {
			if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
				if tenantID == 0 {
					tenantID = novel.TenantID
				}
				vc := novel.VideoConf()
				if vc.CharConsistencyWeight > 0 {
					charConsistencyWeight = vc.CharConsistencyWeight
				}
				// 降级：视频未设置画面风格/宽高比时使用项目设置
				if artStyle == "" && novel.ImageStyle != "" {
					artStyle = novel.ImageStyle
				}
				if imageAspectRatio == "" && vc.VideoAspectRatio != "" {
					imageAspectRatio = vc.VideoAspectRatio
				}
				colorGrade = vc.ColorGrade
				// 注入 OSS 路径提示（项目名+章节序号）
				if novel.Title != "" {
					ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title, ChapterNo: chapterNo})
				}
			}
		}
	}

	// allRefImages 直接传给 API，无需合图（所有图生图 API 均支持多张参考图）

	// 根据宽高比+质量档位计算实际图片尺寸（WxH），直接传给 API
	imageSize := imageAspectRatioToSize(imageAspectRatio, qualityTier)
	logger.Printf("generateShotReferenceImage: shot %d qualityTier=%s aspectRatio=%s imageSize=%s", shot.ShotNo, qualityTier, imageAspectRatio, imageSize)

	// 构建负向提示词：基础解剖/物理规律排除词 + 分镜 LLM 生成的镜头专项排除词
	// 图像生成必须有负向提示词，否则极易出现变形肢体、违反物理规律、比例失调等问题
	imgNegBase := "worst quality, low quality, jpeg artifacts, noise, blurry, " +
		"deformed, ugly, bad anatomy, extra limbs, missing limbs, floating limbs, disconnected limbs, " +
		"malformed hands, missing fingers, fused fingers, extra fingers, poorly drawn hands, extra arms, extra legs, " +
		"bad proportions, gross proportions, long neck, cloned face, " +
		"out of frame, cropped head, poorly drawn face, poorly drawn eyes, asymmetric eyes, " +
		"text, watermark, logo, signature, " +
		"impossible physics, floating objects, gravity defying, " +
		"oversaturated, overexposed, underexposed"
	negPrompt := imgNegBase
	if shot.NegativePrompt != "" {
		negPrompt = imgNegBase + ", " + shot.NegativePrompt
	}

	// Prompt 前缀策略：
	// - shot.Prompt（LLM 生成的 image_prompt）已包含画风/画质词/镜头参数，只补充项目级调色和风格词，
	//   避免重复注入镜头参数（如 35mm vs 85mm）产生冲突，导致画面比例/构图异常。
	// - shot.Prompt 为空时（降级用 description），注入完整电影级前缀补足画质词和镜头描述。
	lensTypeMap := map[string]string{
		"extreme_close_up": "macro lens 100mm, extreme shallow DOF, bokeh",
		"close_up":         "portrait lens 85mm, shallow depth of field, subject isolation",
		"medium":           "standard lens 50mm, natural perspective",
		"wide":             "wide angle lens 24mm, deep focus, environmental context",
		"extreme_wide":     "ultra wide lens 16mm, expansive environment, dramatic perspective",
	}
	lensType := lensTypeMap[shot.ShotSize]
	if lensType == "" {
		lensType = "standard lens 50mm"
	}

	if shot.Prompt != "" {
		// LLM 生成的 image_prompt 已完整，只在最前端注入项目级画面风格和色调。
		// 画面风格无条件注入（不做 Contains 检查）：LLM 嵌入的风格词可能与项目当前设置不一致
		// （如分镜生成时为"anime"但现在改为"古风仙侠"），以项目设置为最终权威，置于 prompt 首位。
		var prefix string
		if artStyle != "" {
			prefix += artStyle + " style, "
		}
		if kw := colorGradeToPromptKeyword(colorGrade); kw != "" {
			prefix += kw + ", "
		}
		if prefix != "" {
			promptText = prefix + promptText
		}
	} else {
		// 降级：description 无画质词，注入完整电影级前缀
		cinematicImgPrefix := "cinematic film photography, 35mm anamorphic lens, professional lighting setup, " + lensType + ", "
		if kw := colorGradeToPromptKeyword(colorGrade); kw != "" {
			cinematicImgPrefix = kw + ", " + cinematicImgPrefix
		}
		if artStyle != "" {
			cinematicImgPrefix = artStyle + " style, " + cinematicImgPrefix
		}
		promptText = cinematicImgPrefix + promptText
	}

	// 画质词兜底：旧格式分镜（storyboard.j2）或 description 降级路径不含画质词，
	// 此处统一补齐，确保新旧分镜生成图片质量基准一致。
	// 画质词按风格分三类：写实摄影、插画/漫画/动漫、古风/仙侠/水墨（默认归为插画类）
	if !strings.Contains(strings.ToLower(promptText), "masterpiece") {
		artLow := strings.ToLower(artStyle)
		isRealistic := strings.Contains(artLow, "realistic") || strings.Contains(artLow, "写实") ||
			strings.Contains(artLow, "photorealistic") || strings.Contains(artLow, "photography")
		isIllustration := strings.Contains(artLow, "anime") || strings.Contains(artLow, "cartoon") ||
			strings.Contains(artLow, "illustration") || strings.Contains(artLow, "动漫") ||
			strings.Contains(artLow, "动画") || strings.Contains(artLow, "漫画") ||
			strings.Contains(artLow, "古风") || strings.Contains(artLow, "仙侠") ||
			strings.Contains(artLow, "水墨") || strings.Contains(artLow, "xianxia") ||
			strings.Contains(artLow, "wuxia") || strings.Contains(artLow, "ink painting")
		if isRealistic {
			promptText += ", masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting"
		} else if isIllustration {
			promptText += ", masterpiece, best quality, ultra-detailed, vibrant colors, clean linework, professional illustration"
		} else {
			// 未知风格默认使用插画类画质词（更通用，适配大多数非写实风格）
			promptText += ", masterpiece, best quality, ultra-detailed, vibrant colors, professional digital art"
		}
	}

	// 二次读取场景锚点参考图（仅在有角色参考图时才追加）：
	// 批量并发时本 goroutine 等待 imageSem 期间，前一个分镜可能已完成并锁定了锚点。
	// 同样遵守"无角色图不追加场景图"的约束，防止 DreamO 误将场景图视为角色外观。
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil && len(characterPortraits) > 0 {
		if _, latestRef, _ := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); latestRef != "" {
			alreadyHaveRef := false
			for _, r := range allRefImages {
				if r == latestRef {
					alreadyHaveRef = true
					break
				}
			}
			if !alreadyHaveRef {
				allRefImages = append(allRefImages, latestRef)
				logger.Printf("generateShotReferenceImage: shot %d late-read scene anchor ref (locked by earlier shot in batch)", shot.ShotNo)
			}
		}
	}

	logger.Printf("generateShotReferenceImage: shot %d prompt=%q negPrompt=%q", shot.ShotNo, promptText[:min(len(promptText), 120)], negPrompt[:min(len(negPrompt), 80)])
	imageURL, err := s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, "", promptText, allRefImages, artStyle, negPrompt, imageSize, charConsistencyWeight)
	if err != nil {
		logger.Printf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return "", err
	}
	if imageURL == "" {
		logger.Printf("generateShotReferenceImage: image gen returned empty URL for shot %d", shot.ShotNo)
		return "", fmt.Errorf("image provider returned empty URL")
	}

	// 首图锁定：场景锚点无参考图时，将本次生成结果存为参考图
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if err := s.sceneAnchorSvc.AutoSetRefImage(*shot.SceneAnchorID, imageURL); err != nil {
			logger.Printf("[VideoService] AutoSetRefImage: %v", err)
		}
	}

	return imageURL, nil
}


// 成功后自动更新 DB 中的 ImageURL 并返回新 URL。
func (s *VideoService) RefineShotImage(shotID uint, suggestion string) (string, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return "", fmt.Errorf("shot %d not found: %w", shotID, err)
	}

	// 构建含修改建议的提示词（操作副本，不改 DB 原始字段）
	shotCopy := *shot
	basePrompt := shot.Prompt
	if basePrompt == "" {
		basePrompt = shot.Description
	}
	if suggestion != "" {
		shotCopy.Prompt = basePrompt + ". Modification: " + suggestion
	} else {
		shotCopy.Prompt = basePrompt
	}

	newURL, err := s.generateShotReferenceImage(&shotCopy)
	if err != nil {
		return "", fmt.Errorf("refine image for shot %d: %w", shotID, err)
	}

	// 持久化新图片 URL
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"image_url": newURL}); err != nil {
		logger.Printf("[VideoService] RefineShot: failed to update shot %d image URL: %v", shotID, err)
	}
	return newURL, nil
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

// emotionToKlingParams 根据情绪/摄像机类型映射最优的 Kling 生成参数。
// 动作/史诗场景使用 pro 模式 + 10 秒时长，获得更高画质；
// 风景/全景使用高 CFG + 10 秒；对话/温情使用 5 秒防止内容填充。
func emotionToKlingParams(emotion, cameraType string) (mode string, cfgScale float64, duration float64) {
	// 将情绪标签规范化到英文
	e := strings.ToLower(emotion)
	ct := strings.ToLower(cameraType)

	switch {
	case strings.Contains(e, "battle") || strings.Contains(e, "combat") ||
		strings.Contains(e, "战斗") || strings.Contains(e, "打斗") ||
		strings.Contains(e, "action") || strings.Contains(e, "fight"):
		return "pro", 0.45, 10

	case strings.Contains(e, "epic") || strings.Contains(e, "史诗") ||
		strings.Contains(e, "宏大") || strings.Contains(e, "壮观") ||
		strings.Contains(e, "climax") || strings.Contains(e, "高潮"):
		return "pro", 0.5, 10

	case strings.Contains(e, "dramatic") || strings.Contains(e, "紧张") ||
		strings.Contains(e, "suspense") || strings.Contains(e, "danger") ||
		strings.Contains(e, "危险") || strings.Contains(e, "恐惧"):
		return "std", 0.7, 5

	case strings.Contains(e, "landscape") || strings.Contains(e, "scenery") ||
		strings.Contains(e, "风景") || strings.Contains(e, "空镜") ||
		ct == "crane" || (ct == "pan" && strings.Contains(e, "wide")):
		return "std", 0.8, 10

	case strings.Contains(e, "romantic") || strings.Contains(e, "浪漫") ||
		strings.Contains(e, "tender") || strings.Contains(e, "温情"):
		return "std", 0.6, 5

	case strings.Contains(e, "sad") || strings.Contains(e, "悲") ||
		strings.Contains(e, "离别") || strings.Contains(e, "grief"):
		return "std", 0.65, 5

	default:
		return "std", 0.5, 5
	}
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

	preferredProvider := "kling"
	if len(providerOverride) > 0 && providerOverride[0] != "" {
		preferredProvider = providerOverride[0]
	}
	// Determine tenantID from associated video for DB provider lookup
	var tenantID uint
	if video, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil {
		tenantID = video.TenantID
	}
	provider, providerName, provErr := s.resolveVideoProvider(tenantID, preferredProvider)
	if provErr != nil {
		return fmt.Errorf("no video provider configured")
	}

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}

	logger.Printf("GenerateShotVideo: shot %d provider=%s aspect=%s duration=%ds", shot.ShotNo, providerName, videoAspectRatio, shot.Duration)

	// 图片优先策略：先确保 shot.ImageURL 已有图片，再用其作为视频参考图（image-to-video）。
	// 若 ImageURL 已存在则直接复用，否则先生成并持久化，保证前端可见且视频有参考帧。
	referenceImage := shot.ReferenceImageURL
	if shot.ImageURL != "" {
		// 已有正式镜头图，直接复用，无需再次生成
		referenceImage = shot.ImageURL
		shot.FrameImageURL = shot.ImageURL
		logger.Printf("GenerateShotVideo: shot %d reusing existing ImageURL as reference: %s", shot.ShotNo, shot.ImageURL)
	} else {
		// 先生成图片：使用 shot.Prompt（image_prompt）+ 角色参考图 → 完整场景图
		logger.Printf("GenerateShotVideo: shot %d ImageURL empty, generating image first (charIDs=%v)", shot.ShotNo, shot.CharacterIDs)
		frameURL, frameErr := s.generateShotReferenceImage(shot)
		if frameErr != nil {
			logger.Printf("GenerateShotVideo: shot %d image generation failed: %v", shot.ShotNo, frameErr)
		} else {
			logger.Printf("GenerateShotVideo: shot %d image generated: %s", shot.ShotNo, frameURL)
		}
		if frameURL == "" {
			errMsg := "image generation failed: empty URL returned"
			if frameErr != nil {
				errMsg = "image generation failed: " + frameErr.Error()
			}
			_ = s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{
				"status":        "failed",
				"error_message": errMsg,
			})
			if frameErr != nil {
				return frameErr
			}
			return fmt.Errorf("shot %d: %s", shot.ShotNo, errMsg)
		}
		shot.ImageURL = frameURL
		shot.FrameImageURL = frameURL
		referenceImage = frameURL
		// 立即持久化图片 URL，确保视频生成失败时图片不丢失
		if updateErr := s.storyboardRepo.Update(shot); updateErr != nil {
			logger.Printf("GenerateShotVideo: shot %d failed to persist ImageURL: %v", shot.ShotNo, updateErr)
		}
	}

	// 场景锚点：将锁定词注入视频生成 prompt
	// 优先使用运镜提示词（MotionPrompt），若为空则降级到静态画面描述（Prompt）
	videoPrompt := shot.MotionPrompt
	if videoPrompt == "" {
		videoPrompt = shot.Prompt
	}
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, _, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && fragment != "" {
			videoPrompt = fragment + ", " + videoPrompt
		}
	}

	// 画面风格：注入视频 prompt（video.ArtStyle 优先，降级到 novel.ImageStyle）
	if videoArtStyle := s.resolveArtStyle(shot.VideoID); videoArtStyle != "" {
		videoPrompt = videoArtStyle + " style, " + videoPrompt
	}

	// TTS 对齐：若分镜有配音，确保视频时长不短于音频时长+缓冲。
	// alignShotDurationToTTS 仅返回调整值，不持久化到 DB。
	shotDuration := alignShotDurationToTTS(shot)

	// 动态 Kling 参数（根据情绪和摄像机类型选择最优配置）
	klingMode, klingCFG, klingDefaultDur := emotionToKlingParams(shot.EmotionalTone, shot.CameraType)
	if shotDuration <= 0 {
		shotDuration = klingDefaultDur
	}

	// 检查项目配置：KlingProForAction、HD、3D、色彩调色
	var hdEnabled, threeDEnabled bool
	var threeDStyle, klingModelOverride, videoColorGrade string
	if vid, vidErr := s.videoRepo.GetByID(shot.VideoID); vidErr == nil && vid.NovelID > 0 && s.novelRepo != nil {
		if novel, novelErr := s.novelRepo.GetByID(vid.NovelID); novelErr == nil {
			vc := novel.VideoConf()
			if klingMode == "pro" && !vc.KlingProForAction {
				klingMode = "std"
			}
			hdEnabled = vc.HDEnabled || strings.Contains(vid.VisualMode, "hd")
			threeDEnabled = vc.ThreeDEnabled || strings.Contains(vid.VisualMode, "3d")
			threeDStyle = vid.ThreeDStyle
			klingModelOverride = vc.KlingModel
			videoColorGrade = vc.ColorGrade
		}
	}
	if threeDStyle == "" {
		threeDStyle = "cg"
	}
	// HD 模式：升级为更高清的模型并强制 pro
	if hdEnabled {
		if klingModelOverride == "" || klingModelOverride == "kling-v1" {
			klingModelOverride = "kling-v1-6"
		}
		klingMode = "pro"
	}

	// 电影级动态前缀——注入运镜词+情绪氛围词，移除 "film still" 静态词避免抑制视频动态感
	cinematicPrefix := buildCinematicPrefix(shot.CameraType, shot.EmotionalTone)
	// 3D 风格前缀
	if threeDEnabled {
		cinematicPrefix = resolve3DStylePrefix(threeDStyle) + ", " + cinematicPrefix
	}
	// 视频生成专属负向词：补充 static/still/frozen/slideshow 防止模型生成静止画面
	negativeBase := "blurry, low quality, watermark, text overlay, deformed, ugly, " +
		"bad anatomy, duplicate, morbid, mutilated, out of frame, extra limbs, " +
		"gross proportions, malformed limbs, " +
		"static image, still frame, frozen, no motion, slideshow, photo, " +
		"flickering, temporal inconsistency, abrupt scene change, jump cut"

	videoPromptFinal := cinematicPrefix + videoPrompt
	// 注入色彩调色关键词（项目设置 → 视频 prompt）
	if kw := colorGradeToPromptKeyword(videoColorGrade); kw != "" {
		videoPromptFinal = kw + ", " + videoPromptFinal
	}
	negativePrompt := negativeBase
	if shot.NegativePrompt != "" {
		negativePrompt = negativeBase + ", " + shot.NegativePrompt
	}

	// Seedance / Kling 均支持多张参考图：在主参考图（scene image）之外追加角色三视图和场景锚点图，
	// 提升角色一致性和场景一致性。
	var extraRefImages []string
	multiImageProviders := map[string]bool{"seedance": true, "kling": true}
	if multiImageProviders[providerName] && s.characterRepo != nil && len(shot.CharacterIDs) > 0 {
		if chars, charErr := s.characterRepo.ListByIDs([]uint(shot.CharacterIDs)); charErr == nil {
			for _, c := range chars {
				// FaceCloseup > Portrait > ThreeViewSheet — 与 generateShotReferenceImage 保持一致
				var img string
				if c.FaceCloseup != "" {
					img = c.FaceCloseup
				} else if c.Portrait != "" {
					img = c.Portrait
				} else if c.ThreeViewSheet != "" {
					img = c.ThreeViewSheet
				}
				if img != "" && img != referenceImage {
					extraRefImages = append(extraRefImages, img)
				}
			}
		}
	}
	if multiImageProviders[providerName] && s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if _, anchorRefURL, anchorErr := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); anchorErr == nil && anchorRefURL != "" && anchorRefURL != referenceImage {
			extraRefImages = append(extraRefImages, anchorRefURL)
		}
	}

	req := &ai.VideoGenerateRequest{
		Prompt:         videoPromptFinal,
		NegativePrompt: negativePrompt,
		Duration:       shotDuration,
		AspectRatio:    videoAspectRatio,
		ImageURL:       referenceImage, // 主参考图（生成的场景图），image-to-video；空时退化为 text-to-video
		ImageURLs:      extraRefImages, // 额外参考图（Seedance 多图支持）
		CFGScale:       klingCFG,
		Mode:           klingMode,
		Model:          klingModelOverride,
	}

	logger.Printf("GenerateShotVideo: shot %d submitting to %s (hasRef=%v extraRefs=%d mode=%s cfg=%.2f prompt=%q)", shot.ShotNo, providerName, referenceImage != "", len(extraRefImages), klingMode, klingCFG, videoPromptFinal)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		logger.Printf("GenerateShotVideo: shot %d submit failed: %v", shot.ShotNo, err)
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	logger.Printf("GenerateShotVideo: shot %d submitted taskID=%s", shot.ShotNo, task.TaskID)
	shot.ShotTaskID = task.TaskID
	shot.ShotProviderName = providerName
	shot.Status = "processing"
	return s.storyboardRepo.Update(shot)
}

// buildCinematicPrefix 根据摄像机类型和情绪生成动态电影级 prompt 前缀。
// 刻意移除了 "film still"（静帧含义），改用 "cinematic sequence" 强化动态感。
func buildCinematicPrefix(cameraType, emotionalTone string) string {
	motion := cameraMotionToken(cameraType)
	atmos := emotionAtmosphereToken(emotionalTone)
	base := "cinematic sequence, professional cinematography, anamorphic lens, natural film grain, high dynamic range"
	if motion != "" {
		base = motion + ", " + base
	}
	if atmos != "" {
		base += ", " + atmos
	}
	return base + ", "
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

// emotionAtmosphereToken 把情绪基调映射为氛围关键词，注入 prompt 以影响画面色调与动态能量。
func emotionAtmosphereToken(emotion string) string {
	e := strings.ToLower(emotion)
	switch {
	case strings.Contains(e, "battle") || strings.Contains(e, "combat") ||
		strings.Contains(e, "战斗") || strings.Contains(e, "打斗") || strings.Contains(e, "action"):
		return "intense action atmosphere, dynamic motion blur, adrenaline energy"
	case strings.Contains(e, "epic") || strings.Contains(e, "史诗") ||
		strings.Contains(e, "宏大") || strings.Contains(e, "climax") || strings.Contains(e, "高潮"):
		return "epic grand atmosphere, sweeping cinematic motion, heroic scale"
	case strings.Contains(e, "dramatic") || strings.Contains(e, "紧张") ||
		strings.Contains(e, "suspense") || strings.Contains(e, "danger") || strings.Contains(e, "tension"):
		return "dramatic tense atmosphere, deep shadows, ominous mood"
	case strings.Contains(e, "romantic") || strings.Contains(e, "浪漫") ||
		strings.Contains(e, "tender") || strings.Contains(e, "温情"):
		return "soft romantic atmosphere, warm golden bokeh, intimate mood"
	case strings.Contains(e, "sad") || strings.Contains(e, "悲") ||
		strings.Contains(e, "grief") || strings.Contains(e, "离别") || strings.Contains(e, "melancholy"):
		return "melancholic somber atmosphere, cool desaturated tones, slow motion feel"
	case strings.Contains(e, "landscape") || strings.Contains(e, "风景") ||
		strings.Contains(e, "scenery") || strings.Contains(e, "空镜"):
		return "breathtaking scenic vista, sweeping majestic atmosphere"
	case strings.Contains(e, "peaceful") || strings.Contains(e, "平静") || strings.Contains(e, "calm"):
		return "serene tranquil atmosphere, soft diffused light, gentle motion"
	case strings.Contains(e, "funny") || strings.Contains(e, "humorous") || strings.Contains(e, "幽默"):
		return "lively energetic atmosphere, bright warm tones"
	default:
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
		logger.Printf("generateStillFrameClip: failed after %.1fs: %v\noutput: %s", time.Since(encStart).Seconds(), err, string(out))
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
	switch shot.CameraType {
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
	// 视频时长不能低于音频时长
	if shot.AudioPath != "" {
		if data, readErr := readLocalOrRemoteFile(shot.AudioPath); readErr == nil && len(data) > 0 {
			ext := audioExtension(shot.AudioPath)
			if micros := parseAudioDurationMicros(data, ext); micros > 0 {
				if audioDur := float64(micros) / 1_000_000; audioDur > duration {
					logger.Printf("generateShotImageOnly: shot %d extending duration %.2f→%.2fs to cover audio", shot.ShotNo, duration, audioDur)
					duration = audioDur
					shot.Duration = audioDur
				}
			}
		}
	}
	shot.GenerationMode = "static"
	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return "", 0, fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return "", 0, fmt.Errorf("image generation failed for shot %d (empty URL)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// Async scene consistency scoring: compare generated image vs scene anchor reference image.
	if s.sceneConsistencySvc != nil && s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		go func(sh *model.StoryboardShot, imgURL string) {
			anchor, err := s.sceneAnchorSvc.Get(*sh.SceneAnchorID)
			if err == nil {
				if report, err := s.sceneConsistencySvc.ScoreScene(sh, anchor, imgURL, 1); err != nil {
					logger.Printf("[VideoService] ScoreScene shot %d: %v", sh.ShotNo, err)
				} else {
					logger.Printf("[VideoService] ScoreScene shot %d: overall=%.2f passed=%v", sh.ShotNo, report.OverallScore, report.Passed)
				}
			}
		}(shot, imageURL)
	}

	localImage, err = downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
	if err != nil {
		return "", 0, fmt.Errorf("download image for shot %d: %w", shot.ShotNo, err)
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
		logger.Printf("generateClipAndUploadWithRetry: shot %d not found: %v", shotID, err)
		return
	}

	var clipPath string
	var lastErr error

	for attempt := 1; attempt <= maxClipRetries; attempt++ {
		// 优先纯 Go Ken Burns；失败时降级为静止画面
		clipPath, lastErr = s.generateKenBurnsPureGo(ctx, shot, localImage, duration, aspectRatio)
		if lastErr != nil {
			logger.Printf("generateClipAndUploadWithRetry: shot %d ken burns attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
			clipPath, lastErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
		}
		if lastErr == nil {
			break
		}
		logger.Printf("generateClipAndUploadWithRetry: shot %d still frame attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
		if attempt < maxClipRetries {
			select {
			case <-time.After(time.Duration(attempt*5) * time.Second):
			case <-ctx.Done():
				logger.Printf("[VideoService] generateClipAndUploadWithRetry: context cancelled for shot %d, stopping retries", shotID)
				return
			}
		}
	}

	fields := map[string]interface{}{"progress": 100}
	if lastErr != nil {
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip failed after %d attempts, keeping image-only: %v", shot.ShotNo, maxClipRetries, lastErr)
	} else if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		fields["video_url"] = ossURL
		fields["clip_path"] = ""
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip → %s", shot.ShotNo, ossURL)
	} else {
		fields["clip_path"] = "file://" + clipPath
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip done (local only)", shot.ShotNo)
	}
	if err := s.storyboardRepo.UpdateFields(shotID, fields); err != nil {
		logger.Printf("[VideoService] generateClipAndUploadWithRetry: failed to update shot %d fields: %v", shotID, err)
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

	// 视频时长不能低于音频时长：读取已生成的 TTS 音频，若音频更长则扩展 duration。
	if shot.AudioPath != "" {
		if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
			ext := audioExtension(shot.AudioPath)
			if micros := parseAudioDurationMicros(data, ext); micros > 0 {
				audioDur := float64(micros) / 1_000_000
				if audioDur > duration {
					logger.Printf("GenerateSlideshowShotVideo: shot %d extending duration %.2f→%.2fs to cover audio",
						shot.ShotNo, duration, audioDur)
					duration = audioDur
					shot.Duration = audioDur
				}
			}
		}
	}

	logger.Printf("GenerateSlideshowShotVideo: shot %d aspect=%s duration=%.1fs", shot.ShotNo, aspectRatio, duration)

	shot.GenerationMode = "static"
	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	// 1. 生成图片
	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		logger.Printf("GenerateSlideshowShotVideo: image gen failed for shot %d: %s", shot.ShotNo, errMsg)
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return fmt.Errorf("image generation failed for shot %d (empty URL returned)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	logger.Printf("GenerateSlideshowShotVideo: shot %d storing image_url=%q (len=%d)", shot.ShotNo, imageURL, len(imageURL))
	// 保存图片 URL（后续步骤失败时图片仍可用）
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// 图片生成完成，不自动合成 MP4（Ken Burns 由独立的 batch-clips 步骤触发）
	shot.Status = "completed"
	shot.Progress = 100
	return s.storyboardRepo.Update(shot)
}

// uploadClipToStorage 将本地 MP4 文件上传到持久存储（OSS），返回持久 URL。
// storageSvc 为 nil 或上传失败时返回 ""（调用方保留 file:// 本地路径）。
// OSS key 格式：novels/{title}/chapters/{no}/videos/{uuid}.mp4
//
//	章节 ID 未知时降级：videos/{uuid}.mp4

// runSlideshowPipeline 异步处理图片解说模式的所有分镜，完成后自动拼接
func (s *VideoService) runSlideshowPipeline(videoID uint) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		logger.Printf("runSlideshowPipeline: get video %d failed: %v", videoID, err)
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
		narrationVoice = vc.NarrationVoice
	}

	var audioWg sync.WaitGroup
	for _, shot := range shots {
		if err := s.GenerateSlideshowShotVideo(shot, video.AspectRatio); err != nil {
			logger.Printf("runSlideshowPipeline: shot %d failed: %v", shot.ShotNo, err)
		}
		audioWg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer audioWg.Done()
			if err := s.GenerateShotAudio(sh, video.TenantID, narrationVoice); err != nil {
				logger.Printf("runSlideshowPipeline: audio gen failed for shot %d: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	audioWg.Wait()
	// 图片生成完成后不自动拼接；拼接由独立步骤（先生成 Ken Burns 片段，再 StitchVideo）触发
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
		if err := s.videoRepo.Update(video); err != nil {
			logger.Printf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
		}
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
	if err := s.videoRepo.Update(video); err != nil {
		logger.Printf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
	}

	// 从小说视频配置读取旁白音色
	narrationVoice := ""
	if vc := s.GetNovelVideoConfig(video.NovelID); vc != nil {
		narrationVoice = vc.NarrationVoice
	}

	for _, shot := range shots {
		if err := s.GenerateShotVideo(shot, video.AspectRatio); err != nil {
			logger.Printf("GenerateAllShotVideos: shot %d failed: %v", shot.ShotNo, err)
			continue
		}
		// TTS audio in parallel
		go s.GenerateShotAudio(shot, video.TenantID, narrationVoice) //nolint:errcheck
	}
	return nil
}
