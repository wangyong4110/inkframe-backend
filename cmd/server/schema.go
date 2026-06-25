package main

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// schemaVersion must be bumped whenever any model struct is added or changed.
// Format: YYYY-MM-DD-vN. This allows autoMigrate to be skipped on unchanged restarts.
const schemaVersion = "2026-06-25-v16"

// ensureCriticalColumns 在版本检查之前无条件补全关键列（应对版本跳过导致列缺失的情况）。
// 直接执行 ALTER TABLE ADD COLUMN，MySQL 1060 = 列已存在时静默忽略。
func ensureCriticalColumns(db *gorm.DB) {
	type colAdd struct{ table, col, def string }
	additions := []colAdd{
		// ink_novel 广场社交字段（2026-05-10 新增；view/like/comment_count 已迁移到 ink_content_stats）
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
		// ink_novel 章节模式（2026-06-07 新增）
		{"ink_novel", "chapter_mode", "VARCHAR(20) NOT NULL DEFAULT 'sequential'"},
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
		// ink_worldview_entity 租户隔离（Fix 5: WorldviewEntity 缺失 tenant_id 补全）
		{"ink_worldview_entity", "tenant_id", "BIGINT UNSIGNED NOT NULL DEFAULT 1"},
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
		// ink_storyboard_shot 音效/音频字段（sfx_service/capcut_service 使用；audio_path/sfx_tags 保留；sfx_url/sfx_volume 已废弃由 runSchemaCleanup 删除）
		{"ink_storyboard_shot", "audio_path", "VARCHAR(2000) NULL"},
		{"ink_storyboard_shot", "sfx_tags",   "VARCHAR(2000) NULL"},
		// ink_novel 自动审查配置（2026-06-01 新增）
		{"ink_novel", "auto_review_rounds",    "INT NOT NULL DEFAULT 0"},
		{"ink_novel", "auto_review_min_score", "DOUBLE NOT NULL DEFAULT 80"},
		// 专业小说质量改进（2026-06-02 新增）
		{"ink_novel",      "core_theme",           "TEXT NULL"},                              // 全书核心主题
		{"ink_chapter",    "reader_expectations",  "TEXT NULL"},                              // 读者期待状态
		{"ink_foreshadow", "planted_chapter_no",   "INT NOT NULL DEFAULT 0"},                 // 种下章节序号
		{"ink_foreshadow", "payoff_chapter_no",    "INT NOT NULL DEFAULT 0"},                 // 预期回收章节序号
		{"ink_foreshadow", "importance",           "VARCHAR(20) NOT NULL DEFAULT 'normal'"}, // 重要程度
		// 章节连贯性修复（2026-06-02-v2 新增）
		{"ink_chapter", "chapter_end_state", "TEXT NULL"}, // 章末精确状态快照（供下一章连续性锚点）
		// 场景一致性日志新增字段（2026-06-06-v1 新增）
		{"ink_scene_consistency_log", "time_score",     "DECIMAL(4,3) NOT NULL DEFAULT 1"},
		{"ink_scene_consistency_log", "suggested_fix",  "TEXT NULL"},
		// ink_character 性别与年龄（2026-06-07 新增）
		{"ink_character", "gender", "VARCHAR(20) NULL"},
		{"ink_character", "age",    "VARCHAR(50) NULL"},
		// ink_character 默认形象 ID（2026-06-09 新增，替代 ink_character_look.is_default）
		{"ink_character", "default_look_id", "BIGINT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_novel 创建者（2026-06-13 新增，协作权限快速判断）
		{"ink_novel", "created_by", "BIGINT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_foreshadow 专业伏笔管理字段（2026-06-17 新增）
		{"ink_foreshadow", "actual_payoff_chapter_no", "INT NOT NULL DEFAULT 0"},   // 实际兑现章节序号
		{"ink_foreshadow", "level",                    "VARCHAR(20) NOT NULL DEFAULT 'sub'"},  // 主线/支线/细节
		{"ink_foreshadow", "foreshadow_type",          "VARCHAR(30) NOT NULL DEFAULT ''"},     // 类型
		{"ink_foreshadow", "linked_hook_id",           "INT UNSIGNED NULL"},                   // 关联钩子
		{"ink_foreshadow", "linked_arc_id",            "INT UNSIGNED NULL"},                   // 关联冲突弧
		// ink_rewrite_project 租户直接隔离（2026-06-20-v5 新增，改写项目列表查询优化）
		{"ink_rewrite_project", "tenant_id", "INT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_hook_chain 关联伏笔（2026-06-17 新增）
		{"ink_hook_chain", "foreshadow_id", "INT UNSIGNED NULL"},
		// ink_foreshadow 专业叙事分析字段（2026-06-17-v2 新增）
		{"ink_foreshadow", "confidence",             "VARCHAR(20) NOT NULL DEFAULT 'medium'"},
		{"ink_foreshadow", "parent_id",              "INT UNSIGNED NULL"},
		{"ink_foreshadow", "character_ids",          "TEXT NULL"},
		{"ink_foreshadow", "reinforcement_chapters", "TEXT NULL"},
		{"ink_foreshadow", "payoff_quality",         "INT NOT NULL DEFAULT 0"},
		{"ink_foreshadow", "payoff_notes",           "TEXT NULL"},
		// ink_hook_chain 兑现质量（2026-06-17-v2 新增）
		{"ink_hook_chain", "payoff_quality", "INT NOT NULL DEFAULT 0"},
		{"ink_hook_chain", "payoff_notes",   "TEXT NULL"},
		// ink_conflict_arc 张力曲线（2026-06-17-v2 新增）
		{"ink_conflict_arc", "tension_levels", "TEXT NULL"},
		// ink_shot_sfx_item 循环播放次数（2026-06-21 新增）
		{"ink_shot_sfx_item", "play_count", "INT NOT NULL DEFAULT 1"},
		// ink_storyboard_shot 角色绑定（2026-06-22 新增，migration SQL 中缺失此列）
		{"ink_storyboard_shot", "character_ids", "JSON NULL"},
		// ink_shot_voice_segment 方言支持（2026-06-24 新增）
		{"ink_shot_voice_segment", "language", "VARCHAR(20) NOT NULL DEFAULT ''"},
		// 角色/道具/快照结构优化（2026-06-25-v12）
		{"ink_character_state_snapshot", "novel_id",    "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_character_state_snapshot", "deleted_at",  "DATETIME(3) NULL"},
		{"ink_chapter_character",        "deleted_at",  "DATETIME(3) NULL"},
		{"ink_chapter_item",             "deleted_at",  "DATETIME(3) NULL"},
		// 情节结构与场景优化（2026-06-25-v13）
		{"ink_plot_point",              "deleted_at",  "DATETIME(3) NULL"},
		{"ink_scene_consistency_log",   "novel_id",    "INT UNSIGNED NOT NULL DEFAULT 0"},
		// 大纲审查与质量控制优化（2026-06-25-v14）
		{"ink_continuity_report",       "updated_at",  "DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)"},
		{"ink_review_record",           "novel_id",    "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_ignored_review_issue",    "novel_id",    "INT UNSIGNED NOT NULL DEFAULT 0"},
		// ink_ai_model 调用限制字段（2026-06-24-v2 新增）
		{"ink_ai_model", "timeout", "INT NOT NULL DEFAULT 0"},
		{"ink_ai_model", "concurrency", "INT NOT NULL DEFAULT 0"},
		{"ink_ai_model", "rate_limit", "INT NOT NULL DEFAULT 0"},
		// ink_model_provider 音色列表字段（2026-06-24-v7 新增）
		{"ink_model_provider", "voices_json", "MEDIUMTEXT NOT NULL DEFAULT ''"},
		// ink_model_provider 默认模型名称（2026-06-24-v7 新增，替代 APIVersion 的模型名用途）
		{"ink_model_provider", "default_model", "VARCHAR(200) NOT NULL DEFAULT ''"},
		// ink_content_stats 社交统计独立表（2026-06-25-v3 新增）
		{"ink_content_stats", "view_count",    "INT NOT NULL DEFAULT 0"},
		{"ink_content_stats", "like_count",    "INT NOT NULL DEFAULT 0"},
		{"ink_content_stats", "comment_count", "INT NOT NULL DEFAULT 0"},
		{"ink_content_stats", "updated_at",    "DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)"},
		// ink_foreshadow 实际兑现章节ID（2026-06-25-v5 新增，与ActualPayoffChapterNo对称）
		{"ink_foreshadow", "actual_payoff_chapter_id", "INT UNSIGNED NULL"},
		// ink_chapter_character 出场信息（从 ink_character_appearance 迁入，2026-06-25-v5）
		{"ink_chapter_character", "role_in_chapter", "VARCHAR(50) NOT NULL DEFAULT ''"},
		{"ink_chapter_character", "action",          "TEXT NULL"},
		{"ink_chapter_character", "change",          "TEXT NULL"},
		// ink_notification 已读时间戳（2026-06-25-v6 新增，记录确切的已读时间）
		{"ink_notification", "read_at", "DATETIME(3) NULL"},
		// tenant_users 软删除（2026-06-25-v7 新增，支持成员历史审计）
		{"tenant_users", "deleted_at", "DATETIME(3) NULL"},
		// ink_audit_log 审计日志新字段（2026-06-25-v8 新增）
		{"ink_audit_log", "novel_id",      "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_audit_log", "resource_type", "VARCHAR(50) NOT NULL DEFAULT ''"},
		{"ink_audit_log", "resource_id",   "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_audit_log", "resource_name", "VARCHAR(200) NOT NULL DEFAULT ''"},
		{"ink_audit_log", "details",       "TEXT NULL"},
		{"ink_audit_log", "ip",            "VARCHAR(64) NOT NULL DEFAULT ''"},
		{"ink_audit_log", "status",        "VARCHAR(20) NOT NULL DEFAULT 'ok'"},
		// 结构优化（2026-06-25-v9）
		{"ink_chapter",          "tenant_id",     "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_chapter_version",  "novel_id",      "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_arc_summary",      "tenant_id",     "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_arc_summary",      "deleted_at",    "DATETIME(3) NULL"},
		{"ink_novel_crawl_job",  "tenant_id",     "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_novel_crawl_job",  "platform",      "VARCHAR(30) NOT NULL DEFAULT ''"},
		{"ink_novel_crawl_job",  "source_url",    "VARCHAR(500) NOT NULL DEFAULT ''"},
		{"ink_novel_crawl_job",  "error_message", "TEXT NULL"},
		// 世界观/知识库/参考小说/章节结构优化（2026-06-25-v10）
		{"ink_knowledge_base",    "tenant_id",       "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_knowledge_base",    "embedded_at",     "DATETIME(3) NULL"},
		{"ink_reference_novel",   "tenant_id",       "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_reference_novel",   "error_message",   "TEXT NULL"},
		{"ink_reference_novel",   "deleted_at",      "DATETIME(3) NULL"},
		{"ink_reference_chapter", "tenant_id",       "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_reference_chapter", "deleted_at",      "DATETIME(3) NULL"},
		{"ink_reference_chapter", "updated_at",      "DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)"},
		// AI 模型管理优化（2026-06-25-v16）
		{"ink_model_comparison_experiment", "tenant_id",  "INT UNSIGNED NOT NULL DEFAULT 0"},
		{"ink_model_comparison_experiment", "deleted_at", "DATETIME(3) NULL"},
	}
	for _, a := range additions {
		// 表不存在时跳过（AutoMigrate 会建表，建表时列也会随 struct 定义一同创建）
		var tblCnt int64
		db.Raw(
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			a.table,
		).Scan(&tblCnt)
		if tblCnt == 0 {
			continue
		}
		// 列已存在则跳过，避免触发 GORM 的 Error 1060 日志
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
			logger.Errorf("ensureCriticalColumns: %s.%s: %v", a.table, a.col, err)
		} else {
			logger.Infof("ensureCriticalColumns: added column %s.%s", a.table, a.col)
		}
	}
}

// dropLegacyCharacterColumns 删除 2026-05-15 角色/道具/场景锚点字段简化后遗留的旧列。
// 使用 information_schema 检查列存在性，幂等安全，可重复调用。
func dropLegacyCharacterColumns(db *gorm.DB) {
	legacy := []struct{ table, col string }{
		// ink_character: Archetype/Appearance/Personality/Background/Relations/
		// CharacterArc/DialogueStyle/VisualDesign/CoverImage → 合并为 Description
		// gender/age 保留（结构体仍在使用）
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
		// 2026-06-20: 删除冗余 tenant_id（通过 novel_id → novel.tenant_id 链校验）
		// 注：ink_chapter.tenant_id 已在 v9 重新启用（struct 有字段），不再删除
		// 注：ink_knowledge_base.tenant_id 已在 v10 重新启用（NovelID 可为 NULL，无其他隔离手段）
		{"ink_plot_point",             "tenant_id"},
		{"ink_scene_anchor",           "tenant_id"},
		{"ink_skill",                  "tenant_id"},
		{"ink_character",              "tenant_id"},
		{"ink_quality_report",         "tenant_id"},
		{"ink_novel_outline_version",  "tenant_id"},
		{"ink_continuity_report",      "tenant_id"},
		{"ink_hook_chain",             "tenant_id"},
		{"ink_satisfaction_point",     "tenant_id"},
		{"ink_conflict_arc",           "tenant_id"},
		{"ink_outline_review",         "tenant_id"},
		{"ink_outline_synthesis",      "tenant_id"},
		// 2026-06-20-v3: 删除剩余冗余 tenant_id（通过 novel_id → novel.tenant_id 链校验）
		{"ink_video",                  "tenant_id"},
		{"ink_review_record",          "tenant_id"},
		{"ink_ignored_review_issue",   "tenant_id"},
		{"ink_foreshadow",             "tenant_id"},
		// ink_rewrite_project.tenant_id 已在 2026-06-20-v5 重新启用，从此列表移除
		// 2026-06-20-v4: 删除评论表冗余 nickname（从 users.nickname 实时读取）
		{"ink_video_comment",          "nickname"},
		{"ink_novel_comment",          "nickname"},
		{"ink_chapter_comment",        "nickname"},
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
			logger.Errorf("[dropLegacyCharacterColumns] drop %s.%s: %v", d.table, d.col, err)
		}
	}
}

// autoMigrate 自动迁移（带版本跳过优化 + MySQL Advisory Lock 防并发 DDL）
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

	// 多实例并发保护：GET_LOCK 是 MySQL 会话级 Advisory Lock，保证同一时刻只有一个
	// 实例执行 DDL。其他实例等待最多 60 秒后拿到锁，发现版本已更新则跳过迁移。
	var lockVal *int
	if err := db.Raw("SELECT GET_LOCK('inkframe:schema_migration', 60)").Scan(&lockVal).Error; err != nil || lockVal == nil || *lockVal != 1 {
		// 等待超时或出错：重新检查版本（另一实例可能已完成迁移）
		var storedVer string
		db.Raw("SELECT ver FROM ink_schema_version WHERE id = 1").Scan(&storedVer)
		if storedVer == schemaVersion {
			logger.Printf("autoMigrate: schema %s migrated by peer instance, skipping", schemaVersion)
			return nil
		}
		return fmt.Errorf("autoMigrate: could not acquire migration lock (GET_LOCK returned %v)", lockVal)
	}
	defer db.Exec("DO RELEASE_LOCK('inkframe:schema_migration')")

	// 无条件补全关键列（防止版本跳过导致列缺失）
	ensureCriticalColumns(db)
	// 升级列类型（text→mediumtext 等，幂等安全）
	ensureColumnTypes(db)
	// 删除遗留列（2026-05-15 模型简化后遗留在 DB 中的旧字段）
	dropLegacyCharacterColumns(db)

	// 读取当前已迁移版本（在锁内读取，保证读到最新值）
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
		&model.ChapterSceneAnchor{},
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
		// 统一社交表（2026-06-25-v5：替代 ink_novel/chapter/video_like/comment 6张表）
		&model.EntityLike{},
		&model.EntityComment{},
		// 阅读进度
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
		// 多人协作
		&model.NovelMember{},
		&model.EditingLock{},
		// 敏感词规则
		&model.SensitiveWordRule{},
		// 内容统计独立表（2026-06-25-v3）
		&model.ContentStats{},
	); err != nil {
		return err
	}

	// 数据迁移（2026-06-25-v5）：将旧社交表数据迁移到统一表 ink_entity_like / ink_entity_comment
	socialMigrations := []string{
		`INSERT IGNORE INTO ink_entity_like (entity_type, entity_id, novel_id, user_id, created_at) SELECT 'novel', novel_id, novel_id, user_id, created_at FROM ink_novel_like`,
		`INSERT IGNORE INTO ink_entity_like (entity_type, entity_id, novel_id, user_id, created_at) SELECT 'chapter', chapter_id, novel_id, user_id, created_at FROM ink_chapter_like`,
		`INSERT IGNORE INTO ink_entity_like (entity_type, entity_id, novel_id, user_id, created_at) SELECT 'video', video_id, 0, user_id, created_at FROM ink_video_like`,
		`INSERT IGNORE INTO ink_entity_comment (entity_type, entity_id, novel_id, user_id, content, parent_id, created_at, updated_at) SELECT 'novel', novel_id, novel_id, user_id, content, parent_id, created_at, updated_at FROM ink_novel_comment`,
		`INSERT IGNORE INTO ink_entity_comment (entity_type, entity_id, novel_id, user_id, content, parent_id, created_at, updated_at) SELECT 'chapter', chapter_id, novel_id, user_id, content, parent_id, created_at, updated_at FROM ink_chapter_comment`,
		`INSERT IGNORE INTO ink_entity_comment (entity_type, entity_id, novel_id, user_id, content, parent_id, created_at, updated_at) SELECT 'video', video_id, 0, user_id, content, parent_id, created_at, updated_at FROM ink_video_comment`,
	}
	for _, sql := range socialMigrations {
		if err := db.Exec(sql).Error; err != nil {
			logger.Warnf("autoMigrate: social data migration (non-fatal, old table may not exist): %v", err)
		}
	}

	// 数据迁移：将历史 status='published' 的章节修正为 is_published=true, status='completed'
	if err := db.Exec(`UPDATE ink_chapter SET is_published = 1, status = 'completed' WHERE status = 'published'`).Error; err != nil {
		logger.Errorf("autoMigrate: chapter status migration failed: %v", err)
	}

	// 数据迁移（2026-06-17）：世界观字段合并 — technology → geography，religion → culture
	// 仅在旧列存在时执行（幂等安全，GORM AutoMigrate 不删旧列）
	var wvTechExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_worldview' AND COLUMN_NAME = 'technology'`).Scan(&wvTechExists)
	if wvTechExists > 0 {
		if err := db.Exec(`UPDATE ink_worldview SET geography = CONCAT(geography, '\n\n[文明水平] ', technology) WHERE technology IS NOT NULL AND technology != '' AND technology != '无'`).Error; err != nil {
			logger.Errorf("autoMigrate: worldview technology→geography merge failed: %v", err)
		}
	}
	var wvReligionExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_worldview' AND COLUMN_NAME = 'religion'`).Scan(&wvReligionExists)
	if wvReligionExists > 0 {
		if err := db.Exec(`UPDATE ink_worldview SET culture = CONCAT(culture, '\n\n[宗教信仰] ', religion) WHERE religion IS NOT NULL AND religion != '' AND religion != '无'`).Error; err != nil {
			logger.Errorf("autoMigrate: worldview religion→culture merge failed: %v", err)
		}
	}

	// 数据迁移（H-1）：将 style_prompt 中误存的大纲 JSON 迁移到 outline 字段
	if err := db.Exec(`UPDATE ink_novel SET outline = style_prompt WHERE style_prompt LIKE '{"chapters":%' AND (outline = '' OR outline IS NULL)`).Error; err != nil {
		logger.Errorf("autoMigrate: outline backfill failed: %v", err)
	}
	if err := db.Exec(`UPDATE ink_novel SET style_prompt = '' WHERE style_prompt LIKE '{"chapters":%'`).Error; err != nil {
		logger.Errorf("autoMigrate: style_prompt cleanup failed: %v", err)
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
			logger.Errorf("autoMigrate: character description migration failed: %v", err)
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
			logger.Errorf("autoMigrate: item description migration failed: %v", err)
		}
	}

	// 数据迁移（2026-06-06-v5）：修正 doubao-speech 的 seed-tts-2.0/1.0 任务类型
	// 这两个是资源端点 ID，不应出现在角色音色选择列表中
	if err := db.Exec(`UPDATE ink_ai_model m
		JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
		SET m.type = 'voice', m.is_active = 0
		WHERE p.name = 'doubao-speech'
		  AND m.name IN ('seed-tts-2.0', 'seed-tts-1.0')
		  AND m.deleted_at IS NULL
		  AND m.type IN ('voice', '')`).Error; err != nil {
		logger.Errorf("autoMigrate: doubao-speech task fix failed: %v", err)
	}

	// 数据迁移（2026-06-06-v6）：doubao-speech V3 中错误添加的 _moon_bigtts 音色
	// 月亮系列属于 seed-tts-1.0 资源，V3 provider 的 2.0 发音人应为 _uranus_bigtts
	if err := db.Exec(`UPDATE ink_ai_model m
		JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
		SET m.type = 'voice', m.is_active = 0
		WHERE p.name = 'doubao-speech'
		  AND m.name LIKE '%_moon_bigtts'
		  AND m.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("autoMigrate: doubao-speech moon_bigtts disable failed: %v", err)
	}

	// 数据迁移（2026-06-23-v2）：suitable_tasks → type（幂等，仅更新 type 为空的行）
	if err := db.Exec(`UPDATE ink_ai_model SET type = CASE
		WHEN suitable_tasks LIKE '%voice_gen%'   THEN 'voice'
		WHEN suitable_tasks LIKE '%image_gen%'   THEN 'image'
		WHEN suitable_tasks LIKE '%img2img_gen%' THEN 'img2img'
		WHEN suitable_tasks LIKE '%video_gen%'   THEN 'video'
		WHEN suitable_tasks LIKE '%music_gen%'   THEN 'music'
		WHEN suitable_tasks LIKE '%sfx_gen%'     THEN 'sfx'
		WHEN suitable_tasks LIKE '%embedding%'   THEN 'embedding'
		ELSE 'llm'
	END WHERE (type IS NULL OR type = '') AND suitable_tasks IS NOT NULL AND suitable_tasks != ''`).Error; err != nil {
		logger.Errorf("autoMigrate: suitable_tasks→type migration failed: %v", err)
	}

	// 删除废弃列 suitable_tasks（MySQL；幂等：列不存在时忽略错误）
	db.Exec(`ALTER TABLE ink_ai_model DROP COLUMN IF EXISTS suitable_tasks`)

	// 数据迁移（2026-06-06-v6）：修正 doubao-speech-v1 中 阳光青年 2.0 音色显示名称错误
	// zh_male_yangguangqingnian_uranus_bigtts 之前被错误命名为"天才童声 2.0"
	if err := db.Exec(`UPDATE ink_ai_model m
		JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
		SET m.display_name = '阳光青年 2.0'
		WHERE p.name = 'doubao-speech-v1'
		  AND m.name = 'zh_male_yangguangqingnian_uranus_bigtts'
		  AND m.display_name = '天才童声 2.0'
		  AND m.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("autoMigrate: doubao-speech-v1 yangguang display_name fix failed: %v", err)
	}

	// 数据迁移（2026-06-20-v5）：回填 ink_rewrite_project.tenant_id（从关联 ink_novel 获取）
	// 新增列 tenant_id 时旧行默认为 0，需要从关联小说的 tenant_id 补全，确保列表查询与详情查询一致。
	if err := db.Exec(`UPDATE ink_rewrite_project rp
		INNER JOIN ink_novel n ON rp.novel_id = n.id
		SET rp.tenant_id = n.tenant_id
		WHERE rp.tenant_id = 0 AND n.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("autoMigrate: rewrite_project tenant_id backfill failed: %v", err)
	}

	// 数据迁移（2026-06-25-v2）：将 StoryboardShot 旧音频字段迁移到新表
	// SFX 字段 (sfx_url, sfx_tags, sfx_volume) → ink_shot_sfx_item
	var sfxURLColExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_storyboard_shot' AND COLUMN_NAME = 'sfx_url'`).Scan(&sfxURLColExists)
	if sfxURLColExists > 0 {
		if err := db.Exec(`INSERT INTO ink_shot_sfx_item (shot_id, seq_no, tag, url, volume, source, disabled, start_offset, duration_secs, sfx_type, loop_enabled, fade_in_ms, fade_out_ms, play_count, created_at, updated_at)
			SELECT s.id, 1, COALESCE(NULLIF(s.sfx_tags,''), s.sfx_url), s.sfx_url,
			       IF(s.sfx_volume > 0, s.sfx_volume, 0.4), 'legacy', 0, 0, 0, 'action', 0, 0, 200, 1, NOW(), NOW()
			FROM ink_storyboard_shot s
			WHERE s.sfx_url != '' AND s.deleted_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM ink_shot_sfx_item i WHERE i.shot_id = s.id AND i.deleted_at IS NULL)`).Error; err != nil {
			logger.Errorf("autoMigrate: sfx migration failed: %v", err)
		}
	}
	// 音频字段 (audio_path) → ink_shot_voice_segment
	var audioPathColExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_storyboard_shot' AND COLUMN_NAME = 'audio_path'`).Scan(&audioPathColExists)
	if audioPathColExists > 0 {
		if err := db.Exec(`INSERT INTO ink_shot_voice_segment (shot_id, seq_no, text, speaker, emotion, language, voice_id, audio_path, duration_secs, created_at, updated_at)
			SELECT s.id, 1, COALESCE(s.narration, ''), '', '', '', '', s.audio_path, 0, NOW(), NOW()
			FROM ink_storyboard_shot s
			WHERE s.audio_path != '' AND s.deleted_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM ink_shot_voice_segment v WHERE v.shot_id = s.id AND v.audio_path != '' AND v.deleted_at IS NULL)`).Error; err != nil {
			logger.Errorf("autoMigrate: audio_path migration failed: %v", err)
		}
	}

	// 数据迁移（2026-06-25-v3）：将 ink_novel/ink_video 的统计计数迁移到 ink_content_stats
	// 仅在旧列尚存在时执行（迁移完成并执行 runSchemaCleanup 后，旧列被删除，此段自动跳过）
	var novelViewCountExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_novel' AND COLUMN_NAME = 'view_count'`).Scan(&novelViewCountExists)
	if novelViewCountExists > 0 {
		if err := db.Exec(`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
			SELECT 'novel', id, view_count, like_count, comment_count, NOW()
			FROM ink_novel WHERE deleted_at IS NULL
			ON DUPLICATE KEY UPDATE view_count=VALUES(view_count), like_count=VALUES(like_count), comment_count=VALUES(comment_count)`).Error; err != nil {
			logger.Errorf("autoMigrate: novel stats migration failed: %v", err)
		}
	}
	var videoViewCountExists int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_video' AND COLUMN_NAME = 'view_count'`).Scan(&videoViewCountExists)
	if videoViewCountExists > 0 {
		if err := db.Exec(`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
			SELECT 'video', id, view_count, like_count, comment_count, NOW()
			FROM ink_video WHERE deleted_at IS NULL
			ON DUPLICATE KEY UPDATE view_count=VALUES(view_count), like_count=VALUES(like_count), comment_count=VALUES(comment_count)`).Error; err != nil {
			logger.Errorf("autoMigrate: video stats migration failed: %v", err)
		}
	}

	// 数据回填（2026-06-25-v9）：为新增冗余列补填历史数据，幂等（仅更新 DEFAULT 0/NULL 的行）
	backfills := []string{
		// ink_chapter.tenant_id 从 ink_novel 反查
		`UPDATE ink_chapter c JOIN ink_novel n ON n.id = c.novel_id SET c.tenant_id = n.tenant_id WHERE c.tenant_id = 0 AND n.tenant_id > 0`,
		// ink_chapter_version.novel_id 从 ink_chapter 反查
		`UPDATE ink_chapter_version cv JOIN ink_chapter c ON c.id = cv.chapter_id SET cv.novel_id = c.novel_id WHERE cv.novel_id = 0`,
		// ink_arc_summary.tenant_id 从 ink_novel 反查
		`UPDATE ink_arc_summary a JOIN ink_novel n ON n.id = a.novel_id SET a.tenant_id = n.tenant_id WHERE a.tenant_id = 0 AND n.tenant_id > 0`,
		// ink_novel_crawl_job.tenant_id 从 ink_novel 反查
		`UPDATE ink_novel_crawl_job j JOIN ink_novel n ON n.id = j.novel_id SET j.tenant_id = n.tenant_id WHERE j.tenant_id = 0 AND n.tenant_id > 0`,
		// 数据回填（2026-06-25-v10）
		// ink_worldview_entity.tenant_id 从 ink_worldview 反查
		`UPDATE ink_worldview_entity we JOIN ink_worldview w ON w.id = we.worldview_id SET we.tenant_id = w.tenant_id WHERE we.tenant_id = 0 AND w.tenant_id > 0`,
		// ink_knowledge_base.tenant_id 从 ink_novel 反查（writing_technique 类型 novel_id=NULL，保持 0）
		`UPDATE ink_knowledge_base kb JOIN ink_novel n ON n.id = kb.novel_id SET kb.tenant_id = n.tenant_id WHERE kb.tenant_id = 0 AND n.tenant_id > 0`,
		// ink_reference_chapter.tenant_id 从 ink_reference_novel 反查
		`UPDATE ink_reference_chapter rc JOIN ink_reference_novel rn ON rn.id = rc.novel_id SET rc.tenant_id = rn.tenant_id WHERE rc.tenant_id = 0 AND rn.tenant_id > 0`,
		// 数据回填（2026-06-25-v12）
		// ink_character_state_snapshot.novel_id 从 ink_character 反查
		`UPDATE ink_character_state_snapshot s JOIN ink_character c ON c.id = s.character_id SET s.novel_id = c.novel_id WHERE s.novel_id = 0 AND c.novel_id > 0`,
		// 数据回填（2026-06-25-v13）
		// ink_scene_consistency_log.novel_id 从 ink_storyboard_shot→ink_video 反查
		`UPDATE ink_scene_consistency_log l JOIN ink_storyboard_shot s ON s.id = l.shot_id JOIN ink_video v ON v.id = s.video_id SET l.novel_id = v.novel_id WHERE l.novel_id = 0 AND v.novel_id > 0`,
		// 数据回填（2026-06-25-v14）
		// ink_review_record.novel_id：章节审查从 ink_chapter 反查，分镜审查从 ink_video 反查
		`UPDATE ink_review_record r JOIN ink_chapter c ON c.id = r.entity_id SET r.novel_id = c.novel_id WHERE r.entity_type = 'chapter' AND r.novel_id = 0 AND c.novel_id > 0`,
		`UPDATE ink_review_record r JOIN ink_video v ON v.id = r.entity_id SET r.novel_id = v.novel_id WHERE r.entity_type = 'storyboard' AND r.novel_id = 0 AND v.novel_id > 0`,
		// ink_ignored_review_issue.novel_id：同上
		`UPDATE ink_ignored_review_issue i JOIN ink_chapter c ON c.id = i.entity_id SET i.novel_id = c.novel_id WHERE i.entity_type = 'chapter' AND i.novel_id = 0 AND c.novel_id > 0`,
		`UPDATE ink_ignored_review_issue i JOIN ink_video v ON v.id = i.entity_id SET i.novel_id = v.novel_id WHERE i.entity_type = 'storyboard' AND i.novel_id = 0 AND v.novel_id > 0`,
	}
	for _, sql := range backfills {
		if err := db.Exec(sql).Error; err != nil {
			logger.Errorf("autoMigrate backfill: %v (sql: %s)", err, sql[:60])
		}
	}

	// 补全缺失索引（幂等，已存在则跳过）
	ensureCriticalIndexes(db)

	// 迁移成功后写入新版本号（UPSERT）
	return db.Exec("INSERT INTO ink_schema_version (id, ver) VALUES (1, ?) ON DUPLICATE KEY UPDATE ver = ?",
		schemaVersion, schemaVersion).Error
}

