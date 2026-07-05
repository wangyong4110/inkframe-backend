#!/bin/bash
exec >> /var/log/sfx-startup.log 2>&1
echo "=== SFX Startup: $(date) ==="

until docker info &>/dev/null; do sleep 3; done

if [ ! -f "/data/models/TangoFlux/config.json" ]; then
    echo "ERROR: Model not found. Run: bash /opt/setup-model.sh"
    exit 1
fi

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
