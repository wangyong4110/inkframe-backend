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

// analyzeSingleShotSFX 为单个分镜调用 AI 生成结构化音效搜索词，更新 sfx_tags 字段。
// 输出格式：[{"tag":"...","type":"action|ambient|emotion","prompt":"..."}, ...]
// tag 字段始终输出英文（Freesound 四元格式），prompt 字段为中文自然语言（供 Kling SFX / AudioLDM 使用）。
func (s *SFXService) analyzeSingleShotSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, userContext string, promptLanguage string) error {
	// 过渡闪切镜头（< 1s）直接跳过 AI 调用，存空数组
	if shot.Duration < 1.0 {
		if err := s.storyboardRepo.UpdateSFXTags(shot.ID, "[]"); err != nil {
			return fmt.Errorf("update sfx_tags: %w", err)
		}
		shot.SFXTags = "[]"
		return nil
	}

	// 构建分镜上下文
	var sceneCtx strings.Builder
	fmt.Fprintf(&sceneCtx, "镜头编号：%d\n", shot.ShotNo)
	fmt.Fprintf(&sceneCtx, "时长：%.1f 秒\n", shot.Duration)
	if shot.ShotSize != "" {
		fmt.Fprintf(&sceneCtx, "景别：%s\n", shot.ShotSize)
	}
	if shot.CameraType != "" && shot.CameraType != "static" {
		fmt.Fprintf(&sceneCtx, "运镜：%s\n", shot.CameraType)
	}
	if shot.Description != "" {
		fmt.Fprintf(&sceneCtx, "画面描述（视觉参考，仅推断声源，禁止把视觉词写进 tag）：%s\n", shot.Description)
	}
	if shot.Scene != "" {
		fmt.Fprintf(&sceneCtx, "场景环境：%s\n", shot.Scene)
	}
	if shot.EmotionalTone != "" {
		fmt.Fprintf(&sceneCtx, "情绪基调：%s\n", shot.EmotionalTone)
	}
	if shot.Dialogue != "" {
		fmt.Fprintf(&sceneCtx, "⚠️ 含对白：所有音效必须为 subtle，禁止任何冲击/爆发音，人声频段（300Hz–3kHz）绝对不可遮蔽\n")
	}
	if userContext != "" {
		fmt.Fprintf(&sceneCtx, "额外背景（最高优先级）：%s\n", userContext)
	}

	// 时长策略 & 景别 & 运镜
	durationStrategy := buildDurationStrategy(shot.Duration)
	sizeGuide := shotSizeGuide(shot.ShotSize)
	motionGuide := cameraMotionGuide(shot.CameraType)
	motionSection := ""
	if motionGuide != "" {
		motionSection = "\n**运镜**：" + motionGuide
	}

	prompt := `你是有15年经验的好莱坞级影视音效设计师，为分镜脚本设计精准的 Freesound 搜索词。

## 第一优先级：直接返回 [] 的条件（满足任一即输出空数组）
- 纯对白特写，画面内无可见肢体动作（说话嘴唇动不算）
- 纯静态凝视/思考空镜，无任何物理事件
- 时长 <1s 的过渡镜头

## 时长驱动策略（强制执行）
当前镜头：` + durationStrategy + `
| 时长 | 层数上限 | ambient | action/emotion |
|------|---------|---------|----------------|
| 1–2s | 1 条    | 禁止    | 仅 single/burst/hit |
| 2–5s | 2 条    | 可选，必须 loop | single |
| >5s  | 3 条    | 必须有，loop | 可叠加 1–2 条 |

## type 与描述符强制绑定（违反视为错误输出）
- **action** → tag 末尾必须含 single / one-shot / burst / hit / snap 之一
- **ambient** → tag 末尾必须含 loop / continuous 之一
- **emotion** → tag 末尾必须含 single / one-shot / rise / sting 之一

## emotion 严格触发条件（必须满足其一，否则禁用）
- 角色死亡、重伤、意识消失瞬间
- 核心秘密揭示或剧情重大反转
- 情感顶点：生死离别、认亲、绝望崩溃
- 主题意象首次出现（具有象征意义的物件/声音）
❌ 普通打斗、移动、环境交代、日常对话——禁用 emotion

## 景别规则
` + sizeGuide + motionSection + `

## Tag 格式（英文 Freesound 四元格式）
结构：[声源/物体] [材质/空间] [动作] [描述符]
描述符必须从上方强制绑定列表中选取，不得省略。

示例（覆盖常见场景）：
- {"tag":"heavy wooden door creak open indoor single","type":"action","prompt":"室内厚重木门缓慢推开的嘎吱声"}
- {"tag":"stone gravel footsteps walk outdoor single","type":"action","prompt":"室外碎石地面上沉稳的脚步声"}
- {"tag":"steel blade unsheathe metal scrape single","type":"action","prompt":"钢制刀剑出鞘的金属刮擦声"}
- {"tag":"desert wind sand drift outdoor loop","type":"ambient","prompt":"荒漠旷野持续低频风沙环境音"}
- {"tag":"wooden interior room tone quiet indoor loop","type":"ambient","prompt":"室内安静木质空间的底噪环境音"}
- {"tag":"deep brass sting emotional rise single","type":"emotion","prompt":"戏剧性情感顶点的铜管上扬音效"}

❌ tag 禁止词汇：
- 视觉词：sunlight / morning / warm / bright / dark / gloomy
- 情绪形容词：epic / mystical / dramatic / intense / scary
- BGM 词：ambience / atmosphere / soundscape / mood
- 笼统单词：sword / rain / fire / wind（必须展开为四元格式）
- 呼吸声：breath / breathing / exhale（仅画面中有明显喘息肢体动作时才允许）

## 分镜信息
` + sceneCtx.String() + `
仅输出 JSON 数组，禁止任何额外文字：`

	// MaxTokens=3000：推理模型（如 DeepSeek-R1）会先输出思考过程再输出 JSON，
	// 3000 token 足以容纳思考过程（~500-800 tok）+ JSON 输出（~100-200 tok）。
	// jsonOnlySystemPrompt（由 ai_service 注入）会抑制大多数推理模型的思考输出。
	// TimeoutSeconds=30：正常请求 10-15s 完成，30s 为宽裕上限。
	callResult := func() (string, error) {
		return s.aiSvc.GenerateWithProvider(tenantID, 0, "sfx_analyze", prompt, "",
			StoryboardOverrides{TimeoutSeconds: 30, MaxTokens: 3000})
	}
	result, err := callResult()
	if err != nil {
		return fmt.Errorf("AI call: %w", err)
	}

	raw := extractJSON(result)
	// DeepSeek-chat (V3) 有时在 content 里先输出推理过程再输出 JSON。
	// 若 extractJSON 未能提取到有效数组（result 不以 [ 开头），尝试直接定位第一个 [ 字符。
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
			return fmt.Errorf("parse JSON: %w (raw=%q)", err, raw)
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

	tagsJSON, _ := json.Marshal(filtered)
	shot.SFXTags = string(tagsJSON)
	if err := s.storyboardRepo.UpdateSFXTags(shot.ID, string(tagsJSON)); err != nil {
		return fmt.Errorf("update sfx_tags: %w", err)
	}
	return nil
}

