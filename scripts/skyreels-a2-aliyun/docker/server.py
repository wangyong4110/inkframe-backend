"""
SkyReels-A2 推理 API 服务
支持：多角色参考图上传 → 视频生成 → OSS 上传 → 返回下载链接
"""
import os
import sys
import uuid
import time
import json
import logging
import tempfile
import shutil
from pathlib import Path
from typing import List, Optional

import oss2
import torch
import uvicorn
from fastapi import FastAPI, UploadFile, File, Form, HTTPException
from fastapi.responses import JSONResponse

# 将 SkyReels-A2 加入 Python 路径
sys.path.insert(0, "/app/SkyReels-A2")

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
logger = logging.getLogger(__name__)

# ── 环境变量 ───────────────────────────────────────────────
MODEL_PATH     = os.environ.get("MODEL_PATH", "/app/model")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))  # 链接有效期(秒)

app = FastAPI(title="SkyReels-A2 API", version="1.0.0")

# ── 全局模型实例（启动时加载，常驻显存）───────────────────
pipeline = None

def load_model():
    global pipeline
    logger.info(f"Loading SkyReels-A2 from {MODEL_PATH} ...")
    t0 = time.time()

    # 导入 SkyReels-A2 推理模块
    # 根据实际仓库结构调整 import 路径
    try:
        from inference import SkyReelsA2Pipeline  # type: ignore
        pipeline = SkyReelsA2Pipeline(
            model_path=MODEL_PATH,
            device="cuda" if torch.cuda.is_available() else "cpu",
            enable_fp8=True,     # FP8 量化，减少约 30% 显存
            enable_offload=True, # 参数 offload，进一步降低峰值 VRAM
        )
    except ImportError:
        # 备用：直接用脚本推理（适配不同版本的仓库结构）
        logger.warning("SkyReelsA2Pipeline not found, using script-based inference")
        pipeline = "script"

    elapsed = round(time.time() - t0, 1)
    logger.info(f"Model loaded in {elapsed}s ✓  CUDA: {torch.cuda.is_available()}")


# ── OSS 客户端 ─────────────────────────────────────────────
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)


def upload_to_oss(local_path: str, oss_key: str) -> str:
    """上传文件到 OSS，返回带签名的临时下载链接"""
    oss_bucket.put_object_from_file(oss_key, local_path)
    url = oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)
    logger.info(f"Uploaded to OSS: {oss_key}")
    return url


def run_inference_script(
    ref_image_paths: List[str],
    prompt: str,
    output_path: str,
    num_frames: int,
    fps: int,
    cfg_scale: float,
    num_steps: int,
    resolution: str,
) -> None:
    """
    通过调用 SkyReels-A2 的推理脚本执行生成。
    适配 script-based 模式（当直接 import 不可用时）。
    """
    import subprocess

    ref_images_arg = ",".join(ref_image_paths)
    width, height = (960, 544) if resolution == "540p" else (1280, 720)

    cmd = [
        "python", "/app/SkyReels-A2/inference.py",
        "--model_path", MODEL_PATH,
        "--ref_images", ref_images_arg,
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

    logger.info(f"Running inference: {' '.join(cmd[:6])} ...")
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=600)

    if result.returncode != 0:
        raise RuntimeError(f"Inference failed:\n{result.stderr}")
    logger.info("Inference script completed ✓")


# ── API 路由 ───────────────────────────────────────────────

@app.on_event("startup")
async def startup_event():
    load_model()


@app.get("/health")
def health():
    """健康检查"""
    gpu_info = {}
    if torch.cuda.is_available():
        gpu_info = {
            "name": torch.cuda.get_device_name(0),
            "vram_total_gb": round(torch.cuda.get_device_properties(0).total_memory / 1e9, 1),
            "vram_free_gb":  round((torch.cuda.get_device_properties(0).total_memory
                                    - torch.cuda.memory_allocated(0)) / 1e9, 1),
        }
    return {
        "status": "ok",
        "model_loaded": pipeline is not None,
        "cuda": torch.cuda.is_available(),
        **gpu_info,
    }


