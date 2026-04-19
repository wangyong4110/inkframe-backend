# InkFrame - 智能小说自动生成系统

## 📋 项目概述

InkFrame 是一个基于 AI 的智能小说自动生成系统，能够根据用户需求生成中长篇小说，通过结构化的世界观管理、角色跟踪、知识库引用等技术手段，确保作品的世界观、角色和剧情的一致性。

### 核心特性

- ✨ **智能小说生成**：支持多种题材，自动生成高质量的中长篇小说
- 📚 **世界观圣经管理**：维护完整的世界观设定，确保一致性
- 👥 **角色全生命周期管理**：角色设定、关系图谱、发展轨迹跟踪
- 🔗 **故事连续性保证**：章节关联、剧情追踪、伏笔回收
- 🌐 **参考小说爬取**：爬取热门小说作为风格参考和训练素材
- 🎭 **多类型支持**：玄幻、都市、科幻、言情、悬疑等主流类型
- 🚀 **高性能架构**：Go 后端 + 现代化前端，支持高并发
- 🔍 **智能质量控制**：全方位的一致性、质量、逻辑检查系统
- 🤝 **人机协作**：审核工作流、版本控制、自动修复建议

---

## 🏗️ 系统架构

### 整体架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                           Frontend (Vue 3 + Nuxt 3)              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ 小说编辑器   │  │ 角色管理     │  │ 世界观管理   │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
                              ▲
                              │ REST API / WebSocket
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                        Backend (Go + Gin)                       │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │                   API Gateway Layer                       │  │
│  └──────────────────────────────────────────────────────────┘  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ Novel Service│  │  Character   │  │ Worldview    │          │
│  │              │  │   Service    │  │   Service    │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │  Generation  │  │  Knowledge   │  │   Crawler    │          │
│  │   Service    │  │   Service    │  │   Service    │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
         ▲               ▲               ▲               ▲
         │               │               │               │
         ▼               ▼               ▼               ▼
┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│   MySQL     │  │   Redis     │  │  Vector DB  │  │   MinIO     │
│  (主数据库)  │  │  (缓存)     │  │  (Weaviate) │  │ (文件存储)  │
└─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘
                              │
                              ▼
                    ┌─────────────────┐
                    │  AI Provider    │
                    │  (OpenAI/Claude)│
                    └─────────────────┘
```

---

## 🗄️ 数据库设计

### 核心数据模型

#### 1. 小说 (Novel)

```go
type Novel struct {
    ID          uint      `json:"id" gorm:"primaryKey"`
    UUID        string    `json:"uuid" gorm:"uniqueIndex"`
    Title       string    `json:"title"`
    Description string    `json:"description"`
    Genre       string    `json:"genre"` // 玄幻、都市、科幻等
    Status      string    `json:"status"` // planning, writing, completed
    AuthorID    uint      `json:"author_id"`
    WorldviewID uint      `json:"worldview_id"`
    TotalWords  int       `json:"total_words"`
    ChapterCount int      `json:"chapter_count"`

    // AI 生成参数
    AIModel      string `json:"ai_model"`
    Temperature  float64 `json:"temperature"`
    MaxTokens    int    `json:"max_tokens"`
    StylePrompt  string `json:"style_prompt"`

    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

#### 2. 章节与剧情点 (Chapter & PlotPoint)

```go
type Chapter struct {
    ID        uint   `json:"id" gorm:"primaryKey"`
    NovelID   uint   `json:"novel_id"`
    ChapterNo int    `json:"chapter_no"`
    Title     string `json:"title"`
    Content   string `json:"content" gorm:"type:text"`
    Summary   string `json:"summary"` // AI 生成的章节摘要
    WordCount int    `json:"word_count"`
    Status    string `json:"status"` // draft, generating, completed

    // 剧情追踪
    PlotPoints      []PlotPoint `json:"plot_points" gorm:"foreignKey:ChapterID"`
    PreviousChapter uint        `json:"previous_chapter"`
    NextChapter     uint        `json:"next_chapter"`

    GeneratedAt time.Time `json:"generated_at"`
}

type PlotPoint struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    ChapterID   uint   `json:"chapter_id"`
    Type        string `json:"type"` // conflict, climax, resolution, twist
    Description string `json:"description"`
    Characters  string `json:"characters"` // JSON array
    Locations   string `json:"locations"`  // JSON array
    IsResolved  bool   `json:"is_resolved"`
    ResolvedIn  uint   `json:"resolved_in"` // 解决这一剧情点的章节 ID
}
```

#### 3. 世界观圣经 (Worldview)

```go
type Worldview struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    Name        string `json:"name"`
    Description string `json:"description"`
    Genre       string `json:"genre"`

    // 世界观元素
    MagicSystem  string `json:"magic_system" gorm:"type:text"`
    Geography    string `json:"geography" gorm:"type:text"`
    History      string `json:"history" gorm:"type:text"`
    Culture      string `json:"culture" gorm:"type:text"`
    Technology   string `json:"technology" gorm:"type:text"`

    // 约束与规则
    Rules        string `json:"rules" gorm:"type:text"` // JSON

    CreatedAt    time.Time `json:"created_at"`
    UpdatedAt    time.Time `json:"updated_at"`
}

// 世界观下的具体实体
type WorldviewEntity struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    WorldviewID uint   `json:"worldview_id"`
    Type        string `json:"type"` // location, organization, artifact, race
    Name        string `json:"name"`
    Description string `json:"description" gorm:"type:text"`
    Attributes  string `json:"attributes" gorm:"type:text"` // JSON
    Relations   string `json:"relations" gorm:"type:text"`  // JSON
}
```

#### 4. 角色 (Character)

```go
type Character struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    WorldviewID uint   `json:"worldview_id"`

    Name        string `json:"name"`
    Role        string `json:"role"` // protagonist, antagonist, supporting
    Archetype   string `json:"archetype"` // 英雄、智者、恶棍等原型

    // 外貌与性格
    Appearance  string `json:"appearance" gorm:"type:text"`
    Personality string `json:"personality" gorm:"type:text"`
    Background  string `json:"background" gorm:"type:text"`

    // 能力与属性
    Abilities   string `json:"abilities" gorm:"type:text"` // JSON
    Attributes  string `json:"attributes" gorm:"type:text"` // JSON

    // 角色关系
    Relations   string `json:"relations" gorm:"type:text"` // JSON: [{character_id, type, description}]

    // 角色弧光
    CharacterArc string `json:"character_arc" gorm:"type:text"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// 角色出现记录
type CharacterAppearance struct {
    ID         uint      `json:"id" gorm:"primaryKey"`
    CharacterID uint     `json:"character_id"`
    ChapterID   uint     `json:"chapter_id"`
    RoleInChapter string `json:"role_in_chapter"` // main, supporting, mentioned
    Action     string    `json:"action" gorm:"type:text"`
    Change     string    `json:"change" gorm:"type:text"` // 角色在本章的变化
}
```

#### 5. 参考小说 (ReferenceNovel)

```go
type ReferenceNovel struct {
    ID          uint      `json:"id" gorm:"primaryKey"`
    Title       string    `json:"title"`
    Author      string    `json:"author"`
    SourceURL   string    `json:"source_url"`
    SourceSite  string    `json:"source_site"` // 起点、晋江、纵横等
    Genre       string    `json:"genre"`

    // 爬取信息
    TotalChapters int     `json:"total_chapters"`
    TotalWords    int     `json:"total_words"`
    Status        string  `json:"status"` // crawling, completed, failed

    // 分析结果
    StyleAnalysis string `json:"style_analysis" gorm:"type:text"`
    Keywords      string `json:"keywords" gorm:"type:text"` // JSON
    SimilarNovels string `json:"similar_novels" gorm:"type:text"` // JSON

    CrawledAt     time.Time `json:"crawled_at"`
    CreatedAt     time.Time `json:"created_at"`
}

// 参考小说的章节内容
type ReferenceChapter struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    ChapterNo   int    `json:"chapter_no"`
    Title       string `json:"title"`
    Content     string `json:"content" gorm:"type:text"`
    Summary     string `json:"summary"`
}
```

#### 6. 知识库 (KnowledgeBase)

```go
type KnowledgeBase struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    Type        string `json:"type"` // character_fact, lore, writing_technique
    Title       string `json:"title"`
    Content     string `json:"content" gorm:"type:text"`
    Tags        string `json:"tags" gorm:"type:text"` // JSON
    NovelID     *uint  `json:"novel_id,omitempty"` // 关联小说（可选）
    ReferenceID *uint  `json:"reference_id,omitempty"` // 关联参考小说（可选）

    // 向量化信息
    VectorID    string `json:"vector_id"` // 向量数据库中的 ID

    CreatedAt   time.Time `json:"created_at"`
}
```

#### 7. 提示词模板 (PromptTemplate)

```go
type PromptTemplate struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    Name        string `json:"name"`
    Genre       string `json:"genre"`
    Stage       string `json:"stage"` // outline, chapter, dialogue, description

    // 模板内容（支持变量占位符）
    Template    string `json:"template" gorm:"type:text"`

    // AI 参数
    SystemPrompt string `json:"system_prompt" gorm:"type:text"`
    Temperature  float64 `json:"temperature"`
    MaxTokens    int    `json:"max_tokens"`

    IsDefault   bool   `json:"is_default"`
    CreatedAt   time.Time `json:"created_at"`
}
```

---

## 🔧 核心模块设计

### 1. 小说生成引擎 (Generation Engine)

#### 1.1 生成流程

```
用户请求 → 需求分析 → 大纲生成 → 角色构建 → 世界观填充 → 章节生成 → 连续性检查 → 输出
```

#### 1.2 核心服务

**NovelGenerationService**

```go
type NovelGenerationService struct {
    aiClient         AIProvider
    knowledgeRepo    KnowledgeRepository
    characterRepo    CharacterRepository
    worldviewRepo    WorldviewRepository
    vectorStore      VectorStore
    cache            *redis.Client
}

