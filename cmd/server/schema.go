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
const schemaVersion = "2026-07-14-v1"

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
	// 注意（2026-07-14）：本次版本升级只是为了给 &model.CharacterLook{} 加 face_prompt 列，
	// 因此本轮 AutoMigrate 暂时只传这一个 model，不再传完整的模型列表。
	//
	// 原因：完整列表里几乎每张带唯一索引（uniqueIndex）的表都会触发 gorm.io/driver/mysql
	// v1.5.2 的一个协调 bug——协调该表时会尝试 ALTER TABLE ... DROP FOREIGN KEY <驱动按自身
	// 默认命名规则算出的约束名>，而这个名字往往并不对应任何真实存在的外键（唯一索引本来就
	// 不是外键，MySQL 的 DROP FOREIGN KEY 语法对它必然报 42000/1091），导致 FATAL 阻断启动。
	// 已实测在 &model.Tenant{}（uni_tenants_code）、&model.User{}（uni_users_uuid）上复现，
	// 全列表里还有十余处同类 uniqueIndex 字段，大概率会逐个复现同样的问题。
	//
	// 这个 bug 与本次要新增的列本身无关，只是"整表全量协调"这个动作本身就会触发它。
	// 因此暂时只精确迁移 CharacterLook，避免连带触碰其它完全不需要变更的表。
	//
	// 后续如果要恢复完整列表（比如升级 gorm/driver/mysql 到修复该问题的版本之后），
	// 完整模型列表可以从 git 历史里找回（git log -p 本文件，本次改动之前的版本），
	// 恢复前请逐个验证受影响的表（参考上面两个已知案例的排查方法）。
	if err := db.AutoMigrate(
		&model.CharacterLook{},
	); err != nil {
		return err
	}

	// 删除已废弃列（voices_json 已迁移至代码内置表 model.BuiltinVoices）
	db.Exec("ALTER TABLE ink_model_provider DROP COLUMN IF EXISTS voices_json")

	// ink_chapter 表结构重设计（2026-07-03-v1）：
	// 将 narrative_meta/quality_meta JSON blob 拆平为直接列，移除 uuid/act_no/hook_type
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS narrative_meta")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS quality_meta")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS uuid")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS act_no")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS hook_type")

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
