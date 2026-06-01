package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// RewriteService handles novel rewriting projects.
type RewriteService struct {
	projectRepo     *repository.RewriteProjectRepository
	analysisRepo    *repository.LiteraryAnalysisRepository
	bibleRepo       *repository.RewriteBibleRepository
	chapterTaskRepo *repository.ChapterRewriteTaskRepository
	chapterRepo     *repository.ChapterRepository
	novelRepo       *repository.NovelRepository
	aiSvc           *AIService
	taskSvc         *TaskService
	continuityRepo  *repository.RewriteContinuityIndexRepository  // optional; nil = no continuity index
	summaryRepo     *repository.RewriteChapterSummaryRepository   // optional; nil = use excerpt fallback
}

// rewriteLevelConfig holds per-level parameters.
type rewriteLevelConfig struct {
	Level            int     // 1-5
	Template         string  // prompt template name (without .j2)
	Goal             string  // human-readable goal
	RetentionTarget  string  // e.g. "80-90%"
	TargetLexSimLow  float64 // lower bound of acceptable lexical similarity
	TargetLexSimHigh float64 // upper bound; exceeding this triggers retry ("改写不足")
}

// rewriteAttempt holds the result of a single AI generation attempt.
type rewriteAttempt struct {
	Content     string
	LexSim      float64
	StructSim   float64
	SemanticSim float64 // entity leakage rate: original entities still present in rewrite
	Passed      bool    // within [TargetLexSimLow, TargetLexSimHigh]
	TooSimilar  bool    // lexSim > TargetLexSimHigh → 改写不足, should retry
}

// ForbiddenDialogue represents a signature dialogue pattern that must be completely rewritten.
type ForbiddenDialogue struct {
	Pattern      string `json:"pattern"`
	Excerpt      string `json:"excerpt"`
	RewriteGuide string `json:"rewrite_guide"`
}

// recentChapterInfo holds opening+closing excerpts as a fallback context when summaryRepo is unavailable.
type recentChapterInfo struct {
	ChapterNo int
	Opening   string
	Closing   string
}

// ── Constants & package-level vars ───────────────────────────────────────────

const maxChapterRetries = 2

var rewriteLevelConfigs = map[int]rewriteLevelConfig{
	1: {Level: 1, Template: "rewrite_depth_shallow", Goal: "仅做词句级同义替换，不改变情节与对话内容", RetentionTarget: "90-95%", TargetLexSimLow: 0.50, TargetLexSimHigh: 0.80},
	2: {Level: 2, Template: "rewrite_depth_medium", Goal: "用全新文学语言重新表达，保留情节骨架", RetentionTarget: "80-90%", TargetLexSimLow: 0.28, TargetLexSimHigh: 0.60},
	3: {Level: 3, Template: "rewrite_depth_medium", Goal: "适度调整场景顺序与细节，改写对话语气", RetentionTarget: "60-75%", TargetLexSimLow: 0.18, TargetLexSimHigh: 0.48},
	4: {Level: 4, Template: "rewrite_depth_deep", Goal: "重构世界观与角色设定，大幅改变故事形式", RetentionTarget: "30-50%", TargetLexSimLow: 0.05, TargetLexSimHigh: 0.33},
	5: {Level: 5, Template: "rewrite_depth_deep", Goal: "只保留精神内核与情感逻辑，全面重创", RetentionTarget: "5-20%", TargetLexSimLow: 0.00, TargetLexSimHigh: 0.18},
}

// retryHints provides progressively stronger instructions on each retry.
var retryHints = []string{
	"", // attempt 0 — no extra hint
	"上次改写不达标（与原文仍过于相似）。请采用更大幅度的文学变形：更换叙事视角（如从第三人称改为第一人称内心独白）、打乱场景顺序、将对话转为心理描写，彻底改变句式结构。",
	"前两次均未达标。请完全重构叙事视角与表达方式：抛弃原文一切表层描述，只保留核心情感逻辑，用截然不同的故事形式（如倒叙、片段化意识流、书信体）重新承载相同的戏剧张力。",
}

// ── Constructor & options ─────────────────────────────────────────────────────

func NewRewriteService(
	projectRepo *repository.RewriteProjectRepository,
	analysisRepo *repository.LiteraryAnalysisRepository,
	bibleRepo *repository.RewriteBibleRepository,
	chapterTaskRepo *repository.ChapterRewriteTaskRepository,
	chapterRepo *repository.ChapterRepository,
	novelRepo *repository.NovelRepository,
	aiSvc *AIService,
) *RewriteService {
	return &RewriteService{
		projectRepo:     projectRepo,
		analysisRepo:    analysisRepo,
		bibleRepo:       bibleRepo,
		chapterTaskRepo: chapterTaskRepo,
		chapterRepo:     chapterRepo,
		novelRepo:       novelRepo,
		aiSvc:           aiSvc,
	}
}

func (s *RewriteService) WithTaskService(svc *TaskService) *RewriteService {
	s.taskSvc = svc
	return s
}

func (s *RewriteService) WithContinuityRepo(r *repository.RewriteContinuityIndexRepository) *RewriteService {
	s.continuityRepo = r
	return s
}

func (s *RewriteService) WithSummaryRepo(r *repository.RewriteChapterSummaryRepository) *RewriteService {
	s.summaryRepo = r
	return s
}

// ── Prompt helpers ─────────────────────────────────────────────────────────────

func renderRewriteTemplate(name string, data interface{}) (string, error) {
	ctx, ok := data.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("renderRewriteTemplate: data must be map[string]interface{}")
	}
	return renderPrompt(name, ctx)
}

