package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/commons"
	"github.com/inkframe/inkframe-backend/internal/model"
)

func (s *AIService) getTenantProvider(tenantId uint, targetType commons.ModelType, modelNames ...string) (ai.ProviderMeta, *model.AIModel, error) {
	dbModels, err := s.modelRepo.ListByTenantAndType(tenantId, targetType)
	if err != nil {
		return nil, nil, err
	}
	if len(dbModels) == 0 {
		return nil, nil, fmt.Errorf("model not configured")
	}
	selectedModel := ""
	if len(modelNames) > 0 {
		selectedModel = modelNames[0]
	}
	var m *model.AIModel
	for _, model := range dbModels {
		if !model.IsActive {
			continue
		}
		if selectedModel == "" || selectedModel == model.Name {
			m = model
			break
		}
	}
	if m == nil {
		return nil, nil, fmt.Errorf("no active %s model found for tenant %d", targetType, tenantId)
	}
	p, err := s.toAIProvider(m)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, fmt.Errorf("AI provider not found")
	}
	return p, m, err
}

func (s *AIService) toAIProvider(m *model.AIModel) (ai.ProviderMeta, error) {
	factory, ok := ai.LookupFactory(m.Provider.Name)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", m.Provider.Name)
	}
	cfg := ai.ProviderConfig{
		APIKey:       m.Provider.APIKey,
		APISecretKey: m.Provider.APISecretKey,
		APIEndpoint:  m.Provider.APIEndpoint,
		ModelName:    m.Name,
		APIVersion:   m.Provider.APIVersion,
		Timeout:      time.Duration(m.Timeout) * time.Second,
		ModelType:    m.Type,
	}
	return factory(cfg)
}

// CheckAvailability 检查指定租户是否有可用的 LLM 提供商（用于 pipeline 预检）
func (s *AIService) CheckAvailability(tenantID uint) error {
	_, _, err := s.getTenantProvider(tenantID, commons.LLM, "")
	return err
}

// GetActiveVideoModelName 从数据库查询指定 provider 的第一个激活视频模型名。
func (s *AIService) GetActiveVideoModelName(tenantID uint, providerName string) (string, error) {
	if s.providerRepo == nil || s.modelRepo == nil {
		return "", fmt.Errorf("repos not available")
	}
	providers, err := s.providerRepo.ListByModelType(tenantID, commons.ModelType("video"))
	if err != nil {
		return "", err
	}
	pnameLower := strings.ToLower(providerName)
	for _, p := range providers {
		if !p.IsActive || strings.ToLower(p.Name) != pnameLower {
			continue
		}
		models, mErr := s.modelRepo.List(&p.ID, tenantID)
		if mErr != nil {
			return "", mErr
		}
		for _, m := range models {
			if m.Type == "video" && m.IsActive {
				return m.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no active video model for provider %q", providerName)
}
