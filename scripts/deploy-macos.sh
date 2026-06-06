#!/usr/bin/env bash
# =============================================================================
#  InkFrame 一键部署脚本 — macOS
#  适用：macOS 12 Monterey 及以上（Intel / Apple Silicon）
#
#  用法：
#    bash scripts/deploy-macos.sh          # 生产模式（构建并启动）
#    bash scripts/deploy-macos.sh --dev    # 开发模式（热更新）
#    bash scripts/deploy-macos.sh --stop   # 停止所有服务
# =============================================================================
set -euo pipefail

# ─── 颜色 ─────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
step()  { echo -e "\n${BOLD}${BLUE}━━━ $* ━━━${NC}"; }
ok()    { echo -e "  ${GREEN}✓${NC} $*"; }

# ─── 路径 ─────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_DIR="$(cd "$BACKEND_DIR/.." && pwd)"
FRONTEND_DIR="$ROOT_DIR/inkframe-frontend"
LOG_DIR="$BACKEND_DIR/logs"
PID_DIR="$BACKEND_DIR/run"

# ─── 版本要求 ─────────────────────────────────────────────────────────────────
GO_VERSION="1.24.4"
NODE_MIN_MAJOR=18

# ─── 模式解析 ─────────────────────────────────────────────────────────────────
MODE="production"
for arg in "$@"; do
  [[ "$arg" == "--dev"  ]] && MODE="development"
  [[ "$arg" == "--stop" ]] && MODE="stop"
done

# =============================================================================
echo -e "\n${BOLD}${CYAN}"
echo "  ╔══════════════════════════════════════╗"
echo "  ║   InkFrame 一键部署  ·  macOS        ║"
echo "  ║   模式: $(printf '%-30s' "$MODE")║"
echo "  ╚══════════════════════════════════════╝"
echo -e "${NC}"

# ─── 停止模式 ─────────────────────────────────────────────────────────────────
if [[ "$MODE" == "stop" ]]; then
  step "停止 InkFrame 服务"
  for f in backend frontend; do
    pid_file="$PID_DIR/${f}.pid"
    if [[ -f "$pid_file" ]]; then
      pid=$(cat "$pid_file")
      if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" && ok "已停止 $f (PID $pid)"
      fi
      rm -f "$pid_file"
    fi
  done
  info "服务已停止"
  exit 0
fi

mkdir -p "$LOG_DIR" "$PID_DIR"

# =============================================================================
step "1 / 7  检查 Homebrew"
# =============================================================================
if ! command -v brew &>/dev/null; then
  info "正在安装 Homebrew..."
  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
  # Apple Silicon 路径
  if [[ -f /opt/homebrew/bin/brew ]]; then
    eval "$(/opt/homebrew/bin/brew shellenv)"
    echo 'eval "$(/opt/homebrew/bin/brew shellenv)"' >> ~/.zprofile
  fi
fi
ok "Homebrew $(brew --version | head -1)"

# =============================================================================
step "2 / 7  安装系统依赖（Go / Node.js / MySQL / Redis）"
# =============================================================================

# ── Go ───────────────────────────────────────────────────────────────────────
if ! command -v go &>/dev/null || ! go version 2>/dev/null | grep -qE "go1\.(2[4-9]|[3-9][0-9])"; then
  info "安装 Go ${GO_VERSION}..."
  brew install go 2>&1 | tail -3
  # 刷新 PATH
  export PATH="$(brew --prefix go)/bin:$PATH"
fi
ok "Go $(go version | awk '{print $3}')"

# ── Node.js ──────────────────────────────────────────────────────────────────
if ! command -v node &>/dev/null; then
  info "安装 Node.js LTS..."
  brew install node 2>&1 | tail -3
fi
NODE_MAJOR=$(node -e "process.stdout.write(process.version.replace('v','').split('.')[0])")
if (( NODE_MAJOR < NODE_MIN_MAJOR )); then
  warn "Node.js ${NODE_MAJOR} < ${NODE_MIN_MAJOR}，升级..."
  brew upgrade node 2>&1 | tail -3
fi
ok "Node.js $(node --version)  npm $(npm --version)"