// 生成小说大纲
func (s *NovelGenerationService) GenerateOutline(req *GenerateOutlineRequest) (*NovelOutline, error) {
    // 1. 从知识库检索相关参考
    references := s.vectorStore.Search(req.Genre, req.Keywords, 10)

    // 2. 构建提示词
    prompt := s.buildOutlinePrompt(req, references)

    // 3. 调用 AI 生成
    outline, err := s.aiClient.Generate(prompt)
    if err != nil {
        return nil, err
    }

    // 4. 解析并验证大纲
    validated := s.validateOutline(outline)

    return validated, nil
}

// 生成单个章节
func (s *NovelGenerationService) GenerateChapter(req *GenerateChapterRequest) (*Chapter, error) {
    // 1. 获取上下文：前 N 章内容、角色状态、世界观设定
    context := s.gatherContext(req.NovelID, req.ChapterNo, 3)

    // 2. 检索相关参考素材
    references := s.vectorStore.SearchByStyle(req.NovelID, req.SceneType, 5)

    // 3. 检查连续性：验证剧情延续性、角色一致性
    continuityCheck := s.checkContinuity(context, req.Outline)

    // 4. 构建提示词
    prompt := s.buildChapterPrompt(req, context, references, continuityCheck)

    // 5. 分段生成（长文本分段策略）
    content := s.generateContentInSegments(prompt, req.TargetWordCount)

    // 6. 后处理：提取关键信息更新知识库
    s.extractAndStoreKnowledge(req.NovelID, req.ChapterNo, content)

    return &Chapter{
        Content:   content,
        Summary:   s.generateSummary(content),
        WordCount: len(strings.Split(content, "")),
    }, nil
}
```

#### 1.3 上下文管理

```go
type ContextManager struct {
    cache         *redis.Client
    db            *gorm.DB
    maxContextLen int
}

// 收集生成上下文
func (m *ContextManager) GatherContext(novelID uint, chapterNo int, historyDepth int) *GenerationContext {
    ctx := &GenerationContext{}

    // 1. 获取前 N 章内容
    ctx.PreviousChapters = m.getRecentChapters(novelID, chapterNo, historyDepth)

    // 2. 获取活跃角色及其状态
    ctx.ActiveCharacters = m.getActiveCharacters(novelID, chapterNo)

    // 3. 获取世界观设定
    ctx.Worldview = m.getWorldview(novelID)

    // 4. 获取未解决的剧情点
    ctx.PendingPlotPoints = m.getPendingPlotPoints(novelID, chapterNo)

    // 5. 压缩上下文（移除冗余信息）
    m.compressContext(ctx, m.maxContextLen)

    return ctx
}
```

#### 1.4 连续性检查

```go
type ContinuityChecker struct {
    characterRepo CharacterRepository
    worldviewRepo WorldviewRepository
    aiClient      AIProvider
}

// 检查连续性
func (c *ContinuityChecker) Check(ctx *GenerationContext, outline *ChapterOutline) *ContinuityReport {
    report := &ContinuityReport{}

    // 1. 角色一致性检查
    report.CharacterIssues = c.checkCharacterConsistency(ctx, outline)

    // 2. 世界观一致性检查
    report.WorldviewIssues = c.checkWorldviewConsistency(ctx, outline)

    // 3. 剧情连续性检查
    report.PlotIssues = c.checkPlotContinuity(ctx, outline)

    // 4. 生成修复建议
    if report.HasIssues() {
        report.FixSuggestions = c.generateFixSuggestions(report)
    }

    return report
}
```

---

### 2. 角色管理系统 (Character Management)

#### 2.1 角色构建流程

```go
type CharacterService struct {
    db         *gorm.DB
    aiClient   AIProvider
    vectorStore VectorStore
}

