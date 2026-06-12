package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// registerTaskResumeHandlers registers resume functions for idempotent task types.
// Must be called after all services are fully wired.
func registerTaskResumeHandlers(svcs *Services, repos *Repositories) {
	// sfx_gen: SFX tag analysis + batch generation (idempotent — skips shots that already have tags/sfx)
	if svcs.SFXService != nil && svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeSFXGen, func(t *model.AsyncTask) {
			videoID := t.EntityID
			if videoID == 0 {
				return
			}
			// Parse saved params
			var params struct {
				UserContext string `json:"user_context"`
				Lang        string `json:"lang"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Lang == "" {
				params.Lang = "zh"
			}

			shots, err := svcs.VideoService.GetStoryboard(videoID)
			if err != nil || len(shots) == 0 {
				svcs.TaskService.Fail(t.TaskID, "storyboard not found on resume") //nolint:errcheck
				return
			}

			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)        //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 5) //nolint:errcheck

			ctx := context.Background()
			if err := svcs.SFXService.AnalyzeSFXForVideo(ctx, shots, tenantID, params.UserContext, params.Lang, false); err != nil {
				logger.Errorf("TaskService resume sfx_gen %s: analyze failed: %v", t.TaskID, err)
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 50) //nolint:errcheck

			progressFn := func(pct int) {
				overall := 50 + pct*45/100
				svcs.TaskService.UpdateProgress(t.TaskID, overall) //nolint:errcheck
			}
			success, fail, failedIDs := svcs.SFXService.BatchAutoGenerateSFX(ctx, shots, tenantID, params.UserContext, "", progressFn)
			logger.Printf("TaskService resume sfx_gen %s done: sfx_success=%d sfx_fail=%d", t.TaskID, success, fail)
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"sfx_success": success, "sfx_fail": fail, "failed_shot_ids": failedIDs}) //nolint:errcheck
		})
	}

	// three_view: batch (novel entity) or single character
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeThreeView, func(t *model.AsyncTask) {
			tenantID := t.TenantID
			if t.EntityType == "novel" {
				// Batch: skip characters that already have images
				novelID := t.EntityID
				if novelID == 0 {
					return
				}
				var params struct {
					Provider string `json:"provider"`
					Force    bool   `json:"force"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
				}
				svcs.TaskService.SetRunning(t.TaskID)                                                           //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) }                 //nolint:errcheck
				succ, fail, err := svcs.CharacterService.BatchGenerateImages(tenantID, novelID, params.Provider, params.Force, progressFn)
				if err != nil {
					logger.Errorf("TaskService resume three_view %s failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					logger.Printf("TaskService resume three_view %s done: succeeded=%d failed=%d", t.TaskID, succ, fail)
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
				}
				return
			}
			if t.EntityType == "character" && svcs.ImageGenerationService != nil {
				charID := t.EntityID
				if charID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				var params struct {
					Provider string `json:"provider"`
					Style    string `json:"style"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
				}
				char, err := svcs.CharacterService.GetCharacter(charID)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "character not found: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				ctx := context.Background()
				novelTitle := svcs.CharacterService.GetNovelTitle(char.NovelID)
				if novelTitle != "" {
					ctx = service.WithImageStorageHint(ctx, service.ImageStorageHint{NovelTitle: novelTitle})
				}
				defaultLook, _ := svcs.CharacterService.GetDefaultLook(charID)
				appearance := char.Description
				if defaultLook != nil && defaultLook.VisualPrompt != "" {
					appearance = defaultLook.VisualPrompt
				}
				gender := service.InferGenderTag(appearance, char.Description)
				img, err := svcs.ImageGenerationService.GenerateThreeViewSheet(ctx, tenantID, char.Name, appearance, params.Style, gender, "", params.Provider)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "generate three-view sheet failed: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 99) //nolint:errcheck
				threeURL := img.URL
				lookReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &threeURL}
				var updatedLook *model.CharacterLook
				if defaultLook != nil {
					updatedLook, err = svcs.CharacterService.UpdateLook(defaultLook.ID, lookReq)
				} else {
					updatedLook, err = svcs.CharacterService.CreateLook(charID, char.NovelID, &model.CreateCharacterLookRequest{
						Label: "默认形象", SetAsDefault: true, ChapterFrom: 1, ThreeViewSheet: threeURL,
					})
				}
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "save three-view sheet failed: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
					"look":      updatedLook,
					"generated": map[string]string{"sheet": img.URL},
				})
				return
			}
			svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
		})
	}

	// chapter_char_extract: extract minor characters from a single chapter
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterCharExtract, func(t *model.AsyncTask) {
			chapterID := t.EntityID
			if chapterID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				NovelID uint `json:"novel_id"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			chars, err := svcs.CharacterService.AIExtractMinorChars(t.TenantID, params.NovelID, chapterID)
			if err != nil {
				logger.Errorf("TaskService resume chapter_char_extract %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"new_count": len(chars)}) //nolint:errcheck
			}
		})
	}

	// char_reanalyze: reanalyze a single character
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharReanalyze, func(t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			char, err := svcs.CharacterService.ReanalyzeCharacter(t.TenantID, charID)
			if err != nil {
				logger.Errorf("TaskService resume char_reanalyze %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"character_id": char.ID}) //nolint:errcheck
			}
		})
	}

	// char_gen: AI batch generate characters for a novel (idempotent — overwrites existing)
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharGen, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			chars, err := svcs.CharacterService.AIBatchGenerate(tenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume char_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(chars)}) //nolint:errcheck
			}
		})
	}

	// item_extract: AI extract items from novel (idempotent — overwrites existing)
	if svcs.ItemService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeItemExtract, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			items, err := svcs.ItemService.AIExtractFromNovel(tenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume item_extract %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(items)}) //nolint:errcheck
			}
		})
	}

	// plot_extract: AI extract plot points from novel (idempotent — overwrites existing)
	if svcs.PlotPointService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypePlotExtract, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			points, err := svcs.PlotPointService.AIExtractFromNovel(tenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume plot_extract %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(points)}) //nolint:errcheck
			}
		})
	}

	// chapter_summary_batch: batch generate chapter summaries (idempotent — skips chapters with existing summaries)
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterSummaryBatch, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)                                               //nolint:errcheck
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) }     //nolint:errcheck
			count, err := svcs.ChapterService.BatchGenerateSummaries(tenantID, novelID, progressFn)
			if err != nil {
				logger.Errorf("TaskService resume chapter_summary_batch %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": count}) //nolint:errcheck
			}
		})
	}

	// storyboard_review: AI review storyboard (creates a new review record, safe to re-run)
	if svcs.StoryboardService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeStoryboardReview, func(t *model.AsyncTask) {
			videoID := t.EntityID
			if videoID == 0 {
				return
			}
			var params struct {
				Provider      string  `json:"provider"`
				PreviousScore float64 `json:"previous_score"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			review, recordID, err := svcs.StoryboardService.ReviewStoryboard(tenantID, videoID, params.Provider, params.PreviousScore)
			if err != nil {
				logger.Errorf("TaskService resume storyboard_review %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			type reviewResult struct {
				*model.StoryboardReview
				RecordID uint `json:"record_id,omitempty"`
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 90)                                                          //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, &reviewResult{StoryboardReview: review, RecordID: recordID}) //nolint:errcheck
		})
	}

	// chapter_review: AI review chapter content (creates a new review record, safe to re-run)
	if svcs.QualityControlService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterReview, func(t *model.AsyncTask) {
			chapterID := t.EntityID
			if chapterID == 0 {
				return
			}
			var params struct {
				Provider string `json:"provider"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			_ = tenantID // QualityControlService.ReviewChapter does not take tenantID
			review, err := svcs.QualityControlService.ReviewChapter(context.Background(), chapterID, params.Provider)
			if err != nil {
				logger.Errorf("TaskService resume chapter_review %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 90) //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, review)    //nolint:errcheck
		})
	}

	// face_closeup: re-generate face closeup for a single character
	if svcs.CharacterService != nil && svcs.ImageGenerationService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeFaceCloseup, func(t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				return
			}
			var params struct {
				Provider string `json:"provider"`
				Style    string `json:"style"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			char, err := svcs.CharacterService.GetCharacter(charID)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "character not found: "+err.Error()) //nolint:errcheck
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			ctx := context.Background()
			novelTitle := svcs.CharacterService.GetNovelTitle(char.NovelID)
			if novelTitle != "" {
				ctx = service.WithImageStorageHint(ctx, service.ImageStorageHint{NovelTitle: novelTitle})
			}
			defaultLook, _ := svcs.CharacterService.GetDefaultLook(charID)
			appearance := char.Description
			ref := char.Portrait
			if defaultLook != nil {
				if defaultLook.VisualPrompt != "" {
					appearance = defaultLook.VisualPrompt
				}
				if defaultLook.Portrait != "" {
					ref = defaultLook.Portrait
				} else if defaultLook.ThreeViewSheet != "" {
					ref = defaultLook.ThreeViewSheet
				}
			}
			gender := service.InferGenderTag(appearance, char.Description)
			img, err := svcs.ImageGenerationService.GenerateFaceCloseupImage(ctx, tenantID, char.Name, appearance, params.Style, gender, ref, params.Provider)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "generate face closeup failed: "+err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 99) //nolint:errcheck
			faceURL := img.URL
			lookReq := &model.UpdateCharacterLookRequest{FaceCloseup: &faceURL, Portrait: &faceURL}
			var updatedLook *model.CharacterLook
			if defaultLook != nil {
				updatedLook, err = svcs.CharacterService.UpdateLook(defaultLook.ID, lookReq)
			} else {
				updatedLook, err = svcs.CharacterService.CreateLook(charID, char.NovelID, &model.CreateCharacterLookRequest{
					Label: "默认形象", SetAsDefault: true, ChapterFrom: 1, FaceCloseup: faceURL, Portrait: faceURL,
				})
			}
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "save face closeup failed: "+err.Error()) //nolint:errcheck
				return
			}
			// 同步 Character.Portrait
			portraitReq := &model.UpdateCharacterRequest{Name: char.Name, Portrait: img.URL}
			svcs.CharacterService.UpdateCharacter(charID, char.TenantID, portraitReq) //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
				"look":      updatedLook,
				"generated": map[string]string{"face_closeup": img.URL},
			})
		})
	}

	// voice_gen: batch voice (video entity) or single segment
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeVoiceGen, func(t *model.AsyncTask) {
			var params struct {
				NarrationVoice string `json:"narration_voice"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			tenantID := t.TenantID

			if t.EntityType == "segment" {
				segID := t.EntityID
				if segID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
				if err := svcs.VideoService.GenerateSegmentAudio(segID, tenantID, params.NarrationVoice); err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, nil) //nolint:errcheck
				}
				return
			}

			// Video batch: re-run, skip shots that already have audio
			videoID := t.EntityID
			if videoID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			shots, err := svcs.VideoService.GetStoryboard(videoID)
			if err != nil || len(shots) == 0 {
				svcs.TaskService.Fail(t.TaskID, "storyboard not found on resume") //nolint:errcheck
				return
			}
			var targets []*model.StoryboardShot
			for _, s := range shots {
				if s.Narration == "" && s.Dialogue == "" && s.Description == "" {
					continue
				}
				if s.AudioPath != "" {
					continue // skip already-voiced
				}
				targets = append(targets, s)
			}
			if len(targets) == 0 {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"success": 0, "fail": 0, "total": 0}) //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			total := len(targets)
			var doneCount atomic.Int32
			const voiceBatchSize = 5
			for i := 0; i < total; i += voiceBatchSize {
				end := i + voiceBatchSize
				if end > total {
					end = total
				}
				var wg sync.WaitGroup
				for _, shot := range targets[i:end] {
					wg.Add(1)
					go func(s *model.StoryboardShot) {
						defer wg.Done()
						if err := svcs.VideoService.GenerateShotAudio(s, tenantID, params.NarrationVoice); err != nil {
							logger.Errorf("TaskService resume voice_gen %s shot %d: %v", t.TaskID, s.ShotNo, err)
						}
						done := int(doneCount.Add(1))
						svcs.TaskService.UpdateProgress(t.TaskID, done*100/total) //nolint:errcheck
					}(shot)
				}
				wg.Wait()
				if end < total {
					time.Sleep(1 * time.Second)
				}
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"total": total}) //nolint:errcheck
		})
	}

	// image_gen: routed by source param
	if svcs.ItemService != nil && svcs.SceneAnchorService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeImageGen, func(t *model.AsyncTask) {
			var params struct {
				Source   string `json:"source"`
				Provider string `json:"provider"`
				Force    bool   `json:"force"`
				RefURL   string `json:"ref_url"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			tenantID := t.TenantID
			switch params.Source {
			case "item_batch":
				novelID := t.EntityID
				if novelID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)                                           //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
				succ, fail, err := svcs.ItemService.BatchGenerateImages(tenantID, novelID, params.Provider, params.Force, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
				}
			case "item_single":
				itemID := t.EntityID
				if itemID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
				item, err := svcs.ItemService.GenerateItemImage(tenantID, itemID, params.RefURL, params.Provider)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.UpdateProgress(t.TaskID, 90) //nolint:errcheck
					svcs.TaskService.Complete(t.TaskID, item)      //nolint:errcheck
				}
			case "scene_anchor_batch":
				novelID := t.EntityID
				if novelID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)                                           //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
				succ, fail, err := svcs.SceneAnchorService.BatchGenerateRefImages(context.Background(), tenantID, novelID, params.Provider, params.Force, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
				}
			default:
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
			}
		})
	}

	// chapter_scene_extract: AI extract scene anchors from a single chapter
	if svcs.SceneAnchorService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterSceneExtract, func(t *model.AsyncTask) {
			chapterID := t.EntityID
			if chapterID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				NovelID  uint   `json:"novel_id"`
				Content  string `json:"content"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Content == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			anchors, err := svcs.SceneAnchorService.ExtractFromChapter(context.Background(), t.TenantID, params.NovelID, "", params.Content, chapterID)
			if err != nil {
				logger.Errorf("TaskService resume chapter_scene_extract %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"new_count": len(anchors)}) //nolint:errcheck
			}
		})
	}

	// scene_anchor_extract: AI extract all scene anchors from novel
	if svcs.SceneAnchorService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeSceneAnchorExtract, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)                                             //nolint:errcheck
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) }   //nolint:errcheck
			anchors, err := svcs.SceneAnchorService.AIExtractAllFromNovel(tenantID, novelID, progressFn)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(anchors)}) //nolint:errcheck
			}
		})
	}

	// bgm_analyze and bgm_generate
	if svcs.BGMService != nil && svcs.VideoService != nil {
		bgmResume := func(generate bool) func(*model.AsyncTask) {
			return func(t *model.AsyncTask) {
				videoID := t.EntityID
				if videoID == 0 {
					return
				}
				var params struct {
					UserPrompt string `json:"user_prompt"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
				}
				shots, err := svcs.VideoService.GetStoryboard(videoID)
				if err != nil || len(shots) == 0 {
					svcs.TaskService.Fail(t.TaskID, "storyboard not found on resume") //nolint:errcheck
					return
				}
				tenantID := t.TenantID
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				ctx := context.Background()
				if !generate {
					segs, err := svcs.BGMService.AnalyzeBGMForVideo(ctx, shots, repos.VideoBGMSegmentRepo, videoID, tenantID, params.UserPrompt)
					if err != nil {
						svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					} else {
						svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(segs)}) //nolint:errcheck
					}
				} else {
					progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
					segs, err := svcs.BGMService.GenerateBGMSegments(ctx, shots, repos.VideoBGMSegmentRepo, videoID, tenantID, params.UserPrompt, progressFn)
					if err != nil {
						svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					} else {
						matched := 0
						for _, s := range segs {
							if s.URL != "" {
								matched++
							}
						}
						svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"total": len(segs), "matched": matched}) //nolint:errcheck
					}
				}
			}
		}
		svcs.TaskService.RegisterResumeHandler("bgm_analyze", bgmResume(false))
		svcs.TaskService.RegisterResumeHandler("bgm_generate", bgmResume(true))
	}

	// storyboard_gen: re-run full storyboard generation
	if svcs.StoryboardService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeStoryboardGen, func(t *model.AsyncTask) {
			videoID := t.EntityID
			if videoID == 0 {
				return
			}
			var params struct {
				ChapterID      uint     `json:"chapter_id"`
				Characters     []string `json:"characters"`
				Style          string   `json:"style"`
				Provider       string   `json:"provider"`
				UserPrompt     string   `json:"user_prompt"`
				MaxTokens      int      `json:"max_tokens"`
				Temperature    float64  `json:"temperature"`
				TimeoutSeconds int      `json:"timeout_seconds"`
				VoiceMode      string   `json:"voice_mode"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			svcs.TaskService.SetRunning(t.TaskID)                                             //nolint:errcheck
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) }   //nolint:errcheck
			overrides := service.StoryboardOverrides{
				MaxTokens:      params.MaxTokens,
				Temperature:    params.Temperature,
				TimeoutSeconds: params.TimeoutSeconds,
				VoiceMode:      params.VoiceMode,
			}
			result, err := svcs.StoryboardService.GenerateStoryboard(videoID, params.ChapterID, params.Characters, params.Style, params.Provider, params.UserPrompt, progressFn, overrides)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			var shotCount int
			if shots, ok := result.([]*model.StoryboardShot); ok {
				shotCount = len(shots)
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": shotCount}) //nolint:errcheck
		})
	}

	// storyboard_optimize: re-run from saved review JSON
	if svcs.StoryboardService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeStoryboardOptimize, func(t *model.AsyncTask) {
			videoID := t.EntityID
			if videoID == 0 {
				return
			}
			if t.ParamsJSON == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Review   model.StoryboardReview `json:"review"`
				Provider string                 `json:"provider"`
			}
			if err := json.Unmarshal([]byte(t.ParamsJSON), &params); err != nil {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			if len(params.Review.GlobalSuggestions) == 0 && len(params.Review.ShotFeedback) == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			count, err := svcs.StoryboardService.OptimizeStoryboardFromReview(tenantID, videoID, &params.Review, params.Provider)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.UpdateProgress(t.TaskID, 90)                                       //nolint:errcheck
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"updated_shots": count}) //nolint:errcheck
			}
		})
	}

	// novel_outline_gen: regenerate novel outline
	if svcs.NovelService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeNovelOutlineGen, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			result, err := svcs.NovelService.GenerateOutline(t.TenantID, &service.GenerateOutlineRequest{
				NovelID:    novelID,
				ChapterNum: 10,
			})
			if err != nil {
				logger.Errorf("TaskService resume novel_outline_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"outline": result}) //nolint:errcheck
			}
		})
	}

	// char_image_gen: re-generate character image
	if svcs.ImageGenerationService != nil && svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharImageGen, func(t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Type    string `json:"type"`
				Emotion string `json:"emotion"`
				Action  string `json:"action"`
				Style   string `json:"style"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			char, err := svcs.CharacterService.GetCharacter(charID)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "character not found: "+err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			image, err := svcs.ImageGenerationService.GenerateCharacterImage(&model.GenerateImageRequest{
				Subject:     char.Name,
				Description: char.Description,
				Type:        params.Type,
				Emotion:     params.Emotion,
				Action:      params.Action,
				Style:       params.Style,
			})
			if err != nil {
				logger.Errorf("TaskService resume char_image_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"image": image}) //nolint:errcheck
			}
		})
	}

	// char_profile_gen: re-generate character profile from description
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharProfileGen, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Description string `json:"description"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Description == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			character, err := svcs.CharacterService.GenerateProfile(t.TenantID, novelID, params.Description)
			if err != nil {
				logger.Errorf("TaskService resume char_profile_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"character": character}) //nolint:errcheck
			}
		})
	}

	// voice_preview: re-generate voice preview audio
	if svcs.AIService != nil && svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeVoicePreview, func(t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Text      string  `json:"text"`
				VoiceID   string  `json:"voice_id"`
				VoiceSpeed float64 `json:"voice_speed"`
				VoiceStyle string  `json:"voice_style"`
				VoiceLang  string  `json:"voice_lang"`
				CharName   string  `json:"char_name"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Text == "" || params.VoiceID == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			rawURL, err := svcs.AIService.AudioGenerateWithOptions(ctx, t.TenantID, params.Text, params.VoiceID, params.VoiceSpeed, params.VoiceStyle, params.VoiceLang)
			if err != nil {
				logger.Errorf("TaskService resume voice_preview %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, "voice generation failed: "+err.Error()) //nolint:errcheck
				return
			}
			playURL := rawURL
			if len(rawURL) > 7 && rawURL[:7] == "file://" {
				filePath := rawURL[7:]
				if data, readErr := os.ReadFile(filePath); readErr == nil && len(data) > 0 {
					playURL = "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(data)
				}
			}
			svcs.CharacterService.UpdateCharacter(charID, t.TenantID, &model.UpdateCharacterRequest{ //nolint:errcheck
				Name:        params.CharName,
				VoiceSample: rawURL,
			})
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"audio_url": playURL, "voice_id": params.VoiceID, "voice_speed": params.VoiceSpeed}) //nolint:errcheck
		})
	}

	// look_prompt_gen: re-generate look visual prompt
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeLookPromptGen, func(t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Description string `json:"description"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Description == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			prompt, err := svcs.CharacterService.GenerateLookVisualPrompt(t.TenantID, charID, params.Description)
			if err != nil {
				logger.Errorf("TaskService resume look_prompt_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"visual_prompt": prompt}) //nolint:errcheck
			}
		})
	}

	// look_image_gen: re-generate look images (face closeup or three view)
	if svcs.ImageGenerationService != nil && svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeLookImageGen, func(t *model.AsyncTask) {
			lookID := t.EntityID
			if lookID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Type     string `json:"type"`
				CharID   uint   `json:"char_id"`
				Provider string `json:"provider"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			look, err := svcs.CharacterService.GetLook(lookID)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "look not found: "+err.Error()) //nolint:errcheck
				return
			}
			char, err := svcs.CharacterService.GetCharacter(params.CharID)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "character not found: "+err.Error()) //nolint:errcheck
				return
			}
			visualPrompt := look.VisualPrompt
			if visualPrompt == "" {
				visualPrompt = char.Description
			}
			style := svcs.CharacterService.GetNovelImageStyle(char.NovelID)
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			ctx := context.Background()
			tenantID := t.TenantID
			var updatedLook *model.CharacterLook
			switch params.Type {
			case "face_closeup", "portrait", "":
				img, err := svcs.ImageGenerationService.GenerateFaceCloseupImage(ctx, tenantID, char.Name, visualPrompt, style, "", look.Portrait, params.Provider)
				if err != nil {
					logger.Errorf("TaskService resume look_image_gen %s face_closeup failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					return
				}
				imageURL := img.URL
				updateReq := &model.UpdateCharacterLookRequest{FaceCloseup: &imageURL, Portrait: &imageURL}
				updatedLook, _ = svcs.CharacterService.UpdateLook(lookID, updateReq)
			case "three_view":
				img, err := svcs.ImageGenerationService.GenerateThreeViewSheet(ctx, tenantID, char.Name, visualPrompt, style, "", look.ThreeViewSheet, params.Provider)
				if err != nil {
					logger.Errorf("TaskService resume look_image_gen %s three_view failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					return
				}
				imageURL := img.URL
				updateReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &imageURL}
				updatedLook, _ = svcs.CharacterService.UpdateLook(lookID, updateReq)
			default:
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"look": updatedLook}) //nolint:errcheck
		})
	}

	// cover_image_gen: re-generate novel cover image
	if svcs.NovelService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCoverImageGen, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Suggestion string `json:"suggestion"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			url, err := svcs.NovelService.GenerateCoverImage(ctx, t.TenantID, novelID, params.Suggestion)
			if err != nil {
				logger.Errorf("TaskService resume cover_image_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"url": url}) //nolint:errcheck
			}
		})
	}

	// image_edit: re-run image editing with saved instruction
	if svcs.AIService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeImageEdit, func(t *model.AsyncTask) {
			var params struct {
				ImageURL    string `json:"image_url"`
				Instruction string `json:"instruction"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.ImageURL == "" || params.Instruction == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			newURL, err := svcs.AIService.EditImageWithInstruction(context.Background(), t.TenantID, params.ImageURL, params.Instruction)
			if err != nil {
				logger.Errorf("TaskService resume image_edit %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, "failed to edit image: "+err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"image_url": newURL}) //nolint:errcheck
			}
		})
	}

	// chapter_gen: re-run chapter generation with saved request
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterGen, func(t *model.AsyncTask) {
			if t.ParamsJSON == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				NovelID uint                         `json:"novel_id"`
				Req     model.GenerateChapterRequest `json:"req"`
			}
			if err := json.Unmarshal([]byte(t.ParamsJSON), &params); err != nil || params.NovelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)        //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 5) //nolint:errcheck
			chapter, err := svcs.ChapterService.GenerateChapter(tenantID, params.NovelID, &params.Req)
			if err != nil {
				logger.Errorf("TaskService resume chapter_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 90)                                               //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"chapter": chapter}) //nolint:errcheck
		})
	}

	// asset_gen: routed by source param
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeAssetGen, func(t *model.AsyncTask) {
			var params struct {
				Source      string `json:"source"`
				VideoID     uint   `json:"video_id"`
				ShotID      uint   `json:"shot_id"`
				ShotIDs     []uint `json:"shot_ids"`
				QualityTier string `json:"quality_tier"`
				Provider    string `json:"provider"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck

			switch params.Source {
			case "single_shot":
				videoID, shotID := params.VideoID, params.ShotID
				if videoID == 0 || shotID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
				shot, err := svcs.VideoService.GenerateSingleShot(videoID, shotID, params.Provider)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.UpdateProgress(t.TaskID, 90)                                                          //nolint:errcheck
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_id": shot.ID, "status": shot.Status}) //nolint:errcheck
				}
			case "batch_shots":
				videoID := t.EntityID
				if videoID == 0 || len(params.ShotIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				shots, err := svcs.VideoService.BatchGenerateShots(videoID, params.ShotIDs, params.QualityTier, progressFn, params.Provider)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": len(shots)}) //nolint:errcheck
				}
			case "batch_images":
				videoID := t.EntityID
				if videoID == 0 || len(params.ShotIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				shots, err := svcs.VideoService.BatchGenerateShotImages(videoID, params.ShotIDs, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": len(shots)}) //nolint:errcheck
				}
			case "batch_clips":
				videoID := t.EntityID
				if videoID == 0 || len(params.ShotIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				shots, err := svcs.VideoService.BatchGenerateShotClips(videoID, params.ShotIDs, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": len(shots)}) //nolint:errcheck
				}
			default:
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
			}
		})
	}

	// novel_analysis
	if svcs.NovelAnalysisService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeNovelAnalysis, func(t *model.AsyncTask) {
			var p struct {
				CreateOutlines bool `json:"create_outlines"`
			}
			_ = json.Unmarshal([]byte(t.ParamsJSON), &p)
			svcs.NovelAnalysisService.ResumeAnalysis(t, p.CreateOutlines)
		})
	}

	// rewrite_analysis + rewrite_chapters
	if svcs.RewriteService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeRewriteAnalysis, func(t *model.AsyncTask) {
			svcs.RewriteService.ResumeAnalysis(t)
		})
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeRewriteChapters, func(t *model.AsyncTask) {
			svcs.RewriteService.ResumeRewriting(t)
		})
	}

	// import / crawl_job: cannot safely re-run after restart (would create duplicates)
	svcs.TaskService.RegisterResumeHandler(service.TaskTypeImport, func(t *model.AsyncTask) {
		svcs.TaskService.Fail(t.TaskID, "服务重启，请重新提交") //nolint:errcheck
	})
	svcs.TaskService.RegisterResumeHandler(service.TaskTypeCrawlJob, func(t *model.AsyncTask) {
		svcs.TaskService.Fail(t.TaskID, "服务重启，请重新提交") //nolint:errcheck
	})

	// skill_gen: re-generate skills for a novel (idempotent — overwrites existing)
	if svcs.SkillService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeSkillGen, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			skills, err := svcs.SkillService.GenerateSkills(t.TenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume skill_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(skills)}) //nolint:errcheck
			}
		})
	}

	// batch_chapter_gen: re-run batch chapter generation, skipping chapters that already have content
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeBatchChapterGen, func(t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			chapters, err := svcs.ChapterService.ListChapters(novelID)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "list chapters failed: "+err.Error()) //nolint:errcheck
				return
			}
			var toGenerate []*model.Chapter
			for _, ch := range chapters {
				if strings.TrimSpace(ch.Content) == "" {
					toGenerate = append(toGenerate, ch)
				}
			}
			if len(toGenerate) == 0 {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"total": 0, "generated": 0, "failed": 0}) //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			total := len(toGenerate)
			var generated, failed int
			var failedChapters []int
			for i, ch := range toGenerate {
				progress := (i*90)/total + 5
				svcs.TaskService.UpdateProgress(t.TaskID, progress) //nolint:errcheck
				genReq := &model.GenerateChapterRequest{
					NovelID:   novelID,
					ChapterNo: ch.ChapterNo,
				}
				if _, genErr := svcs.ChapterService.GenerateChapter(t.TenantID, novelID, genReq); genErr != nil {
					logger.Errorf("TaskService resume batch_chapter_gen %s chapter %d failed: %v", t.TaskID, ch.ChapterNo, genErr)
					failed++
					failedChapters = append(failedChapters, ch.ChapterNo)
				} else {
					generated++
				}
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
				"total":           total,
				"generated":       generated,
				"failed":          failed,
				"failed_chapters": failedChapters,
			})
		})
	}
}