// runSchemaCleanup 幂等数据迁移与废弃列清理（AutoMigrate 不删列，需手动执行）
// 安全：所有操作均有 IF EXISTS 或 WHERE 守卫，可重复执行。
func runSchemaCleanup(db *gorm.DB) {
	// ── 1. 迁移视频配置：ink_novel → ink_novel_video_config（INSERT IGNORE 幂等）
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
			logger.Errorf("[runSchemaCleanup] migrate video config: %v", err)
		}
	}

	// ── 3a. 软删除 ink_character 中同一小说下重复的角色名（保留 id 最小的，其余 soft-delete）。
	// 原因：2026-06-20 修复前，并发提取任务在极端时序下可写入 (novel_id, name) 重复行。
	if err := db.Exec(`UPDATE ink_character c1
		JOIN (
			SELECT novel_id, name, MIN(id) AS min_id
			FROM ink_character
			WHERE deleted_at IS NULL
			GROUP BY novel_id, name
			HAVING COUNT(*) > 1
		) dups ON c1.novel_id = dups.novel_id AND c1.name = dups.name AND c1.id != dups.min_id
		SET c1.deleted_at = NOW()
		WHERE c1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[runSchemaCleanup] dedup ink_character: %v", err)
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
		// 废弃的 ink_ai_model 无用元数据列（2026-06-24-v4 删除，从未被任何代码读取）
		{"ink_ai_model", "version"},
		{"ink_ai_model", "capabilities"},
		{"ink_ai_model", "context_window"},
		{"ink_ai_model", "speed"},
		{"ink_ai_model", "default_temperature"},
		// 废弃的 ink_model_provider 成本列（2026-06-24-v5 删除：从未被任何代码读取）
		{"ink_model_provider", "cost_per_1k_tokens"},
		// 废弃的 ink_model_provider 类型/容量列（2026-06-24-v6 删除：类型由 ink_ai_model.type 承载）
		{"ink_model_provider", "type"},
		{"ink_model_provider", "max_tokens"},
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
		// 孤儿字段删除（2026-06-25-v1）：struct 无映射、全库零引用
		{"ink_character",      "arc_design"},        // 从未被任何 Go struct 字段映射
		{"ink_character",      "current_arc_stage"}, // 同上
		{"ink_storyboard_shot", "frames"},            // struct 有声明但 service/handler 层零读写
		// 冗余字段删除（2026-06-25-v3）：is_available 与 is_active 语义重复
		{"ink_ai_model", "is_available"},
		// 社交统计迁移到 ink_content_stats（2026-06-25-v3）
		{"ink_novel", "view_count"},
		{"ink_novel", "like_count"},
		{"ink_novel", "comment_count"},
		{"ink_video", "view_count"},
		{"ink_video", "like_count"},
		{"ink_video", "comment_count"},
		// 废弃字段删除（2026-06-25-v4）
		{"ink_chapter", "quality_issues"}, // 已从 Chapter struct 移除，service 层不再读写
		{"ink_chapter", "like_count"},     // 改为从 ink_chapter_like COUNT 实时聚合，gorm:"-"
		{"ink_media_asset", "data"},       // 二进制数据已迁移至 OSS，DB 存储后端已废弃
		// 废弃字段删除（2026-06-25-v8）
		{"ink_arc_summary", "peak_tension"}, // 从未被写入（Upsert 不赋值），始终为 0
		// 废弃的 ink_model_provider 分组字段（2026-06-25-v5 删除，group 机制已全面移除）
		{"ink_model_provider", "group_name"},
		{"ink_model_provider", "is_group_canonical"},
		// ink_model_provider 限流字段迁移到 ink_ai_model（2026-06-25-v6）
		{"ink_model_provider", "timeout"},
		{"ink_model_provider", "concurrency"},
		{"ink_model_provider", "rate_limit"},
		// 废弃字段删除（2026-06-25-v11）：从未被任何 service/handler 代码读写
		{"ink_worldview_entity",  "attributes"},   // 从未被 handler/service 赋值，全部为 NULL
		{"ink_worldview_entity",  "relations"},    // 同上
		{"ink_knowledge_base",    "reference_id"}, // 从未被赋值，ReferenceNovel 分析管道未实现
		{"ink_reference_novel",   "uuid"},         // 从未被赋值；uniqueIndex 导致第二次插入崩溃（已在 preMigrateCleanup 删索引）
		{"ink_reference_novel",   "cover_image"},  // 从未被赋值
		{"ink_reference_novel",   "crawled_at"},   // 从未被赋值
		{"ink_reference_novel",   "total_words"},  // 从未被赋值（只有 total_chapters 被更新）
		{"ink_reference_chapter", "uuid"},         // 从未被赋值；uniqueIndex 同上（已在 preMigrateCleanup 删索引）
		{"ink_reference_chapter", "summary"},      // 从未被赋值
		{"ink_reference_chapter", "word_count"},   // 从未被赋值
		// 废弃字段删除（2026-06-25-v12）
		{"ink_character_state_snapshot", "snapshot_time"}, // 与 created_at 完全重叠，从未单独赋值
		{"ink_character_look",           "sort_order"},    // 与 chapter_from 语义重叠，从未用于查询排序
		// 废弃字段删除（2026-06-25-v13）
		{"ink_hook_chain", "foreshadow_id"}, // 与 ink_foreshadow.linked_hook_id 构成双向引用，保留 foreshadow 侧，删除 hook_chain 侧
		// 废弃字段删除（2026-06-25-v14）
		{"ink_outline_synthesis", "synthesized_at"}, // gorm.Model.updated_at 已代表最后一次综合分析时间，无需冗余字段
		// 废弃字段删除（2026-06-25-v15）
		{"ink_storyboard_shot",    "sfx_url"},         // 无任何 service 写入；ink_shot_sfx_item.url 为正式字段
		{"ink_storyboard_shot",    "sfx_volume"},      // ink_shot_sfx_item.volume 已承接混音音量职责
		{"ink_storyboard_shot",    "frame_image_url"}, // 始终等于 image_url，冗余；历史迁移已在 autoMigrate v2 完成
		{"ink_novel_video_config", "asset_export_path"}, // 从未被任何 service 读取用于实际导出
		{"ink_novel_video_config", "hd_enabled"},         // VisualMode ("hd"/"hd_3d") 已完整承接高清模式判断
		// 废弃字段删除（2026-06-25-v16）AI 模型管理优化
		{"ink_task_model_config",          "system_prompt"},     // 从未从 DB 读取注入 AI 请求，system prompt 由 ai_service 硬编码注入
		{"ink_task_model_config",          "max_cost_per_task"}, // 无 cost_per_1k_tokens 数据源，该字段无法产生有效成本计算
		{"ink_model_comparison_experiment","results"},           // 从未被 RunExperiment 写入，始终为 NULL
		{"ink_experiment_result",          "relevance_score"},   // RunExperiment 从未写入，始终 0
		{"ink_experiment_result",          "creativity_score"},  // 同上
		{"ink_experiment_result",          "consistency_score"}, // 同上
		{"ink_experiment_result",          "user_rating"},       // 无用户评分 API，从未写入
		{"ink_experiment_result",          "user_comment"},      // 同上
		{"ink_experiment_result",          "tokens_used"},       // GenerateWithProvider 不返回 token 数到 RunExperiment
		{"ink_experiment_result",          "cost"},              // 同上
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
			logger.Errorf("[runSchemaCleanup] drop %s.%s: %v", d.table, d.col, err)
		}
	}
}

// initSystemAdmin creates the system admin user if it doesn't exist.
// Call this from main.go after DB is ready.
func initSystemAdmin(db *gorm.DB, cfg *config.Config) {
	email := cfg.Admin.Email
	if email == "" {
		email = "admin@inkframe.io"
	}
	password := cfg.Admin.Password
	if password == "" {
		password = "Admin@123456"
	}

	var user model.User
	if err := db.Where("role = ?", model.RoleSystemAdmin).First(&user).Error; err == nil {
		return // already exists
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		logger.Errorf("[initSystemAdmin] bcrypt: %v", err)
		return
	}

	now := time.Now()
	admin := &model.User{
		UUID:            uuid.New().String(),
		Username:        "sysadmin",
		Email:           email,
		Password:        string(hashed),
		Nickname:        "System Admin",
		Status:          "active",
		Role:            model.RoleSystemAdmin,
		EmailVerifiedAt: &now,
	}
	if err := db.Create(admin).Error; err != nil {
		logger.Errorf("[initSystemAdmin] create: %v", err)
		return
	}
	logger.Printf("[initSystemAdmin] created system admin: %s", email)
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
	// ink_ai_model (provider_id, name) 新建唯一索引前先去重，保留每对 (provider_id, name) 中 id 最小的行
	if err := db.Exec(`DELETE FROM ink_ai_model WHERE id NOT IN (
		SELECT min_id FROM (
			SELECT MIN(id) AS min_id FROM ink_ai_model GROUP BY provider_id, name
		) t
	)`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_ai_model: %v", err)
	}
	// Ollama 预置模型清理：历史 seed 自动写入了 Ollama 模型，但 Ollama 是本地服务，
	// 哪些模型可用取决于用户实际安装情况，不应预置。删除系统级（tenant_id=0）的 Ollama 模型，
	// 保留用户手动添加（通过有租户 ID 的 provider）的模型。
	db.Exec(`DELETE m FROM ink_ai_model m
		INNER JOIN ink_model_provider p ON m.provider_id = p.id
		WHERE p.name = 'ollama' AND p.tenant_id = 0 AND m.deleted_at IS NULL`)
	// 无实现供应商清理：freesound/pixabay-sfx/pixabay-bgm/audioldm/jamendo/volcengine-i2i/kling-i2i
	// 这些供应商没有 Go 实现，也无 service 层调用，从 seed 移除并清理已有 DB 记录。
	for _, unsupported := range []string{
		"freesound", "pixabay-sfx", "pixabay-bgm", "audioldm", "jamendo",
		"volcengine-i2i", "kling-i2i",
	} {
		db.Exec(`DELETE m FROM ink_ai_model m
			INNER JOIN ink_model_provider p ON m.provider_id = p.id
			WHERE p.name = ? AND p.tenant_id = 0`, unsupported)
		db.Exec(`DELETE FROM ink_model_provider WHERE name = ? AND tenant_id = 0`, unsupported)
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

	// ── v11: 删除废弃字段遗留的唯一索引（DROP COLUMN 会自动删单列索引，但显式删除更稳）
	// ink_reference_novel.uuid uniqueIndex — uuid 字段从未被赋值，uniqueIndex 会导致第二次插入崩溃
	for _, idxName := range []string{"idx_reference_novels_uuid", "uni_ink_reference_novel_uuid"} {
		var cnt int64
		db.Raw(`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_reference_novel' AND INDEX_NAME = ?`, idxName).Scan(&cnt)
		if cnt > 0 {
			db.Exec("ALTER TABLE ink_reference_novel DROP INDEX `" + idxName + "`")
		}
	}
	// 按列名查找 uuid 上的所有索引并删除（兜底，覆盖 GORM 不同版本的命名差异）
	var refNovelUUIDIdxes []string
	db.Raw(`SELECT DISTINCT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_reference_novel' AND COLUMN_NAME = 'uuid'`).Scan(&refNovelUUIDIdxes)
	for _, idxName := range refNovelUUIDIdxes {
		db.Exec("ALTER TABLE ink_reference_novel DROP INDEX `" + idxName + "`")
	}
	// ink_reference_chapter.uuid uniqueIndex — 同上
	var refChapUUIDIdxes []string
	db.Raw(`SELECT DISTINCT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_reference_chapter' AND COLUMN_NAME = 'uuid'`).Scan(&refChapUUIDIdxes)
	for _, idxName := range refChapUUIDIdxes {
		db.Exec("ALTER TABLE ink_reference_chapter DROP INDEX `" + idxName + "`")
	}

	// ── v10: 建唯一索引前去重（保留 id 最小的行，软删除其余重复行）
	// ink_worldview_entity: (worldview_id, name, type) 唯一
	if err := db.Exec(`UPDATE ink_worldview_entity e1
		JOIN (
			SELECT worldview_id, name, type, MIN(id) AS min_id
			FROM ink_worldview_entity
			WHERE deleted_at IS NULL
			GROUP BY worldview_id, name, type
			HAVING COUNT(*) > 1
		) dups ON e1.worldview_id = dups.worldview_id AND e1.name = dups.name AND e1.type = dups.type AND e1.id != dups.min_id
		SET e1.deleted_at = NOW()
		WHERE e1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_worldview_entity: %v", err)
	}
	// ink_reference_novel: (source_url, source_site) 唯一（仅对有 deleted_at 列的 DB 有效；新列由 ensureCriticalColumns 提前添加）
	if err := db.Exec(`UPDATE ink_reference_novel r1
		JOIN (
			SELECT source_url, source_site, MIN(id) AS min_id
			FROM ink_reference_novel
			WHERE deleted_at IS NULL AND source_url != '' AND source_url IS NOT NULL
			GROUP BY source_url, source_site
			HAVING COUNT(*) > 1
		) dups ON r1.source_url = dups.source_url AND r1.source_site = dups.source_site AND r1.id != dups.min_id
		SET r1.deleted_at = NOW()
		WHERE r1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_reference_novel: %v", err)
	}
	// ink_reference_chapter: (novel_id, chapter_no) 唯一
	if err := db.Exec(`UPDATE ink_reference_chapter c1
		JOIN (
			SELECT novel_id, chapter_no, MIN(id) AS min_id
			FROM ink_reference_chapter
			WHERE deleted_at IS NULL
			GROUP BY novel_id, chapter_no
			HAVING COUNT(*) > 1
		) dups ON c1.novel_id = dups.novel_id AND c1.chapter_no = dups.chapter_no AND c1.id != dups.min_id
		SET c1.deleted_at = NOW()
		WHERE c1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_reference_chapter: %v", err)
	}

	// ── v12: 建唯一索引前去重（角色/道具/快照）
	// ink_character: (novel_id, name) 唯一 — 保留 id 最小的，软删除其余重复行
	if err := db.Exec(`UPDATE ink_character c1
		JOIN (
			SELECT novel_id, name, MIN(id) AS min_id
			FROM ink_character
			WHERE deleted_at IS NULL
			GROUP BY novel_id, name
			HAVING COUNT(*) > 1
		) dups ON c1.novel_id = dups.novel_id AND c1.name = dups.name AND c1.id != dups.min_id
		SET c1.deleted_at = NOW()
		WHERE c1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_character: %v", err)
	}
	// ink_item: (novel_id, name) 唯一
	if err := db.Exec(`UPDATE ink_item i1
		JOIN (
			SELECT novel_id, name, MIN(id) AS min_id
			FROM ink_item
			WHERE deleted_at IS NULL
			GROUP BY novel_id, name
			HAVING COUNT(*) > 1
		) dups ON i1.novel_id = dups.novel_id AND i1.name = dups.name AND i1.id != dups.min_id
		SET i1.deleted_at = NOW()
		WHERE i1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_item: %v", err)
	}
	// ink_character_state_snapshot: (character_id, chapter_id) 唯一 — 保留 id 最大（最新）的快照
	// deleted_at 列由 ensureCriticalColumns 提前添加，此处可安全使用
	if err := db.Exec(`UPDATE ink_character_state_snapshot s1
		JOIN (
			SELECT character_id, chapter_id, MAX(id) AS max_id
			FROM ink_character_state_snapshot
			WHERE deleted_at IS NULL
			GROUP BY character_id, chapter_id
			HAVING COUNT(*) > 1
		) dups ON s1.character_id = dups.character_id AND s1.chapter_id = dups.chapter_id AND s1.id != dups.max_id
		SET s1.deleted_at = NOW()
		WHERE s1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_character_state_snapshot: %v", err)
	}
	// ink_ignored_review_issue: (entity_type, entity_id, issue_hash) 唯一 — 保留 id 最大的记录（硬删除重复行）
	// 2026-06-25-v14 新增唯一索引前清理历史重复数据
	if err := db.Exec(`DELETE i1 FROM ink_ignored_review_issue i1
		JOIN (
			SELECT entity_type, entity_id, issue_hash, MAX(id) AS max_id
			FROM ink_ignored_review_issue
			WHERE deleted_at IS NULL
			GROUP BY entity_type, entity_id, issue_hash
			HAVING COUNT(*) > 1
		) dups ON i1.entity_type = dups.entity_type AND i1.entity_id = dups.entity_id
			AND i1.issue_hash = dups.issue_hash AND i1.id != dups.max_id
		WHERE i1.deleted_at IS NULL`).Error; err != nil {
		logger.Errorf("[preMigrateCleanup] dedup ink_ignored_review_issue: %v", err)
	}
}

