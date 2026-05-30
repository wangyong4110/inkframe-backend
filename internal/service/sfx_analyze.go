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
		fmt.Fprintf(&sceneCtx, "画面描述（视觉，仅用于推断声音来源，不要把视觉词写进标签）：%s\n", shot.Description)
	}
	if shot.Scene != "" {
		fmt.Fprintf(&sceneCtx, "场景环境：%s\n", shot.Scene)
	}
	if shot.EmotionalTone != "" {
		fmt.Fprintf(&sceneCtx, "情绪基调：%s\n", shot.EmotionalTone)
	}
	if shot.Dialogue != "" {
		fmt.Fprintf(&sceneCtx, "⚠️ 有人物台词（对白）：环境底层音必须非常 subtle，禁止动作冲击音，避免掩盖人声\n")
	}
	if userContext != "" {
		fmt.Fprintf(&sceneCtx, "额外背景（优先参考）：%s\n", userContext)
	}

	// 景别 & 运镜引导
	sizeGuide := shotSizeGuide(shot.ShotSize)
	motionGuide := cameraMotionGuide(shot.CameraType)
	motionSection := ""
	if motionGuide != "" {
		motionSection = "\n运镜音提示：" + motionGuide
	}

	langInstruction := `
## Tag 格式（英文，Freesound 四元格式）：[物体/来源] [材质/空间] [动作] [音色描述符]
音色描述符：single/one-shot/burst（触发音）；loop/continuous（循环音）；indoor/outdoor/reverb/dry/distant（空间感）；heavy/light/sharp/soft/subtle/crisp（质感）
示例：
- {"tag":"wooden door creak open indoor single","type":"action","prompt":"室内木门缓慢打开，嘎吱声"}
- {"tag":"forest birds chirping outdoor loop","type":"ambient","prompt":"森林清晨，鸟鸣环境音循环"}
- {"tag":"heartbeat tense pulse close single","type":"emotion","prompt":"紧张特写，心跳加速"}
❌ tag 禁止：视觉词（sunlight/morning/warm/bright）、情绪形容词（epic/mystical/dramatic）、BGM词（ambience/atmosphere/soundscape）、单词笼统词（sword/rain/fire）
✅ prompt 字段用中文自然语言描述声音场景（供 AI 文生音效使用，可与 tag 描述同一声音）
`

	prompt := `你是有15年经验的好莱坞级影视音效设计师，负责为分镜脚本设计精准的音效搜索词。

## 核心原则：声音必须真实可被听见
搜索词只能描述听觉现象——物体发出的真实声音。
绝对禁止视觉概念（光线/色彩/时段/情绪抽象）出现在搜索词里。

## 三层分层设计框架
| 层次 | 类型标记 | 触发方式 | 优先级 |
|------|----------|----------|--------|
| 动作音 | action | 单次触发（one-shot） | 最高，与画面强同步 |
| 环境底层音 | ambient | 循环（loop），贯穿全镜 | 中，建立空间感 |
| 情绪点缀音 | emotion | 单次触发，场景转折/强调 | 低，谨慎使用 |

## 景别设计规则
` + sizeGuide + motionSection + langInstruction + `

## 输出规则
- 输出 0~3 条，按 action → ambient → emotion 优先级排列
- 极近景特写（无动作）：只输出 0~1 条 action（细节音），不加 ambient
- 纯情感特写/空镜：最多 1 条极 subtle 的 ambient
- 每条必须包含 tag（英文搜索词）、type、prompt（中文描述，供 AI 生成使用）三个字段
- 仅输出 JSON 数组，禁止任何额外文字

## 避免滥用的标签
- ❌ 呼吸声（breath/breathing/exhale）：只在画面中有明显喘息/憋气等肢体行为时才输出；通用情绪特写禁止
- ❌ 两字笼统词：wind_howling / desert_wind / fire_crackle 等不符合四元格式，必须展开为完整 4 元描述
- ❌ 连续镜头 ambient 重复：同场景环境底层音最多出现一次，后续镜头若无场景切换则省略 ambient

## 分镜信息
` + sceneCtx.String() + `
请输出：`

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
