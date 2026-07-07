#!/bin/bash
exec >> /var/log/hunyuan15-startup.log 2>&1
echo "=== Startup: $(date) ==="
until docker info &>/dev/null; do sleep 3; done

MODEL_DIR="/data/models/HunyuanVideo-1.5"
if [ ! -f "${MODEL_DIR}/model_index.json" ]; then
    echo "Downloading tencent/HunyuanVideo-1.5 from hf-mirror.com ..."
    mkdir -p "${MODEL_DIR}"
    pip install -q huggingface_hub
    HF_ENDPOINT=https://hf-mirror.com python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(repo_id='tencent/HunyuanVideo-1.5', local_dir='${MODEL_DIR}',
    ignore_patterns=['*.git*','README.md'], endpoint='https://hf-mirror.com')
print('Download complete!')
"
    echo "Model ready: $(date)"
else
    echo "Model exists, skip download."
fi

docker login __ACR_REGISTRY__ -u __ACCESS_KEY__ -p __ACCESS_SECRET__
docker pull __IMAGE__ || echo "Using cached"
docker stop hunyuan15 2>/dev/null || true
docker rm   hunyuan15 2>/dev/null || true

docker run -d --name hunyuan15 --gpus all --restart unless-stopped -p 8000:8000   -v ${MODEL_DIR}:${MODEL_DIR}:ro   -e MODEL_DIR="${MODEL_DIR}"      -e OSS_ACCESS_KEY="__OSS_ACCESS_KEY__"   -e OSS_SECRET_KEY="__OSS_SECRET_KEY__"   -e OSS_ENDPOINT="__OSS_ENDPOINT__"   -e OSS_BUCKET="__OSS_BUCKET__"   __IMAGE__

echo "Container started: $(date)"
