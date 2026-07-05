#!/usr/bin/env bash
# ══════════════════════════════════════════════════════════════════
# SkyReels-A2 阿里云一键部署脚本
# ══════════════════════════════════════════════════════════════════
# 前置条件：
#   1. 安装 Docker、aliyun CLI
#   2. 已在控制台创建：
#      - ECS A100 80G 抢占式实例（ecs.gn7e-c16g1.4xlarge）
#        系统盘 100GB ESSD，已安装 Docker + NVIDIA Driver
#      - OSS Bucket（与 ECS 同 Region）
#      - ACR 命名空间
#      - VPC + 安全组（开放 8000 端口，仅 VPC 内访问）
#   3. cp infra/config.env.example infra/config.env 并填写配置
#
# 使用：
#   chmod +x deploy.sh && ./deploy.sh
# ══════════════════════════════════════════════════════════════════
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── 颜色输出 ──────────────────────────────────────────────
BLUE='\033[0;34m'; GREEN='\033[0;32m'
YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${BLUE}[$(date +%H:%M:%S)]${NC} $1"; }
ok()   { echo -e "${GREEN}  ✓ $1${NC}"; }
warn() { echo -e "${YELLOW}  ⚠ $1${NC}"; }
err()  { echo -e "${RED}  ✗ $1${NC}" >&2; exit 1; }
step() { echo -e "\n${BLUE}━━ $1 ━━${NC}"; }

# ── 加载配置 ──────────────────────────────────────────────
CONFIG="${SCRIPT_DIR}/infra/config.env"
[ -f "${CONFIG}" ] || err "找不到 infra/config.env\n  请执行：cp infra/config.env.example infra/config.env 并填写配置"
source "${CONFIG}"

IMAGE="${ACR_REGISTRY}/${ACR_NAMESPACE}/${ACR_REPO}:${IMAGE_TAG}"

# ── 检查依赖 ──────────────────────────────────────────────
step "环境检查"
for tool in docker aliyun zip base64; do
    command -v "$tool" &>/dev/null && ok "$tool" || err "未找到 $tool，请先安装"
done

echo ""
echo -e "${BLUE}╔══════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║       SkyReels-A2 × 阿里云 一键部署                 ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════════════════╝${NC}"
echo -e "  镜像:  ${GREEN}${IMAGE}${NC}"
echo -e "  ECS:   ${GREEN}${ECS_INSTANCE_ID} (${ECS_PRIVATE_IP})${NC}"
echo -e "  OSS:   ${GREEN}${OSS_BUCKET}${NC}"
echo -e "  FC:    ${GREEN}${FC_SERVICE_NAME}/${FC_FUNCTION_NAME}${NC}"
echo ""
warn "SkyReels-A2 模型约 28GB，首次构建镜像耗时较长（30-60 分钟）"
read -p "  确认继续？[y/N] " confirm
[[ "${confirm}" =~ ^[Yy]$ ]] || { echo "已取消"; exit 0; }

# ══════════════════════════════════════════════════════════
# Step 1 / 5 — 登录 ACR
# ══════════════════════════════════════════════════════════
step "[1/5] 登录阿里云容器镜像服务 ACR"
echo "${ALIBABA_ACCESS_SECRET}" | docker login "${ACR_REGISTRY}" \
    -u "${ALIBABA_ACCESS_KEY}" --password-stdin
ok "ACR 登录成功"

# ══════════════════════════════════════════════════════════
# Step 2 / 5 — 构建 Docker 镜像
# ══════════════════════════════════════════════════════════
step "[2/5] 构建 Docker 镜像"
warn "首次构建：下载 SkyReels-A2 模型权重约 28GB，预计 30-60 分钟"
warn "后续构建因 Docker 缓存层，速度大幅提升"

docker build \
    --platform linux/amd64 \
    --progress=plain \
    -t "${IMAGE}" \
    "${SCRIPT_DIR}/docker/"

ok "镜像构建完成: ${IMAGE}"
echo -e "  镜像大小: $(docker image inspect ${IMAGE} --format='{{.Size}}' | awk '{printf "%.1f GB", $1/1073741824}')"

