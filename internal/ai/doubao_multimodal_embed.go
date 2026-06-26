package ai

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// EmbedMultimodal 使用 Volcengine Ark Go SDK 调用多模态 Embedding API。
//
// 实现 MultimodalEmbedder 接口，上层按需类型断言：
//
//	if emb, ok := provider.(ai.MultimodalEmbedder); ok {
//	    resp, err := emb.EmbedMultimodal(ctx, req)
//	}
//
// 默认模型：doubao-embedding-vision-250328
// API 文档：POST /embeddings/multimodal
func (p *DoubaoProvider) EmbedMultimodal(ctx context.Context, req *MultimodalEmbedRequest) (*MultimodalEmbedResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = "doubao-embedding-vision-250328"
	}

	// 使用 SDK 构建客户端（复用 provider 已有的 http.Client 和 endpoint）
	client := arkruntime.NewClientWithApiKey(
		p.apiKey,
		arkruntime.WithBaseUrl(p.endpoint),
		arkruntime.WithHTTPClient(p.client),
	)

	// 将通用 MultimodalEmbedItem 转换为 SDK 类型
	inputs := make([]arkmodel.MultimodalEmbeddingInput, 0, len(req.Input))
	for _, item := range req.Input {
		switch item.Type {
		case "text":
			text := item.Text
			inputs = append(inputs, arkmodel.MultimodalEmbeddingInput{
				Type: arkmodel.MultiModalEmbeddingInputTypeText,
				Text: &text,
			})
		case "image_url":
			inputs = append(inputs, arkmodel.MultimodalEmbeddingInput{
				Type:     arkmodel.MultiModalEmbeddingInputTypeImageURL,
				ImageURL: &arkmodel.MultimodalEmbeddingImageURL{URL: item.ImageURL},
			})
		case "video_url":
			videoURL := &arkmodel.MultimodalEmbeddingVideoURL{URL: item.VideoURL}
			if item.VideoFPS != nil {
				videoURL.FPS = item.VideoFPS
			}
			if item.VideoMaxTokens != nil {
				videoURL.MaxVideoTokens = item.VideoMaxTokens
			}
			if item.VideoMinFrameTokens != nil {
				videoURL.MinFrameTokens = item.VideoMinFrameTokens
			}
			if item.VideoMaxFrameTokens != nil {
				videoURL.MaxFrameTokens = item.VideoMaxFrameTokens
			}
			if item.VideoMinFrames != nil {
				videoURL.MinFrames = item.VideoMinFrames
			}
			inputs = append(inputs, arkmodel.MultimodalEmbeddingInput{
				Type:     arkmodel.MultiModalEmbeddingInputTypeVideoURL,
				VideoURL: videoURL,
			})
		default:
			return nil, fmt.Errorf("不支持的多模态 Embedding 输入类型: %q", item.Type)
		}
	}

	arkReq := arkmodel.MultiModalEmbeddingRequest{
		Model: model,
		Input: inputs,
	}
	if req.Dimensions != nil {
		arkReq.Dimensions = req.Dimensions
	}
	if req.Instructions != "" {
		s := req.Instructions
		arkReq.Instructions = &s
	}

	// 稀疏向量开关（仅纯文本输入支持）
	if req.SparseEmbedding != nil {
		t := arkmodel.SparseEmbeddingInputTypeDisabled
		if req.SparseEmbedding.Enabled {
			t = arkmodel.SparseEmbeddingInputTypeEnabled
		}
		arkReq.SparseEmbedding = &arkmodel.SparseEmbeddingInput{Type: t}
	}

	// 多向量（multi-vector）配置
	if req.MultiEmbedding != nil {
		t := arkmodel.MultiEmbeddingTypeDisabled
		if req.MultiEmbedding.Enabled {
			t = arkmodel.MultiEmbeddingTypeEnabled
		}
		cfg := &arkmodel.MultiEmbeddingConfig{Type: t}
		if req.MultiEmbedding.Compression != "" {
			c := arkmodel.MultiEmbeddingCompression(req.MultiEmbedding.Compression)
			cfg.Compression = &c
		}
		arkReq.MultiEmbedding = cfg
	}

	resp, err := client.CreateMultiModalEmbeddings(ctx, arkReq)
	if err != nil {
		return nil, fmt.Errorf("豆包多模态 Embedding 错误: %w", err)
	}

	out := &MultimodalEmbedResponse{
		Embedding:   resp.Data.Embedding,
		TokensUsed:  resp.Usage.TotalTokens,
		TextTokens:  resp.Usage.PromptTokensDetails.TextTokens,
		ImageTokens: resp.Usage.PromptTokensDetails.ImageTokens,
		Model:       resp.Model,
	}

	// 稀疏向量：转换为内部类型
	if resp.Data.SparseEmbedding != nil {
		points := make([]SparseEmbedPoint, 0, len(*resp.Data.SparseEmbedding))
		for _, p := range *resp.Data.SparseEmbedding {
			points = append(points, SparseEmbedPoint{Index: p.Index, Value: p.Value})
		}
		out.SparseEmbedding = points
	}

	// 多向量：解压（zstd 压缩场景由 SDK Decode 处理）
	if resp.Data.MultiEmbedding != nil {
		compression := arkmodel.MultiEmbeddingCompression("")
		if req.MultiEmbedding != nil && req.MultiEmbedding.Compression != "" {
			compression = arkmodel.MultiEmbeddingCompression(req.MultiEmbedding.Compression)
		}
		vectors, decErr := resp.Data.MultiEmbedding.Decode(compression)
		if decErr != nil {
			log.Printf("[doubao] EmbedMultimodal: 多向量解码失败: %v", decErr)
		} else {
			out.MultiEmbedding = vectors
		}
	}

	log.Printf("[doubao] EmbedMultimodal model=%s inputs=%d tokens=%d(text=%d img=%d) latency=%dms",
		model, len(req.Input), resp.Usage.TotalTokens,
		resp.Usage.PromptTokensDetails.TextTokens, resp.Usage.PromptTokensDetails.ImageTokens,
		time.Since(start).Milliseconds())

	return out, nil
}
