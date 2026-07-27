package ai

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	volcvisual "github.com/volcengine/volc-sdk-golang/service/visual"
)

// ProviderNameVolcengineVisual is the canonical name for the Volcengine Visual AI provider.
const ProviderNameVolcengineVisual = "volcengine-visual"

// 即梦AI 图像生成模型 req_key
// 文档：https://www.volcengine.com/docs/86081/1804546
const (
	// 通用3.0-文生图
	VolcModelText2ImgV3 = "high_aes_general_v30l_zt2i"
	// 图生图3.0-人像写真（需要输入参考图 image_input URL）
	VolcModelPortraitPhoto = "i2i_portrait_photo"
	// 图生图3.0-指令编辑 SeedEdit3.0（需要 image_urls[] 或 binary_data_base64[] + prompt）
	VolcModelSeedEditV3 = "seededit_v3.0"
	// 图生图3.0-角色特征保持 DreamO（需要 image_urls[] 或 binary_data_base64[] + prompt）
	VolcModelDreamO = "seed3l_single_ip"
	// 图像特效（需要 image_input1 URL + template_id）
	VolcModelImageEffect = "i2i_multi_style_zx2x"
	// 即梦4.0 文生图/图像编辑（支持0~10张输入图，单次最多输出15张，支持4K）
	VolcModelJimengT2Iv40 = "jimeng_t2i_v40"
	// 即梦4.6 图像生成（基于Seedream4.0，聚焦人像写真/平面设计/风格化，支持0~14张输入图）
	VolcModelJimengSeedream46 = "jimeng_seedream46_cvtob"
	// 即梦文生图3.0（文字响应/图文排版/人像质感显著提升，纯文生图，支持2K输出）
	VolcModelJimengT2Iv30 = "jimeng_t2i_v30"
	// 即梦文生图3.1（画面美感/风格精准/细节丰富度升级，兼具文字响应，支持2K输出）
	VolcModelJimengT2Iv31 = "jimeng_t2i_v31"
	// 即梦图生图3.0智能参考（精准执行编辑指令，真实图像/海报设计场景卓越，单图输入）
	VolcModelJimengI2Iv30 = "jimeng_i2i_v30"
)

// VolcengineVisualProvider 火山引擎即梦AI图像生成提供者
//
// 鉴权：通过 volc-sdk-golang 自动完成 HMAC-SHA256 AK/SK 签名
// API：两步异步接口（CVSync2AsyncSubmitTask → CVSync2AsyncGetResult）
//
// 文档：https://www.volcengine.com/docs/86081/1804546
type VolcengineVisualProvider struct {
	svc *volcvisual.Visual
}

// NewVolcengineVisualProvider 创建即梦AI图像提供者
func NewVolcengineVisualProvider(accessKey, secretKey string) *VolcengineVisualProvider {
	svc := volcvisual.NewInstance()
	svc.Client.SetAccessKey(accessKey)
	svc.Client.SetSecretKey(secretKey)
	return &VolcengineVisualProvider{svc: svc}
}

func (p *VolcengineVisualProvider) GetName() string { return ProviderNameVolcengineVisual }

func (p *VolcengineVisualProvider) GetModels() []string {
	return []string{
		VolcModelJimengSeedream46, // 即梦4.6-人像写真/平面设计/风格化（旗舰）
		VolcModelJimengT2Iv40,     // 即梦4.0-文生图/图像编辑
		VolcModelJimengI2Iv30,     // 即梦3.0-图生图智能参考（编辑/真实图/海报）
		VolcModelJimengT2Iv31,     // 即梦3.1-文生图（美感/风格/细节升级）
		VolcModelJimengT2Iv30,     // 即梦3.0-文生图（文字/排版/人像）
		VolcModelText2ImgV3,       // 通用3.0-文生图
		VolcModelPortraitPhoto,    // 人像写真3.0
		VolcModelSeedEditV3,       // SeedEdit3.0-指令编辑
		VolcModelDreamO,           // DreamO-角色特征保持
		VolcModelImageEffect,      // 图像特效
	}
}

func (p *VolcengineVisualProvider) HealthCheck(ctx context.Context) error {
	probe := map[string]interface{}{
		"req_key":  VolcModelText2ImgV3,
		"task_id":  "health_check_probe",
		"req_json": `{"return_url":true}`,
	}
	_, _, err := p.svc.CVSync2AsyncGetResult(probe)
	if err != nil {
		return fmt.Errorf("volcengine-visual: HealthCheck 失败 (AK/SK 可能无效): %w", err)
	}
	return nil
}

