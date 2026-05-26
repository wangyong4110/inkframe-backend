package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)


// ─── AI upsert helpers ───────────────────────────────────────────────────────

// fillIfEmpty returns (ai, true) when existing is blank and ai is non-blank;
// otherwise returns (existing, false). Used to avoid overwriting user-curated data.
func fillIfEmpty(existing, ai string) (string, bool) {
	if existing == "" && ai != "" {
		return ai, true
	}
	return existing, false
}

// collectContent joins chapter content up to maxChapters chapters and maxRunes runes total.
func collectContent(chapters []*model.Chapter, maxChapters, maxRunes int) string {
	var sb strings.Builder
	runeCount := 0
	for i, ch := range chapters {
		if i >= maxChapters || runeCount >= maxRunes {
			break
		}
		if ch.Content == "" {
			continue
		}
		runes := []rune(ch.Content)
		if runeCount > 0 {
			sb.WriteString("\n\n")
			runeCount += 2
		}
		remaining := maxRunes - runeCount
		if len(runes) > remaining {
			runes = runes[:remaining]
		}
		sb.WriteString(string(runes))
		runeCount += len(runes)
	}
	return sb.String()
}

// marshalExistingNames serialises a slice of items via transform and returns a compact JSON array string.
// Returns "" when items is empty.
func marshalExistingNames[T any](items []T, transform func(T) any) string {
	if len(items) == 0 {
		return ""
	}
	arr := make([]any, len(items))
	for i, it := range items {
		arr[i] = transform(it)
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return ""
	}
	return string(b)
}

// charNameEntry 阶段一提取的角色简要信息
type charNameEntry struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Brief string `json:"brief"`
}

// extractCharNamesFromContent 从单章内容中提取角色名单（纯 AI 提取，不操作 DB）
// existingNamesJSON：已知角色的 JSON 数组字符串，传入后 AI 会复用已有名称而非产生别名
func (s *CharacterService) extractCharNamesFromContent(
	tenantID, novelID uint,
	novelTitle, genre, content, existingNamesJSON string,
) ([]charNameEntry, error) {
	prompt, err := renderPrompt("extract_character_names", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"Summaries":     content,
		"ExistingNames": existingNamesJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_character_names: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_character_names", prompt, "")
	if err != nil {
		logger.Printf("[CharacterService] extractCharNamesFromContent: AI call failed: %v", err)
		return nil, err
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var entries []charNameEntry
	if err := json.Unmarshal([]byte(cleaned), &entries); err != nil {
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, e := dec.Token(); e == nil {
			for dec.More() {
				var e charNameEntry
				if dec.Decode(&e) == nil && e.Name != "" {
					entries = append(entries, e)
				}
			}
		}
	}
	valid := entries[:0]
	for _, e := range entries {
		if e.Name != "" {
			valid = append(valid, e)
		}
	}
	return valid, nil
}

// extractCharacterNamesFromChapters Phase 1：逐章并发提取角色名单，合并去重
func (s *CharacterService) extractCharacterNamesFromChapters(
	tenantID, novelID uint,
	novelTitle, genre string,
	chapters []*model.Chapter,
) ([]charNameEntry, error) {
	const maxChapters = 10
	const concurrency = 3

	// 过滤有内容的章节（最多 maxChapters 章）
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
	logger.Printf("[CharacterService] extractCharacterNamesFromChapters: novelID=%d chapters=%d", novelID, len(candidates))

	// 加载 DB 中已有角色名，作为 ExistingNames 上下文传入提取提示词，
	// 让 AI 在各章提取时复用已知名称，减少别名产生。
	var existingNamesJSON string
	if s.characterRepo != nil {
		if existing, err := s.characterRepo.ListByNovel(novelID); err == nil && len(existing) > 0 {
			existingNamesJSON = marshalExistingNames(existing, func(c *model.Character) any {
				return struct {
					Name string `json:"name"`
					Role string `json:"role"`
				}{c.Name, c.Role}
			})
		}
	}

	type chResult struct {
		entries []charNameEntry
		err     error
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
			entries, err := s.extractCharNamesFromContent(tenantID, novelID, novelTitle, genre, content, existingNamesJSON)
			results[idx] = chResult{entries, err}
		}(i, ch)
	}
	wg.Wait()

	// 合并去重（按小写名字，保留第一次出现）
	seen := make(map[string]bool)
	var merged []charNameEntry
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for _, e := range r.entries {
			key := strings.ToLower(e.Name)
			if !seen[key] {
				seen[key] = true
				merged = append(merged, e)
			}
		}
	}

	// 合并后若仍有多条记录，用 AI 做一次别名整合（消除跨章产生的同一角色不同名）
	if len(merged) > 1 {
		if consolidated, err := s.consolidateCharacterNames(tenantID, novelID, novelTitle, merged); err == nil && len(consolidated) > 0 {
			logger.Printf("[CharacterService] consolidateCharacterNames: %d → %d entries", len(merged), len(consolidated))
			merged = consolidated
		} else if err != nil {
			logger.Printf("[CharacterService] consolidateCharacterNames: warn: %v (keeping original list)", err)
		}
	}
	return merged, nil
}

// consolidateCharacterNames 用 AI 合并别名，消除跨章节提取产生的同一角色多名问题
func (s *CharacterService) consolidateCharacterNames(
	tenantID, novelID uint,
	novelTitle string,
	entries []charNameEntry,
) ([]charNameEntry, error) {
	namesJSON, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("marshal entries: %w", err)
	}
	prompt, err := renderPrompt("consolidate_character_names", map[string]interface{}{
		"NovelTitle": novelTitle,
		"Names":      string(namesJSON),
	})
	if err != nil {
		return nil, fmt.Errorf("render consolidate_character_names: %w", err)
	}
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "consolidate_character_names", prompt, "")
	if err != nil {
		return nil, fmt.Errorf("AI call: %w", err)
	}
	cleaned := extractJSON(strings.TrimSpace(result))
	var consolidated []charNameEntry
	if err := json.Unmarshal([]byte(cleaned), &consolidated); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	valid := consolidated[:0]
	for _, e := range consolidated {
		if e.Name != "" {
			valid = append(valid, e)
		}
	}
	return valid, nil
}

