# InkFrame Backend

InkFrame 后端服务 — AI 驱动的智能小说创作与视频生成平台

## 功能特性

- **智能小说生成** — 多题材、自动生成高质量中长篇小说，支持大纲、章节逐步创作
- **章节质量控制** — 一致性、逻辑、文风多维度质量评分，支持版本历史与回滚
- **角色 & 世界观管理** — 角色弧光追踪、角色关系图谱、世界观实体管理
- **小说导入** — 支持文件上传、URL 解析、起点/晋江/纵横等平台爬取
- **AI 分镜生成** — 自动将章节拆解为分镜脚本，支持静态/视频两种生成模式
- **视频生成** — 对接 Kling / Seedance，支持草稿/预览/正式三档质量
- **MCP 工具管理** — Model Context Protocol 工具注册、连通性测试、与模型绑定
- **多模型管理** — OpenAI、Claude、Gemini、豆包、DeepSeek、通义千问，支持任务级分配
- **风格控制** — 写作风格、图像风格、视频风格的 Prompt 预设与自定义
- **多租户** — 租户 / 成员 / 配额管理

## 技术栈

| 层次 | 技术 |
|------|------|
| 语言 | Go 1.21+ |
| HTTP 框架 | Gin |
| 数据库 | MySQL 8.0+ (GORM) |
| 缓存 | Redis |
| 向量存储 | Qdrant / Chroma |
| 文件存储 | 阿里云 OSS (HMAC-SHA256 签名) |
| AI 提供商 | OpenAI / Anthropic / Google / 豆包 / DeepSeek / 通义千问 |
| 视频生成 | Kling / Seedance |

## 项目结构

```
inkframe-backend/
├── cmd/server/              # 启动入口，依赖注入 & 路由装配
├── internal/
│   ├── ai/                  # AI 提供商抽象层（openai/claude/doubao/…）
│   ├── config/              # Viper YAML 配置
│   ├── handler/             # Gin HTTP 处理器（一文件对应一领域）
│   ├── middleware/          # CORS / JWT 鉴权 / 日志 / 限流 / 恢复
│   ├── model/               # GORM 数据模型（ink_ 前缀表）
│   ├── repository/          # 数据访问层（含 Redis 缓存）
│   ├── router/              # 所有路由注册
│   ├── service/             # 业务逻辑层
│   │   └── prompts/         # AI Prompt 模板
│   ├── vector/              # 向量存储抽象（Qdrant / Chroma）
│   └── oss/                 # 阿里云 OSS 客户端
├── config.example.yaml      # 配置示例
└── Makefile
```

## 快速开始

### 前置要求

- Go 1.21+
- MySQL 8.0+
- Redis

### 安装与运行

```bash
# 1. 克隆项目
git clone <repo-url>
cd inkframe-backend

# 2. 配置（填入 DB / Redis / AI Key 等）
cp config.example.yaml config.yaml

# 3. 安装依赖
make deps

# 4. 运行
make run
```

### 环境变量（AI API Key）

```bash
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GOOGLE_API_KEY=...
export DOUBAO_API_KEY=...
export DEEPSEEK_API_KEY=...
export QIANWEN_API_KEY=...
export KLING_API_KEY=...
export SEEDANCE_API_KEY=...
export QDRANT_ENDPOINT=http://localhost:6333
export QDRANT_API_KEY=...
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
make build-linux    # 交叉编译 Linux 版本
make migrate-up     # 执行数据库迁移
make migrate-down   # 回滚迁移
make docs           # 生成 Swagger（swag）
```

## API 文档

> 所有受保护接口需要在 `Authorization: Bearer <token>` Header 中携带 JWT。

### 健康检查
```
GET  /health
```

### 认证
```
POST /api/v1/auth/register
POST /api/v1/auth/login
POST /api/v1/auth/refresh
GET  /api/v1/auth/me
```

### 小说
```
GET    /api/v1/novels
POST   /api/v1/novels
GET    /api/v1/novels/:id
PUT    /api/v1/novels/:id
DELETE /api/v1/novels/:id
POST   /api/v1/novels/:id/outline           # 生成大纲
POST   /api/v1/novels/:id/chapters/generate # AI 生成章节
GET    /api/v1/novels/:id/foreshadows       # 伏笔列表
POST   /api/v1/novels/:id/foreshadows/:foreshadow_id/fulfill
GET    /api/v1/novels/:id/timeline          # 时间线
POST   /api/v1/novels/:id/timeline/build
GET    /api/v1/novels/:id/context
POST   /api/v1/novels/:id/generate-video    # 从小说生成视频
```

### 章节
```
GET    /api/v1/novels/:novel_id/chapters
POST   /api/v1/novels/:novel_id/chapters
GET    /api/v1/novels/:novel_id/chapters/:chapter_no
PUT    /api/v1/novels/:novel_id/chapters/:chapter_no
DELETE /api/v1/novels/:novel_id/chapters/:chapter_no
POST   /api/v1/chapters/:id/regenerate
GET    /api/v1/chapters/:id/versions
POST   /api/v1/chapters/:id/versions/:version_no/restore
POST   /api/v1/chapters/:id/quality-check
GET    /api/v1/chapters/:id/quality-report
POST   /api/v1/chapters/:id/approve
POST   /api/v1/chapters/:id/reject
```

