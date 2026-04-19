package service

// 本文件展示如何使用 TemplateService 替换硬编码的提示词

// 示例 1: 在 NovelService 中使用模板
/*
type NovelService struct {
	novelRepo   *repository.NovelRepository
	templateSvc *TemplateService
}

func (s *NovelService) GenerateOutline(req *GenerateOutlineRequest) (*NovelOutline, error) {
	// 使用模板渲染提示词
	data := &NovelOutlineTemplateData{
		Title:          req.Title,
		Genre:          req.Genre,
		Theme:          req.Theme,
		Style:          req.Style,
		TargetAudience: req.TargetAudience,
		ChapterCount:   req.ChapterCount,
	}

	prompt, err := s.templateSvc.RenderNovelOutlinePrompt(data)
	if err != nil {
		return nil, err
	}

	// 调用 AI 生成
	response, err := s.aiService.Generate(context.Background(), &ai.GenerateRequest{
		Prompt: prompt,
		Model:  "gpt-4",
		MaxTokens: 4000,
	})

	if err != nil {
		return nil, err
	}

	// 解析响应
	var outline NovelOutline
	if err := json.Unmarshal([]byte(response.Content), &outline); err != nil {
		return nil, err
	}

	return &outline, nil
}
*/

// 示例 2: 在 ChapterService 中使用模板
/*
func (s *ChapterService) GenerateChapter(novelID uint, chapterNo int, userPrompt string) (*Chapter, error) {
	// 获取小说信息
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}

	// 获取前几章作为上下文
	recentChapters, _ := s.chapterRepo.GetRecent(novelID, chapterNo, 3)

	// 获取角色列表
	characters, _ := s.characterRepo.ListByNovel(novelID)

	// 使用模板渲染提示词
	data := &ChapterTemplateData{
		Novel: NovelInfo{
			Title: novel.Title,
			Genre: novel.Genre,
		},
		Chapter: ChapterInfo{
			ChapterNo: chapterNo,
			Title:     fmt.Sprintf("第%d章", chapterNo),
			Summary:   "",
		},
		ChapterNo:   chapterNo,
		WordCount:   3000,
		Style:       novel.StylePrompt,
		UserPrompt:  userPrompt,
		RecentChapters: convertToChapterInfo(recentChapters),
	}

	prompt, err := s.templateSvc.RenderChapterPrompt(data)
	if err != nil {
		return nil, err
	}

	// 调用 AI 生成章节内容
	response, err := s.aiService.Generate(context.Background(), &ai.GenerateRequest{
		Prompt: prompt,
		Model:  novel.AIModel,
		MaxTokens: 4096,
	})

	if err != nil {
		return nil, err
	}

	// 创建章节记录
	chapter := &Chapter{
		NovelID:    novelID,
		ChapterNo:  chapterNo,
		Title:      fmt.Sprintf("第%d章", chapterNo),
		Content:    response.Content,
		WordCount:  len([]rune(response.Content)),
		Status:     "draft",
	}

	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}

	return chapter, nil
}
*/

// 示例 3: 在 VideoEnhancementService 中使用模板
/*
func (s *VideoEnhancementService) GenerateStoryboard(chapter *Chapter) (*Storyboard, error) {
	// 使用模板渲染提示词
	data := &StoryboardTemplateData{
		NovelTitle:     chapter.Novel.Title,
		ChapterNo:      chapter.ChapterNo,
		ChapterContent: chapter.Content,
	}

	prompt, err := s.templateSvc.RenderStoryboardPrompt(data)
	if err != nil {
		return nil, err
	}

	// 调用 AI 生成分镜头脚本
	response, err := s.aiService.Generate(context.Background(), &ai.GenerateRequest{
		Prompt: prompt,
		Model:  "gpt-4",
		MaxTokens: 4000,
	})

	if err != nil {
		return nil, err
	}

	// 解析响应
	var storyboard Storyboard
	if err := json.Unmarshal([]byte(response.Content), &storyboard); err != nil {
		return nil, err
	}

	return &storyboard, nil
}
*/

// 辅助函数：转换 Chapter 列片
/*
func convertToChapterInfo(chapters []*Chapter) []ChapterInfo {
	result := make([]ChapterInfo, len(chapters))
	for i, ch := range chapters {
		result[i] = ChapterInfo{
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			Summary:   ch.Summary,
		}
	}
	return result
}
*/

// 集成到 main.go 中的示例
/*
func main() {
	// 初始化模板服务
	templateSvc, err := service.NewTemplateService()
	if err != nil {
		log.Fatalf("Failed to initialize template service: %v", err)
	}

	// 初始化 NovelService 并传入模板服务
	novelService := service.NewNovelService(novelRepo, chapterRepo, templateSvc, aiService)

	// ... 其他初始化代码
}
*/
