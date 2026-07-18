package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ─── ScreenplayService ────────────────────────────────────────────────────────
// 分场剧本：一章拆分为多场（ScreenplayScene），每场再拆分为多个分镜（由 VideoService 消费）。
// 升级自原来纯内存的 P1a 节拍表（generateBeatSheet）——现在落库、可人工审校/锁定、可复用。

type ScreenplayService struct {
	repo          *repository.ScreenplaySceneRepository
	chapterRepo   *repository.ChapterRepository
	novelRepo     *repository.NovelRepository
	characterRepo *repository.CharacterRepository
	anchorRepo    *repository.SceneAnchorRepository
	aiSvc         *AIService
	versionRepo   *repository.ScreenplaySceneVersionRepository // optional：注入后覆盖场次前会落一条历史快照
}

func NewScreenplayService(
	repo *repository.ScreenplaySceneRepository,
	chapterRepo *repository.ChapterRepository,
	novelRepo *repository.NovelRepository,
	characterRepo *repository.CharacterRepository,
	anchorRepo *repository.SceneAnchorRepository,
	aiSvc *AIService,
) *ScreenplayService {
	return &ScreenplayService{
		repo: repo, chapterRepo: chapterRepo, novelRepo: novelRepo,
		characterRepo: characterRepo, anchorRepo: anchorRepo, aiSvc: aiSvc,
	}
}

// WithVersionRepo 注入分场剧本历史版本仓库（可选：未注入时覆盖场次不会保留历史快照）。
func (s *ScreenplayService) WithVersionRepo(repo *repository.ScreenplaySceneVersionRepository) *ScreenplayService {
	s.versionRepo = repo
	return s
}

// screenplaySceneSnapshot 是覆盖某场次前存入 ScreenplaySceneVersion.Content 的 JSON 快照结构，
// 字段对应 GenerateScreenplayScenes 里 UpdateFields 会覆盖的那些字段。
type screenplaySceneSnapshot struct {
	Heading            string              `json:"heading"`
	Synopsis           string              `json:"synopsis"`
	SceneAnchorID      *uint               `json:"scene_anchor_id"`
	CharacterIDs       model.JSONUintSlice `json:"character_ids"`
	Beats              string              `json:"beats"`
	EstimatedShotCount int                 `json:"estimated_shot_count"`
}

// snapshotSceneBeforeOverwrite 在原地覆盖某场次前落一条历史版本记录（best-effort：失败只记日志，
// 不阻断调用方本身的操作）。changeType 标注这次覆盖的原因（"regenerate"=重新生成剧本覆盖，
// "restore"=恢复到历史版本前保留当前内容）。
func (s *ScreenplayService) snapshotSceneBeforeOverwrite(old *model.ScreenplayScene, changeType string) {
	if s.versionRepo == nil {
		return
	}
	content, err := json.Marshal(screenplaySceneSnapshot{
		Heading:            old.Heading,
		Synopsis:           old.Synopsis,
		SceneAnchorID:      old.SceneAnchorID,
		CharacterIDs:       old.CharacterIDs,
		Beats:              old.Beats,
		EstimatedShotCount: old.EstimatedShotCount,
	})
	if err != nil {
		logger.Errorf("[ScreenplayService] marshal snapshot for scene id=%d: %v", old.ID, err)
		return
	}
	if err := s.versionRepo.CreateAtomic(&model.ScreenplaySceneVersion{
		ScreenplaySceneID: old.ID,
		ChapterID:         old.ChapterID,
		NovelID:           old.NovelID,
		Content:           string(content),
		ChangeType:        changeType,
	}); err != nil {
		logger.Errorf("[ScreenplayService] create version for scene id=%d: %v", old.ID, err)
	}
}

// screenplaySceneJSON / screenplayBeatJSON 对应 AI 输出的 JSON 结构（字段名与 prompt 输出一致）。
type screenplayBeatJSON struct {
	BeatType        string `json:"beat_type"`
	ActionLine      string `json:"action_line"`
	DialogueSpeaker string `json:"dialogue_speaker"`
	DialogueLine    string `json:"dialogue_line"`
}

