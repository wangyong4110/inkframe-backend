#!/usr/bin/env bash
# Creative Hub 一键部署脚本
# Wan 2.2 TI2V-5B + TangoFlux + ACE-Step 1.5
# 目标：A10 24GB，三模型共用显存（约 20GB 峰值）
# 使用：cp infra/config.env.example infra/config.env && ./deploy.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BLUE='\033[0;34m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${BLUE}[$(date +%H:%M:%S)]${NC} $1"; }
ok()   { echo -e "${GREEN}  ✓ $1${NC}"; }
warn() { echo -e "${YELLOW}  ⚠ $1${NC}"; }
err()  { echo -e "${RED}  ✗ $1${NC}" >&2; exit 1; }

CONFIG="${SCRIPT_DIR}/infra/config.env"
[ -f "${CONFIG}" ] || err "找不到 infra/config.env\n  cp infra/config.env.example infra/config.env"
source "${CONFIG}"
IMAGE="${ACR_REGISTRY}/${ACR_NAMESPACE}/${ACR_REPO}:${IMAGE_TAG}"

for tool in docker aliyun zip base64; do command -v "$tool" &>/dev/null || err "未找到 $tool"; done

echo -e "\n${BLUE}━━ Creative Hub × 阿里云 一键部署 ━━${NC}"
echo -e "  镜像: ${GREEN}${IMAGE}${NC}"
echo -e "  ECS:  ${GREEN}${ECS_INSTANCE_ID} (${ECS_PRIVATE_IP})${NC}"
echo -e "  组合: ${YELLOW}Wan 2.2 TI2V-5B + TangoFlux + ACE-Step 1.5${NC}"
echo -e "  显存: ${YELLOW}A10 24GB  峰值约 20GB${NC}\n"

# 1. 登录 ACR
log "[1/5] 登录 ACR..."
echo "${ALIBABA_ACCESS_SECRET}" | docker login "${ACR_REGISTRY}" \
    -u "${ALIBABA_ACCESS_KEY}" --password-stdin
ok "ACR 登录"

# 2. 构建镜像
log "[2/5] 构建 Docker 镜像..."
docker build --platform linux/amd64 --progress=plain \
    -t "${IMAGE}" "${SCRIPT_DIR}/docker/"
ok "构建完成: ${IMAGE}"

# 3. 推送
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
ENCODED=$(echo "${STARTUP}" | base64 | tr -d '\n')
aliyun ecs ModifyInstanceAttribute \
    --RegionId "${REGION_ID}" --InstanceId "${ECS_INSTANCE_ID}" \
    --UserData "${ENCODED}" --output json > /dev/null
ok "ECS user_data 配置完成"

# 5. 部署 FC
log "[5/5] 部署函数计算..."
cd "${SCRIPT_DIR}/fc" && zip -r /tmp/fc-creative-hub.zip . > /dev/null
FC_CODE_B64=$(base64 /tmp/fc-creative-hub.zip | tr -d '\n')
cd "${SCRIPT_DIR}"

FC_ENV="{
  \"ALIBABA_ACCESS_KEY\":    \"${ALIBABA_ACCESS_KEY}\",
  \"ALIBABA_ACCESS_SECRET\": \"${ALIBABA_ACCESS_SECRET}\",
  \"REGION_ID\":             \"${REGION_ID}\",
  \"ECS_INSTANCE_ID\":       \"${ECS_INSTANCE_ID}\",
  \"ECS_PRIVATE_IP\":        \"${ECS_PRIVATE_IP}\",
  \"TIMEOUT_START\":         \"${TIMEOUT_START:-3600}\",
  \"TIMEOUT_INFER\":         \"${TIMEOUT_INFER:-600}\",
  \"AUTO_STOP\":             \"${AUTO_STOP:-true}\"
}"

aliyun fc POST /services --header "Content-Type=application/json" \
    --body "{\"serviceName\":\"${FC_SERVICE_NAME}\",\"description\":\"Creative Hub\",\"vpcConfig\":{\"vpcId\":\"${VPC_ID}\",\"vSwitchIds\":[\"${VSWITCH_ID}\"],\"securityGroupId\":\"${SECURITY_GROUP_ID}\"}}" \
    2>/dev/null && ok "FC 服务创建" || warn "已存在"

FUNC="{\"functionName\":\"${FC_FUNCTION_NAME}\",\"runtime\":\"python3.10\",\"handler\":\"handler.handler\",\"timeout\":${FC_TIMEOUT:-600},\"memorySize\":256,\"environmentVariables\":${FC_ENV},\"code\":{\"zipFile\":\"${FC_CODE_B64}\"}}"

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions" \
    --header "Content-Type=application/json" --body "${FUNC}" 2>/dev/null \
    && ok "FC 函数创建" \
    || { warn "更新中..."
         aliyun fc PUT "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}" \
             --header "Content-Type=application/json" --body "${FUNC}" > /dev/null
         ok "FC 更新"; }

aliyun fc POST "/services/${FC_SERVICE_NAME}/functions/${FC_FUNCTION_NAME}/triggers" \
    --header "Content-Type=application/json" \
    --body "{\"triggerName\":\"http-trigger\",\"triggerType\":\"http\",\"triggerConfig\":{\"authType\":\"anonymous\",\"methods\":[\"POST\",\"GET\"]}}" \
    2>/dev/null && ok "触发器创建" || warn "已存在"

echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  🎬🔊🎵  Creative Hub 部署完成！                     ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${YELLOW}四个端点：${NC}"
echo ""
echo "  # 文本转视频（Wan 2.2 TI2V-5B）"
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/video \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"prompt\": \"A serene lake at sunrise, golden light\", \"num_frames\": 81}'"
echo ""
echo "  # 图片转视频"
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/video/i2v \\"
echo "    -F 'image=@input.jpg' -F 'prompt=The scene comes alive' -F 'num_frames=81'"
echo ""
echo "  # 文本转音效（TangoFlux）"
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/sfx \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"prompt\": \"gentle wind through trees, birds chirping\", \"duration\": 10}'"
echo ""
echo "  # 文本转音乐（ACE-Step 1.5）"
echo "  curl -X POST http://${ECS_PRIVATE_IP}:8000/music \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"prompt\": \"peaceful ambient, nature, instrumental\", \"duration\": 60}'"
echo ""
echo "  # 健康检查"
echo "  curl http://${ECS_PRIVATE_IP}:8000/health"
echo ""
echo -e "  ${YELLOW}显存分配（A10 24GB）：${NC}"
echo "  · 启动后常驻：TangoFlux ~6GB + ACE-Step ~4GB = ~10GB，剩余 ~14GB"
echo "  · 视频推理时：Wan 2.2 子进程额外占用 ~10GB，峰值约 20GB"
echo "  · 视频推理结束后：子进程退出，显存降回 ~10GB"
echo ""
echo -e "  ${YELLOW}首次调用说明：${NC}"
echo "  · 模型下载约 20GB（hf-mirror），首次约 20-40 分钟"
echo "  · 后续冷启动（模型已在磁盘）约 90-120 秒"
