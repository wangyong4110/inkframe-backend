package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ============================================
// HookChainService 钩子链服务
// ============================================

type HookChainService struct {
	repo *repository.HookChainRepository
}

func NewHookChainService(repo *repository.HookChainRepository) *HookChainService {
	return &HookChainService{repo: repo}
}

func (s *HookChainService) ListByNovel(novelID uint) ([]*model.HookChain, error) {
	return s.repo.ListByNovel(novelID)
}

func (s *HookChainService) Create(tenantID, novelID uint, req *model.HookChain) (*model.HookChain, error) {
	req.NovelID = novelID
	req.IsFulfilled = false
	if err := s.repo.Create(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *HookChainService) Update(id uint, req *model.HookChain) (*model.HookChain, error) {
	h, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Type != "" {
		h.Type = req.Type
	}
	if req.Description != "" {
		h.Description = req.Description
	}
	if req.PlantedAt != 0 {
		h.PlantedAt = req.PlantedAt
	}
	if req.PlannedPayoffAt != 0 {
		h.PlannedPayoffAt = req.PlannedPayoffAt
	}
	if req.Intensity != 0 {
		h.Intensity = req.Intensity
	}
	if req.Notes != "" {
		h.Notes = req.Notes
	}
	if err := s.repo.Update(h); err != nil {
		return nil, err
	}
	return h, nil
}

func (s *HookChainService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// Fulfill 标记钩子已兑现
func (s *HookChainService) Fulfill(id uint, actualChapter int) (*model.HookChain, error) {
	h, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	h.IsFulfilled = true
	h.ActualPayoffAt = actualChapter
	if err := s.repo.Update(h); err != nil {
		return nil, err
	}
	return h, nil
}

// RatePayoff 评分钩子兑现质量
func (s *HookChainService) RatePayoff(id uint, quality int, notes string) (*model.HookChain, error) {
	h, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if quality < 1 || quality > 5 {
		return nil, fmt.Errorf("quality must be 1-5")
	}
	h.PayoffQuality = quality
	h.PayoffNotes = notes
	if err := s.repo.Update(h); err != nil {
		return nil, err
	}
	return h, nil
}

// maxHookInjection 单次注入的最大钩子数量，防止 prompt 超出 token 预算
const maxHookInjection = 5

// GetInjectionContext 为章节生成提供钩子上下文提示
// 查询强度≥6且未兑现的钩子（PlantedAt ≤ currentChapter-1），最多注入 maxHookInjection 个（按强度降序）。
func (s *HookChainService) GetInjectionContext(novelID uint, currentChapter int) string {
	hooks, err := s.repo.ListPending(novelID)
	if err != nil || len(hooks) == 0 {
		return ""
	}

	var relevant []*model.HookChain
	for _, h := range hooks {
		if h.Intensity >= 6 && h.PlantedAt <= currentChapter-1 {
			relevant = append(relevant, h)
		}
	}
	if len(relevant) == 0 {
		return ""
	}

	// 按强度降序排序，优先注入最重要的钩子
	sort.Slice(relevant, func(i, j int) bool {
		return relevant[i].Intensity > relevant[j].Intensity
	})
	// Token 预算限制：最多注入 maxHookInjection 个
	if len(relevant) > maxHookInjection {
		relevant = relevant[:maxHookInjection]
	}

	var sb strings.Builder
	sb.WriteString("待延续钩子（必须在本章回应或加深）：\n")
	for _, h := range relevant {
		overdue := currentChapter - h.PlantedAt
		planned := ""
		if h.PlannedPayoffAt > 0 {
			if currentChapter >= h.PlannedPayoffAt {
				planned = fmt.Sprintf("（已超期%d章，必须兑现）", currentChapter-h.PlannedPayoffAt)
			} else {
				planned = fmt.Sprintf("（计划第%d章兑现）", h.PlannedPayoffAt)
			}
		}
		sb.WriteString(fmt.Sprintf("- [intensity=%d] 第%d章埋（已%d章）：「%s」%s\n",
			h.Intensity, h.PlantedAt, overdue, h.Description, planned))
	}
	return sb.String()
}

// ============================================
// SatisfactionPointService 爽点服务
// ============================================

type SatisfactionPointService struct {
	repo *repository.SatisfactionPointRepository
}

func NewSatisfactionPointService(repo *repository.SatisfactionPointRepository) *SatisfactionPointService {
	return &SatisfactionPointService{repo: repo}
}

func (s *SatisfactionPointService) ListByNovel(novelID uint) ([]*model.SatisfactionPoint, error) {
	return s.repo.ListByNovel(novelID)
}

func (s *SatisfactionPointService) Create(tenantID, novelID uint, req *model.SatisfactionPoint) (*model.SatisfactionPoint, error) {
	req.NovelID = novelID
	req.IsPlanned = true
	if err := s.repo.Create(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *SatisfactionPointService) Update(id uint, req *model.SatisfactionPoint) (*model.SatisfactionPoint, error) {
	sp, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Type != "" {
		sp.Type = req.Type
	}
	if req.Description != "" {
		sp.Description = req.Description
	}
	if req.PlannedChapter != 0 {
		sp.PlannedChapter = req.PlannedChapter
	}
	if req.BuildupStart != 0 {
		sp.BuildupStart = req.BuildupStart
	}
	if req.IntensityTarget != 0 {
		sp.IntensityTarget = req.IntensityTarget
	}
	if req.ChapterID != nil {
		sp.ChapterID = req.ChapterID
		sp.IsPlanned = false
	}
	if req.Notes != "" {
		sp.Notes = req.Notes
	}
	if err := s.repo.Update(sp); err != nil {
		return nil, err
	}
	return sp, nil
}

func (s *SatisfactionPointService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// GetInjectionContext 返回计划在 currentChapter±2 范围内的爽点铺垫提示
func (s *SatisfactionPointService) GetInjectionContext(novelID uint, currentChapter int) string {
	sps, err := s.repo.ListByNovel(novelID)
	if err != nil || len(sps) == 0 {
		return ""
	}

	var relevant []*model.SatisfactionPoint
	for _, sp := range sps {
		if !sp.IsPlanned {
			continue
		}
		diff := sp.PlannedChapter - currentChapter
		if diff >= -2 && diff <= 2 {
			relevant = append(relevant, sp)
		}
	}
	if len(relevant) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("即将到来的爽点（本章需铺垫或触发）：\n")
	for _, sp := range relevant {
		timing := ""
		diff := sp.PlannedChapter - currentChapter
		switch {
		case diff < 0:
			timing = "（本章应已触发）"
		case diff == 0:
			timing = "（本章触发）"
		default:
			timing = fmt.Sprintf("（第%d章触发，提前铺垫）", sp.PlannedChapter)
		}
		sb.WriteString(fmt.Sprintf("- [%s intensity=%d] %s：%s%s\n",
			sp.Type, sp.IntensityTarget, sp.Type, sp.Description, timing))
	}
	return sb.String()
}

// ============================================
// ConflictArcService 冲突弧服务
// ============================================

// conflictPhases 三幕六阶段：铺垫→点燃→升级→转折→高潮→余震
// 兼容旧数据：resolution→aftershock 视为同义，escalation 直接映射
var conflictPhases = []string{"setup", "ignition", "escalation", "turning_point", "climax", "aftershock"}

type ConflictArcService struct {
	repo *repository.ConflictArcRepository
}

func NewConflictArcService(repo *repository.ConflictArcRepository) *ConflictArcService {
	return &ConflictArcService{repo: repo}
}

func (s *ConflictArcService) ListByNovel(novelID uint) ([]*model.ConflictArc, error) {
	return s.repo.ListByNovel(novelID)
}

func (s *ConflictArcService) Create(tenantID, novelID uint, req *model.ConflictArc) (*model.ConflictArc, error) {
	req.NovelID = novelID
	if req.CurrentPhase == "" {
		req.CurrentPhase = "setup"
	}
	req.IsResolved = false
	if err := s.repo.Create(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *ConflictArcService) Update(id uint, req *model.ConflictArc) (*model.ConflictArc, error) {
	arc, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		arc.Title = req.Title
	}
	if req.Type != "" {
		arc.Type = req.Type
	}
	if req.Description != "" {
		arc.Description = req.Description
	}
	if req.Antagonist != "" {
		arc.Antagonist = req.Antagonist
	}
	if req.StartChapter != 0 {
		arc.StartChapter = req.StartChapter
	}
	if req.PeakChapter != 0 {
		arc.PeakChapter = req.PeakChapter
	}
	if req.EndChapter != 0 {
		arc.EndChapter = req.EndChapter
	}
	if req.Notes != "" {
		arc.Notes = req.Notes
	}
	if err := s.repo.Update(arc); err != nil {
		return nil, err
	}
	return arc, nil
}

func (s *ConflictArcService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// AdvancePhase 推进阶段（三幕六阶段：setup→ignition→escalation→turning_point→climax→aftershock）
func (s *ConflictArcService) AdvancePhase(id uint) (*model.ConflictArc, error) {
	arc, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	// 兼容旧数据：resolution 映射到 aftershock
	if arc.CurrentPhase == "resolution" {
		arc.CurrentPhase = "aftershock"
		arc.IsResolved = true
		if err := s.repo.Update(arc); err != nil {
			return nil, err
		}
		return arc, nil
	}
	nextPhase := conflictPhases[0]
	for i, p := range conflictPhases {
		if p == arc.CurrentPhase && i+1 < len(conflictPhases) {
			nextPhase = conflictPhases[i+1]
			break
		}
	}
	arc.CurrentPhase = nextPhase
	if nextPhase == "aftershock" {
		arc.IsResolved = true
	}
	if err := s.repo.Update(arc); err != nil {
		return nil, err
	}
	return arc, nil
}

// UpdateTension 更新指定阶段的张力值
func (s *ConflictArcService) UpdateTension(id uint, phase string, level int) (*model.ConflictArc, error) {
	arc, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if level < 1 || level > 10 {
		return nil, fmt.Errorf("tension level must be 1-10")
	}
	tensionMap := make(map[string]int)
	if arc.TensionLevels != "" {
		_ = json.Unmarshal([]byte(arc.TensionLevels), &tensionMap)
	}
	tensionMap[phase] = level
	data, _ := json.Marshal(tensionMap)
	arc.TensionLevels = string(data)
	if err := s.repo.Update(arc); err != nil {
		return nil, err
	}
	return arc, nil
}

// GetInjectionContext 返回活跃冲突弧的当前阶段和对立方提示
func (s *ConflictArcService) GetInjectionContext(novelID uint, currentChapter int) string {
	arcs, err := s.repo.ListByNovel(novelID)
	if err != nil || len(arcs) == 0 {
		return ""
	}

	var active []*model.ConflictArc
	for _, arc := range arcs {
		if arc.IsResolved {
			continue
		}
		if arc.StartChapter > currentChapter {
			continue
		}
		active = append(active, arc)
	}
	if len(active) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("活跃冲突弧（本章需体现其进展）：\n")
	for _, arc := range active {
		antagonist := ""
		if arc.Antagonist != "" {
			antagonist = fmt.Sprintf("，对立方：%s", arc.Antagonist)
		}
		sb.WriteString(fmt.Sprintf("- 「%s」[%s阶段%s]：%s\n",
			arc.Title, arc.CurrentPhase, antagonist, arc.Description))
	}
	return sb.String()
}

// ============================================
// PacingService 节奏曲线服务
// ============================================

// PacingPoint 节奏曲线数据点
type PacingPoint struct {
	ChapterNo     int    `json:"chapter_no"`
	TensionLevel  int    `json:"tension_level"`
	ActNo         int    `json:"act_no"`
	EmotionalTone string `json:"emotional_tone"`
	Title         string `json:"title"`
}

// PacingWarning 节奏健康警告
type PacingWarning struct {
	Type     string `json:"type"`    // consecutive_low/consecutive_high/no_midpoint/no_satisfaction
	Message  string `json:"message"`
	Chapters []int  `json:"chapters"` // 涉及的章节号
}

// PacingHealth 节奏健康报告
type PacingHealth struct {
	Status   string          `json:"status"` // healthy/warning/critical
	Warnings []PacingWarning `json:"warnings"`
}

type PacingService struct {
	chapterRepo *repository.ChapterRepository
	spRepo      *repository.SatisfactionPointRepository
}

func NewPacingService(chapterRepo *repository.ChapterRepository, spRepo *repository.SatisfactionPointRepository) *PacingService {
	return &PacingService{chapterRepo: chapterRepo, spRepo: spRepo}
}

// GetCurve 获取节奏曲线
func (s *PacingService) GetCurve(novelID uint) ([]*PacingPoint, error) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	points := make([]*PacingPoint, 0, len(chapters))
	for _, ch := range chapters {
		points = append(points, &PacingPoint{
			ChapterNo:     ch.ChapterNo,
			TensionLevel:  ch.NarrativeMeta.TensionLevel,
			ActNo:         ch.NarrativeMeta.ActNo,
			EmotionalTone: ch.NarrativeMeta.EmotionalTone,
			Title:         ch.Title,
		})
	}
	return points, nil
}

// GetHealth 检测节奏健康状况
func (s *PacingService) GetHealth(novelID uint) (*PacingHealth, error) {
	points, err := s.GetCurve(novelID)
	if err != nil {
		return nil, err
	}

	health := &PacingHealth{Status: "healthy"}

	if len(points) < 3 {
		return health, nil
	}

	// 规则1：连续≥5章 tension≤4 → consecutive_low
	if w := detectConsecutive(points, func(p *PacingPoint) bool { return p.TensionLevel <= 4 }, 5); w != nil {
		w.Type = "consecutive_low"
		w.Message = fmt.Sprintf("连续%d章张力过低（≤4），节奏可能沉闷", len(w.Chapters))
		health.Warnings = append(health.Warnings, *w)
	}

	// 规则2：连续≥4章 tension≥8 → consecutive_high
	if w := detectConsecutive(points, func(p *PacingPoint) bool { return p.TensionLevel >= 8 }, 4); w != nil {
		w.Type = "consecutive_high"
		w.Message = fmt.Sprintf("连续%d章张力过高（≥8），读者可能疲劳", len(w.Chapters))
		health.Warnings = append(health.Warnings, *w)
	}

	// 规则3：全书50%位置无张力反转（tension变化>3）→ no_midpoint
	midIdx := len(points) / 2
	hasMidReversal := false
	for i := 1; i <= midIdx; i++ {
		if abs(points[i].TensionLevel-points[i-1].TensionLevel) > 3 {
			hasMidReversal = true
			break
		}
	}
	if !hasMidReversal && len(points) >= 10 {
		health.Warnings = append(health.Warnings, PacingWarning{
			Type:     "no_midpoint",
			Message:  "全书前半段缺乏显著张力反转（变化>3），建议加入中段高潮",
			Chapters: []int{points[midIdx].ChapterNo},
		})
	}

	// 规则4：最近10章无 SatisfactionPoint → no_satisfaction
	if len(points) >= 10 {
		lastChapterNo := points[len(points)-1].ChapterNo
		fromChapter := lastChapterNo - 9
		if fromChapter < 1 {
			fromChapter = 1
		}
		recent, _ := s.spRepo.ListRecentFulfilled(novelID, fromChapter)
		if len(recent) == 0 {
			recentChapters := make([]int, 0, 10)
			for _, p := range points[max(0, len(points)-10):] {
				recentChapters = append(recentChapters, p.ChapterNo)
			}
			health.Warnings = append(health.Warnings, PacingWarning{
				Type:     "no_satisfaction",
				Message:  "最近10章无已触发爽点，读者满足感可能不足",
				Chapters: recentChapters,
			})
		}
	}

	// 评定总体状态
	switch {
	case len(health.Warnings) == 0:
		health.Status = "healthy"
	case len(health.Warnings) <= 1:
		health.Status = "warning"
	default:
		health.Status = "critical"
	}

	return health, nil
}

// detectConsecutive 检测是否有连续 minLen 个点满足 cond，返回首个违规段
func detectConsecutive(points []*PacingPoint, cond func(*PacingPoint) bool, minLen int) *PacingWarning {
	streak := 0
	var streakChapters []int
	for _, p := range points {
		if cond(p) {
			streak++
			streakChapters = append(streakChapters, p.ChapterNo)
			if streak >= minLen {
				return &PacingWarning{Chapters: streakChapters}
			}
		} else {
			streak = 0
			streakChapters = nil
		}
	}
	return nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

