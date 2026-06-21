package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/redis/go-redis/v9"
)

// bgmCacheEntry caches API search results to avoid duplicate Jamendo/Pixabay requests.
type bgmCacheEntry struct {
	url       string
	name      string
	artist    string
	expiresAt time.Time
}

const bgmCacheTTL = 24 * time.Hour

// BGMService BGM 背景音乐服务。
// 三层降级：本地目录 → Jamendo API → Pixabay API。
type BGMService struct {
	bgmDir           string       // 本地 BGM 文件目录（优先）
	aiSvc            *AIService   // AI 分析（可选）
	storageSvc       storage.Service
	assetRepo        *repository.AssetRepository
	tagRepo          *repository.TagRepository
	httpClient       *http.Client
	cache            *redis.Client // optional: cross-instance BGM URL cache
	localUploadCache sync.Map // local path → OSS URL (process-local fallback)
	queryCache       sync.Map // "jamendo:query" / "pixabay:query" → bgmCacheEntry
	localFileCache   sync.Map // dirPath → []string (已扫描的文件名列表)
}

// NewBGMService 创建 BGM 服务
// bgmDir: 本地 BGM 文件目录，文件名格式 <emotion>.mp3 或 <emotion>.wav
func NewBGMService(bgmDir string, _ ...map[string]string) *BGMService {
	return &BGMService{
		bgmDir:     bgmDir,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// WithAIService 注入 AI 服务（启用 AI 分段分析）
func (s *BGMService) WithAIService(aiSvc *AIService) *BGMService {
	s.aiSvc = aiSvc
	return s
}

// WithStorage 注入存储服务（本地文件上传 OSS，生成可公开访问的 URL）
func (s *BGMService) WithStorage(svc storage.Service) *BGMService {
	s.storageSvc = svc
	return s
}

// WithRedis enables cross-instance BGM URL caching so multiple instances share
// the same local-file→OSS-URL mapping instead of re-uploading the same file.
func (s *BGMService) WithRedis(c *redis.Client) *BGMService {
	s.cache = c
	return s
}

// WithAssetRepo 注入素材库仓库（可选；注入后生成的 BGM 将自动发布至公共素材库）
func (s *BGMService) WithAssetRepo(assetRepo *repository.AssetRepository, tagRepo *repository.TagRepository) *BGMService {
	s.assetRepo = assetRepo
	s.tagRepo = tagRepo
	return s
}

// bgmProviderCreds 从 DB 取 music 类型供应商凭据。
func (s *BGMService) bgmProviderCreds(tenantID uint, name string) (apiKey, endpoint string) {
	if s.aiSvc == nil {
		return "", ""
	}
	return s.aiSvc.GetBGMProviderCreds(tenantID, name)
}

// localDirFiles 返回目录内的文件名列表（不含路径），结果缓存在 localFileCache 中。
// 首次调用时扫描目录，后续调用直接返回缓存结果，避免高并发下重复 os.Stat 系统调用。
func (s *BGMService) localDirFiles(dir string) []string {
	if cached, ok := s.localFileCache.Load(dir); ok {
		return cached.([]string)
	}
	entries, err := os.ReadDir(dir)
	var names []string
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	s.localFileCache.Store(dir, names)
	return names
}

// SelectBGM 根据情感关键词在本地目录选择 BGM，返回本地路径（供 MixBGM 使用）。
// 返回空字符串表示无本地文件。
func (s *BGMService) SelectBGM(emotion string) string {
	if s.bgmDir == "" {
		return ""
	}
	if emotion == "" {
		emotion = "default"
	}
	emotion = strings.ToLower(strings.TrimSpace(emotion))
	// Reject path traversal: emotion must be a plain filename component.
	if strings.ContainsAny(emotion, "/\\") || strings.Contains(emotion, "..") {
		emotion = "default"
	}

	// 使用缓存的文件名列表，避免高并发下对同一目录重复 os.Stat。
	files := s.localDirFiles(s.bgmDir)
	fileSet := make(map[string]struct{}, len(files))
	for _, f := range files {
		fileSet[f] = struct{}{}
	}

	for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg"} {
		name := emotion + ext
		if _, ok := fileSet[name]; ok {
			return filepath.Join(s.bgmDir, name)
		}
	}
	// 降级到 default 文件
	for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg"} {
		name := "default" + ext
		if _, ok := fileSet[name]; ok {
			return filepath.Join(s.bgmDir, name)
		}
	}
	return ""
}

// resolveLocalBGMURL 将本地文件路径转为可公开访问的 OSS URL。
// 查找顺序：进程内缓存 → Redis 缓存 → OSS 上传（加分布式锁防重复上传）。
// storageSvc 未配置时返回 ("", false)；调用方应降级到下一个来源。
func (s *BGMService) resolveLocalBGMURL(ctx context.Context, localPath string) (string, bool) {
	if s.storageSvc == nil || localPath == "" {
		return "", false
	}
	// 1. Process-local cache (fastest)
	if cached, ok := s.localUploadCache.Load(localPath); ok {
		return cached.(string), true
	}
	filename := filepath.Base(localPath)
	redisKey := fmt.Sprintf("bgm:local:%s", filename)
	// 2. Redis cache (cross-instance)
	if s.cache != nil {
		if u, err := s.cache.Get(ctx, redisKey).Result(); err == nil && u != "" {
			s.localUploadCache.Store(localPath, u)
			return u, true
		}
	}
	// 3. Upload to OSS — use a distributed lock so only one instance uploads.
	if s.cache != nil {
		lockKey := fmt.Sprintf("lock:bgm:upload:%s", filename)
		lock, acquired, lockErr := acquireDistLock(s.cache, lockKey, 30*time.Second)
		if lockErr != nil {
			logger.Errorf("[BGMService] distlock error for %s: %v, proceeding without lock", filename, lockErr)
		} else if !acquired {
			// Another instance is uploading; wait briefly then re-check Redis.
			time.Sleep(3 * time.Second)
			if u, err := s.cache.Get(ctx, redisKey).Result(); err == nil && u != "" {
				s.localUploadCache.Store(localPath, u)
				return u, true
			}
			// Still not ready — fall through and upload ourselves.
		} else {
			defer lock.release()
			// Re-check Redis after acquiring lock (another instance may have finished).
			if u, err := s.cache.Get(ctx, redisKey).Result(); err == nil && u != "" {
				s.localUploadCache.Store(localPath, u)
				return u, true
			}
		}
	}
	f, err := os.Open(localPath)
	if err != nil {
		logger.Errorf("[BGMService] open local file failed (%s): %v", localPath, err)
		return "", false
	}
	defer f.Close()
	fi, _ := f.Stat()
	ext := strings.ToLower(filepath.Ext(localPath))
	mime := "audio/mpeg"
	if ext == ".wav" {
		mime = "audio/wav"
	} else if ext == ".ogg" {
		mime = "audio/ogg"
	} else if ext == ".m4a" {
		mime = "audio/mp4"
	}
	ossKey := fmt.Sprintf("bgm/local/%s", filename)
	u, err := s.storageSvc.Upload(ctx, ossKey, f, fi.Size(), mime)
	if err != nil {
		logger.Errorf("[BGMService] OSS upload failed (%s): %v", filename, err)
		return "", false
	}
	s.localUploadCache.Store(localPath, u)
	if s.cache != nil {
		_ = s.cache.Set(ctx, redisKey, u, 7*24*time.Hour).Err()
	}
	return u, true
}

