# 提示词模板系统 - 快速开始

## ✨ 特性

- 🎨 **模板化管理**：所有提示词统一管理，易于维护
- 🔄 **参数化支持**：支持动态数据注入
- 📦 **嵌入编译**：使用 Go embed，模板编译进二进制
- 🚀 **高性能**：基于 Go text/template，渲染快速
- 📝 **易于协作**：团队可独立修改提示词

## 📁 项目结构

```
inkframe-backend/
├── internal/service/prompts/          # 提示词模板目录
│   ├── novel_outline.tmpl   # 小说大纲生成
│   ├── chapter.tmpl          # 章节内容生成
│   ├── character.tmpl        # 角色设计
│   ├── scene.tmpl            # 场景描写
│   ├── storyboard.tmpl       # 分镜头脚本
│   └── README.md             # 模板说明
├── internal/service/
│   ├── template_service.go           # 模板服务实现
│   ├── template_service_example.go   # 使用示例
│   └── template_service_test.go      # 单元测试
├── examples/
│   └── template_quickstart.go       # 快速开始示例
├── PROMPT_TEMPLATES.md        # 详细使用文档
└── MIGRATION_GUIDE.md         # 迁移指南
```

## 🚀 快速开始

### 1. 初始化模板服务

```go
import "github.com/inkframe/inkframe-backend/internal/service"

templateSvc, err := service.NewTemplateService()
if err != nil {
    log.Fatal(err)
}
```

### 2. 渲染提示词

```go
// 准备数据
data := &service.NovelOutlineTemplateData{
    Title:          "我的小说",
    Genre:          "玄幻",
    Theme:          "成长与冒险",
    Style:          "热血",
    TargetAudience: "青少年",
    ChapterCount:   50,
}

// 渲染提示词
prompt, err := templateSvc.RenderNovelOutlinePrompt(data)
if err != nil {
    log.Fatal(err)
}

fmt.Println(prompt)
```

### 3. 在服务中使用

```go
type NovelService struct {
    novelRepo   *repository.NovelRepository
    templateSvc *TemplateService  // ← 添加模板服务
    aiService   *ai.AIService
}

func NewNovelService(
    novelRepo *repository.NovelRepository,
    templateSvc *TemplateService,  // ← 传入模板服务
    aiService *ai.AIService,
) *NovelService {
    return &NovelService{
        novelRepo:   novelRepo,
        templateSvc: templateSvc,
        aiService:   aiService,
    }
}

func (s *NovelService) GenerateOutline(req *GenerateOutlineRequest) (*NovelOutline, error) {
    // 使用模板渲染提示词
    data := &service.NovelOutlineTemplateData{
        Title:          req.Title,
        Genre:          req.Genre,
        Theme:          req.Theme,
        Style:          req.Style,
        TargetAudience: req.TargetAudience,
        ChapterCount:   req.ChapterCount,
    }

    prompt, err := s.templateSvc.RenderNovelOutlinePrompt(data)
    if err != nil {
        return nil, err
    }

    // 调用 AI 生成
    response, err := s.aiService.Generate(ctx, &ai.GenerateRequest{
        Prompt: prompt,
        Model:  "gpt-4",
    })
    // ...
}
```

## 📖 可用模板

| 模板 | 方法 | 用途 |
|------|------|------|
| novel_outline.tmpl | RenderNovelOutlinePrompt | 生成小说大纲 |
| chapter.tmpl | RenderChapterPrompt | 生成章节内容 |
| character.tmpl | RenderCharacterPrompt | 设计角色 |
| scene.tmpl | RenderScenePrompt | 描写场景 |
| storyboard.tmpl | RenderStoryboardPrompt | 生成分镜头脚本 |

## 🔧 自定义模板

### 1. 创建模板文件

```bash
touch internal/service/prompts/my_template.tmpl
```

### 2. 编写模板内容

```go
// internal/service/prompts/my_template.tmpl
你是{{.Role}}，请完成以下任务：

任务：{{.Task}}

要求：
{{range .Requirements}}
- {{.}}
{{end}}
```

### 3. 添加数据结构和方法

```go
// 在 template_service.go 中添加

type MyTemplateData struct {
    Role         string
    Task         string
    Requirements []string
}

func (s *TemplateService) RenderMyTemplatePrompt(data *MyTemplateData) (string, error) {
    return s.Render("my_template", data)
}
```

### 4. 使用自定义模板

```go
data := &MyTemplateData{
    Role: "故事编辑",
    Task: "审查小说逻辑",
    Requirements: []string{
        "检查剧情漏洞",
        "验证角色一致性",
        "评估节奏感",
    },
}

prompt, err := templateSvc.RenderMyTemplatePrompt(data)
```

## 📝 模板语法

Go 模板基本语法：

```go
{{.Variable}}           // 输出变量
{{range .List}}...{{end}}   // 循环
{{if .Condition}}...{{end}}   // 条件判断
{{.Field | upper}}             // 管道操作
```

详细语法参考：https://pkg.go.dev/text/template

## 🎯 迁移现有提示词

查看 `MIGRATION_GUIDE.md` 了解如何将硬编码的提示词迁移到模板系统。

**迁移示例：**

```go
// ❌ 旧方式（硬编码）
prompt := fmt.Sprintf("请为小说《%s》创作%d章的大纲...", title, count)
prompt += fmt.Sprintf("类型：%s，风格：%s...", genre, style)

// ✅ 新方式（模板）
data := &NovelOutlineTemplateData{
    Title:        title,
    Genre:        genre,
    Style:        style,
    ChapterCount: count,
}

prompt, _ := templateSvc.RenderNovelOutlinePrompt(data)
```

## 🧪 测试

```bash
# 运行模板服务测试
go test ./internal/service -run TestTemplateService -v
```

## 📚 更多文档

- **使用指南**: `PROMPT_TEMPLATES.md`
- **迁移指南**: `MIGRATION_GUIDE.md`
- **代码示例**: `internal/service/template_service_example.go`
- **快速开始**: `examples/template_quickstart.go`
- **模板说明**: `internal/service/prompts/README.md`

## ❓ 常见问题

### Q: 修改模板后需要重新编译吗？
A: 当前版本是的，因为模板使用 embed 嵌入。未来可以支持热加载。

### Q: 如何支持多语言提示词？
A: 创建子目录（如 `internal/service/prompts/en/`, `internal/service/prompts/zh/`），在模板服务中根据语言选择。

### Q: 模板语法和 Jinja2 一样吗？
A: 不完全一样。本项目使用 Go text/template，语法略有不同。参见 Go 官方文档。

### Q: 可以在一个模板中引用另一个模板吗？
A: 可以！使用 `{{template "other_template" .}}` 语法。

## 🤝 贡献

添加新模板时：
1. 在 `internal/service/prompts/` 创建 `.tmpl` 文件
2. 在 `template_service.go` 添加数据结构
3. 在 `template_service.go` 添加渲染方法
4. 添加单元测试
5. 更新文档

## 📄 许可

遵循项目整体许可协议。