// formatCharNamesForPrompt converts a JSON map of original→new names into
// a human-readable bullet list that LLMs process more reliably than raw JSON.
func formatCharNamesForPrompt(charNamesJSON string) string {
	if charNamesJSON == "" || charNamesJSON == "null" || charNamesJSON == "{}" {
		return "（无角色名替换）"
	}
	var nameMap map[string]string
	if err := json.Unmarshal([]byte(charNamesJSON), &nameMap); err != nil {
		return charNamesJSON // fallback: pass raw
	}
	var sb strings.Builder
	for orig, newName := range nameMap {
		if orig != "" && newName != "" {
			sb.WriteString(fmt.Sprintf("- %s → %s\n", orig, newName))
		}
	}
	if sb.Len() == 0 {
		return "（无角色名替换）"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatPropsForPrompt converts a JSON map of prop replacements into a bullet list.
func formatPropsForPrompt(propsJSON string) string {
	if propsJSON == "" || propsJSON == "null" || propsJSON == "{}" {
		return "（无道具替换）"
	}
	var propsMap map[string]string
	if err := json.Unmarshal([]byte(propsJSON), &propsMap); err != nil {
		return propsJSON
	}
	var sb strings.Builder
	for orig, newProp := range propsMap {
		if orig != "" && newProp != "" {
			sb.WriteString(fmt.Sprintf("- %s → %s\n", orig, newProp))
		}
	}
	if sb.Len() == 0 {
		return "（无道具替换）"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// parseForbiddenPhrases extracts []string from the new ForbiddenPhrases field,
// with fallback to the legacy ForbiddenElems mixed-type array.
func parseForbiddenPhrases(bible *model.RewriteBible) []string {
	if bible.ForbiddenPhrases != "" && bible.ForbiddenPhrases != "null" {
		var phrases []string
		if err := json.Unmarshal([]byte(bible.ForbiddenPhrases), &phrases); err == nil {
			return phrases
		}
	}
	// Fallback: try to parse legacy ForbiddenElems as a plain string array
	if bible.ForbiddenElems != "" && bible.ForbiddenElems != "null" {
		var phrases []string
		if err := json.Unmarshal([]byte(bible.ForbiddenElems), &phrases); err == nil {
			return phrases
		}
	}
	return nil
}

// parseForbiddenDialogues extracts []ForbiddenDialogue from the new ForbiddenDialogues field,
// with fallback to extracting objects from the legacy mixed-type ForbiddenElems array.
func parseForbiddenDialogues(bible *model.RewriteBible) []ForbiddenDialogue {
	if bible.ForbiddenDialogues != "" && bible.ForbiddenDialogues != "null" {
		var dialogues []ForbiddenDialogue
		if err := json.Unmarshal([]byte(bible.ForbiddenDialogues), &dialogues); err == nil {
			return dialogues
		}
	}
	// Fallback: parse mixed-type ForbiddenElems, keep only objects
	if bible.ForbiddenElems != "" && bible.ForbiddenElems != "null" {
		var mixed []json.RawMessage
		if err := json.Unmarshal([]byte(bible.ForbiddenElems), &mixed); err == nil {
			var dialogues []ForbiddenDialogue
			for _, item := range mixed {
				var d ForbiddenDialogue
				if err := json.Unmarshal(item, &d); err == nil && d.Pattern != "" {
					dialogues = append(dialogues, d)
				}
			}
			return dialogues
		}
	}
	return nil
}

// formatForbiddenForPrompt builds a human-readable forbidden-elements block from the Bible.
func formatForbiddenForPrompt(bible *model.RewriteBible) string {
	var sb strings.Builder
	phrases := parseForbiddenPhrases(bible)
	if len(phrases) > 0 {
		sb.WriteString("禁止出现的短语：\n")
		for _, p := range phrases {
			if p != "" {
				sb.WriteString(fmt.Sprintf("  ▸「%s」\n", p))
			}
		}
	}
	for _, d := range parseForbiddenDialogues(bible) {
		sb.WriteString(fmt.Sprintf("\n标志性对话「%s」改写指引：\n", d.Pattern))
		if d.Excerpt != "" {
			sb.WriteString(fmt.Sprintf("  原文片段：「%s」\n", d.Excerpt))
		}
		sb.WriteString(fmt.Sprintf("  改写方向：%s\n", d.RewriteGuide))
	}
	if sb.Len() == 0 {
		return "（无特别禁止元素）"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// extractVocabRegister parses the StyleGuide JSON and returns the vocabulary_register value.
func extractVocabRegister(styleGuideJSON string) string {
	if styleGuideJSON == "" || styleGuideJSON == "null" {
		return ""
	}
	var sg map[string]interface{}
	if err := json.Unmarshal([]byte(styleGuideJSON), &sg); err != nil {
		return ""
	}
	v, _ := sg["vocabulary_register"].(string)
	return v
}

// targetWordRange returns the acceptable word count range for a given level and original length.
func targetWordRange(origLen, level int) (minWords, maxWords int) {
	switch level {
	case 1, 2:
		return int(float64(origLen) * 0.8), int(float64(origLen) * 1.2)
	case 3:
		return int(float64(origLen) * 0.7), int(float64(origLen) * 1.4)
	case 4:
		return int(float64(origLen) * 0.5), int(float64(origLen) * 1.8)
	case 5:
		return int(float64(origLen) * 0.4), int(float64(origLen) * 2.0)
	default:
		return int(float64(origLen) * 0.8), int(float64(origLen) * 1.2)
	}
}

// ── Sampling & analysis helpers ───────────────────────────────────────────────

// stratifiedSample picks up to maxSamples chapters at 7 semantic positions across the full novel.
func stratifiedSample(chapters []*model.Chapter, maxSamples int) []*model.Chapter {
	n := len(chapters)
	if n == 0 {
		return nil
	}
	if n <= maxSamples {
		return chapters
	}
	positions := []float64{0, 0.15, 0.35, 0.55, 0.75, 0.90, 1.0}
	if maxSamples < len(positions) {
		positions = make([]float64, maxSamples)
		for i := range positions {
			if maxSamples == 1 {
				positions[i] = 0
			} else {
				positions[i] = float64(i) / float64(maxSamples-1)
			}
		}
	}
	seen := make(map[int]bool)
	result := make([]*model.Chapter, 0, len(positions))
	for _, p := range positions {
		idx := int(p*float64(n-1) + 0.5)
		if idx >= n {
			idx = n - 1
		}
		if !seen[idx] {
			seen[idx] = true
			result = append(result, chapters[idx])
		}
	}
	return result
}

// extractCoreElements returns a reference excerpt for deep-rewrite prompts.
// Level 1-3: leading 500 chars. Level 4-5: beginning + middle + end.
func extractCoreElements(content string, level int) string {
	runes := []rune(content)
	n := len(runes)
	if level < 4 {
		return string(runes[:min(500, n)])
	}
	if n <= 1500 {
		return content
	}
	segLen := 300
	begin := string(runes[:segLen])
	mid := string(runes[n/2-segLen/2 : n/2+segLen/2])
	end := string(runes[n-segLen:])
	return begin + "\n[...中段...]\n" + mid + "\n[...末段...]\n" + end
}

// emotionalArcStage maps chapter position to a narrative stage label.
func emotionalArcStage(chapterNo, total int) string {
	if total <= 0 {
		return ""
	}
	ratio := float64(chapterNo) / float64(total)
	switch {
	case ratio < 0.12:
		return "开篇建立期（世界观铺垫，人物登场）"
	case ratio < 0.30:
		return "矛盾上升期（冲突萌芽，关系确立）"
	case ratio < 0.50:
		return "发展深化期（情节推进，张力积累）"
	case ratio < 0.70:
		return "高潮酝酿期（危机激化，情感爆发前夕）"
	case ratio < 0.85:
		return "高潮决战期（核心冲突爆发，命运转折）"
	case ratio < 0.95:
		return "收束期（余波平息，伏笔揭晓）"
	default:
		return "终章（主题升华，情感落幕）"
	}
}

// ── Context building ──────────────────────────────────────────────────────────

// buildRichPrevContext is the excerpt-based fallback context (no summaryRepo).
func buildRichPrevContext(recent []recentChapterInfo) string {
	if len(recent) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【前文已改写内容摘要（保持叙事连贯，角色状态以此为准）】\n")
	for _, r := range recent {
		sb.WriteString(fmt.Sprintf("第%d章 开头：%s\n      末尾：%s\n", r.ChapterNo, r.Opening, r.Closing))
	}
	return sb.String()
}

// buildRecentSummaryContext uses AI-generated chapter summaries as rolling context.
func (s *RewriteService) buildRecentSummaryContext(projectID uint, currentChapterNo int) string {
	if s.summaryRepo == nil {
		return ""
	}
	summaries, err := s.summaryRepo.GetRecentByProject(projectID, currentChapterNo, 3)
	if err != nil || len(summaries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【前文语义摘要（保持叙事连贯，角色状态以此为准）】\n")
	for _, sum := range summaries {
		sb.WriteString(fmt.Sprintf("第%d章：%s\n", sum.ChapterNo, sum.Summary))
		if sum.CharStateSnap != "" && sum.CharStateSnap != "null" {
			var charState map[string]string
			if json.Unmarshal([]byte(sum.CharStateSnap), &charState) == nil && len(charState) > 0 {
				sb.WriteString("  └ 章末人物状态：")
				for char, state := range charState {
					sb.WriteString(fmt.Sprintf("%s(%s) ", char, state))
				}
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// buildPrevContext returns the best available previous-chapter context:
// prefers AI summaries, falls back to excerpt-based context.
func (s *RewriteService) buildPrevContext(projectID uint, currentChapterNo int, fallback []recentChapterInfo) string {
	if ctx := s.buildRecentSummaryContext(projectID, currentChapterNo); ctx != "" {
		return ctx
	}
	return buildRichPrevContext(fallback)
}

// buildContinuityContext queries the ContinuityIndex and returns an entity replacement table
// containing only entries whose original key appears in the given chapter content.
func (s *RewriteService) buildContinuityContext(projectID uint, chapterContent string) string {
	if s.continuityRepo == nil {
		return ""
	}
	entries, err := s.continuityRepo.GetByProject(projectID)
	if err != nil || len(entries) == 0 {
		return ""
	}

	var chars, locs, props, others []string
	for _, e := range entries {
		if e.EntityKey == "" || !strings.Contains(chapterContent, e.EntityKey) {
			continue
		}
		entry := fmt.Sprintf("%s→%s", e.EntityKey, e.NewName)
		switch e.EntityType {
		case "char":
			chars = append(chars, entry)
		case "location":
			locs = append(locs, entry)
		case "prop":
			props = append(props, entry)
		default:
			others = append(others, entry)
		}
	}

	if len(chars)+len(locs)+len(props)+len(others) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("【已确定的实体替换表（本章涉及的部分，务必严格遵守，不得偏离）】\n")
	if len(chars) > 0 {
		sb.WriteString("- 人物：" + strings.Join(chars, " | ") + "\n")
	}
	if len(locs) > 0 {
		sb.WriteString("- 地点：" + strings.Join(locs, " | ") + "\n")
	}
	if len(props) > 0 {
		sb.WriteString("- 道具：" + strings.Join(props, " | ") + "\n")
	}
	if len(others) > 0 {
		sb.WriteString("- 其他：" + strings.Join(others, " | ") + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// seedContinuityIndex pre-populates the ContinuityIndex from the Bible's CharNames and PropsTransform.
func (s *RewriteService) seedContinuityIndex(projectID uint, bible *model.RewriteBible) {
	if s.continuityRepo == nil {
		return
	}
	var entries []*model.RewriteContinuityIndex
	if bible.NewCharNames != "" && bible.NewCharNames != "null" {
		var nameMap map[string]string
		if json.Unmarshal([]byte(bible.NewCharNames), &nameMap) == nil {
			for orig, newName := range nameMap {
				if orig != "" && newName != "" {
					entries = append(entries, &model.RewriteContinuityIndex{
						ProjectID: projectID, EntityKey: orig, EntityType: "char",
						NewName: newName, FirstSeen: 0,
					})
				}
			}
		}
	}
	if bible.PropsTransform != "" && bible.PropsTransform != "null" {
		var propsMap map[string]string
		if json.Unmarshal([]byte(bible.PropsTransform), &propsMap) == nil {
			for orig, newProp := range propsMap {
				if orig != "" && newProp != "" {
					entries = append(entries, &model.RewriteContinuityIndex{
						ProjectID: projectID, EntityKey: orig, EntityType: "prop",
						NewName: newProp, FirstSeen: 0,
					})
				}
			}
		}
	}
	if len(entries) > 0 {
		if err := s.continuityRepo.BatchUpsert(entries); err != nil {
			logger.Printf("[Rewrite] seedContinuityIndex: %v", err)
		}
	}
}

// ── Similarity metrics ─────────────────────────────────────────────────────────

func splitSentences(text string) []string {
	var result []string
	var buf strings.Builder
	for _, r := range text {
		buf.WriteRune(r)
		if r == '。' || r == '！' || r == '？' || r == '\n' {
			if s := strings.TrimSpace(buf.String()); s != "" {
				result = append(result, s)
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		result = append(result, s)
	}
	return result
}

func calculateStructuralSimilarity(original, rewritten string) float64 {
	origSents := splitSentences(original)
	rewSents := splitSentences(rewritten)
	if len(origSents) == 0 || len(rewSents) == 0 {
		return 0
	}
	origFP := make(map[string]bool, len(origSents)*10)
	for _, s := range origSents {
		r := []rune(s)
		for i := 0; i+4 <= len(r); i++ {
			origFP[string(r[i:i+4])] = true
		}
	}
	matches := 0
	for _, s := range rewSents {
		r := []rune(s)
		for i := 0; i+4 <= len(r); i++ {
			if origFP[string(r[i:i+4])] {
				matches++
				break
			}
		}
	}
	denom := len(origSents)
	if len(rewSents) > denom {
		denom = len(rewSents)
	}
	return float64(matches) / float64(denom)
}

func calculateLexicalSimilarity(original, rewritten string) float64 {
	origRunes := []rune(original)
	rewRunes := []rune(rewritten)
	if len(origRunes) == 0 || len(rewRunes) == 0 {
		return 0
	}
	origBigrams := make(map[string]int)
	for i := 0; i < len(origRunes)-1; i++ {
		origBigrams[string(origRunes[i:i+2])]++
	}
	matches := 0
	for i := 0; i < len(rewRunes)-1; i++ {
		bg := string(rewRunes[i : i+2])
		if origBigrams[bg] > 0 {
			matches++
			origBigrams[bg]--
		}
	}
	total := len(origRunes) - 1
	if total <= 0 {
		return 0
	}
	return math.Min(float64(matches)/float64(total), 1.0)
}

// ── Quality evaluation ─────────────────────────────────────────────────────────

// checkConsistency scans the rewritten text for original names / forbidden phrases that
// must NOT appear. Returns a list of violation descriptions.
func checkConsistency(rewritten string, bible *model.RewriteBible) []string {
	var issues []string
	if bible.NewCharNames != "" && bible.NewCharNames != "null" {
		var nameMap map[string]string
		if err := json.Unmarshal([]byte(bible.NewCharNames), &nameMap); err == nil {
			for origName := range nameMap {
				if origName != "" && strings.Contains(rewritten, origName) {
					issues = append(issues, fmt.Sprintf("原著角色名残留：「%s」", origName))
				}
			}
		}
	}
	for _, phrase := range parseForbiddenPhrases(bible) {
		if phrase != "" && len([]rune(phrase)) >= 2 && strings.Contains(rewritten, phrase) {
			issues = append(issues, fmt.Sprintf("禁止短语残留：「%s」", phrase))
		}
	}
	return issues
}

// calculateLevelAwareQuality scores a rewrite result 0-100.
// The similarity component rewards results that land within the level's target range.
func calculateLevelAwareQuality(lexSim, structSim float64, origLen, rewLen int, issues []string, cfg rewriteLevelConfig) float64 {
	// Similarity score (0-50): ideal when within [TargetLexSimLow, TargetLexSimHigh]
	var simScore float64
	switch {
	case lexSim >= cfg.TargetLexSimLow && lexSim <= cfg.TargetLexSimHigh:
		simScore = 50
	case lexSim < cfg.TargetLexSimLow:
		if cfg.TargetLexSimLow > 0 {
			simScore = 50 * (lexSim / cfg.TargetLexSimLow)
		} else {
			simScore = 50
		}
	default: // too similar
		span := 1.0 - cfg.TargetLexSimHigh
		if span > 0 {
			simScore = 50 * ((1.0 - lexSim) / span)
		}
	}

	// Word count score (0-30): penalise when outside the level-specific target range.
	// Use targetWordRange so Level 4-5 rewrites at 60-200% of original still score well.
	var ratioScore float64
	if origLen > 0 {
		ratio := float64(rewLen) / float64(origLen)
		minW, maxW := targetWordRange(origLen, cfg.Level)
		minRatio := float64(minW) / float64(origLen)
		maxRatio := float64(maxW) / float64(origLen)
		switch {
		case ratio >= minRatio && ratio <= maxRatio:
			ratioScore = 30
		case ratio < minRatio && minRatio > 0:
			ratioScore = 30 * (ratio / minRatio)
		case ratio > maxRatio:
			// Allow up to 50% above max before zeroing
			excess := maxRatio * 1.5
			if ratio <= excess {
				ratioScore = 30 * (excess - ratio) / (excess - maxRatio)
			}
		}
	}

	// Consistency score (0-20): deduct 5 per issue, capped at 20
	consistencyScore := math.Max(0, 20-float64(len(issues))*5)

	score := simScore + ratioScore + consistencyScore
	return math.Round(math.Min(math.Max(score, 0), 100)*10) / 10
}

// ── CRUD ──────────────────────────────────────────────────────────────────────

func (s *RewriteService) CreateProject(tenantID, novelID uint, name string, level int) (*model.RewriteProject, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil || novel.TenantID != tenantID {
		return nil, fmt.Errorf("novel not found")
	}
	project := &model.RewriteProject{
		TenantID: tenantID,
		NovelID:  novelID,
		Name:     name,
		Level:    level,
		Status:   "pending",
	}
	if err := s.projectRepo.Create(project); err != nil {
		return nil, err
	}
	return project, nil
}

func (s *RewriteService) ListProjects(tenantID uint, page, pageSize int) ([]*model.RewriteProject, int64, error) {
	return s.projectRepo.ListByTenant(tenantID, page, pageSize)
}

func (s *RewriteService) GetProject(id uint) (*model.RewriteProject, error) {
	return s.projectRepo.GetByID(id)
}

func (s *RewriteService) DeleteProject(id uint) error {
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", id, TaskTypeRewriteAnalysis)
		s.taskSvc.CancelActiveByEntity("rewrite_project", id, TaskTypeRewriteChapters)
	}
	// Cascade-delete all derived data to avoid orphaned records
	s.analysisRepo.DeleteByProjectID(id)
	s.bibleRepo.DeleteByProjectID(id)
	s.chapterTaskRepo.DeleteByProjectID(id)
	if s.continuityRepo != nil {
		s.continuityRepo.DeleteByProject(id)
	}
	if s.summaryRepo != nil {
		s.summaryRepo.DeleteByProject(id)
	}
	return s.projectRepo.Delete(id)
}

// ── Phase 0+1: Analysis & Bible ───────────────────────────────────────────────

func (s *RewriteService) StartAnalysis(tenantID, projectID uint) (string, error) {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return "", err
	}
	// Guard: refuse to re-run analysis on a project that already has chapter rewrite data,
	// since generateBible deletes all ChapterRewriteTask rows before recreating them.
	switch project.Status {
	case "pending", "failed":
		// allowed: either no prior run or prior run failed before chapter data existed
	default:
		return "", fmt.Errorf("cannot re-run analysis on a project in status %q; delete and recreate the project to start fresh", project.Status)
	}
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteAnalysis)
	}
	task, err := s.taskSvc.Create(tenantID, TaskTypeRewriteAnalysis,
		"文学分析 & 改写圣经生成", "rewrite_project", projectID)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	go func(taskID string) {
		ctx, cancel := context.WithCancel(context.Background())
		s.taskSvc.RegisterCancel(taskID, cancel)
		defer s.taskSvc.DeregisterCancel(taskID)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(projectID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runAnalysis(ctx, taskID, project); err != nil {
			s.taskSvc.Fail(taskID, err.Error())
			s.projectRepo.UpdateStatus(projectID, "failed", err.Error())
			return
		}
		s.taskSvc.Complete(taskID, map[string]interface{}{
			"project_id": projectID,
			"status":     "bible_ready",
		})
	}(task.TaskID)

	return task.TaskID, nil
}

// ResumeAnalysis resumes an orphaned rewrite_analysis task reusing its existing task ID.
func (s *RewriteService) ResumeAnalysis(t *model.AsyncTask) {
	project, err := s.projectRepo.GetByID(t.EntityID)
	if err != nil {
		_ = s.taskSvc.Fail(t.TaskID, "project not found: "+err.Error())
		return
	}
	go func(taskID string) {
		ctx, cancel := context.WithCancel(context.Background())
		s.taskSvc.RegisterCancel(taskID, cancel)
		defer s.taskSvc.DeregisterCancel(taskID)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(project.ID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runAnalysis(ctx, taskID, project); err != nil {
			s.taskSvc.Fail(taskID, err.Error())
			s.projectRepo.UpdateStatus(project.ID, "failed", err.Error())
			return
		}
		s.taskSvc.Complete(taskID, map[string]interface{}{
			"project_id": t.EntityID,
			"status":     "bible_ready",
		})
	}(t.TaskID)
}

func (s *RewriteService) runAnalysis(ctx context.Context, taskID string, project *model.RewriteProject) error {
	s.projectRepo.UpdateStatus(project.ID, "analyzing", "")

	novel, err := s.novelRepo.GetByID(project.NovelID)
	if err != nil {
		return fmt.Errorf("get novel: %w", err)
	}
	chapters, err := s.chapterRepo.ListByNovelWithContent(project.NovelID)
	if err != nil {
		return fmt.Errorf("get chapters: %w", err)
	}

	sampled := stratifiedSample(chapters, 7)
	var sampleContent strings.Builder
	for _, ch := range sampled {
		sampleContent.WriteString(ch.Content)
		sampleContent.WriteString("\n\n---\n\n")
	}

	prompt, err := renderRewriteTemplate("rewrite_analyze", map[string]interface{}{
		"Title":   novel.Title,
		"Content": sampleContent.String(),
	})
	if err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("task cancelled")
	}

	result, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled")
		}
		return fmt.Errorf("ai generate: %w", err)
	}

	s.taskSvc.UpdateProgress(taskID, 40)

	jsonStr := extractJSONObject(result)
	var analysisData map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &analysisData); err != nil {
		return fmt.Errorf("parse analysis: %w", err)
	}

	toJSON := func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	analysis := &model.LiteraryAnalysis{
		ProjectID:          project.ID,
		VoiceFingerprint:   toJSON(analysisData["voice_fingerprint"]),
		SceneArchitecture:  toJSON(analysisData["scene_architecture"]),
		CharacterPsych:     toJSON(analysisData["character_psychology"]),
		ThemeCore:          toJSON(analysisData["theme_core"]),
		WorldLogic:         toJSON(analysisData["world_logic"]),
		HighRiskMarkers:    toJSON(analysisData["high_risk_markers"]),
		RhythmPattern:      toJSON(analysisData["rhythm_pattern"]),
		ImagerySystem:      toJSON(analysisData["imagery_system"]),
		InterChapterHooks:  toJSON(analysisData["inter_chapter_hooks"]),
	}
	if err := s.analysisRepo.Create(analysis); err != nil {
		return fmt.Errorf("save analysis: %w", err)
	}

	total, _ := s.chapterRepo.CountByNovel(project.NovelID)
	s.projectRepo.UpdateTotalChapters(project.ID, int(total))

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("task cancelled")
	}
	s.taskSvc.UpdateProgress(taskID, 50)
	return s.generateBible(ctx, taskID, project, analysis, novel)
}

func (s *RewriteService) generateBible(ctx context.Context, taskID string, project *model.RewriteProject, analysis *model.LiteraryAnalysis, novel *model.Novel) error {
	_ = novel

	toObj := func(raw string) interface{} {
		var v interface{}
		if json.Unmarshal([]byte(raw), &v) == nil {
			return v
		}
		return raw
	}
	analysisJSON, _ := json.Marshal(map[string]interface{}{
		"voice_fingerprint":    toObj(analysis.VoiceFingerprint),
		"scene_architecture":   toObj(analysis.SceneArchitecture),
		"character_psychology": toObj(analysis.CharacterPsych),
		"theme_core":           toObj(analysis.ThemeCore),
		"world_logic":          toObj(analysis.WorldLogic),
		"high_risk_markers":    toObj(analysis.HighRiskMarkers),
		"rhythm_pattern":       toObj(analysis.RhythmPattern),
		"imagery_system":       toObj(analysis.ImagerySystem),
		"inter_chapter_hooks":  toObj(analysis.InterChapterHooks),
	})

	levelDesc := map[int]string{
		1: "字词润色：保留90-95%情节，仅做词句级同义替换",
		2: "文学精炼：保留80-90%情节，用全新文学语言重写",
		3: "情节调整：保留60-75%情节，适度调整场景与对话",
		4: "结构重构：保留30-50%情节，重构世界观和角色",
		5: "精神蒸馏：只保留5-20%精神内核，全面重创",
	}

	prompt, err := renderRewriteTemplate("rewrite_bible_generate", map[string]interface{}{
		"Analysis": string(analysisJSON),
		"Level":    project.Level,
		"Goal":     levelDesc[project.Level],
	})
	if err != nil {
		return err
	}

	result, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled")
		}
		return err
	}

	s.taskSvc.UpdateProgress(taskID, 80)

	jsonStr := extractJSONObject(result)
	var bibleData map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &bibleData); err != nil {
		return fmt.Errorf("parse bible: %w", err)
	}

	toJSON := func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	worldName, _ := bibleData["new_world_name"].(string)
	namingStyle, _ := bibleData["naming_style"].(string)
	bible := &model.RewriteBible{
		ProjectID:          project.ID,
		NewWorldName:       worldName,
		NewCharNames:       toJSON(bibleData["new_char_names"]),
		NamingStyle:        namingStyle,
		PlotTransform:      toJSON(bibleData["plot_transform"]),
		PropsTransform:     toJSON(bibleData["props_transform"]),
		VoiceStrategy:      toJSON(bibleData["voice_strategy"]),
		StyleGuide:         toJSON(bibleData["style_guide"]),
		ForbiddenPhrases:   toJSON(bibleData["forbidden_phrases"]),
		ForbiddenDialogues: toJSON(bibleData["forbidden_dialogues"]),
		ImageryTransform:   toJSON(bibleData["imagery_transform"]),
	}
	if err := s.bibleRepo.Create(bible); err != nil {
		return err
	}

	// Pre-seed continuity index from Bible
	s.seedContinuityIndex(project.ID, bible)

	s.chapterTaskRepo.DeleteByProjectID(project.ID)

	chapters, err := s.chapterRepo.ListByNovelWithContent(project.NovelID)
	if err != nil {
		return err
	}
	for _, ch := range chapters {
		task := &model.ChapterRewriteTask{
			ProjectID:       project.ID,
			ChapterID:       ch.ID,
			ChapterNo:       ch.ChapterNo,
			Status:          "pending",
			OriginalContent: ch.Content,
		}
		if err := s.chapterTaskRepo.Create(task); err != nil {
			return fmt.Errorf("create chapter task for ch%d: %w", ch.ChapterNo, err)
		}
	}

	return s.projectRepo.UpdateStatus(project.ID, "bible_ready", "")
}

// ── Phase 2: Chapter Rewriting ────────────────────────────────────────────────

func (s *RewriteService) StartRewriting(tenantID, projectID uint) (string, error) {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return "", err
	}
	if project.Status == "failed" {
		// Allow retry only when the bible already exists (analysis completed before failure)
		if _, err := s.bibleRepo.GetByProjectID(projectID); err != nil {
			return "", fmt.Errorf("bible not found, please re-run analysis first: %w", err)
		}
	} else if project.Status != "bible_ready" {
		return "", fmt.Errorf("project must be in bible_ready status, got: %s", project.Status)
	}

	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteChapters)
	}

	// P0 fix: reset any chapters stuck in "rewriting" state from a previous interrupted run
	s.chapterTaskRepo.ResetStaleRewriting(projectID)

	task, err := s.taskSvc.Create(tenantID, TaskTypeRewriteChapters,
		"章节改写", "rewrite_project", projectID)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	go func(taskID string) {
		ctx, cancel := context.WithCancel(context.Background())
		s.taskSvc.RegisterCancel(taskID, cancel)
		defer s.taskSvc.DeregisterCancel(taskID)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(projectID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runRewriting(ctx, taskID, project); err != nil {
			s.taskSvc.Fail(taskID, err.Error())
			s.projectRepo.UpdateStatus(projectID, "failed", err.Error())
			return
		}
		s.taskSvc.Complete(taskID, map[string]interface{}{
			"project_id": projectID,
			"status":     "completed",
		})
	}(task.TaskID)

	return task.TaskID, nil
}

// ResumeRewriting resumes an orphaned rewrite_chapters task reusing its existing task ID.
func (s *RewriteService) ResumeRewriting(t *model.AsyncTask) {
	project, err := s.projectRepo.GetByID(t.EntityID)
	if err != nil {
		_ = s.taskSvc.Fail(t.TaskID, "project not found: "+err.Error())
		return
	}
	// Reset chapters stuck in "rewriting" from the interrupted run.
	s.chapterTaskRepo.ResetStaleRewriting(t.EntityID)
	go func(taskID string) {
		ctx, cancel := context.WithCancel(context.Background())
		s.taskSvc.RegisterCancel(taskID, cancel)
		defer s.taskSvc.DeregisterCancel(taskID)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(project.ID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runRewriting(ctx, taskID, project); err != nil {
			s.taskSvc.Fail(taskID, err.Error())
			s.projectRepo.UpdateStatus(project.ID, "failed", err.Error())
			return
		}
		s.taskSvc.Complete(taskID, map[string]interface{}{
			"project_id": t.EntityID,
			"status":     "completed",
		})
	}(t.TaskID)
}

func (s *RewriteService) runRewriting(ctx context.Context, taskID string, project *model.RewriteProject) error {
	s.projectRepo.UpdateStatus(project.ID, "rewriting", "")

	bible, err := s.bibleRepo.GetByProjectID(project.ID)
	if err != nil {
		return err
	}

	tasks, err := s.chapterTaskRepo.ListByProject(project.ID)
	if err != nil {
		return err
	}

	done := 0
	failed := 0
	total := len(tasks)
	// Excerpt-based fallback context (used when summaryRepo is unavailable or has no data yet)
	var recentContext []recentChapterInfo

	for _, task := range tasks {
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled")
		}
		if task.Status == "completed" {
			done++
			s.updateRewriteProgress(taskID, project.ID, total)
			continue
		}

		// ── Three-layer context injection ──────────────────────────────
		prevContext := s.buildPrevContext(project.ID, task.ChapterNo, recentContext)
		continuityCtx := s.buildContinuityContext(project.ID, task.OriginalContent)
		arcStage := emotionalArcStage(task.ChapterNo, project.TotalChapters)

		att, err := s.rewriteChapterWithRetry(ctx, project, bible, task, prevContext, continuityCtx, arcStage)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("task cancelled")
			}
			failed++
			// Chapter marked failed; continue to next chapter
		} else {
			done++
			finalContent := att.Content

			// Quality Gate 2: consistency check (rule-based, no AI)
			issues := checkConsistency(finalContent, bible)
			issuesJSON := "[]"
			if len(issues) > 0 {
				if b, e := json.Marshal(issues); e == nil {
					issuesJSON = string(b)
				}
				logger.Printf("[Rewrite] chapter %d consistency issues: %v", task.ChapterNo, issues)
			}

			// De-AI pass for Level 2+ (non-fatal)
			deaiApplied := false
			if project.Level >= 2 {
				if polished := s.deAIPass(ctx, project, finalContent, bible); polished != "" {
					finalContent = polished
					deaiApplied = true
					logger.Printf("[Rewrite] deAI pass applied for chapter %d", task.ChapterNo)
				}
			}

			// Quality Gate 3: heuristic quality score (no AI)
			origLen := len([]rune(task.OriginalContent))
			rewLen := len([]rune(finalContent))
			cfg := rewriteLevelConfigs[project.Level]
			qualityScore := calculateLevelAwareQuality(att.LexSim, att.StructSim, origLen, rewLen, issues, cfg)
			logger.Printf("[Rewrite] chapter %d quality=%.1f deai=%v passed=%v", task.ChapterNo, qualityScore, deaiApplied, att.Passed)

			// Persist final content + metadata
			if err := s.chapterTaskRepo.UpdatePostProcess(task.ID, finalContent, qualityScore, deaiApplied, issuesJSON); err != nil {
				logger.Printf("[Rewrite] UpdatePostProcess ch%d: %v", task.ChapterNo, err)
			}

			// Async: generate chapter summary and update continuity index
			s.generateChapterSummaryAsync(project.TenantID, project.NovelID, project.ID, task.ID, task.ChapterNo, finalContent)

			// Update excerpt-based fallback context (Fix 5: dynamic lengths for short chapters)
			runes := []rune(finalContent)
			n := len(runes)
			openingLen := 250
			if n < 500 {
				openingLen = n / 2
			}
			closingLen := 200
			if n-openingLen < 200 {
				closingLen = n - openingLen
			}
			if closingLen < 0 {
				closingLen = 0
			}
			opening := string(runes[:openingLen])
			closing := ""
			if closingLen > 0 {
				closing = string(runes[n-closingLen:])
			}
			recentContext = append(recentContext, recentChapterInfo{
				ChapterNo: task.ChapterNo, Opening: opening, Closing: closing,
			})
			if len(recentContext) > 3 {
				recentContext = recentContext[1:]
			}
		}
		s.updateRewriteProgress(taskID, project.ID, total)
	}

	if done == 0 && total > 0 {
		return s.projectRepo.UpdateStatus(project.ID, "failed", "所有章节改写均失败")
	}
	if failed > 0 {
		msg := fmt.Sprintf("%d/%d 章节改写失败，其余章节已完成", failed, total)
		return s.projectRepo.UpdateStatus(project.ID, "partial_failed", msg)
	}
	return s.projectRepo.UpdateStatus(project.ID, "completed", "")
}

// rewriteChapterWithRetry manages the full retry loop for a single chapter.
// It uses the new AttemptContent/AcceptAttempt flow to prevent DB data pollution.
func (s *RewriteService) rewriteChapterWithRetry(
	ctx context.Context,
	project *model.RewriteProject,
	bible *model.RewriteBible,
	task *model.ChapterRewriteTask,
	prevContext, continuityCtx, arcStage string,
) (*rewriteAttempt, error) {
	cfg, ok := rewriteLevelConfigs[project.Level]
	if !ok {
		cfg = rewriteLevelConfigs[2]
	}

	var lastAttempt *rewriteAttempt
	var lastAIErr error

	for attempt := 0; attempt <= maxChapterRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		hint := ""
		if attempt < len(retryHints) {
			hint = retryHints[attempt]
		}

		att, err := s.rewriteChapter(ctx, project, bible, task, prevContext, continuityCtx, arcStage, hint, attempt+1, cfg)
		if err != nil {
			// AI call or template failure — no content generated
			lastAIErr = err
			logger.Printf("[Rewrite] chapter %d attempt %d/%d AI error: %v",
				task.ChapterNo, attempt+1, maxChapterRetries+1, err)
			if attempt < maxChapterRetries {
				s.chapterTaskRepo.UpdateStatus(task.ID, "pending", "")
			}
			continue
		}

		// Save to AttemptContent only (P0: not RewrittenContent)
		if saveErr := s.chapterTaskRepo.SaveAttempt(task.ID, att.Content); saveErr != nil {
			logger.Printf("[Rewrite] SaveAttempt ch%d: %v", task.ChapterNo, saveErr)
		}

		lastAttempt = att

		// Only retry when 改写不足 (too similar) and retries remain
		if att.TooSimilar && attempt < maxChapterRetries {
			logger.Printf("[Rewrite] chapter %d attempt %d/%d 改写不足 lexSim=%.3f > %.2f",
				task.ChapterNo, attempt+1, maxChapterRetries+1, att.LexSim, cfg.TargetLexSimHigh)
			s.chapterTaskRepo.UpdateStatus(task.ID, "pending", "")
			continue
		}

		// Accept: promote AttemptContent → RewrittenContent
		if err := s.chapterTaskRepo.AcceptAttempt(task.ID, att.LexSim, att.StructSim, att.SemanticSim, att.Passed); err != nil {
			return nil, err
		}
		// Sync accepted rewrite content back to the source chapter
		if att.Content != "" {
			if err := s.chapterRepo.UpdateContent(task.ChapterID, att.Content); err != nil {
				logger.Printf("AcceptAttempt: sync chapter content failed: %v", err)
				// non-fatal: task is accepted, chapter update is best-effort
			}
		}
		return att, nil
	}

	// Retry loop exhausted
	if lastAttempt != nil {
		// Degraded accept: last attempt had content but failed quality gate
		logger.Printf("[Rewrite] chapter %d exhausted retries, accepting degraded result (passed=%v)", task.ChapterNo, lastAttempt.Passed)
		if err := s.chapterTaskRepo.AcceptAttempt(task.ID, lastAttempt.LexSim, lastAttempt.StructSim, lastAttempt.SemanticSim, false); err != nil {
			logger.Printf("[Rewrite] AcceptAttempt (degraded) ch%d: %v", task.ChapterNo, err)
		}
		// Sync accepted rewrite content back to the source chapter
		if lastAttempt.Content != "" {
			if err := s.chapterRepo.UpdateContent(task.ChapterID, lastAttempt.Content); err != nil {
				logger.Printf("AcceptAttempt: sync chapter content failed: %v", err)
				// non-fatal: task is accepted, chapter update is best-effort
			}
		}
		return lastAttempt, nil
	}

	// All attempts had AI failures — mark chapter as failed
	errMsg := "all attempts failed"
	if lastAIErr != nil {
		errMsg = lastAIErr.Error()
	}
	s.chapterTaskRepo.UpdateStatus(task.ID, "failed", errMsg)
	return nil, fmt.Errorf("chapter %d: %s", task.ChapterNo, errMsg)
}

// rewriteChapter performs a single AI generation attempt.
// It does NOT write to the DB — the caller manages DB state via SaveAttempt/AcceptAttempt.
func (s *RewriteService) rewriteChapter(
	ctx context.Context,
	project *model.RewriteProject,
	bible *model.RewriteBible,
	task *model.ChapterRewriteTask,
	prevContext, continuityCtx, arcStage, retryHint string,
	attemptNo int,
	cfg rewriteLevelConfig,
) (*rewriteAttempt, error) {
	s.chapterTaskRepo.UpdateStatus(task.ID, "rewriting", "")

	origLen := len([]rune(task.OriginalContent))
	minWords, maxWords := targetWordRange(origLen, project.Level)
	coreElements := extractCoreElements(task.OriginalContent, project.Level)

	templateData := map[string]interface{}{
		"WorldName":        bible.NewWorldName,
		"NamingStyle":      bible.NamingStyle,
		"CharNames":        formatCharNamesForPrompt(bible.NewCharNames),
		"PropsTransform":   formatPropsForPrompt(bible.PropsTransform),
		"PlotTransform":    bible.PlotTransform,
		"VoiceStrategy":    bible.VoiceStrategy,
		"StyleGuide":       bible.StyleGuide,
		"ImageryTransform": bible.ImageryTransform,
		"ForbiddenBlock":   formatForbiddenForPrompt(bible),
		"OriginalContent":  task.OriginalContent,
		"CoreElements":     coreElements,
		"LevelGoal":        cfg.Goal,
		"RetentionTarget":  cfg.RetentionTarget,
		"OrigWords":        origLen,
		"MinWords":         minWords,
		"MaxWords":         maxWords,
		"Level":            project.Level,
		"PrevContext":      prevContext,
		"ContinuityContext": continuityCtx,
		"ArcStage":         arcStage,
		"RetryHint":        retryHint,
		"AttemptNo":        attemptNo,
	}

	prompt, err := renderRewriteTemplate(cfg.Template, templateData)
	if err != nil {
		return nil, err
	}

	rewritten, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		return nil, err
	}

	lexSim := calculateLexicalSimilarity(task.OriginalContent, rewritten)
	structSim := calculateStructuralSimilarity(task.OriginalContent, rewritten)
	semanticSim := s.calculateSemanticLeakage(rewritten, project.ID)
	passed := lexSim >= cfg.TargetLexSimLow && lexSim <= cfg.TargetLexSimHigh
	tooSimilar := lexSim > cfg.TargetLexSimHigh

	return &rewriteAttempt{
		Content:     rewritten,
		LexSim:      lexSim,
		StructSim:   structSim,
		SemanticSim: semanticSim,
		Passed:      passed,
		TooSimilar:  tooSimilar,
	}, nil
}