// InvalidateCacheByFile removes all cached entries associated with the given filename,
// covering both the in-process localUploadCache and the Redis bgm:local:{filename} key.
func (s *BGMService) InvalidateCacheByFile(filename string) {
	if s.cache != nil {
		redisKey := fmt.Sprintf("bgm:local:%s", filename)
		s.cache.Del(context.Background(), redisKey)
	}
	s.localUploadCache.Range(func(k, _ any) bool {
		if p, ok := k.(string); ok && filepath.Base(p) == filename {
			s.localUploadCache.Delete(k)
		}
		return true
	})
}

// MixBGM 将 BGM 混入视频（BGM 音量 30%，对话优先）
// P1-3: ctx 传播至 ffprobe/ffmpeg，避免父任务取消后仍占用 WASM worker。
func (s *BGMService) MixBGM(ctx context.Context, videoPath, bgmSource, outputPath string) error {
	videoPath = strings.TrimPrefix(videoPath, "file://")
	bgmSource = strings.TrimPrefix(bgmSource, "file://")

	if videoPath == "" || bgmSource == "" || outputPath == "" {
		return fmt.Errorf("MixBGM: invalid arguments")
	}

	bgmLocalPath := bgmSource
	if strings.HasPrefix(bgmSource, "http://") || strings.HasPrefix(bgmSource, "https://") {
		// Fix ②: use os.CreateTemp to avoid pid-based name collision in concurrent requests
		tmp, err := os.CreateTemp(inkframeTempDir(), "inkframe-bgm-*.mp3")
		if err != nil {
			return fmt.Errorf("MixBGM: create temp file failed: %w", err)
		}
		tmp.Close()
		bgmLocalPath = tmp.Name()
		// P1-5: bounded download timeout (3 min) via ctx sub-context
		dlCtx, dlCancel := context.WithTimeout(ctx, 3*time.Minute)
		dlErr := downloadFileCtx(dlCtx, bgmSource, bgmLocalPath)
		dlCancel()
		if dlErr != nil {
			os.Remove(bgmLocalPath)
			return fmt.Errorf("MixBGM: download BGM failed: %w", dlErr)
		}
		defer os.Remove(bgmLocalPath)
	}

	// P0-1 + P1-4: 修正 BGM 混音参数
	// - P0-1: 将 volume=0.3 施加在 BGM 输入上（语义正确），amix 使用 normalize=0（不再除以权重之和）
	//   旧实现 weights=1 0.3 会将人声压到 77%，与"BGM 30%、人声 100%"的预期相反
	// - 分别 loudnorm 两路再混合会导致最终 LUFS 不确定；改为只对 BGM 做基础 EQ，不做 loudnorm
	// - P1-4: 探测视频时长，为 BGM 添加淡入（0.5s）和淡出（1s）以消除突兀感
	videoDur := probeClipDuration(ctx, videoPath) // P1-3: use ctx
	// P2-4: skip fade-in when video is shorter than the fade duration to avoid near-silent BGM
	fadeIn := ""
	fadeOut := ""
	if videoDur <= 0 || videoDur >= 0.6 {
		// unknown duration or long enough: apply 0.5s fade-in
		fadeIn = "afade=t=in:st=0:d=0.5"
	}
	if videoDur > 2 {
		fadeOutSt := videoDur - 1.0
		if fadeOutSt < 0.5 {
			fadeOutSt = 0.5
		}
		fadeOut = fmt.Sprintf(",afade=t=out:st=%.2f:d=1", fadeOutSt)
	}
	// BGM：降至 30% + EQ 去浑浊 + 淡入淡出
	// 人声：80Hz 高通去低频噪声（不做 loudnorm，保留原始电平）
	// amix normalize=0：不自动归一化，保留我们的显式音量控制
	// P2-4: build fade chain dynamically to avoid trailing comma when fadeIn is empty
	bgmChain := "volume=0.3,equalizer=f=250:t=o:w=2:g=-2,equalizer=f=4000:t=o:w=2:g=-1"
	if fadeIn != "" {
		bgmChain += "," + fadeIn
	}
	bgmChain += fadeOut // fadeOut already starts with "," when non-empty
	filterComplex := fmt.Sprintf(
		"[1:a]%s[bgm];[0:a]highpass=f=80[voice];[voice][bgm]amix=inputs=2:duration=first:normalize=0[out]",
		bgmChain,
	)
	if out, err := runFFmpegCtx(ctx, "-y", // P1-3: use ctx
		"-i", videoPath,
		"-stream_loop", "-1",
		"-i", bgmLocalPath,
		"-filter_complex", filterComplex,
		"-map", "0:v",
		"-map", "[out]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-shortest",
		outputPath,
	); err != nil {
		logger.Errorf("MixBGM: ffmpeg failed: %v\n%s", err, string(out))
		return fmt.Errorf("ffmpeg BGM mix failed: %w", err)
	}
	return nil
}

// ─── AI BGM 分段分析 ──────────────────────────────────────────────────────────