# ── MySQL ────────────────────────────────────────────────────────────────────
if ! command -v mysql &>/dev/null; then
  info "安装 MySQL..."
  brew install mysql 2>&1 | tail -3
fi
ok "MySQL $(mysql --version | awk '{print $5}' | tr -d ',')"

# ── Redis ────────────────────────────────────────────────────────────────────
if ! command -v redis-server &>/dev/null; then
  info "安装 Redis..."
  brew install redis 2>&1 | tail -3
fi
ok "Redis $(redis-server --version | awk '{print $3}' | cut -d= -f2)"

# =============================================================================
step "3 / 7  启动 MySQL / Redis 后台服务"
# =============================================================================
brew services start mysql 2>/dev/null  || true
brew services start redis 2>/dev/null  || true
sleep 1  # 等待就绪

# 验证
if mysql -u root --connect-timeout=5 -e "SELECT 1" &>/dev/null 2>&1; then
  ok "MySQL 服务运行中"
else
  warn "MySQL 无法以 root（空密码）连接，如已设置密码后续步骤会提示输入"
fi
redis-cli ping &>/dev/null && ok "Redis 服务运行中" || warn "Redis 连接失败，请检查服务"

# =============================================================================
step "4 / 7  配置 config.yaml"
# =============================================================================
CONFIG="$BACKEND_DIR/config.yaml"
if [[ ! -f "$CONFIG" ]]; then
  cp "$BACKEND_DIR/config.example.yaml" "$CONFIG"
  info "已从 config.example.yaml 创建 config.yaml"
fi

# 读取数据库配置
echo ""
echo -e "${CYAN}请输入数据库配置（直接回车使用括号内默认值）：${NC}"
read -rp "  MySQL 主机    [localhost]: " db_host;   db_host="${db_host:-localhost}"
read -rp "  MySQL 端口    [3306]: "      db_port;   db_port="${db_port:-3306}"
read -rp "  数据库名称    [inkframe]: "  db_name;   db_name="${db_name:-inkframe}"
read -rp "  用户名        [root]: "      db_user;   db_user="${db_user:-root}"
read -rsp "  密码          (空=无密码): " db_pass; echo
read -rp "  后端端口      [8080]: "      be_port;   be_port="${be_port:-8080}"
read -rp "  前端端口      [3000]: "      fe_port;   fe_port="${fe_port:-3000}"

# 更新 config.yaml（macOS sed 需要 '' 参数）
SED="sed -i ''"
$SED "s|host: \"localhost\"        # \[必填\]|host: \"${db_host}\"|" "$CONFIG"
$SED "s|port: 3306|port: ${db_port}|" "$CONFIG"
$SED "s|database: \"inkframe\"     # \[必填\]|database: \"${db_name}\"|" "$CONFIG"
$SED "s|username: \"root\"         # \[必填\]|username: \"${db_user}\"|" "$CONFIG"
$SED "s|password: \"\"             # \[必填\]|password: \"${db_pass}\"|" "$CONFIG"
$SED "s|port: 8080|port: ${be_port}|" "$CONFIG"

# 生产模式自动生成 JWT secret
if [[ "$MODE" == "production" ]]; then
  jwt_secret=$(openssl rand -base64 48 | tr -d '\n')
  $SED "s|jwt_secret: \"inkframe-dev-secret-change-in-production-2024\"|jwt_secret: \"${jwt_secret}\"|" "$CONFIG"
fi
ok "config.yaml 已更新"

# 创建数据库
MYSQL_CMD="mysql -h \"${db_host}\" -P \"${db_port}\" -u \"${db_user}\""
[[ -n "$db_pass" ]] && MYSQL_CMD="$MYSQL_CMD -p\"${db_pass}\""
if eval "$MYSQL_CMD" -e "CREATE DATABASE IF NOT EXISTS \`${db_name}\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" 2>/dev/null; then
  ok "数据库 '${db_name}' 就绪"
else
  warn "数据库创建失败，请手动执行：CREATE DATABASE \`${db_name}\` CHARACTER SET utf8mb4;"
fi

# =============================================================================
step "5 / 7  构建后端"
# =============================================================================
cd "$BACKEND_DIR"
info "下载 Go 依赖..."
go mod download
go mod tidy