type screenplaySceneJSON struct {
	SceneNo        int                  `json:"scene_no"`
	Heading        string               `json:"heading"`
	Synopsis       string               `json:"synopsis"`
	EstimatedShots int                  `json:"estimated_shots"`
	Beats          []screenplayBeatJSON `json:"beats"`
}

// GenerateScreenplayScenes 从章节内容生成分场剧本并落库。
// 已锁定（Locked）的场次不受影响：锁定场次的 scene_no 会被跳过复用（生成结果里遇到已被锁定
// 场次占用的编号会顺延）。
//
// preserveEdited 为 true 时，未锁定但已被人工编辑过（Edited）的场次也一并保护，不被覆盖。
// 用户在界面上点击"生成剧本"时应传 false，遵循"未锁定=可覆盖"的语义（覆盖前会自动落一条
// 历史版本快照，被覆盖的内容不会真正丢失，可在"历史版本"里恢复）。
//
// 重要：同一 scene_no 已存在旧场次时原地更新（保留其数据库 ID），而不是删除重建——
// StoryboardShot.ScreenplaySceneID 等下游数据通过场次 ID 引用场次，删除重建会让这些引用
// 静默失效（分镜表面上"消失"，实际是指向了一个已不存在的旧 ID）。只有当新生成结果的场次数
// 少于旧场次数时，才删除多出来、不再被使用的旧 scene_no 行。
func (s *ScreenplayService) GenerateScreenplayScenes(tenantID, chapterID uint, providerName string, preserveEdited bool) ([]*model.ScreenplayScene, error) {
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}

	existing, err := s.repo.ListByChapter(chapterID)
	if err != nil {
		return nil, fmt.Errorf("list existing scenes: %w", err)
	}
	existingByNo := make(map[int]*model.ScreenplayScene, len(existing))
	protectedByNo := make(map[int]*model.ScreenplayScene, len(existing))
	var maxProtectedNo int
	for _, sc := range existing {
		existingByNo[sc.SceneNo] = sc
		if sc.Locked || (preserveEdited && sc.Edited) {
			protectedByNo[sc.SceneNo] = sc
			if sc.SceneNo > maxProtectedNo {
				maxProtectedNo = sc.SceneNo
			}
		}
	}

	var characters []*model.Character
	if s.characterRepo != nil {
		characters, _ = s.characterRepo.ListByNovel(chapter.NovelID)
	}
	var anchors []*model.SceneAnchor
	if s.anchorRepo != nil {
		anchors, _ = s.anchorRepo.ListByNovel(chapter.NovelID)
	}

	type promptChar struct{ Name, Role string }
	var promptChars []promptChar
	for _, c := range characters {
		promptChars = append(promptChars, promptChar{Name: c.Name, Role: c.Role})
	}
	type promptAnchor struct{ Name string }
	var promptAnchors []promptAnchor
	for _, a := range anchors {
		promptAnchors = append(promptAnchors, promptAnchor{Name: a.Name})
	}

	rendered, tplErr := renderPrompt("screenplay_generate", map[string]interface{}{
		"Content":    chapter.Content,
		"Characters": promptChars,
		"Anchors":    promptAnchors,
	})
	if tplErr != nil {
		return nil, fmt.Errorf("render screenplay_generate: %w", tplErr)
	}

	result, err := s.aiSvc.GenerateWithProvider(tenantID, chapter.NovelID, "screenplay_generate", rendered, providerName)
	if err != nil {
		return nil, fmt.Errorf("AI generate screenplay: %w", err)
	}

	parsed, err := parseScreenplayResult(result)
	if err != nil {
		return nil, fmt.Errorf("parse screenplay result: %w", err)
	}

	// 场景锚点名称匹配（沿用分镜阶段同款的大小写不敏感包含匹配）
	anchorMap := make(map[string]uint, len(anchors))
	for _, a := range anchors {
		anchorMap[strings.ToLower(a.Name)] = a.ID
	}
	matchAnchor := func(heading string) *uint {
		h := strings.ToLower(heading)
		for name, id := range anchorMap {
			if strings.Contains(h, name) {
				id := id
				return &id
			}
		}
		return nil
	}
	charIDByName := make(map[string]uint, len(characters))
	for _, c := range characters {
		charIDByName[c.Name] = c.ID
	}

	// 受保护的场次（已锁定，或 preserveEdited=true 时的已编辑场次）原样保留，不做任何改动。
	sceneNo := maxProtectedNo
	usedNos := make(map[int]bool, len(parsed.Scenes)+len(protectedByNo))
	for no := range protectedByNo {
		usedNos[no] = true
	}
	for _, sj := range parsed.Scenes {
		if _, occupied := protectedByNo[sj.SceneNo]; occupied {
			continue // 让位给受保护场次，不覆盖
		}
		sceneNo++
		usedNos[sceneNo] = true
		var charIDs model.JSONUintSlice
		beatLines := make([]string, 0, len(sj.Beats))
		for _, bj := range sj.Beats {
			if bj.BeatType == "dialogue" {
				beatLines = append(beatLines, fmt.Sprintf("%s：%s", bj.DialogueSpeaker, bj.DialogueLine))
			} else {
				beatLines = append(beatLines, bj.ActionLine)
			}
			if bj.DialogueSpeaker != "" {
				if id, ok := charIDByName[bj.DialogueSpeaker]; ok {
					charIDs = appendUniqueUint(charIDs, id)
				}
			}
		}
		beats := strings.Join(beatLines, "\n")

		if old, ok := existingByNo[sceneNo]; ok {
			// 同一 scene_no 已有旧场次：原地更新内容，保留其 ID，避免下游按 ID 的引用失效。
			// 覆盖前先落一条历史快照，供用户在"历史版本"里查看/恢复。
			s.snapshotSceneBeforeOverwrite(old, "regenerate")
			fields := map[string]interface{}{
				"heading":              sj.Heading,
				"synopsis":             sj.Synopsis,
				"scene_anchor_id":      matchAnchor(sj.Heading),
				"character_ids":        charIDs,
				"beats":                beats,
				"estimated_shot_count": sj.EstimatedShots,
				"edited":               false, // AI 已重新生成覆盖内容，不再算作"人工编辑过"
			}
			if err := s.repo.UpdateFields(old.ID, fields); err != nil {
				logger.Errorf("[ScreenplayService] update scene id=%d chapterID=%d sceneNo=%d: %v", old.ID, chapterID, sceneNo, err)
			}
			continue
		}

		scene := &model.ScreenplayScene{
			ChapterID: chapterID, NovelID: chapter.NovelID,
			SceneNo: sceneNo, Heading: sj.Heading, Synopsis: sj.Synopsis,
			SceneAnchorID:      matchAnchor(sj.Heading),
			CharacterIDs:       charIDs,
			Beats:              beats,
			EstimatedShotCount: sj.EstimatedShots,
		}
		if err := s.repo.Create(scene); err != nil {
			logger.Errorf("[ScreenplayService] create scene chapterID=%d sceneNo=%d: %v", chapterID, sceneNo, err)
		}
	}

	// 新生成结果比旧场次数少：删除不再被使用的多余旧 scene_no 行（保护场次已在 usedNos 里，不会被误删）。
	for no, old := range existingByNo {
		if !usedNos[no] {
			if err := s.repo.Delete(old.ID); err != nil {
				logger.Errorf("[ScreenplayService] delete stale scene id=%d chapterID=%d sceneNo=%d: %v", old.ID, chapterID, no, err)
			}
		}
	}

	return s.repo.ListByChapter(chapterID)
}