// calculateSemanticLeakage measures how many original entity names still appear
// in the rewritten text. Returns a ratio 0.0–1.0 (lower = fewer original entities leaked).
func (s *RewriteService) calculateSemanticLeakage(rewritten string, projectID uint) float64 {
	if s.continuityRepo == nil {
		return 0
	}
	entries, err := s.continuityRepo.GetByProject(projectID)
	if err != nil {
		logger.Printf("[Rewrite] calculateSemanticLeakage: GetByProject(project=%d): %v — returning 0", projectID, err)
		return 0
	}
	if len(entries) == 0 {
		logger.Printf("[Rewrite] calculateSemanticLeakage: no continuity entries for project %d (continuityRepo not populated?)", projectID)
		return 0
	}
	leaked := 0
	for _, e := range entries {
		if e.EntityKey != "" && strings.Contains(rewritten, e.EntityKey) {
			leaked++
		}
	}
	return float64(leaked) / float64(len(entries))
}

// generateChapterSummaryAsync calls AI to produce a semantic summary after each accepted chapter.
// Runs in a background goroutine; failures are logged but do not block subsequent chapters.
func (s *RewriteService) generateChapterSummaryAsync(tenantID, novelID, projectID, taskID uint, chapterNo int, content string) {
	if s.summaryRepo == nil {
		return
	}
	go func() {
		defer func() { recover() }()
		genCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		prompt, err := renderRewriteTemplate("rewrite_chapter_summary", map[string]interface{}{
			"ChapterNo": chapterNo,
			"Content":   content,
		})
		if err != nil {
			logger.Printf("[Rewrite] summary template ch%d: %v", chapterNo, err)
			return
		}

		var result string
		var genErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(5 * time.Second)
			}
			result, genErr = s.aiSvc.GenerateWithProviderCtx(genCtx, tenantID, novelID, "chapter_gen", prompt, "")
			if genErr == nil && result != "" {
				break
			}
			logger.Printf("[Rewrite] summary AI ch%d attempt %d failed: %v", chapterNo, attempt+1, genErr)
		}
		if genErr != nil || result == "" {
			logger.Printf("[Rewrite] summary AI ch%d: all attempts failed: %v", chapterNo, genErr)
			return
		}

		jsonStr := extractJSONObject(result)
		var meta struct {
			Summary     string            `json:"summary"`
			CharState   map[string]string `json:"char_state"`
			NewEntities []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"new_entities"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
			logger.Printf("[Rewrite] summary parse ch%d: %v", chapterNo, err)
			return
		}

		charStateJSON, _ := json.Marshal(meta.CharState)
		if err := s.summaryRepo.Upsert(projectID, chapterNo, meta.Summary, string(charStateJSON)); err != nil {
			logger.Printf("[Rewrite] summaryRepo.Upsert ch%d: %v", chapterNo, err)
		}

		// Update continuity index with newly confirmed entity names
		if s.continuityRepo != nil && len(meta.NewEntities) > 0 {
			for _, e := range meta.NewEntities {
				if e.Key != "" && e.Name != "" {
					if err := s.continuityRepo.Upsert(projectID, e.Key, e.Type, e.Name, chapterNo); err != nil {
						logger.Printf("[Rewrite] continuityRepo.Upsert ch%d %s: %v", chapterNo, e.Key, err)
					}
				}
			}
		}
		if err := s.chapterTaskRepo.MarkSummaryWritten(taskID); err != nil {
			logger.Printf("[Rewrite] MarkSummaryWritten ch%d: %v", chapterNo, err)
		}
		logger.Printf("[Rewrite] summary written ch%d", chapterNo)
	}()
}

