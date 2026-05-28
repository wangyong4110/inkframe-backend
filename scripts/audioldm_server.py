#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import asyncio
import os
import uuid
import warnings
from concurrent.futures import ThreadPoolExecutor
from typing import Optional

# 抑制 OpenMP 运行时冲突（临时绕行方案，如在生产环境建议使用 conda install nomkl 彻底解决）
os.environ["KMP_DUPLICATE_LIB_OK"] = "TRUE"

import torch
import soundfile as sf
import uvicorn
from fastapi import FastAPI, HTTPException
from fastapi.responses import FileResponse
from pydantic import BaseModel, Field
from diffusers import AudioLDMPipeline

# 忽略 Hugging Face 的一些过期警告
warnings.filterwarnings("ignore", category=FutureWarning)

# ==================== 初始化 FastAPI ====================
app = FastAPI(
    title="AudioLDM API",
    description="文本生成音效服务 (CPU 版本)",
    version="1.0.0"
)

# ==================== 加载模型 ====================
print("正在加载 AudioLDM 模型，首次运行会自动下载（约1.2GB），请稍候...")
# Intel Mac CPU 只能使用 float32，不能使用 float16
pipe = AudioLDMPipeline.from_pretrained("cvssp/audioldm-s-full-v2", torch_dtype=torch.float32)
device = "cuda" if torch.cuda.is_available() else "cpu"
pipe = pipe.to(device)
print(f"模型加载完成，运行设备: {device.upper()}")

# ==================== 线程池（CPU 密集型任务，避免阻塞事件循环） ====================
executor = ThreadPoolExecutor(max_workers=2)

# ==================== 请求/响应模型定义 ====================
class GenerateRequest(BaseModel):
    prompt: str = Field(..., description="文本提示，例如: 'A cat is meowing'")
    duration: int = Field(default=5, description="音频时长（秒）", ge=1, le=10)
    guidance_scale: float = Field(default=2.5, description="文本引导强度", ge=0.5, le=5.0)
    num_inference_steps: int = Field(default=50, description="推理步数（越大质量越高，速度越慢）", ge=10, le=100)

class GenerateResponse(BaseModel):
    task_id: str
    status: str
    message: Optional[str] = None

class ResultResponse(BaseModel):
    status: str
    file_name: Optional[str] = None
    message: Optional[str] = None

# ==================== 任务存储（演示用，生产环境请使用 Redis 或数据库） ====================
tasks = {}

# ==================== 后台任务函数（在线程池中执行，不阻塞事件循环） ====================
def generate_audio_task(task_id: str, request: GenerateRequest):
    try:
        audio = pipe(
            request.prompt,
            num_inference_steps=request.num_inference_steps,
            guidance_scale=request.guidance_scale,
            audio_length_in_s=request.duration
        ).audios[0]

        file_name = f"{task_id}.wav"
        file_path = os.path.join("/tmp", file_name)
        sf.write(file_path, audio, samplerate=16000)

        tasks[task_id] = {
            "status": "completed",
            "file_path": file_path,
            "file_name": file_name,
        }
    except Exception as e:
        tasks[task_id] = {
            "status": "failed",
            "message": str(e),
        }

# ==================== API 端点 ====================
@app.post("/generate/", response_model=GenerateResponse)
async def generate_audio(request: GenerateRequest):
    """提交音频生成任务，返回 task_id"""
    task_id = str(uuid.uuid4())
    tasks[task_id] = {"status": "processing"}

    loop = asyncio.get_event_loop()
    loop.run_in_executor(executor, generate_audio_task, task_id, request)

    return GenerateResponse(
        task_id=task_id,
        status="processing",
        message="任务已提交，请通过 GET /result/{task_id} 轮询状态，完成后用 GET /download/{task_id} 下载文件"
    )

@app.get("/result/{task_id}", response_model=ResultResponse)
async def get_result(task_id: str):
    """查询任务状态"""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="任务不存在")

    task = tasks[task_id]
    if task["status"] == "completed":
        return ResultResponse(
            status="completed",
            file_name=task["file_name"],
            message="生成成功，请通过 GET /download/{task_id} 下载文件"
        )
    elif task["status"] == "failed":
        return ResultResponse(status="failed", message=task["message"])
    else:
        return ResultResponse(status="processing", message="任务处理中，请稍后重试")

@app.get("/download/{task_id}")
async def download_audio(task_id: str):
    """下载生成的音频文件"""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="任务不存在")

    task = tasks[task_id]
    if task["status"] != "completed":
        raise HTTPException(status_code=400, detail=f"任务状态: {task['status']}，尚未完成")

    file_path = task["file_path"]
    if not os.path.exists(file_path):
        raise HTTPException(status_code=404, detail="文件已被清理，请重新生成")

    return FileResponse(file_path, media_type="audio/wav", filename=task["file_name"])

@app.get("/health")
async def health_check():
    """健康检查"""
    return {
        "status": "ok",
        "device": device,
        "model_loaded": True,
    }

# ==================== 启动服务器（仅当直接运行此脚本时） ====================
if __name__ == "__main__":
    uvicorn.run(
        "audioldm_server:app",
        host="0.0.0.0",
        port=8000,
        reload=False,
        log_level="info"
    )
