# 提示词模板系统迁移计划

## 当前状态

项目中的提示词目前是**硬编码**在服务代码中的字符串拼接和格式化，存在的问题：

1. ❌ 提示词散落在各个服务文件中，难以统一管理
2. ❌ 修改提示词需要重新编译代码
3. ❌ 提示词版本无法追踪
4. ❌ 无法快速切换不同的提示词策略
5. ❌ 团队协作时难以同步提示词修改

## 目标状态

✅ 所有提示词模板化管理在 `config/prompts/` 目录
✅ 提示词修改无需重新编译代码（通过配置文件）
✅ 提示词版本通过 Git 追踪
✅ 支持模板继承和复用
✅ 易于团队协作和维护

## 迁移步骤

### Phase 1: 创建模板系统（已完成）✅

- [x] 创建 `config/prompts/` 目录
- [x] 实现 `TemplateService` 服务
- [x] 创建基础模板文件：
  - [x] `novel_outline.tmpl` - 小说大纲
  - [x] `chapter.tmpl` - 章节生成
  - [x] `character.tmpl` - 角色设计
  - [x] `scene.tmpl` - 场景描写
  - [x] `storyboard.tmpl` - 分镜头脚本
- [x] 编写使用文档和示例

### Phase 2: 迁移现有提示词（进行中）

#### 2.1 迁移 `PromptService` 中的提示词

**文件**: `internal/service/extended_service.go`

| 原始方法 | 对应模板 | 迁移状态 |
|----------|----------|----------|
| `BuildOutlinePrompt` | `novel_outline.tmpl` | ⏳ 待迁移 |
| `BuildChapterPrompt` | `chapter.tmpl` | ⏳ 待迁移 |
| `BuildScenePrompt` | `scene.tmpl` | ⏳ 待迁移 |

#### 2.2 迁移其他服务中的提示词

| 服务 | 原始函数 | 对应模板 | 迁移状态 |
|------|----------|----------|----------|
| `NovelService` | `GenerateOutline` | `novel_outline.tmpl` | ⏳ 待迁移 |
| `ChapterService` | `GenerateChapter` | `chapter.tmpl` | ⏳ 待迁移 |
| `VideoEnhancementService` | `GenerateIntelligentShots` | `storyboard.tmpl` | ⏳ 待迁移 |

### Phase 3: 重构服务代码

每个服务的迁移步骤：

```go
// 1. 在服务结构中添加 TemplateService
type NovelService struct {
	novelRepo   *repository.NovelRepository
	templateSvc *TemplateService  // ← 新增
	aiService   *ai.AIService
}

// 2. 修改构造函数
func NewNovelService(
	novelRepo *repository.NovelRepository,
	templateSvc *TemplateService,  // ← 新增参数
	aiService *ai.AIService,
) *NovelService {
	return &NovelService{
		novelRepo:   novelRepo,
		templateSvc: templateSvc,  // ← 保存引用
		aiService:   aiService,
	}
}

// 3. 替换硬编码的提示词生成逻辑
// ❌ 旧方式
func (s *NovelService) GenerateOutline(req *GenerateOutlineRequest) (*NovelOutline, error) {
	prompt := fmt.Sprintf("请为小说《%s》创作大纲...", req.Title)
	// ... 拼接更多内容
}

// ✅ 新方式
func (s *NovelService) GenerateOutline(req *GenerateOutlineRequest) (*NovelOutline, error) {
	data := &NovelOutlineTemplateData{
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
	// ... 使用 prompt
}
```

### Phase 4: 测试验证

```bash
# 运行模板服务测试
go test ./internal/service -run TestTemplateService

# 运行服务测试
go test ./internal/service -run TestNovelService

# 集成测试
go test ./...
```

### Phase 5: 文档和培训

- [x] 编写使用文档 `PROMPT_TEMPLATES.md`
- [x] 编写迁移指南 `MIGRATION_GUIDE.md`
- [x] 提供代码示例 `template_service_example.go`
- [ ] 为团队提供培训
- [ ] 编写最佳实践文档

## 迁移检查清单

- [ ] 所有硬编码的提示词都已迁移到模板
- [ ] 模板文件已添加到版本控制
- [ ] 服务代码已重构使用模板系统
- [ ] 所有测试通过
- [ ] 文档已更新
- [ ] 代码审查通过
- [ ] 部署到测试环境
- [ ] 监控提示词效果

## 回滚计划

如果迁移后出现问题：

```bash
# 1. 回滚到迁移前的版本
git revert <commit-hash>

# 2. 或者使用分支管理
git checkout -b feature/prompt-migration
git checkout master

# 3. 或者保留模板系统，但允许切换回硬编码
// 在配置文件中添加开关
enable_template_prompts: true
```

## 预期收益

### 开发效率
- ✅ 修改提示词只需编辑 `.tmpl` 文件
- ✅ 无需重新编译即可调整提示词（未来可支持热加载）
- ✅ 提示词版本清晰可追溯

### 代码质量
- ✅ 服务代码更简洁，关注业务逻辑
- ✅ 提示词集中管理，避免重复
- ✅ 易于代码审查和维护

### 团队协作
- ✅ 非开发人员也可以修改提示词
- ✅ 提示词变更可以独立版本控制
- ✅ 支持提示词的 A/B 测试

## 后续优化

1. **支持多语言提示词**
   - 创建 `config/prompts/en/`、`config/prompts/zh/` 子目录
   - 根据用户语言选择对应模板

2. **支持提示词变体**
   - 为同一任务创建多个模板版本
   - 通过配置选择使用哪个版本

3. **支持热加载**
   - 监控模板文件变化
   - 运行时重新加载模板

4. **提示词效果追踪**
   - 记录每次使用的模板版本
   - 分析不同提示词的效果差异

## 联系方式

如有问题，请联系：
- 技术负责人：[姓名]
- 提示词设计：[姓名]
