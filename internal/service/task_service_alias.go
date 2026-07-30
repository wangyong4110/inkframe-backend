package service

import (
	"context"

	"github.com/inkframe/inkframe-backend/internal/async"
)

// TaskService re-exports async.TaskService so that handler/wiring code can
// continue to reference service.TaskService without importing the async package.
type TaskService = async.TaskService

// NewTaskService is a convenience alias for async.NewTaskService.
var NewTaskService = async.NewTaskService

// Task type constant aliases — re-exported from async package for backward compatibility
// with handler code that references service.TaskType*.
const (
	TaskTypeStoryboardGen          = async.TaskTypeStoryboardGen
	TaskTypeScreenplayGen          = async.TaskTypeScreenplayGen
	TaskTypeChapterGen             = async.TaskTypeChapterGen
	TaskTypeVoiceGen               = async.TaskTypeVoiceGen
	TaskTypeImageGen               = async.TaskTypeImageGen
	TaskTypeThreeView              = async.TaskTypeThreeView
	TaskTypeCharGen                = async.TaskTypeCharGen
	TaskTypeItemExtract            = async.TaskTypeItemExtract
	TaskTypePlotExtract            = async.TaskTypePlotExtract
	TaskTypeAssetGen               = async.TaskTypeAssetGen
	TaskTypeSceneAnchorExtract     = async.TaskTypeSceneAnchorExtract
	TaskTypeChapterSummaryBatch    = async.TaskTypeChapterSummaryBatch
	TaskTypeSFXGen                 = async.TaskTypeSFXGen
	TaskTypeChapterReview          = async.TaskTypeChapterReview
	TaskTypeChapterOutlineReview   = async.TaskTypeChapterOutlineReview
	TaskTypeChapterReviewBatch     = async.TaskTypeChapterReviewBatch
	TaskTypeStoryboardReview       = async.TaskTypeStoryboardReview
	TaskTypeStoryboardOptimize     = async.TaskTypeStoryboardOptimize
	TaskTypeStoryboardSceneRegen   = async.TaskTypeStoryboardSceneRegen
	TaskTypeImport                 = async.TaskTypeImport
	TaskTypeNovelAnalysis          = async.TaskTypeNovelAnalysis
	TaskTypeRewriteAnalysis        = async.TaskTypeRewriteAnalysis
	TaskTypeRewriteChapters        = async.TaskTypeRewriteChapters
	TaskTypeCrawlJob               = async.TaskTypeCrawlJob
	TaskTypeSkillGen               = async.TaskTypeSkillGen
	TaskTypeBatchChapterGen        = async.TaskTypeBatchChapterGen
	TaskTypeCharReanalyze          = async.TaskTypeCharReanalyze
	TaskTypeChapterCharExtract     = async.TaskTypeChapterCharExtract
	TaskTypeChapterSceneExtract    = async.TaskTypeChapterSceneExtract
	TaskTypeChapterItemExtract     = async.TaskTypeChapterItemExtract
	TaskTypeCharImageGen           = async.TaskTypeCharImageGen
	TaskTypeVoicePreview           = async.TaskTypeVoicePreview
	TaskTypeLookPromptGen          = async.TaskTypeLookPromptGen
	TaskTypeLookImageGen           = async.TaskTypeLookImageGen
	TaskTypeCoverImageGen          = async.TaskTypeCoverImageGen
	TaskTypeImageEdit              = async.TaskTypeImageEdit
	TaskTypeImageUpscale           = async.TaskTypeImageUpscale
	TaskTypeLipSync                = async.TaskTypeLipSync
	TaskTypeChapterRewriteInstr    = async.TaskTypeChapterRewriteInstr
	TaskTypeVideoGen               = async.TaskTypeVideoGen
	TaskTypeVideoSynthesis         = async.TaskTypeVideoSynthesis
	TaskTypeChapterPostProcess     = async.TaskTypeChapterPostProcess
)

// ImageRefResult is a single image-reference search hit returned to the frontend.
type ImageRefResult struct {
	URL      string `json:"url"`
	ThumbURL string `json:"thumb_url,omitempty"`
	Tags     string `json:"tags,omitempty"`
	PageURL  string `json:"page_url,omitempty"`
}

// ImageRefSearcher is the interface implemented by image-reference search backends.
type ImageRefSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]ImageRefResult, error)
	Name() string
}
