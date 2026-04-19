-- ============================================
-- InkFrame Database Migration Script
-- Version: 001_init_schema
-- Description: Initial database schema for InkFrame
-- Author: InkFrame Team
-- Created: 2026-04-19
-- ============================================

SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS = 0;

-- ============================================
-- Part 1: Multi-Tenant Support Tables
-- ============================================

-- Tenant table (租户/组织)
DROP TABLE IF EXISTS `tenants`;
CREATE TABLE `tenants` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '租户名称',
  `code` VARCHAR(50) NOT NULL UNIQUE COMMENT '租户代码(唯一标识)',
  `logo` VARCHAR(500) COMMENT 'Logo URL',
  `settings` TEXT COMMENT '租户配置JSON',
  `plan` VARCHAR(20) NOT NULL DEFAULT 'free' COMMENT '套餐: free/pro/enterprise',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态: active/suspended/banned',
  
  -- Quotas (配额)
  `max_projects` INT NOT NULL DEFAULT 5 COMMENT '最大项目数',
  `max_users` INT NOT NULL DEFAULT 3 COMMENT '最大用户数',
  `max_storage_mb` INT NOT NULL DEFAULT 1000 COMMENT '最大存储MB',
  `used_projects` INT NOT NULL DEFAULT 0 COMMENT '已用项目数',
  `used_users` INT NOT NULL DEFAULT 0 COMMENT '已用用户数',
  `used_storage_mb` INT NOT NULL DEFAULT 0 COMMENT '已用存储MB',
  
  -- Billing (计费)
  `billing_cycle` VARCHAR(20) NOT NULL DEFAULT 'monthly' COMMENT '计费周期: monthly/yearly',
  `expires_at` DATETIME COMMENT '到期时间',
  
  -- Contact (联系信息)
  `description` VARCHAR(500) COMMENT '描述',
  `contact_email` VARCHAR(100) COMMENT '联系邮箱',
  `contact_phone` VARCHAR(20) COMMENT '联系电话',
  
  -- SEO
  `meta_title` VARCHAR(200) COMMENT 'SEO标题',
  `meta_keywords` VARCHAR(500) COMMENT 'SEO关键词',
  `meta_desc` VARCHAR(500) COMMENT 'SEO描述',
  
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_code` (`code`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='租户表';

-- TenantUser table (租户用户关联)
DROP TABLE IF EXISTS `tenant_users`;
CREATE TABLE `tenant_users` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `tenant_id` BIGINT UNSIGNED NOT NULL COMMENT '租户ID',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
  `role` VARCHAR(20) NOT NULL DEFAULT 'member' COMMENT '角色: owner/admin/member/viewer',
  `nickname` VARCHAR(50) COMMENT '在租户内的昵称',
  `avatar` VARCHAR(500) COMMENT '头像',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态: active/inactive',
  `permissions` TEXT COMMENT '自定义权限JSON',
  
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  UNIQUE KEY `uk_tenant_user` (`tenant_id`, `user_id`),
  INDEX `idx_tenant_id` (`tenant_id`),
  INDEX `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='租户用户关联表';

