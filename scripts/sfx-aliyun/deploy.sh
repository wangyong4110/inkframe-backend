#!/usr/bin/env bash
# =============================================================================
# SFX-on-Aliyun 一键部署脚本
# =============================================================================
# 前置条件：
#   1. 已安装 Docker 和 aliyun CLI
#   2. 已在控制台手动创建：ECS T4 gn6i 抢占式实例、OSS Bucket、ACR 命名空间
#   3. 填好 infra/config.env
# 使用：
#   cp infra/config.env.example infra/config.env
#   ./deploy.sh
# =============================================================================
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BLUE='\033[0;34m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${BLUE}[$(date +%H:%M:%S)]${NC} $1"; }
ok()   { echo -e "${GREEN}  ✓ $1${NC}"; }
warn() { echo -e "${YELLOW}  ⚠ $1${NC}"; }
err()  { echo -e "${RED}  ✗ $1${NC}" >&2; exit 1; }

CONFIG_FILE="${SCRIPT_DIR}/infra/config.env"
[ -f "${CONFIG_FILE}" ] || err "找不到 infra/config.env\n  请执行：cp infra/config.env.example infra/config.env 并填写配置"
source "${CONFIG_FILE}"
IMAGE="${ACR_REGISTRY}/${ACR_NAMESPACE}/${ACR_REPO}:${IMAGE_TAG}"

log "检查依赖工具..."
command -v docker &>/dev/null || err "未找到 docker"
command -v aliyun &>/dev/null || err "未找到 aliyun CLI"
ok "工具检查通过"

echo ""
echo -e "${BLUE}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${BLUE}  SFX-on-Aliyun 一键部署${NC}"
echo -e "${BLUE}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "  镜像: ${GREEN}${IMAGE}${NC}"
echo -e "  ECS:  ${GREEN}${ECS_INSTANCE_ID} (${ECS_PRIVATE_IP})${NC}"
echo ""

# Step 1: 登录 ACR
log "[1/5] 登录 ACR..."
echo "${ALIBABA_ACCESS_SECRET}" | docker login "${ACR_REGISTRY}" -u "${ALIBABA_ACCESS_KEY}" --password-stdin
ok "ACR 登录成功"

# Step 2: 构建镜像
log "[2/5] 构建 Docker 镜像..."
warn "首次构建需下载模型权重（约 2GB），预计 10-30 分钟"
docker build --platform linux/amd64 --progress=plain -t "${IMAGE}" "${SCRIPT_DIR}/docker/"
ok "镜像构建完成"

# Step 3: 推送到 ACR
log "[3/5] 推送镜像..."
docker push "${IMAGE}"
ok "镜像推送完成"

# Step 4: 配置 ECS user_data（开机自动启动容器）
log "[4/5] 配置 ECS 自动启动脚本..."
STARTUP_SCRIPT=$(cat <<INNER
#!/bin/bash
exec >> /var/log/sfx-startup.log 2>&1
echo "=== Startup: \$(date) ==="
until docker info &>/dev/null; do sleep 3; done
docker login ${ACR_REGISTRY} -u ${ALIBABA_ACCESS_KEY} -p ${ALIBABA_ACCESS_SECRET}
docker pull ${IMAGE} || true
docker stop sfx-server 2>/dev/null || true
docker rm   sfx-server 2>/dev/null || true
docker run -d --name sfx-server --gpus all --restart unless-stopped -p 8000:8000 \
  -e OSS_ACCESS_KEY='${ALIBABA_ACCESS_KEY}' \
  -e OSS_SECRET_KEY='${ALIBABA_ACCESS_SECRET}' \
  -e OSS_ENDPOINT='${OSS_ENDPOINT}' \
  -e OSS_BUCKET='${OSS_BUCKET}' \
  ${IMAGE}
echo "Started: \$(date)"
INNER
)
ENCODED=$(echo "${STARTUP_SCRIPT}" | base64 | tr -d '\n')
aliyun ecs ModifyInstanceAttribute \
    --RegionId "${REGION_ID}" \
    --InstanceId "${ECS_INSTANCE_ID}" \
    --UserData "${ENCODED}" \
    --output json > /dev/null
ok "ECS user_data 配置完成"

# Step 5: 部署函数计算
log "[5/5] 部署函数计算..."
cd "${SCRIPT_DIR}/fc"
zip -r /tmp/fc-handler.zip . > /dev/null
FC_CODE_B64=$(base64 /tmp/fc-handler.zip | tr -d '\n')
cd "${SCRIPT_DIR}"

FC_ENV="{
  \"ALIBABA_ACCESS_KEY\": \"${ALIBABA_ACCESS_KEY}\",
  \"ALIBABA_ACCESS_SECRET\": \"${ALIBABA_ACCESS_SECRET}\",
  \"REGION_ID\": \"${REGION_ID}\",
  \"ECS_INSTANCE_ID\": \"${ECS_INSTANCE_ID}\",
  \"ECS_PRIVATE_IP\": \"${ECS_PRIVATE_IP}\",
  \"TIMEOUT_START\": \"180\"
}"

# 创建服务（已存在则跳过）
aliyun fc POST /services \
    --header "Content-Type=application/json" \
    --body "{\"serviceName\":\"${FC_SERVICE_NAME}\",\"description\":\"SFX触发器\",\"vpcConfig\":{\"vpcId\":\"${VPC_ID}\",\"vSwitchIds\":[\"${VSWITCH_ID}\"],\"securityGroupId\":\"${SECURITY_GROUP_ID}\"}}" \
    2>/dev/null && ok "FC 服务创建成功" || warn "FC 服务已存在"

# 创建或更新函数
FUNC_BODY="{\"functionName\":\"${FC_FUNCTION_NAME}\",\"runtime\":\"python3.10\",\"handler\":\"handler.handler\",\"timeout\":300,\"memorySize\":256,\"environmentVariables\":${FC_ENV},\"code\":{\"zipFile\":\"${FC_CODE_B64}\"}}"

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions" \
    --header "Content-Type=application/json" \
    --body "${FUNC_BODY}" 2>/dev/null \
  && ok "FC 函数创建成功" \
  || { warn "函数已存在，执行更新...";
       aliyun fc PUT "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}" \
           --header "Content-Type=application/json" \
           --body "${FUNC_BODY}" > /dev/null && ok "FC 函数更新成功"; }

# 创建 HTTP 触发器
aliyun fc POST "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}/triggers" \
    --header "Content-Type=application/json" \
    --body "{\"triggerName\":\"http-trigger\",\"triggerType\":\"http\",\"triggerConfig\":{\"authType\":\"anonymous\",\"methods\":[\"POST\"]}}" \
    2>/dev/null && ok "HTTP 触发器创建成功" || warn "触发器已存在"

echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  🎉 部署完成！                                   ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════╝${NC}"
echo ""
echo "测试命令："
echo "  curl -X POST https://<账号ID>.cn-hangzhou.fc.aliyuncs.com/2016-08-15/proxy/${FC_SERVICE_NAME}/${FC_FUNCTION_NAME}/ \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"prompt\": \"heavy rain on a metal roof\", \"duration\": 10}'"
echo ""
echo "⚠️  首次调用冷启动约 60-90 秒"
