package service

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/flosch/pongo2/v4"
)

//go:embed prompts/*
var promptTemplates embed.FS

// templateCache holds compiled pongo2 templates keyed by name (without extension).
var templateCache sync.Map

// renderPrompt renders a Jinja2 prompt template by name (without extension).
// ctx is a map[string]interface{} of template variables.
func renderPrompt(name string, ctx map[string]interface{}) (string, error) {
	var tpl *pongo2.Template
	if cached, ok := templateCache.Load(name); ok {
		tpl = cached.(*pongo2.Template)
	} else {
		data, err := promptTemplates.ReadFile("prompts/" + name + ".j2")
		if err != nil {
			return "", fmt.Errorf("load template %s: %w", name, err)
		}
		tpl, err = pongo2.FromString(string(data))
		if err != nil {
			return "", fmt.Errorf("parse template %s: %w", name, err)
		}
		templateCache.Store(name, tpl)
	}
	if ctx == nil {
		ctx = map[string]interface{}{}
	}
	out, err := tpl.Execute(pongo2.Context(ctx))
	if err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return out, nil
}

// LoadRawPrompt reads a prompt file by name (without extension) from the embedded FS.
// It returns the raw text without any template rendering.
func LoadRawPrompt(name string) (string, error) {
	data, err := promptTemplates.ReadFile("prompts/" + name + ".j2")
	if err != nil {
		return "", fmt.Errorf("load prompt %s: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// toContext converts any value to pongo2.Context via JSON round-trip.
// Accepts map[string]interface{} directly or converts structs via JSON.
// Uses json.Decoder with UseNumber() so integer/float values are preserved as
// json.Number instead of float64, avoiding precision loss for large integers.
// time.Time fields are serialised as RFC3339 strings, which templates can use directly.
func toContext(data interface{}) (pongo2.Context, error) {
	if data == nil {
		return pongo2.Context{}, nil
	}
	if ctx, ok := data.(map[string]interface{}); ok {
		return pongo2.Context(ctx), nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal template data: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var ctx map[string]interface{}
	if err := dec.Decode(&ctx); err != nil {
		return nil, fmt.Errorf("unmarshal template context: %w", err)
	}
	return pongo2.Context(ctx), nil
}

// TemplateService 模板服务（保留向后兼容 API）
type TemplateService struct{}

// NewTemplateService 创建模板服务，预验证所有 .j2 模板语法
func NewTemplateService() (*TemplateService, error) {
	entries, err := promptTemplates.ReadDir("prompts")
	if err != nil {
		return nil, fmt.Errorf("read prompts dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".j2") {
			continue
		}
		data, err := promptTemplates.ReadFile("prompts/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read template %s: %w", entry.Name(), err)
		}
		name := strings.TrimSuffix(entry.Name(), ".j2")
		tpl, err := pongo2.FromString(string(data))
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", entry.Name(), err)
		}
		templateCache.Store(name, tpl)
	}
	return &TemplateService{}, nil
}

// Render 渲染模板（兼容旧 API，data 可为 map 或 struct 指针）
func (s *TemplateService) Render(name string, data interface{}) (string, error) {
	ctx, err := toContext(data)
	if err != nil {
		return "", err
	}
	var tpl *pongo2.Template
	if cached, ok := templateCache.Load(name); ok {
		tpl = cached.(*pongo2.Template)
	} else {
		raw, err := promptTemplates.ReadFile("prompts/" + name + ".j2")
		if err != nil {
			return "", fmt.Errorf("template not found: %s", name)
		}
		tpl, err = pongo2.FromString(string(raw))
		if err != nil {
			return "", fmt.Errorf("parse template %s: %w", name, err)
		}
		templateCache.Store(name, tpl)
	}
	out, err := tpl.Execute(ctx)
	if err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return out, nil
}

// RenderNovelOutlinePrompt 渲染小说大纲提示词
func (s *TemplateService) RenderNovelOutlinePrompt(data *NovelOutlineTemplateData) (string, error) {
	return s.Render("novel_outline", data)
}

// RenderChapterPrompt 渲染章节提示词
func (s *TemplateService) RenderChapterPrompt(data *ChapterTemplateData) (string, error) {
	return s.Render("chapter", data)
}

// RenderCharacterPrompt 渲染角色提示词
func (s *TemplateService) RenderCharacterPrompt(data *CharacterTemplateData) (string, error) {
	return s.Render("character", data)
}

// RenderScenePrompt 渲染场景提示词
func (s *TemplateService) RenderScenePrompt(data *SceneTemplateData) (string, error) {
	return s.Render("scene", data)
}

// RenderStoryboardPrompt 渲染分镜头脚本提示词
func (s *TemplateService) RenderStoryboardPrompt(data *StoryboardTemplateData) (string, error) {
	return s.Render("storyboard", data)
}

// ============================================
// 模板数据结构（向后兼容）
// ============================================

// NovelOutlineTemplateData 小说大纲模板数据
type NovelOutlineTemplateData struct {
	Title          string
	Genre          string
	Theme          string
	Style          string
	TargetAudience string
	ChapterCount   int
}

// ChapterTemplateData 章节模板数据
type ChapterTemplateData struct {
	Novel           NovelInfo
	Chapter         ChapterInfo
	ChapterNo       int
	WordCount       int
	Style           string
	UserPrompt      string
	RecentChapters  []ChapterInfo
	Characters      []CharacterInfo
	CharacterStates string
	Foreshadows     string
}

// CharacterInfo 角色信息
type CharacterInfo struct {
	Name        string
	Personality string
}

// CharacterTemplateData 角色模板数据
type CharacterTemplateData struct {
	NovelTitle      string
	NovelGenre      string
	NovelTheme      string
	Role            string
	PersonalityHint string
	RoleDescription string
}

// SceneTemplateData 场景模板数据
type SceneTemplateData struct {
	NovelTitle   string
	NovelGenre   string
	SceneType    string
	Mood         string
	TimeSetting  string
	Requirements string
}

// StoryboardCharacterInfo 分镜角色外貌信息
type StoryboardCharacterInfo struct {
	Name       string
	Appearance string
	HairColor  string
	Outfit     string
	Features   string
}

// StoryboardTemplateData 分镜头模板数据
type StoryboardTemplateData struct {
	NovelTitle     string
	ChapterNo      int
	ChapterContent string
	Characters     []StoryboardCharacterInfo
	ArtStyle       string
	ColorTone      string
	LightingStyle  string
}

// NovelInfo 小说信息
type NovelInfo struct {
	Title string
	Genre string
}

// ChapterInfo 章节信息
type ChapterInfo struct {
	ChapterNo int
	Title     string
	Summary   string
}
