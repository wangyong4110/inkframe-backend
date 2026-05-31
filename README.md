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

所有 AI API Key（LLM、TTS、图片、视频、音效）均通过管理界面（`/api/v1/model-providers`）配置，**无需**设置环境变量。以下是真正需要的运行时环境变量：

```bash
# 向量数据库（覆盖 config.yaml 中的 vector_db 配置，可选）
export QDRANT_ENDPOINT=http://localhost:6333
export QDRANT_API_KEY=...          # Qdrant Cloud 时填写，本地部署留空
export DASHVECTOR_API_KEY=...      # 使用阿里云 DashVector 时填写

# 素材爬取（可选，不配置则对应爬取来源不可用）
export UNSPLASH_ACCESS_KEY=...
export FREESOUND_API_KEY=...
export PIXABAY_API_KEY=...

# macOS 本地开发：Homebrew Go 路径（仅 macOS 需要）
export GOROOT=/opt/homebrew/Cellar/go/1.24.4/libexec
```

> **已移除的变量**：`KLING_API_KEY`、`ALIYUN_TTS_API_KEY`、`BAIDU_TTS_API_KEY` 等 TTS/视频/图片相关密钥均已统一由数据库管理，通过 AI 模型配置界面录入，不再从环境变量读取。

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

---

**2. 安装 Go 1.21+**

**macOS（Homebrew）：**

```bash
# 安装 Go
brew install go

# Homebrew Go 路径与系统 Go 不同，需将以下两行写入 ~/.zshrc 或 ~/.bash_profile
echo 'export GOROOT=/opt/homebrew/opt/go/libexec' >> ~/.zshrc
echo 'export PATH=$GOROOT/bin:$PATH' >> ~/.zshrc
source ~/.zshrc

# 验证
go version
```

> 如果版本较旧（`brew install go` 拉取的是最新稳定版），也可指定：`brew install go@1.21`，并将 `/opt/homebrew/opt/go@1.21/libexec` 写入 `GOROOT`。

**Linux（官方二进制，推荐）：**

```bash
# 下载并解压（以 1.21.0 amd64 为例，ARM 设备替换为 linux-arm64）
wget https://go.dev/dl/go1.21.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.21.0.linux-amd64.tar.gz

# 写入环境变量（写入 ~/.bashrc 或 /etc/profile.d/go.sh 使其永久生效）
echo 'export PATH=/usr/local/go/bin:$PATH' >> ~/.bashrc
source ~/.bashrc

# 验证
go version
```

Linux（包管理器，版本可能偏旧，不推荐生产）：

```bash
# Ubuntu / Debian
sudo apt update && sudo apt install -y golang-go

# CentOS / RHEL / Fedora
sudo dnf install -y golang
```

**Windows：**

