package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/redis/go-redis/v9"
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
		logger.Errorf("[CharacterService] extractCharNamesFromContent: AI call failed: %v", err)
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
			logger.Errorf("[CharacterService] consolidateCharacterNames: warn: %v (keeping original list)", err)
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
		"NovelTitle":       novelTitle,
		"Genre":            genre,
		"CharacterName":    entry.Name,
		"CharacterRole":    entry.Role,
		"CharacterBrief":   entry.Brief,
		"Summaries":        shortSummaries,
		"PromptLanguage":   promptLanguage,
		"GenreVisualHints": genreVisualHints(genre),
	})
	if err != nil {
		return nil, fmt.Errorf("render generate_character_profile: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "generate_character_profile", prompt, "",
		StoryboardOverrides{})
	if err != nil {
		logger.Errorf("[CharacterService] generateOneCharacterProfile: AI call failed for %q: %v", entry.Name, err)
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
	snapshotRepo         *repository.CharacterStateSnapshotRepository // optional, for cascade delete
	lookRepo             *repository.CharacterLookRepository          // optional, for look management
	aiService            *AIService
	novelRepo            *repository.NovelRepository   // optional, for AIBatchGenerate
	chapterRepo          *repository.ChapterRepository // optional, for AIBatchGenerate
	modelRepo            *repository.AIModelRepository // optional, for voice auto-suggestion
	cache                *redis.Client                 // optional: cross-instance extract lock

	// extractLocks 防止同一 novel 并发提取导致角色重复：key = novelID
	extractLocks sync.Map
}

// WithRedis injects a Redis client for cross-instance character extraction deduplication.
func (s *CharacterService) WithRedis(c *redis.Client) *CharacterService {
	s.cache = c
	return s
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

// suggestVoiceForCharacter 根据角色性别/描述/标签从可用音色中自动选择合适的音色 ID。
// gender 为显式性别（male/female/neutral），优先于从文本推断；若无可用音色返回空字符串。
func suggestVoiceForCharacter(description, gender string, personalityTags []string, role string, voices []*model.AIModel) string {
	if len(voices) == 0 {
		return ""
	}

	// 优先使用显式 gender，否则从描述/标签推断
	if gender == "" || gender == "neutral" {
		combined := description + " " + strings.Join(personalityTags, " ")
		gender = inferGenderFromText(combined)
	}

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
	return voices[0].Name
}

// suggestVoiceStyle 根据性别、年龄感、角色定位和性格标签推断最合适的语音风格。
// 返回值为 STYLES 枚举：""(默认)/calm/excited/sad/angry/cheerful/serious
func suggestVoiceStyle(gender, age, role string, personalityTags []string, description string) string {
	combined := strings.ToLower(age + " " + strings.Join(personalityTags, " ") + " " + description)

	// 儿童/幼年 → 欢快
	childKws := []string{"儿童", "幼儿", "孩童", "幼年", "小孩", "baby", "child", "kid", "toddler"}
	for _, kw := range childKws {
		if strings.Contains(combined, kw) {
			return "cheerful"
		}
	}
	// 少年/少女/青少年 → 欢快（活力）
	youthKws := []string{"少年", "少女", "青少年", "teenager", "teen", "young"}
	for _, kw := range youthKws {
		if strings.Contains(combined, kw) {
			return "cheerful"
		}
	}
	// 老年/年迈 → 平静
	elderKws := []string{"老年", "年迈", "苍老", "老人", "老者", "elderly", "elder", "old"}
	for _, kw := range elderKws {
		if strings.Contains(combined, kw) {
			return "calm"
		}
	}

	// 性格关键词优先
	calmKws := []string{"冷静", "沉稳", "淡漠", "冷淡", "冷峻", "内敛", "沉默", "平静", "calm"}
	for _, kw := range calmKws {
		if strings.Contains(combined, kw) {
			return "calm"
		}
	}
	cheerfulKws := []string{"活泼", "开朗", "欢快", "乐观", "开心", "欢乐", "sunny", "cheerful"}
	for _, kw := range cheerfulKws {
		if strings.Contains(combined, kw) {
			return "cheerful"
		}
	}
	sadKws := []string{"忧郁", "悲伤", "哀愁", "忧愁", "悲苦", "sad", "melancholy"}
	for _, kw := range sadKws {
		if strings.Contains(combined, kw) {
			return "sad"
		}
	}
	angryKws := []string{"暴躁", "愤怒", "易怒", "火爆", "激进", "angry"}
	for _, kw := range angryKws {
		if strings.Contains(combined, kw) {
			return "angry"
		}
	}

	// 反派默认严肃
	if role == "antagonist" {
		return "serious"
	}
	return ""
}

// suggestVoiceLanguage 根据小说 PromptLanguage 推断配音语言编码（存入 voice_language 字段）。
func suggestVoiceLanguage(promptLanguage string) string {
	switch promptLanguage {
	case "en":
		return "en"
	case "ja":
		return "ja"
	case "ko":
		return "ko"
	default:
		return "zh-cmn" // 中文普通话
	}
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

// GetNovelImageStyle 返回小说的画面风格（image_style），用于图像生成风格一致性。
func (s *CharacterService) GetNovelImageStyle(novelID uint) string {
	if s.novelRepo == nil || novelID == 0 {
		return ""
	}
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		return novel.ImageStyle
	}
	return ""
}

// InjectDefaultLooks 批量查询一组角色的默认形象，将三视图 URL 注入 DefaultThreeView 字段。
func (s *CharacterService) InjectDefaultLooks(characters []*model.Character) {
	if s.lookRepo == nil || len(characters) == 0 {
		return
	}
	// Collect distinct DefaultLookIDs
	lookIDs := make([]uint, 0, len(characters))
	charByLookID := make(map[uint]*model.Character, len(characters))
	for _, c := range characters {
		if c.DefaultLookID != 0 {
			lookIDs = append(lookIDs, c.DefaultLookID)
			charByLookID[c.DefaultLookID] = c
		}
	}
	if len(lookIDs) == 0 {
		return
	}
	lookMap, err := s.lookRepo.BatchGetLooksByIDs(lookIDs)
	if err != nil || lookMap == nil {
		return
	}
	for lookID, look := range lookMap {
		if c, ok := charByLookID[lookID]; ok && look.ThreeViewSheet != "" {
			c.DefaultThreeView = look.ThreeViewSheet
		}
	}
}

// WithChapterCharacterRepo 注入章节角色覆盖仓库（可选）
func (s *CharacterService) WithLookRepo(r *repository.CharacterLookRepository) *CharacterService {
	s.lookRepo = r
	return s
}

func (s *CharacterService) WithChapterCharacterRepo(r *repository.ChapterCharacterRepository) *CharacterService {
	s.chapterCharacterRepo = r
	return s
}

// WithSnapshotRepo 注入角色状态快照仓库（可选），用于 DeleteCharacter 级联清理
func (s *CharacterService) WithSnapshotRepo(r *repository.CharacterStateSnapshotRepository) *CharacterService {
	s.snapshotRepo = r
	return s
}

func (s *CharacterService) WithNovelRepo(r *repository.NovelRepository) *CharacterService {
	s.novelRepo = r
	return s
}

// characterBelongsToTenant 通过小说验证角色归属（角色 → 小说 → 租户，而非直接比较 character.TenantID）。
// novelRepo 未注入时降级为允许（内部调用不做跨租户隔离）。
func (s *CharacterService) characterBelongsToTenant(char *model.Character, tenantID uint) bool {
	if s.novelRepo == nil {
		return true
	}
	novel, err := s.novelRepo.GetByID(char.NovelID)
	if err != nil {
		return false
	}
	return novel.TenantID == 0 || novel.TenantID == tenantID
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

// ListByNovelFiltered 列出角色，可按 role 过滤（空字符串 = 不过滤）；传播 ctx 用于超时/取消
func (s *CharacterService) ListByNovelFiltered(ctx context.Context, novelID uint, role string) ([]*model.Character, error) {
	return s.characterRepo.ListByNovelFilteredCtx(ctx, novelID, role)
}

func (s *CharacterService) UpdateCharacter(id, tenantID uint, req *model.UpdateCharacterRequest) (*model.Character, error) {
	character, err := s.characterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("not found")
	}
	if !s.characterBelongsToTenant(character, tenantID) {
		return nil, fmt.Errorf("not found")
	}
	if req.Name != "" {
		character.Name = req.Name
	}
	if req.Role != "" {
		character.Role = req.Role
	}
	if req.Gender != "" {
		character.Gender = req.Gender
	}
	if req.Age != "" {
		character.Age = req.Age
	}
	if req.Description != "" {
		character.Description = req.Description
	}
	// 内在动机字段：空字符串也允许覆盖（支持清空）
	if req.InnerConflict != "" {
		character.InnerConflict = req.InnerConflict
	}
	if req.CoreDesire != "" {
		character.CoreDesire = req.CoreDesire
	}
	if req.ArcDesign != "" {
		character.ArcDesign = req.ArcDesign
	}
	if req.CurrentArcStage != "" {
		character.CurrentArcStage = req.CurrentArcStage
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
	if err := s.characterRepo.Update(character); err != nil {
		return nil, err
	}
	// Auto-snapshot when key characterization fields change (best-effort).
	if s.snapshotRepo != nil && (req.Description != "" || req.InnerConflict != "" || req.CoreDesire != "") {
		snap := &model.CharacterStateSnapshot{
			CharacterID:  character.ID,
			Motivation:   character.CoreDesire,
			SnapshotTime: time.Now(),
		}
		_ = s.snapshotRepo.Create(snap) // ignore error
	}
	return character, nil
}

// ListCharacterSnapshots 列出角色状态快照
func (s *CharacterService) ListCharacterSnapshots(characterID uint) ([]*model.CharacterStateSnapshot, error) {
	if s.snapshotRepo == nil {
		return nil, fmt.Errorf("snapshot repo not configured")
	}
	return s.snapshotRepo.ListByCharacter(characterID)
}

// BatchDeleteCharacters 批量删除角色，仅删除属于指定小说的角色
func (s *CharacterService) BatchDeleteCharacters(ctx context.Context, novelID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return s.characterRepo.BatchDeleteByNovel(novelID, ids)
}

// CreateCharacterSnapshot 手动创建角色状态快照
func (s *CharacterService) CreateCharacterSnapshot(characterID uint, motivation, mood string) (*model.CharacterStateSnapshot, error) {
	if s.snapshotRepo == nil {
		return nil, fmt.Errorf("snapshot repo not configured")
	}
	snap := &model.CharacterStateSnapshot{
		CharacterID:  characterID,
		Motivation:   motivation,
		Mood:         mood,
		SnapshotTime: time.Now(),
	}
	if err := s.snapshotRepo.Create(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *CharacterService) DeleteCharacter(id, tenantID uint) error {
	char, err := s.characterRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("not found")
	}
	if !s.characterBelongsToTenant(char, tenantID) {
		return fmt.Errorf("not found")
	}

	// 级联清理关联数据：删除角色状态快照
	if s.snapshotRepo != nil {
		if err := s.snapshotRepo.DeleteByCharacter(id); err != nil {
			logger.Errorf("[CharacterService] DeleteCharacter: delete snapshots for char %d: %v", id, err)
		}
	}

	// 级联清理章节角色覆盖
	if s.chapterCharacterRepo != nil {
		if err := s.chapterCharacterRepo.DeleteByCharacter(id); err != nil {
			logger.Errorf("[CharacterService] DeleteCharacter: delete chapter overrides for char %d: %v", id, err)
		}
	}

	return s.characterRepo.Delete(id)
}

// ListEffectiveCharacters 获取章节绑定的角色列表（仅返回已绑定到本章节的角色，章节级覆盖优先）
func (s *CharacterService) ListEffectiveCharacters(novelID, chapterID uint) ([]*EffectiveCharacter, error) {
	overrideMap := make(map[uint]*model.ChapterCharacter)
	boundIDs := make(map[uint]bool)
	if s.chapterCharacterRepo != nil {
		overrides, _ := s.chapterCharacterRepo.ListByChapter(chapterID)
		for _, o := range overrides {
			overrideMap[o.CharacterID] = o
			boundIDs[o.CharacterID] = true
		}
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	// No chapter-specific bindings: show all novel characters (protagonist/supporting appear on every chapter by default).
	// When bindings exist, only show explicitly bound characters (supports chapter-specific overrides).
	hasBindings := len(boundIDs) > 0
	result := make([]*EffectiveCharacter, 0)
	for _, ch := range chars {
		if hasBindings && !boundIDs[ch.ID] {
			continue
		}
		ec := &EffectiveCharacter{Character: *ch}
		if o, ok := overrideMap[ch.ID]; ok {
			ec.ChapterOverride = o
			base := ch.Description
			var parts []string
			if o.Appearance != "" { parts = append(parts, "外貌（本章）："+o.Appearance) }
			if o.Personality != "" { parts = append(parts, "性格（本章）："+o.Personality) }
			if len(parts) > 0 {
				ec.EffectiveDescription = base + "\n" + strings.Join(parts, "\n")
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
	prompt := fmt.Sprintf(`根据以下描述生成小说角色档案：%s

请以单个JSON对象格式返回，包含以下字段：
{
  "name": "角色名",
  "role": "protagonist/antagonist/supporting",
  "description": "连贯段落，涵盖外貌概述、性格特点、背景故事、说话风格，200字以内",
  "visual_prompt": "专业角色造型描述，最少150字，按顺序覆盖：[1]性别体型 [2]面部骨骼 [3]眼睛 [4]眉毛眼妆 [5]鼻唇 [6]肤色肌感 [7]发型 [8]服装逐层 [9]鞋履 [10]配饰道具 [11]体态 [12]色彩叙事（主色/辅色/点缀色+象征含义）[13]整体廓形（1-3词）[14]标志性造型元素（具体描述）[15]造型逻辑（2句话说明造型与角色内心的关联）。禁止出现画风、质量标签或渲染词汇。"
}`, description)
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "character_profile", prompt, "",
		StoryboardOverrides{})
	if err != nil {
		return nil, err
	}

	var profile struct {
		Name         string `json:"name"`
		Role         string `json:"role"`
		Description  string `json:"description"`
		VisualPrompt string `json:"visual_prompt"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(strings.TrimSpace(result))), &profile); err != nil {
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
		voiceModels, _ = s.modelRepo.GetAvailableByTaskType("voice_gen", tenantID)
	}

	// ── Upsert ───────────────────────────────────────────────────────────────
	upserted := make([]*model.Character, 0, len(nameList))
	for i, res := range results {
		if res.err != nil {
			logger.Errorf("CharacterService.AIBatchGenerate: generate profile for %q: %v", nameList[i].Name, res.err)
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

		suggestedVoice := suggestVoiceForCharacter(description, p.Gender, p.PersonalityTags, role, voiceModels)
		suggestedStyle := suggestVoiceStyle(p.Gender, p.Age, role, p.PersonalityTags, description)
		suggestedLang := suggestVoiceLanguage(novelPromptLanguage)

		if ch, ok := byName[p.Name]; ok {
			logger.Printf("[CharacterService] AIBatchGenerate upsert(update) %q", p.Name)
			// AI 生成字段直接覆盖（用户点击"AI 更新角色"语义就是刷新）
			if description != "" { ch.Description = description }
			if p.Gender != "" { ch.Gender = p.Gender }
			if p.Age != "" { ch.Age = p.Age }
			// 用户手动配置字段仅在空时填充
			if v, ok := fillIfEmpty(ch.Role, role); ok { ch.Role = v }
			if v, ok := fillIfEmpty(ch.VoiceID, suggestedVoice); ok { ch.VoiceID = v }
			if v, ok := fillIfEmpty(ch.VoiceStyle, suggestedStyle); ok { ch.VoiceStyle = v }
			if v, ok := fillIfEmpty(ch.VoiceLanguage, suggestedLang); ok { ch.VoiceLanguage = v }
			if err := s.characterRepo.Update(ch); err != nil {
				logger.Errorf("CharacterService.AIBatchGenerate: update %s: %v", ch.Name, err)
				continue
			}
			// 同步默认形象的 VisualPrompt
			if p.VisualPrompt != "" {
				s.upsertDefaultLookVisualPrompt(ch.ID, ch.NovelID, p.VisualPrompt)
			}
			upserted = append(upserted, ch)
		} else {
			character := &model.Character{
				UUID:    uuid.New().String(),
				NovelID: novelID,
				Name:    p.Name,
				Role:          role,
				Gender:        p.Gender,
				Age:           p.Age,
				Description:   description,
				VoiceID:       suggestedVoice,
				VoiceStyle:    suggestedStyle,
				VoiceLanguage: suggestedLang,
				Status:        "active",
			}
			if err := s.characterRepo.Create(character); err != nil {
				logger.Errorf("CharacterService.AIBatchGenerate: create %s: %v", p.Name, err)
				continue
			}
			// 为新角色创建携带 VisualPrompt 的默认形象
			if p.VisualPrompt != "" {
				s.upsertDefaultLookVisualPrompt(character.ID, character.NovelID, p.VisualPrompt)
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

// ReanalyzeCharacter 重新分析并生成单个角色的信息（description / visual_prompt）。
// 基于小说全部章节摘要，调用与 AIBatchGenerate 相同的 generateOneCharacterProfile 逻辑。
func (s *CharacterService) ReanalyzeCharacter(tenantID, characterID uint) (*model.Character, error) {
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil {
		return nil, fmt.Errorf("character not found: %w", err)
	}
	if !s.characterBelongsToTenant(char, tenantID) {
		return nil, fmt.Errorf("character not found")
	}

	novelTitle := "本小说"
	novelGenre := ""
	novelPromptLanguage := "zh"
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(char.NovelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
			if novel.PromptLanguage != "" {
				novelPromptLanguage = novel.PromptLanguage
			}
		}
	}

	var shortSummaries string
	if s.chapterRepo != nil {
		if chapters, e := s.chapterRepo.ListByNovelWithContent(char.NovelID); e == nil {
			shortSummaries = buildChapterSummariesText(chapters, 5, 2000)
			if shortSummaries == "" {
				shortSummaries = collectContent(chapters, 5, 2000)
			}
		}
	}

	role := char.Role
	if role != "protagonist" && role != "antagonist" && role != "supporting" {
		role = "supporting"
	}
	entry := charNameEntry{
		Name:  char.Name,
		Role:  role,
		Brief: char.Description,
	}

	profile, err := s.generateOneCharacterProfile(tenantID, char.NovelID, novelTitle, novelGenre, novelPromptLanguage, entry, shortSummaries)
	if err != nil {
		return nil, fmt.Errorf("AI reanalyze: %w", err)
	}

	if profile.Description != "" {
		char.Description = profile.Description
	}
	if profile.Gender != "" {
		char.Gender = profile.Gender
	}
	if profile.Age != "" {
		char.Age = profile.Age
	}

	// 根据最新 gender/age/role 重新推荐配音设置（仅在用户未手动配置时填充）
	var voiceModels []*model.AIModel
	if s.modelRepo != nil {
		voiceModels, _ = s.modelRepo.GetAvailableByTaskType("voice_gen", tenantID)
	}
	suggestedVoice := suggestVoiceForCharacter(char.Description, char.Gender, profile.PersonalityTags, char.Role, voiceModels)
	suggestedStyle := suggestVoiceStyle(char.Gender, char.Age, char.Role, profile.PersonalityTags, char.Description)
	suggestedLang := suggestVoiceLanguage(novelPromptLanguage)
	if v, ok := fillIfEmpty(char.VoiceID, suggestedVoice); ok { char.VoiceID = v }
	if v, ok := fillIfEmpty(char.VoiceStyle, suggestedStyle); ok { char.VoiceStyle = v }
	if v, ok := fillIfEmpty(char.VoiceLanguage, suggestedLang); ok { char.VoiceLanguage = v }

	if err := s.characterRepo.Update(char); err != nil {
		return nil, fmt.Errorf("save character: %w", err)
	}
	// 同步默认形象的 VisualPrompt
	if profile.VisualPrompt != "" {
		s.upsertDefaultLookVisualPrompt(char.ID, char.NovelID, profile.VisualPrompt)
	}
	return char, nil
}

// AIExtractMinorChars 从单章内容中提取次要角色（role=minor），并写入 ChapterCharacter 关联。
// 复用与主角色分析相同的 description/visual_prompt/音色推荐逻辑，保证次要角色档案质量一致。
func (s *CharacterService) AIExtractMinorChars(tenantID, novelID, chapterID uint, userPrompt string) ([]*model.Character, error) {
	logger.Printf("[CharacterService] AIExtractMinorChars: tenantID=%d novelID=%d chapterID=%d", tenantID, novelID, chapterID)

	// 序列化同一 novel 的并发提取，防止两个任务同时读到空的 existingNames 而重复创建角色。
	// Redis SETNX 提供跨实例互斥；本地 mutex 作为 fallback（Redis 不可用时）及进程内二次防护。
	redisLockKey := fmt.Sprintf("lock:char:extract:%d", novelID)
	redisLocked := false
	if s.cache != nil {
		ok, err := s.cache.SetNX(context.Background(), redisLockKey, "1", 10*time.Minute).Result()
		if err == nil {
			if !ok {
				return nil, fmt.Errorf("character extraction for novel %d is already in progress on another instance", novelID)
			}
			redisLocked = true
			defer s.cache.Del(context.Background(), redisLockKey)
		}
		// err != nil: Redis unavailable, fall through to local mutex
	}
	// 注意：mutex 永不从 map 中删除。若先 Unlock 再 Delete，第三个 goroutine 可在 Delete
	// 窗口内创建新 mutex 并绕过串行化；保持 mutex 常驻（每 novel 仅占 8B）是最简正确方案。
	if !redisLocked {
		lockVal, _ := s.extractLocks.LoadOrStore(novelID, &sync.Mutex{})
		mu := lockVal.(*sync.Mutex)
		mu.Lock()
		defer mu.Unlock()
	}

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
	logger.Printf("[CharacterService] AIExtractMinorChars: chapterID=%d contentLen=%d", chapterID, len(content))

	novelTitle := "本小说"
	novelGenre := ""
	novelPromptLanguage := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
			novelPromptLanguage = novel.PromptLanguage
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
	logger.Printf("[CharacterService] AIExtractMinorChars: novelID=%d existingChars=%d existingNames=%v", novelID, len(existing), existingNames)

	minorCharsPrompt, err := renderPrompt("extract_minor_characters", map[string]interface{}{
		"NovelTitle":       novelTitle,
		"Genre":            novelGenre,
		"ExistingNames":    existingNames,
		"Content":          content,
		"PromptLanguage":   novelPromptLanguage,
		"UserPrompt":       userPrompt,
		"GenreVisualHints": genreVisualHints(novelGenre),
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_minor_characters: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_minor_characters", minorCharsPrompt, "",
		StoryboardOverrides{})
	if err != nil {
		logger.Errorf("[CharacterService] AIExtractMinorChars: AI call failed: %v", err)
		return nil, fmt.Errorf("AI extract minor chars: %w", err)
	}
	logger.Printf("[CharacterService] AIExtractMinorChars: AI response len=%d raw=%.300s", len(result), result)

	// 解析新格式 {"new_characters": [...], "appearing_characters": [...]}
	// 注意：必须用 extractJSONObject 而非 extractJSON，后者会把内嵌的第一个 [ 提取出来，
	// 导致 appearing_characters 字段丢失（new_characters 为空数组时尤其明显）。
	var aiResp extractMinorCharsResponse
	cleaned := extractJSONObject(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &aiResp); err != nil {
		// 兼容旧格式：直接是数组
		var chars []analysisCharJSON
		if err2 := json.Unmarshal([]byte(cleaned), &chars); err2 != nil {
			logger.Errorf("[CharacterService] AIExtractMinorChars: JSON parse failed: %v, cleaned=%.300s", err, cleaned)
			return nil, fmt.Errorf("parse minor chars JSON: %w", err)
		}
		aiResp.NewCharacters = chars
	}
	logger.Printf("[CharacterService] AIExtractMinorChars: parsed new_characters=%d appearing_characters=%v",
		len(aiResp.NewCharacters), aiResp.AppearingCharacters)

	// 加载可用音色，用于自动推荐（与主角色提取逻辑一致）
	var voiceModels []*model.AIModel
	if s.modelRepo != nil {
		voiceModels, _ = s.modelRepo.GetAvailableByTaskType("voice_gen", tenantID)
	}

	// 构建已有角色名→ID 映射，用于 AI 识别的出场角色绑定
	existingNameToID := make(map[string]uint, len(existing))
	for _, c := range existing {
		existingNameToID[strings.ToLower(c.Name)] = c.ID
	}

	if s.chapterCharacterRepo == nil {
		logger.Errorf("[CharacterService] AIExtractMinorChars: chapterCharacterRepo is nil, chapter bindings will be skipped")
	}

	var created []*model.Character
	for _, c := range aiResp.NewCharacters {
		if c.Name == "" || existingNameSet[strings.ToLower(c.Name)] {
			continue
		}

		// 优先使用 AI 生成的统一 description，兼容旧格式分离字段（与主角色提取逻辑一致）
		finalDesc := c.Description
		if finalDesc == "" {
			var parts []string
			if c.Appearance != "" {
				parts = append(parts, "外貌："+c.Appearance)
			}
			if c.Personality != "" {
				parts = append(parts, "性格："+c.Personality)
			}
			if c.Background != "" {
				parts = append(parts, "背景："+c.Background)
			}
			if c.CharacterArc != "" {
				parts = append(parts, "弧光："+c.CharacterArc)
			}
			if c.DialogueStyle.SpeechHabits != "" {
				parts = append(parts, "说话风格："+c.DialogueStyle.SpeechHabits)
			} else if len(c.DialogueStyle.Patterns) > 0 {
				parts = append(parts, "说话风格："+strings.Join(c.DialogueStyle.Patterns, "；"))
			}
			finalDesc = strings.Join(parts, "\n")
		}

		suggestedVoice := suggestVoiceForCharacter(finalDesc, c.Gender, c.PersonalityTags, "minor", voiceModels)
		suggestedStyle := suggestVoiceStyle(c.Gender, c.Age, "minor", c.PersonalityTags, finalDesc)
		suggestedLang := suggestVoiceLanguage(novelPromptLanguage)

		char := &model.Character{
			NovelID:       novelID,
			UUID:          uuid.New().String(),
			Name:          c.Name,
			Role:          "minor",
			Gender:        c.Gender,
			Age:           c.Age,
			Description:   finalDesc,
			VoiceID:       suggestedVoice,
			VoiceStyle:    suggestedStyle,
			VoiceLanguage: suggestedLang,
			Status:        "active",
		}
		// 插入前再次确认（mutex 内，但 reload 防止极端情况）
		existingNameSet[strings.ToLower(c.Name)] = true // 先占位，防止同批次重复
		// DB 级二次兜底：mutex 内仍有极小窗口让 NovelAnalysisService 同时创建相同角色。
		// 找到已存在记录时，仍需将其绑定到本章节（角色是真实出场的）。
		if dup, _ := s.characterRepo.FindByNovelAndName(novelID, c.Name); dup != nil {
			logger.Printf("[CharacterService] AIExtractMinorChars: DB dedup: %q already exists (id=%d), binding to chapter instead", c.Name, dup.ID)
			if s.chapterCharacterRepo != nil {
				_ = s.chapterCharacterRepo.Upsert(&model.ChapterCharacter{
					CharacterID: dup.ID,
					ChapterID:   chapterID,
					NovelID:     novelID,
				})
			}
			continue
		}
		if e := s.characterRepo.Create(char); e != nil {
			logger.Errorf("[CharacterService] AIExtractMinorChars: create %q: %v", c.Name, e)
			continue
		}
		if s.lookRepo != nil && c.VisualPrompt != "" {
			defaultLook := &model.CharacterLook{
				CharacterID:  char.ID,
				NovelID:      char.NovelID,
				Label:        "默认形象",
				ChapterFrom:  1,
				VisualPrompt: c.VisualPrompt,
			}
			if e := s.lookRepo.Create(defaultLook); e != nil {
				logger.Errorf("[CharacterService] AIExtractMinorChars: create default look for %q: %v", char.Name, e)
			} else {
				_ = s.characterRepo.UpdateDefaultLookID(char.ID, defaultLook.ID)
			}
		}
		logger.Printf("[CharacterService] AIExtractMinorChars: created character %q id=%d", char.Name, char.ID)
		// 关联到章节
		if s.chapterCharacterRepo != nil {
			cc := &model.ChapterCharacter{
				CharacterID: char.ID,
				ChapterID:   chapterID,
				NovelID:     novelID,
			}
			if e := s.chapterCharacterRepo.Upsert(cc); e != nil {
				logger.Errorf("[CharacterService] AIExtractMinorChars: link charID=%d to chapterID=%d: %v", char.ID, chapterID, e)
			} else {
				logger.Printf("[CharacterService] AIExtractMinorChars: bound new char %q (id=%d) to chapterID=%d", char.Name, char.ID, chapterID)
			}
		}
		created = append(created, char)
	}

	// 将 AI 识别的已有出场角色绑定到本章节。
	// 重新从 DB 加载最新角色列表：AI 调用期间（30s+）可能有新角色被其他 goroutine 创建，
	// 仅依赖函数开始时的快照会遗漏这些角色的绑定。
	if freshChars, freshErr := s.characterRepo.ListByNovel(novelID); freshErr == nil {
		existingNameToID = make(map[string]uint, len(freshChars))
		for _, fc := range freshChars {
			existingNameToID[strings.ToLower(fc.Name)] = fc.ID
		}
	}
	if s.chapterCharacterRepo != nil {
		for _, name := range aiResp.AppearingCharacters {
			charID, ok := existingNameToID[strings.ToLower(name)]
			if !ok {
				logger.Printf("[CharacterService] AIExtractMinorChars: appearing char %q not found in existing list, skipping", name)
				continue
			}
			cc := &model.ChapterCharacter{
				CharacterID: charID,
				ChapterID:   chapterID,
				NovelID:     novelID,
			}
			if e := s.chapterCharacterRepo.Upsert(cc); e != nil {
				logger.Errorf("[CharacterService] AIExtractMinorChars: bind appearing charID=%d %q to chapterID=%d: %v", charID, name, chapterID, e)
			} else {
				logger.Printf("[CharacterService] AIExtractMinorChars: bound existing char %q (id=%d) to chapterID=%d", name, charID, chapterID)
			}
		}
	}

	logger.Printf("[CharacterService] AIExtractMinorChars done: chapterID=%d newCreated=%d appearing=%d",
		chapterID, len(created), len(aiResp.AppearingCharacters))
	return created, nil
}

// BatchGenerateImages 批量为小说的角色生成面部特写（同时用作头像）和三视图合图。
// 每个角色在同一 goroutine 中顺序执行：先生成面部特写（兼头像），再生成三视图（以面部特写为参考）。
// force=false：跳过已有对应图片的角色；force=true：全量重新生成（风格变更时使用）。
// 并发度由 AIService.imageSem 统一管控（系统设置 image_concurrency）。
func (s *CharacterService) BatchGenerateImages(tenantID, novelID uint, provider string, force bool, progressFn func(int)) (succeeded, failed int, err error) {
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

	// 批量预取默认形象，用于判断哪些角色需要生成图片
	lookIDs := make([]uint, 0, len(chars))
	charByLookID := make(map[uint]*model.Character, len(chars))
	for _, c := range chars {
		if c.DefaultLookID != 0 {
			lookIDs = append(lookIDs, c.DefaultLookID)
			charByLookID[c.DefaultLookID] = c
		}
	}
	var defaultLookMap map[uint]*model.CharacterLook // charID → look
	if s.lookRepo != nil && len(lookIDs) > 0 {
		byLookID, _ := s.lookRepo.BatchGetLooksByIDs(lookIDs)
		defaultLookMap = make(map[uint]*model.CharacterLook, len(byLookID))
		for lid, look := range byLookID {
			if c, ok := charByLookID[lid]; ok {
				defaultLookMap[c.ID] = look
			}
		}
	}
	if defaultLookMap == nil {
		defaultLookMap = map[uint]*model.CharacterLook{}
	}

	// force=true 全量重新生成；否则仅处理缺图的角色
	var todo []*model.Character
	for _, c := range chars {
		look := defaultLookMap[c.ID]
		if force || look == nil || look.FaceCloseup == "" || look.ThreeViewSheet == "" {
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

			look := defaultLookMap[char.ID]
			charFailed := false

			// 优先使用默认形象的 visual_prompt，降级使用 description
			charAppearance := ""
			if look != nil && look.VisualPrompt != "" {
				charAppearance = look.VisualPrompt
			}
			if charAppearance == "" {
				charAppearance = char.Description
			}
			gender := InferGenderTag(charAppearance, char.Description)

			var newFaceURL, newThreeURL string

			// 1. 面部特写
			faceRef := char.Portrait
			if look != nil && look.Portrait != "" {
				faceRef = look.Portrait
			}
			if force || look == nil || look.FaceCloseup == "" {
				faceImg, faceErr := imgSvc.GenerateFaceCloseupImage(genCtx, tenantID, char.Name, charAppearance, imageStyle, gender, faceRef, provider)
				if faceErr != nil {
					logger.Errorf("[CharacterService] BatchGenerateImages: face closeup char %d (%s) failed: %v", char.ID, char.Name, faceErr)
					charFailed = true
				} else {
					newFaceURL = faceImg.URL
					faceRef = faceImg.URL
				}
			}
			if faceRef == "" && look != nil {
				faceRef = look.FaceCloseup
			}

			// 2. 三视图（使用面部特写作为参考以锁定面部一致性）
			if force || look == nil || look.ThreeViewSheet == "" {
				threeImg, threeErr := imgSvc.GenerateThreeViewSheet(genCtx, tenantID, char.Name, charAppearance, imageStyle, gender, faceRef, provider)
				if threeErr != nil {
					logger.Errorf("[CharacterService] BatchGenerateImages: three-view char %d (%s) failed: %v", char.ID, char.Name, threeErr)
					charFailed = true
				} else {
					newThreeURL = threeImg.URL
				}
			}

			if newFaceURL != "" || newThreeURL != "" {
				if s.lookRepo != nil {
					if look != nil {
						lookUpdateReq := &model.UpdateCharacterLookRequest{}
						if newFaceURL != "" {
							lookUpdateReq.FaceCloseup = &newFaceURL
							lookUpdateReq.Portrait = &newFaceURL
						}
						if newThreeURL != "" {
							lookUpdateReq.ThreeViewSheet = &newThreeURL
						}
						if _, saveErr := s.UpdateLook(look.ID, lookUpdateReq); saveErr != nil {
							logger.Errorf("[CharacterService] BatchGenerateImages: save look for char %d: %v", char.ID, saveErr)
							charFailed = true
						}
					} else {
						// 角色尚无默认形象，自动创建
						newLook := &model.CharacterLook{
							CharacterID:    char.ID,
							NovelID:        char.NovelID,
							Label:          "默认形象",
							ChapterFrom:    1,
							VisualPrompt:   charAppearance,
							FaceCloseup:    newFaceURL,
							Portrait:       newFaceURL,
							ThreeViewSheet: newThreeURL,
						}
						if createErr := s.lookRepo.Create(newLook); createErr != nil {
							logger.Errorf("[CharacterService] BatchGenerateImages: create default look for char %d: %v", char.ID, createErr)
							charFailed = true
						} else {
							_ = s.characterRepo.UpdateDefaultLookID(char.ID, newLook.ID)
						}
					}
				}
				// 同步更新 Character.Portrait 用于列表头像显示
				if newFaceURL != "" {
					portraitReq := &model.UpdateCharacterRequest{Name: char.Name, Portrait: newFaceURL}
					if _, saveErr := s.UpdateCharacter(char.ID, tenantID, portraitReq); saveErr != nil {
						logger.Errorf("[CharacterService] BatchGenerateImages: save portrait for char %d: %v", char.ID, saveErr)
					}
				}
			}

			mu.Lock()
			if charFailed {
				failed++
				metrics.CharacterImageBatchTotal.WithLabelValues("failed").Inc()
			} else {
				succeeded++
				metrics.CharacterImageBatchTotal.WithLabelValues("succeeded").Inc()
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
	if len(images) == 0 {
		return map[string]interface{}{
			"character_id":      id,
			"consistency_score": 0.0,
			"images_analyzed":   0,
			"message":           "no images provided",
		}, nil
	}
	if s.aiService == nil || len(images) == 1 {
		return map[string]interface{}{
			"character_id":      id,
			"consistency_score": 1.0,
			"images_analyzed":   len(images),
			"message":           "single image, consistency assumed",
		}, nil
	}

	char, err := s.characterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("character not found: %w", err)
	}

	prompt := fmt.Sprintf(`You are a visual consistency analyst. Compare the following %d images of the character "%s" and assess their visual consistency.

Rate consistency from 0.0 (completely inconsistent) to 1.0 (perfectly consistent), focusing on:
- Facial features (face shape, eyes, nose, mouth)
- Hair color and style
- Overall art style and proportions

Respond with ONLY a JSON object in this exact format:
{"score": 0.85, "notes": "brief explanation"}`, len(images), char.Name)

	response, err := s.aiService.GenerateWithVision(prompt, images)
	if err != nil {
		logger.Errorf("[CharacterService] AnalyzeConsistency: vision call failed for char %d: %v", id, err)
		return map[string]interface{}{
			"character_id":      id,
			"consistency_score": 0.0,
			"images_analyzed":   len(images),
			"error":             "vision analysis unavailable",
		}, nil
	}

	// Parse the score from the JSON response
	score := 0.0
	notes := ""
	start := strings.Index(response, "{")
	end := strings.LastIndex(response, "}")
	if start >= 0 && end > start {
		var parsed struct {
			Score float64 `json:"score"`
			Notes string  `json:"notes"`
		}
		if jsonErr := json.Unmarshal([]byte(response[start:end+1]), &parsed); jsonErr == nil {
			score = parsed.Score
			notes = parsed.Notes
		}
	}

	return map[string]interface{}{
		"character_id":      id,
		"consistency_score": score,
		"images_analyzed":   len(images),
		"notes":             notes,
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
		Size:     "1024x1024",
		Steps:    50,
		CFGScale: 7.5,
	}
	image, err := s.aiService.GenerateImage(options.Prompt, options)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: image.URL, Description: req.Description}, nil
}

// InferGenderTag extracts gender ("male" / "female" / "") from a character's VisualPrompt or
// Description. VisualPrompt is AI-generated and typically starts with booru tokens (1girl / 1boy),
// so those are checked first. Keyword fallback handles plain-text descriptions.
// The result is passed to image generation functions to lock gender across all generated images.
func InferGenderTag(visualPrompt, description string) string {
	combined := strings.ToLower(visualPrompt + " " + description)
	// Booru tokens — highest confidence; AI-generated VisualPrompt almost always starts with these.
	if strings.Contains(combined, "1girl") || strings.Contains(combined, "1woman") {
		return "female"
	}
	if strings.Contains(combined, "1boy") || strings.Contains(combined, "1man") {
		return "male"
	}
	// Ordered keyword fallback — check female before male to avoid "woman"⊃"man" substring collisions.
	for _, w := range []string{"female", "woman", "girl", "lady", "princess", "queen", "她", "女性", "女子", "女孩", "少女", "姑娘", "女士"} {
		if strings.Contains(combined, w) {
			return "female"
		}
	}
	for _, w := range []string{"male", "man", "boy", "lord", "prince", "king", "他", "男性", "男子", "男孩", "少年", "男生"} {
		if strings.Contains(combined, w) {
			return "male"
		}
	}
	return ""
}

// resolveStyleDesc maps image_style ID to an AI-prompt-friendly style description.
// Falls back to the raw style string, or "日系动漫插画" when style is empty.
func resolveStyleDesc(style string) string {
	m := map[string]string{
		"anime":             "日系动漫插画",
		"realistic":         "写实摄影",
		"real_person":       "真实人像摄影",
		"ink_painting":      "水墨中国风插画",
		"cyberpunk":         "赛博朋克风格插画",
		"xianxia_style":     "古典仙侠国风插画",
		"oil_painting":      "油画风格插画",
		"watercolor":        "水彩插画",
		"pixel_art":         "像素复古风格插画",
		"chinese_animation": "国产动漫插画",
		"game_concept":      "游戏原画概念设计",
		"steampunk":         "蒸汽朋克风格插画",
		"sketch":            "铅笔素描黑白插画",
		"render_3d":         "三维立体渲染风格",
		"ukiyo_e":           "日本浮世绘风格",
		"gothic_dark":       "哥特暗黑风格插画",
	}
	if d, ok := m[style]; ok {
		return d
	}
	if style != "" {
		return style
	}
	return "日系动漫插画"
}

// resolveStyleCategory 将风格 ID 归入大类，用于选择匹配的质量提升词。
// 返回值："realistic" / "anime" / "classic_illustration" / "dark_stylized" / "pixel" / "render_3d" / "" (未知)
func resolveStyleCategory(styleID string) string {
	switch styleID {
	case "realistic", "real_person", "game_concept":
		return "realistic"
	case "anime", "chinese_animation", "ukiyo_e":
		return "anime"
	case "ink_painting", "xianxia_style", "watercolor", "oil_painting":
		return "classic_illustration"
	case "cyberpunk", "steampunk", "gothic_dark":
		return "dark_stylized"
	case "pixel_art", "sketch":
		return "pixel"
	case "render_3d":
		return "render_3d"
	}
	return ""
}

// universalQualityTags 是所有图像生成 prompt 必须携带的通用质量指令，保证输出基准一致。
const universalQualityTags = "masterpiece, best quality, ultra-detailed, sharp focus, 8K, ultra high resolution, professional"

// resolveStyleQualityTokens 返回与风格匹配的英文质量提升词串，末尾不加逗号。
// 场景图和角色图共用同一套质量词，保证输出基准一致。
func resolveStyleQualityTokens(styleID string) string {
	base := universalQualityTags
	switch resolveStyleCategory(styleID) {
	case "realistic":
		return base + ", photorealistic, cinematic lighting, 8k uhd"
	case "render_3d":
		return base + ", 3D render, ray tracing, volumetric lighting, high-fidelity 3D"
	case "pixel":
		return base + ", crisp pixel art, clean sharp pixels, retro game aesthetic"
	case "classic_illustration":
		return base + ", exquisite brushwork, vibrant colors, professional illustration"
	case "dark_stylized":
		return base + ", dramatic atmosphere, vibrant colors, professional digital art"
	default: // anime, unknown
		return base + ", vibrant colors, clean linework, professional illustration"
	}
}


// resolveStyleIllustrationDesc returns English-language style descriptor tokens for non-realistic styles.
// Replaces the Chinese-language style names that were previously embedded in English prompts,
// improving tokenizer coverage and semantic precision.
func resolveStyleIllustrationDesc(style string) string {
	m := map[string]string{
		"anime":             "anime illustration style, vibrant colors, clean lineart, flat color cel shading",
		"chinese_animation": "Chinese donghua animation style, clean lineart, vibrant flat colors",
		"ink_painting":      "Chinese ink wash painting style, brush stroke texture, monochrome ink wash, xuan paper aesthetic",
		"xianxia_style":     "Chinese xianxia fantasy illustration, ethereal ink-wash atmosphere, intricate traditional patterns",
		"oil_painting":      "oil painting style, rich impasto texture, visible brushstrokes, classical portrait painting",
		"watercolor":        "watercolor illustration style, soft color washes, wet-on-wet blending, translucent layered pigment",
		"pixel_art":         "pixel art style, crisp retro pixels, limited palette, 16-bit game sprite aesthetic",
		"cyberpunk":         "cyberpunk digital concept art, neon accent lighting, high contrast, near-future sci-fi aesthetic",
		"steampunk":         "steampunk illustration, intricate mechanical details, warm brass and sepia tones, Victorian industrial fantasy",
		"gothic_dark":       "gothic dark fantasy illustration, dramatic chiaroscuro shadows, deep jewel tones, macabre atmosphere",
		"sketch":            "pencil sketch illustration, graphite line work, subtle cross-hatching, monochrome drawing",
		"render_3d":         "3D rendered character, subsurface scattering skin, physically-based rendering, high-fidelity 3D model",
		"ukiyo_e":           "ukiyo-e woodblock print style, flat bold color areas, strong black outlines, traditional Japanese Edo period art",
		"game_concept":      "game concept art illustration, professional character design, detailed rendering, RPG fantasy style",
	}
	if d, ok := m[style]; ok {
		return d
	}
	return "detailed digital illustration, professional character design, clean linework"
}

// resolveGenderInfo returns (promptTag, negativeFragment) for a given gender.
// promptTag is the booru-style leading token for positive prompts ("1boy" / "1girl" / "androgynous" / "").
// negativeFragment lists opposite-gender tokens to suppress in the negative prompt.
func resolveGenderInfo(gender string) (tag string, neg string) {
	switch gender {
	case "male":
		return "1boy", "female, girl, woman, 女性, 女生, 裙子, 女装, feminine"
	case "female":
		return "1girl", "male, man, boy, 男性, 男生, 胡须, beard, mustache, masculine"
	case "neutral":
		return "androgynous", ""
	default:
		return "", ""
	}
}

// condenseVisualPrompt trims s to at most maxWords space-separated tokens,
// preferring to break at a comma boundary (within the last 10 words of the budget)
// to avoid cutting mid-phrase.
func condenseVisualPrompt(s string, maxWords int) string {
	words := strings.Fields(s)
	if len(words) <= maxWords {
		return s
	}
	cutIdx := maxWords
	for i := maxWords; i > maxWords-10 && i > 0; i-- {
		if strings.HasSuffix(words[i-1], ",") {
			cutIdx = i
			break
		}
	}
	return strings.TrimRight(strings.Join(words[:cutIdx], " "), ", ")
}

// GenerateThreeViewImage 生成单个视角的角色三视图
// viewType: "front" | "side" | "back"
// gender: "male" | "female" | "neutral" | ""（空时不注入性别词）
// referenceImage: 肖像参考图 URL（可为空）
// provider: 指定图像生成提供者（可为空，空时自动选择）
func (s *ImageGenerationService) GenerateThreeViewImage(ctx context.Context, tenantID uint, name, appearance, viewType, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	// Use precise orthographic angle descriptions to avoid the model interpreting "side view" as a 3/4 angle.
	viewDesc := map[string]string{
		"front": "front view, facing camera directly, full body from head to toe",
		"side":  "pure right side view, 90-degree profile, looking right, full body from head to toe",
		"back":  "back view, facing away from camera, full body from head to toe",
	}
	angleDesc, ok := viewDesc[viewType]
	if !ok {
		return nil, fmt.Errorf("invalid view type: %s", viewType)
	}
	styleStr := resolveStyleDesc(style)
	genderTag, genderNeg := resolveGenderInfo(gender)

	var prompt string
	if style == "realistic" || style == "real_person" {
		realisticGender := map[string]string{"male": "1man, male, ", "female": "1woman, female, ", "neutral": ""}[gender]
		photoQuality := universalQualityTags + ", realistic photography style, pure white background, detailed features, clean composition"
		if style == "real_person" {
			photoQuality = universalQualityTags + ", photorealistic portrait photography, ultra-realistic skin texture, natural lighting, DSLR quality, 8k uhd, pure white background"
		}
		prompt = fmt.Sprintf(
			"%ssolo, full body, %s, %s, "+
				"%s, "+
				"no props, no background elements, no text, no watermarks",
			realisticGender, appearance, angleDesc, photoQuality)
	} else if genderTag != "" {
		// 英文 booru 标签（1boy/1girl）对插画模型权重最高，置于最前
		prompt = fmt.Sprintf(
			"%s, solo, full body, %s, %s, "+
				"%s风格, flat color illustration, clean lineart, character design, "+
				"white background, %s, "+
				"no props, no background elements, no text, no watermarks",
			genderTag, appearance, angleDesc, styleStr, universalQualityTags)
	} else {
		prompt = fmt.Sprintf(
			"solo, full body, %s, %s, "+
				"%s风格, flat color illustration, clean lineart, character design, "+
				"white background, %s, "+
				"no props, no background elements, no text, no watermarks",
			appearance, angleDesc, styleStr, universalQualityTags)
	}

	// Only pass an absolute HTTP(S) URL — local/relative paths cannot be fetched by remote APIs.
	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}
	if aiRef != "" {
		logger.Printf("GenerateThreeViewImage: %s/%s using reference image %s", name, viewType, aiRef)
	} else {
		logger.Printf("GenerateThreeViewImage: %s/%s no valid reference image", name, viewType)
	}

	baseNeg := "multiple people, two people, duo, couple, group, 多人, nsfw, lowres, bad anatomy, " +
		"cropped body, cut off at legs, missing feet, bottom cut off, partial body, floating figure, " +
		"different character, inconsistent appearance, " +
		"makeup, eyeshadow, eye shadow, eyeliner, eye liner, mascara, lipstick, blush, rouge, cosmetics, " +
		"text, labels, watermark, signature"
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}
	url, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, provider, prompt, aiRef, style, negativePrompt, "")
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: fmt.Sprintf("%s %s", name, viewType)}, nil
}

// GenerateThreeViewSheet 生成三合一角色参考图（正视/侧视/背视放在同一张图中）。
// 与 GenerateThreeViewImage 的区别：使用 turnaround sheet 提示词，期望 AI 在单张图内展示三个视角。
// ctx 可携带 ImageStorageHint 用于 OSS 路径构建。
func (s *ImageGenerationService) GenerateThreeViewSheet(ctx context.Context, tenantID uint, name, appearance, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	genderTag, genderNeg := resolveGenderInfo(gender)
	qualityTokens := resolveStyleQualityTokens(style)

	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}

	// 结构控制词置于提示词最前段，确保在 cross-attention 中获得最高权重。
	// 4 格布局：前三格为全身三视图，第四格为面部特写，合并在同一张横版图中。
	// 关键实践：
	//   - "equal-width 4-panel" 约束四格等宽
	//   - panel 4 明确为 bust/face closeup，与全身格区分
	//   - "A-pose arms 30-45 degrees" 量化手臂角度，避免 T-pose
	//   - "same ground baseline" 对齐全身三格脚底基线
	//   - "horizontal wide format" 强化横版构图，匹配 1600×720 输出尺寸
	layoutFrame :=
		"character model sheet, equal-width 4-panel reference sheet, horizontal wide format: " +
			"[panel 1] face closeup bust shot front view, neutral expression, " +
			"[panel 2] 0-degree front-facing full body, " +
			"[panel 3] 90-degree right side profile full body, " +
			"[panel 4] 180-degree back view full body, " +
			"A-pose arms 30-45 degrees from sides on full-body panels, " +
			"same ground baseline for full-body panels, identical appearance across all panels"

	// appearance 截断至 50 词，与结构词（~38）、修饰词（~35）合计控制在 100-150 词范围内。
	// 参考图通过图像编码通道（IP-Adapter）处理面部一致性；
	// 文字层面无法传递实际面部特征，故不使用文字前缀锚定。
	condensedAppearance := condenseVisualPrompt(appearance, 50)

	var prompt string
	if style == "realistic" || style == "real_person" {
		genderPrefix := map[string]string{
			"male": "1man, male, ", "female": "1woman, female, ", "neutral": "androgynous person, ",
		}[gender]
		sheetStyle := "photorealistic character design reference, natural even studio lighting"
		if style == "real_person" {
			sheetStyle = "ultra-realistic skin texture, natural studio lighting, DSLR quality, 8k uhd, sharp focus"
		}
		prompt = genderPrefix + layoutFrame + ", " +
			condensedAppearance + ", " +
			"no makeup natural bare face, " +
			"orthographic projection, character only pure white background, " +
			sheetStyle + ", " +
			qualityTokens + ", " +
			"no text no labels no watermarks"
	} else {
		styleDesc := resolveStyleIllustrationDesc(style)
		genderPrefix := ""
		if genderTag != "" {
			genderPrefix = genderTag + ", "
		}
		prompt = genderPrefix + layoutFrame + ", " +
			condensedAppearance + ", " +
			"no makeup natural bare face, " +
			"orthographic projection, character only white background, " +
			styleDesc + ", " +
			qualityTokens + ", " +
			"no text no labels no watermarks"
	}

	logger.Printf("GenerateThreeViewSheet: %s style=%s ref=%v", name, style, aiRef != "")

	baseNeg := "text, labels, annotations, watermark, signature, caption, speech bubble, " +
		"background objects, scene elements, environment, complex background, " +
		"T-pose, arms straight horizontal, arms glued to body, " +
		"three-quarter view, 45-degree angle, diagonal angle, oblique angle, " +
		"perspective distortion, foreshortening, dynamic pose, action pose, " +
		"different face, inconsistent face, face change, different person, face inconsistency, " +
		"different hairstyle, hair color change, costume mismatch, " +
		"merged panels, overlapping panels, panels bleeding into each other, " +
		"cut off feet on full-body panels, missing feet, missing legs on body panels, " +
		"makeup, eyeshadow, eyeliner, mascara, lipstick, blush, rouge, cosmetics, " +
		"extra limbs, bad anatomy, nsfw, lowres, poorly drawn"
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}

	// 4 格参考图使用 1600x720 横版布局（比三视图 1280x720 更宽，为第 4 格面部特写留出空间）
	refs := []string{}
	if aiRef != "" {
		refs = []string{aiRef}
	}
	url, err := s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, provider, prompt, refs, style, negativePrompt, "1600x720")
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: name + " character sheet with face closeup"}, nil
}

// GenerateFaceCloseupImage 生成角色面部特写图片。
// ctx 可携带 ImageStorageHint 用于 OSS 路径构建。
func (s *ImageGenerationService) GenerateFaceCloseupImage(ctx context.Context, tenantID uint, name, appearance, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	genderTag, genderNeg := resolveGenderInfo(gender)
	qualityTokens := resolveStyleQualityTokens(style)
	condensed := condenseVisualPrompt(appearance, 40)

	var prompt string
	if style == "realistic" || style == "real_person" {
		genderPrefix := map[string]string{"male": "1man, male, ", "female": "1woman, female, ", "neutral": "androgynous person, "}[gender]
		faceStyle := "natural studio lighting, sharp focus on face, high quality portrait"
		if style == "real_person" {
			faceStyle = "ultra-realistic skin texture, 85mm portrait lens, natural studio lighting, DSLR quality, 8k uhd"
		}
		prompt = genderPrefix +
			"bust shot, face centered, front view, solo, " +
			condensed + ", " +
			"no makeup natural bare face, soft even lighting, " +
			faceStyle + ", " +
			qualityTokens + ", " +
			"character only, pure white background, no text no watermarks"
	} else {
		styleDesc := resolveStyleIllustrationDesc(style)
		genderPrefix := ""
		if genderTag != "" {
			genderPrefix = genderTag + ", "
		}
		prompt = genderPrefix +
			"bust shot, face centered, front view, solo, " +
			condensed + ", " +
			"no makeup natural bare face, soft even lighting, sharp focus on face, " +
			styleDesc + ", " +
			qualityTokens + ", " +
			"character only, white background, no text no watermarks"
	}

	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}
	logger.Printf("GenerateFaceCloseupImage: %s ref=%v", name, aiRef != "")

	baseNeg := "multiple people, group, 多人, nsfw, lowres, bad anatomy, " +
		"full body, feet, legs below waist, body below chest, waist and below, " +
		"turnaround, multiple views, front and side and back, three views, character sheet, model sheet, " +
		"props, weapons, furniture, additional objects, background objects, scene elements, environment, " +
		"makeup, eyeshadow, eye shadow, eyeliner, eye liner, mascara, lipstick, blush, rouge, cosmetics, " +
		"text, labels, annotations, watermark, signature, caption, " +
		"harsh shadows, dramatic lighting, complex background"
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}

	// 面部特写使用 9:16（720x1280）竖版布局，适合头像/肖像
	refs := []string{}
	if aiRef != "" {
		refs = []string{aiRef}
	}
	url, err := s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, provider, prompt, refs, style, negativePrompt, "720x1280")
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: name + " face closeup"}, nil
}

// ─── CharacterLook methods ────────────────────────────────────────────────────

func (s *CharacterService) CreateLook(characterID, novelID uint, req *model.CreateCharacterLookRequest) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, fmt.Errorf("look repository not wired")
	}
	look := &model.CharacterLook{
		CharacterID:    characterID,
		NovelID:        novelID,
		Label:          req.Label,
		ChapterFrom:    req.ChapterFrom,
		ChapterTo:      req.ChapterTo,
		SortOrder:      req.SortOrder,
		Description:    req.Description,
		VisualPrompt:   req.VisualPrompt,
		ThreeViewSheet: req.ThreeViewSheet,
		FaceCloseup:    req.FaceCloseup,
		Portrait:       req.Portrait,
	}
	if look.ChapterFrom == 0 {
		look.ChapterFrom = 1
	}
	if err := s.lookRepo.Create(look); err != nil {
		return nil, err
	}
	if req.SetAsDefault {
		_ = s.characterRepo.UpdateDefaultLookID(characterID, look.ID)
	}
	return look, nil
}

// GetDefaultLook 返回角色的默认形象，取 Character.DefaultLookID 指向的 look；未设置则返回 nil。
func (s *CharacterService) GetDefaultLook(characterID uint) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, nil //nolint:nilnil
	}
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil || char.DefaultLookID == 0 {
		return nil, nil //nolint:nilnil
	}
	return s.lookRepo.GetByID(char.DefaultLookID)
}

// upsertDefaultLookVisualPrompt 将 visualPrompt 写入默认形象；若不存在则创建并设为默认。
func (s *CharacterService) upsertDefaultLookVisualPrompt(charID, novelID uint, visualPrompt string) {
	if s.lookRepo == nil || visualPrompt == "" {
		return
	}
	if s.aiService != nil {
		visualPrompt = s.aiService.FilterPrompt(visualPrompt)
	}
	defaultLook, err := s.GetDefaultLook(charID)
	if err != nil {
		return
	}
	if defaultLook != nil {
		defaultLook.VisualPrompt = visualPrompt
		if err := s.lookRepo.Update(defaultLook); err != nil {
			logger.Errorf("[CharacterService] upsertDefaultLookVisualPrompt: update look %d: %v", defaultLook.ID, err)
		}
	} else {
		newLook := &model.CharacterLook{
			CharacterID:  charID,
			NovelID:      novelID,
			Label:        "默认形象",
			ChapterFrom:  1,
			VisualPrompt: visualPrompt,
		}
		if err := s.lookRepo.Create(newLook); err == nil {
			_ = s.characterRepo.UpdateDefaultLookID(charID, newLook.ID)
		}
	}
}

func (s *CharacterService) GetLook(id uint) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, fmt.Errorf("look repository not wired")
	}
	return s.lookRepo.GetByID(id)
}

func (s *CharacterService) ListLooks(characterID uint) ([]*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, fmt.Errorf("look repository not wired")
	}
	return s.lookRepo.ListByCharacter(characterID)
}

func (s *CharacterService) UpdateLook(id uint, req *model.UpdateCharacterLookRequest) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, fmt.Errorf("look repository not wired")
	}
	look, err := s.lookRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Label != nil {
		look.Label = *req.Label
	}
	if req.ChapterFrom != nil {
		look.ChapterFrom = *req.ChapterFrom
	}
	if req.ChapterTo != nil {
		look.ChapterTo = *req.ChapterTo
	}
	if req.SetAsDefault != nil && *req.SetAsDefault {
		_ = s.characterRepo.UpdateDefaultLookID(look.CharacterID, look.ID)
	}
	if req.SortOrder != nil {
		look.SortOrder = *req.SortOrder
	}
	if req.Description != nil {
		look.Description = *req.Description
	}
	if req.VisualPrompt != nil {
		look.VisualPrompt = *req.VisualPrompt
	}
	if req.ThreeViewSheet != nil {
		look.ThreeViewSheet = *req.ThreeViewSheet
	}
	if req.FaceCloseup != nil {
		look.FaceCloseup = *req.FaceCloseup
	}
	if req.Portrait != nil {
		look.Portrait = *req.Portrait
	}
	if err := s.lookRepo.Update(look); err != nil {
		return nil, err
	}
	return look, nil
}

func (s *CharacterService) DeleteLook(id uint) error {
	if s.lookRepo == nil {
		return fmt.Errorf("look repository not wired")
	}
	look, err := s.lookRepo.GetByID(id)
	if err != nil {
		return err
	}
	remaining, err := s.lookRepo.ListByCharacter(look.CharacterID)
	if err != nil {
		return err
	}
	if len(remaining) <= 1 {
		return fmt.Errorf("角色至少需要保留一个形象")
	}
	characterID := look.CharacterID
	char, _ := s.characterRepo.GetByID(characterID)
	wasDefault := char != nil && char.DefaultLookID == id
	if err := s.lookRepo.Delete(id); err != nil {
		return err
	}
	if wasDefault {
		remaining, err := s.lookRepo.ListByCharacter(characterID)
		if err == nil && len(remaining) > 0 {
			_ = s.characterRepo.UpdateDefaultLookID(characterID, remaining[0].ID)
		} else {
			_ = s.characterRepo.UpdateDefaultLookID(characterID, 0)
		}
	}
	return nil
}

// GetActiveLook 返回指定章节号的激活形象；先按章节范围匹配，无匹配则返回默认形象。
func (s *CharacterService) GetActiveLook(characterID uint, chapterNo int) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, nil //nolint:nilnil
	}
	look, err := s.lookRepo.GetActiveLook(characterID, chapterNo)
	if err != nil {
		return nil, err
	}
	if look != nil {
		return look, nil
	}
	// Fallback: default look
	return s.GetDefaultLook(characterID)
}

// GenerateLookVisualPrompt 根据角色基础描述和形象描述生成 AI 图像 Prompt。
// 语言跟随小说 PromptLanguage 设置：zh 输出中文，en 输出英文。
func (s *CharacterService) GenerateLookVisualPrompt(tenantID, characterID uint, lookDesc string) (string, error) {
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil {
		return "", err
	}
	promptLanguage := "zh"
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(char.NovelID); e == nil && novel.PromptLanguage != "" {
			promptLanguage = novel.PromptLanguage
		}
	}
	basePrompt := char.Description
	var sysPrompt string
	if promptLanguage == "en" {
		sysPrompt = fmt.Sprintf(`You are a professional visual designer for novels. Given a character's base description and a specific appearance change description, generate a concise English visual prompt suitable for AI image generation. The prompt should describe physical appearance only (clothing, hair, accessories, body features). Keep it under 200 words. Output only the prompt, no explanation.

Base character: %s

Appearance change: %s

English visual prompt:`, basePrompt, lookDesc)
	} else {
		sysPrompt = fmt.Sprintf(`你是专业的小说视觉设计师。根据角色基础描述和形象描述，生成适合 AI 图像生成的简洁中文视觉提示词。提示词只描述外貌（服装、发型、配饰、体型特征），不超过200字。只输出提示词，不要任何解释。

角色基础描述：%s

形象描述：%s

中文视觉提示词：`, basePrompt, lookDesc)
	}
	result, err := s.aiService.GenerateWithProvider(tenantID, char.NovelID, "character_profile", sysPrompt, "",
		StoryboardOverrides{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result), nil
}

// chapterAppearanceResult AI 返回的角色形象更新结果
type chapterAppearanceResult struct {
	VisualPrompt string `json:"visual_prompt"`
	Description  string `json:"description"`
}

// GenerateChapterImages 根据章节内容，用 AI 重写选定角色的视觉描述（visual_prompt + description），并生成形象图片。
// 返回 (succeeded, failed, error)。
func (s *CharacterService) GenerateChapterImages(
	ctx context.Context,
	tenantID, novelID uint,
	chapter *model.Chapter,
	characterIDs []uint,
	provider string,
	progressFn func(int),
) (succeeded, failed int, err error) {
	all, e := s.characterRepo.ListByNovel(novelID)
	if e != nil {
		return 0, 0, fmt.Errorf("list characters: %w", e)
	}
	idSet := make(map[uint]bool, len(characterIDs))
	for _, id := range characterIDs {
		idSet[id] = true
	}
	var chars []*model.Character
	for _, c := range all {
		if idSet[c.ID] {
			chars = append(chars, c)
		}
	}
	if len(chars) == 0 {
		return 0, 0, nil
	}

	promptLanguage := "zh"
	imageStyle := ""
	var novelTitle string
	if s.novelRepo != nil {
		if novel, e2 := s.novelRepo.GetByID(novelID); e2 == nil {
			if novel.PromptLanguage != "" {
				promptLanguage = novel.PromptLanguage
			}
			imageStyle = novel.ImageStyle
			novelTitle = novel.Title
		}
	}

	imgSvc := NewImageGenerationService(s.aiService)
	total := len(chars)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int

	for _, char := range chars {
		char := char
		wg.Add(1)
		go func() {
			defer wg.Done()
			charFailed := false

			baseDesc := char.Description
			promptText, renderErr := renderPrompt("chapter_character_appearance", map[string]interface{}{
				"CharacterName":        char.Name,
				"CharacterDescription": baseDesc,
				"ChapterContent":       truncateForPrompt(chapter.Content, 4000),
				"PromptLanguage":       promptLanguage,
			})
			if renderErr != nil {
				logger.Errorf("[CharacterService] GenerateChapterImages render char %d: %v", char.ID, renderErr)
				charFailed = true
			} else {
				result, llmErr := s.aiService.GenerateWithProvider(tenantID, novelID, "chapter_character_appearance",
					promptText, "", StoryboardOverrides{MaxTokens: 1024})
				if llmErr != nil {
					logger.Errorf("[CharacterService] GenerateChapterImages LLM char %d (%s): %v", char.ID, char.Name, llmErr)
					charFailed = true
				} else {
					cleaned := extractJSON(strings.TrimSpace(result))
					var res chapterAppearanceResult
					if jsonErr := json.Unmarshal([]byte(cleaned), &res); jsonErr != nil {
						logger.Errorf("[CharacterService] GenerateChapterImages parse char %d: %v raw=%q", char.ID, jsonErr, result)
						charFailed = true
					} else {
						if res.Description != "" {
							updateReq := &model.UpdateCharacterRequest{Name: char.Name, Description: res.Description}
							if _, saveErr := s.UpdateCharacter(char.ID, tenantID, updateReq); saveErr != nil {
								logger.Errorf("[CharacterService] GenerateChapterImages save char %d: %v", char.ID, saveErr)
								charFailed = true
							}
						}
						if res.VisualPrompt != "" {
							s.upsertDefaultLookVisualPrompt(char.ID, char.NovelID, res.VisualPrompt)

							// 生成实际图片：面部特写 + 三视图
							genCtx := ctx
							if novelTitle != "" {
								genCtx = WithImageStorageHint(genCtx, ImageStorageHint{NovelTitle: novelTitle})
							}
							appearance := res.VisualPrompt
							gender := InferGenderTag(appearance, res.Description)

							lookUpdateReq := &model.UpdateCharacterLookRequest{}
							needUpdate := false

							faceRef := ""
							if faceImg, faceErr := imgSvc.GenerateFaceCloseupImage(genCtx, tenantID, char.Name, appearance, imageStyle, gender, "", provider); faceErr == nil {
								lookUpdateReq.FaceCloseup = &faceImg.URL
								lookUpdateReq.Portrait = &faceImg.URL
								faceRef = faceImg.URL
								needUpdate = true
							} else {
								logger.Errorf("[CharacterService] GenerateChapterImages face char %d (%s): %v", char.ID, char.Name, faceErr)
							}

							if threeImg, threeErr := imgSvc.GenerateThreeViewSheet(genCtx, tenantID, char.Name, appearance, imageStyle, gender, faceRef, provider); threeErr == nil {
								lookUpdateReq.ThreeViewSheet = &threeImg.URL
								needUpdate = true
							} else {
								logger.Errorf("[CharacterService] GenerateChapterImages three-view char %d (%s): %v", char.ID, char.Name, threeErr)
							}

							if needUpdate {
								if defaultLook, e3 := s.GetDefaultLook(char.ID); e3 == nil && defaultLook != nil {
									if _, saveErr := s.UpdateLook(defaultLook.ID, lookUpdateReq); saveErr != nil {
										logger.Errorf("[CharacterService] GenerateChapterImages save look %d: %v", defaultLook.ID, saveErr)
									}
								}
							}
						}
					}
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
	logger.Printf("[CharacterService] GenerateChapterImages done: novelID=%d chapterNo=%d succeeded=%d failed=%d",
		novelID, chapter.ChapterNo, succeeded, failed)
	return succeeded, failed, nil
}