// bgmShotBrief 供 AI 分析的分镜摘要
type bgmShotBrief struct {
	ShotID        uint    `json:"shot_id"`
	ShotNo        int     `json:"shot_no"`
	Description   string  `json:"description,omitempty"`
	EmotionalTone string  `json:"emotional_tone,omitempty"`
	Narration     string  `json:"narration,omitempty"`
	Duration      float64 `json:"duration"`
}

// bgmSegmentAnalysis AI 输出的单个 BGM 分段
type bgmSegmentAnalysis struct {
	StartShotNo   int      `json:"start_shot_no"`
	EndShotNo     int      `json:"end_shot_no"`
	Mood          string   `json:"mood"`
	Tempo         string   `json:"tempo"` // fast/medium/slow
	SearchQueries []string `json:"search_queries"`
}

// AnalyzeBGMForVideo 调用 AI 批量分析全部分镜，生成 BGM 分段计划并持久化。
// 使用事务原子替换旧分段，避免分析失败时数据丢失。
func (s *BGMService) AnalyzeBGMForVideo(
	ctx context.Context,
	shots []*model.StoryboardShot,
	bgmRepo *repository.VideoBGMSegmentRepository,
	videoID uint,
	tenantID uint,
	userPrompt string,
) ([]*model.VideoBGMSegment, error) {
	if s.aiSvc == nil {
		return nil, fmt.Errorf("BGMService: AI service not configured")
	}
	if len(shots) == 0 {
		return nil, nil
	}

	// Fix ⑥: truncate both Description and Narration to save tokens
	briefs := make([]bgmShotBrief, 0, len(shots))
	for _, sh := range shots {
		desc := sh.Description
		if len([]rune(desc)) > 80 {
			desc = string([]rune(desc)[:80]) + "…"
		}
		// P1-7: 按 rune 截断，防止截断 UTF-8 序列中间（中文每字 3 字节）
		narration := sh.Narration
		if nr := []rune(narration); len(nr) > 60 {
			narration = string(nr[:60]) + "…"
		}
		briefs = append(briefs, bgmShotBrief{
			ShotID:        sh.ID,
			ShotNo:        sh.ShotNo,
			Description:   desc,
			EmotionalTone: sh.EmotionalTone,
			Narration:     narration,
			Duration:      sh.Duration,
		})
	}

	briefsJSON, err := json.MarshalIndent(briefs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal briefs: %w", err)
	}

	bgmPrompt, err := renderPrompt("bgm_analyze", map[string]interface{}{
		"ShotsJSON":  string(briefsJSON),
		"UserPrompt": userPrompt,
	})
	if err != nil {
		return nil, fmt.Errorf("render bgm_analyze: %w", err)
	}

	raw, err := s.aiSvc.GenerateWithProvider(tenantID, 0, "bgm_analyze", bgmPrompt, "")
	if err != nil {
		return nil, fmt.Errorf("BGM AI analysis failed: %w", err)
	}

	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return nil, fmt.Errorf("BGM AI returned no JSON: %s", raw[:min(len(raw), 200)])
	}

	var analyses []bgmSegmentAnalysis
	if err := json.Unmarshal([]byte(jsonStr), &analyses); err != nil {
		return nil, fmt.Errorf("BGM AI JSON parse failed: %w\nraw: %s", err, jsonStr[:min(len(jsonStr), 300)])
	}

	// Fix ⑤: validate that segments cover all shot_nos without gaps or overlaps
	if warns := validateBGMCoverage(shots, analyses); len(warns) > 0 {
		for _, w := range warns {
			logger.Errorf("[BGMService] coverage warning: %s", w)
		}
		// 自动修复：先修重叠（截短前段的 EndShotNo），再修 gap（延伸前段的 EndShotNo 填满空洞）。
		// 两次扫描保证顺序处理：overlap pass → gap pass。
		if len(analyses) > 1 {
			// Pass 1: 修 overlap（前段 EndShotNo >= 后段 StartShotNo）
			for i := 0; i < len(analyses)-1; i++ {
				if analyses[i].EndShotNo >= analyses[i+1].StartShotNo {
					fixed := analyses[i+1].StartShotNo - 1
					logger.Printf("[BGMService] fix overlap: seg %d EndShotNo %d→%d", i+1, analyses[i].EndShotNo, fixed)
					analyses[i].EndShotNo = fixed
				}
			}
			// Pass 2: 修 gap（前段 EndShotNo < 后段 StartShotNo - 1）
			for i := 0; i < len(analyses)-1; i++ {
				if analyses[i].EndShotNo < analyses[i+1].StartShotNo-1 {
					fixed := analyses[i+1].StartShotNo - 1
					logger.Printf("[BGMService] fix gap: seg %d EndShotNo %d→%d", i+1, analyses[i].EndShotNo, fixed)
					analyses[i].EndShotNo = fixed
				}
			}
		}
	}

	// Fix ⑩: volume based on mood/tempo
	segments := make([]*model.VideoBGMSegment, 0, len(analyses))
	for i, a := range analyses {
		qJSON, _ := json.Marshal(a.SearchQueries)
		segments = append(segments, &model.VideoBGMSegment{
			VideoID:       videoID,
			SeqNo:         i + 1,
			StartShotNo:   a.StartShotNo,
			EndShotNo:     a.EndShotNo,
			Mood:          a.Mood,
			Tempo:         a.Tempo,
			SearchQueries: string(qJSON),
			Volume:        bgmSegmentVolume(a.Mood, a.Tempo),
		})
	}

	// Fix ⑧: atomic replace — create new then delete old in one transaction
	if bgmRepo != nil {
		if err := bgmRepo.ReplaceForVideo(videoID, segments); err != nil {
			return segments, fmt.Errorf("persist BGM segments failed: %w", err)
		}
	}

	return segments, nil
}

// validateBGMCoverage 检查 AI 返回的分段是否无遗漏、无重叠地覆盖所有 shot_no。
func validateBGMCoverage(shots []*model.StoryboardShot, segs []bgmSegmentAnalysis) []string {
	shotSet := make(map[int]bool, len(shots))
	for _, sh := range shots {
		shotSet[sh.ShotNo] = true
	}
	covered := make(map[int]int) // shot_no → segment index (1-based)
	var warns []string
	for i, seg := range segs {
		for sno := seg.StartShotNo; sno <= seg.EndShotNo; sno++ {
			if prev, dup := covered[sno]; dup {
				warns = append(warns, fmt.Sprintf("shot %d covered by segments %d and %d (overlap)", sno, prev, i+1))
			}
			covered[sno] = i + 1
		}
	}
	for sno := range shotSet {
		if covered[sno] == 0 {
			warns = append(warns, fmt.Sprintf("shot %d not covered by any BGM segment (gap)", sno))
		}
	}
	return warns
}