// ImageGenerate 调用即梦AI生成图像（异步接口，内部自动轮询）
//
// 参数映射：
//   - Model          → req_key（见上方模型常量）
//   - Prompt         → prompt
//   - ReferenceImage → image_input / image_urls / binary_data_base64（自动判断 URL 或 base64）
//   - Style          → template_id（仅 i2i_multi_style_zx2x 图像特效模型）
//   - CFGScale       → scale（SeedEdit3.0/DreamO）
//   - Seed           → seed
//   - Size           → width x height（格式 "1024x1024"）
func (p *VolcengineVisualProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	start := time.Now()

	reqKey := req.Model
	if reqKey == "" {
		reqKey = VolcModelText2ImgV3
	}

	// Step 1：构建参数并提交任务
	submitParams := p.buildSubmitParams(reqKey, req)
	taskID, err := p.submitTask(submitParams)
	if err != nil {
		return &ImageResponse{Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()}, nil
	}

	// Step 2：轮询结果（最多等待 5 分钟）
	return p.pollResult(ctx, reqKey, taskID, start)
}

// buildSubmitParams 根据模型类型构建提交参数
func (p *VolcengineVisualProvider) buildSubmitParams(reqKey string, req *ImageGenerateRequest) map[string]interface{} {
	seed := int64(-1)
	if req.Seed != 0 {
		seed = req.Seed
	}
	params := map[string]interface{}{
		"req_key": reqKey,
		"seed":    seed,
	}

	width, height := parseSizeWH(req.Size)
	// 即梦AI（Volcengine Jimeng）系列模型对 prompt 有 800 字符的硬性长度限制，超出会被 API
	// 直接拒绝（code=50200 "prompt can not be more than 800 chars"）。模板层的字数指导只是
	// 建议，LLM 不总是遵守，这里做最后一道保险，超出时截断而不是让整次生成失败。
	prompt := truncatePromptRunes(req.Prompt, 800)

	switch reqKey {
	case VolcModelJimengT2Iv30, VolcModelJimengT2Iv31:
		params["prompt"] = prompt
		if req.Size != "" {
			params["width"] = width
			params["height"] = height
		}
		// negative_prompt：即梦3.0/3.1 支持负向提示词，明确排除低质量/解剖异常/模糊面部等词，
		// 显著提升生成质量。不传时模型无任何约束，极易出现模糊、变形、水印等问题。
		if req.NegativePrompt != "" {
			params["negative_prompt"] = req.NegativePrompt
		}
		// use_pre_llm：API 侧默认 true（内置 LLM 二次改写 prompt，额外增加 10-30s 延迟）。
		// 分镜 prompt 已由系统 LLM 精心构造（100+ 词结构化描述），内置 LLM 改写反而会
		// 将详细描述缩减为通用短句，降低构图/光线/角色描述的精确度。
		// 默认禁用；调用方可通过 Extra["use_pre_llm"]=true 显式开启。
		params["use_pre_llm"] = false
		if v, ok := req.Extra["use_pre_llm"].(bool); ok {
			params["use_pre_llm"] = v
		}

	case VolcModelText2ImgV3:
		params["prompt"] = prompt
		params["width"] = width
		params["height"] = height
		if req.NegativePrompt != "" {
			params["negative_prompt"] = req.NegativePrompt
		}

	case VolcModelPortraitPhoto:
		if img := pickSingleRef(req.ReferenceURL, req.ReferenceImage); img != "" {
			params["image_input"] = img
		}
		if prompt != "" {
			params["prompt"] = prompt
		}
		if req.NegativePrompt != "" {
			params["negative_prompt"] = req.NegativePrompt
		}
		params["width"] = width
		params["height"] = height

	case VolcModelJimengI2Iv30:
		params["prompt"] = prompt
		// 输入图（恰好1张，支持 URL 或 base64）。ReferenceURL 优先：调用方在判断所有参考图
		// 均为可直接访问的 HTTP URL 时会跳过 base64 转换，此时 ReferenceImage 为空，
		// 必须回退读取 ReferenceURL，否则参考图会被静默丢弃（image_input 缺失）。
		p.setImageInput(params, pickSingleRef(req.ReferenceURL, req.ReferenceImage), "image_urls", "binary_data_base64")
		// scale：文本描述影响程度 float [0,1]，默认 0.5。
		if req.Size != "" {
			params["width"] = width
			params["height"] = height
		}

	case VolcModelSeedEditV3:
		params["prompt"] = prompt
		if imgs := pickMultiRef(req.ReferenceURLs, req.ReferenceImages); len(imgs) > 0 {
			p.setMultiImageInput(params, imgs, "image_urls", "binary_data_base64")
		} else {
			p.setImageInput(params, pickSingleRef(req.ReferenceURL, req.ReferenceImage), "image_urls", "binary_data_base64")
		}

	case VolcModelDreamO:
		params["prompt"] = prompt
		params["width"] = width
		params["height"] = height
		if imgs := pickMultiRef(req.ReferenceURLs, req.ReferenceImages); len(imgs) > 0 {
			p.setMultiImageInput(params, imgs, "image_urls", "binary_data_base64")
		} else {
			p.setImageInput(params, pickSingleRef(req.ReferenceURL, req.ReferenceImage), "image_urls", "binary_data_base64")
		}

	case VolcModelImageEffect:
		params["image_input1"] = pickSingleRef(req.ReferenceURL, req.ReferenceImage)
		params["template_id"] = req.Style
		params["width"] = width
		params["height"] = height

	case VolcModelJimengT2Iv40:
		params["prompt"] = prompt
		// 输入图（0~10张，仅支持 HTTP/HTTPS URL）
		// 优先使用 buildReq 预筛的 ReferenceURLs（纯 HTTP URL 列表），避免 ReferenceImages 中
		// 夹杂本地相对路径（/api/v1/media/...）时被 HTTP 检查过滤、导致后续参考图全部丢失。
		var imgURLs []string
		seen40 := make(map[string]bool)
		addURL40 := func(u string) {
			if u != "" && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && !seen40[u] && len(imgURLs) < 10 {
				seen40[u] = true
				imgURLs = append(imgURLs, u)
			}
		}
		if len(req.ReferenceURLs) > 0 {
			for _, u := range req.ReferenceURLs {
				addURL40(u)
			}
		} else {
			addURL40(req.ReferenceURL)
			addURL40(req.ReferenceImage)
			for _, u := range req.ReferenceImages {
				addURL40(u)
			}
		}
		if len(imgURLs) > 0 {
			params["image_urls"] = imgURLs
		}
		// 宽高（优先使用 size 字符串解析结果，未传 size 则由模型智能判断）
		// 即梦4.0 要求 width*height ∈ [1024*1024, 4096*4096]，draft 档位的 1280x720 低于下限。
		if req.Size != "" {
			w40, h40 := ensureMinPixelArea(width, height, 1024*1024)
			params["width"] = w40
			params["height"] = h40
		}
		// force_single / min_ratio / max_ratio 通过 Extra 透传
		if req.Extra != nil {
			if v, ok := req.Extra["force_single"].(bool); ok {
				params["force_single"] = v
			}
			if v, ok := req.Extra["min_ratio"].(float64); ok {
				params["min_ratio"] = v
			}
			if v, ok := req.Extra["max_ratio"].(float64); ok {
				params["max_ratio"] = v
			}
		}

	case VolcModelJimengSeedream46:
		params["prompt"] = prompt
		// 输入图（0~14张，仅支持 HTTP/HTTPS URL）
		// 优先使用 buildReq 预筛的 ReferenceURLs（纯 HTTP URL 列表），避免 ReferenceImages 中
		// 夹杂本地相对路径（/api/v1/media/...）时被 HTTP 检查过滤、导致后续参考图全部丢失。
		var imgURLs46 []string
		seen46 := make(map[string]bool)
		addURL46 := func(u string) {
			if u != "" && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && !seen46[u] && len(imgURLs46) < 14 {
				seen46[u] = true
				imgURLs46 = append(imgURLs46, u)
			}
		}
		if len(req.ReferenceURLs) > 0 {
			// ReferenceURLs 已由 ai_service.go buildReq 过滤为纯 HTTP URL，直接使用
			for _, u := range req.ReferenceURLs {
				addURL46(u)
			}
		} else {
			// fallback：从单张字段逐一提取
			addURL46(req.ReferenceURL)
			addURL46(req.ReferenceImage)
			for _, u := range req.ReferenceImages {
				addURL46(u)
			}
		}
		if len(imgURLs46) > 0 {
			params["image_urls"] = imgURLs46
		}
		// 即梦4.6 要求 width*height ∈ [1024*1024, 4096*4096]，draft 档位的 1280x720（921,600px）
		// 低于下限，会被 API 拒绝（code=50200 "width*height must in [1024 * 1024, 4096 * 4096]"）。
		if req.Size != "" {
			w46, h46 := ensureMinPixelArea(width, height, 1024*1024)
			params["width"] = w46
			params["height"] = h46
		}
		if req.Extra != nil {
			if v, ok := req.Extra["force_single"].(bool); ok {
				params["force_single"] = v
			}
			if v, ok := req.Extra["min_ratio"].(float64); ok {
				params["min_ratio"] = v
			}
			if v, ok := req.Extra["max_ratio"].(float64); ok {
				params["max_ratio"] = v
			}
		}
	}

	return params
}

