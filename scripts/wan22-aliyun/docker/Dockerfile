FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

WORKDIR /app

RUN apt-get update && apt-get install -y \
    ffmpeg \
    libgl1-mesa-glx \
    libglib2.0-0 \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

# 克隆 Wan2.2 推理代码
RUN git clone https://github.com/Wan-Video/Wan2.2.git /app/Wan2.2

# 安装 Wan2.2 依赖（flash_attn 放最后，单独安装避免超时）
WORKDIR /app/Wan2.2
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
# MODEL_TASK: t2v-5B（默认，TI2V-5B，单卡 24GB）| t2v-A14B（A100 80G）
ENV MODEL_DIR=/data/models/Wan2.2
ENV MODEL_TASK=ti2v-5B

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000", "--timeout-keep-alive", "300"]