// 根据角色描述生成完整角色档案
func (s *CharacterService) BuildCharacterProfile(novelID uint, description string) (*Character, error) {
    // 1. 从参考小说中检索相似角色
    similar := s.vectorStore.SearchSimilarCharacters(description, 5)

    // 2. 生成角色档案
    profile := s.aiClient.GenerateProfile(description, similar)

    // 3. 验证角色与世界观的一致性
    s.validateConsistency(novelID, profile)

    // 4. 保存角色
    character := &Character{
        NovelID:     novelID,
        Name:        profile.Name,
        Role:        profile.Role,
        Appearance:  profile.Appearance,
        Personality: profile.Personality,
        Background:  profile.Background,
        Abilities:   profile.Abilities,
    }

    s.db.Create(character)

    // 5. 向量化角色信息
    s.vectorizeCharacter(character)

    return character, nil
}

// 更新角色状态
func (s *CharacterService) UpdateCharacterState(characterID uint, chapterID uint, changes string) error {
    appearance := &CharacterAppearance{
        CharacterID:   characterID,
        ChapterID:     chapterID,
        Action:        changes,
    }

    s.db.Create(appearance)

    // 触发角色弧光更新
    s.updateCharacterArc(characterID)

    return nil
}
```

---

### 3. 世界观管理系统 (Worldview Management)

#### 3.1 世界观构建

```go
type WorldviewService struct {
    db         *gorm.DB
    aiClient   AIProvider
    vectorStore VectorStore
}

// 生成世界观设定
func (s *WorldviewService) GenerateWorldview(genre string, hints []string) (*Worldview, error) {
    // 1. 检索同类型世界观参考
    references := s.vectorStore.SearchWorldviews(genre, 10)

    // 2. 生成世界观
    worldview := s.aiClient.GenerateWorldview(genre, hints, references)

    // 3. 保存
    saved := &Worldview{
        Name:       worldview.Name,
        Genre:      genre,
        MagicSystem: worldview.MagicSystem,
        Geography:  worldview.Geography,
        History:    worldview.History,
        Culture:    worldview.Culture,
        Technology: worldview.Technology,
        Rules:      worldview.Rules,
    }

    s.db.Create(saved)

    // 4. 生成实体（地点、组织、种族等）
    s.generateEntities(saved)

    return saved, nil
}

// 约束检查
func (s *WorldviewService) ValidateConstraint(novelID uint, content string) []ConstraintViolation {
    worldview := s.getWorldviewForNovel(novelID)

    // 使用 AI 检查内容是否符合世界观约束
    violations := s.aiClient.CheckConstraints(content, worldview)

    return violations
}
```

---

### 4. 参考小说爬取与分析 (Crawler & Analysis)

#### 4.1 爬虫架构

```go
type CrawlerService struct {
    db           *gorm.DB
    httpClient   *http.Client
    analyzers    map[string]NovelAnalyzer // 站点特定的分析器
}

type NovelAnalyzer interface {
    ParseNovelList(html string) []*NovelInfo
    ParseChapter(html string) *ChapterContent
    ExtractMetadata(html string) *NovelMetadata
}

// 爬取小说
func (s *CrawlerService) CrawlNovel(url string) (*ReferenceNovel, error) {
    // 1. 识别站点
    site := s.identifySite(url)

    // 2. 使用对应的分析器
    analyzer := s.analyzers[site]

    // 3. 获取小说元数据
    metadata := analyzer.ExtractMetadata(s.fetch(url))

    // 4. 创建记录
    novel := &ReferenceNovel{
        Title:       metadata.Title,
        Author:      metadata.Author,
        SourceURL:   url,
        SourceSite:  site,
        Genre:       metadata.Genre,
        Status:      "crawling",
    }
    s.db.Create(novel)

    // 5. 异步爬取章节
    go s.crawlChapters(novel, analyzer)

    return novel, nil
}

// 爬取章节（异步）
func (s *CrawlerService) crawlChapters(novel *ReferenceNovel, analyzer NovelAnalyzer) {
    chapterList := analyzer.ParseChapterList(s.fetch(novel.SourceURL))

    for _, chapterURL := range chapterList {
        content := analyzer.ParseChapter(s.fetch(chapterURL))

        s.db.Create(&ReferenceChapter{
            NovelID:   novel.ID,
            ChapterNo: content.ChapterNo,
            Title:     content.Title,
            Content:   content.Body,
        })

        // 避免被封，添加延时
        time.Sleep(2 * time.Second)
    }

    // 更新状态
    novel.Status = "completed"
    s.db.Save(novel)

    // 触发分析任务
    s.analyzeNovel(novel.ID)
}
```

#### 4.2 风格分析

```go
type AnalysisService struct {
    db         *gorm.DB
    aiClient   AIProvider
    vectorStore VectorStore
}

// 分析小说风格
func (s *AnalysisService) AnalyzeNovelStyle(novelID uint) error {
    novel := s.getReferenceNovel(novelID)
    chapters := s.getChapters(novelID)

    // 1. 采样分析（避免处理全部内容）
    sample := s.sampleChapters(chapters, 10)

    // 2. 使用 AI 分析风格
    analysis := s.aiClient.AnalyzeStyle(sample)

    // 3. 保存分析结果
    novel.StyleAnalysis = analysis.JSON()
    novel.Keywords = analysis.Keywords
    s.db.Save(novel)

    // 4. 向量化并存储
    s.vectorizeNovel(novel, sample)

    return nil
}

// 向量化小说内容
func (s *AnalysisService) vectorizeNovel(novel *ReferenceNovel, chapters []ReferenceChapter) {
    for _, chapter := range chapters {
        // 分块向量化（避免超长文本）
        chunks := s.chunkText(chapter.Content, 500)

        for _, chunk := range chunks {
            vector := s.aiClient.Embed(chunk)
            s.vectorStore.Store(vector, &KnowledgeBase{
                Type:    "reference_content",
                Title:   fmt.Sprintf("%s - %s", novel.Title, chapter.Title),
                Content: chunk,
                Tags:    []string{novel.Genre, novel.Title},
                ReferenceID: &novel.ID,
            })
        }
    }
}
```

---

### 5. 知识库与向量检索 (Knowledge Base & Vector Search)

#### 5.1 向量数据库选择

推荐使用 **Weaviate** 或 **Qdrant**（支持 Docker 部署）

**Weaviate 集成：**

```go
type VectorStore struct {
    client *weaviate.Client
}

// 存储向量
func (v *VectorStore) Store(vector []float32, kb *KnowledgeBase) error {
    obj := &models.Object{
        Class: "KnowledgeBase",
        Properties: map[string]string{
            "type":        kb.Type,
            "title":       kb.Title,
            "content":     kb.Content,
            "tags":        strings.Join(kb.Tags, ","),
        },
        Vector: vector,
    }

    res, err := v.client.Data().Creator().
        WithObject(obj).
        Do(context.Background())

    if err != nil {
        return err
    }

    // 保存向量 ID
    kb.VectorID = res[0].ID
    return nil
}