// extractCharacterNameList 阶段一：从小说摘要中提取角色名单（输出极短，避免截断）
func (s *CharacterService) extractCharacterNameList(
	tenantID, novelID uint,
	novelTitle, genre, summariesText string,
	existing []*model.Character,
) ([]charNameEntry, error) {
	existingJSON := marshalExistingNames(existing, func(c *model.Character) any {
		return struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}{c.Name, c.Role}
	})

	prompt, err := renderPrompt("extract_character_names", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"Summaries":     summariesText,
		"ExistingNames": existingJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_character_names: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_character_names", prompt, "")
	if err != nil {
		return nil, fmt.Errorf("AI call failed: %w", err)
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var entries []charNameEntry
	if err := json.Unmarshal([]byte(cleaned), &entries); err != nil {
		// 兜底：尝试用 Decoder 部分恢复
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, e := dec.Token(); e == nil {
			for dec.More() {
				var e charNameEntry
				if dec.Decode(&e) == nil && e.Name != "" {
					entries = append(entries, e)
				}
			}
		}
	}
	// 过滤掉名字为空的
	valid := entries[:0]
	for _, e := range entries {
		if e.Name != "" {
			valid = append(valid, e)
		}
	}
	return valid, nil
}

// generateOneCharacterProfile 阶段二：为单个角色生成完整档案
func (s *CharacterService) generateOneCharacterProfile(
	tenantID, novelID uint,
	novelTitle, genre, promptLanguage string,
	entry charNameEntry,
	shortSummaries string,
) (*analysisCharJSON, error) {
	prompt, err := renderPrompt("generate_character_profile", map[string]interface{}{
		"NovelTitle":     novelTitle,
		"Genre":          genre,
		"CharacterName":  entry.Name,
		"CharacterRole":  entry.Role,
		"CharacterBrief": entry.Brief,
		"Summaries":      shortSummaries,
		"PromptLanguage": promptLanguage,
	})
	if err != nil {
		return nil, fmt.Errorf("render generate_character_profile: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "generate_character_profile", prompt, "",
		StoryboardOverrides{MaxTokens: 8192})
	if err != nil {
		logger.Printf("[CharacterService] generateOneCharacterProfile: AI call failed for %q: %v", entry.Name, err)
		return nil, fmt.Errorf("AI call: %w", err)
	}

	logger.Printf("[CharacterService] generateOneCharacterProfile %q: raw response len=%d tail=%q",
		entry.Name, len(result), result[max(0, len(result)-200):])

	// Use extractJSONObject (not extractJSON) because the expected response is a single
	// JSON object. extractJSON would incorrectly unwrap inner arrays (e.g. personality_tags)
	// instead of returning the full character profile object.
	cleaned := extractJSONObject(strings.TrimSpace(result))
	var profile analysisCharJSON
	if err := json.Unmarshal([]byte(cleaned), &profile); err != nil {
		// 如果是包裹对象 {"character":{...}}，尝试解包
		var wrapper map[string]json.RawMessage
		if json.Unmarshal([]byte(cleaned), &wrapper) == nil {
			for _, v := range wrapper {
				if json.Unmarshal(v, &profile) == nil && profile.Name != "" {
					logger.Printf("[CharacterService] generateOneCharacterProfile %q (unwrapped): VisualPrompt=%q", entry.Name, profile.VisualPrompt)
					return &profile, nil
				}
			}
		}
		return nil, fmt.Errorf("parse profile JSON: %w", err)
	}
	logger.Printf("[CharacterService] generateOneCharacterProfile %q: parsed VisualPrompt=%q", entry.Name, profile.VisualPrompt)
	if profile.Name == "" {
		profile.Name = entry.Name
	}
	if profile.Role == "" {
		profile.Role = entry.Role
	}
	return &profile, nil
}

// parseCharacterJSONResult 从 AI 响应中解析 []analysisCharJSON。
// 兼容以下几种常见输出形式：
//  1. 裸数组:        [{"name":"xxx",...}]
//  2. 被包裹的对象:  {"characters":[...]} / {"data":[...]} 等
//  3. 截断的 JSON:   输出被 token 上限截断，通过 json.Decoder 逐对象恢复
func parseCharacterJSONResult(raw string) ([]analysisCharJSON, error) {
	cleaned := extractJSON(strings.TrimSpace(raw))
	var profiles []analysisCharJSON
	if err := json.Unmarshal([]byte(cleaned), &profiles); err == nil {
		return profiles, nil
	}
	// 如果直接解析失败，尝试从包裹对象中提取数组
	var wrapper map[string]json.RawMessage
	if json.Unmarshal([]byte(cleaned), &wrapper) == nil {
		for _, v := range wrapper {
			if json.Unmarshal(v, &profiles) == nil {
				return profiles, nil
			}
		}
	}
	// 最后尝试部分恢复：用 json.Decoder 逐个解析，遇到截断就停止
	if partial := extractPartialCharacterObjects(raw); len(partial) > 0 {
		logger.Printf("[parseCharacterJSONResult] partial recovery: got %d characters from truncated JSON", len(partial))
		return partial, nil
	}
	return nil, fmt.Errorf("cannot parse as character array: %.200s", raw)
}

// extractPartialCharacterObjects 从截断的 JSON 数组中尽量多地解析完整对象
func extractPartialCharacterObjects(raw string) []analysisCharJSON {
	start := strings.IndexByte(raw, '[')
	if start == -1 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw[start:]))
	if _, err := dec.Token(); err != nil { // consume '['
		return nil
	}
	var results []analysisCharJSON
	for dec.More() {
		var obj analysisCharJSON
		if err := dec.Decode(&obj); err != nil {
			break // truncated — stop here
		}
		if obj.Name != "" {
			results = append(results, obj)
		}
	}
	return results
}
// ============================================
// CharacterService 角色服务
// ============================================

