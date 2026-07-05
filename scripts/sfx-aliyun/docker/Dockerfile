# ── Stage 1: 下载模型权重 ──────────────────────────────────
FROM registry.cn-hangzhou.aliyuncs.com/docker_hub_mirror/python:3.10-slim AS model-downloader

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    huggingface_hub

# 构建时下载模型权重，烘焙进镜像（约 2GB）
RUN python -c "\
from huggingface_hub import snapshot_download; \
snapshot_download(\
    repo_id='declare-lab/TangoFlux', \
    local_dir='/model-cache', \
    ignore_patterns=['*.md', '*.txt'] \
)"

# ── Stage 2: 推理服务 ──────────────────────────────────────
FROM registry.cn-shanghai.aliyuncs.com/pai-dlc/pytorch-training:2.3.0-gpu-py310-cu121-ubuntu22.04

WORKDIR /app

# 安装系统依赖
RUN apt-get update && apt-get install -y \
    libsndfile1 ffmpeg curl \
    && rm -rf /var/lib/apt/lists/*

# 安装 Python 依赖
COPY requirements.txt .
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -r requirements.txt

# 从 Stage 1 复制模型权重
COPY --from=model-downloader /model-cache /app/model

# 复制服务代码
COPY server.py .

EXPOSE 8000

HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000"]