// 相似度搜索
func (v *VectorStore) Search(query string, limit int, filters map[string]string) []KnowledgeBase {
    // 1. 向量化查询
    queryVector := v.embed(query)

    // 2. 搜索
    res := v.client.GraphQL().
        Get().
        WithClassName("KnowledgeBase").
        WithNearVector(v.client.GraphQL().NearVectorArgBuilder().
            WithVector(queryVector)).
        WithLimit(limit).
        Do(context.Background())

    // 3. 解析结果
    return v.parseResults(res)
}

// 按风格搜索（用于生成参考）
func (v *VectorStore) SearchByStyle(novelID uint, sceneType string, limit int) []KnowledgeBase {
    novel := v.getNovel(novelID)

    // 构建风格查询向量
    styleQuery := fmt.Sprintf("小说类型:%s 场景类型:%s", novel.Genre, sceneType)
    return v.Search(styleQuery, limit, map[string]string{"type": "reference_content"})
}
```

---

### 6. 提示词管理系统 (Prompt Management)

#### 6.1 提示词模板引擎

```go
type PromptEngine struct {
    db       *gorm.DB
    cache    *redis.Client
    renderer *template.Template
}

// 渲染提示词
func (e *PromptEngine) Render(templateID uint, variables map[string]interface{}) (string, error) {
    // 1. 从缓存获取
    cacheKey := fmt.Sprintf("prompt:%d", templateID)
    if cached, err := e.cache.Get(cacheKey).Result(); err == nil {
        return e.renderTemplate(cached, variables)
    }

    // 2. 从数据库获取
    tmpl := e.getTemplate(templateID)

    // 3. 渲染
    rendered, err := e.renderTemplate(tmpl.Template, variables)
    if err != nil {
        return "", err
    }

    // 4. 缓存
    e.cache.Set(cacheKey, tmpl.Template, 1*time.Hour)

    return rendered, nil
}

// 构建章节生成提示词
func (e *PromptEngine) BuildChapterPrompt(ctx *GenerationContext, references []KnowledgeBase) string {
    variables := map[string]interface{}{
        "NovelTitle":        ctx.Novel.Title,
        "ChapterOutline":    ctx.Outline,
        "PreviousChapters":  ctx.PreviousChapters,
        "ActiveCharacters":  ctx.ActiveCharacters,
        "WorldviewRules":    ctx.Worldview.Rules,
        "PendingPlotPoints": ctx.PendingPlotPoints,
        "ReferenceStyles":   references,
    }

    prompt, _ := e.Render(ctx.PromptTemplateID, variables)
    return prompt
}
```

---

## 🎨 前端技术方案

### 技术栈选择

**Vue 3 + Nuxt 3** + **Tailwind CSS** + **Shadcn-vue**

选择理由：
- **Nuxt 3**: 现代化 Vue 全栈框架，SSR/SSG 支持，优秀的 SEO
- **Shadcn-vue**: 基于 Radix UI 的组件库，可定制性强，设计美观
- **Pinia**: 状态管理
- **VueUse**: 实用工具库

### 前端架构

```
┌─────────────────────────────────────────────────────────┐
│                      Nuxt 3 App                          │
│  ┌─────────────────────────────────────────────────┐    │
│  │              Pages (页面路由)                     │    │
│  │  /novel/[id]       /character/[id]               │    │
│  └─────────────────────────────────────────────────┘    │
│  ┌─────────────────────────────────────────────────┐    │
│  │           Components (可复用组件)                 │    │
│  │  NovelEditor      CharacterCard  WorldviewTree   │    │
│  └─────────────────────────────────────────────────┘    │
│  ┌─────────────────────────────────────────────────┐    │
│  │           Composables (组合式函数)                │    │
│  │  useNovel          useCharacter    useAI          │    │
│  └─────────────────────────────────────────────────┘    │
│  ┌─────────────────────────────────────────────────┐    │
│  │              Stores (Pinia)                       │    │
│  │  novelStore      characterStore  uiStore         │    │
│  └─────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────┘
```

### 核心页面设计

#### 1. 小说创作工作台

```vue
<template>
  <div class="novel-workspace">
    <!-- 左侧：章节列表 -->
    <ChapterList
      :chapters="chapters"
      @select="handleChapterSelect"
    />

    <!-- 中间：编辑器 -->
    <NovelEditor
      :chapter="currentChapter"
      :readonly="isGenerating"
      @save="handleSave"
      @generate="handleGenerate"
    />

    <!-- 右侧：角色与世界观面板 -->
    <div class="sidebar">
      <CharacterList :characters="activeCharacters" />
      <WorldviewPanel :worldview="worldview" />
      <AIPanel :status="aiStatus" />
    </div>
  </div>
</template>

<script setup>
const chapters = ref([])
const currentChapter = ref(null)
const isGenerating = ref(false)

// 生成章节
const handleGenerate = async () => {
  isGenerating.value = true
  try {
    const result = await $fetch('/api/novels/generate-chapter', {
      method: 'POST',
      body: {
        novel_id: route.params.id,
        chapter_no: chapters.value.length + 1,
      }
    })
    currentChapter.value = result
    chapters.value.push(result)
  } finally {
    isGenerating.value = false
  }
}
</script>
```

#### 2. 角色管理器

```vue
<template>
  <div class="character-manager">
    <!-- 角色列表 -->
    <CharacterList
      :characters="characters"
      @select="handleSelect"
    />

    <!-- 角色编辑器 -->
    <CharacterEditor
      v-if="selectedCharacter"
      :character="selectedCharacter"
      @update="handleUpdate"
    />

    <!-- 关系图谱 -->
    <RelationshipGraph
      v-if="selectedCharacter"
      :character-id="selectedCharacter.id"
    />
  </div>
</template>
```

#### 3. 世界观圣经编辑器

```vue
<template>
  <div class="worldview-editor">
    <Tabs>
      <Tab value="overview">
        <WorldviewOverview :worldview="worldview" />
      </Tab>
      <Tab value="entities">
        <EntityManager :entities="worldview.entities" />
      </Tab>
      <Tab value="rules">
        <RulesEditor :rules="worldview.rules" />
      </Tab>
    </Tabs>
  </div>
</template>
```

---

## 🔌 API 设计

### RESTful API 端点

#### 小说管理

```
GET    /api/novels                    # 获取小说列表
POST   /api/novels                    # 创建小说
GET    /api/novels/:id                # 获取小说详情
PUT    /api/novels/:id                # 更新小说
DELETE /api/novels/:id                # 删除小说

GET    /api/novels/:id/chapters       # 获取章节列表
POST   /api/novels/:id/chapters       # 创建章节
GET    /api/novels/:id/chapters/:no   # 获取章节内容
PUT    /api/novels/:id/chapters/:no   # 更新章节
DELETE /api/novels/:id/chapters/:no   # 删除章节

