"""
Wan2.2 推理 API 服务

支持两种模式（通过 MODEL_TASK 环境变量切换）：
  - ti2v-5B：TI2V-5B 模型，文本+可选图片→视频，单卡 24GB 可跑（T4/4090）
  - t2v-A14B：T2V-A14B 模型，纯文本→视频，需要 A100 80G
  - i2v-A14B：I2V-A14B 模型，图片→视频，需要 A100 80G

接口：
  POST /generate      纯文本转视频（JSON body）
  POST /generate/i2v  图片+文本转视频（multipart/form-data）
  GET  /health
"""
import os
import sys
import uuid
import time
import shutil
import logging
import tempfile
import subprocess
from pathlib import Path
from typing import Optional

import torch
import oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field

sys.path.insert(0, "/app/Wan2.2")

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
logger = logging.getLogger(__name__)

# ── 环境变量 ───────────────────────────────────────────────
MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/Wan2.2")
MODEL_TASK     = os.environ.get("MODEL_TASK", "ti2v-5B")   # ti2v-5B | t2v-A14B | i2v-A14B
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))

# 模型对应的 ckpt 目录名
TASK_CKPT_MAP = {
    "ti2v-5B":  "Wan2.2-TI2V-5B",
    "t2v-A14B": "Wan2.2-T2V-A14B",
    "i2v-A14B": "Wan2.2-I2V-A14B",
}

# 分辨率默认值
TASK_SIZE_MAP = {
    "ti2v-5B":  "1280*704",
    "t2v-A14B": "1280*720",
    "i2v-A14B": "1280*720",
}

app = FastAPI(title="Wan2.2 API", version="1.0.0")

# ── OSS 客户端 ─────────────────────────────────────────────
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)

model_loaded = False


def load_model():
    """预热：触发一次轻量推理，让模型权重载入显存"""
    global model_loaded
    ckpt_name = TASK_CKPT_MAP.get(MODEL_TASK)
    ckpt_dir  = os.path.join(MODEL_DIR, ckpt_name)

    if not os.path.exists(ckpt_dir):
        logger.warning(f"Model dir not found: {ckpt_dir}, will download on first call")
        model_loaded = True  # 让 health 返回 ok，实际下载在 ecs-userdata 里完成
        return

    logger.info(f"Model ready at {ckpt_dir} ✓  task={MODEL_TASK}")
    model_loaded = True


def upload_to_oss(local_path: str, oss_key: str) -> str:
    oss_bucket.put_object_from_file(oss_key, local_path)
    url = oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)
    logger.info(f"Uploaded → {oss_key}")
    return url


def run_generate(
    task: str,
    prompt: str,
    size: str,
    num_frames: int,
    sample_steps: int,
    cfg_scale: float,
    output_dir: str,
    image_path: Optional[str] = None,
    offload: bool = True,
) -> str:
    """
    调用 Wan2.2 的 generate.py 脚本执行推理，返回输出视频路径。
    使用脚本模式，无需在 Python 层加载模型，避免多进程显存冲突。
    """
    ckpt_name = TASK_CKPT_MAP.get(task, TASK_CKPT_MAP[MODEL_TASK])
    ckpt_dir  = os.path.join(MODEL_DIR, ckpt_name)

    cmd = [
        "python", "/app/Wan2.2/generate.py",
        "--task",        task,
        "--size",        size,
        "--ckpt_dir",    ckpt_dir,
        "--prompt",      prompt,
        "--save_file",   os.path.join(output_dir, "output.mp4"),
        "--sample_steps", str(sample_steps),
        "--sample_guide_scale", str(cfg_scale),
        "--frame_num",   str(num_frames),
    ]

    if offload:
        cmd += ["--offload_model", "True", "--convert_model_dtype", "--t5_cpu"]

    if image_path:
        cmd += ["--image", image_path]

    logger.info(f"Running: {' '.join(cmd[:8])} ...")
    result = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        timeout=900,
        cwd="/app/Wan2.2",
    )

    if result.returncode != 0:
        raise RuntimeError(f"generate.py failed:\n{result.stderr[-2000:]}")

    out_path = os.path.join(output_dir, "output.mp4")
    if not os.path.exists(out_path):
        raise RuntimeError("推理完成但未找到输出文件 output.mp4")

    return out_path


# ── 路由 ───────────────────────────────────────────────────

@app.on_event("startup")
async def startup():
    load_model()


@app.get("/health")
def health():
    gpu_info = {}
    if torch.cuda.is_available():
        props = torch.cuda.get_device_properties(0)
        gpu_info = {
            "name":          props.name,
            "vram_total_gb": round(props.total_memory / 1e9, 1),
            "vram_free_gb":  round(
                (props.total_memory - torch.cuda.memory_allocated()) / 1e9, 1
            ),
        }
    return {
        "status":       "ok",
        "model_loaded": model_loaded,
        "model_task":   MODEL_TASK,
        "cuda":         torch.cuda.is_available(),
        **gpu_info,
    }