// bgmSegmentVolume 根据情绪和节奏返回建议 BGM 混音音量（0.15–0.45）。
// 避免动作戏 BGM 过响盖过音效，慢场景则适度增强氛围感。
func bgmSegmentVolume(mood, tempo string) float64 {
	m := strings.ToLower(mood)
	switch tempo {
	case "fast":
		// 动感节奏：稍压低，防止与音效打架
		return 0.35
	case "slow":
		// 慢场景/情感戏：适度增强氛围
		for _, kw := range []string{"悲", "伤", "泪", "离", "死", "痛", "哀"} {
			if strings.Contains(m, kw) {
				return 0.4 // 悲伤慢场景略高
			}
		}
		return 0.35
	default: // medium
		for _, kw := range []string{"紧张", "压迫", "危险", "恐", "战", "搏", "对峙"} {
			if strings.Contains(m, kw) {
				return 0.28 // 紧张场景降低 BGM，突出环境音
			}
		}
		for _, kw := range []string{"温馨", "浪漫", "平和", "轻松", "喜", "幸"} {
			if strings.Contains(m, kw) {
				return 0.38
			}
		}
		return 0.3
	}
}

// SearchBGMForSegment 为单个 BGM 分段搜索匹配曲目，更新 URL/TrackName/TrackArtist/Source。
// 优先级：素材库 → 本地目录 → Jamendo → Pixabay。
func (s *BGMService) SearchBGMForSegment(ctx context.Context, tenantID uint, seg *model.VideoBGMSegment) error {
	var queries []string
	if seg.SearchQueries != "" {
		_ = json.Unmarshal([]byte(seg.SearchQueries), &queries)
	}
	if len(queries) == 0 {
		if seg.Mood != "" {
			queries = []string{seg.Mood}
		} else {
			return nil
		}
	}

	// 0. 素材库（公共 + 个人，优先复用避免重复拉取）
	// 双路搜索：① 标签匹配（mood/tempo 词精确命中）→ ② 关键词搜索（title/description LIKE）
	if s.assetRepo != nil {
		// 0a. 标签匹配：以 mood 及 searchQueries 中各词作为 OR 标签搜索
		tagCandidates := make([]string, 0, len(queries)+1)
		if seg.Mood != "" {
			tagCandidates = append(tagCandidates, seg.Mood)
		}
		tagCandidates = append(tagCandidates, queries...)
		if len(tagCandidates) > 0 {
			assets, _, err := s.assetRepo.Search(repository.AssetSearchParams{
				Type:     "audio",
				SubType:  "bgm",
				TagsOr:   tagCandidates,
				Scope:    "all",
				CallerID: tenantID,
				Sort:     "use_count",
				PageSize: 3,
			})
			if err == nil {
				for _, a := range assets {
					if a.StorageURL != "" {
						logger.Printf("[BGMService] segment %d (%s) asset-lib tag-hit tags=%v", seg.SeqNo, seg.Mood, tagCandidates)
						_ = s.assetRepo.IncrUseCount(a.ID)
						seg.URL = a.StorageURL
						seg.TrackName = a.Title
						seg.Source = "asset-lib"
						return nil
					}
				}
			}
		}
		// 0b. 关键词搜索：按 searchQueries 逐个 LIKE 匹配 title/description
		for _, q := range queries {
			assets, _, err := s.assetRepo.Search(repository.AssetSearchParams{
				Type:     "audio",
				SubType:  "bgm",
				Q:        q,
				Scope:    "all",
				CallerID: tenantID,
				Sort:     "use_count",
				PageSize: 3,
			})
			if err == nil {
				for _, a := range assets {
					if a.StorageURL != "" {
						logger.Printf("[BGMService] segment %d (%s) asset-lib q-hit q=%q", seg.SeqNo, seg.Mood, q)
						_ = s.assetRepo.IncrUseCount(a.ID)
						seg.URL = a.StorageURL
						seg.TrackName = a.Title
						seg.Source = "asset-lib"
						return nil
					}
				}
			}
		}
	}

	// 1. 本地文件（上传 OSS 得到可访问 URL）
	if localPath := s.SelectBGM(seg.Mood); localPath != "" {
		if publicURL, ok := s.resolveLocalBGMURL(ctx, localPath); ok {
			seg.URL = publicURL
			seg.Source = "local"
			return nil
		}
		// storageSvc 未配置：跳过本地，继续尝试 API
	}

	// 2. Jamendo 搜索
	if jKey, _ := s.bgmProviderCreds(tenantID, "jamendo"); jKey != "" {
		for _, q := range queries {
			if trackURL, name, artist := s.jamendoSearch(ctx, tenantID, q); trackURL != "" {
				seg.URL = trackURL
				seg.TrackName = name
				seg.TrackArtist = artist
				seg.Source = "jamendo"
				return nil
			}
		}
		logger.Printf("[BGMService] segment %d (%s) Jamendo miss for all queries", seg.SeqNo, seg.Mood)
	}

	// 3. Pixabay 降级
	if pKey, _ := s.bgmProviderCreds(tenantID, "pixabay-bgm"); pKey != "" {
		for _, q := range queries {
			if trackURL, name := s.pixabaySearchBGM(ctx, tenantID, q); trackURL != "" {
				seg.URL = trackURL
				seg.TrackName = name
				seg.TrackArtist = "Pixabay"
				seg.Source = "pixabay"
				return nil
			}
		}
		logger.Printf("[BGMService] segment %d (%s) Pixabay miss for all queries", seg.SeqNo, seg.Mood)
	}

	return nil
}

// JamendoTrack Jamendo 音轨信息（供前端搜索结果展示）
type JamendoTrack struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	ArtistName           string   `json:"artist_name"`
	Duration             int      `json:"duration"`
	Audio                string   `json:"audio"`
	AudioDownload        string   `json:"audiodownload"`
	AudioDownloadAllowed bool     `json:"audiodownload_allowed"`
	Tags                 []string `json:"tags,omitempty"`
}

