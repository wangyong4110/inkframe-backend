package service

import (
	"strings"
)

// ColorPalette defines a mood-driven visual color scheme for video/image generation.
type ColorPalette struct {
	Mood        string   `json:"mood"`
	Primary     string   `json:"primary"`     // main color hex
	Secondary   string   `json:"secondary"`   // supporting color hex
	Accent      string   `json:"accent"`      // highlight color hex
	Background  string   `json:"background"`  // background color hex
	Tone        string   `json:"tone"`        // human-readable description
	LightStyle  string   `json:"light_style"` // lighting advice
	Temperature string   `json:"temperature"` // warm/cold description
	Keywords    []string `json:"keywords"`    // English prompt keywords
	PromptHints string   `json:"prompt_hints"` // ready-to-use Stable Diffusion/Kling prompt fragment
}

// paletteLibrary maps mood labels to color palettes.
var paletteLibrary = map[string]ColorPalette{
	"tension": {
		Mood:        "tension",
		Primary:     "#1a1a2e",
		Secondary:   "#16213e",
		Accent:      "#e94560",
		Background:  "#0f0f23",
		Tone:        "冷色+高对比",
		LightStyle:  "强侧光，深阴影，戏剧性丁达尔光束",
		Temperature: "冷色调（蓝黑紫）",
		Keywords:    []string{"dark", "dramatic", "cinematic", "blue-black", "high contrast"},
		PromptHints: "dark cinematic lighting, deep shadows, cold blue tones, high contrast, tense atmosphere",
	},
	"battle": {
		Mood:        "battle",
		Primary:     "#2d0000",
		Secondary:   "#8b0000",
		Accent:      "#ff4500",
		Background:  "#1a0000",
		Tone:        "暗红+火焰色",
		LightStyle:  "动态火光/能量爆发，多点光源",
		Temperature: "暖色调（红橙黄）",
		Keywords:    []string{"fire", "energy", "explosive", "red-orange", "dynamic"},
		PromptHints: "dynamic battle lighting, fire effects, energy aura, red-orange palette, explosive atmosphere",
	},
	"cultivation": {
		Mood:        "cultivation",
		Primary:     "#1e3a1e",
		Secondary:   "#2d5a2d",
		Accent:      "#7fff00",
		Background:  "#0a1a0a",
		Tone:        "深绿+仙气",
		LightStyle:  "灵气粒子光效，仙雾缭绕，神圣光晕",
		Temperature: "冷绿调",
		Keywords:    []string{"mystical", "green aura", "spiritual energy", "ancient", "ethereal mist"},
		PromptHints: "mystical green spiritual aura, ancient Chinese landscape, ethereal mist, cultivation atmosphere",
	},
	"romance": {
		Mood:        "romance",
		Primary:     "#ffd6e0",
		Secondary:   "#ffb3c1",
		Accent:      "#c9184a",
		Background:  "#fff0f3",
		Tone:        "暖粉+柔光",
		LightStyle:  "柔和散射光，暖色逆光，唯美光晕",
		Temperature: "暖色调（粉白玫瑰红）",
		Keywords:    []string{"soft", "warm", "romantic", "pink", "ethereal glow"},
		PromptHints: "soft warm lighting, ethereal glow, pink and rose tones, romantic bokeh, gentle atmosphere",
	},
	"mystery": {
		Mood:        "mystery",
		Primary:     "#1c1c3a",
		Secondary:   "#2d2d5e",
		Accent:      "#7b68ee",
		Background:  "#0e0e24",
		Tone:        "深紫+神秘蓝",
		LightStyle:  "幽冷月光，薄雾效果，神秘荧光",
		Temperature: "冷色调（深紫蓝）",
		Keywords:    []string{"mysterious", "dark purple", "moonlight", "fog", "arcane"},
		PromptHints: "mysterious atmosphere, deep purple and blue, moonlight, misty fog, arcane glow",
	},
	"joy": {
		Mood:        "joy",
		Primary:     "#ffd700",
		Secondary:   "#ffa500",
		Accent:      "#ff6b35",
		Background:  "#fffaf0",
		Tone:        "暖金+明亮",
		LightStyle:  "明亮均匀光，金色阳光，逆光剪影",
		Temperature: "暖色调（金橙黄）",
		Keywords:    []string{"bright", "golden", "warm", "joyful", "sunlight"},
		PromptHints: "bright golden sunlight, warm joyful atmosphere, vibrant colors, lens flare, cheerful",
	},
	"sadness": {
		Mood:        "sadness",
		Primary:     "#4a6fa5",
		Secondary:   "#6b8cba",
		Accent:      "#b0c4de",
		Background:  "#e8f0f7",
		Tone:        "冷蓝+灰调",
		LightStyle:  "散漫阴天光，冷调窗光，雨雾效果",
		Temperature: "冷色调（蓝灰）",
		Keywords:    []string{"melancholy", "blue-grey", "overcast", "soft", "muted rain"},
		PromptHints: "melancholy atmosphere, blue-grey muted tones, overcast soft light, rain, desaturated",
	},
	"epic": {
		Mood:        "epic",
		Primary:     "#2c1810",
		Secondary:   "#8b4513",
		Accent:      "#ffd700",
		Background:  "#1a0e08",
		Tone:        "史诗金棕",
		LightStyle:  "大范围体积光，宏观天光，晨曦或黄昏",
		Temperature: "暖色调偏暗",
		Keywords:    []string{"epic", "grand", "cinematic", "golden hour", "vast"},
		PromptHints: "epic cinematic shot, vast landscape, golden hour, dramatic volumetric light, grand scale",
	},
	"horror": {
		Mood:        "horror",
		Primary:     "#0d0d0d",
		Secondary:   "#1a0000",
		Accent:      "#8b0000",
		Background:  "#000000",
		Tone:        "极暗+深红",
		LightStyle:  "极暗底光，单点红光，阴影压制",
		Temperature: "极冷+暗红",
		Keywords:    []string{"dark", "horror", "deep red", "shadow", "ominous"},
		PromptHints: "horror atmosphere, extreme darkness, deep red single light source, ominous shadows",
	},
	"ancient": {
		Mood:        "ancient",
		Primary:     "#8b7355",
		Secondary:   "#deb887",
		Accent:      "#a0522d",
		Background:  "#f5deb3",
		Tone:        "古朴褐黄",
		LightStyle:  "烛光/灯笼暖光，复古滤镜质感",
		Temperature: "暖色调（棕黄）",
		Keywords:    []string{"ancient", "vintage", "warm brown", "traditional", "ink wash"},
		PromptHints: "ancient Chinese aesthetic, warm candlelight, traditional ink wash style, vintage brown tones",
	},
	"normal": {
		Mood:        "normal",
		Primary:     "#e8dcc8",
		Secondary:   "#d4b896",
		Accent:      "#8b6914",
		Background:  "#f5f0e8",
		Tone:        "自然暖调",
		LightStyle:  "柔和自然光，日常场景",
		Temperature: "中性偏暖",
		Keywords:    []string{"natural", "warm", "neutral", "balanced", "everyday"},
		PromptHints: "natural lighting, warm neutral tones, balanced composition, everyday atmosphere",
	},
}

