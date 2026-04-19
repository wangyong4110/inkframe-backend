# 提示词模板使用说明

本目录包含用于 AI 生成任务的各种提示词模板，使用 Go 模板语法编写。

## 模板文件说明

- `novel_outline.tmpl` - 小说大纲生成模板
- `chapter.tmpl` - 章节内容生成模板
- `character.tmpl` - 角色设计模板
- `scene.tmpl` - 场景描写模板
- `storyboard.tmpl` - 分镜头脚本生成模板

## 使用示例

```go
import "github.com/inkframe/inkframe-backend/internal/service"

// 创建模板服务
templateSvc, err := service.NewTemplateService()
if err != nil {
    log.Fatal(err)
}

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

fmt.Println(prompt)

// 渲染章节提示词
chapterData := &service.ChapterTemplateData{
    Novel: service.NovelInfo{
        Title: "我的小说",
        Genre: "玄幻",
    },
    Chapter: service.ChapterInfo{
        ChapterNo: 1,
        Title:     "第一章",
        Summary:   "主角觉醒天赋",
    },
    ChapterNo:  1,
    WordCount:  3000,
    Style:      "热血",
    UserPrompt: "开头要震撼",
    RecentChapters: []service.ChapterInfo{},
}

chapterPrompt, err := templateSvc.RenderChapterPrompt(chapterData)
if err != nil {
    log.Fatal(err)
}

fmt.Println(chapterPrompt)
```

## 添加新模板

1. 在此目录下创建新的 `.tmpl` 文件
2. 使用 Go 模板语法编写内容
3. 在 `internal/service/template_service.go` 中添加对应的渲染方法
4. 在 `internal/service/template_service.go` 中添加对应的数据结构

## Go 模板语法

- `{{.Variable}}` - 输出变量
- `{{range .List}}...{{end}}` - 循环
- `{{if .Condition}}...{{end}}` - 条件判断
- `{{pipeline}}` - 管道操作（如 `{{.Name | upper}}`）

详细语法参考：https://pkg.go.dev/text/template