// AnalyzeSFXForVideo 并行为每个分镜单独调用 AI 生成结构化音效搜索词，写入 sfx_tags 字段。
// promptLanguage：项目提示词语言（"zh"=中文，"en"=英文）；影响 AI 输出标签语言。
// force=true 时强制重新分析所有镜头（用于用户主动触发），force=false 时跳过已有有效标签的镜头。
// 每个分镜独立分析，并发度最多 15，单个失败不影响其余镜头。
func (s *SFXService) AnalyzeSFXForVideo(ctx context.Context, shots []*model.StoryboardShot, tenantID uint, userContext string, promptLanguage string, force bool) error {
	if len(shots) == 0 {
		return nil
	}
	logger.Printf("[SFXService] AnalyzeSFXForVideo: parallel analysis for %d shots (lang=%s force=%v)", len(shots), promptLanguage, force)

	const maxConcurrency = 15
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var updated, failed atomic.Int32

	var skipped atomic.Int32
	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		// 非强制模式：已有有效结构化 tags（非空且含 tag 字段）则跳过，避免重复调用 AI
		if !force {
			if existing := parseSFXTags(shot.SFXTags); len(existing) > 0 && existing[0].Tag != "" {
				skipped.Add(1)
				continue
			}
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(sh *model.StoryboardShot) {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.analyzeSingleShotSFX(ctx, sh, tenantID, userContext, promptLanguage)
			if err != nil {
				logger.Printf("[SFXService] AnalyzeSFXForVideo: shot %d failed: %v", sh.ShotNo, err)
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
