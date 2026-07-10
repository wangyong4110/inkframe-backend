"""
Creative Hub — Wan 2.2 TI2V-5B + TangoFlux + ACE-Step 1.5
===========================================================
POST /video        文本转视频（Wan 2.2 T2V）
POST /video/i2v    图片转视频（Wan 2.2 I2V，multipart）
POST /sfx          文本转音效（TangoFlux）
POST /music        文本+歌词转音乐（ACE-Step 1.5）
GET  /health       服务状态

显存策略（A10 24GB）：
  Wan 2.2    通过子进程调用 generate.py，推理完毕显存自动释放
  TangoFlux  FastAPI 进程内持有，6GB 常驻（推理锁保护）
  ACE-Step   独立子进程，CPU offload，~4GB 激活层常驻
  三者互不冲突，顺序调用时峰值约 10-14GB，远低于 24GB
"""
import os, sys, uuid, time, shutil, logging, tempfile, subprocess, threading
from pathlib import Path
from typing import Optional

import torch, oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field
import requests as req_lib

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger(__name__)

# ── 环境变量 ────────────────────────────────────────────────
WAN_MODEL_DIR  = os.environ.get("WAN_MODEL_DIR",  "/data/models/Wan2.2")
WAN_TASK       = os.environ.get("WAN_TASK",       "ti2v-5B")
TANGOFLUX_DIR  = os.environ.get("TANGOFLUX_DIR",  "/data/models/TangoFlux")
ACESTEP_DIR    = os.environ.get("ACESTEP_DIR",    "/data/models/ACE-Step-1.5")
ACESTEP_CONFIG = os.environ.get("ACESTEP_CONFIG", "acestep-v15-turbo")

OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))

DEVICE = "cuda" if torch.cuda.is_available() else "cpu"
DTYPE  = torch.bfloat16 if torch.cuda.is_available() else torch.float32

WAN_CKPT_MAP = {
    "ti2v-5B":  "Wan2.2-TI2V-5B",
    "t2v-A14B": "Wan2.2-T2V-A14B",
    "i2v-A14B": "Wan2.2-I2V-A14B",
}
WAN_SIZE_MAP = {
    "ti2v-5B":  "1280*704",
    "t2v-A14B": "1280*720",
    "i2v-A14B": "1280*720",
}

oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)

# ── 模型状态 ────────────────────────────────────────────────
_tangoflux_model = None
_tangoflux_lock  = threading.Lock()   # TangoFlux 推理互斥
_load_status     = {"wan22": False, "tangoflux": False, "acestep": False}
_acestep_proc: Optional[subprocess.Popen] = None
ACESTEP_INNER = "http://127.0.0.1:8001"

app = FastAPI(title="Creative Hub", description="Wan 2.2 + TangoFlux + ACE-Step 1.5")


# ── 显存监控 ────────────────────────────────────────────────
def _free_vram() -> float:
    if not torch.cuda.is_available(): return 99.0
    p = torch.cuda.get_device_properties(0)
    return (p.total_memory - torch.cuda.memory_allocated()) / 1e9


# ══════════════════════════════════════════════════════════════
# 模型加载
# ══════════════════════════════════════════════════════════════

def _load_tangoflux():
    global _tangoflux_model
    if _tangoflux_model is not None:
        return _tangoflux_model
    from tangoflux import TangoFlux
    logger.info(f"[TangoFlux] Loading from {TANGOFLUX_DIR} ...")
    t0 = time.time()
    _tangoflux_model = TangoFlux.from_pretrained(TANGOFLUX_DIR).to(DEVICE).eval()
    _load_status["tangoflux"] = True
    logger.info(f"[TangoFlux] Loaded in {round(time.time()-t0,1)}s ✓  free={_free_vram():.1f}GB")
    return _tangoflux_model


def _start_acestep():
    global _acestep_proc
    if _acestep_proc and _acestep_proc.poll() is None:
        return
    logger.info("[ACE-Step] Starting inner API :8001 ...")
    cmd = [
        sys.executable, "/app/ACE-Step-1.5/acestep/api_server.py",
        "--port", "8001",
        "--config_path", ACESTEP_CONFIG,
        "--checkpoint_path", ACESTEP_DIR,
        "--cpu_offload", "true",
        "--torch_compile", "false",
        "--init_service", "true",
    ]
    _acestep_proc = subprocess.Popen(cmd, cwd="/app/ACE-Step-1.5")
    deadline = time.time() + 180
    while time.time() < deadline:
        try:
            if req_lib.get(f"{ACESTEP_INNER}/health", timeout=3).status_code == 200:
                _load_status["acestep"] = True
                logger.info(f"[ACE-Step] Ready ✓  free={_free_vram():.1f}GB")
                return
        except: pass
        time.sleep(5)
    logger.error("[ACE-Step] Failed to start within 180s")