// pickSingleRef 优先返回 ReferenceURL（调用方在所有参考图均为公网可访问 HTTP URL 时会跳过
// base64 转换，此时 ReferenceImage 为空）；为空时回退到 ReferenceImage（base64 或 URL，
// 由 setImageInput 按前缀自动判断）。避免参考图在 URL-only 场景下被静默丢弃。
func pickSingleRef(url, image string) string {
	if url != "" {
		return url
	}
	return image
}

// pickMultiRef 同 pickSingleRef，适用于多张参考图场景。
func pickMultiRef(urls, images []string) []string {
	if len(urls) > 0 {
		return urls
	}
	return images
}

func (p *VolcengineVisualProvider) setImageInput(params map[string]interface{}, image, urlField, b64Field string) {
	if image == "" {
		return
	}
	if strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		params[urlField] = []string{image}
	} else {
		params[b64Field] = []string{image}
	}
}

func (p *VolcengineVisualProvider) setMultiImageInput(params map[string]interface{}, images []string, urlField, b64Field string) {
	var urls, b64s []string
	for _, img := range images {
		if img == "" {
			continue
		}
		if strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://") {
			urls = append(urls, img)
		} else {
			b64s = append(b64s, img)
		}
	}
	if len(urls) > 0 {
		params[urlField] = urls
	}
	if len(b64s) > 0 {
		params[b64Field] = b64s
	}
}

