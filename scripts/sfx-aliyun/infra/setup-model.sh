#!/bin/bash
# ECS 实例首次初始化：下载 TangoFlux 模型到本地磁盘
# 执行一次即可，实例每次启停无需重新下载
set -e
exec >> /var/log/model-setup.log 2>&1
echo "=== Model Setup: $(date) ==="

MODEL_DIR="/data/models/TangoFlux"

if [ -f "${MODEL_DIR}/config.json" ]; then
    echo "Model already exists at ${MODEL_DIR}, skip."
    exit 0
fi

mkdir -p "${MODEL_DIR}"
pip install -q huggingface_hub

export HF_ENDPOINT=https://hf-mirror.com
python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(
    repo_id='declare-lab/TangoFlux',
    local_dir='${MODEL_DIR}',
    ignore_patterns=['*.md', '*.txt'],
    endpoint='https://hf-mirror.com',
)
print('Download complete!')
"

echo "=== Model ready: $(date) ==="
ls -lh "${MODEL_DIR}"
