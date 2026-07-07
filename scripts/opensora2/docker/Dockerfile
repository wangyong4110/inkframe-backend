FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

WORKDIR /app
RUN apt-get update && apt-get install -y ffmpeg curl git && rm -rf /var/lib/apt/lists/*

# 克隆 Open-Sora 仓库
RUN git clone https://github.com/hpcaitech/Open-Sora.git /app/Open-Sora
WORKDIR /app/Open-Sora
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -e ".[inference]"

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    fastapi==0.111.0 "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9 oss2==2.18.5

WORKDIR /app
COPY server.py .

EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=10s --retries=5 CMD curl -f http://localhost:8000/health || exit 1
ENV MODEL_DIR=/data/models/OpenSora2
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000", "--timeout-keep-alive", "300"]