-- TenantProject table (租户项目)
DROP TABLE IF EXISTS `tenant_projects`;
CREATE TABLE `tenant_projects` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `tenant_id` BIGINT UNSIGNED NOT NULL COMMENT '租户ID',
  `project_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '项目ID',
  `project_type` VARCHAR(20) NOT NULL DEFAULT 'novel' COMMENT '项目类型: novel/custom',
  `name` VARCHAR(100) NOT NULL COMMENT '项目名称',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态: active/archived/deleted',
  `members` TEXT COMMENT '成员列表JSON',
  `settings` TEXT COMMENT '项目设置JSON',
  `tags` TEXT COMMENT '标签JSON',
  `storage_used` BIGINT NOT NULL DEFAULT 0 COMMENT '已用存储字节',
  
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_tenant_id` (`tenant_id`),
  INDEX `idx_project_id` (`project_id`),
  UNIQUE KEY `uk_tenant_project` (`tenant_id`, `project_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='租户项目表';

-- User table (用户)
DROP TABLE IF EXISTS `users`;
CREATE TABLE `users` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `username` VARCHAR(50) NOT NULL UNIQUE COMMENT '用户名',
  `email` VARCHAR(100) NOT NULL UNIQUE COMMENT '邮箱',
  `phone` VARCHAR(20) COMMENT '手机号',
  `password` VARCHAR(100) NOT NULL COMMENT '密码哈希',
  `nickname` VARCHAR(50) COMMENT '昵称',
  `avatar` VARCHAR(500) COMMENT '头像',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态: active/inactive/banned',
  `role` VARCHAR(20) NOT NULL DEFAULT 'user' COMMENT '系统角色: admin/user',
  
  -- OAuth
  `oauth_provider` VARCHAR(20) COMMENT 'OAuth提供商',
  `oauth_id` VARCHAR(100) COMMENT 'OAuth ID',
  
  -- Settings
  `settings` TEXT COMMENT '用户设置JSON',
  `preferences` TEXT COMMENT '偏好设置JSON',
  
  -- Stats
  `total_projects` INT NOT NULL DEFAULT 0 COMMENT '总项目数',
  `total_novels` INT NOT NULL DEFAULT 0 COMMENT '总小说数',
  `total_words` INT NOT NULL DEFAULT 0 COMMENT '总字数',
  
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `last_login_at` DATETIME COMMENT '最后登录时间',
  
  INDEX `idx_email` (`email`),
  INDEX `idx_phone` (`phone`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户表';

-- ============================================
-- Part 2: Novel & Chapter Tables
-- ============================================

-- Novel table (小说)
DROP TABLE IF EXISTS `ink_novel`;
CREATE TABLE `ink_novel` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID',
  `project_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '项目ID',
  
  `title` VARCHAR(255) NOT NULL COMMENT '小说标题',
  `description` TEXT COMMENT '小说描述',
  `genre` VARCHAR(50) NOT NULL COMMENT '类型: fantasy/xianxia/urban/scifi/romance/mystery/historical',
  `status` VARCHAR(20) NOT NULL DEFAULT 'planning' COMMENT '状态: planning/writing/paused/completed/archived',
  
  -- Stats
  `total_words` INT NOT NULL DEFAULT 0 COMMENT '总字数',
  `chapter_count` INT NOT NULL DEFAULT 0 COMMENT '章节数',
  `view_count` INT NOT NULL DEFAULT 0 COMMENT '浏览数',
  `like_count` INT NOT NULL DEFAULT 0 COMMENT '点赞数',
  
  -- Relations
  `worldview_id` BIGINT UNSIGNED COMMENT '世界观ID',
  `cover_image` VARCHAR(500) COMMENT '封面图片URL',
  
  -- AI Config
  `ai_model` VARCHAR(100) COMMENT 'AI模型',
  `temperature` DECIMAL(3,2) DEFAULT 0.70 COMMENT '温度参数',
  `max_tokens` INT DEFAULT 4096 COMMENT '最大token数',
  `style_prompt` TEXT COMMENT '风格提示词',
  
  -- Visibility
  `is_public` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否公开',
  `access_code` VARCHAR(100) COMMENT '访问密码',
  
  -- Storage
  `storage_size` BIGINT NOT NULL DEFAULT 0 COMMENT '存储大小字节',
  
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` DATETIME COMMENT '软删除时间',
  
  INDEX `idx_uuid` (`uuid`),
  INDEX `idx_tenant_id` (`tenant_id`),
  INDEX `idx_project_id` (`project_id`),
  INDEX `idx_genre` (`genre`),
  INDEX `idx_status` (`status`),
  INDEX `idx_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='小说表';

-- Chapter table (章节)
DROP TABLE IF EXISTS `ink_chapter`;
CREATE TABLE `ink_chapter` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID',
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `chapter_no` INT NOT NULL COMMENT '章节号',
  `title` VARCHAR(255) COMMENT '章节标题',
  `content` LONGTEXT COMMENT '章节内容',
  `summary` TEXT COMMENT '章节摘要',
  `word_count` INT NOT NULL DEFAULT 0 COMMENT '字数',
  `outline` TEXT COMMENT '章节大纲',
  `plot_points` TEXT COMMENT '剧情点JSON',
  `status` VARCHAR(20) NOT NULL DEFAULT 'draft' COMMENT '状态: draft/generating/completed/published',
  `previous_chapter_id` BIGINT UNSIGNED COMMENT '上一章ID',
  `next_chapter_id` BIGINT UNSIGNED COMMENT '下一章ID',
  `quality_score` DECIMAL(5,4) COMMENT '质量评分',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `published_at` DATETIME COMMENT '发布时间',
  
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_tenant_id` (`tenant_id`),
  INDEX `idx_chapter_no` (`chapter_no`),
  UNIQUE KEY `uk_novel_chapter` (`novel_id`, `chapter_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='章节表';

-- PlotPoint table (剧情点)
DROP TABLE IF EXISTS `ink_plot_point`;
CREATE TABLE `ink_plot_point` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: conflict/climax/resolution/twist/foreshadow',
  `description` TEXT NOT NULL COMMENT '描述',
  `characters` TEXT COMMENT '涉及角色JSON',
  `locations` TEXT COMMENT '涉及地点JSON',
  `is_resolved` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否已解决',
  `resolved_in` BIGINT UNSIGNED COMMENT '解决此剧情点的章节ID',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_chapter_id` (`chapter_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='剧情点表';

-- ChapterVersion table (章节版本)
DROP TABLE IF EXISTS `ink_chapter_version`;
CREATE TABLE `ink_chapter_version` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `version_no` INT NOT NULL COMMENT '版本号',
  `content` LONGTEXT COMMENT '版本内容',
  `summary` TEXT COMMENT '版本摘要',
  `word_count` INT NOT NULL DEFAULT 0 COMMENT '字数',
  `created_by` BIGINT UNSIGNED COMMENT '创建人',
  `change_summary` VARCHAR(500) COMMENT '变更说明',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_chapter_id` (`chapter_id`),
  UNIQUE KEY `uk_chapter_version` (`chapter_id`, `version_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='章节版本表';

-- ============================================
-- Part 3: Character Tables
-- ============================================

-- Character table (角色)
DROP TABLE IF EXISTS `ink_character`;
CREATE TABLE `ink_character` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID',
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `name` VARCHAR(100) NOT NULL COMMENT '角色名',
  `role` VARCHAR(50) NOT NULL COMMENT '角色类型: protagonist/antagonist/supporting/minor',
  `archetype` VARCHAR(100) COMMENT '角色原型',
  `appearance` TEXT COMMENT '外貌描述',
  `personality` TEXT COMMENT '性格特点',
  `background` TEXT COMMENT '背景故事',
  `abilities` TEXT COMMENT '能力JSON数组',
  `character_arc` TEXT COMMENT '角色弧光',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_tenant_id` (`tenant_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色表';

-- CharacterAppearance table (角色外貌变体)
DROP TABLE IF EXISTS `ink_character_appearance`;
CREATE TABLE `ink_character_appearance` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `character_id` BIGINT UNSIGNED NOT NULL COMMENT '角色ID',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: portrait/expression/pose/costume',
  `name` VARCHAR(100) NOT NULL COMMENT '变体名称',
  `description` TEXT COMMENT '描述',
  `image_url` VARCHAR(500) COMMENT '图片URL',
  `emotion` VARCHAR(50) COMMENT '表情',
  `pose` VARCHAR(100) COMMENT '姿态',
  `scene` VARCHAR(100) COMMENT '场景',
  `lighting` VARCHAR(50) COMMENT '灯光',
  `style` VARCHAR(50) COMMENT '风格',
  `lora_model_id` VARCHAR(100) COMMENT 'LoRA模型ID',
  `is_default` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否默认',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_character_id` (`character_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色外貌变体表';

-- CharacterStateSnapshot table (角色状态快照)
DROP TABLE IF EXISTS `ink_character_state_snapshot`;
CREATE TABLE `ink_character_state_snapshot` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `character_id` BIGINT UNSIGNED NOT NULL COMMENT '角色ID',
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `chapter_no` INT COMMENT '章节号',
  `power_level` INT COMMENT '能力等级',
  `mood` VARCHAR(50) COMMENT '情绪状态',
  `physical_state` TEXT COMMENT '身体状态',
  `mental_state` TEXT COMMENT '心理状态',
  `relationships` TEXT COMMENT '关系变化JSON',
  `achievements` TEXT COMMENT '成就JSON',
  `location` VARCHAR(100) COMMENT '当前位置',
  `status` TEXT COMMENT '详细状态JSON',
  `snapshot_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_character_id` (`character_id`),
  INDEX `idx_chapter_id` (`chapter_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色状态快照表';

-- ============================================
-- Part 4: Worldview Tables
-- ============================================

-- Worldview table (世界观)
DROP TABLE IF EXISTS `ink_worldview`;
CREATE TABLE `ink_worldview` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `name` VARCHAR(100) NOT NULL COMMENT '世界观名称',
  `genre` VARCHAR(50) NOT NULL COMMENT '类型: fantasy/xianxia/urban/scifi',
  `description` TEXT COMMENT '描述',
  `magic_system` TEXT COMMENT '修炼/魔法体系',
  `geography` TEXT COMMENT '地理环境',
  `history` TEXT COMMENT '历史背景',
  `culture` TEXT COMMENT '文化设定',
  `technology` TEXT COMMENT '科技水平',
  `rules` TEXT COMMENT '世界规则',
  `status` VARCHAR(20) NOT NULL DEFAULT 'draft' COMMENT '状态',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_novel_id` (`novel_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='世界观表';

-- WorldviewEntity table (世界观实体)
DROP TABLE IF EXISTS `ink_worldview_entity`;
CREATE TABLE `ink_worldview_entity` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `worldview_id` BIGINT UNSIGNED NOT NULL COMMENT '世界观ID',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: sect/organization/country/religion',
  `name` VARCHAR(100) NOT NULL COMMENT '名称',
  `description` TEXT COMMENT '描述',
  `power_level` INT COMMENT '势力等级',
  `leader` VARCHAR(100) COMMENT '领袖',
  `location` VARCHAR(200) COMMENT '所在地',
  `history` TEXT COMMENT '历史',
  `relationships` TEXT COMMENT '关系JSON',
  `founded_at` VARCHAR(50) COMMENT '创立时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_worldview_id` (`worldview_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='世界观实体表';

-- ============================================
-- Part 5: Video Tables
-- ============================================

-- Video table (视频)
DROP TABLE IF EXISTS `ink_video`;
CREATE TABLE `ink_video` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `title` VARCHAR(255) NOT NULL COMMENT '标题',
  `description` TEXT COMMENT '描述',
  `type` VARCHAR(50) NOT NULL DEFAULT 'image_sequence' COMMENT '类型: image_sequence/real_video',
  `status` VARCHAR(50) NOT NULL DEFAULT 'planning' COMMENT '状态',
  `resolution` VARCHAR(20) NOT NULL DEFAULT '1080p' COMMENT '分辨率',
  `frame_rate` INT NOT NULL DEFAULT 24 COMMENT '帧率',
  `aspect_ratio` VARCHAR(10) NOT NULL DEFAULT '16:9' COMMENT '宽高比',
  `art_style` VARCHAR(50) COMMENT '艺术风格',
  `total_shots` INT NOT NULL DEFAULT 0 COMMENT '总镜头数',
  `generated_shots` INT NOT NULL DEFAULT 0 COMMENT '已生成镜头数',
  `video_url` VARCHAR(500) COMMENT '视频URL',
  `thumbnail_url` VARCHAR(500) COMMENT '缩略图URL',
  `duration` INT COMMENT '时长(秒)',
  `file_size` BIGINT COMMENT '文件大小(字节)',
  `error_message` TEXT COMMENT '错误信息',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_chapter_id` (`chapter_id`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频表';

-- StoryboardShot table (分镜)
DROP TABLE IF EXISTS `ink_storyboard_shot`;
CREATE TABLE `ink_storyboard_shot` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `video_id` BIGINT UNSIGNED NOT NULL COMMENT '视频ID',
  `shot_no` INT NOT NULL COMMENT '镜头编号',
  `description` TEXT COMMENT '镜头描述',
  `dialogue` TEXT COMMENT '对话内容',
  `shot_type` VARCHAR(50) COMMENT '镜头类型: wide/medium/close_up/extreme',
  `shot_angle` VARCHAR(50) COMMENT '镜头角度: eye_level/low/high/dutch',
  `camera_movement` VARCHAR(50) COMMENT '摄像机运动: static/pan/tilt/zoom/dolly',
  `duration` DECIMAL(5,2) COMMENT '时长(秒)',
  `characters` TEXT COMMENT '角色列表JSON',
  `location` VARCHAR(100) COMMENT '地点',
  `time_of_day` VARCHAR(50) COMMENT '时间段',
  `emotion` VARCHAR(50) COMMENT '情感',
  `lighting` VARCHAR(50) COMMENT '灯光',
  `prompt` TEXT COMMENT '生成提示词',
  `negative_prompt` TEXT COMMENT '负面提示词',
  `image_url` VARCHAR(500) COMMENT '生成图片URL',
  `status` VARCHAR(50) NOT NULL DEFAULT 'pending' COMMENT '状态',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_video_id` (`video_id`),
  UNIQUE KEY `uk_video_shot` (`video_id`, `shot_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='分镜表';

-- CharacterVisualDesign table (角色视觉设计)
DROP TABLE IF EXISTS `ink_character_visual_design`;
CREATE TABLE `ink_character_visual_design` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `character_id` BIGINT UNSIGNED NOT NULL COMMENT '角色ID',
  `name` VARCHAR(100) NOT NULL COMMENT '设计名称',
  `base_image_url` VARCHAR(500) COMMENT '基础图片URL',
  `lora_model_id` VARCHAR(100) COMMENT 'LoRA模型ID',
  `lora_weight` DECIMAL(3,2) DEFAULT 0.8 COMMENT 'LoRA权重',
  `style` VARCHAR(50) COMMENT '风格',
  `color_palette` TEXT COMMENT '色彩方案JSON',
  `settings` TEXT COMMENT '其他设置JSON',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_character_id` (`character_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色视觉设计表';

-- SceneVisualDesign table (场景视觉设计)
DROP TABLE IF EXISTS `ink_scene_visual_design`;
CREATE TABLE `ink_scene_visual_design` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `worldview_id` BIGINT UNSIGNED NOT NULL COMMENT '世界观ID',
  `name` VARCHAR(100) NOT NULL COMMENT '场景名称',
  `description` TEXT COMMENT '描述',
  `location` VARCHAR(200) COMMENT '位置',
  `time_period` VARCHAR(50) COMMENT '时代',
  `base_image_url` VARCHAR(500) COMMENT '基础图片URL',
  `lora_model_id` VARCHAR(100) COMMENT 'LoRA模型ID',
  `lora_weight` DECIMAL(3,2) DEFAULT 0.8 COMMENT 'LoRA权重',
  `lighting_style` VARCHAR(50) COMMENT '灯光风格',
  `color_grade` VARCHAR(50) COMMENT '色调',
  `atmosphere` TEXT COMMENT '氛围描述',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_worldview_id` (`worldview_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='场景视觉设计表';

-- ============================================
-- Part 6: Model Management Tables
-- ============================================

-- ModelProvider table (模型提供商)
DROP TABLE IF EXISTS `ink_model_provider`;
CREATE TABLE `ink_model_provider` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '提供商名称',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: openai/anthropic/google/local',
  `endpoint` VARCHAR(500) COMMENT 'API端点',
  `api_key_encrypted` VARCHAR(500) COMMENT '加密的API密钥',
  `health_status` VARCHAR(20) NOT NULL DEFAULT 'unknown' COMMENT '健康状态',
  `health_check_url` VARCHAR(500) COMMENT '健康检查URL',
  `is_enabled` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
  `settings` TEXT COMMENT '其他设置JSON',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_type` (`type`),
  INDEX `idx_health_status` (`health_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模型提供商表';

-- AIModel table (AI模型)
DROP TABLE IF EXISTS `ink_ai_model`;
CREATE TABLE `ink_ai_model` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `provider_id` BIGINT UNSIGNED NOT NULL COMMENT '提供商ID',
  `name` VARCHAR(100) NOT NULL COMMENT '模型名称',
  `display_name` VARCHAR(100) COMMENT '显示名称',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: chat/image/video',
  `context_window` INT COMMENT '上下文窗口大小',
  `max_output_tokens` INT COMMENT '最大输出token',
  `quality` DECIMAL(3,2) COMMENT '质量评分(0-1)',
  `cost_per_1k` DECIMAL(10,6) COMMENT '每1K token成本',
  `latency_ms` INT COMMENT '延迟(毫秒)',
  `is_active` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
  `is_default` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否默认',
  `settings` TEXT COMMENT '其他设置JSON',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_provider_id` (`provider_id`),
  INDEX `idx_type` (`type`),
  INDEX `idx_is_active` (`is_active`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='AI模型表';

-- TaskModelConfig table (任务模型配置)
DROP TABLE IF EXISTS `ink_task_model_config`;
CREATE TABLE `ink_task_model_config` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `task` VARCHAR(50) NOT NULL COMMENT '任务类型',
  `strategy` VARCHAR(50) NOT NULL DEFAULT 'balanced' COMMENT '选择策略',
  `temperature` DECIMAL(3,2) DEFAULT 0.7 COMMENT '温度参数',
  `max_tokens` INT DEFAULT 4096 COMMENT '最大token',
  `primary_model_id` BIGINT UNSIGNED COMMENT '主要模型ID',
  `fallback_model_ids` TEXT COMMENT '备用模型ID列表JSON',
  `settings` TEXT COMMENT '其他设置JSON',
  `is_active` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_task` (`task`),
  UNIQUE KEY `uk_task` (`task`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='任务模型配置表';

-- ModelUsageLog table (模型使用日志)
DROP TABLE IF EXISTS `ink_model_usage_log`;
CREATE TABLE `ink_model_usage_log` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `model_id` BIGINT UNSIGNED NOT NULL COMMENT '模型ID',
  `novel_id` BIGINT UNSIGNED COMMENT '小说ID',
  `user_id` BIGINT UNSIGNED COMMENT '用户ID',
  `input_tokens` INT NOT NULL DEFAULT 0 COMMENT '输入token数',
  `output_tokens` INT NOT NULL DEFAULT 0 COMMENT '输出token数',
  `total_cost` DECIMAL(10,4) COMMENT '总成本',
  `latency_ms` INT COMMENT '延迟(毫秒)',
  `status` VARCHAR(20) NOT NULL DEFAULT 'success' COMMENT '状态',
  `error_message` TEXT COMMENT '错误信息',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_model_id` (`model_id`),
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模型使用日志表';

-- ModelComparisonExperiment table (模型对比实验)
DROP TABLE IF EXISTS `ink_model_comparison_experiment`;
CREATE TABLE `ink_model_comparison_experiment` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(200) NOT NULL COMMENT '实验名称',
  `description` TEXT COMMENT '描述',
  `task_type` VARCHAR(50) NOT NULL COMMENT '任务类型',
  `model_ids` TEXT NOT NULL COMMENT '对比的模型ID列表JSON',
  `test_prompts` TEXT COMMENT '测试提示词JSON',
  `results` TEXT COMMENT '实验结果JSON',
  `winner_model_id` BIGINT UNSIGNED COMMENT '获胜模型ID',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态: pending/running/completed/failed',
  `conclusion` TEXT COMMENT '结论',
  `created_by` BIGINT UNSIGNED COMMENT '创建人',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_task_type` (`task_type`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模型对比实验表';

-- ExperimentResult table (实验结果)
DROP TABLE IF EXISTS `ink_experiment_result`;
CREATE TABLE `ink_experiment_result` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `experiment_id` BIGINT UNSIGNED NOT NULL COMMENT '实验ID',
  `model_id` BIGINT UNSIGNED NOT NULL COMMENT '模型ID',
  `metric_name` VARCHAR(100) NOT NULL COMMENT '指标名称',
  `metric_value` DECIMAL(10,4) NOT NULL COMMENT '指标值',
  `rank` INT COMMENT '排名',
  `response_content` TEXT COMMENT '响应内容',
  `latency_ms` INT COMMENT '延迟(毫秒)',
  `cost` DECIMAL(10,4) COMMENT '成本',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_experiment_id` (`experiment_id`),
  INDEX `idx_model_id` (`model_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='实验结果表';

-- ============================================
-- Part 7: Quality & Review Tables
-- ============================================

-- QualityReport table (质量报告)
DROP TABLE IF EXISTS `ink_quality_report`;
CREATE TABLE `ink_quality_report` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `overall_score` DECIMAL(5,4) COMMENT '总体评分',
  `consistency_score` DECIMAL(5,4) COMMENT '一致性评分',
  `quality_score` DECIMAL(5,4) COMMENT '质量评分',
  `logic_score` DECIMAL(5,4) COMMENT '逻辑评分',
  `style_score` DECIMAL(5,4) COMMENT '风格评分',
  `issues` TEXT COMMENT '问题JSON',
  `suggestions` TEXT COMMENT '建议JSON',
  `checked_by` VARCHAR(50) COMMENT '检查者: ai/human',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_chapter_id` (`chapter_id`),
  INDEX `idx_novel_id` (`novel_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='质量报告表';

-- ReviewTask table (审核任务)
DROP TABLE IF EXISTS `ink_review_task`;
CREATE TABLE `ink_review_task` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `assignee_id` BIGINT UNSIGNED COMMENT '审核人ID',
  `type` VARCHAR(50) NOT NULL COMMENT '审核类型',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态',
  `priority` INT NOT NULL DEFAULT 5 COMMENT '优先级(1-5)',
  `review_notes` TEXT COMMENT '审核备注',
  `result` VARCHAR(20) COMMENT '审核结果: approved/rejected',
  `result_notes` TEXT COMMENT '结果备注',
  `reviewed_at` DATETIME COMMENT '审核时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_chapter_id` (`chapter_id`),
  INDEX `idx_assignee_id` (`assignee_id`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='审核任务表';

-- FeedbackRecord table (反馈记录)
DROP TABLE IF EXISTS `ink_feedback_record`;
CREATE TABLE `ink_feedback_record` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `novel_id` BIGINT UNSIGNED COMMENT '小说ID',
  `user_id` BIGINT UNSIGNED COMMENT '用户ID',
  `type` VARCHAR(50) NOT NULL COMMENT '反馈类型',
  `content` TEXT NOT NULL COMMENT '反馈内容',
  `position` VARCHAR(100) COMMENT '位置信息',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态',
  `response` TEXT COMMENT '回复',
  `resolved_at` DATETIME COMMENT '解决时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_chapter_id` (`chapter_id`),
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='反馈记录表';

-- ============================================
-- Part 8: Knowledge & Reference Tables
-- ============================================

-- KnowledgeBase table (知识库)
DROP TABLE IF EXISTS `ink_knowledge_base`;
CREATE TABLE `ink_knowledge_base` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID',
  `name` VARCHAR(100) NOT NULL COMMENT '知识库名称',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: lore/worldbuilding/character',
  `content` TEXT COMMENT '内容',
  `source` VARCHAR(100) COMMENT '来源',
  `tags` TEXT COMMENT '标签JSON',
  `embedding` TEXT COMMENT '向量嵌入',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_tenant_id` (`tenant_id`),
  INDEX `idx_type` (`type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='知识库表';

-- PromptTemplate table (提示词模板)
DROP TABLE IF EXISTS `ink_prompt_template`;
CREATE TABLE `ink_prompt_template` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '模板名称',
  `type` VARCHAR(50) NOT NULL COMMENT '类型: outline/dialogue/description',
  `template` TEXT NOT NULL COMMENT '模板内容',
  `variables` TEXT COMMENT '变量定义JSON',
  `description` TEXT COMMENT '描述',
  `is_public` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否公开',
  `created_by` BIGINT UNSIGNED COMMENT '创建人',
  `usage_count` INT NOT NULL DEFAULT 0 COMMENT '使用次数',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  
  INDEX `idx_type` (`type`),
  INDEX `idx_is_public` (`is_public`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='提示词模板表';

-- ReferenceNovel table (参考小说)
DROP TABLE IF EXISTS `ink_reference_novel`;
CREATE TABLE `ink_reference_novel` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `title` VARCHAR(255) NOT NULL COMMENT '参考小说标题',
  `author` VARCHAR(100) COMMENT '作者',
  `url` VARCHAR(500) COMMENT 'URL',
  `genre` VARCHAR(50) COMMENT '类型',
  `description` TEXT COMMENT '描述',
  `relevance` DECIMAL(3,2) COMMENT '相关度',
  `notes` TEXT COMMENT '备注',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_novel_id` (`novel_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='参考小说表';

-- ReferenceChapter table (参考章节)
DROP TABLE IF EXISTS `ink_reference_chapter`;
CREATE TABLE `ink_reference_chapter` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `reference_novel_id` BIGINT UNSIGNED NOT NULL COMMENT '参考小说ID',
  `chapter_no` INT COMMENT '章节号',
  `title` VARCHAR(255) COMMENT '章节标题',
  `summary` TEXT COMMENT '摘要',
  `content_snippet` TEXT COMMENT '内容片段',
  `relevance` DECIMAL(3,2) COMMENT '相关度',
  `notes` TEXT COMMENT '备注',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  
  INDEX `idx_reference_novel_id` (`reference_novel_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='参考章节表';

-- ============================================
-- Foreign Key Constraints
-- ============================================

ALTER TABLE `ink_novel` ADD CONSTRAINT `fk_novel_worldview` FOREIGN KEY (`worldview_id`) REFERENCES `ink_worldview`(`id`) ON DELETE SET NULL;
ALTER TABLE `ink_chapter` ADD CONSTRAINT `fk_chapter_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_plot_point` ADD CONSTRAINT `fk_plotpoint_chapter` FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_chapter_version` ADD CONSTRAINT `fk_version_chapter` FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_character` ADD CONSTRAINT `fk_character_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_character_appearance` ADD CONSTRAINT `fk_appearance_character` FOREIGN KEY (`character_id`) REFERENCES `ink_character`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_character_state_snapshot` ADD CONSTRAINT `fk_snapshot_character` FOREIGN KEY (`character_id`) REFERENCES `ink_character`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_worldview` ADD CONSTRAINT `fk_worldview_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_worldview_entity` ADD CONSTRAINT `fk_entity_worldview` FOREIGN KEY (`worldview_id`) REFERENCES `ink_worldview`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_video` ADD CONSTRAINT `fk_video_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_storyboard_shot` ADD CONSTRAINT `fk_shot_video` FOREIGN KEY (`video_id`) REFERENCES `ink_video`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_character_visual_design` ADD CONSTRAINT `fk_design_character` FOREIGN KEY (`character_id`) REFERENCES `ink_character`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_scene_visual_design` ADD CONSTRAINT `fk_design_worldview` FOREIGN KEY (`worldview_id`) REFERENCES `ink_worldview`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_ai_model` ADD CONSTRAINT `fk_model_provider` FOREIGN KEY (`provider_id`) REFERENCES `ink_model_provider`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_quality_report` ADD CONSTRAINT `fk_report_chapter` FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_quality_report` ADD CONSTRAINT `fk_report_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_review_task` ADD CONSTRAINT `fk_task_chapter` FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_feedback_record` ADD CONSTRAINT `fk_feedback_chapter` FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_feedback_record` ADD CONSTRAINT `fk_feedback_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_knowledge_base` ADD CONSTRAINT `fk_kb_novel` FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE;
ALTER TABLE `ink_reference_chapter` ADD CONSTRAINT `fk_refchapter_novel` FOREIGN KEY (`reference_novel_id`) REFERENCES `ink_reference_novel`(`id`) ON DELETE CASCADE;

SET FOREIGN_KEY_CHECKS = 1;

-- ============================================
-- Default Data
-- ============================================

-- Insert default task configurations
INSERT INTO `ink_task_model_config` (`task`, `strategy`, `temperature`, `max_tokens`, `is_active`) VALUES
('outline_generation', 'balanced', 0.7, 2048, 1),
('chapter_generation', 'quality_first', 0.8, 8192, 1),
('dialogue_generation', 'balanced', 0.75, 2048, 1),
('description_generation', 'quality_first', 0.7, 4096, 1),
('worldview_generation', 'quality_first', 0.8, 8192, 1),
('character_generation', 'balanced', 0.75, 4096, 1),
('storyboard_generation', 'balanced', 0.7, 4096, 1);

-- ============================================
-- Migration Complete
-- ============================================
-- Total Tables: 30
-- Total Lines: ~667
-- Generated: 2026-04-19
-- ============================================
