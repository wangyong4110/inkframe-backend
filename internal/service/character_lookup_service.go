package service

import (
	"context"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// CharacterLookupService MCP 工具：按名称查询角色档案和最近快照
type CharacterLookupService struct {
	charRepo     *repository.CharacterRepository
	snapshotRepo *repository.CharacterStateSnapshotRepository
}

func NewCharacterLookupService(
	charRepo *repository.CharacterRepository,
	snapshotRepo *repository.CharacterStateSnapshotRepository,
) *CharacterLookupService {
	return &CharacterLookupService{
		charRepo:     charRepo,
		snapshotRepo: snapshotRepo,
	}
}

// CharacterLookupResult 角色查询结果
type CharacterLookupResult struct {
	Found     bool              `json:"found"`
	Character *CharacterProfile `json:"character,omitempty"`
	Snapshots []SnapshotSummary `json:"snapshots,omitempty"`
}

// CharacterProfile 精简的角色档案（对外暴露）
type CharacterProfile struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Description string `json:"description"`
	Age         string `json:"age"`
}

// SnapshotSummary 角色状态快照摘要
type SnapshotSummary struct {
	ChapterID  uint   `json:"chapter_id"`
	Mood       string `json:"mood"`
	Location   string `json:"location"`
	Health     string `json:"health"`
	PowerLevel int    `json:"power_level"`
	Motivation string `json:"motivation"`
}

// Lookup 按 novel_id 和名称查询角色及其最近快照
func (s *CharacterLookupService) Lookup(_ context.Context, novelID uint, characterName string) (*CharacterLookupResult, error) {
	// 获取该小说所有角色
	characters, err := s.charRepo.ListByNovel(novelID)
	if err != nil {
		return &CharacterLookupResult{Found: false}, nil
	}

	// 优先精确匹配，降级到包含匹配
	var found *model.Character
	needle := strings.ToLower(characterName)
	for _, c := range characters {
		if strings.ToLower(c.Name) == needle {
			found = c
			break
		}
	}
	if found == nil {
		for _, c := range characters {
			if strings.Contains(strings.ToLower(c.Name), needle) {
				found = c
				break
			}
		}
	}

	if found == nil {
		return &CharacterLookupResult{Found: false}, nil
	}

	result := &CharacterLookupResult{
		Found: true,
		Character: &CharacterProfile{
			ID:          found.ID,
			Name:        found.Name,
			Role:        found.Role,
			Description: found.Description,
			Age:         found.Age,
		},
	}

	// 获取最近 5 个快照
	if s.snapshotRepo != nil {
		snapshots, err := s.snapshotRepo.ListByCharacter(found.ID)
		if err == nil {
			limit := min(5, len(snapshots))
			for i := 0; i < limit; i++ {
				ss := snapshots[i]
				result.Snapshots = append(result.Snapshots, SnapshotSummary{
					ChapterID:  ss.ChapterID,
					Mood:       ss.Mood,
					Location:   ss.Location,
					Health:     ss.Health,
					PowerLevel: ss.PowerLevel,
					Motivation: ss.Motivation,
				})
			}
		}
	}

	return result, nil
}
