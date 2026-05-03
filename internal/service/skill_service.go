package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"text/template"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SkillService 技能管理业务逻辑
type SkillService struct {
	skillRepo     *repository.SkillRepository
	characterRepo *repository.CharacterRepository
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository // optional, for AIExtractChapterSkills
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

func (s *SkillService) WithChapterRepo(r *repository.ChapterRepository) *SkillService {
	s.chapterRepo = r
	return s
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
	if req.EffectVisualPrompt != "" {
		skill.EffectVisualPrompt = req.EffectVisualPrompt
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
func (s *SkillService) GenerateSkills(tenantID, novelID uint, req *model.GenerateSkillsRequest) ([]*model.Skill, error) {
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

	resp, err := s.aiService.GenerateWithProvider(tenantID, novelID, "skill_generation", prompt, "")
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
		log.Printf("SkillService.GenerateSkills: parse JSON failed: %v (raw first 200: %.200s)", err, jsonStr)
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

// GenerateSkillEffect 为技能生成释放特效图片
func (s *SkillService) GenerateSkillEffect(id uint) (*model.Skill, error) {
	skill, err := s.skillRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("skill not found: %w", err)
	}

	prompt := skill.EffectVisualPrompt
	if prompt == "" {
		prompt = buildSkillEffectPrompt(skill)
	}

	url, err := s.aiService.GenerateCharacterThreeView(context.Background(), 0, "", prompt, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("generate effect image failed: %w", err)
	}

	skill.EffectImageURL = url
	return skill, s.skillRepo.Update(skill)
}

// buildSkillEffectPrompt 根据技能属性自动构建特效图片提示词
func buildSkillEffectPrompt(skill *model.Skill) string {
	parts := []string{}

	// Category-specific visual style
	categoryVisuals := map[string]string{
		"武技": "martial arts energy burst, sword aura, physical force wave",
		"法术": "magical spell effect, arcane glow, mystical runes",
		"身法": "speed blur, movement afterimage, agile silhouette",
		"心法": "inner energy swirl, golden qi circulation, meditation aura",
		"阵法": "formation array, geometric patterns, barrier glow",
		"神通": "divine power manifestation, heavenly light, godly aura",
		"秘法": "forbidden dark magic, mysterious shadow tendrils, ancient runes",
		"特性": "special ability glow, unique power field",
	}
	if vis, ok := categoryVisuals[skill.Category]; ok {
		parts = append(parts, vis)
	}

	parts = append(parts, fmt.Sprintf("skill name: %s", skill.Name))

	if skill.Effect != "" && len(skill.Effect) < 100 {
		parts = append(parts, skill.Effect)
	}

	// Skill type visual hints
	typeVisuals := map[string]string{
		"active":   "active release, dynamic explosion",
		"passive":  "subtle aura glow, passive energy field",
		"toggle":   "transformation effect, state change",
		"ultimate": "ultimate technique, massive energy release, climactic visual",
	}
	if tv, ok := typeVisuals[skill.SkillType]; ok {
		parts = append(parts, tv)
	}

	parts = append(parts, "fantasy art, concept art, dramatic lighting, vivid colors, high detail, no background text")

	return strings.Join(parts, ", ")
}

// extractSkillsFromContent 从章节内容中提取技能（纯 AI 提取，不操作 DB）
type skillExtractedJSON struct {
	Name          string `json:"name"`
	Category      string `json:"category"`
	SkillType     string `json:"skill_type"`
	Level         int    `json:"level"`
	Realm         string `json:"realm"`
	Description   string `json:"description"`
	Effect        string `json:"effect"`
	FlavorText    string `json:"flavor_text"`
	Cost          string `json:"cost"`
	CharacterName string `json:"character_name"`
	ChapterNo     int    // 记录来自哪章（不是 JSON 字段）
}

func (s *SkillService) extractSkillsFromContent(
	tenantID, novelID uint,
	novelTitle, genre, content string,
	existingNames []string,
	chapterNo int,
) ([]skillExtractedJSON, error) {
	tmplStr := loadPromptTemplate("extract_chapter_skills.tmpl")
	tmpl, err := template.New("extract_chapter_skills").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"ExistingNames": existingNames,
		"Content":       content,
	}); err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_chapter_skills", buf.String(), "")
	if err != nil {
		return nil, err
	}

	type rawSkill struct {
		Name          string `json:"name"`
		Category      string `json:"category"`
		SkillType     string `json:"skill_type"`
		Level         int    `json:"level"`
		Realm         string `json:"realm"`
		Description   string `json:"description"`
		Effect        string `json:"effect"`
		FlavorText    string `json:"flavor_text"`
		Cost          string `json:"cost"`
		CharacterName string `json:"character_name"`
	}
	cleaned := extractJSON(strings.TrimSpace(result))
	var raw []rawSkill
	if err := json.Unmarshal([]byte(cleaned), &raw); err != nil {
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, e := dec.Token(); e == nil {
			for dec.More() {
				var sk rawSkill
				if dec.Decode(&sk) == nil && sk.Name != "" {
					raw = append(raw, sk)
				}
			}
		}
	}
	skills := make([]skillExtractedJSON, 0, len(raw))
	for _, r := range raw {
		if r.Name == "" {
			continue
		}
		skills = append(skills, skillExtractedJSON{
			Name:          r.Name,
			Category:      r.Category,
			SkillType:     r.SkillType,
			Level:         r.Level,
			Realm:         r.Realm,
			Description:   r.Description,
			Effect:        r.Effect,
			FlavorText:    r.FlavorText,
			Cost:          r.Cost,
			CharacterName: r.CharacterName,
			ChapterNo:     chapterNo,
		})
	}
	return skills, nil
}

// AIExtractAllFromNovel 逐章并发提取技能：先并发 AI 提取，再统一去重、入库
func (s *SkillService) AIExtractAllFromNovel(tenantID, novelID uint) ([]*model.Skill, error) {
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, fmt.Errorf("load chapters: %w", err)
	}

	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	// 已有技能名单
	existing, _ := s.skillRepo.ListByNovel(novelID, repository.ListSkillsOpts{})
	existingNames := make([]string, 0, len(existing))
	existingNameSet := make(map[string]bool, len(existing))
	for _, sk := range existing {
		existingNames = append(existingNames, sk.Name)
		existingNameSet[strings.ToLower(sk.Name)] = true
	}

	// 过滤有内容的章节（最多 10 章）
	const maxChapters = 10
	const concurrency = 3
	var candidates []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" || ch.Summary != "" {
			candidates = append(candidates, ch)
			if len(candidates) >= maxChapters {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no chapter content available")
	}

	// 并发提取（纯 AI 调用，不操作 DB）
	type chResult struct {
		skills []skillExtractedJSON
		err    error
	}
	results := make([]chResult, len(candidates))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, ch := range candidates {
		wg.Add(1)
		go func(idx int, c *model.Chapter) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content := c.Content
			if content == "" {
				content = c.Summary
			}
			skills, err := s.extractSkillsFromContent(tenantID, novelID, novelTitle, novelGenre, content, existingNames, c.ChapterNo)
			results[idx] = chResult{skills, err}
		}(i, ch)
	}
	wg.Wait()

	// 合并去重（按小写名字，保留第一次出现）
	seen := make(map[string]bool)
	for k := range existingNameSet {
		seen[k] = true
	}
	var allSkills []skillExtractedJSON
	for _, r := range results {
		if r.err != nil {
			log.Printf("SkillService.AIExtractAllFromNovel: chapter extract error: %v", r.err)
			continue
		}
		for _, sk := range r.skills {
			key := strings.ToLower(sk.Name)
			if !seen[key] {
				seen[key] = true
				allSkills = append(allSkills, sk)
			}
		}
	}

	// 构建角色名→ID map
	chars, _ := s.characterRepo.ListByNovel(novelID)
	charNameToID := make(map[string]uint, len(chars))
	for _, c := range chars {
		charNameToID[strings.ToLower(c.Name)] = c.ID
	}

	// 统一入库（单线程，无竞争）
	validTypes := map[string]bool{"active": true, "passive": true, "toggle": true, "ultimate": true}
	var created []*model.Skill
	for _, sk := range allSkills {
		skillType := sk.SkillType
		if !validTypes[skillType] {
			skillType = "active"
		}
		level := sk.Level
		if level <= 0 {
			level = 1
		}
		chNo := sk.ChapterNo
		skill := &model.Skill{
			NovelID:           novelID,
			Name:              sk.Name,
			Category:          sk.Category,
			SkillType:         skillType,
			Level:             level,
			Realm:             sk.Realm,
			Description:       sk.Description,
			Effect:            sk.Effect,
			FlavorText:        sk.FlavorText,
			Cost:              sk.Cost,
			AcquiredChapterNo: &chNo,
			Status:            "active",
		}
		if sk.CharacterName != "" {
			if id, ok := charNameToID[strings.ToLower(sk.CharacterName)]; ok {
				skill.CharacterID = &id
			}
		}
		if e := s.skillRepo.Create(skill); e != nil {
			log.Printf("SkillService.AIExtractAllFromNovel: create %q: %v", sk.Name, e)
			continue
		}
		created = append(created, skill)
	}
	return created, nil
}

