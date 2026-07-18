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
	ctx context.Context,
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

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "extract_character_names", prompt, "")
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
	ctx context.Context,
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
			entries, err := s.extractCharNamesFromContent(ctx, tenantID, novelID, novelTitle, genre, content, existingNamesJSON)
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
		if consolidated, err := s.consolidateCharacterNames(ctx, tenantID, novelID, novelTitle, merged); err == nil && len(consolidated) > 0 {
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
	ctx context.Context,
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
	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "consolidate_character_names", prompt, "")
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
	ctx context.Context,
	tenantID, novelID uint,
	novelTitle, genre, worldviewContext string,
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
		"GenreVisualHints": genreVisualHints(genre),
		"WorldviewContext": worldviewContext,
	})
	if err != nil {
		return nil, fmt.Errorf("render generate_character_profile: %w", err)
	}

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "generate_character_profile", prompt, "",
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

// GenerateCharacterInfo 根据角色名称、类型（及用户可选的草稿描述提示）AI 生成角色描述（外貌、性格、背景）。
// 用于"新建角色"弹窗的一键填充：不依赖章节内容，仅返回生成结果，不落库，由前端展示后随用户确认的表单一起走 CreateCharacter 创建。
// 与 generateOneCharacterProfile 不同——那个函数专用于从真实章节摘要重新分析已有角色，必须有 Summaries 才不会生成空泛内容。
func (s *CharacterService) GenerateCharacterInfo(tenantID, novelID uint, name, role, userHint string) (string, error) {
	novelTitle, novelGenre := novelPromptContext(s.novelRepo, novelID)

	rendered, tplErr := renderPrompt("generate_character_info", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         novelGenre,
		"CharacterName": name,
		"CharacterRole": role,
		"UserHint":      userHint,
	})
	if tplErr != nil {
		return "", fmt.Errorf("render generate_character_info: %w", tplErr)
	}

	result, genErr := s.aiService.GenerateWithProvider(tenantID, novelID, "generate_character_info", rendered, "")
	if genErr != nil {
		return "", fmt.Errorf("AI generate character info: %w", genErr)
	}

	type charInfoJSON struct {
		Description string `json:"description"`
	}
	var info charInfoJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if parseErr := json.Unmarshal([]byte(cleaned), &info); parseErr != nil {
		logger.Errorf("[CharacterService] GenerateCharacterInfo: parse error: %v, raw: %.300s", parseErr, result)
		return "", fmt.Errorf("parse character info JSON: %w", parseErr)
	}
	return info.Description, nil
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
	ChapterOverride      *model.ChapterCharacter `json:"chapter_override,omitempty"`
	EffectiveDescription string                  `json:"effective_description"`
	EffectiveStatus      string                  `json:"effective_status"`
	EffectiveLocation    string                  `json:"effective_location"`
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

	onDeleteHook func(novelID uint) // fired after a character is deleted
}

// OnDeleteCharacter registers a callback fired whenever a character is deleted.
// The callback receives the novel ID so downstream caches can be invalidated.
func (s *CharacterService) OnDeleteCharacter(fn func(novelID uint)) {
	s.onDeleteHook = fn
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

// suggestVoiceLanguage 推荐配音语言编码（存入 voice_language 字段），固定为中文普通话。
func suggestVoiceLanguage() string {
	return "zh-cmn" // 中文普通话
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
		return novel.AIConfig.ImageStyle
	}
	return ""
}

