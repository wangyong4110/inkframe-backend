#!/usr/bin/env bash
# =============================================================================
#  InkFrame 一键部署脚本 — Linux
#  适用：Ubuntu 20.04+  /  Debian 11+  /  AlmaLinux 8+  /  Rocky Linux 8+  /  RHEL 8+
#
#  用法：
#    bash scripts/deploy-linux.sh          # 生产模式
#    bash scripts/deploy-linux.sh --dev    # 开发模式（热更新）
#    bash scripts/deploy-linux.sh --stop   # 停止所有服务
#    sudo bash scripts/deploy-linux.sh     # 需要 root 权限安装系统包
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

# ─── 版本 ─────────────────────────────────────────────────────────────────────
GO_VERSION="1.24.4"
NODE_LTS_MAJOR=20
NODE_MIN_MAJOR=18

# ─── 模式解析 ─────────────────────────────────────────────────────────────────
MODE="production"
for arg in "$@"; do
  [[ "$arg" == "--dev"  ]] && MODE="development"
  [[ "$arg" == "--stop" ]] && MODE="stop"
done

# ─── 权限检测 ─────────────────────────────────────────────────────────────────
HAS_SUDO=false
SUDO=""
if [[ $EUID -eq 0 ]]; then
  HAS_SUDO=true
elif command -v sudo &>/dev/null && sudo -n true 2>/dev/null; then
  HAS_SUDO=true; SUDO="sudo"
fi

run_privileged() {
  if $HAS_SUDO; then
    $SUDO "$@"
  else
    error "需要 root 权限执行: $*"
    error "请以 root 运行，或先执行 sudo -v"
    exit 1
  fi
}

# ─── sed 转义辅助（避免变量中的特殊字符破坏 sed 命令） ───────────────────────
# 对 sed 替换字段（使用 | 作为分隔符）中的特殊字符进行转义：\ & |
sed_esc() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/&/\\&/g; s/|/\\|/g'
}

# =============================================================================
echo -e "\n${BOLD}${CYAN}"
echo "  ╔══════════════════════════════════════╗"
echo "  ║   InkFrame 一键部署  ·  Linux        ║"
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
  # 同时检查 systemd 服务（如可用）
  if command -v systemctl &>/dev/null; then
    for svc in inkframe-backend inkframe-frontend; do
      if systemctl is-active --quiet "$svc" 2>/dev/null; then
        run_privileged systemctl stop "$svc"
        ok "已停止 systemd 服务 $svc"
      fi
    done
  fi
  info "服务已停止"
  exit 0
fi

mkdir -p "$LOG_DIR" "$PID_DIR" || {
  error "无法创建目录 $LOG_DIR / $PID_DIR"
  exit 1
}

# =============================================================================
step "1 / 7  检测 Linux 发行版"
# =============================================================================
if [[ -f /etc/os-release ]]; then
  . /etc/os-release
  DISTRO_ID="${ID:-unknown}"
  DISTRO_ID_LIKE="${ID_LIKE:-}"
else
  DISTRO_ID="unknown"
  DISTRO_ID_LIKE=""
fi

# 归一化为 debian / rhel 两大系
PKG_FAMILY="unknown"
case "$DISTRO_ID" in
  ubuntu|debian|linuxmint|pop)         PKG_FAMILY="debian" ;;
  centos|rhel|almalinux|rocky|fedora)  PKG_FAMILY="rhel"   ;;
  *)
    if echo "$DISTRO_ID_LIKE" | grep -qi "debian"; then PKG_FAMILY="debian"
    elif echo "$DISTRO_ID_LIKE" | grep -qi "rhel\|fedora"; then PKG_FAMILY="rhel"
    fi ;;
esac

if [[ "$PKG_FAMILY" == "unknown" ]]; then
  error "不支持的发行版: $DISTRO_ID"
  error "已测试: Ubuntu 20.04+, Debian 11+, AlmaLinux 8+, Rocky Linux 8+, RHEL 8+, Fedora 36+"
  exit 1
fi
ok "发行版: ${PRETTY_NAME:-$DISTRO_ID}  (包系: $PKG_FAMILY)"

