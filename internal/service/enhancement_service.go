package service

import (
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// ============================================
// Timeline Service - 时间线管理
// ============================================

type TimelineService struct {
	chapterRepo interface {
		ListByNovel(novelID uint) ([]*model.Chapter, error)
		GetByID(id uint) (*model.Chapter, error)
	}
}

func NewTimelineService(chapterRepo interface {
	GetByID(id uint) (*model.Chapter, error)
	ListByNovel(novelID uint) ([]*model.Chapter, error)
}) *TimelineService {
	return &TimelineService{chapterRepo: chapterRepo}
}

// TimelineEvent 时间线事件
type TimelineEvent struct {
	ChapterNo   int    `json:"chapter_no"`
	Title       string `json:"title"`
	TimePoint   string `json:"time_point"` // 故事内时间
	DayOffset   int    `json:"day_offset"` // 相对于起始日的天数偏移
	Description string `json:"description"`
	Type        string `json:"type"` // plot/character/world
}

// Timeline 时间线
type Timeline struct {
	NovelID   uint               `json:"novel_id"`
	StartDate string             `json:"start_date"`
	EndDate   string             `json:"end_date"`
	Events    []*TimelineEvent   `json:"events"`
	Conflicts []TimelineConflict `json:"conflicts"`
}

// TimelineConflict 时间线冲突
type TimelineConflict struct {
	Event1      *TimelineEvent `json:"event1"`
	Event2      *TimelineEvent `json:"event2"`
	Type        string         `json:"type"` // overlap_gap_contradiction
	Description string         `json:"description"`
}

// BuildTimeline 构建时间线
func (s *TimelineService) BuildTimeline(novelID uint) (*Timeline, error) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}

	timeline := &Timeline{
		NovelID: novelID,
		Events:  make([]*TimelineEvent, 0),
	}

	dayOffset := 0
	for _, ch := range chapters {
		event := &TimelineEvent{
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			DayOffset: dayOffset,
		}

		// 从章节内容中提取时间信息
		timePoint, days := s.extractTimeInfo(ch.Content)
		if timePoint != "" {
			event.TimePoint = timePoint
		}
		if days > 0 {
			dayOffset += days
			event.DayOffset = dayOffset
		}

		timeline.Events = append(timeline.Events, event)
	}

	// 检测冲突
	timeline.Conflicts = s.detectConflicts(timeline)

	return timeline, nil
}

// extractTimeInfo 从内容中提取时间信息
func (s *TimelineService) extractTimeInfo(content string) (string, int) {
	// 简化实现：检测时间相关词汇
	dayIncrement := 0

	timeKeywords := map[string]int{
		"第二天": 1,
		"次日":  1,
		"几天后": 3,
		"数日后": 7,
		"一周后": 7,
		"一月后": 30,
	}

	for keyword, days := range timeKeywords {
		if strings.Contains(content, keyword) {
			dayIncrement = days
			return keyword, dayIncrement
		}
	}

	return "", 0
}

// detectConflicts 检测时间线冲突
func (s *TimelineService) detectConflicts(timeline *Timeline) []TimelineConflict {
	conflicts := make([]TimelineConflict, 0)

	// 检测时间倒退
	for i := 1; i < len(timeline.Events); i++ {
		if timeline.Events[i].DayOffset < timeline.Events[i-1].DayOffset {
			conflicts = append(conflicts, TimelineConflict{
				Event1: timeline.Events[i-1],
				Event2: timeline.Events[i],
				Type:   "time_regression",
				Description: fmt.Sprintf("时间倒退：从第%d章的第%d天回到第%d天",
					timeline.Events[i].ChapterNo,
					timeline.Events[i-1].DayOffset,
					timeline.Events[i].DayOffset),
			})
		}
	}

	return conflicts
}

// ============================================
// Character Arc Service - 角色弧光追踪
// ============================================

type CharacterArcService struct {
	charRepo interface {
		GetByID(id uint) (*model.Character, error)
		ListByNovel(novelID uint) ([]*model.Character, error)
	}
	snapshotRepo interface {
		Create(snapshot *model.CharacterStateSnapshot) error
		ListByCharacter(characterID uint) ([]*model.CharacterStateSnapshot, error)
	}
}

func NewCharacterArcService(charRepo interface {
	GetByID(id uint) (*model.Character, error)
	ListByNovel(novelID uint) ([]*model.Character, error)
}, snapshotRepo interface {
	Create(snapshot *model.CharacterStateSnapshot) error
	ListByCharacter(characterID uint) ([]*model.CharacterStateSnapshot, error)
}) *CharacterArcService {
	return &CharacterArcService{
		charRepo:     charRepo,
		snapshotRepo: snapshotRepo,
	}
}

