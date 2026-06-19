package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SceneAnchorService 场景锚点服务
// 将命名场景的视觉描述、风格 token 和参考图固定下来，
// 在分镜图像生成时强制注入，确保跨镜头布景一致。
type SceneAnchorService struct {
	repo                   *repository.SceneAnchorRepository
	chapterSceneAnchorRepo *repository.ChapterSceneAnchorRepository
	shotRepo               *repository.StoryboardRepository
	novelRepo              *repository.NovelRepository
	chapterRepo            *repository.ChapterRepository
	aiSvc                  *AIService
}

func NewSceneAnchorService(repo *repository.SceneAnchorRepository, shotRepo *repository.StoryboardRepository, aiSvc *AIService, novelRepo *repository.NovelRepository) *SceneAnchorService {
	return &SceneAnchorService{repo: repo, shotRepo: shotRepo, aiSvc: aiSvc, novelRepo: novelRepo}
}

func (s *SceneAnchorService) WithChapterSceneAnchorRepo(r *repository.ChapterSceneAnchorRepository) *SceneAnchorService {
	s.chapterSceneAnchorRepo = r
	return s
}

// WithChapterRepo 注入章节仓库（可选，用于批量提取所有章节的场景锚点）
func (s *SceneAnchorService) WithChapterRepo(r *repository.ChapterRepository) *SceneAnchorService {
	s.chapterRepo = r
	return s
}

// ListChapterAnchors 返回绑定到指定章节的场景锚点列表
func (s *SceneAnchorService) ListChapterAnchors(novelID, chapterID uint) ([]*model.SceneAnchor, error) {
	if s.chapterSceneAnchorRepo == nil {
		return []*model.SceneAnchor{}, nil
	}
	bindings, err := s.chapterSceneAnchorRepo.ListByChapter(chapterID)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return []*model.SceneAnchor{}, nil
	}
	all, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	boundIDs := make(map[uint]bool, len(bindings))
	for _, b := range bindings {
		boundIDs[b.SceneAnchorID] = true
	}
	result := make([]*model.SceneAnchor, 0, len(bindings))
	for _, a := range all {
		if boundIDs[a.ID] {
			result = append(result, a)
		}
	}
	return result, nil
}

// BindChapterAnchor 手动绑定场景锚点到章节
func (s *SceneAnchorService) BindChapterAnchor(chapterID, novelID, anchorID uint) error {
	if s.chapterSceneAnchorRepo == nil {
		return fmt.Errorf("chapter scene anchor repository not configured")
	}
	return s.chapterSceneAnchorRepo.Upsert(&model.ChapterSceneAnchor{
		ChapterID: chapterID, NovelID: novelID, SceneAnchorID: anchorID,
	})
}

// UnbindChapterAnchor 解除章节与场景锚点的绑定
func (s *SceneAnchorService) UnbindChapterAnchor(chapterID, anchorID uint) error {
	if s.chapterSceneAnchorRepo == nil {
		return fmt.Errorf("chapter scene anchor repository not configured")
	}
	return s.chapterSceneAnchorRepo.Delete(chapterID, anchorID)
}

// CreateSceneAnchorReq 创建请求
type CreateSceneAnchorReq struct {
	Name           string `json:"name" binding:"required"`
	Type           string `json:"type"`
	Description    string `json:"description"`
	Variant        string `json:"variant"`
	ParentAnchorID *uint  `json:"parent_anchor_id"`
}

// UpdateSceneAnchorReq 更新请求
type UpdateSceneAnchorReq struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Description    string `json:"description"`
	Variant        string `json:"variant"`
	ParentAnchorID *uint  `json:"parent_anchor_id"`
}

func (s *SceneAnchorService) Get(id uint) (*model.SceneAnchor, error) {
	return s.repo.GetByID(id)
}

// GetByID 通过 ID 获取场景锚点（与 Get 等价，供内部服务调用）
func (s *SceneAnchorService) GetByID(id uint) (*model.SceneAnchor, error) {
	return s.repo.GetByID(id)
}

