package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

type ModelService struct {
	modelRepo      *repository.AIModelRepository
	providerRepo   *repository.ModelProviderRepository
	taskRepo       *repository.TaskModelConfigRepository
	experimentRepo *repository.ModelComparisonRepository
	aiService      *AIService
}

func NewModelService(
	modelRepo *repository.AIModelRepository,
	providerRepo *repository.ModelProviderRepository,
	taskRepo *repository.TaskModelConfigRepository,
	experimentRepo *repository.ModelComparisonRepository,
	aiService ...*AIService,
) *ModelService {
	svc := &ModelService{
		modelRepo:      modelRepo,
		providerRepo:   providerRepo,
		taskRepo:       taskRepo,
		experimentRepo: experimentRepo,
	}
	if len(aiService) > 0 {
		svc.aiService = aiService[0]
	}
	return svc
}

func selectByQuality(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestScore := 0.0

	for _, m := range models {
		score := m.Quality
		if score > bestScore {
			bestScore = score
			best = m
		}
	}

	return best
}

func selectByCost(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestCost := 999999.0

	for _, m := range models {
		if m.CostPer1K < bestCost {
			bestCost = m.CostPer1K
			best = m
		}
	}

	return best
}

func selectBalanced(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestScore := 0.0

	for _, m := range models {
		// 质量/成本比
		score := m.Quality / m.CostPer1K
		if score > bestScore {
			bestScore = score
			best = m
		}
	}

	return best
}

// RunExperiment runs a model comparison experiment: generates output with every listed model
// in parallel, stores ExperimentResult rows, and marks the winner by quality score.
func (s *ModelService) RunExperiment(id uint) error {
	exp, err := s.experimentRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("experiment %d not found: %w", id, err)
	}
	if exp.Status == "running" {
		return fmt.Errorf("experiment %d is already running", id)
	}

	// Parse model IDs
	var modelIDs []uint
	if err := json.Unmarshal([]byte(exp.ModelIDs), &modelIDs); err != nil || len(modelIDs) == 0 {
		return fmt.Errorf("experiment %d has no valid model_ids", id)
	}

	// Mark running
	exp.Status = "running"
	exp.Progress = 0
	_ = s.experimentRepo.Update(exp)

	go func() {
		type slot struct {
			modelID uint
			result  *model.ExperimentResult
			err     error
		}
		ch := make(chan slot, len(modelIDs))
		var wg sync.WaitGroup

		for _, mid := range modelIDs {
			mid := mid
			wg.Add(1)
			go func() {
				defer wg.Done()
				m, err := s.modelRepo.GetByID(mid)
				if err != nil || m == nil || m.Provider == nil {
					ch <- slot{mid, nil, fmt.Errorf("model %d not found or has no provider", mid)}
					return
				}
				start := time.Now()
				cfg := &model.TaskModelConfig{
					PrimaryModelID: mid,
					Temperature:    0.7,
					MaxTokens:      2048,
				}
				content, err := s.aiService.GenerateWithProvider(0, 0, exp.TaskType, exp.InputData, m.Provider.Name, StoryboardOverrides{MaxTokens: cfg.MaxTokens})
				elapsed := time.Since(start)
				res := &model.ExperimentResult{
					ExperimentID: exp.ID,
					ModelID:      mid,
					Latency:      elapsed.Seconds(),
					Success:      err == nil,
				}
				if err != nil {
					res.Error = err.Error()
				} else {
					res.Output = content
					res.QualityScore = estimateQuality(content)
				}
				ch <- slot{mid, res, err}
			}()
		}

		wg.Wait()
		close(ch)

		var results []*model.ExperimentResult
		for item := range ch {
			if item.result != nil {
				_ = s.experimentRepo.AddResult(item.result)
				results = append(results, item.result)
			}
		}

		// Determine winner: highest quality score among successful runs
		var winner *model.ExperimentResult
		for _, r := range results {
			if r.Success && (winner == nil || r.QualityScore > winner.QualityScore) {
				winner = r
			}
		}

		exp.Status = "completed"
		exp.Progress = 100
		if winner != nil {
			exp.WinnerModelID = &winner.ModelID
		}
		_ = s.experimentRepo.Update(exp)
	}()

	return nil
}

// estimateQuality assigns a rough quality score (0–1) based on output length heuristics.
// A real implementation would call QualityControlService.
func estimateQuality(content string) float64 {
	l := len([]rune(content))
	switch {
	case l >= 1500:
		return 0.9
	case l >= 800:
		return 0.7
	case l >= 300:
		return 0.5
	default:
		return 0.3
	}
}
