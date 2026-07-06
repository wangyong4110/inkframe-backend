#!/bin/bash
exec >> /var/log/sfx-startup.log 2>&1
echo "=== SFX Startup: $(date) ==="

# ── 等待 Docker 就绪 ──────────────────────────────────────
until docker info &>/dev/null; do sleep 3; done

# ── 自动下载模型（已存在则跳过）──────────────────────────
MODEL_DIR="/data/models/TangoFlux"
if [ ! -f "${MODEL_DIR}/config.json" ]; then
    echo "Model not found, downloading from hf-mirror.com ..."
    mkdir -p "${MODEL_DIR}"
    pip install -q huggingface_hub
    HF_ENDPOINT=https://hf-mirror.com python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(
    repo_id='declare-lab/TangoFlux',
    local_dir='${MODEL_DIR}',
    ignore_patterns=['*.md', '*.txt'],
    endpoint='https://hf-mirror.com',
)
print('Model download complete!')
"
    echo "Model ready: $(date)"
else
    echo "Model already exists at ${MODEL_DIR}, skip download."
fi

# ── 拉取并启动容器 ────────────────────────────────────────
docker login __ACR_REGISTRY__ -u __ACCESS_KEY__ -p __ACCESS_SECRET__
docker pull __IMAGE__ || echo "Using cached image"

docker stop sfx-server 2>/dev/null || true
docker rm   sfx-server 2>/dev/null || true

docker run -d \
  --name sfx-server \
  --gpus all \
  --restart unless-stopped \
  -p 8000:8000 \
  -v /data/models/TangoFlux:/data/models/TangoFlux:ro \
  -e MODEL_PATH="/data/models/TangoFlux" \
  -e OSS_ACCESS_KEY="__OSS_ACCESS_KEY__" \
  -e OSS_SECRET_KEY="__OSS_SECRET_KEY__" \
  -e OSS_ENDPOINT="__OSS_ENDPOINT__" \
  -e OSS_BUCKET="__OSS_BUCKET__" \
  __IMAGE__

echo "Container started: $(date)"