// deAIPass calls the rewrite_deai template to remove AI writing patterns.
// Non-fatal: on any error it logs and returns "" so the caller keeps the original content.
func (s *RewriteService) deAIPass(ctx context.Context, project *model.RewriteProject, content string, bible *model.RewriteBible) string {
	if ctx.Err() != nil {
		return ""
	}
	prompt, err := renderRewriteTemplate("rewrite_deai", map[string]interface{}{
		"Content":            content,
		"StyleGuide":         bible.StyleGuide,
		"VoiceStrategy":      bible.VoiceStrategy,
		"VocabularyRegister": extractVocabRegister(bible.StyleGuide),
	})
	if err != nil {
		logger.Printf("[Rewrite] deAIPass render: %v", err)
		return ""
	}
	result, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		logger.Printf("[Rewrite] deAIPass generate: %v", err)
		return ""
	}
	if strings.TrimSpace(result) == "" {
		return ""
	}
	return result
}

func (s *RewriteService) updateRewriteProgress(taskID string, projectID uint, total int) {
	// Query DB for actual completed count — more accurate than an in-memory counter
	// that can drift if the task is resumed or chapters are skipped.
	done, err := s.chapterTaskRepo.CountByProjectAndStatus(projectID, "completed")
	if err != nil {
		logger.Printf("[Rewrite] updateRewriteProgress: count failed: %v", err)
		return
	}
	pct := 0
	if total > 0 {
		pct = int(done) * 100 / total
		if pct > 100 {
			pct = 100
		}
	}
	s.taskSvc.UpdateProgress(taskID, pct)
	s.projectRepo.UpdateProgress(projectID, int(done), total)
}