// PlayURL 返回优先可下载的播放 URL
func (t JamendoTrack) PlayURL() string {
	if t.AudioDownloadAllowed && t.AudioDownload != "" {
		return t.AudioDownload
	}
	return t.Audio
}

// JamendoSearchParams Jamendo 搜索参数
type JamendoSearchParams struct {
	Query  string // 自然语言短语（内部自动拆词传给 fuzzytags）
	Tags   string // 精确逗号分隔标签
	Speed  string // slow / medium / fast / veryslow / veryfast
	BpmMin int    // BPM 下限，0=不限
	BpmMax int    // BPM 上限，0=不限
	Limit  int    // 结果数量，默认 10，最多 50
}

// JamendoSearch 在 Jamendo API 中搜索器乐曲目，返回完整音轨列表供前端展示/选择。
func (s *BGMService) JamendoSearch(ctx context.Context, tenantID uint, p JamendoSearchParams) ([]JamendoTrack, error) {
	clientID, _ := s.bgmProviderCreds(tenantID, "jamendo")
	if clientID == "" {
		return nil, fmt.Errorf("Jamendo client_id not configured")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("format", "json")
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("vocalinstrumental", "instrumental")
	params.Set("order", "popularity_month")
	params.Set("include", "musicinfo")

	// Fix ④: fuzzytags 接受逗号分隔的标签列表，不适合整句传参
	// 将自然语言短语拆成关键词，过滤停用词后逗号连接
	if p.Query != "" {
		params.Set("fuzzytags", phraseToTags(p.Query))
	}
	if p.Tags != "" {
		params.Set("tags", p.Tags)
	}
	if p.Speed != "" {
		params.Set("speed", p.Speed)
	}
	// Fix ⑫: bpm_between 避免使用 0 作为下限
	if p.BpmMin > 0 && p.BpmMax > 0 {
		params.Set("bpm_between", fmt.Sprintf("%d_%d", p.BpmMin, p.BpmMax))
	} else if p.BpmMin > 0 {
		params.Set("bpm_between", fmt.Sprintf("%d_300", p.BpmMin))
	} else if p.BpmMax > 0 {
		params.Set("bpm_between", fmt.Sprintf("1_%d", p.BpmMax))
	}

	apiURL := "https://api.jamendo.com/v3.0/tracks/?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Jamendo request failed: %w", err)
	}
	defer resp.Body.Close()
	// Fix ⑦: log non-200 with body
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("Jamendo HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Results []struct {
			ID                   string `json:"id"`
			Name                 string `json:"name"`
			ArtistName           string `json:"artist_name"`
			Duration             int    `json:"duration"`
			Audio                string `json:"audio"`
			AudioDownload        string `json:"audiodownload"`
			AudioDownloadAllowed bool   `json:"audiodownload_allowed"`
			MusicInfo            struct {
				Tags struct {
					Genres  []string `json:"genres"`
					Vartags []string `json:"vartags"`
				} `json:"tags"`
			} `json:"musicinfo"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("Jamendo response parse failed: %w", err)
	}

	tracks := make([]JamendoTrack, 0, len(result.Results))
	for _, r := range result.Results {
		tags := append(r.MusicInfo.Tags.Genres, r.MusicInfo.Tags.Vartags...)
		tracks = append(tracks, JamendoTrack{
			ID:                   r.ID,
			Name:                 r.Name,
			ArtistName:           r.ArtistName,
			Duration:             r.Duration,
			Audio:                r.Audio,
			AudioDownload:        r.AudioDownload,
			AudioDownloadAllowed: r.AudioDownloadAllowed,
			Tags:                 tags,
		})
	}
	return tracks, nil
}

// phraseToTags 将自然语言短语（如 "cinematic orchestral dark tense"）转换为
// Jamendo fuzzytags 所需的逗号分隔标签（过滤英文停用词，取前 5 个有意义的词）。
// 新版 BGM 提示词已生成简短标签式搜索词（2~5词），此函数作为额外清洗层。
func phraseToTags(phrase string) string {
	stopwords := map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true,
		"in": true, "on": true, "at": true, "for": true, "to": true,
		"of": true, "with": true, "by": true, "from": true, "as": true,
		"is": true, "it": true, "its": true, "be": true, "music": true,
		"background": true, "theme": true, "song": true, "track": true,
	}
	words := strings.Fields(strings.ToLower(phrase))
	var tags []string
	for _, w := range words {
		if len(w) > 2 && !stopwords[w] {
			tags = append(tags, w)
			if len(tags) == 5 {
				break
			}
		}
	}
	if len(tags) == 0 {
		return phrase
	}
	return strings.Join(tags, ",")
}

// pixabaySearchBGM 在 Pixabay API 中按关键词搜索背景音乐，返回 (url, trackName)。
func (s *BGMService) pixabaySearchBGM(ctx context.Context, tenantID uint, query string) (string, string) {
	key, _ := s.bgmProviderCreds(tenantID, "pixabay-bgm")
	if key == "" || query == "" {
		return "", ""
	}

	cacheKey := "pixabay:" + query
	if v, ok := s.queryCache.Load(cacheKey); ok {
		e := v.(bgmCacheEntry)
		if time.Now().Before(e.expiresAt) {
			return e.url, e.name
		}
		s.queryCache.Delete(cacheKey)
	}

	params := url.Values{}
	params.Set("key", key)
	params.Set("q", query)
	params.Set("media_type", "music")
	params.Set("lang", "en")
	params.Set("order", "popular")
	params.Set("per_page", "10")
	params.Set("safesearch", "true")

	req, err := http.NewRequestWithContext(ctx, "GET", "https://pixabay.com/api/?"+params.Encode(), nil)
	if err != nil {
		return "", ""
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		logger.Errorf("[BGMService] Pixabay request error for %q: %v", query, err)
		return "", ""
	}
	defer resp.Body.Close()
	// Fix ⑦: log non-200
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Printf("[BGMService] Pixabay HTTP %d for %q: %s", resp.StatusCode, query, body)
		return "", ""
	}

	var result struct {
		Hits []struct {
			Tags     string `json:"tags"`
			AudioURL string `json:"audio_url"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", ""
	}
	for _, hit := range result.Hits {
		if hit.AudioURL != "" {
			audioURL := hit.AudioURL
			// 上传到 OSS，保证 Pixabay CDN 链接过期后仍可访问
			if s.storageSvc != nil {
				ossKey := fmt.Sprintf("bgm/%s.mp3", uuid.New().String())
				if ossURL, err := downloadURLAndUploadToOSS(ctx, s.storageSvc, audioURL, ossKey); err == nil {
					audioURL = ossURL
				} else {
					logger.Warnf("[BGMService] Pixabay OSS upload failed: %v", err)
				}
			}
			s.queryCache.Store(cacheKey, bgmCacheEntry{
				url: audioURL, name: hit.Tags, expiresAt: time.Now().Add(bgmCacheTTL),
			})
			return audioURL, hit.Tags
		}
	}
	return "", ""
}