// EffectiveCharacter 有效角色（合并项目级与章节级覆盖）
type EffectiveCharacter struct {
	model.Character
	ChapterOverride     *model.ChapterCharacter `json:"chapter_override,omitempty"`
	EffectiveDescription string                 `json:"effective_description"`
	EffectiveStatus     string                  `json:"effective_status"`
	EffectiveLocation   string                  `json:"effective_location"`
}

type CharacterService struct {
	characterRepo        *repository.CharacterRepository
	chapterCharacterRepo *repository.ChapterCharacterRepository
	aiService            *AIService
	novelRepo            *repository.NovelRepository   // optional, for AIBatchGenerate
	chapterRepo          *repository.ChapterRepository // optional, for AIBatchGenerate
	modelRepo            *repository.AIModelRepository // optional, for voice auto-suggestion
}

// inferGenderFromText 从角色描述文本中推断性别，返回 "male"/"female"/""
func inferGenderFromText(text string) string {
	femaleKws := []string{"女性", "女子", "少女", "姑娘", "女侠", "女郎", "女孩", "小姐", "夫人", "女王", "女帝", "她的", "1girl", "female", "girl", "woman"}
	maleKws := []string{"男性", "男子", "少年", "男孩", "男侠", "公子", "大侠", "他的", "1boy", "male", "man", "boy"}
	fCount, mCount := 0, 0
	lower := strings.ToLower(text)
	for _, kw := range femaleKws {
		fCount += strings.Count(lower, strings.ToLower(kw))
	}
	for _, kw := range maleKws {
		mCount += strings.Count(lower, strings.ToLower(kw))
	}
	if fCount > 0 && fCount >= mCount {
		return "female"
	}
	if mCount > 0 {
		return "male"
	}
	return ""
}

// suggestVoiceForCharacter 根据角色描述/标签/角色类型从可用音色中自动选择合适的音色 ID。
// 若无可用音色，返回空字符串（调用方保持 VoiceID 为空）。
func suggestVoiceForCharacter(description string, personalityTags []string, role string, voices []*model.AIModel) string {
	if len(voices) == 0 {
		return ""
	}

	// 合并描述和标签，用于性别推断
	combined := description + " " + strings.Join(personalityTags, " ")
	gender := inferGenderFromText(combined)

	femaleKws := []string{"female", "女", "girl", "woman", "f_"}
	maleKws := []string{"male", "男", "boy", "man", "m_"}

	var femaleVoices, maleVoices []*model.AIModel
	for _, v := range voices {
		haystack := strings.ToLower(v.Name + " " + v.DisplayName)
		isFemale, isMale := false, false
		for _, kw := range femaleKws {
			if strings.Contains(haystack, kw) {
				isFemale = true
				break
			}
		}
		for _, kw := range maleKws {
			if strings.Contains(haystack, kw) {
				isMale = true
				break
			}
		}
		if isFemale && !isMale {
			femaleVoices = append(femaleVoices, v)
		} else if isMale && !isFemale {
			maleVoices = append(maleVoices, v)
		}
	}

	switch gender {
	case "female":
		if len(femaleVoices) > 0 {
			return femaleVoices[0].Name
		}
	case "male":
		if len(maleVoices) > 0 {
			return maleVoices[0].Name
		}
	}
	// 性别不明或无对应性别音色，返回第一个可用音色
	return voices[0].Name
}

func NewCharacterService(
	characterRepo *repository.CharacterRepository,
	aiService *AIService,
) *CharacterService {
	return &CharacterService{
		characterRepo: characterRepo,
		aiService:     aiService,
	}
}

// GetNovelTitle 返回小说标题，用于 OSS 路径构建；未注入 novelRepo 或查询失败时返回空字符串。
func (s *CharacterService) GetNovelTitle(novelID uint) string {
	if s.novelRepo == nil || novelID == 0 {
		return ""
	}
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		return novel.Title
	}
	return ""
}

// WithChapterCharacterRepo 注入章节角色覆盖仓库（可选）
func (s *CharacterService) WithChapterCharacterRepo(r *repository.ChapterCharacterRepository) *CharacterService {
	s.chapterCharacterRepo = r
	return s
}

func (s *CharacterService) WithNovelRepo(r *repository.NovelRepository) *CharacterService {
	s.novelRepo = r
	return s
}

func (s *CharacterService) WithChapterRepo(r *repository.ChapterRepository) *CharacterService {
	s.chapterRepo = r
	return s
}

func (s *CharacterService) WithModelRepo(r *repository.AIModelRepository) *CharacterService {
	s.modelRepo = r
	return s
}

func (s *CharacterService) CreateCharacter(novelID uint, req *model.CreateCharacterRequest) (*model.Character, error) {
	character := &model.Character{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		Name:        req.Name,
		Role:        req.Role,
		Description: req.Description,
		Status:      "active",
	}
	return character, s.characterRepo.Create(character)
}

func (s *CharacterService) GetCharacter(id uint) (*model.Character, error) {
	return s.characterRepo.GetByID(id)
}

func (s *CharacterService) ListCharacters(novelID uint) ([]*model.Character, error) {
	return s.characterRepo.ListByNovel(novelID)
}