// ── Bible editing ─────────────────────────────────────────────────────────────

type UpdateBibleRequest struct {
	NewWorldName       *string `json:"new_world_name,omitempty"`
	NamingStyle        *string `json:"naming_style,omitempty"`
	NewCharNames       *string `json:"new_char_names,omitempty"`
	PlotTransform      *string `json:"plot_transform,omitempty"`
	PropsTransform     *string `json:"props_transform,omitempty"`
	VoiceStrategy      *string `json:"voice_strategy,omitempty"`
	StyleGuide         *string `json:"style_guide,omitempty"`
	ForbiddenPhrases   *string `json:"forbidden_phrases,omitempty"`
	ForbiddenDialogues *string `json:"forbidden_dialogues,omitempty"`
	ImageryTransform   *string `json:"imagery_transform,omitempty"`
	// Legacy field kept for backward compat
	ForbiddenElems *string `json:"forbidden_elems,omitempty"`
}

func (s *RewriteService) UpdateBible(projectID uint, req UpdateBibleRequest) error {
	fields := map[string]interface{}{}
	if req.NewWorldName != nil {
		fields["new_world_name"] = *req.NewWorldName
	}
	if req.NamingStyle != nil {
		fields["naming_style"] = *req.NamingStyle
	}
	if req.NewCharNames != nil {
		fields["new_char_names"] = *req.NewCharNames
	}
	if req.PlotTransform != nil {
		fields["plot_transform"] = *req.PlotTransform
	}
	if req.PropsTransform != nil {
		fields["props_transform"] = *req.PropsTransform
	}
	if req.VoiceStrategy != nil {
		fields["voice_strategy"] = *req.VoiceStrategy
	}
	if req.StyleGuide != nil {
		fields["style_guide"] = *req.StyleGuide
	}
	if req.ForbiddenPhrases != nil {
		fields["forbidden_phrases"] = *req.ForbiddenPhrases
	}
	if req.ForbiddenDialogues != nil {
		fields["forbidden_dialogues"] = *req.ForbiddenDialogues
	}
	if req.ImageryTransform != nil {
		fields["imagery_transform"] = *req.ImageryTransform
	}
	if req.ForbiddenElems != nil {
		fields["forbidden_elems"] = *req.ForbiddenElems
	}
	if len(fields) == 0 {
		return nil
	}
	return s.bibleRepo.UpdateFields(projectID, fields)
}