func (s *SceneAnchorService) ListByNovel(novelID uint) ([]*model.SceneAnchor, error) {
	return s.repo.ListByNovel(novelID)
}

func (s *SceneAnchorService) Create(tenantID, novelID uint, req CreateSceneAnchorReq) (*model.SceneAnchor, error) {
	anchor := &model.SceneAnchor{
		NovelID:        novelID,
		Name:           req.Name,
		Type:           req.Type,
		Description:    req.Description,
		Variant:        req.Variant,
		ParentAnchorID: req.ParentAnchorID,
	}
	if err := s.repo.Create(anchor); err != nil {
		return nil, err
	}
	return anchor, nil
}

func (s *SceneAnchorService) Update(id uint, req UpdateSceneAnchorReq) (*model.SceneAnchor, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		anchor.Name = req.Name
	}
	if req.Type != "" {
		anchor.Type = req.Type
	}
	if req.Description != "" {
		anchor.Description = req.Description
	}
	if req.Variant != "" {
		anchor.Variant = req.Variant
	}
	if req.ParentAnchorID != nil {
		anchor.ParentAnchorID = req.ParentAnchorID
	}
	if err := s.repo.Update(anchor); err != nil {
		return nil, err
	}
	return anchor, nil
}

func (s *SceneAnchorService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// SetRefImage 锁定参考图（强制覆盖）
func (s *SceneAnchorService) SetRefImage(id uint, imageURL string, shotID *uint) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		logger.Errorf("[SceneAnchorService] SetRefImage: getByID id=%d: %v", id, err)
		return err
	}
	anchor.RefImageURL = imageURL
	now := time.Now()
	anchor.RefImageLockedAt = &now
	if err := s.repo.Update(anchor); err != nil {
		logger.Errorf("[SceneAnchorService] SetRefImage: update id=%d: %v", id, err)
		return err
	}
	return nil
}

// AutoSetRefImage 首次自动锁定参考图（仅当 RefImageURL 为空时更新）
func (s *SceneAnchorService) AutoSetRefImage(id uint, imageURL string) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		logger.Errorf("[SceneAnchorService] AutoSetRefImage: getByID id=%d: %v", id, err)
		return err
	}
	if anchor.RefImageURL != "" {
		return nil // already locked
	}
	anchor.RefImageURL = imageURL
	now := time.Now()
	anchor.RefImageLockedAt = &now
	if err := s.repo.Update(anchor); err != nil {
		logger.Errorf("[SceneAnchorService] AutoSetRefImage: update id=%d: %v", id, err)
		return err
	}
	return nil
}

// BuildPromptFragment 返回拼接好的 prompt 片段和参考图URL
// 供分镜图像生成时注入 ImagePromptConfig
func (s *SceneAnchorService) BuildPromptFragment(id uint) (promptFragment string, refImageURL string, err error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return "", "", err
	}
	fragment := anchor.Description
	if anchor.Name != "" && fragment != "" {
		fragment = fmt.Sprintf("[scene: %s] %s", anchor.Name, fragment)
	}
	// 追加 PromptLock 锁定关键词（风格/色调/光线等约束）
	if anchor.PromptLock != "" {
		fragment = fragment + ", " + anchor.PromptLock
	}
	return fragment, anchor.RefImageURL, nil
}

// SetShotAnchor 绑定分镜到场景锚点
func (s *SceneAnchorService) SetShotAnchor(shotID uint, anchorID *uint) error {
	shot, err := s.shotRepo.GetByID(shotID)
	if err != nil {
		return err
	}
	shot.SceneAnchorID = anchorID
	return s.shotRepo.Update(shot)
}

// extractedAnchor LLM 返回的 JSON 锚点结构
type extractedAnchor struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Variant     string `json:"variant"`      // day/night/winter/battle 等变体标签
	ParentName  string `json:"parent_name"`  // 父级锚点名称（变体时填写，用于解析 ParentAnchorID）
}

// extractAnchorResponse 是新版 LLM 返回格式
type extractAnchorResponse struct {
	NewAnchors       []extractedAnchor `json:"new_anchors"`
	AppearingAnchors []string          `json:"appearing_anchors"`
}