func (s *CharacterService) UpdateCharacter(id uint, req *model.UpdateCharacterRequest) (*model.Character, error) {
	character, err := s.characterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		character.Name = req.Name
	}
	if req.Role != "" {
		character.Role = req.Role
	}
	if req.Description != "" {
		character.Description = req.Description
	}
	if req.VisualPrompt != "" {
		character.VisualPrompt = req.VisualPrompt
	}
	if req.ThreeViewSheet != "" {
		character.ThreeViewSheet = req.ThreeViewSheet
	}
	if req.FaceCloseup != "" {
		character.FaceCloseup = req.FaceCloseup
	}
	if req.Portrait != "" {
		character.Portrait = req.Portrait
	}
	if req.VoiceID != "" {
		character.VoiceID = req.VoiceID
		// When updating voice, also sync style (allow clearing to empty/default)
		character.VoiceStyle = req.VoiceStyle
	} else if req.VoiceStyle != "" {
		character.VoiceStyle = req.VoiceStyle
	}
	if req.VoiceSpeed != nil {
		character.VoiceSpeed = *req.VoiceSpeed
	}
	if req.VoiceLanguage != "" {
		character.VoiceLanguage = req.VoiceLanguage
	}
	if req.VoiceSample != "" {
		character.VoiceSample = req.VoiceSample
	}
	return character, s.characterRepo.Update(character)
}

func (s *CharacterService) DeleteCharacter(id uint) error {
	return s.characterRepo.Delete(id)
}

// ListEffectiveCharacters 获取章节的有效角色列表（章节级覆盖优先）
func (s *CharacterService) ListEffectiveCharacters(novelID, chapterID uint) ([]*EffectiveCharacter, error) {
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	overrideMap := make(map[uint]*model.ChapterCharacter)
	if s.chapterCharacterRepo != nil {
		overrides, _ := s.chapterCharacterRepo.ListByChapter(chapterID)
		for _, o := range overrides {
			overrideMap[o.CharacterID] = o
		}
	}
	result := make([]*EffectiveCharacter, 0, len(chars))
	for _, ch := range chars {
		ec := &EffectiveCharacter{Character: *ch}
		if o, ok := overrideMap[ch.ID]; ok {
			ec.ChapterOverride = o
			// Merge chapter-level appearance/personality overrides into description
			base := ch.Description
			var overrides []string
			if o.Appearance != "" { overrides = append(overrides, "外貌（本章）："+o.Appearance) }
			if o.Personality != "" { overrides = append(overrides, "性格（本章）："+o.Personality) }
			if len(overrides) > 0 {
				ec.EffectiveDescription = base + "\n" + strings.Join(overrides, "\n")
			} else {
				ec.EffectiveDescription = base
			}
			if o.Status != "" {
				ec.EffectiveStatus = o.Status
			} else {
				ec.EffectiveStatus = ch.Status
			}
			ec.EffectiveLocation = o.Location
		} else {
			ec.EffectiveDescription = ch.Description
			ec.EffectiveStatus = ch.Status
		}
		result = append(result, ec)
	}
	return result, nil
}

// UpsertChapterCharacter 创建或更新章节级角色覆盖
func (s *CharacterService) UpsertChapterCharacter(novelID, chapterID, characterID uint, req *model.UpsertChapterCharacterRequest) (*model.ChapterCharacter, error) {
	if s.chapterCharacterRepo == nil {
		return nil, fmt.Errorf("chapter character repository not configured")
	}
	cc := &model.ChapterCharacter{
		CharacterID: characterID,
		ChapterID:   chapterID,
		NovelID:     novelID,
		Appearance:  req.Appearance,
		Personality: req.Personality,
		Status:      req.Status,
		Location:    req.Location,
		Notes:       req.Notes,
	}
	if err := s.chapterCharacterRepo.Upsert(cc); err != nil {
		return nil, err
	}
	saved, err := s.chapterCharacterRepo.GetByChapterAndCharacter(chapterID, characterID)
	if err != nil {
		return cc, nil
	}
	return saved, nil
}

// DeleteChapterCharacter 删除章节级角色覆盖（回退到项目级）
func (s *CharacterService) DeleteChapterCharacter(chapterID, characterID uint) error {
	if s.chapterCharacterRepo == nil {
		return fmt.Errorf("chapter character repository not configured")
	}
	return s.chapterCharacterRepo.Delete(chapterID, characterID)
}

func (s *CharacterService) GenerateProfile(tenantID uint, novelID uint, description string) (*model.Character, error) {
	prompt := fmt.Sprintf("根据以下描述生成小说角色档案：%s\n以JSON格式返回：{\"name\":\"角色名\",\"role\":\"protagonist/antagonist/supporting\",\"description\":\"外貌、性格、背景等综合描述\"}", description)
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "character_profile", prompt, "")
	if err != nil {
		return nil, err
	}

	var profile struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &profile); err != nil {
		return &model.Character{
			UUID:        uuid.New().String(),
			NovelID:     novelID,
			Name:        "AI生成角色",
			Role:        "supporting",
			Description: result,
			Status:      "active",
		}, nil
	}
	return &model.Character{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		Name:        profile.Name,
		Role:        profile.Role,
		Description: profile.Description,
		Status:      "active",
	}, nil
}

