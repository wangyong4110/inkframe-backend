"""
SkyReels-A2 函数计算触发器

架构：
  FC HTTP 触发器（接收请求）
    → 1. 启动 ECS GPU 实例
    → 2. 等待推理服务就绪
    → 3a. 同步模式：直接转发请求等待结果（适合短视频，< 5min）
    → 3b. 异步模式：提交任务后立即返回 job_id，轮询 /status 查结果

注意：视频生成耗时 2-10 分钟，FC 函数超时设为 600 秒。
如果需要更长时长，应改为异步模式（提交到队列，结果写 OSS 后回调）。
"""
import os
import json
import time
import uuid
import base64
import logging
import requests

from aliyunsdkcore.client import AcsClient
from aliyunsdkecs.request.v20140526 import (
    StartInstanceRequest,
    StopInstanceRequest,
    DescribeInstancesRequest,
)

logger = logging.getLogger()
logger.setLevel(logging.INFO)

# ── 环境变量（在 FC 控制台配置）─────────────────────────────
ACCESS_KEY     = os.environ["ALIBABA_ACCESS_KEY"]
ACCESS_SECRET  = os.environ["ALIBABA_ACCESS_SECRET"]
REGION_ID      = os.environ.get("REGION_ID", "cn-hangzhou")
INSTANCE_ID    = os.environ["ECS_INSTANCE_ID"]
ECS_PRIVATE_IP = os.environ["ECS_PRIVATE_IP"]
INFER_PORT     = os.environ.get("INFER_PORT", "8000")
INFER_BASE     = f"http://{ECS_PRIVATE_IP}:{INFER_PORT}"
TIMEOUT_START  = int(os.environ.get("TIMEOUT_START", "300"))   # 实例启动超时(秒)
TIMEOUT_INFER  = int(os.environ.get("TIMEOUT_INFER", "600"))   # 推理超时(秒)
AUTO_STOP      = os.environ.get("AUTO_STOP", "true").lower() == "true"

acs_client = AcsClient(ACCESS_KEY, ACCESS_SECRET, REGION_ID)


# ── ECS 控制 ──────────────────────────────────────────────

def get_instance_status() -> str:
    """查询 ECS 实例当前状态"""
    req = DescribeInstancesRequest.DescribeInstancesRequest()
    req.set_InstanceIds(json.dumps([INSTANCE_ID]))
    resp = json.loads(acs_client.do_action_with_exception(req))
    instances = resp.get("Instances", {}).get("Instance", [])
    if not instances:
        raise RuntimeError(f"Instance {INSTANCE_ID} not found")
    return instances[0]["Status"]  # Running / Stopped / Starting / ...


def start_instance():
    status = get_instance_status()
    if status == "Running":
        logger.info(f"Instance {INSTANCE_ID} already Running, skip start")
        return
    logger.info(f"Starting instance {INSTANCE_ID} (current: {status})")
    req = StartInstanceRequest.StartInstanceRequest()
    req.set_InstanceId(INSTANCE_ID)
    acs_client.do_action_with_exception(req)


def stop_instance():
    if not AUTO_STOP:
        logger.info("AUTO_STOP=false, skipping shutdown")
        return
    try:
        req = StopInstanceRequest.StopInstanceRequest()
        req.set_InstanceId(INSTANCE_ID)
        req.set_StoppedMode("KeepCharging")  # 停机不释放磁盘
        acs_client.do_action_with_exception(req)
        logger.info(f"Instance {INSTANCE_ID} stop requested")
    except Exception as e:
        logger.warning(f"Stop instance warning: {e}")


def wait_for_service(timeout: int = TIMEOUT_START) -> bool:
    """轮询 /health，等待推理服务就绪"""
    logger.info(f"Waiting for service at {INFER_BASE}/health (timeout={timeout}s)")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(f"{INFER_BASE}/health", timeout=5)
            if r.status_code == 200:
                data = r.json()
                if data.get("model_loaded"):
                    logger.info(f"Service ready ✓  GPU: {data.get('name')}  "
                                f"VRAM free: {data.get('vram_free_gb')}GB")
                    return True
                else:
                    logger.info("Service up but model still loading...")
        except requests.exceptions.RequestException:
            pass
        time.sleep(8)
    logger.error("Service startup timeout")
    return False


