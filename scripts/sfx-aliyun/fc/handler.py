import os, json, time, requests, logging
from aliyunsdkcore.client import AcsClient
from aliyunsdkecs.request.v20140526 import (
    StartInstanceRequest,
    StopInstanceRequest,
)

logger = logging.getLogger()
logger.setLevel(logging.INFO)

# 从环境变量读取（FC 控制台配置）
ACCESS_KEY     = os.environ["ALIBABA_ACCESS_KEY"]
ACCESS_SECRET  = os.environ["ALIBABA_ACCESS_SECRET"]
REGION_ID      = os.environ.get("REGION_ID", "cn-hangzhou")
INSTANCE_ID    = os.environ["ECS_INSTANCE_ID"]
ECS_PRIVATE_IP = os.environ["ECS_PRIVATE_IP"]   # VPC 内网 IP
INFER_PORT     = os.environ.get("INFER_PORT", "8000")
INFER_BASE     = f"http://{ECS_PRIVATE_IP}:{INFER_PORT}"
TIMEOUT_START  = int(os.environ.get("TIMEOUT_START", "180"))  # 秒

acs_client = AcsClient(ACCESS_KEY, ACCESS_SECRET, REGION_ID)


def start_instance():
    req = StartInstanceRequest.StartInstanceRequest()
    req.set_InstanceId(INSTANCE_ID)
    acs_client.do_action_with_exception(req)
    logger.info(f"Instance {INSTANCE_ID} start requested")


def stop_instance():
    req = StopInstanceRequest.StopInstanceRequest()
    req.set_InstanceId(INSTANCE_ID)
    req.set_StoppedMode("KeepCharging")  # 停机不释放，保留磁盘
    acs_client.do_action_with_exception(req)
    logger.info(f"Instance {INSTANCE_ID} stop requested")


def wait_for_service(timeout: int = TIMEOUT_START) -> bool:
    """轮询健康检查，等待推理服务就绪"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            r = requests.get(f"{INFER_BASE}/health", timeout=3)
            if r.status_code == 200:
                logger.info("Service is ready ✓")
                return True
        except requests.exceptions.RequestException:
            pass
        time.sleep(5)
    return False


def handler(event, context):
    """FC 函数入口"""
    evt = json.loads(event)
    body = evt.get("body", evt)  # 兼容 HTTP 触发器
    if isinstance(body, str):
        body = json.loads(body)

    logger.info(f"Request: {body}")

    # 1. 启动 ECS 实例
    try:
        start_instance()
    except Exception as e:
        # 实例已在运行时会报错，忽略
        logger.warning(f"Start instance warning (may already be running): {e}")

    # 2. 等待服务就绪
    if not wait_for_service():
        stop_instance()
        return json.dumps({"error": "Service startup timeout"}, ensure_ascii=False)

    # 3. 转发推理请求
    try:
        resp = requests.post(
            f"{INFER_BASE}/generate",
            json=body,
            timeout=120,
        )
        result = resp.json()
    except Exception as e:
        result = {"error": str(e)}
    finally:
        # 4. 无论成功失败都关机
        stop_instance()

    return json.dumps(result, ensure_ascii=False)