POST   /api/novels/:id/generate-chapter      # 生成章节
POST   /api/novels/:id/generate-outline      # 生成大纲
POST   /api/novels/:id/continuity-check      # 连续性检查
```

#### 角色管理

```
GET    /api/novels/:id/characters          # 获取角色列表
POST   /api/novels/:id/characters          # 创建角色
GET    /api/characters/:id                 # 获取角色详情
PUT    /api/characters/:id                 # 更新角色
DELETE /api/characters/:id                 # 删除角色
GET    /api/characters/:id/relations       # 获取角色关系
GET    /api/characters/:id/appearances     # 获取角色出现记录
```

#### 世界观管理

```
GET    /api/worldviews                # 获取世界观列表
POST   /api/worldviews                # 创建世界观
GET    /api/worldviews/:id            # 获取世界观详情
PUT    /api/worldviews/:id            # 更新世界观
GET    /api/worldviews/:id/entities   # 获取世界观实体
POST   /api/worldviews/:id/entities   # 创建实体
```

#### 参考小说

```
POST   /api/references/crawl          # 爬取小说
GET    /api/references                # 获取参考列表
GET    /api/references/:id            # 获取参考详情
DELETE /api/references/:id            # 删除参考
GET    /api/references/:id/analysis   # 获取分析结果
```

#### 知识库

```
POST   /api/knowledge/search          # 知识检索
POST   /api/knowledge/store           # 存储知识
GET    /api/knowledge/types           # 获取知识类型
```

### WebSocket 端点

```
WS /ws/novel/:id/generation   # 实时生成进度推送
WS /ws/crawler/:id/status     # 爬取状态推送
```

---

## 🗂️ 项目目录结构

```
inkframe-backend/
├── cmd/
│   └── server/
│       └── main.go                    # 应用入口
├── internal/
│   ├── api/
│   │   ├── handler/                   # HTTP 处理器
│   │   │   ├── novel.go
│   │   │   ├── character.go
│   │   │   ├── worldview.go
│   │   │   ├── reference.go
│   │   │   └── knowledge.go
│   │   ├── middleware/                # 中间件
│   │   │   ├── auth.go
│   │   │   ├── cors.go
│   │   │   └── logger.go
│   │   └── router.go                  # 路由配置
│   ├── service/
│   │   ├── novel_service.go           # 小说生成服务
│   │   ├── character_service.go       # 角色管理服务
│   │   ├── worldview_service.go       # 世界观管理服务
│   │   ├── crawler_service.go         # 爬虫服务
│   │   ├── analysis_service.go        # 分析服务
│   │   ├── knowledge_service.go       # 知识库服务
│   │   ├── prompt_service.go          # 提示词服务
│   │   └── continuity_service.go      # 连续性检查服务
│   ├── repository/
│   │   ├── novel_repo.go
│   │   ├── character_repo.go
│   │   ├── worldview_repo.go
│   │   └── reference_repo.go
│   ├── model/
│   │   ├── novel.go
│   │   ├── character.go
│   │   ├── worldview.go
│   │   └── reference.go
│   ├── ai/
│   │   ├── provider.go                # AI 提供者接口
│   │   ├── openai.go
│   │   ├── claude.go
│   │   └── prompt.go
│   ├── vector/
│   │   ├── store.go                   # 向量存储接口
│   │   ├── weaviate.go
│   │   └── qdrant.go
│   ├── crawler/
│   │   ├── crawler.go
│   │   ├── parser.go                  # 解析器接口
│   │   ├── qidian.go                  # 起点中文网
│   │   ├── jjwxc.go                   # 晋江文学城
│   │   └── zongheng.go                # 纵横中文网
│   └── config/
│       ├── config.go
│       └── database.go
├── pkg/
│   ├── utils/
│   │   ├── text.go                    # 文本处理工具
│   │   ├── compress.go                # 上下文压缩
│   │   └── validation.go              # 验证工具
│   └── logger/
│       └── logger.go
├── web/                                # 前端项目（可选，也可单独仓库）
│   ├── pages/
│   ├── components/
│   ├── composables/
│   └── stores/
├── deployments/
│   ├── docker-compose.yml             # 容器编排
│   ├── Dockerfile.backend
│   ├── Dockerfile.frontend
│   └── kubernetes/                    # K8s 配置（可选）
├── scripts/
│   ├── migrate.sh                     # 数据库迁移
│   └── seed.sh                        # 初始化数据
├── docs/
│   ├── API.md                         # API 文档
│   └── ARCHITECTURE.md                # 架构文档
├── go.mod
├── go.sum
├── Makefile
├── .env.example
└── README.md
```

---

## 🚀 部署方案

### Docker Compose 部署

```yaml
version: '3.8'

services:
  # 后端服务
  backend:
    build:
      context: .
      dockerfile: deployments/Dockerfile.backend
    ports:
      - "8080:8080"
    environment:
      - DB_HOST=mysql
      - REDIS_HOST=redis
      - VECTOR_STORE_URL=http://weaviate:8080
      - OPENAI_API_KEY=${OPENAI_API_KEY}
    depends_on:
      - mysql
      - redis
      - weaviate
      - minio

  # 前端服务
  frontend:
    build:
      context: ./web
      dockerfile: ../deployments/Dockerfile.frontend
    ports:
      - "3000:3000"
    environment:
      - API_BASE_URL=http://localhost:8080

  # MySQL 数据库
  mysql:
    image: mysql:8.0
    environment:
      - MYSQL_ROOT_PASSWORD=${MYSQL_ROOT_PASSWORD}
      - MYSQL_DATABASE=inkframe
    volumes:
      - mysql_data:/var/lib/mysql
    ports:
      - "3306:3306"

  # Redis 缓存
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data

  # 向量数据库
  weaviate:
    image: semitechnologies/weaviate:latest
    ports:
      - "8081:8080"
    environment:
      - QUERY_DEFAULTS_LIMIT=25
      - AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true
      - PERSISTENCE_DATA_PATH=/var/lib/weaviate
    volumes:
      - weaviate_data:/var/lib/weaviate

  # 文件存储
  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    ports:
      - "9000:9000"
      - "9001:9001"
    environment:
      - MINIO_ROOT_USER=${MINIO_ROOT_USER}
      - MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}
    volumes:
      - minio_data:/data

volumes:
  mysql_data:
  redis_data:
  weaviate_data:
  minio_data:
