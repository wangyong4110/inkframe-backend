# Jinja2 模板提示词管理系统使用指南

## 概述

本系统使用 Go 标准库 `text/template` 替代 Python Jinja2，实现提示词模板化管理。所有提示词模板位于 `config/prompts/` 目录下。

## 快速开始

### 1. 创建模板服务

```go
import "github.com/inkframe/inkframe-backend/internal/service"

// 初始化模板服务
templateSvc, err := service.NewTemplateService()
if err != nil {
    log.Fatal(err)
}
```

### 2. 渲染提示词

```go
// 渲染小说大纲提示词
data := &service.NovelOutlineTemplateData{
    Title:          "我的小说",
    Genre:          "玄幻",
    Theme:          "成长与冒险",
    Style:          "热血",
    TargetAudience: "青少年",
    ChapterCount:   50,
}

prompt, err := templateSvc.RenderNovelOutlinePrompt(data)
if err != nil {
    log.Fatal(err)
}

// 使用 prompt 调用 AI
response, err := aiService.Generate(ctx, &ai.GenerateRequest{
    Prompt: prompt,
    Model:  "gpt-4",
})
```

## 迁移现有提示词

### 步骤 1: 识别硬编码的提示词

在现有代码中查找字符串拼接或格式化的提示词：

```go
// ❌ 旧的硬编码方式
prompt := fmt.Sprintf("请为小说《%s》创作大纲...", novel.Title)
prompt += fmt.Sprintf("类型：%s，章节数：%d...", novel.Genre, chapterCount)
```

### 步骤 2: 创建或选择模板

在 `config/prompts/` 中创建对应的 `.tmpl` 文件：

```go
// ✅ 新的模板方式（config/prompts/novel_outline.tmpl）
你是一位专业的小说作家和策划，擅长创作精彩的小说大纲。

# 任务
请为一部{{.Genre}}类型小说创作详细大纲。

# 基本信息
- 小说标题：{{.Title}}
- 主题：{{.Theme}}
- 预期章节数：{{.ChapterCount}}章
```

### 步骤 3: 定义模板数据结构

```go
// 在 template_service.go 中定义
type NovelOutlineTemplateData struct {
    Title          string
    Genre          string
    Theme          string
    Style          string
    TargetAudience string
    ChapterCount   int
}
```

### 步骤 4: 添加渲染方法

```go
func (s *TemplateService) RenderNovelOutlinePrompt(data *NovelOutlineTemplateData) (string, error) {
    return s.Render("novel_outline", data)
}
```

### 步骤 5: 使用模板替换硬编码

```go
// ✅ 使用模板系统
data := &NovelOutlineTemplateData{
    Title:        novel.Title,
    Genre:        novel.Genre,
    Theme:        novel.Theme,
    Style:        novel.Style,
    TargetAudience: novel.TargetAudience,
    ChapterCount: chapterCount,
}

prompt, err := s.templateSvc.RenderNovelOutlinePrompt(data)
if err != nil {
    return nil, err
}
```

## 已有提示词迁移对照表

| 服务 | 原始函数 | 模板文件 | 新方法 |
|------|----------|----------|--------|
| NovelService | BuildOutlinePrompt | novel_outline.tmpl | RenderNovelOutlinePrompt |
| ChapterService | BuildChapterPrompt | chapter.tmpl | RenderChapterPrompt |
| CharacterService | BuildCharacterPrompt | character.tmpl | RenderCharacterPrompt |
| PromptService | BuildScenePrompt | scene.tmpl | RenderScenePrompt |
| VideoEnhancementService | BuildStoryboardPrompt | storyboard.tmpl | RenderStoryboardPrompt |

## 添加新模板

### 1. 创建模板文件

```bash
# 在 config/prompts/ 目录下创建
touch config/prompts/my_template.tmpl
```

### 2. 编写模板内容

```go
// config/prompts/my_template.tmpl
你是{{.Role}}，请完成以下任务：

任务：{{.Task}}

要求：
{{range .Requirements}}
- {{.}}
{{end}}
```

### 3. 添加数据结构

```go
// 在 template_service.go 中添加
type MyTemplateData struct {
    Role         string
    Task         string
    Requirements []string
}
```

### 4. 添加渲染方法

```go
func (s *TemplateService) RenderMyTemplatePrompt(data *MyTemplateData) (string, error) {
    return s.Render("my_template", data)
}
```

## 高级功能

### 条件渲染

```go
{{if .UserPrompt}}
# 创作要求（用户指定）
{{.UserPrompt}}
{{end}}
```

### 循环渲染

```go
# 前情提要
{{range .RecentChapters}}
第{{.ChapterNo}}章「{{.Title}}」：{{.Summary}}
{{end}}
```

### 管道操作

```go
{{.Title | upper}}  // 转大写
{{.WordCount | printf "%d字"}}  // 格式化
```

## 最佳实践

1. **保持模板简洁**：模板只负责格式化，逻辑在代码中处理
2. **使用结构化数据**：定义清晰的数据结构传递给模板
3. **分离关注点**：提示词内容在模板中，业务逻辑在代码中
4. **版本控制**：所有模板都在 Git 中管理，便于追踪变化
5. **易于维护**：修改提示词只需编辑 `.tmpl` 文件，无需重新编译

## 注意事项

1. 模板文件使用 Go 模板语法，不是 Jinja2
2. 模板在编译时嵌入到二进制文件中（使用 `//go:embed`）
3. 修改模板后需要重新编译程序
4. 变量名必须使用大写字母开头（导出字段）

## 相关文档

- Go 模板语法：https://pkg.go.dev/text/template
- 模板服务实现：`internal/service/template_service.go`
- 使用示例：`internal/service/template_service_example.go`
