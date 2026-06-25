package service

import (
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SceneIssue 一致性问题描述
type SceneIssue struct {
	Category string  `json:"category"` // arch/light/atmo/prop/time
	Severity string  `json:"severity"` // warning/error
	Detail   string  `json:"detail"`
	Score    float64 `json:"score"`
}

// SceneConsistencyReport 一致性评分报告
type SceneConsistencyReport struct {
	ShotID       uint
	AnchorID     uint
	OverallScore float64
	ArchScore    float64
	LightScore   float64
	AtmoScore    float64
	PropScore    float64
	TimeScore    float64 // 时间/季节一致性
	Issues       []SceneIssue
	// SuggestedFix 由 LLM 生成的下次图像生成 prompt 修正关键词，NeedsRetry 时注入重试 prompt
	SuggestedFix string
	Passed       bool
	NeedsRetry   bool // 0.70 <= score < 0.85
	NeedsHuman   bool // score < 0.70
}

// SceneConsistencyService 场景一致性评分服务
type SceneConsistencyService struct {
	logRepo *repository.SceneConsistencyLogRepository
	aiSvc   *AIService
}

func NewSceneConsistencyService(logRepo *repository.SceneConsistencyLogRepository, aiSvc *AIService) *SceneConsistencyService {
	return &SceneConsistencyService{logRepo: logRepo, aiSvc: aiSvc}
}

// ScoreScene 调用 Vision LLM 比对生成图 vs anchor.RefImageURL，返回多维评分。
// 如果锚点无参考图，直接返回满分（pass）以避免阻塞流程。
func (s *SceneConsistencyService) ScoreScene(
	shot *model.StoryboardShot,
	anchor *model.SceneAnchor,
	generatedImageURL string,
	attempt int,
	tenantID uint,
	novelID uint,
) (*SceneConsistencyReport, error) {
	// 无参考图时返回中性分并标记需人工审核（不给满分以免掩盖问题）
	if anchor.RefImageURL == "" {
		metrics.SceneConsistencyTotal.WithLabelValues("noref").Inc()
		report := &SceneConsistencyReport{
			ShotID:       shot.ID,
			AnchorID:     anchor.ID,
			OverallScore: 0.5,
			ArchScore:    0.5,
			LightScore:   0.5,
			AtmoScore:    0.5,
			PropScore:    0.5,
			TimeScore:    0.5,
			Passed:       false,
			NeedsHuman:   true,
		}
		return report, nil
	}

	prompt, err := renderPrompt("scene_consistency_score", map[string]interface{}{
		"AnchorName":        anchor.Name,
		"RefImageURL":       anchor.RefImageURL,
		"GeneratedImageURL": generatedImageURL,
		"Description":       anchor.Description,
	})
	if err != nil {
		return nil, fmt.Errorf("render scene_consistency_score: %w", err)
	}

	raw, err := s.aiSvc.GenerateWithProvider(tenantID, 0, "scene_consistency", prompt, "")
	if err != nil {
		return nil, fmt.Errorf("LLM consistency score: %w", err)
	}

	report, err := parseConsistencyResponse(raw, shot.ID, anchor.ID)
	if err != nil {
		logger.Errorf("[SceneConsistencyService] parse failed for shot %d: %v, raw=%q", shot.ID, err, raw)
		// 解析失败时标记人工审核，不给高分静默通过
		report = &SceneConsistencyReport{
			ShotID:       shot.ID,
			AnchorID:     anchor.ID,
			OverallScore: 0.5,
			ArchScore:    0.5,
			LightScore:   0.5,
			AtmoScore:    0.5,
			PropScore:    0.5,
			TimeScore:    0.5,
			Passed:       false,
			NeedsHuman:   true,
		}
	}

	// 确定是否需要重试或人工干预（仅在解析成功时重新判定，parse 失败已设好）
	if err == nil {
		report.NeedsRetry = report.OverallScore >= 0.70 && report.OverallScore < 0.85
		report.NeedsHuman = report.OverallScore < 0.70
	}

	// 记录 Prometheus 指标
	metrics.SceneConsistencyScoreHist.Observe(report.OverallScore)
	switch {
	case report.Passed:
		metrics.SceneConsistencyTotal.WithLabelValues("passed").Inc()
	case report.NeedsRetry:
		metrics.SceneConsistencyTotal.WithLabelValues("retry").Inc()
	default:
		metrics.SceneConsistencyTotal.WithLabelValues("human").Inc()
	}

	// 持久化评分日志（修复：同时写入 PropScore、TimeScore、Passed 和 SuggestedFix）
	issuesJSON, _ := json.Marshal(report.Issues)
	logEntry := &model.SceneConsistencyLog{
		NovelID:      novelID,
		ShotID:       shot.ID,
		AnchorID:     anchor.ID,
		Attempt:      attempt,
		OverallScore: report.OverallScore,
		ArchScore:    report.ArchScore,
		LightScore:   report.LightScore,
		AtmoScore:    report.AtmoScore,
		PropScore:    report.PropScore,
		TimeScore:    report.TimeScore,
		Issues:       string(issuesJSON),
		SuggestedFix: report.SuggestedFix,
		Passed:       report.Passed, // 使用 report.Passed（score>=0.85），而非 !NeedsHuman
	}
	if err := s.logRepo.Create(logEntry); err != nil {
		logger.Errorf("[SceneConsistencyService] save log: %v", err)
	}

	return report, nil
}

// GetLogsByShotID 查询某 shot 的所有评分历史
func (s *SceneConsistencyService) GetLogsByShotID(shotID uint) ([]*model.SceneConsistencyLog, error) {
	return s.logRepo.ListByShotID(shotID)
}

// GetLogsByAnchorID 查询某锚点的所有评分历史
func (s *SceneConsistencyService) GetLogsByAnchorID(anchorID uint) ([]*model.SceneConsistencyLog, error) {
	return s.logRepo.ListByAnchorID(anchorID)
}


// consistencyLLMResponse LLM 返回结构
type consistencyLLMResponse struct {
	OverallScore float64      `json:"overall_score"`
	ArchScore    float64      `json:"arch_score"`
	LightScore   float64      `json:"light_score"`
	AtmoScore    float64      `json:"atmo_score"`
	PropScore    float64      `json:"prop_score"`
	TimeScore    float64      `json:"time_score"`
	Issues       []SceneIssue `json:"issues"`
	// SuggestedFix 下次图像生成的 prompt 修正关键词（score<0.85 时 LLM 填写）
	SuggestedFix string `json:"suggested_fix"`
}

func parseConsistencyResponse(raw string, shotID, anchorID uint) (*SceneConsistencyReport, error) {
	// Use extractJSONObject to skip any leading reasoning text with [...] brackets
	// that would cause extractJSON (which prefers arrays) to pick the wrong block.
	jsonStr := extractJSONObject(raw)
	var resp consistencyLLMResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, err
	}
	// time_score defaults to 1.0 when absent (older LLM responses without the field)
	if resp.TimeScore == 0 {
		resp.TimeScore = 1.0
	}
	// overall_score 校验：若 LLM 未按权重公式返回，在此重算保证一致性
	// 权重：arch×0.35 + light×0.25 + atmo×0.20 + prop×0.15 + time×0.05
	if resp.ArchScore > 0 || resp.LightScore > 0 {
		weighted := resp.ArchScore*0.35 + resp.LightScore*0.25 +
			resp.AtmoScore*0.20 + resp.PropScore*0.15 + resp.TimeScore*0.05
		// 取 LLM 给出的 overall 与加权值的均值，保留 LLM 的整体判断同时约束偏差
		resp.OverallScore = (resp.OverallScore + weighted) / 2.0
	}
	return &SceneConsistencyReport{
		ShotID:       shotID,
		AnchorID:     anchorID,
		OverallScore: resp.OverallScore,
		ArchScore:    resp.ArchScore,
		LightScore:   resp.LightScore,
		AtmoScore:    resp.AtmoScore,
		PropScore:    resp.PropScore,
		TimeScore:    resp.TimeScore,
		Issues:       resp.Issues,
		SuggestedFix: resp.SuggestedFix,
		Passed:       resp.OverallScore >= 0.85,
	}, nil
}
