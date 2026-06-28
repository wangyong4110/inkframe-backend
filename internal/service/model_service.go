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
	experimentRepo *repository.ModelComparisonRepository
	aiService      *AIService
}

func NewModelService(
	modelRepo *repository.AIModelRepository,
	providerRepo *repository.ModelProviderRepository,
	experimentRepo *repository.ModelComparisonRepository,
	aiService ...*AIService,
) *ModelService {
	svc := &ModelService{
		modelRepo:      modelRepo,
		providerRepo:   providerRepo,
		experimentRepo: experimentRepo,
	}
	if len(aiService) > 0 {
		svc.aiService = aiService[0]
	}
	return svc
}

// selectByQuality 按 Quality 降序选模；Quality 相等时以 ID 升序（插入顺序）作为 tiebreaker，
// 保证全部 Quality=0 时仍能确定性地选出第一个添加的模型。
func selectByQuality(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	for _, m := range models {
		if best == nil {
			best = m
			continue
		}
		if m.Quality > best.Quality || (m.Quality == best.Quality && m.ID < best.ID) {
			best = m
		}
	}
	return best
}

func selectByCost(models []*model.AIModel) *model.AIModel    { return selectByQuality(models) }
func selectBalanced(models []*model.AIModel) *model.AIModel  { return selectByQuality(models) }

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
		sem := make(chan struct{}, 2)

		for _, mid := range modelIDs {
			mid := mid
			wg.Add(1)
			go func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				defer wg.Done()
				m, err := s.modelRepo.GetByID(mid)
				if err != nil || m == nil || m.Provider == nil {
					ch <- slot{mid, nil, fmt.Errorf("model %d not found or has no provider", mid)}
					return
				}
				start := time.Now()
				content, err := s.aiService.GenerateWithProvider(0, 0, exp.TaskType, exp.InputData, m.Provider.Name, StoryboardOverrides{MaxTokens: 2048})
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