if [[ "$MODE" == "production" ]]; then
  info "编译后端..."
  mkdir -p bin
  go build -ldflags "-s -w" -o bin/inkframe-backend ./cmd/server
  ok "后端二进制: $BACKEND_DIR/bin/inkframe-backend"
else
  ok "开发模式：跳过编译，将使用 go run"
fi

# =============================================================================
step "6 / 7  安装前端依赖并构建"
# =============================================================================
if [[ ! -d "$FRONTEND_DIR" ]]; then
  error "前端目录不存在: $FRONTEND_DIR"
  error "请确认 inkframe-frontend 与 inkframe-backend 在同一父目录下"
  exit 1
fi

cd "$FRONTEND_DIR"
info "安装 npm 依赖..."
npm install --prefer-offline 2>&1 | tail -5

if [[ "$MODE" == "production" ]]; then
  info "构建前端（nuxt build）..."
  NUXT_PUBLIC_API_BASE="http://localhost:${be_port}/api/v1" npm run build 2>&1 | tail -10
  ok "前端构建完成: $FRONTEND_DIR/.output/"
else
  ok "开发模式：跳过构建，将使用 npm run dev"
fi

# =============================================================================
step "7 / 7  启动服务"
# =============================================================================
cd "$BACKEND_DIR"

# 停掉已有进程
for f in backend frontend; do
  pid_file="$PID_DIR/${f}.pid"
  if [[ -f "$pid_file" ]]; then
    pid=$(cat "$pid_file")
    kill "$pid" 2>/dev/null || true
    rm -f "$pid_file"
  fi
done

if [[ "$MODE" == "production" ]]; then
  # 后端
  nohup ./bin/inkframe-backend > "$LOG_DIR/backend.log" 2>&1 &
  echo $! > "$PID_DIR/backend.pid"
  ok "后端已启动 (PID $!，日志: logs/backend.log)"

  # 前端
  cd "$FRONTEND_DIR"
  nohup node .output/server/index.mjs \
    > "$LOG_DIR/frontend.log" 2>&1 &
  echo $! > "$PID_DIR/frontend.pid"
  ok "前端已启动 (PID $!，日志: logs/frontend.log)"
else
  # 开发模式 —— tmux 分窗口（如无 tmux 则后台启动）
  if command -v tmux &>/dev/null; then
    tmux new-session -d -s inkframe -x 220 -y 50 2>/dev/null || true
    tmux send-keys -t inkframe "cd '$BACKEND_DIR' && go run ./cmd/server 2>&1 | tee logs/backend.log" C-m
    tmux split-window -h -t inkframe
    tmux send-keys -t inkframe "cd '$FRONTEND_DIR' && npm run dev 2>&1 | tee '$LOG_DIR/frontend.log'" C-m
    ok "已在 tmux 会话 'inkframe' 中启动（运行 tmux attach -t inkframe 查看）"
  else
    cd "$BACKEND_DIR"
    nohup go run ./cmd/server > "$LOG_DIR/backend.log" 2>&1 &
    echo $! > "$PID_DIR/backend.pid"
    cd "$FRONTEND_DIR"
    PORT=$fe_port nohup npm run dev > "$LOG_DIR/frontend.log" 2>&1 &
    echo $! > "$PID_DIR/frontend.pid"
    ok "服务已在后台启动（日志: logs/）"
  fi
fi

# 等待后端就绪
info "等待后端就绪..."
for i in $(seq 1 20); do
  if curl -sf "http://localhost:${be_port}/health" &>/dev/null; then
    break
  fi
  sleep 1
done

echo ""
echo -e "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${BOLD}${GREEN}  ✅  InkFrame 部署成功！${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
if [[ "$MODE" == "production" ]]; then
  echo -e "  前端地址: ${CYAN}http://localhost:${fe_port}${NC}"
else
  echo -e "  前端地址: ${CYAN}http://localhost:${fe_port}${NC}  (开发模式)"
fi
echo -e "  后端地址: ${CYAN}http://localhost:${be_port}${NC}"
echo -e "  日志目录: ${CYAN}$LOG_DIR${NC}"
echo -e "  停止服务: ${CYAN}bash scripts/deploy-macos.sh --stop${NC}"
echo ""
