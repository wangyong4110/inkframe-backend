package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// analyzeSingleShotSFX 为单个分镜调用 AI 生成结构化音效搜索词，返回 tag 列表。
// 输出格式：[{"tag":"...","type":"action|ambient|emotion","prompt":"..."}, ...]
// tag 字段始终输出英文（最多 3 词），prompt 字段为中文自然语言（供 Kling SFX / AudioLDM 使用）。
func (s *SFXService) analyzeSingleShotSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, userContext string) ([]sfxTagItem, error) {
	// 过渡闪切镜头（< 1s）直接跳过 AI 调用
	if shot.Duration < 1.0 {
		return nil, nil
	}

	// 构建分镜上下文
	var sceneCtx strings.Builder
	fmt.Fprintf(&sceneCtx, "镜头编号：%d\n", shot.ShotNo)
	fmt.Fprintf(&sceneCtx, "时长：%.1f 秒\n", shot.Duration)
	if shot.CamDir.CameraType != "" && shot.CamDir.CameraType != "static" {
		fmt.Fprintf(&sceneCtx, "运镜：%s\n", shot.CamDir.CameraType)
	}
	if shot.Description != "" {
		fmt.Fprintf(&sceneCtx, "画面描述（视觉参考，仅推断声源，禁止把视觉词写进 tag）：%s\n", shot.Description)
	}
	if shot.GenMeta.Scene != "" {
		fmt.Fprintf(&sceneCtx, "场景环境：%s\n", shot.GenMeta.Scene)
	}
	if shot.Dialogue() != "" {
		fmt.Fprintf(&sceneCtx, "⚠️ 含对白：所有音效必须为 subtle，禁止任何冲击/爆发音，人声频段（300Hz–3kHz）绝对不可遮蔽\n")
	}
	if userContext != "" {
		fmt.Fprintf(&sceneCtx, "额外背景（最高优先级）：%s\n", userContext)
	}

	// 时长策略 & 运镜
	durationStrategy := buildDurationStrategy(shot.Duration)
	motionSection := ""
	if mg := cameraMotionGuide(shot.CamDir.CameraType); mg != "" {
		motionSection = "**运镜**：" + mg
	}

	prompt, err := renderPrompt("sfx_analyze", map[string]interface{}{
		"DurationStrategy": durationStrategy,
		"MotionSection":    motionSection,
		"SceneContext":     sceneCtx.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("render sfx_analyze template: %w", err)
	}

	// MaxTokens=3000：推理模型（如 DeepSeek-R1）会先输出思考过程再输出 JSON，
	// 3000 token 足以容纳思考过程（~500-800 tok）+ JSON 输出（~100-200 tok）。
	callResult := func() (string, error) {
		return s.aiSvc.GenerateWithProvider(tenantID, 0, "sfx_analyze", prompt, "")
	}
	result, err := callResult()
	if err != nil {
		return nil, fmt.Errorf("AI call: %w", err)
	}

	raw := extractJSON(result)
	// DeepSeek-chat (V3) 有时在 content 里先输出推理过程再输出 JSON。
	if len(raw) == 0 || raw[0] != '[' {
		if idx := strings.Index(result, "["); idx != -1 {
			raw = extractJSON(result[idx:])
		}
	}
	// 响应异常短（< 80 字节）说明模型输出不完整或被截断，重试一次。
	if len(strings.TrimSpace(raw)) < 80 {
		logger.Printf("[SFXService] shot %d: response too short (%d bytes), retrying", shot.ShotNo, len(raw))
		if r2, err2 := callResult(); err2 == nil {
			if raw2 := extractJSON(r2); len(strings.TrimSpace(raw2)) > len(raw) {
				raw = raw2
			}
		}
	}

	// 解析结构化格式
	var items []sfxTagItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil || len(items) == 0 || items[0].Tag == "" {
		// 兼容旧版纯字符串输出
		var strs []string
		if err2 := json.Unmarshal([]byte(raw), &strs); err2 != nil {
			return nil, fmt.Errorf("parse JSON: %w (raw=%q)", err, raw)
		}
		items = make([]sfxTagItem, 0, len(strs))
		for _, s2 := range strs {
			items = append(items, sfxTagItem{Tag: s2, SFXType: guessSFXType(s2)})
		}
	}

	// 过滤空 tag
	filtered := items[:0]
	for _, it := range items {
		if strings.TrimSpace(it.Tag) != "" {
			if it.SFXType == "" {
				it.SFXType = guessSFXType(it.Tag)
			}
			filtered = append(filtered, it)
		}
	}

	// 为每个 tag 填充中文 Prompt（供 Kling SFX / ElevenLabs AI 文生音效使用）
	shotPrompt := buildShotAIPrompt(shot)
	for i := range filtered {
		if filtered[i].Prompt == "" {
			filtered[i].Prompt = shotPrompt
		}
	}

	return filtered, nil
}

// AnalyzeSFXForVideo 并行为每个分镜单独调用 AI 生成结构化音效搜索词。
// force=true 时强制重新分析所有镜头；force=false 时跳过已有 SFX 条目的镜头。
// 每个分镜独立分析，并发度最多 15，单个失败不影响其余镜头。
func (s *SFXService) AnalyzeSFXForVideo(ctx context.Context, shots []*model.StoryboardShot, tenantID uint, userContext string, force bool) error {
	if len(shots) == 0 {
		return nil
	}
	logger.Printf("[SFXService] AnalyzeSFXForVideo: parallel analysis for %d shots (force=%v)", len(shots), force)

	const maxConcurrency = 15
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var updated, failed atomic.Int32

	var skipped atomic.Int32
	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		// 非强制模式：已有 SFX 条目则跳过
		if !force && s.sfxItemRepo != nil {
			if count, _ := s.sfxItemRepo.CountByShotID(shot.ID); count > 0 {
				skipped.Add(1)
				continue
			}
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(sh *model.StoryboardShot) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := s.analyzeSingleShotSFX(ctx, sh, tenantID, userContext)
			if err != nil {
				logger.Errorf("[SFXService] AnalyzeSFXForVideo: shot %d failed: %v", sh.ShotNo, err)
				failed.Add(1)
			} else {
				updated.Add(1)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("[SFXService] AnalyzeSFXForVideo: updated=%d failed=%d skipped=%d(already tagged)",
		updated.Load(), failed.Load(), skipped.Load())
	return nil
}
