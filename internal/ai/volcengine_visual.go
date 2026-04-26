package ai

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 火山引擎 Visual API 常量
const (
	volcengineVisualEndpoint = "https://visual.volcengineapi.com"
	volcengineVisualVersion  = "2022-08-31"
	volcengineVisualRegion   = "cn-north-1"
	volcengineVisualService  = "cv"
	volcengineVisualHost     = "visual.volcengineapi.com"

	// 异步任务接口（3.0 系列模型均使用）
	volcengineActionSubmit    = "CVSync2AsyncSubmitTask"
	volcengineActionGetResult = "CVSync2AsyncGetResult"
)

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
)

// VolcengineVisualProvider 火山引擎即梦AI图像生成提供者
//
// 鉴权：Volcengine HMAC-SHA256 AK/SK 签名
// API：两步异步接口
//   - 提交任务：CVSync2AsyncSubmitTask
//   - 查询结果：CVSync2AsyncGetResult
//
// 文档：https://www.volcengine.com/docs/86081/1804546
type VolcengineVisualProvider struct {
	accessKey string
	secretKey string
	client    *http.Client
}

// NewVolcengineVisualProvider 创建即梦AI图像提供者
func NewVolcengineVisualProvider(accessKey, secretKey string) *VolcengineVisualProvider {
	return &VolcengineVisualProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *VolcengineVisualProvider) GetName() string { return "volcengine-visual" }

func (p *VolcengineVisualProvider) GetModels() []string {
	return []string{
		VolcModelText2ImgV3,   // 通用3.0-文生图
		VolcModelPortraitPhoto, // 人像写真3.0
		VolcModelSeedEditV3,   // SeedEdit3.0-指令编辑
		VolcModelDreamO,       // DreamO-角色特征保持
		VolcModelImageEffect,  // 图像特效
	}
}

func (p *VolcengineVisualProvider) HealthCheck(ctx context.Context) error {
	if p.accessKey == "" || p.secretKey == "" {
		return fmt.Errorf("volcengine-visual: AccessKey 和 SecretKey 不能为空")
	}
	// 用伪 task_id 调用 GetResult 验证 AK/SK 签名：
	//   - 鉴权失败 → HTTP 非200 → doRequest 返回 error
	//   - 鉴权通过但任务不存在 → HTTP 200 → 签名有效，视为健康
	// 不提交真实任务，不产生计费。
	hcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	probeBody, _ := json.Marshal(map[string]interface{}{
		"req_key":  VolcModelText2ImgV3,
		"task_id":  "health_check_probe",
		"req_json": `{"return_url":true}`,
	})
	if _, err := p.doRequest(hcCtx, volcengineActionGetResult, probeBody); err != nil {
		return fmt.Errorf("volcengine-visual: HealthCheck 失败 (AK/SK 可能无效): %w", err)
	}
	return nil
}

// ImageGenerate 调用即梦AI生成图像（异步接口，内部自动轮询）
//
// 参数映射：
//   - Model         → req_key（见上方模型常量）
//   - Prompt        → prompt
//   - ReferenceImage → image_input / image_urls / binary_data_base64（自动判断 URL 或 base64）
//   - Style         → template_id（仅 i2i_multi_style_zx2x 图像特效模型）
//   - CFGScale      → scale（SeedEdit3.0）
//   - Seed          → seed
//   - Size          → width x height（格式 "1024x1024"）
func (p *VolcengineVisualProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	start := time.Now()

	reqKey := req.Model
	if reqKey == "" {
		reqKey = VolcModelText2ImgV3
	}

	// Step 1：构建并提交任务
	submitParams := p.buildSubmitParams(reqKey, req)
	taskID, err := p.submitTask(ctx, submitParams)
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
	case VolcModelText2ImgV3:
		// 文生图：prompt + 尺寸，可选 scale（默认2.5，范围[1,10]）
		params["prompt"] = req.Prompt
		params["width"] = width
		params["height"] = height
		if req.CFGScale > 0 {
			params["scale"] = req.CFGScale
		}

	case VolcModelPortraitPhoto:
		// 人像写真：image_input（单 URL 字符串）+ 可选 prompt
		if req.ReferenceImage != "" {
			params["image_input"] = req.ReferenceImage
		}
		if req.Prompt != "" {
			params["prompt"] = req.Prompt
		}
		params["width"] = width
		params["height"] = height

	case VolcModelSeedEditV3:
		// 指令编辑：image_urls[] 或 binary_data_base64[]，必填 prompt
		params["prompt"] = req.Prompt
		if req.CFGScale > 0 {
			params["scale"] = req.CFGScale
		}
		p.setImageInput(params, req.ReferenceImage, "image_urls", "binary_data_base64")

	case VolcModelDreamO:
		// 角色特征保持：image_urls[] 或 binary_data_base64[]，必填 prompt
		params["prompt"] = req.Prompt
		params["width"] = width
		params["height"] = height
		p.setImageInput(params, req.ReferenceImage, "image_urls", "binary_data_base64")

	case VolcModelImageEffect:
		// 图像特效：image_input1（URL）+ template_id（必填，来自 Style 字段）
		params["image_input1"] = req.ReferenceImage
		params["template_id"] = req.Style
		params["width"] = width
		params["height"] = height
	}

	return params
}