@app.post("/generate")
async def generate(
    # 多张参考图（multipart/form-data）
    ref_images: List[UploadFile] = File(..., description="角色参考图，可上传多张"),
    # 文本参数
    prompt: str = Form(...,  description="视频描述 prompt"),
    num_frames: int  = Form(81,   description="帧数（81=约5s@16fps）"),
    fps: int         = Form(16,   description="帧率"),
    cfg_scale: float = Form(5.0,  description="CFG guidance scale"),
    num_steps: int   = Form(50,   description="推理步数"),
    resolution: str  = Form("540p", description="分辨率：540p 或 720p"),
):
    """
    多角色视频生成接口

    - 上传 1-4 张参考图（角色A、角色B、背景等）
    - 提供文本 prompt 描述场景与动作
    - 返回 OSS 视频下载链接（有效期 2 小时）

    示例 curl：
    ```
    curl -X POST http://host:8000/generate \\
      -F "ref_images=@char_a.jpg" \\
      -F "ref_images=@char_b.jpg" \\
      -F 'prompt=Two women talking in a cafe' \\
      -F 'num_frames=81' \\
      -F 'resolution=540p'
    ```
    """
    if not ref_images:
        raise HTTPException(status_code=400, detail="至少需要上传 1 张参考图")
    if len(ref_images) > 4:
        raise HTTPException(status_code=400, detail="最多支持 4 张参考图")
    if resolution not in ("540p", "720p"):
        raise HTTPException(status_code=400, detail="resolution 必须是 540p 或 720p")

    job_id = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"a2_{job_id}_"))

    try:
        t0 = time.time()
        logger.info(f"[{job_id}] Job start | refs={len(ref_images)} prompt={prompt[:60]!r}")

        # 1. 保存上传的参考图到临时目录
        ref_paths = []
        for i, img_file in enumerate(ref_images):
            suffix = Path(img_file.filename or "ref.jpg").suffix or ".jpg"
            save_path = str(tmp_dir / f"ref_{i}{suffix}")
            with open(save_path, "wb") as f:
                f.write(await img_file.read())
            ref_paths.append(save_path)
            logger.info(f"[{job_id}] Saved ref image {i}: {save_path}")

        # 2. 执行推理
        output_path = str(tmp_dir / "output.mp4")

        if pipeline == "script":
            # script 模式：调用命令行脚本
            run_inference_script(
                ref_image_paths=ref_paths,
                prompt=prompt,
                output_path=output_path,
                num_frames=num_frames,
                fps=fps,
                cfg_scale=cfg_scale,
                num_steps=num_steps,
                resolution=resolution,
            )
        else:
            # Pipeline 模式：直接调用 Python API
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

        # 3. 上传视频到 OSS
        oss_key = f"videos/{job_id}.mp4"
        video_url = upload_to_oss(output_path, oss_key)

        # 4. 上传参考图到 OSS（方便复查）
        ref_urls = []
        for i, ref_path in enumerate(ref_paths):
            rkey = f"videos/{job_id}_ref{i}{Path(ref_path).suffix}"
            rurl = upload_to_oss(ref_path, rkey)
            ref_urls.append(rurl)

        elapsed = round(time.time() - t0, 1)
        logger.info(f"[{job_id}] Done in {elapsed}s → {oss_key}")

        return {
            "job_id":      job_id,
            "status":      "success",
            "video_url":   video_url,          # 视频下载链接（2h 有效）
            "ref_urls":    ref_urls,            # 参考图链接
            "oss_key":     oss_key,
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
        logger.error(f"[{job_id}] Generation failed: {e}", exc_info=True)
        raise HTTPException(status_code=500, detail=str(e))

    finally:
        # 清理临时文件
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.get("/")
def root():
    return {
        "service": "SkyReels-A2 API",
        "docs":    "/docs",
        "health":  "/health",
        "generate": "POST /generate  (multipart/form-data)",
    }


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8000)