```

### 生产环境建议

1. **负载均衡**：使用 Nginx 反向代理
2. **数据库主从**：MySQL 主从复制
3. **缓存集群**：Redis Cluster
4. **监控告警**：Prometheus + Grafana
5. **日志收集**：ELK Stack
6. **CI/CD**：GitHub Actions 或 GitLab CI

---

## 📝 开发路线图

### Phase 1: MVP（最小可行产品）- 2 个月

- [x] 数据库设计与初始化
- [ ] 基础 AI 集成（OpenAI/Claude）
- [ ] 简单的小说生成功能（大纲 + 章节生成）
- [ ] 基础角色管理
- [ ] 基础世界观管理
- [ ] 简单的前端界面（小说列表、编辑器）
- [ ] 基础知识库（向量检索）

### Phase 2: 核心功能 - 2 个月

- [ ] 参考小说爬取（1-2 个站点）
- [ ] 风格分析与参考推荐
- [ ] 连续性检查系统
- [ ] 剧情点追踪系统
- [ ] 角色关系图谱
- [ ] 提示词模板系统
- [ ] WebSocket 实时推送

### Phase 3: 增强功能 - 2 个月

- [ ] 多 AI 提供者支持
- [ ] 高级爬虫（更多站点）
- [ ] 批量生成与管理
- [ ] 小说导出（EPUB、PDF）
- [ ] 用户权限系统
- [ ] 协作编辑功能

### Phase 4: 优化与扩展 - 持续

- [ ] 性能优化
- [ ] AI 模型微调
- [ ] 多语言支持
- [ ] 移动端适配
- [ ] 社区功能

---

## 🔒 安全考虑

1. **API 认证**：JWT Token
2. **数据加密**：敏感数据加密存储
3. **访问控制**：基于角色的权限管理（RBAC）
4. **内容过滤**：AI 生成内容合规性检查
5. **爬虫限流**：遵守站点规则，避免被封
6. **输入验证**：严格的参数验证
7. **SQL 注入防护**：使用 ORM 和参数化查询

---

## 📊 性能优化

1. **缓存策略**：
   - Redis 缓存热点数据
   - 向量检索结果缓存
   - AI 生成结果缓存

2. **数据库优化**：
   - 合理的索引设计
   - 读写分离
   - 分页查询

3. **AI 调用优化**：
   - 批量请求
   - 结果缓存
   - 上下文压缩

4. **前端优化**：
   - Nuxt SSR/SSG
   - 组件懒加载
   - 图片优化

---

## 📚 参考资料

### 相关论文

- "Large Language Models for Creative Writing"
- "Maintaining Consistency in AI-Generated Long-form Content"

### 开源项目

- [Sudowrite](https://sudowrite.com/) - AI 写作工具参考
- [NovelAI](https://novelai.net/) - AI 故事生成
- [GPT-3 Longform Writing](https://github.com/openai/gpt-3-examples)

---

## 👥 贡献指南

欢迎贡献！请遵循以下流程：

1. Fork 本仓库
2. 创建特性分支 (`git checkout -b feature/AmazingFeature`)
3. 提交更改 (`git commit -m 'Add some AmazingFeature'`)
4. 推送到分支 (`git push origin feature/AmazingFeature`)
5. 开启 Pull Request

---

## 📄 许可证

本项目采用 MIT 许可证 - 详见 LICENSE 文件

---

**InkFrame** - 让每个人都能创作属于自己的故事 🖋️

---

## 🔍 质量控制系统（增强版）

InkFrame 针对AI生成小说的常见问题，构建了全方位的质量控制体系。详细技术方案请参阅 **[QUALITY_CONTROL.md](./QUALITY_CONTROL.md)**

### 核心问题与解决方案

| 问题类型 | 严重程度 | 解决方案 | 预期效果 |
|---------|---------|---------|---------|
| 角色不一致 | 🔴 高 | 角色状态快照 + 性格约束检查 | 检测率 > 85% |
| 世界观崩坏 | 🔴 高 | 约束规则系统 + 实时验证 | 检测率 > 90% |
| 时间线混乱 | 🔴 高 | 时间线追踪系统 | 检测率 > 95% |
| 内容重复 | 🟡 中 | 重复性检测 + 多样性优化 | 准确率 > 80% |
| 对话不自然 | 🟡 中 | 对话优化器 + 口语化检查 | 检测率 > 70% |
| 描述空洞 | 🟡 中 | 感官细节增强 + 意象优化 | 识别率 > 75% |
| 剧情漏洞 | 🟡 中 | 逻辑推理引擎 + 因果检查 | 检测率 > 75% |
| 伏笔遗忘 | 🟠 中低 | 剧情点追踪系统 | 减少 > 80% |

### 质量控制架构

```
┌─────────────────────────────────────────────────────────┐
│              质量控制与优化系统                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ 一致性检查   │  │ 质量分析     │  │ 逻辑验证     │  │
│  │ - 角色       │  │ - 重复性     │  │ - 剧情逻辑   │  │
│  │ - 世界观     │  │ - 描述质量   │  │ - 伏笔追踪   │  │
│  │ - 时间线     │  │ - 对话质量   │  │ - 战力体系   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ 风格检查     │  │ AI 生成优化  │  │ 人机协作     │  │
│  │ - 文风一致性 │  │ - 长期记忆   │  │ - 审核工作流 │  │
│  │ - 叙述视角   │  │ - 提示词优化 │  │ - 版本控制   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### 实施路线图

**第一阶段：核心一致性保证（P0）** - 4周
- 角色状态快照系统
- 角色一致性检查器
- 世界观约束规则系统
- 世界观约束验证器
- 时间线追踪系统

**第二阶段：内容质量控制（P1）** - 3周
- 重复性检测器
- 描述质量分析器
- 对话质量分析器
- 风格一致性检查器

**第三阶段：逻辑与剧情验证（P1）** - 3周
- 剧情逻辑检查器
- 伏笔追踪系统
- 力量体系验证器
- 因果关系检查器

**第四阶段：AI 生成优化（P2）** - 2周
- 长期记忆管理器
- 提示词优化器
- 上下文压缩算法
- 智能参考检索

**第五阶段：人机协作系统（P2）** - 2周
- 审核任务管理器
- 版本控制系统
- 自动修复建议系统
- 反馈学习系统

**总计：14 周（约 3.5 个月）**

### 质量指标目标

| 指标 | 当前AI平均 | InkFrame目标 | 提升幅度 |
|-----|-----------|------------|---------|
| 一致性得分 | 65% | 90% | +50% |
| 可读性得分 | 70% | 85% | +21% |
| 逻辑性得分 | 68% | 88% | +29% |
| 风格稳定性 | 60% | 85% | +42% |
| 人工修正率 | 40% | 15% | -62% |

---

**InkFrame v2.0** - 通过全方位质量控制，让 AI 生成小说质量飞跃式提升 🚀



---

## 🎬 视频生成系统（扩展功能）

InkFrame 不仅支持生成小说，还能基于小说内容自动生成高质量视频，并保证角色和场景的视觉一致性。详细技术方案请参阅 **[VIDEO_GENERATION.md](./VIDEO_GENERATION.md)**

### 核心特性

