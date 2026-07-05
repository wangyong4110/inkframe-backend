# SkyReels-A2 × 阿里云 FC 部署方案

多角色视频生成服务：**ECS A100 按需启停 + 函数计算 FC 触发器**

---

## 架构

```
你的应用
  ↓ POST /generate (multipart: ref_images + prompt)
函数计算 FC  ← 常驻，几乎免费
  ↓ 启动
ECS A100 80G（抢占式，约 ¥8-12/h）
  └─ Docker → SkyReels-A2 推理服务（FastAPI）
       ↓ 推理完成
       └─ 视频上传 OSS → 返回下载链接
  ↓ 推理完成后自动关机
```

## 硬件要求

SkyReels-A2（Wan2.1 14B 微调）在 540P 分辨率下峰值 VRAM 约 51GB。

| 规格 | GPU | VRAM | 按量价格 | 抢占价格 |
|------|-----|------|---------|---------|
| ecs.gn7e-c16g1.4xlarge | A100 × 1 | 80GB | ~¥34/h | ~¥8-12/h ✅ |
| ecs.gn7e-c16g1.8xlarge | A100 × 2 | 160GB | ~¥68/h | ~¥16-24/h |

**推荐：A100 80G 单卡抢占式**

## 快速开始

```bash
# 1. 克隆/下载项目
git clone <this-repo>
cd skyreels-a2-aliyun

# 2. 填写配置
cp infra/config.env.example infra/config.env
vim infra/config.env

# 3. 一键部署（约 45-90 分钟，主要是模型下载）
chmod +x deploy.sh
./deploy.sh
```

## 调用示例

```bash
FC_URL="https://<账号ID>.cn-hangzhou.fc.aliyuncs.com/2016-08-15/proxy/skyreels-service/skyreels-a2-trigger"

# 两张参考图 → 多角色视频
curl -X POST "${FC_URL}/generate" \
  -F "ref_images=@char_a.jpg" \
  -F "ref_images=@char_b.jpg" \
  -F 'prompt=Two women talking in a dimly lit cafe. The woman on the left has curly red hair. The woman on the right has straight black hair. They are having a serious conversation.' \
  -F 'num_frames=81' \
  -F 'fps=16' \
  -F 'resolution=540p' \
  | python3 -m json.tool
```

返回：
```json
{
  "job_id": "abc123...",
  "status": "success",
  "video_url": "https://your-bucket.oss-cn-hangzhou.aliyuncs.com/videos/abc123.mp4?...",
  "elapsed_sec": 245.3,
  "params": {
    "prompt": "Two women talking...",
    "num_frames": 81,
    "resolution": "540p",
    "num_refs": 2
  }
}
```

## Prompt 写法建议

角色区分越清晰，生成效果越好：

```
Two [角色描述] in [场景].
[角色A描述，包含位置、外貌特征].
[角色B描述，包含位置、外貌特征].
They are [动作/交互].
```

示例：
```
Two young women talking in a cozy coffee shop, afternoon light.
The woman on the left has curly auburn hair, wearing a blue denim jacket, smiling.
The woman on the right has straight black bob, wearing a white blouse, nodding.
They are having a heartfelt conversation over cups of coffee.
```

## 成本估算

| 费用项 | 单价 | 月均（20次/月） |
|--------|------|----------------|
| A100 抢占式（推理 5min/次） | ¥10/h | ≈ ¥17 |
| OSS 存储（约 200MB/视频） | ¥0.12/GB | ≈ ¥0.5 |
| 系统盘（100GB ESSD） | ¥0.4/GB/月 | ≈ ¥40 |
| 函数计算 FC | 免费额度内 | ¥0 |
| **月均合计** | | **≈ ¥58** |

## 常见问题

**Q: 推理超时？**
生成 540P 81帧视频约需 3-8 分钟。FC 函数超时设为 600 秒，如需更长，调整 `FC_TIMEOUT` 并考虑异步模式。

**Q: 显存不足？**
在 `server.py` 中已开启 `enable_fp8=True` 和 `enable_offload=True`，可显著降低显存需求。720P 需要更多显存，建议先用 540P 验证。

**Q: 抢占式实例被回收？**
FC handler 会检查实例状态，若实例被回收会尝试重新启动。建议在业务低峰期使用。

**Q: 多角色身份混淆？**
Prompt 中明确指定每个角色的位置（left/right）和特征，尽量避免两个角色外貌相似。