### 角色
```
GET    /api/v1/novels/:novel_id/characters
POST   /api/v1/novels/:novel_id/characters
POST   /api/v1/novels/:novel_id/characters/generate
GET    /api/v1/novels/:novel_id/character-arcs
GET    /api/v1/novels/:novel_id/character-arcs/:character_id
PUT    /api/v1/novels/:novel_id/character-arcs/:character_id
GET    /api/v1/characters/:id
PUT    /api/v1/characters/:id
DELETE /api/v1/characters/:id
POST   /api/v1/characters/:id/images
POST   /api/v1/characters/:id/analyze-consistency
```

### 世界观
```
GET    /api/v1/worldviews
POST   /api/v1/worldviews
POST   /api/v1/worldviews/generate
GET    /api/v1/worldviews/:id
PUT    /api/v1/worldviews/:id
DELETE /api/v1/worldviews/:id
GET    /api/v1/worldviews/:id/entities
POST   /api/v1/worldviews/:id/entities
PUT    /api/v1/worldviews/:id/entities/:entity_id
DELETE /api/v1/worldviews/:id/entities/:entity_id
```

### 视频 & 分镜
```
GET    /api/v1/novels/:novel_id/videos
POST   /api/v1/novels/:novel_id/videos           # 创建视频（支持 quality_tier: draft/preview/final）
GET    /api/v1/videos
GET    /api/v1/videos/:id
PUT    /api/v1/videos/:id
DELETE /api/v1/videos/:id
POST   /api/v1/videos/:id/storyboard/generate    # AI 生成分镜
GET    /api/v1/videos/:id/storyboard
PUT    /api/v1/videos/:id/storyboard/:shot_id    # 更新镜头（支持 generation_mode: static/video）
GET    /api/v1/videos/:id/shots
POST   /api/v1/videos/:id/shots/batch-generate   # 批量生成镜头
POST   /api/v1/videos/:id/shots/:shot_id/generate# 单镜头生成
POST   /api/v1/videos/:id/generate               # 启动视频生成
GET    /api/v1/videos/:id/status
POST   /api/v1/videos/:id/stitch                 # 合成最终视频
POST   /api/v1/storyboard/analyze-emotions
POST   /api/v1/video/enhance
POST   /api/v1/consistency/score
```

### AI 模型管理
```
GET    /api/v1/model-providers
POST   /api/v1/model-providers
GET    /api/v1/model-providers/:id
PUT    /api/v1/model-providers/:id
DELETE /api/v1/model-providers/:id
POST   /api/v1/model-providers/:id/test
GET    /api/v1/models
POST   /api/v1/models
GET    /api/v1/models/available/:task_type
POST   /api/v1/models/select
PUT    /api/v1/models/:id
DELETE /api/v1/models/:id
POST   /api/v1/models/:id/test
GET    /api/v1/models/:id/mcp-tools              # 获取模型绑定的 MCP 工具
POST   /api/v1/models/:id/mcp-tools              # 绑定 MCP 工具
DELETE /api/v1/models/:id/mcp-tools/:tool_id     # 解绑
GET    /api/v1/task-configs/:task
PUT    /api/v1/task-configs/:task
```

### MCP 工具（Model Context Protocol）
```
GET    /api/v1/mcp-tools
POST   /api/v1/mcp-tools
PUT    /api/v1/mcp-tools/:id
DELETE /api/v1/mcp-tools/:id
POST   /api/v1/mcp-tools/:id/test               # 连通性探测
GET    /api/v1/mcp-tools/:id/models             # 获取绑定此工具的模型
```

### 导入
```
POST   /api/v1/import/novel
POST   /api/v1/import/novel/file
POST   /api/v1/import/novel/url
POST   /api/v1/import/novel/crawl               # 支持起点/晋江/纵横
POST   /api/v1/import/novel/video
GET    /api/v1/import/status/:task_id
```

### 风格 & 租户
```
GET    /api/v1/styles/default
POST   /api/v1/styles/prompt
GET    /api/v1/styles/presets
POST   /api/v1/styles/presets/:name/apply
GET    /api/v1/tenants
POST   /api/v1/tenants
GET    /api/v1/tenants/:id
...
```

## 数据模型（核心表）

所有表使用 `ink_` 前缀，由 GORM AutoMigrate 在启动时自动创建/更新。

| 表名 | 说明 |
|------|------|
| `ink_novel` | 小说基本信息 |
| `ink_chapter` | 章节内容 & 版本 |
| `ink_character` | 角色设定 |
| `ink_worldview` | 世界观 |
| `ink_worldview_entity` | 世界观实体（势力/地点/物品等）|
| `ink_video` | 视频任务（含 quality_tier）|
| `ink_storyboard_shot` | 分镜镜头（含 generation_mode）|
| `ink_ai_model` | AI 模型配置 |
| `ink_model_provider` | 模型提供商 |
| `ink_mcp_tool` | MCP 工具注册 |
| `ink_model_mcp_binding` | 模型-工具绑定关系 |
| `ink_tenant` | 租户 |
| `ink_user` | 用户 |

## 开发指南

### 添加新 API

1. 在 `internal/model/` 中定义数据模型
2. 在 `internal/repository/` 中实现数据访问
3. 在 `internal/service/` 中实现业务逻辑
4. 在 `internal/handler/` 中创建处理器
5. 在 `internal/router/router.go` 中注册路由
6. 在 `cmd/server/main.go` 中完成依赖注入

### 测试

```bash
make test
# 运行单个测试
go test -v ./internal/service/... -run TestTemplateName
```

## 许可证

MIT License — 详见 LICENSE 文件

---

**InkFrame** — 让每个人都能创作属于自己的故事