// submitTask 通过 SDK 提交异步任务，返回 task_id
func (p *VolcengineVisualProvider) submitTask(params map[string]interface{}) (string, error) {
	log.Printf("[volcengine-visual] submitTask params=%v", redactBase64Fields(params, "binary_data_base64"))

	resp, _, err := p.svc.CVSync2AsyncSubmitTask(params)
	if err != nil {
		return "", fmt.Errorf("即梦AI 提交任务失败: %w", err)
	}

	code, _ := resp["code"].(float64)
	if int(code) != 10000 {
		msg, _ := resp["message"].(string)
		return "", fmt.Errorf("即梦AI 提交任务失败 code=%d: %s", int(code), msg)
	}

	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		return "", fmt.Errorf("即梦AI: 响应缺少 data 字段")
	}
	taskID, _ := data["task_id"].(string)
	if taskID == "" {
		return "", fmt.Errorf("即梦AI 未返回 task_id")
	}
	return taskID, nil
}

// pollResult 轮询任务结果，最多等待 5 分钟（或父 context 更早超时时以父为准）
func (p *VolcengineVisualProvider) pollResult(ctx context.Context, reqKey, taskID string, start time.Time) (*ImageResponse, error) {
	getParams := map[string]interface{}{
		"req_key":  reqKey,
		"task_id":  taskID,
		"req_json": `{"return_url":true}`,
	}

	const maxPollDuration = 5 * time.Minute
	deadline := time.Now().Add(maxPollDuration)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	pollCtx, cancelPoll := context.WithDeadline(ctx, deadline)
	defer cancelPoll()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("即梦AI: 任务超时（taskID=%s）", taskID)
		case <-ticker.C:
			resp, _, err := p.svc.CVSync2AsyncGetResult(getParams)
			if err != nil {
				continue // 网络瞬时错误，继续轮询
			}

			code, _ := resp["code"].(float64)
			if int(code) != 10000 {
				msg, _ := resp["message"].(string)
				return &ImageResponse{
					Error:     fmt.Sprintf("即梦AI 获取结果失败 code=%d: %s", int(code), msg),
					LatencyMs: time.Since(start).Milliseconds(),
				}, nil
			}

			data, _ := resp["data"].(map[string]interface{})
			if data == nil {
				continue
			}
			status, _ := data["status"].(string)

			switch status {
			case "done":
				// 继续处理结果
			case "not_found":
				return &ImageResponse{
					Error:     fmt.Sprintf("即梦AI: 任务未找到（taskID=%s）", taskID),
					LatencyMs: time.Since(start).Milliseconds(),
				}, nil
			case "expired":
				return &ImageResponse{
					Error:     fmt.Sprintf("即梦AI: 任务已过期（taskID=%s）", taskID),
					LatencyMs: time.Since(start).Milliseconds(),
				}, nil
			default:
				continue // in_queue / generating，继续轮询
			}

			// 提取图片 URL（支持单张和多张组图）
			if urls, ok := data["image_urls"].([]interface{}); ok && len(urls) > 0 {
				var allURLs []string
				for _, u := range urls {
					if s, ok := u.(string); ok && s != "" {
						allURLs = append(allURLs, s)
					}
				}
				if len(allURLs) > 0 {
					ir := &ImageResponse{
						URL:       allURLs[0],
						LatencyMs: time.Since(start).Milliseconds(),
					}
					if len(allURLs) > 1 {
						ir.URLs = allURLs
					}
					return ir, nil
				}
			}
			return &ImageResponse{
				Error:     "即梦AI: 任务完成但未返回图片 URL",
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}
	}
}

