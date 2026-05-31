# InkFrame Backend

> **简影**平台后端服务 — AI 驱动的"小说→视频"全链路内容生产系统

## 功能特性

### 小说创作
- 多题材大纲生成（玄幻/都市/言情/历史等 18 种类型）
- 三步章节流水线：场景大纲 → 正文生成 → 细节润色
- AI 深度审查与差异化修订（章节/分镜均支持，统一 Review 表）
- 质量评分（逻辑 30% · 一致性 25% · 文笔 25% · 风格 20%）
- 剧情点追踪、伏笔管理、时间线构建
- 叙事记忆系统：全局摘要 → 弧线摘要 → 近期章节，支持 100+ 章不失忆

### 角色 & 世界观
- 角色状态快照（跨章节状态追踪）、角色弧线
- 角色形象生成：三视图 / 面部特写 / 肖像
- 世界观实体管理（地点/组织/神器/种族/生物）
- 场景锚点：AI 提取 → 生成参考图 → 锁定一致性基准 → 评分追踪

### 视频生成
- 分镜脚本生成，支持节奏控制（慢/标准/快）和目标时长
- 图片生成（阿里云 OSS + 火山引擎即梦）、Ken Burns 动效
- AI 视频片段（Kling / Seedance）
- 多段配音：每镜头多声音段落，支持插入/删除
- 音效（SFX）：AI 标签分析 + 6 级优先级降级链（含 Kling 文生音效）
- BGM：情绪分析 + Jamendo 版权音乐搜索
- 多格式导出：剪映 / FCP / B 剪 / EDL / OTIO / SRT / VTT / CSV

### AI 提供商
- 文本 LLM：OpenAI、Anthropic Claude、DeepSeek、豆包、通义千问、Ollama
- 图片生成：火山引擎即梦（HMAC-SHA256 签名，异步轮询）
- 视频生成：Kling、Seedance
- TTS 语音合成：阿里云 DashScope、百度、MiniMax、腾讯云
- SFX 文生音效：Kling SFX（异步轮询，3~10s）
- RetryProvider 自动重试：HTTP 429/502/503/504，指数退避，最多 3 次

### 素材库 & 平台
- 素材版本管理、标签、分享审批工作流
- 外部素材爬取：Unsplash / Freesound / Pixabay / BBC
- 小说改写（规避版权风险）
- 站内社交（点赞/评论/阅读进度）
- 外部发布（YouTube / 抖音 / Bilibili）
- 多租户：租户隔离、成员角色（owner/admin/member）、配额管理

## 技术栈

| 层次 | 技术 |
|------|------|
| 语言 | Go 1.21+ |
| HTTP 框架 | Gin |
| 数据库 | MySQL 8.0+（GORM，表前缀 `ink_`） |
| 缓存 | Redis 7+（热数据 TTL 30min） |
| 向量存储 | Qdrant / Chroma / DashVector（可选） |
| 文件存储 | 阿里云 OSS（HMAC-SHA256 签名） |
| AI LLM | OpenAI / Claude / DeepSeek / 豆包 / 通义 / Ollama |
| 图片生成 | 火山引擎即梦（volcengine） |
| 视频生成 | Kling / Seedance |
| TTS | 阿里云 DashScope / 百度 / MiniMax / 腾讯云 |
| SFX | Kling SFX / AudioLDM / ElevenLabs / Freesound |

## 项目结构

```
inkframe-backend/
├── cmd/server/              # 启动入口，依赖注入 & 路由装配
├── internal/
│   ├── ai/                  # AI 提供商抽象层
│   │   ├── openai.go        # OpenAI / 兼容接口
│   │   ├── claude.go        # Anthropic Claude
│   │   ├── kling_provider.go     # 可灵视频/图片
│   │   ├── kling_sfx_provider.go # 可灵文生音效
│   │   ├── volcengine_visual.go  # 火山引擎即梦
│   │   ├── aliyun_tts.go / baidu_tts.go / minimax_tts.go / tencent_tts.go
│   │   └── retry_provider.go    # 指数退避重试包装
│   ├── config/              # Viper YAML 配置
│   ├── handler/             # Gin HTTP 处理器（一文件对应一领域）
│   ├── middleware/          # CORS / JWT / 限流 / 日志 / 恢复
│   ├── model/               # GORM 数据模型（model.go + tenant.go）
│   ├── repository/          # 数据访问层（含 Redis 缓存）
│   ├── router/              # 所有路由注册
│   ├── service/             # 业务逻辑层
│   │   └── prompts/         # AI Prompt 模板（.j2 / .tmpl）
│   ├── vector/              # 向量存储抽象（Qdrant / Chroma）
│   └── oss/                 # 阿里云 OSS 客户端
├── config.example.yaml
└── Makefile
```

## 快速开始

### 前置要求

| 依赖 | 最低版本 |
|------|----------|
| Go | 1.21 |
| MySQL | 8.0（utf8mb4） |
| Redis | 7.0 |
| Qdrant | 1.7+（可选，语义搜索） |

