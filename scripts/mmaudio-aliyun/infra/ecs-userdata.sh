#!/bin/bash
exec >> /var/log/mmaudio-startup.log 2>&1
echo "=== MMAudio Startup: $(date) ==="

# ── 等待 Docker 就绪 ──────────────────────────────────────
until docker info &>/dev/null; do sleep 3; done

# ── 自动下载模型（已存在则跳过）──────────────────────────
# MMAudio 通过 MMAUDIO_HOME 环境变量指定模型目录
# 模型文件包括：large_44k_v2.pth / vae.pth / synchformer.pt 等，共约 4GB
MODEL_DIR="/data/models/MMAudio"
MODEL_MARKER="${MODEL_DIR}/large_44k_v2.pth"

if [ ! -f "${MODEL_MARKER}" ]; then
    echo "Models not found, downloading via MMAudio downloader ..."
    mkdir -p "${MODEL_DIR}"

    # 使用 hf-mirror.com 加速下载
    export HF_ENDPOINT=https://hf-mirror.com
    export MMAUDIO_HOME="${MODEL_DIR}"

    pip install -q huggingface_hub

    python3 -c "
import os
os.environ['MMAUDIO_HOME'] = '${MODEL_DIR}'
os.environ['HF_ENDPOINT']  = 'https://hf-mirror.com'

# 触发 MMAudio 内置下载工具
from mmaudio.eval_utils import all_model_cfg
cfg = all_model_cfg['large_44k_v2']
cfg.download_if_needed()
print('Model download complete!')
" || {
    # 备用：直接用 huggingface_hub 下载
    echo "Fallback: downloading via huggingface_hub ..."
    python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(
    repo_id='hkchengrex/MMAudio',
    local_dir='${MODEL_DIR}',
    endpoint='https://hf-mirror.com',
)
print('Fallback download complete!')
"
    }
    echo "Models ready: $(date)"
else
    echo "Models already exist at ${MODEL_DIR}, skip download."
fi

# ── 拉取并启动容器 ────────────────────────────────────────
docker login __ACR_REGISTRY__ -u __ACCESS_KEY__ -p __ACCESS_SECRET__
docker pull __IMAGE__ || echo "Using cached image"

docker stop mmaudio-server 2>/dev/null || true
docker rm   mmaudio-server 2>/dev/null || true

docker run -d \
  --name mmaudio-server \
  --gpus all \
  --restart unless-stopped \
  -p 8000:8000 \
  -v /data/models/MMAudio:/data/models/MMAudio:ro \
  -e MODEL_DIR="/data/models/MMAudio" \
  -e OSS_ACCESS_KEY="__OSS_ACCESS_KEY__" \
  -e OSS_SECRET_KEY="__OSS_SECRET_KEY__" \
  -e OSS_ENDPOINT="__OSS_ENDPOINT__" \
  -e OSS_BUCKET="__OSS_BUCKET__" \
  __IMAGE__

echo "Container started: $(date)"