- 🎬 **智能视频生成**：从小说文本自动生成分镜和视频
- 👤 **角色一致性**：同一角色在不同场景、表情、动作下保持视觉一致（>90%）
- 🌆 **场景一致性**：同一场景在不同视角、光照下保持视觉一致（>85%）
- 🎨 **风格统一**：整个视频保持一致的视觉风格
- 🔊 **自动配音**：AI 生成角色配音、背景音乐、音效
- ✏️ **交互编辑**：可视化的视频编辑器和分镜调整工具

### 视频生成架构

```
┌─────────────────────────────────────────────────────────┐
│              视频生成系统                                │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ 角色视觉系统 │  │ 场景视觉系统 │  │ 分镜生成系统 │  │
│  │ - 角色设计   │  │ - 场景设计   │  │ - 文本分析   │  │
│  │ - 表情库     │  │ - 视角库     │  │ - 摄像机配置 │  │
│  │ - 动作库     │  │ - 环境元素   │  │ - 节奏控制   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ 帧生成系统   │  │ 音频生成系统 │  │ 一致性控制   │  │
│  │ - 帧序列     │  │ - 角色配音   │  │ - 角色验证   │  │
│  │ - 插值动画   │  │ - 背景音乐   │  │ - 场景验证   │  │
│  │ - 视频渲染   │  │ - 音效       │  │ - 自动修复   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### 一致性保证机制

#### 角色一致性

| 技术 | 说明 | 效果 |
|------|------|------|
| **角色 LoRA 训练** | 为每个角色训练专用模型 | 95%+ 一致性 |
| **IP-Adapter** | 使用参考图像保持一致性 | 90%+ 一致性 |
| **表情/动作库** | 预生成的标准表情和动作 | 85%+ 准确性 |
| **视觉 Embedding** | 向量化角色特征，实时验证 | 实时检测偏差 |

#### 场景一致性

| 技术 | 说明 | 效果 |
|------|------|------|
| **场景 LoRA 训练** | 为关键场景训练专用模型 | 90%+ 一致性 |
| **多视角生成** | 预生成不同视角的场景图 | 85%+ 一致性 |
| **环境元素追踪** | 自动检测关键元素是否缺失 | 90%+ 检测率 |
| **光影控制** | 保持场景的光照和氛围一致 | 85%+ 一致性 |

### 视频生成流程

```
小说章节
    ↓
文本分析（场景、对话、动作）
    ↓
分镜生成（摄像机角度、镜头类型、时长）
    ↓
角色/场景视觉设计（如果未生成）
    ↓
帧序列生成（使用一致性控制）
    ↓
视频渲染（FFmpeg）
    ↓
音频生成（配音 + 音乐 + 音效）
    ↓
最终视频
```

### AI 模型集成

| 功能 | 推荐模型 | 说明 |
|------|---------|------|
| **图像生成** | Stable Diffusion XL | 高质量图像生成 |
| **角色一致性** | IP-Adapter + LoRA | 保持角色视觉一致 |
| **视频生成** | Deforum / AnimateDiff | 帧间连续性 |
| **配音生成** | ElevenLabs | 高质量 TTS |
| **音乐生成** | Suno AI / MusicLM | 背景音乐 |
| **视觉分析** | CLIP / BLIP | 特征提取和验证 |

### 实施路线图

| 阶段 | 时间 | 主要内容 |
|-----|------|---------|
| **第一阶段** | 4周 | 角色和场景视觉生成系统 |
| **第二阶段** | 3周 | 分镜系统和编辑器 |
| **第三阶段** | 4周 | 视频生成和渲染 |
| **第四阶段** | 3周 | 音频生成系统 |
| **第五阶段** | 4周 | 高级功能和优化 |

**总计：18 周（约 4.5 个月）**

### 质量指标

| 指标 | 目标值 |
|-----|-------|
| 角色视觉一致性 | > 90% |
| 场景视觉一致性 | > 85% |
| 帧间连续性 | > 80% |
| 表情准确性 | > 85% |
| 单帧生成时间 | < 10 秒 |

---

**InkFrame v2.0** - 小说 + 视频，完整的内容创作生态 🎬📚



---

## ⚠️ 常见问题与解决方案

InkFrame 视频生成系统针对 AI 生成视频的 30+ 常见问题，提供了系统化的解决方案。详细方案请参阅 **[VIDEO_GENERATION.md](./VIDEO_GENERATION.md)**

### 问题分类与解决率

| 问题类别 | 典型问题 | 解决方案 | 预期解决率 |
|---------|---------|---------|-----------|
| **角色一致性** | 外观突变、表情不连贯、比例失调 | LoRA + IP-Adapter + 插值算法 | 90%+ |
| **场景一致性** | 元素消失、光照不一致、透视错误 | 场景LoRA + 元素追踪 + 光照模板 | 85%+ |
| **运动连续性** | 帧间抖动、运动不自然、物体穿模 | 视频稳定 + 物理模拟 + 贝塞尔曲线 | 85%+ |
| **音频同步** | 配音不自然、音画不同步 | 情感TTS + 自动对齐 | 90%+ |
| **质量控制** | 色彩不一致、分辨率波动 | 统一调色 + 质量验证 | 95%+ |

### 核心解决方案

#### 1. 角色一致性保证

- **LoRA 微调训练**：为每个角色训练专用模型（95%+ 一致性）
- **IP-Adapter 参考**：使用参考图像控制生成（90%+ 一致性）
- **表情/动作库**：预生成标准表情和动作（85%+ 准确性）
- **多参考融合**：融合多个参考图像（95%+ 一致性）

#### 2. 场景一致性保证

- **场景 LoRA 训练**：为关键场景训练专用模型（90%+ 一致性）
- **元素追踪系统**：实时追踪关键元素（90%+ 检测率）
- **光照模板系统**：统一光照和阴影（85%+ 一致性）
- **多视角生成**：预生成标准视角（85%+ 一致性）

#### 3. 运动连续性保证

- **视频稳定算法**：消除帧间抖动（90%+ 改善）
- **物理模拟引擎**：自然的运动轨迹（85%+ 自然度）
- **贝塞尔曲线平滑**：流畅的运动过渡（85%+ 平滑度）
- **光流平滑**：帧间连续性增强（80%+ 连续性）

#### 4. 音频同步保证

- **情感 TTS 系统**：自然的配音（90%+ 自然度）
- **音画自动对齐**：精确的时间同步（95%+ 同步率）
- **智能音效匹配**：场景适配的音效（85%+ 匹配度）
- **多音轨混合**：专业的音频处理（90%+ 质量）

#### 5. 质量控制系统

- **实时验证**：每帧生成时检测问题
- **自动修复**：自动检测和修复常见问题
- **统一调色**：整个视频色彩一致（95%+ 一致性）
- **质量监控**：实时质量指标追踪

### 问题解决流程

```
问题检测
    ↓
实时监控（每帧生成时）
    ↓
问题分类（角色/场景/运动/音频/质量）
    ↓