// AIBatchGenerate 使用 AI 批量生成/更新小说角色（按 novel_id+name upsert，仅补填空字段）
// AIBatchGenerate 使用 AI 批量生成/更新小说角色（两阶段：先提名单，再并发生成档案）
func (s *CharacterService) AIBatchGenerate(tenantID, novelID uint) ([]*model.Character, error) {
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}

	existing, _ := s.characterRepo.ListByNovel(novelID)
	byName := make(map[string]*model.Character, len(existing))
	for _, c := range existing {
		byName[c.Name] = c
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, fmt.Errorf("failed to load chapters: %w", err)
	}

	// 获取小说标题/类型/语言配置
	novelTitle := "本小说"
	novelGenre := ""
	novelPromptLanguage := "zh"
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
			if novel.PromptLanguage != "" {
				novelPromptLanguage = novel.PromptLanguage
			}
		}
	}

	// ── 阶段一：逐章并发提取角色名单，合并去重 ──────────────────────────────
	nameList, err := s.extractCharacterNamesFromChapters(tenantID, novelID, novelTitle, novelGenre, chapters)
	if err != nil {
		return nil, fmt.Errorf("phase 1 (extract names per chapter): %w", err)
	}
	if len(nameList) == 0 {
		return nil, fmt.Errorf("AI 未识别到任何主要角色，请确认小说内容是否充足")
	}
	logger.Printf("CharacterService.AIBatchGenerate: phase1 got %d characters: %v", len(nameList), func() []string {
		ns := make([]string, len(nameList))
		for i, e := range nameList {
			ns[i] = e.Name
		}
		return ns
	}())

	// ── 阶段二：并发生成每个角色的完整档案（短摘要，最多 3 并发）────────────
	// 阶段二每次只处理一个角色，用较短摘要节省 token
	shortSummaries := buildChapterSummariesText(chapters, 5, 2000)
	if shortSummaries == "" {
		shortSummaries = collectContent(chapters, 5, 2000)
	}

	type profileResult struct {
		profile *analysisCharJSON
		err     error
	}
	results := make([]profileResult, len(nameList))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, entry := range nameList {
		wg.Add(1)
		go func(idx int, e charNameEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := s.generateOneCharacterProfile(tenantID, novelID, novelTitle, novelGenre, novelPromptLanguage, e, shortSummaries)
			results[idx] = profileResult{p, err}
		}(i, entry)
	}
	wg.Wait()
	logger.Printf("[CharacterService] AIBatchGenerate: phase2 done, processing %d profiles", len(nameList))

	// ── 加载可用音色（一次，用于后续自动推荐）────────────────────────────────
	var voiceModels []*model.AIModel
	if s.modelRepo != nil {
		voiceModels, _ = s.modelRepo.GetAvailableByTaskType("voice_gen")
	}

	// ── Upsert ───────────────────────────────────────────────────────────────
	upserted := make([]*model.Character, 0, len(nameList))
	for i, res := range results {
		if res.err != nil {
			logger.Printf("CharacterService.AIBatchGenerate: generate profile for %q: %v", nameList[i].Name, res.err)
			continue
		}
		p := res.profile
		if p == nil || p.Name == "" {
			continue
		}

		role := p.Role
		if role != "protagonist" && role != "antagonist" && role != "supporting" {
			role = "supporting"
		}

		// 优先使用新格式的统一 description，兼容旧格式分离字段
		description := p.Description
		if description == "" {
			var descParts []string
			if p.Appearance != "" { descParts = append(descParts, "外貌："+p.Appearance) }
			if p.Personality != "" { descParts = append(descParts, "性格："+p.Personality) }
			if p.Background != "" { descParts = append(descParts, "背景："+p.Background) }
			if p.CharacterArc != "" { descParts = append(descParts, "弧光："+p.CharacterArc) }
			if len(p.DialogueStyle.Patterns) > 0 {
				descParts = append(descParts, "说话风格："+strings.Join(p.DialogueStyle.Patterns, "；"))
			} else if p.DialogueStyle.VocabularyLevel != "" {
				descParts = append(descParts, "说话风格："+p.DialogueStyle.VocabularyLevel)
			}
			description = strings.Join(descParts, "\n")
		}

		suggestedVoice := suggestVoiceForCharacter(description, p.PersonalityTags, role, voiceModels)

		if ch, ok := byName[p.Name]; ok {
			logger.Printf("[CharacterService] AIBatchGenerate upsert(update) %q: p.VisualPrompt=%q ch.VisualPrompt(existing)=%q", p.Name, p.VisualPrompt, ch.VisualPrompt)
			changed := false
			if v, ok := fillIfEmpty(ch.Role, role); ok { ch.Role = v; changed = true }
			if v, ok := fillIfEmpty(ch.Description, description); ok { ch.Description = v; changed = true }
			if v, ok := fillIfEmpty(ch.VisualPrompt, p.VisualPrompt); ok { ch.VisualPrompt = v; changed = true }
			if v, ok := fillIfEmpty(ch.VoiceID, suggestedVoice); ok { ch.VoiceID = v; changed = true }
			if !changed {
				upserted = append(upserted, ch)
				continue
			}
			if err := s.characterRepo.Update(ch); err != nil {
				logger.Printf("CharacterService.AIBatchGenerate: update %s: %v", ch.Name, err)
				continue
			}
			upserted = append(upserted, ch)
		} else {
			character := &model.Character{
				UUID:         uuid.New().String(),
				NovelID:      novelID,
				Name:         p.Name,
				Role:         role,
				Description:  description,
				VisualPrompt: p.VisualPrompt,
				VoiceID:      suggestedVoice,
				Status:       "active",
			}
			if err := s.characterRepo.Create(character); err != nil {
				logger.Printf("CharacterService.AIBatchGenerate: create %s: %v", p.Name, err)
				continue
			}
			upserted = append(upserted, character)
		}
	}

	if len(upserted) == 0 && len(nameList) > 0 {
		return nil, fmt.Errorf("所有角色档案生成均失败，请检查 AI 提供商配置")
	}
	logger.Printf("[CharacterService] AIBatchGenerate done: novelID=%d upserted=%d", novelID, len(upserted))
	return upserted, nil
}

