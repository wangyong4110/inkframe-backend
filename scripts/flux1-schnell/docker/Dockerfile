FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

WORKDIR /app
RUN apt-get update && apt-get install -y curl git && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    diffusers>=0.31.0 transformers accelerate sentencepiece \
    fastapi==0.111.0 "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9 oss2==2.18.5 Pillow

COPY server.py .
EXPOSE 8000
HEALTHCHECK --interval=10s --timeout=5s --retries=5 CMD curl -f http://localhost:8000/health || exit 1
ENV MODEL_DIR=/data/models/FLUX.1-schnell
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000"]
