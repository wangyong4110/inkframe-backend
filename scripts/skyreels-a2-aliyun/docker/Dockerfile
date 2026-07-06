FROM pytorch/pytorch:2.3.0-cuda11.8-cudnn8-runtime

WORKDIR /app

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
    "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9 \
    oss2==2.18.5 \
    aiofiles==23.2.1

WORKDIR /app
COPY server.py .

EXPOSE 8000

HEALTHCHECK --interval=15s --timeout=10s --retries=5 \
    CMD curl -f http://localhost:8000/health || exit 1

# 模型通过 ECS 磁盘挂载
ENV MODEL_PATH=/data/models/SkyReels-A2

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000", "--timeout-keep-alive", "300"]