// AIExtractMinorChars 从单章内容中提取次要角色（role=minor），并写入 ChapterCharacter 关联
func (s *CharacterService) AIExtractMinorChars(tenantID, novelID, chapterID uint) ([]*model.Character, error) {
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

	// 已有角色名列表，用于去重
	existing, _ := s.characterRepo.ListByNovel(novelID)
	existingNames := make([]string, 0, len(existing))
	existingNameSet := make(map[string]bool, len(existing))
	for _, c := range existing {
		existingNames = append(existingNames, c.Name)
		existingNameSet[strings.ToLower(c.Name)] = true
	}

	minorCharsPrompt, err := renderPrompt("extract_minor_characters", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         novelGenre,
		"ExistingNames": existingNames,
		"Content":       content,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_minor_characters: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_minor_characters", minorCharsPrompt, "")
	if err != nil {
		return nil, fmt.Errorf("AI extract minor chars: %w", err)
	}

	var chars []analysisCharJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &chars); err != nil {
		return nil, fmt.Errorf("parse minor chars JSON: %w", err)
	}

	var created []*model.Character
	for _, c := range chars {
		if c.Name == "" || existingNameSet[strings.ToLower(c.Name)] {
			continue
		}
		var minorDescParts []string
		if c.Appearance != "" { minorDescParts = append(minorDescParts, "外貌："+c.Appearance) }
		if c.Personality != "" { minorDescParts = append(minorDescParts, "性格："+c.Personality) }
		if c.Background != "" { minorDescParts = append(minorDescParts, "背景："+c.Background) }
		if c.CharacterArc != "" { minorDescParts = append(minorDescParts, "弧光："+c.CharacterArc) }
		char := &model.Character{
			NovelID:     novelID,
			UUID:        uuid.New().String(),
			Name:        c.Name,
			Role:        "minor",
			Description: strings.Join(minorDescParts, "\n"),
			Status:      "active",
		}
		if e := s.characterRepo.Create(char); e != nil {
			logger.Printf("CharacterService.AIExtractMinorChars: create %q: %v", c.Name, e)
			continue
		}
		existingNameSet[strings.ToLower(c.Name)] = true
		// 关联到章节
		if s.chapterCharacterRepo != nil {
			cc := &model.ChapterCharacter{
				CharacterID: char.ID,
				ChapterID:   chapterID,
				NovelID:     novelID,
			}
			if e := s.chapterCharacterRepo.Upsert(cc); e != nil {
				logger.Printf("CharacterService.AIExtractMinorChars: link chapter %v: %v", chapterID, e)
			}
		}
		created = append(created, char)
	}
	return created, nil
}