1. 前往 [https://go.dev/dl/](https://go.dev/dl/) 下载 `go1.21.x.windows-amd64.msi`
2. 运行安装包，默认安装到 `C:\Program Files\Go`，安装程序自动配置 `PATH`
3. 打开新 PowerShell 验证：`go version`

---

**3. 安装并初始化 MySQL 8**

**macOS（Homebrew）：**

```bash
brew install mysql@8.0
brew services start mysql@8.0

# 验证连接
mysql -u root -p
```

> 首次启动 MySQL 会生成临时 root 密码，在 `/usr/local/var/mysql/<hostname>.err` 中搜索 `temporary password`，或运行 `mysql_secure_installation` 完成初始化。

**Linux（Ubuntu / Debian）：**

```bash
sudo apt update && sudo apt install -y mysql-server
sudo systemctl enable mysql && sudo systemctl start mysql

# 安全初始化（设置 root 密码、删除匿名用户、禁止远程 root 登录）
sudo mysql_secure_installation

# 以 root 身份进入
sudo mysql
```

**Linux（CentOS / RHEL 8+）：**

```bash
sudo dnf install -y mysql-server
sudo systemctl enable mysqld && sudo systemctl start mysqld

# 查看临时 root 密码
sudo grep 'temporary password' /var/log/mysqld.log

# 安全初始化
sudo mysql_secure_installation
```

**Windows：**

1. 前往 [https://dev.mysql.com/downloads/installer/](https://dev.mysql.com/downloads/installer/) 下载 MySQL Installer
2. 选择 **MySQL Server 8.0**，安装类型选 `Developer Default`
3. 安装完成后，MySQL 服务自动注册并启动

**创建数据库和用户（所有平台通用）：**

```sql
CREATE DATABASE inkframe CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'inkframe'@'localhost' IDENTIFIED BY 'your_password';
GRANT ALL PRIVILEGES ON inkframe.* TO 'inkframe'@'localhost';
FLUSH PRIVILEGES;
EXIT;
```

---

**4. 安装 Redis 7**

**macOS（Homebrew）：**

```bash
brew install redis
brew services start redis

# 验证
redis-cli ping
# 期望输出：PONG
```

**Linux（Ubuntu / Debian）：**

```bash
sudo apt update && sudo apt install -y redis-server

# 编辑配置（可选：绑定本地地址，禁止外部访问）
sudo sed -i 's/^bind 127.0.0.1 -::1/bind 127.0.0.1/' /etc/redis/redis.conf

sudo systemctl enable redis-server && sudo systemctl start redis-server

# 验证
redis-cli ping
```

**Linux（CentOS / RHEL）：**

```bash
sudo dnf install -y redis
sudo systemctl enable redis && sudo systemctl start redis
redis-cli ping
```

**Linux（源码编译，获取最新版）：**

```bash
wget https://download.redis.io/redis-stable.tar.gz
tar xzf redis-stable.tar.gz && cd redis-stable
make && sudo make install
sudo cp redis.conf /etc/redis.conf
# 按需修改 /etc/redis.conf 中的 daemonize yes
redis-server /etc/redis.conf
```

**Windows：** 见"第四节 Windows 本地开发注意事项"。

---

**5. 配置 config.yaml**

```bash
# macOS / Linux
cp config.example.yaml config.yaml

# Windows PowerShell
Copy-Item config.example.yaml config.yaml
```

编辑 `config.yaml`，填写以下核心配置：

```yaml
server:
  port: 8080
  mode: debug        # 开发时用 debug，生产改为 release

database:
  host: localhost
  port: 3306
  user: inkframe
  password: your_password
  name: inkframe
  max_idle_conns: 10
  max_open_conns: 100

redis:
  addr: localhost:6379
  password: ""       # 有密码时填写
  db: 0

storage:
  provider: oss
  oss:
    endpoint: https://oss-cn-shanghai.aliyuncs.com
    bucket: your-bucket
    access_key_id: your-ak
    access_key_secret: your-sk
    base_url: https://your-bucket.oss-cn-shanghai.aliyuncs.com

# 向量存储（可选，不填则禁用语义搜索）
vector_db:
  provider: qdrant
  qdrant:
    endpoint: http://localhost:6333
    api_key: ""
```

---

**6. 安装依赖并启动**

macOS / Linux：

```bash
make deps   # 等价于 go mod download && go mod tidy
make run    # 编译并启动，监听 :8080
```

热重载开发模式（需先安装 [reflex](https://github.com/cespare/reflex)）：

```bash
go install github.com/cespare/reflex@latest
make dev    # 文件变更时自动重新编译
```

Windows（PowerShell，`make` 不可用时）：

```powershell
go mod download
go run ./cmd/server
```

> Windows 安装 `make`：`choco install make`（需先安装 [Chocolatey](https://chocolatey.org/)）

---

**7. 数据库表自动创建**

服务首次启动时，GORM AutoMigrate 自动创建所有 `ink_*` 数据表，**无需**手动执行任何 SQL 建表语句。升级时只新增列，不删除已有数据，安全幂等。

---

**8. 验证服务**

```bash
# macOS / Linux
curl http://localhost:8080/health
# 期望响应：{"status":"ok"}

# Windows PowerShell
Invoke-RestMethod http://localhost:8080/health
```

登录管理界面，前往 **AI 模型 → 添加提供商**，配置至少一个 LLM 提供商（如 DeepSeek），即可开始使用所有 AI 功能。

---

**macOS 常见问题**

| 问题 | 原因 | 解决方案 |
|------|------|----------|
| `go: command not found` | Homebrew Go 路径未写入 PATH | 确认 `~/.zshrc` 中有 `export PATH=$GOROOT/bin:$PATH`，重新 `source ~/.zshrc` |
| MySQL 启动失败 | 端口 3306 被占用 | `lsof -i :3306` 查看占用进程，或修改 `config.yaml` 中的 `port` |
| Redis 连接被拒绝 | 服务未运行 | `brew services list` 确认 redis 状态为 `started` |
| `make: command not found` | Xcode CLT 未安装 | `xcode-select --install` |

**Linux 常见问题**

| 问题 | 原因 | 解决方案 |
|------|------|----------|
| MySQL 无法用密码登录 | 默认 auth_socket 插件 | `ALTER USER 'root'@'localhost' IDENTIFIED WITH mysql_native_password BY 'pwd';` |
| Redis 外部无法访问 | 绑定了 127.0.0.1 | 修改 `/etc/redis/redis.conf` 中 `bind` 项，并配置防火墙 |
| `permission denied` 运行二进制 | 文件权限 | `chmod +x ./bin/inkframe-backend` |
| 端口 8080 被占用 | 其他进程 | `lsof -i :8080` 或修改 `config.yaml` 中 `server.port` |

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

### 四、Windows 本地开发注意事项

> Windows 不支持 `make`，且 Redis 无官方原生版本，建议优先使用 **WSL2 + Docker Desktop** 组合。

#### 推荐工具链

| 工具 | 安装方式 | 说明 |
|------|----------|------|
| Git | [git-scm.com](https://git-scm.com/download/win) | 含 Git Bash，可运行 shell 脚本 |
| Go 1.21+ | [go.dev/dl](https://go.dev/dl/) | 下载 `.msi` 安装包 |
| MySQL 8 | [MySQL Installer](https://dev.mysql.com/downloads/installer/) | 含图形化配置向导 |
| Redis | WSL2 / Docker / Memurai | 见下方说明 |
| make（可选） | `choco install make` | 需先安装 [Chocolatey](https://chocolatey.org/) |
| WSL2（推荐） | `wsl --install` | 运行 Linux 子系统，最接近生产环境 |

#### WSL2 完整流程（推荐）

```powershell
# 1. 安装 WSL2（需重启）
wsl --install

# 2. 进入 Ubuntu 子系统
wsl -d Ubuntu

# 3. 后续步骤与 Linux 完全相同
sudo apt update && sudo apt install -y mysql-server redis-server
sudo service mysql start && sudo service redis start
# 按照"本地开发环境"的 Linux 步骤继续...
```

#### 纯 Windows PowerShell 流程

```powershell
# 安装依赖
go mod download

# 设置环境变量（当前会话）
$env:GIN_MODE = "debug"

# 启动服务
go run ./cmd/server

# 验证
Invoke-RestMethod http://localhost:8080/health
```

#### Windows 常见问题

| 问题 | 原因 | 解决方案 |
|------|------|----------|
| `make: command not found` | Windows 无内置 `make` | 安装 Chocolatey 后执行 `choco install make`，或直接用 `go run` / `go build` |
| MySQL 连接被拒绝 | 服务未启动 | 打开「服务」管理器（`services.msc`），确认 MySQL80 服务已运行 |
| Redis 连接失败 | Windows 无官方 Redis | 使用 WSL2 内的 Redis，或 `docker run -p 6379:6379 redis:7-alpine` |
| 编译报 CGO 错误 | Windows 缺少 C 编译器 | 设置 `$env:CGO_ENABLED=0`（本项目不依赖 CGO） |
| 路径分隔符问题 | Windows 用 `\`，Go 用 `/` | 在代码中使用 `filepath.Join`，配置文件路径统一用正斜线 |

---

### 五、Linux 生产环境部署（systemd）

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

### 六、Nginx 反向代理配置

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

### 七、config.yaml 完整参考

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

### 八、数据库迁移说明

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

### 九、升级部署

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
