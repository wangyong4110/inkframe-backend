package main

import (
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/inkframe/inkframe-backend/internal/vector"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Repositories 仓库层
type Repositories struct {
	NovelRepo               *repository.NovelRepository
	ChapterRepo             *repository.ChapterRepository
	CharacterRepo           *repository.CharacterRepository
	WorldviewRepo           *repository.WorldviewRepository
	AIModelRepo             *repository.AIModelRepository
	TaskModelConfigRepo     *repository.TaskModelConfigRepository
	VideoRepo               *repository.VideoRepository
	StoryboardRepo          *repository.StoryboardRepository
	KnowledgeBaseRepo       *repository.KnowledgeBaseRepository
	ModelProviderRepo       *repository.ModelProviderRepository
	ModelComparisonRepo     *repository.ModelComparisonRepository
	TaskRepo                *repository.TaskRepository

	ChapterVersionRepo      *repository.ChapterVersionRepository
	SnapshotRepo            *repository.CharacterStateSnapshotRepository
	CharacterLookRepo       *repository.CharacterLookRepository
	UserRepo                *repository.UserRepository
	TenantRepo              *repository.TenantRepository
	TenantUserRepo          *repository.TenantUserRepository
	ArcSummaryRepo          *repository.ArcSummaryRepository
	ItemRepo                *repository.ItemRepository
	SkillRepo               *repository.SkillRepository
	ChapterItemRepo         *repository.ChapterItemRepository
	ChapterCharacterRepo    *repository.ChapterCharacterRepository
	PlotPointRepo           *repository.PlotPointRepository
	HookChainRepo           *repository.HookChainRepository
	SatisfactionPointRepo   *repository.SatisfactionPointRepository
	ConflictArcRepo         *repository.ConflictArcRepository
	SceneAnchorRepo            *repository.SceneAnchorRepository
	ChapterSceneAnchorRepo     *repository.ChapterSceneAnchorRepository
	SceneConsistencyLogRepo    *repository.SceneConsistencyLogRepository
	SystemSettingRepo       *repository.SystemSettingRepository
	ShotVoiceSegmentRepo       *repository.ShotVoiceSegmentRepository
	ReviewRecordRepo            *repository.ReviewRecordRepository
	IgnoredReviewIssueRepo      *repository.IgnoredReviewIssueRepository
	ShotSFXItemRepo         *repository.ShotSFXItemRepository
	VideoBGMSegmentRepo     *repository.VideoBGMSegmentRepository
	RewriteProjectRepo           *repository.RewriteProjectRepository
	LiteraryAnalysisRepo         *repository.LiteraryAnalysisRepository
	RewriteBibleRepo             *repository.RewriteBibleRepository
	ChapterRewriteTaskRepo       *repository.ChapterRewriteTaskRepository
	RewriteContinuityIndexRepo   *repository.RewriteContinuityIndexRepository
	RewriteChapterSummaryRepo    *repository.RewriteChapterSummaryRepository
	PlatformAccountRepo      *repository.PlatformAccountRepository
	VideoPublishRecordRepo   *repository.VideoPublishRecordRepository
	ContinuityReportRepo     *repository.ContinuityReportRepository
	// Asset Library
	AssetRepo               *repository.AssetRepository
	TagRepo                 *repository.TagRepository
	AssetVersionRepo        *repository.AssetVersionRepository
	AssetCollectionRepo     *repository.AssetCollectionRepository
	AssetShareRequestRepo   *repository.AssetShareRequestRepository
	AssetUsageRepo          *repository.AssetUsageRepository
	AssetLikeRepo           *repository.AssetLikeRepository
	AssetCommentRepo        *repository.AssetCommentRepository
	CrawlJobRepo            *repository.CrawlJobRepository
	ShareLinkRepo           *repository.ShareLinkRepository
	SearchLogRepo           *repository.SearchLogRepository
	TenantQuotaRepo         *repository.AssetStorageQuotaRepository
	VideoLikeRepo           *repository.VideoLikeRepository
	VideoCommentRepo        *repository.VideoCommentRepository
	NovelLikeRepo           *repository.NovelLikeRepository
	NovelCommentRepo        *repository.NovelCommentRepository
	ChapterLikeRepo         *repository.ChapterLikeRepository
	ChapterCommentRepo      *repository.ChapterCommentRepository
	ReadingProgressRepo     *repository.ReadingProgressRepository
	ChapterReadRecordRepo   *repository.ChapterReadRecordRepository
	UserTokenRepo           *repository.UserTokenRepository
	NovelCrawlJobRepo        *repository.NovelCrawlJobRepository
	NotificationRepo         *repository.NotificationRepository
	ForeshadowRepo           *repository.ForeshadowRepository
	NovelOutlineVersionRepo  *repository.NovelOutlineVersionRepository
	OutlineReviewRepo        *repository.OutlineReviewRepository
	OutlineSynthesisRepo     *repository.NovelOutlineSynthesisRepository
	NovelMemberRepo          *repository.NovelMemberRepository
	EditingLockRepo          *repository.EditingLockRepository
}

// initRepositories 初始化仓库层
func initRepositories(db *gorm.DB, redis *redis.Client) *Repositories {
	return &Repositories{
		NovelRepo:               repository.NewNovelRepository(db, redis),
		ChapterRepo:             repository.NewChapterRepository(db, redis),
		CharacterRepo:           repository.NewCharacterRepository(db),
		WorldviewRepo:           repository.NewWorldviewRepository(db),
		AIModelRepo:             repository.NewAIModelRepository(db),
		TaskModelConfigRepo:     repository.NewTaskModelConfigRepository(db),
		VideoRepo:               repository.NewVideoRepository(db),
		StoryboardRepo:          repository.NewStoryboardRepository(db),
		KnowledgeBaseRepo:       repository.NewKnowledgeBaseRepository(db),
		ModelProviderRepo:       repository.NewModelProviderRepository(db),
		ModelComparisonRepo:     repository.NewModelComparisonRepository(db),
		TaskRepo:                repository.NewTaskRepository(db),

		ChapterVersionRepo:      repository.NewChapterVersionRepository(db),
		SnapshotRepo:            repository.NewCharacterStateSnapshotRepository(db),
		CharacterLookRepo:       repository.NewCharacterLookRepository(db),
		UserRepo:                repository.NewUserRepository(db),
		TenantRepo:              repository.NewTenantRepository(db),
		TenantUserRepo:          repository.NewTenantUserRepository(db),
		ArcSummaryRepo:          repository.NewArcSummaryRepository(db),
		ItemRepo:                repository.NewItemRepository(db),
		SkillRepo:               repository.NewSkillRepository(db),
		ChapterItemRepo:         repository.NewChapterItemRepository(db),
		ChapterCharacterRepo:    repository.NewChapterCharacterRepository(db),
		PlotPointRepo:           repository.NewPlotPointRepository(db),
		HookChainRepo:           repository.NewHookChainRepository(db),
		SatisfactionPointRepo:   repository.NewSatisfactionPointRepository(db),
		ConflictArcRepo:         repository.NewConflictArcRepository(db),
		SceneAnchorRepo:         repository.NewSceneAnchorRepository(db),
		ChapterSceneAnchorRepo:  repository.NewChapterSceneAnchorRepository(db),
		SceneConsistencyLogRepo: repository.NewSceneConsistencyLogRepository(db),
		SystemSettingRepo:       repository.NewSystemSettingRepository(db),
		ShotVoiceSegmentRepo:       repository.NewShotVoiceSegmentRepository(db),
		ReviewRecordRepo:       repository.NewReviewRecordRepository(db),
		IgnoredReviewIssueRepo: repository.NewIgnoredReviewIssueRepository(db),
		ShotSFXItemRepo:         repository.NewShotSFXItemRepository(db),
		VideoBGMSegmentRepo:     repository.NewVideoBGMSegmentRepository(db),
		RewriteProjectRepo:           repository.NewRewriteProjectRepository(db, redis),
		LiteraryAnalysisRepo:         repository.NewLiteraryAnalysisRepository(db),
		RewriteBibleRepo:             repository.NewRewriteBibleRepository(db),
		ChapterRewriteTaskRepo:       repository.NewChapterRewriteTaskRepository(db),
		RewriteContinuityIndexRepo:   repository.NewRewriteContinuityIndexRepository(db),
		RewriteChapterSummaryRepo:    repository.NewRewriteChapterSummaryRepository(db),
		PlatformAccountRepo:      repository.NewPlatformAccountRepository(db),
		VideoPublishRecordRepo:   repository.NewVideoPublishRecordRepository(db),
		ContinuityReportRepo:     repository.NewContinuityReportRepository(db),
		// Asset Library
		AssetRepo:             repository.NewAssetRepository(db),
		TagRepo:               repository.NewTagRepository(db),
		AssetVersionRepo:      repository.NewAssetVersionRepository(db),
		AssetCollectionRepo:   repository.NewAssetCollectionRepository(db),
		AssetShareRequestRepo: repository.NewAssetShareRequestRepository(db),
		AssetUsageRepo:        repository.NewAssetUsageRepository(db),
		AssetLikeRepo:         repository.NewAssetLikeRepository(db),
		AssetCommentRepo:      repository.NewAssetCommentRepository(db),
		CrawlJobRepo:          repository.NewCrawlJobRepository(db),
		ShareLinkRepo:         repository.NewShareLinkRepository(db),
		SearchLogRepo:         repository.NewSearchLogRepository(db),
		TenantQuotaRepo:       repository.NewAssetStorageQuotaRepository(db),
		VideoLikeRepo:         repository.NewVideoLikeRepository(db),
		VideoCommentRepo:      repository.NewVideoCommentRepository(db),
		NovelLikeRepo:         repository.NewNovelLikeRepository(db),
		NovelCommentRepo:      repository.NewNovelCommentRepository(db),
		ChapterLikeRepo:       repository.NewChapterLikeRepository(db),
		ChapterCommentRepo:    repository.NewChapterCommentRepository(db),
		ReadingProgressRepo:   repository.NewReadingProgressRepository(db),
		ChapterReadRecordRepo: repository.NewChapterReadRecordRepository(db),
		UserTokenRepo:         repository.NewUserTokenRepository(db),
		NovelCrawlJobRepo:        repository.NewNovelCrawlJobRepository(db),
		NotificationRepo:         repository.NewNotificationRepository(db),
		ForeshadowRepo:           repository.NewForeshadowRepository(db),
		NovelOutlineVersionRepo:  repository.NewNovelOutlineVersionRepository(db),
		OutlineReviewRepo:        repository.NewOutlineReviewRepository(db),
		OutlineSynthesisRepo:     repository.NewNovelOutlineSynthesisRepository(db),
		NovelMemberRepo:          repository.NewNovelMemberRepository(db),
		EditingLockRepo:          repository.NewEditingLockRepository(db),
	}
}

// Services 服务层
type Services struct {
	NovelAnalysisService        *service.NovelAnalysisService
	McpService                  *service.McpService
	NovelService                *service.NovelService
	ChapterService              *service.ChapterService
	CharacterService            *service.CharacterService
	WorldviewService            *service.WorldviewService
	QualityControlService       *service.QualityControlService
	VideoService                *service.VideoService
	ModelService                *service.ModelService
	PromptService               *service.PromptService
	ContinuityService           *service.ContinuityService
	KnowledgeService            *service.KnowledgeService

	ChapterVersionService       *service.ChapterVersionService
	ForeshadowService           *service.ForeshadowService
	TimelineService             *service.TimelineService
	CharacterArcService         *service.CharacterArcService
	StyleService                *service.StyleService
	GenerationContextService    *service.GenerationContextService
	ImageGenerationService      *service.ImageGenerationService
	StoryboardService           *service.StoryboardService
	VideoEnhancementService     *service.VideoEnhancementService
	CharacterConsistencyService *service.CharacterConsistencyService
	FrameGeneratorService       *service.FrameGeneratorService
	ConsistencyValidatorService *service.ConsistencyValidatorService
	BGMService                  *service.BGMService
	SFXService                  *service.SFXService
	CrawlerService              *crawler.NovelCrawler
	NovelImportService          *service.NovelImportService
	NovelToVideoService         *service.NovelToVideoService
	AuthService                 *service.AuthService
	TenantService               *service.TenantService
	SMSService                  *service.SMSService
	OAuthService                *service.OAuthService
	FrontendURL                 string
	ItemService                 *service.ItemService
	SkillService                *service.SkillService
	PlotPointService            *service.PlotPointService
	TaskService                 *service.TaskService
	AIService                   *service.AIService
	HookChainService            *service.HookChainService
	SatisfactionPointService    *service.SatisfactionPointService
	ConflictArcService          *service.ConflictArcService
	PacingService               *service.PacingService
	SceneAnchorService          *service.SceneAnchorService
	SceneConsistencyService     *service.SceneConsistencyService
	RewriteService              *service.RewriteService
	PlatformPublishService      *service.PlatformPublishService
	AssetService                *service.AssetService
	ReadingService              *service.ReadingService
	NotificationService         *service.NotificationService
	ForeshadowCRUDService       *service.ForeshadowCRUDService
	// ── Webhook ──
	WebhookService              *service.WebhookService
	// ── Audit ──
	AuditService                *service.AuditService
	// ── Email notification ──
	EmailNotificationService    *service.EmailNotificationService
	// ── Outline Review ──
	OutlineReviewService        *service.OutlineReviewService
	// ── Collab ──
	CollabService               *service.CollabService
}

// ──────────────────────────────────────────────────────────────
// Service group structs  (intermediate, only used during init)
// ──────────────────────────────────────────────────────────────

// coreSvcs holds foundational AI/model services that all other groups depend on.
type coreSvcs struct {
	AI        *service.AIService
	Model     *service.ModelService
	Task      *service.TaskService
	PlotPoint *service.PlotPointService
	Quality   *service.QualityControlService
}

// contentSvcs holds novel/chapter/character domain services.
type contentSvcs struct {
	Novel             *service.NovelService
	Chapter           *service.ChapterService
	Character         *service.CharacterService
	Worldview         *service.WorldviewService
	Knowledge         *service.KnowledgeService
	Continuity        *service.ContinuityService
	Prompt            *service.PromptService
	ChapterVersion    *service.ChapterVersionService
	Foreshadow        *service.ForeshadowService
	Timeline          *service.TimelineService
	CharacterArc      *service.CharacterArcService
	Style             *service.StyleService
	GenContext        *service.GenerationContextService
	ImageGen          *service.ImageGenerationService
	HookChain         *service.HookChainService
	SatisfactionPoint *service.SatisfactionPointService
	ConflictArc       *service.ConflictArcService
	Pacing            *service.PacingService
	Item              *service.ItemService
	Skill             *service.SkillService
	SceneAnchor       *service.SceneAnchorService
	ForeshadowCRUD    *service.ForeshadowCRUDService
	NovelAnalysis     *service.NovelAnalysisService
	NovelImport       *service.NovelImportService
	Crawler           *crawler.NovelCrawler
}

// videoSvcs holds video / media generation services.
type videoSvcs struct {
	Video                *service.VideoService
	Storyboard           *service.StoryboardService
	Enhancement          *service.VideoEnhancementService
	CharConsistency      *service.CharacterConsistencyService
	BGM                  *service.BGMService
	FrameGenerator       *service.FrameGeneratorService
	ConsistencyValidator *service.ConsistencyValidatorService
	NovelToVideo         *service.NovelToVideoService
	SceneConsistency     *service.SceneConsistencyService
}

// ──────────────────────────────────────────────────────────────
// Group initializers
// ──────────────────────────────────────────────────────────────

func initCoreServiceGroup(repos *Repositories, aiManager *ai.ModelManager, cfg *config.Config) *coreSvcs {
	// AI服务（注入 providerRepo 以支持按租户加载 AK/SK，注入 novelRepo 以读取小说项目级 AI 配置）
	// WithTaskRouting: configure via config.yaml ai.tasks section (no AI.Tasks config key exists yet).
	aiSvc := service.NewAIService(repos.AIModelRepo, repos.TaskModelConfigRepo, aiManager, repos.ModelProviderRepo).
		WithNovelRepo(repos.NovelRepo).
		WithEncryptionKey(cfg.Server.EncryptionKey).
		WithImageConcurrency(5)

	// 模型服务（注入 aiService 以支持 TestProvider 实例化验证）
	modelSvc := service.NewModelService(repos.AIModelRepo, repos.ModelProviderRepo, repos.TaskModelConfigRepo, repos.ModelComparisonRepo, aiSvc)
	// Fix 11: Assert AIService is non-nil before seeding providers.
	if aiSvc == nil {
		panic("FATAL: AIService is nil before SeedAllProviders")
	}
	modelSvc.SeedAllProviders()

	// 异步任务服务
	taskSvc := service.NewTaskService(repos.TaskRepo)

	// 剧情点服务
	plotPointSvc := service.NewPlotPointService(repos.PlotPointRepo, aiSvc).
		WithChapterRepo(repos.ChapterRepo)

	// 质量控制服务
	qualitySvc := service.NewQualityControlService(aiSvc, repos.ChapterRepo, repos.NovelRepo).
		WithReviewRepos(repos.ReviewRecordRepo, repos.IgnoredReviewIssueRepo)

	return &coreSvcs{AI: aiSvc, Model: modelSvc, Task: taskSvc, PlotPoint: plotPointSvc, Quality: qualitySvc}
}

func initContentServiceGroup(db *gorm.DB, repos *Repositories, core *coreSvcs, aiManager *ai.ModelManager, vectorStore *vector.StoreManager) *contentSvcs {
	aiSvc := core.AI

	// 小说服务
	novelSvc := service.NewNovelService(repos.NovelRepo, repos.ChapterRepo, aiSvc).
		WithCharacterRepos(repos.CharacterRepo, repos.SnapshotRepo).
		WithPlotPointService(core.PlotPoint).
		WithOutlineVersionRepo(repos.NovelOutlineVersionRepo).
		WithMemberRepo(repos.NovelMemberRepo)

	// 角色服务
	characterSvc := service.NewCharacterService(repos.CharacterRepo, aiSvc).
		WithChapterCharacterRepo(repos.ChapterCharacterRepo).
		WithSnapshotRepo(repos.SnapshotRepo).
		WithLookRepo(repos.CharacterLookRepo).
		WithNovelRepo(repos.NovelRepo).
		WithChapterRepo(repos.ChapterRepo).
		WithModelRepo(repos.AIModelRepo)

	// 世界观服务
	worldviewSvc := service.NewWorldviewService(repos.WorldviewRepo, aiSvc).
		WithNovelRepos(repos.NovelRepo, repos.ChapterRepo)

	// 提示词 / 连续性
	promptSvc := service.NewPromptService(nil)
	continuitySvc := service.NewContinuityService(repos.CharacterRepo, repos.ChapterRepo).
		WithReportRepo(repos.ContinuityReportRepo)

	// 知识库服务（传入 AI provider 用于向量化）
	var defaultAIProvider ai.AIProvider
	if aiManager != nil {
		if p, err := aiManager.GetProvider(""); err == nil {
			defaultAIProvider = p
		} else {
			logger.Errorf("Warning: could not load default AI provider: %v — knowledge base embedding will be unavailable", err)
		}
	}
	if defaultAIProvider == nil {
		logger.Errorf("Warning: no default AI provider available; knowledge base embedding disabled")
	}
	knowledgeSvc := service.NewKnowledgeService(repos.KnowledgeBaseRepo, vectorStore, defaultAIProvider)

	// 章节版本 / 伏笔 / 时间线 / 角色弧光 / 风格
	chapterVersionSvc := service.NewChapterVersionService(repos.ChapterVersionRepo, repos.ChapterRepo)
	foreshadowSvc := service.NewForeshadowService(repos.KnowledgeBaseRepo, aiSvc)
	timelineSvc := service.NewTimelineService(repos.ChapterRepo)
	characterArcSvc := service.NewCharacterArcService(repos.CharacterRepo, repos.SnapshotRepo)
	styleSvc := service.NewStyleService(nil)

	// 生成上下文服务
	genCtxSvc := service.NewGenerationContextService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, characterArcSvc, foreshadowSvc)

	// 层次化叙事记忆服务
	narrativeMemorySvc := service.NewNarrativeMemoryService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, repos.ArcSummaryRepo, aiSvc).
		WithSnapshotRepo(repos.SnapshotRepo).
		WithOutlineVersionRepo(repos.NovelOutlineVersionRepo)

	// 戏剧张力服务
	hookChainSvc := service.NewHookChainService(repos.HookChainRepo)
	satisfactionSvc := service.NewSatisfactionPointService(repos.SatisfactionPointRepo)
	conflictArcSvc := service.NewConflictArcService(repos.ConflictArcRepo)
	pacingSvc := service.NewPacingService(repos.ChapterRepo, repos.SatisfactionPointRepo)

	// 章节服务（依赖 genCtxSvc + narrativeMemorySvc）
	chapterSvc := service.NewChapterService(repos.ChapterRepo, repos.NovelRepo, aiSvc, genCtxSvc).
		WithNarrativeMemory(narrativeMemorySvc).
		WithDramaticServices(hookChainSvc, satisfactionSvc, conflictArcSvc).
		WithPlotPointRepo(repos.PlotPointRepo).
		WithCharacterRepo(repos.CharacterRepo).
		WithSnapshotRepo(repos.SnapshotRepo).
		WithVersionRepo(repos.ChapterVersionRepo).
		WithContinuityService(continuitySvc).
		WithKnowledgeService(knowledgeSvc).
		WithQualityService(core.Quality).
		WithForeshadowRepo(repos.ForeshadowRepo)

	// 图像生成服务
	imageGenSvc := service.NewImageGenerationService(aiSvc)

	// 物品 / 技能 / 场景锚点
	itemSvc := service.NewItemService(repos.ItemRepo, repos.ChapterItemRepo, repos.ChapterRepo, aiSvc).
		WithNovelRepo(repos.NovelRepo)
	skillSvc := service.NewSkillService(repos.SkillRepo, aiSvc).
		WithNovelRepo(repos.NovelRepo)
	sceneAnchorSvc := service.NewSceneAnchorService(repos.SceneAnchorRepo, repos.StoryboardRepo, aiSvc, repos.NovelRepo).
		WithChapterRepo(repos.ChapterRepo).
		WithChapterSceneAnchorRepo(repos.ChapterSceneAnchorRepo)

	// 伏笔 CRUD 服务（带 AI 提取能力）
	foreshadowCRUDSvc := service.NewForeshadowCRUDService(repos.ForeshadowRepo).
		WithAIDeps(aiSvc, repos.NovelRepo, repos.ChapterRepo)

	// 小说分析服务（依赖大部分上面的服务）
	novelAnalysisSvc := service.NewNovelAnalysisService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, repos.WorldviewRepo, novelSvc, aiSvc).
		WithItemRepo(repos.ItemRepo).
		WithItemService(itemSvc).
		WithPlotPointService(core.PlotPoint).
		WithSceneAnchorService(sceneAnchorSvc).
		WithForeshadowService(foreshadowCRUDSvc).
		WithTaskService(core.Task).
		WithModelRepo(repos.AIModelRepo).
		WithLookRepo(repos.CharacterLookRepo)

	// 导入服务
	crawlerSvc := crawler.NewNovelCrawler(db)
	novelImportSvc := service.NewNovelImportService(repos.NovelRepo, repos.ChapterRepo, crawlerSvc).
		WithNarrativeMemory(narrativeMemorySvc)

	return &contentSvcs{
		Novel: novelSvc, Chapter: chapterSvc, Character: characterSvc, Worldview: worldviewSvc,
		Knowledge: knowledgeSvc, Continuity: continuitySvc, Prompt: promptSvc,
		ChapterVersion: chapterVersionSvc, Foreshadow: foreshadowSvc, Timeline: timelineSvc,
		CharacterArc: characterArcSvc, Style: styleSvc, GenContext: genCtxSvc,
		ImageGen: imageGenSvc, HookChain: hookChainSvc, SatisfactionPoint: satisfactionSvc,
		ConflictArc: conflictArcSvc, Pacing: pacingSvc, Item: itemSvc, Skill: skillSvc,
		SceneAnchor: sceneAnchorSvc, ForeshadowCRUD: foreshadowCRUDSvc,
		NovelAnalysis: novelAnalysisSvc, NovelImport: novelImportSvc,
		Crawler: crawlerSvc,
	}
}

