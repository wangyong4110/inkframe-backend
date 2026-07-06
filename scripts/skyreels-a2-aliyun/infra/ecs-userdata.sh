#!/bin/bash
exec >> /var/log/skyreels-startup.log 2>&1
echo "=== SkyReels-A2 Startup: $(date) ==="

# ── 等待 Docker 就绪 ──────────────────────────────────────
until docker info &>/dev/null; do sleep 3; done

# ── 自动下载模型（已存在则跳过）──────────────────────────
# SkyReels-A2 基于 Wan2.1 14B，权重约 28GB
MODEL_DIR="/data/models/SkyReels-A2"
MODEL_MARKER="${MODEL_DIR}/model_index.json"

if [ ! -f "${MODEL_MARKER}" ]; then
    echo "Model not found, downloading from hf-mirror.com ..."
    mkdir -p "${MODEL_DIR}"
    pip install -q huggingface_hub

    HF_ENDPOINT=https://hf-mirror.com python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(
    repo_id='Skywork/SkyReels-A2',
    local_dir='${MODEL_DIR}',
    ignore_patterns=['*.git*', 'README.md', 'docs'],
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

docker stop skyreels-server 2>/dev/null || true
docker rm   skyreels-server 2>/dev/null || true

docker run -d \
  --name skyreels-server \
  --gpus all \
  --shm-size 64g \
  --restart unless-stopped \
  -p 8000:8000 \
  -v /data/models/SkyReels-A2:/data/models/SkyReels-A2:ro \
  -e MODEL_PATH="/data/models/SkyReels-A2" \
  -e OSS_ACCESS_KEY="__OSS_ACCESS_KEY__" \
  -e OSS_SECRET_KEY="__OSS_SECRET_KEY__" \
  -e OSS_ENDPOINT="__OSS_ENDPOINT__" \
  -e OSS_BUCKET="__OSS_BUCKET__" \
  __IMAGE__

echo "Container started: $(date)"
