package service

import (
	"encoding/json"
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
		return "极近景/特写：强调微观细节音（衣物摩擦、皮肤/毛发接触、呼吸、心跳），禁止远景环境音"
	case "close_up":
		return "近景：突出物体近距离动作音，环境音压低至 subtle"
	case "wide":
		return "远景/全景：以环境底层音为主，动作音选 distant/reverb 版本"
	default: // medium
		return "中景：动作音与环境底层音并重，保持自然比例"
	}
}

// cameraMotionGuide 根据运镜类型返回额外音效提示。
func cameraMotionGuide(cameraType string) string {
	switch cameraType {
	case "pan":
		return "横移镜头：可加一条极短的 whoosh 扫场音（0.3–0.5s）"
	case "zoom":
		return "推拉镜头：快速推进可加 zoom in swoosh，拉远可不加额外音"
	case "tracking":
		return "跟随镜头：动作音随角色移动节奏，环境音保持稳定"
	default:
		return ""
	}
}