// ── Accessors ─────────────────────────────────────────────────────────────────

func (s *RewriteService) GetBible(projectID uint) (*model.RewriteBible, error) {
	return s.bibleRepo.GetByProjectID(projectID)
}

func (s *RewriteService) GetAnalysis(projectID uint) (*model.LiteraryAnalysis, error) {
	return s.analysisRepo.GetByProjectID(projectID)
}

func (s *RewriteService) ListChapterTasks(projectID uint) ([]*model.ChapterRewriteTask, error) {
	return s.chapterTaskRepo.ListByProject(projectID)
}

func (s *RewriteService) GetChapterTask(taskID uint) (*model.ChapterRewriteTask, error) {
	return s.chapterTaskRepo.GetByID(taskID)
}

func (s *RewriteService) ApproveChapter(taskID uint) error {
	return s.chapterTaskRepo.UpdateStatus(taskID, "completed", "")
}

// CancelRewrite cancels active rewrite tasks for a project and marks it cancelled.
func (s *RewriteService) CancelRewrite(projectID uint) error {
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteAnalysis)
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteChapters)
	}
	return s.projectRepo.UpdateStatus(projectID, "cancelled", "user cancelled")
}

// ── Compliance Report ─────────────────────────────────────────────────────────

// ChapterComplianceItem is one row in the compliance report.
type ChapterComplianceItem struct {
	ChapterNo           int     `json:"chapter_no"`
	Passed              bool    `json:"passed"`
	LexicalSim          float64 `json:"lexical_sim"`
	StructuralSim       float64 `json:"structural_sim"`
	SemanticSim         float64 `json:"semantic_sim"`
	SemanticSimComputed bool    `json:"semantic_sim_computed"` // false means 0 = not computed, not perfect
	QualityScore        float64 `json:"quality_score"`
	Rating              string  `json:"rating"` // "green" | "yellow" | "red"
}