func initVideoServiceGroup(repos *Repositories, core *coreSvcs, content *contentSvcs, cfg *config.Config) *videoSvcs {
	aiSvc := core.AI

	// 视频服务（视频提供商从 DB 按租户加载，无需静态注册）
	videoSvc := service.NewVideoService(repos.VideoRepo, repos.StoryboardRepo, repos.ChapterRepo, repos.CharacterRepo, repos.NovelRepo, repos.TenantRepo, aiSvc, nil)

	// 图像服务（内部，用于视频增强和一致性服务）
	imageSvc := service.NewImageService(nil)

	// 分镜 / 视频增强 / BGM / 角色一致性
	intelligentStoryboardSvc := service.NewIntelligentStoryboardService(aiSvc, imageSvc)
	storyboardSvc := service.NewStoryboardService(videoSvc, aiSvc)
	enhancementSvc := service.NewVideoEnhancementService(imageSvc, "/tmp/inkframe-enhance")
	bgmSvc := service.NewBGMService(getEnv("BGM_DIR", "")).
		WithAIService(aiSvc).
		WithAssetRepo(repos.AssetRepo, repos.TagRepo)

	charConsistencySvc := service.NewCharacterConsistencyService(imageSvc, nil, aiSvc)

	// 将依赖注回 videoService
	videoSvc.WithConsistencyService(charConsistencySvc)
	videoSvc.WithBGMService(bgmSvc)
	videoSvc.WithBGMSegmentRepo(repos.VideoBGMSegmentRepo)
	videoSvc.WithPlotPointRepo(repos.PlotPointRepo)
	videoSvc.WithSystemSettingRepo(repos.SystemSettingRepo)
	videoSvc.WithChapterCharacterRepo(repos.ChapterCharacterRepo)
	videoSvc.WithLookRepo(repos.CharacterLookRepo)
	videoSvc.WithVideoConcurrency(1)
	videoSvc.WithAudioConcurrency(3)

	// 帧生成 / 一致性验证 / 小说转视频
	frameGenSvc := service.NewFrameGeneratorService(aiSvc)
	consistencyValidatorSvc := service.NewConsistencyValidatorService(aiSvc)
	novelToVideoSvc := service.NewNovelToVideoService(
		content.NovelImport,
		intelligentStoryboardSvc,
		frameGenSvc,
		enhancementSvc,
		consistencyValidatorSvc,
		repos.NovelRepo,
		repos.ChapterRepo,
		repos.VideoRepo,
		repos.StoryboardRepo,
	)

	sceneConsistencySvc := service.NewSceneConsistencyService(repos.SceneConsistencyLogRepo, aiSvc)
	videoSvc.WithSceneConsistencyService(sceneConsistencySvc)

	return &videoSvcs{
		Video: videoSvc, Storyboard: storyboardSvc, Enhancement: enhancementSvc,
		CharConsistency: charConsistencySvc, BGM: bgmSvc, FrameGenerator: frameGenSvc,
		ConsistencyValidator: consistencyValidatorSvc,
		NovelToVideo: novelToVideoSvc, SceneConsistency: sceneConsistencySvc,
	}
}