# ── 请求解析 ──────────────────────────────────────────────

def parse_event(event_bytes: bytes) -> dict:
    """解析 FC HTTP 触发器事件，兼容 base64 编码的 multipart body"""
    try:
        evt = json.loads(event_bytes)
    except Exception:
        return {}

    # FC HTTP 触发器：body 可能是 base64 编码
    body = evt.get("body", "")
    if evt.get("isBase64Encoded") and body:
        body = base64.b64decode(body)
        evt["body_raw"] = body
    elif isinstance(body, str):
        try:
            evt["body_parsed"] = json.loads(body)
        except Exception:
            pass
    return evt


def forward_multipart(evt: dict, timeout: int) -> dict:
    """
    将 FC 收到的 multipart/form-data 请求原样转发到 ECS 推理服务。
    FC HTTP 触发器会把原始 body 和 headers 都传进来。
    """
    headers = evt.get("headers", {})
    content_type = headers.get("content-type", headers.get("Content-Type", ""))

    if "multipart/form-data" in content_type:
        # 原始 multipart 转发
        raw_body = evt.get("body_raw") or evt.get("body", b"")
        if isinstance(raw_body, str):
            raw_body = raw_body.encode()
        resp = requests.post(
            f"{INFER_BASE}/generate",
            data=raw_body,
            headers={"Content-Type": content_type},
            timeout=timeout,
        )
    else:
        # JSON 模式（body 里是 JSON 参数，不含图片文件）
        body = evt.get("body_parsed") or evt.get("body") or {}
        resp = requests.post(
            f"{INFER_BASE}/generate",
            json=body,
            timeout=timeout,
        )

    resp.raise_for_status()
    return resp.json()


# ── FC 函数入口 ────────────────────────────────────────────

def handler(event, context):
    """
    FC 函数入口

    请求格式（multipart/form-data，通过 HTTP 触发器）：
      ref_images: File[]   # 多张参考图
      prompt:     string
      num_frames: int      # 默认 81
      fps:        int      # 默认 16
      cfg_scale:  float    # 默认 5.0
      num_steps:  int      # 默认 50
      resolution: string   # "540p" | "720p"

    返回：
      {
        "job_id": "...",
        "status": "success",
        "video_url": "https://oss.../videos/xxx.mp4?...",
        "elapsed_sec": 180.5,
        ...
      }
    """
    request_id = getattr(context, "request_id", uuid.uuid4().hex[:8])
    logger.info(f"[{request_id}] FC handler invoked")

    evt = parse_event(event if isinstance(event, bytes) else event.encode()
                      if isinstance(event, str) else bytes(event))

    try:
        # ── 1. 启动 ECS 实例 ──────────────────────────────
        logger.info(f"[{request_id}] Step 1/4: Start ECS instance")
        start_instance()

        # ── 2. 等待服务就绪 ───────────────────────────────
        logger.info(f"[{request_id}] Step 2/4: Wait for service ready")
        if not wait_for_service(TIMEOUT_START):
            stop_instance()
            return _error_response("推理服务启动超时，请稍后重试", 503)

        # ── 3. 转发推理请求 ───────────────────────────────
        logger.info(f"[{request_id}] Step 3/4: Forward inference request")
        result = forward_multipart(evt, timeout=TIMEOUT_INFER)
        logger.info(f"[{request_id}] Inference completed: job_id={result.get('job_id')}")

        return json.dumps(result, ensure_ascii=False)

    except requests.exceptions.Timeout:
        logger.error(f"[{request_id}] Inference timeout after {TIMEOUT_INFER}s")
        return _error_response("推理超时，视频生成耗时过长，请减少帧数或降低分辨率", 504)

    except Exception as e:
        logger.error(f"[{request_id}] Unexpected error: {e}", exc_info=True)
        return _error_response(str(e), 500)

    finally:
        # ── 4. 推理完成后关机 ─────────────────────────────
        logger.info(f"[{request_id}] Step 4/4: Stop ECS instance")
        stop_instance()


def _error_response(message: str, code: int = 500) -> str:
    return json.dumps({
        "status":  "error",
        "code":    code,
        "message": message,
    }, ensure_ascii=False)