// BatchGenerateImages 批量为小说的角色生成面部特写（同时用作头像）和三视图合图。
// 每个角色在同一 goroutine 中顺序执行：先生成面部特写（兼头像），再生成三视图（以面部特写为参考）。
// 已有对应图片的角色跳过该步骤；两张图均已存在则整个角色跳过。
// 并发度由 AIService.imageSem 统一管控（config.yaml ai.image_concurrency）。
func (s *CharacterService) BatchGenerateImages(tenantID, novelID uint, provider string, progressFn func(int)) (succeeded, failed int, err error) {
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return 0, 0, fmt.Errorf("list characters: %w", err)
	}

	imageStyle := ""
	var novelTitle string
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			imageStyle = novel.ImageStyle
			novelTitle = novel.Title
		}
	}

	// 需要至少生成一张图的角色
	var todo []*model.Character
	for _, c := range chars {
		if c.FaceCloseup == "" || c.ThreeViewSheet == "" {
			todo = append(todo, c)
		}
	}
	total := len(todo)

	imgSvc := NewImageGenerationService(s.aiService)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int

	for _, char := range todo {
		char := char
		wg.Add(1)
		go func() {
			defer wg.Done()
			genCtx := context.Background()
			if novelTitle != "" {
				genCtx = WithImageStorageHint(genCtx, ImageStorageHint{NovelTitle: novelTitle})
			}

			updateReq := &model.UpdateCharacterRequest{Name: char.Name}
			charFailed := false

			// 1. 面部特写（兼头像）
			if char.FaceCloseup == "" {
				faceImg, faceErr := imgSvc.GenerateFaceCloseupImage(genCtx, tenantID, char.Name, char.Description, imageStyle, "", char.Portrait, provider)
				if faceErr != nil {
					logger.Printf("[CharacterService] BatchGenerateImages: face closeup char %d (%s) failed: %v", char.ID, char.Name, faceErr)
					charFailed = true
				} else {
					updateReq.FaceCloseup = faceImg.URL
					updateReq.Portrait = faceImg.URL
					char.Portrait = faceImg.URL // use as reference for three-view below
				}
			}

			// 2. 三视图（使用面部特写或已有头像作为参考）
			if char.ThreeViewSheet == "" {
				threeImg, threeErr := imgSvc.GenerateThreeViewSheet(genCtx, tenantID, char.Name, char.Description, imageStyle, "", char.Portrait, provider)
				if threeErr != nil {
					logger.Printf("[CharacterService] BatchGenerateImages: three-view char %d (%s) failed: %v", char.ID, char.Name, threeErr)
					charFailed = true
				} else {
					updateReq.ThreeViewSheet = threeImg.URL
				}
			}

			if updateReq.FaceCloseup != "" || updateReq.ThreeViewSheet != "" {
				if _, saveErr := s.UpdateCharacter(char.ID, updateReq); saveErr != nil {
					logger.Printf("[CharacterService] BatchGenerateImages: save char %d: %v", char.ID, saveErr)
					charFailed = true
				}
			}

			mu.Lock()
			if charFailed {
				failed++
			} else {
				succeeded++
			}
			done++
			cur := done
			mu.Unlock()
			if progressFn != nil && total > 0 {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[CharacterService] BatchGenerateImages: novelID=%d succeeded=%d failed=%d", novelID, succeeded, failed)
	return succeeded, failed, nil
}

func (s *CharacterService) AnalyzeConsistency(id uint, images []string) (interface{}, error) {
	return map[string]interface{}{
		"character_id":      id,
		"consistency_score": 0.9,
		"images_analyzed":   len(images),
	}, nil
}

// ============================================
// ImageGenerationService 图像生成服务
// ============================================

type ImageGenerationService struct {
	aiService *AIService
}

func NewImageGenerationService(aiService *AIService) *ImageGenerationService {
	return &ImageGenerationService{aiService: aiService}
}

type GeneratedCharacterImage struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

func (s *ImageGenerationService) GenerateCharacterImage(req *model.GenerateImageRequest) (*GeneratedCharacterImage, error) {
	options := &ImageGenerationOptions{
		Prompt:   fmt.Sprintf("%s, %s, %s style", req.Subject, req.Description, req.Style),
		Size:     "512x512",
		Steps:    50,
		CFGScale: 7.5,
	}
	image, err := s.aiService.GenerateImage(options.Prompt, options)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: image.URL, Description: req.Description}, nil
}

// GenerateThreeViewImage 生成单个视角的角色三视图
// viewType: "front" | "side" | "back"
// gender: "male" | "female" | "neutral" | ""（空时不注入性别词）
// referenceImage: 肖像参考图 URL（用于 IP-Adapter 保持面部一致性，可为空）
// provider: 指定图像生成提供者（可为空，空时自动选择）
// ctx 可携带 ImageStorageHint 用于 OSS 路径构建，传 context.Background() 亦可
func (s *ImageGenerationService) GenerateThreeViewImage(ctx context.Context, tenantID uint, name, appearance, viewType, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	// "角色设定参考图" 会被模型理解成"多视角设计总表"，导致单图出现多个人物，改用单视角描述词。
	viewDesc := map[string]string{
		"front": "正面站立，面朝镜头，全身",
		"side":  "侧身站立，侧面朝向，全身",
		"back":  "背对镜头站立，全身",
	}
	angleDesc, ok := viewDesc[viewType]
	if !ok {
		return nil, fmt.Errorf("invalid view type: %s", viewType)
	}
	genderDesc := map[string]string{
		"male":    "男性",
		"female":  "女性",
		"neutral": "中性",
	}
	genderStr := genderDesc[gender] // empty string if gender not set
	// 将前端 image_style ID 映射为 AI 可理解的中文风格描述
	styleDesc := map[string]string{
		"anime":        "日系动漫插画",
		"realistic":    "写实摄影",
		"ink_painting": "水墨中国风插画",
		"cyberpunk":    "赛博朋克风格插画",
		"xianxia_style": "古典仙侠国风插画",
		"oil_painting": "油画风格插画",
		"watercolor":   "水彩插画",
	}
	styleStr := styleDesc[style]
	if styleStr == "" {
		if style != "" {
			styleStr = style // 未知风格直接透传
		} else {
			styleStr = "日系动漫插画"
		}
	}

	// 性别 token 放在提示词最前面以获得最高权重。
	// 英文 booru 标签（1boy/1girl）对插画模型约束力最强；中文作为辅助。
	genderTag := map[string]string{"male": "1boy", "female": "1girl"}[gender]
	genderLeader := genderTag // prefix: "1boy" / "1girl" / ""
	if genderStr != "" && genderLeader == "" {
		genderLeader = genderStr // neutral: 用中文作前缀
	}

	var prompt string
	if style == "realistic" {
		// 写实：English terms 更有效
		realisticGender := map[string]string{"male": "1man, male, ", "female": "1woman, female, ", "neutral": ""}[gender]
		prompt = fmt.Sprintf("%ssolo, 单人, 只有一个人物, %s, %s, %s, realistic photography, pure white background, detailed lighting, high quality portrait",
			realisticGender, name, appearance, angleDesc)
	} else {
		if genderLeader != "" {
			prompt = fmt.Sprintf("%s, solo, 单人, 只有一个人物，%s，%s，%s，%s风格，白色背景，线条清晰，高品质",
				genderLeader, name, appearance, angleDesc, styleStr)
		} else {
			prompt = fmt.Sprintf("solo, 单人, 只有一个人物，%s，%s，%s，%s风格，白色背景，线条清晰，高品质",
				name, appearance, angleDesc, styleStr)
		}
	}
	// Only pass an absolute HTTP(S) URL to the AI — local/relative paths cannot be fetched by remote APIs.
	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}
	if aiRef != "" {
		logger.Printf("GenerateThreeViewImage: %s/%s using reference image %s", name, viewType, aiRef)
	} else {
		logger.Printf("GenerateThreeViewImage: %s/%s no valid reference image", name, viewType)
	}
	// 负向提示词：始终禁止多人，再叠加性别排斥词
	baseNeg := "multiple people, two people, duo, couple, group, 多人, 两人, 三人, 合照, nsfw, lowres, bad anatomy"
	genderNeg := map[string]string{
		"male":   "female, girl, woman, 女性, 女生, 裙子, 长裙, 女装, feminine, she, her",
		"female": "male, man, boy, 男性, 男生, 胡须, beard, mustache, masculine, he, him",
	}[gender]
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}
	url, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, provider, prompt, aiRef, style, negativePrompt)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: fmt.Sprintf("%s %s", name, viewType)}, nil
}

