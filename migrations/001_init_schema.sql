-- InkFrame Database Migration Script
-- Version: 001_init_schema
-- Description: Initial database schema for InkFrame

-- ============================================
-- Create Database (run this separately if needed)
-- CREATE DATABASE IF NOT EXISTS inkframe DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
-- USE inkframe;

-- ============================================
-- Novel related tables
-- ============================================

-- Novel table
CREATE TABLE IF NOT EXISTS `ink_novel` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `title` VARCHAR(255) NOT NULL COMMENT '小说标题',
  `description` TEXT COMMENT '小说描述',
  `genre` VARCHAR(50) NOT NULL COMMENT '类型: fantasy/xianxia/urban/scifi/romance/mystery/historical',
  `status` VARCHAR(20) NOT NULL DEFAULT 'planning' COMMENT '状态: planning/writing/paused/completed/archived',
  `total_words` INT NOT NULL DEFAULT 0 COMMENT '总字数',
  `chapter_count` INT NOT NULL DEFAULT 0 COMMENT '章节数',
  `worldview_id` BIGINT UNSIGNED COMMENT '世界观ID',
  `cover_image` VARCHAR(500) COMMENT '封面图片URL',
  `ai_model` VARCHAR(100) COMMENT 'AI模型',
  `temperature` DECIMAL(3,2) DEFAULT 0.70 COMMENT '温度参数',
  `max_tokens` INT DEFAULT 4096 COMMENT '最大token数',
  `style_prompt` TEXT COMMENT '风格提示词',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` DATETIME COMMENT '软删除时间',
  INDEX `idx_genre` (`genre`),
  INDEX `idx_status` (`status`),
  INDEX `idx_deleted_at` (`deleted_at`),
  FOREIGN KEY (`worldview_id`) REFERENCES `ink_worldview`(`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='小说表';

-- Chapter table
CREATE TABLE IF NOT EXISTS `ink_chapter` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
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
  INDEX `idx_chapter_no` (`chapter_no`),
  UNIQUE KEY `uk_novel_chapter` (`novel_id`, `chapter_no`),
  FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='章节表';

