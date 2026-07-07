"""
FLUX.1 [dev] 推理服务
最高质量开源 T2I，需要 A10 24G 或开启 CPU offload 在 16G 运行
"""
import os, uuid, time, logging, io
import torch, oss2
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from PIL import Image

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger(__name__)
app = FastAPI(title="FLUX.1-dev API")

MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/FLUX.1-dev")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "3600"))
CPU_OFFLOAD    = os.environ.get("CPU_OFFLOAD", "true").lower() == "true"

pipe       = None
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)
model_loaded = False

def load_model():
    global pipe, model_loaded
    from diffusers import FluxPipeline
    logger.info(f"Loading FLUX.1-dev from {MODEL_DIR} ...")
    pipe = FluxPipeline.from_pretrained(MODEL_DIR, torch_dtype=torch.bfloat16)
    if CPU_OFFLOAD:
        pipe.enable_model_cpu_offload()   # 16GB 可用，但速度略慢
    else:
        pipe.to("cuda")                   # A10 24G 全量加载，速度更快
    model_loaded = True
    logger.info("FLUX.1-dev loaded ✓")

class T2IRequest(BaseModel):
    prompt:      str   = Field(..., min_length=2, max_length=1000)
    width:       int   = Field(1024, ge=256, le=2048, multiple_of=64)
    height:      int   = Field(1024, ge=256, le=2048, multiple_of=64)
    num_steps:   int   = Field(20, ge=4, le=50)
    guidance:    float = Field(3.5, ge=1.0, le=10.0)
    seed:        int   = Field(-1)

@app.on_event("startup")
async def startup(): load_model()

@app.get("/health")
def health():
    return {"status": "ok", "model_loaded": model_loaded,
            "cuda": torch.cuda.is_available(), "model": "FLUX.1-dev"}

@app.post("/generate")
async def generate(req: T2IRequest):
    job_id = uuid.uuid4().hex
    t0 = time.time()
    try:
        generator = torch.Generator("cuda").manual_seed(req.seed) if req.seed >= 0 else None
        with torch.no_grad():
            result = pipe(
                prompt=req.prompt,
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
