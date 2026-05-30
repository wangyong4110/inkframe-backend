package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sfxTagItem 结构化音效标签，包含搜索词和分类类型。
// SFXType: action=动作音（单次触发）/ ambient=环境底层音（循环）/ emotion=情绪点缀（冲击/rise）
// Tag:    搜索词（英文时为 Freesound/Pixabay/BBC/Aigei 格式；中文时直接用于 Kling SFX）
// Prompt: AI 生成提示词（用于 Kling SFX/ElevenLabs；通常为中文自然语言描述，为空时退化为 Tag）
type sfxTagItem struct {
	Tag     string `json:"tag"`
	SFXType string `json:"type"`             // action / ambient / emotion
	Prompt  string `json:"prompt,omitempty"` // AI 生成提示词（Kling SFX / ElevenLabs 专用）
}

// SFXTagItemPublic 是 sfxTagItem 的公开版本，供 handler 层使用。
type SFXTagItemPublic struct {
	Tag     string `json:"tag"`
	SFXType string `json:"type"`
	Prompt  string `json:"prompt,omitempty"`
}

// parseSFXTags 解析 sfx_tags 字段，兼容旧版纯字符串数组和新版结构化格式。
func parseSFXTags(raw string) []sfxTagItem {
	if raw == "" {
		return nil
	}
	// 尝试新格式 [{"tag":"...","type":"..."}]
	var items []sfxTagItem
	if err := json.Unmarshal([]byte(raw), &items); err == nil && len(items) > 0 && items[0].Tag != "" {
		return items
	}
	// 兼容旧格式 ["...","..."]
	var strs []string
	if err := json.Unmarshal([]byte(raw), &strs); err == nil {
		items = make([]sfxTagItem, 0, len(strs))
		for _, s := range strs {
			items = append(items, sfxTagItem{Tag: s, SFXType: guessSFXType(s)})
		}
		return items
	}
	return nil
}

// guessSFXType 根据标签词汇推断音效类型（旧数据迁移用）。
func guessSFXType(tag string) string {
	lower := strings.ToLower(tag)
	ambientKW := []string{"loop", "ambient", "continuous", "sustained", "rain", "wind", "forest", "river", "crowd", "city", "room", "birds", "insects", "fire"}
	for _, kw := range ambientKW {
		if strings.Contains(lower, kw) {
			return "ambient"
		}
	}
	emotionKW := []string{"heartbeat", "clock", "tick", "rise", "stinger", "boom", "impact", "sub-bass", "breath"}
	for _, kw := range emotionKW {
		if strings.Contains(lower, kw) {
			return "emotion"
		}
	}
	return "action"
}

// shotSizeGuide 根据景别返回音效设计侧重说明。
func shotSizeGuide(shotSize string) string {
	switch shotSize {
	case "extreme_close_up":
		return "极近景/特写：只保留微观细节音（衣物摩擦、关节/肌肉收紧、细小触碰），环境音一律压到不可感知；动作音选 close-mic dry crisp"
	case "close_up":
		return "近景：近距离动作音为主，环境音降低 6–10dB（subtle/soft）；避免大空间混响"
	case "wide":
		return "远景/全景：环境底层音主导（outdoor/reverb），动作音必须选 distant 版本；不加细节音"
	default: // medium
		return "中景：动作音与环境底层音比例均衡（约 1:1），两者互不遮蔽频段"
	}
}

// cameraMotionGuide 根据运镜类型返回额外音效提示。
func cameraMotionGuide(cameraType string) string {
	switch cameraType {
	case "pan":
		return "横移镜头：仅当画面有明显主体扫过时才加 whoosh；无主体移动的摇镜不加额外音"
	case "zoom":
		return "推拉镜头：快推可加 zoom swoosh（≤0.3s）；慢推/拉远不加"
	case "tracking":
		return "跟随镜头：动作音跟随角色节奏，ambient 保持稳定，不随摄影机移动而变化"
	default:
		return ""
	}
}

// buildDurationStrategy 根据镜头时长返回对应的音效层数/类型策略说明。
func buildDurationStrategy(dur float64) string {
	switch {
	case dur < 1.0:
		return fmt.Sprintf("%.1fs → 过渡闪切镜头，直接输出 []", dur)
	case dur < 2.0:
		return fmt.Sprintf("%.1fs → 极短镜头：最多 1 条 action（必须 single/burst/hit），禁止 ambient 和 emotion", dur)
	case dur < 5.0:
		return fmt.Sprintf("%.1fs → 中短镜头：最多 1 action（single）+ 1 ambient（loop）；emotion 仅叙事节点可选", dur)
	default:
		return fmt.Sprintf("%.1fs → 长镜头：ambient 必须有且为 loop；action 可 1–2 条；emotion 谨慎，仅叙事顶点", dur)
	}
}