// parseSizeWH 将尺寸字符串转换为宽高，默认 1328x1328。
// 支持两种格式：
//   - "1024x1024" — 直接宽高像素
//   - "16:9" / "9:16" / "4:3" / "1:1" 等宽高比 — 以 1328px 为长边换算
func parseSizeWH(size string) (int, int) {
	const base = 1328
	if size == "" {
		return base, base
	}
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err == nil && w > 0 && h > 0 {
		return w, h
	}
	var rw, rh int
	if _, err := fmt.Sscanf(size, "%d:%d", &rw, &rh); err == nil && rw > 0 && rh > 0 {
		if rw >= rh {
			return base, base * rh / rw
		}
		return base * rw / rh, base
	}
	return base, base
}

// ensureMinPixelArea 按比例放大 width/height（保持长宽比），使 width*height >= minArea。
// 即梦4.0/4.6 家族（jimeng_t2i_v40/jimeng_seedream46_cvtob）要求 width*height 落在
// [1024*1024, 4096*4096] 区间内；draft 质量档位常用的 1280x720（=921,600px）低于下限，
// 直接提交会被 API 拒绝（code=50200 "width*height must in [1024 * 1024, 4096 * 4096]"）。
// 已经满足下限时原样返回，不影响其他档位/模型。
func ensureMinPixelArea(width, height, minArea int) (int, int) {
	if width <= 0 || height <= 0 || width*height >= minArea {
		return width, height
	}
	scale := math.Sqrt(float64(minArea) / float64(width*height))
	return int(math.Ceil(float64(width) * scale)), int(math.Ceil(float64(height) * scale))
}

// truncatePromptRunes 按 Unicode 字符（而非字节）截断 prompt，避免把中文等多字节字符从中间切断。
// 即梦AI 系列模型对 prompt 有硬性长度上限，超出会被 API 直接拒绝（code=50200），模板层的字数
// 指导只是建议、LLM 不总是遵守，这里在提交前做最后一道保险。
func truncatePromptRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	log.Printf("[volcengine-visual] prompt exceeds %d chars (actual %d), truncating", maxRunes, len(runes))
	return string(runes[:maxRunes])
}

// ─── AIProvider 接口的剩余方法（不支持）─────────────────────────────────────

func (p *VolcengineVisualProvider) Generate(_ context.Context, _ *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("volcengine-visual 不支持文本生成")
}
func (p *VolcengineVisualProvider) GenerateStream(_ context.Context, _ *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("volcengine-visual 不支持流式生成")
}
func (p *VolcengineVisualProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("volcengine-visual 不支持向量嵌入")
}
func (p *VolcengineVisualProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("volcengine-visual 不支持音频生成")
}

func init() {
	// 不注册 SelectModel：模型由用户在模型管理里显式配置（entry.Model），必须原样使用，严禁
	// 使用用户未配置的模型。这里原来有一套按参考图数量/风格/一致性权重自动切换旧版模型
	// （DreamO/SeedEditV3/PortraitPhoto/Text2ImgV3）的逻辑，会在用户已经选定某个模型的情况下
	// 悄悄换成别的模型，效果差且让人无法用配置界面预测实际会调用哪个模型。SelectModel 留空时
	// selectImageModel 会直接返回 entry.Model。
	RegisterImageEngineTraits(ProviderNameVolcengineVisual, ImageEngineTraits{
		SupportsReferenceImage: true,
	})
}