// ensureColumnTypes 将指定列的类型升级为更大的类型（如 text→mediumtext）。
// 幂等安全：先检查 information_schema.COLUMNS 的 DATA_TYPE，已是目标类型则跳过。
// 在 ensureCriticalColumns 之后调用，确保列存在后再判断类型。
func ensureColumnTypes(db *gorm.DB) {
	type colMod struct{ table, col string }
	toMediumtext := []colMod{
		// ink_worldview：世界观 7 个文本字段，玄幻/仙侠内容可能超过 text 64KB 上限
		{"ink_worldview", "magic_system"},
		{"ink_worldview", "geography"},
		{"ink_worldview", "history"},
		{"ink_worldview", "culture"},
		{"ink_worldview", "rules"},
		{"ink_worldview", "factions"},
		{"ink_worldview", "glossary"},
		// ink_knowledge_base：知识条目内容可能超过 64KB
		{"ink_knowledge_base", "content"},
		// ink_reference_chapter：爬取原文内容可能超过 64KB
		{"ink_reference_chapter", "content"},
	}
	for _, m := range toMediumtext {
		var colType string
		db.Raw(
			`SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			m.table, m.col,
		).Scan(&colType)
		if colType == "" || colType == "mediumtext" || colType == "longtext" {
			continue // 列不存在（AutoMigrate 会建）或已是更大类型，跳过
		}
		sql := "ALTER TABLE `" + m.table + "` MODIFY COLUMN `" + m.col + "` mediumtext"
		if err := db.Exec(sql).Error; err != nil {
			logger.Errorf("ensureColumnTypes: %s.%s text→mediumtext: %v", m.table, m.col, err)
		} else {
			logger.Infof("ensureColumnTypes: %s.%s promoted to mediumtext", m.table, m.col)
		}
	}

	// varchar→text：确保短 varchar 列升级为 text（2026-06-25-v13）
	toText := []colMod{
		{"ink_foreshadow", "tags"}, // VARCHAR(500) → text（标签数量多时可能截断）
		{"ink_video",      "tags"}, // VARCHAR(500) → text（2026-06-25-v15）
	}
	for _, m := range toText {
		var colType string
		db.Raw(
			`SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			m.table, m.col,
		).Scan(&colType)
		if colType == "" || colType == "text" || colType == "mediumtext" || colType == "longtext" {
			continue
		}
		sql := "ALTER TABLE `" + m.table + "` MODIFY COLUMN `" + m.col + "` text"
		if err := db.Exec(sql).Error; err != nil {
			logger.Errorf("ensureColumnTypes: %s.%s varchar→text: %v", m.table, m.col, err)
		} else {
			logger.Infof("ensureColumnTypes: %s.%s promoted to text", m.table, m.col)
		}
	}
}