# =============================================================================
step "2 / 7  安装系统依赖"
# =============================================================================
if [[ "$PKG_FAMILY" == "debian" ]]; then
  run_privileged apt-get update -qq
  run_privileged apt-get install -y -q curl wget git make gcc build-essential openssl ca-certificates gnupg lsb-release
else
  run_privileged dnf install -y curl wget git make gcc openssl ca-certificates gnupg 2>/dev/null \
    || run_privileged yum install -y curl wget git make gcc openssl ca-certificates gnupg
fi
ok "基础工具已就绪"

# ── Go ───────────────────────────────────────────────────────────────────────
INSTALLED_GO_OK=false
if command -v go &>/dev/null; then
  # grep -oE 'go[0-9]+\.[0-9]+' 只取 major.minor（如 go1.24），不含 patch
  go_ver=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | tr -d go)
  go_major=${go_ver%%.*}
  go_minor=${go_ver#*.}
  (( go_major > 1 || (go_major == 1 && go_minor >= 24) )) && INSTALLED_GO_OK=true
fi

if ! $INSTALLED_GO_OK; then
  info "安装 Go ${GO_VERSION}..."
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64)  GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    *)
      error "不支持的架构: $ARCH"
      exit 1 ;;
  esac
  GOTAR="go${GO_VERSION}.linux-${GOARCH}.tar.gz"

  info "下载 Go $GO_VERSION..."
  wget -q --https-only --show-progress -O "/tmp/$GOTAR" \
    "https://go.dev/dl/$GOTAR"

  info "验证 SHA256 校验和..."
  EXPECTED_SHA=$(wget -q --https-only -O- "https://go.dev/dl/$GOTAR.sha256")
  ACTUAL_SHA=$(sha256sum "/tmp/$GOTAR" | awk '{print $1}')
  if [[ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]]; then
    error "Go 安装包 SHA256 校验失败，可能已损坏或被篡改，中止安装。"
    rm -f "/tmp/$GOTAR"
    exit 1
  fi
  ok "SHA256 校验通过"

  run_privileged rm -rf /usr/local/go
  run_privileged tar -C /usr/local -xzf "/tmp/$GOTAR"
  rm -f "/tmp/$GOTAR"
  # 写入全局 profile
  if [[ ! -f /etc/profile.d/go.sh ]]; then
    echo 'export PATH=$PATH:/usr/local/go/bin' | run_privileged tee /etc/profile.d/go.sh > /dev/null
  fi
  export PATH=$PATH:/usr/local/go/bin
fi
ok "Go $(go version | awk '{print $3}')"

# ── Node.js ──────────────────────────────────────────────────────────────────
NEED_NODE=true
if command -v node &>/dev/null; then
  NODE_MAJOR=$(node -e "process.stdout.write(process.version.replace('v','').split('.')[0])")
  (( NODE_MAJOR >= NODE_MIN_MAJOR )) && NEED_NODE=false
fi

if $NEED_NODE; then
  info "安装 Node.js ${NODE_LTS_MAJOR}..."
  # 下载 setup 脚本到临时文件后再执行，避免直接管道执行远程脚本
  SETUP_SCRIPT="/tmp/nodesource_setup_${NODE_LTS_MAJOR}.sh"
  if [[ "$PKG_FAMILY" == "debian" ]]; then
    curl -fsSL --proto '=https' \
      "https://deb.nodesource.com/setup_${NODE_LTS_MAJOR}.x" \
      -o "$SETUP_SCRIPT"
    run_privileged bash "$SETUP_SCRIPT"
    rm -f "$SETUP_SCRIPT"
    run_privileged apt-get install -y -q nodejs
  else
    curl -fsSL --proto '=https' \
      "https://rpm.nodesource.com/setup_${NODE_LTS_MAJOR}.x" \
      -o "$SETUP_SCRIPT"
    run_privileged bash "$SETUP_SCRIPT"
    rm -f "$SETUP_SCRIPT"
    run_privileged dnf install -y nodejs 2>/dev/null || run_privileged yum install -y nodejs
  fi
fi
ok "Node.js $(node --version)  npm $(npm --version)"

