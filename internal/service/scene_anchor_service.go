package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"

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
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
}

// UpdateSceneAnchorReq 更新请求
type UpdateSceneAnchorReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *SceneAnchorService) GetByID(id uint) (*model.SceneAnchor, error) {
	return s.repo.GetByID(id)
}

func (s *SceneAnchorService) ListByNovel(novelID uint) ([]*model.SceneAnchor, error) {
	return s.repo.ListByNovel(novelID)
}

// Create 创建场景锚点。
// novel_id+name 有唯一索引（idx_scene_anchor_novel_name），而删除锚点是软删除（deleted_at
// 置位，行仍占用这个唯一索引）——删除后用同名重新创建会撞唯一索引报 MySQL 1062 错误。这里
// 先查一次（含软删除记录）：命中软删除记录就恢复并用新请求覆盖字段，命中活跃记录则返回明确
// 的重名错误，而不是让原始 SQL 错误往上抛。
func (s *SceneAnchorService) Create(tenantID, novelID uint, req CreateSceneAnchorReq) (*model.SceneAnchor, error) {
	if existing, err := s.repo.FindByNovelAndNameUnscoped(novelID, req.Name); err == nil && existing != nil {
		if !existing.DeletedAt.Valid {
			return nil, fmt.Errorf("场景「%s」已存在", req.Name)
		}
		if err := s.repo.RestoreByID(existing.ID); err != nil {
			return nil, fmt.Errorf("restore soft-deleted scene anchor: %w", err)
		}
		existing.DeletedAt.Valid = false
		existing.Description = req.Description
		// 清空旧的参考图：用户是在创建一个"新"场景（只是复用了同名的旧行），不应该带着
		// 已删除锚点残留的参考图，否则画面和新填的描述对不上。
		existing.RefImageURL = ""
		existing.RefImageLockedAt = nil
		if err := s.repo.Update(existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	anchor := &model.SceneAnchor{
		NovelID:     novelID,
		Name:        req.Name,
		Description: req.Description,
	}
	if err := s.repo.Create(anchor); err != nil {
		return nil, err
	}
	return anchor, nil
}

// GenerateSceneAnchorInfo 根据场景名称（及用户可选的草稿描述提示）AI 生成场景的视觉描述。
// 用于"新建场景"弹窗的一键填充：不依赖章节内容，仅返回生成结果，不落库，由前端展示后随用户确认的表单一起走 Create 创建。
// 生成的 description 会直接作为 image_scene_ref 模板生成参考图时的核心视觉约束注入 prompt（见 GenerateRefImage），
// 因此要求覆盖建筑/材质/色彩/光线等视觉维度，而非叙事性文字。
func (s *SceneAnchorService) GenerateSceneAnchorInfo(tenantID, novelID uint, name, userHint string) (string, error) {
	novelTitle, novelGenre := novelPromptContext(s.novelRepo, novelID)

	rendered, tplErr := renderPrompt("generate_scene_anchor_info", map[string]interface{}{
		"NovelTitle": novelTitle,
		"Genre":      novelGenre,
		"AnchorName": name,
		"UserHint":   userHint,
	})
	if tplErr != nil {
		return "", fmt.Errorf("render generate_scene_anchor_info: %w", tplErr)
	}

	result, genErr := s.aiSvc.GenerateWithProvider(tenantID, "generate_scene_anchor_info", rendered)
	if genErr != nil {
		return "", fmt.Errorf("AI generate scene anchor info: %w", genErr)
	}

	type sceneInfoJSON struct {
		Description string `json:"description"`
	}
	var info sceneInfoJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if parseErr := json.Unmarshal([]byte(cleaned), &info); parseErr != nil {
		logger.Errorf("[SceneAnchorService] GenerateSceneAnchorInfo: parse error: %v, raw: %.300s", parseErr, result)
		return "", fmt.Errorf("parse scene anchor info JSON: %w", parseErr)
	}
	return info.Description, nil
}

func (s *SceneAnchorService) Update(id uint, req UpdateSceneAnchorReq) (*model.SceneAnchor, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		anchor.Name = req.Name
	}
	if req.Description != "" {
		logger.Printf("[SceneAnchorService] Update id=%d: description BEFORE len=%d prev=%.120q", id, len(anchor.Description), anchor.Description)
		logger.Printf("[SceneAnchorService] Update id=%d: description AFTER  len=%d new=%.120q", id, len(req.Description), req.Description)
		anchor.Description = req.Description
	} else {
		logger.Printf("[SceneAnchorService] Update id=%d: req.Description is empty, NOT updated (DB has len=%d val=%.80q)", id, len(anchor.Description), anchor.Description)
	}
	if err := s.repo.Update(anchor); err != nil {
		return nil, err
	}
	logger.Printf("[SceneAnchorService] Update id=%d: saved OK, description len=%d", id, len(anchor.Description))
	return anchor, nil
}

func (s *SceneAnchorService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// AutoSetRefImage 首次自动锁定参考图（仅当 RefImageURL 为空时更新）。
// 同 SetRefImage，若 imageURL 是临时签名 URL 则先转存 OSS。
func (s *SceneAnchorService) AutoSetRefImage(ctx context.Context, id uint, imageURL string) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		logger.Errorf("[SceneAnchorService] AutoSetRefImage: getByID id=%d: %v", id, err)
		return err
	}
	if anchor.RefImageURL != "" {
		return nil // already locked
	}
	imageURL, err = s.persistIfEphemeral(ctx, imageURL)
	if err != nil {
		logger.Errorf("[SceneAnchorService] AutoSetRefImage: id=%d: %v", id, err)
		return err
	}
	now := time.Now()
	if err := s.repo.UpdateFields(id, map[string]interface{}{
		"ref_image_url":       imageURL,
		"ref_image_locked_at": now,
	}); err != nil {
		logger.Errorf("[SceneAnchorService] AutoSetRefImage: update id=%d: %v", id, err)
		return err
	}
	return nil
}

