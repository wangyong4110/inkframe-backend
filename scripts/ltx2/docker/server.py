"""
LTX-2 推理 API（基于 Lightricks/LTX-2）
22B 参数，速度最快，唯一支持原生音频的开源视频模型
官方最低 32GB VRAM，FP8 量化可在 16GB 运行（720p 降质）
支持 T2V 和 I2V，两阶段生成（base + upsampler）
"""
import os, uuid, time, shutil, logging, tempfile
from pathlib import Path

import torch, oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field
from diffusers import FlowMatchEulerDiscreteScheduler
from diffusers.utils import export_to_video

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger(__name__)
app = FastAPI(title="LTX-2 API")

MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/LTX-2")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))
# FAST_MODE=true 使用 distilled 模型（8步，更快）
FAST_MODE      = os.environ.get("FAST_MODE", "true").lower() == "true"

pipe         = None
pipe_up      = None
model_loaded = False
oss_auth     = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket   = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)


def load_model():
    global pipe, pipe_up, model_loaded
    from diffusers.pipelines.ltx2 import LTX2Pipeline, LTX2LatentUpsamplePipeline
    from diffusers.pipelines.ltx2.latent_upsampler import LTX2LatentUpsamplerModel

    model_id = os.path.join(MODEL_DIR, "LTX-2-Distilled" if FAST_MODE else "LTX-2")
    up_id    = os.path.join(MODEL_DIR, "ltxv-spatial-upscaler")

    logger.info(f"Loading LTX-2 ({'distilled' if FAST_MODE else 'dev'}) from {MODEL_DIR} ...")
    t0 = time.time()

    pipe = LTX2Pipeline.from_pretrained(model_id, torch_dtype=torch.bfloat16)
    pipe.enable_sequential_cpu_offload()  # 适配低显存

    # 空间上采样器（可选，提升分辨率）
    if os.path.exists(up_id):
        up_model = LTX2LatentUpsamplerModel.from_pretrained(up_id, torch_dtype=torch.bfloat16)
        pipe_up  = LTX2LatentUpsamplePipeline(vae=pipe.vae, latent_upsampler=up_model)
        pipe_up.enable_sequential_cpu_offload()
        logger.info("Upsampler loaded ✓")

    model_loaded = True
    logger.info(f"LTX-2 loaded in {round(time.time()-t0,1)}s ✓")


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
            "model": "LTX-2", "fast_mode": FAST_MODE, "vram_free_gb": vram}


NEGATIVE_PROMPT = (
    "shaky, glitchy, low quality, worst quality, deformed, distorted, "
    "disfigured, motion smear, motion artifacts, fused fingers, bad anatomy, "
    "weird hand, ugly, transition, static"
)


class T2VRequest(BaseModel):
    prompt:          str   = Field(..., min_length=2, max_length=2000)
    negative_prompt: str   = Field(NEGATIVE_PROMPT)
    width:           int   = Field(768,  ge=256, le=1920, multiple_of=32)
    height:          int   = Field(512,  ge=256, le=1080, multiple_of=32)
    num_frames:      int   = Field(121,  ge=9,   le=257,  description="必须为 8n+1")
    num_steps:       int   = Field(8 if True else 30, ge=4, le=50,
                                   description="distilled 建议 8，dev 建议 30")
    guidance:        float = Field(1.0,  ge=0.0, le=10.0,
                                   description="distilled 建议 1.0，dev 建议 3.0")
    fps:             int   = Field(24)
    seed:            int   = Field(-1)
    use_upsampler:   bool  = Field(True, description="是否使用两阶段上采样提升清晰度")


@app.post("/generate")
async def generate_t2v(req: T2VRequest):
    """文本转视频（text-to-video）"""
    # num_frames 强制为 8n+1
    nf = req.num_frames if (req.num_frames - 1) % 8 == 0 else ((req.num_frames // 8) * 8 + 1)
    job_id  = uuid.uuid4().hex
    tmp_dir = tempfile.mkdtemp(prefix=f"ltx_{job_id}_")
    t0      = time.time()
    logger.info(f"[{job_id}] T2V prompt={req.prompt[:60]!r} frames={nf}")

    try:
        generator = torch.Generator("cpu").manual_seed(req.seed) if req.seed >= 0 else None

        with torch.no_grad():
            output = pipe(
                prompt=req.prompt,
                negative_prompt=req.negative_prompt,
                width=req.width, height=req.height,
                num_frames=nf,
                num_inference_steps=req.num_steps,
                guidance_scale=req.guidance,
                generator=generator,
                output_type="latent" if (req.use_upsampler and pipe_up) else "pil",
            )

            # 两阶段上采样
            if req.use_upsampler and pipe_up and hasattr(output, "frames") is False:
                from diffusers.pipelines.ltx2.utils import STAGE_2_DISTILLED_SIGMA_VALUES
                output = pipe_up(
                    latents=output.latents,
                    prompt=req.prompt,
                    negative_prompt=req.negative_prompt,
                    sigmas=STAGE_2_DISTILLED_SIGMA_VALUES,
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
                           "height": req.height, "num_frames": nf}}
    except Exception as e:
        logger.error(f"[{job_id}] {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.post("/generate/i2v")
async def generate_i2v(
    image:      UploadFile = File(...),
    prompt:     str        = Form(...),
    width:      int        = Form(768),
    height:     int        = Form(512),
    num_frames: int        = Form(121),
    num_steps:  int        = Form(8),
    guidance:   float      = Form(1.0),
    fps:        int        = Form(24),
    seed:       int        = Form(-1),
):
    """图片转视频（image-to-video）"""
    from PIL import Image as PILImage
    job_id  = uuid.uuid4().hex
    tmp_dir = Path(tempfile.mkdtemp(prefix=f"ltx_{job_id}_"))
    t0      = time.time()

    try:
        img_bytes = await image.read()
        pil_img = PILImage.open(__import__("io").BytesIO(img_bytes)).convert("RGB")
        nf = num_frames if (num_frames - 1) % 8 == 0 else ((num_frames // 8) * 8 + 1)

        generator = torch.Generator("cpu").manual_seed(seed) if seed >= 0 else None
        with torch.no_grad():
            output = pipe(
                prompt=prompt,
                image=pil_img,
                width=width, height=height,
                num_frames=nf,
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