# ── MySQL ────────────────────────────────────────────────────────────────────
if ! command -v mysqld &>/dev/null && ! command -v mysqld_safe &>/dev/null; then
  info "安装 MySQL..."
  if [[ "$PKG_FAMILY" == "debian" ]]; then
    run_privileged apt-get install -y -q mysql-server
  else
    if ! rpm -q mysql-community-server &>/dev/null 2>&1; then
      MYSQL_RPM_URL="https://dev.mysql.com/get/mysql80-community-release-el9-1.noarch.rpm"
      wget -q -O /tmp/mysql-repo.rpm "$MYSQL_RPM_URL"
      run_privileged rpm -ivh /tmp/mysql-repo.rpm 2>/dev/null || true
      rm -f /tmp/mysql-repo.rpm
    fi
    run_privileged dnf install -y mysql-community-server 2>/dev/null \
      || run_privileged yum install -y mysql-community-server
  fi
fi
ok "MySQL 已安装"

# ── Redis ────────────────────────────────────────────────────────────────────
if ! command -v redis-server &>/dev/null; then
  info "安装 Redis..."
  if [[ "$PKG_FAMILY" == "debian" ]]; then
    run_privileged apt-get install -y -q redis-server
  else
    run_privileged dnf install -y redis 2>/dev/null || run_privileged yum install -y redis
  fi
fi
ok "Redis 已安装"

# =============================================================================
step "3 / 7  启动 MySQL / Redis 服务"
# =============================================================================
if command -v systemctl &>/dev/null; then
  for svc in mysql mysqld redis redis-server; do
    systemctl list-unit-files "${svc}.service" &>/dev/null || continue
    run_privileged systemctl enable --now "$svc" 2>/dev/null || true
  done
else
  warn "systemd 不可用（容器环境？），请手动确认 MySQL 和 Redis 已运行"
fi

sleep 2

# MySQL 初始化 root 密码（仅首次安装时 root 无密码）
if mysql -u root --connect-timeout=3 -e "SELECT 1" &>/dev/null 2>&1; then
  ok "MySQL 服务运行中（root 无密码）"
elif [[ -f /var/log/mysqld.log ]]; then
  TMP_PASS=$(grep -oP '(?<=temporary password is generated for root@localhost: )\S+' /var/log/mysqld.log | tail -1 || true)
  if [[ -n "$TMP_PASS" ]]; then
    warn "MySQL 8 检测到临时 root 密码，请在配置步骤中输入新密码"
  fi
  ok "MySQL 服务运行中"
else
  ok "MySQL 服务运行中"
fi

redis-cli ping &>/dev/null && ok "Redis 服务运行中" || warn "Redis 连接失败，请手动启动"

# =============================================================================
step "4 / 7  配置 config.yaml"
# =============================================================================
CONFIG="$BACKEND_DIR/config.yaml"
if [[ ! -f "$CONFIG" ]]; then
  cp "$BACKEND_DIR/config.example.yaml" "$CONFIG"
  info "已从 config.example.yaml 创建 config.yaml"
else
  # 重复运行时备份现有配置
  cp "$CONFIG" "${CONFIG}.bak.$(date +%s)"
  info "已备份现有 config.yaml"
fi

echo ""
echo -e "${CYAN}请输入数据库配置（直接回车使用括号内默认值）：${NC}"
read -rp "  MySQL 主机    [localhost]: "  db_host;  db_host="${db_host:-localhost}"
read -rp "  MySQL 端口    [3306]: "       db_port;  db_port="${db_port:-3306}"
read -rp "  数据库名称    [inkframe]: "   db_name;  db_name="${db_name:-inkframe}"
read -rp "  用户名        [root]: "       db_user;  db_user="${db_user:-root}"
read -rsp "  密码          (空=无密码): " db_pass; echo
read -rp "  后端端口      [8080]: "       be_port;  be_port="${be_port:-8080}"
read -rp "  前端端口      [3000]: "       fe_port;  fe_port="${fe_port:-3000}"

