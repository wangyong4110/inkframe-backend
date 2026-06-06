package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SkillService 技能服务
type SkillService struct {
	skillRepo *repository.SkillRepository
	novelRepo *repository.NovelRepository
	aiService *AIService
}

func NewSkillService(skillRepo *repository.SkillRepository, aiSvc *AIService) *SkillService {
	return &SkillService{
		skillRepo: skillRepo,
		aiService: aiSvc,
	}
}

// WithNovelRepo 注入小说仓库（可选，用于 AI 提示词中携带标题/类型）
func (s *SkillService) WithNovelRepo(r *repository.NovelRepository) *SkillService {
	s.novelRepo = r
	return s
}

// CreateSkill 创建技能
func (s *SkillService) CreateSkill(tenantID, novelID uint, req *model.CreateSkillRequest) (*model.Skill, error) {
	skill := &model.Skill{
		TenantID:    tenantID,
		NovelID:     novelID,
		Name:        req.Name,
		SkillType:   req.SkillType,
		Level:       req.Level,
		Description: req.Description,
		Effect:      req.Effect,
		Tags:        req.Tags,
		ChapterNo:   req.ChapterNo,
	}
	if skill.Level == 0 {
		skill.Level = 1
	}
	return skill, s.skillRepo.Create(skill)
}

// GetSkill 获取技能详情
func (s *SkillService) GetSkill(id uint) (*model.Skill, error) {
	return s.skillRepo.GetByID(id)
}

// ListSkills 列出小说下所有技能
func (s *SkillService) ListSkills(novelID uint) ([]*model.Skill, error) {
	return s.skillRepo.List(novelID)
}

// ListSkillsPaged 分页列出小说下的技能，返回数据列表和总数
func (s *SkillService) ListSkillsPaged(novelID uint, page, pageSize int) ([]*model.Skill, int64, error) {
	return s.skillRepo.ListPaged(novelID, page, pageSize)
}

// UpdateSkill 更新技能
func (s *SkillService) UpdateSkill(id uint, req *model.UpdateSkillRequest) (*model.Skill, error) {
	skill, err := s.skillRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("skill not found: %w", err)
	}
	if req.Name != "" {
		skill.Name = req.Name
	}
	if req.SkillType != "" {
		skill.SkillType = req.SkillType
	}
	if req.Level != 0 {
		skill.Level = req.Level
	}
	if req.Description != "" {
		skill.Description = req.Description
	}
	if req.Effect != "" {
		skill.Effect = req.Effect
	}
	if req.Tags != "" {
		skill.Tags = req.Tags
	}
	if req.ChapterNo != 0 {
		skill.ChapterNo = req.ChapterNo
	}
	return skill, s.skillRepo.Update(skill)
}

// BatchDeleteSkills 批量删除技能，仅删除属于指定小说的技能
func (s *SkillService) BatchDeleteSkills(novelID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return s.skillRepo.BatchDeleteByNovel(novelID, ids)
}

// DeleteSkill 删除技能（含租户校验）
func (s *SkillService) DeleteSkill(id, tenantID uint) error {
	skill, err := s.skillRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("not found")
	}
	if skill.TenantID != tenantID {
		return fmt.Errorf("not found")
	}
	return s.skillRepo.Delete(id)
}

// skillJSON is the AI response format for skill generation.
type skillJSON struct {
	Name        string `json:"name"`
	SkillType   string `json:"skill_type"`
	Level       int    `json:"level"`
	Description string `json:"description"`
	Effect      string `json:"effect"`
}

// parseSkillsJSON attempts to parse a []skillJSON from AI output using multiple strategies:
// 1. Standard json.Unmarshal after extractJSON cleanup
// 2. extractJSON again with re-attempt (same as step 1 but explicit)
// 3. Partial streaming recovery using json.Decoder
// Returns whatever was successfully parsed (at least an empty slice).
func parseSkillsJSON(raw string) ([]skillJSON, error) {
	cleaned := extractJSON(strings.TrimSpace(raw))

	// Strategy 1: standard unmarshal on cleaned output
	var extracted []skillJSON
	if err := json.Unmarshal([]byte(cleaned), &extracted); err == nil {
		return extracted, nil
	}

	// Strategy 2: partial array recovery via streaming Decoder
	start := strings.IndexByte(cleaned, '[')
	if start == -1 {
		start = strings.IndexByte(raw, '[')
	}
	if start >= 0 {
		src := cleaned[start:]
		dec := json.NewDecoder(strings.NewReader(src))
		if _, tokErr := dec.Token(); tokErr == nil { // consume '['
			for dec.More() {
				var sk skillJSON
				if dec.Decode(&sk) != nil {
					break // truncated — stop here
				}
				if sk.Name != "" {
					extracted = append(extracted, sk)
				}
			}
		}
		if len(extracted) > 0 {
			return extracted, nil
		}
	}

	return nil, fmt.Errorf("cannot parse skill JSON from response (len=%d)", len(raw))
}