// AIExtractChapterSkills 从单章内容中提取技能，写入 ink_skill 并关联 acquired_chapter_no
func (s *SkillService) AIExtractChapterSkills(tenantID, novelID, chapterID uint, chapterNo int) ([]*model.Skill, error) {
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}
	content := chapter.Content
	if content == "" {
		content = chapter.Summary
	}
	if content == "" {
		return nil, fmt.Errorf("chapter has no content")
	}

	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	// 已有技能名列表，用于去重
	existing, _ := s.skillRepo.ListByNovel(novelID, repository.ListSkillsOpts{})
	existingNames := make([]string, 0, len(existing))
	existingNameSet := make(map[string]bool, len(existing))
	for _, sk := range existing {
		existingNames = append(existingNames, sk.Name)
		existingNameSet[strings.ToLower(sk.Name)] = true
	}

	tmplStr := loadPromptTemplate("extract_chapter_skills.tmpl")
	tmpl, err := template.New("extract_chapter_skills").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         novelGenre,
		"ExistingNames": existingNames,
		"Content":       content,
	}); err != nil {
		return nil, err
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_chapter_skills", buf.String(), "")
	if err != nil {
		return nil, fmt.Errorf("AI extract chapter skills: %w", err)
	}

	type skillJSON struct {
		Name          string `json:"name"`
		Category      string `json:"category"`
		SkillType     string `json:"skill_type"`
		Level         int    `json:"level"`
		Realm         string `json:"realm"`
		Description   string `json:"description"`
		Effect        string `json:"effect"`
		FlavorText    string `json:"flavor_text"`
		Cost          string `json:"cost"`
		CharacterName string `json:"character_name"`
	}
	var skills []skillJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &skills); err != nil {
		return nil, fmt.Errorf("parse skills JSON: %w", err)
	}

	// 构建角色名→ID map
	chars, _ := s.characterRepo.ListByNovel(novelID)
	charNameToID := make(map[string]uint, len(chars))
	for _, c := range chars {
		charNameToID[strings.ToLower(c.Name)] = c.ID
	}

	validTypes := map[string]bool{"active": true, "passive": true, "toggle": true, "ultimate": true}
	var created []*model.Skill
	for _, sk := range skills {
		if sk.Name == "" || existingNameSet[strings.ToLower(sk.Name)] {
			continue
		}
		skillType := sk.SkillType
		if !validTypes[skillType] {
			skillType = "active"
		}
		level := sk.Level
		if level <= 0 {
			level = 1
		}
		chNo := chapterNo
		skill := &model.Skill{
			NovelID:           novelID,
			Name:              sk.Name,
			Category:          sk.Category,
			SkillType:         skillType,
			Level:             level,
			Realm:             sk.Realm,
			Description:       sk.Description,
			Effect:            sk.Effect,
			FlavorText:        sk.FlavorText,
			Cost:              sk.Cost,
			AcquiredChapterNo: &chNo,
			Status:            "active",
		}
		if sk.CharacterName != "" {
			if id, ok := charNameToID[strings.ToLower(sk.CharacterName)]; ok {
				skill.CharacterID = &id
			}
		}
		if e := s.skillRepo.Create(skill); e != nil {
			log.Printf("SkillService.AIExtractChapterSkills: create %q: %v", sk.Name, e)
			continue
		}
		existingNameSet[strings.ToLower(sk.Name)] = true
		created = append(created, skill)
	}
	return created, nil
}