// initServices 初始化服务层
func initServices(db *gorm.DB, repos *Repositories, aiManager *ai.ModelManager, vectorStore *vector.StoreManager, cfg *config.Config, redisClient *redis.Client) *Services {
	core    := initCoreServiceGroup(repos, aiManager, cfg)
	core.Task.WithDB(db)
	content := initContentServiceGroup(db, repos, core, aiManager, vectorStore)
	video   := initVideoServiceGroup(repos, core, content, cfg)

	// 改写服务（依赖统一异步任务系统）
	rewriteSvc := service.NewRewriteService(
		repos.RewriteProjectRepo,
		repos.LiteraryAnalysisRepo,
		repos.RewriteBibleRepo,
		repos.ChapterRewriteTaskRepo,
		repos.ChapterRepo,
		repos.NovelRepo,
		core.AI,
	).WithTaskService(core.Task).
		WithContinuityRepo(repos.RewriteContinuityIndexRepo).
		WithSummaryRepo(repos.RewriteChapterSummaryRepo).
		WithChapterVersionRepo(repos.ChapterVersionRepo)

	// 认证 / 租户 / 通信服务（依赖 db 和 redisClient，数量少，直接内联）
	smsSvc := service.NewSMSService(redisClient, cfg.SMS)

	var emailSender service.EmailSender
	switch {
	case cfg.Email.WebhookURL != "":
		emailSender = service.NewWebhookEmailSender(cfg.Email.WebhookURL, cfg.Email.WebhookToken)
		logger.Printf("[Email] Webhook sender configured: url=%s require_verification=%v",
			cfg.Email.WebhookURL, cfg.Email.RequireVerification)
	case cfg.Email.Host != "":
		emailSender = service.NewSMTPEmailSender(cfg.Email.Host, cfg.Email.Port, cfg.Email.Username, cfg.Email.Password, cfg.Email.From, cfg.Email.UseTLS)
		logger.Printf("[Email] SMTP configured: host=%s port=%d tls=%v require_verification=%v",
			cfg.Email.Host, cfg.Email.Port, cfg.Email.UseTLS, cfg.Email.RequireVerification)
	default:
		emailSender = &service.NoopEmailSender{}
		logger.Printf("[Email] No sender configured (noop); require_verification=%v", cfg.Email.RequireVerification)
	}

	frontendURL := cfg.Server.FrontendURL
	if frontendURL == "" {
		frontendURL = "http://localhost:3000"
	}

	authSvc := service.NewAuthService(db, repos.UserRepo, repos.TenantRepo, repos.TenantUserRepo, cfg.Server.JWTSecret, cfg.Server.JWTExpiry).
		WithSMSService(smsSvc).
		WithTokenRepo(repos.UserTokenRepo).
		WithRedis(redisClient).
		WithEmailSender(emailSender).
		WithAppBaseURL(frontendURL).
		WithAppName(cfg.Server.AppName).
		WithEmailVerifyTTL(cfg.Email.EmailVerifyTTL).
		WithRequireVerification(cfg.Email.RequireVerification)
	tenantSvc := service.NewTenantService(repos.TenantRepo, repos.TenantUserRepo)
	oauthSvc  := service.NewOAuthService(cfg.OAuth)
	mcpSvc    := service.NewMcpService(db)

	// 通知服务（先于内容服务后置注入步骤创建）
	notifSvc := service.NewNotificationService(repos.NotificationRepo)

	// 后置注入：将通知服务与技能仓库注入章节生成管道
	content.Novel.WithNotificationService(notifSvc)
	content.Chapter.WithNotificationService(notifSvc).WithSkillRepo(repos.SkillRepo)
	// NOTE: WithStorage/WithDB/WithAnalysisService/WithAIService/WithNotificationService/WithCrawlJobRepo
	// are injected in main.go after storageSvc and db are available.

	return &Services{
		// ── AI core ──
		AIService:             core.AI,
		ModelService:          core.Model,
		TaskService:           core.Task,
		PlotPointService:      core.PlotPoint,
		QualityControlService: core.Quality,
		// ── Content ──
		NovelService:          content.Novel,
		ChapterService:        content.Chapter,
		CharacterService:      content.Character,
		WorldviewService:      content.Worldview,
		KnowledgeService:      content.Knowledge,
		ContinuityService:     content.Continuity,
		PromptService:         content.Prompt,
		ChapterVersionService: content.ChapterVersion,
		ForeshadowService:     content.Foreshadow,
		TimelineService:       content.Timeline,
		CharacterArcService:   content.CharacterArc,
		StyleService:          content.Style,
		GenerationContextService: content.GenContext,
		ImageGenerationService:   content.ImageGen,
		HookChainService:         content.HookChain,
		SatisfactionPointService: content.SatisfactionPoint,
		ConflictArcService:       content.ConflictArc,
		PacingService:            content.Pacing,
		ItemService:              content.Item,
		SkillService:             content.Skill,
		SceneAnchorService:       content.SceneAnchor,
		NovelAnalysisService:     content.NovelAnalysis,
		NovelImportService:       content.NovelImport,
		CrawlerService:           content.Crawler,
		// ── Video ──
		VideoService:                video.Video,
		StoryboardService:           video.Storyboard,
		VideoEnhancementService:     video.Enhancement,
		CharacterConsistencyService: video.CharConsistency,
		BGMService:                  video.BGM,
		FrameGeneratorService:       video.FrameGenerator,
		ConsistencyValidatorService: video.ConsistencyValidator,
		NovelToVideoService:         video.NovelToVideo,
		SceneConsistencyService:     video.SceneConsistency,
		// ── Auth / platform ──
		AuthService:   authSvc,
		TenantService: tenantSvc,
		SMSService:    smsSvc,
		OAuthService:  oauthSvc,
		McpService:    mcpSvc,
		FrontendURL:   cfg.Server.FrontendURL,
		// ── Rewrite ──
		RewriteService: rewriteSvc,
		// ── Platform publish ──
		PlatformPublishService: service.NewPlatformPublishService(
			repos.PlatformAccountRepo,
			repos.VideoPublishRecordRepo,
			core.Task,
		),
		// ── Asset Library ──
		AssetService: service.NewAssetService(
			repos.AssetRepo,
			repos.TagRepo,
			repos.AssetVersionRepo,
			repos.AssetCollectionRepo,
			repos.AssetShareRequestRepo,
			repos.AssetUsageRepo,
			repos.AssetLikeRepo,
			repos.AssetCommentRepo,
			repos.CrawlJobRepo,
			repos.ShareLinkRepo,
			repos.SearchLogRepo,
			repos.TenantQuotaRepo,
			core.Task,
		),
		// ── Reading / Chapter Social ──
		ReadingService: service.NewReadingService(
			repos.ChapterLikeRepo,
			repos.ChapterCommentRepo,
			repos.ReadingProgressRepo,
			repos.ChapterReadRecordRepo,
			repos.ChapterRepo,
		).WithNovelRepo(repos.NovelRepo),
		// ── Notifications ──
		NotificationService: notifSvc,
		// ── Dedicated foreshadow table ──
		ForeshadowCRUDService: content.ForeshadowCRUD,
		// ── Webhook ──
		WebhookService: service.NewWebhookService(db),
		// ── Audit ──
		AuditService: service.NewAuditService(db),
		// ── Email notification ──
		EmailNotificationService: service.NewEmailNotificationService(cfg.Email),
		// ── Outline Review ──
		OutlineReviewService: service.NewOutlineReviewService(
			repos.OutlineReviewRepo,
			repos.OutlineSynthesisRepo,
			repos.ChapterRepo,
			repos.NovelRepo,
			core.AI,
		).WithForeshadowRepo(repos.ForeshadowRepo).
			WithArcSummaryRepo(repos.ArcSummaryRepo),
		// ── Collab ──
		CollabService: service.NewCollabService(
			repos.NovelMemberRepo,
			repos.EditingLockRepo,
			repos.UserRepo,
			repos.NovelRepo,
		).WithTenantUserRepo(repos.TenantUserRepo).
			WithNotificationService(notifSvc),
	}
}

