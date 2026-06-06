package main

import (
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// schemaVersion must be bumped whenever any model struct is added or changed.
// Format: YYYY-MM-DD-vN. This allows autoMigrate to be skipped on unchanged restarts.
const schemaVersion = "2026-06-06-v7"

// ensureCriticalColumns 在版本检查之前无条件补全关键列（应对版本跳过导致列缺失的情况）。
// 直接执行 ALTER TABLE ADD COLUMN，MySQL 1060 = 列已存在时静默忽略。
func ensureCriticalColumns(db *gorm.DB) {
	type colAdd struct{ table, col, def string }
	additions := []colAdd{
		// ink_novel 广场社交字段（2026-05-10 新增）
		{"ink_novel", "view_count", "INT NOT NULL DEFAULT 0"},
		{"ink_novel", "like_count", "INT NOT NULL DEFAULT 0"},
		{"ink_novel", "comment_count", "INT NOT NULL DEFAULT 0"},
		{"ink_novel", "hot_score", "DOUBLE NOT NULL DEFAULT 0"},
		{"ink_novel", "is_published", "TINYINT(1) NOT NULL DEFAULT 0"},
		{"ink_novel", "published_at", "DATETIME(3) NULL"},
		{"ink_novel", "visibility", "VARCHAR(20) NOT NULL DEFAULT 'private'"},
		{"ink_novel", "plaza_tags", "VARCHAR(500) NULL"},
		// ink_chapter 广场发布字段（2026-05-11 新增，与内容状态解耦）
		{"ink_chapter", "is_published", "TINYINT(1) NOT NULL DEFAULT 0"},
		{"ink_chapter", "published_at", "DATETIME(3) NULL"},
		// ink_character 新增字段（2026-05-25 新增）
		{"ink_character", "visual_prompt", "TEXT NULL"},
		// ink_novel 提示词语言（2026-05-26 新增）
		{"ink_novel", "prompt_language", "VARCHAR(10) NOT NULL DEFAULT 'zh'"},
		// ink_rewrite_bible 命名风格 & 道具映射（2026-05-26 新增）
		{"ink_rewrite_bible", "naming_style", "TEXT NULL"},
		{"ink_rewrite_bible", "props_transform", "TEXT NULL"},
		// ink_rewrite_bible 新增禁止元素拆分字段 & 意象映射（2026-05-27 新增）
		{"ink_rewrite_bible", "forbidden_phrases", "TEXT NULL"},
		{"ink_rewrite_bible", "forbidden_dialogues", "TEXT NULL"},
		{"ink_rewrite_bible", "imagery_transform", "TEXT NULL"},
		// ink_chapter_rewrite_task 质量评分 & 去AI字段（2026-05-27 Phase1 新增）
		{"ink_chapter_rewrite_task", "quality_score", "DOUBLE NOT NULL DEFAULT 0"},
		{"ink_chapter_rewrite_task", "deai_applied", "TINYINT(1) NOT NULL DEFAULT 0"},
		{"ink_chapter_rewrite_task", "consistency_issues", "VARCHAR(1000) NULL"},
		// ink_chapter_rewrite_task 新增原子写入字段（2026-05-27 新增）
		{"ink_chapter_rewrite_task", "attempt_content", "LONGTEXT NULL"},
		{"ink_chapter_rewrite_task", "summary_written", "TINYINT(1) NOT NULL DEFAULT 0"},
		// ink_literary_analysis 新增节奏/意象/章际钩子字段（2026-05-27 新增）
		{"ink_literary_analysis", "rhythm_pattern", "TEXT NULL"},
		{"ink_literary_analysis", "imagery_system", "TEXT NULL"},
		{"ink_literary_analysis", "inter_chapter_hooks", "TEXT NULL"},
		// ink_crawl_job TaskService 集成字段（2026-05-28 新增）
		{"ink_crawl_job", "task_id", "VARCHAR(50) NULL"},
		{"ink_crawl_job", "tenant_id", "INT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_async_task 续跑参数快照（2026-05-28 新增）
		{"ink_async_task", "params", "TEXT NULL"},
		// ink_character 人物深层动机（2026-05-30 新增）
		{"ink_character", "inner_conflict", "TEXT NULL"},
		{"ink_character", "core_desire", "TEXT NULL"},
		// ink_model_provider 并发度（2026-05-30 新增）
		{"ink_model_provider", "concurrency", "INT NOT NULL DEFAULT 0"},
		// ink_task_model_config provider 显式绑定（2026-05-31 新增）
		{"ink_task_model_config", "primary_provider_id", "INT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_novel 内容审核字段（2026-05-31 新增）
		{"ink_novel", "review_status", "VARCHAR(20) NOT NULL DEFAULT 'draft'"},
		{"ink_novel", "review_note", "VARCHAR(500) NULL"},
		{"ink_novel", "reviewed_at", "DATETIME(3) NULL"},
		{"ink_novel", "reviewed_by", "INT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_novel 已发布章节计数（2026-05-31 新增）
		{"ink_novel", "published_count", "INT NOT NULL DEFAULT 0"},
		// ink_knowledge_base 来源章节（2026-05-31 新增，用于重提取时去重）
		{"ink_knowledge_base", "source_chapter_id", "INT UNSIGNED NULL"},
		// ink_video 分镜审查状态（2026-05-31 新增）
		{"ink_video", "review_status", "VARCHAR(20) NOT NULL DEFAULT 'none'"},
		// ink_worldview 租户隔离（2026-05-31 新增，修复跨租户数据泄露）
		{"ink_worldview", "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		// users 账号安全锁定字段
		{"users", "failed_login_count", "INT NOT NULL DEFAULT 0"},
		{"users", "lock_until", "DATETIME(3) NULL"},
		// users 最后登录时间（auth_service.go UpdateLastLogin 依赖此列）
		{"users", "last_login_at", "DATETIME(3) NULL"},
		// ink_model_usage_log 租户隔离（Fix 1: logUsage 增加 tenantID）
		{"ink_model_usage_log", "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 0"},
		// 多租户隔离列补充（Fix 8: 确保关键表均有 tenant_id）
		{"ink_character",      "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		{"ink_item",           "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		{"ink_plot_point",     "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		{"ink_scene_anchor",   "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		{"ink_knowledge_base", "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		// ink_worldview_entity 租户隔离（Fix 5: WorldviewEntity 缺失 tenant_id 补全）
		{"ink_worldview_entity", "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		// ink_quality_report 租户隔离（Fix 2: QualityReport 新增 tenant_id）
		{"ink_quality_report", "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
		// ink_chapter 连贯性高危问题标记（2026-06-01 新增）
		{"ink_chapter", "continuity_blocked", "TINYINT(1) NOT NULL DEFAULT 0"},
		// ink_async_task 死信队列字段（2026-06-01 新增）
		{"ink_async_task", "retry_count", "INT NOT NULL DEFAULT 0"},
		{"ink_async_task", "max_retries", "INT NOT NULL DEFAULT 3"},
		{"ink_async_task", "failure_log", "TEXT NULL"},
		// ink_novel 文件去重哈希（2026-06-01 新增）
		{"ink_novel", "source_file_hash", "VARCHAR(64)"},
		// ink_chapter 质量状态（2026-06-01 Fix1 新增）
		{"ink_chapter", "quality_status", "VARCHAR(20) NOT NULL DEFAULT 'ok'"},
		{"ink_chapter", "quality_issues", "TEXT NULL"},
		// ink_novel 自动审查配置（2026-06-01 新增）
		{"ink_novel", "auto_review_rounds",    "INT NOT NULL DEFAULT 0"},
		{"ink_novel", "auto_review_min_score", "DOUBLE NOT NULL DEFAULT 80"},
		// 专业小说质量改进（2026-06-02 新增）
		{"ink_novel",      "core_theme",           "TEXT NULL"},                              // 全书核心主题
		{"ink_chapter",    "reader_expectations",  "TEXT NULL"},                              // 读者期待状态
		{"ink_character",  "arc_design",           "TEXT NULL"},                              // 角色弧光设计 JSON
		{"ink_character",  "current_arc_stage",    "VARCHAR(50) NULL"},                       // 当前弧光阶段
		{"ink_foreshadow", "planted_chapter_no",   "INT NOT NULL DEFAULT 0"},                 // 种下章节序号
		{"ink_foreshadow", "payoff_chapter_no",    "INT NOT NULL DEFAULT 0"},                 // 预期回收章节序号
		{"ink_foreshadow", "importance",           "VARCHAR(20) NOT NULL DEFAULT 'normal'"}, // 重要程度
		// 章节连贯性修复（2026-06-02-v2 新增）
		{"ink_chapter", "chapter_end_state", "TEXT NULL"}, // 章末精确状态快照（供下一章连续性锚点）
		// 场景一致性日志新增字段（2026-06-06-v1 新增）
		{"ink_scene_consistency_log", "time_score",     "DECIMAL(4,3) NOT NULL DEFAULT 1"},
		{"ink_scene_consistency_log", "suggested_fix",  "TEXT NULL"},
	}
	for _, a := range additions {
		// 先查 information_schema，列已存在则跳过，避免触发 GORM 的 Error 1060 日志
		var cnt int64
		db.Raw(
			"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
			a.table, a.col,
		).Scan(&cnt)
		if cnt > 0 {
			continue // 列已存在，无需操作
		}
		sql := "ALTER TABLE `" + a.table + "` ADD COLUMN `" + a.col + "` " + a.def
		if err := db.Exec(sql).Error; err != nil {
			logger.Warnf("ensureCriticalColumns: %s.%s: %v", a.table, a.col, err)
		} else {
			logger.Infof("ensureCriticalColumns: added column %s.%s", a.table, a.col)
		}
	}
}

// dropLegacyCharacterColumns 删除 2026-05-15 角色/道具/场景锚点字段简化后遗留的旧列。
// 使用 information_schema 检查列存在性，幂等安全，可重复调用。
func dropLegacyCharacterColumns(db *gorm.DB) {
	legacy := []struct{ table, col string }{
		// ink_character: Gender/Archetype/Appearance/Personality/Background/Relations/
		// CharacterArc/DialogueStyle/VisualDesign/CoverImage → 合并为 Description
		{"ink_character", "gender"},
		{"ink_character", "archetype"},
		{"ink_character", "appearance"},
		{"ink_character", "personality"},
		{"ink_character", "background"},
		{"ink_character", "relations"},
		{"ink_character", "character_arc"},
		{"ink_character", "dialogue_style"},
		{"ink_character", "visual_design"},
		{"ink_character", "cover_image"},
		// ink_item: Category/Appearance → 合并为 Description
		{"ink_item", "category"},
		{"ink_item", "appearance"},
		// ink_scene_anchor: RefImageShotID → 已删除
		{"ink_scene_anchor", "ref_image_shot_id"},
	}
	for _, d := range legacy {
		var cnt int64
		db.Raw(
			"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
			d.table, d.col,
		).Scan(&cnt)
		if cnt == 0 {
			continue
		}
		if err := db.Exec("ALTER TABLE `" + d.table + "` DROP COLUMN `" + d.col + "`").Error; err != nil {
			logger.Printf("[dropLegacyCharacterColumns] drop %s.%s: %v", d.table, d.col, err)
		}
	}
}

// autoMigrate 自动迁移（带版本跳过优化）
// 如果 DB 中记录的 schema 版本与 schemaVersion 一致，跳过迁移直接返回，大幅加速启动。
// 当模型变更时，请同时更新 schemaVersion 常量。
func autoMigrate(db *gorm.DB) error {
	// 先确保版本表存在（首次启动时自动建表，几乎无开销）
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ink_schema_version (
		id   INT NOT NULL DEFAULT 1,
		ver  VARCHAR(64) NOT NULL DEFAULT '',
		PRIMARY KEY (id)
	)`).Error; err != nil {
		return err
	}

	// 无条件补全关键列（防止版本跳过导致列缺失）
	ensureCriticalColumns(db)
	// 删除遗留列（2026-05-15 模型简化后遗留在 DB 中的旧字段）
	dropLegacyCharacterColumns(db)

	// 读取当前已迁移版本
	var storedVer string
	db.Raw("SELECT ver FROM ink_schema_version WHERE id = 1").Scan(&storedVer)
	if storedVer == schemaVersion {
		logger.Printf("autoMigrate: schema version %s already up-to-date, skipping", schemaVersion)
		return nil
	}

	logger.Printf("autoMigrate: migrating schema %s → %s", storedVer, schemaVersion)
	preMigrateCleanup(db)
	// 禁用外键约束创建：避免手动加列类型不匹配、循环依赖等问题
	// AutoMigrate 只负责同步列定义，外键由应用层保证一致性
	db.DisableForeignKeyConstraintWhenMigrating = true
	if err := db.AutoMigrate(
		&model.Tenant{},
		&model.User{},
		&model.TenantUser{},
		&model.Novel{},
		&model.NovelVideoConfig{},
		&model.Chapter{},
		&model.PlotPoint{},
		&model.Character{},
		&model.CharacterAppearance{},
		&model.CharacterStateSnapshot{},
		&model.CharacterLook{},
		&model.Worldview{},
		&model.WorldviewEntity{},
		&model.ReferenceNovel{},
		&model.ReferenceChapter{},
		&model.KnowledgeBase{},
		&model.AIModel{},
		&model.ModelProvider{},
		&model.TaskModelConfig{},
		&model.ModelComparisonExperiment{},
		&model.ExperimentResult{},
		&model.ModelUsageLog{},
		&model.Video{},
		&model.StoryboardShot{},
		&model.QualityReport{},
		&model.ChapterVersion{},
		&model.McpTool{},
		&model.ModelMcpBinding{},
		&model.ArcSummary{},
		&model.Item{},
		&model.Skill{},
		&model.ChapterItem{},
		&model.ChapterCharacter{},
		&model.AsyncTask{},
		&model.MediaAsset{},
		&model.HookChain{},
		&model.SatisfactionPoint{},
		&model.ConflictArc{},
		&model.SceneAnchor{},
		&model.SceneConsistencyLog{},
		&model.SystemSetting{},
		&model.ShotVoiceSegment{},
		&model.ReviewRecord{},
		&model.IgnoredReviewIssue{},
		&model.ShotSFXItem{},
		&model.VideoBGMSegment{},
		&model.RewriteProject{},
		&model.LiteraryAnalysis{},
		&model.RewriteBible{},
		&model.ChapterRewriteTask{},
		&model.RewriteContinuityIndex{},
		&model.RewriteChapterSummary{},
		&model.PlatformAccount{},
		&model.VideoPublishRecord{},
		// Asset Library (Phase 3)
		&model.Asset{},
		&model.Tag{},
		&model.AssetTagMap{},
		&model.AssetShareRequest{},
		&model.AssetVersion{},
		&model.AssetCollection{},
		&model.AssetCollectionItem{},
		&model.CrawlJob{},
		&model.AssetLike{},
		&model.AssetUsage{},
		&model.AssetComment{},
		&model.ShareLink{},
		&model.AssetRequest{},
		&model.SearchLog{},
		&model.AssetStorageQuota{},
		// 广场社交
		&model.VideoLike{},
		&model.VideoComment{},
		&model.NovelLike{},
		&model.NovelComment{},
		// 章节社交 & 阅读进度
		&model.ChapterLike{},
		&model.ChapterComment{},
		&model.ReadingProgress{},
		&model.ChapterReadRecord{},
		// 用户 token（密码重置 & 邮箱验证）
		&model.UserToken{},
		// 小说章节爬取任务（进度持久化）
		&model.NovelCrawlJob{},
		// 站内通知
		&model.Notification{},
		// 连续性检查报告
		&model.ContinuityReportRecord{},
		// 专用伏笔表
		&model.Foreshadow{},
		// Webhook 订阅与投递记录
		&model.WebhookSubscription{},
		&model.WebhookDelivery{},
		// 审计日志
		&model.AuditLog{},
		// 小说大纲历史版本
		&model.NovelOutlineVersion{},
		// 章节大纲审查
		&model.OutlineReview{},
		&model.NovelOutlineSynthesis{},
	); err != nil {
		return err
	}

	// 数据迁移：将历史 status='published' 的章节修正为 is_published=true, status='completed'
	if err := db.Exec(`UPDATE ink_chapter SET is_published = 1, status = 'completed' WHERE status = 'published'`).Error; err != nil {
		logger.Warnf("autoMigrate: chapter status migration failed: %v", err)
	}

	// 数据迁移（H-1）：将 style_prompt 中误存的大纲 JSON 迁移到 outline 字段
	if err := db.Exec(`UPDATE ink_novel SET outline = style_prompt WHERE style_prompt LIKE '{"chapters":%' AND (outline = '' OR outline IS NULL)`).Error; err != nil {
		logger.Warnf("autoMigrate: outline backfill failed: %v", err)
	}
	if err := db.Exec(`UPDATE ink_novel SET style_prompt = '' WHERE style_prompt LIKE '{"chapters":%'`).Error; err != nil {
		logger.Warnf("autoMigrate: style_prompt cleanup failed: %v", err)
	}

	// 数据迁移（2026-05-15-v2）：将 Character 旧字段合并到 description
	// 仅在旧列尚未删除时执行（列已删则跳过，避免 Unknown column 错误）
	var charAppearanceExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_character' AND COLUMN_NAME = 'appearance'`).Scan(&charAppearanceExists)
	if charAppearanceExists > 0 {
		if err := db.Exec(`UPDATE ink_character SET description = CONCAT_WS('\n',
			IF(appearance != '' AND appearance IS NOT NULL, CONCAT('外貌：', appearance), NULL),
			IF(personality != '' AND personality IS NOT NULL, CONCAT('性格：', personality), NULL),
			IF(background != '' AND background IS NOT NULL, CONCAT('背景：', background), NULL),
			IF(character_arc != '' AND character_arc IS NOT NULL, CONCAT('弧光：', character_arc), NULL),
			IF(dialogue_style != '' AND dialogue_style IS NOT NULL, CONCAT('说话风格：', dialogue_style), NULL)
		) WHERE (description = '' OR description IS NULL)
			AND (appearance IS NOT NULL AND appearance != ''
				OR personality IS NOT NULL AND personality != ''
				OR background IS NOT NULL AND background != ''
				OR character_arc IS NOT NULL AND character_arc != ''
				OR dialogue_style IS NOT NULL AND dialogue_style != '')`).Error; err != nil {
			logger.Warnf("autoMigrate: character description migration failed: %v", err)
		}
	}

	// 数据迁移（2026-05-15-v2）：将 Item 旧字段合并到 description
	// 仅在旧列尚未删除时执行（列已删则跳过，避免 Unknown column 错误）
	var itemCategoryExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_item' AND COLUMN_NAME = 'category'`).Scan(&itemCategoryExists)
	if itemCategoryExists > 0 {
		if err := db.Exec(`UPDATE ink_item SET description = CONCAT_WS('，',
			IF(category != '' AND category IS NOT NULL, category, NULL),
			IF(description != '' AND description IS NOT NULL, description, NULL),
			IF(appearance != '' AND appearance IS NOT NULL, appearance, NULL)
		) WHERE (category IS NOT NULL AND category != '') OR (appearance IS NOT NULL AND appearance != '')`).Error; err != nil {
			logger.Warnf("autoMigrate: item description migration failed: %v", err)
		}
	}

	// 数据迁移（2026-06-06-v5）：修正 doubao-speech 的 seed-tts-2.0/1.0 任务类型
	// 这两个是资源端点 ID，不应出现在角色音色选择列表（voice_gen）中
	if err := db.Exec(`UPDATE ink_ai_model m
		JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
		SET m.suitable_tasks = '["tts_resource"]', m.is_available = 0
		WHERE p.name = 'doubao-speech'
		  AND m.name IN ('seed-tts-2.0', 'seed-tts-1.0')
		  AND m.deleted_at IS NULL
		  AND m.suitable_tasks LIKE '%voice_gen%'`).Error; err != nil {
		logger.Warnf("autoMigrate: doubao-speech task fix failed: %v", err)
	}

	// 数据迁移（2026-06-06-v6）：doubao-speech V3 中错误添加的 _moon_bigtts 音色
	// 月亮系列属于 seed-tts-1.0 资源，V3 provider 的 2.0 发音人应为 _uranus_bigtts
	if err := db.Exec(`UPDATE ink_ai_model m
		JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
		SET m.suitable_tasks = '["legacy_voice"]', m.is_available = 0
		WHERE p.name = 'doubao-speech'
		  AND m.name LIKE '%_moon_bigtts'
		  AND m.deleted_at IS NULL`).Error; err != nil {
		logger.Warnf("autoMigrate: doubao-speech moon_bigtts disable failed: %v", err)
	}

	// 数据迁移（2026-06-06-v6）：修正 doubao-speech-v1 中 阳光青年 2.0 音色显示名称错误
	// zh_male_yangguangqingnian_uranus_bigtts 之前被错误命名为"天才童声 2.0"
	if err := db.Exec(`UPDATE ink_ai_model m
		JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
		SET m.display_name = '阳光青年 2.0'
		WHERE p.name = 'doubao-speech-v1'
		  AND m.name = 'zh_male_yangguangqingnian_uranus_bigtts'
		  AND m.display_name = '天才童声 2.0'
		  AND m.deleted_at IS NULL`).Error; err != nil {
		logger.Warnf("autoMigrate: doubao-speech-v1 yangguang display_name fix failed: %v", err)
	}

	// 迁移成功后写入新版本号（UPSERT）
	return db.Exec("INSERT INTO ink_schema_version (id, ver) VALUES (1, ?) ON DUPLICATE KEY UPDATE ver = ?",
		schemaVersion, schemaVersion).Error
}

// runSchemaCleanup 幂等数据迁移与废弃列清理（AutoMigrate 不删列，需手动执行）
// 安全：所有操作均有 IF EXISTS 或 WHERE 守卫，可重复执行。
func runSchemaCleanup(db *gorm.DB) {
	// ── 1. 回填 Chapter.tenant_id（只处理 tenant_id=0 的行）
	if err := db.Exec(`UPDATE ink_chapter c
		JOIN ink_novel n ON c.novel_id = n.id
		SET c.tenant_id = n.tenant_id
		WHERE c.tenant_id = 0`).Error; err != nil {
		logger.Printf("[runSchemaCleanup] backfill chapter tenant_id: %v", err)
	}

	// ── 2. 回填 Character.tenant_id
	if err := db.Exec(`UPDATE ink_character c
		JOIN ink_novel n ON c.novel_id = n.id
		SET c.tenant_id = n.tenant_id
		WHERE c.tenant_id = 0`).Error; err != nil {
		logger.Printf("[runSchemaCleanup] backfill character tenant_id: %v", err)
	}

	// ── 3. 迁移视频配置：ink_novel → ink_novel_video_config（INSERT IGNORE 幂等）
	// 仅在 ink_novel.video_type 列尚未删除时执行（避免重复迁移报错）
	var videoTypeExists int
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_novel' AND COLUMN_NAME = 'video_type'`).Scan(&videoTypeExists)
	if videoTypeExists > 0 {
		if err := db.Exec(`INSERT IGNORE INTO ink_novel_video_config
			(novel_id, video_type, video_resolution, video_fps, video_aspect_ratio,
			 char_consistency_weight, asset_export_path, narration_voice,
			 subtitle_enabled, subtitle_position, subtitle_font_size, subtitle_color, subtitle_bg_style,
			 created_at, updated_at)
			SELECT id,
			       COALESCE(video_type, 'animation'),
			       COALESCE(video_resolution, '1080p'),
			       COALESCE(video_fps, 30),
			       COALESCE(video_aspect_ratio, '16:9'),
			       COALESCE(char_consistency_weight, 1.0),
			       COALESCE(asset_export_path, ''),
			       COALESCE(narration_voice, ''),
			       COALESCE(subtitle_enabled, 1),
			       COALESCE(subtitle_position, 'bottom'),
			       COALESCE(subtitle_font_size, 48),
			       COALESCE(subtitle_color, '#FFFFFF'),
			       COALESCE(subtitle_bg_style, 'shadow'),
			       NOW(), NOW()
			FROM ink_novel n
			WHERE NOT EXISTS (SELECT 1 FROM ink_novel_video_config vc WHERE vc.novel_id = n.id)`).Error; err != nil {
			logger.Printf("[runSchemaCleanup] migrate video config: %v", err)
		}
	}

	// ── 4. 删除废弃列（information_schema 守卫，防止重复 DROP 报错）
	// 使用硬编码 SQL 字面量，避免 fmt.Sprintf 拼接 DDL 语句（SQL 注入风险）。
	type dropSpec struct{ table, col string }
	drops := []dropSpec{
		// 废弃的视频/字幕列（已迁移到 ink_novel_video_config）
		{"ink_novel", "video_type"},
		{"ink_novel", "video_resolution"},
		{"ink_novel", "video_fps"},
		{"ink_novel", "video_aspect_ratio"},
		{"ink_novel", "char_consistency_weight"},
		{"ink_novel", "asset_export_path"},
		{"ink_novel", "narration_voice"},
		{"ink_novel", "subtitle_enabled"},
		{"ink_novel", "subtitle_position"},
		{"ink_novel", "subtitle_font_size"},
		{"ink_novel", "subtitle_color"},
		{"ink_novel", "subtitle_bg_style"},
		// 废弃的链表列
		{"ink_chapter", "previous_chapter_id"},
		{"ink_chapter", "next_chapter_id"},
		// 废弃的 plot_points 列
		{"ink_chapter", "plot_points"},
		// 废弃的 chapter 统计列
		{"ink_chapter", "quality_score"},
		// 废弃的 novel 列
		{"ink_novel", "reference_style"},
		// 废弃的 reference_novel 分析列
		{"ink_reference_novel", "style_analysis"},
		{"ink_reference_novel", "keywords"},
		{"ink_reference_novel", "similar_novels"},
		// 废弃的 user 列
		{"users", "total_projects"},
		{"users", "total_novels"},
		{"users", "total_words"},
		{"users", "settings"},
		{"users", "preferences"},
		{"users", "last_login_at"},
		// 废弃的 tenant 列
		{"tenants", "used_projects"},
		{"tenants", "used_storage_mb"},
		// 废弃的 ink_video 成本列（2026-06-06 删除，无任何代码引用）
		{"ink_video", "generation_cost"},
		// 废弃的 ink_asset 列（2026-06-06 删除，无任何代码引用）
		{"ink_asset", "waveform_url"},
		{"ink_asset", "hls_url"},
		{"ink_asset", "license_url"},
		{"ink_asset", "text_vector"},
		{"ink_asset", "image_vector"},
		{"ink_asset", "ocr_text"},
		// 废弃的 ink_ai_model 默认参数列（2026-06-06 删除，无任何代码引用）
		{"ink_ai_model", "default_top_p"},
		{"ink_ai_model", "default_top_k"},
		// 废弃的 ink_task_model_config 质量阈值列（2026-06-06 删除，无任何代码引用）
		{"ink_task_model_config", "min_quality_score"},
		// 废弃的 ink_video 统计列（2026-06-06-v3 删除，无任何代码引用）
		{"ink_video", "total_frames"},
		{"ink_video", "total_words"},
		// 废弃的 ink_arc_summary 低谷张力列（2026-06-06-v3 删除）
		{"ink_arc_summary", "low_tension"},
		// 废弃的 ink_quality_report 统计列（2026-06-06-v3 删除，无任何代码引用）
		{"ink_quality_report", "total_issues"},
		{"ink_quality_report", "high_priority"},
		{"ink_quality_report", "medium_priority"},
		{"ink_quality_report", "low_priority"},
		// 废弃的 ink_chapter_version 评分与作者列（2026-06-06-v3 删除）
		{"ink_chapter_version", "quality_score"},
		{"ink_chapter_version", "consistency_score"},
		{"ink_chapter_version", "change_author_id"},
		// 废弃的 ink_worldview 封面列（2026-06-06-v3 删除）
		{"ink_worldview", "cover_image"},
	}
	// Allowlist of valid (table, col) pairs — any entry NOT in this list is skipped.
	allowedDrops := map[string]bool{}
	for _, d := range drops {
		allowedDrops[d.table+"."+d.col] = true
	}
	for _, d := range drops {
		if !allowedDrops[d.table+"."+d.col] {
			continue // defensive: should never happen since drops == allowlist
		}
		var cnt int64
		db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, d.table, d.col).Scan(&cnt)
		if cnt == 0 {
			continue
		}
		// Execute with a pre-validated literal pair from the drops table.
		// The table+col values come exclusively from the hardcoded drops slice above,
		// so no external input can reach this code path.
		sql := "ALTER TABLE `" + d.table + "` DROP COLUMN `" + d.col + "`"
		if err := db.Exec(sql).Error; err != nil {
			logger.Printf("[runSchemaCleanup] drop %s.%s: %v", d.table, d.col, err)
		}
	}
}

// preMigrateCleanup 清理会阻塞 AutoMigrate 唯一索引迁移的无效行
func preMigrateCleanup(db *gorm.DB) {
	// ink_task_model_config.task_type 为 UNIQUE NOT NULL，旧空行会导致 Duplicate entry '' 错误
	// 若 task_type 列尚不存在，DELETE WHERE task_type='' 会报错，此时直接清空整张表
	if err := db.Exec("DELETE FROM ink_task_model_config WHERE task_type = '' OR task_type IS NULL").Error; err != nil {
		db.Exec("DELETE FROM ink_task_model_config")
	}
	// ink_worldview.novel_id 是历史遗留列（旧版 auto-migrate 写入，当前 model 无此字段）
	// 用 information_schema 判断列是否存在，兼容所有 MySQL 版本
	var colCount int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_worldview' AND COLUMN_NAME = 'novel_id'`).Scan(&colCount)
	if colCount > 0 {
		// 先删除引用该列的所有外键约束
		var fkNames []string
		db.Raw(`SELECT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_worldview'
			AND COLUMN_NAME = 'novel_id' AND REFERENCED_TABLE_NAME IS NOT NULL`).Scan(&fkNames)
		for _, fk := range fkNames {
			db.Exec("ALTER TABLE `ink_worldview` DROP FOREIGN KEY `" + fk + "`")
		}
		db.Exec("ALTER TABLE ink_worldview DROP COLUMN novel_id")
	}
	// ink_model_usage_log.model_id 曾有 FK 约束指向 ink_ai_model(id)，
	// 已改为软引用（无 FK），需删除旧约束避免 1452 错误
	var usageLogFKs []string
	db.Raw(`SELECT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_model_usage_log'
		AND COLUMN_NAME = 'model_id' AND REFERENCED_TABLE_NAME IS NOT NULL`).Scan(&usageLogFKs)
	for _, fk := range usageLogFKs {
		db.Exec("ALTER TABLE ink_model_usage_log DROP FOREIGN KEY " + fk)
	}
	// ink_video.chapter_id FK 约束导致创建无章节视频时 1452 错误（chapter_id=0 非法值）
	// chapter_id 为可选字段，改为软引用（保留 index，删除 FK 约束）
	var videoChapterFKs []string
	db.Raw(`SELECT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_video'
		AND COLUMN_NAME = 'chapter_id' AND REFERENCED_TABLE_NAME IS NOT NULL`).Scan(&videoChapterFKs)
	for _, fk := range videoChapterFKs {
		db.Exec("ALTER TABLE ink_video DROP FOREIGN KEY `" + fk + "`")
	}
	// ink_asset.external_id 曾有 uniqueIndex(where:external_id != '')，该语法在 MySQL 不支持。
	// MySQL 会忽略 WHERE 子句，创建无条件唯一索引，导致本地上传（external_id=''）第二次就报 Duplicate key。
	// 现改为普通 index，唯一性由应用层 ExistsByExternalID 保证。
	var extIDIdxCount int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_asset'
		AND INDEX_NAME = 'uq_external_id'`).Scan(&extIDIdxCount)
	if extIDIdxCount > 0 {
		db.Exec("ALTER TABLE ink_asset DROP INDEX uq_external_id")
	}
	// ink_task_model_config.task_type 从 UNIQUE 改为普通 index（Fix 10）：
	// 允许同类型多条配置共存（如不同 tenant 的配置）。先删旧唯一索引，AutoMigrate 再建普通索引。
	var taskTypeIdxRows []struct{ NonUnique int }
	db.Raw(`SELECT NON_UNIQUE FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_task_model_config'
		AND COLUMN_NAME = 'task_type' AND INDEX_NAME != 'PRIMARY'`).Scan(&taskTypeIdxRows)
	for _, row := range taskTypeIdxRows {
		if row.NonUnique == 0 {
			// unique index exists — drop it so AutoMigrate can create the non-unique one
			db.Exec("ALTER TABLE ink_task_model_config DROP INDEX idx_task_model_configs_task_type")
			db.Exec("ALTER TABLE ink_task_model_config DROP INDEX task_type")
		}
	}
}