# ══════════════════════════════════════════════════════════
# Step 3 / 5 — 推送镜像到 ACR
# ══════════════════════════════════════════════════════════
step "[3/5] 推送镜像到 ACR"
docker push "${IMAGE}"
ok "镜像推送完成"

# ══════════════════════════════════════════════════════════
# Step 4 / 5 — 配置 ECS 实例启动脚本（user_data）
# ══════════════════════════════════════════════════════════
step "[4/5] 配置 ECS 实例自动启动脚本"

# ECS 开机自动执行的脚本（拉取最新镜像并启动容器）
STARTUP_SCRIPT=$(cat <<INNER_SCRIPT
#!/bin/bash
exec >> /var/log/skyreels-startup.log 2>&1
echo "=== SkyReels-A2 Startup: \$(date) ==="

# 等待 Docker daemon 就绪
until docker info &>/dev/null; do
    echo "Waiting for Docker daemon..."
    sleep 3
done

# 登录 ACR
docker login ${ACR_REGISTRY} \
    -u ${ALIBABA_ACCESS_KEY} \
    -p ${ALIBABA_ACCESS_SECRET}

# 拉取最新镜像（已缓存时秒级完成）
docker pull ${IMAGE} || echo "Using cached image"

# 停止旧容器
docker stop skyreels-a2 2>/dev/null || true
docker rm   skyreels-a2 2>/dev/null || true

# 启动推理服务容器
# --shm-size 64g：SkyReels-A2 多进程推理需要大 shared memory
docker run -d \
  --name skyreels-a2 \
  --gpus all \
  --shm-size 64g \
  --restart unless-stopped \
  -p 8000:8000 \
  -e MODEL_PATH="/app/model" \
  -e OSS_ACCESS_KEY="${ALIBABA_ACCESS_KEY}" \
  -e OSS_SECRET_KEY="${ALIBABA_ACCESS_SECRET}" \
  -e OSS_ENDPOINT="${OSS_ENDPOINT}" \
  -e OSS_BUCKET="${OSS_BUCKET}" \
  -e OSS_URL_EXPIRE="${OSS_URL_EXPIRE}" \
  ${IMAGE}

echo "Container started at \$(date)"
echo "Waiting for model to load (may take 2-3 minutes)..."
INNER_SCRIPT
)

ENCODED=$(echo "${STARTUP_SCRIPT}" | base64 | tr -d '\n')

aliyun ecs ModifyInstanceAttribute \
    --RegionId "${REGION_ID}" \
    --InstanceId "${ECS_INSTANCE_ID}" \
    --UserData "${ENCODED}" \
    --output json > /dev/null

ok "ECS user_data 配置完成"

# ══════════════════════════════════════════════════════════
# Step 5 / 5 — 部署函数计算 FC
# ══════════════════════════════════════════════════════════
step "[5/5] 部署函数计算触发器"

# 打包 FC 代码
cd "${SCRIPT_DIR}/fc"
zip -r /tmp/fc-skyreels.zip . > /dev/null
FC_CODE_B64=$(base64 /tmp/fc-skyreels.zip | tr -d '\n')
cd "${SCRIPT_DIR}"
ok "FC 代码打包完成"

# 环境变量 JSON
FC_ENV_JSON=$(cat <<EOF_ENV
{
  "ALIBABA_ACCESS_KEY":    "${ALIBABA_ACCESS_KEY}",
  "ALIBABA_ACCESS_SECRET": "${ALIBABA_ACCESS_SECRET}",
  "REGION_ID":             "${REGION_ID}",
  "ECS_INSTANCE_ID":       "${ECS_INSTANCE_ID}",
  "ECS_PRIVATE_IP":        "${ECS_PRIVATE_IP}",
  "INFER_PORT":            "8000",
  "TIMEOUT_START":         "${TIMEOUT_START:-300}",
  "TIMEOUT_INFER":         "${TIMEOUT_INFER:-600}",
  "AUTO_STOP":             "${AUTO_STOP:-true}"
}
EOF_ENV
)

