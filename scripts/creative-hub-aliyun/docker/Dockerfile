# ══════════════════════════════════════════════════════════════
# Creative Hub — Wan 2.2 TI2V-5B + TangoFlux + ACE-Step 1.5
# A10 24GB，VRAM 分配：视频 ~10GB / 音效 ~6GB / 音乐 ~4GB
# ══════════════════════════════════════════════════════════════
FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

WORKDIR /app

RUN apt-get update && apt-get install -y \
    ffmpeg libsndfile1 curl git \
    && rm -rf /var/lib/apt/lists/*

# ── Wan 2.2 推理代码 ──────────────────────────────────────────
RUN git clone https://github.com/Wan-Video/Wan2.2.git /app/Wan2.2
WORKDIR /app/Wan2.2
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -r requirements.txt

# ── TangoFlux ─────────────────────────────────────────────────
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    --no-deps tangoflux soundfile

# ── ACE-Step 1.5 ──────────────────────────────────────────────
RUN git clone https://github.com/ace-step/ACE-Step-1.5.git /app/ACE-Step-1.5
WORKDIR /app/ACE-Step-1.5
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -e . || pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    -e ".[cuda]"

# ── 公共依赖 ──────────────────────────────────────────────────
RUN pip install --no-cache-dir \
    -i https://mirrors.aliyun.com/pypi/simple/ \
    fastapi==0.111.0 "uvicorn[standard]==0.30.0" \
    python-multipart==0.0.9 oss2==2.18.5

WORKDIR /app
COPY server.py .

EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=10s --retries=5 \
    CMD curl -f http://localhost:8000/health || exit 1

# ── 环境变量（通过 ecs-userdata 挂载模型目录后注入）──────────
ENV WAN_MODEL_DIR=/data/models/Wan2.2
ENV WAN_TASK=ti2v-5B
ENV TANGOFLUX_DIR=/data/models/TangoFlux
ENV ACESTEP_DIR=/data/models/ACE-Step-1.5
ENV ACESTEP_CONFIG=acestep-v15-turbo

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8000", "--timeout-keep-alive", "600"]
