# InkFrame Backend

InkFrame 后端服务 - 智能小说自动生成系统

## 功能特性

- ✨ **智能小说生成**：支持多种题材，自动生成高质量的中长篇小说
- 📚 **世界观管理**：维护完整的世界观设定，确保一致性
- 👥 **角色管理**：角色设定、关系图谱、发展轨迹跟踪
- 🎬 **视频生成**：基于小说内容自动生成视频
- 🤖 **多模型支持**：支持 OpenAI、Claude、Gemini 等多种 AI 模型
- 🎯 **质量控制**：全方位的一致性、质量、逻辑检查系统

## 技术栈

- **语言**：Go 1.21+
- **框架**：Gin Web Framework
- **数据库**：MySQL 8.0+
- **缓存**：Redis
- **向量数据库**：Weaviate / Qdrant
- **文件存储**：MinIO / S3

## 项目结构

```
inkframe-backend/
├── cmd/
│   └── server/          # 应用入口
├── internal/
│   ├── api/            # API 层
│   │   ├── handler/     # 处理器
│   │   └── middleware/  # 中间件
│   ├── config/          # 配置
│   ├── model/           # 数据模型
│   ├── repository/      # 仓库层
│   └── service/         # 服务层
├── pkg/                 # 公共包
├── deployments/         # 部署文件
├── docs/               # 文档
├── scripts/            # 脚本
└── web/               # 前端（可选）
```

## 快速开始

### 前置要求

- Go 1.21+
- MySQL 8.0+
- Redis
- Make

### 安装

1. 克隆项目
```bash
git clone https://gitee.com/wangyong1024/inkframe-backend.git
cd inkframe-backend
```

2. 配置
```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml 填入数据库等信息
```

3. 安装依赖
```bash
make deps
```

4. 运行
```bash
make run
```

### 使用 Docker

```bash
# 构建镜像
make docker-build

# 运行容器
make docker-run
```

## API 文档

### 健康检查
```
GET /health
```

### 小说管理
```
GET    /api/v1/novels           # 获取小说列表
POST   /api/v1/novels           # 创建小说
GET    /api/v1/novels/:id       # 获取小说详情
PUT    /api/v1/novels/:id       # 更新小说
DELETE /api/v1/novels/:id       # 删除小说
POST   /api/v1/novels/:id/outline # 生成大纲
```

### 章节管理
```
GET    /api/v1/novels/:novel_id/chapters      # 获取章节列表
POST   /api/v1/novels/:novel_id/chapters      # 生成章节
```

### 模型管理
```
GET    /api/v1/models                          # 获取模型列表
POST   /api/v1/model/select                    # 选择模型
```

### 视频管理
```
POST   /api/v1/videos                          # 创建视频
POST   /api/v1/videos/:id/storyboard          # 生成分镜
```

## 开发

### 代码格式
```bash
make fmt
```

### 代码检查
```bash
make vet
make lint
```

### 运行测试
```bash
make test
```

## 部署

### Docker Compose

```yaml
services:
  backend:
    build: .
    ports:
      - "8080:8080"
    depends_on:
      - mysql
      - redis
```

### Kubernetes

参考 `deployments/kubernetes/` 目录

## 文档

详细技术文档请参考 `docs/` 目录：

- [TECHNICAL_DESIGN.md](./docs/TECHNICAL_DESIGN.md) - 主技术文档
- [QUALITY_CONTROL.md](./docs/QUALITY_CONTROL.md) - 质量控制系统
- [VIDEO_GENERATION.md](./docs/VIDEO_GENERATION.md) - 视频生成系统
- [MULTI_MODEL_MANAGEMENT.md](./docs/MULTI_MODEL_MANAGEMENT.md) - 多模型管理系统
- [PRODUCT_DOCUMENTATION.md](./docs/PRODUCT_DOCUMENTATION.md) - 产品文档

## 贡献

欢迎提交 Issue 和 Pull Request！

## 许可证

MIT License - 详见 LICENSE 文件

---

**InkFrame** - 让每个人都能创作属于自己的故事 📚✨