# 创建 FC 服务（已存在则跳过）
aliyun fc POST /services \
    --header "Content-Type=application/json" \
    --body "{
      \"serviceName\": \"${FC_SERVICE_NAME}\",
      \"description\": \"SkyReels-A2 多角色视频生成服务\",
      \"vpcConfig\": {
        \"vpcId\": \"${VPC_ID}\",
        \"vSwitchIds\": [\"${VSWITCH_ID}\"],
        \"securityGroupId\": \"${SECURITY_GROUP_ID}\"
      }
    }" 2>/dev/null \
  && ok "FC 服务创建成功" \
  || warn "FC 服务已存在，跳过"

# 创建或更新函数
FUNC_BODY=$(cat <<EOF_FUNC
{
  "functionName": "${FC_FUNCTION_NAME}",
  "runtime": "python3.10",
  "handler": "handler.handler",
  "timeout": ${FC_TIMEOUT:-600},
  "memorySize": 512,
  "environmentVariables": ${FC_ENV_JSON},
  "code": {
    "zipFile": "${FC_CODE_B64}"
  }
}
EOF_FUNC
)

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions" \
    --header "Content-Type=application/json" \
    --body "${FUNC_BODY}" 2>/dev/null \
  && ok "FC 函数创建成功" \
  || {
    warn "函数已存在，执行更新..."
    aliyun fc PUT "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}" \
        --header "Content-Type=application/json" \
        --body "${FUNC_BODY}" > /dev/null
    ok "FC 函数更新成功"
  }

# 创建 HTTP 触发器（已存在则跳过）
aliyun fc POST \
    "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}/triggers" \
    --header "Content-Type=application/json" \
    --body '{
      "triggerName": "http-trigger",
      "triggerType": "http",
      "triggerConfig": {
        "authType": "anonymous",
        "methods": ["POST", "GET"]
      }
    }' 2>/dev/null \
  && ok "HTTP 触发器创建成功" \
  || warn "HTTP 触发器已存在，跳过"

# ══════════════════════════════════════════════════════════
# 完成
# ══════════════════════════════════════════════════════════
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  🎉 部署完成！                                       ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${YELLOW}📌 测试方式（完整链路，冷启动约 3-5 分钟）：${NC}"
echo ""
echo "  FC_URL=\"https://<账号ID>.${REGION_ID}.fc.aliyuncs.com/2016-08-15/proxy/${FC_SERVICE_NAME}/${FC_FUNCTION_NAME}/\""
echo ""
echo "  # 上传多张参考图，生成多角色视频"
echo "  curl -X POST \"\${FC_URL}generate\" \\"
echo "    -F \"ref_images=@/path/to/char_a.jpg\" \\"
echo "    -F \"ref_images=@/path/to/char_b.jpg\" \\"
echo "    -F 'prompt=Two women talking in a cafe, char A on left with red hair, char B on right with black hair' \\"
echo "    -F 'num_frames=81' \\"
echo "    -F 'resolution=540p'"
echo ""
echo -e "  ${YELLOW}📌 直连 ECS 测试（需先手动启动实例）：${NC}"
echo ""
echo "  # 健康检查"
echo "  curl http://${ECS_PRIVATE_IP}:8000/health"
echo ""
echo "  # 查看 API 文档"
echo "  curl http://${ECS_PRIVATE_IP}:8000/docs"
echo ""
echo -e "  ${YELLOW}⚠️  注意事项：${NC}"
echo "  · A100 80G 抢占式实例约 ¥8-12/小时（按量约 ¥34/小时）"
echo "  · 540P 视频（81帧）生成约需 3-8 分钟"
echo "  · 首次冷启动：ECS 启动(30s) + Docker(15s) + 模型加载(120s) ≈ 3 分钟"
echo "  · AUTO_STOP=true：推理完成后自动关机，闲置零成本"
echo "  · 系统盘(100GB ESSD)持续计费约 ¥40/月"
echo ""