// parseExtractAnchorResponse 解析 LLM 返回。
// 新格式：{"new_anchors":[...],"appearing_anchors":[...]}
// 旧格式（向后兼容）：bare array [...]
func parseExtractAnchorResponse(raw string) (extractAnchorResponse, error) {
	trimmed := strings.TrimSpace(raw)

	// Detect top-level JSON type before extracting.
	// When the LLM returns a bare array [...], extractJSONObject would truncate
	// it to the first element object {…}, causing the streaming-decoder fallback
	// (step 3) to spin indefinitely on object-internal ':' / ',' tokens that
	// dec.More() cannot distinguish from array separators.
	firstSig := strings.IndexAny(trimmed, "[{")
	var cleaned string
	if firstSig >= 0 && trimmed[firstSig] == '[' {
		cleaned = extractJSON(trimmed) // preserves full bare array
	} else {
		cleaned = extractJSONObject(trimmed) // preserves full object
	}

	// 1. 尝试新对象格式
	var resp extractAnchorResponse
	if err := json.Unmarshal([]byte(cleaned), &resp); err == nil && (len(resp.NewAnchors) > 0 || len(resp.AppearingAnchors) > 0) {
		return resp, nil
	}

	// 2. 向后兼容：bare array
	var arr []extractedAnchor
	if err := json.Unmarshal([]byte(cleaned), &arr); err == nil {
		return extractAnchorResponse{NewAnchors: arr}, nil
	}

	// 3. 部分恢复：streaming decoder — only for arrays.
	// Never run on objects: dec.More() returns true for ':' and ',' tokens
	// inside an object context, causing Decode to spin without advancing.
	if strings.HasPrefix(strings.TrimSpace(cleaned), "[") {
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, err := dec.Token(); err == nil {
			var partial []extractedAnchor
			for dec.More() {
				var item extractedAnchor
				if err := dec.Decode(&item); err == nil && item.Name != "" {
					partial = append(partial, item)
				}
			}
			if len(partial) > 0 {
				logger.Printf("[SceneAnchor] partial JSON recovery: got %d anchors", len(partial))
				return extractAnchorResponse{NewAnchors: partial}, nil
			}
		}
	}

	return extractAnchorResponse{}, fmt.Errorf("anchor JSON fully unparseable: %.200s", raw)
}