自动修复（如果可自动解决）
    ↓
人工审核（如果需要）
    ↓
问题解决
```

### 预期改善效果

| 指标 | 当前AI平均 | InkFrame目标 | 改善幅度 |
|-----|-----------|------------|---------|
| 角色一致性 | 60% | 90% | **+50%** 🚀 |
| 场景一致性 | 55% | 85% | **+55%** 🚀 |
| 运动连续性 | 65% | 80% | **+23%** 📈 |
| 音画同步 | 70% | 90% | **+29%** 📈 |
| 整体质量 | 65% | 85% | **+31%** 📈 |

---

**InkFrame v2.0** - 完整的问题解决方案体系，让 AI 视频生成质量飞跃提升 🎬✨



---

## 🤖 多模型管理系统

InkFrame 支持集成多种 LLM（大语言模型），允许用户在不同步骤选择合适的模型，并提供模型效果对比功能。详细技术方案请参阅 **[MULTI_MODEL_MANAGEMENT.md](./MULTI_MODEL_MANAGEMENT.md)**

### 核心特性

- 🔄 **多模型支持**：支持 OpenAI、Claude、Gemini、Llama、Qwen 等主流模型
- 🎯 **智能模型选择**：根据任务类型自动选择最优模型
- 📊 **效果对比**：A/B 测试和模型效果对比分析
- 💰 **成本优化**：平衡质量和成本，自动选择性价比最高的模型
- 🔧 **灵活配置**：每个步骤可独立配置模型和参数
- 📈 **性能监控**：实时监控模型性能和质量指标
- 🧪 **实验功能**：支持模型实验和对比

### 支持的模型提供商

| 提供商 | 模型示例 | 特点 |
|--------|---------|------|
| **OpenAI** | GPT-4, GPT-4-Turbo, GPT-3.5 | 高质量、强大的推理能力 |
| **Anthropic** | Claude-3-Opus, Claude-3-Sonnet | 优秀的安全性和长上下文 |
| **Google** | Gemini-Pro, Gemini-Ultra | 多模态能力、Google生态 |
| **Meta** | Llama-3-70B, Llama-3-8B | 开源、可本地部署 |
| **Alibaba** | Qwen-Max, Qwen-Plus | 中文优化、性价比高 |
| **Local** | 自定义本地模型 | 完全控制、零成本 |

### 模型选择策略

| 策略 | 描述 | 适用场景 |
|------|------|---------|
| **质量优先** | 选择质量最高的模型 | 重要创作、关键决策 |
| **成本优先** | 选择成本最低的模型 | 批量生成、草稿创作 |
| **平衡模式** | 平衡质量和成本 | 日常创作、标准流程 |
| **自定义** | 根据用户权重配置 | 特殊需求、实验场景 |

### 智能选择系统

```
任务输入
    ↓
分析任务类型和需求
    ↓
获取可用模型列表
    ↓
应用选择策略
    ├─ 质量优先
    ├─ 成本优先
    ├─ 平衡模式
    └─ 自定义
    ↓
应用约束检查
    ├─ 成本限制
    ├─ 上下文窗口
    └─ 最低质量要求
    ↓
选择最佳模型
    ↓
执行任务
    ↓
故障转移（如果失败）
    ↓
返回结果
```

### 对比实验系统

#### 功能特性

- **并行执行**：同时运行多个模型进行对比
- **自动评估**：AI 自动评估生成质量
- **多维对比**：质量、成本、延迟、相关性、创造性
- **可视化报告**：直观的对比结果展示
- **自动推荐**：根据对比结果推荐最佳模型

#### 对比维度

| 维度 | 说明 |
|------|------|
| **质量评分** | 整体质量（0-1.0） |
| **相关性** | 与任务要求的相关性 |
| **创造性** 内容的独创性和新颖性 |
| **一致性** | 与已有内容的一致性 |
| **成本** | Token 使用成本 |
| **延迟** | 响应时间 |

### 任务类型与推荐模型

| 任务类型 | 推荐模型 | 备选模型 |
|---------|---------|---------|
| 小说生成 | Claude-3-Opus | GPT-4-Turbo, Qwen-Max |
| 章节生成 | GPT-4-Turbo | Claude-3-Sonnet, Llama-3-70B |
| 角色创建 | Claude-3-Opus | GPT-4, Gemini-Pro |
| 世界观生成 | GPT-4 | Claude-3-Opus, Qwen-Max |
| 对话生成 | Claude-3-Sonnet | GPT-3.5-Turbo, Llama-3-8B |
| 分镜生成 | GPT-4-Turbo | Claude-3-Sonnet |
| 帧生成 | GPT-4 | Claude-3-Opus |
| 图像生成 | SDXL, Midjourney | DALL-E 3 |
| 音频生成 | ElevenLabs | Azure TTS |
| 音乐生成 | Suno AI | MusicLM |

### API 核心端点

```
模型管理：
GET    /api/model-providers              # 获取提供商列表
POST   /api/model-providers              # 添加提供商
GET    /api/models                       # 获取模型列表
POST   /api/models                       # 添加模型
GET    /api/models/available/:task_type  # 获取可用模型

模型选择：
POST   /api/model/select                 # 选择模型
GET    /api/model/recommendations/:task_type  # 获取推荐模型

对比实验：
GET    /api/model-comparisons             # 获取对比实验列表
POST   /api/model-comparisons             # 创建对比实验
POST   /api/model-comparisons/:id/run     # 运行实验
GET    /api/model-comparisons/:id/report  # 获取对比报告

任务执行：
POST   /api/tasks/execute                # 执行任务
POST   /api/tasks/execute-batch          # 批量执行（对比）
GET    /api/tasks/:id                    # 获取任务结果

统计分析：
GET    /api/model-usage                   # 获取使用记录
GET    /api/model-performance            # 获取性能统计
GET    /api/model-performance/:model_id/trends  # 获取性能趋势
```

### 实施路线图

| 阶段 | 时间 | 主要内容 |
|-----|------|---------|
| **第一阶段** | 2周 | 基础模型管理（提供商、模型、执行） |
| **第二阶段** | 2周 | 智能选择系统（策略、约束、故障转移） |
| **第三阶段** | 2周 | 对比实验系统（创建、执行、分析） |
| **第四阶段** | 2周 | 前端界面（管理、对比、监控） |
| **第五阶段** | 2周 | 优化扩展（缓存、性能、自定义模型） |

**总计：10 周（约 2.5 个月）**

### 预期效果

| 指标 | 目标值 |
|-----|-------|
| 支持模型数量 | 10+ |
| 支持提供商数量 | 6+ |
| 模型选择准确率 | > 90% |
| 故障转移成功率 | > 95% |
| 对比结果一致性 | > 85% |
| 缓存命中率 | > 80% |

---

**InkFrame v2.0** - 多模型支持，灵活智能的 AI 能力 🤖✨

