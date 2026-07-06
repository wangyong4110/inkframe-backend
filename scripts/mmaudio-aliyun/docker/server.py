"""
MMAudio 推理 API 服务
支持：
  - 文本转音频（text-to-audio）：POST /generate，JSON body
  - 视频转音频（video-to-audio）：POST /generate/video，multipart 上传视频文件
输出：.flac 音频，上传 OSS 后返回下载链接
"""
import os
import sys
import uuid
import time
import shutil
import logging
import tempfile
from pathlib import Path

import torch
import oss2
from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from pydantic import BaseModel, Field

# 将 MMAudio 加入 Python 路径
sys.path.insert(0, "/app/MMAudio")

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
logger = logging.getLogger(__name__)

# ── 环境变量 ───────────────────────────────────────────────
MODEL_DIR      = os.environ.get("MODEL_DIR", "/data/models/MMAudio")
OSS_ACCESS_KEY = os.environ["OSS_ACCESS_KEY"]
OSS_SECRET_KEY = os.environ["OSS_SECRET_KEY"]
OSS_ENDPOINT   = os.environ["OSS_ENDPOINT"]
OSS_BUCKET     = os.environ["OSS_BUCKET"]
OSS_URL_EXPIRE = int(os.environ.get("OSS_URL_EXPIRE", "3600"))

app = FastAPI(title="MMAudio API", version="1.0.0")

# ── OSS 客户端 ─────────────────────────────────────────────
oss_auth   = oss2.Auth(OSS_ACCESS_KEY, OSS_SECRET_KEY)
oss_bucket = oss2.Bucket(oss_auth, OSS_ENDPOINT, OSS_BUCKET)

# ── 全局模型（启动时加载）─────────────────────────────────
net        = None
feature_utils = None
seq_cfg    = None

def load_model():
    global net, feature_utils, seq_cfg

    logger.info(f"Loading MMAudio models from {MODEL_DIR} ...")
    t0 = time.time()

    # 将模型目录告知 MMAudio 的下载工具
    # MMAudio 用环境变量 MMAUDIO_HOME 指定模型存放路径
    os.environ["MMAUDIO_HOME"] = MODEL_DIR

    from mmaudio.model.flow_matching import FlowMatching
    from mmaudio.model.networks import MMAudio, get_my_mmaudio
    from mmaudio.model.utils.features_utils import FeaturesUtils
    from mmaudio.eval_utils import (
        ModelConfig,
        all_model_cfg,
        load_video,
        make_video,
        setup_eval_logging,
    )

    device = "cuda" if torch.cuda.is_available() else "cpu"
    dtype  = torch.bfloat16 if torch.cuda.is_available() else torch.float32

    # 使用 large_44k_v2（质量最高，6GB 显存）
    model_cfg: ModelConfig = all_model_cfg["large_44k_v2"]
    model_cfg.download_if_needed()   # 如模型已在 MODEL_DIR，跳过下载

    net = get_my_mmaudio(model_cfg.model_name).to(device, dtype).eval()
    net.load_weights(torch.load(model_cfg.model_path, map_location=device, weights_only=True))

    feature_utils = FeaturesUtils(
        tod_vae_ckpt=model_cfg.vae_path,
        synchformer_ckpt=model_cfg.synchformer_ckpt,
        enable_conditions=True,
        mode=model_cfg.mode,
        bigvgan_vocoder_ckpt=model_cfg.bigvgan_16k_path,
        need_vae_encoder=False,
    ).to(device, dtype).eval()

    seq_cfg = model_cfg.seq_cfg

    elapsed = round(time.time() - t0, 1)
    logger.info(f"MMAudio loaded in {elapsed}s ✓  device={device}")


# ── 请求模型 ───────────────────────────────────────────────
class TextRequest(BaseModel):
    prompt:          str   = Field(..., min_length=2, max_length=500)
    negative_prompt: str   = Field("music", description="不希望出现的声音")
    duration:        float = Field(8.0, ge=1.0, le=30.0)
    num_steps:       int   = Field(25,  ge=10, le=100)
    cfg_strength:    float = Field(4.5, ge=1.0, le=10.0)
    seed:            int   = Field(-1,  description="-1 表示随机")


def _run_inference(
    prompt: str,
    negative_prompt: str,
    duration: float,
    num_steps: int,
    cfg_strength: float,
    seed: int,
    video_path: str | None,
) -> str:
    """执行推理，返回输出 .flac 文件路径"""
    import torchaudio
    from mmaudio.eval_utils import load_video, make_video
    from mmaudio.model.flow_matching import FlowMatching

    device = "cuda" if torch.cuda.is_available() else "cpu"
    dtype  = torch.bfloat16 if torch.cuda.is_available() else torch.float32

    # 随机种子
    rng = torch.Generator(device=device)
    if seed >= 0:
        rng.manual_seed(seed)
    else:
        rng.seed()

    fm = FlowMatching(min_sigma=0, inference_mode="euler", num_steps=num_steps)

    with torch.no_grad():
        clip_frames = sync_frames = None

        if video_path:
            clip_frames, sync_frames, duration = load_video(video_path, duration)
            clip_frames = clip_frames.unsqueeze(0).to(device, dtype)
            sync_frames = sync_frames.unsqueeze(0).to(device, dtype)

        seq_cfg.duration = duration
        net.update_seq_lengths(seq_cfg.latent_seq_len, seq_cfg.clip_seq_len, seq_cfg.sync_seq_len)

        audios = net(
            clip_frames,
            sync_frames,
            [prompt],
            negative_text=[negative_prompt],
            feature_utils=feature_utils,
            seq_cfg=seq_cfg,
            fm=fm,
            cfg_strength=cfg_strength,
            rng=rng,
        )

    audio = audios.float().cpu()[0]

    out_path = f"/tmp/{uuid.uuid4().hex}.flac"
    torchaudio.save(out_path, audio, 44100)
    return out_path