// ExtractFromChapter 调用 LLM 提取章节中的场景锚点，去重后批量创建。
// chapterID=0 表示不绑定章节；userPrompt 为空表示无附加指令。
func (s *SceneAnchorService) ExtractFromChapter(ctx context.Context, tenantID, novelID uint, novelTitle, chapterContent string, chapterID uint, userPrompt string) ([]*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] ExtractFromChapter: tenantID=%d novelID=%d chapterID=%d contentLen=%d",
		tenantID, novelID, chapterID, len(chapterContent))

	// 获取已存在锚点（去重用 + appearing 绑定用）
	existing, err := s.repo.ListByNovel(novelID)
	if err != nil {
		logger.Errorf("[SceneAnchorService] ExtractFromChapter: list existing anchors failed: %v", err)
		return nil, fmt.Errorf("list existing anchors: %w", err)
	}

	type existingEntry struct {
		Name        string
		Description string
	}
	existingEntries := make([]existingEntry, 0, len(existing))
	existingNames := make(map[string]bool, len(existing))
	existingNameToID := make(map[string]uint, len(existing)) // 规范化名→ID，用于绑定
	existingNameList := make([]string, 0, len(existing))
	for _, a := range existing {
		existingEntries = append(existingEntries, existingEntry{Name: a.Name, Description: a.Description})
		existingNames[a.Name] = true
		existingNameToID[strings.ToLower(a.Name)] = a.ID
		existingNameList = append(existingNameList, a.Name)
	}
	logger.Printf("[SceneAnchorService] ExtractFromChapter: novelID=%d existingAnchors=%d names=%v",
		novelID, len(existing), existingNameList)

	// 获取提示词语言配置
	promptLanguage := "zh"
	if s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(novelID); nErr == nil && novel.PromptLanguage != "" {
			promptLanguage = novel.PromptLanguage
		}
	}

	// 渲染 prompt
	anchorPrompt, err := renderPrompt("scene_anchor_extract", map[string]interface{}{
		"NovelTitle":      novelTitle,
		"ExistingAnchors": existingEntries,
		"ChapterContent":  truncateForPrompt(chapterContent, 8000),
		"PromptLanguage":  promptLanguage,
		"UserPrompt":      userPrompt,
	})
	if err != nil {
		logger.Errorf("[SceneAnchorService] ExtractFromChapter: render prompt failed: %v", err)
		return nil, fmt.Errorf("render scene_anchor_extract: %w", err)
	}

	// 调用 LLM（带租户 ID + ctx，确保使用正确的 provider 且可被超时/取消）
	jsonStr, err := s.aiSvc.generateJSONForTenantCtx(ctx, tenantID, novelID, "scene_anchor_extract", anchorPrompt, 2)
	if err != nil {
		logger.Errorf("[SceneAnchorService] ExtractFromChapter: LLM call failed: %v", err)
		return nil, fmt.Errorf("LLM extract anchors: %w", err)
	}
	logger.Printf("[SceneAnchorService] ExtractFromChapter: AI response len=%d raw=%.400s", len(jsonStr), jsonStr)

	// 解析 JSON（新格式：{new_anchors,appearing_anchors}；兼容旧裸数组格式）
	parsed, err := parseExtractAnchorResponse(jsonStr)
	if err != nil {
		logger.Errorf("[SceneAnchorService] ExtractFromChapter: JSON parse failed: %v, jsonStr=%q", err, jsonStr)
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}
	logger.Printf("[SceneAnchorService] ExtractFromChapter: parsed new_anchors=%d appearing_anchors=%v",
		len(parsed.NewAnchors), parsed.AppearingAnchors)

	// 构建规范化名称集合（用于语义去重：忽略大小写 + 空格）
	normalizedNames := make(map[string]bool, len(existing))
	for name := range existingNames {
		normalizedNames[normalizeAnchorName(name)] = true
	}

	// 批量创建新锚点（改进去重：精确匹配 + 规范化匹配 + 子串包含匹配）
	created := make([]*model.SceneAnchor, 0, len(parsed.NewAnchors))
	for _, e := range parsed.NewAnchors {
		if e.Name == "" {
			continue
		}
		normName := normalizeAnchorName(e.Name)
		if existingNames[e.Name] || normalizedNames[normName] || anchorNameOverlaps(normName, normalizedNames) {
			logger.Printf("[SceneAnchorService] ExtractFromChapter: skip duplicate anchor %q", e.Name)
			continue
		}
		anchorType := e.Type
		if anchorType == "" {
			anchorType = "exterior"
		}
		anchor := &model.SceneAnchor{
			NovelID:     novelID,
			Name:        e.Name,
			Type:        anchorType,
			Description: e.Description,
			Variant:     e.Variant,
		}
		if e.ParentName != "" {
			for _, a := range existing {
				if a.Name == e.ParentName || normalizeAnchorName(a.Name) == normalizeAnchorName(e.ParentName) {
					id := a.ID
					anchor.ParentAnchorID = &id
					break
				}
			}
		}
		if err := s.repo.Create(anchor); err != nil {
			logger.Errorf("[SceneAnchorService] ExtractFromChapter: create anchor %q: %v", e.Name, err)
			continue
		}
		logger.Printf("[SceneAnchorService] ExtractFromChapter: created anchor %q id=%d type=%s", anchor.Name, anchor.ID, anchor.Type)
		created = append(created, anchor)
		existingNames[e.Name] = true
		normalizedNames[normName] = true
		existingNameToID[strings.ToLower(e.Name)] = anchor.ID
	}

	logger.Printf("[SceneAnchorService] ExtractFromChapter done: novelID=%d created=%d appearing=%d chapterID=%d",
		novelID, len(created), len(parsed.AppearingAnchors), chapterID)

	// 若传入 chapterID，绑定新建锚点 + appearing 已有锚点到该章节
	if chapterID > 0 {
		chapID := chapterID
		if s.chapterSceneAnchorRepo == nil {
			logger.Errorf("[SceneAnchorService] chapterSceneAnchorRepo is nil, skipping chapter bindings for chapterID=%d", chapID)
		} else {
			// 绑定新建锚点
			for _, a := range created {
				if err := s.chapterSceneAnchorRepo.Upsert(&model.ChapterSceneAnchor{
					ChapterID: chapID, NovelID: novelID, SceneAnchorID: a.ID,
				}); err != nil {
					logger.Errorf("[SceneAnchorService] bind created anchor %d to chapter %d: %v", a.ID, chapID, err)
				} else {
					logger.Printf("[SceneAnchorService] bound new anchor %q (id=%d) to chapterID=%d", a.Name, a.ID, chapID)
				}
			}
			// 绑定 appearing 已有锚点（语义名称匹配）
			for _, name := range parsed.AppearingAnchors {
				anchorID, ok := existingNameToID[strings.ToLower(name)]
				if !ok {
					// 二次模糊查找：规范化匹配
					normName := normalizeAnchorName(name)
					for existingNorm, aid := range func() map[string]uint {
						m := make(map[string]uint, len(existing))
						for _, a := range existing {
							m[normalizeAnchorName(a.Name)] = a.ID
						}
						return m
					}() {
						if existingNorm == normName {
							anchorID = aid
							ok = true
							break
						}
					}
				}
				if !ok {
					logger.Printf("[SceneAnchorService] appearing anchor %q not found in novel %d, skipping", name, novelID)
					continue
				}
				if err := s.chapterSceneAnchorRepo.Upsert(&model.ChapterSceneAnchor{
					ChapterID: chapID, NovelID: novelID, SceneAnchorID: anchorID,
				}); err != nil {
					logger.Errorf("[SceneAnchorService] bind appearing anchor %d %q to chapter %d: %v", anchorID, name, chapID, err)
				} else {
					logger.Printf("[SceneAnchorService] bound existing anchor %q (id=%d) to chapterID=%d", name, anchorID, chapID)
				}
			}
		}
	}

	return created, nil
}