# 转义变量中的 sed 特殊字符（& \ |），防止注入
sed -i "s|host: \"localhost\"        # \[必填\]|host: \"$(sed_esc "$db_host")\"|"     "$CONFIG"
sed -i "s|port: 3306|port: ${db_port}|"                                                "$CONFIG"
sed -i "s|database: \"inkframe\"     # \[必填\]|database: \"$(sed_esc "$db_name")\"|" "$CONFIG"
sed -i "s|username: \"root\"         # \[必填\]|username: \"$(sed_esc "$db_user")\"|" "$CONFIG"
sed -i "s|password: \"\"             # \[必填\]|password: \"$(sed_esc "$db_pass")\"|" "$CONFIG"
sed -i "s|port: 8080|port: ${be_port}|"                                                "$CONFIG"
if [[ "$MODE" == "production" ]]; then
  JWT=$(openssl rand -base64 48 | tr -d '\n')
  sed -i "s|jwt_secret: \"inkframe-dev-secret-change-in-production-2024\"|jwt_secret: \"$(sed_esc "$JWT")\"|" "$CONFIG"
  unset JWT
fi
ok "config.yaml 已更新"

# 创建数据库：密码通过 --defaults-extra-file 传入，不在进程参数中暴露
_MYSQL_CNF_FILE=""
if [[ -n "$db_pass" ]]; then
  _MYSQL_CNF_FILE=$(mktemp)
  chmod 600 "$_MYSQL_CNF_FILE"
  printf '[client]\npassword=%s\n' "$db_pass" > "$_MYSQL_CNF_FILE"
  _MYSQL_AUTH="--defaults-extra-file=$_MYSQL_CNF_FILE"
else
  _MYSQL_AUTH=""
fi

if mysql $_MYSQL_AUTH -h "${db_host}" -P "${db_port}" -u "${db_user}" -e \
    "CREATE DATABASE IF NOT EXISTS \`${db_name}\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" 2>/dev/null; then
  ok "数据库 '${db_name}' 就绪"
else
  warn "数据库创建失败，请手动执行：CREATE DATABASE \`${db_name}\` CHARACTER SET utf8mb4;"
fi
[[ -n "$_MYSQL_CNF_FILE" ]] && rm -f "$_MYSQL_CNF_FILE"
unset db_pass _MYSQL_CNF_FILE _MYSQL_AUTH

# =============================================================================
step "5 / 7  构建后端"
# =============================================================================
cd "$BACKEND_DIR"
info "下载 Go 依赖..."
go mod download || { error "go mod download 失败，请检查网络或 go.sum 文件"; exit 1; }
go mod tidy     || { error "go mod tidy 失败"; exit 1; }

if [[ "$MODE" == "production" ]]; then
  info "编译后端..."
  mkdir -p bin
  CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/inkframe-backend ./cmd/server \
    || { error "后端编译失败"; exit 1; }
  ok "后端二进制: $BACKEND_DIR/bin/inkframe-backend"
else
  ok "开发模式：跳过编译"
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
npm install --prefer-offline 2>&1 | tail -5 \
  || { error "npm install 失败，请检查 Node.js 版本或网络"; exit 1; }

if [[ "$MODE" == "production" ]]; then
  info "构建前端..."
  NUXT_PUBLIC_API_BASE="http://localhost:${be_port}/api/v1" npm run build 2>&1 | tail -10 \
    || { error "前端构建失败，请查看以上输出"; exit 1; }
  ok "前端构建完成"
else
  ok "开发模式：跳过构建"
fi

# =============================================================================
step "7 / 7  启动服务"
# =============================================================================
cd "$BACKEND_DIR"

# 停掉旧进程
for f in backend frontend; do
  pid_file="$PID_DIR/${f}.pid"
  if [[ -f "$pid_file" ]]; then
    pid=$(cat "$pid_file")
    kill "$pid" 2>/dev/null || true
    rm -f "$pid_file"
  fi
done

if [[ "$MODE" == "production" ]]; then
  if command -v systemctl &>/dev/null; then
    # ── 生成 systemd 服务单元 ────────────────────────────────────────────────
    CURRENT_USER=$(id -un)

    run_privileged tee /etc/systemd/system/inkframe-backend.service > /dev/null << EOF
[Unit]
Description=InkFrame Backend
After=network.target mysql.service mysqld.service redis.service

[Service]
Type=simple
User=$CURRENT_USER
WorkingDirectory=$BACKEND_DIR
ExecStart=$BACKEND_DIR/bin/inkframe-backend
Restart=on-failure
RestartSec=5
StandardOutput=append:$LOG_DIR/backend.log
StandardError=append:$LOG_DIR/backend.log

