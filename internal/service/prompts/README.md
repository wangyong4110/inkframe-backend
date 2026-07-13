# 提示词模板使用说明

本目录包含用于 AI 生成任务的各种提示词模板，使用 [pongo2](https://github.com/flosch/pongo2)（Jinja2/Django 模板语法的 Go 实现）编写，通过 `//go:embed` 打包进二进制。

## 使用方式

调用方在 `internal/service` 包内直接调用 `renderPrompt`（`template_service.go`），传入模板名（不含 `.j2` 后缀）和上下文变量：

```go
prompt, err := renderPrompt("chapter_from_outline", map[string]interface{}{
    "NovelTitle": novel.Title,
    "ChapterNo":  chapter.ChapterNo,
    // ...
})
```

只需要原始文本（不做变量替换）时用 `LoadRawPrompt(name)`。

## 添加新模板

1. 在此目录下新建 `xxx.j2` 文件，使用 pongo2 语法编写
2. 在调用点直接 `renderPrompt("xxx", ctx)`，无需在 `template_service.go` 里额外注册
3. `fragments/` 目录下是被多个模板 `{% include "fragments/xxx.j2" %}` 复用的公共片段

## pongo2 语法速查

- `{{ Variable }}` - 输出变量
- `{% for x in List %}...{% endfor %}` - 循环
- `{% if Condition %}...{% elif %}...{% else %}...{% endif %}` - 条件判断
- `{{ List|join:", " }}` - 过滤器

详细语法参考：https://github.com/flosch/pongo2
