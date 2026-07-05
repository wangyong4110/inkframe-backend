# ── Stage 1: 下载模型权重 ──────────────────────────────────
FROM registry.cn-hangzhou.aliyuncs.com/docker_hub_mirror/python:3.10-slim AS model-downloader

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    huggingface_hub

# 构建时下载模型权重，烘焙进镜像（约 2GB）
# 单独一层：只要 repo_id 不变，Docker 缓存永久有效
RUN python -c "\
from huggingface_hub import snapshot_download; \
snapshot_download(\
    repo_id='declare-lab/TangoFlux', \
    local_dir='/model-cache', \
    ignore_patterns=['*.md', '*.txt'] \
)"

# ── Stage 2: 推理服务 ──────────────────────────────────────
# pai-dlc 镜像已内置：torch 2.3.0 + CUDA 12.1 + cuDNN
FROM registry.cn-shanghai.aliyuncs.com/pai-dlc/pytorch-training:2.3.0-gpu-py310-cu121-ubuntu22.04

WORKDIR /app

# 系统依赖（单独一层，几乎不变，缓存稳定）
RUN apt-get update && apt-get install -y \
    libsndfile1 ffmpeg curl \
    && rm -rf /var/lib/apt/lists/*

# 先安装无 CUDA 依赖的轻量包（缓存友好，秒级完成）
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    soundfile==0.12.1 \
    oss2==2.18.5 \
    fastapi==0.111.0 \
    "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9

# 单独安装 tangoflux（依赖 torch，但不重新下载 torch）
# 放最后一层：tangoflux 版本更新时只重建这一层
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    --no-deps \
    tangoflux

# 从 Stage 1 复制模型权重（约 2GB，独立缓存层）
COPY --from=model-downloader /model-cache /app/model

# 复制服务代码（最后复制，代码改动不影响上面各层缓存）
COPY server.py .

EXPOSE 8000

HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000"]
