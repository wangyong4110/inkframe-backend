package ai

import (
	"context"
	"fmt"
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
		VolcModelText2ImgV3,    // 通用3.0-文生图
		VolcModelPortraitPhoto, // 人像写真3.0
		VolcModelSeedEditV3,    // SeedEdit3.0-指令编辑
		VolcModelDreamO,        // DreamO-角色特征保持
		VolcModelImageEffect,   // 图像特效
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

	switch reqKey {
	case VolcModelJimengT2Iv30, VolcModelJimengT2Iv31:
		params["prompt"] = req.Prompt
		if req.Size != "" {
			params["width"] = width
			params["height"] = height
		}
		// use_pre_llm 默认 true，仅在 Extra 中明确设为 false 时关闭
		if v, ok := req.Extra["use_pre_llm"].(bool); ok {
			params["use_pre_llm"] = v
		}

	case VolcModelText2ImgV3:
		params["prompt"] = req.Prompt
		params["width"] = width
		params["height"] = height
		if req.CFGScale > 0 {
			params["scale"] = req.CFGScale
		}
		if req.NegativePrompt != "" {
			params["negative_prompt"] = req.NegativePrompt
		}

	case VolcModelPortraitPhoto:
		if req.ReferenceImage != "" {
			params["image_input"] = req.ReferenceImage
		}
		if req.Prompt != "" {
			params["prompt"] = req.Prompt
		}
		if req.NegativePrompt != "" {
			params["negative_prompt"] = req.NegativePrompt
		}
		params["width"] = width
		params["height"] = height

	case VolcModelJimengI2Iv30:
		params["prompt"] = req.Prompt
		// 输入图（恰好1张，支持 URL 或 base64）
		p.setImageInput(params, req.ReferenceImage, "image_urls", "binary_data_base64")
		// scale：文本描述影响程度 float [0,1]，默认 0.5
		if req.CFGScale > 0 {
			s := req.CFGScale
			if s > 1 {
				s = 1
			}
			params["scale"] = s
		}
		if req.Size != "" {
			params["width"] = width
			params["height"] = height
		}

	case VolcModelSeedEditV3:
		params["prompt"] = req.Prompt
		if req.CFGScale > 0 {
			params["scale"] = req.CFGScale
		}
		if len(req.ReferenceImages) > 0 {
			p.setMultiImageInput(params, req.ReferenceImages, "image_urls", "binary_data_base64")
		} else {
			p.setImageInput(params, req.ReferenceImage, "image_urls", "binary_data_base64")
		}

	case VolcModelDreamO:
		params["prompt"] = req.Prompt
		params["width"] = width
		params["height"] = height
		if req.CFGScale > 0 {
			params["scale"] = req.CFGScale
		}
		if len(req.ReferenceImages) > 0 {
			p.setMultiImageInput(params, req.ReferenceImages, "image_urls", "binary_data_base64")
		} else {
			p.setImageInput(params, req.ReferenceImage, "image_urls", "binary_data_base64")
		}

	case VolcModelImageEffect:
		params["image_input1"] = req.ReferenceImage
		params["template_id"] = req.Style
		params["width"] = width
		params["height"] = height

	case VolcModelJimengT2Iv40:
		params["prompt"] = req.Prompt
		// 输入图（0~10张，仅支持 HTTP/HTTPS URL）
		var imgURLs []string
		collect := func(u string) {
			if u != "" && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && len(imgURLs) < 10 {
				imgURLs = append(imgURLs, u)
			}
		}
		if req.ReferenceImage != "" {
			collect(req.ReferenceImage)
		}
		for _, u := range req.ReferenceImages {
			if u != req.ReferenceImage {
				collect(u)
			}
		}
		if len(imgURLs) > 0 {
			params["image_urls"] = imgURLs
		}
		// 宽高（优先使用 size 字符串解析结果，未传 size 则由模型智能判断）
		if req.Size != "" {
			params["width"] = width
			params["height"] = height
		}
		// scale：文本描述影响程度 float [0,1]，默认 0.5
		if req.CFGScale > 0 {
			s := req.CFGScale
			if s > 1 {
				s = 1
			}
			params["scale"] = s
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
		params["prompt"] = req.Prompt
		// 输入图（0~14张，仅支持 HTTP/HTTPS URL）
		var imgURLs46 []string
		collect46 := func(u string) {
			if u != "" && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && len(imgURLs46) < 14 {
				imgURLs46 = append(imgURLs46, u)
			}
		}
		if req.ReferenceImage != "" {
			collect46(req.ReferenceImage)
		}
		for _, u := range req.ReferenceImages {
			if u != req.ReferenceImage {
				collect46(u)
			}
		}
		if len(imgURLs46) > 0 {
			params["image_urls"] = imgURLs46
		}
		if req.Size != "" {
			params["width"] = width
			params["height"] = height
		}
		// scale：文本描述影响程度 int [1,100]，默认 50
		// CFGScale [0,1] → 乘以100；CFGScale (1,100] → 直接取整
		if req.CFGScale > 0 {
			var scaleInt int
			if req.CFGScale <= 1 {
				scaleInt = int(req.CFGScale * 100)
			} else {
				scaleInt = int(req.CFGScale)
			}
			if scaleInt < 1 {
				scaleInt = 1
			} else if scaleInt > 100 {
				scaleInt = 100
			}
			params["scale"] = scaleInt
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
				errMsg := fmt.Sprintf("即梦AI 获取结果失败 code=%d: %s", int(code), msg)
				// 50511=图像后审核拦截，50400=文本预审核拦截，统一标记为内容审核错误以触发上层重试
				if int(code) == 50511 || int(code) == 50400 {
					errMsg = ErrPrefixSensitiveContent + errMsg
				}
				return &ImageResponse{
					Error:     errMsg,
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
