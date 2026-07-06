FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

WORKDIR /app

# 系统依赖（ffmpeg 用于视频解码，av 库依赖）
RUN apt-get update && apt-get install -y \
    ffmpeg \
    libavcodec-dev \
    libavformat-dev \
    libavutil-dev \
    libsm6 \
    libxext6 \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

# torchvision + torchaudio（MMAudio 必须）
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    torchvision \
    torchaudio

# 克隆 MMAudio 仓库
RUN git clone https://github.com/hkchengrex/MMAudio.git /app/MMAudio

# 安装 MMAudio 依赖
WORKDIR /app/MMAudio
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -e .

# 安装 API 服务依赖
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    fastapi==0.111.0 \
    "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9 \
    oss2==2.18.5 \
    av

WORKDIR /app
COPY server.py .

EXPOSE 8000

HEALTHCHECK --interval=15s --timeout=10s --retries=5 \
    CMD curl -f http://localhost:8000/health || exit 1

# 模型通过 ECS 磁盘挂载到 /data/models/MMAudio
ENV MODEL_DIR=/data/models/MMAudio

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000"]