// GenerateRefImage 使用 AI 图像生成为锚点生成参考图并锁定
func (s *SceneAnchorService) GenerateRefImage(ctx context.Context, tenantID, id uint, providerName string) (*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] GenerateRefImage: tenantID=%d anchorID=%d provider=%s", tenantID, id, providerName)
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		logger.Errorf("[SceneAnchorService] GenerateRefImage: getByID id=%d: %v", id, err)
		return nil, fmt.Errorf("anchor not found: %w", err)
	}

	// 查询小说的图片风格（用于模型选择）和标题（用于 OSS 路径）
	imageStyle := ""
	aspectRatio := ""
	if s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(anchor.NovelID); nErr != nil {
			logger.Errorf("[SceneAnchorService] GenerateRefImage: fetch novel novelID=%d: %v (using defaults)", anchor.NovelID, nErr)
		} else {
			imageStyle = novel.ImageStyle
			aspectRatio = novel.VideoConf().VideoAspectRatio
			if novel.Title != "" {
				ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title})
			}
		}
	}

	// 组装图像生成 prompt（注入场景描述 + PromptLock + 标准化场景生成词）
	prompt := anchor.Description
	if anchor.PromptLock != "" {
		prompt += ", " + anchor.PromptLock
	}
	sceneType := anchor.Type
	if sceneType == "" {
		sceneType = "exterior"
	}
	sceneSuffix := "establishing shot, " + sceneType + " scene, no humans, no people, no figures, " +
		"cinematic composition, three depth layers foreground midground background, " +
		"architectural detail, atmospheric lighting, masterpiece, best quality, ultra-detailed, 8k uhd, " +
		"photorealistic, sharp focus"
	if prompt != "" {
		prompt += ", " + sceneSuffix
	} else {
		prompt = sceneSuffix
	}
	// negative prompt 通过 imageStyle 字段携带（GenerateCharacterThreeView 支持）
	// 此处额外追加场景专用 negative 词到 prompt 注释段（不同模型处理方式不同，此为兜底）
	negativeHint := " | negative: person, people, human, man, woman, figure, silhouette, character, " +
		"blurry, low quality, watermark, text, floating objects, modern elements"
	prompt += negativeHint

	sizeOverride := imageAspectRatioToSize(aspectRatio, "master")
	imageURL, err := s.aiSvc.GenerateCharacterThreeView(ctx, tenantID, providerName, prompt, "", imageStyle, "", sizeOverride)
	if err != nil {
		logger.Errorf("[SceneAnchorService] GenerateRefImage: AI generate failed anchorID=%d: %v", id, err)
		return nil, fmt.Errorf("generate ref image: %w", err)
	}

	if err := s.SetRefImage(id, imageURL, nil); err != nil {
		logger.Errorf("[SceneAnchorService] GenerateRefImage: save ref image anchorID=%d url=%s: %v", id, imageURL, err)
		return nil, fmt.Errorf("save ref image: %w", err)
	}

	logger.Printf("[SceneAnchorService] GenerateRefImage: done anchorID=%d url=%s", id, imageURL)
	return s.repo.GetByID(id)
}

