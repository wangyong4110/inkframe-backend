"""
Creative Hub FC 触发器

路由规则（优先级递减）：
  1. URL path 末尾：/video/i2v、/video、/sfx、/music
  2. body 中 model 字段：wan/wan22/video → /video；tangoflux/sfx → /sfx；acestep/music → /music
  3. multipart + video 文件 → /video/i2v
  4. body 含 num_frames → /video
  5. body 含 lyrics → /music
  6. 默认 → /sfx
"""
import os, json, time, base64, logging, requests
from aliyunsdkcore.client import AcsClient
from aliyunsdkecs.request.v20140526 import (
    StartInstanceRequest, StopInstanceRequest, DescribeInstancesRequest,
)

logger = logging.getLogger()
logger.setLevel(logging.INFO)

ACCESS_KEY     = os.environ["ALIBABA_ACCESS_KEY"]
ACCESS_SECRET  = os.environ["ALIBABA_ACCESS_SECRET"]
REGION_ID      = os.environ.get("REGION_ID", "cn-hangzhou")
INSTANCE_ID    = os.environ["ECS_INSTANCE_ID"]
ECS_PRIVATE_IP = os.environ["ECS_PRIVATE_IP"]
INFER_BASE     = f"http://{ECS_PRIVATE_IP}:8000"
TIMEOUT_START  = int(os.environ.get("TIMEOUT_START", "3600"))
TIMEOUT_INFER  = int(os.environ.get("TIMEOUT_INFER", "600"))
AUTO_STOP      = os.environ.get("AUTO_STOP", "true").lower() == "true"

acs_client = AcsClient(ACCESS_KEY, ACCESS_SECRET, REGION_ID)

MODEL_ROUTE = {
    "wan": "/video", "wan22": "/video", "video": "/video",
    "i2v": "/video/i2v",
    "tangoflux": "/sfx", "sfx": "/sfx",
    "acestep": "/music", "music": "/music",
}


def get_status():
    req = DescribeInstancesRequest.DescribeInstancesRequest()
    req.set_InstanceIds(json.dumps([INSTANCE_ID]))
    return json.loads(acs_client.do_action_with_exception(req))["Instances"]["Instance"][0]["Status"]


def start_instance():
    if get_status() == "Running":
        logger.info("Already Running"); return
    req = StartInstanceRequest.StartInstanceRequest()
    req.set_InstanceId(INSTANCE_ID)
    acs_client.do_action_with_exception(req)
    logger.info("Instance start requested")


def stop_instance():
    if not AUTO_STOP: return
    try:
        req = StopInstanceRequest.StopInstanceRequest()
        req.set_InstanceId(INSTANCE_ID)
        req.set_StoppedMode("KeepCharging")
        acs_client.do_action_with_exception(req)
        logger.info("Instance stop requested")
    except Exception as e:
        logger.warning(f"Stop: {e}")


def wait_for_service() -> bool:
    deadline = time.time() + TIMEOUT_START
    last_log = time.time()
    while time.time() < deadline:
        try:
            r = requests.get(f"{INFER_BASE}/health", timeout=5)
            if r.status_code == 200:
                d = r.json()
                models = d.get("models", {})
                # TangoFlux + ACE-Step 就绪即可（Wan 2.2 按需子进程）
                if models.get("tangoflux") and models.get("acestep"):
                    logger.info(f"Service ready ✓  VRAM free={d.get('vram_free_gb')}GB")
                    return True
        except: pass
        if time.time() - last_log >= 30:
            logger.info(f"Waiting {int(time.time()-(deadline-TIMEOUT_START))}s elapsed")
            last_log = time.time()
        time.sleep(8)
    return False


def resolve_path(evt: dict, body_bytes: bytes, ct: str) -> str:
    # 1. URL path
    raw_path = evt.get("path", "") or evt.get("rawPath", "")
    for ep in ["/video/i2v", "/video", "/sfx", "/music"]:
        if raw_path.endswith(ep):
            return ep

    # 2. model 字段
    if b"model" in body_bytes:
        try:
            key = json.loads(body_bytes).get("model", "").lower()
            if key in MODEL_ROUTE:
                return MODEL_ROUTE[key]
        except: pass

    # 3. multipart + video → i2v
    if "multipart" in ct and b"video" in body_bytes[:8192]:
        return "/video/i2v"

    # 4. num_frames → video
    if b"num_frames" in body_bytes:
        return "/video"

    # 5. lyrics → music
    if b"lyrics" in body_bytes:
        return "/music"

    return "/sfx"


def handler(event, context):
    raw = event if isinstance(event, bytes) else event.encode() if isinstance(event, str) else bytes(event)
    try: evt = json.loads(raw)
    except: evt = {}

    body = evt.get("body", evt)
    if evt.get("isBase64Encoded") and isinstance(body, str):
        body_bytes = base64.b64decode(body)
    elif isinstance(body, str):
        body_bytes = body.encode()
    else:
        body_bytes = body or b"{}"

    headers = evt.get("headers", {})
    ct = headers.get("content-type") or headers.get("Content-Type") or "application/json"
    path = resolve_path(evt, body_bytes, ct)
    logger.info(f"Routing → {path}  ct={ct[:40]}")

    try:
        start_instance()
        if not wait_for_service():
            return json.dumps({"error": "startup timeout"})
        resp = requests.post(
            f"{INFER_BASE}{path}",
            data=body_bytes,
            headers={"Content-Type": ct},
            timeout=TIMEOUT_INFER,
        )
        return json.dumps(resp.json(), ensure_ascii=False)
    except requests.exceptions.Timeout:
        return json.dumps({"error": "inference timeout"})
    except Exception as e:
        logger.error(f"Error: {e}", exc_info=True)
        return json.dumps({"error": str(e)})
    finally:
        stop_instance()