// ColorPaletteService provides mood-to-palette lookups.
type ColorPaletteService struct{}

// NewColorPaletteService creates a ColorPaletteService.
func NewColorPaletteService() *ColorPaletteService {
	return &ColorPaletteService{}
}

// GetPalette returns the best matching palette for a mood string.
// Falls back to "normal" if no match is found.
func (s *ColorPaletteService) GetPalette(mood string) ColorPalette {
	key := strings.ToLower(strings.TrimSpace(mood))

	if p, ok := paletteLibrary[key]; ok {
		return p
	}
	// Fuzzy match
	for k, p := range paletteLibrary {
		if strings.Contains(key, k) || strings.Contains(k, key) {
			return p
		}
	}
	return paletteLibrary["normal"]
}

// GetMultiple returns palettes for multiple moods (deduplicated).
func (s *ColorPaletteService) GetMultiple(moods []string) []ColorPalette {
	seen := map[string]bool{}
	result := make([]ColorPalette, 0, len(moods))
	for _, m := range moods {
		p := s.GetPalette(m)
		if !seen[p.Mood] {
			seen[p.Mood] = true
			result = append(result, p)
		}
	}
	return result
}

// ListAll returns all available palettes.
func (s *ColorPaletteService) ListAll() []ColorPalette {
	result := make([]ColorPalette, 0, len(paletteLibrary))
	for _, p := range paletteLibrary {
		result = append(result, p)
	}
	return result
}
