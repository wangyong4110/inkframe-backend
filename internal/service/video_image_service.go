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
			logger.Errorf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", shot.ShotNo, err)
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
			hasProvider := s.hasVideoProvider(s.videoTenantID(video))
			logger.Printf("BatchGenerateShots: shot %d start mode=%q hasVideoProvider=%v effectiveProvider=%q", sh.ShotNo, mode, hasProvider, effectiveProvider)
			const maxRetries = 3
			var genErr error
			if !hasProvider {
				// ── 两阶段异步模式 ──────────────────────────────────────────────────
				// 阶段一（同步，占用 sem）：AI 图片生成 → 下载到本地
				// 阶段二（异步，释放 sem 后后台执行）：Ken Burns 编码 → OSS 上传，支持自动重试
				// 只生成图片，不自动合成 MP4（Ken Burns 由独立的 batch-clips 步骤触发）
				for attempt := 1; attempt <= maxRetries; attempt++ {
					_, _, genErr = s.generateShotImageOnly(sh, aspectRatio)
					if genErr == nil {
						break
					}
					logger.Errorf("BatchGenerateShots: shot %d image attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr == nil {
					if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
						"status": "completed", "progress": 100,
					}); err != nil {
						logger.Errorf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", sh.ShotNo, err)
					}
					logger.Printf("BatchGenerateShots: shot %d image ready", sh.ShotNo)
				} else {
					logger.Errorf("BatchGenerateShots: shot %d image failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
					if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "failed"}); e != nil {
						logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", sh.ID, e)
					}
				}
			} else {
				// ── AI 视频模式：原有同步逻辑（提交 → provider 轮询）──────────────
				for attempt := 1; attempt <= maxRetries; attempt++ {
					genErr = s.GenerateShotVideo(sh, aspectRatio, effectiveProvider)
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
			}
		}(shot)
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
		if shot.Status == "generating" {
			// Currently generating in another goroutine — skip.
			advanceProgress()
			continue
		}
		if shot.ImageURL != "" && shot.Status != "failed" && !force {
			// Already has a successfully generated image — skip (idempotent).
			// "failed" shots keep their ImageURL from a partial run but should be retried.
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
			metrics.ShotImageGenerationInFlight.Inc()
			defer func() {
				metrics.ShotImageGenerationInFlight.Dec()
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
				logger.Errorf("BatchGenerateShotImages: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt*2) * time.Second)
				}
			}
			if localImage != "" {
				os.Remove(localImage) //nolint:errcheck  // temp file not needed; ImageURL is in DB
			}
			if genErr == nil {
				metrics.ShotImageGenerationTotal.WithLabelValues("success").Inc()
				if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
					"status": "completed", "progress": 50,
				}); err != nil {
					logger.Errorf("[VideoService] BatchGenerateShotImages: failed to update shot %d status: %v", sh.ShotNo, err)
				}
				logger.Printf("BatchGenerateShotImages: shot %d image ready", sh.ShotNo)
			} else {
				metrics.ShotImageGenerationTotal.WithLabelValues("error").Inc()
				logger.Errorf("BatchGenerateShotImages: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				if e := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{"status": "failed"}); e != nil {
					logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d status=failed: %v", sh.ID, e)
				}
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
			logger.Errorf("compositeRefImages: download failed (%s): %v", u, dlErr)
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
			logger.Errorf("compositeRefImages: decode failed (%s): %v", u, decErr)
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
		key := fmt.Sprintf("images/%s.jpg", uuid.New().String())
		ossURL, upErr := s.storageSvc.Upload(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "image/jpeg")
		if upErr == nil {
			return ossURL, nil
		}
		logger.Errorf("compositeRefImages: OSS upload failed (falling back to temp file): %v", upErr)
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

// getCharActiveLook 返回角色在指定章节的激活形象。
// 优先按章节范围匹配；无匹配时回退到默认形象（DefaultLookID）；最终兜底取第一个有图的形象。
func (s *VideoService) getCharActiveLook(char *model.Character, chapterNo int) *model.CharacterLook {
	if s.lookRepo == nil {
		return nil
	}
	look, _ := s.lookRepo.GetActiveLook(char.ID, chapterNo)
	if look != nil {
		return look
	}
	// 无章节范围匹配（含 chapterNo=0 的降级场景）：回退到角色默认形象
	if char.DefaultLookID != 0 {
		if defaultLook, err := s.lookRepo.GetByID(char.DefaultLookID); err == nil && defaultLook != nil {
			logger.Printf("[getCharActiveLook] charID=%d chapterNo=%d: no range match, using DefaultLookID=%d", char.ID, chapterNo, char.DefaultLookID)
			return defaultLook
		}
	}
	// 最终兜底：角色有形象但 DefaultLookID 未设置（如老数据），取第一个含三视图的形象
	if looks, err := s.lookRepo.ListByCharacter(char.ID); err == nil {
		for _, l := range looks {
			if l.ThreeViewSheet != "" {
				logger.Printf("[getCharActiveLook] charID=%d chapterNo=%d: fallback to first look with ThreeViewSheet id=%d", char.ID, chapterNo, l.ID)
				return l
			}
		}
	}
	return nil
}

// ─── 分镜参考图生成 ──────────────────────────────────────────────────────────

func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) (string, error) {
	if s.aiService == nil {
		return "", fmt.Errorf("AI service not initialized")
	}

	// 提前计算章节序号，用于按章节匹配角色形象
	var chapterNo int
	if shot.ChapterID != nil && s.chapterRepo != nil {
		if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
			chapterNo = chapter.ChapterNo
		}
	}

	// 精准匹配：批量加载 shot.CharacterIDs 中的所有角色三视图（ThreeViewSheet），最多 maxCharRefs 张
	const maxCharRefs = maxCompositeImages - 1
	var characterPortraits []string
	var characterVisualPrompts []string
	var charNamesForPrompt []string
	var refSources []string
	if len(shot.CharacterIDs) > 0 {
		ids := []uint(shot.CharacterIDs)
		batchChars, batchErr := s.characterRepo.ListByIDs(ids)
		if batchErr != nil {
			logger.Errorf("[CharRef] shot#%d ListByIDs(%v) failed: %v", shot.ShotNo, ids, batchErr)
		} else if len(batchChars) == 0 {
			logger.Errorf("[CharRef] shot#%d ListByIDs(%v) returned empty — characters may have been deleted", shot.ShotNo, ids)
		} else {
			for _, char := range batchChars {
				charNamesForPrompt = append(charNamesForPrompt, char.Name)
				activeLook := s.getCharActiveLook(char, chapterNo)
				var refImage, vprompt string
				if activeLook != nil {
					refImage = normalizeMediaURL(activeLook.ThreeViewSheet)
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
				logger.Printf("[CharRef] shot#%d charID=%d name=%q chapterNo=%d activeLook=%v threeView=%q urlType=%s",
					shot.ShotNo, char.ID, char.Name, chapterNo, activeLook != nil, refImage, urlType)
				if vprompt != "" {
					characterVisualPrompts = append(characterVisualPrompts, vprompt)
				} else {
					characterVisualPrompts = append(characterVisualPrompts, buildCharTextAnchor(char))
				}
				if refImage != "" && len(characterPortraits) < maxCharRefs {
					characterPortraits = append(characterPortraits, refImage)
					refSources = append(refSources, fmt.Sprintf("charID=%d ThreeViewSheet", char.ID))
				}
			}
		}
	}

	// cachedNovelChars 延迟加载：降级一名称匹配使用
	var cachedNovelChars []*model.Character

	// 降级一：若 CharacterIDs 未命中，从 shot.GenMeta.Characters JSON 内联名称匹配
	// （CharacterIDs 由 autoMatchShotCharacters 在分镜生成时设置，若名称有偏差则可能为空）
	if len(characterPortraits) == 0 && shot.GenMeta.Characters != "" {
		var shotChars []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(shot.GenMeta.Characters), &shotChars); err == nil && len(shotChars) > 0 {
			if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil && video.NovelID > 0 {
				if cachedNovelChars == nil {
					var e error
					cachedNovelChars, e = s.characterRepo.ListByNovel(video.NovelID)
					if e != nil {
						logger.Errorf("[VideoService] characterRepo.ListByNovel novelID=%d: %v", video.NovelID, e)
					}
				}
				if len(cachedNovelChars) > 0 {
					nameMap := make(map[string]*model.Character, len(cachedNovelChars))
					for _, c := range cachedNovelChars {
						nameMap[strings.ToLower(c.Name)] = c
					}
					// 匹配并收集所有命中角色
					type inlineRef struct {
						name    string
						char    *model.Character
						look    *model.CharacterLook // 预取的激活形象
					}
					var inlineChars []inlineRef
					seenIDs := make(map[uint]bool)
					for _, sc := range shotChars {
						nameLow := strings.ToLower(sc.Name)
						char, ok := nameMap[nameLow]
						if !ok {
							for n, c := range nameMap {
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
						if ok && char != nil && !seenIDs[char.ID] {
							seenIDs[char.ID] = true
							activeLook := s.getCharActiveLook(char, chapterNo)
							inlineChars = append(inlineChars, inlineRef{name: sc.Name, char: char, look: activeLook})
							if activeLook != nil && activeLook.VisualPrompt != "" {
								characterVisualPrompts = append(characterVisualPrompts, activeLook.VisualPrompt)
							} else {
								// 无 VisualPrompt：用角色名+描述作为文本锚点（兜底同上）
								characterVisualPrompts = append(characterVisualPrompts, buildCharTextAnchor(char))
							}
						}
					}
					for _, ir := range inlineChars {
						if len(characterPortraits) >= maxCharRefs {
							break
						}
						if ir.look != nil && ir.look.ThreeViewSheet != "" {
							characterPortraits = append(characterPortraits, normalizeMediaURL(ir.look.ThreeViewSheet))
							refSources = append(refSources, fmt.Sprintf("inline name=%q ThreeViewSheet", ir.name))
						}
					}
				}
			}
		}
	}

	logger.Printf("generateShotReferenceImage: shot %d charIDs=%v sources=%v portraits=%d",
		shot.ShotNo, shot.CharacterIDs, refSources, len(characterPortraits))
	if len(shot.CharacterIDs) > 0 && len(characterPortraits) == 0 {
		logger.Errorf("[WARN] generateShotReferenceImage: shot %d has CharacterIDs=%v but no portrait/ThreeViewSheet found — characters may not have images generated yet", shot.ShotNo, shot.CharacterIDs)
	}

	promptText := shot.GenMeta.Prompt
	if promptText == "" {
		promptText = shot.Description
	}

	// 场景锚点：注入锁定词，并收集场景参考图。
	// !! 必须在角色注入之前 prepend，这样角色信息最终排在场景描述前面。
	// 对 Seedream 等非 IP-Adapter 模型，prompt 靠前的 token 权重更高；
	// 若场景描述排在第一位，模型优先渲染场景而忽略角色。
	var sceneRefImage string
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, refURL, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil {
			if fragment != "" {
				promptText = fragment + ", " + promptText
			}
			sceneRefImage = refURL
		}
	}

	// 角色外观 token 注入（prepend，排在场景锚点前）：
	// - DreamO（有参考图）：IP-Adapter 负责外貌精准还原，文字描述提供存在性约束；
	// - Seedream/其他：reference 仅作风格/主体提示；文字描述是让模型渲染角色的关键。
	// - 无参考图（Text2ImgV3）：文字锚点是约束外貌的唯一手段。
	if len(characterVisualPrompts) > 0 {
		promptText = strings.Join(characterVisualPrompts, ", ") + ", " + promptText
	}

	// 角色名 + 动作/姿态（最后 prepend → 最终排在 prompt 最前面）：
	// 角色名排在 prompt 最前使 Seedream 将其识别为画面主体。
	if len(characterPortraits) > 0 {
		var presenceTokens []string // 人物存在性 + 动作/表情
		if shot.GenMeta.Characters != "" {
			var shotCharsAction []struct {
				Name       string `json:"name"`
				Pose       string `json:"pose"`
				Expression string `json:"expression"`
			}
			if err := json.Unmarshal([]byte(shot.GenMeta.Characters), &shotCharsAction); err == nil && len(shotCharsAction) > 0 {
				for _, c := range shotCharsAction {
					if c.Name != "" {
						presenceTokens = append(presenceTokens, c.Name)
					}
				}
				for _, c := range shotCharsAction {
					if c.Pose != "" {
						presenceTokens = append(presenceTokens, c.Pose)
					}
					if c.Expression != "" {
						presenceTokens = append(presenceTokens, c.Expression)
					}
				}
			}
		}
		// shot.GenMeta.Characters 为空时，从 DB 加载的角色名兜底
		if len(presenceTokens) == 0 && len(charNamesForPrompt) > 0 {
			presenceTokens = append(presenceTokens, charNamesForPrompt...)
		}
		if len(presenceTokens) > 0 {
			promptText = strings.Join(presenceTokens, ", ") + ", " + promptText
		}
	}

	// 参考图列表：角色三视图在前，场景锚点图排最后。
	//
	// 有角色时：[char1, char2, ..., sceneRef]
	//   角色三视图排在前（Seedream 对靠前 token 注意力更高），场景图排最后作为视觉风格锚定。
	//   场景图与角色图竞争时角色优先，保证角色出现；场景图锁定色调/光照，保证跨镜一致性。
	// 无角色时：[sceneRef]
	//   仅场景图，全力锁定视觉风格。
	//
	// DreamO 注意：selectImageModel 用 firstRef 判断是否启用 DreamO。
	//   有角色时 firstRef 是角色三视图 → 正确触发 DreamO（角色特征保持）；
	//   无角色时 firstRef 是场景图 → 正确触发 Text2ImgV3（纯文生图）。
	cappedPortraits := characterPortraits
	allRefImages := make([]string, 0, len(cappedPortraits)+1)
	allRefImages = append(allRefImages, cappedPortraits...)
	if sceneRefImage != "" {
		allRefImages = append(allRefImages, sceneRefImage)
	}
	logger.Printf("generateShotReferenceImage: shot %d allRefImages=%d (charPortraits=%d sceneRef=%v)",
		shot.ShotNo, len(allRefImages), len(cappedPortraits), sceneRefImage != "")

	ctx := context.Background()

	// 获取视频的 ArtStyle、TenantID、质量档位、宽高比、角色一致性权重和色彩调色
	artStyle := ""
	var tenantID uint
	charConsistencyWeight := 0.85  // 较高一致性：DreamO scale≈8.65，优先保持角色面部特征清晰
	qualityTier := "production"   // 默认质量档位（preview=768px 对视频参考帧质量不够）
	var imageAspectRatio, colorGrade string
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
				if vc.CharConsistencyWeight > 0 {
					charConsistencyWeight = vc.CharConsistencyWeight
				}
				// 项目设置的画面风格优先于视频级别的默认值
				if novel.AIConfig.ImageStyle != "" {
					artStyle = novel.AIConfig.ImageStyle
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
	// 质量档位对应的 CFG scale（引导强度），无参考图时注入 Text2ImgV3 的 scale 参数
	_, _, qualityCFG := qualityTierImageParams(qualityTier)
	logger.Printf("generateShotReferenceImage: shot %d qualityTier=%s aspectRatio=%s imageSize=%s qualityCFG=%.1f", shot.ShotNo, qualityTier, imageAspectRatio, imageSize, qualityCFG)

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
	shotHasAnyCharacter := len(characterPortraits) > 0 || len(shot.CharacterIDs) > 0 ||
		(shot.GenMeta.Characters != "" && shot.GenMeta.Characters != "[]" && shot.GenMeta.Characters != "null")
	noPersonNeg := "person, people, human, man, woman, figure, silhouette, character, face, body, limbs, hands, clothing, portrait"
	if !shotHasAnyCharacter && (shot.GenMeta.NegativePrompt == "" || !strings.Contains(shot.GenMeta.NegativePrompt, "person")) {
		imgNegBase = noPersonNeg + ", " + imgNegBase
	}
	// 有角色时追加面部模糊专项负向词
	faceNeg := "blurry face, out of focus face, soft focus face, unfocused face, " +
		"pixelated face, low res face, motion blur on face, smeared face, smudged face, " +
		"faceless, featureless face, undefined face, indistinct face"
	if shotHasAnyCharacter {
		imgNegBase = imgNegBase + ", " + faceNeg
	}
	negPrompt := imgNegBase
	if shot.GenMeta.NegativePrompt != "" {
		negPrompt = imgNegBase + ", " + shot.GenMeta.NegativePrompt
	}

	// Prompt 前缀策略：
	// - shot.GenMeta.Prompt（LLM 生成的 image_prompt）已包含画风/画质词/镜头参数，只补充项目级调色和风格词，
	//   避免重复注入镜头参数（如 35mm vs 85mm）产生冲突，导致画面比例/构图异常。
	// - shot.GenMeta.Prompt 为空时（降级用 description），注入完整电影级前缀补足画质词和镜头描述。
	lensTypeMap := map[string]string{
		"extreme_close_up": "macro lens 100mm, extreme shallow DOF, bokeh",
		"close_up":         "portrait lens 85mm, shallow depth of field, subject isolation",
		"medium":           "standard lens 50mm, natural perspective",
		"wide":             "wide angle lens 24mm, deep focus, environmental context",
		"extreme_wide":     "ultra wide lens 16mm, expansive environment, dramatic perspective",
	}
	lensType := lensTypeMap[shot.CamDir.ShotSize]
	if lensType == "" {
		lensType = "standard lens 50mm"
	}

	// 将风格 ID 解析为图像模型可识别的中文描述词，与 GenerateThreeViewSheet 保持一致。
	// 无条件注入（不做 Contains 检查）：LLM 生成的分镜 prompt 可能使用旧风格词，以项目当前设置为最终权威。
	styleDesc := ""
	if artStyle != "" {
		styleDesc = resolveStyleDesc(artStyle) + "风格"
	}

	if shot.GenMeta.Prompt != "" {
		// LLM 生成的 image_prompt 已完整，只在最前端注入项目级画面风格和色调。
		var prefix string
		if styleDesc != "" {
			prefix += styleDesc + ", "
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
		if styleDesc != "" {
			cinematicImgPrefix = styleDesc + ", " + cinematicImgPrefix
		}
		promptText = cinematicImgPrefix + promptText
	}

	// 画质词兜底：旧格式分镜或 description 降级路径不含画质词，统一补齐。
	// 使用 resolveStyleQualityTokens 按风格 ID 分类，覆盖全部 15 种预设风格。
	if !strings.Contains(strings.ToLower(promptText), "masterpiece") {
		promptText += ", " + resolveStyleQualityTokens(artStyle)
	}

	// 有角色时追加面部锐化词，解决 DreamO 多参考图下面部模糊问题
	if len(cappedPortraits) > 0 {
		promptText += ", sharp face, detailed face, crisp facial features, high facial detail, perfect face"
	}

	// DreamO 模式（有角色参考图）：IP-Adapter 已保证角色外貌，过长的 prompt 会分散注意力。
	// 截断至 600 字符，优先保留前段（场景/构图/动作），最多保留到最近一个逗号边界。
	if len(cappedPortraits) > 0 && len(promptText) > 600 {
		truncated := promptText[:600]
		if idx := strings.LastIndex(truncated, ","); idx > 300 {
			truncated = truncated[:idx]
		}
		promptText = truncated
	}

	// 场景锚点图片不加入 allRefImages：
	// 见上方"参考图列表"注释。二次读取也同样跳过场景图，防止并发批次中后加进来。

	logger.Printf("generateShotReferenceImage: shot %d prompt=%q negPrompt=%q", shot.ShotNo, promptText[:min(len(promptText), 120)], negPrompt[:min(len(negPrompt), 80)])
	// 无角色参考图时（Text2ImgV3 纯文生图）：用质量档位 CFG 替代 consistencyWeight，让文生图遵从 prompt；
	// 有参考图时（DreamO）：consistencyWeight 控制 IP-Adapter 强度（0.75 → scale≈7.75）。
	imageConsistencyWeight := charConsistencyWeight
	if len(allRefImages) == 0 {
		// Text2ImgV3 scale 参数（默认2.5，范围1-10），用质量 CFG 映射到合理范围（draft:6→0.56, production:7.5→0.72, master:8→0.78）
		imageConsistencyWeight = (qualityCFG - 1.0) / 9.0
	}
	var sceneSeed int64
	if shot.SceneAnchorID != nil {
		sceneSeed = int64(*shot.SceneAnchorID) * 31337
	}
	imageURL, err := s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, "", promptText, allRefImages, artStyle, negPrompt, imageSize, sceneSeed, imageConsistencyWeight)
	if err != nil {
		logger.Errorf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return "", err
	}
	if imageURL == "" {
		logger.Printf("generateShotReferenceImage: image gen returned empty URL for shot %d", shot.ShotNo)
		return "", fmt.Errorf("image provider returned empty URL")
	}

	// 首图锁定：场景锚点无参考图时，将本次生成结果存为参考图
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if err := s.sceneAnchorSvc.AutoSetRefImage(*shot.SceneAnchorID, imageURL); err != nil {
			logger.Errorf("[VideoService] AutoSetRefImage: %v", err)
		}
	}

	return imageURL, nil
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
	basePrompt := shot.GenMeta.Prompt
	if basePrompt == "" {
		basePrompt = shot.Description
	}
	if suggestion != "" {
		shotCopy.GenMeta.Prompt = basePrompt + ". Modification: " + suggestion
	} else {
		shotCopy.GenMeta.Prompt = basePrompt
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
		// 默认 CFG=0.65：偏高忠实度，视频贴近参考帧，减少偏离场景的随机发挥
		return "std", 0.65, 5
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

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}

	logger.Printf("GenerateShotVideo: shot %d provider=%s aspect=%s duration=%.2fs", shot.ShotNo, providerName, videoAspectRatio, shot.Duration)

	// 参考图策略（优先级从高到低）：
	//   ① shot.GenMeta.ReferenceImageURL 非空（上一镜最后一帧）→ I2V 时序链接，最高优先级
	//   ② shot.GenMeta.ReferenceImageURL 空 + shot.ImageURL 已生成 → 复用场景图
	//   ③ 两者均空 + 角色三视图/场景锚点存在 → 直接用这些作参考图
	//   ④ 无任何参考图 → 先生成场景图
	referenceImage := ""
	var refLabel string // HappyHorse r2v: label for referenceImage ("角色名" or "")

	if shot.GenMeta.ReferenceImageURL != "" {
		// ① 上一镜最后一帧（I2V 链接）：作为主参考图，保证时序连贯
		referenceImage = shot.GenMeta.ReferenceImageURL
		logger.Printf("GenerateShotVideo: shot %d using last-frame I2V reference: %s", shot.ShotNo, referenceImage)
		// ImageURL（静态场景图）降级为附加参考图，维持外观一致性
		// （extraRefImages 在下方逻辑中追加）
	} else if shot.ImageURL != "" {
		// ② 已有正式镜头图，直接复用，无需再次生成
		referenceImage = shot.ImageURL
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
	} else {
		// ③④ 无正式场景图
		var charRefImages []string // 按 CharacterIDs 顺序排列，严格一一对应
		var charRefNames []string  // 与 charRefImages 并行：用于 r2v [Image N] 注解
		var sceneRefImage string

		if s.characterRepo != nil && len(shot.CharacterIDs) > 0 {
			chapterNo := 0
			if shot.ChapterID != nil && s.chapterRepo != nil {
				if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
					chapterNo = chapter.ChapterNo
				}
			}
			chars, charErr := s.characterRepo.ListByIDs([]uint(shot.CharacterIDs))
			if charErr == nil && len(chars) > 0 {
				charMap := make(map[uint]*model.Character, len(chars))
				for _, c := range chars {
					charMap[c.ID] = c
				}
				for _, cid := range shot.CharacterIDs {
					c, ok := charMap[cid]
					if !ok {
						continue
					}
					look := s.getCharActiveLook(c, chapterNo)
					if look != nil && look.ThreeViewSheet != "" {
						img := normalizeMediaURL(look.ThreeViewSheet)
						if img != "" {
							charRefImages = append(charRefImages, img)
							charRefNames = append(charRefNames, c.Name)
							logger.Printf("GenerateShotVideo: shot %d charID=%d name=%q → threeView=%q", shot.ShotNo, c.ID, c.Name, img)
						}
					} else {
						logger.Printf("GenerateShotVideo: shot %d charID=%d name=%q has no ThreeViewSheet", shot.ShotNo, c.ID, c.Name)
					}
				}
			}
		}
		if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
			if _, anchorURL, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && anchorURL != "" {
				sceneRefImage = anchorURL
			}
		}

		if len(charRefImages) > 0 || sceneRefImage != "" {
			// ③ 有角色三视图或场景图，直接用，无需生成中间图片
			if len(charRefImages) > 0 {
				referenceImage = charRefImages[0]
				if len(charRefNames) > 0 {
					refLabel = charRefNames[0]
				}
			} else {
				referenceImage = sceneRefImage
				sceneRefImage = ""
			}
			_ = sceneRefImage
			logger.Printf("GenerateShotVideo: shot %d no ImageURL — using char refs=%d scene=%v directly", shot.ShotNo, len(charRefImages), sceneRefImage != "")
		} else {
			// ④ 无任何参考图 → 先生成场景图
			logger.Printf("GenerateShotVideo: shot %d ImageURL empty and no char refs, generating image first", shot.ShotNo)
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
					return frameErr
				}
				return fmt.Errorf("shot %d: %s", shot.ShotNo, errMsg)
			}
			shot.ImageURL = frameURL
			referenceImage = frameURL
			if updateErr := s.storyboardRepo.Update(shot); updateErr != nil {
				logger.Errorf("GenerateShotVideo: shot %d failed to persist ImageURL: %v", shot.ShotNo, updateErr)
			}
		}
	}

	// 衔接语义注入（优先级：TransitionIn > 前一镜头 TransitionOut > 前一镜头 MotionPrompt 截断）
	// TransitionIn 由 AI 分镜师生成，精确描述本镜头应如何衔接上一镜头的结束状态。
	// 仅在无 I2V 末帧时注入（有末帧时视频模型已能自动感知运动延续，文字引导可能干扰）。
	continuityPrefix := ""
	if shot.ShotNo > 1 && shot.GenMeta.ReferenceImageURL == "" && s.storyboardRepo != nil {
		if shot.CamDir.TransitionIn != "" {
			// ① 使用本镜头自己的 transition_in（最精确）
			continuityPrefix = shot.CamDir.TransitionIn
		} else if prev, prevErr := s.storyboardRepo.GetByVideoAndShotNo(shot.VideoID, shot.ShotNo-1); prevErr == nil && prev != nil {
			if prev.CamDir.TransitionOut != "" {
				// ② 使用上一镜头的 transition_out（次精确）
				continuityPrefix = "continuing from: " + prev.CamDir.TransitionOut
			} else {
				// ③ 降级：截取上一镜头 MotionPrompt/Description 作粗粒度引导
				desc := prev.GenMeta.MotionPrompt
				if desc == "" {
					desc = prev.Description
				}
				if len([]rune(desc)) > 80 {
					desc = string([]rune(desc)[:80]) + "..."
				}
				if desc != "" {
					continuityPrefix = "continuing from previous shot (" + desc + ")"
				}
			}
		}
	}

	// 场景锚点：将锁定词注入视频生成 prompt
	// 优先使用运镜提示词（MotionPrompt），若为空则降级到静态画面描述（Prompt）
	videoPrompt := shot.GenMeta.MotionPrompt
	if videoPrompt == "" {
		videoPrompt = shot.GenMeta.Prompt
	}
	if continuityPrefix != "" {
		videoPrompt = continuityPrefix + ", " + videoPrompt
	}
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, _, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && fragment != "" {
			videoPrompt = fragment + ", " + videoPrompt
		}
	}

	// 画面风格：注入视频 prompt（video.ArtStyle 优先，降级到 novel.AIConfig.ImageStyle）
	if videoArtStyle := s.resolveArtStyle(shot.VideoID); videoArtStyle != "" {
		videoPrompt = videoArtStyle + " style, " + videoPrompt
	}

	// 台词与音效：将旁白、角色台词、音效标签注入 prompt，帮助模型理解画面动作和声音氛围
	{
		var extras []string
		if shot.Narration != "" {
			n := shot.Narration
			if len([]rune(n)) > 50 {
				n = string([]rune(n)[:50]) + "…"
			}
			extras = append(extras, "narration: "+n)
		}
		if shot.GenMeta.Dialogue != "" {
			d := shot.GenMeta.Dialogue
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
		if len(extras) > 0 {
			videoPrompt += ", " + strings.Join(extras, ", ")
		}
	}

	// TTS 对齐：若分镜有配音，确保视频时长不短于音频时长+缓冲。
	// alignShotDurationToTTS 仅返回调整值，不持久化到 DB。
	shotDuration := shot.Duration

	// 动态 Kling 参数（根据情绪和摄像机类型选择最优配置）
	klingMode, klingCFG, klingDefaultDur := emotionToKlingParams(shot.CamDir.EmotionalTone, shot.CamDir.CameraType)
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
			hdEnabled = strings.Contains(vid.RenderConfig.VisualMode, "hd")
			threeDEnabled = vc.ThreeDEnabled || strings.Contains(vid.RenderConfig.VisualMode, "3d")
			threeDStyle = vid.RenderConfig.ThreeDStyle
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
	cinematicPrefix := buildCinematicPrefix(shot.CamDir.CameraType, shot.CamDir.EmotionalTone)
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
	if shot.GenMeta.NegativePrompt != "" {
		negativePrompt = negativeBase + ", " + shot.GenMeta.NegativePrompt
	}

	// Seedance / Kling / HappyHorse 均支持多张参考图：在主参考图之外追加角色三视图和场景锚点图，
	// 角色按 CharacterIDs 顺序严格排列，保持和分镜角色的一一对应关系。
	var extraRefImages []string
	var extraRefLabels []string // HappyHorse r2v：与 extraRefImages 并行的标签（角色名 / "场景背景"）
	multiImageProviders := map[string]bool{"seedance": true, "kling": true, "happyhorse": true}
	// I2V 模式：shot.ImageURL（静态场景图）追加为第一额外参考图，维持外观一致性
	if shot.GenMeta.ReferenceImageURL != "" && shot.ImageURL != "" && multiImageProviders[providerName] {
		if absImg := s.resolveAbsURL(shot.ImageURL); absImg != "" && absImg != s.resolveAbsURL(referenceImage) {
			extraRefImages = append(extraRefImages, absImg)
			extraRefLabels = append(extraRefLabels, "场景参考")
		}
	}
	if multiImageProviders[providerName] && s.characterRepo != nil && len(shot.CharacterIDs) > 0 {
		chapterNo := 0
		if shot.ChapterID != nil && s.chapterRepo != nil {
			if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
				chapterNo = chapter.ChapterNo
			}
		}
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
				look := s.getCharActiveLook(c, chapterNo)
				var img string
				if look != nil {
					img = normalizeMediaURL(look.ThreeViewSheet)
				}
				if img != "" && img != referenceImage {
					extraRefImages = append(extraRefImages, img)
					extraRefLabels = append(extraRefLabels, c.Name)
				}
			}
		}
	}
	if multiImageProviders[providerName] && s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if _, anchorRefURL, anchorErr := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); anchorErr == nil && anchorRefURL != "" && anchorRefURL != referenceImage {
			extraRefImages = append(extraRefImages, anchorRefURL)
			extraRefLabels = append(extraRefLabels, "场景背景")
		}
	}

	// 外部 API 不能访问相对路径，将 /api/v1/media/* 补全为绝对 URL
	absRef := s.resolveAbsURL(referenceImage)
	var absExtras []string
	for _, u := range extraRefImages {
		if resolved := s.resolveAbsURL(u); resolved != "" {
			absExtras = append(absExtras, resolved)
		}
	}

	// HappyHorse r2v：在 prompt 前缀注入 [Image N] 角色引用，帮助模型区分多张参考图中的人物
	// 官方文档：prompt 中使用 "[Image N]中的{名字}" 引用 media 数组第 N 张图（1-based）
	if providerName == "happyhorse" && (absRef != "" || len(absExtras) > 0) {
		totalImages := 0
		if absRef != "" {
			totalImages++
		}
		totalImages += len(absExtras)
		if totalImages >= 2 {
			allLabels := make([]string, 0, 1+len(extraRefLabels))
			allLabels = append(allLabels, refLabel) // label for absRef ("角色名" or "")
			allLabels = append(allLabels, extraRefLabels...)
			var annotations []string
			for i, label := range allLabels {
				if i < totalImages && label != "" {
					annotations = append(annotations, fmt.Sprintf("[Image %d]为%s", i+1, label))
				}
			}
			if len(annotations) > 0 {
				videoPromptFinal = strings.Join(annotations, "，") + "。" + videoPromptFinal
			}
		}
	}

	// HappyHorse 分辨率：HD 模式用 1080P，否则 720P
	videoResolution := ""
	if providerName == "happyhorse" {
		if hdEnabled {
			videoResolution = "1080p"
		} else {
			videoResolution = "720p"
		}
	}

	// Seedance 多模态时序链接：查找前一分镜的完成视频 URL 作为运动参考
	var prevVideoURLs []string
	if providerName == "seedance" && shot.ShotNo > 1 && s.storyboardRepo != nil {
		if prev, prevErr := s.storyboardRepo.GetByVideoAndShotNo(shot.VideoID, shot.ShotNo-1); prevErr == nil && prev != nil {
			if prev.VideoURL != "" && strings.HasPrefix(prev.VideoURL, "http") {
				prevVideoURLs = []string{prev.VideoURL}
				logger.Printf("GenerateShotVideo: shot %d Seedance video-chain: %s", shot.ShotNo, prev.VideoURL)
			}
		}
	}

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
		Mode:           klingMode,
		Model:          klingModelOverride,
	}

	logger.Printf("GenerateShotVideo: shot %d submitting to %s (hasRef=%v extraRefs=%d mode=%s cfg=%.2f prompt=%q)", shot.ShotNo, providerName, referenceImage != "", len(extraRefImages), klingMode, klingCFG, videoPromptFinal)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		metrics.ShotVideoSubmissionTotal.WithLabelValues(providerName, "error").Inc()
		logger.Errorf("GenerateShotVideo: shot %d submit failed: %v", shot.ShotNo, err)
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	metrics.ShotVideoSubmissionTotal.WithLabelValues(providerName, "success").Inc()
	logger.Printf("GenerateShotVideo: shot %d submitted taskID=%s", shot.ShotNo, task.TaskID)
	shot.TaskMeta.ShotTaskID = task.TaskID
	shot.TaskMeta.ShotProviderName = providerName
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
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Errorf("[VideoService] generateShotImageOnly: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return "", 0, fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return "", 0, fmt.Errorf("image generation failed for shot %d (empty URL)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
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

	fields := map[string]interface{}{"progress": 100}
	if lastErr != nil {
		logger.Errorf("generateClipAndUploadWithRetry: shot %d clip failed after %d attempts, keeping image-only: %v", shot.ShotNo, maxClipRetries, lastErr)
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

	shot.GenMeta.GenerationMode = "static"
	shot.Status = "generating"
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
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Errorf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to failed: %v", shot.ShotNo, err)
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
		logger.Errorf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// 2. 生成 Ken Burns 动效视频片段
	logger.Printf("GenerateSlideshowShotVideo: shot %d starting Ken Burns encode", shot.ShotNo)
	localImage, dlErr := s.resolveImageURLToLocalFile(shot.ImageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID))
	if dlErr != nil {
		logger.Errorf("GenerateSlideshowShotVideo: shot %d resolve image failed: %v — skipping Ken Burns", shot.ShotNo, dlErr)
		shot.Status = "completed"
		shot.TaskMeta.Progress = 100
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
		shot.VideoURL = ossURL
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("GenerateSlideshowShotVideo: shot %d video → %s", shot.ShotNo, ossURL)
	} else {
		shot.VideoURL = "file://" + clipPath
		logger.Printf("GenerateSlideshowShotVideo: shot %d video local → %s", shot.ShotNo, clipPath)
	}

	shot.Status = "completed"
	shot.TaskMeta.Progress = 100
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
func (s *VideoService) runSlideshowPipeline(videoID uint) {
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
			if err := s.GenerateShotAudio(sh, s.videoTenantID(video), narrationVoice); err != nil {
				logger.Errorf("runSlideshowPipeline: audio gen failed for shot %d: %v", sh.ShotNo, err)
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
		s.runSlideshowPipeline(videoID)
		// 拼接所有 completed 分镜
		if _, stitchErr := s.StitchVideoCtx(context.Background(), videoID); stitchErr != nil {
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
		go s.GenerateShotAudio(shot, s.videoTenantID(video), narrationVoice) //nolint:errcheck
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
//  阶段1 - 并发为所有分镜生成 TTS，测量实际配音时长
//  阶段2 - 将各分镜 Duration 更新为配音时长（保证视频不短于配音）
//  阶段3 - 调用 BatchGenerateShots 正常生成视频
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
			if genErr := s.GenerateShotAudio(sh, s.videoTenantID(video), narrationVoice); genErr != nil {
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