// ComplianceReport aggregates similarity and quality metrics across all chapters.
type ComplianceReport struct {
	ProjectID            uint                    `json:"project_id"`
	Level                int                     `json:"level"`
	TotalChapters        int                     `json:"total_chapters"`
	DoneChapters         int                     `json:"done_chapters"`
	PassedChapters       int                     `json:"passed_chapters"`
	AvgLexicalSim        float64                 `json:"avg_lexical_sim"`
	AvgStructuralSim     float64                 `json:"avg_structural_sim"`
	AvgSemanticSim       float64                 `json:"avg_semantic_sim"`
	SemanticSimComputed  bool                    `json:"semantic_sim_computed"` // true when any chapter has real semantic sim
	AvgQualityScore      float64                 `json:"avg_quality_score"`
	OverallRating        string                  `json:"overall_rating"`
	Chapters             []ChapterComplianceItem `json:"chapters"`
}

// chapterComplianceRating computes a compliance rating using level-specific thresholds.
// Fixed thresholds (0.20/0.35) are wrong for levels 1-3 where high lexical similarity is expected.
func chapterComplianceRating(passed bool, lexSim float64, level int) string {
	cfg, ok := rewriteLevelConfigs[level]
	if !ok {
		cfg = rewriteLevelConfigs[3]
	}
	if passed {
		// Within acceptable range: green if below midpoint, yellow if toward upper bound
		mid := (cfg.TargetLexSimLow + cfg.TargetLexSimHigh) / 2.0
		if lexSim <= mid {
			return "green"
		}
		return "yellow"
	}
	// Not passed: distinguish copyright risk (too similar) from quality gap (too different)
	if lexSim > cfg.TargetLexSimHigh {
		return "red" // still too similar to original — copyright risk
	}
	return "yellow" // too different from expected — quality concern, not copyright risk
}