// persistIfEphemeral 检测临时签名 URL（Volcengine TOS 等），若匹配则通过 AIService 转存到 OSS 并返回永久 URL。
// 非临时 URL（OSS、本地路径等）直接原样返回，避免重复下载/上传开销。
//
// 转存失败时必须返回 error 而不是静默回退到原始临时 URL：ref_image_url 会被长期锁定复用，
// 若在此处放行一个仍带 X-Tos-Expires 的签名 URL，几小时后签名过期，后续所有引用该锚点的分镜生成
// 都会以 403 Download Url Error 失败——比在写入前直接报错更难排查、影响面也更大。
func (s *SceneAnchorService) persistIfEphemeral(ctx context.Context, imageURL string) (string, error) {
	if s.aiSvc == nil || imageURL == "" {
		return imageURL, nil
	}
	// Volcengine TOS 签名 URL 特征：包含 X-Tos-Algorithm 或 X-Tos-Expires 查询参数
	if strings.Contains(imageURL, "X-Tos-Algorithm") || strings.Contains(imageURL, "X-Tos-Expires") {
		persisted := s.aiSvc.PersistExternalImage(ctx, imageURL)
		if persisted == imageURL || strings.Contains(persisted, "X-Tos-Algorithm") || strings.Contains(persisted, "X-Tos-Expires") {
			return "", fmt.Errorf("转存参考图到永久存储失败，图片链接为临时签名 URL，过期后将无法访问：%s", imageURL)
		}
		logger.Printf("[SceneAnchorService] persistIfEphemeral: TOS URL → OSS %s", persisted)
		return persisted, nil
	}
	return imageURL, nil
}

