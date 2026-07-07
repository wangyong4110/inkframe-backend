#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BLUE='[0;34m'; GREEN='[0;32m'; YELLOW='[1;33m'; RED='[0;31m'; NC='[0m'
log()  { echo -e "${BLUE}[$(date +%H:%M:%S)]${NC} $1"; }
ok()   { echo -e "${GREEN}  ✓ $1${NC}"; }
warn() { echo -e "${YELLOW}  ⚠ $1${NC}"; }
err()  { echo -e "${RED}  ✗ $1${NC}" >&2; exit 1; }

CONFIG="${SCRIPT_DIR}/infra/config.env"
[ -f "${CONFIG}" ] || err "找不到 infra/config.env
  cp infra/config.env.example infra/config.env"
source "${CONFIG}"
IMAGE="${ACR_REGISTRY}/${ACR_NAMESPACE}/${ACR_REPO}:${IMAGE_TAG}"

for tool in docker aliyun zip base64; do command -v "$tool" &>/dev/null || err "未找到 $tool"; done

echo -e "
${BLUE}━━ ${ACR_REPO} × 阿里云 部署 ━━${NC}"
echo -e "  镜像: ${GREEN}${IMAGE}${NC}  ECS: ${GREEN}${ECS_PRIVATE_IP}${NC}
"

log "[1/5] 登录 ACR..."
echo "${ALIBABA_ACCESS_SECRET}" | docker login "${ACR_REGISTRY}" -u "${ALIBABA_ACCESS_KEY}" --password-stdin
ok "ACR 登录"

log "[2/5] 构建镜像..."
docker build --platform linux/amd64 --progress=plain -t "${IMAGE}" "${SCRIPT_DIR}/docker/"
ok "构建完成"

log "[3/5] 推送镜像..."
docker push "${IMAGE}"
ok "推送完成"

log "[4/5] 配置 ECS user_data..."
STARTUP=$(sed \
    -e "s|__ACR_REGISTRY__|${ACR_REGISTRY}|g" \
    -e "s|__ACCESS_KEY__|${ALIBABA_ACCESS_KEY}|g" \
    -e "s|__ACCESS_SECRET__|${ALIBABA_ACCESS_SECRET}|g" \
    -e "s|__IMAGE__|${IMAGE}|g" \
    -e "s|__OSS_ACCESS_KEY__|${ALIBABA_ACCESS_KEY}|g" \
    -e "s|__OSS_SECRET_KEY__|${ALIBABA_ACCESS_SECRET}|g" \
    -e "s|__OSS_ENDPOINT__|${OSS_ENDPOINT}|g" \
    -e "s|__OSS_BUCKET__|${OSS_BUCKET}|g" \
    "${SCRIPT_DIR}/infra/ecs-userdata.sh")
ENCODED=$(echo "${STARTUP}" | base64 | tr -d '
')
aliyun ecs ModifyInstanceAttribute --RegionId "${REGION_ID}" --InstanceId "${ECS_INSTANCE_ID}" \
    --UserData "${ENCODED}" --output json > /dev/null
ok "ECS user_data 配置完成"

log "[5/5] 部署 FC..."
cd "${SCRIPT_DIR}/fc" && zip -r /tmp/fc-handler.zip . > /dev/null
FC_CODE_B64=$(base64 /tmp/fc-handler.zip | tr -d '
')
cd "${SCRIPT_DIR}"

FC_ENV="{\"ALIBABA_ACCESS_KEY\":\"${ALIBABA_ACCESS_KEY}\",\"ALIBABA_ACCESS_SECRET\":\"${ALIBABA_ACCESS_SECRET}\",\"REGION_ID\":\"${REGION_ID}\",\"ECS_INSTANCE_ID\":\"${ECS_INSTANCE_ID}\",\"ECS_PRIVATE_IP\":\"${ECS_PRIVATE_IP}\",\"TIMEOUT_START\":\"${TIMEOUT_START:-1200}\",\"TIMEOUT_INFER\":\"${TIMEOUT_INFER:-120}\",\"AUTO_STOP\":\"${AUTO_STOP:-true}\"}"

aliyun fc POST /services --header "Content-Type=application/json" \
    --body "{\"serviceName\":\"${FC_SERVICE_NAME}\",\"description\":\"T2I触发器\",\"vpcConfig\":{\"vpcId\":\"${VPC_ID}\",\"vSwitchIds\":[\"${VSWITCH_ID}\"],\"securityGroupId\":\"${SECURITY_GROUP_ID}\"}}" \
    2>/dev/null && ok "FC 服务创建" || warn "已存在"

FUNC_BODY="{\"functionName\":\"${FC_FUNCTION_NAME}\",\"runtime\":\"python3.10\",\"handler\":\"handler.handler\",\"timeout\":${FC_TIMEOUT:-120},\"memorySize\":256,\"environmentVariables\":${FC_ENV},\"code\":{\"zipFile\":\"${FC_CODE_B64}\"}}"

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions" --header "Content-Type=application/json" \
    --body "${FUNC_BODY}" 2>/dev/null && ok "FC 函数创建" \
    || { warn "已存在，更新中..."
         aliyun fc PUT "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}" \
             --header "Content-Type=application/json" --body "${FUNC_BODY}" > /dev/null && ok "FC 更新完成"; }

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}/triggers" \
    --header "Content-Type=application/json" \
    --body "{\"triggerName\":\"http-trigger\",\"triggerType\":\"http\",\"triggerConfig\":{\"authType\":\"anonymous\",\"methods\":[\"POST\"]}}" \
    2>/dev/null && ok "触发器创建" || warn "已存在"

echo -e "
${GREEN}╔═══════════════════════════════╗${NC}"
echo -e "${GREEN}║  🎉 部署完成！                 ║${NC}"
echo -e "${GREEN}╚═══════════════════════════════╝${NC}"
echo ""
echo "测试："
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/generate \"
echo "    -H 'Content-Type: application/json' \"
echo "    -d '{\"prompt\": \"a cat sitting on a mountain, cinematic lighting\", \"width\": 1024, \"height\": 1024}'"

