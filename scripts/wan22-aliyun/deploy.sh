#!/usr/bin/env bash
# Wan2.2 on 阿里云 一键部署脚本
# 使用：cp infra/config.env.example infra/config.env && ./deploy.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BLUE='\033[0;34m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${BLUE}[$(date +%H:%M:%S)]${NC} $1"; }
ok()   { echo -e "${GREEN}  ✓ $1${NC}"; }
warn() { echo -e "${YELLOW}  ⚠ $1${NC}"; }
err()  { echo -e "${RED}  ✗ $1${NC}" >&2; exit 1; }

CONFIG="${SCRIPT_DIR}/infra/config.env"
[ -f "${CONFIG}" ] || err "找不到 infra/config.env\n  请执行：cp infra/config.env.example infra/config.env"
source "${CONFIG}"
IMAGE="${ACR_REGISTRY}/${ACR_NAMESPACE}/${ACR_REPO}:${IMAGE_TAG}"

for tool in docker aliyun zip base64; do
    command -v "$tool" &>/dev/null || err "未找到 $tool"
done

echo -e "\n${BLUE}━━ Wan2.2 × 阿里云 一键部署 ━━${NC}"
echo -e "  镜像:      ${GREEN}${IMAGE}${NC}"
echo -e "  ECS:       ${GREEN}${ECS_INSTANCE_ID} (${ECS_PRIVATE_IP})${NC}"
echo -e "  MODEL_TASK:${GREEN} ${MODEL_TASK}${NC}\n"

# 1. 登录 ACR
log "[1/5] 登录 ACR..."
echo "${ALIBABA_ACCESS_SECRET}" | docker login "${ACR_REGISTRY}" \
    -u "${ALIBABA_ACCESS_KEY}" --password-stdin
ok "ACR 登录成功"

# 2. 构建镜像
log "[2/5] 构建 Docker 镜像..."
docker build \
    --platform linux/amd64 \
    --progress=plain \
    -t "${IMAGE}" \
    "${SCRIPT_DIR}/docker/"
ok "构建完成: ${IMAGE}"

# 3. 推送到 ACR
log "[3/5] 推送镜像..."
docker push "${IMAGE}"
ok "推送完成"

# 4. 配置 ECS user_data
log "[4/5] 配置 ECS 启动脚本..."
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
# 注入 MODEL_TASK
STARTUP=$(echo "${STARTUP}" | sed "s|MODEL_TASK:-ti2v-5B|MODEL_TASK:-${MODEL_TASK}|g")
ENCODED=$(echo "${STARTUP}" | base64 | tr -d '\n')
aliyun ecs ModifyInstanceAttribute \
    --RegionId "${REGION_ID}" \
    --InstanceId "${ECS_INSTANCE_ID}" \
    --UserData "${ENCODED}" \
    --output json > /dev/null
ok "ECS user_data 配置完成（MODEL_TASK=${MODEL_TASK}）"

# 5. 部署函数计算 FC
log "[5/5] 部署函数计算..."
cd "${SCRIPT_DIR}/fc" && zip -r /tmp/fc-wan22.zip . > /dev/null
FC_CODE_B64=$(base64 /tmp/fc-wan22.zip | tr -d '\n')
cd "${SCRIPT_DIR}"

FC_ENV="{
  \"ALIBABA_ACCESS_KEY\":    \"${ALIBABA_ACCESS_KEY}\",
  \"ALIBABA_ACCESS_SECRET\": \"${ALIBABA_ACCESS_SECRET}\",
  \"REGION_ID\":             \"${REGION_ID}\",
  \"ECS_INSTANCE_ID\":       \"${ECS_INSTANCE_ID}\",
  \"ECS_PRIVATE_IP\":        \"${ECS_PRIVATE_IP}\",
  \"TIMEOUT_START\":         \"${TIMEOUT_START:-1800}\",
  \"TIMEOUT_INFER\":         \"${TIMEOUT_INFER:-600}\",
  \"AUTO_STOP\":             \"${AUTO_STOP:-true}\"
}"

aliyun fc POST /services \
    --header "Content-Type=application/json" \
    --body "{\"serviceName\":\"${FC_SERVICE_NAME}\",\"description\":\"Wan2.2触发器\",\"vpcConfig\":{\"vpcId\":\"${VPC_ID}\",\"vSwitchIds\":[\"${VSWITCH_ID}\"],\"securityGroupId\":\"${SECURITY_GROUP_ID}\"}}" \
    2>/dev/null && ok "FC 服务创建" || warn "FC 服务已存在"

FUNC_BODY="{\"functionName\":\"${FC_FUNCTION_NAME}\",\"runtime\":\"python3.10\",\"handler\":\"handler.handler\",\"timeout\":${FC_TIMEOUT:-600},\"memorySize\":512,\"environmentVariables\":${FC_ENV},\"code\":{\"zipFile\":\"${FC_CODE_B64}\"}}"

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions" \
    --header "Content-Type=application/json" \
    --body "${FUNC_BODY}" 2>/dev/null \
  && ok "FC 函数创建成功" \
  || { warn "函数已存在，更新中..."
       aliyun fc PUT "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}" \
           --header "Content-Type=application/json" \
           --body "${FUNC_BODY}" > /dev/null
       ok "FC 函数更新成功"; }

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}/triggers" \
    --header "Content-Type=application/json" \
    --body "{\"triggerName\":\"http-trigger\",\"triggerType\":\"http\",\"triggerConfig\":{\"authType\":\"anonymous\",\"methods\":[\"POST\"]}}" \
    2>/dev/null && ok "HTTP 触发器创建" || warn "触发器已存在"

echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  🎉 部署完成！                                   ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${YELLOW}MODEL_TASK = ${MODEL_TASK}${NC}"
echo ""
echo "  # 文本转视频 (T2V)"
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/generate \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"prompt\": \"Two cats boxing on a stage\", \"num_frames\": 81}'"
echo ""
echo "  # 图片转视频 (I2V)"
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/generate/i2v \\"
echo "    -F 'image=@input.jpg' \\"
echo "    -F 'prompt=The cat starts swimming in the ocean' \\"
echo "    -F 'num_frames=81'"
echo ""
echo -e "  ${YELLOW}注意：${NC}"
case "${MODEL_TASK}" in
    "ti2v-5B")
        echo "  · ti2v-5B 模型约 10GB，首次下载约 15-20 分钟"
        echo "  · 后续启动约 60-90 秒，T4 16G 可运行"
        ;;
    "t2v-A14B"|"i2v-A14B")
        echo "  · A14B 模型约 30GB，首次下载约 45-60 分钟，TIMEOUT_START 建议改为 3600"
        echo "  · 需要 A100 80G，抢占式约 ¥8-12/小时"
        ;;
esac
echo "  · 推理完成后自动关机（AUTO_STOP=true）"
