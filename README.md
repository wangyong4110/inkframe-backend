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

---

## 部署指南

### 一、本地开发环境

#### 前置依赖

| 依赖 | 最低版本 | 说明 |
|------|----------|------|
| Go | 1.21+ | 编程语言运行时 |
| MySQL | 8.0+ | 主数据库（utf8mb4） |
| Redis | 7.0+ | 缓存与任务队列 |
| Qdrant | 1.7+（可选） | 向量存储，语义搜索功能 |

#### 步骤

**1. 克隆代码仓库**

```bash
git clone <repo-url>
cd inkframe-backend
```

**2. 安装 Go**

macOS（使用 Homebrew）：

```bash
brew install go
# Homebrew 安装路径与系统 Go 不同，需指定 GOROOT
export GOROOT=/opt/homebrew/Cellar/go/1.24.4/libexec
export PATH=$GOROOT/bin:$PATH
```

Linux：

```bash
wget https://go.dev/dl/go1.21.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.0.linux-amd64.tar.gz
export PATH=/usr/local/go/bin:$PATH
```

**3. 安装并初始化 MySQL 8**

```bash
# macOS
brew install mysql@8.0
brew services start mysql@8.0

# Ubuntu / Debian
sudo apt update && sudo apt install -y mysql-server
sudo systemctl start mysql
```

创建数据库和用户：

```sql
CREATE DATABASE inkframe CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'inkframe'@'localhost' IDENTIFIED BY 'your_password';
GRANT ALL PRIVILEGES ON inkframe.* TO 'inkframe'@'localhost';
FLUSH PRIVILEGES;
```

**4. 安装 Redis 7**

```bash
# macOS
brew install redis
brew services start redis

# Ubuntu / Debian
sudo apt install -y redis-server
sudo systemctl start redis
```

**5. 配置 config.yaml**

```bash
cp config.example.yaml config.yaml
```

编辑 `config.yaml`，填写以下核心配置：

```yaml
server:
  port: 8080
  mode: debug

database:
  host: localhost
  port: 3306
  user: inkframe
  password: your_password
  name: inkframe

redis:
  addr: localhost:6379
  password: ""
  db: 0

storage:
  provider: oss
  oss:
    endpoint: https://oss-cn-shanghai.aliyuncs.com
    bucket: your-bucket
    access_key_id: ...
    access_key_secret: ...
```

**6. 安装依赖并启动**

```bash
make deps
make run
```

**7. 数据库表自动创建**

服务首次启动时，GORM AutoMigrate 会自动创建所有 `ink_*` 数据表，无需手动执行 SQL 建表语句。

**8. 验证服务**

```bash
curl http://localhost:8080/health
# 期望响应：{"status":"ok"}
```

---

### 二、Docker 单容器部署

适用于快速试用或简单场景（需自行准备外部 MySQL 和 Redis）。

#### Dockerfile

在项目根目录创建 `Dockerfile`：

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o inkframe-backend ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=builder /app/inkframe-backend .
COPY --from=builder /app/config.example.yaml ./config.example.yaml
COPY --from=builder /app/internal/service/prompts ./internal/service/prompts
EXPOSE 8080
CMD ["./inkframe-backend"]
```

#### 构建与运行

```bash
# 构建镜像
docker build -t inkframe-backend:latest .

# 运行容器（挂载本地 config.yaml）
docker run -d \
  --name inkframe-backend \
  -p 8080:8080 \
  -v $(pwd)/config.yaml:/app/config.yaml \
  inkframe-backend:latest

# 查看日志
docker logs -f inkframe-backend
```

> 注意：容器内 config.yaml 中 `database.host` 应填写宿主机可访问的 MySQL 地址，`redis.addr` 同理。如果 MySQL/Redis 也跑在 Docker 中，推荐改用下方的 Docker Compose 方案。

---

### 三、Docker Compose 完整部署（推荐）

一键启动后端服务及所有依赖（MySQL、Redis、Qdrant），适合本地演示和测试环境。

#### docker-compose.yml

在项目根目录创建 `docker-compose.yml`：

```yaml
version: "3.9"

