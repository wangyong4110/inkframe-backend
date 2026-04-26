package service

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SkillService 技能管理业务逻辑
type SkillService struct {
	skillRepo     *repository.SkillRepository
	characterRepo *repository.CharacterRepository
	novelRepo     *repository.NovelRepository
	aiService     *AIService
}

func NewSkillService(
	skillRepo *repository.SkillRepository,
	characterRepo *repository.CharacterRepository,
	novelRepo *repository.NovelRepository,
	aiService *AIService,
) *SkillService {
	return &SkillService{
		skillRepo:     skillRepo,
		characterRepo: characterRepo,
		novelRepo:     novelRepo,
		aiService:     aiService,
	}
}

// CreateSkill 创建技能
func (s *SkillService) CreateSkill(novelID uint, req *model.CreateSkillRequest) (*model.Skill, error) {
	level := req.Level
	if level <= 0 {
		level = 1
	}
	maxLevel := req.MaxLevel
	if maxLevel <= 0 {
		maxLevel = 10
	}
	skill := &model.Skill{
		NovelID:           novelID,
		CharacterID:       req.CharacterID,
		ParentID:          req.ParentID,
		Name:              req.Name,
		Category:          req.Category,
		SkillType:         req.SkillType,
		Level:             level,
		MaxLevel:          maxLevel,
		Realm:             req.Realm,
		Description:       req.Description,
		Effect:            req.Effect,
		FlavorText:        req.FlavorText,
		Cost:              req.Cost,
		Cooldown:          req.Cooldown,
		Tags:              req.Tags,
		AcquiredChapterNo: req.AcquiredChapterNo,
		AcquiredDesc:      req.AcquiredDesc,
		Status:            "active",
		Notes:             req.Notes,
	}
	if err := s.skillRepo.Create(skill); err != nil {
		return nil, err
	}
	return skill, nil
}

// GetSkill 获取技能详情
func (s *SkillService) GetSkill(id uint) (*model.Skill, error) {
	return s.skillRepo.GetByID(id)
}

// ListSkills 查询技能列表
func (s *SkillService) ListSkills(novelID uint, opts repository.ListSkillsOpts) ([]*model.Skill, error) {
	return s.skillRepo.ListByNovel(novelID, opts)
}

// UpdateSkill 更新技能
func (s *SkillService) UpdateSkill(id uint, req *model.UpdateSkillRequest) (*model.Skill, error) {
	skill, err := s.skillRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		skill.Name = req.Name
	}
	if req.Category != "" {
		skill.Category = req.Category
	}
	if req.SkillType != "" {
		skill.SkillType = req.SkillType
	}
	if req.Level > 0 {
		skill.Level = req.Level
	}
	if req.MaxLevel > 0 {
		skill.MaxLevel = req.MaxLevel
	}
	if req.Realm != "" {
		skill.Realm = req.Realm
	}
	if req.Description != "" {
		skill.Description = req.Description
	}
	if req.Effect != "" {
		skill.Effect = req.Effect
	}
	if req.FlavorText != "" {
		skill.FlavorText = req.FlavorText
	}
	if req.Cost != "" {
		skill.Cost = req.Cost
	}
	if req.Cooldown != "" {
		skill.Cooldown = req.Cooldown
	}
	if req.Tags != "" {
		skill.Tags = req.Tags
	}
	if req.Status != "" {
		skill.Status = req.Status
	}
	if req.Notes != "" {
		skill.Notes = req.Notes
	}
	// nullable fields — always overwrite (allow clearing)
	skill.CharacterID = req.CharacterID
	skill.ParentID = req.ParentID
	if req.AcquiredChapterNo != nil {
		skill.AcquiredChapterNo = req.AcquiredChapterNo
	}
	if req.AcquiredDesc != "" {
		skill.AcquiredDesc = req.AcquiredDesc
	}
	return skill, s.skillRepo.Update(skill)
}

// DeleteSkill 删除技能
func (s *SkillService) DeleteSkill(id uint) error {
	return s.skillRepo.Delete(id)
}

// GenerateSkills 使用AI批量生成技能
func (s *SkillService) GenerateSkills(novelID uint, req *model.GenerateSkillsRequest) ([]*model.Skill, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, fmt.Errorf("novel not found: %w", err)
	}

	var charSection string
	if req.CharacterID != nil {
		char, cerr := s.characterRepo.GetByID(*req.CharacterID)
		if cerr == nil && char != nil {
			charSection = fmt.Sprintf("\n角色信息：\n- 姓名：%s\n- 定位：%s\n- 能力：%s\n- 背景：%s",
				char.Name, char.Role, char.Abilities, char.Background)
		}
	}

	count := req.Count
	if count <= 0 {
		count = 3
	}
	if count > 10 {
		count = 10
	}

	prompt := fmt.Sprintf(`小说《%s》，题材：%s
世界观简述：%s%s

额外要求：%s

请为以上设定设计 %d 个技能，以 JSON 数组输出，每个元素包含以下字段：
name（技能名称）, category（分类）, skill_type（类型）, level（当前等级）, max_level（最高等级）,
realm（境界要求）, description（简述）, effect（效果详情）, flavor_text（世界观描述文字）,
cost（消耗）, cooldown（冷却）, tags（标签，逗号分隔）

category 可选值：武技/法术/身法/心法/阵法/神通/秘法/特性
skill_type 可选值：active/passive/toggle/ultimate

只输出合法 JSON 数组，不包含任何额外文字或 markdown 代码块。`,
		novel.Title, novel.Genre, novel.Description,
		charSection, req.Hints, count,
	)

	resp, err := s.aiService.Generate(novelID, "skill_generation", prompt)
	if err != nil {
		return nil, fmt.Errorf("AI generation failed: %w", err)
	}

	jsonStr := extractJSON(resp)

	type rawSkill struct {
		Name        string `json:"name"`
		Category    string `json:"category"`
		SkillType   string `json:"skill_type"`
		Level       int    `json:"level"`
		MaxLevel    int    `json:"max_level"`
		Realm       string `json:"realm"`
		Description string `json:"description"`
		Effect      string `json:"effect"`
		FlavorText  string `json:"flavor_text"`
		Cost        string `json:"cost"`
		Cooldown    string `json:"cooldown"`
		Tags        string `json:"tags"`
	}

	var rawSkills []rawSkill
	if err := json.Unmarshal([]byte(jsonStr), &rawSkills); err != nil {
		log.Printf("SkillService.GenerateSkills: parse JSON failed: %v\nraw=%s", err, jsonStr)
		return nil, fmt.Errorf("failed to parse AI response: %w", err)
	}

	skills := make([]*model.Skill, 0, len(rawSkills))
	for _, rs := range rawSkills {
		level := rs.Level
		if level <= 0 {
			level = 1
		}
		maxLevel := rs.MaxLevel
		if maxLevel <= 0 {
			maxLevel = 10
		}
		skills = append(skills, &model.Skill{
			NovelID:     novelID,
			CharacterID: req.CharacterID,
			Name:        rs.Name,
			Category:    rs.Category,
			SkillType:   rs.SkillType,
			Level:       level,
			MaxLevel:    maxLevel,
			Realm:       rs.Realm,
			Description: rs.Description,
			Effect:      rs.Effect,
			FlavorText:  rs.FlavorText,
			Cost:        rs.Cost,
			Cooldown:    rs.Cooldown,
			Tags:        rs.Tags,
			Status:      "active",
		})
	}
	if err := s.skillRepo.BatchCreate(skills); err != nil {
		return nil, fmt.Errorf("save skills: %w", err)
	}
	return skills, nil
}