// InjectDefaultLooks 批量查询一组角色的默认形象，将完整 CharacterLook 对象注入 DefaultLook 字段。
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
		if c, ok := charByLookID[lookID]; ok {
			c.DefaultLook = look
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

// CreateCharacter 创建角色。
// novel_id+name 有唯一索引（uniq_char_novel_name），而删除角色是软删除（deleted_at 置位，
// 行仍占用这个唯一索引）——删除后用同名重新创建会撞唯一索引报 MySQL 1062 错误。这里先查一次
// （含软删除记录，AIBatchGenerate 里已经用同一套方法处理过这个问题，这里补齐单个手动创建的
// 入口）：命中软删除记录就恢复并用新请求覆盖字段，命中活跃记录则返回明确的重名错误。
func (s *CharacterService) CreateCharacter(novelID uint, req *model.CreateCharacterRequest) (*model.Character, error) {
	if existing, err := s.characterRepo.FindByNovelAndNameUnscoped(novelID, req.Name); err == nil && existing != nil {
		if !existing.DeletedAt.Valid {
			return nil, fmt.Errorf("角色「%s」已存在", req.Name)
		}
		if err := s.characterRepo.RestoreByID(existing.ID); err != nil {
			return nil, fmt.Errorf("restore soft-deleted character: %w", err)
		}
		existing.DeletedAt.Valid = false
		existing.Role = req.Role
		existing.Description = req.Description
		existing.Meta.Gender = req.Gender
		existing.Meta.Age = req.Age
		existing.Status = "active"
		return existing, s.characterRepo.Update(existing)
	}

	character := &model.Character{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		Name:        req.Name,
		Role:        req.Role,
		Description: req.Description,
		Meta: model.CharacterMeta{
			Gender: req.Gender,
			Age:    req.Age,
		},
		Status: "active",
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
		character.Meta.Gender = req.Gender
	}
	if req.Age != "" {
		character.Meta.Age = req.Age
	}
	if req.Description != "" {
		character.Description = req.Description
	}
	// 内在动机字段：空字符串也允许覆盖（支持清空）
	if req.InnerConflict != "" {
		character.Meta.InnerConflict = req.InnerConflict
	}
	if req.CoreDesire != "" {
		character.Meta.CoreDesire = req.CoreDesire
	}
	if req.VoiceID != "" {
		character.VoiceConfig.VoiceID = req.VoiceID
		// When updating voice, also sync style (allow clearing to empty/default)
		character.VoiceConfig.VoiceStyle = req.VoiceStyle
	} else if req.VoiceStyle != "" {
		character.VoiceConfig.VoiceStyle = req.VoiceStyle
	}
	if req.VoiceSpeed != nil {
		character.VoiceConfig.VoiceSpeed = *req.VoiceSpeed
	}
	if req.VoiceLanguage != "" {
		character.VoiceConfig.VoiceLanguage = req.VoiceLanguage
	}
	if req.VoiceSample != "" {
		character.VoiceConfig.VoiceSample = req.VoiceSample
	}
	if err := s.characterRepo.Update(character); err != nil {
		if isDuplicateKeyError(err) {
			return nil, fmt.Errorf("角色名 %q 在本小说中已存在，请先删除重名角色再修改: %w", character.Name, err)
		}
		return nil, err
	}
	// Auto-snapshot when key characterization fields change (best-effort).
	if s.snapshotRepo != nil && (req.Description != "" || req.InnerConflict != "" || req.CoreDesire != "") {
		snap := &model.CharacterStateSnapshot{
			NovelID:     character.NovelID,
			CharacterID: character.ID,
			Motivation:  character.Meta.CoreDesire,
		}
		_ = s.snapshotRepo.Upsert(snap) // ignore error
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
	if err := s.characterRepo.BatchDeleteByNovel(novelID, ids); err != nil {
		return err
	}
	if s.onDeleteHook != nil {
		s.onDeleteHook(novelID)
	}
	return nil
}

// CreateCharacterSnapshot 手动创建角色状态快照
func (s *CharacterService) CreateCharacterSnapshot(characterID uint, motivation, mood string) (*model.CharacterStateSnapshot, error) {
	if s.snapshotRepo == nil {
		return nil, fmt.Errorf("snapshot repo not configured")
	}
	snap := &model.CharacterStateSnapshot{
		CharacterID: characterID,
		Motivation:  motivation,
		Mood:        mood,
	}
	if err := s.snapshotRepo.Upsert(snap); err != nil {
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

	if err := s.characterRepo.Delete(id); err != nil {
		return err
	}
	if s.onDeleteHook != nil {
		s.onDeleteHook(char.NovelID)
	}
	return nil
}

// ListEffectiveCharacters 获取章节有效角色列表：
// 仅返回明确绑定到本章节的角色（与道具/场景行为一致）。
// 未绑定任何角色时返回空列表；分镜生成等下游服务已内置"无绑定则退回小说全量"的降级逻辑。
func (s *CharacterService) ListEffectiveCharacters(novelID, chapterID uint) ([]*EffectiveCharacter, error) {
	overrideMap := make(map[uint]*model.ChapterCharacter)
	boundIDs := make(map[uint]bool)
	if s.chapterCharacterRepo != nil {
		overrides, err := s.chapterCharacterRepo.ListByChapter(chapterID)
		if err != nil {
			logger.Errorf("[CharacterService] ListEffectiveCharacters: ListByChapter chapterID=%d err=%v", chapterID, err)
		}
		for _, o := range overrides {
			overrideMap[o.CharacterID] = o
			boundIDs[o.CharacterID] = true
		}
		logger.Printf("[CharacterService] ListEffectiveCharacters: novelID=%d chapterID=%d boundCount=%d", novelID, chapterID, len(boundIDs))
	}
	if len(boundIDs) == 0 {
		return []*EffectiveCharacter{}, nil
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	result := make([]*EffectiveCharacter, 0)
	for _, ch := range chars {
		if !boundIDs[ch.ID] {
			continue
		}
		ec := &EffectiveCharacter{Character: *ch}
		if o, ok := overrideMap[ch.ID]; ok {
			ec.ChapterOverride = o
			base := ch.Description
			var parts []string
			if o.Appearance != "" {
				parts = append(parts, "外貌（本章）："+o.Appearance)
			}
			if o.Personality != "" {
				parts = append(parts, "性格（本章）："+o.Personality)
			}
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
	logger.Printf("[CharacterService] ListEffectiveCharacters: novelID=%d chapterID=%d totalChars=%d resultCount=%d", novelID, chapterID, len(chars), len(result))
	return result, nil
}

// UpsertChapterCharacter 创建或更新章节级角色覆盖
func (s *CharacterService) UpsertChapterCharacter(novelID, chapterID, characterID uint, req *model.UpsertChapterCharacterRequest) (*model.ChapterCharacter, error) {
	if s.chapterCharacterRepo == nil {
		return nil, fmt.Errorf("chapter character repository not configured")
	}
	cc := &model.ChapterCharacter{
		CharacterID:   characterID,
		ChapterID:     chapterID,
		NovelID:       novelID,
		Appearance:    req.Appearance,
		Personality:   req.Personality,
		Status:        req.Status,
		Location:      req.Location,
		Notes:         req.Notes,
		RoleInChapter: req.RoleInChapter,
		Action:        req.Action,
		Change:        req.Change,
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

// AIBatchGenerate 使用 AI 批量生成/更新小说角色（按 novel_id+name upsert，仅补填空字段）
// AIBatchGenerate 使用 AI 批量生成/更新小说角色（两阶段：先提名单，再并发生成档案）
func (s *CharacterService) AIBatchGenerate(ctx context.Context, tenantID, novelID uint) ([]*model.Character, error) {
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

	// 获取小说标题/类型/世界观
	novelTitle := "本小说"
	novelGenre := ""
	worldviewContext := ""
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelTitle = novel.Title
			novelGenre = novel.Meta.Genre
			worldviewContext = buildWorldviewVisualContext(novel.Worldview)
		}
	}

	// ── 阶段一：逐章并发提取角色名单，合并去重 ──────────────────────────────
	nameList, err := s.extractCharacterNamesFromChapters(ctx, tenantID, novelID, novelTitle, novelGenre, chapters)
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
			p, err := s.generateOneCharacterProfile(ctx, tenantID, novelID, novelTitle, novelGenre, worldviewContext, e, shortSummaries)
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
			if p.Appearance != "" {
				descParts = append(descParts, "外貌："+p.Appearance)
			}
			if p.Personality != "" {
				descParts = append(descParts, "性格："+p.Personality)
			}
			if p.Background != "" {
				descParts = append(descParts, "背景："+p.Background)
			}
			if p.CharacterArc != "" {
				descParts = append(descParts, "弧光："+p.CharacterArc)
			}
			if len(p.DialogueStyle.Patterns) > 0 {
				descParts = append(descParts, "说话风格："+strings.Join(p.DialogueStyle.Patterns, "；"))
			} else if p.DialogueStyle.VocabularyLevel != "" {
				descParts = append(descParts, "说话风格："+p.DialogueStyle.VocabularyLevel)
			}
			description = strings.Join(descParts, "\n")
		}

		suggestedVoice := suggestVoiceForCharacter(description, p.Gender, p.PersonalityTags, role, voiceModels)
		suggestedStyle := suggestVoiceStyle(p.Gender, p.Age, role, p.PersonalityTags, description)
		suggestedLang := suggestVoiceLanguage()

		if ch, ok := byName[p.Name]; ok {
			logger.Printf("[CharacterService] AIBatchGenerate upsert(update) %q", p.Name)
			// AI 生成字段直接覆盖（用户点击"AI 更新角色"语义就是刷新）
			if description != "" {
				ch.Description = description
			}
			if p.Gender != "" {
				ch.Meta.Gender = p.Gender
			}
			if p.Age != "" {
				ch.Meta.Age = p.Age
			}
			// 用户手动配置字段仅在空时填充
			if v, ok := fillIfEmpty(ch.Role, role); ok {
				ch.Role = v
			}
			if v, ok := fillIfEmpty(ch.VoiceConfig.VoiceID, suggestedVoice); ok {
				ch.VoiceConfig.VoiceID = v
			}
			if v, ok := fillIfEmpty(ch.VoiceConfig.VoiceStyle, suggestedStyle); ok {
				ch.VoiceConfig.VoiceStyle = v
			}
			if v, ok := fillIfEmpty(ch.VoiceConfig.VoiceLanguage, suggestedLang); ok {
				ch.VoiceConfig.VoiceLanguage = v
			}
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
			// DB 级二次兜底：byName 快照可能在并发/重试间过期。
			// 同时检查软删除记录，避免触发唯一索引冲突。
			if existing, _ := s.characterRepo.FindByNovelAndNameUnscoped(novelID, p.Name); existing != nil {
				if existing.DeletedAt.Valid {
					// 软删除记录：恢复并更新字段
					logger.Printf("[CharacterService] AIBatchGenerate: restoring soft-deleted character %q (id=%d)", p.Name, existing.ID)
					if err := s.characterRepo.RestoreByID(existing.ID); err != nil {
						logger.Errorf("CharacterService.AIBatchGenerate: restore %s: %v", p.Name, err)
						continue
					}
					existing.DeletedAt.Valid = false
					if description != "" {
						existing.Description = description
					}
					if p.Gender != "" {
						existing.Meta.Gender = p.Gender
					}
					if p.Age != "" {
						existing.Meta.Age = p.Age
					}
					existing.Status = "active"
					if err := s.characterRepo.Update(existing); err != nil {
						logger.Errorf("CharacterService.AIBatchGenerate: update restored %s: %v", p.Name, err)
					}
					if p.VisualPrompt != "" {
						s.upsertDefaultLookVisualPrompt(existing.ID, existing.NovelID, p.VisualPrompt)
					}
					upserted = append(upserted, existing)
				} else {
					// 活跃记录（race condition / 并发写入）：按更新逻辑处理
					logger.Printf("[CharacterService] AIBatchGenerate: DB dedup (active) %q (id=%d)", p.Name, existing.ID)
					if description != "" {
						existing.Description = description
					}
					if p.Gender != "" {
						existing.Meta.Gender = p.Gender
					}
					if p.Age != "" {
						existing.Meta.Age = p.Age
					}
					if err := s.characterRepo.Update(existing); err != nil {
						logger.Errorf("CharacterService.AIBatchGenerate: update dedup %s: %v", p.Name, err)
					}
					if p.VisualPrompt != "" {
						s.upsertDefaultLookVisualPrompt(existing.ID, existing.NovelID, p.VisualPrompt)
					}
					upserted = append(upserted, existing)
				}
				continue
			}
			character := &model.Character{
				UUID:        uuid.New().String(),
				NovelID:     novelID,
				Name:        p.Name,
				Role:        role,
				Description: description,
				Meta: model.CharacterMeta{
					Gender: p.Gender,
					Age:    p.Age,
				},
				VoiceConfig: model.CharacterVoiceConfig{
					VoiceID:       suggestedVoice,
					VoiceStyle:    suggestedStyle,
					VoiceLanguage: suggestedLang,
				},
				Status: "active",
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
func (s *CharacterService) ReanalyzeCharacter(ctx context.Context, tenantID, characterID uint) (*model.Character, error) {
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil {
		return nil, fmt.Errorf("character not found: %w", err)
	}
	if !s.characterBelongsToTenant(char, tenantID) {
		return nil, fmt.Errorf("character not found")
	}

	novelTitle := "本小说"
	novelGenre := ""
	worldviewContext := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(char.NovelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Meta.Genre
			worldviewContext = buildWorldviewVisualContext(novel.Worldview)
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

	profile, err := s.generateOneCharacterProfile(ctx, tenantID, char.NovelID, novelTitle, novelGenre, worldviewContext, entry, shortSummaries)
	if err != nil {
		return nil, fmt.Errorf("AI reanalyze: %w", err)
	}

	if profile.Description != "" {
		char.Description = profile.Description
	}
	if profile.Gender != "" {
		char.Meta.Gender = profile.Gender
	}
	if profile.Age != "" {
		char.Meta.Age = profile.Age
	}

	// 根据最新 gender/age/role 重新推荐配音设置（仅在用户未手动配置时填充）
	var voiceModels []*model.AIModel
	if s.modelRepo != nil {
		voiceModels, _ = s.modelRepo.GetAvailableByTaskType("voice_gen", tenantID)
	}
	suggestedVoice := suggestVoiceForCharacter(char.Description, char.Meta.Gender, profile.PersonalityTags, char.Role, voiceModels)
	suggestedStyle := suggestVoiceStyle(char.Meta.Gender, char.Meta.Age, char.Role, profile.PersonalityTags, char.Description)
	suggestedLang := suggestVoiceLanguage()
	if v, ok := fillIfEmpty(char.VoiceConfig.VoiceID, suggestedVoice); ok {
		char.VoiceConfig.VoiceID = v
	}
	if v, ok := fillIfEmpty(char.VoiceConfig.VoiceStyle, suggestedStyle); ok {
		char.VoiceConfig.VoiceStyle = v
	}
	if v, ok := fillIfEmpty(char.VoiceConfig.VoiceLanguage, suggestedLang); ok {
		char.VoiceConfig.VoiceLanguage = v
	}

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
func (s *CharacterService) AIExtractMinorChars(ctx context.Context, tenantID, novelID, chapterID uint, userPrompt string) ([]*model.Character, error) {
	logger.Printf("[CharacterService] AIExtractMinorChars: tenantID=%d novelID=%d chapterID=%d", tenantID, novelID, chapterID)

	// 序列化同一 novel 的并发提取，防止两个任务同时读到空的 existingNames 而重复创建角色。
	// Redis SETNX 提供跨实例互斥；本地 mutex 作为 fallback（Redis 不可用时）及进程内二次防护。
	redisLockKey := lockKey("char", "extract", novelID)
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
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Meta.Genre
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
		"UserPrompt":       userPrompt,
		"GenreVisualHints": genreVisualHints(novelGenre),
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_minor_characters: %w", err)
	}

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "extract_minor_characters", minorCharsPrompt, "",
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
		suggestedLang := suggestVoiceLanguage()

		char := &model.Character{
			NovelID:     novelID,
			UUID:        uuid.New().String(),
			Name:        c.Name,
			Role:        "minor",
			Description: finalDesc,
			Meta: model.CharacterMeta{
				Gender: c.Gender,
				Age:    c.Age,
			},
			VoiceConfig: model.CharacterVoiceConfig{
				VoiceID:       suggestedVoice,
				VoiceStyle:    suggestedStyle,
				VoiceLanguage: suggestedLang,
			},
			Status: "active",
		}
		// 插入前再次确认（mutex 内，但 reload 防止极端情况）
		existingNameSet[strings.ToLower(c.Name)] = true // 先占位，防止同批次重复
		// DB 级二次兜底（含软删除）：避免唯一索引冲突。
		if dup, _ := s.characterRepo.FindByNovelAndNameUnscoped(novelID, c.Name); dup != nil {
			if dup.DeletedAt.Valid {
				// 软删除记录：恢复并绑定到本章节
				logger.Printf("[CharacterService] AIExtractMinorChars: restoring soft-deleted %q (id=%d)", c.Name, dup.ID)
				if e := s.characterRepo.RestoreByID(dup.ID); e != nil {
					logger.Errorf("[CharacterService] AIExtractMinorChars: restore %q: %v", c.Name, e)
				}
				if s.chapterCharacterRepo != nil {
					if e := s.chapterCharacterRepo.Upsert(&model.ChapterCharacter{
						CharacterID: dup.ID,
						ChapterID:   chapterID,
						NovelID:     novelID,
					}); e != nil {
						logger.Errorf("[CharacterService] AIExtractMinorChars: bind restored char %q (id=%d) to chapterID=%d: %v", c.Name, dup.ID, chapterID, e)
					} else {
						logger.Printf("[CharacterService] AIExtractMinorChars: bound restored char %q (id=%d) to chapterID=%d", c.Name, dup.ID, chapterID)
					}
				}
			} else {
				logger.Printf("[CharacterService] AIExtractMinorChars: DB dedup: %q already exists (id=%d), binding to chapter instead", c.Name, dup.ID)
				if s.chapterCharacterRepo != nil {
					if e := s.chapterCharacterRepo.Upsert(&model.ChapterCharacter{
						CharacterID: dup.ID,
						ChapterID:   chapterID,
						NovelID:     novelID,
					}); e != nil {
						logger.Errorf("[CharacterService] AIExtractMinorChars: bind dedup char %q (id=%d) to chapterID=%d: %v", c.Name, dup.ID, chapterID, e)
					} else {
						logger.Printf("[CharacterService] AIExtractMinorChars: bound dedup char %q (id=%d) to chapterID=%d", c.Name, dup.ID, chapterID)
					}
				}
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
			charID, matched := existingNameToID[strings.ToLower(name)]
			if !matched {
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
			imageStyle = novel.AIConfig.ImageStyle
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

	// force=true 全量重新生成；否则仅处理缺三视图的角色
	var todo []*model.Character
	for _, c := range chars {
		look := defaultLookMap[c.ID]
		if force || look == nil || look.ThreeViewSheet == "" {
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

			// ── 生成角色参考图（三视图+面部特写同框合图）──────────────────────
			faceRef := ""
			if look != nil {
				faceRef = look.ThreeViewSheet
			}

			var newThreeURL string
			if force || look == nil || look.ThreeViewSheet == "" {
				sheet, sheetErr := imgSvc.GenerateThreeViewSheet(genCtx, tenantID, char.Name, charAppearance, imageStyle, faceRef, provider)
				if sheetErr != nil {
					logger.Errorf("[CharacterService] BatchGenerateImages: character sheet gen for char %d (%s) failed: %v", char.ID, char.Name, sheetErr)
					charFailed = true
				} else {
					newThreeURL = sheet.SheetURL
				}
			}

			// ── Step 3: 保存结果到 Look 记录 ────────────────────────────────────
			var savedLookID uint
			if look != nil {
				// 更新已有 Look
				if newThreeURL != "" {
					updateReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &newThreeURL}
					if _, saveErr := s.UpdateLook(look.ID, updateReq); saveErr != nil {
						logger.Errorf("[CharacterService] BatchGenerateImages: save look for char %d: %v", char.ID, saveErr)
						charFailed = true
					}
				}
				savedLookID = look.ID
			} else if newThreeURL != "" {
				// 角色尚无默认形象，自动创建
				newLook := &model.CharacterLook{
					CharacterID:    char.ID,
					NovelID:        char.NovelID,
					Label:          "默认形象",
					VisualPrompt:   charAppearance,
					ThreeViewSheet: newThreeURL,
				}
				if createErr := s.lookRepo.Create(newLook); createErr != nil {
					logger.Errorf("[CharacterService] BatchGenerateImages: create default look for char %d: %v", char.ID, createErr)
					charFailed = true
				} else {
					_ = s.characterRepo.UpdateDefaultLookID(char.ID, newLook.ID)
					savedLookID = newLook.ID
				}
			}
			_ = savedLookID

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

func (s *CharacterService) AnalyzeConsistency(tenantID, id uint, images []string) (interface{}, error) {
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

	response, err := s.aiService.GenerateWithVision(tenantID, prompt, images)
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

func (s *ImageGenerationService) GenerateCharacterImage(ctx context.Context, tenantID uint, req *model.GenerateImageRequest) (*GeneratedCharacterImage, error) {
	options := &ImageGenerationOptions{
		Prompt:   fmt.Sprintf("%s, %s, %s style", req.Subject, req.Description, req.Style),
		Size:     "1024x1024",
		Steps:    50,
		CFGScale: 7.5,
	}
	image, err := s.aiService.GenerateImage(ctx, tenantID, options.Prompt, options)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: image.URL, Description: req.Description}, nil
}

// resolveStyleCategory 从风格库（ImageStylePreset.PromptCategory，管理员可在 /image-style-presets
// 管理页编辑）读取风格 ID 归入的大类，用于选择匹配的质量提升词/冲突清理词。
// 返回值："realistic" / "anime" / "classic_illustration" / "dark_stylized" / "pixel" / "render_3d" / "" (未知)
func resolveStyleCategory(styleID string) string {
	if c, ok := lookupStylePresetFromCache(styleID); ok {
		return c.category
	}
	return ""
}

// universalQualityTags 是所有图像生成 prompt 必须携带的通用质量指令，保证输出基准一致。
const universalQualityTags = "杰作，最佳质量，超精细，锐利对焦，8K，超高分辨率，专业级"

// resolveStyleQualityTokens 返回与风格匹配的中文质量提升词串，末尾不加逗号。
// 场景图和角色图共用同一套质量词，保证输出基准一致。
// 重要：写实/3D 风格才使用 "8K，超高分辨率" 等摄影写实信号；
// 动漫/插画等风格禁止使用这些词，否则模型会偏向写实输出。
func resolveStyleQualityTokens(styleID string) string {
	// 写实/3D 专用基础词（含 8K/UHD 摄影信号）
	realisticBase := universalQualityTags
	// 插画/动漫基础词（不含 8K/UHD，避免推向写实）
	illustrationBase := "杰作，最佳质量，超精细，锐利对焦，专业级"
	switch resolveStyleCategory(styleID) {
	case "realistic":
		return realisticBase + "，照片级真实感，电影感光效，8k超高清"
	case "render_3d":
		return realisticBase + "，3D渲染，光线追踪，体积光，高保真3D"
	case "anime":
		return illustrationBase + "，鲜艳色彩，干净线稿，专业动漫插画"
	case "pixel":
		return illustrationBase + "，清晰像素画，锐利像素点，复古游戏美术风格"
	case "classic_illustration":
		return illustrationBase + "，精湛笔触，鲜艳色彩，专业插画"
	case "dark_stylized":
		return illustrationBase + "，戏剧化氛围，鲜艳色彩，专业数字绘画"
	default: // unknown
		return illustrationBase + "，鲜艳色彩，干净线稿，专业插画"
	}
}

// removeConflictingQualityTokens strips style-conflicting quality tokens from a prompt.
// Non-realistic styles: removes realistic/photography tokens from character VPs or old storyboard data.
// Realistic/3D styles: removes anime/illustration tokens that may come from character VPs generated
// under a different style setting.
func removeConflictingQualityTokens(prompt, styleID string) string {
	cat := resolveStyleCategory(styleID)
	var conflicts []string
	switch cat {
	case "realistic", "render_3d":
		// Remove anime/illustration tokens that contaminate realistic/3D prompts.
		// 保留旧版英文 token（清理历史存量数据），并追加中文等效表述（清理迁移后新生成的数据）。
		conflicts = []string{
			"anime illustration style",
			"anime illustration",
			"clean lineart, flat color cel shading",
			"flat color cel shading",
			"cel shading",
			"clean lineart",
			"vibrant colors, clean linework, professional anime illustration",
			"professional anime illustration",
			"Chinese donghua animation style",
			"donghua animation style",
			"ink wash painting style",
			"brush stroke texture, monochrome ink wash",
			"xuan paper aesthetic",
			"xianxia fantasy illustration",
			"watercolor illustration style",
			"soft color washes, wet-on-wet blending",
			"pixel art style",
			"crisp retro pixels",
			"pencil sketch illustration",
			"graphite line work",
			"动漫插画风格",
			"平涂赛璐璐上色",
			"赛璐璐上色",
			"干净线稿",
			"专业动漫插画",
			"国产动画风格",
			"水墨画风格",
			"笔触质感，单色水墨",
			"宣纸质感",
			"仙侠奇幻插画",
			"水彩插画风格",
			"柔和色彩晕染",
			"像素画风格",
			"清晰复古像素",
			"铅笔素描插画",
			"石墨线条",
		}
	case "":
		return prompt // unknown style: keep all tokens
	default:
		// For non-realistic styles: remove tokens that belong exclusively to realistic photography.
		// 保留旧版英文 token（清理历史存量数据），并追加中文等效表述（清理迁移后新生成的数据）。
		conflicts = []string{
			"photorealistic, cinematic lighting, 8k uhd",
			"photorealistic, cinematic lighting",
			"photorealistic",
			"cinematic lighting",
			"cinematic film photography",
			"cinematic photography",
			"film photography",
			"realistic skin texture",
			"8k uhd",
			"8K uhd",
			"shot on DSLR",
			"DSLR photography",
			"hyperrealistic",
			"ultra realistic",
			"照片级真实感，电影感光效，8k超高清",
			"照片级真实感，电影感光效",
			"照片级真实感",
			"电影感光效",
			"复古胶片摄影美术风格",
			"胶片摄影",
			"写实皮肤质感",
			"8k超高清",
			"单反相机拍摄",
			"单反摄影",
			"超写实",
		}
	}
	result := prompt
	for _, tok := range conflicts {
		// Case-insensitive removal: replace ", TOKEN" or "TOKEN, " or standalone
		lower := strings.ToLower(result)
		lowerTok := strings.ToLower(tok)
		for {
			idx := strings.Index(lower, lowerTok)
			if idx < 0 {
				break
			}
			// Determine the full token span including surrounding commas/spaces
			start := idx
			end := idx + len(tok)
			// Absorb leading ", " or ", "
			if start >= 2 && result[start-2:start] == ", " {
				start -= 2
			} else if start >= 1 && (result[start-1] == ',' || result[start-1] == ' ') {
				start--
			}
			// Absorb trailing ", " or ", "
			if end+2 <= len(result) && result[end:end+2] == ", " {
				end += 2
			} else if end < len(result) && (result[end] == ',' || result[end] == ' ') {
				end++
			}
			result = result[:start] + result[end:]
			lower = strings.ToLower(result)
		}
	}
	return strings.TrimRight(strings.TrimLeft(result, ", "), ", ")
}

// resolveStyleIllustrationDesc returns Chinese-language style descriptor tokens for non-realistic styles.
func resolveStyleIllustrationDesc(style string) string {
	// 风格库（管理员可在 /image-style-presets 管理页编辑 prompt 字段）是风格描述词的唯一来源。
	if c, ok := lookupStylePresetFromCache(style); ok && c.prompt != "" {
		return c.prompt
	}
	// 自定义画风（用户在"自定义"风格页填写的自然语言描述）不在风格库中，
	// 但已经是完整的风格描述文本（含中文或空格），应直接透传而非被通用兜底词静默替换；
	// 只有真正无法识别的短 ID（如失效的旧预设）才落到下面的通用兜底。
	if style != "" && (containsChinese(style) || strings.Contains(style, " ")) {
		return style
	}
	return "精细数字插画，专业角色设计，干净线稿"
}

// animalKeywords 用于检测纯动物（非人形）角色的关键词。
// 拟人化（如"狐女""兽人"）不在此列，它们依然使用性别 token。
var animalKeywords = []string{
	// 英文常见动物
	"tiger", "lion", "wolf", "fox", "bear", "dragon", "snake", "horse", "deer", "rabbit",
	"eagle", "hawk", "crow", "cat", "dog", "puppy", "kitten", "leopard", "panther", "cheetah",
	"elephant", "monkey", "ape", "gorilla", "shark", "whale", "dolphin", "phoenix", "griffin",
	"qilin", "pig", "cow", "bull", "sheep", "goat", "chicken", "duck", "goose", "parrot",
	"panda", "raccoon", "squirrel", "hamster", "frog", "turtle", "crocodile", "alligator",
	// 中文常见动物（之前缺失的核心词）
	"狗", "小狗", "大狗", "狗狗", "猎狗", "猎犬", "犬", "幼犬",
	"猫", "小猫", "猫咪", "猫猫", "猫儿", "幼猫",
	"鸟", "小鸟", "鸡", "公鸡", "母鸡", "鸭", "鹅", "鹦鹉",
	"猪", "小猪", "牛", "耕牛", "羊", "绵羊", "山羊",
	"熊猫", "浣熊", "松鼠", "仓鼠", "青蛙", "乌龟", "鳄鱼",
	// 中文原有词
	"老虎", "狮子", "狼", "狐狸", "熊", "龙", "蛇", "马", "鹿", "兔子", "兔",
	"鹰", "鸦", "乌鸦", "豹", "猎豹", "大象", "猴子", "猩猩", "鲨鱼", "鲸鱼",
	"海豚", "凤凰", "麒麟", "玄武", "神兽", "灵兽", "圣兽",
	// 形态描述
	"quadruped", "four-legged", "feral", "beast form", "animal form",
	"四足", "兽形", "兽态", "动物形态",
}

// anthropomorphicKeywords 表示"人形+动物特征"的词，出现则不视为纯动物。
var anthropomorphicKeywords = []string{
	"anthropomorphic", "anthro", "furry", "kemono",
	"beastman", "beast man", "half-beast",
	"兽人", "人兽", "拟人", "兽耳", "兽娘", "狐女", "猫娘", "猫耳",
	"半兽", "人形", "bipedal", "upright", "stands on two legs",
}

// isAnimalCharacter 返回 true 表示角色是纯动物（非人形），应跳过人类性别 token。
func isAnimalCharacter(appearance string) bool {
	lower := strings.ToLower(appearance)
	for _, kw := range anthropomorphicKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return false // 拟人化，仍按人形处理
		}
	}
	for _, kw := range animalKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
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

// GeneratedCharacterSheet 是 GenerateThreeViewSheet 的生成结果。
type GeneratedCharacterSheet struct {
	SheetURL    string
	Description string
}

// GenerateThreeViewSheet 生成角色参考图：横版16:9，左侧大幅胸部以上特写肖像 +
// 右侧三格全身视图（正面/四分之三侧面/背面），顶部由 AI 直接生成名称标牌。
// ctx 可携带 ImageStorageHint 用于 OSS 路径构建。
func (s *ImageGenerationService) GenerateThreeViewSheet(ctx context.Context, tenantID uint, name, appearance, style, referenceImage, provider string) (*GeneratedCharacterSheet, error) {
	aiRef := ""
	if strings.HasPrefix(referenceImage, "http://") || strings.HasPrefix(referenceImage, "https://") {
		aiRef = referenceImage
	}

	// 人形/动物角色的版式+规则文案见 characterSheetFormatHumanoid / characterSheetFormatAnimal 常量。
	format := characterSheetFormatHumanoid
	if isAnimalCharacter(appearance) {
		format = characterSheetFormatAnimal
	}
	layoutDetails := fmt.Sprintf(format, condenseVisualPrompt(appearance, 40), name)

	// 名称标牌由 AI 直接在图中生成，prompt 固定为「外观描述（截断至80词，跨格一致性的唯一文字锚点）+ 版式规则」。
	prompt := condenseVisualPrompt(appearance, 80) + "，" + layoutDetails

	logger.Printf("GenerateThreeViewSheet: %s style=%s ref=%v", name, style, aiRef != "")

	var refs []string
	if aiRef != "" {
		refs = []string{aiRef}
	}

	// 一致性权重设为 0.4（低权重）：合图需要 prompt 主导布局结构，
	// DreamO（weight>=0.7）以参考图为主会压制多面板布局 prompt 导致所有格都生成正面图。
	// 0.4 → selectImageModel 选 SeedEditV3（volcengine-visual 路径），prompt 主导效果更好。
	size := fmt.Sprintf("%dx%d", characterSheetCanvasWidth, characterSheetCanvasHeight)
	url, err := s.aiService.GenerateCharacterThreeViewMulti(ctx, tenantID, provider, prompt, refs, style, "", size, 0, 0.4)
	if err != nil {
		return nil, err
	}

	return &GeneratedCharacterSheet{
		SheetURL:    url,
		Description: name + " character sheet (portrait + front/three-quarter/back)",
	}, nil
}

const (
	// 横版16:9：左侧40%胸部以上特写肖像 + 右侧三格全身视图（正面/四分之三侧面/背面）。
	characterSheetCanvasWidth  = 1600
	characterSheetCanvasHeight = characterSheetCanvasWidth * 9 / 16
)

// characterSheetFormatHumanoid / characterSheetFormatAnimal 是角色三视图的版式+规则文案，
// 占位符依次为：condensedFace（胸部以上/头部特写的外观锚点）、name（顶部名称标牌文字）。
const (
	characterSheetFormatHumanoid = "格式：角色设计稿，横版16:9，四格布局。左侧面板（最大，约占40%%宽度）：胸部以上的特写肖像，高细节，清晰展示面部、发型、配饰和上装，%s。右侧（3个面板，各约占20%%宽度）：三个全身视图——正面、四分之三侧面、背面。全身面板使用A-Pose站姿。所有全身视图必须展示从头到脚的完整人物，鞋子完全可见且不被裁切。顶部包含大尺寸高对比名称标牌\"%s\"（字号明显增大、优先保证清晰可读）。纯白(#FFFFFF)背景，无环境元素，背景无阴影。" +
		"规则：角色在所有四个面板中必须看起来完全一致——相同的面部、发型、服装、配饰。静态A-Pose站姿，无动作姿势，鞋子清晰可见且不被遮挡。不添加水印或多余文字。特写肖像必须达到高分辨率品质，可见皮肤纹理、眼部细节和配饰细节。"

	characterSheetFormatAnimal = "格式：角色设计稿，横版16:9，四格布局。左侧面板（最大，约占40%%宽度）：头部特写，高细节，清晰展示面部、毛发/皮肤质感，%s。右侧（3个面板，各约占20%%宽度）：三个全身视图——正面、四分之三侧面、背面，自然站立姿势，均展示完整身形，不被裁切。顶部包含大尺寸高对比名称标牌\"%s\"（字号明显增大、优先保证清晰可读）。纯白(#FFFFFF)背景，无环境元素，背景无阴影。" +
		"规则：角色在所有四个面板中必须看起来完全一致——相同的毛色/皮肤、体型特征。静态自然站姿，无动作姿势。不添加水印或多余文字。特写肖像必须达到高分辨率品质，可见毛发/皮肤纹理与眼部细节。"
)

// ─── CharacterLook methods ────────────────────────────────────────────────────

func (s *CharacterService) CreateLook(characterID, novelID uint, req *model.CreateCharacterLookRequest) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, fmt.Errorf("look repository not wired")
	}
	look := &model.CharacterLook{
		CharacterID:    characterID,
		NovelID:        novelID,
		Label:          req.Label,
		Description:    req.Description,
		VisualPrompt:   req.VisualPrompt,
		ThreeViewSheet: req.ThreeViewSheet,
	}
	if err := s.lookRepo.Create(look); err != nil {
		return nil, err
	}
	// 明确要求设为默认，或角色尚无默认形象（第一个形象自动成为默认）
	if req.SetAsDefault {
		_ = s.characterRepo.UpdateDefaultLookID(characterID, look.ID)
	} else if char, err := s.characterRepo.GetByID(characterID); err == nil && char.DefaultLookID == 0 {
		_ = s.characterRepo.UpdateDefaultLookID(characterID, look.ID)
	}
	return look, nil
}

// GetDefaultLook 返回角色的默认形象。
// 若 DefaultLookID 已设置，直接返回该 look；
// 若未设置但角色有形象列表，则将第一个形象自动升级为默认并返回。
func (s *CharacterService) GetDefaultLook(characterID uint) (*model.CharacterLook, error) {
	if s.lookRepo == nil {
		return nil, nil //nolint:nilnil
	}
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil {
		return nil, nil //nolint:nilnil
	}
	if char.DefaultLookID != 0 {
		return s.lookRepo.GetByID(char.DefaultLookID)
	}
	// DefaultLookID 未设置：取第一个形象并自动设为默认
	looks, err := s.lookRepo.ListByCharacter(characterID)
	if err != nil || len(looks) == 0 {
		return nil, nil //nolint:nilnil
	}
	first := looks[0]
	_ = s.characterRepo.UpdateDefaultLookID(characterID, first.ID)
	return first, nil
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
	if req.SetAsDefault != nil && *req.SetAsDefault {
		if err := s.characterRepo.UpdateDefaultLookID(look.CharacterID, look.ID); err != nil {
			return nil, fmt.Errorf("set default look: %w", err)
		}
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

// buildWorldviewVisualContext 从世界观中提取与角色外观强相关的字段（地理格局/文明发展
// 水平、历史时代脉络、文化习俗与服饰传统），供图像提示词生成时保持服装/时代背景与世界观
// 一致。与 chapter_service.go 的 buildWorldRulesText 不同——那个面向写作叙事约束
// （强制规则/修炼体系/势力/术语），这里只取视觉外观相关的字段，避免无关内容稀释 prompt。
func buildWorldviewVisualContext(wv *model.Worldview) string {
	if wv == nil {
		return ""
	}
	var sb strings.Builder
	if wv.History != "" {
		sb.WriteString("时代背景（服装、发型、器物工艺水平必须与此时代相符）：\n")
		sb.WriteString(wv.History)
		sb.WriteString("\n")
	}
	if wv.Geography != "" {
		sb.WriteString("地理格局与文明发展水平（影响服饰材质、建筑风格、器物外观）：\n")
		sb.WriteString(wv.Geography)
		sb.WriteString("\n")
	}
	if wv.Culture != "" {
		sb.WriteString("文化习俗与宗教信仰（影响服饰规范、配饰、礼仪姿态）：\n")
		sb.WriteString(wv.Culture)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// LookVisualPromptResult 是 GenerateLookVisualPrompt 的结构化返回值。
type LookVisualPromptResult struct {
	VisualPrompt string `json:"visual_prompt"`
}

// GenerateLookVisualPrompt 根据角色基础描述和形象描述生成 AI 图像 Prompt（固定输出中文）。
func (s *CharacterService) GenerateLookVisualPrompt(ctx context.Context, tenantID, characterID uint, lookDesc string) (*LookVisualPromptResult, error) {
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil {
		return nil, err
	}
	worldviewContext := ""
	novelContext := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(char.NovelID); e == nil {
			worldviewContext = buildWorldviewVisualContext(novel.Worldview)
			novelContext = strings.TrimSpace(fmt.Sprintf("%s（%s）：%s", novel.Title, novel.Meta.Genre, novel.Meta.Description))
		}
	}
	basePrompt := char.Description
	// 如果 lookDesc 和 basePrompt 完全相同（前端传空、后端 fallback），只保留一份避免重复
	if lookDesc == basePrompt {
		lookDesc = ""
	}
	extraSection := ""
	if lookDesc != "" {
		extraSection = lookDesc
	}
	// GenderAnchor：性别锚定标签，让 visual_prompt 以该 tag 开头，提升扩散模型对角色性别的稳定还原。
	genderAnchor := "人物"
	switch char.Meta.Gender {
	case "male":
		genderAnchor = "男孩"
	case "female":
		genderAnchor = "女孩"
	}
	sysPrompt, err := renderPrompt("character_visual_prompt", map[string]interface{}{
		"WorldviewContext": worldviewContext,
		"NovelContext":     novelContext,
		"BasePrompt":       basePrompt,
		"ExtraSection":     extraSection,
		"CharName":         char.Name,
		"CharRole":         char.Role,
		"CharAge":          char.Meta.Age,
		"CharGender":       char.Meta.Gender,
		"GenderAnchor":     genderAnchor,
	})
	if err != nil {
		return nil, fmt.Errorf("render character_visual_prompt: %w", err)
	}
	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, char.NovelID, "character_profile", sysPrompt, "",
		StoryboardOverrides{})
	if err != nil {
		return nil, err
	}
	var parsed LookVisualPromptResult
	cleaned := extractJSONObject(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("parse character_visual_prompt JSON: %w", err)
	}
	parsed.VisualPrompt = strings.TrimSpace(parsed.VisualPrompt)
	if parsed.VisualPrompt == "" {
		return nil, fmt.Errorf("character_visual_prompt: empty visual_prompt in AI response")
	}
	return &parsed, nil
}

// GenerateCostumeDesign 基于角色描述和世界观背景，AI 生成符合时代背景的统一形象提示词。
// 输出存入 Character.AppearancePrompt，并同步更新默认 CharacterLook 的 VisualPrompt。
//
// 复用 GenerateLookVisualPrompt 的同一次 AI 调用（同一模板 character_visual_prompt.j2，已
// 合并原 character_costume_design.j2 的人种/时代/身份/分层设计规则），不再单独渲染模板、
// 单独调用 LLM。此方法只负责其特有的落库位置（AppearancePrompt 字段）。
func (s *CharacterService) GenerateCostumeDesign(ctx context.Context, tenantID, characterID uint) (string, error) {
	char, err := s.characterRepo.GetByID(characterID)
	if err != nil {
		return "", fmt.Errorf("character not found: %w", err)
	}
	if char.Description == "" {
		return "", fmt.Errorf("角色描述为空，请先填写角色描述")
	}

	result, err := s.GenerateLookVisualPrompt(ctx, tenantID, characterID, "")
	if err != nil {
		return "", err
	}

	// 保存到 Character.AppearancePrompt（旧字段，仍有读取路径依赖）
	char.Meta.AppearancePrompt = result.VisualPrompt
	if err := s.characterRepo.Update(char); err != nil {
		logger.Errorf("[GenerateCostumeDesign] update char %d AppearancePrompt: %v", characterID, err)
	}

	// 同步更新默认 look 的 VisualPrompt（确保 look-based 生成路径也受益）
	s.upsertDefaultLookVisualPrompt(characterID, char.NovelID, result.VisualPrompt)

	logger.Printf("[GenerateCostumeDesign] charID=%d generated visual=%d chars",
		characterID, len(result.VisualPrompt))
	return result.VisualPrompt, nil
}