def _upload_and_cleanup(local_path: str, oss_key: str) -> str:
    """上传到 OSS，返回签名 URL，删除本地临时文件"""
    try:
        oss_bucket.put_object_from_file(oss_key, local_path)
        url = oss_bucket.sign_url("GET", oss_key, OSS_URL_EXPIRE)
        logger.info(f"Uploaded → {oss_key}")
        return url
    finally:
        Path(local_path).unlink(missing_ok=True)


# ── 路由 ───────────────────────────────────────────────────

@app.on_event("startup")
async def startup():
    load_model()


@app.get("/health")
def health():
    return {
        "status":       "ok",
        "model_loaded": net is not None,
        "cuda":         torch.cuda.is_available(),
        "device":       torch.cuda.get_device_name(0) if torch.cuda.is_available() else "cpu",
        "vram_free_gb": round(
            (torch.cuda.get_device_properties(0).total_memory - torch.cuda.memory_allocated())
            / 1e9, 1
        ) if torch.cuda.is_available() else None,
    }


@app.post("/generate")
async def generate_text(req: TextRequest):
    """
    文本转音频（text-to-audio）

    示例：
    curl -X POST http://host:8000/generate \\
      -H 'Content-Type: application/json' \\
      -d '{"prompt": "heavy rain on a metal roof", "duration": 8}'
    """
    if net is None:
        raise HTTPException(status_code=503, detail="Model not loaded")

    t0 = time.time()
    logger.info(f"[T2A] prompt={req.prompt!r} duration={req.duration}s")

    try:
        out_path = _run_inference(
            prompt=req.prompt,
            negative_prompt=req.negative_prompt,
            duration=req.duration,
            num_steps=req.num_steps,
            cfg_strength=req.cfg_strength,
            seed=req.seed,
            video_path=None,
        )
        oss_key = f"sfx/{uuid.uuid4().hex}.flac"
        url = _upload_and_cleanup(out_path, oss_key)
        elapsed = round(time.time() - t0, 2)
        logger.info(f"[T2A] Done in {elapsed}s")
        return {
            "url":      url,
            "oss_key":  oss_key,
            "elapsed":  elapsed,
            "mode":     "text-to-audio",
            "prompt":   req.prompt,
            "duration": req.duration,
        }
    except Exception as e:
        logger.error(f"[T2A] Failed: {e}", exc_info=True)
        raise HTTPException(status_code=500, detail=str(e))


@app.post("/generate/video")
async def generate_video(
    video: UploadFile = File(..., description="输入视频文件"),
    prompt:          str   = Form("",      description="音频描述（可为空，模型自动理解视频内容）"),
    negative_prompt: str   = Form("music", description="不希望出现的声音"),
    duration:        float = Form(-1.0,    description="-1 表示跟随视频时长"),
    num_steps:       int   = Form(25),
    cfg_strength:    float = Form(4.5),
    seed:            int   = Form(-1),
):
    """
    视频转音频（video-to-audio）
    上传视频文件，生成与视频画面同步的音效

    示例：
    curl -X POST http://host:8000/generate/video \\
      -F "video=@input.mp4" \\
      -F 'prompt=footsteps on gravel' \\
      -F 'duration=-1'
    """
    if net is None:
        raise HTTPException(status_code=503, detail="Model not loaded")

    t0 = time.time()
    tmp_dir = Path(tempfile.mkdtemp())

    try:
        # 保存上传的视频
        suffix = Path(video.filename or "video.mp4").suffix or ".mp4"
        video_path = str(tmp_dir / f"input{suffix}")
        with open(video_path, "wb") as f:
            f.write(await video.read())

        logger.info(f"[V2A] video={video.filename} prompt={prompt!r} duration={duration}s")

        actual_duration = None if duration <= 0 else duration

        out_path = _run_inference(
            prompt=prompt,
            negative_prompt=negative_prompt,
            duration=actual_duration or 8.0,
            num_steps=num_steps,
            cfg_strength=cfg_strength,
            seed=seed,
            video_path=video_path,
        )

        oss_key = f"sfx/{uuid.uuid4().hex}.flac"
        url = _upload_and_cleanup(out_path, oss_key)
        elapsed = round(time.time() - t0, 2)
        logger.info(f"[V2A] Done in {elapsed}s")

        return {
            "url":      url,
            "oss_key":  oss_key,
            "elapsed":  elapsed,
            "mode":     "video-to-audio",
            "prompt":   prompt,
        }
    except Exception as e:
        logger.error(f"[V2A] Failed: {e}", exc_info=True)
        raise HTTPException(status_code=500, detail=str(e))
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


@app.get("/")
def root():
    return {
        "service":        "MMAudio API",
        "docs":           "/docs",
        "text_to_audio":  "POST /generate",
        "video_to_audio": "POST /generate/video",
    }