-- PlotPoint table
CREATE TABLE IF NOT EXISTS `ink_plot_point` (
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
  INDEX `idx_chapter_id` (`chapter_id`),
  FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='剧情点表';

-- ============================================
-- Character related tables
-- ============================================

-- Character table
CREATE TABLE IF NOT EXISTS `ink_character` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
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
  FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色表';

-- CharacterAppearance table
CREATE TABLE IF NOT EXISTS `ink_character_appearance` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `character_id` BIGINT UNSIGNED NOT NULL COMMENT '角色ID',
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `role_in_chapter` VARCHAR(50) NOT NULL DEFAULT 'mentioned' COMMENT '在章节中的角色: main/supporting/mentioned',
  `first_appearance` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否首次出场',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_character_id` (`character_id`),
  INDEX `idx_chapter_id` (`chapter_id`),
  FOREIGN KEY (`character_id`) REFERENCES `ink_character`(`id`) ON DELETE CASCADE,
  FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色出场记录表';

-- CharacterStateSnapshot table
CREATE TABLE IF NOT EXISTS `ink_character_state_snapshot` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `character_id` BIGINT UNSIGNED NOT NULL COMMENT '角色ID',
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `age` DECIMAL(5,2) COMMENT '年龄',
  `height` DECIMAL(5,2) COMMENT '身高(米)',
  `weight` DECIMAL(5,2) COMMENT '体重(公斤)',
  `health` VARCHAR(50) COMMENT '健康状态: healthy/injured/critical',
  `injuries` TEXT COMMENT '伤势JSON',
  `power_level` INT COMMENT '能力等级',
  `abilities` TEXT COMMENT '能力状态JSON',
  `equipment` TEXT COMMENT '装备JSON',
  `mood` VARCHAR(100) COMMENT '情绪状态',
  `motivation` VARCHAR(255) COMMENT '当前动机',
  `goals` TEXT COMMENT '目标JSON',
  `fears` TEXT COMMENT '恐惧JSON',
  `location` VARCHAR(255) COMMENT '当前位置',
  `known_locations` TEXT COMMENT '已知地点JSON',
  `relations` TEXT COMMENT '关系状态JSON',
  `snapshot_time` DATETIME NOT NULL COMMENT '快照时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_character_id` (`character_id`),
  INDEX `idx_chapter_id` (`chapter_id`),
  FOREIGN KEY (`character_id`) REFERENCES `ink_character`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色状态快照表';

-- ============================================
-- Worldview related tables
-- ============================================

-- Worldview table
CREATE TABLE IF NOT EXISTS `ink_worldview` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '世界观名称',
  `description` TEXT COMMENT '世界观描述',
  `genre` VARCHAR(50) NOT NULL COMMENT '类型',
  `magic_system` TEXT COMMENT '魔法/修炼体系',
  `geography` TEXT COMMENT '地理环境',
  `history` TEXT COMMENT '历史背景',
  `culture` TEXT COMMENT '文化设定',
  `technology` TEXT COMMENT '科技水平',
  `rules` TEXT COMMENT '世界规则限制',
  `used_count` INT NOT NULL DEFAULT 0 COMMENT '使用次数',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_genre` (`genre`),
  INDEX `idx_used_count` (`used_count`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='世界观表';

-- WorldviewEntity table
CREATE TABLE IF NOT EXISTS `ink_worldview_entity` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `worldview_id` BIGINT UNSIGNED NOT NULL COMMENT '世界观ID',
  `type` VARCHAR(50) NOT NULL COMMENT '实体类型: location/organization/artifact/race/other',
  `name` VARCHAR(100) NOT NULL COMMENT '实体名称',
  `description` TEXT COMMENT '实体描述',
  `attributes` JSON COMMENT '属性JSON',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_worldview_id` (`worldview_id`),
  INDEX `idx_type` (`type`),
  FOREIGN KEY (`worldview_id`) REFERENCES `ink_worldview`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='世界观实体表';

-- ============================================
-- Video related tables
-- ============================================

-- Video table
CREATE TABLE IF NOT EXISTS `ink_video` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `title` VARCHAR(255) NOT NULL COMMENT '视频标题',
  `status` VARCHAR(20) NOT NULL DEFAULT 'planning' COMMENT '状态: planning/generating/completed/failed',
  `frame_rate` INT NOT NULL DEFAULT 24 COMMENT '帧率',
  `resolution` VARCHAR(20) NOT NULL DEFAULT '1080p' COMMENT '分辨率',
  `aspect_ratio` VARCHAR(10) NOT NULL DEFAULT '16:9' COMMENT '宽高比',
  `total_shots` INT NOT NULL DEFAULT 0 COMMENT '总镜头数',
  `url` VARCHAR(500) COMMENT '视频URL',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_chapter_id` (`chapter_id`),
  FOREIGN KEY (`novel_id`) REFERENCES `ink_novel`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频表';

-- StoryboardShot table
CREATE TABLE IF NOT EXISTS `ink_storyboard_shot` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `video_id` BIGINT UNSIGNED NOT NULL COMMENT '视频ID',
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `shot_no` INT NOT NULL COMMENT '镜头编号',
  `description` TEXT COMMENT '镜头描述',
  `dialogue` TEXT COMMENT '对话内容',
  `camera_type` VARCHAR(50) NOT NULL DEFAULT 'static' COMMENT '相机类型: static/pan/zoom/tracking/dolly/crane',
  `camera_angle` VARCHAR(50) NOT NULL DEFAULT 'eye_level' COMMENT '相机角度',
  `shot_size` VARCHAR(50) NOT NULL DEFAULT 'medium' COMMENT '镜头尺寸',
  `duration` DECIMAL(5,2) NOT NULL DEFAULT 5.0 COMMENT '时长(秒)',
  `character_configs` JSON COMMENT '角色配置JSON',
  `scene_config` JSON COMMENT '场景配置JSON',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态: pending/generating/completed/failed',
  `image_url` VARCHAR(500) COMMENT '生成图片URL',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_video_id` (`video_id`),
  UNIQUE KEY `uk_video_shot` (`video_id`, `shot_no`),
  FOREIGN KEY (`video_id`) REFERENCES `ink_video`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='分镜表';

-- ============================================
-- AI Model related tables
-- ============================================

-- ModelProvider table
CREATE TABLE IF NOT EXISTS `ink_model_provider` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '提供商名称',
  `type` VARCHAR(50) NOT NULL COMMENT '提供商类型: openai/anthropic/google/alibaba/local',
  `endpoint` VARCHAR(255) COMMENT 'API端点',
  `api_key` VARCHAR(255) COMMENT 'API密钥(加密存储)',
  `health_status` VARCHAR(20) NOT NULL DEFAULT 'healthy' COMMENT '健康状态: healthy/degraded/down',
  `health_check` VARCHAR(255) COMMENT '健康检查结果',
  `last_checked` DATETIME COMMENT '最后检查时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='AI模型提供商表';

-- AIModel table
CREATE TABLE IF NOT EXISTS `ink_ai_model` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `provider_id` BIGINT UNSIGNED NOT NULL COMMENT '提供商ID',
  `name` VARCHAR(100) NOT NULL COMMENT '模型名称',
  `display_name` VARCHAR(100) NOT NULL COMMENT '显示名称',
  `description` TEXT COMMENT '模型描述',
  `is_active` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
  `is_available` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否可用',
  `quality` DECIMAL(5,4) NOT NULL DEFAULT 0.5 COMMENT '质量评分0-1',
  `cost_per_1k` DECIMAL(10,6) NOT NULL DEFAULT 0 COMMENT '每1K token成本(美元)',
  `context_window` INT NOT NULL DEFAULT 4096 COMMENT '上下文窗口大小',
  `suitable_tasks` TEXT COMMENT '适合任务JSON数组',
  `config` JSON COMMENT '额外配置JSON',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_provider_id` (`provider_id`),
  FOREIGN KEY (`provider_id`) REFERENCES `ink_model_provider`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='AI模型表';

-- TaskModelConfig table
CREATE TABLE IF NOT EXISTS `ink_task_model_config` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `task_type` VARCHAR(50) NOT NULL COMMENT '任务类型: outline/chapter/dialogue/etc',
  `primary_model_id` BIGINT UNSIGNED NOT NULL COMMENT '主模型ID',
  `fallback_model_ids` TEXT COMMENT '备选模型ID JSON数组',
  `temperature` DECIMAL(3,2) NOT NULL DEFAULT 0.7 COMMENT '温度参数',
  `max_tokens` INT NOT NULL DEFAULT 4096 COMMENT '最大token数',
  `strategy` VARCHAR(50) NOT NULL DEFAULT 'balanced' COMMENT '选择策略: quality_first/cost_first/balanced/custom',
  `is_active` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_task_type` (`task_type`),
  FOREIGN KEY (`primary_model_id`) REFERENCES `ink_ai_model`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='任务模型配置表';

-- ModelUsageLog table
CREATE TABLE IF NOT EXISTS `ink_model_usage_log` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `model_id` BIGINT UNSIGNED NOT NULL COMMENT '模型ID',
  `task_type` VARCHAR(50) COMMENT '任务类型',
  `novel_id` BIGINT UNSIGNED COMMENT '小说ID',
  `input_tokens` INT NOT NULL DEFAULT 0 COMMENT '输入token数',
  `output_tokens` INT NOT NULL DEFAULT 0 COMMENT '输出token数',
  `total_tokens` INT NOT NULL DEFAULT 0 COMMENT '总token数',
  `cost` DECIMAL(10,6) NOT NULL DEFAULT 0 COMMENT '成本(美元)',
  `latency` DECIMAL(10,2) NOT NULL DEFAULT 0 COMMENT '延迟(毫秒)',
  `success` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否成功',
  `error_message` TEXT COMMENT '错误信息',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_model_id` (`model_id`),
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_created_at` (`created_at`),
  FOREIGN KEY (`model_id`) REFERENCES `ink_ai_model`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模型使用日志表';

-- ============================================
-- Quality and Review tables
-- ============================================

-- QualityReport table
CREATE TABLE IF NOT EXISTS `ink_quality_report` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `overall_score` DECIMAL(5,4) NOT NULL COMMENT '整体评分',
  `consistency_score` DECIMAL(5,4) NOT NULL COMMENT '一致性评分',
  `quality_score` DECIMAL(5,4) NOT NULL COMMENT '质量评分',
  `logic_score` DECIMAL(5,4) NOT NULL COMMENT '逻辑评分',
  `style_score` DECIMAL(5,4) NOT NULL COMMENT '风格评分',
  `issues` TEXT COMMENT '问题JSON数组',
  `suggestions` TEXT COMMENT '建议JSON数组',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_chapter_id` (`chapter_id`),
  FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='质量报告表';

-- ReviewTask table
CREATE TABLE IF NOT EXISTS `ink_review_task` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `task_type` VARCHAR(50) NOT NULL COMMENT '任务类型',
  `priority` VARCHAR(20) NOT NULL DEFAULT 'medium' COMMENT '优先级: high/medium/low',
  `description` TEXT COMMENT '任务描述',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态: pending/in_progress/completed/rejected',
  `reviewer_note` TEXT COMMENT '审核备注',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `completed_at` DATETIME COMMENT '完成时间',
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_status` (`status`),
  INDEX `idx_priority` (`priority`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='审核任务表';

-- ChapterVersion table
CREATE TABLE IF NOT EXISTS `ink_chapter_version` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `chapter_id` BIGINT UNSIGNED NOT NULL COMMENT '章节ID',
  `version_no` INT NOT NULL COMMENT '版本号',
  `content` LONGTEXT NOT NULL COMMENT '版本内容',
  `note` VARCHAR(255) COMMENT '版本说明',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_chapter_id` (`chapter_id`),
  UNIQUE KEY `uk_chapter_version` (`chapter_id`, `version_no`),
  FOREIGN KEY (`chapter_id`) REFERENCES `ink_chapter`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='章节版本表';

-- ============================================
-- Reference and Knowledge tables
-- ============================================

-- ReferenceNovel table
CREATE TABLE IF NOT EXISTS `ink_reference_novel` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `title` VARCHAR(255) NOT NULL COMMENT '小说标题',
  `author` VARCHAR(100) COMMENT '作者',
  `source_url` VARCHAR(500) COMMENT '来源URL',
  `source_site` VARCHAR(50) COMMENT '来源站点: qidian/jjwxc/zongheng',
  `genre` VARCHAR(50) COMMENT '类型',
  `total_chapters` INT NOT NULL DEFAULT 0 COMMENT '总章节数',
  `total_words` INT NOT NULL DEFAULT 0 COMMENT '总字数',
  `status` VARCHAR(20) NOT NULL DEFAULT 'crawling' COMMENT '状态: crawling/completed/failed',
  `style_analysis` TEXT COMMENT '风格分析JSON',
  `keywords` TEXT COMMENT '关键词JSON数组',
  `similar_novels` TEXT COMMENT '相似小说JSON数组',
  `cover_image` VARCHAR(500) COMMENT '封面图片URL',
  `crawled_at` DATETIME COMMENT '爬取时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_source_site` (`source_site`),
  INDEX `idx_genre` (`genre`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='参考小说表';

-- ReferenceChapter table
CREATE TABLE IF NOT EXISTS `ink_reference_chapter` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED NOT NULL COMMENT '小说ID',
  `chapter_no` INT NOT NULL COMMENT '章节号',
  `title` VARCHAR(255) COMMENT '章节标题',
  `content` LONGTEXT COMMENT '章节内容',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_novel_id` (`novel_id`),
  UNIQUE KEY `uk_novel_chapter` (`novel_id`, `chapter_no`),
  FOREIGN KEY (`novel_id`) REFERENCES `ink_reference_novel`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='参考章节表';

-- KnowledgeBase table
CREATE TABLE IF NOT EXISTS `ink_knowledge_base` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED COMMENT '小说ID',
  `type` VARCHAR(50) NOT NULL COMMENT '知识类型',
  `title` VARCHAR(255) NOT NULL COMMENT '标题',
  `content` TEXT COMMENT '内容',
  `tags` TEXT COMMENT '标签JSON数组',
  `usage_count` INT NOT NULL DEFAULT 0 COMMENT '使用次数',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_type` (`type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='知识库表';

-- PromptTemplate table
CREATE TABLE IF NOT EXISTS `ink_prompt_template` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '模板名称',
  `genre` VARCHAR(50) COMMENT '适用类型',
  `stage` VARCHAR(50) COMMENT '适用阶段: outline/chapter/dialogue/etc',
  `template` LONGTEXT NOT NULL COMMENT '模板内容',
  `variables` TEXT COMMENT '变量定义JSON',
  `description` TEXT COMMENT '模板描述',
  `is_active` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
  `usage_count` INT NOT NULL DEFAULT 0 COMMENT '使用次数',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_genre` (`genre`),
  INDEX `idx_stage` (`stage`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='提示词模板表';

-- ============================================
-- Visual Design tables
-- ============================================

-- CharacterVisualDesign table
CREATE TABLE IF NOT EXISTS `ink_character_visual_design` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `character_id` BIGINT UNSIGNED NOT NULL COMMENT '角色ID',
  `appearance_description` TEXT COMMENT '外观描述',
  `facial_features` TEXT COMMENT '面部特征JSON',
  `hair_style` TEXT COMMENT '发型JSON',
  `skin_tone` VARCHAR(50) COMMENT '肤色',
  `body_type` VARCHAR(50) COMMENT '体型',
  `age` INT COMMENT '年龄',
  `gender` VARCHAR(20) COMMENT '性别',
  `outfit` TEXT COMMENT '服装JSON',
  `accessories` TEXT COMMENT '配饰JSON',
  `weapons` TEXT COMMENT '武器JSON',
  `art_style` VARCHAR(50) DEFAULT 'realistic' COMMENT '艺术风格',
  `color_palette` TEXT COMMENT '色彩调色板JSON',
  `reference_image_urls` TEXT COMMENT '参考图片URL数组JSON',
  `generated_images` TEXT COMMENT '生成图片JSON',
  `lora_model_id` VARCHAR(100) COMMENT 'LoRA模型ID',
  `lora_weight` DECIMAL(3,2) DEFAULT 0.8 COMMENT 'LoRA权重',
  `visual_embedding` JSON COMMENT '视觉特征向量',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_character_id` (`character_id`),
  FOREIGN KEY (`character_id`) REFERENCES `ink_character`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色视觉设计表';

-- SceneVisualDesign table
CREATE TABLE IF NOT EXISTS `ink_scene_visual_design` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `worldview_id` BIGINT UNSIGNED COMMENT '世界观ID',
  `name` VARCHAR(100) NOT NULL COMMENT '场景名称',
  `description` TEXT COMMENT '场景描述',
  `location_type` VARCHAR(50) COMMENT '位置类型: indoor/outdoor/virtual',
  `time_of_day` VARCHAR(50) COMMENT '时间段',
  `weather` VARCHAR(50) COMMENT '天气',
  `lighting` VARCHAR(100) COMMENT '光照设置',
  `color_palette` TEXT COMMENT '色彩调色板JSON',
  `elements` TEXT COMMENT '场景元素JSON',
  `reference_images` TEXT COMMENT '参考图片JSON',
  `generated_images` TEXT COMMENT '生成图片JSON',
  `art_style` VARCHAR(50) DEFAULT 'realistic' COMMENT '艺术风格',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_worldview_id` (`worldview_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='场景视觉设计表';

-- ============================================
-- Model Comparison Experiment tables
-- ============================================

-- ModelComparisonExperiment table
CREATE TABLE IF NOT EXISTS `ink_model_comparison_experiment` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(255) NOT NULL COMMENT '实验名称',
  `description` TEXT COMMENT '实验描述',
  `task_type` VARCHAR(50) NOT NULL COMMENT '任务类型',
  `prompt` LONGTEXT NOT NULL COMMENT '测试提示词',
  `model_ids` TEXT NOT NULL COMMENT '对比模型ID JSON数组',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态: pending/running/completed/failed',
  `winner_id` BIGINT UNSIGNED COMMENT '获胜模型ID',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX `idx_task_type` (`task_type`),
  INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模型对比实验表';

-- ExperimentResult table
CREATE TABLE IF NOT EXISTS `ink_experiment_result` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `experiment_id` BIGINT UNSIGNED NOT NULL COMMENT '实验ID',
  `model_id` BIGINT UNSIGNED NOT NULL COMMENT '模型ID',
  `output` LONGTEXT COMMENT '模型输出',
  `quality_score` DECIMAL(5,4) NOT NULL COMMENT '质量评分',
  `latency_ms` INT NOT NULL COMMENT '延迟(毫秒)',
  `cost` DECIMAL(10,6) NOT NULL COMMENT '成本(美元)',
  `metadata` JSON COMMENT '额外元数据',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_experiment_id` (`experiment_id`),
  INDEX `idx_model_id` (`model_id`),
  FOREIGN KEY (`experiment_id`) REFERENCES `ink_model_comparison_experiment`(`id`) ON DELETE CASCADE,
  FOREIGN KEY (`model_id`) REFERENCES `ink_ai_model`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='实验结果表';

-- FeedbackRecord table
CREATE TABLE IF NOT EXISTS `ink_feedback_record` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `novel_id` BIGINT UNSIGNED COMMENT '小说ID',
  `chapter_id` BIGINT UNSIGNED COMMENT '章节ID',
  `type` VARCHAR(50) NOT NULL COMMENT '反馈类型: user_review/ai_suggestion/model_output',
  `content` TEXT NOT NULL COMMENT '反馈内容',
  `rating` INT COMMENT '评分(1-5)',
  `status` VARCHAR(20) NOT NULL DEFAULT 'pending' COMMENT '状态: pending/applied/rejected',
  `applied_at` DATETIME COMMENT '应用时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX `idx_novel_id` (`novel_id`),
  INDEX `idx_chapter_id` (`chapter_id`),
  INDEX `idx_type` (`type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='反馈记录表';

-- ============================================
-- Multi-tenant support tables (Multi-tenancy)
-- ============================================

-- Tenant table (租户/组织)
CREATE TABLE IF NOT EXISTS `tenants` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `name` VARCHAR(100) NOT NULL COMMENT '租户名称',
  `code` VARCHAR(50) NOT NULL UNIQUE COMMENT '租户代码',
  `logo` VARCHAR(500) COMMENT 'Logo URL',
  `settings` TEXT COMMENT '租户配置JSON',
  `plan` VARCHAR(20) NOT NULL DEFAULT 'free' COMMENT '套餐: free/pro/enterprise',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态: active/suspended/banned',
  
  -- Quotas
  `max_projects` INT NOT NULL DEFAULT 5 COMMENT '最大项目数',
  `max_users` INT NOT NULL DEFAULT 3 COMMENT '最大用户数',
  `max_storage_mb` INT NOT NULL DEFAULT 1000 COMMENT '最大存储MB',
  `used_projects` INT NOT NULL DEFAULT 0 COMMENT '已用项目数',
  `used_users` INT NOT NULL DEFAULT 0 COMMENT '已用用户数',
  `used_storage_mb` INT NOT NULL DEFAULT 0 COMMENT '已用存储MB',
  
  -- Billing
  `billing_cycle` VARCHAR(20) NOT NULL DEFAULT 'monthly' COMMENT '计费周期',
  `expires_at` DATETIME COMMENT '到期时间',
  
  -- Contact
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
CREATE TABLE IF NOT EXISTS `tenant_users` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `tenant_id` BIGINT UNSIGNED NOT NULL COMMENT '租户ID',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
  `role` VARCHAR(20) NOT NULL DEFAULT 'member' COMMENT '角色: owner/admin/member/viewer',
  `nickname` VARCHAR(50) COMMENT '在租户内的昵称',
  `avatar` VARCHAR(500) COMMENT '头像',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态',
  `permissions` TEXT COMMENT '自定义权限JSON',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_tenant_user` (`tenant_id`, `user_id`),
  INDEX `idx_tenant_id` (`tenant_id`),
  INDEX `idx_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='租户用户关联表';

-- TenantProject table (租户项目)
CREATE TABLE IF NOT EXISTS `tenant_projects` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `tenant_id` BIGINT UNSIGNED NOT NULL COMMENT '租户ID',
  `project_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '项目ID',
  `project_type` VARCHAR(20) NOT NULL DEFAULT 'novel' COMMENT '项目类型: novel/custom',
  `name` VARCHAR(100) NOT NULL COMMENT '项目名称',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态',
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
CREATE TABLE IF NOT EXISTS `users` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uuid` VARCHAR(36) NOT NULL UNIQUE COMMENT 'UUID',
  `username` VARCHAR(50) NOT NULL UNIQUE COMMENT '用户名',
  `email` VARCHAR(100) NOT NULL UNIQUE COMMENT '邮箱',
  `phone` VARCHAR(20) COMMENT '手机号',
  `password` VARCHAR(100) NOT NULL COMMENT '密码哈希',
  `nickname` VARCHAR(50) COMMENT '昵称',
  `avatar` VARCHAR(500) COMMENT '头像',
  `status` VARCHAR(20) NOT NULL DEFAULT 'active' COMMENT '状态',
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
-- Add tenant_id to existing tables
-- ============================================

-- Add tenant_id and project_id to ink_novel
ALTER TABLE `ink_novel` 
  ADD COLUMN `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID' AFTER `uuid`,
  ADD COLUMN `project_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '项目ID' AFTER `tenant_id`,
  ADD COLUMN `is_public` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否公开' AFTER `style_prompt`,
  ADD COLUMN `access_code` VARCHAR(100) COMMENT '访问密码' AFTER `is_public`,
  ADD COLUMN `storage_size` BIGINT NOT NULL DEFAULT 0 COMMENT '存储大小字节' AFTER `access_code`,
  ADD INDEX `idx_tenant_id` (`tenant_id`),
  ADD INDEX `idx_project_id` (`project_id`);

-- Add tenant_id to ink_chapter
ALTER TABLE `ink_chapter` 
  ADD COLUMN `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID' AFTER `novel_id`,
  ADD INDEX `idx_tenant_id` (`tenant_id`);

-- Add tenant_id to ink_character
ALTER TABLE `ink_character` 
  ADD COLUMN `tenant_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '租户ID' AFTER `novel_id`,
  ADD INDEX `idx_tenant_id` (`tenant_id`);
