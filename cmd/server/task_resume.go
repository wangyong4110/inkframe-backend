package main

import (
	"context"
	"encoding/json"
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
			if err := svcs.SFXService.AnalyzeSFXForVideo(ctx, shots, tenantID, params.UserContext, params.Lang); err != nil {
				logger.Printf("TaskService resume sfx_gen %s: analyze failed: %v", t.TaskID, err)
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 50) //nolint:errcheck

			progressFn := func(pct int) {
				overall := 50 + pct*45/100
				svcs.TaskService.UpdateProgress(t.TaskID, overall) //nolint:errcheck
			}
			success, fail := svcs.SFXService.BatchAutoGenerateSFX(ctx, shots, tenantID, params.UserContext, "", progressFn)
			logger.Printf("TaskService resume sfx_gen %s done: sfx_success=%d sfx_fail=%d", t.TaskID, success, fail)
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"sfx_success": success, "sfx_fail": fail}) //nolint:errcheck
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
					logger.Printf("TaskService resume three_view %s failed: %v", t.TaskID, err)
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
				appearance := char.VisualPrompt
				if appearance == "" {
					appearance = char.Description
				}
				ref := char.Portrait
				if ref == "" {
					ref = char.FaceCloseup
				}
				gender := service.InferGenderTag(char.VisualPrompt, char.Description)
				img, err := svcs.ImageGenerationService.GenerateThreeViewSheet(ctx, tenantID, char.Name, appearance, params.Style, gender, ref, params.Provider)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "generate three-view sheet failed: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 99) //nolint:errcheck
				updateReq := &model.UpdateCharacterRequest{
					Name:           char.Name,
					Role:           char.Role,
					Description:    char.Description,
					VisualPrompt:   char.VisualPrompt,
					ThreeViewSheet: img.URL,
					FaceCloseup:    char.FaceCloseup,
					Portrait:       char.Portrait,
				}
				updated, err := svcs.CharacterService.UpdateCharacter(charID, updateReq)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "save three-view sheet failed: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
					"character": updated,
					"generated": map[string]string{"sheet": img.URL},
				})
				return
			}
			svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
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
				logger.Printf("TaskService resume char_gen %s failed: %v", t.TaskID, err)
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
				logger.Printf("TaskService resume item_extract %s failed: %v", t.TaskID, err)
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
				logger.Printf("TaskService resume plot_extract %s failed: %v", t.TaskID, err)
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
				logger.Printf("TaskService resume chapter_summary_batch %s failed: %v", t.TaskID, err)
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
				logger.Printf("TaskService resume storyboard_review %s failed: %v", t.TaskID, err)
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
				logger.Printf("TaskService resume chapter_review %s failed: %v", t.TaskID, err)
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
			ref := char.Portrait
			if ref == "" {
				ref = char.ThreeViewSheet
			}
			appearance := char.VisualPrompt
			if appearance == "" {
				appearance = char.Description
			}
			gender := service.InferGenderTag(char.VisualPrompt, char.Description)
			img, err := svcs.ImageGenerationService.GenerateFaceCloseupImage(ctx, tenantID, char.Name, appearance, params.Style, gender, ref, params.Provider)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "generate face closeup failed: "+err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 99) //nolint:errcheck
			updateReq := &model.UpdateCharacterRequest{
				Name:           char.Name,
				Role:           char.Role,
				Description:    char.Description,
				VisualPrompt:   char.VisualPrompt,
				ThreeViewSheet: char.ThreeViewSheet,
				FaceCloseup:    img.URL,
				Portrait:       img.URL,
			}
			updated, err := svcs.CharacterService.UpdateCharacter(charID, updateReq)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "save face closeup failed: "+err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
				"character": updated,
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
							logger.Printf("TaskService resume voice_gen %s shot %d: %v", t.TaskID, s.ShotNo, err)
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
				logger.Printf("TaskService resume chapter_gen %s failed: %v", t.TaskID, err)
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
}