// CharacterArc 角色弧光
type CharacterArc struct {
	CharacterID   uint                 `json:"character_id"`
	CharacterName string               `json:"character_name"`
	ArcType       string               `json:"arc_type"` // growth/fall/redemption
	Stages        []*CharacterArcStage `json:"stages"`
	CurrentStage  int                  `json:"current_stage"`
}

// CharacterArcStage 角色弧光阶段
type CharacterArcStage struct {
	ChapterNo     int    `json:"chapter_no"`
	Title         string `json:"title"`
	State         string `json:"state"` // 心态/状态描述
	PowerLevel    int    `json:"power_level"`
	Mood          string `json:"mood"`
	Relationships string `json:"relationships"`
	Note          string `json:"note"`
}

// GetCharacterArc 获取角色弧光
func (s *CharacterArcService) GetCharacterArc(novelID, characterID uint) (*CharacterArc, error) {
	char, err := s.charRepo.GetByID(characterID)
	if err != nil {
		return nil, err
	}
	if char.NovelID != novelID {
		return nil, fmt.Errorf("character does not belong to the specified novel")
	}

	snapshots, err := s.snapshotRepo.ListByCharacter(characterID)
	if err != nil {
		return nil, err
	}

	arc := &CharacterArc{
		CharacterID:   char.ID,
		CharacterName: char.Name,
		ArcType:       s.determineArcType(snapshots),
		Stages:        make([]*CharacterArcStage, 0),
	}

	for i, snap := range snapshots {
		stage := &CharacterArcStage{
			ChapterNo:  len(snapshots) - i,
			State:      snap.Motivation,
			PowerLevel: snap.PowerLevel,
			Mood:       snap.Mood,
		}
		arc.Stages = append(arc.Stages, stage)
	}

	arc.CurrentStage = len(arc.Stages)

	return arc, nil
}

// determineArcType 确定弧光类型
func (s *CharacterArcService) determineArcType(snapshots []*model.CharacterStateSnapshot) string {
	if len(snapshots) < 2 {
		return "growth"
	}

	// 简化：根据能力等级变化判断
	first := snapshots[0].PowerLevel
	last := float64(snapshots[len(snapshots)-1].PowerLevel)

	if last > (float64(first) * 1.5) {
		return "growth"
	} else if last < (float64(first) * 0.5) {
		return "fall"
	}
	return "flat"
}

// CreateSnapshot 创建角色状态快照
func (s *CharacterArcService) CreateSnapshot(chapterID uint, characterID uint, content string) error {
	// 简化实现
	snapshot := &model.CharacterStateSnapshot{
		CharacterID: characterID,
		ChapterID:   chapterID,
	}

	return s.snapshotRepo.Create(snapshot)
}

// ============================================
// Style Service - 风格控制
// ============================================

type StyleService struct{}

func NewStyleService(_ interface{}) *StyleService {
	return &StyleService{}
}

// StyleConfig 风格配置
type StyleConfig struct {
	// 叙事视角
	NarrativeVoice string `json:"narrative_voice"` // first_person/third_limited/third_omniscient

	// 叙事距离
	NarrativeDistance string `json:"narrative_distance"` // close/medium/distant

	// 情感温度
	EmotionalTone string `json:"emotional_tone"` // warm/neutral/cold

	// 句式复杂度
	SentenceComplexity string `json:"sentence_complexity"` // simple/moderate/complex

	// 对话比例
	DialogueRatio float64 `json:"dialogue_ratio"` // 0.2-0.5

	// 描写密度
	DescriptionDensity string `json:"description_density"` // minimal/moderate/rich
}

// NarrativeVoicePrompts 叙事视角提示词
var NarrativeVoicePrompts = map[string]string{
	"first_person":     "使用第一人称「我」叙述，让读者代入主角视角",
	"third_limited":    "使用第三人称有限视角，聚焦于主角的所见所感",
	"third_omniscient": "使用全知视角，可以自由描述任何角色的内心和背景",
}

// NarrativeDistancePrompts 叙事距离提示词
var NarrativeDistancePrompts = map[string]string{
	"close":   "近距离描写，深入角色内心，详细展现心理活动",
	"medium":  "中等距离，平衡外在描写和内心活动",
	"distant": "远距离描写，侧重于事件和行动，描写较为简洁",
}

// EmotionalTonePrompts 情感温度提示词
var EmotionalTonePrompts = map[string]string{
	"warm":    "温暖的情感基调，充满人情味和温情",
	"neutral": "中性的情感基调，客观冷静的叙述",
	"cold":    "冷淡的情感基调，克制内敛的描写",
}