// EditRefImageWithInstruction 以文生图模型（DreamO）重新生成参考图，原图作为参考保持风格一致性。
// instruction 为自然语言提示词，如"让场景更暗，增加烟雾"
func (s *SceneAnchorService) EditRefImageWithInstruction(ctx context.Context, tenantID, id uint, instruction string) (*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] EditRefImageWithInstruction: tenantID=%d anchorID=%d instruction=%.100s", tenantID, id, instruction)
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		logger.Errorf("[SceneAnchorService] EditRefImageWithInstruction: getByID id=%d: %v", id, err)
		return nil, fmt.Errorf("anchor not found: %w", err)
	}
	if anchor.RefImageURL == "" {
		return nil, fmt.Errorf("no ref image to edit; generate one first")
	}

	// 读取小说画面风格，保持风格一致
	imageStyle := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(anchor.NovelID); e == nil {
			imageStyle = novel.ImageStyle
		}
	}

	// consistencyWeight=0.7 → GenerateCharacterThreeViewMulti 自动选用 DreamO（文生图+参考图）
	// scale = weight * 10 = 7（中等参考强度，兼顾提示词创意与原图风格一致性）
	imageURL, err := s.aiSvc.GenerateCharacterThreeViewMulti(
		ctx, tenantID, "", instruction,
		[]string{anchor.RefImageURL},
		imageStyle, "", "", 0.7,
	)
	if err != nil {
		logger.Errorf("[SceneAnchorService] EditRefImageWithInstruction: AI generate failed anchorID=%d: %v", id, err)
		return nil, fmt.Errorf("edit ref image: %w", err)
	}

	if err := s.SetRefImage(id, imageURL, nil); err != nil {
		logger.Errorf("[SceneAnchorService] EditRefImageWithInstruction: save edited ref image anchorID=%d: %v", id, err)
		return nil, fmt.Errorf("save edited ref image: %w", err)
	}
	logger.Printf("[SceneAnchorService] EditRefImageWithInstruction: anchor %d edited, url=%s", id, imageURL)
	return s.repo.GetByID(id)
}

// UpdateStats 更新锚点使用统计（usage_count++，avg_cons_score 滚动平均）
func (s *SceneAnchorService) UpdateStats(id uint, score float64) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	n := float64(anchor.UsageCount)
	anchor.AvgConsScore = (anchor.AvgConsScore*n + score) / (n + 1)
	anchor.UsageCount++
	return s.repo.Update(anchor)
}

