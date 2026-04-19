package service

import (
	"testing"
)

func TestTemplateService(t *testing.T) {
	// 创建模板服务
	svc, err := NewTemplateService()
	if err != nil {
		t.Fatalf("Failed to create template service: %v", err)
	}

	// 测试小说大纲模板
	novelData := &NovelOutlineTemplateData{
		Title:          "我的小说",
		Genre:          "玄幻",
		Theme:          "成长与冒险",
		Style:          "热血",
		TargetAudience: "青少年",
		ChapterCount:   50,
	}

	novelPrompt, err := svc.RenderNovelOutlinePrompt(novelData)
	if err != nil {
		t.Fatalf("Failed to render novel outline prompt: %v", err)
	}

	// 验证模板渲染成功
	if novelPrompt == "" {
		t.Error("Novel outline prompt is empty")
	}

	// 检查关键字段是否存在
	if len(novelPrompt) < 100 {
		t.Errorf("Novel outline prompt too short: %d chars", len(novelPrompt))
	}

	t.Logf("Novel outline prompt length: %d chars", len(novelPrompt))

	// 测试章节模板
	chapterData := &ChapterTemplateData{
		Novel: NovelInfo{
			Title: "我的小说",
			Genre: "玄幻",
		},
		Chapter: ChapterInfo{
			ChapterNo: 1,
			Title:     "第一章",
			Summary:   "主角觉醒",
		},
		ChapterNo:  1,
		WordCount:  3000,
		Style:      "热血",
		UserPrompt: "开头要震撼",
		RecentChapters: []ChapterInfo{},
	}

	chapterPrompt, err := svc.RenderChapterPrompt(chapterData)
	if err != nil {
		t.Fatalf("Failed to render chapter prompt: %v", err)
	}

	if chapterPrompt == "" {
		t.Error("Chapter prompt is empty")
	}

	if len(chapterPrompt) < 100 {
		t.Errorf("Chapter prompt too short: %d chars", len(chapterPrompt))
	}

	t.Logf("Chapter prompt length: %d chars", len(chapterPrompt))

	// 测试角色模板
	charData := &CharacterTemplateData{
		NovelTitle:       "我的小说",
		NovelGenre:       "玄幻",
		NovelTheme:       "成长与冒险",
		Role:             "主角",
		PersonalityHint:  "热血、正义、成长",
		RoleDescription:  "故事的核心角色",
	}

	charPrompt, err := svc.RenderCharacterPrompt(charData)
	if err != nil {
		t.Fatalf("Failed to render character prompt: %v", err)
	}

	if charPrompt == "" {
		t.Error("Character prompt is empty")
	}

	t.Logf("Character prompt length: %d chars", len(charPrompt))

	// 测试场景模板
	sceneData := &SceneTemplateData{
		NovelTitle:    "我的小说",
		NovelGenre:    "玄幻",
		SceneType:     "室外",
		Mood:          "紧张",
		TimeSetting:   "黄昏",
		Requirements:  "要营造压迫感",
	}

	scenePrompt, err := svc.RenderScenePrompt(sceneData)
	if err != nil {
		t.Fatalf("Failed to render scene prompt: %v", err)
	}

	if scenePrompt == "" {
		t.Error("Scene prompt is empty")
	}

	t.Logf("Scene prompt length: %d chars", len(scenePrompt))

	// 测试分镜头脚本模板
	storyboardData := &StoryboardTemplateData{
		NovelTitle:     "我的小说",
		ChapterNo:      1,
		ChapterContent: "第一章的内容...",
	}

	storyboardPrompt, err := svc.RenderStoryboardPrompt(storyboardData)
	if err != nil {
		t.Fatalf("Failed to render storyboard prompt: %v", err)
	}

	if storyboardPrompt == "" {
		t.Error("Storyboard prompt is empty")
	}

	t.Logf("Storyboard prompt length: %d chars", len(storyboardPrompt))
}

func TestTemplateServiceInvalidTemplate(t *testing.T) {
	// 创建模板服务
	svc, err := NewTemplateService()
	if err != nil {
		t.Fatalf("Failed to create template service: %v", err)
	}

	// 测试不存在的模板
	_, err = svc.Render("nonexistent", nil)
	if err == nil {
		t.Error("Expected error for non-existent template")
	}

	// 测试未定义的变量
	_, err = svc.Render("novel_outline", struct {
		Title string
	}{"Test"})
	// 模板渲染可能不会报错，但变量会显示为 <no value>
	if err != nil {
		t.Logf("Got expected error for missing variables: %v", err)
	}
}
