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
TIMEOUT_START  = int(os.environ.get("TIMEOUT_START", "1800"))
TIMEOUT_INFER  = int(os.environ.get("TIMEOUT_INFER", "120"))
AUTO_STOP      = os.environ.get("AUTO_STOP", "true").lower() == "true"

acs_client = AcsClient(ACCESS_KEY, ACCESS_SECRET, REGION_ID)

def get_status():
    req = DescribeInstancesRequest.DescribeInstancesRequest()
    req.set_InstanceIds(json.dumps([INSTANCE_ID]))
    resp = json.loads(acs_client.do_action_with_exception(req))
    return resp["Instances"]["Instance"][0]["Status"]

def start_instance():
    if get_status() == "Running":
        logger.info("Already Running"); return
    req = StartInstanceRequest.StartInstanceRequest()
    req.set_InstanceId(INSTANCE_ID)
    acs_client.do_action_with_exception(req)

def stop_instance():
    if not AUTO_STOP: return
    try:
        req = StopInstanceRequest.StopInstanceRequest()
        req.set_InstanceId(INSTANCE_ID)
        req.set_StoppedMode("KeepCharging")
        acs_client.do_action_with_exception(req)
    except Exception as e:
        logger.warning(f"Stop warning: {e}")

def wait_for_service():
    deadline = time.time() + TIMEOUT_START
    last_log = time.time()
    while time.time() < deadline:
        try:
            r = requests.get(f"{INFER_BASE}/health", timeout=5)
            if r.status_code == 200 and r.json().get("model_loaded"):
                logger.info(f"Ready ✓ {r.json()}"); return True
        except: pass
        if time.time() - last_log >= 30:
            logger.info(f"Waiting... {int(time.time()-(deadline-TIMEOUT_START))}s elapsed")
            last_log = time.time()
        time.sleep(5)
    return False

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
    try:
        start_instance()
        if not wait_for_service():
            return json.dumps({"error": "startup timeout"})
        resp = requests.post(f"{INFER_BASE}/generate",
            data=body_bytes, headers={"Content-Type": ct}, timeout=TIMEOUT_INFER)
        return json.dumps(resp.json(), ensure_ascii=False)
    except requests.exceptions.Timeout:
        return json.dumps({"error": "inference timeout"})
    except Exception as e:
        logger.error(f"Error: {e}", exc_info=True)
        return json.dumps({"error": str(e)})
    finally:
        stop_instance()