// Handlers 处理器
type Handlers struct {
	NovelHandler       *handler.NovelHandler
	ChapterHandler     *handler.ChapterHandler
	CharacterHandler   *handler.CharacterHandler
	VideoHandler       *handler.VideoHandler
	ModelHandler       *handler.ModelHandler
	McpHandler         *handler.McpHandler
	StyleHandler       *handler.StyleHandler
	ContextHandler     *handler.ContextHandler
	AuthHandler        *handler.AuthHandler
	ImportHandler      *handler.ImportHandler
	WorldviewHandler   *handler.WorldviewHandler
	TenantHandler      *handler.TenantHandler
	ItemHandler        *handler.ItemHandler
	SkillHandler       *handler.SkillHandler
	UploadHandler      *handler.UploadHandler
	PlotPointHandler   *handler.PlotPointHandler
	TaskHandler        *handler.TaskHandler
	MediaHandler       *handler.MediaHandler
	SceneAnchorHandler *handler.SceneAnchorHandler
	SystemHandler      *handler.SystemHandler
	FsHandler          *handler.FsHandler
	RewriteHandler     *handler.RewriteHandler
	PlatformHandler    *handler.PlatformHandler
	AssetHandler       *handler.AssetHandler
	ImageHandler       *handler.ImageHandler
	WebSearchHandler      *handler.WebSearchHandler
	WikiSearchHandler     *handler.WikiSearchHandler
	StoryPatternHandler   *handler.StoryPatternHandler
	ImageRefSearchHandler *handler.ImageRefSearchHandler
	ColorPaletteHandler   *handler.ColorPaletteHandler
	NotificationHandler   *handler.NotificationHandler
	KnowledgeHandler      *handler.KnowledgeHandler
	DramaticHandler       *handler.DramaticHandler
	DashboardHandler      *handler.DashboardHandler
	ForeshadowHandler     *handler.ForeshadowHandler
	WebhookHandler        *handler.WebhookHandler
	AuditHandler          *handler.AuditHandler
	OutlineReviewHandler  *handler.OutlineReviewHandler
	CollabHandler         *handler.CollabHandler
}

