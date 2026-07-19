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
const schemaVersion = "2026-07-18-v4"

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

	// ink_character_look 精简（2026-07-17-v2）：角色形象不再支持按章节区间自动切换
	// （chapter_from/chapter_to 连带的选取逻辑已从代码中整体移除，统一改为
	// Character.DefaultLookID 直接指定当前形象）；面部参考图（portrait）不再单独生成/存储，
	// 分镜/视频一致性参考图统一改用 three_view_sheet（正/侧/背/面部特写合图）；
	// face_prompt（面部特写专用提示词）随面部参考图功能一并下线。
	db.Exec("ALTER TABLE ink_character_look DROP COLUMN IF EXISTS chapter_from")
	db.Exec("ALTER TABLE ink_character_look DROP COLUMN IF EXISTS chapter_to")
	db.Exec("ALTER TABLE ink_character_look DROP COLUMN IF EXISTS face_prompt")
	db.Exec("ALTER TABLE ink_character_look DROP COLUMN IF EXISTS portrait")

	// ink_chapter 表结构重设计（2026-07-03-v1）：
	// 将 narrative_meta/quality_meta JSON blob 拆平为直接列，移除 uuid/act_no/hook_type
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS narrative_meta")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS quality_meta")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS uuid")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS act_no")
	db.Exec("ALTER TABLE ink_chapter DROP COLUMN IF EXISTS hook_type")

	// 修正 deepseek-v4 → deepseek-v4-pro（API 已更名，旧记录需要同步）
	db.Exec("UPDATE ink_ai_model SET model_id = 'deepseek-v4-pro', name = 'DeepSeek V4 Pro' WHERE model_id = 'deepseek-v4'")

	// ink_asset.description 从 JSON blob（asset_media_meta.description）提升为独立列，
	// 支持 FULLTEXT 检索，不再需要每次查询都做 JSON_EXTRACT + 全表扫描（2026-07-16-v1）。
	//
	// 注意：不用 "ADD COLUMN IF NOT EXISTS"（MySQL 8.0.29+ 才支持，旧版本/MariaDB 会报语法错误）。
	// 用 information_schema 查询列是否存在，再决定要不要 ALTER，且必须检查错误——
	// 否则一旦 ALTER 失败，下面还是会把 schemaVersion 写进去，导致这次迁移永久被跳过，
	// 列却始终没加上（历史上就是这样导致 INSERT INTO ink_asset 报 Unknown column 'description'）。
	var descColCnt int64
	db.Raw(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_asset' AND COLUMN_NAME = 'description'`,
	).Scan(&descColCnt)
	if descColCnt == 0 {
		if err := db.Exec("ALTER TABLE ink_asset ADD COLUMN description TEXT").Error; err != nil {
			return fmt.Errorf("autoMigrate: failed to add ink_asset.description: %w", err)
		}
	}
	// 回填旧数据：只在新列为空、且 JSON 里确实有值时才写入，避免覆盖已经迁移过的记录。
	db.Exec(`UPDATE ink_asset SET description = JSON_UNQUOTE(JSON_EXTRACT(asset_media_meta, '$.description'))
		WHERE (description IS NULL OR description = '')
		AND JSON_EXTRACT(asset_media_meta, '$.description') IS NOT NULL`)
	ensureAssetDescriptionFulltextIndex(db)

	// ink_asset_collection / ink_asset_collection_item 已废弃（收藏夹功能后端已实现但前端
	// 从未接入，纯死代码，已随本次改动一并删除 Go 侧 model/repository/service/handler/router），
	// ink_asset_version 也已废弃（素材只保留最新数据，不再需要版本历史/回滚功能，前后端相关
	// 代码已一并删除），显式 DROP 掉物理表——GORM AutoMigrate 只会新增列/表，永远不会删除，
	// 不然这些表会一直留在库里。子表（*_item）先删，避免残留的孤儿行；用 information_schema
	// 判断表是否存在，避免对不存在的表重复执行 DROP 报错（虽然 DROP TABLE IF EXISTS 本身也是
	// 幂等的，这里仍统一走已建立的 existence-check 模式，方便和其它迁移语句一起阅读）。
	for _, table := range []string{"ink_asset_collection_item", "ink_asset_collection", "ink_asset_version"} {
		var tblCnt int64
		db.Raw(
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			table,
		).Scan(&tblCnt)
		if tblCnt == 0 {
			continue
		}
		if err := db.Exec("DROP TABLE IF EXISTS `" + table + "`").Error; err != nil {
			return fmt.Errorf("autoMigrate: failed to drop %s: %w", table, err)
		}
	}

	// ink_video.progress / ink_storyboard_shot.progress：视频合成/分镜批处理的实时进度列，
	// 代码里一直在用 videoRepo.UpdateFields(id, {"progress": pct})／storyboardRepo.UpdateFields(id,
	// {"progress": ...}) 直接 SET 这个列（比读改写整个 task_meta JSON 便宜得多），但一直没有配
	// 对应的迁移，导致线上报 Error 1054 Unknown column 'progress'（2026-07-16-v3 遗漏，本次补上）。
	// 同样：先查 information_schema 再 ALTER，且必须检查错误，避免重蹈 description 列的覆辙。
	type progressCol struct{ table, column string }
	for _, c := range []progressCol{
		{"ink_video", "progress"},
		{"ink_storyboard_shot", "progress"},
	} {
		var colCnt int64
		db.Raw(
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			c.table, c.column,
		).Scan(&colCnt)
		if colCnt == 0 {
			if err := db.Exec("ALTER TABLE `" + c.table + "` ADD COLUMN `" + c.column + "` INT NOT NULL DEFAULT 0").Error; err != nil {
				return fmt.Errorf("autoMigrate: failed to add %s.%s: %w", c.table, c.column, err)
			}
		}
	}

	// ink_screenplay_scene：分场剧本，全新表（不是加列）——这里也不能指望 GORM AutoMigrate
	// 自动建表（本仓库只对 CharacterLook 做 AutoMigrate，见上方大段说明），必须手写建表 SQL，
	// 字段需与 model.ScreenplayScene 的 gorm 标签手动对应；CREATE TABLE IF NOT EXISTS 本身
	// 幂等，仍需检查错误，理由同上（避免"表没建但版本号已写入"导致后续启动永久跳过）。
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ink_screenplay_scene (
		id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		chapter_id           BIGINT UNSIGNED NOT NULL,
		novel_id             BIGINT UNSIGNED NOT NULL,
		scene_no             INT NOT NULL,
		heading              VARCHAR(255) NOT NULL DEFAULT '',
		scene_anchor_id      BIGINT UNSIGNED DEFAULT NULL,
		synopsis             TEXT,
		character_ids        JSON,
		emotional_tone       VARCHAR(100) NOT NULL DEFAULT '',
		beats                TEXT,
		estimated_shot_count INT NOT NULL DEFAULT 0,
		locked               TINYINT(1) NOT NULL DEFAULT 0,
		edited               TINYINT(1) NOT NULL DEFAULT 0,
		created_at           DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		updated_at           DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
		PRIMARY KEY (id),
		KEY idx_screenplay_scene_chapter_no (chapter_id, scene_no),
		KEY idx_screenplay_scene_novel_id (novel_id),
		KEY idx_screenplay_scene_anchor_id (scene_anchor_id)
	)`).Error; err != nil {
		return fmt.Errorf("autoMigrate: failed to create ink_screenplay_scene: %w", err)
	}
	// ink_screenplay_scene.edited：表本身在上面的 CREATE TABLE IF NOT EXISTS 里已经带了这一列，
	// 但如果该表在本次改动前已经被创建过（不含这一列），CREATE TABLE IF NOT EXISTS 不会给已存在
	// 的表补列，所以仍需要单独一条幂等 ALTER，和本文件其它"新增列"迁移保持同样的写法。
	var screenplayEditedColCnt int64
	db.Raw(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_screenplay_scene' AND COLUMN_NAME = 'edited'`,
	).Scan(&screenplayEditedColCnt)
	if screenplayEditedColCnt == 0 {
		if err := db.Exec("ALTER TABLE ink_screenplay_scene ADD COLUMN edited TINYINT(1) NOT NULL DEFAULT 0").Error; err != nil {
			return fmt.Errorf("autoMigrate: failed to add ink_screenplay_scene.edited: %w", err)
		}
	}

	// ink_screenplay_scene.character_ids：分场剧本"出场角色"字段——生成/展示这场戏用了哪些角色
	// 的下游消费方从来没有实现过（分镜生成是逐行重新解析 beats 文本识别角色，前端也只展示
	// 分镜级别的角色绑定），一直是写了但没人读的孤立字段，删除。
	var screenplayCharacterIDsColCnt int64
	db.Raw(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_screenplay_scene' AND COLUMN_NAME = 'character_ids'`,
	).Scan(&screenplayCharacterIDsColCnt)
	if screenplayCharacterIDsColCnt > 0 {
		if err := db.Exec("ALTER TABLE ink_screenplay_scene DROP COLUMN character_ids").Error; err != nil {
			return fmt.Errorf("autoMigrate: failed to drop ink_screenplay_scene.character_ids: %w", err)
		}
	}

	// ink_screenplay_scene_version：分场剧本历史快照，全新表——"生成剧本"覆盖某场次前先在这里
	// 存一条覆盖前内容的快照，供用户查看/恢复历史版本。
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ink_screenplay_scene_version (
		id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		screenplay_scene_id  BIGINT UNSIGNED NOT NULL,
		chapter_id           BIGINT UNSIGNED NOT NULL,
		novel_id             BIGINT UNSIGNED NOT NULL,
		version_no           INT NOT NULL,
		content              JSON,
		change_type          VARCHAR(50) NOT NULL DEFAULT '',
		created_at           DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		PRIMARY KEY (id),
		UNIQUE KEY uk_scene_version (screenplay_scene_id, version_no),
		KEY idx_scene_version_chapter_id (chapter_id)
	)`).Error; err != nil {
		return fmt.Errorf("autoMigrate: failed to create ink_screenplay_scene_version: %w", err)
	}

	// ink_storyboard_shot_version：分镜历史快照，全新表——整视频重新生成分镜前，把该视频当时
	// 全部分镜行序列化成一条 JSON 快照存这里（分镜是整视频删除重建，不是按行覆盖，所以按视频
	// 存一份快照，而不是按单条 shot）。
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ink_storyboard_shot_version (
		id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		video_id     BIGINT UNSIGNED NOT NULL,
		version_no   INT NOT NULL,
		content      JSON,
		shot_count   INT NOT NULL DEFAULT 0,
		change_type  VARCHAR(50) NOT NULL DEFAULT '',
		created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		PRIMARY KEY (id),
		UNIQUE KEY uk_shot_version (video_id, version_no)
	)`).Error; err != nil {
		return fmt.Errorf("autoMigrate: failed to create ink_storyboard_shot_version: %w", err)
	}

	// ink_storyboard_shot.screenplay_scene_id：分镜归属的分场剧本，nil 兼容旧的直接生成路径。
	var screenplaySceneColCnt int64
	db.Raw(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_storyboard_shot' AND COLUMN_NAME = 'screenplay_scene_id'`,
	).Scan(&screenplaySceneColCnt)
	if screenplaySceneColCnt == 0 {
		if err := db.Exec("ALTER TABLE ink_storyboard_shot ADD COLUMN screenplay_scene_id BIGINT UNSIGNED DEFAULT NULL, ADD INDEX idx_shot_screenplay_scene (screenplay_scene_id)").Error; err != nil {
			return fmt.Errorf("autoMigrate: failed to add ink_storyboard_shot.screenplay_scene_id: %w", err)
		}
	}

	// ink_item：精简道具配置——删除"类别"（category，早已不在 Go model 里，是历史遗留列）、
	// "持有状态"（status）、"道具描述"（description，图片生成 prompt 已改为只用 VisualPrompt
	// 兜底，不再依赖这个字段）三列。DROP COLUMN 本身幂等，仍统一走 existence-check 模式。
	for _, col := range []string{"category", "status", "description"} {
		var cnt int64
		db.Raw(
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_item' AND COLUMN_NAME = ?`,
			col,
		).Scan(&cnt)
		if cnt > 0 {
			if err := db.Exec("ALTER TABLE ink_item DROP COLUMN `" + col + "`").Error; err != nil {
				return fmt.Errorf("autoMigrate: failed to drop ink_item.%s: %w", col, err)
			}
		}
	}

	// ink_scene_anchor：精简场景锚点配置——删除"类型"（type，interior/exterior/imaginary）、
	// "变体"（variant，day/night/winter 等）两列。parent_anchor_id 完全依附于 variant 机制而存在
	// （唯一赋值路径是 AI 提取时解析 variant 场景的父级名称），variant 删除后它变成永远不会被
	// 写入、也没有任何代码读取的孤儿列，一并删除。usage_count 同样是孤儿列：全库搜索确认没有任何
	// 代码路径会递增它（历史上曾有的 UpdateStats 维护逻辑已不存在，只留了一条对不上任何函数的
	// 孤立注释），永远停留在建表时的默认值 0，随类型/变体一并清理。DROP COLUMN 幂等，走
	// existence-check 模式。
	for _, col := range []string{"type", "variant", "parent_anchor_id", "usage_count"} {
		var cnt int64
		db.Raw(
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_scene_anchor' AND COLUMN_NAME = ?`,
			col,
		).Scan(&cnt)
		if cnt > 0 {
			if err := db.Exec("ALTER TABLE ink_scene_anchor DROP COLUMN `" + col + "`").Error; err != nil {
				return fmt.Errorf("autoMigrate: failed to drop ink_scene_anchor.%s: %w", col, err)
			}
		}
	}

	// ink_image_style_preset：画风预设（风格库页面），全新表——同样不能指望 GORM AutoMigrate
	// 自动建表（本仓库只对 CharacterLook 做 AutoMigrate，见上方大段说明），必须手写建表 SQL，
	// 字段需与 model.ImageStylePreset 的 gorm 标签手动对应。
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ink_image_style_preset (
		id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		style_id            VARCHAR(50) NOT NULL,
		name                VARCHAR(100) NOT NULL,
		description         TEXT,
		tags                TEXT,
		category            VARCHAR(20) NOT NULL DEFAULT '',
		prompt_category     VARCHAR(24) NOT NULL DEFAULT '',
		preview_colors      TEXT,
		preview_image_url   VARCHAR(1000),
		prompt              TEXT,
		sort_order          INT NOT NULL DEFAULT 0,
		is_builtin          TINYINT(1) NOT NULL DEFAULT 0,
		enabled             TINYINT(1) NOT NULL DEFAULT 1,
		created_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		updated_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
		PRIMARY KEY (id),
		UNIQUE KEY uni_image_style_preset_style_id (style_id),
		KEY idx_image_style_preset_category (category)
	)`).Error; err != nil {
		return fmt.Errorf("autoMigrate: failed to create ink_image_style_preset: %w", err)
	}
	// ink_image_style_preset.prompt_category：风格库统一分类字段，供 resolveStyleCategory()
	// （character_service.go）读取以选择质量提升词/冲突清理词大类，替代此前的硬编码 styleID switch。
	// 表在上面的 CREATE TABLE IF NOT EXISTS 里已经带了这一列，但已存在的表不会被补列，
	// 仍需要单独一条幂等 ALTER（模式同 ink_asset.description，见上方说明）。
	var promptCategoryColCnt int64
	db.Raw(
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_image_style_preset' AND COLUMN_NAME = 'prompt_category'`,
	).Scan(&promptCategoryColCnt)
	if promptCategoryColCnt == 0 {
		if err := db.Exec("ALTER TABLE ink_image_style_preset ADD COLUMN prompt_category VARCHAR(24) NOT NULL DEFAULT ''").Error; err != nil {
			return fmt.Errorf("autoMigrate: failed to add ink_image_style_preset.prompt_category: %w", err)
		}
	}

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

// ensureAssetDescriptionFulltextIndex 幂等地给 ink_asset.description 加 FULLTEXT 索引。
// 单独处理（而非塞进 ensureCriticalIndexes 的 idxDef 表）：FULLTEXT 是第三种索引类型，
// idxDef 目前只用 unique bool 区分 INDEX/UNIQUE INDEX，加个字段就要改动全部现有条目。
func ensureAssetDescriptionFulltextIndex(db *gorm.DB) {
	const table, name = "ink_asset", "idx_asset_description_ft"
	var tblCnt int64
	db.Raw(
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		table,
	).Scan(&tblCnt)
	if tblCnt == 0 {
		return
	}
	var cnt int64
	db.Raw(
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?",
		table, name,
	).Scan(&cnt)
	if cnt > 0 {
		return
	}
	if err := db.Exec("ALTER TABLE `" + table + "` ADD FULLTEXT INDEX `" + name + "` (description)").Error; err != nil {
		logger.Errorf("ensureAssetDescriptionFulltextIndex: %v", err)
	} else {
		logger.Infof("ensureAssetDescriptionFulltextIndex: added fulltext index %s.%s", table, name)
	}
}
