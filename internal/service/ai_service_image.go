package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/commons"
	"github.com/inkframe/inkframe-backend/internal/logger"
)

// resolveStyleCategory 从风格库（ImageStylePreset.PromptCategory，管理员可在 /image-style-presets
// 管理页编辑）读取风格 ID 归入的大类，用于选择匹配的质量提升词/冲突清理词。
// 返回值："realistic" / "anime" / "classic_illustration" / "dark_stylized" / "pixel" / "render_3d" / "" (未知)
func resolveStyleCategory(styleID string) string {
	if c, ok := lookupStylePresetFromCache(styleID); ok {
		return c.category
	}
	return ""
}

// GenerateImage 调用AI生成图像。DB 是唯一权威来源（同 GenerateCharacterThreeView 的 auto 分支）：
// 按 tenantID 加载已配置的 IMAGE 类型 provider，依次尝试直到成功；无 providerRepo 时退回静态
// aiManager（config.yaml/env 静态注册场景）。
func (s *AIService) GenerateImage(ctx context.Context, tenantID uint, options *ImageGenerationOptions) (*GeneratedImage, error) {
	pMeta, m, err := s.getTenantProvider(tenantID, commons.Image, "")
	if err != nil {
		return nil, err
	}
	p, ok := pMeta.(ai.ImageProvider)
	if !ok {
		return nil, fmt.Errorf("configured provider %q does not support image generation", pMeta.GetName())
	}

	category := resolveStyleCategory(options.ImageStyle)

	resp, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{
		Model:          m.Name,
		Prompt:         options.Prompt + "\n" + category,
		NegativePrompt: options.NegativePrompt,
		Size:           options.Size,
	})
	if err != nil {
		return nil, err
	}
	return &GeneratedImage{
		URL:    s.uploadImageToStorage(ctx, resp.URL),
		Width:  resp.Width,
		Height: resp.Height,
	}, nil
}

// resolveReferenceImagesForProviders 下载参考图并按需转换为 base64。
// extFirst/extRefs：转换后的 base64（fetch 失败的图直接丢弃，见函数内注释——不回退到原始
// URL，因为本服务器都下载不到的图，第三方 AI 服务器同样下载不到）。
// refURLFirst/refURLSlice：原始可公开访问的 HTTP URL；若发生了 base64 转换，只保留转换
// 成功的对应项（避免把已过期的签名 URL 传给 Seedream 服务端二次下载导致失败）。
// 若所有参考图都是 HTTP URL 且候选 provider 都接受 URL（不强制要求 base64），则跳过下载。
func (s *AIService) resolveReferenceImagesForProviders(ctx context.Context, modelName string, referenceImages []string) (extFirst string, extRefs []string, refURLFirst string, refURLSlice []string) {
	firstRef := ""
	if len(referenceImages) > 0 {
		firstRef = referenceImages[0]
	}

	// 预先将参考图转换为 base64，供 non-volcengine-visual 提供商使用。
	// volcengine-visual 自身在 setImageInput/setMultiImageInput 中处理相对路径；
	// 其他提供商（doubao/kling-image 等）使用官方 image 字段，必须提供 base64 data URI 或可公开访问的 URL。
	// 注意：OSS 图片可能存储在私有桶或签名 URL 中，Seedream/Kling 服务器无法直接访问；
	// 因此对所有参考图（包括 https:// 绝对 URL）均主动下载并转为 base64，确保提供商能访问图片数据。
	resolveForExternal := func(url string) string {
		if url == "" {
			return ""
		}
		// 始终 fetch 转 base64：绝对 URL（OSS）可能不被第三方 AI 服务器访问；
		// 相对路径由 fetchImageAsBase64 拼接 serverBaseURL 处理。
		b64 := s.fetchImageAsBase64(ctx, url)
		if b64 != "" {
			logger.Printf("GenerateCharacterThreeViewMulti: resolved ref %q → base64 len=%d", url, len(b64))
			return b64
		}
		// fetchImageAsBase64 失败（403/404/网络错误）：说明 URL 已失效（如 Volcengine TOS 签名 URL 过期）。
		// 不再 fallback 到原始 URL：若连本服务器都 403，Seedream 服务器下载同样会 403，
		// 传入无效 URL 只会让 API 返回 InvalidParameter，不如直接丢弃（返回空串跳过此参考图）。
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			logger.Warnf("GenerateCharacterThreeViewMulti: ref %q inaccessible (base64 fetch failed), dropping from reference images", url)
			return ""
		}
		logger.Errorf("GenerateCharacterThreeViewMulti: cannot resolve ref %q — relative path with no dbMediaReader and no serverBaseURL configured; ref image will be skipped", url)
		return ""
	}
	// 提取原始 HTTP URL（同步、零延迟），供支持 URL 的接口优先使用（避免 base64 大小限制）。
	// 必须在 resolveForExternal 之前完成，因为 buildReq 中 DreamO 优先判断 refURLFirst。
	if strings.HasPrefix(firstRef, "http://") || strings.HasPrefix(firstRef, "https://") {
		refURLFirst = firstRef
	}
	for _, r := range referenceImages {
		if strings.HasPrefix(r, "http://") || strings.HasPrefix(r, "https://") {
			refURLSlice = append(refURLSlice, r)
		}
	}

	// 判断是否需要 base64：
	// - volcengine-visual 的新一代 Jimeng 模型（T2Iv40/Seedream46/T2Iv31/T2Iv30/I2Iv30）仅使用 image_urls（HTTP URL），base64 无效
	// - volcengine-visual 的 DreamO/SeedEditV3：有 HTTP URL 时优先走 URL 分支（buildReq 中已判断）
	// - 非 volcengine-visual 提供商（doubao/kling-image 等）：必须 base64（OSS 私有 URL 无法被第三方服务器访问）
	// 只有当"所有参考图都有 HTTP URL + 所有提供商都是 volcengine-visual"时，才可完全跳过 base64 下载。
	allRefsAreHTTP := len(refURLSlice) == len(referenceImages) && len(referenceImages) > 0

	needsBase64 := !(allRefsAreHTTP)

	// 并行下载参考图转 base64（仅在需要时执行；保持原始顺序）。
	// 旧实现串行下载：N 张图 × 最多 30s/张 = 潜在数分钟阻塞。
	// 并行后：总耗时 ≈ 最慢的单张图下载时间。
	extRefs = make([]string, len(referenceImages))
	if needsBase64 && len(referenceImages) > 0 {
		var wg sync.WaitGroup
		for i, r := range referenceImages {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				extRefs[idx] = resolveForExternal(url)
			}(i, r)
		}
		wg.Wait()
		// extRefs 仍与 referenceImages 下标对齐（未压缩）。
		// 过滤 refURLSlice / refURLFirst：只保留 base64 下载成功的对应 URL。
		// 过期签名 URL（如 Volcengine TOS 24h 有效期）会让 extRefs[i] 为空；
		// 若不过滤，这些 URL 仍会以 ReferenceURLs 优先通道传给 Seedream，
		// Seedream 服务端下载时同样 403 → InvalidParameter 生成失败。
		var filteredURLs []string
		for i, r := range referenceImages {
			if extRefs[i] != "" && (strings.HasPrefix(r, "http://") || strings.HasPrefix(r, "https://")) {
				filteredURLs = append(filteredURLs, r)
			}
		}
		refURLSlice = filteredURLs
		if len(refURLSlice) > 0 {
			refURLFirst = refURLSlice[0]
		} else {
			refURLFirst = ""
		}
		// 压缩掉空槽（下载失败的项），保持非空项有序
		compact := extRefs[:0]
		for _, v := range extRefs {
			if v != "" {
				compact = append(compact, v)
			}
		}
		extRefs = compact
	} else {
		extRefs = extRefs[:0]
	}
	if len(extRefs) > 0 {
		extFirst = extRefs[0]
	}
	return extFirst, extRefs, refURLFirst, refURLSlice
}