def _check_wan():
    ckpt = os.path.join(WAN_MODEL_DIR, WAN_CKPT_MAP[WAN_TASK])
    if os.path.exists(ckpt):
        _load_status["wan22"] = True
        logger.info(f"[Wan2.2] Model ready at {ckpt} ✓")
    else:
        logger.warning(f"[Wan2.2] Model not found: {ckpt}")
        _load_status["wan22"] = True   # health 返回 ok，推理时报错


# ══════════════════════════════════════════════════════════════
# 启动：TangoFlux 常驻，ACE-Step 子进程，Wan 2.2 按需子进程
# ══════════════════════════════════════════════════════════════

@app.on_event("startup")
async def startup():
    _check_wan()
    _load_tangoflux()    # 6GB 常驻，占用最小，优先加载
    _start_acestep()     # CPU offload 子进程，~4GB 激活层
    logger.info(f"[Hub] All ready  free VRAM={_free_vram():.1f}GB")


# ── 公共工具 ────────────────────────────────────────────────

def _upload(path: str, key: str) -> str:
    oss_bucket.put_object_from_file(key, path)
    return oss_bucket.sign_url("GET", key, OSS_URL_EXPIRE)


def _run_wan(
    task: str,
    prompt: str,
    size: str,
    num_frames: int,
    sample_steps: int,
    cfg_scale: float,
    output_dir: str,
    image_path: Optional[str] = None,
) -> str:
    """
    通过子进程调用 Wan2.2 generate.py。
    子进程独立管理显存，结束后 GPU 完全释放。
    24GB 下 ti2v-5B 峰值约 10GB，结束后恢复。
    """
    ckpt_dir = os.path.join(WAN_MODEL_DIR, WAN_CKPT_MAP[task])
    cmd = [
        "python", "/app/Wan2.2/generate.py",
        "--task",               task,
        "--size",               size,
        "--ckpt_dir",           ckpt_dir,
        "--prompt",             prompt,
        "--save_file",          os.path.join(output_dir, "output.mp4"),
        "--sample_steps",       str(sample_steps),
        "--sample_guide_scale", str(cfg_scale),
        "--frame_num",          str(num_frames),
        "--offload_model",      "True",
        "--t5_cpu",
    ]
    if image_path:
        cmd += ["--image", image_path]

    logger.info(f"[Wan2.2] {' '.join(cmd[:6])} ...")
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=900, cwd="/app/Wan2.2")
    if result.returncode != 0:
        raise RuntimeError(f"Wan2.2 failed:\n{result.stderr[-2000:]}")

    out = os.path.join(output_dir, "output.mp4")
    if not os.path.exists(out):
        raise RuntimeError("No output.mp4 found")
    return out


# ══════════════════════════════════════════════════════════════
# 路由
# ══════════════════════════════════════════════════════════════

@app.get("/health")
def health():
    props = torch.cuda.get_device_properties(0) if torch.cuda.is_available() else None
    return {
        "status":       "ok",
        "cuda":         torch.cuda.is_available(),
        "device":       props.name if props else "cpu",
        "vram_total_gb": round(props.total_memory / 1e9, 1) if props else None,
        "vram_free_gb":  round(_free_vram(), 1),
        "models":        _load_status,
        "endpoints": {
            "/video":     "POST — 文本转视频 JSON",
            "/video/i2v": "POST — 图片转视频 multipart",
            "/sfx":       "POST — 文本转音效 JSON",
            "/music":     "POST — 文本+歌词转音乐 JSON",
        },
    }


# ── /video  Wan 2.2 T2V ─────────────────────────────────────

class VideoRequest(BaseModel):
    prompt:       str   = Field(..., min_length=2, max_length=1000)
    size:         str   = Field("",  description="留空使用默认 1280*704")
    num_frames:   int   = Field(81,  ge=16, le=241, description="81≈5s@16fps")
    sample_steps: int   = Field(50,  ge=10, le=100)
    cfg_scale:    float = Field(5.0, ge=1.0, le=10.0)
    seed:         int   = Field(-1)


