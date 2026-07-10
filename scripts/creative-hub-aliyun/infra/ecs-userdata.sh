#!/bin/bash
exec >> /var/log/creative-hub-startup.log 2>&1
echo "=== Creative Hub Startup: $(date) ==="
until docker info &>/dev/null; do sleep 3; done

pip install -q huggingface_hub

download_if_missing() {
    local REPO=$1 DIR=$2 MARKER=$3
    if [ ! -f "${DIR}/${MARKER}" ]; then
        echo "Downloading ${REPO} ..."
        mkdir -p "${DIR}"
        HF_ENDPOINT=https://hf-mirror.com python3 -c "
from huggingface_hub import snapshot_download
snapshot_download(repo_id='${REPO}', local_dir='${DIR}',
    ignore_patterns=['*.git*','README.md'], endpoint='https://hf-mirror.com')
print('Done: ${REPO}')
"
    else
        echo "Exists: ${DIR}"
    fi
}

# Wan 2.2 TI2V-5B（约 10GB）
download_if_missing "Wan-AI/Wan2.2-TI2V-5B" \
    "/data/models/Wan2.2/Wan2.2-TI2V-5B" "model_index.json"

# TangoFlux（约 2GB）
download_if_missing "declare-lab/TangoFlux" \
    "/data/models/TangoFlux" "config.json"

# ACE-Step 1.5（约 8GB）
download_if_missing "ACE-Step/Ace-Step1.5" \
    "/data/models/ACE-Step-1.5" "model_index.json"

echo "All models ready: $(date)"

docker login __ACR_REGISTRY__ -u __ACCESS_KEY__ -p __ACCESS_SECRET__
docker pull __IMAGE__ || echo "Using cached"
docker stop creative-hub 2>/dev/null || true
docker rm   creative-hub 2>/dev/null || true

docker run -d \
  --name creative-hub \
  --gpus all \
  --restart unless-stopped \
  -p 8000:8000 \
  -v /data/models/Wan2.2:/data/models/Wan2.2:ro \
  -v /data/models/TangoFlux:/data/models/TangoFlux:ro \
  -v /data/models/ACE-Step-1.5:/data/models/ACE-Step-1.5:ro \
  -e WAN_MODEL_DIR="/data/models/Wan2.2" \
  -e WAN_TASK="ti2v-5B" \
  -e TANGOFLUX_DIR="/data/models/TangoFlux" \
  -e ACESTEP_DIR="/data/models/ACE-Step-1.5" \
  -e ACESTEP_CONFIG="acestep-v15-turbo" \
  -e OSS_ACCESS_KEY="__OSS_ACCESS_KEY__" \
  -e OSS_SECRET_KEY="__OSS_SECRET_KEY__" \
  -e OSS_ENDPOINT="__OSS_ENDPOINT__" \
  -e OSS_BUCKET="__OSS_BUCKET__" \
  __IMAGE__

echo "Container started: $(date)"
