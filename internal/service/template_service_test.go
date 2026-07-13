package service

import (
	"strings"
	"testing"
)

// TestAllTemplatesParse 确保 prompts/ 下所有顶层 .j2 模板语法有效
// （fragments/ 下的片段由被 include 的模板惰性验证）。
func TestAllTemplatesParse(t *testing.T) {
	entries, err := promptTemplates.ReadDir("prompts")
	if err != nil {
		t.Fatalf("read prompts dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".j2") {
			continue
		}
		if _, err := templateSet.FromFile("prompts/" + entry.Name()); err != nil {
			t.Errorf("parse template %s: %v", entry.Name(), err)
		}
	}
}

func TestRenderStoryboardGeneratePrompt(t *testing.T) {
	prompt, err := renderPrompt("storyboard_generate", map[string]interface{}{
		"ExpectedShots": 8,
		"Content":       "第一章的内容，主角踏上了冒险之旅。",
		"NovelTitle":    "我的小说",
		"ChapterNo":     1,
		"IsEn":          false,
		"IsImageEn":     false,
	})
	if err != nil {
		t.Fatalf("render storyboard_generate: %v", err)
	}
	if prompt == "" {
		t.Error("storyboard_generate prompt is empty")
	}
}

func TestRenderPromptUnknownTemplate(t *testing.T) {
	if _, err := renderPrompt("nonexistent", nil); err == nil {
		t.Error("expected error for non-existent template")
	}
}
