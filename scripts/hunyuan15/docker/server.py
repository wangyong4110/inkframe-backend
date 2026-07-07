"""
HunyuanVideo 1.5 推理 API
8.3B 参数，14GB VRAM（offload + tiling），T4 可跑
支持 T2V（文本转视频）和 I2V（图片转视频）
"""
import os, uuid, time, shutil, logging, tempfile, io
from pathlib import Path
from typing import Optional

import torch, oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field
from diffusers import HunyuanVideo15Pipeline
from diffusers.utils import export_to_video

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger(__name__)
app = FastAPI(title="HunyuanVideo-1.5 API")

MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/HunyuanVideo-1.5")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))

pipe         = None
model_loaded = False
oss_auth     = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket   = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)


def load_model():
    global pipe, model_loaded
    logger.info(f"Loading HunyuanVideo-1.5 from {MODEL_DIR} ...")
    t0 = time.time()
    pipe = HunyuanVideo15Pipeline.from_pretrained(
        MODEL_DIR,
        torch_dtype=torch.bfloat16,
    )
    pipe.enable_model_cpu_offload()   # 显著降低 VRAM，T4 16GB 可运行
    pipe.vae.enable_tiling()          # VAE 分块解码，进一步节省显存
    model_loaded = True
    logger.info(f"Model loaded in {round(time.time()-t0,1)}s ✓")


def upload_video(local_path: str, oss_key: str) -> str:
    oss_bucket.put_object_from_file(oss_key, local_path)
    return oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)


@app.on_event("startup")
async def startup():
    load_model()


@app.get("/health")
def health():
    vram = None
    if torch.cuda.is_available():
        props = torch.cuda.get_device_properties(0)
        vram = round((props.total_memory - torch.cuda.memory_allocated()) / 1e9, 1)
    return {"status": "ok", "model_loaded": model_loaded,
            "model": "HunyuanVideo-1.5", "vram_free_gb": vram}


class T2VRequest(BaseModel):
    prompt:       str   = Field(..., min_length=2, max_length=1000)
    width:        int   = Field(848,  ge=256, le=1280, multiple_of=16)
    height:       int   = Field(480,  ge=256, le=720,  multiple_of=16)
    num_frames:   int   = Field(61,   ge=9,   le=121,  description="建议 61（4s@15fps）或 121（8s）")
    num_steps:    int   = Field(30,   ge=8,   le=50)
    guidance:     float = Field(6.0,  ge=1.0, le=10.0)
    fps:          int   = Field(15)
    seed:         int   = Field(-1)


@app.post("/generate")
async def generate_t2v(req: T2VRequest):
    """文本转视频（text-to-video）"""
    job_id  = uuid.uuid4().hex
    tmp_dir = tempfile.mkdtemp(prefix=f"hv_{job_id}_")
    t0      = time.time()
    logger.info(f"[{job_id}] T2V prompt={req.prompt[:60]!r}")

    try:
        generator = torch.Generator("cpu").manual_seed(req.seed) if req.seed >= 0 else None
        with torch.no_grad():
            output = pipe(
                prompt=req.prompt,
                height=req.height,
                width=req.width,
                num_frames=req.num_frames,
                num_inference_steps=req.num_steps,
                guidance_scale=req.guidance,
                generator=generator,
            )
        frames = output.frames[0]
        out_path = os.path.join(tmp_dir, "output.mp4")
        export_to_video(frames, out_path, fps=req.fps)

        oss_key = f"videos/{job_id}.mp4"
        url = upload_video(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        logger.info(f"[{job_id}] Done in {elapsed}s")
        return {"job_id": job_id, "status": "success", "video_url": url,
                "elapsed_sec": elapsed,
                "params": {"prompt": req.prompt, "width": req.width,
                           "height": req.height, "num_frames": req.num_frames}}
    except Exception as e:
        logger.error(f"[{job_id}] {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.post("/generate/i2v")
async def generate_i2v(
    image:      UploadFile = File(...),
    prompt:     str        = Form(...),
    width:      int        = Form(848),
    height:     int        = Form(480),
    num_frames: int        = Form(61),
    num_steps:  int        = Form(30),
    guidance:   float      = Form(6.0),
    fps:        int        = Form(15),
    seed:       int        = Form(-1),
):
    """图片转视频（image-to-video）"""
    from PIL import Image as PILImage
    job_id  = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"hv_{job_id}_"))
    t0      = time.time()

    try:
        suffix = Path(image.filename or "img.jpg").suffix or ".jpg"
        img_path = str(tmp_dir / f"input{suffix}")
        with open(img_path, "wb") as f:
            f.write(await image.read())
        pil_img = PILImage.open(img_path).convert("RGB").resize((width, height))

        generator = torch.Generator("cpu").manual_seed(seed) if seed >= 0 else None
        with torch.no_grad():
            output = pipe(
                prompt=prompt,
                image=pil_img,
                height=height, width=width,
                num_frames=num_frames,
                num_inference_steps=num_steps,
                guidance_scale=guidance,
                generator=generator,
            )
        out_path = str(tmp_dir / "output.mp4")
        export_to_video(output.frames[0], out_path, fps=fps)

        oss_key = f"videos/{job_id}.mp4"
        url = upload_video(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        return {"job_id": job_id, "status": "success", "video_url": url,
                "elapsed_sec": elapsed, "mode": "i2v"}
    except Exception as e:
        logger.error(f"[{job_id}] {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(str(tmp_dir), ignore_errors=True)