// setImageInput 将参考图设置到 URL 数组或 base64 数组字段
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

// submitTask 提交异步任务，返回 task_id
func (p *VolcengineVisualProvider) submitTask(ctx context.Context, params map[string]interface{}) (string, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("volcengine-visual: 序列化提交参数失败: %w", err)
	}

	respBody, err := p.doRequest(ctx, volcengineActionSubmit, body)
	if err != nil {
		return "", err
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("volcengine-visual: 解析提交响应失败: %w", err)
	}
	if resp.Code != 10000 {
		return "", fmt.Errorf("即梦AI 提交任务失败 code=%d: %s", resp.Code, resp.Message)
	}
	if resp.Data.TaskID == "" {
		return "", fmt.Errorf("即梦AI 未返回 task_id")
	}
	return resp.Data.TaskID, nil
}

// pollResult 轮询任务结果，最多等待 5 分钟
func (p *VolcengineVisualProvider) pollResult(ctx context.Context, reqKey, taskID string, start time.Time) (*ImageResponse, error) {
	reqJSON := `{"return_url":true}`
	getParams := map[string]interface{}{
		"req_key":  reqKey,
		"task_id":  taskID,
		"req_json": reqJSON,
	}
	getBody, _ := json.Marshal(getParams)

	deadline := time.Now().Add(5 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("即梦AI: 任务超时（taskID=%s）", taskID)
			}

			respBody, err := p.doRequest(ctx, volcengineActionGetResult, getBody)
			if err != nil {
				// 网络瞬时错误，继续轮询
				continue
			}

			var resp struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    struct {
					Status          string   `json:"status"`
					ImageURLs       []string `json:"image_urls"`
					BinaryDataBase64 []string `json:"binary_data_base64"`
				} `json:"data"`
			}
			if err := json.Unmarshal(respBody, &resp); err != nil {
				continue
			}

			if resp.Code != 10000 {
				return &ImageResponse{
					Error:     fmt.Sprintf("即梦AI 获取结果失败 code=%d: %s", resp.Code, resp.Message),
					LatencyMs: time.Since(start).Milliseconds(),
				}, nil
			}

			switch resp.Data.Status {
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

			if len(resp.Data.ImageURLs) > 0 {
				return &ImageResponse{
					URL:       resp.Data.ImageURLs[0],
					LatencyMs: time.Since(start).Milliseconds(),
				}, nil
			}
			// 未请求 return_url 时从 base64 返回（不常见）
			return &ImageResponse{
				Error:     "即梦AI: 任务完成但未返回图片 URL",
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}
	}
}

// doRequest 发送带 Volcengine HMAC-SHA256 签名的请求
func (p *VolcengineVisualProvider) doRequest(ctx context.Context, action string, body []byte) ([]byte, error) {
	now := time.Now().UTC()
	queryString := fmt.Sprintf("Action=%s&Version=%s", action, volcengineVisualVersion)
	uri := "/"

	longDate := now.Format("20060102T150405Z")
	shortDate := now.Format("20060102")

	bodyHash := volcSHA256Hex(body)

	// 签名头（按字母序）
	signedHeaders := "content-type;host;x-content-sha256;x-date"
	canonicalHeaders := fmt.Sprintf(
		"content-type:application/json\nhost:%s\nx-content-sha256:%s\nx-date:%s\n",
		volcengineVisualHost, bodyHash, longDate,
	)

	// Canonical Request
	canonicalRequest := strings.Join([]string{
		"POST",
		uri,
		queryString,
		canonicalHeaders,
		signedHeaders,
		bodyHash,
	}, "\n")

	scope := shortDate + "/" + volcengineVisualRegion + "/" + volcengineVisualService + "/request"
	stringToSign := "HMAC-SHA256\n" + longDate + "\n" + scope + "\n" + volcSHA256Hex([]byte(canonicalRequest))

	// 签名密钥派生（Volcengine SigV4 兼容）
	kDate := volcHMACSHA256([]byte(p.secretKey), []byte(shortDate))
	kRegion := volcHMACSHA256(kDate, []byte(volcengineVisualRegion))
	kService := volcHMACSHA256(kRegion, []byte(volcengineVisualService))
	kSigning := volcHMACSHA256(kService, []byte("request"))

	signature := hex.EncodeToString(volcHMACSHA256(kSigning, []byte(stringToSign)))
	authorization := fmt.Sprintf(
		"HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.accessKey, scope, signedHeaders, signature,
	)

	url := fmt.Sprintf("%s/?%s", volcengineVisualEndpoint, queryString)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("volcengine-visual: 创建请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Host", volcengineVisualHost)
	httpReq.Header.Set("X-Content-Sha256", bodyHash)
	httpReq.Header.Set("X-Date", longDate)
	httpReq.Header.Set("Authorization", authorization)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("volcengine-visual: HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("volcengine-visual: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("volcengine-visual: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// parseSizeWH 将 "1024x1024" 转换为宽高，默认 1328x1328
func parseSizeWH(size string) (int, int) {
	if size == "" {
		return 1328, 1328
	}
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err == nil && w > 0 && h > 0 {
		return w, h
	}
	return 1328, 1328
}

// volcSHA256Hex 计算 SHA256 十六进制摘要
func volcSHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// volcHMACSHA256 计算 HMAC-SHA256
func volcHMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
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