// GetComplianceReport computes a compliance report from completed chapter tasks.
func (s *RewriteService) GetComplianceReport(projectID uint) (*ComplianceReport, error) {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.chapterTaskRepo.ListByProject(projectID)
	if err != nil {
		return nil, err
	}

	report := &ComplianceReport{
		ProjectID:     projectID,
		Level:         project.Level,
		TotalChapters: project.TotalChapters,
		Chapters:      make([]ChapterComplianceItem, 0, len(tasks)),
	}

	var sumLex, sumStruct, sumSemantic, sumQuality float64
	done := 0
	passed := 0
	anySemanticComputed := false

	for _, t := range tasks {
		if t.Status != "completed" {
			continue
		}
		done++
		if t.Passed {
			passed++
		}
		sumLex += t.LexicalSim
		sumStruct += t.StructuralSim
		semComputed := t.SemanticSim > 0
		if semComputed {
			sumSemantic += t.SemanticSim
			anySemanticComputed = true
		}
		sumQuality += t.QualityScore

		rating := chapterComplianceRating(t.Passed, t.LexicalSim, project.Level)
		report.Chapters = append(report.Chapters, ChapterComplianceItem{
			ChapterNo:           t.ChapterNo,
			Passed:              t.Passed,
			LexicalSim:          t.LexicalSim,
			StructuralSim:       t.StructuralSim,
			SemanticSim:         t.SemanticSim,
			SemanticSimComputed: semComputed,
			QualityScore:        t.QualityScore,
			Rating:              rating,
		})
	}

	report.DoneChapters = done
	report.PassedChapters = passed
	report.SemanticSimComputed = anySemanticComputed

	if done > 0 {
		report.AvgLexicalSim = math.Round(sumLex/float64(done)*1000) / 1000
		report.AvgStructuralSim = math.Round(sumStruct/float64(done)*1000) / 1000
		if anySemanticComputed {
			report.AvgSemanticSim = math.Round(sumSemantic/float64(done)*1000) / 1000
		}
		report.AvgQualityScore = math.Round(sumQuality/float64(done)*10) / 10
	}

	passRate := 0.0
	if done > 0 {
		passRate = float64(passed) / float64(done)
	}
	// Use level-aware threshold: passed_rate drives the overall rating since
	// "passed" already incorporates level-specific lexical similarity targets.
	cfg := rewriteLevelConfigs[project.Level]
	if _, ok := rewriteLevelConfigs[project.Level]; !ok {
		cfg = rewriteLevelConfigs[3]
	}
	switch {
	case passRate >= 0.8 && report.AvgLexicalSim <= cfg.TargetLexSimHigh:
		report.OverallRating = "green"
	case passRate < 0.6 || report.AvgLexicalSim > cfg.TargetLexSimHigh:
		report.OverallRating = "red"
	default:
		report.OverallRating = "yellow"
	}

	return report, nil
}