// imageStorageHintKey is the context key for ImageStorageHint.
type imageStorageHintKey struct{}

// ImageStorageHint carries novel/chapter metadata for OSS key building.
type ImageStorageHint struct {
	NovelTitle string
	ChapterNo  int // 0 = novel-level, non-zero = chapter-level
}

// WithImageStorageHint enriches a context with novel/chapter metadata for OSS key building.
func WithImageStorageHint(ctx context.Context, hint ImageStorageHint) context.Context {
	return context.WithValue(ctx, imageStorageHintKey{}, hint)
}

// fetchImageAsBase64 下载图片并返回 base64 编码的原始数据（不含 data URI 前缀）。
// 对 /api/v1/media/* 相对路径优先用 dbMediaReader 直接读 DB，避免依赖 serverBaseURL（127.0.0.1）。
// 下载失败时返回空字符串，由调用方决定是否降级。
func (s *AIService) fetchImageAsBase64(ctx context.Context, imageURL string) string {
	if imageURL == "" {
		return ""
	}
	dlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, imageURL, nil)
	if err != nil {
		logger.Errorf("fetchImageAsBase64: build request for %s: %v", imageURL, err)
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Errorf("fetchImageAsBase64: download %s: %v", imageURL, err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Printf("fetchImageAsBase64: HTTP %d for %s", resp.StatusCode, imageURL)
		return ""
	}
	const maxFetchSize = 20 << 20 // 20 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchSize+1))
	if err != nil {
		logger.Errorf("fetchImageAsBase64: read body: %v", err)
		return ""
	}
	if len(data) > maxFetchSize {
		logger.Printf("fetchImageAsBase64: image too large (>20MB) from %s", imageURL)
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// imageExtFromContentType 根据 Content-Type 返回图片文件扩展名。
func imageExtFromContentType(ct string) string {
	switch {
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		return ".jpg"
	case strings.Contains(ct, "png"):
		return ".png"
	case strings.Contains(ct, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}
