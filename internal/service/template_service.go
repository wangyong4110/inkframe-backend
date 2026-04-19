package service

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed
//go:embed ../../config/prompts/chapter.tmpl
//go:embed ../../config/prompts/character.tmpl
//go:embed ../../config/prompts/novel_outline.tmpl
//go:embed ../../config/prompts/scene.tmpl
//go:embed ../../config/prompts/storyboard.tmpl

var promptTemplates embed.FS

// TemplateService 模板服务
type TemplateService struct {
	templates map[string]*template.Template
}

// NewTemplateService 创建模板服务
func NewTemplateService() (*TemplateService, error) {
	svc := &TemplateService{
		templates: make(map[string]*template.Template),
	}

	// 加载所有模板文件
	entries, err := promptTemplates.ReadDir("config/prompts")
	if err != nil {
		return nil, fmt.Errorf("failed to read templates directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// 读取模板文件
		content, err := promptTemplates.ReadFile("config/prompts/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("failed to read template %s: %w", entry.Name(), err)
		}

		// 解析模板
		tmplName := entry.Name()[:len(entry.Name())-5] // 去掉 .tmpl 扩展名
		tmpl, err := template.New(tmplName).Parse(string(content))
		if err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %w", entry.Name(), err)
		}

		svc.templates[tmplName] = tmpl
	}

	return svc, nil
}

// Render 渲染模板
func (s *TemplateService) Render(name string, data interface{}) (string, error) {
	tmpl, ok := s.templates[name]
	if !ok {
		return "", fmt.Errorf("template not found: %s", name)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", name, err)
	}

	return buf.String(), nil
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
// 模板数据结构
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
	Novel         NovelInfo
	Chapter       ChapterInfo
	ChapterNo     int
	WordCount     int
	Style         string
	UserPrompt    string
	RecentChapters []ChapterInfo
}

// CharacterTemplateData 角色模板数据
type CharacterTemplateData struct {
	NovelTitle       string
	NovelGenre       string
	NovelTheme       string
	Role             string
	PersonalityHint  string
	RoleDescription  string
}

// SceneTemplateData 场景模板数据
type SceneTemplateData struct {
	NovelTitle    string
	NovelGenre    string
	SceneType     string
	Mood          string
	TimeSetting   string
	Requirements  string
}

// StoryboardTemplateData 分镜头模板数据
type StoryboardTemplateData struct {
	NovelTitle     string
	ChapterNo      int
	ChapterContent string
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
