#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import asyncio
import os
import secrets
import sys
import tempfile
import threading
import time
import uuid
import warnings
from concurrent.futures import ThreadPoolExecutor
from typing import Optional

# 抑制 OpenMP 运行时冲突（临时绕行方案，如在生产环境建议使用 conda install nomkl 彻底解决）
os.environ["KMP_DUPLICATE_LIB_OK"] = "TRUE"

# ── HF_ENDPOINT 白名单验证 ─────────────────────────────────────────────────────
_ALLOWED_HF_ENDPOINTS = frozenset({
    "https://huggingface.co",
    "https://hf-mirror.com",
})
_hf_ep = os.environ.get("HF_ENDPOINT", "")
if _hf_ep:
    if _hf_ep not in _ALLOWED_HF_ENDPOINTS:
        print(f"[WARN] HF_ENDPOINT '{_hf_ep}' 不在白名单中，已忽略，使用默认镜像站", file=sys.stderr)
        del os.environ["HF_ENDPOINT"]
        os.environ["HF_ENDPOINT"] = "https://hf-mirror.com"
else:
    os.environ["HF_ENDPOINT"] = "https://hf-mirror.com"

import torch
import soundfile as sf
import uvicorn
from fastapi import Depends, FastAPI, HTTPException, status
from fastapi.responses import FileResponse
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials
from pydantic import BaseModel, Field
from diffusers import AudioLDMPipeline

# 忽略 Hugging Face 的一些过期警告
warnings.filterwarnings("ignore", category=FutureWarning)

# ── API Key 认证 ───────────────────────────────────────────────────────────────
_API_KEY = os.environ.get("AUDIOLDM_API_KEY", "")
if not _API_KEY:
    print("[WARN] 环境变量 AUDIOLDM_API_KEY 未设置，API 处于未受保护状态！"
          "  建议设置: export AUDIOLDM_API_KEY=<your-secret>", file=sys.stderr)

_http_bearer = HTTPBearer(auto_error=False)

def _require_api_key(credentials: HTTPAuthorizationCredentials = Depends(_http_bearer)):
    """Bearer Token 验证。未设置 AUDIOLDM_API_KEY 时跳过（开发模式）。"""
    if not _API_KEY:
        return
    if credentials is None or not secrets.compare_digest(credentials.credentials, _API_KEY):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid or missing API key",
            headers={"WWW-Authenticate": "Bearer"},
        )

# ── FastAPI 初始化 ─────────────────────────────────────────────────────────────
app = FastAPI(
    title="AudioLDM API",
    description="文本生成音效服务",
    version="1.1.0",
)

# ── 模型加载 ───────────────────────────────────────────────────────────────────
MODEL_ID = "cvssp/audioldm-s-full-v2"
print("正在加载 AudioLDM 模型，首次运行会自动下载（约1.2GB），请稍候...")
# 优先加载本地缓存，缓存缺失时走 HF_ENDPOINT 镜像下载
# Intel Mac CPU 只能使用 float32，不能使用 float16
try:
    pipe = AudioLDMPipeline.from_pretrained(
        MODEL_ID, torch_dtype=torch.float32, local_files_only=True
    )
    print("已从本地缓存加载模型")
except (OSError, FileNotFoundError):
    print(f"本地缓存未找到，从镜像站下载: {os.environ.get('HF_ENDPOINT', 'huggingface.co')}")
    pipe = AudioLDMPipeline.from_pretrained(MODEL_ID, torch_dtype=torch.float32)
except Exception as exc:
    print(f"[ERROR] 模型加载失败: {exc}", file=sys.stderr)
    raise

device = "cuda" if torch.cuda.is_available() else "cpu"
pipe = pipe.to(device)
print(f"模型加载完成，运行设备: {device.upper()}")

# ── 并发控制 ───────────────────────────────────────────────────────────────────
_MAX_CONCURRENT = 2
_active_tasks = 0
_task_lock = threading.Lock()

# ── 线程池（CPU 密集型任务，避免阻塞事件循环） ───────────────────────────────
executor = ThreadPoolExecutor(max_workers=_MAX_CONCURRENT)

# ── 请求/响应模型 ──────────────────────────────────────────────────────────────
class GenerateRequest(BaseModel):
    prompt: str = Field(..., description="文本提示，例如: 'A cat is meowing'")
    duration: int = Field(default=5, description="音频时长（秒）", ge=1, le=10)
    guidance_scale: float = Field(default=2.5, description="文本引导强度", ge=0.5, le=5.0)
    num_inference_steps: int = Field(
        default=50, description="推理步数（越大质量越高，速度越慢）", ge=10, le=100
    )

class GenerateResponse(BaseModel):
    task_id: str
    status: str
    message: Optional[str] = None

class ResultResponse(BaseModel):
    status: str
    file_name: Optional[str] = None
    message: Optional[str] = None

# ── 任务存储（内存字典，含 created_at 用于 TTL 清理） ─────────────────────────
tasks: dict = {}
TASK_TTL_SECS = 24 * 3600  # 24 小时后自动清理

