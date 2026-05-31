# InkFrame Backend 用户手册

> 版本：2026-05-31 · 适用服务端版本 ≥ 2026-05-31

---

## 目录

1. [项目概述](#1-项目概述)
2. [快速开始](#2-快速开始)
3. [配置说明](#3-配置说明)
4. [核心概念](#4-核心概念)
5. [小说创作](#5-小说创作)
6. [视频生成](#6-视频生成)
7. [角色与世界观](#7-角色与世界观)
8. [素材库](#8-素材库)
9. [平台与发布](#9-平台与发布)
10. [AI 提供商管理](#10-ai-提供商管理)
11. [多租户管理](#11-多租户管理)
12. [API 参考速查](#12-api-参考速查)
13. [部署指南](#13-部署指南)
14. [常见问题](#14-常见问题)

---

## 1. 项目概述

### 1.1 功能定位

InkFrame Backend 是一套 AI 驱动的"小说→视频"全链路内容生产系统。它将大语言模型（LLM）、图像生成、语音合成、视频生成等多路 AI 能力统一封装，支持从零开始创作小说，并将章节内容自动转化为带配音、字幕、音效的短视频草稿。

**核心能力一览：**

| 能力域 | 说明 |
|--------|------|
| 小说创作 | 大纲生成、分章写作（三步流水线）、质量评分、AI 审查与修订、剧情一致性校验 |
| 叙事记忆 | 多层上下文（全局摘要 → 弧线摘要 → 近期章节），支持 100+ 章长篇不失忆 |
| 角色管理 | 状态快照、形象生成（三视图 / 面部特写 / 肖像）、配音预览 |
| 视频生成 | 分镜生成、Ken Burns 动效、配音、SFX 音效、BGM、多格式导出 |
| 素材库 | 版本管理、标签、分享审批工作流、外部爬取（Unsplash/Freesound/Pixabay） |
| 平台发布 | 站内社交（点赞/评论）、外部发布（YouTube / 抖音 / Bilibili） |
| 多租户 | 完整的租户隔离与成员角色体系 |

### 1.2 技术栈

- **语言 / 框架**：Go 1.21+，Gin HTTP 框架
- **数据库**：MySQL 8.0+（GORM），Redis 7+
- **向量库**：Qdrant（推荐）/ Chroma / DashVector
- **存储**：阿里云 OSS / MinIO / 本地文件系统
- **AI 接入**：OpenAI、Anthropic Claude、DeepSeek、豆包、通义千问、Ollama、Kling、Seedance、火山引擎即梦

### 1.3 系统架构

```
HTTP Request
     │
     ▼
┌─────────────────────────────────────────┐
│  Middleware（CORS · JWT Auth · Rate Limit · Logger）│
└─────────────────────────────────────────┘
     │
     ▼
┌─────────────────────────────────────────┐
│              Handler 层（Gin）           │
│  NovelHandler · VideoHandler · ...      │
└─────────────────────────────────────────┘
     │
     ▼
┌─────────────────────────────────────────┐
│              Service 层（业务逻辑）      │
│  NovelService · VideoService · AIService│
│  NarrativeMemoryService · SFXService··· │
└─────────────────────────────────────────┘
     │          │           │
     ▼          ▼           ▼
┌─────────┐ ┌──────────┐ ┌──────────────┐
│Repository│ │ AI Module│ │ Vector Store │
│ (GORM)  │ │  多提供商 │ │Qdrant/Chroma │
└─────────┘ └──────────┘ └──────────────┘
     │          │
     ▼          ▼
  MySQL       外部 AI API
  Redis       (OpenAI/Kling…)
  OSS/MinIO
```

---

## 2. 快速开始

### 2.1 环境要求

| 依赖 | 最低版本 | 备注 |
|------|----------|------|
| Go | 1.21 | 推荐使用 Homebrew 安装的最新版 |
| MySQL | 8.0 | 字符集 utf8mb4 |
| Redis | 7.0 | — |
| Qdrant | 1.7+ | 可选，语义搜索功能需要 |

### 2.2 安装步骤

```bash
# 1. 克隆代码
git clone <repo-url>
cd inkframe-backend

# 2. 安装依赖
make deps

# 3. 创建配置文件
cp config.example.yaml config.yaml
# 然后编辑 config.yaml，填写数据库、Redis 等连接信息

# 4. 创建 MySQL 数据库
mysql -u root -p -e "CREATE DATABASE inkframe CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# 5. 启动服务（会自动执行 AutoMigrate）
make run
```

服务默认监听 `:8080`，启动成功后可通过 `GET /health` 验证：

```bash
curl http://localhost:8080/health
# → {"status":"ok"}
```

### 2.3 首次配置 AI 提供商

服务启动后，通过 API 添加至少一个 AI 提供商，才能使用生成功能：

```bash
# 登录（使用启动时种子的默认用户，开发模式下存在）
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@example.com","password":"admin123"}'

# 保存返回的 token
TOKEN="Bearer <your_jwt_token>"

# 添加 DeepSeek 提供商
curl -X POST http://localhost:8080/api/v1/model-providers \
  -H "Authorization: $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deepseek",
    "display_name": "DeepSeek",
    "api_key": "sk-xxxx",
    "base_url": "https://api.deepseek.com/v1",
    "is_active": true
  }'
```

---

## 3. 配置说明

配置文件路径默认为工作目录下的 `config.yaml`，支持通过 `--config` 标志指定。

### 3.1 完整配置结构

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  mode: "debug"          # debug | release | test
  jwt_secret: "your-secret-key"
  jwt_expiry: "24h"
  read_timeout: "30s"
  write_timeout: "60s"
  shutdown_timeout: "10s"

database:
  host: "127.0.0.1"
  port: 3306
  database: "inkframe"
  username: "root"
  password: "yourpassword"
  charset: "utf8mb4"
  max_idle_conns: 10
  max_open_conns: 100
  conn_max_lifetime: "1h"
  table_prefix: "ink_"   # 所有表名前缀

redis:
  host: "127.0.0.1"
  port: 6379
  password: ""
  db: 0
  pool_size: 10

storage:
  type: "local"           # oss | minio | local
  # OSS / MinIO 额外字段：
  # endpoint: "oss-cn-hangzhou.aliyuncs.com"
  # access_key: "..."
  # secret_key: "..."
  # bucket: "inkframe"
  # region: "cn-hangzhou"
  # base_url: "https://cdn.example.com"
  local_path: "./uploads"  # type=local 时有效

vector_db:
  type: "qdrant"          # qdrant | chroma | dashvector | "" (禁用)
  endpoint: "http://localhost:6333"
  api_key: ""
  index_name: "inkframe"

logger:
  level: "info"           # debug | info | warn | error
  format: "text"          # json | text
  output_path: ""         # 空=stdout

web_search:
  provider: ""            # searxng | bing | tavily | "" (禁用)
  api_key: ""
  endpoint: ""

sms:
  provider: ""            # aliyun | tencent | "" (禁用)
  access_key: ""
  secret_key: ""
  sign_name: ""
  template_code: ""
```

### 3.2 环境变量覆盖

以下环境变量在运行时读取，优先级高于配置文件：

| 变量名 | 用途 |
|--------|------|
| `OSS_ACCESS_KEY` / `OSS_SECRET_KEY` | 存储服务凭证 |
| `QDRANT_ENDPOINT` / `QDRANT_API_KEY` | Qdrant 连接覆盖 |
| `DASHVECTOR_API_KEY` | DashVector 连接 |
| `WEB_SEARCH_API_KEY` | Bing/Tavily API Key 覆盖 |
| `CRAWL_PROXY_URL` | 爬虫代理（如 `http://127.0.0.1:7890`） |
| `UNSPLASH_ACCESS_KEY` | Unsplash 图片爬取 |
| `FREESOUND_API_KEY` | Freesound 音效爬取 |
| `PIXABAY_API_KEY` | Pixabay 素材爬取 |
| `SFX_DIR` | 本地音效目录 |
| `BGM_DIR` | 本地 BGM 目录 |
| `KLING_API_KEY` | 可灵 API Key（视频/图片/TTS/SFX 共用） |
| `ALIYUN_TTS_API_KEY` | 阿里云 DashScope TTS Key |
| `BAIDU_TTS_API_KEY` / `BAIDU_TTS_SECRET_KEY` | 百度 TTS OAuth 凭证 |
| `MINIMAX_TTS_API_KEY` / `MINIMAX_TTS_GROUP_ID` | MiniMax TTS 凭证 |
| `TENCENT_TTS_SECRET_ID` / `TENCENT_TTS_SECRET_KEY` | 腾讯云 TTS 签名密钥 |
| `TENCENT_TTS_REGION` | 腾讯云区域（默认 `ap-guangzhou`） |
| `GOROOT` | Go 安装路径（macOS Homebrew 需要指定） |

### 3.3 数据库自动迁移

服务启动时会自动执行 AutoMigrate（只新增列/表，不删除现有数据）。如需手动控制迁移：

```bash
make migrate-up    # 应用 migrations/ 目录下的 SQL 文件
make migrate-down  # 回滚最后一次迁移
```

---

## 4. 核心概念

### 4.1 多租户模型

所有核心数据（小说、章节、视频、角色等）都包含 `tenant_id` 字段。JWT 中携带 `tenant_id`，Repository 层自动按租户过滤查询。

**用户 vs 租户的关系：**
- 一个 **租户（Tenant）** 是一个独立的内容空间
- 用户通过 `ink_tenant_member` 表加入租户，角色为 `owner / admin / member`
- 同一用户可以属于多个租户

### 4.2 异步任务系统

耗时操作（视频生成、批量图片生成、大纲分析等）以异步任务方式执行，持久化在 `ink_async_task` 表中。

**任务状态流转：**
```
pending → running → completed
                 → failed
         → cancelled（可主动取消）
```

**轮询任务进度：**
```bash
# 提交任务后返回 task_id
GET /api/v1/tasks/{task_id}
# → {"status":"running","progress":45,"title":"生成分镜"}
```

服务重启后会自动恢复 `running` 状态的未完成任务。

### 4.3 叙事记忆系统

针对 100+ 章长篇小说的上下文管理，避免超出 LLM 上下文窗口：

```
全局摘要（所有已完成弧线的压缩概述）
    ↓
弧线摘要（每 10 章生成一次弧线摘要）
    ↓
近期详细（最近 2 章完整内容）
    ↓
近期摘要（前 8 章每章 ≤40 字压缩）
```

弧线摘要由 `TriggerArcSummaryIfNeeded` 在每章生成后异步触发，不阻塞正文写作。

### 4.4 AI 提供商体系

```
AIService
  └── ModelManager
        ├── 已注册提供商（按名称索引）
        │     ├── openai → OpenAIProvider（带 RetryProvider 包装）
        │     ├── deepseek → DeepSeekProvider（同上）
        │     ├── kling → KlingProvider（视频/图片/TTS/SFX）
        │     └── ...
        └── FallbackManager（主提供商失败时切换备用）
```

**RetryProvider** 自动重试：HTTP 429/502/503/504 及连接错误，指数退避，最多 3 次，基础延迟 500ms。

### 4.5 速率限制

每个 IP 独立的令牌桶：容量 60，补充速率 10 token/秒。所有受保护路由均受限。

---

## 5. 小说创作

### 5.1 创建小说

```bash
POST /api/v1/novels
Content-Type: application/json

{
  "title": "万古长河",
  "description": "一个普通少年在修仙世界的成长故事",
  "genre": "玄幻",
  "target_chapters": 100,
  "target_word_count": 300000,
  "prompt_language": "zh"    # zh（中文）| en（英文）
}
```

**响应**中包含 `id` 字段，后续所有操作均通过该 ID 引用。

### 5.2 生成大纲

大纲生成是异步任务，调用后返回 `task_id`：

```bash
POST /api/v1/novels/{id}/outline
{
  "chapter_num": 100,        # 0=AI 自决章节数
  "keywords": ["修炼", "复仇"],
  "prompt": "主角性格内敛，复仇线贯穿始终",
  "max_tokens": 8192
}
```

大纲以 JSON 存储在 `novel.outline` 字段，包含每章的 `title`、`summary`、`plot_points`、`tension_level`、`hook` 等字段。

### 5.3 章节生成（三步流水线）

```
┌─────────────────┐    ┌──────────────────┐    ┌──────────────────┐
│ 1. 场景大纲生成  │ →  │ 2. 正文生成       │ →  │ 3. 细节润色       │
│ chapter_scene_  │    │ chapter_from_    │    │ refinement_pass  │
│ outline.tmpl    │    │ outline.tmpl     │    │ .tmpl            │
│ 3-5个场景分解    │    │ 2000-3000字正文  │    │ 质量问题修复      │
└─────────────────┘    └──────────────────┘    └──────────────────┘
```

触发下一章生成：

```bash
POST /api/v1/novels/{id}/chapters/generate
{
  "chapter_no": 1,       # 可选，默认生成下一章
  "prompt": "本章重点描写主角初入宗门",
  "max_tokens": 4096
}
```

生成后异步执行：章节摘要 → 创意标题 → 角色状态快照 → 弧线摘要。

### 5.4 质量控制

**AI 深度审查**（检查逻辑/一致性/文笔/风格）：

```bash
POST /api/v1/chapters/{id}/review
# → 返回 review_id

GET /api/v1/chapters/{id}/reviews
# 列出所有历史审查记录

POST /api/v1/chapters/{id}/review/apply-diffs
{
  "review_id": 42,
  "issue_ids": [1, 3, 5]    # 选择性应用修改建议
}
```

**忽略特定问题**（下次审查不再提示）：

```bash
POST /api/v1/chapters/{id}/ignored-issues
{
  "issue_text": "主角年龄描写不一致",
  "note": "已知设定，不需要修改"
}
```

**质量评分维度**（0-100 分）：

| 维度 | 权重 | 说明 |
|------|------|------|
| 逻辑性 | 30% | 情节连贯、因果合理 |
| 一致性 | 25% | 角色行为、世界观规则 |
| 文笔质量 | 25% | 词句多样性、对话比例 |
| 风格契合 | 20% | 与设定风格的一致性 |

### 5.5 剧情点追踪

剧情点用于追踪未解决的伏笔和剧情线索，大纲生成时会自动注入未解决的剧情点以引导 AI：

```bash
# 手动添加剧情点
POST /api/v1/novels/{id}/plot-points
{
  "type": "foreshadow",      # foreshadow | conflict | revelation
  "description": "主角剑的来历之谜",
  "chapter_no": 3            # 最早出现的章节
}

# AI 从章节中自动提取
POST /api/v1/chapters/{id}/plot-points/extract

# 标记为已解决
PUT /api/v1/plot-points/{id}/resolve
```

### 5.6 伏笔与时间线

```bash
# 伏笔管理
GET  /api/v1/novels/{id}/foreshadows
POST /api/v1/novels/{id}/foreshadows
POST /api/v1/novels/{id}/foreshadows/{foreshadow_id}/fulfill  # 标记回收

# 时间线
GET  /api/v1/novels/{id}/timeline
POST /api/v1/novels/{id}/timeline/build   # AI 自动构建时间线
```

### 5.7 小说导入

支持从多种来源导入参考小说：

```bash
# 从文本/JSON 直接导入
POST /api/v1/import/novel
{
  "title": "xxx",
  "chapters": [{"chapter_no":1, "title":"...", "content":"..."}]
}

# 从文件上传（支持分块）
POST /api/v1/import/novel/file/init
PUT  /api/v1/import/novel/file/chunk
POST /api/v1/import/novel/file/complete

# 从 URL 导入
POST /api/v1/import/novel/url
{"url": "https://..."}

# 爬取（起点/晋江/纵横）
POST /api/v1/import/novel/crawl
{"url": "https://www.qidian.com/book/xxx"}
```

---

## 6. 视频生成

### 6.1 创建视频项目

一个视频项目对应一个章节（或自定义内容）的视频化：

```bash
POST /api/v1/videos
{
  "novel_id": 1,
  "chapter_id": 5,          # 可选，绑定具体章节
  "title": "第五章：初入宗门",
  "type": "slideshow",      # slideshow（图片序列）| animation（AI 视频）
  "aspect_ratio": "9:16",   # 16:9 | 9:16 | 1:1
  "pacing": "normal",       # slow | normal | fast
  "target_duration": 180    # 目标时长（秒），0=AI 自决
}
```

### 6.2 生成分镜

分镜是视频的核心结构单元，每个分镜对应一段画面：

```bash
POST /api/v1/videos/{id}/storyboard/generate
{
  "script": "章节正文内容...",   # 可选，不传则使用绑定章节
  "style": "写实",
  "force": false               # true=强制重新生成所有分镜
}
```

**分镜字段说明：**

| 字段 | 说明 |
|------|------|
| `shot_no` | 镜头编号（1, 2, 3…） |
| `description` | 画面视觉描述（英文，用于图片生成） |
| `narration` | 旁白文案（中文） |
| `dialogue` | 角色台词（有台词则不生成旁白）|
| `duration` | 时长（秒） |
| `shot_size` | 景别（close-up / medium / wide / extreme-wide）|
| `camera_type` | 运镜（static / pan / zoom / tilt）|
| `transition` | 转场（cut / fade / dissolve）|
| `emotional_tone` | 情绪基调 |

### 6.3 生成画面

```bash
# 批量生成所有分镜的图片（只生成图，不合成视频）
POST /api/v1/videos/{id}/shots/batch-images

# 批量生成 Ken Burns 动效片段（图片→动态视频片段）
POST /api/v1/videos/{id}/shots/batch-clips

# 批量（图片+片段一起，旧流程）
POST /api/v1/videos/{id}/shots/batch-generate
{"shot_ids": [1, 2, 3]}    # 可选，不传则处理所有分镜

# 单个分镜重新生成
POST /api/v1/videos/{id}/shots/{shot_id}/generate

# 图片精修（基于参考图局部重绘）
POST /api/v1/videos/{id}/shots/{shot_id}/refine-image
{"prompt": "修改要求", "seed": 42}
```

**Ken Burns 动效规则（slideshow 模式）：**
- `zoom-in` / `zoom-out`：缩放
- `pan-left` / `pan-right`：平移
- `static`：静止（无动效）

动效生成是 CPU 密集型操作，单镜头约 30-60 秒（使用 WASM libx264 编码）。

### 6.4 配音（TTS）

```bash
# 批量为所有分镜生成配音
POST /api/v1/videos/{id}/shots/batch-voice
{
  "voice_id": "longxiaochun",   # 音色 ID，见下表
  "speed": 1.0,
  "force": false
}
```

**多 TTS 提供商与常用音色：**

| 提供商 | 注册名 | 常用音色 ID（示例） | 所需环境变量 |
|--------|--------|---------------------|-------------|
| 阿里云 DashScope | `aliyun-tts` | longxiaochun, longxiaoxia, longxiaobai, longfei, longjielidou, longmiaomiao, longshu, longwan, longcheng, longhua, longxiang | `ALIYUN_TTS_API_KEY` |
| 百度 | `baidu-tts` | 0=度小美, 1=度小宇, 3=度逍遥, 4=度丫丫, 5=度小娇, 103=度米朵, 106=度博文, 110=度小童, 111=度小萌 | `BAIDU_TTS_API_KEY` + `BAIDU_TTS_SECRET_KEY` |
| MiniMax | `minimax-tts` | female-shaonv, female-yujie, female-tianmei, male-qn-qingse, male-qn-jingying, presenter_male, presenter_female, audiobook_male_1, audiobook_female_1 | `MINIMAX_TTS_API_KEY` + `MINIMAX_TTS_GROUP_ID` |
| 腾讯云 | `tencent-tts` | 101001=智言(男), 101002=智雅(女), 101003=智燕(女/暖), 101050=WeJack(EN男), 101051=WeRose(EN女) | `TENCENT_TTS_SECRET_ID` + `TENCENT_TTS_SECRET_KEY` |
| 可灵 | `kling`（共用） | — | `KLING_API_KEY` |

**多段配音**（一个分镜多个声音段落）：

```bash
# 查看分镜的配音段落
GET /api/v1/videos/{id}/shots/{shot_id}/segments

# 追加一段
POST /api/v1/videos/{id}/shots/{shot_id}/segments
{
  "text": "你好，我是主角",
  "speaker": "主角",
  "voice_id": "longxiaochun",
  "seq_no": 1
}

# 在指定位置插入
POST /api/v1/videos/{id}/shots/{shot_id}/segments/insert
{"after_seq_no": 2, "text": "...", "speaker": "..."}

# 为单段生成语音
POST /api/v1/videos/{id}/shots/{shot_id}/segments/{seg_id}/voice
```

### 6.5 音效（SFX）

```bash
# AI 分析分镜并生成音效搜索标签
POST /api/v1/videos/{id}/shots/sfx-tags
{"force": false}   # true=重新分析已有标签的分镜

# 批量生成音效（搜索+生成）
POST /api/v1/videos/{id}/shots/sfx
{"shot_ids": [1, 2], "force": false}

# 查看/管理单个分镜的音效条目
GET    /api/v1/videos/{id}/shots/{shot_id}/sfx-items
POST   /api/v1/videos/{id}/shots/{shot_id}/sfx-items   # 手动添加
PUT    /api/v1/videos/{id}/shots/{shot_id}/sfx-items/{item_id}
PATCH  /api/v1/videos/{id}/shots/{shot_id}/sfx-items/{item_id}  # 切换启用
DELETE /api/v1/videos/{id}/shots/{shot_id}/sfx-items/{item_id}
```

**SFX 标签格式**：英文 ≤3 个单词，格式为 `[声源] [动作] [描述符]`。  
描述符规则：`action` 类必须含 `single/burst/hit/snap`，`ambient` 类必须含 `loop/continuous`，`emotion` 类必须含 `single/rise/sting`。

**SFX 生成优先级链**（自动降级）：
1. 素材库本地搜索 → 2. AudioLDM 生成 → 3. Freesound 搜索 → 4. Pixabay → 5. BBC → 6. Kling SFX 文生音效 → 7. ElevenLabs AI 生成

**Kling SFX 文生音效**（第 6 优先级，异步轮询）：
- 调用 `/v1/audio/text-to-audio` 提交任务，轮询至 `task_status=succeed`，返回 CDN `url_mp3`
- 音效时长 3~10 秒，通过 `duration` 字段控制
- 与视频生成共用 `KLING_API_KEY` 环境变量

### 6.6 背景音乐（BGM）

```bash
# AI 分析分镜情绪，自动分配 BGM
POST /api/v1/videos/{id}/bgm/analyze

# 搜索 Jamendo 免费版权音乐
GET /api/v1/videos/{id}/bgm/search?q=epic+adventure&duration=120

# 查看 BGM 段落列表
GET /api/v1/videos/{id}/bgm/segments

# 应用某首 BGM 到指定段落
PATCH /api/v1/videos/{id}/bgm/segments/{seg_id}/track
{"url": "https://...", "track_name": "Epic Adventure", "volume": 0.3}

# 切换启用/禁用
PATCH /api/v1/videos/{id}/bgm/segments/{seg_id}
{"disabled": false}
```

### 6.7 拼接与导出

```bash
# 服务端拼接最终 MP4（需要所有分镜的视频片段已生成）
POST /api/v1/videos/{id}/stitch

# 客户端合成（通过 FFmpeg）
POST /api/v1/videos/{id}/synthesize

# 下载最终视频
GET /api/v1/videos/{id}/download

# 多格式导出
GET /api/v1/videos/{id}/export/{format}
```

**导出格式说明：**

| `format` 参数 | 文件类型 | 用途 |
|--------------|----------|------|
| `capcut` | `.zip` | 剪映草稿（含视频/配音/字幕/BGM/Ken Burns） |
| `broll` | `.zip` | B 剪草稿（静态图 + 分镜注释 + 配音，供二剪参考） |
| `fcpxml` | `.zip` | Final Cut Pro / DaVinci Resolve |
| `zip` | `.zip` | 素材包（图片/视频/音频原始文件） |
| `srt` | `.srt` | SRT 字幕（通用格式） |
| `vtt` | `.vtt` | WebVTT 字幕（浏览器/网络视频）|
| `edl` | `.edl` | Avid / Premiere / Vegas |
| `otio` | `.otio` | OpenTimelineIO（Premiere / FCP / DaVinci）|
| `csv` | `.csv` | 分镜表（Excel / Notion）|

**导入剪映草稿步骤（macOS）：**
1. 解压 ZIP，得到文件夹 `{项目名}/`
2. 将文件夹移动到：`~/Movies/JianyingPro/User Data/Projects/com.lveditor.draft/`
3. 重启剪映，草稿出现在列表中

### 6.8 分镜 AI 审查

分镜（Storyboard）同样支持 AI 深度审查，与章节审查共用统一审查表（`entity_type = "storyboard"`）：

```bash
# 发起分镜审查
POST /api/v1/videos/{id}/storyboard/review
# → 返回 review_id

# 列出历史审查记录
GET /api/v1/videos/{id}/storyboard/reviews

# 获取单条审查详情
GET /api/v1/videos/{id}/storyboard/reviews/{rid}

# 应用修改建议（选择性）
POST /api/v1/videos/{id}/storyboard/review/apply-diffs
{
  "review_id": 10,
  "issue_ids": [2, 4]
}

# 忽略特定问题（含镜头号上下文）
POST /api/v1/videos/{id}/storyboard/ignored-issues
{
  "issue_text": "第 3 镜画面描述过于抽象",
  "shot_no": 3,
  "note": "风格刻意为之"
}

# 查看 / 删除已忽略问题
GET    /api/v1/videos/{id}/storyboard/ignored-issues
DELETE /api/v1/videos/{id}/storyboard/ignored-issues/{iid}
```

### 6.9 分镜管理

```bash
# 插入新分镜
POST /api/v1/videos/{id}/shots/insert
{"after_shot_no": 3, "description": "...", "duration": 5.0}

# 复制分镜（在指定位置后创建副本）
POST /api/v1/videos/{id}/shots/{shot_id}/copy
{"after_shot_no": -1}   # -1=紧跟在原镜头后

# 删除分镜
DELETE /api/v1/videos/{id}/shots/{shot_id}

# 更新分镜字段
PUT /api/v1/videos/{id}/storyboard/{shot_id}
{"description": "...", "duration": 6.0, "camera_type": "pan"}
```

---

## 7. 角色与世界观

### 7.1 角色管理

```bash
# 创建角色
POST /api/v1/novels/{id}/characters
{
  "name": "李晨风",
  "role": "protagonist",     # protagonist | antagonist | supporting | minor
  "description": "十六岁少年，外表普通，内心坚韧。出身普通猎户家庭，因家仇走上修炼之路。",
  "visual_prompt": "16-year-old Chinese teenage boy, short black hair, determined eyes, simple hunter clothing"
}

# AI 批量生成角色设定
POST /api/v1/novels/{id}/characters/ai-batch
{"count": 5, "context": "玄幻门派故事"}
```

### 7.2 角色形象生成

```bash
# 三视图（正/侧/背）
POST /api/v1/characters/{id}/three-view
{"style": "anime", "seed": 42}

# 面部特写
POST /api/v1/characters/{id}/face-closeup

# 肖像
POST /api/v1/characters/{id}/portrait/upload
# 或 POST /api/v1/novels/{id}/characters/batch-images 批量生成

# 一致性检查（与参考图对比）
POST /api/v1/characters/{id}/analyze-consistency
```

### 7.3 角色状态快照

每章生成后自动触发快照更新，记录角色在该章结束时的状态：

```bash
# 手动触发快照同步
POST /api/v1/novels/{id}/chapters/{chapter_no}/character-snapshots

# 查看角色弧线（跨章节的变化轨迹）
GET /api/v1/novels/{id}/character-arcs
GET /api/v1/novels/{id}/character-arcs/{character_id}
```

快照字段包含：年龄/体格/健康状况/受伤情况、修为/装备、心理状态/动机/目标/恐惧、所在位置、人物关系。

### 7.4 角色配音

```bash
# 预览配音效果
POST /api/v1/characters/{id}/voice/preview
{"text": "这是一段测试台词", "voice_id": "longxiaochun", "speed": 1.0}

# 下载配音样本
GET /api/v1/characters/{id}/voice/sample
```

### 7.5 世界观管理

```bash
# 创建世界观
POST /api/v1/worldviews
{
  "name": "玄元大陆",
  "description": "以灵气修炼为主的奇幻大陆...",
  "genre": "玄幻",
  "magic_system": "分金/木/水/火/土五系灵根，修炼分练气/筑基/金丹/元婴四大境界",
  "geography": "大陆分东西南北四州..."
}

# AI 生成世界观
POST /api/v1/worldviews/generate
{"genre": "玄幻", "keywords": ["修炼", "宗门", "妖兽"]}

# 添加世界观实体
POST /api/v1/worldviews/{id}/entities
{
  "type": "location",        # location | organization | artifact | race | creature
  "name": "青云宗",
  "description": "东州第一大宗，拥有上千年历史..."
}
```

### 7.6 场景锚点（视觉参考位置）

场景锚点是小说中的重要地点/场景的视觉参考，用于保证视频画面的一致性：

```bash
# AI 从章节中提取场景锚点
POST /api/v1/novels/{id}/scene-anchors/extract
{"chapter_no": 1}

# 生成参考图
POST /api/v1/scene-anchors/{id}/generate-ref-image
{"prompt_override": "morning light, misty mountain"}

# 锁定参考图（后续分镜生成以此为一致性基准）
PUT /api/v1/scene-anchors/{id}/ref-image
{"url": "https://cdn.../ref.jpg"}

# 查看一致性得分历史
GET /api/v1/scene-anchors/{id}/consistency-logs
```

---

## 8. 素材库

素材库（Asset Library）是独立于小说/视频的共享资源管理系统，支持图片、音频、视频等多类型素材的版本管理与协作分享。

### 8.1 上传与搜索

```bash
# 上传素材
POST /api/v1/assets
Content-Type: multipart/form-data
file=@image.jpg&title=山景&tags=mountain,landscape

# 全文/语义搜索
GET /api/v1/assets?q=山景&type=image&page=1&page_size=20

# 自动打标签
POST /api/v1/assets/{id}/auto-tag
```

### 8.2 版本管理

```bash
# 上传新版本
POST /api/v1/assets/{id}/versions

# 查看版本列表
GET /api/v1/assets/{id}/versions

# 回滚到指定版本
POST /api/v1/assets/{id}/versions/{v}/restore
```

### 8.3 分享审批工作流

```bash
# 申请将私有素材分享到公共库
POST /api/v1/assets/{id}/share-request
{"note": "高质量山景图，适合玄幻背景"}

# 管理员审批
POST /api/v1/admin/share-requests/{id}/approve
POST /api/v1/admin/share-requests/{id}/reject
{"reason": "分辨率不足"}

# 撤回分享申请
POST /api/v1/assets/{id}/withdraw
```

### 8.4 收藏集

```bash
# 创建收藏集
POST /api/v1/asset-collections
{"name": "玄幻场景参考", "description": "..."}

# 添加素材到收藏集
POST /api/v1/asset-collections/{id}/items
{"asset_ids": [1, 2, 3]}
```

### 8.5 爬虫任务

```bash
# 创建爬取任务（从 Unsplash/Freesound/Pixabay 批量抓取）
POST /api/v1/crawl-jobs
{
  "source": "unsplash",     # unsplash | freesound | pixabay | bbc
  "keywords": ["mountain", "forest"],
  "max_count": 100
}

# 取消/重试
POST /api/v1/crawl-jobs/{id}/cancel
POST /api/v1/crawl-jobs/{id}/retry
```

---

## 9. 平台与发布

### 9.1 发布小说/章节

```bash
# 发布小说（全局可见）
POST /api/v1/novels/{id}/publish
{"visibility": "public"}    # public | members_only | private

# 发布单章
POST /api/v1/novels/{id}/chapters/{chapter_no}/publish

# 批量发布
POST /api/v1/novels/{id}/chapters/batch-publish
{"chapter_nos": [1, 2, 3, 4, 5]}
```

### 9.2 社交功能

```bash
# 点赞
POST /api/v1/platform/novels/{id}/like
POST /api/v1/platform/videos/{id}/like

# 评论
POST /api/v1/platform/novels/{id}/chapters/{chapter_no}/comments
{"content": "精彩！"}

DELETE /api/v1/platform/novels/{id}/chapters/{chapter_no}/comments/{comment_id}
```

### 9.3 阅读进度

```bash
# 保存阅读进度
PUT /api/v1/platform/novels/{id}/progress
{"chapter_no": 23, "position": 1500}

# 获取进度
GET /api/v1/platform/novels/{id}/progress

# 历史记录
GET /api/v1/platform/me/reading-history
```

### 9.4 发布到外部平台

```bash
POST /api/v1/videos/{id}/publish-external
{
  "platform": "youtube",    # youtube | douyin | bilibili
  "account_id": 1,
  "title": "第五章精彩片段",
  "description": "...",
  "tags": ["修仙", "玄幻"]
}

# 查看发布记录
GET /api/v1/videos/{id}/publish-records
```

---

## 10. AI 提供商管理

### 10.1 注册提供商

```bash
POST /api/v1/model-providers
{
  "name": "deepseek",           # 内部标识符（唯一）
  "display_name": "DeepSeek",
  "provider_type": "openai_compatible",
  "api_key": "sk-xxxx",
  "base_url": "https://api.deepseek.com/v1",
  "is_active": true
}
```

**支持的提供商类型（`provider_type`）：**

| 类型 | 代表服务 |
|------|----------|
| `openai` | OpenAI 官方 |
| `openai_compatible` | DeepSeek、豆包、通义千问等兼容接口 |
| `anthropic` | Claude |
| `kling` | 快手可灵（视频/图片/TTS/SFX 文生音效） |
| `seedance` | 生数科技（视频）|
| `volcengine` | 火山引擎即梦（图片）|
| `ollama` | 本地 Ollama 服务 |
| `baidu-tts` | 百度语音合成 |
| `minimax-tts` | MiniMax 语音合成 |
| `aliyun-tts` | 阿里云 DashScope 语音合成 |
| `tencent-tts` | 腾讯云语音合成 |

### 10.2 添加模型

```bash
POST /api/v1/models
{
  "provider_id": 1,
  "model_id": "deepseek-chat",      # API 中实际的模型 ID
  "display_name": "DeepSeek V3",
  "task_types": ["outline", "chapter", "character", "storyboard"],
  "max_tokens": 8192,
  "context_window": 65536,
  "is_active": true
}

# 从提供商接口自动拉取可用模型列表
POST /api/v1/model-providers/{id}/fetch-models
```

### 10.3 任务模型配置

每种任务类型可以独立配置使用哪个模型及生成参数：

```bash
# 查看任务配置
GET /api/v1/task-configs/{task_type}
# task_type 可选值见下表

# 更新任务配置
PUT /api/v1/task-configs/{task_type}
{
  "model_id": 5,
  "temperature": 0.8,
  "max_tokens": 4096,
  "timeout_seconds": 120
}
```

**主要任务类型：**

| `task_type` | 用途 |
|-------------|------|
| `outline` | 大纲生成 |
| `chapter` | 章节正文 |
| `chapter_outline` | 章节场景大纲 |
| `character` | 角色设定生成 |
| `worldview` | 世界观生成 |
| `storyboard` | 分镜脚本 |
| `storyboard_review` | 分镜审查 |
| `chapter_review` | 章节深度审查 |
| `sfx_analyze` | 音效标签分析 |
| `extract_items` | 物品提取 |
| `scene_anchor_extract` | 场景锚点提取 |

### 10.4 MCP 工具集成

MCP（Model Context Protocol）工具允许模型调用外部工具（如 Web 搜索、代码执行等）：

```bash
# 注册 MCP 工具
POST /api/v1/mcp-tools
{
  "name": "web_search",
  "description": "搜索互联网获取最新信息",
  "transport": "http",           # http | stdio
  "endpoint": "http://localhost:9090/search",
  "schema": {...}                # JSON Schema 描述工具参数
}

# 将工具绑定到模型
POST /api/v1/models/{id}/mcp-tools
{"tool_id": 1}

# 测试工具连接
POST /api/v1/mcp-tools/{id}/test
```

**内置工具（无需注册，直接调用）：**

```bash
POST /api/v1/tools/web-search    {"query": "修仙小说热门题材2024"}
POST /api/v1/tools/wiki-search   {"query": "中国古代官职体系"}
POST /api/v1/tools/image-ref-search  {"query": "mountain sunrise fog"}
GET  /api/v1/tools/color-palette/list
POST /api/v1/tools/story-pattern  {"genre": "玄幻", "theme": "复仇"}
```

---

## 11. 多租户管理

### 11.1 租户管理

```bash
# 创建租户
POST /api/v1/tenants
{
  "name": "Studio Alpha",
  "description": "创作团队"
}

# 查看用量配额
GET /api/v1/tenants/{id}/quota
# → {"word_count_used": 125000, "word_count_limit": 500000, ...}
```

### 11.2 成员管理

```bash
# 添加成员
POST /api/v1/tenants/{id}/members
{
  "user_id": 42,
  "role": "member"    # owner | admin | member
}

# 更新成员角色
PUT /api/v1/tenants/{id}/members/{user_id}/role
{"role": "admin"}

# 移除成员
DELETE /api/v1/tenants/{id}/members/{user_id}
```

**角色权限说明：**

| 角色 | 权限 |
|------|------|
| `owner` | 全部权限，包括删除租户、转让所有权 |
| `admin` | 管理成员、修改配置，不可删除租户 |
| `member` | 读写内容，不可管理成员 |

---

## 12. API 参考速查

### 12.1 认证

所有受保护接口需在请求头携带 JWT：

```
Authorization: Bearer <token>
```

**获取 Token：**
```bash
POST /api/v1/auth/login
{"email": "user@example.com", "password": "yourpassword"}
# → {"token": "eyJ...", "refresh_token": "...", "expires_in": 86400}

# 刷新 Token
POST /api/v1/auth/refresh
{"refresh_token": "..."}
```

### 12.2 通用响应格式

```json
// 成功
{"code": 0, "message": "ok", "data": {...}}

// 失败
{"code": 400, "message": "参数错误: title 不能为空"}
```

### 12.3 分页参数

所有列表接口均支持：

```
GET /api/v1/novels?page=1&page_size=20
# page: ≥1，默认 1
# page_size: 1-100，默认 20
```

### 12.4 主要 API 端点索引

| 资源 | 基础路径 |
|------|---------|
| 认证 | `/api/v1/auth` |
| 小说 | `/api/v1/novels` |
| 章节 | `/api/v1/novels/{id}/chapters` |
| 角色 | `/api/v1/novels/{id}/characters` |
| 世界观 | `/api/v1/worldviews` |
| 物品 | `/api/v1/novels/{id}/items` |
| 剧情点 | `/api/v1/novels/{id}/plot-points` |
| 场景锚点 | `/api/v1/novels/{id}/scene-anchors` |
| 视频 | `/api/v1/videos` |
| 分镜 | `/api/v1/videos/{id}/storyboard` |
| 配音段落 | `/api/v1/videos/{id}/shots/{shot_id}/segments` |
| 音效 | `/api/v1/videos/{id}/shots/{shot_id}/sfx-items` |
| BGM | `/api/v1/videos/{id}/bgm` |
| 素材库 | `/api/v1/assets` |
| 任务 | `/api/v1/tasks` |
| AI 提供商 | `/api/v1/model-providers` |
| AI 模型 | `/api/v1/models` |
| 任务配置 | `/api/v1/task-configs` |
| MCP 工具 | `/api/v1/mcp-tools` |
| 租户 | `/api/v1/tenants` |
| 平台 | `/api/v1/platform` |
| 文件上传 | `/api/v1/upload/image` |
| 健康检查 | `/health` |

### 12.5 文件下载

**媒体文件**（配音、参考图等，无需登录）：

```bash
GET /api/v1/media/{id}
# 按数据库中的 audio_path / image_url 字段查找，流式返回
```

**视频文件：**

```bash
GET /api/v1/videos/{id}/download
# 返回最终合成视频文件（需要先 stitch/synthesize）
```

---

## 13. 部署指南

### 13.1 生产配置要点

**config.yaml 生产调整：**

```yaml
server:
  mode: "release"          # 关闭 Gin 调试输出
  jwt_secret: "<随机32位字符串>"

database:
  max_idle_conns: 20
  max_open_conns: 200

logger:
  level: "info"
  format: "json"
  output_path: "/var/log/inkframe/app.log"
```

### 13.2 Docker 部署

```bash
# 构建镜像
make docker-build

# 运行容器
docker run -d \
  --name inkframe-backend \
  -p 8080:8080 \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -v /data/uploads:/app/uploads \
  -e JWT_SECRET=your-secret \
  inkframe-backend:latest
```

或使用 `docker-compose`（建议生产环境）：

```yaml
version: '3.8'
services:
  app:
    image: inkframe-backend:latest
    ports: ["8080:8080"]
    volumes:
      - ./config.yaml:/app/config.yaml
      - uploads:/app/uploads
    depends_on: [mysql, redis]
    restart: unless-stopped

  mysql:
    image: mysql:8.0
    environment:
      MYSQL_ROOT_PASSWORD: rootpass
      MYSQL_DATABASE: inkframe
    volumes: [mysql-data:/var/lib/mysql]

  redis:
    image: redis:7-alpine
    volumes: [redis-data:/data]

volumes:
  uploads:
  mysql-data:
  redis-data:
```

### 13.3 交叉编译

```bash
make build-linux    # 生成 Linux amd64 二进制
make build-windows  # 生成 Windows amd64 exe
```

### 13.4 健康检查与监控

```bash
# 健康检查（返回 200 即正常）
GET /health
→ {"status":"ok"}

# 推荐监控指标
- /health 接口响应时间 & 状态码
- MySQL 连接池使用率（max_open_conns）
- Redis 内存使用量
- ink_async_task 表中 status='running' 的任务数量（异常堆积预警）
- 异步任务 failed 比例
```

### 13.5 日志结构

生产模式（`format: json`）输出结构化日志，每行一个 JSON：

```json
{
  "level": "INFO",
  "time": "2026-05-30T21:51:33.791+0800",
  "msg": "[AI] provider=deepseek elapsed=3.045s maxTokens=0 respLen=222",
  "caller": "service/ai_service.go:669"
}
```

关键日志前缀：

| 前缀 | 含义 |
|------|------|
| `[AI]` | AI 调用记录（provider/耗时/token 数） |
| `[VideoService]` | 视频生成流程 |
| `[SFXService]` | 音效生成流程 |
| `[CapCutService]` | 导出操作 |
| `[NovelAnalysis]` | 小说分析流程 |

---

## 14. 常见问题

### Q1：大纲生成失败，提示"outline parse failed"

**原因**：AI 返回了非标准 JSON（常见于 DeepSeek，偶发缺少冒号或格式错误）。

**解决**：
1. 检查日志中的 `preview=` 字段，查看 AI 实际返回的前 500 字符
2. 尝试在小说设置中更换 AI 提供商（Claude 或 GPT-4 更稳定）
3. 减少 `chapter_num`（章节数过多会导致响应过长）
4. 系统已内置自动修复机制，大多数情况下会自动处理

### Q2：Ken Burns 动效生成卡住或超时

**原因**：WASM libx264 编码是 CPU 密集型操作，1920×1080 分辨率下单镜头约 30-60 秒，多镜头并发会导致 CPU 满载。

**解决**：
- 避免同时对大量镜头生成动效片段
- 优先生成图片（`batch-images`），之后再按需生成片段（`batch-clips`）
- 生产环境增加 CPU 核心数

### Q3：视频导出到剪映后素材显示"离线"

**原因**：视频草稿中的素材使用 CDN 链接，打开草稿时需要网络访问。

**解决**：
- 检查 CDN 链接是否有效（可在浏览器中直接访问测试）
- 如使用私有存储，确保 CDN 签名未过期
- 使用 `format=zip` 导出素材包，在剪映中手动替换素材

### Q4：章节生成质量较低

**解决方案（按优先级）：**
1. **增加世界观和角色设定**：内容越丰富，AI 上下文越充分
2. **调整 `temperature`**：创意度过高时（>0.9）降低至 0.7-0.8
3. **使用更强的模型**：章节生成建议使用 GPT-4o / Claude-sonnet
4. **添加创作 `prompt`**：在章节级别注入具体要求
5. **使用精修功能**：生成后调用 `POST /api/v1/chapters/{id}/review` 进行 AI 深度审查

### Q5：TTS 配音与视频时长不同步

**原因**：`shot.Duration`（画面时长）与实际音频时长不一致。

**说明**：系统已自动处理——配音时长超过画面时长时会截断，短于时则以画面时长为准。如遇同步问题，尝试重新生成配音（`force: true`）。

### Q6：如何支持中英文双语创作

在小说设置中将 `prompt_language` 设置为 `"en"`，系统会：
- 角色描述（`Description`）输出为英文
- 物品描述输出为英文  
- 视觉 Prompt（`VisualPrompt`）保持英文（图片生成始终使用英文）
- 分镜画面描述（`Description`）输出为英文

中文大纲/章节正文不受影响。

### Q7：异步任务长时间停留在 pending/running 状态

**诊断步骤：**
```bash
# 查看任务详情
GET /api/v1/tasks/{task_id}

# 检查服务日志中是否有对应任务 ID 的 ERROR
grep task_id app.log
```

**常见原因：**
- AI 提供商超时（默认 5 分钟，可通过任务配置调整 `timeout_seconds`）
- 并发视频生成任务过多（受 `VideoConcurrency` 配置限制）
- 服务重启后任务会自动恢复，一般无需手动干预

### Q8：Qdrant 未部署时能否使用

可以。将 `vector_db.type` 设为 `""` 或不配置，系统会禁用向量搜索功能，`KnowledgeService` 中的语义搜索会自动降级为关键词搜索。其余功能不受影响。

---

## 附录 A：数据库表前缀

所有表名以 `ink_` 为前缀（可通过 `database.table_prefix` 配置）。

**主要数据表：**

| 表名 | 说明 |
|------|------|
| `ink_novel` | 小说 |
| `ink_chapter` | 章节 |
| `ink_chapter_version` | 章节版本历史 |
| `ink_character` | 角色 |
| `ink_character_state_snapshot` | 角色状态快照 |
| `ink_worldview` | 世界观 |
| `ink_worldview_entity` | 世界观实体 |
| `ink_video` | 视频项目 |
| `ink_storyboard_shot` | 分镜 |
| `ink_shot_voice_segment` | 配音段落 |
| `ink_arc_summary` | 弧线摘要 |
| `ink_async_task` | 异步任务 |
| `ink_review_record` | 审查记录（章节/分镜通用）|
| `ink_ignored_review_issue` | 忽略的审查问题 |
| `ink_model_provider` | AI 提供商 |
| `ink_ai_model` | AI 模型 |
| `ink_task_model_config` | 任务模型配置 |
| `ink_mcp_tool` | MCP 工具 |
| `ink_tenant` | 租户 |
| `ink_user` | 用户 |
| `ink_asset` | 素材 |

## 附录 B：常用 Make 命令

```bash
make deps          # 下载并整理 Go 依赖
make build         # 编译到 ./bin/inkframe-backend
make run           # 编译并运行
make dev           # 热重载开发（需安装 reflex）
make test          # 运行测试（含竞态检测）
make test-coverage # 生成 coverage.html
make fmt           # 代码格式化
make vet           # go vet 静态检查
make lint          # golangci-lint 检查
make docker-build  # 构建 Docker 镜像
make migrate-up    # 执行数据库迁移
make migrate-down  # 回滚迁移
make docs          # 生成 Swagger 文档
```

---

*手册最后更新：2026-05-31 · 如发现内容有误或需要补充，请在代码仓库提交 Issue。*