@app.post("/video", summary="文本转视频（Wan 2.2 TI2V-5B）")
async def video_t2v(req: VideoRequest):
    """
    文本转视频，支持中英文 prompt。

    ```bash
    curl -X POST http://host:8000/video -H 'Content-Type: application/json' -d '{
      "prompt": "A serene lake at sunrise, mist rising from the water, golden light",
      "num_frames": 81
    }'
    ```
    """
    job_id  = uuid.uuid4().hex
    tmp_dir = tempfile.mkdtemp(prefix=f"vid_{job_id}_")
    t0      = time.time()
    size    = req.size or WAN_SIZE_MAP[WAN_TASK]
    logger.info(f"[VIDEO/{job_id[:6]}] {req.prompt[:60]!r} frames={req.num_frames}")

    try:
        out_path = _run_wan(
            task=WAN_TASK, prompt=req.prompt, size=size,
            num_frames=req.num_frames, sample_steps=req.sample_steps,
            cfg_scale=req.cfg_scale, output_dir=tmp_dir,
        )
        oss_key = f"videos/{job_id}.mp4"
        url = _upload(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        logger.info(f"[VIDEO/{job_id[:6]}] Done {elapsed}s")
        return {
            "job_id": job_id, "status": "success",
            "video_url": url, "elapsed_sec": elapsed,
            "model": "wan2.2-ti2v-5b", "type": "video",
            "params": {"prompt": req.prompt, "size": size, "num_frames": req.num_frames},
        }
    except Exception as e:
        logger.error(f"[VIDEO] {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.post("/video/i2v", summary="图片转视频（Wan 2.2 TI2V-5B I2V）")
async def video_i2v(
    image:        UploadFile = File(...),
    prompt:       str        = Form(...),
    size:         str        = Form(""),
    num_frames:   int        = Form(81),
    sample_steps: int        = Form(50),
    cfg_scale:    float      = Form(5.0),
):
    """
    上传参考图，生成从该图像出发的视频。

    ```bash
    curl -X POST http://host:8000/video/i2v \\
      -F 'image=@cat.jpg' \\
      -F 'prompt=The cat starts walking toward the camera' \\
      -F 'num_frames=81'
    ```
    """
    job_id  = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"i2v_{job_id}_"))
    t0      = time.time()

    try:
        suffix = Path(image.filename or "input.jpg").suffix or ".jpg"
        img_path = str(tmp_dir / f"input{suffix}")
        with open(img_path, "wb") as f:
            f.write(await image.read())

        actual_size = size or WAN_SIZE_MAP[WAN_TASK]
        logger.info(f"[I2V/{job_id[:6]}] {prompt[:60]!r} frames={num_frames}")

        out_path = _run_wan(
            task=WAN_TASK, prompt=prompt, size=actual_size,
            num_frames=num_frames, sample_steps=sample_steps,
            cfg_scale=cfg_scale, output_dir=str(tmp_dir),
            image_path=img_path,
        )
        oss_key = f"videos/{job_id}.mp4"
        url = _upload(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        logger.info(f"[I2V/{job_id[:6]}] Done {elapsed}s")
        return {
            "job_id": job_id, "status": "success",
            "video_url": url, "elapsed_sec": elapsed,
            "model": "wan2.2-ti2v-5b", "type": "i2v",
        }
    except Exception as e:
        logger.error(f"[I2V] {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(str(tmp_dir), ignore_errors=True)


# ── /sfx  TangoFlux ─────────────────────────────────────────

class SfxRequest(BaseModel):
    prompt:    str   = Field(..., min_length=3, max_length=500)
    duration:  float = Field(10.0, ge=1, le=30)
    steps:     int   = Field(50,   ge=20, le=100)
    cfg_scale: float = Field(4.5,  ge=1,  le=10)
    seed:      int   = Field(-1)


@app.post("/sfx", summary="文本转音效（TangoFlux）")
async def sfx(req: SfxRequest):
    """
    精确 Foley 音效，44.1kHz WAV。

    ```bash
    curl -X POST http://host:8000/sfx -H 'Content-Type: application/json' -d '{
      "prompt": "heavy rain on a metal roof with distant thunder",
      "duration": 10
    }'
    ```
    """
    import soundfile as sf
    job_id = uuid.uuid4().hex
    t0     = time.time()
    logger.info(f"[SFX/{job_id[:6]}] {req.prompt[:60]!r} {req.duration}s")

    with _tangoflux_lock:
        model = _load_tangoflux()
        try:
            gen = torch.Generator(DEVICE).manual_seed(req.seed) if req.seed >= 0 else None
            with torch.no_grad():
                audio = model.generate(
                    prompt=req.prompt,
                    duration=req.duration,
                    num_inference_steps=req.steps,
                    guidance_scale=req.cfg_scale,
                    generator=gen,
                )
        except Exception as e:
            raise HTTPException(500, str(e))

    local = f"/tmp/{job_id}.wav"
    sf.write(local, audio.cpu().numpy(), 44100)
    oss_key = f"sfx/{job_id}.wav"
    url = _upload(local, oss_key)
    Path(local).unlink(missing_ok=True)
    elapsed = round(time.time() - t0, 2)
    logger.info(f"[SFX/{job_id[:6]}] Done {elapsed}s")
    return {
        "job_id": job_id, "url": url, "elapsed": elapsed,
        "model": "tangoflux", "type": "sfx",
        "prompt": req.prompt, "duration": req.duration,
    }


# ── /music  ACE-Step ────────────────────────────────────────

class MusicRequest(BaseModel):
    prompt:    str   = Field(..., min_length=2, max_length=1000)
    lyrics:    str   = Field("",  description="歌词（可选），支持 [verse]/[chorus] 结构")
    duration:  float = Field(30.0, ge=10, le=240)
    num_steps: int   = Field(8,    ge=4,  le=50,  description="turbo 模型建议 8")
    guidance:  float = Field(7.0,  ge=1,  le=15)
    seed:      int   = Field(-1)
    format:    str   = Field("mp3", description="mp3 / wav / flac")


@app.post("/music", summary="文本+歌词转音乐（ACE-Step 1.5）")
async def music(req: MusicRequest):
    """
    完整歌曲生成，支持人声+歌词，50+ 语言，Apache 2.0。

    ```bash
    curl -X POST http://host:8000/music -H 'Content-Type: application/json' -d '{
      "prompt": "cinematic orchestral, epic, dramatic, film score",
      "duration": 60
    }'
    ```
    """
    _start_acestep()
    if not _load_status["acestep"]:
        raise HTTPException(503, "ACE-Step not ready")

    job_id = uuid.uuid4().hex
    t0     = time.time()
    logger.info(f"[MUSIC/{job_id[:6]}] {req.prompt[:60]!r} {req.duration}s")

    try:
        resp = req_lib.post(
            f"{ACESTEP_INNER}/generate",
            json={
                "prompt":         req.prompt,
                "lyrics":         req.lyrics or "",
                "audio_duration": req.duration,
                "infer_step":     req.num_steps,
                "guidance_scale": req.guidance,
                "seed":           req.seed if req.seed >= 0 else None,
                "format":         req.format,
            },
            timeout=300,
        )
        if resp.status_code != 200:
            raise HTTPException(500, f"ACE-Step {resp.status_code}: {resp.text[:300]}")

        result     = resp.json()
        audio_path = result.get("audio_path") or result.get("output_path")

        if not audio_path or not Path(audio_path).exists():
            import base64 as b64
            raw = result.get("audio_base64") or result.get("audio")
            if raw:
                audio_path = f"/tmp/{job_id}.{req.format}"
                with open(audio_path, "wb") as f:
                    f.write(b64.b64decode(raw))
            else:
                raise HTTPException(500, "No audio from ACE-Step")

        ext     = Path(audio_path).suffix or f".{req.format}"
        oss_key = f"music/{job_id}{ext}"
        url     = _upload(audio_path, oss_key)
        elapsed = round(time.time() - t0, 2)
        logger.info(f"[MUSIC/{job_id[:6]}] Done {elapsed}s")
        return {
            "job_id": job_id, "url": url, "elapsed": elapsed,
            "model": "acestep-1.5", "type": "music",
            "prompt": req.prompt, "duration": req.duration,
        }
    except HTTPException: raise
    except Exception as e:
        logger.error(f"[MUSIC] {e}", exc_info=True)
        raise HTTPException(500, str(e))


@app.get("/")
def root():
    return {
        "service": "Creative Hub",
        "models":  ["Wan 2.2 TI2V-5B", "TangoFlux", "ACE-Step 1.5"],
        "docs":    "/docs",
        "endpoints": {
            "/video":     "POST — 文本转视频（JSON）",
            "/video/i2v": "POST — 图片转视频（multipart）",
            "/sfx":       "POST — 文本转音效（JSON）",
            "/music":     "POST — 文本+歌词转音乐（JSON）",
            "/health":    "GET  — 服务状态 + 显存信息",
        },
    }
