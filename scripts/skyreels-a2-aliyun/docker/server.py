"""
SkyReels-A2 推理 API 服务
支持多张参考图上传 → 视频生成 → OSS 上传 → 返回下载链接
"""
import os
import sys
import uuid
import time
import shutil
import logging
import tempfile
from pathlib import Path
from typing import List

import torch
import oss2
from fastapi import FastAPI, UploadFile, File, Form, HTTPException

sys.path.insert(0, "/app/SkyReels-A2")

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
logger = logging.getLogger(__name__)

# ── 环境变量 ───────────────────────────────────────────────
MODEL_PATH     = os.environ.get("MODEL_PATH", "/data/models/SkyReels-A2")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))

app = FastAPI(title="SkyReels-A2 API", version="1.0.0")

# ── OSS 客户端 ─────────────────────────────────────────────
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)

# ── 全局 pipeline（启动时加载）────────────────────────────
pipeline = None

def load_model():
    global pipeline
    logger.info(f"Loading SkyReels-A2 from {MODEL_PATH} ...")
    t0 = time.time()
    try:
        from inference import SkyReelsA2Pipeline  # type: ignore
        pipeline = SkyReelsA2Pipeline(
            model_path=MODEL_PATH,
            device="cuda" if torch.cuda.is_available() else "cpu",
            enable_fp8=True,
            enable_offload=True,
        )
    except ImportError:
        logger.warning("SkyReelsA2Pipeline not found, using script mode")
        pipeline = "script"
    elapsed = round(time.time() - t0, 1)
    logger.info(f"Model loaded in {elapsed}s ✓")


def upload_to_oss(local_path: str, oss_key: str) -> str:
    oss_bucket.put_object_from_file(oss_key, local_path)
    return oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)


def run_script(ref_paths, prompt, output_path, num_frames, fps, cfg_scale, num_steps, resolution):
    import subprocess
    width, height = (960, 544) if resolution == "540p" else (1280, 720)
    cmd = [
        "python", "/app/SkyReels-A2/inference.py",
        "--model_path", MODEL_PATH,
        "--ref_images", ",".join(ref_paths),
        "--prompt", prompt,
        "--output_path", output_path,
        "--num_frames", str(num_frames),
        "--fps", str(fps),
        "--guidance_scale", str(cfg_scale),
        "--num_inference_steps", str(num_steps),
        "--width", str(width),
        "--height", str(height),
        "--enable_fp8",
        "--enable_offload",
    ]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
    if result.returncode != 0:
        raise RuntimeError(f"Inference failed:\n{result.stderr}")


# ── 路由 ───────────────────────────────────────────────────

@app.on_event("startup")
async def startup():
    load_model()


@app.get("/health")
def health():
    gpu_info = {}
    if torch.cuda.is_available():
        gpu_info = {
            "name":         torch.cuda.get_device_name(0),
            "vram_total_gb": round(torch.cuda.get_device_properties(0).total_memory / 1e9, 1),
            "vram_free_gb":  round(
                (torch.cuda.get_device_properties(0).total_memory - torch.cuda.memory_allocated())
                / 1e9, 1
            ),
        }
    return {
        "status":       "ok",
        "model_loaded": pipeline is not None,
        "cuda":         torch.cuda.is_available(),
        **gpu_info,
    }


@app.post("/generate")
async def generate(
    ref_images: List[UploadFile] = File(...,   description="角色参考图，可上传多张（最多4张）"),
    prompt:     str   = Form(...,              description="视频描述"),
    num_frames: int   = Form(81,               description="帧数（81≈5s@16fps）"),
    fps:        int   = Form(16),
    cfg_scale:  float = Form(5.0),
    num_steps:  int   = Form(50),
    resolution: str   = Form("540p",           description="540p 或 720p"),
):
    """
    多角色视频生成

    示例：
    curl -X POST http://host:8000/generate \\
      -F "ref_images=@char_a.jpg" \\
      -F "ref_images=@char_b.jpg" \\
      -F 'prompt=Two women talking in a cafe' \\
      -F 'resolution=540p'
    """
    if not ref_images:
        raise HTTPException(400, "至少需要上传 1 张参考图")
    if len(ref_images) > 4:
        raise HTTPException(400, "最多支持 4 张参考图")
    if resolution not in ("540p", "720p"):
        raise HTTPException(400, "resolution 必须是 540p 或 720p")

    job_id  = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"a2_{job_id}_"))
    t0      = time.time()

    try:
        logger.info(f"[{job_id}] refs={len(ref_images)} prompt={prompt[:60]!r}")

        # 保存参考图
        ref_paths = []
        for i, img in enumerate(ref_images):
            suffix = Path(img.filename or "ref.jpg").suffix or ".jpg"
            p = str(tmp_dir / f"ref_{i}{suffix}")
            with open(p, "wb") as f:
                f.write(await img.read())
            ref_paths.append(p)

        output_path = str(tmp_dir / "output.mp4")

        # 推理
        if pipeline == "script":
            run_script(ref_paths, prompt, output_path, num_frames, fps, cfg_scale, num_steps, resolution)
        else:
            with torch.no_grad():
                pipeline.generate(
                    ref_images=ref_paths,
                    prompt=prompt,
                    output_path=output_path,
                    num_frames=num_frames,
                    fps=fps,
                    guidance_scale=cfg_scale,
                    num_inference_steps=num_steps,
                )

        if not Path(output_path).exists():
            raise RuntimeError("推理完成但未找到输出文件")

        # 上传视频
        video_key = f"videos/{job_id}.mp4"
        video_url = upload_to_oss(output_path, video_key)

        # 上传参考图（可复查）
        ref_urls = []
        for i, p in enumerate(ref_paths):
            rkey = f"videos/{job_id}_ref{i}{Path(p).suffix}"
            ref_urls.append(upload_to_oss(p, rkey))

        elapsed = round(time.time() - t0, 1)
        logger.info(f"[{job_id}] Done in {elapsed}s")

        return {
            "job_id":      job_id,
            "status":      "success",
            "video_url":   video_url,
            "ref_urls":    ref_urls,
            "elapsed_sec": elapsed,
            "params": {
                "prompt":     prompt,
                "num_frames": num_frames,
                "fps":        fps,
                "resolution": resolution,
                "num_refs":   len(ref_images),
            },
        }

    except Exception as e:
        logger.error(f"[{job_id}] Failed: {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.get("/")
def root():
    return {"service": "SkyReels-A2 API", "docs": "/docs", "generate": "POST /generate"}
