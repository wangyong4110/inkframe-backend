# ══════════════════════════════════════════════════════════
# SkyReels-A2 推理服务镜像
# 基础：阿里云 PAI 官方 PyTorch 镜像（国内可直接拉取）
# 模型：SkyReels-A2（Wan2.1 14B 微调，约 28GB 权重）
# ══════════════════════════════════════════════════════════

# ── Stage 1: 下载模型权重 ──────────────────────────────────
FROM registry.cn-hangzhou.aliyuncs.com/docker_hub_mirror/python:3.10-slim AS model-downloader

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    "huggingface_hub[cli]"

# 下载 SkyReels-A2 模型权重（约 28GB，构建时烘焙进镜像）
# 注：首次构建耗时较长，后续 rebuild 会使用 Docker 缓存层
RUN huggingface-cli download Skywork/SkyReels-A2 \
    --local-dir /model-cache \
    --exclude "*.git*" "README.md" "docs"

# ── Stage 2: 推理服务 ──────────────────────────────────────
FROM registry.cn-shanghai.aliyuncs.com/pai-dlc/pytorch-training:2.3.0-gpu-py310-cu121-ubuntu22.04

WORKDIR /app

# 系统依赖
RUN apt-get update && apt-get install -y \
    libgl1-mesa-glx \
    libglib2.0-0 \
    libsm6 \
    libxext6 \
    libxrender-dev \
    ffmpeg \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

# 克隆 SkyReels-A2 推理代码
RUN git clone https://github.com/SkyworkAI/SkyReels-A2.git /app/SkyReels-A2

# 安装推理依赖
WORKDIR /app/SkyReels-A2
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -r requirements.txt

# 安装 API 服务依赖
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    fastapi==0.111.0 \
    uvicorn[standard]==0.30.0 \
    python-multipart==0.0.9 \
    oss2==2.18.5 \
    aiofiles==23.2.1

WORKDIR /app

# 从 Stage 1 复制模型权重
COPY --from=model-downloader /model-cache /app/model

# 复制 API 服务代码
COPY server.py .

EXPOSE 8000

HEALTHCHECK --interval=15s --timeout=10s --retries=5 \
    CMD curl -f http://localhost:8000/health || exit 1

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000", "--timeout-keep-alive", "300"]