### 安装与运行

```bash
git clone <repo-url>
cd inkframe-backend
cp config.example.yaml config.yaml   # 填写 DB / Redis 连接信息
make deps
make run                              # 启动，监听 :8080
```

验证：

```bash
curl http://localhost:8080/health
# → {"status":"ok"}
```

### 环境变量

AI API Key 通过管理界面（`/api/v1/model-providers`）配置，下列变量为运行时覆盖项：

```bash
# 存储 & 向量
export QDRANT_ENDPOINT=http://localhost:6333
export QDRANT_API_KEY=...
export DASHVECTOR_API_KEY=...

# 素材爬取
export UNSPLASH_ACCESS_KEY=...
export FREESOUND_API_KEY=...
export PIXABAY_API_KEY=...

# TTS（也可通过管理界面配置）
export KLING_API_KEY=...
export ALIYUN_TTS_API_KEY=...
export BAIDU_TTS_API_KEY=...
export BAIDU_TTS_SECRET_KEY=...
export MINIMAX_TTS_API_KEY=...
export MINIMAX_TTS_GROUP_ID=...
export TENCENT_TTS_SECRET_ID=...
export TENCENT_TTS_SECRET_KEY=...

# macOS Homebrew Go
export GOROOT=/opt/homebrew/Cellar/go/1.24.4/libexec
```

## 常用命令

```bash
make run            # 编译并运行（:8080）
make dev            # 热重载（需安装 reflex）
make test           # 带竞态检测运行测试
make test-coverage  # 生成 coverage.html
make fmt            # gofmt -s -w
make vet            # go vet
make lint           # golangci-lint
make build          # 输出到 ./bin/inkframe-backend
make build-linux    # 交叉编译 Linux amd64
make migrate-up     # 执行 migrations/ 下的 SQL
make migrate-down   # 回滚迁移
make docs           # 生成 Swagger（swag）
```

## 核心数据表

所有表由 GORM AutoMigrate 在启动时自动创建/更新（只新增列，不删除数据）。

| 表名 | 说明 |
|------|------|
| `ink_novel` | 小说（含 `prompt_language`） |
| `ink_chapter` | 章节内容 & 状态 |
| `ink_chapter_version` | 章节版本历史 |
| `ink_character` | 角色（`description` 统一描述字段） |
| `ink_character_state_snapshot` | 角色跨章节状态快照 |
| `ink_worldview` | 世界观 |
| `ink_worldview_entity` | 世界观实体 |
| `ink_item` | 小说物品/道具 |
| `ink_scene_anchor` | 场景锚点（视觉一致性参考） |
| `ink_scene_consistency_log` | 场景一致性评分日志 |
| `ink_arc_summary` | 弧线摘要（叙事记忆） |
| `ink_video` | 视频项目（含 `pacing` / `target_duration`） |
| `ink_storyboard_shot` | 分镜镜头 |
| `ink_shot_voice_segment` | 分镜多段配音 |
| `ink_review_record` | AI 审查记录（`entity_type`: chapter/storyboard） |
| `ink_ignored_review_issue` | 忽略的审查问题 |
| `ink_async_task` | 异步任务（pending→running→completed/failed） |
| `ink_model_provider` | AI 提供商 |
| `ink_ai_model` | AI 模型配置 |
| `ink_task_model_config` | 任务级模型分配 |
| `ink_mcp_tool` | MCP 工具注册 |
| `ink_asset` | 素材库 |
| `ink_tenant` | 租户 |
| `ink_user` | 用户 |

## 架构说明

### 依赖注入

所有依赖在 `cmd/server/main.go` 的 `initServices()` 中完成装配，服务间通过 functional option（`WithXxx()`）注入可选依赖：

```go
chapterSvc.WithNarrativeMemory(narrativeMemSvc)
videoSvc.WithSegmentRepo(segmentRepo).WithReviewRecordRepo(reviewRepo)
```

### 异步任务

耗时操作以异步任务形式执行，持久化在 `ink_async_task`，重启后自动恢复 running 状态的任务。

### 叙事记忆

`NarrativeMemoryService.BuildHierarchicalContext()` 构建四层上下文：
全局摘要 → 弧线摘要（每 10 章） → 近期详细（最近 2 章） → 近期摘要（前 8 章 ≤40 字）

## 开发指南

### 添加新功能

1. `internal/model/` — 定义 GORM 模型
2. `internal/repository/` — 实现数据访问
3. `internal/service/` — 实现业务逻辑（Prompt 模板放 `prompts/`）
4. `internal/handler/` — 创建 Gin 处理器
5. `internal/router/router.go` — 注册路由
6. `cmd/server/main.go` — 完成依赖注入 & AutoMigrate 注册

### 测试

```bash
make test
go test -v ./internal/service/... -run TestTemplateName
```

详细 API 文档见 [`docs/USER_MANUAL.md`](docs/USER_MANUAL.md)。

## 许可证

MIT License — 详见 LICENSE 文件

---

**简影 (InkFrame)** — 让每个人都能创作属于自己的故事