services:
  backend:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./config.yaml:/app/config.yaml
    depends_on:
      mysql:
        condition: service_healthy
      redis:
        condition: service_healthy
    restart: unless-stopped
    environment:
      - TZ=Asia/Shanghai

  mysql:
    image: mysql:8.0
    environment:
      MYSQL_ROOT_PASSWORD: rootpassword
      MYSQL_DATABASE: inkframe
      MYSQL_USER: inkframe
      MYSQL_PASSWORD: inkframe123
    volumes:
      - mysql_data:/var/lib/mysql
    ports:
      - "3306:3306"
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "localhost"]
      interval: 10s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  redis:
    image: redis:7-alpine
    volumes:
      - redis_data:/data
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 3s
      retries: 3
    restart: unless-stopped

  qdrant:
    image: qdrant/qdrant:v1.7.4
    volumes:
      - qdrant_data:/qdrant/storage
    ports:
      - "6333:6333"
    restart: unless-stopped

volumes:
  mysql_data:
  redis_data:
  qdrant_data:
```

#### config.yaml 中的服务地址

使用 Docker Compose 时，各服务通过容器名互相访问，需在 `config.yaml` 中对应修改：

```yaml
database:
  host: mysql          # 容器服务名
  port: 3306
  user: inkframe
  password: inkframe123
  name: inkframe

redis:
  addr: redis:6379     # 容器服务名

vector_db:
  provider: qdrant
  qdrant:
    endpoint: http://qdrant:6333   # 容器服务名
```

#### 启动与管理

```bash
# 准备配置文件
cp config.example.yaml config.yaml
# 根据上方说明编辑 database / redis / storage 等字段

# 启动所有服务（后台运行）
docker compose up -d

# 查看后端日志
docker compose logs -f backend

# 查看所有服务状态
docker compose ps

# 停止所有服务
docker compose down

# 停止并清除数据卷（慎用，会清空数据库）
docker compose down -v
```

---

### 四、Linux 生产环境部署（systemd）

适用于正式生产服务器，以二进制文件 + systemd 守护进程方式运行。

#### 步骤

**1. 交叉编译 Linux 二进制**

在 macOS 或 CI 环境中执行：

```bash
make build-linux
# 输出：./bin/inkframe-backend（linux/amd64 静态二进制）
```

**2. 上传文件到服务器**

```bash
# 上传二进制
scp ./bin/inkframe-backend user@your-server:/opt/inkframe/

# 上传配置文件
scp config.yaml user@your-server:/opt/inkframe/

# 上传 Prompt 模板（服务运行时需要）
scp -r internal/service/prompts user@your-server:/opt/inkframe/internal/service/
```

**3. 在服务器上创建运行用户**

```bash
sudo useradd -r -s /usr/sbin/nologin inkframe
sudo chown -R inkframe:inkframe /opt/inkframe
sudo chmod +x /opt/inkframe/inkframe-backend
```

**4. 创建 systemd 服务文件**

```bash
sudo vi /etc/systemd/system/inkframe-backend.service
```

内容如下：

```ini
[Unit]
Description=InkFrame Backend Service
After=network.target mysql.service redis.service

[Service]
Type=simple
User=inkframe
WorkingDirectory=/opt/inkframe
ExecStart=/opt/inkframe/inkframe-backend
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=inkframe-backend
Environment=TZ=Asia/Shanghai
Environment=GIN_MODE=release

[Install]
WantedBy=multi-user.target
```

**5. 启用并启动服务**

```bash
# 重新加载 systemd 配置
sudo systemctl daemon-reload

# 设置开机自启
sudo systemctl enable inkframe-backend

# 启动服务
sudo systemctl start inkframe-backend

# 查看服务状态
sudo systemctl status inkframe-backend

# 实时查看日志
sudo journalctl -u inkframe-backend -f

# 查看最近 100 条日志
sudo journalctl -u inkframe-backend -n 100
```

---

### 五、Nginx 反向代理配置

在 Nginx 前端代理 InkFrame 后端，支持 HTTPS 和 WebSocket。

**创建站点配置文件：**

```bash
sudo vi /etc/nginx/sites-available/inkframe
```

```nginx
upstream inkframe_backend {
    server 127.0.0.1:8080;
    keepalive 32;
}

server {
    listen 80;
    server_name api.yourdomain.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name api.yourdomain.com;

    ssl_certificate     /etc/letsencrypt/live/api.yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.yourdomain.com/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;

    client_max_body_size 100m;

    location / {
        proxy_pass         http://inkframe_backend;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade $http_upgrade;
        proxy_set_header   Connection "upgrade";
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_set_header   X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;
        proxy_connect_timeout 10s;
    }

    location /health {
        proxy_pass http://inkframe_backend;
        access_log off;
    }
}
```

**启用站点并重载 Nginx：**

```bash
sudo ln -s /etc/nginx/sites-available/inkframe /etc/nginx/sites-enabled/
sudo nginx -t          # 检查配置语法
sudo systemctl reload nginx
```

**使用 Let's Encrypt 申请免费 SSL 证书：**

```bash
sudo apt install -y certbot python3-certbot-nginx
sudo certbot --nginx -d api.yourdomain.com
```

---

### 六、config.yaml 完整参考

以下为带注释的完整配置文件参考，所有字段均有说明：

```yaml
server:
  port: 8080              # HTTP 监听端口
  mode: release           # debug / release / test