# ── 后台任务：定期清理过期任务和临时文件 ──────────────────────────────────────
async def _cleanup_loop():
    while True:
        await asyncio.sleep(3600)  # 每小时检查一次
        cutoff = time.time() - TASK_TTL_SECS
        expired = [
            tid for tid, t in list(tasks.items())
            if t.get("created_at", 0) < cutoff
        ]
        for tid in expired:
            info = tasks.pop(tid, {})
            fp = info.get("file_path", "")
            if fp and os.path.exists(fp):
                try:
                    os.unlink(fp)
                except OSError:
                    pass

@app.on_event("startup")
async def _startup():
    asyncio.create_task(_cleanup_loop())

# ── 后台生成函数（在线程池中执行，不阻塞事件循环） ───────────────────────────
def _run_generation(task_id: str, request: GenerateRequest) -> None:
    """线程池内执行，完成后递减并发计数。"""
    global _active_tasks
    try:
        _do_generate(task_id, request)
    finally:
        with _task_lock:
            _active_tasks -= 1
        if device == "cuda":
            torch.cuda.empty_cache()

def _do_generate(task_id: str, request: GenerateRequest) -> None:
    """实际调用模型生成音频，写入临时文件（仅所有者可读）。"""
    try:
        audio = pipe(
            request.prompt,
            num_inference_steps=request.num_inference_steps,
            guidance_scale=request.guidance_scale,
            audio_length_in_s=request.duration,
        ).audios[0]

        # 创建权限为 600 的安全临时文件（避免 /tmp 世界可读）
        fd, file_path = tempfile.mkstemp(suffix=".wav", prefix=f"audioldm_{task_id}_")
        try:
            os.close(fd)
            os.chmod(file_path, 0o600)
            sf.write(file_path, audio, samplerate=16000)
        except Exception:
            try:
                os.unlink(file_path)
            except OSError:
                pass
            raise

        file_name = os.path.basename(file_path)
        tasks[task_id].update({
            "status": "completed",
            "file_path": file_path,
            "file_name": file_name,
        })

    except (RuntimeError, ValueError, OSError) as exc:
        tasks[task_id].update({"status": "failed", "message": str(exc)})
    except Exception as exc:
        tasks[task_id].update({
            "status": "failed",
            "message": f"内部错误: {type(exc).__name__}: {exc}",
        })

# ── API 端点 ───────────────────────────────────────────────────────────────────
@app.post("/generate/", response_model=GenerateResponse,
          dependencies=[Depends(_require_api_key)])
async def generate_audio(request: GenerateRequest):
    """提交音频生成任务，返回 task_id。超过并发上限时返回 429。"""
    global _active_tasks
    with _task_lock:
        if _active_tasks >= _MAX_CONCURRENT:
            raise HTTPException(
                status_code=429,
                detail=f"服务繁忙（最多 {_MAX_CONCURRENT} 个并发任务），请稍后重试",
            )
        _active_tasks += 1

    task_id = str(uuid.uuid4())
    tasks[task_id] = {"status": "processing", "created_at": time.time()}

    loop = asyncio.get_running_loop()
    loop.run_in_executor(executor, _run_generation, task_id, request)

    return GenerateResponse(
        task_id=task_id,
        status="processing",
        message="任务已提交，请通过 GET /result/{task_id} 轮询状态，完成后用 GET /download/{task_id} 下载文件",
    )

@app.get("/result/{task_id}", response_model=ResultResponse,
         dependencies=[Depends(_require_api_key)])
async def get_result(task_id: str):
    """查询任务状态。"""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="任务不存在或已过期（超过24小时）")

    task = tasks[task_id]
    if task["status"] == "completed":
        return ResultResponse(
            status="completed",
            file_name=task["file_name"],
            message="生成成功，请通过 GET /download/{task_id} 下载文件",
        )
    if task["status"] == "failed":
        return ResultResponse(status="failed", message=task["message"])
    return ResultResponse(status="processing", message="任务处理中，请稍后重试")

@app.get("/download/{task_id}", dependencies=[Depends(_require_api_key)])
async def download_audio(task_id: str):
    """下载生成的音频文件。"""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="任务不存在或已过期（超过24小时）")

    task = tasks[task_id]
    if task["status"] != "completed":
        raise HTTPException(
            status_code=400,
            detail=f"任务状态: {task['status']}，尚未完成",
        )

    file_path = task["file_path"]
    if not os.path.exists(file_path):
        raise HTTPException(status_code=404, detail="文件已被清理，请重新生成")

    return FileResponse(
        file_path,
        media_type="audio/wav",
        filename=task["file_name"],
    )

@app.get("/health")
async def health_check():
    """健康检查（无需认证）。"""
    return {
        "status": "ok",
        "device": device,
        "model_loaded": True,
        "active_tasks": _active_tasks,
    }

# ── 启动（仅当直接运行此脚本时） ──────────────────────────────────────────────
if __name__ == "__main__":
    uvicorn.run(
        app,
        host="0.0.0.0",
        port=8000,
        reload=False,
        log_level="info",
        access_log=False,
    )