// jamendoSearch 在 Jamendo 内部批量搜索 BGM 曲目（供 SearchBGMForSegment 调用）。
// 先按 fuzzytags 搜索，无结果时尝试 namesearch 降级。结果缓存 bgmCacheTTL。
func (s *BGMService) jamendoSearch(ctx context.Context, tenantID uint, query string) (string, string, string) {
	cacheKey := "jamendo:" + query
	if v, ok := s.queryCache.Load(cacheKey); ok {
		e := v.(bgmCacheEntry)
		if time.Now().Before(e.expiresAt) {
			return e.url, e.name, e.artist
		}
		s.queryCache.Delete(cacheKey)
	}

	// Fix ④: try fuzzytags (comma-separated keywords) first
	tracks, err := s.JamendoSearch(ctx, tenantID, JamendoSearchParams{Query: query, Limit: 5})
	if err != nil {
		logger.Errorf("[BGMService] Jamendo search error for %q: %v", query, err)
	}
	// namesearch fallback: search by full phrase as track name
	if len(tracks) == 0 {
		tracks2, err2 := s.jamendoNameSearch(ctx, tenantID, query)
		if err2 != nil {
			logger.Errorf("[BGMService] Jamendo namesearch error for %q: %v", query, err2)
		}
		tracks = append(tracks, tracks2...)
	}

	for _, t := range tracks {
		if u := t.PlayURL(); u != "" {
			// 上传到 OSS，保证 CDN 临时链接过期后仍可访问
			if s.storageSvc != nil {
				ossKey := fmt.Sprintf("bgm/%s.mp3", uuid.New().String())
				if ossURL, err := downloadURLAndUploadToOSS(ctx, s.storageSvc, u, ossKey); err == nil {
					u = ossURL
				} else {
					logger.Warnf("[BGMService] Jamendo OSS upload failed for %q: %v", t.Name, err)
				}
			}
			s.queryCache.Store(cacheKey, bgmCacheEntry{
				url: u, name: t.Name, artist: t.ArtistName,
				expiresAt: time.Now().Add(bgmCacheTTL),
			})
			return u, t.Name, t.ArtistName
		}
	}
	return "", "", ""
}