// GenerateSkills 使用 AI 为小说生成技能体系
func (s *SkillService) GenerateSkills(tenantID, novelID uint) (result []*model.Skill, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("[SkillService] GenerateSkills: recovered panic: %v", r)
			if retErr == nil {
				retErr = fmt.Errorf("GenerateSkills: recovered panic: %v", r)
			}
		}
	}()
	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	prompt := fmt.Sprintf(
		`你是一名专业的玄幻/仙侠小说世界观设计师。请为小说《%s》（类型：%s）设计5-10个核心技能/功法体系。
要求：
1. 技能类型包括：主动/被动/天赋/武技/法术/功法
2. 每个技能有名称、类型、等级（1-10）、描述和效果
3. 体现该小说世界观的独特性

请严格以JSON数组返回，格式如下：
[{"name":"","skill_type":"","level":1,"description":"","effect":""}]
只返回JSON，不要其他内容。`,
		novelTitle, novelGenre,
	)

	aiResult, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_items", prompt, "",
		StoryboardOverrides{})
	if err != nil {
		return nil, fmt.Errorf("AI generation failed: %w", err)
	}

	extracted, parseErr := parseSkillsJSON(aiResult)
	if parseErr != nil {
		raw := aiResult
		if len(raw) > 500 {
			raw = raw[:500]
		}
		logger.Printf("[SkillService] GenerateSkills: JSON parse failed (raw: %s): %v", raw, parseErr)
		return nil, fmt.Errorf("AI returned unparseable skill data")
	}
	if len(extracted) == 0 {
		// AI 合法返回了空数组（小说无技能体系），区别于解析失败
		// 判断依据：原始响应中包含 [] 或 "skills":[]
		trimmed := strings.TrimSpace(aiResult)
		if strings.Contains(trimmed, "[]") || strings.Contains(trimmed, `"skills":[]`) || strings.Contains(trimmed, `"skills": []`) {
			return []*model.Skill{}, nil
		}
		return nil, fmt.Errorf("AI returned unparseable skill data")
	}

	existing, _ := s.skillRepo.List(novelID)
	byName := make(map[string]*model.Skill, len(existing))
	for _, sk := range existing {
		byName[sk.Name] = sk
	}

	upserted := make([]*model.Skill, 0, len(extracted))
	for _, e := range extracted {
		if e.Name == "" {
			continue
		}
		if sk, ok := byName[e.Name]; ok {
			// Update existing skill with fresh AI-generated values
			sk.Description = e.Description
			sk.Effect = e.Effect
			if e.SkillType != "" {
				sk.SkillType = e.SkillType
			}
			if e.Level != 0 {
				sk.Level = e.Level
			}
			if updateErr := s.skillRepo.Update(sk); updateErr != nil {
				logger.Printf("[SkillService] update skill %q failed: %v", e.Name, updateErr)
			}
			upserted = append(upserted, sk)
			continue
		}
		level := e.Level
		if level == 0 {
			level = 1
		}
		skill := &model.Skill{
			TenantID:    tenantID,
			NovelID:     novelID,
			Name:        e.Name,
			SkillType:   e.SkillType,
			Level:       level,
			Description: e.Description,
			Effect:      e.Effect,
		}
		if err := s.skillRepo.Create(skill); err != nil {
			continue
		}
		upserted = append(upserted, skill)
	}
	return upserted, nil
}

// GenerateSkillEffect 为技能生成效果图
func (s *SkillService) GenerateSkillEffect(tenantID, id uint, provider string) (*model.Skill, error) {
	skill, err := s.skillRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("skill not found: %w", err)
	}
	if skill.ImagePath != "" {
		return skill, nil // already generated, skip regeneration
	}
	visualPrompt := fmt.Sprintf("Magic skill effect for: %s. %s. Dynamic cinematic style, fantasy art", skill.Name, skill.Description)
	imageURL, imgErr := s.aiService.GenerateCharacterThreeView(context.Background(), tenantID, provider, visualPrompt, "", "fantasy", "", "")
	if imgErr != nil {
		logger.Printf("[SkillService] GenerateSkillEffect: image generation failed for skill %d: %v", skill.ID, imgErr)
		// Continue without image — skill metadata is still updated
	} else {
		skill.ImagePath = imageURL
	}
	return skill, s.skillRepo.Update(skill)
}