func appendUniqueUint(s model.JSONUintSlice, v uint) model.JSONUintSlice {
	for _, existing := range s {
		if existing == v {
			return s
		}
	}
	return append(s, v)
}

func parseScreenplayResult(raw string) (*struct {
	Scenes []screenplaySceneJSON `json:"scenes"`
}, error) {
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "{"); idx > 0 {
		raw = raw[idx:]
	}
	if idx := strings.LastIndex(raw, "}"); idx >= 0 && idx < len(raw)-1 {
		raw = raw[:idx+1]
	}
	var parsed struct {
		Scenes []screenplaySceneJSON `json:"scenes"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON (raw=%.500q): %w", raw, err)
	}
	return &parsed, nil
}

// ListScenes 返回一章的全部分场剧本（按场次顺序）。
func (s *ScreenplayService) ListScenes(chapterID uint) ([]*model.ScreenplayScene, error) {
	return s.repo.ListByChapter(chapterID)
}

// UpdateScene 更新分场剧本内容（人工审校：heading/synopsis/beats）。
// 强制打上 edited=true 标记，供前端展示"已编辑"提示（不影响是否被覆盖，覆盖保护只看 Locked）。
func (s *ScreenplayService) UpdateScene(id uint, fields map[string]interface{}) (*model.ScreenplayScene, error) {
	fields["edited"] = true
	if err := s.repo.UpdateFields(id, fields); err != nil {
		return nil, err
	}
	return s.repo.GetByID(id)
}

// SetLocked 锁定/解锁分场剧本：锁定后重新生成剧本时该场内容不会被覆盖。
func (s *ScreenplayService) SetLocked(id uint, locked bool) (*model.ScreenplayScene, error) {
	if err := s.repo.UpdateFields(id, map[string]interface{}{"locked": locked}); err != nil {
		return nil, err
	}
	return s.repo.GetByID(id)
}

func (s *ScreenplayService) DeleteScene(id uint) error {
	return s.repo.Delete(id)
}

func (s *ScreenplayService) GetScene(id uint) (*model.ScreenplayScene, error) {
	return s.repo.GetByID(id)
}

// ListScenesByNovel 返回一部小说下的全部分场剧本（按章节+场次顺序），供剧集列表展示场次明细使用。
func (s *ScreenplayService) ListScenesByNovel(novelID uint) ([]*model.ScreenplayScene, error) {
	return s.repo.ListByNovel(novelID)
}

// GetSceneVersions 返回某场次的全部历史版本（按版本号倒序）。
func (s *ScreenplayService) GetSceneVersions(sceneID uint) ([]*model.ScreenplaySceneVersion, error) {
	if s.versionRepo == nil {
		return nil, fmt.Errorf("version history not available")
	}
	return s.versionRepo.List(sceneID)
}

// RestoreSceneVersion 把某场次恢复到指定历史版本的内容（不改变场次 ID/scene_no）。恢复前会把
// 当前内容也落一条历史快照，所以恢复本身可逆——用户可以在历史版本间反复切换。
func (s *ScreenplayService) RestoreSceneVersion(sceneID uint, versionNo int) (*model.ScreenplayScene, error) {
	if s.versionRepo == nil {
		return nil, fmt.Errorf("version history not available")
	}
	version, err := s.versionRepo.GetVersion(sceneID, versionNo)
	if err != nil {
		return nil, fmt.Errorf("version not found: %w", err)
	}
	var snap screenplaySceneSnapshot
	if err := json.Unmarshal([]byte(version.Content), &snap); err != nil {
		return nil, fmt.Errorf("invalid version snapshot: %w", err)
	}
	current, err := s.repo.GetByID(sceneID)
	if err != nil {
		return nil, fmt.Errorf("scene not found: %w", err)
	}
	s.snapshotSceneBeforeOverwrite(current, "restore")
	fields := map[string]interface{}{
		"heading":              snap.Heading,
		"synopsis":             snap.Synopsis,
		"scene_anchor_id":      snap.SceneAnchorID,
		"character_ids":        snap.CharacterIDs,
		"beats":                snap.Beats,
		"estimated_shot_count": snap.EstimatedShotCount,
		"edited":               true,
	}
	if err := s.repo.UpdateFields(sceneID, fields); err != nil {
		return nil, err
	}
	return s.repo.GetByID(sceneID)
}