// jamendoNameSearch 使用 namesearch 参数在 Jamendo 中按曲名模糊搜索（fuzzytags 降级）。
func (s *BGMService) jamendoNameSearch(ctx context.Context, tenantID uint, query string) ([]JamendoTrack, error) {
	clientID, _ := s.bgmProviderCreds(tenantID, "jamendo")
	if clientID == "" {
		return nil, fmt.Errorf("Jamendo client_id not configured")
	}
	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("format", "json")
	params.Set("limit", "5")
	params.Set("namesearch", query)
	params.Set("vocalinstrumental", "instrumental")
	params.Set("order", "popularity_month")

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.jamendo.com/v3.0/tracks/?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("Jamendo namesearch HTTP %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Results []struct {
			ID                   string `json:"id"`
			Name                 string `json:"name"`
			ArtistName           string `json:"artist_name"`
			Audio                string `json:"audio"`
			AudioDownload        string `json:"audiodownload"`
			AudioDownloadAllowed bool   `json:"audiodownload_allowed"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	tracks := make([]JamendoTrack, 0, len(result.Results))
	for _, r := range result.Results {
		tracks = append(tracks, JamendoTrack{
			ID: r.ID, Name: r.Name, ArtistName: r.ArtistName,
			Audio: r.Audio, AudioDownload: r.AudioDownload,
			AudioDownloadAllowed: r.AudioDownloadAllowed,
		})
	}
	return tracks, nil
}

// ValidateCoverageBeforeSynthesis 在视频合成开始前校验 BGM 分段是否完整覆盖所有分镜。
// 如果存在未覆盖的分镜（gap），返回 error 阻止合成。
// bgmRepo 或 shots 为 nil/空时直接返回 nil（跳过校验）。
func (s *BGMService) ValidateCoverageBeforeSynthesis(_ context.Context, videoID uint, shots []*model.StoryboardShot, bgmRepo *repository.VideoBGMSegmentRepository) error {
	if bgmRepo == nil || len(shots) == 0 {
		return nil
	}
	segs, err := bgmRepo.ListByVideoID(videoID)
	if err != nil || len(segs) == 0 {
		// No BGM configured — not an error (BGM is optional)
		return nil
	}

	// Convert *model.VideoBGMSegment to bgmSegmentAnalysis for reuse of validateBGMCoverage
	analyses := make([]bgmSegmentAnalysis, 0, len(segs))
	for _, seg := range segs {
		if seg.Disabled {
			continue
		}
		analyses = append(analyses, bgmSegmentAnalysis{
			StartShotNo: seg.StartShotNo,
			EndShotNo:   seg.EndShotNo,
			Mood:        seg.Mood,
		})
	}
	if len(analyses) == 0 {
		return nil // all segments disabled; skip
	}

	warns := validateBGMCoverage(shots, analyses)
	if len(warns) > 0 {
		return fmt.Errorf("BGM coverage incomplete before synthesis (%d issue(s)): %s",
			len(warns), strings.Join(warns, "; "))
	}
	return nil
}

// RepairCoverageGaps 自动修复 BGM 分段覆盖空洞（gap）。
// 对于每个未被任何分段覆盖的 shot_no，将其前一个分段的 EndShotNo 延伸至覆盖该空洞，
// 并持久化修改。若无法确定前一分段（gap 在首段之前），则静默跳过。
// 此方法在合成流程中作为非致命修复步骤调用；任何 DB 错误均只记录日志，不中断合成。
func (s *BGMService) RepairCoverageGaps(ctx context.Context, videoID uint, shots []*model.StoryboardShot, bgmRepo *repository.VideoBGMSegmentRepository) error {
	if bgmRepo == nil || len(shots) == 0 {
		return nil
	}
	segs, err := bgmRepo.ListByVideoID(videoID)
	if err != nil || len(segs) == 0 {
		return nil
	}

	// 按 StartShotNo 排序分段（DB 返回顺序通常已排序，但确保正确）
	sortedSegs := make([]*model.VideoBGMSegment, len(segs))
	copy(sortedSegs, segs)
	for i := 1; i < len(sortedSegs); i++ {
		for j := i; j > 0 && sortedSegs[j].StartShotNo < sortedSegs[j-1].StartShotNo; j-- {
			sortedSegs[j], sortedSegs[j-1] = sortedSegs[j-1], sortedSegs[j]
		}
	}

	// 建立 shot_no → 覆盖状态的映射
	covered := make(map[int]bool, len(shots))
	for _, seg := range sortedSegs {
		if seg.Disabled {
			continue
		}
		for sno := seg.StartShotNo; sno <= seg.EndShotNo; sno++ {
			covered[sno] = true
		}
	}

	// 收集未覆盖的 shot_no，找到各自的前一个分段并延伸其 EndShotNo
	updated := make(map[uint]bool)
	for _, sh := range shots {
		if covered[sh.ShotNo] {
			continue
		}
		// 找到 StartShotNo 最大且 <= sh.ShotNo 的分段
		var bestSeg *model.VideoBGMSegment
		for _, seg := range sortedSegs {
			if seg.Disabled {
				continue
			}
			if seg.StartShotNo <= sh.ShotNo {
				if bestSeg == nil || seg.StartShotNo > bestSeg.StartShotNo {
					bestSeg = seg
				}
			}
		}
		if bestSeg == nil {
			// gap 出现在所有分段之前，将最早分段的 StartShotNo 延伸
			if len(sortedSegs) > 0 {
				bestSeg = sortedSegs[0]
				if bestSeg.StartShotNo > sh.ShotNo {
					logger.Printf("[BGMService] RepairCoverageGaps: extending seg %d StartShotNo %d→%d to cover shot %d",
						bestSeg.SeqNo, bestSeg.StartShotNo, sh.ShotNo, sh.ShotNo)
					bestSeg.StartShotNo = sh.ShotNo
					covered[sh.ShotNo] = true
					updated[bestSeg.ID] = true
				}
			}
			continue
		}
		if bestSeg.EndShotNo < sh.ShotNo {
			logger.Printf("[BGMService] RepairCoverageGaps: extending seg %d EndShotNo %d→%d to cover shot %d",
				bestSeg.SeqNo, bestSeg.EndShotNo, sh.ShotNo, sh.ShotNo)
			bestSeg.EndShotNo = sh.ShotNo
			covered[sh.ShotNo] = true
			updated[bestSeg.ID] = true
		}
	}

	// 持久化修改
	for _, seg := range sortedSegs {
		if updated[seg.ID] {
			if err := bgmRepo.Update(seg); err != nil {
				logger.Errorf("[BGMService] RepairCoverageGaps: failed to update seg %d: %v", seg.SeqNo, err)
			}
		}
	}
	_ = ctx
	return nil
}

// GenerateBGMSegments AI分析 + 并行搜索，一步完成全部BGM分段生成。
func (s *BGMService) GenerateBGMSegments(
	ctx context.Context,
	shots []*model.StoryboardShot,
	bgmRepo *repository.VideoBGMSegmentRepository,
	videoID uint,
	tenantID uint,
	userPrompt string,
	progressFn func(int),
) ([]*model.VideoBGMSegment, error) {
	progress := func(pct int) {
		if progressFn != nil {
			progressFn(pct)
		}
	}

	if s.bgmDir == "" && s.aiSvc == nil {
		logger.Errorf("[BGMService] WARNING: no audio source configured; segments will be saved without audio URLs")
	}

	// Step 1: AI 分析（生成分段计划）
	progress(5)
	segments, err := s.AnalyzeBGMForVideo(ctx, shots, bgmRepo, videoID, tenantID, userPrompt)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("BGM AI returned 0 segments for video %d", videoID)
	}
	logger.Printf("[BGMService] AI produced %d BGM segments for video %d", len(segments), videoID)
	progress(50)

	// Fix ③: Step 2: 并行搜索每个分段（最多 3 并发，Jamendo 有 API 限速）
	const maxConcurrency = 3
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var doneCount atomic.Int32
	total := len(segments)

	for _, seg := range segments {
		wg.Add(1)
		sem <- struct{}{}
		go func(sg *model.VideoBGMSegment) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.SearchBGMForSegment(ctx, tenantID, sg); err != nil {
				logger.Errorf("[BGMService] segment %d search error: %v", sg.SeqNo, err)
			}
			logger.Printf("[BGMService] segment %d (%s): url=%q source=%q", sg.SeqNo, sg.Mood, sg.URL, sg.Source)
			if bgmRepo != nil && sg.ID > 0 {
				if err := bgmRepo.Update(sg); err != nil {
					logger.Errorf("[BGMService] segment %d Update failed: %v", sg.SeqNo, err)
				}
			}
			// 自动发布到公共素材库（异步，失败不影响主流程）
			if s.assetRepo != nil && s.tagRepo != nil && sg.URL != "" {
				go s.saveBGMToAssetLibrary(context.Background(), sg)
			}
			n := int(doneCount.Add(1))
			progress(50 + n*50/total)
		}(seg)
	}
	wg.Wait()

	return segments, nil
}

// MixBGMWithDucking 将BGM混入视频，并在人声轨存在时自动压低BGM音量（Audio Ducking）。
// 当 audioTrackPath 非空时，使用人声作为 sidechain 信号压制 BGM；
// 否则退化为普通 MixBGM。
// audioTrackPath: 合并后的人声轨道文件路径（或空串表示无人声）
// duckingLevel: 闪避后 BGM 的目标音量（0.1-0.5，默认0.15）
// P1-3: ctx 传播至 ffprobe/ffmpeg。
func (s *BGMService) MixBGMWithDucking(ctx context.Context, videoPath, bgmSource, audioTrackPath, outputPath string, bgmVolume, duckingLevel float64) error {
	videoPath = strings.TrimPrefix(videoPath, "file://")
	bgmSource = strings.TrimPrefix(bgmSource, "file://")

	if videoPath == "" || bgmSource == "" || outputPath == "" {
		return fmt.Errorf("MixBGMWithDucking: invalid arguments")
	}
	if bgmVolume <= 0 {
		bgmVolume = 0.3
	}
	if duckingLevel <= 0 {
		duckingLevel = 0.15
	}

	bgmLocalPath := bgmSource
	if strings.HasPrefix(bgmSource, "http://") || strings.HasPrefix(bgmSource, "https://") {
		tmp, err := os.CreateTemp(inkframeTempDir(), "inkframe-bgm-*.mp3")
		if err != nil {
			return fmt.Errorf("MixBGMWithDucking: create temp file failed: %w", err)
		}
		tmp.Close()
		bgmLocalPath = tmp.Name()
		// P1-5: bounded download timeout
		dlCtx, dlCancel := context.WithTimeout(ctx, 3*time.Minute)
		dlErr := downloadFileCtx(dlCtx, bgmSource, bgmLocalPath)
		dlCancel()
		if dlErr != nil {
			os.Remove(bgmLocalPath)
			return fmt.Errorf("MixBGMWithDucking: download BGM failed: %w", dlErr)
		}
		defer os.Remove(bgmLocalPath)
	}

	// 若无人声轨，退化为普通混音（无需闪避）
	if audioTrackPath == "" {
		return s.MixBGM(ctx, videoPath, bgmSource, outputPath)
	}

	audioTrackPath = strings.TrimPrefix(audioTrackPath, "file://")

	// FFmpeg sidechaincompress 实现专业人声闪避（Audio Ducking）：
	// - 输入0: 视频文件（含视频流）
	// - 输入1: BGM 文件（循环）
	// - 输入2: 人声轨道（sidechain 信号）
	// 专业闪避参数：
	// - threshold=0.02（语音检测灵敏度）
	// - ratio=6:1（中等压缩，不会太死板）
	// - attack=10ms（快速响应，避免语音开头被切掉）
	// - release=600ms（自然释放，不会太突然回来）
	// - knee=3dB（软拐点，更自然）
	// - makeup=1（补偿压缩造成的音量损失）
	// 输入0=videoPath（含原始音轨）, 输入1=bgmLocalPath, 输入2=audioTrackPath（人声）
	filterComplex := fmt.Sprintf(
		"[1:a]volume=%.2f,equalizer=f=250:t=o:w=2:g=-2[bgm_eq];"+
			"[2:a]highpass=f=80[voice_hp];"+
			"[bgm_eq][voice_hp]sidechaincompress=threshold=0.02:ratio=6:attack=10:release=600:knee=3:makeup=1[bgm_ducked];"+
			"[voice_hp][bgm_ducked]amix=inputs=2:duration=first:weights=1 1[out]",
		bgmVolume,
	)

	args := []string{
		"-y",
		"-i", videoPath,
		"-stream_loop", "-1", "-i", bgmLocalPath,
		"-i", audioTrackPath,
		"-filter_complex", filterComplex,
		"-map", "0:v",
		"-map", "[out]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-shortest",
		outputPath,
	}

	if out, err := runFFmpegCtx(ctx, args...); err != nil { // P1-3: use ctx
		logger.Errorf("MixBGMWithDucking: ffmpeg failed: %v\n%s", err, string(out))
		// 降级到普通混音
		logger.Printf("MixBGMWithDucking: falling back to simple mix")
		return s.MixBGM(ctx, videoPath, bgmSource, outputPath)
	}
	return nil
}

// saveBGMToAssetLibrary 将 BGM 分段发布到公共素材库，以 URL 去重。
// 失败只记录日志，不影响主流程。
func (s *BGMService) saveBGMToAssetLibrary(ctx context.Context, seg *model.VideoBGMSegment) {
	if seg.URL == "" || seg.Source == "none" || seg.Source == "local" {
		return
	}
	if exists, err := s.assetRepo.ExistsByExternalID(seg.URL); err != nil {
		logger.Errorf("[BGMService] AssetLib: ExistsByExternalID error (seg %d): %v", seg.SeqNo, err)
		return
	} else if exists {
		return
	}

	title := seg.TrackName
	if title == "" {
		title = seg.Mood
	}

	asset := &model.Asset{
		TenantID:   0,
		CreatorID:  0,
		Scope:      model.AssetScopePublic,
		Type:       "audio",
		SubType:    "bgm",
		Title:      title,
		StorageURL: seg.URL,
		ExternalID: seg.URL,
		Source:     "crawled",
		Duration:   seg.DurationSecs,
		Status:     "active",
	}
	if err := s.assetRepo.Create(asset); err != nil {
		logger.Errorf("[BGMService] AssetLib: create asset failed (seg %d): %v", seg.SeqNo, err)
		return
	}

	// 标签：mood + tempo + artist + searchQueries 各词（提升标签维度搜索命中率）
	seen := make(map[string]bool)
	var tagNames []string
	addTag := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			tagNames = append(tagNames, s)
		}
	}
	addTag(seg.Mood)
	addTag(seg.Tempo)
	addTag(seg.TrackArtist)
	var queries []string
	if seg.SearchQueries != "" {
		_ = json.Unmarshal([]byte(seg.SearchQueries), &queries)
	}
	for _, q := range queries {
		for _, word := range strings.Fields(q) {
			if len(word) > 2 {
				addTag(strings.ToLower(word))
			}
		}
	}
	for _, name := range tagNames {
		tag, err := s.tagRepo.FindOrCreate(name, "audio")
		if err != nil {
			logger.Errorf("[BGMService] AssetLib: FindOrCreate tag %q failed: %v", name, err)
			continue
		}
		if err := s.tagRepo.AddToAsset(asset.ID, tag.ID, "ai", 1.0); err != nil {
			logger.Errorf("[BGMService] AssetLib: AddToAsset failed (asset %d tag %q): %v", asset.ID, name, err)
		}
	}
	_ = ctx
	logger.Printf("[BGMService] AssetLib: published asset %d mood=%q source=%s", asset.ID, seg.Mood, seg.Source)
}