// initHandlers 初始化处理器
func initHandlers(services *Services, storageSvc storage.Service, db *gorm.DB, repos *Repositories, cfg *config.Config) *Handlers {
	return &Handlers{
		NovelHandler: handler.NewNovelHandler(
			services.NovelService,
			services.ChapterService,
			services.ForeshadowService,
			services.TimelineService,
			services.QualityControlService,
		).WithTaskService(services.TaskService).WithModelService(services.ModelService).
			WithNotificationService(services.NotificationService),
		ChapterHandler: handler.NewChapterHandler(
			services.ChapterService,
			services.ChapterVersionService,
			services.QualityControlService,
			services.TaskService,
		).WithNovelService(services.NovelService).
			WithContinuityService(services.ContinuityService),
		CharacterHandler: handler.NewCharacterHandler(
			services.CharacterService,
			services.CharacterArcService,
			services.ImageGenerationService,
		).WithChapterService(services.ChapterService).WithStorage(storageSvc).WithTaskService(services.TaskService).WithAIService(services.AIService).WithNovelService(services.NovelService),
		VideoHandler: handler.NewVideoHandler(
			services.VideoService,
			services.StoryboardService,
			services.VideoEnhancementService,
			services.CharacterConsistencyService,
		).WithTaskService(services.TaskService).WithSFXService(services.SFXService).WithSFXItemRepo(repos.ShotSFXItemRepo).
			WithBGMService(services.BGMService).WithBGMRepo(repos.VideoBGMSegmentRepo).
			WithSubtitleService(service.NewSubtitleService()).WithStorage(storageSvc).WithAssetRepo(repos.AssetRepo).
			WithCapCutSegmentRepo(repos.ShotVoiceSegmentRepo). // P1-2: VoiceSegment audio in CapCut exports
			WithServerBaseURL(buildServerBaseURL(cfg)),         // 本地/DB 存储媒体 URL 解析
		ModelHandler:   handler.NewModelHandler(services.ModelService),
		McpHandler:     handler.NewMcpHandler(services.McpService),
		StyleHandler:   handler.NewStyleHandler(services.StyleService),
		ContextHandler: handler.NewContextHandler(services.GenerationContextService),
		AuthHandler: handler.NewAuthHandler(services.AuthService).
			WithSMSService(services.SMSService).
			WithOAuthService(services.OAuthService).
			WithFrontendURL(services.FrontendURL),
		ImportHandler: handler.NewImportHandler(services.NovelImportService, services.NovelToVideoService).
			SetAnalysisService(services.NovelAnalysisService).
			WithTaskService(services.TaskService).
			WithNovelService(services.NovelService),
		WorldviewHandler:   handler.NewWorldviewHandler(services.WorldviewService),
		TenantHandler:      handler.NewTenantHandler(services.TenantService),
		ItemHandler:        handler.NewItemHandler(services.ItemService, services.ChapterService).WithStorage(storageSvc).WithTaskService(services.TaskService).WithNovelService(services.NovelService),
		SkillHandler:       handler.NewSkillHandler(services.SkillService).WithNovelService(services.NovelService).WithTaskService(services.TaskService),
		UploadHandler:      handler.NewUploadHandler(storageSvc),
		PlotPointHandler:   handler.NewPlotPointHandler(services.PlotPointService).WithChapterService(services.ChapterService).WithTaskService(services.TaskService).WithNovelService(services.NovelService),
		TaskHandler:        handler.NewTaskHandler(services.TaskService),
		MediaHandler:       handler.NewMediaHandler(db),
		SceneAnchorHandler: handler.NewSceneAnchorHandler(services.SceneAnchorService, services.SceneConsistencyService).WithTaskService(services.TaskService).WithChapterService(services.ChapterService).WithVideoService(services.VideoService).WithNovelService(services.NovelService).WithStorageService(storageSvc),
		SystemHandler: handler.NewSystemHandler(repos.SystemSettingRepo),
		FsHandler:     handler.NewFsHandler(getEnv("BGM_DIR", "")),
		RewriteHandler: handler.NewRewriteHandler(services.RewriteService),
		PlatformHandler: handler.NewPlatformHandler(services.NovelService, services.VideoService, services.PlatformPublishService).
			WithChapterService(services.ChapterService).
			WithReadingService(services.ReadingService),
		AssetHandler:    handler.NewAssetHandler(services.AssetService),
		ImageHandler:    handler.NewImageHandler(services.AIService).WithTaskService(services.TaskService),
		WebSearchHandler: handler.NewWebSearchHandler(
			service.NewWebSearcher(
				cfg.WebSearch.Provider,
				getEnv("WEB_SEARCH_API_KEY", cfg.WebSearch.APIKey),
				cfg.WebSearch.Endpoint,
			),
		),
		WikiSearchHandler:   handler.NewWikiSearchHandler(service.NewWikiSearcher()),
		StoryPatternHandler: handler.NewStoryPatternHandler(service.NewStoryPatternService()),
		ImageRefSearchHandler: handler.NewImageRefSearchHandler(
			service.NewImageRefSearcher(
				"pixabay",
				getEnv("PIXABAY_API_KEY", ""),
			),
		),
		ColorPaletteHandler: handler.NewColorPaletteHandler(service.NewColorPaletteService()),
		NotificationHandler: handler.NewNotificationHandler(services.NotificationService),
		KnowledgeHandler:    handler.NewKnowledgeHandler(services.KnowledgeService).WithNovelService(services.NovelService),
		DramaticHandler: handler.NewDramaticHandler(
			services.HookChainService,
			services.SatisfactionPointService,
			services.ConflictArcService,
			services.PacingService,
		),
		DashboardHandler:  handler.NewDashboardHandler(db),
		ForeshadowHandler: handler.NewForeshadowHandler(services.ForeshadowCRUDService),
		WebhookHandler:       handler.NewWebhookHandler(services.WebhookService),
		AuditHandler:         handler.NewAuditHandler(services.AuditService),
		OutlineReviewHandler: handler.NewOutlineReviewHandler(services.OutlineReviewService, services.TaskService),
		CollabHandler:        handler.NewCollabHandler(services.CollabService),
	}
}

// buildServerBaseURL 从 config 构造服务器自身 base URL，用于解析本地/DB 存储的相对媒体路径。
func buildServerBaseURL(cfg *config.Config) string {
	host := cfg.Server.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, cfg.Server.Port)
}