database:
  host: localhost         # MySQL 主机地址
  port: 3306              # MySQL 端口
  user: inkframe          # 数据库用户名
  password: ""            # 数据库密码
  name: inkframe          # 数据库名
  max_idle_conns: 10      # 最大空闲连接数
  max_open_conns: 100     # 最大活跃连接数

redis:
  addr: localhost:6379    # Redis 地址（host:port）
  password: ""            # Redis 密码（无则留空）
  db: 0                   # Redis 数据库编号（0~15）

storage:
  provider: oss           # 存储提供商：oss（阿里云）
  oss:
    endpoint: https://oss-cn-shanghai.aliyuncs.com   # OSS Endpoint
    bucket: inkframe-assets                          # Bucket 名称
    access_key_id: ""                                # AccessKey ID
    access_key_secret: ""                            # AccessKey Secret
    base_url: https://your-bucket.oss-cn-shanghai.aliyuncs.com  # 公网访问 URL 前缀

vector_db:
  provider: qdrant        # 向量存储：qdrant / chroma / dashvector / none
  qdrant:
    endpoint: http://localhost:6333   # Qdrant 服务地址
    api_key: ""                       # Qdrant API Key（无认证则留空）
    collection: inkframe_knowledge    # 集合名称
  dashvector:
    api_key: ""           # 阿里云 DashVector API Key
    endpoint: ""          # DashVector Endpoint

ai:
  image_concurrency: 2    # 并发图片生成请求数（根据 API 配额调整）
  video_concurrency: 1    # 并发视频生成请求数（视频生成较耗时，建议保持为 1）
  embedding_model: ""     # 向量嵌入模型名称（留空则禁用语义搜索功能）

jwt:
  secret: "change-this-in-production"   # JWT 签名密钥（生产环境务必修改为随机长字符串）
  expiry_hours: 72                       # JWT 有效期（小时）

log:
  level: info             # 日志级别：debug / info / warn / error
  format: json            # 日志格式：json（结构化，生产推荐） / text（可读性好，开发推荐）
```

> **注意：** AI 提供商的 API Key（OpenAI、Claude、豆包、Kling 等）统一通过管理界面（`/api/v1/model-providers`）配置，不在 `config.yaml` 中填写。

---

### 七、数据库迁移说明

#### 自动迁移

服务每次启动时，GORM AutoMigrate 会自动执行：
- 新增数据表（如果不存在）
- 新增列（如果该列在表中不存在）
- **不会删除已有列或数据**，升级安全

#### 手动迁移

如需执行手动 SQL 迁移脚本（位于 `./migrations/` 目录）：

```bash
make migrate-up     # 向前执行迁移
make migrate-down   # 回滚最近一次迁移
```

#### 升级前备份

强烈建议在升级前对数据库进行完整备份：

```bash
mysqldump -u inkframe -p inkframe > backup_$(date +%Y%m%d).sql
```

---

### 八、升级部署

以下为在 Linux 生产环境（systemd 方式）的完整升级流程：

```bash
# 步骤 1：备份数据库
mysqldump -u inkframe -p inkframe > backup_$(date +%Y%m%d_%H%M%S).sql

# 步骤 2：拉取新代码并重新编译
git pull
make build-linux

# 步骤 3：上传新二进制到服务器
scp ./bin/inkframe-backend user@server:/opt/inkframe/inkframe-backend.new

# 步骤 4：零停机切换（原子替换 + 重启）
ssh user@server "
  mv /opt/inkframe/inkframe-backend /opt/inkframe/inkframe-backend.bak && \
  mv /opt/inkframe/inkframe-backend.new /opt/inkframe/inkframe-backend && \
  sudo systemctl restart inkframe-backend && \
  sleep 3 && systemctl is-active inkframe-backend
"
```

如果升级后服务异常，可快速回滚：

```bash
ssh user@server "
  sudo systemctl stop inkframe-backend && \
  mv /opt/inkframe/inkframe-backend /opt/inkframe/inkframe-backend.failed && \
  mv /opt/inkframe/inkframe-backend.bak /opt/inkframe/inkframe-backend && \
  sudo systemctl start inkframe-backend
"
```

---

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
