#!/bin/bash
# ══════════════════════════════════════════════════════════
# ECS 实例首次初始化脚本
# 在 ECS 实例上手动执行一次，下载模型到本地磁盘
# 之后实例每次启停无需重新下载
# ══════════════════════════════════════════════════════════
set -e
exec >> /var/log/model-setup.log 2>&1
echo "=== Model Setup: $(date) ==="

MODEL_DIR="/data/models/TangoFlux"

# 已存在则跳过
if [ -f "${MODEL_DIR}/config.json" ]; then
    echo "Model already exists at ${MODEL_DIR}, skip."
    exit 0
fi

mkdir -p "${MODEL_DIR}"

# 安装 huggingface_hub
pip install -q huggingface_hub

# 配置 HuggingFace 镜像（国内加速）
export HF_ENDPOINT=https://hf-mirror.com

echo "Downloading TangoFlux from hf-mirror.com ..."
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

echo "=== Model ready at ${MODEL_DIR} ==="
ls -lh "${MODEL_DIR}"
