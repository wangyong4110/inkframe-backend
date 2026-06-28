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
const schemaVersion = "2026-06-28-v4"

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

	// 读取当前已迁移版本（在锁内读取，保证读到最新值）
	var storedVer string
	db.Raw("SELECT ver FROM ink_schema_version WHERE id = 1").Scan(&storedVer)
	if storedVer == schemaVersion {
		logger.Printf("autoMigrate: schema version %s already up-to-date, skipping", schemaVersion)
		return nil
	}

	logger.Printf("autoMigrate: migrating schema %s → %s", storedVer, schemaVersion)
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
		&model.AssetPublishRequest{},
		&model.AssetVersion{},
		&model.AssetCollection{},
		&model.AssetCollectionItem{},
		&model.CrawlJob{},
		&model.AssetLike{},
		&model.AssetUsage{},
		&model.AssetComment{},
		&model.AssetShareLink{},
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
		// 用户反馈
		&model.UserFeedback{},
		// 短剧模板
		&model.DramaTemplate{},
	); err != nil {
		return err
	}

	// 删除已废弃列（voices_json 已迁移至代码内置表 model.BuiltinVoices）
	db.Exec("ALTER TABLE ink_model_provider DROP COLUMN IF EXISTS voices_json")

	// 修正 deepseek-v4 → deepseek-v4-pro（API 已更名，旧记录需要同步）
	db.Exec("UPDATE ink_ai_model SET model_id = 'deepseek-v4-pro', name = 'DeepSeek V4 Pro' WHERE model_id = 'deepseek-v4'")

	// 补全缺失索引（幂等，已存在则跳过）
	ensureCriticalIndexes(db)

	// 迁移成功后写入新版本号（UPSERT）
	return db.Exec("INSERT INTO ink_schema_version (id, ver) VALUES (1, ?) ON DUPLICATE KEY UPDATE ver = ?",
		schemaVersion, schemaVersion).Error
}

// initSystemAdmin creates the system admin user if it doesn't exist.
// Only runs when admin.email and admin.password are explicitly set in config.yaml.
// Call this from main.go after DB is ready.
func initSystemAdmin(db *gorm.DB, cfg *config.Config) {
	email := cfg.Admin.Email
	password := cfg.Admin.Password
	if email == "" || password == "" {
		logger.Printf("[initSystemAdmin] skipped: admin.email/password not configured in config.yaml")
		return
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
		UUID:     uuid.New().String(),
		Username: "sysadmin",
		Email:    email,
		Password: string(hashed),
		Nickname: "System Admin",
		Status:   "active",
		Role:     model.RoleSystemAdmin,
		SecurityMeta: model.UserSecurityMeta{
			EmailVerifiedAt: &now,
		},
	}
	if err := db.Create(admin).Error; err != nil {
		logger.Errorf("[initSystemAdmin] create: %v", err)
		return
	}
	logger.Printf("[initSystemAdmin] created system admin: %s", email)
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
