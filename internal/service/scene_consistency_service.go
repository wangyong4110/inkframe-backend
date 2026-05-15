package service

import (
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SceneIssue 一致性问题描述
type SceneIssue struct {
	Category string  `json:"category"` // arch/light/atmo/prop
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
	Issues       []SceneIssue
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
) (*SceneConsistencyReport, error) {
	// 无参考图时跳过评分
	if anchor.RefImageURL == "" {
		report := &SceneConsistencyReport{
			ShotID:       shot.ID,
			AnchorID:     anchor.ID,
			OverallScore: 1.0,
			ArchScore:    1.0,
			LightScore:   1.0,
			AtmoScore:    1.0,
			PropScore:    1.0,
			Passed:       true,
		}
		return report, nil
	}

	prompt, err := renderPrompt("scene_consistency_score", map[string]interface{}{
		"AnchorName":        anchor.Name,
		"RefImageURL":       anchor.RefImageURL,
		"GeneratedImageURL": generatedImageURL,
		"Description":       anchor.Description,
		"PromptLock":        anchor.PromptLock,
	})
	if err != nil {
		return nil, fmt.Errorf("render scene_consistency_score: %w", err)
	}

	raw, err := s.aiSvc.Generate(shot.VideoID, "scene_consistency", prompt)
	if err != nil {
		return nil, fmt.Errorf("LLM consistency score: %w", err)
	}

	report, err := parseConsistencyResponse(raw, shot.ID, anchor.ID)
	if err != nil {
		logger.Printf("[SceneConsistencyService] parse failed for shot %d: %v, raw=%q", shot.ID, err, raw)
		// 解析失败时给中性分，不阻断流程
		report = &SceneConsistencyReport{
			ShotID:       shot.ID,
			AnchorID:     anchor.ID,
			OverallScore: 0.8,
			ArchScore:    0.8,
			LightScore:   0.8,
			AtmoScore:    0.8,
			PropScore:    0.8,
			Passed:       true,
		}
	}

	// 确定是否需要重试或人工干预
	report.NeedsRetry = report.OverallScore >= 0.70 && report.OverallScore < 0.85
	report.NeedsHuman = report.OverallScore < 0.70

	// 持久化评分日志
	issuesJSON, _ := json.Marshal(report.Issues)
	logEntry := &model.SceneConsistencyLog{
		ShotID:       shot.ID,
		AnchorID:     anchor.ID,
		Attempt:      attempt,
		OverallScore: report.OverallScore,
		ArchScore:    report.ArchScore,
		LightScore:   report.LightScore,
		AtmoScore:    report.AtmoScore,
		Issues:       string(issuesJSON),
		Passed:       !report.NeedsHuman,
	}
	if err := s.logRepo.Create(logEntry); err != nil {
		logger.Printf("[SceneConsistencyService] save log: %v", err)
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
	Issues       []SceneIssue `json:"issues"`
}

func parseConsistencyResponse(raw string, shotID, anchorID uint) (*SceneConsistencyReport, error) {
	jsonStr := extractJSON(raw)
	var resp consistencyLLMResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, err
	}
	return &SceneConsistencyReport{
		ShotID:       shotID,
		AnchorID:     anchorID,
		OverallScore: resp.OverallScore,
		ArchScore:    resp.ArchScore,
		LightScore:   resp.LightScore,
		AtmoScore:    resp.AtmoScore,
		PropScore:    resp.PropScore,
		Issues:       resp.Issues,
		Passed:       resp.OverallScore >= 0.85,
	}, nil
}