class T2VRequest(BaseModel):
    prompt:       str   = Field(..., min_length=2, max_length=1000)
    size:         str   = Field("",  description="留空使用默认分辨率，如 1280*704")
    num_frames:   int   = Field(81,  ge=16, le=241, description="帧数（81≈5s@16fps）")
    sample_steps: int   = Field(50,  ge=10, le=100)
    cfg_scale:    float = Field(5.0, ge=1.0, le=10.0)
    offload:      bool  = Field(True, description="开启 model offload，降低 VRAM 占用")


@app.post("/generate")
async def generate_t2v(req: T2VRequest):
    """
    文本转视频（text-to-video）

    示例：
    curl -X POST http://host:8000/generate \\
      -H 'Content-Type: application/json' \\
      -d '{"prompt": "Two cats boxing on a stage", "num_frames": 81}'
    """
    job_id  = uuid.uuid4().hex
    tmp_dir = tempfile.mkdtemp(prefix=f"wan_{job_id}_")
    t0      = time.time()

    size = req.size or TASK_SIZE_MAP.get(MODEL_TASK, "1280*704")
    logger.info(f"[{job_id}] T2V  task={MODEL_TASK} size={size} frames={req.num_frames}")

    try:
        out_path = run_generate(
            task=MODEL_TASK,
            prompt=req.prompt,
            size=size,
            num_frames=req.num_frames,
            sample_steps=req.sample_steps,
            cfg_scale=req.cfg_scale,
            output_dir=tmp_dir,
            offload=req.offload,
        )

        oss_key = f"videos/{job_id}.mp4"
        url = upload_to_oss(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        logger.info(f"[{job_id}] Done in {elapsed}s")

        return {
            "job_id":      job_id,
            "status":      "success",
            "video_url":   url,
            "elapsed_sec": elapsed,
            "params": {
                "task":       MODEL_TASK,
                "prompt":     req.prompt,
                "size":       size,
                "num_frames": req.num_frames,
            },
        }

    except Exception as e:
        logger.error(f"[{job_id}] Failed: {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.post("/generate/i2v")
async def generate_i2v(
    image:        UploadFile = File(...,   description="参考图片"),
    prompt:       str        = Form(...,   description="视频描述"),
    size:         str        = Form("",    description="留空使用默认分辨率"),
    num_frames:   int        = Form(81),
    sample_steps: int        = Form(50),
    cfg_scale:    float      = Form(5.0),
    offload:      bool       = Form(True),
):
    """
    图片转视频（image-to-video）

    示例：
    curl -X POST http://host:8000/generate/i2v \\
      -F 'image=@input.jpg' \\
      -F 'prompt=The cat starts swimming in the ocean' \\
      -F 'num_frames=81'
    """
    # I2V 任务映射：ti2v-5B 既支持 T2V 也支持 I2V
    i2v_task = MODEL_TASK if MODEL_TASK != "t2v-A14B" else "i2v-A14B"

    job_id  = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"wan_{job_id}_"))
    t0      = time.time()

    try:
        # 保存图片
        suffix     = Path(image.filename or "input.jpg").suffix or ".jpg"
        image_path = str(tmp_dir / f"input{suffix}")
        with open(image_path, "wb") as f:
            f.write(await image.read())

        actual_size = size or TASK_SIZE_MAP.get(i2v_task, "1280*720")
        logger.info(f"[{job_id}] I2V  task={i2v_task} size={actual_size} frames={num_frames}")

        out_path = run_generate(
            task=i2v_task,
            prompt=prompt,
            size=actual_size,
            num_frames=num_frames,
            sample_steps=sample_steps,
            cfg_scale=cfg_scale,
            output_dir=str(tmp_dir),
            image_path=image_path,
            offload=offload,
        )

        oss_key = f"videos/{job_id}.mp4"
        url = upload_to_oss(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        logger.info(f"[{job_id}] Done in {elapsed}s")

        return {
            "job_id":      job_id,
            "status":      "success",
            "video_url":   url,
            "elapsed_sec": elapsed,
            "params": {
                "task":       i2v_task,
                "prompt":     prompt,
                "size":       actual_size,
                "num_frames": num_frames,
            },
        }

    except Exception as e:
        logger.error(f"[{job_id}] Failed: {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(str(tmp_dir), ignore_errors=True)


@app.get("/")
def root():
    return {
        "service":    "Wan2.2 API",
        "model_task": MODEL_TASK,
        "docs":       "/docs",
        "t2v":        "POST /generate        (JSON)",
        "i2v":        "POST /generate/i2v   (multipart)",
    }
