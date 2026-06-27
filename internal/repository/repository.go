// Package repository provides data access layer implementations for all domain
// entities. Each domain has its own file:
//
//   - novel.go       — NovelRepository, NovelLikeRepository, NovelCommentRepository
//   - chapter.go     — ChapterRepository, ChapterVersionRepository, ChapterItemRepository, ChapterCharacterRepository
//   - character.go   — CharacterRepository, CharacterStateSnapshotRepository
//   - worldview.go   — WorldviewRepository, ItemRepository, SkillRepository
//   - video.go       — VideoRepository, VideoLikeRepository, VideoCommentRepository
//   - storyboard.go  — StoryboardRepository, ShotVoiceSegmentRepository, ShotSFXItemRepository, VideoBGMSegmentRepository, StoryboardReviewRecordRepository
//   - knowledge.go   — KnowledgeBaseRepository
//   - scene_anchor.go — SceneAnchorRepository, SceneConsistencyLogRepository
//   - model_provider.go — ModelProviderRepository, AIModelRepository, TaskModelConfigRepository, ModelComparisonRepository
//   - system.go      — SystemSettingRepository, HookChainRepository, SatisfactionPointRepository, ConflictArcRepository
//   - rewrite.go     — RewriteProjectRepository, LiteraryAnalysisRepository, RewriteBibleRepository, ChapterRewriteTaskRepository
//   - common.go      — shared helpers: isForeignKeyError
//
// Pre-existing separate files:
//   - arc_summary_repository.go  — ArcSummaryRepository
//   - task_repository.go         — TaskRepository
//   - user_repository.go         — UserRepository, TenantRepository, TenantUserRepository
//   - asset_repository.go        — AssetRepository, TagRepository, AssetVersionRepository, AssetCollectionRepository, etc.
//   - platform_repository.go     — PlatformAccountRepository, VideoPublishRecordRepository
//   - plot_point_repository.go   — PlotPointRepository
package repository
