import os, json, time, requests, logging
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
TIMEOUT_START  = int(os.environ.get("TIMEOUT_START", "180"))
TIMEOUT_INFER  = int(os.environ.get("TIMEOUT_INFER", "120"))
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
        logger.info("Instance already Running, skip start")
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
    logger.info(f"Waiting for service at {INFER_BASE}/health (timeout={timeout}s)")
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(f"{INFER_BASE}/health", timeout=5)
            if r.status_code == 200:
                logger.info(f"Service ready ✓  {r.json()}")
                return True
        except requests.exceptions.RequestException:
            pass
        time.sleep(5)
    return False


def handler(event, context):
    evt = json.loads(event if isinstance(event, (str, bytes)) else event)
    body = evt.get("body", evt)
    if isinstance(body, str):
        try:
            body = json.loads(body)
        except Exception:
            pass

    logger.info(f"Request body: {body}")

    try:
        # 1. 启动实例
        start_instance()

        # 2. 等待服务就绪
        if not wait_for_service():
            return json.dumps({"error": "Service startup timeout"}, ensure_ascii=False)

        # 3. 转发推理请求
        resp = requests.post(
            f"{INFER_BASE}/generate",
            json=body,
            timeout=TIMEOUT_INFER,
        )
        return json.dumps(resp.json(), ensure_ascii=False)

    except requests.exceptions.Timeout:
        return json.dumps({"error": "Inference timeout"}, ensure_ascii=False)
    except Exception as e:
        logger.error(f"Error: {e}")
        return json.dumps({"error": str(e)}, ensure_ascii=False)
    finally:
        stop_instance()
