package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

const promptEnhanceSystemPrompt = `You are an expert image/video generation prompt engineer specializing in Chinese web-novel visual adaptation.

Given a scene description (possibly in Chinese), you will:
1. Translate and expand it into vivid, detailed English visual language.
2. Add appropriate composition, lighting, atmosphere, and style keywords.
3. Avoid any NSFW content.
4. Return ONLY valid JSON in this exact format:
{
  "enhanced_prompt": "...",
  "negative_prompt": "...",
  "style_tags": ["tag1", "tag2", ...]
}

The enhanced_prompt should be a comma-separated list of English descriptors suitable for Stable Diffusion or similar models.
The negative_prompt should list elements to avoid.
The style_tags should be 3-8 short style keywords.`

// PromptEnhanceResult 增强后的 prompt 结果
type PromptEnhanceResult struct {
	EnhancedPrompt string   `json:"enhanced_prompt"`
	NegativePrompt string   `json:"negative_prompt"`
	StyleTags      []string `json:"style_tags"`
}

// PromptEnhanceService MCP 工具：使用 LLM 增强图像/视频生成 prompt
type PromptEnhanceService struct {
	aiService *AIService
}

func NewPromptEnhanceService(aiService *AIService) *PromptEnhanceService {
	return &PromptEnhanceService{aiService: aiService}
}

// Enhance 将中文场景描述翻译并增强为适合图像/视频生成的 prompt
func (s *PromptEnhanceService) Enhance(ctx context.Context, sceneDescription, style, promptType string) (*PromptEnhanceResult, error) {
	if sceneDescription == "" {
		return nil, fmt.Errorf("scene_description is required")
	}

	userMsg := buildEnhanceUserMessage(sceneDescription, style, promptType)

	// Use GenerateWithProviderCtx with a lightweight task type.
	// novelID=0 and providerName="" so the default provider is used.
	raw, err := s.aiService.GenerateWithProviderCtx(ctx, 0, 0, "storyboard", userMsg, "",
		StoryboardOverrides{MaxTokens: 1024, Temperature: 0.3})
	if err != nil {
		return nil, fmt.Errorf("prompt enhance LLM call failed: %w", err)
	}

	result, parseErr := parsePromptEnhanceResponse(raw)
	if parseErr != nil {
		logger.Errorf("[PromptEnhanceService] parse failed: %v, raw=%.300s", parseErr, raw)
		// Fallback: return a best-effort result using the raw text
		return &PromptEnhanceResult{
			EnhancedPrompt: strings.TrimSpace(raw),
			NegativePrompt: "blurry, low quality, text, watermark",
			StyleTags:      []string{style},
		}, nil
	}
	return result, nil
}

func buildEnhanceUserMessage(sceneDescription, style, promptType string) string {
	var sb strings.Builder
	sb.WriteString("Scene description: ")
	sb.WriteString(sceneDescription)
	if style != "" {
		sb.WriteString("\nVisual style: ")
		sb.WriteString(style)
	}
	if promptType != "" {
		sb.WriteString("\nPrompt type: ")
		sb.WriteString(promptType)
		sb.WriteString(" (optimise for ")
		sb.WriteString(promptType)
		sb.WriteString(" generation)")
	}
	sb.WriteString("\n\nReturn JSON only.")
	return sb.String()
}

func parsePromptEnhanceResponse(raw string) (*PromptEnhanceResult, error) {
	cleaned := extractJSON(strings.TrimSpace(raw))
	var result PromptEnhanceResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	if result.EnhancedPrompt == "" {
		return nil, fmt.Errorf("enhanced_prompt is empty")
	}
	return &result, nil
}
