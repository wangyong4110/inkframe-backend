"""
Open-Sora 2.0 推理 API
11B 参数，MIT 协议，唯一公开完整训练代码
24GB+ VRAM，支持 T2V / I2V / V2V
"""
import os, uuid, time, shutil, logging, tempfile, subprocess
from pathlib import Path

import torch, oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger(__name__)
app = FastAPI(title="Open-Sora-2 API")

MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/OpenSora2")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "7200"))

model_loaded = False
oss_auth     = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket   = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)


def load_model():
    global model_loaded
    # Open-Sora 通过 CLI 脚本推理，启动时检查模型文件是否就绪
    marker = os.path.join(MODEL_DIR, "model.safetensors")
    if not os.path.exists(marker):
        logger.warning(f"Model not found at {MODEL_DIR}, will fail on inference")
    else:
        logger.info(f"Open-Sora 2.0 model ready at {MODEL_DIR} ✓")
    model_loaded = True


def upload_video(local_path: str, oss_key: str) -> str:
    oss_bucket.put_object_from_file(oss_key, local_path)
    return oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)


@app.on_event("startup")
async def startup():
    load_model()


@app.get("/health")
def health():
    return {"status": "ok", "model_loaded": model_loaded, "model": "Open-Sora-2.0"}


class T2VRequest(BaseModel):
    prompt:        str   = Field(..., min_length=2, max_length=2000)
    resolution:    str   = Field("480p", description="240p / 480p / 720p")
    num_frames:    int   = Field(51,  ge=17, le=204, description="建议 51（约 3s@17fps）")
    num_steps:     int   = Field(30,  ge=10, le=100)
    guidance:      float = Field(7.0, ge=1.0, le=15.0)
    fps:           int   = Field(17)
    seed:          int   = Field(-1)


@app.post("/generate")
async def generate_t2v(req: T2VRequest):
    """文本转视频（text-to-video）"""
    job_id  = uuid.uuid4().hex
    tmp_dir = tempfile.mkdtemp(prefix=f"os2_{job_id}_")
    out_path = os.path.join(tmp_dir, "output.mp4")
    t0      = time.time()
    logger.info(f"[{job_id}] T2V prompt={req.prompt[:60]!r}")

    # 分辨率映射
    res_map = {"240p": "240x426", "480p": "480x854", "720p": "720x1280"}
    wh = res_map.get(req.resolution, "480x854")
    h, w = wh.split("x")

    cmd = [
        "python", "/app/Open-Sora/scripts/inference.py",
        "/app/Open-Sora/configs/opensora-v2-0/inference/t2v.py",
        "--ckpt-path", MODEL_DIR,
        "--prompt", req.prompt,
        "--num-frames", str(req.num_frames),
        "--height", h, "--width", w,
        "--num-sampling-steps", str(req.num_steps),
        "--guidance-scale", str(req.guidance),
        "--save-dir", tmp_dir,
    ]
    if req.seed >= 0:
        cmd += ["--seed", str(req.seed)]

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=900,
                                cwd="/app/Open-Sora")
        if result.returncode != 0:
            raise RuntimeError(f"Inference failed:\n{result.stderr[-2000:]}")

        # Open-Sora 输出文件名不固定，找第一个 mp4
        mp4_files = list(Path(tmp_dir).glob("*.mp4"))
        if not mp4_files:
            raise RuntimeError("No output mp4 found")
        out_path = str(mp4_files[0])

        oss_key = f"videos/{job_id}.mp4"
        url = upload_video(out_path, oss_key)
        elapsed = round(time.time() - t0, 1)
        logger.info(f"[{job_id}] Done in {elapsed}s")
        return {"job_id": job_id, "status": "success", "video_url": url,
                "elapsed_sec": elapsed,
                "params": {"prompt": req.prompt, "resolution": req.resolution,
                           "num_frames": req.num_frames}}
    except Exception as e:
        logger.error(f"[{job_id}] {e}", exc_info=True)
        raise HTTPException(500, str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)