// GenerateThreeViewSheet 生成三合一角色参考图（正视/侧视/背视放在同一张图中）。
// 与 GenerateThreeViewImage 的区别：使用 turnaround sheet 提示词，期望 AI 在单张图内展示三个视角。
// ctx 可携带 ImageStorageHint 用于 OSS 路径构建。
func (s *ImageGenerationService) GenerateThreeViewSheet(ctx context.Context, tenantID uint, name, appearance, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	genderDesc := map[string]string{
		"male":    "男性",
		"female":  "女性",
		"neutral": "中性",
	}
	genderStr := genderDesc[gender]
	styleDesc := map[string]string{
		"anime":         "日系动漫插画",
		"realistic":     "写实摄影",
		"ink_painting":  "水墨中国风插画",
		"cyberpunk":     "赛博朋克风格插画",
		"xianxia_style": "古典仙侠国风插画",
		"oil_painting":  "油画风格插画",
		"watercolor":    "水彩插画",
	}
	styleStr := styleDesc[style]
	if styleStr == "" {
		if style != "" {
			styleStr = style
		} else {
			styleStr = "日系动漫插画"
		}
	}

	genderTag := map[string]string{"male": "1boy", "female": "1girl"}[gender]
	genderLeader := genderTag
	if genderStr != "" && genderLeader == "" {
		genderLeader = genderStr
	}

	// 三合一参考图使用 turnaround/character sheet 专用提示词。
	// 三视图术语（character design sheet, turnaround）能引导模型在一张图内排列三个视角。
	var prompt string
	if style == "realistic" {
		realisticGender := map[string]string{"male": "1man, male, ", "female": "1woman, female, ", "neutral": ""}[gender]
		prompt = fmt.Sprintf(
			"%scharacter design reference sheet, full body turnaround, front view and side view and back view of the same character, "+
				"3-angle orthographic views arranged horizontally, %s, %s, "+
				"realistic photography style, pure white background, clean composition, high quality, "+
				"no text, no labels, no annotations, no watermarks, no captions",
			realisticGender, name, appearance)
	} else {
		if genderLeader != "" {
			prompt = fmt.Sprintf(
				"%s, 角色三视图参考图，同一角色的正面视角+侧面视角+背面视角横向排列，角色设计总表，"+
					"%s，%s，%s风格，白色背景，线条清晰，三视图均为全身，高品质插画，无文字标注，无标签，无水印",
				genderLeader, name, appearance, styleStr)
		} else {
			prompt = fmt.Sprintf(
				"角色三视图参考图，同一角色的正面视角+侧面视角+背面视角横向排列，角色设计总表，"+
					"%s，%s，%s风格，白色背景，线条清晰，三视图均为全身，高品质插画，无文字标注，无标签，无水印",
				name, appearance, styleStr)
		}
	}

	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}
	logger.Printf("GenerateThreeViewSheet: %s ref=%v", name, aiRef != "")

	// 负向提示词：禁止不同角色出现，但允许同一角色的多个视角；同时禁止文字标注
	baseNeg := "text, labels, annotations, watermark, signature, caption, speech bubble, character name tag, " +
		"different characters, multiple distinct people, inconsistent appearance, nsfw, lowres, bad anatomy, poorly drawn"
	genderNeg := map[string]string{
		"male":   "female, girl, woman, 女性, 女生, 裙子, 女装, feminine",
		"female": "male, man, boy, 男性, 男生, 胡须, beard, mustache, masculine",
	}[gender]
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}

	url, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, provider, prompt, aiRef, style, negativePrompt)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: name + " three-view sheet"}, nil
}

// GenerateFaceCloseupImage 生成角色面部特写图片。
// ctx 可携带 ImageStorageHint 用于 OSS 路径构建。
func (s *ImageGenerationService) GenerateFaceCloseupImage(ctx context.Context, tenantID uint, name, appearance, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	genderDesc := map[string]string{
		"male":    "男性",
		"female":  "女性",
		"neutral": "中性",
	}
	genderStr := genderDesc[gender]
	styleDesc := map[string]string{
		"anime":         "日系动漫插画",
		"realistic":     "写实摄影",
		"ink_painting":  "水墨中国风插画",
		"cyberpunk":     "赛博朋克风格插画",
		"xianxia_style": "古典仙侠国风插画",
		"oil_painting":  "油画风格插画",
		"watercolor":    "水彩插画",
	}
	styleStr := styleDesc[style]
	if styleStr == "" {
		if style != "" {
			styleStr = style
		} else {
			styleStr = "日系动漫插画"
		}
	}

	genderTag := map[string]string{"male": "1boy", "female": "1girl"}[gender]
	genderLeader := genderTag
	if genderStr != "" && genderLeader == "" {
		genderLeader = genderStr
	}

	var prompt string
	if style == "realistic" {
		realisticGender := map[string]string{"male": "1man, male, ", "female": "1woman, female, ", "neutral": ""}[gender]
		prompt = fmt.Sprintf(
			"%sclose-up portrait, face and upper chest, head shot, solo, %s, %s, "+
				"detailed facial features, expressive eyes, realistic photography, pure white background, high quality portrait photo",
			realisticGender, name, appearance)
	} else {
		if genderLeader != "" {
			prompt = fmt.Sprintf(
				"%s, solo, 面部特写，头部特写，胸像，%s，%s，"+
					"细腻的五官，精致的眼睛，表情生动，%s风格，白色背景，线条清晰，高品质",
				genderLeader, name, appearance, styleStr)
		} else {
			prompt = fmt.Sprintf(
				"solo, 面部特写，头部特写，胸像，%s，%s，"+
					"细腻的五官，精致的眼睛，表情生动，%s风格，白色背景，线条清晰，高品质",
				name, appearance, styleStr)
		}
	}

	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}
	logger.Printf("GenerateFaceCloseupImage: %s ref=%v", name, aiRef != "")

	baseNeg := "multiple people, two people, group, 多人, nsfw, lowres, bad anatomy, full body, feet, legs below waist"
	genderNeg := map[string]string{
		"male":   "female, girl, woman, 女性, 女生, 裙子, 女装, feminine",
		"female": "male, man, boy, 男性, 男生, 胡须, beard, mustache, masculine",
	}[gender]
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}

	url, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, provider, prompt, aiRef, style, negativePrompt)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: name + " face closeup"}, nil
}
