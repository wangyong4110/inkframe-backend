# AI 短视频生产流水线 · 阿里云 FC 部署手册

> 基于开源模型的 AI 短视频全链路自动化方案，所有模型统一部署在阿里云 ECS GPU 实例，通过函数计算 FC 按需触发，闲置零成本。

---

## 目录

- [整体架构](#整体架构)
- [快速开始](#快速开始)
- [模型分类总览](#模型分类总览)
- [文生图（T2I）](#文生图t2i)
- [文生视频（T2V）](#文生视频t2v)
- [文生音效（T2SFX）](#文生音效t2sfx)
- [文生音乐（T2M）](#文生音乐t2m)
- [各类横向对比](#各类横向对比)
- [成本估算](#成本估算)
- [部署规范](#部署规范)

---

## 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                    AI 短视频制作流水线                        │
├──────────┬──────────┬──────────┬──────────┬────────────────┤
│  文生图  │  文生视频 │  文生音效 │  文生音乐 │  （待扩展）    │
│  T2I     │  T2V     │  T2SFX   │  T2M     │  TTS / ASR     │
│          │          │          │          │  超分 / 帧插值  │
│          │          │          │          │  数字人        │
└──────────┴──────────┴──────────┴──────────┴────────────────┘
                           ↕
          ┌────────────────────────────────┐
          │     阿里云函数计算 FC（触发层） │  常驻，几乎免费
          │     HTTP 触发器 · 按需路由     │
          └─────────────┬──────────────────┘
                        ↕ 启动/停止
          ┌─────────────────────────────────┐
          │   阿里云 ECS GPU 实例（推理层）  │  按秒计费
          │   抢占式实例 · 推理完成后自动关机 │
          │   Docker 容器 · 模型磁盘挂载    │
          └─────────────┬───────────────────┘
                        ↕
          ┌─────────────────────────────────┐
          │      阿里云 OSS（存储层）        │  生成文件持久化
          │      音频 / 图片 / 视频文件      │
          └─────────────────────────────────┘
```

### 核心设计原则

| 原则 | 说明 |
|------|------|
| **按需启停** | FC 触发时才启动 ECS 实例，推理完成立即关机，闲置零成本 |
| **模型磁盘挂载** | 模型权重存储在 ECS 系统盘，不烘焙进 Docker 镜像，首次下载后永久缓存 |
| **自动下载** | `ecs-userdata.sh` 开机自动检测模型是否存在，缺失时从 `hf-mirror.com` 下载 |
| **统一接口** | 所有模型服务均暴露 `POST /generate` + `GET /health`，FC handler 统一转发 |
| **国内镜像** | Docker 基础镜像使用 `pytorch/pytorch` 官方镜像，pip 使用阿里云 PyPI 镜像 |

---

## 快速开始

### 前置条件

```bash
# 必须安装的工具
docker      # Docker 本地构建
aliyun      # 阿里云 CLI
zip base64  # 脚本依赖

# 阿里云控制台需提前创建
# - ECS GPU 实例（规格按模型选择，见各章节）
# - OSS Bucket（与 ECS 同 Region）
# - ACR 命名空间（个人版免费）
# - VPC + 安全组（开放 8000 端口，VPC 内访问）
```

### 通用部署流程（所有方案一致）

```bash
# 1. 进入任意模型目录
cd <model-name>/

# 2. 填写配置
cp infra/config.env.example infra/config.env
vim infra/config.env          # 填写 AK/SK、ECS 实例 ID、OSS Bucket 等

# 3. 一键部署（构建镜像 → 推送 ACR → 配置 ECS → 部署 FC）
chmod +x deploy.sh && ./deploy.sh

# 4. 调用测试
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{"prompt": "your prompt here"}'
```

### 首次调用说明

```
首次调用完整耗时：
  ECS 启动          30s
  Docker 启动       15s
  模型下载（首次）   5-60min（视模型大小）
  模型加载          20-120s
  推理              视模型和参数

后续调用（模型已在磁盘）：
  ECS 启动 + Docker + 模型加载   60-120s
  推理                           按模型规格
```

---

## 模型分类总览

| 类别 | 方案数 | 最低 VRAM | T4 可用 | Apache 2.0 |
|------|--------|---------|---------|------------|
| 文生图 T2I | 4 | 8GB | ✅ 全部 | 2/4 |
| 文生视频 T2V | 6 | 16GB | ✅ 部分 | 4/6 |
| 文生音效 T2SFX | 2 | 6GB | ✅ 全部 | 1/2 |
| 文生音乐 T2M | 4 | 8GB | ✅ 3/4 | 2/4 |
| **合计** | **16** | — | — | — |

---

## 文生图（T2I）

> 目录：`t2i-aliyun/`

### FLUX.1 [schnell]

**简介**
FLUX.1 schnell 是 Black Forest Labs 发布的快速蒸馏版本，4 步即可生成高质量图像，Apache 2.0 协议完全商用。采用 12B 参数的 Diffusion Transformer 架构，在写实度、文字渲染和解剖准确性上均超越 SDXL。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 12B |
| 推理步数 | 1–4 步（建议 4） |
| 最低 VRAM | 16GB（CPU offload） |
| 输出分辨率 | 最高 2048×2048 |
| 采样率 | — |
| 许可证 | Apache 2.0 ✅ |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "a portrait of a young woman, cinematic lighting, 8k",
    "width": 1024,
    "height": 1024,
    "num_steps": 4,
    "guidance": 0.0,
    "seed": 42
  }'
```

**适合场景**：商业图像生成、内容批量创作、需要极速出图的流水线

---

### FLUX.1 [dev]

**简介**
FLUX.1 dev 是 schnell 的高质量版本，推理步数更多但图像质量显著提升。在写实人像基准测试中，与同等分辨率 SDXL 相比产生的解剖变形减少 30–40%。适合对质量要求高于速度的场景。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 12B |
| 推理步数 | 20–50 步（建议 20） |
| 最低 VRAM | 16GB（CPU offload）/ 24GB（全量） |
| 输出分辨率 | 最高 2048×2048 |
| 许可证 | 非商用需申请授权 |

**ECS 规格**：`ecs.gn6v-c8g1.2xlarge`（V100 32G，A10 24G，抢占式约 ¥3–5/h）或 T4+CPU offload

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "professional product photography, white background, soft shadow",
    "width": 1024,
    "height": 1024,
    "num_steps": 20,
    "guidance": 3.5
  }'
```

**适合场景**：高质量图像生成、写实人像、产品图

---

### FLUX.2 [klein] 4B

**简介**
FLUX.2 klein 是 Black Forest Labs 的下一代轻量模型，4B 参数、Apache 2.0 协议，约 8GB VRAM 即可运行，速度比 FLUX.1 schnell 更快，质量持平甚至更优。是目前「质量 × 显存占用 × 商用许可」综合最优的选择。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 4B |
| 推理步数 | 1–4 步（蒸馏模型） |
| 最低 VRAM | 8GB |
| 输出分辨率 | 最高 2048×2048 |
| 许可证 | Apache 2.0 ✅ |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "futuristic city at night, neon lights, rain reflections",
    "width": 1024,
    "height": 1024,
    "num_steps": 4,
    "guidance": 3.5
  }'
```

**适合场景**：资源受限环境、高频批量生成、商用项目首选

---

### Stable Diffusion 3.5 Large

**简介**
Stability AI 推出的 SD 3.5 Large 改善了早期版本在手部渲染和文字生成上的弱点，拥有最丰富的生态（LoRA、ControlNet、ComfyUI 工作流），适合需要风格定制或 LoRA 微调的场景。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 8B MMDiT |
| 推理步数 | 20–50 步（建议 28） |
| 最低 VRAM | 16GB（T5 CPU offload） |
| 输出分辨率 | 最高 2048×2048 |
| 许可证 | Stability AI 社区许可（查官方条款） |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "oil painting, impressionist style, sunflower field at sunset",
    "negative_prompt": "blurry, watermark, low quality",
    "width": 1024,
    "height": 1024,
    "num_steps": 28,
    "guidance": 7.0
  }'
```

**适合场景**：艺术风格生成、LoRA 风格迁移、ComfyUI 工作流集成

---

## 文生视频（T2V）

> 目录：`t2v-aliyun/`（新增 4 套）+ `wan22-aliyun/`、`skyreels-a2-aliyun/`

### Wan 2.2

**简介**
阿里巴巴 Tongyi 团队发布的最通用开源视频模型，MoE 架构在质量与算力之间取得最佳平衡，在 Wan-Bench 2.0 上超越多数商业闭源模型。一套代码支持 T2V、I2V、视频编辑三种任务，Apache 2.0 协议完全商用。

**技术参数**
| 变体 | 参数 | VRAM | 分辨率 | FPS |
|------|------|------|--------|-----|
| TI2V-5B（默认） | 5B | 24GB | 720p | 24fps |
| T2V-A14B | 27B MoE（14B 激活） | 80GB | 720p | 24fps |
| I2V-A14B | 27B MoE（14B 激活） | 80GB | 720p | 24fps |

**ECS 规格**
- TI2V-5B：`ecs.gn6i-c4g1.xlarge`（T4 16G，约 ¥0.5–0.9/h）
- A14B 变体：`ecs.gn7e-c16g1.4xlarge`（A100 80G，约 ¥8–12/h）

**API 示例**
```bash
# 文本转视频（T2V）
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "Two cats boxing on a stage, dramatic lighting, slow motion",
    "num_frames": 81,
    "resolution": "720p"
  }'

# 图片转视频（I2V）
curl -X POST http://<ECS_IP>:8000/generate/i2v \
  -F 'image=@input.jpg' \
  -F 'prompt=The cat starts swimming in the ocean' \
  -F 'num_frames=81'
```

**适合场景**：通用视频生成、商用项目首选、多任务单模型部署

---

### SkyReels-A2

**简介**
SkyworkAI 发布的多角色视频生成模型，专为 Elements-to-Video（E2V）任务设计——将多个视觉元素（角色、物体、背景参考图）组合进视频，同时对每个元素保持严格的参考图一致性。是第一个面向 E2V 任务的开源商业级模型，在 A2-Bench 上与闭源商业模型相当。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | Wan2.1 14B 微调 |
| 最低 VRAM | 51GB（540P），推荐 A100 80G |
| 最大分辨率 | 720P |
| 参考图数量 | 最多 4 张（角色 + 背景） |
| 许可证 | Apache 2.0 ✅ |

**ECS 规格**：`ecs.gn7e-c16g1.4xlarge`（A100 80G，抢占式约 ¥8–12/h）

**API 示例**
```bash
# 多角色视频生成（multipart 上传参考图）
curl -X POST http://<ECS_IP>:8000/generate \
  -F 'ref_images=@char_a.jpg' \
  -F 'ref_images=@char_b.jpg' \
  -F 'prompt=Two women talking in a cozy cafe. The woman on the left has curly red hair. The woman on the right has straight black hair. They are laughing.' \
  -F 'resolution=540p' \
  -F 'num_frames=81'
```

**Prompt 建议**：明确指定每个角色的位置（left/right）和特征，避免角色身份混淆

**适合场景**：多角色短剧、商业广告（指定人物形象）、影视预演

---

### HunyuanVideo 1.5

**简介**
腾讯发布的视频生成模型，8.3B 参数，以运动物理真实感著称，流体、布料模拟和物体交互最接近真实物理规律。480p I2V 在单张 RTX 4090 上约 75 秒渲染完成，是开源方案中速度与质量最均衡的选择之一。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 8.3B |
| 最低 VRAM | 14GB（CPU offload + VAE tiling） |
| 推荐分辨率 | 848×480（480P），1280×720（720P） |
| 推荐帧数 | 61 帧（约 4s@15fps）或 121 帧（约 8s） |
| 许可证 | 查官方条款 |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，CPU offload 可运行，约 ¥0.5–0.9/h）

**API 示例**
```bash
# 文本转视频
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "A golden retriever runs along the beach at sunset, splashing in the waves",
    "width": 848, "height": 480,
    "num_frames": 61,
    "num_steps": 30,
    "fps": 15
  }'

# 图片转视频
curl -X POST http://<ECS_IP>:8000/generate/i2v \
  -F 'image=@dog.jpg' \
  -F 'prompt=The dog starts running towards the ocean' \
  -F 'num_frames=61'
```

**适合场景**：需要真实物理运动的视频（水、火、布料、动物）

---

### LTX-2

**简介**
Lightricks 发布的 22B 参数模型，2026 年 3 月发布，是目前唯一原生支持音视频同步生成的开源视频模型，支持 4K@50fps 输出。Distilled 版本（xl-turbo）8 步即可生成完整视频，在 A100 上约 1 分钟生成 5 秒 720P 视频。训练数据来自 Getty Images 和 Shutterstock 授权，商用无版权风险。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 22B |
| 最低 VRAM | 12GB（Q4 量化 720P），32GB（官方推荐） |
| 最高分辨率 | 4K@50fps |
| 帧数限制 | 必须为 8n+1（如 121、81、49） |
| 音频支持 | ✅ 原生音视频同步 |
| 许可证 | Apache 2.0（年营收 <$10M） |

**ECS 规格**：推荐 `ecs.gn6v-c8g1.2xlarge`（A10 24G，约 ¥3–5/h）；16G 需开启 CPU offload

**API 示例**
```bash
# 文本转视频（distilled 快速模式）
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "A time-lapse of a city street transitioning from day to night, people walking",
    "width": 768, "height": 512,
    "num_frames": 121,
    "num_steps": 8,
    "guidance": 1.0,
    "use_upsampler": true
  }'
```

**适合场景**：需要带配音的产品演示、快速内容生产、4K 高清视频输出

---

### Open-Sora 2.0

**简介**
浪潮（HPCAI）发布的 11B 参数模型，MIT 协议，是开源视频生成领域唯一公开完整训练代码的模型。研究团队估计训练一个商业级模型成本约 20 万美元，适合需要在私有数据上微调的团队。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 11B |
| 最低 VRAM | 24GB（A10） |
| 推荐分辨率 | 240P / 480P / 720P |
| 推荐帧数 | 51 帧（约 3s@17fps） |
| 许可证 | MIT ✅ |

**ECS 规格**：`ecs.gn6v-c8g1.2xlarge`（A10 24G，约 ¥3–5/h）或 T4 16G（480P）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "A butterfly emerging from a cocoon in slow motion, macro photography",
    "resolution": "480p",
    "num_frames": 51,
    "num_steps": 30
  }'
```

**适合场景**：需要私有数据微调、科研用途、定制风格视频生成

---

### CogVideoX 1.5

**简介**
智谱 AI 发布的视频生成模型，2B/5B 两个参数档位，Apache 2.0 协议，是所有方案中 VRAM 需求最低的。通过 VAE tiling + slicing + CPU offload 三重优化，5B 模型在 T4 16G 上可稳定运行。生态完整，diffusers 原生支持。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 2B / 5B（推荐 5B） |
| 最低 VRAM | 16GB（CPU offload + VAE tiling） |
| 推荐分辨率 | 1360×768 |
| 推荐帧数 | 49 帧（约 6s@8fps） |
| 许可证 | Apache 2.0 ✅ |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
# 文本转视频
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "A panda eating bamboo in a misty forest, peaceful, high quality",
    "width": 1360, "height": 768,
    "num_frames": 49,
    "num_steps": 50,
    "guidance": 6.0,
    "fps": 8
  }'

# 图片转视频
curl -X POST http://<ECS_IP>:8000/generate/i2v \
  -F 'image=@panda.jpg' \
  -F 'prompt=The panda starts walking through the forest' \
  -F 'num_frames=49'
```

**适合场景**：资源受限、快速原型验证、商用项目入门首选

---

## 文生音效（T2SFX）

> 目录：`sfx-aliyun/`、`mmaudio-aliyun/`

### TangoFlux

**简介**
新加坡管理大学发布的 515M 参数高效音效生成模型，基于 Flow Matching 训练，A40 GPU 仅需 3.7 秒即可生成最长 30 秒、44.1kHz 的音频。CLAP score 达到 0.480，在 prompt 跟随精度和多事件场景（如"鸟鸣+雷声"）上显著优于同类模型。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 515M |
| 最低 VRAM | 6GB |
| 最长时长 | 30 秒 |
| 采样率 | 44.1kHz |
| 推理速度 | 3.7s/30s（A40） |
| 许可证 | 开源（查官方条款） |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "heavy rain falling on a metal roof with distant thunder rolling",
    "duration": 10,
    "steps": 50,
    "cfg_scale": 4.5
  }'
```

**Prompt 技巧**：指定录音视角效果显著，"close-up"和"distant"会产生截然不同的声音

**适合场景**：精确 Foley 音效、环境音设计、视频配音音效

---

### MMAudio

**简介**
UIUC + Sony AI 联合发布的多模态音频生成模型（CVPR 2025），支持视频和/或文本输入生成同步音频。多模态联合训练使其能在 AudioSet、VGGSound 等多种数据集上训练，视频同步精度是所有开源方案中最强的。

**技术参数**
| 项目 | 参数 |
|------|------|
| 输入模态 | 文本 / 视频 / 图像（均可） |
| 最低 VRAM | 约 6GB（16-bit 模式） |
| 默认输出时长 | 8 秒（可调） |
| 输出格式 | .flac |
| 采样率 | 44kHz |
| 许可证 | 开源（非商用） |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
# 文本转音效
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "footsteps on wooden floor in an empty hallway, echo",
    "duration": 8
  }'

# 视频转音效（画面同步）
curl -X POST http://<ECS_IP>:8000/generate/video \
  -F 'video=@input.mp4' \
  -F 'prompt=ambient crowd noise matching the scene' \
  -F 'duration=-1'
```

**适合场景**：视频自动配音、画面同步音效、AI 视频制作流水线

---

## 文生音乐（T2M）

> 目录：`t2m-aliyun/`

### ACE-Step 1.5 XL

**简介**
ACE Studio + StepFun 发布，2026 年 1 月推出，被称为「音乐界的 Stable Diffusion」。XL 版本采用 4B DiT 解码器，混合 LM（规划器）+ Diffusion Transformer（生成器）架构，在 A100 上不到 2 秒生成一首完整歌曲，RTX 3090 上不到 10 秒。内置 FastAPI REST API 服务，支持 50+ 语言歌词、remix/edit/cover 完整工具链。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 4B DiT + LM（0.6B–4B） |
| 最低 VRAM | 12GB（开启 offload） |
| 最长时长 | 240 秒（4 分钟） |
| 采样率 | 44.1kHz 立体声 |
| 歌词支持 | ✅ 50+ 语言 |
| 人声 | ✅ |
| 许可证 | Apache 2.0 ✅ |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
# 纯文本生成
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "upbeat electronic pop, female vocal, synthesizer, 120bpm, energetic",
    "duration": 60
  }'

# 带歌词生成
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "indie folk, acoustic guitar, warm, storytelling",
    "lyrics": "[verse]\nIn the morning light I see your smile\nAcross the miles we walked together\n[chorus]\nRemember when the world was ours\nBeneath the summer stars",
    "duration": 90,
    "num_steps": 8
  }'
```

**适合场景**：完整歌曲生成、带人声的创意音乐、商用音乐内容

---

### YuE 7B

**简介**
M-A-P 发布的 7B 参数 LM-only 自回归架构音乐模型，Apache 2.0 协议。专注于长时歌曲叙事连贯性，能生成最长 5 分钟含人声伴奏的完整歌曲，自动根据歌词语义生成匹配的器乐伴奏，支持多语言歌词和情感表达。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 7B（Stage 1）+ 1B（Stage 2） |
| 最低 VRAM | 24GB |
| 最长时长 | 5 分钟 |
| 人声 | ✅ |
| 生成速度 | L40S 上约 5 分钟/曲 |
| 许可证 | Apache 2.0 ✅ |

**ECS 规格**：推荐 `ecs.gn6v-c8g1.2xlarge`（A10 24G，约 ¥3–5/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "pop, piano, upbeat, female vocal, hopeful",
    "lyrics": "Walking through the city streets\nEvery step a new beginning\nThe sun sets on yesterday\nTomorrow is ours for the taking",
    "num_segments": 3,
    "language": "en"
  }'
```

**适合场景**：长叙事歌曲（3–5 分钟）、歌词驱动音乐创作

---

### MusicGen Stereo Large

**简介**
Meta FAIR 发布的 3.3B 立体声音乐生成模型，单阶段 Transformer 架构，diffusers 原生支持，集成最简单。每次生成约 30 秒，可链式调用生成更长曲目。CC BY-NC 4.0 协议，不可商用，但学术和个人项目生态最成熟。

**技术参数**
| 项目 | 参数 |
|------|------|
| 参数量 | 3.3B |
| 最低 VRAM | 12GB |
| 最长时长 | 30s/次（可链式扩展） |
| 输出格式 | 立体声 WAV（32kHz） |
| 人声 | ❌ |
| 许可证 | CC BY-NC 4.0（非商用） |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "chill lo-fi hip hop beat, vinyl crackle, piano, 80bpm, relaxing",
    "duration": 60,
    "top_k": 250,
    "temperature": 1.0
  }'
```

**适合场景**：背景音乐、Loop 素材、非商用学术研究

---

### Stable Audio Open 1.5

**简介**
Stability AI 发布的音景和采样生成模型，最长支持 6 分钟生成时长，是所有方案中生成时长最长的。训练数据来自 Freesound CC0/CC BY 授权内容，专注于器乐、环境音景和音效采样，不支持人声生成。

**技术参数**
| 项目 | 参数 |
|------|------|
| 架构 | Diffusion Transformer |
| 最低 VRAM | 8GB |
| 最长时长 | 360 秒（6 分钟） |
| 采样率 | 44.1kHz 立体声 |
| 人声 | ❌ |
| 许可证 | Stability AI 社区许可（查条款） |

**ECS 规格**：`ecs.gn6i-c4g1.xlarge`（T4 16G，抢占式约 ¥0.5–0.9/h）

**API 示例**
```bash
curl -X POST http://<ECS_IP>:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "peaceful ambient forest at dawn, birds singing, gentle water stream, morning mist",
    "duration": 120,
    "num_steps": 100,
    "guidance": 7.0
  }'
```

**适合场景**：环境音景、采样库制作、长时背景氛围音效

---

## 各类横向对比

### 文生图（T2I）对比

| 模型 | 参数 | VRAM | 速度 | 质量 | 商用 | 最适场景 |
|------|------|------|------|------|------|---------|
| **FLUX.2 klein** | 4B | 8GB | ⚡⚡⚡⚡ | ⭐⭐⭐⭐ | ✅ Apache | 批量生产，商用首选 |
| **FLUX.1 schnell** | 12B | 16GB | ⚡⚡⚡⚡ | ⭐⭐⭐⭐ | ✅ Apache | 写实图像快速生成 |
| **FLUX.1 dev** | 12B | 24GB | ⚡⚡ | ⭐⭐⭐⭐⭐ | ⚠️ 授权 | 最高质量 |
| **SD 3.5 Large** | 8B | 16GB | ⚡⚡⚡ | ⭐⭐⭐⭐ | ⚠️ 查条款 | 艺术风格/LoRA 定制 |

> **推荐首选**：FLUX.2 klein（8GB VRAM + Apache 2.0 + 4步极速）

---

### 文生视频（T2V）对比

| 模型 | 参数 | VRAM | 速度 | 质量 | 多角色 | 商用 | 最适场景 |
|------|------|------|------|------|--------|------|---------|
| **CogVideoX 1.5** | 5B | 16GB | ⚡⚡⚡ | ⭐⭐⭐ | ❌ | ✅ Apache | 入门，T4 低成本 |
| **HunyuanVideo 1.5** | 8.3B | 14GB | ⚡⚡⚡ | ⭐⭐⭐⭐⭐ | ❌ | ⚠️ 查条款 | 运动物理真实 |
| **Wan 2.2 TI2V-5B** | 5B | 24GB | ⚡⚡⚡ | ⭐⭐⭐⭐ | ❌ | ✅ Apache | 通用多任务 |
| **LTX-2** | 22B | 32GB | ⚡⚡⚡⚡ | ⭐⭐⭐⭐ | ❌ | ✅ Apache | 速度最快+原生音频 |
| **Open-Sora 2.0** | 11B | 24GB | ⚡⚡ | ⭐⭐⭐⭐ | ❌ | ✅ MIT | 可微调私有数据 |
| **SkyReels-A2** | 14B | 80GB | ⚡ | ⭐⭐⭐⭐⭐ | ✅ 多参考图 | ✅ Apache | 多角色一致性 |
| **Wan 2.2 A14B** | 27B MoE | 80GB | ⚡ | ⭐⭐⭐⭐⭐ | ❌ | ✅ Apache | 最高质量 |

> **推荐首选**：CogVideoX 1.5（入门）→ Wan 2.2 TI2V-5B（进阶）→ SkyReels-A2（多角色）

---

### 文生音效（T2SFX）对比

| 模型 | 参数 | VRAM | 速度 | 质量 | 视频同步 | 商用 | 最适场景 |
|------|------|------|------|------|---------|------|---------|
| **TangoFlux** | 515M | 6GB | ⚡⚡⚡⚡⚡ | ⭐⭐⭐⭐⭐ | ❌ | ⚠️ 查条款 | 精确 Foley 音效 |
| **MMAudio** | — | 6GB | ⚡⚡⚡ | ⭐⭐⭐⭐⭐ | ✅ 画面同步 | ❌ NC | 视频同步配音 |

> **推荐组合**：TangoFlux（精确音效）+ MMAudio（视频同步）两者互补

---

### 文生音乐（T2M）对比

| 模型 | 参数 | VRAM | 速度 | 质量 | 人声 | 歌词 | 最长时长 | 商用 | 最适场景 |
|------|------|------|------|------|------|------|---------|------|---------|
| **ACE-Step 1.5 XL** | 4B DiT | 12GB | ⚡⚡⚡⚡⚡ | ⭐⭐⭐⭐⭐ | ✅ | ✅ 50+语言 | 240s | ✅ Apache | 完整歌曲，综合最强 |
| **YuE 7B** | 7B | 24GB | ⚡⚡ | ⭐⭐⭐⭐ | ✅ | ✅ | 300s | ✅ Apache | 长叙事歌曲 |
| **MusicGen Stereo** | 3.3B | 12GB | ⚡⚡⚡⚡ | ⭐⭐⭐⭐ | ❌ | ❌ | 30s/次 | ❌ NC | 背景音乐 |
| **Stable Audio Open** | — | 8GB | ⚡⚡⚡ | ⭐⭐⭐⭐ | ❌ | ❌ | **360s** | ⚠️ 查条款 | 最长环境音景 |

> **推荐首选**：ACE-Step 1.5（Apache 2.0 + 速度最快 + 功能最全）

---

## 成本估算

### 按月调用量（抢占式实例，100 次/月）

| 类别 | 模型 | ECS 规格 | 抢占价 | 每次推理时长 | 月成本 |
|------|------|---------|--------|------------|--------|
| T2I | FLUX.2 klein | T4 gn6i | ¥0.7/h | ~30s | **≈ ¥2** |
| T2I | FLUX.1 dev | A10 gn6v | ¥4/h | ~90s | **≈ ¥17** |
| T2V | CogVideoX 1.5 | T4 gn6i | ¥0.7/h | ~8min | **≈ ¥9** |
| T2V | Wan 2.2 TI2V | T4 gn6i | ¥0.7/h | ~9min | **≈ ¥11** |
| T2V | SkyReels-A2 | A100 gn7e | ¥10/h | ~8min | **≈ ¥133** |
| T2SFX | TangoFlux | T4 gn6i | ¥0.7/h | ~2min | **≈ ¥2** |
| T2SFX | MMAudio | T4 gn6i | ¥0.7/h | ~3min | **≈ ¥4** |
| T2M | ACE-Step 1.5 | T4 gn6i | ¥0.7/h | ~2min | **≈ ¥2** |
| T2M | YuE 7B | A10 gn6v | ¥4/h | ~5min | **≈ ¥33** |

> 以上不含 ECS 系统盘（建议 200GB ESSD，约 ¥40–80/月/盘）和 OSS 存储费用

### 固定成本（无论调用次数）

| 项目 | 月费 |
|------|------|
| ECS 系统盘 100GB ESSD | ¥40 |
| ECS 系统盘 200GB ESSD | ¥80 |
| OSS 存储（10GB） | ¥2 |
| ACR 个人版 | 免费 |
| 函数计算 FC | 免费（100万次额度内） |

---

## 部署规范

### 目录结构（所有方案统一）

```
<model-name>/
├── docker/
│   ├── Dockerfile          # 基础镜像 + 推理依赖
│   └── server.py           # FastAPI 推理服务
├── fc/
│   ├── handler.py          # FC 触发器：启停 ECS + 转发请求
│   └── requirements.txt
├── infra/
│   ├── config.env.example  # 配置模板
│   └── ecs-userdata.sh     # ECS 开机脚本：自动下载模型 + 启动容器
└── deploy.sh               # 一键部署（5步全自动）
```

### 统一 API 规范

所有模型服务均实现以下接口：

```
GET  /health     健康检查，返回 {"status": "ok", "model_loaded": true, ...}
POST /generate   主推理接口，JSON body
POST /generate/video   视频输入（MMAudio）
POST /generate/i2v     图片转视频（T2V 模型）
```

### 环境变量（所有服务通用）

```bash
MODEL_DIR        # 模型权重挂载路径
OSS_ACCESS_KEY   # 阿里云 Access Key ID
OSS_SECRET_KEY   # 阿里云 Access Key Secret
OSS_ENDPOINT     # OSS 内网端点（节省流量费）
OSS_BUCKET       # OSS Bucket 名称
OSS_URL_EXPIRE   # 生成文件链接有效期（秒），默认 3600
```

### FC 触发器超时配置

| 场景 | TIMEOUT_START | TIMEOUT_INFER | FC timeout |
|------|--------------|--------------|-----------|
| 首次（含模型下载） | 1200–2400s | 按模型 | 600s |
| 后续（模型已在磁盘） | 实际 60–120s | 按模型 | 600s |

> `TIMEOUT_START` 设置较大是为了兜住首次下载模型的等待时间，实际等待时间远小于此值

### 安全规范

- AK/SK 通过 FC 环境变量注入，不写入代码或镜像
- ECS 安全组仅开放 8000 端口，来源限制为 FC 所在 VSwitch 网段
- OSS 使用内网端点，避免公网流量费用
- 生成文件 URL 为带签名临时链接，默认 1–2 小时后失效

---

*最后更新：2026 年 7 月*