// SentenceComplexityPrompts 句式复杂度提示词
var SentenceComplexityPrompts = map[string]string{
	"simple":   "使用短句为主，简洁有力，适合快节奏场景",
	"moderate": "长短句结合，节奏感强",
	"complex":  "使用复合句，长句较多，适合细腻描写",
}

// DescriptionDensityPrompts 描写密度提示词
var DescriptionDensityPrompts = map[string]string{
	"minimal":  "简洁描写，只保留必要信息，节奏紧凑",
	"moderate": "适度描写，有细节但不冗长",
	"rich":     "细腻丰富的描写，环境、动作、心理全方位展现",
}

// buildStylePromptInternal 构建风格提示词（内部）
func (s *StyleService) buildStylePromptInternal(config *StyleConfig) string {
	var prompts []string

	if voice, ok := NarrativeVoicePrompts[config.NarrativeVoice]; ok {
		prompts = append(prompts, voice)
	}

	if distance, ok := NarrativeDistancePrompts[config.NarrativeDistance]; ok {
		prompts = append(prompts, distance)
	}

	if tone, ok := EmotionalTonePrompts[config.EmotionalTone]; ok {
		prompts = append(prompts, tone)
	}

	if complexity, ok := SentenceComplexityPrompts[config.SentenceComplexity]; ok {
		prompts = append(prompts, complexity)
	}

	if density, ok := DescriptionDensityPrompts[config.DescriptionDensity]; ok {
		prompts = append(prompts, density)
	}

	// 对话比例
	if config.DialogueRatio > 0 {
		prompts = append(prompts, fmt.Sprintf("对话比例约占%d%%", int(int(config.DialogueRatio*100))))
	}

	return strings.Join(prompts, "。") + "。"
}

// MergeStyleConfig 合并风格配置
func (s *StyleService) MergeStyleConfig(base, override *StyleConfig) *StyleConfig {
	result := *base

	if override == nil {
		return &result
	}

	if override.NarrativeVoice != "" {
		result.NarrativeVoice = override.NarrativeVoice
	}
	if override.NarrativeDistance != "" {
		result.NarrativeDistance = override.NarrativeDistance
	}
	if override.EmotionalTone != "" {
		result.EmotionalTone = override.EmotionalTone
	}
	if override.SentenceComplexity != "" {
		result.SentenceComplexity = override.SentenceComplexity
	}
	if override.DescriptionDensity != "" {
		result.DescriptionDensity = override.DescriptionDensity
	}
	if override.DialogueRatio > 0 {
		result.DialogueRatio = override.DialogueRatio
	}

	return &result
}

// getDefaultStyleConfig 获取默认风格配置（内部）
func (s *StyleService) getDefaultStyleConfig() *StyleConfig {
	return &StyleConfig{
		NarrativeVoice:     "third_limited",
		NarrativeDistance:  "medium",
		EmotionalTone:      "neutral",
		SentenceComplexity: "moderate",
		DialogueRatio:      0.3,
		DescriptionDensity: "moderate",
	}
}

// ============================================
// Generation Context Service - 生成上下文管理
// ============================================

type GenerationContextService struct {
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	}
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
		GetRecent(novelID uint, chapterNo, count int) ([]*model.Chapter, error)
		ListByNovel(novelID uint) ([]*model.Chapter, error)
	}
	charRepo interface {
		ListByNovel(novelID uint) ([]*model.Character, error)
	}
	snapshotSvc   *CharacterArcService
}

func NewGenerationContextService(
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	},
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
		GetRecent(novelID uint, chapterNo int, count int) ([]*model.Chapter, error)
		ListByNovel(novelID uint) ([]*model.Chapter, error)
	},
	charRepo interface {
		GetByID(id uint) (*model.Character, error)
		ListByNovel(novelID uint) ([]*model.Character, error)
	},
	snapshotSvc *CharacterArcService,
) *GenerationContextService {
	return &GenerationContextService{
		novelRepo:     novelRepo,
		chapterRepo:   chapterRepo,
		charRepo:      charRepo,
		snapshotSvc:   snapshotSvc,
	}
}

// GenerationContext 生成上下文
type GenerationContext struct {
	Novel          *model.Novel       `json:"novel"`
	Characters     []*model.Character `json:"characters"`
	RecentChapters []*model.Chapter   `json:"recent_chapters"`

	// 新增：增强上下文
	Timeline      *Timeline              `json:"timeline"`
	CharacterArcs map[uint]*CharacterArc `json:"character_arcs"`

	// 全局摘要
	GlobalSummary string `json:"global_summary"`
}

