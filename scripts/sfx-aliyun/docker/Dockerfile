# ── 推理服务（模型通过 ECS 磁盘挂载，不烘焙进镜像）──────
FROM registry.cn-hangzhou.aliyuncs.com/docker_hub_mirror/pytorch/pytorch:2.3.0-cuda11.8-cudnn8-runtime

WORKDIR /app

RUN apt-get update && apt-get install -y \
    libsndfile1 ffmpeg curl \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    soundfile==0.12.1 \
    oss2==2.18.5 \
    fastapi==0.111.0 \
    "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    --no-deps \
    tangoflux

COPY server.py .

EXPOSE 8000

HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

# MODEL_PATH 默认指向挂载目录，可通过环境变量覆盖
ENV MODEL_PATH=/data/models/TangoFlux

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000"]