[Install]
WantedBy=multi-user.target
EOF

    run_privileged tee /etc/systemd/system/inkframe-frontend.service > /dev/null << EOF
[Unit]
Description=InkFrame Frontend
After=network.target inkframe-backend.service

[Service]
Type=simple
User=$CURRENT_USER
WorkingDirectory=$FRONTEND_DIR
Environment=PORT=$fe_port
ExecStart=/usr/bin/node .output/server/index.mjs
Restart=on-failure
RestartSec=5
StandardOutput=append:$LOG_DIR/frontend.log
StandardError=append:$LOG_DIR/frontend.log

[Install]
WantedBy=multi-user.target
EOF

    run_privileged systemctl daemon-reload
    run_privileged systemctl enable --now inkframe-backend
    run_privileged systemctl enable --now inkframe-frontend
    ok "systemd 服务已注册并启动"
    ok "后端日志: journalctl -u inkframe-backend -f"
    ok "前端日志: journalctl -u inkframe-frontend -f"

  else
    warn "systemd 不可用，降级为后台进程模式"
    nohup ./bin/inkframe-backend > "$LOG_DIR/backend.log" 2>&1 &
    _be_pid=$!
    if kill -0 "$_be_pid" 2>/dev/null; then
      echo "$_be_pid" > "$PID_DIR/backend.pid"
      ok "后端已启动 (PID $_be_pid, 日志: logs/backend.log)"
    else
      error "后端启动失败，请查看 $LOG_DIR/backend.log"; exit 1
    fi

    cd "$FRONTEND_DIR"
    PORT=$fe_port nohup node .output/server/index.mjs > "$LOG_DIR/frontend.log" 2>&1 &
    _fe_pid=$!
    if kill -0 "$_fe_pid" 2>/dev/null; then
      echo "$_fe_pid" > "$PID_DIR/frontend.pid"
      ok "前端已启动 (PID $_fe_pid, 日志: logs/frontend.log)"
    else
      error "前端启动失败，请查看 $LOG_DIR/frontend.log"; exit 1
    fi
  fi

else
  # 开发模式
  cd "$BACKEND_DIR"
  nohup go run ./cmd/server > "$LOG_DIR/backend.log" 2>&1 &
  _be_pid=$!
  if kill -0 "$_be_pid" 2>/dev/null; then
    echo "$_be_pid" > "$PID_DIR/backend.pid"
    ok "后端已启动 (PID $_be_pid, 日志: logs/backend.log)"
  else
    error "后端启动失败，请查看 $LOG_DIR/backend.log"; exit 1
  fi

  cd "$FRONTEND_DIR"
  PORT=$fe_port nohup npm run dev > "$LOG_DIR/frontend.log" 2>&1 &
  _fe_pid=$!
  if kill -0 "$_fe_pid" 2>/dev/null; then
    echo "$_fe_pid" > "$PID_DIR/frontend.pid"
    ok "前端已启动 (PID $_fe_pid, 日志: logs/frontend.log)"
  else
    error "前端启动失败，请查看 $LOG_DIR/frontend.log"; exit 1
  fi
fi

# 等待后端就绪（最多 30 秒）
info "等待后端就绪..."
_BE_READY=false
for i in $(seq 1 30); do
  if curl -sf "http://localhost:${be_port}/health" &>/dev/null; then
    _BE_READY=true; break
  fi
  sleep 1
done
if ! $_BE_READY; then
  warn "后端在 30 秒内未响应 /health，请检查日志: $LOG_DIR/backend.log"
fi

echo ""
echo -e "${BOLD}${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${BOLD}${GREEN}  ✅  InkFrame 部署成功！${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "  前端地址: ${CYAN}http://localhost:${fe_port}${NC}"
echo -e "  后端地址: ${CYAN}http://localhost:${be_port}${NC}"
echo -e "  日志目录: ${CYAN}$LOG_DIR${NC}"
if [[ "$MODE" == "production" ]] && command -v systemctl &>/dev/null; then
  echo -e "  停止服务: ${CYAN}sudo systemctl stop inkframe-backend inkframe-frontend${NC}"
else
  echo -e "  停止服务: ${CYAN}bash scripts/deploy-linux.sh --stop${NC}"
fi
echo ""