// ensureCriticalIndexes 幂等补全缺失索引（检查 information_schema.STATISTICS 后再 CREATE）。
// 查询前先确认表存在，避免表尚未 AutoMigrate 时报错。
func ensureCriticalIndexes(db *gorm.DB) {
	type idxDef struct {
		table  string
		name   string
		cols   string
		unique bool
	}
	indexes := []idxDef{
		{"ink_storyboard_shot", "idx_shot_video_shot_no", "(video_id, shot_no)", false},
		{"ink_chapter_read_record", "idx_read_user_novel", "(user_id, novel_id)", false},
		{"ink_asset", "idx_asset_creator", "(creator_id, type, status)", false},
		{"ink_chapter", "idx_chapter_novel_published", "(novel_id, is_published, chapter_no)", false},
		{"ink_entity_comment", "idx_comment_entity_created", "(entity_type, entity_id, created_at)", false},
		// 唯一约束：防止同一小说写入重复大纲版本号（2026-06-25-v9）
		{"ink_novel_outline_version", "idx_outline_novel_ver", "(novel_id, version)", true},
		// 唯一约束：防止重复实体/章节（2026-06-25-v10）
		{"ink_worldview_entity",  "idx_we_name_type",         "(worldview_id, name, type)", true},
		{"ink_reference_novel",   "idx_ref_novel_url_site",   "(source_url, source_site)", true},
		{"ink_reference_chapter", "idx_ref_chapter_novel_no", "(novel_id, chapter_no)", true},
		// 唯一约束：角色/道具/快照（2026-06-25-v12）
		{"ink_character",                "uniq_char_novel_name",        "(novel_id, name)", true},
		{"ink_item",                     "uniq_item_novel_name",        "(novel_id, name)", true},
		{"ink_character_state_snapshot", "uniq_snapshot_char_chapter",  "(character_id, chapter_id)", true},
		// 大纲审查与质量控制优化（2026-06-25-v14）
		{"ink_continuity_report",    "idx_continuity_novel_chapter", "(novel_id, chapter_id)", false},
		{"ink_review_record",        "idx_review_entity",            "(entity_type, entity_id)", false},
		{"ink_ignored_review_issue", "idx_ignored_entity",           "(entity_type, entity_id)", false},
		{"ink_ignored_review_issue", "uniq_ignored_issue",           "(entity_type, entity_id, issue_hash)", true},
	}
	for _, idx := range indexes {
		// 先检查表是否存在，避免在 AutoMigrate 之前报错
		var tblCnt int64
		db.Raw(
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			idx.table,
		).Scan(&tblCnt)
		if tblCnt == 0 {
			continue
		}
		// 检查索引是否已存在
		var cnt int64
		db.Raw(
			"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?",
			idx.table, idx.name,
		).Scan(&cnt)
		if cnt > 0 {
			continue
		}
		idxType := "INDEX"
		if idx.unique {
			idxType = "UNIQUE INDEX"
		}
		sql := "ALTER TABLE `" + idx.table + "` ADD " + idxType + " `" + idx.name + "` " + idx.cols
		if err := db.Exec(sql).Error; err != nil {
			logger.Errorf("ensureCriticalIndexes: %s.%s: %v", idx.table, idx.name, err)
		} else {
			logger.Infof("ensureCriticalIndexes: added index %s.%s", idx.table, idx.name)
		}
	}
}