// GetContext 获取生成上下文
func (s *GenerationContextService) GetContext(novelID uint, currentChapterNo int) (*GenerationContext, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}

	// 获取前5章作为上下文
	recentChapters, err := s.chapterRepo.GetRecent(novelID, currentChapterNo, 5)
	if err != nil {
		return nil, err
	}

	// 获取所有角色
	characters, err := s.charRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}

	ctx := &GenerationContext{
		Novel:          novel,
		Characters:     characters,
		RecentChapters: recentChapters,
		CharacterArcs:  make(map[uint]*CharacterArc),
	}

	// 获取时间线
	timelineSvc := NewTimelineService(s.chapterRepo)
	ctx.Timeline, _ = timelineSvc.BuildTimeline(novelID)

	// 获取角色弧光
	if s.snapshotSvc != nil {
		for _, char := range characters {
			if arc, err := s.snapshotSvc.GetCharacterArc(novelID, char.ID); err == nil {
				ctx.CharacterArcs[char.ID] = arc
			}
		}
	}

	// 生成全局摘要
	ctx.GlobalSummary = s.generateGlobalSummary(ctx)

	return ctx, nil
}

// generateGlobalSummary 生成分隔摘要
func (s *GenerationContextService) generateGlobalSummary(ctx *GenerationContext) string {
	var sb strings.Builder

	sb.WriteString("【故事概要】\n")
	sb.WriteString(ctx.Novel.Meta.Description)
	sb.WriteString("\n\n")

	if ctx.Novel.Worldview != nil {
		sb.WriteString("【世界观】\n")
		if ctx.Novel.Worldview.MagicSystem != "" {
			sb.WriteString(fmt.Sprintf("修炼体系：%s\n", ctx.Novel.Worldview.MagicSystem))
		}
		if ctx.Novel.Worldview.Geography != "" {
			sb.WriteString(fmt.Sprintf("地理环境：%s\n", ctx.Novel.Worldview.Geography))
		}
	}

	sb.WriteString("\n【主要角色】\n")
	for _, char := range ctx.Characters {
		if char.Role == "protagonist" || char.Role == "antagonist" {
			sb.WriteString(fmt.Sprintf("- %s（%s）：%s\n", char.Name, char.Role, char.Description))
		}
	}

	return sb.String()
}

// buildGenerationPrompt 构建带上下文的生成提示词（内部）
func (s *GenerationContextService) buildGenerationPrompt(ctx *GenerationContext, chapterNo int, style *StyleConfig, extraPrompt string) string {
	var sb strings.Builder

	// 1. 全局摘要
	sb.WriteString(ctx.GlobalSummary)
	sb.WriteString("\n")

	// 2. 风格要求
	if style != nil {
		styleSvc := NewStyleService(nil)
		sb.WriteString("【风格要求】\n")
		sb.WriteString(styleSvc.buildStylePromptInternal(style))
		sb.WriteString("\n")
	}

	// 3. 章节信息
	sb.WriteString(fmt.Sprintf("【当前章节】第%d章\n\n", chapterNo))

	// 4. 前文回顾
	if len(ctx.RecentChapters) > 0 {
		sb.WriteString("【前情提要】\n")
		for i := len(ctx.RecentChapters) - 1; i >= 0; i-- {
			ch := ctx.RecentChapters[i]
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n",
				ch.ChapterNo, ch.Title, ch.Summary))
		}
		sb.WriteString("\n")
	}

	// 5. 角色状态
	if len(ctx.CharacterArcs) > 0 {
		sb.WriteString("【角色状态】\n")
		for _, arc := range ctx.CharacterArcs {
			if arc.CurrentStage > 0 && arc.CurrentStage <= len(arc.Stages) {
				latest := arc.Stages[arc.CurrentStage-1]
				sb.WriteString(fmt.Sprintf("- %s：当前处于「%s」阶段，心态「%s」\n",
					arc.CharacterName,
					s.getArcStageName(arc.ArcType, latest.PowerLevel),
					latest.State))
			}
		}
		sb.WriteString("\n")
	}

	// 6. 额外要求
	if extraPrompt != "" {
		sb.WriteString(fmt.Sprintf("\n【额外要求】\n%s\n", extraPrompt))
	}

	return sb.String()
}

// getArcStageName 获取弧光阶段名称
func (s *GenerationContextService) getArcStageName(arcType string, powerLevel int) string {
	switch arcType {
	case "growth":
		if powerLevel < 30 {
			return "觉醒期"
		} else if powerLevel < 70 {
			return "成长期"
		} else {
			return "巅峰期"
		}
	case "fall":
		if powerLevel > 70 {
			return "巅峰期"
		} else if powerLevel > 30 {
			return "衰落期"
		} else {
			return "谷底期"
		}
	default:
		return "平稳期"
	}
}
