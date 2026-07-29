package service

import (
	"context"
	"errors"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// CharacterLookupService MCP 工具：按名称查询角色档案和最近快照
type CharacterLookupService struct {
	charRepo     *repository.CharacterRepository
	lookRepo     *repository.CharacterLookRepository
	snapshotRepo *repository.CharacterStateSnapshotRepository
}

func NewCharacterLookupService(
	charRepo *repository.CharacterRepository,
	lookRepo *repository.CharacterLookRepository,
	snapshotRepo *repository.CharacterStateSnapshotRepository,
) *CharacterLookupService {
	return &CharacterLookupService{
		charRepo:     charRepo,
		lookRepo:     lookRepo,
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
			Age:         found.Meta.Age,
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

func (s *CharacterLookupService) getCharDefaultLook(charId uint) (*model.CharacterLook, error) {
	char, err := s.charRepo.GetByID(charId)
	if err != nil {
		return nil, err
	}
	if char.DefaultLookID != 0 {
		if defaultLook, err := s.lookRepo.GetByID(char.DefaultLookID); err == nil && defaultLook != nil {
			return defaultLook, nil
		}
	}
	// 兜底：角色有形象但 DefaultLookID 未设置（如老数据），取第一个含三视图的形象
	if looks, err := s.lookRepo.ListByCharacter(char.ID); err == nil {
		for _, l := range looks {
			if l.ThreeViewSheet != "" {
				logger.Printf("[getCharDefaultLook] charID=%d: DefaultLookID unset, fallback to first look with ThreeViewSheet id=%d", char.ID, l.ID)
				return l, nil
			}
		}
	}
	return nil, errors.New("no character look found")

}
