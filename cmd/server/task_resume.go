package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// registerTaskResumeHandlers registers resume functions for idempotent task types.
// Must be called after all services are fully wired.
func registerTaskResumeHandlers(svcs *Services, repos *Repositories) {
	// sfx_gen: SFX tag analysis + batch generation (idempotent — skips shots that already have tags/sfx)
	if svcs.SFXService != nil && svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeSFXGen, func(ctx context.Context, t *model.AsyncTask) {
			// Single-shot SFX: entity_type == "shot", params carry {shot_id, video_id, provider}
			if t.EntityType == "shot" {
				var params struct {
					ShotID   uint   `json:"shot_id"`
					VideoID  uint   `json:"video_id"`
					Provider string `json:"provider"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
				}
				if params.ShotID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				shot, err := svcs.VideoService.GetShotByID(params.VideoID, params.ShotID)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "shot not found on resume") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				if err := svcs.SFXService.AutoGenerateSFX(ctx, shot, t.TenantID, params.Provider, true); err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_id": shot.ID}) //nolint:errcheck
				}
				return
			}

			// Video-level batch SFX: entity_type == "video". Two handler flows share this:
			//   - BatchGenerateSFX: force=false (skip shots that already have tags)
			//   - AnalyzeSFXTags: force=true (force re-analyze all shots' tags)
			videoID := t.EntityID
			if videoID == 0 {
				return
			}
			// Parse saved params
			var params struct {
				UserContext string `json:"user_context"`
				Lang        string `json:"lang"`
				Provider    string `json:"provider"`
				Force       bool   `json:"force"`
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

			if err := svcs.SFXService.AnalyzeSFXForVideo(ctx, shots, tenantID, params.UserContext, params.Lang, params.Force); err != nil {
				logger.Errorf("TaskService resume sfx_gen %s: analyze failed: %v", t.TaskID, err)
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 50) //nolint:errcheck

			progressFn := func(pct int) {
				overall := 50 + pct*45/100
				svcs.TaskService.UpdateProgress(t.TaskID, overall) //nolint:errcheck
			}
			success, fail, failedIDs := svcs.SFXService.BatchAutoGenerateSFX(ctx, shots, tenantID, params.UserContext, params.Provider, progressFn)
			logger.Printf("TaskService resume sfx_gen %s done: sfx_success=%d sfx_fail=%d", t.TaskID, success, fail)
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
				"count":           len(shots),
				"success":         success,
				"fail":            fail,
				"sfx_success":     success,
				"sfx_fail":        fail,
				"failed_shot_ids": failedIDs,
			})
		})
	}

	// three_view: batch (novel entity) or single character
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeThreeView, func(ctx context.Context, t *model.AsyncTask) {
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
				svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
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
				facePrompt := ""
				if defaultLook != nil {
					facePrompt = defaultLook.FacePrompt
				}
				sheet, err := svcs.ImageGenerationService.GenerateThreeViewSheet(ctx, tenantID, char.Name, appearance, facePrompt, params.Style, gender, "", params.Provider)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "generate three-view sheet failed: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 99) //nolint:errcheck
				lookReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &sheet.SheetURL}
				if sheet.PortraitURL != "" {
					lookReq.Portrait = &sheet.PortraitURL
				}
				var updatedLook *model.CharacterLook
				if defaultLook != nil {
					updatedLook, err = svcs.CharacterService.UpdateLook(defaultLook.ID, lookReq)
				} else {
					updatedLook, err = svcs.CharacterService.CreateLook(charID, char.NovelID, &model.CreateCharacterLookRequest{
						Label: "默认形象", SetAsDefault: true, ChapterFrom: 1, ThreeViewSheet: sheet.SheetURL, Portrait: sheet.PortraitURL,
					})
				}
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "save three-view sheet failed: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
					"look":      updatedLook,
					"generated": map[string]string{"sheet": sheet.SheetURL, "portrait": sheet.PortraitURL},
				})
				return
			}
			svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
		})
	}

	// chapter_char_extract: extract minor characters from a single chapter
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterCharExtract, func(ctx context.Context, t *model.AsyncTask) {
			chapterID := t.EntityID
			if chapterID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				NovelID    uint   `json:"novel_id"`
				UserPrompt string `json:"user_prompt"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			chars, err := svcs.CharacterService.AIExtractMinorChars(ctx, t.TenantID, params.NovelID, chapterID, params.UserPrompt)
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharReanalyze, func(ctx context.Context, t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			char, err := svcs.CharacterService.ReanalyzeCharacter(ctx, t.TenantID, charID)
			if err != nil {
				logger.Errorf("TaskService resume char_reanalyze %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"character": handler.CharacterResponse(char)}) //nolint:errcheck
			}
		})
	}

	// char_gen: AI batch generate characters for a novel (idempotent — overwrites existing)
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharGen, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			chars, err := svcs.CharacterService.AIBatchGenerate(ctx, tenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume char_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"characters": chars, "count": len(chars)}) //nolint:errcheck
			}
		})
	}

	// item_extract: AI extract items from novel (idempotent — overwrites existing).
	// Uses ItemService.AIExtractFromNovel to match what ItemHandler.AIExtractFromNovel has
	// always actually called in production — NOT AIExtractAllFromNovel (a separate, newer
	// implementation with existing-item dedup that was never wired to the live handler).
	// Switching to it would be a real behavior change, not just a migration.
	if svcs.ItemService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeItemExtract, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			items, err := svcs.ItemService.AIExtractFromNovel(ctx, tenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume item_extract %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"items": items, "count": len(items)}) //nolint:errcheck
			}
		})
	}

	// plot_extract: AI extract plot points from novel (idempotent — overwrites existing)
	if svcs.PlotPointService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypePlotExtract, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			points, err := svcs.PlotPointService.AIExtractFromNovel(ctx, tenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume plot_extract %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"plot_points": points, "count": len(points)}) //nolint:errcheck
			}
		})
	}

	// chapter_summary_batch: batch generate chapter summaries (idempotent — skips chapters with existing summaries)
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterSummaryBatch, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
			count, err := svcs.ChapterService.BatchGenerateSummaries(ctx, tenantID, novelID, progressFn)
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeStoryboardReview, func(ctx context.Context, t *model.AsyncTask) {
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
			review, recordID, err := svcs.StoryboardService.ReviewStoryboard(ctx, tenantID, videoID, params.Provider, params.PreviousScore)
			if err != nil {
				logger.Errorf("TaskService resume storyboard_review %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			type reviewResult struct {
				*model.StoryboardReview
				RecordID uint `json:"record_id,omitempty"`
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 90)                                                    //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, &reviewResult{StoryboardReview: review, RecordID: recordID}) //nolint:errcheck
		})
	}

	// chapter_review: AI review chapter content (creates a new review record, safe to re-run)
	if svcs.QualityControlService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterReview, func(ctx context.Context, t *model.AsyncTask) {
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
			_ = tenantID                                  // QualityControlService.ReviewChapter does not take tenantID
			review, err := svcs.QualityControlService.ReviewChapter(ctx, chapterID, params.Provider)
			if err != nil {
				logger.Errorf("TaskService resume chapter_review %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 90) //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, review)   //nolint:errcheck
		})
	}

	// chapter_outline_review: AI review a chapter's outline (OutlineReviewHandler.ReviewChapter).
	// Was previously misfiled under the same task type as chapter_review (chapter content
	// review) — split into its own type since they call different service methods.
	if svcs.OutlineReviewService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterOutlineReview, func(ctx context.Context, t *model.AsyncTask) {
			chapterID := t.EntityID
			if chapterID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			review, err := svcs.OutlineReviewService.ReviewChapterOutline(ctx, t.TenantID, chapterID)
			if err != nil {
				logger.Errorf("TaskService resume chapter_outline_review %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"review": review}) //nolint:errcheck
		})
	}

	// voice_gen: batch voice (video entity), single segment, or single shot
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeVoiceGen, func(ctx context.Context, t *model.AsyncTask) {
			var params struct {
				NarrationVoice  string `json:"narration_voice"`
				SubtitleEnabled bool   `json:"subtitle_enabled"`
				VideoID         uint   `json:"video_id"`
				SkipExisting    bool   `json:"skip_existing"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			tenantID := t.TenantID

			if t.EntityType == "shot" {
				shotID := t.EntityID
				if shotID == 0 || params.VideoID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				shot, err := svcs.VideoService.GetShotByID(params.VideoID, shotID)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "shot not found: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
				// 删除已有语音段落，强制重新合成。
				if segs, err := svcs.VideoService.ListVoiceSegments(shot.ID); err == nil {
					for _, seg := range segs {
						svcs.VideoService.DeleteVoiceSegment(seg.ID) //nolint:errcheck
					}
				}
				const maxRetries = 3
				var audioErr error
				for attempt := 1; attempt <= maxRetries; attempt++ {
					audioErr = svcs.VideoService.GenerateShotAudio(ctx, shot, tenantID, params.NarrationVoice)
					if audioErr == nil {
						break
					}
					logger.Errorf("TaskService resume voice_gen(shot) %s shot %d attempt %d/%d failed: %v", t.TaskID, shot.ShotNo, attempt, maxRetries, audioErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if audioErr != nil {
					svcs.TaskService.Fail(t.TaskID, audioErr.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 90) //nolint:errcheck
				audioURL := ""
				if m := svcs.VideoService.GetShotAudioMap([]uint{shot.ID}); m != nil {
					if _, ok := m[shot.ID]; ok {
						audioURL = handler.ResolveAudioURL(params.VideoID, shot)
					}
				}
				result := map[string]interface{}{"audio_url": audioURL, "shot_id": shot.ID}
				if params.SubtitleEnabled {
					if srt := service.GenerateShotSRT(shot); srt != "" {
						result["subtitle_srt"] = srt
					}
				}
				svcs.TaskService.Complete(t.TaskID, result) //nolint:errcheck
				return
			}

			if t.EntityType == "segment" {
				segID := t.EntityID
				if segID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
				const maxRetries = 3
				var audioErr error
				for attempt := 1; attempt <= maxRetries; attempt++ {
					audioErr = svcs.VideoService.GenerateSegmentAudio(ctx, segID, tenantID, params.NarrationVoice)
					if audioErr == nil {
						break
					}
					logger.Printf("TaskService resume voice_gen(segment) %s seg %d attempt %d/%d: %v", t.TaskID, segID, attempt, maxRetries, audioErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if audioErr != nil {
					svcs.TaskService.Fail(t.TaskID, audioErr.Error()) //nolint:errcheck
					return
				}
				seg, _ := svcs.VideoService.GetVoiceSegment(segID)
				svcs.TaskService.UpdateProgress(t.TaskID, 90) //nolint:errcheck
				svcs.TaskService.Complete(t.TaskID, seg)      //nolint:errcheck
				return
			}

			// Video batch (BatchGenerateVoice): re-run, skip shots that already have voice-segment
			// audio unless skip_existing=false (hasAudio is determined via ListVoiceSegments,
			// the current source of truth — not the legacy shot.TaskMeta.AudioPath field, which
			// multi-segment voice storage no longer writes to).
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
			hasAudio := func(shotID uint) bool {
				segs, _ := svcs.VideoService.ListVoiceSegments(shotID)
				for _, seg := range segs {
					if seg.AudioPath != "" {
						return true
					}
				}
				return false
			}
			var targets []*model.StoryboardShot
			for _, s := range shots {
				if s.Narration == "" && s.GenMeta.Dialogue == "" && s.Description == "" {
					continue
				}
				if params.SkipExisting && hasAudio(s.ID) {
					continue
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
						if err := svcs.VideoService.GenerateShotAudio(ctx, s, tenantID, params.NarrationVoice); err != nil {
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
			// 统计最终结果（与原 handler 一致：按 targets 集合重新核对是否配音成功）
			targetSet := make(map[uint]bool, len(targets))
			for _, s := range targets {
				targetSet[s.ID] = true
			}
			success, fail := 0, 0
			for shotID := range targetSet {
				if hasAudio(shotID) {
					success++
				} else {
					fail++
				}
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"success": success, "fail": fail, "total": total}) //nolint:errcheck
		})
	}

	// image_gen: routed by source param
	if svcs.ItemService != nil && svcs.SceneAnchorService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeImageGen, func(ctx context.Context, t *model.AsyncTask) {
			var params struct {
				Source    string `json:"source"`
				Provider  string `json:"provider"`
				Force     bool   `json:"force"`
				RefURL    string `json:"ref_url"`
				NovelID   uint   `json:"novel_id"`
				ItemIDs   []uint `json:"item_ids"`
				AnchorIDs []uint `json:"anchor_ids"`
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
				svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
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
					svcs.TaskService.Complete(t.TaskID, item)     //nolint:errcheck
				}
			case "item_chapter":
				if len(params.ItemIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
				succ, fail, err := svcs.ItemService.GenerateChapterImages(tenantID, params.NovelID, params.ItemIDs, params.Provider, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
				}
			case "scene_anchor_batch":
				novelID := t.EntityID
				if novelID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
				succ, fail, err := svcs.SceneAnchorService.BatchGenerateRefImages(ctx, tenantID, novelID, params.Provider, params.Force, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
				}
			case "scene_anchor_chapter":
				if len(params.AnchorIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
				succ, fail, err := svcs.SceneAnchorService.GenerateChapterRefImages(ctx, tenantID, params.NovelID, params.AnchorIDs, params.Provider, progressFn)
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterSceneExtract, func(ctx context.Context, t *model.AsyncTask) {
			chapterID := t.EntityID
			if chapterID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				NovelID    uint   `json:"novel_id"`
				Content    string `json:"content"`
				UserPrompt string `json:"user_prompt"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Content == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
			defer cancel()
			anchors, err := svcs.SceneAnchorService.ExtractFromChapter(ctx, t.TenantID, params.NovelID, "", params.Content, chapterID, params.UserPrompt)
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeSceneAnchorExtract, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				return
			}
			tenantID := t.TenantID
			svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
			anchors, err := svcs.SceneAnchorService.AIExtractAllFromNovel(ctx, tenantID, novelID, progressFn)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"scene_anchors": anchors, "count": len(anchors)}) //nolint:errcheck
			}
		})
	}

	// bgm_analyze and bgm_generate
	if svcs.BGMService != nil && svcs.VideoService != nil {
		bgmResume := func(generate bool) func(context.Context, *model.AsyncTask) {
			return func(ctx context.Context, t *model.AsyncTask) {
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeStoryboardGen, func(ctx context.Context, t *model.AsyncTask) {
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
			svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
			progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
			overrides := service.StoryboardOverrides{
				MaxTokens:      params.MaxTokens,
				Temperature:    params.Temperature,
				TimeoutSeconds: params.TimeoutSeconds,
				VoiceMode:      params.VoiceMode,
			}
			result, err := svcs.StoryboardService.GenerateStoryboardCtx(ctx, videoID, params.ChapterID, params.Characters, params.Style, params.Provider, params.UserPrompt, progressFn, overrides)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			if result.FailedSegments > 0 {
				svcs.TaskService.CompletePartial(t.TaskID, map[string]interface{}{"shot_count": len(result.Shots)}, //nolint:errcheck
					fmt.Sprintf("预计生成约 %d 个镜头，但 %d/%d 个分段生成失败，实际生成 %d 个镜头，建议检查后重新生成缺失部分",
						result.RequestedShots, result.FailedSegments, result.TotalSegments, len(result.Shots)))
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": len(result.Shots)}) //nolint:errcheck
		})
	}

	// storyboard_optimize: re-run from saved review JSON
	if svcs.StoryboardService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeStoryboardOptimize, func(ctx context.Context, t *model.AsyncTask) {
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

	// novel_outline_gen: regenerate novel outline with original params
	if svcs.NovelService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeNovelOutlineGen, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var req service.GenerateOutlineRequest
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &req)
			}
			req.NovelID = novelID
			if req.ChapterNum == 0 {
				req.ChapterNum = 10 // fallback for tasks created before params were saved
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			result, err := svcs.NovelService.GenerateOutline(ctx, t.TenantID, &req)
			if err != nil {
				logger.Errorf("TaskService resume novel_outline_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"outline": result}) //nolint:errcheck
			}
		})
	}

	// char_image_gen: two entity_types share this task type:
	//   - "character": re-generate a single character's portrait/expression/pose
	//   - "chapter": batch-generate images for selected characters mentioned in a chapter
	//     (GenerateChapterCharacterImages) — needs novel_id/character_ids/provider from params
	//     since chapter_id alone (t.EntityID) isn't enough to know which characters to generate.
	if svcs.ImageGenerationService != nil && svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCharImageGen, func(ctx context.Context, t *model.AsyncTask) {
			if t.EntityType == "chapter" && svcs.ChapterService != nil {
				chapterID := t.EntityID
				if chapterID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				var params struct {
					NovelID      uint   `json:"novel_id"`
					CharacterIDs []uint `json:"character_ids"`
					Provider     string `json:"provider"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
				}
				if len(params.CharacterIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				chapter, err := svcs.ChapterService.GetChapter(chapterID, t.TenantID)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, "chapter not found: "+err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
				progressFn := func(pct int) { svcs.TaskService.UpdateProgress(t.TaskID, pct) } //nolint:errcheck
				succeeded, failed, genErr := svcs.CharacterService.GenerateChapterImages(
					ctx, t.TenantID, params.NovelID, chapter, params.CharacterIDs, params.Provider, progressFn,
				)
				if genErr != nil {
					svcs.TaskService.Fail(t.TaskID, genErr.Error()) //nolint:errcheck
					return
				}
				if failed > 0 && succeeded == 0 {
					svcs.TaskService.Fail(t.TaskID, fmt.Sprintf("all %d character image generations failed", failed)) //nolint:errcheck
					return
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"succeeded": succeeded, "failed": failed}) //nolint:errcheck
				return
			}
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
			image, err := svcs.ImageGenerationService.GenerateCharacterImage(ctx, t.TenantID, &model.GenerateImageRequest{
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

	// voice_preview: two entity_types share this task type:
	//   - "voice" (entity_id=0): narration-only voice preview, not tied to any character
	//     (ModelHandler.VoicePreview) — just voice_id/text, no character side effects.
	//   - "character": preview a character's configured voice, also writes the result back
	//     to the character's VoiceSample field (CharacterHandler.PreviewVoice).
	if svcs.AIService != nil && svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeVoicePreview, func(ctx context.Context, t *model.AsyncTask) {
			if t.EntityType == "voice" {
				var vparams struct {
					Text    string `json:"text"`
					VoiceID string `json:"voice_id"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &vparams)
				}
				if vparams.Text == "" || vparams.VoiceID == "" {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				rawURL, err := svcs.AIService.AudioGenerateWithOptions(ctx, t.TenantID, vparams.Text, vparams.VoiceID, 1.0, "")
				if err != nil {
					logger.Errorf("TaskService resume voice_preview(voice) %s failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, "语音生成失败: "+err.Error()) //nolint:errcheck
					return
				}
				playURL := rawURL
				if len(rawURL) > 7 && rawURL[:7] == "file://" {
					if data, readErr := os.ReadFile(rawURL[7:]); readErr == nil && len(data) > 0 {
						playURL = "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(data)
					}
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"audio_url": playURL, "voice_id": vparams.VoiceID}) //nolint:errcheck
				return
			}
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Text       string  `json:"text"`
				VoiceID    string  `json:"voice_id"`
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
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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
				} else {
					// Fallback to sample endpoint if the temp file cannot be read.
					playURL = "/api/v1/characters/" + strconv.FormatUint(uint64(charID), 10) + "/voice/sample?t=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
				}
			}
			svcs.CharacterService.UpdateCharacter(charID, t.TenantID, &model.UpdateCharacterRequest{ //nolint:errcheck
				Name:        params.CharName,
				VoiceSample: rawURL,
			})
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"audio_url": playURL, "voice_id": params.VoiceID, "voice_speed": params.VoiceSpeed}) //nolint:errcheck
		})
	}

	// look_prompt_gen: two distinct handler flows share this task type (discriminated by
	// params.Kind, mirroring the source-field routing convention used by asset_gen/image_gen):
	//   - "" or "prompt": re-generate look visual prompt from a description (GenerateLookVisualPrompt)
	//   - "design": AI-generate a costume/appearance design purely from the character's own
	//     Description field already in the DB (GenerateCostumeDesign) — no extra params needed.
	if svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeLookPromptGen, func(ctx context.Context, t *model.AsyncTask) {
			charID := t.EntityID
			if charID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Kind        string `json:"kind"`
				Description string `json:"description"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Kind == "design" {
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				prompt, err := svcs.CharacterService.GenerateCostumeDesign(ctx, t.TenantID, charID)
				if err != nil {
					logger.Errorf("TaskService resume look_prompt_gen(design) %s failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"appearance_prompt": prompt}) //nolint:errcheck
				}
				return
			}
			if params.Description == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			result, err := svcs.CharacterService.GenerateLookVisualPrompt(ctx, t.TenantID, charID, params.Description)
			if err != nil {
				logger.Errorf("TaskService resume look_prompt_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
					"visual_prompt": result.VisualPrompt,
					"face_prompt":   result.FacePrompt,
				})
			}
		})
	}

	// look_image_gen: re-generate look images (face closeup or three view)
	if svcs.ImageGenerationService != nil && svcs.CharacterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeLookImageGen, func(ctx context.Context, t *model.AsyncTask) {
			lookID := t.EntityID
			if lookID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				Type         string `json:"type"`
				CharID       uint   `json:"char_id"`
				Provider     string `json:"provider"`
				FacePrompt   string `json:"face_prompt"`
				VisualPrompt string `json:"visual_prompt"`
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
			// 优先使用请求时传入的表单内容（可能尚未保存到数据库），为空才回退到 look 的数据库存值
			// ——兼容章节批量生成等没有编辑表单、只能走数据库存值的调用场景。
			facePrompt := params.FacePrompt
			if facePrompt == "" {
				facePrompt = look.FacePrompt
			}
			visualPrompt := params.VisualPrompt
			if visualPrompt == "" {
				visualPrompt = look.VisualPrompt
			}
			if visualPrompt == "" {
				visualPrompt = char.Description
			}
			style := svcs.CharacterService.GetNovelImageStyle(char.NovelID)
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			tenantID := t.TenantID
			// 三视图与面部参考图不再分两步生成——一次调用同时产出，第4格(面部特写)自动裁剪为 Portrait。
			sheet, err := svcs.ImageGenerationService.GenerateThreeViewSheet(ctx, tenantID, char.Name, visualPrompt, facePrompt, style, "", look.Portrait, params.Provider)
			if err != nil {
				logger.Errorf("TaskService resume look_image_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			updateReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &sheet.SheetURL}
			if sheet.PortraitURL != "" {
				updateReq.Portrait = &sheet.PortraitURL
			}
			updatedLook, _ := svcs.CharacterService.UpdateLook(lookID, updateReq)
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"look": updatedLook}) //nolint:errcheck
		})
	}

	// cover_image_gen: re-generate novel cover image
	if svcs.NovelService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCoverImageGen, func(ctx context.Context, t *model.AsyncTask) {
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
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeImageEdit, func(ctx context.Context, t *model.AsyncTask) {
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
			newURL, err := svcs.AIService.EditImageWithInstruction(ctx, t.TenantID, params.ImageURL, params.Instruction)
			if err != nil {
				logger.Errorf("TaskService resume image_edit %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, "failed to edit image: "+err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"image_url": newURL}) //nolint:errcheck
			}
		})
	}

	// chapter_gen: three flows share this task type, disambiguated by entity_type and,
	// for "chapter", by whether an entity_id is set:
	//   - "novel": generate the next new chapter (ChapterHandler.GenerateChapter)
	//   - "chapter" with entity_id>0: regenerate an existing chapter's content
	//     (ChapterHandler.RegenerateChapter)
	//   - "chapter" with entity_id==0: also generates the next new chapter, but from
	//     NovelHandler.GenerateChapter — a second, slightly different entry point for the same
	//     underlying operation that additionally sends a completion notification and chains two
	//     best-effort, fire-and-forget post-processing steps (foreshadow extraction, quality
	//     check) whose errors are only logged, never fail the task — kept as detached goroutines
	//     here to match that "don't block/don't fail on these" intent exactly.
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterGen, func(ctx context.Context, t *model.AsyncTask) {
			if t.EntityType == "chapter" && t.EntityID > 0 {
				chapterID := t.EntityID
				var req model.GenerateChapterRequest
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &req)
				}
				svcs.TaskService.SetRunning(t.TaskID)        //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 5) //nolint:errcheck
				chapter, err := svcs.ChapterService.RegenerateChapter(ctx, t.TenantID, chapterID, &req)
				if err != nil {
					logger.Errorf("TaskService resume chapter_gen(chapter) %s failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 90)                                   //nolint:errcheck
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"chapter": chapter}) //nolint:errcheck
				return
			}
			if t.EntityType == "chapter" && t.EntityID == 0 {
				var params struct {
					NovelID      uint                         `json:"novel_id"`
					Req          model.GenerateChapterRequest `json:"req"`
					CallerUserID uint                         `json:"caller_user_id"`
				}
				if t.ParamsJSON != "" {
					_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
				}
				if params.NovelID == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				tenantID := t.TenantID
				svcs.TaskService.SetRunning(t.TaskID)        //nolint:errcheck
				svcs.TaskService.UpdateProgress(t.TaskID, 5) //nolint:errcheck
				chapter, err := svcs.ChapterService.GenerateChapter(ctx, tenantID, params.NovelID, &params.Req)
				if err != nil {
					logger.Errorf("TaskService resume chapter_gen(chapter,new) %s failed: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 90) //nolint:errcheck
				modelUsed := params.Req.ModelOverride
				if modelUsed == "" && svcs.NovelService != nil {
					modelUsed = svcs.NovelService.GetAIService().GetDefaultProviderName()
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"chapter": chapter, "model_used": modelUsed}) //nolint:errcheck

				if svcs.NotificationService != nil && params.CallerUserID > 0 {
					_ = svcs.NotificationService.Send(
						tenantID, params.CallerUserID,
						"chapter_done",
						fmt.Sprintf("第%d章生成完成", chapter.ChapterNo),
						chapter.Title,
						"chapter", chapter.ID,
						fmt.Sprintf("/novel/%d/chapter/%d", chapter.NovelID, chapter.ChapterNo),
					)
				}
				if svcs.ForeshadowService != nil {
					go func(ch *model.Chapter, tid uint) {
						if _, err := svcs.ForeshadowService.ExtractForeshadows(ch, tid, ch.NovelID); err != nil {
							logger.Errorf("TaskService resume chapter_gen(chapter,new) %s: foreshadow extraction failed (ch %d): %v", t.TaskID, ch.ID, err)
						}
					}(chapter, tenantID)
				}
				if svcs.QualityControlService != nil {
					go func(chID uint) {
						if _, err := svcs.QualityControlService.CheckChapter(chID); err != nil {
							logger.Errorf("TaskService resume chapter_gen(chapter,new) %s: quality check failed (ch %d): %v", t.TaskID, chID, err)
						}
					}(chapter.ID)
				}
				return
			}
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
			chapter, err := svcs.ChapterService.GenerateChapter(ctx, tenantID, params.NovelID, &params.Req)
			if err != nil {
				logger.Errorf("TaskService resume chapter_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 90)                                   //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"chapter": chapter}) //nolint:errcheck
		})
	}

	// chapter_post_process: re-run the summary/title/refine/arc-summary tail after a restart
	// interrupted it mid-flight. Safe to re-run — see ResumePostProcessChapter's doc comment.
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterPostProcess, func(ctx context.Context, t *model.AsyncTask) {
			svcs.ChapterService.ResumePostProcessChapter(t)
		})
	}

	// asset_gen: routed by source param
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeAssetGen, func(ctx context.Context, t *model.AsyncTask) {
			var params struct {
				Source   string                          `json:"source"`
				VideoID  uint                            `json:"video_id"`
				ShotID   uint                            `json:"shot_id"`
				Provider string                          `json:"provider"`
				Req      model.BatchGenerateShotsRequest `json:"req"`
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
					return
				}
				svcs.TaskService.UpdateProgress(t.TaskID, 50) //nolint:errcheck
				// AI 视频模式：只轮询当前分镜直到完成，避免触发其他分镜的生成
				if shot.Status == "processing" {
					svcs.VideoService.PollSingleShotUntilDone(videoID, shotID)
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_id": shot.ID, "status": shot.Status}) //nolint:errcheck
			case "batch_shots":
				videoID := t.EntityID
				if videoID == 0 || len(params.Req.ShotIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				var shots []*model.StoryboardShot
				var err error
				switch {
				case params.Req.VoiceFirst:
					// 配音优先：先生成 TTS，以配音时长决定视频时长，保证声画同步
					shots, err = svcs.VideoService.VoiceFirstGenerateShots(videoID, params.Req.ShotIDs, params.Req.QualityTier, progressFn, params.Req.Provider)
				case params.Req.Sequential:
					// 顺序模式：每镜完成后同步 chain 最后一帧再提交下一镜，保证 I2V 链接
					shots, err = svcs.VideoService.SequentialGenerateShots(videoID, params.Req.ShotIDs, params.Req.QualityTier, progressFn, params.Req.Provider)
				default:
					shots, err = svcs.VideoService.BatchGenerateShots(videoID, params.Req.ShotIDs, params.Req.QualityTier, progressFn, params.Req.Provider)
				}
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
					return
				}
				// 并发模式下：AI 视频任务仍在 processing，启动轮询等待所有镜头完成
				// 顺序模式下：所有镜头已完成，PollAndStitchVideo 会快速完成
				for _, sh := range shots {
					if sh.Status == "processing" {
						svcs.VideoService.PollAndStitchVideo(ctx, videoID)
						break
					}
				}
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": len(shots)}) //nolint:errcheck
			case "batch_images":
				videoID := t.EntityID
				if videoID == 0 || len(params.Req.ShotIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				shots, err := svcs.VideoService.BatchGenerateShotImages(videoID, params.Req.ShotIDs, params.Req.Force, progressFn)
				if err != nil {
					svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				} else {
					svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"shot_count": len(shots)}) //nolint:errcheck
				}
			case "batch_clips":
				videoID := t.EntityID
				if videoID == 0 || len(params.Req.ShotIDs) == 0 {
					svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
					return
				}
				svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
				shots, err := svcs.VideoService.BatchGenerateShotClips(videoID, params.Req.ShotIDs, progressFn)
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
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeNovelAnalysis, func(ctx context.Context, t *model.AsyncTask) {
			var p struct {
				CreateOutlines bool `json:"create_outlines"`
			}
			_ = json.Unmarshal([]byte(t.ParamsJSON), &p)
			svcs.NovelAnalysisService.ResumeAnalysis(t, p.CreateOutlines)
		})
	}

	// rewrite_analysis + rewrite_chapters
	if svcs.RewriteService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeRewriteAnalysis, func(ctx context.Context, t *model.AsyncTask) {
			svcs.RewriteService.ResumeAnalysis(t)
		})
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeRewriteChapters, func(ctx context.Context, t *model.AsyncTask) {
			svcs.RewriteService.ResumeRewriting(t)
		})
	}

	// outline_review_batch: batch review all chapter outlines in a novel (idempotent — creates new review records)
	if svcs.OutlineReviewService != nil {
		svcs.TaskService.RegisterResumeHandler("outline_review_batch", func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			progressFn := func(done, total int) {
				if total > 0 {
					svcs.TaskService.UpdateProgress(t.TaskID, done*99/total) //nolint:errcheck
				}
			}
			result, err := svcs.OutlineReviewService.BatchReviewNovel(ctx, t.TenantID, novelID, progressFn)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"count": len(result.Reviews), "synthesis": result.Synthesis}) //nolint:errcheck
			}
		})
	}

	// import: two shapes share this task type, disambiguated by entity_id:
	//   - entity_id > 0: ResumeCrawl — continue an already-running crawl job for an existing
	//     novel (ImportHandler.ResumeCrawl). Kicks off importService.ResumeCrawl then polls.
	//   - entity_id == 0: fresh import (ImportNovel/ImportFromFile/ImportFromURL/
	//     ImportFromCrawl/CompleteChunkedUpload), params carry the full ImportRequest
	//     (minus FileData, which is staged in storage — see file_url) discriminated by
	//     req.Source.
	// Not safe to blindly re-run after a crash mid-import (could create a duplicate novel),
	// so params-less/incomplete tasks fail out with a resubmit message rather than guessing.
	if svcs.NovelImportService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeImport, func(ctx context.Context, t *model.AsyncTask) {
			if t.EntityID > 0 {
				resumeImportCrawl(svcs, t, t.EntityID)
				return
			}

			var params struct {
				Req     service.ImportRequest `json:"req"`
				FileURL string                `json:"file_url,omitempty"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.Req.Source == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			if params.FileURL != "" {
				data, err := svcs.NovelImportService.DownloadStagedFile(ctx, params.FileURL)
				if err != nil {
					logger.Errorf("TaskService resume import %s: failed to download staged file: %v", t.TaskID, err)
					svcs.TaskService.Fail(t.TaskID, "读取暂存文件失败: "+err.Error()) //nolint:errcheck
					return
				}
				params.Req.FileData = data
			}

			if params.Req.Source == service.SourceCrawl {
				resumeImportCrawlFresh(svcs, t, &params.Req)
				return
			}

			svcs.TaskService.SetRunning(t.TaskID)                                          //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 20)                                  //nolint:errcheck
			svcs.TaskService.SetMeta(t.TaskID, map[string]interface{}{"step": "解析导入中..."}) //nolint:errcheck

			result, err := svcs.NovelImportService.Import(&params.Req)
			if err != nil {
				logger.Errorf("TaskService resume import %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}

			analysisTaskID := ""
			if svcs.NovelAnalysisService != nil {
				if id, aErr := svcs.NovelAnalysisService.StartAnalysis(t.TenantID, result.NovelID, false); aErr == nil {
					analysisTaskID = id
				}
			}

			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
				"novel_id":          result.NovelID,
				"imported_chapters": result.ImportedChapters,
				"oss_url":           result.OSSUrl,
				"analysis_task_id":  analysisTaskID,
				"message":           fmt.Sprintf("导入完成，共 %d 章", result.ImportedChapters),
			})
		})
	}
	if svcs.AssetService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeCrawlJob, func(ctx context.Context, t *model.AsyncTask) {
			svcs.AssetService.ResumeCrawlJob(t)
		})
	}

	// video_synthesis: resume the stitch→subtitle→cover→upload pipeline
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeVideoSynthesis, func(ctx context.Context, t *model.AsyncTask) {
			var params struct {
				VideoID uint `json:"video_id"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.VideoID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			// 补上取消注册——之前这里直接裸调 RunSynthesisPipeline，恢复的任务完全绕过了取消体系。
			ctx, cancel := context.WithCancel(ctx)
			svcs.TaskService.RegisterCancel(t.TaskID, cancel)
			defer svcs.TaskService.DeregisterCancel(t.TaskID)
			defer cancel()
			svcs.VideoService.RunSynthesisPipelineCtx(ctx, t.TaskID, params.VideoID)
		})
	}

	// video_gen: resume submit-all-shots + poll + stitch pipeline
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeVideoGen, func(ctx context.Context, t *model.AsyncTask) {
			var params struct {
				VideoID uint   `json:"video_id"`
				Mode    string `json:"mode"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.VideoID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID)        //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 5) //nolint:errcheck
			if err := svcs.VideoService.GenerateAllShotVideos(ctx, params.VideoID); err != nil {
				logger.Errorf("TaskService resume video_gen %s: submit failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			if params.Mode != "slideshow" {
				svcs.VideoService.PollAndStitchVideo(ctx, params.VideoID) // blocks until done or timeout
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"video_id": params.VideoID}) //nolint:errcheck
		})
	}

	// skill_gen: re-generate skills for a novel (idempotent — overwrites existing)
	if svcs.SkillService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeSkillGen, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			skills, err := svcs.SkillService.GenerateSkills(ctx, t.TenantID, novelID)
			if err != nil {
				logger.Errorf("TaskService resume skill_gen %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
			} else {
				svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"skills": skills, "count": len(skills)}) //nolint:errcheck
			}
		})
	}

	// batch_chapter_gen: re-run batch chapter generation, respecting the same
	// start/end/skip_existing filters and per-chapter overrides as the original request.
	if svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeBatchChapterGen, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var req model.BatchGenerateChaptersRequest
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &req)
			}
			chapters, err := svcs.ChapterService.ListChapters(novelID)
			if err != nil {
				svcs.TaskService.Fail(t.TaskID, "list chapters failed: "+err.Error()) //nolint:errcheck
				return
			}
			var toGenerate []*model.Chapter
			for _, ch := range chapters {
				if req.StartChapterNo > 0 && ch.ChapterNo < req.StartChapterNo {
					continue
				}
				if req.EndChapterNo > 0 && ch.ChapterNo > req.EndChapterNo {
					continue
				}
				if req.SkipExisting && strings.TrimSpace(ch.Content) != "" {
					continue
				}
				toGenerate = append(toGenerate, ch)
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
				svcs.TaskService.UpdateProgressAndTitle(t.TaskID, progress, //nolint:errcheck
					fmt.Sprintf("正在生成第%d章《%s》（%d/%d）", ch.ChapterNo, ch.Title, i+1, total))
				genReq := &model.GenerateChapterRequest{
					NovelID:       novelID,
					ChapterNo:     ch.ChapterNo,
					WordCount:     req.WordCount,
					MaxTokens:     req.MaxTokens,
					ModelOverride: req.ModelOverride,
					EnabledTools:  req.EnabledTools,
				}
				if _, genErr := svcs.ChapterService.GenerateChapter(ctx, t.TenantID, novelID, genReq); genErr != nil {
					logger.Errorf("TaskService resume batch_chapter_gen %s chapter %d failed: %v", t.TaskID, ch.ChapterNo, genErr)
					failed++
					failedChapters = append(failedChapters, ch.ChapterNo)
				} else {
					generated++
				}
			}
			resultTitle := fmt.Sprintf("批量生成完成：成功%d章", generated)
			if failed > 0 {
				resultTitle += fmt.Sprintf("，失败%d章", failed)
			}
			svcs.TaskService.UpdateProgressAndTitle(t.TaskID, 99, resultTitle) //nolint:errcheck
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{        //nolint:errcheck
				"total":           total,
				"generated":       generated,
				"failed":          failed,
				"failed_chapters": failedChapters,
			})
		})
	}

	// chapter_review_batch: 批量章节审查（novelID 存在 EntityID，无需额外参数）
	if svcs.QualityControlService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterReviewBatch, func(ctx context.Context, t *model.AsyncTask) {
			novelID := t.EntityID
			if novelID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			progressFn := func(done, total int) {
				if total > 0 {
					svcs.TaskService.UpdateProgress(t.TaskID, done*100/total) //nolint:errcheck
				}
			}
			if err := svcs.QualityControlService.BatchReviewNovelChapters(ctx, t.TenantID, novelID, progressFn); err != nil {
				logger.Errorf("TaskService resume chapter_review_batch %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"novel_id": novelID}) //nolint:errcheck
		})
	}

	// chapter_rewrite_instr: 按指令修改章节（需从 ParamsJSON 读 instruction）
	if svcs.QualityControlService != nil && svcs.ChapterService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeChapterRewriteInstr, func(ctx context.Context, t *model.AsyncTask) {
			chapterID := t.EntityID
			var params struct {
				Instruction string `json:"instruction"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if chapterID == 0 || params.Instruction == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID)         //nolint:errcheck
			svcs.TaskService.UpdateProgress(t.TaskID, 10) //nolint:errcheck
			newContent, err := svcs.QualityControlService.RewriteByInstruction(ctx, chapterID, params.Instruction)
			if err != nil {
				logger.Errorf("TaskService resume chapter_rewrite_instr %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 80) //nolint:errcheck
			if err2 := svcs.ChapterService.ArchiveVersionBeforeRewrite(chapterID, params.Instruction); err2 != nil {
				logger.Errorf("TaskService resume chapter_rewrite_instr %s archive failed: %v", t.TaskID, err2)
			}
			updated, applyErr := svcs.ChapterService.ApplyRewrittenContent(chapterID, newContent)
			if applyErr != nil {
				logger.Errorf("TaskService resume chapter_rewrite_instr %s apply failed: %v", t.TaskID, applyErr)
				svcs.TaskService.Fail(t.TaskID, "保存修改内容失败: "+applyErr.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"chapter": updated}) //nolint:errcheck
		})
	}

	// image_upscale: 高清放大（AI 增强，需从 ParamsJSON 读 image_url/scale）
	if svcs.AIService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeImageUpscale, func(ctx context.Context, t *model.AsyncTask) {
			var params struct {
				ImageURL string `json:"image_url"`
				Scale    int    `json:"scale"`
				NovelID  uint   `json:"novel_id"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.ImageURL == "" {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			if params.Scale <= 0 {
				params.Scale = 2
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			newURL, err := svcs.AIService.UpscaleImage(ctx, t.TenantID, params.NovelID, params.ImageURL, params.Scale)
			if err != nil {
				logger.Errorf("TaskService resume image_upscale %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, "高清处理失败: "+err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"image_url": newURL}) //nolint:errcheck
		})
	}

	// lipsync: shot-level lip-sync video generation (VideoHandler.GenerateLipSync)
	if svcs.VideoService != nil {
		svcs.TaskService.RegisterResumeHandler(service.TaskTypeLipSync, func(ctx context.Context, t *model.AsyncTask) {
			shotID := t.EntityID
			if shotID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			var params struct {
				VideoID uint                   `json:"video_id"`
				Req     service.LipSyncRequest `json:"req"`
			}
			if t.ParamsJSON != "" {
				_ = json.Unmarshal([]byte(t.ParamsJSON), &params)
			}
			if params.VideoID == 0 {
				svcs.TaskService.Fail(t.TaskID, "任务超时或服务重启，请重新提交") //nolint:errcheck
				return
			}
			svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
			result, err := svcs.VideoService.GenerateLipSyncVideoWithReq(ctx, params.VideoID, shotID, params.Req)
			if err != nil {
				logger.Errorf("TaskService resume lipsync %s failed: %v", t.TaskID, err)
				svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.UpdateProgress(t.TaskID, 20) //nolint:errcheck
			// 同步轮询直到完成（timeout 内部控制）
			if pollErr := svcs.VideoService.PollLipSyncUntilDone(ctx, params.VideoID, shotID); pollErr != nil {
				logger.Errorf("TaskService resume lipsync(poll) %s failed: %v", t.TaskID, pollErr)
				svcs.TaskService.Fail(t.TaskID, pollErr.Error()) //nolint:errcheck
				return
			}
			svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"lip_sync_task_id": result.TaskID}) //nolint:errcheck
		})
	}
}

// resumeImportCrawl continues an already-running crawl job for an existing novel
// (ImportHandler.ResumeCrawl): kicks off NovelImportService.ResumeCrawl, then polls progress
// until it settles, mirroring the goroutine body ResumeCrawl used to run inline.
func resumeImportCrawl(svcs *Services, t *model.AsyncTask, novelID uint) {
	svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck
	if err := svcs.NovelImportService.ResumeCrawl(novelID); err != nil {
		svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
		return
	}
	for {
		progress, _ := svcs.NovelImportService.GetCrawlProgress(novelID)
		if progress == nil {
			svcs.TaskService.Fail(t.TaskID, "crawl job not found") //nolint:errcheck
			return
		}
		if progress.Status == "completed" || progress.Status == "failed" || progress.Status == "paused" {
			break
		}
		pct := 0
		if progress.Total > 0 {
			pct = int(float64(progress.Done) / float64(progress.Total) * 100)
		}
		svcs.TaskService.UpdateProgress(t.TaskID, pct)             //nolint:errcheck
		svcs.TaskService.SetMeta(t.TaskID, map[string]interface{}{ //nolint:errcheck
			"novel_id":      novelID,
			"crawl_done":    progress.Done,
			"crawl_total":   progress.Total,
			"crawl_current": progress.Current,
		})
		time.Sleep(2 * time.Second)
	}
	svcs.TaskService.Complete(t.TaskID, map[string]interface{}{"novel_id": novelID, "message": "续爬完成"}) //nolint:errcheck
}

// resumeImportCrawlFresh runs a brand-new crawl import (ImportHandler.ImportFromCrawl):
// creates the novel + chapter stubs via Import (which kicks off background crawling),
// registers a completion callback that triggers analysis, then polls progress until it
// settles, mirroring the goroutine body ImportFromCrawl used to run inline.
func resumeImportCrawlFresh(svcs *Services, t *model.AsyncTask, req *service.ImportRequest) {
	svcs.TaskService.SetRunning(t.TaskID) //nolint:errcheck

	result, err := svcs.NovelImportService.Import(req)
	if err != nil {
		logger.Errorf("TaskService resume import(crawl) %s failed: %v", t.TaskID, err)
		svcs.TaskService.Fail(t.TaskID, err.Error()) //nolint:errcheck
		return
	}
	novelID := result.NovelID
	svcs.TaskService.UpdateProgress(t.TaskID, 5)               //nolint:errcheck
	svcs.TaskService.SetMeta(t.TaskID, map[string]interface{}{ //nolint:errcheck
		"step":        "爬取章节内容中...",
		"novel_id":    novelID,
		"crawl_total": result.TotalChapters,
	})

	analysisDone := make(chan string, 1)
	svcs.NovelImportService.RegisterCrawlDoneCallback(novelID, func() {
		id := ""
		if svcs.NovelAnalysisService != nil {
			if aid, aErr := svcs.NovelAnalysisService.StartAnalysis(t.TenantID, novelID, false); aErr == nil {
				id = aid
			}
		}
		analysisDone <- id
	})

	noProgressCount := 0
	const maxNoProgress = 20 // 最多等 40 秒无变化
	for {
		progress, _ := svcs.NovelImportService.GetCrawlProgress(novelID)
		if progress == nil {
			noProgressCount++
			if noProgressCount >= maxNoProgress {
				logger.Printf("TaskService resume import(crawl) %s: crawl progress lost for novel %d, aborting poll", t.TaskID, novelID)
				break
			}
		} else {
			noProgressCount = 0
			if progress.Status == "completed" || progress.Status == "failed" || progress.Status == "paused" {
				break
			}
			pct := 5
			if progress.Total > 0 {
				pct = 5 + int(float64(progress.Done)/float64(progress.Total)*55)
			}
			svcs.TaskService.UpdateProgress(t.TaskID, pct)             //nolint:errcheck
			svcs.TaskService.SetMeta(t.TaskID, map[string]interface{}{ //nolint:errcheck
				"step":          "爬取章节内容中...",
				"novel_id":      novelID,
				"crawl_done":    progress.Done,
				"crawl_total":   progress.Total,
				"crawl_current": progress.Current,
			})
		}
		time.Sleep(2 * time.Second)
	}

	analysisTaskID := ""
	select {
	case id := <-analysisDone:
		analysisTaskID = id
	case <-time.After(10 * time.Second):
		// 回调可能已在爬取完成前触发，直接继续
	}

	svcs.TaskService.Complete(t.TaskID, map[string]interface{}{ //nolint:errcheck
		"novel_id":          novelID,
		"imported_chapters": result.ImportedChapters,
		"analysis_task_id":  analysisTaskID,
		"message":           fmt.Sprintf("爬取完成，共 %d 章", result.ImportedChapters),
	})
}
