#!/bin/bash
exec >> /var/log/wan22-startup.log 2>&1
echo "=== Wan2.2 Startup: $(date) ==="

# ── 等待 Docker 就绪 ──────────────────────────────────────
until docker info &>/dev/null; do sleep 3; done

# ── 自动下载模型（已存在则跳过）──────────────────────────
# MODEL_TASK 决定下载哪个模型：
#   ti2v-5B  → Wan2.2-TI2V-5B  约 10GB，单卡 24GB 可跑（T4/4090）
#   t2v-A14B → Wan2.2-T2V-A14B 约 30GB，需要 A100 80G
#   i2v-A14B → Wan2.2-I2V-A14B 约 30GB，需要 A100 80G
MODEL_TASK="${MODEL_TASK:-ti2v-5B}"
MODEL_DIR="/data/models/Wan2.2"

declare -A TASK_REPO=(
    ["ti2v-5B"]="Wan-AI/Wan2.2-TI2V-5B"
    ["t2v-A14B"]="Wan-AI/Wan2.2-T2V-A14B"
    ["i2v-A14B"]="Wan-AI/Wan2.2-I2V-A14B"
)

declare -A TASK_DIR=(
    ["ti2v-5B"]="Wan2.2-TI2V-5B"
    ["t2v-A14B"]="Wan2.2-T2V-A14B"
    ["i2v-A14B"]="Wan2.2-I2V-A14B"
)

REPO_ID="${TASK_REPO[$MODEL_TASK]}"
CKPT_DIR="${MODEL_DIR}/${TASK_DIR[$MODEL_TASK]}"
MODEL_MARKER="${CKPT_DIR}/model_index.json"

if [ ! -f "${MODEL_MARKER}" ]; then
    echo "Model not found (task=${MODEL_TASK}), downloading ${REPO_ID} ..."
    mkdir -p "${CKPT_DIR}"
    pip install -q huggingface_hub

    HF_ENDPOINT=https://hf-mirror.com python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(
    repo_id='${REPO_ID}',
    local_dir='${CKPT_DIR}',
    ignore_patterns=['*.git*', 'README.md'],
    endpoint='https://hf-mirror.com',
)
print('Download complete: ${REPO_ID}')
"
    echo "Model ready: $(date)"
else
    echo "Model already exists: ${CKPT_DIR}, skip download."
fi

# ── 拉取并启动容器 ────────────────────────────────────────
docker login __ACR_REGISTRY__ -u __ACCESS_KEY__ -p __ACCESS_SECRET__
docker pull __IMAGE__ || echo "Using cached image"

docker stop wan22-server 2>/dev/null || true
docker rm   wan22-server 2>/dev/null || true

docker run -d \
  --name wan22-server \
  --gpus all \
  --shm-size 32g \
  --restart unless-stopped \
  -p 8000:8000 \
  -v /data/models/Wan2.2:/data/models/Wan2.2:ro \
  -e MODEL_DIR="/data/models/Wan2.2" \
  -e MODEL_TASK="${MODEL_TASK}" \
  -e OSS_ACCESS_KEY="__OSS_ACCESS_KEY__" \
  -e OSS_SECRET_KEY="__OSS_SECRET_KEY__" \
  -e OSS_ENDPOINT="__OSS_ENDPOINT__" \
  -e OSS_BUCKET="__OSS_BUCKET__" \
  __IMAGE__

echo "Container started: $(date)"
