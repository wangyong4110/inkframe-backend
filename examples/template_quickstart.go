package main

import (
	"log"

	"github.com/inkframe/inkframe-backend/internal/service"
)

// main 函数集成示例 - 如何初始化和使用模板服务
func main() {
	// ============================================
	// 1. 初始化模板服务
	// ============================================
	templateSvc, err := service.NewTemplateService()
	if err != nil {
		log.Fatalf("Failed to initialize template service: %v", err)
	}
	log.Println("✓ Template service initialized")

	// ============================================
	// 2. 使用模板服务渲染提示词
	// ============================================

	// 示例 1: 渲染小说大纲提示词
	novelData := &service.NovelOutlineTemplateData{
		Title:          "星辰变",
		Genre:          "玄幻",
		Theme:          "从凡人到宇宙主宰的成长之路",
		Style:          "热血、升级流",
		TargetAudience: "18-35岁男性",
		ChapterCount:   300,
	}

	novelPrompt, err := templateSvc.RenderNovelOutlinePrompt(novelData)
	if err != nil {
		log.Fatalf("Failed to render novel outline prompt: %v", err)
	}
	log.Printf("✓ Novel outline prompt generated (%d chars)", len(novelPrompt))

	// 示例 2: 渲染章节提示词
	chapterData := &service.ChapterTemplateData{
		Novel: service.NovelInfo{
			Title: "星辰变",
			Genre: "玄幻",
		},
		Chapter: service.ChapterInfo{
			ChapterNo: 1,
			Title:     "第一章：觉醒",
			Summary:   "主角获得神秘传承",
		},
		ChapterNo:  1,
		WordCount:  3000,
		Style:      "热血",
		UserPrompt: "开头要震撼，主角要霸气",
		RecentChapters: []service.ChapterInfo{},
	}

	chapterPrompt, err := templateSvc.RenderChapterPrompt(chapterData)
	if err != nil {
		log.Fatalf("Failed to render chapter prompt: %v", err)
	}
	log.Printf("✓ Chapter prompt generated (%d chars)", len(chapterPrompt))

	// 示例 3: 渲染角色提示词
	charData := &service.CharacterTemplateData{
		NovelTitle:       "星辰变",
		NovelGenre:       "玄幻",
		NovelTheme:       "成长与冒险",
		Role:             "主角",
		PersonalityHint:  "坚韧、不屈服、有正义感",
		RoleDescription:  "故事的绝对核心",
	}

	charPrompt, err := templateSvc.RenderCharacterPrompt(charData)
	if err != nil {
		log.Fatalf("Failed to render character prompt: %v", err)
	}
	log.Printf("✓ Character prompt generated (%d chars)", len(charPrompt))

	// ============================================
	// 3. 在服务中集成模板服务
	// ============================================

	// 初始化其他服务（传入模板服务）
	// novelSvc := service.NewNovelService(novelRepo, chapterRepo, templateSvc, aiSvc)
	// chapterSvc := service.NewChapterService(chapterRepo, characterRepo, templateSvc, aiSvc)
	// ...

	// ============================================
	// 4. 实际使用
	// ============================================

	// novelOutline, err := novelSvc.GenerateOutline(req)
	// chapter, err := chapterSvc.GenerateChapter(novelID, chapterNo, userPrompt)
	// ...

	log.Println("\n✓ All prompts generated successfully!")
	log.Println("\n提示词模板系统已就绪，可以在服务中使用了。")
	log.Println("\n下一步：")
	log.Println("1. 查看 PROMPT_TEMPLATES.md 了解详细用法")
	log.Println("2. 查看 template_service_example.go 查看集成示例")
	log.Println("3. 查看 MIGRATION_GUIDE.md 了解如何迁移现有代码")
}
