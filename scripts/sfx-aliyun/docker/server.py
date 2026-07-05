import os, uuid, time, logging
import soundfile as sf
import torch
import oss2
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = FastAPI(title="SFX API", version="1.0")

# ── 环境变量 ───────────────────────────────────────────────
MODEL_PATH     = os.environ.get("MODEL_PATH", "/data/models/TangoFlux")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]

# ── 启动时加载模型（常驻显存）──────────────────────────────
logger.info(f"Loading TangoFlux from {MODEL_PATH} ...")
from tangoflux import TangoFlux
model = TangoFlux.from_pretrained(MODEL_PATH)
model = model.to("cuda" if torch.cuda.is_available() else "cpu")
model.eval()
logger.info("Model loaded ✓")

# ── OSS 客户端 ─────────────────────────────────────────────
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)

# ── 请求模型 ───────────────────────────────────────────────
class SFXRequest(BaseModel):
    prompt:    str   = Field(..., min_length=3, max_length=500)
    duration:  float = Field(10.0, ge=1, le=30)
    steps:     int   = Field(50,   ge=20, le=100)
    cfg_scale: float = Field(4.5,  ge=1,  le=10)

# ── 推理接口 ───────────────────────────────────────────────
@app.post("/generate")
async def generate(req: SFXRequest):
    t0 = time.time()
    file_id = uuid.uuid4().hex

    try:
        logger.info(f"Generating: '{req.prompt}' duration={req.duration}s")

        with torch.no_grad():
            audio = model.generate(
                prompt=req.prompt,
                duration=req.duration,
                num_inference_steps=req.steps,
                guidance_scale=req.cfg_scale,
            )

        local_path = f"/tmp/{file_id}.wav"
        sf.write(local_path, audio.cpu().numpy(), 44100)

        oss_key = f"sfx/{file_id}.wav"
        oss_bucket.put_object_from_file(oss_key, local_path)
        url = oss_bucket.sign_url("GET", oss_key, 3600)

        elapsed = round(time.time() - t0, 2)
        logger.info(f"Done in {elapsed}s → {oss_key}")
        return {
            "url":      url,
            "oss_key":  oss_key,
            "duration": req.duration,
            "elapsed":  elapsed,
            "prompt":   req.prompt,
        }

    except Exception as e:
        logger.error(f"Generation failed: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/health")
def health():
    return {
        "status": "ok",
        "cuda":   torch.cuda.is_available(),
        "device": torch.cuda.get_device_name(0) if torch.cuda.is_available() else "cpu",
    }
