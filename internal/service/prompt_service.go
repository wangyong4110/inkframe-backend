package service

import (
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// PromptService 提示词服务
type PromptService struct {
	templateRepo interface {
		GetByGenreAndStage(genre, stage string) (*model.PromptTemplate, error)
		GetByID(id uint) (*model.PromptTemplate, error)
		List() ([]*model.PromptTemplate, error)
	}
}

func NewPromptService(repo interface {
	GetByGenreAndStage(genre, stage string) (*model.PromptTemplate, error)
	GetByID(id uint) (*model.PromptTemplate, error)
	List() ([]*model.PromptTemplate, error)
}) *PromptService {
	return &PromptService{templateRepo: repo}
}

// RenderPrompt 渲染提示词
func (s *PromptService) RenderPrompt(templateID uint, variables map[string]interface{}) (string, error) {
	tmpl, err := s.templateRepo.GetByID(templateID)
	if err != nil {
		return "", err
	}

	return s.render(tmpl.Template, variables), nil
}

// BuildOutlinePrompt 构建大纲提示词
func (s *PromptService) BuildOutlinePrompt(novel *model.Novel, req *GenerateOutlineRequest) string {
	var sb strings.Builder

	// 系统提示
	sb.WriteString("你是一位专业的小说作家，擅长创作中长篇小说。\n\n")

	// 用户需求
	sb.WriteString(fmt.Sprintf("请为小说《%s》生成一个详细的大纲。\n\n", novel.Title))

	if novel.Description != "" {
		sb.WriteString(fmt.Sprintf("故事简介：%s\n\n", novel.Description))
	}

	if novel.Worldview != nil {
		sb.WriteString("【世界观设定】\n")
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString(fmt.Sprintf("修炼体系：%s\n", novel.Worldview.MagicSystem))
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString(fmt.Sprintf("地理环境：%s\n", novel.Worldview.Geography))
		}
		if novel.Worldview.Culture != "" {
			sb.WriteString(fmt.Sprintf("文化背景：%s\n", novel.Worldview.Culture))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("请生成%d章的大纲。\n", req.ChapterNum))

	// 输出格式
	sb.WriteString("\n请以JSON格式返回，格式如下：\n")
	sb.WriteString(`{"title":"小说标题","chapters":[{"chapter_no":1,"title":"章节标题","summary":"章节概述","word_count":2500,"plot_points":["剧情点1","剧情点2"]}]}`)

	return sb.String()
}

// BuildChapterPrompt 构建章节提示词
func (s *PromptService) BuildChapterPrompt(
	novel *model.Novel,
	chapter *model.Chapter,
	recentChapters []*model.Chapter,
	characters []*model.Character,
	characterSnapshots []*model.CharacterStateSnapshot,
	unfulfilledForeshadows []*model.KnowledgeBase,
) string {
	var sb strings.Builder

	// 系统提示
	sb.WriteString("你是一位专业的小说作家，创作内容需要：\n")
	sb.WriteString("1. 保持与前文的剧情连贯性\n")
	sb.WriteString("2. 角色性格和对话风格保持一致\n")
	sb.WriteString("3. 遵循世界观设定\n")
	sb.WriteString("4. 适当埋下伏笔并呼应已有伏笔\n")
	sb.WriteString("5. 语言生动，描写细腻\n\n")

	// 小说信息
	sb.WriteString(fmt.Sprintf("【小说标题】%s\n\n", novel.Title))

	// 世界观
	if novel.Worldview != nil {
		sb.WriteString("【世界观设定】\n")
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString(fmt.Sprintf("- 修炼体系：%s\n", novel.Worldview.MagicSystem))
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString(fmt.Sprintf("- 地理环境：%s\n", novel.Worldview.Geography))
		}
		if novel.Worldview.Culture != "" {
			sb.WriteString(fmt.Sprintf("- 文化背景：%s\n", novel.Worldview.Culture))
		}
		sb.WriteString("\n")
	}

	// 角色信息
	if len(characters) > 0 {
		sb.WriteString("【主要角色设定】\n")
		for _, char := range characters {
			sb.WriteString(fmt.Sprintf("- %s：%s\n", char.Name, char.Personality))
		}
		sb.WriteString("\n")
	}

	// 角色当前状态快照
	if len(characterSnapshots) > 0 {
		sb.WriteString("【角色当前状态（上章末）】\n")
		for _, snap := range characterSnapshots {
			sb.WriteString(fmt.Sprintf("- 角色ID[%d]：位置=%s，情绪=%s，目标=%s，能力等级=%d\n",
				snap.CharacterID, snap.Location, snap.Mood, snap.Motivation, snap.PowerLevel))
		}
		sb.WriteString("⚠️ 本章角色行为必须与上述状态保持连续性\n\n")
	}

	// 前情提要
	if len(recentChapters) > 0 {
		sb.WriteString("【前情提要】\n")
		for i := len(recentChapters) - 1; i >= 0; i-- {
			ch := recentChapters[i]
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, ch.Summary))
		}
		sb.WriteString("\n")
	}

	// 未兑现伏笔
	if len(unfulfilledForeshadows) > 0 {
		sb.WriteString("【待兑现伏笔（可酌情呼应）】\n")
		for _, kb := range unfulfilledForeshadows {
			sb.WriteString(fmt.Sprintf("- %s：%s\n", kb.Title, kb.Content))
		}
		sb.WriteString("\n")
	}

	// 章节要求
	sb.WriteString("【章节要求】\n")
	sb.WriteString(fmt.Sprintf("- 章节标题：%s\n", chapter.Title))
	sb.WriteString("- 字数要求：2000-3000字\n")

	return sb.String()
}

func (s *PromptService) render(template string, variables map[string]interface{}) string {
	result := template

	for key, value := range variables {
		placeholder := fmt.Sprintf("{{%s}}", key)
		var replacement string
		switch v := value.(type) {
		case string:
			replacement = v
		case int, int64, float64:
			replacement = fmt.Sprintf("%v", v)
		default:
			replacement = fmt.Sprintf("%v", v)
		}
		result = strings.ReplaceAll(result, placeholder, replacement)
	}

	return result
}
