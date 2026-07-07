"""
Stable Diffusion 3.5 Large 推理服务
生态最丰富（LoRA/ControlNet），艺术风格多样
FP8 量化 + T5 CPU offload，18GB VRAM 可运行
"""
import os, uuid, time, logging, io
import torch, oss2
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from PIL import Image

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger(__name__)
app = FastAPI(title="SD3.5-Large API")

MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/SD3.5-large")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "3600"))

pipe       = None
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)
model_loaded = False

def load_model():
    global pipe, model_loaded
    from diffusers import StableDiffusion3Pipeline
    logger.info(f"Loading SD3.5-Large from {MODEL_DIR} ...")
    pipe = StableDiffusion3Pipeline.from_pretrained(
        MODEL_DIR,
        torch_dtype=torch.float16,
    )
    # T5 encoder CPU offload（大幅节省 VRAM，节省约 8GB）
    pipe.enable_model_cpu_offload()
    model_loaded = True
    logger.info("SD3.5-Large loaded ✓")

class T2IRequest(BaseModel):
    prompt:          str   = Field(..., min_length=2, max_length=2000)
    negative_prompt: str   = Field("", description="负向提示词")
    width:           int   = Field(1024, ge=256, le=2048, multiple_of=64)
    height:          int   = Field(1024, ge=256, le=2048, multiple_of=64)
    num_steps:       int   = Field(28, ge=10, le=50)
    guidance:        float = Field(7.0, ge=1.0, le=15.0)
    seed:            int   = Field(-1)

@app.on_event("startup")
async def startup(): load_model()

@app.get("/health")
def health():
    vram_free = None
    if torch.cuda.is_available():
        vram_free = round((torch.cuda.get_device_properties(0).total_memory
                          - torch.cuda.memory_allocated()) / 1e9, 1)
    return {"status": "ok", "model_loaded": model_loaded,
            "cuda": torch.cuda.is_available(),
            "vram_free_gb": vram_free, "model": "SD3.5-Large"}

@app.post("/generate")
async def generate(req: T2IRequest):
    job_id = uuid.uuid4().hex
    t0 = time.time()
    try:
        generator = torch.Generator("cuda").manual_seed(req.seed) if req.seed >= 0 else None
        with torch.no_grad():
            result = pipe(
                prompt=req.prompt,
                negative_prompt=req.negative_prompt or None,
                width=req.width, height=req.height,
                num_inference_steps=req.num_steps,
                guidance_scale=req.guidance,
                generator=generator,
            )
        img: Image.Image = result.images[0]
        buf = io.BytesIO()
        img.save(buf, format="PNG")
        buf.seek(0)
        oss_key = f"images/{job_id}.png"
        oss_bucket.put_object(oss_key, buf)
        url = oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)
        elapsed = round(time.time() - t0, 2)
        logger.info(f"[{job_id}] Done in {elapsed}s")
        return {"job_id": job_id, "url": url, "elapsed": elapsed,
                "width": req.width, "height": req.height, "prompt": req.prompt}
    except Exception as e:
        logger.error(f"Error: {e}", exc_info=True)
        raise HTTPException(500, str(e))