// BatchGenerateRefImages 批量为小说的场景锚点生成参考图。
// force=false：跳过已有参考图的锚点；force=true：全量重新生成（风格变更时使用）。
// 外层并发度固定为 3（避免大批量时无限创建 goroutine），内层 imageSem 进一步限流。
func (s *SceneAnchorService) BatchGenerateRefImages(ctx context.Context, tenantID, novelID uint, provider string, force bool, progressFn func(int)) (succeeded, failed int, err error) {
	anchors, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return 0, 0, fmt.Errorf("list anchors: %w", err)
	}

	var todo []*model.SceneAnchor
	for _, a := range anchors {
		if force || a.RefImageURL == "" {
			todo = append(todo, a)
		}
	}
	total := len(todo)

	const outerConcurrency = 3
	sem := make(chan struct{}, outerConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int

	for _, anchor := range todo {
		anchor := anchor
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if _, genErr := s.GenerateRefImage(ctx, tenantID, anchor.ID, provider); genErr != nil {
				logger.Errorf("[SceneAnchorService] BatchGenerateRefImages: anchor %d (%s) failed: %v", anchor.ID, anchor.Name, genErr)
				mu.Lock()
				failed++
				done++
				cur := done
				mu.Unlock()
				if progressFn != nil && total > 0 {
					progressFn(cur * 99 / total)
				}
				return
			}
			mu.Lock()
			succeeded++
			done++
			cur := done
			mu.Unlock()
			if progressFn != nil && total > 0 {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[SceneAnchorService] BatchGenerateRefImages: novelID=%d succeeded=%d failed=%d", novelID, succeeded, failed)
	return succeeded, failed, nil
}

// AIExtractAllFromNovel 批量从小说所有章节中提取场景锚点（并发 3 goroutine）。
// 无章节数量上限，支持增量提取（已有同名锚点自动跳过）。
func (s *SceneAnchorService) AIExtractAllFromNovel(ctx context.Context, tenantID, novelID uint, progressFn func(int)) ([]*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] AIExtractAllFromNovel: novelID=%d", novelID)
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapterRepo not configured")
	}
	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, fmt.Errorf("list chapters: %w", err)
	}

	novelTitle := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
		}
	}

	// 收集所有有内容的章节（无数量上限，支持全量提取）
	var candidates []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" {
			candidates = append(candidates, ch)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	const maxConcurrent = 3
	sem := make(chan struct{}, maxConcurrent)
	var mu sync.Mutex
	var wg sync.WaitGroup
	allCreated := make([]*model.SceneAnchor, 0)
	failCount := 0
	total := len(candidates)
	var done int

	for _, ch := range candidates {
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			anchors, err := s.ExtractFromChapter(ctx, tenantID, novelID, novelTitle, ch.Content, 0, "")
			mu.Lock()
			done++
			if err != nil {
				logger.Errorf("[SceneAnchorService] AIExtractAllFromNovel chapter %d: %v", ch.ID, err)
				failCount++
			} else {
				allCreated = append(allCreated, anchors...)
			}
			cur := done
			mu.Unlock()
			if progressFn != nil && total > 0 {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[SceneAnchorService] AIExtractAllFromNovel done: novelID=%d total=%d created=%d failed=%d", novelID, total, len(allCreated), failCount)
	if failCount == len(candidates) {
		return nil, fmt.Errorf("所有章节场景锚点提取均失败，请检查 AI 提供商配置")
	}
	if failCount > 0 {
		logger.Errorf("[SceneAnchorService] AIExtractAllFromNovel: partial failure novelID=%d failed=%d/%d", novelID, failCount, total)
	}
	return allCreated, nil
}

// normalizeAnchorName 规范化场景名称用于去重比较（转小写，去除多余空格）
func normalizeAnchorName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), ""))
}

// anchorNameOverlaps 检测 normName 与 existing 集合中是否存在高重叠（防止同质化锚点）。
// 若 normName 是某个已有名称的子串，或某个已有名称是 normName 的子串，视为重叠。
func anchorNameOverlaps(normName string, existing map[string]bool) bool {
	for en := range existing {
		if len(en) >= 2 && len(normName) >= 2 {
			if strings.Contains(normName, en) || strings.Contains(en, normName) {
				return true
			}
		}
	}
	return false
}
