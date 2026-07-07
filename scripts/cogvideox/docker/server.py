"""
CogVideoX 1.5 推理 API（智谱 AI）
2B / 5B 参数，Apache 2.0，16GB VRAM 可运行
支持 T2V 和 I2V，生态完整
"""
import os, uuid, time, shutil, logging, tempfile
from pathlib import Path

import torch, oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field
from diffusers import CogVideoXPipeline, CogVideoXImageToVideoPipeline
from diffusers.utils import export_to_video

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger(__name__)
app = FastAPI(title="CogVideoX-1.5 API")

MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/CogVideoX-1.5")
MODEL_SIZE     = os.environ.get("MODEL_SIZE", "5b")   # 2b 或 5b
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))

pipe_t2v     = None
pipe_i2v     = None
model_loaded = False
oss_auth     = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket   = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)


def load_model():
    global pipe_t2v, pipe_i2v, model_loaded
    logger.info(f"Loading CogVideoX-1.5-{MODEL_SIZE} from {MODEL_DIR} ...")
    t0 = time.time()

    t2v_dir = os.path.join(MODEL_DIR, f"CogVideoX1.5-{MODEL_SIZE}")
    i2v_dir = os.path.join(MODEL_DIR, f"CogVideoX1.5-{MODEL_SIZE}-I2V")

    pipe_t2v = CogVideoXPipeline.from_pretrained(
        t2v_dir, torch_dtype=torch.bfloat16
    )
    pipe_t2v.enable_model_cpu_offload()
    pipe_t2v.vae.enable_tiling()
    pipe_t2v.vae.enable_slicing()

    if os.path.exists(i2v_dir):
        pipe_i2v = CogVideoXImageToVideoPipeline.from_pretrained(
            i2v_dir, torch_dtype=torch.bfloat16
        )
        pipe_i2v.enable_model_cpu_offload()
        pipe_i2v.vae.enable_tiling()
        pipe_i2v.vae.enable_slicing()
        logger.info("I2V pipeline loaded ✓")

    model_loaded = True
    logger.info(f"CogVideoX-1.5 loaded in {round(time.time()-t0,1)}s ✓")


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
            "model": f"CogVideoX-1.5-{MODEL_SIZE}", "vram_free_gb": vram,
            "i2v_available": pipe_i2v is not None}


class T2VRequest(BaseModel):
    prompt:          str   = Field(..., min_length=2, max_length=2000)
    negative_prompt: str   = Field("")
    width:           int   = Field(1360, ge=256, le=1360, multiple_of=8)
    height:          int   = Field(768,  ge=256, le=768,  multiple_of=8)
    num_frames:      int   = Field(49,  ge=9, le=81,
                                   description="建议 49（约 6s@8fps）")
    num_steps:       int   = Field(50, ge=10, le=100)
    guidance:        float = Field(6.0, ge=1.0, le=15.0)
    fps:             int   = Field(8)
    seed:            int   = Field(-1)


@app.post("/generate")
async def generate_t2v(req: T2VRequest):
    """文本转视频（text-to-video）"""
    job_id  = uuid.uuid4().hex
    tmp_dir = tempfile.mkdtemp(prefix=f"cgx_{job_id}_")
    t0      = time.time()
    logger.info(f"[{job_id}] T2V prompt={req.prompt[:60]!r}")

    try:
        generator = torch.Generator(device="cpu").manual_seed(req.seed) if req.seed >= 0 else None
        with torch.no_grad():
            output = pipe_t2v(
                prompt=req.prompt,
                negative_prompt=req.negative_prompt or None,
                width=req.width, height=req.height,
                num_frames=req.num_frames,
                num_inference_steps=req.num_steps,
                guidance_scale=req.guidance,
                generator=generator,
            )
        out_path = os.path.join(tmp_dir, "output.mp4")
        export_to_video(output.frames[0], out_path, fps=req.fps)

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
    width:      int        = Form(1360),
    height:     int        = Form(768),
    num_frames: int        = Form(49),
    num_steps:  int        = Form(50),
    guidance:   float      = Form(6.0),
    fps:        int        = Form(8),
    seed:       int        = Form(-1),
):
    """图片转视频（image-to-video）"""
    if pipe_i2v is None:
        raise HTTPException(503, "I2V model not loaded")
    from PIL import Image as PILImage
    job_id  = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"cgx_{job_id}_"))
    t0      = time.time()

    try:
        img_bytes = await image.read()
        pil_img = PILImage.open(__import__("io").BytesIO(img_bytes)).convert("RGB").resize((width, height))
        generator = torch.Generator(device="cpu").manual_seed(seed) if seed >= 0 else None

        with torch.no_grad():
            output = pipe_i2v(
                prompt=prompt,
                image=pil_img,
                width=width, height=height,
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