// BuildPromptFragment 返回拼接好的 prompt 片段、参考图URL 和锚点名称。
// 供分镜图像/视频生成时注入 prompt 并做参考图编号替换。
func (s *SceneAnchorService) BuildPromptFragment(id uint) (promptFragment string, refImageURL string, anchorName string, err error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return "", "", "", err
	}
	anchorName = anchor.Name
	fragment := anchor.Description
	if anchor.Name != "" && fragment != "" {
		fragment = fmt.Sprintf("[场景：%s] %s", anchor.Name, fragment)
	}
	return fragment, anchor.RefImageURL, anchorName, nil
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
	Description string `json:"description"`
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
	if err := json.Unmarshal([]byte(cleaned), &resp); err == nil && strings.HasPrefix(strings.TrimSpace(cleaned), "{") {
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

	// 渲染 prompt
	anchorPrompt, err := renderPrompt("scene_anchor_extract", map[string]interface{}{
		"NovelTitle":      novelTitle,
		"ExistingAnchors": existingEntries,
		"ChapterContent":  truncateForPrompt(chapterContent, 8000),
		"UserPrompt":      userPrompt,
	})
	if err != nil {
		logger.Errorf("[SceneAnchorService] ExtractFromChapter: render prompt failed: %v", err)
		return nil, fmt.Errorf("render scene_anchor_extract: %w", err)
	}

	// 调用 LLM（带租户 ID + ctx，确保使用正确的 provider 且可被超时/取消）
	jsonStr, err := s.aiSvc.GenerateWithProviderCtx(ctx, tenantID, "scene_anchor_extract", anchorPrompt)
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
		anchor := &model.SceneAnchor{
			NovelID:     novelID,
			Name:        e.Name,
			Description: e.Description,
		}
		if err := s.repo.Create(anchor); err != nil {
			logger.Errorf("[SceneAnchorService] ExtractFromChapter: create anchor %q: %v", e.Name, err)
			continue
		}
		logger.Printf("[SceneAnchorService] ExtractFromChapter: created anchor %q id=%d", anchor.Name, anchor.ID)
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

// AIAnalyzeSceneAnchorResult AI 分析返回的建议字段（不含 name，name 由用户维护）
type AIAnalyzeSceneAnchorResult struct {
	Description string `json:"description"` // 视觉提示词
}

// AIAnalyze 用 AI 分析场景锚点，返回建议参数（不自动保存，由前端填入表单后用户确认）
func (s *SceneAnchorService) AIAnalyze(ctx context.Context, tenantID, id uint) (*AIAnalyzeSceneAnchorResult, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("anchor not found: %w", err)
	}

	novelTitle := ""
	novelDesc := ""
	novelGenre := ""
	if novel, nErr := s.novelRepo.GetByID(anchor.NovelID); nErr == nil {
		novelTitle = novel.Title
		novelDesc = novel.Meta.Description
		novelGenre = novel.Meta.Genre // 与 NovelDesc 合并传给模板
	}
	_ = novelGenre // 合入 NovelDesc 字段，避免 unused 警告

	// 搜索提到该场景名称的章节片段（最多取 3 章，每章截取前后 500 字）
	var excerpts []string
	if chapters, cErr := s.chapterRepo.ListByNovelWithContent(anchor.NovelID); cErr == nil {
		for _, ch := range chapters {
			if ch.Content == "" || !strings.Contains(ch.Content, anchor.Name) {
				continue
			}
			content := ch.Content
			if idx := strings.Index(content, anchor.Name); idx >= 0 {
				lo := idx - 500
				if lo < 0 {
					lo = 0
				}
				hi := idx + 500
				if hi > len(content) {
					hi = len(content)
				}
				excerpts = append(excerpts, fmt.Sprintf("（第%d章节选）…%s…", ch.ChapterNo, content[lo:hi]))
			}
			if len(excerpts) >= 3 {
				break
			}
		}
	}

	// 渲染 scene_anchor_analyze 模板（与 scene_anchor_extract 使用相同的描述规则）
	prompt, err := renderPrompt("scene_anchor_analyze", map[string]interface{}{
		"AnchorName":          anchor.Name,
		"NovelTitle":          novelTitle,
		"NovelDesc":           truncateForPrompt(novelDesc+novelGenre, 400),
		"ChapterExcerpts":     excerpts,
		"ExistingDescription": truncateForPrompt(anchor.Description, 400),
	})
	if err != nil {
		return nil, fmt.Errorf("render scene_anchor_analyze: %w", err)
	}

	jsonStr, err := s.aiSvc.GenerateWithProviderCtx(ctx, tenantID, "scene_anchor_analyze", prompt)
	if err != nil {
		return nil, fmt.Errorf("AI analyze: %w", err)
	}

	clean := extractJSON(jsonStr)
	var result AIAnalyzeSceneAnchorResult
	if err := json.Unmarshal([]byte(clean), &result); err != nil {
		return nil, fmt.Errorf("parse AI response: %w (raw: %.200s)", err, jsonStr)
	}

	return &result, nil
}

// sceneRefFormatRules 是场景参考图的版式+规则文案，占位符依次为：
// nameQuoted（标题旁引号内的场景名，可为空）、titleNote（顶部标题标牌的补充说明，可为空）。
const sceneRefFormatRules = "格式：场景概念设计稿%s，横版16:9，多角度布局。至少4个面板从不同摄影角度/方向展示同一场景：全景定场镜头、对向视角、俯瞰/鸟瞰视角、低角度或细节特写。如有剧情需要，各面板可变化光照或时间段。顶部包含标题标牌%s，底部包含氛围色条。不添加细节标注或平面图——完全聚焦于全场景视图。" +
	"规则：不包含角色或人物——纯环境展示。场景在所有面板中必须看起来一致且可辨认（相同的建筑、道具、布局）。不添加水印或多余文字。"

