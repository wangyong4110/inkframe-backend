import os
import json
import time
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

ACCESS_KEY     = os.environ["ALIBABA_ACCESS_KEY"]
ACCESS_SECRET  = os.environ["ALIBABA_ACCESS_SECRET"]
REGION_ID      = os.environ.get("REGION_ID", "cn-hangzhou")
INSTANCE_ID    = os.environ["ECS_INSTANCE_ID"]
ECS_PRIVATE_IP = os.environ["ECS_PRIVATE_IP"]
INFER_PORT     = os.environ.get("INFER_PORT", "8000")
INFER_BASE     = f"http://{ECS_PRIVATE_IP}:{INFER_PORT}"
# 首次启动需下载约 28GB 模型，保守设 2400 秒（约 40 分钟）兜底
# 后续启动模型已在磁盘，实际约 90-120 秒
TIMEOUT_START  = int(os.environ.get("TIMEOUT_START", "2400"))
TIMEOUT_INFER  = int(os.environ.get("TIMEOUT_INFER", "600"))
AUTO_STOP      = os.environ.get("AUTO_STOP", "true").lower() == "true"

acs_client = AcsClient(ACCESS_KEY, ACCESS_SECRET, REGION_ID)


def get_instance_status() -> str:
    req = DescribeInstancesRequest.DescribeInstancesRequest()
    req.set_InstanceIds(json.dumps([INSTANCE_ID]))
    resp = json.loads(acs_client.do_action_with_exception(req))
    instances = resp.get("Instances", {}).get("Instance", [])
    if not instances:
        raise RuntimeError(f"Instance {INSTANCE_ID} not found")
    return instances[0]["Status"]


def start_instance():
    status = get_instance_status()
    if status == "Running":
        logger.info("Instance already Running")
        return
    logger.info(f"Starting instance (current: {status})")
    req = StartInstanceRequest.StartInstanceRequest()
    req.set_InstanceId(INSTANCE_ID)
    acs_client.do_action_with_exception(req)


def stop_instance():
    if not AUTO_STOP:
        return
    try:
        req = StopInstanceRequest.StopInstanceRequest()
        req.set_InstanceId(INSTANCE_ID)
        req.set_StoppedMode("KeepCharging")
        acs_client.do_action_with_exception(req)
        logger.info("Instance stop requested")
    except Exception as e:
        logger.warning(f"Stop instance warning: {e}")


def wait_for_service(timeout: int = TIMEOUT_START) -> bool:
    logger.info(f"Waiting for service (timeout={timeout}s)")
    deadline  = time.time() + timeout
    last_log  = time.time()
    while time.time() < deadline:
        try:
            r = requests.get(f"{INFER_BASE}/health", timeout=5)
            if r.status_code == 200 and r.json().get("model_loaded"):
                logger.info(f"Service ready ✓  {r.json()}")
                return True
        except requests.exceptions.RequestException:
            pass
        if time.time() - last_log >= 30:
            elapsed = int(time.time() - (deadline - timeout))
            logger.info(f"Still waiting... ({elapsed}s elapsed)")
            last_log = time.time()
        time.sleep(8)
    return False


def handler(event, context):
    """
    FC 函数入口，转发 multipart/form-data 请求到 ECS /generate

    请求格式（multipart/form-data）：
      ref_images: File[]   多张参考图
      prompt:     string
      num_frames: int      默认 81
      fps:        int      默认 16
      cfg_scale:  float    默认 5.0
      num_steps:  int      默认 50
      resolution: string   540p | 720p
    """
    if isinstance(event, (bytes, bytearray)):
        raw = event
    elif isinstance(event, str):
        raw = event.encode()
    else:
        raw = bytes(event)

    try:
        evt = json.loads(raw)
    except Exception:
        evt = {}

    is_b64   = evt.get("isBase64Encoded", False)
    body_raw = evt.get("body", "")
    if is_b64 and body_raw:
        body_bytes = base64.b64decode(body_raw)
    elif isinstance(body_raw, str):
        body_bytes = body_raw.encode()
    else:
        body_bytes = body_raw or b""

    headers      = evt.get("headers", {})
    content_type = (
        headers.get("content-type")
        or headers.get("Content-Type")
        or "multipart/form-data"
    )

    logger.info(f"Forwarding to /generate  content-type={content_type[:60]}")

    try:
        start_instance()

        if not wait_for_service():
            return json.dumps({"error": "Service startup timeout"}, ensure_ascii=False)

        resp = requests.post(
            f"{INFER_BASE}/generate",
            data=body_bytes,
            headers={"Content-Type": content_type},
            timeout=TIMEOUT_INFER,
        )
        return json.dumps(resp.json(), ensure_ascii=False)

    except requests.exceptions.Timeout:
        return json.dumps({"error": "Inference timeout"}, ensure_ascii=False)
    except Exception as e:
        logger.error(f"Error: {e}", exc_info=True)
        return json.dumps({"error": str(e)}, ensure_ascii=False)
    finally:
        stop_instance()