// GenerateRefImage 使用 AI 图像生成为锚点生成参考图并锁定
// descriptionOverride 非空时优先于数据库中的 anchor.Description（用于编辑框尚未保存的最新内容），为空则回退查库。
func (s *SceneAnchorService) GenerateRefImage(ctx context.Context, tenantID, id uint, descriptionOverride string) (*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] GenerateRefImage: tenantID=%d anchorID=%d", tenantID, id)
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		logger.Errorf("[SceneAnchorService] GenerateRefImage: getByID id=%d: %v", id, err)
		return nil, fmt.Errorf("anchor not found: %w", err)
	}
	description := descriptionOverride
	if description == "" {
		description = anchor.Description
	}

	// 查询小说的图片风格（用于模型选择）、画面比例和标题（用于 OSS 路径）
	imageStyle := ""
	sizeOverride := ""
	if s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(anchor.NovelID); nErr != nil {
			logger.Errorf("[SceneAnchorService] GenerateRefImage: fetch novel novelID=%d: %v (using defaults)", anchor.NovelID, nErr)
		} else {
			imageStyle = novel.AIConfig.ImageStyle
			if novel.VideoConfig != nil && novel.VideoConfig.Config.VideoAspectRatio != "" {
				sizeOverride = novel.VideoConfig.Config.VideoAspectRatio // e.g. "16:9", "9:16", "1:1"
			}
		}
	}

	logger.Printf("[SceneAnchorService] GenerateRefImage: anchorID=%d description_len=%d description=%.200q", id, len(description), description)
	nameQuoted, titleNote := "", ""
	if anchor.Name != "" {
		nameQuoted = fmt.Sprintf("\"%s\"", anchor.Name)
		titleNote = fmt.Sprintf("，居中显示加粗场景标题\"%s\"", anchor.Name)
	}
	formatRules := fmt.Sprintf(sceneRefFormatRules, nameQuoted, titleNote)

	rendered, tplErr := renderPrompt("image_scene_ref", map[string]interface{}{
		"Description": description,
		"FormatRules": formatRules,
	})
	if tplErr != nil {
		return nil, fmt.Errorf("render image_scene_ref: %w", tplErr)
	}

	resp, err := s.aiSvc.GenerateImage(ctx, tenantID, &ImageGenerationOptions{
		Prompt:          rendered,
		Size:            sizeOverride,
		ReferenceImages: []string{anchor.RefImageURL},
		ImageStyle:      imageStyle,
	})
	if err != nil {
		logger.Errorf("[SceneAnchorService] GenerateRefImage: AI generate failed anchorID=%d: %v", id, err)
		return nil, fmt.Errorf("generate ref image: %w", err)
	}

	if err := s.AutoSetRefImage(ctx, id, resp.URL); err != nil {
		logger.Errorf("[SceneAnchorService] GenerateRefImage: save ref image anchorID=%d url=%s: %v", id, resp.URL, err)
		return nil, fmt.Errorf("save ref image: %w", err)
	}

	logger.Printf("[SceneAnchorService] GenerateRefImage: done anchorID=%d url=%s", id, resp.URL)
	return s.repo.GetByID(id)
}

// BatchGenerateRefImages 批量为小说的场景锚点生成参考图。
// force=false：跳过已有参考图的锚点；force=true：全量重新生成（风格变更时使用）。
// 外层并发度固定为 3（避免大批量时无限创建 goroutine），内层 imageSem 进一步限流。
func (s *SceneAnchorService) BatchGenerateRefImages(ctx context.Context, tenantID, novelID uint, force bool, progressFn func(int)) (succeeded, failed int, err error) {
	anchors, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return 0, 0, fmt.Errorf("list anchors: %w", err)
	}

	var todo []uint
	for _, a := range anchors {
		if force || a.RefImageURL == "" {
			todo = append(todo, a.ID)
		}
	}
	return s.GenerateChapterRefImages(ctx, tenantID, novelID, todo, progressFn)
}

// GenerateChapterRefImages 仅为本章绑定的选定场景锚点生成参考图，不影响该小说的其他场景。
// anchorIDs 与 novelID 做交集校验，避免跨小说/租户的越权生成。
func (s *SceneAnchorService) GenerateChapterRefImages(ctx context.Context, tenantID, novelID uint, anchorIDs []uint, progressFn func(int)) (succeeded, failed int, err error) {
	all, e := s.repo.ListByNovel(novelID)
	if e != nil {
		return 0, 0, fmt.Errorf("list anchors: %w", e)
	}
	idSet := make(map[uint]bool, len(anchorIDs))
	for _, id := range anchorIDs {
		idSet[id] = true
	}
	var todo []*model.SceneAnchor
	for _, a := range all {
		if idSet[a.ID] {
			todo = append(todo, a)
		}
	}
	total := len(todo)
	if total == 0 {
		return 0, 0, nil
	}

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
			if _, genErr := s.GenerateRefImage(ctx, tenantID, anchor.ID, ""); genErr != nil {
				logger.Errorf("[SceneAnchorService] GenerateChapterRefImages: anchor %d (%s) failed: %v", anchor.ID, anchor.Name, genErr)
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
	logger.Printf("[SceneAnchorService] GenerateChapterRefImages: novelID=%d succeeded=%d failed=%d", novelID, succeeded, failed)
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
