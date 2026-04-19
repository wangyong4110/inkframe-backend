# InkFrame - 质量控制与问题解决方案

## 📋 概述

本文档针对 AI 生成小说的常见问题（一致性、质量、风格、逻辑等），提供详细的技术解决方案和优化策略，作为主技术文档的补充。

---

## 🎯 问题分类与解决策略

### 问题矩阵

| 问题类型 | 严重程度 | 技术解决方案 | 优先级 |
|---------|---------|------------|-------|
| 角色不一致 | 🔴 高 | 角色状态快照、性格约束检查 | P0 |
| 世界观崩坏 | 🔴 高 | 约束规则系统、实时验证 | P0 |
| 时间线混乱 | 🔴 高 | 时间线追踪系统 | P0 |
| 内容重复 | 🟡 中 | 重复性检测、多样性优化 | P1 |
| 对话不自然 | 🟡 中 | 对话优化器、口语化检查 | P1 |
| 描述空洞 | 🟡 中 | 感官细节增强、意象优化 | P1 |
| 剧情漏洞 | 🟡 中 | 逻辑推理引擎、因果检查 | P1 |
| 伏笔遗忘 | 🟠 中低 | 剧情点追踪系统 | P2 |
| 风格不稳定 | 🟠 中低 | 风格一致性检查 | P2 |

---

## 🔗 一致性保证系统（增强版）

### 1. 角色一致性增强

#### 1.1 增强的角色状态管理

```go
// 角色状态快照（关键改进）
type CharacterStateSnapshot struct {
    ID          uint      `json:"id" gorm:"primaryKey"`
    CharacterID uint      `json:"character_id"`
    ChapterID   uint      `json:"chapter_id"`

    // 物理状态
    Age         float64   `json:"age"`
    Height      float64   `json:"height"` // 单位：米
    Weight      float64   `json:"weight"` // 单位：公斤
    Health      string    `json:"health"` // healthy, injured, critical
    Injuries    string    `json:"injuries" gorm:"type:text"` // JSON: [{part, severity, description}]

    // 能力状态
    PowerLevel  int       `json:"power_level"`
    Abilities   string    `json:"abilities" gorm:"type:text"` // JSON
    Equipment   string    `json:"equipment" gorm:"type:text"` // JSON

    // 心理状态
    Mood        string    `json:"mood"`
    Motivation  string    `json:"motivation"`
    Goals       string    `json:"goals" gorm:"type:text"` // JSON
    Fears       string    `json:"fears" gorm:"type:text"` // JSON

    // 位置状态
    Location    string    `json:"location"`
    KnownLocations []string `json:"known_locations" gorm:"type:text"` // JSON

    // 关系状态
    Relations   string    `json:"relations" gorm:"type:text"` // JSON: [{character_id, attitude, recent_interaction}]

    SnapshotTime time.Time `json:"snapshot_time"`
}

// 性格约束定义
type PersonalityConstraint struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"`

    // 核心特质（禁止改变）
    CoreTraits  string `json:"core_traits" gorm:"type:text"` // JSON: [{trait, description, weight}]

    // 行为约束
    Behaviors   string `json:"behaviors" gorm:"type:text"` // JSON: [{type, description, allowed}]

    // 说话风格约束
    SpeechStyle string `json:"speech_style" gorm:"type:text"` // JSON: {formal, slang, catchphrase, tone}

    // 决策偏好
    DecisionPattern string `json:"decision_pattern" gorm:"type:text"` // JSON: [{situation, preferred_action, weight}]

    CreatedAt   time.Time `json:"created_at"`
}

// 角色一致性检查器
type CharacterConsistencyChecker struct {
    db       *gorm.DB
    aiClient AIProvider
    vectorStore VectorStore
}

// 检查角色一致性（增强版）
func (c *CharacterConsistencyChecker) CheckConsistency(novelID uint, chapterNo int, content string) *CharacterConsistencyReport {
    report := &CharacterConsistencyReport{
        Issues: []CharacterIssue{},
    }

    // 1. 提取本章出现的角色
    appearedCharacters := c.extractCharacters(content)

    // 2. 对每个角色进行检查
    for _, charID := range appearedCharacters {
        // 获取角色最新快照
        latestSnapshot := c.getLatestSnapshot(charID, chapterNo)

        // 获取角色约束
        constraints := c.getConstraints(charID)

        // AI 检测行为一致性
        behaviorIssues := c.checkBehaviorConsistency(charID, content, latestSnapshot, constraints)
        report.Issues = append(report.Issues, behaviorIssues...)

        // 检测能力一致性
        powerIssues := c.checkPowerConsistency(charID, content, latestSnapshot)
        report.Issues = append(report.Issues, powerIssues...)

        // 检测对话风格一致性
        dialogueIssues := c.checkDialogueConsistency(charID, content, constraints)
        report.Issues = append(report.Issues, dialogueIssues...)

        // 检测外貌描述一致性
        appearanceIssues := c.checkAppearanceConsistency(charID, content, latestSnapshot)
        report.Issues = append(report.Issues, appearanceIssues...)
    }

    return report
}

// 检查行为一致性
func (c *CharacterConsistencyChecker) checkBehaviorConsistency(
    characterID uint,
    content string,
    snapshot *CharacterStateSnapshot,
    constraints *PersonalityConstraint,
) []CharacterIssue {
    issues := []CharacterIssue{}

    // 使用 AI 分析角色行为
    analysis := c.aiClient.AnalyzeCharacterBehavior(content, snapshot, constraints)

    // 检测性格突变
    if analysis.PersonalityDeviation > 0.7 {
        issues = append(issues, CharacterIssue{
            Type:        "personality_mutation",
            Severity:    "high",
            Description: fmt.Sprintf("角色行为与性格设定偏差过大（%.2f）", analysis.PersonalityDeviation),
            Location:    analysis.Locations,
            Suggestion:  "检查角色行为是否符合其性格设定，考虑是否有合理的心理变化过程",
        })
    }

    // 检测动机不匹配
    if analysis.ActionMismatch {
        issues = append(issues, CharacterIssue{
            Type:        "motivation_mismatch",
            Severity:    "medium",
            Description: "角色行为与其当前动机不符",
            Location:    analysis.Locations,
            Suggestion:  "明确角色当前的动机和目标，确保行为符合逻辑",
        })
    }

    return issues
}

// 检测能力一致性
func (c *CharacterConsistencyChecker) checkPowerConsistency(
    characterID uint,
    content string,
    snapshot *CharacterStateSnapshot,
) []CharacterIssue {
    issues := []CharacterIssue{}

    // 提取能力描述
    powerDescriptions := c.extractPowerDescriptions(content, characterID)

    // 与快照对比
    for _, desc := range powerDescriptions {
        if desc.PowerLevel > snapshot.PowerLevel*1.5 {
            issues = append(issues, CharacterIssue{
                Type:        "power_inflation",
                Severity:    "high",
                Description: fmt.Sprintf("角色能力突然提升（%d -> %d），缺乏合理解释", snapshot.PowerLevel, desc.PowerLevel),
                Location:    desc.Location,
                Suggestion:  "增加能力提升的铺垫和训练过程，或调整世界观设定的成长速度",
            })
        }
    }

    return issues
}

// 检测对话风格一致性
func (c *CharacterConsistencyChecker) checkDialogueConsistency(
    characterID uint,
    content string,
    constraints *PersonalityConstraint,
) []CharacterIssue {
    issues := []CharacterIssue{}

    // 提取对话
    dialogues := c.extractDialogues(content, characterID)

    // 分析风格
    styleAnalysis := c.aiClient.AnalyzeDialogueStyle(dialogues, constraints)

    if styleAnalysis.StyleDeviation > 0.6 {
        issues = append(issues, CharacterIssue{
            Type:        "dialogue_style_inconsistency",
            Severity:    "medium",
            Description: fmt.Sprintf("对话风格与设定偏差（%.2f）", styleAnalysis.StyleDeviation),
            Location:    styleAnalysis.Locations,
            Suggestion:  "调整对话语言，使其符合角色的说话风格设定",
        })
    }

    return issues
}
```

#### 1.2 角色弧光追踪

```go
// 角色弧光里程碑
type CharacterArcMilestone struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"`
    ChapterID   uint   `json:"chapter_id"`

    MilestoneType string `json:"milestone_type"` // awakening, transformation, redemption, sacrifice, etc.
    Description   string `json:"description"`
    EmotionalJourney string `json:"emotional_journey" gorm:"type:text"` // JSON: [{emotion, intensity, trigger}]

    // 变化对比
    BeforeState string `json:"before_state" gorm:"type:text"` // JSON
    AfterState  string `json:"after_state" gorm:"type:text"` // JSON

    IsMajorChange bool `json:"is_major_change"`
    Verified      bool `json:"verified"` // 是否符合角色弧光设定
}

// 角色弧光验证器
type CharacterArcValidator struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 验证角色变化是否符合弧光
func (v *CharacterArcValidator) ValidateChange(characterID uint, chapterID int, changeDescription string) *ArcValidation {
    // 1. 获取角色弧光设定
    arc := v.getCharacterArc(characterID)

    // 2. 获取历史里程碑
    milestones := v.getMilestones(characterID)

    // 3. AI 验证
    validation := v.aiClient.ValidateArcChange(changeDescription, arc, milestones)

    return validation
}
```

---

### 2. 世界观约束系统（增强版）

#### 2.1 约束规则定义

```go
// 世界观约束规则
type WorldviewConstraint struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    WorldviewID uint   `json:"worldview_id"`

    RuleType    string `json:"rule_type"` // magic_system, physics, geography, culture, technology
    Category    string `json:"category"`
    Description string `json:"description"`

    // 约束内容
    Constraint  string `json:"constraint" gorm:"type:text"` // JSON: 具体约束规则

    // 验证方法
    ValidationMethod string `json:"validation_method"` // keyword_match, semantic_check, logic_inference

    // 严重程度
    Severity    string `json:"severity"` // error, warning, info

    // 示例
    Examples    string `json:"examples" gorm:"type:text"` // JSON: [{text, is_valid, reason}]

    IsActive    bool   `json:"is_active"`
    CreatedAt   time.Time `json:"created_at"`
}

// 世界观约束验证器
type WorldviewConstraintValidator struct {
    db         *gorm.DB
    aiClient   AIProvider
    vectorStore VectorStore
}

// 验证内容是否违反世界观约束
func (v *WorldviewConstraintValidator) Validate(novelID uint, content string) *ConstraintValidationReport {
    report := &ConstraintValidationReport{
        Violations: []ConstraintViolation{},
    }

    // 1. 获取世界观约束
    constraints := v.getActiveConstraints(novelID)

    // 2. 批量验证
    for _, constraint := range constraints {
        switch constraint.ValidationMethod {
        case "keyword_match":
            violations := v.validateByKeyword(content, constraint)
            report.Violations = append(report.Violations, violations...)

        case "semantic_check":
            violations := v.validateBySemantic(content, constraint)
            report.Violations = append(report.Violations, violations...)

        case "logic_inference":
            violations := v.validateByLogic(content, constraint)
            report.Violations = append(report.Violations, violations...)
        }
    }

    return report
}

// 关键词匹配验证
func (v *WorldviewConstraintValidator) validateByKeyword(content string, constraint *WorldviewConstraint) []ConstraintViolation {
    violations := []ConstraintViolation{}

    rule := v.parseConstraint(constraint.Constraint)
    keywords := rule.ForbiddenKeywords

    for _, keyword := range keywords {
        if strings.Contains(content, keyword) {
            violations = append(violations, ConstraintViolation{
                Type:        constraint.RuleType,
                Severity:    constraint.Severity,
                Description: fmt.Sprintf("违反约束：%s（关键词：%s）", constraint.Description, keyword),
                Location:    v.findKeywordLocation(content, keyword),
                Suggestion:  rule.ReplacementSuggestion,
            })
        }
    }

    return violations
}

// 语义检查验证
func (v *WorldviewConstraintValidator) validateBySemantic(content string, constraint *WorldviewConstraint) []ConstraintViolation {
    violations := []ConstraintViolation{}

    // 使用 AI 检测语义违反
    analysis := v.aiClient.CheckSemanticViolation(content, constraint)

    if analysis.IsViolation {
        violations = append(violations, ConstraintViolation{
            Type:        constraint.RuleType,
            Severity:    constraint.Severity,
            Description: analysis.Description,
            Location:    analysis.Locations,
            Suggestion:  analysis.Suggestion,
        })
    }

    return violations
}

// 逻辑推理验证
func (v *WorldviewConstraintValidator) validateByLogic(content string, constraint *WorldviewConstraint) []ConstraintViolation {
    violations := []ConstraintViolation{}

    // 构建逻辑规则
    rule := v.parseConstraint(constraint.Constraint)

    // 提取相关实体
    entities := v.extractEntities(content, rule.RelatedEntityTypes)

    // 检查逻辑关系
    for _, entity := range entities {
        if !v.checkLogicRule(entity, rule) {
            violations = append(violations, ConstraintViolation{
                Type:        constraint.RuleType,
                Severity:    constraint.Severity,
                Description: fmt.Sprintf("逻辑违反：%s（实体：%s）", constraint.Description, entity.Name),
                Location:    entity.Locations,
                Suggestion:  rule.LogicExplanation,
            })
        }
    }

    return violations
}
```

#### 2.2 力量体系验证器

```go
// 力量体系规则
type PowerSystemRule struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    WorldviewID uint   `json:"worldview_id"`

    SystemName  string `json:"system_name"` // 修炼体系、魔法体系等
    Ranks       string `json:"ranks" gorm:"type:text"` // JSON: 等级定义

    // 成长规则
    ProgressionRule string `json:"progression_rule" gorm:"type:text"` // JSON

    // 能力限制
    AbilityLimit string `json:"ability_limit" gorm:"type:text"` // JSON

    // 战力对比
    PowerComparison string `json:"power_comparison" gorm:"type:text"` // JSON

    CreatedAt   time.Time `json:"created_at"`
}

// 力量体系验证器
type PowerSystemValidator struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 验证战斗场景
func (v *PowerSystemValidator) ValidateCombat(novelID uint, content string) *CombatValidationReport {
    report := &CombatValidationReport{
        Issues: []CombatIssue{},
    }

    // 1. 获取力量体系规则
    rules := v.getPowerSystemRules(novelID)

    // 2. 提取战斗场景
    combatScenes := v.extractCombatScenes(content)

    // 3. 验证每个战斗场景
    for _, scene := range combatScenes {
        // 检查战力对比
        powerIssues := v.checkPowerBalance(scene, rules)
        report.Issues = append(report.Issues, powerIssues...)

        // 检查能力使用
        abilityIssues := v.checkAbilityUsage(scene, rules)
        report.Issues = append(report.Issues, abilityIssues...)

        // 检查成长合理性
        growthIssues := v.checkGrowthLogic(scene, rules)
        report.Issues = append(report.Issues, growthIssues...)
    }

    return report
}
```

---

### 3. 时间线追踪系统

```go
// 时间线事件
type TimelineEvent struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    ChapterID   uint   `json:"chapter_id"`

    EventType   string `json:"event_type"` // movement, meeting, combat, discovery, change
    EventTime   time.Time `json:"event_time"` // 小说内时间
    Duration    string `json:"duration"` // 持续时间

    Location    string `json:"location"`
    Characters  string `json:"characters" gorm:"type:text"` // JSON
    Description string `json:"description"`

    // 时间关系
    PrecedingEvents  string `json:"preceding_events" gorm:"type:text"` // JSON
    SubsequentEvents string `json:"subsequent_events" gorm:"type:text"` // JSON

    IsAnchored  bool   `json:"is_anchored"` // 是否为时间锚点
    CreatedAt   time.Time `json:"created_at"`
}

// 时间线验证器
type TimelineValidator struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 检查时间线一致性
func (v *TimelineValidator) ValidateTimeline(novelID uint) *TimelineValidationReport {
    report := &TimelineValidationReport{
        Issues: []TimelineIssue{},
    }

    // 1. 获取所有时间线事件
    events := v.getAllEvents(novelID)

    // 2. 构建时间线图
    timeline := v.buildTimelineGraph(events)

    // 3. 检查时间冲突
    conflicts := v.detectTimeConflicts(timeline)
    report.Issues = append(report.Issues, conflicts...)

    // 4. 检查因果关系
    causalIssues := v.checkCausality(timeline)
    report.Issues = append(report.Issues, causalIssues...)

    // 5. 检查移动逻辑
    movementIssues := v.checkMovementLogic(timeline)
    report.Issues = append(report.Issues, movementIssues...)

    return report
}

// 检查移动逻辑
func (v *TimelineValidator) checkMovementLogic(timeline *TimelineGraph) []TimelineIssue {
    issues := []TimelineIssue{}

    // 获取所有移动事件
    movements := v.extractMovements(timeline)

    for _, movement := range movements {
        // 计算所需时间
        requiredTime := v.calculateTravelTime(movement.From, movement.To)

        // 检查是否有足够时间
        if movement.Duration < requiredTime {
            issues = append(issues, TimelineIssue{
                Type:        "movement_impossible",
                Severity:    "high",
                Description: fmt.Sprintf("从 %s 到 %s 需要至少 %s，但只用了 %s",
                    movement.From, movement.To, requiredTime, movement.Duration),
                Location:    []string{movement.ChapterID},
                Suggestion:  "增加旅行时间或使用魔法/科技手段缩短时间",
            })
        }
    }

    return issues
}
```

---

## 📊 内容质量控制系统

### 1. 重复性检测与优化

```go
// 重复性检测器
type RepetitionDetector struct {
    ngramSize int
    threshold float64
}

// 检测重复内容
func (d *RepetitionDetector) DetectRepetition(content string) *RepetitionReport {
    report := &RepetitionReport{
        Repetitions: []Repetition{},
    }

    // 1. 检测句子结构重复
    sentenceRepetitions := d.detectSentencePatternRepetition(content)
    report.Repetitions = append(report.Repetitions, sentenceRepetitions...)

    // 2. 检测词汇重复
    wordRepetitions := d.detectWordRepetition(content)
    report.Repetitions = append(report.Repetitions, wordRepetitions...)

    // 3. 检测段落结构重复
    paragraphRepetitions := d.detectParagraphPatternRepetition(content)
    report.Repetitions = append(report.Repetitions, paragraphRepetitions...)

    // 4. 检测对话模式重复
    dialogueRepetitions := d.detectDialoguePatternRepetition(content)
    report.Repetitions = append(report.Repetitions, dialogueRepetitions...)

    return report
}

// 检测句子模式重复
func (d *RepetitionDetector) detectSentencePatternRepetition(content string) []Repetition {
    sentences := d.splitSentences(content)
    patterns := make(map[string][]int) // pattern -> occurrence positions

    for i, sentence := range sentences {
        pattern := d.extractSentencePattern(sentence)
        patterns[pattern] = append(patterns[pattern], i)
    }

    repetitions := []Repetition{}
    for pattern, positions := range patterns {
        if len(positions) >= 3 {
            repetitions = append(repetitions, Repetition{
                Type:        "sentence_pattern",
                Pattern:     pattern,
                Count:       len(positions),
                Positions:   positions,
                Suggestion:  "使用不同的句式结构表达相同的意思",
            })
        }
    }

    return repetitions
}

// 提取句子模式（抽象化）
func (d *RepetitionDetector) extractSentencePattern(sentence string) string {
    // 将具体词汇替换为词性标记
    // 例如："他快速地跑向那座古老的建筑"
    // -> "他/副词/动词/那/形容词/名词"

    tagged := d.posTag(sentence)
    pattern := d.abstractize(tagged)
    return pattern
}

// 内容多样性优化器
type DiversityOptimizer struct {
    aiClient AIProvider
}

// 优化重复内容
func (o *DiversityOptimizer) OptimizeRepetition(content string, repetitions []Repetition) (string, []OptimizationSuggestion) {
    suggestions := []OptimizationSuggestion{}

    optimized := content

    for _, rep := range repetitions {
        // 使用 AI 提供多样化建议
        variations := o.aiClient.GenerateVariations(rep.Pattern)

        // 选择最佳替换
        for _, pos := range rep.Positions {
            suggestion := variations[rand.Intn(len(variations))]
            suggestions = append(suggestions, OptimizationSuggestion{
                Position:   pos,
                Original:   rep.OriginalText,
                Suggestion: suggestion,
                Reason:     "增加表达多样性",
            })
        }
    }

    return optimized, suggestions
}
```

---

### 2. 描述空洞检测与增强

```go
// 描述质量分析器
type DescriptionQualityAnalyzer struct {
    aiClient AIProvider
}

// 分析描述质量
func (a *DescriptionQualityAnalyzer) AnalyzeDescriptionQuality(content string) *DescriptionQualityReport {
    report := &DescriptionQualityReport{
        Issues: []DescriptionIssue{},
    }

    // 1. 提取描述片段
    descriptions := a.extractDescriptions(content)

    for _, desc := range descriptions {
        // 检查感官细节
        sensoryIssues := a.checkSensoryDetails(desc)
        report.Issues = append(report.Issues, sensoryIssues...)

        // 检查具体性
        specificityIssues := a.checkSpecificity(desc)
        report.Issues = append(report.Issues, specificityIssues...)

        // 检查意象质量
        imageryIssues := a.checkImageryQuality(desc)
        report.Issues = append(report.Issues, imageryIssues...)
    }

    return report
}

// 检查感官细节
func (a *DescriptionQualityAnalyzer) checkSensoryDetails(desc *Description) []DescriptionIssue {
    issues := []DescriptionIssue{}

    // 统计感官类型
    sensoryCount := map[string]int{
        "visual":    0,
        "auditory":  0,
        "tactile":   0,
        "olfactory": 0,
        "gustatory": 0,
    }

    tokens := a.tokenize(desc.Content)
    for _, token := range tokens {
        if sensoryType := a.getSensoryType(token); sensoryType != "" {
            sensoryCount[sensoryType]++
        }
    }

    // 检查是否过于依赖单一感官
    if sensoryCount["visual"] > len(tokens)*0.8 {
        issues = append(issues, DescriptionIssue{
            Type:        "visual_only",
            Severity:    "medium",
            Description: "描述过于依赖视觉，缺乏其他感官细节",
            Location:    []string{desc.Location},
            Suggestion:  "加入声音、触觉、嗅觉等多感官描述",
        })
    }

    // 检查感官细节是否充足
    totalSensory := sensoryCount["visual"] + sensoryCount["auditory"] + sensoryCount["tactile"] +
        sensoryCount["olfactory"] + sensoryCount["gustatory"]
    if totalSensory < len(tokens)*0.3 {
        issues = append(issues, DescriptionIssue{
            Type:        "insufficient_sensory",
            Severity:    "medium",
            Description: "感官细节不足，描述显得空洞",
            Location:    []string{desc.Location},
            Suggestion:  "增加具体的感官描述词汇",
        })
    }

    return issues
}

// 描述增强器
type DescriptionEnhancer struct {
    aiClient AIProvider
}

// 增强描述
func (e *DescriptionEnhancer) EnhanceDescription(content string) (string, []EnhancementSuggestion) {
    suggestions := []EnhancementSuggestion{}

    // 1. 分析质量
    qualityReport := e.analyzeQuality(content)

    // 2. 对每个问题生成增强建议
    for _, issue := range qualityReport.Issues {
        enhanced := e.aiClient.EnhanceDescription(issue.Content, issue.Type)

        suggestions = append(suggestions, EnhancementSuggestion{
            Location:   issue.Location,
            Original:   issue.Content,
            Enhanced:   enhanced,
            Reason:     issue.Suggestion,
        })
    }

    return content, suggestions
}
```

---

### 3. 对话质量优化

```go
// 对话质量分析器
type DialogueQualityAnalyzer struct {
    aiClient AIProvider
}

// 分析对话质量
func (a *DialogueQualityAnalyzer) AnalyzeDialogueQuality(content string, characterConstraints map[uint]*PersonalityConstraint) *DialogueQualityReport {
    report := &DialogueQualityReport{
        Issues: []DialogueIssue{},
    }

    // 1. 提取对话
    dialogues := a.extractDialogues(content)

    for _, dialogue := range dialogues {
        // 检查自然度
        naturalnessIssues := a.checkNaturalness(dialogue)
        report.Issues = append(report.Issues, naturalnessIssues...)

        // 检查角色独特性
        if constraints, ok := characterConstraints[dialogue.CharacterID]; ok {
            uniquenessIssues := a.checkUniqueness(dialogue, constraints)
            report.Issues = append(report.Issues, uniquenessIssues...)
        }

        // 检查信息倾倒
        infoDumpIssues := a.checkInfoDump(dialogue)
        report.Issues = append(report.Issues, infoDumpIssues...)

        // 检查潜台词
        subtextIssues := a.checkSubtext(dialogue)
        report.Issues = append(report.Issues, subtextIssues...)
    }

    return report
}

// 检查对话自然度
func (a *DialogueQualityAnalyzer) checkNaturalness(dialogue *Dialogue) []DialogueIssue {
    issues := []DialogueIssue{}

    // 检查句子长度
    sentences := a.splitSentences(dialogue.Content)
    avgSentenceLength := a.averageLength(sentences)

    if avgSentenceLength > 20 {
        issues = append(issues, DialogueIssue{
            Type:        "unnatural_length",
            Severity:    "medium",
            Description: "对话句子过长，不够口语化",
            Location:    dialogue.Locations,
            Suggestion:  "拆分长句，使用更简短的口语表达",
        })
    }

    // 检查书面语
    formalMarkers := []string{"因为", "所以", "由于", "因此"}
    for _, marker := range formalMarkers {
        if strings.Contains(dialogue.Content, marker) {
            issues = append(issues, DialogueIssue{
                Type:        "formal_language",
                Severity:    "low",
                Description: fmt.Sprintf("使用了书面语 '%s'，不够口语化", marker),
                Location:    dialogue.Locations,
                Suggestion:  "替换为口语化表达",
            })
        }
    }

    // 检查完整句子（口语常有省略）
    if a.tooComplete(dialogue.Content) {
        issues = append(issues, DialogueIssue{
            Type:        "too_complete",
            Severity:    "low",
            Description: "对话过于完整，缺乏口语的省略和随意性",
            Location:    dialogue.Locations,
            Suggestion:  "加入适当的省略、打断、重复等口语特征",
        })
    }

    return issues
}

// 对话优化器
type DialogueOptimizer struct {
    aiClient AIProvider
}

// 优化对话
func (o *DialogueOptimizer) OptimizeDialogue(content string, characterID uint) (string, []OptimizationSuggestion) {
    suggestions := []OptimizationSuggestion{}

    // 1. 获取角色约束
    constraints := o.getCharacterConstraints(characterID)

    // 2. 分析现有对话
    analysis := o.analyzeDialogue(content, constraints)

    // 3. 生成优化建议
    for _, issue := range analysis.Issues {
        optimized := o.aiClient.OptimizeDialogue(content, issue.Type, constraints)

        suggestions = append(suggestions, OptimizationSuggestion{
            Original:   content,
            Suggestion: optimized,
            Reason:     issue.Suggestion,
        })
    }

    return content, suggestions
}
```

---

## 🧠 逻辑与剧情验证

### 1. 剧情逻辑检查器

```go
// 剧情逻辑检查器
type PlotLogicChecker struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 检查剧情逻辑
func (c *PlotLogicChecker) CheckPlotLogic(novelID uint, chapterNo int, content string) *PlotLogicReport {
    report := &PlotLogicReport{
        Issues: []PlotLogicIssue{},
    }

    // 1. 提取事件序列
    events := c.extractEventSequence(content)

    // 2. 检查因果关系
    causalIssues := c.checkCausality(events)
    report.Issues = append(report.Issues, causalIssues...)

    // 3. 检查角色动机
    motivationIssues := c.checkCharacterMotivation(events, novelID)
    report.Issues = append(report.Issues, motivationIssues...)

    // 4. 检查信息合理性
    infoIssues := c.checkInformationRationality(events, novelID, chapterNo)
    report.Issues = append(report.Issues, infoIssues...)

    // 5. 检查结局合理性
    endingIssues := c.checkEndingRationality(events)
    report.Issues = append(report.Issues, endingIssues...)

    return report
}

// 检查角色动机
func (c *PlotLogicChecker) checkCharacterMotivation(events []Event, novelID uint) []PlotLogicIssue {
    issues := []PlotLogicIssue{}

    // 获取角色动机设定
    characterMotivations := c.getCharacterMotivations(novelID)

    for _, event := range events {
        // 检查每个行为的动机
        if event.ActorID != 0 {
            motivation, ok := characterMotivations[event.ActorID]
            if !ok {
                continue
            }

            // 检查行为是否符合动机
            if !c.actionMatchesMotivation(event.Action, motivation) {
                issues = append(issues, PlotLogicIssue{
                    Type:        "motivation_mismatch",
                    Severity:    "medium",
                    Description: fmt.Sprintf("角色行为 '%s' 与其动机 '%s' 不符", event.Action, motivation.Description),
                    Location:    []string{fmt.Sprintf("章节 %d", event.ChapterNo)},
                    Suggestion:  "增加角色的内在动机或调整行为使其更合理",
                })
            }
        }
    }

    return issues
}

// 检查信息合理性
func (c *PlotLogicChecker) checkInformationRationality(events []Event, novelID uint, chapterNo int) []PlotLogicIssue {
    issues := []PlotLogicIssue{}

    // 获取角色已知信息
    characterKnowledge := c.getCharacterKnowledge(novelID, chapterNo)

    for _, event := range events {
        if event.Type == "discovery" || event.Type == "knowledge_use" {
            characterID := event.ActorID

            // 检查角色是否应该知道这个信息
            knowledge, ok := characterKnowledge[characterID]
            if !ok {
                continue
            }

            if !c.hasAccessToInfo(knowledge, event.Info) {
                issues = append(issues, PlotLogicIssue{
                    Type:        "unrealistic_knowledge",
                    Severity:    "high",
                    Description: fmt.Sprintf("角色在没有合理来源的情况下知道了 '%s'", event.Info),
                    Location:    []string{fmt.Sprintf("章节 %d", chapterNo)},
                    Suggestion:  "增加信息来源的铺垫或调整剧情",
                })
            }
        }
    }

    return issues
}
```

### 2. 伏笔追踪系统

```go
// 伏笔记录
type ForeshadowingRecord struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    ChapterID   uint   `json:"chapter_id"`

    Type        string `json:"type"` // item, event, statement, symbolism
    Content     string `json:"content"`
    Hint        string `json:"hint"` // 伏笔暗示的内容
    TargetChapter *int  `json:"target_chapter"` // 预计在第几章回收

    IsResolved  bool   `json:"is_resolved"`
    ResolvedIn  *uint  `json:"resolved_in"` // 实际回收章节

    Priority    string `json:"priority"` // high, medium, low

    CreatedAt   time.Time `json:"created_at"`
}

// 伏笔管理器
type ForeshadowingManager struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 自动检测伏笔
func (m *ForeshadowingManager) DetectForeshadowing(novelID uint, chapterNo int, content string) []ForeshadowingRecord {
    records := []ForeshadowingRecord{}

    // 使用 AI 检测可能的伏笔
    detected := m.aiClient.DetectForeshadowing(content, novelID)

    for _, item := range detected {
        record := &ForeshadowingRecord{
            NovelID:       novelID,
            ChapterID:     uint(chapterNo),
            Type:          item.Type,
            Content:       item.Content,
            Hint:          item.Hint,
            TargetChapter: item.EstimatedChapter,
            Priority:      item.Priority,
            IsResolved:    false,
        }
        m.db.Create(record)
        records = append(records, *record)
    }

    return records
}

// 检查未回收的伏笔
func (m *ForeshadowingManager) CheckUnresolvedForeshadowings(novelID uint, currentChapter int) *ForeshadowingStatus {
    status := &ForeshadowingStatus{
        Unresolved: []ForeshadowingRecord{},
        Overdue:    []ForeshadowingRecord{},
        Suggestions: []string{},
    }

    // 获取所有未回收的伏笔
    var records []ForeshadowingRecord
    m.db.Where("novel_id = ? AND is_resolved = false", novelID).Find(&records)

    for _, record := range records {
        status.Unresolved = append(status.Unresolved, record)

        // 检查是否逾期
        if record.TargetChapter != nil && currentChapter > *record.TargetChapter {
            status.Overdue = append(status.Overdue, record)
            status.Suggestions = append(status.Suggestions,
                fmt.Sprintf("伏笔 '%s' 原计划在第 %d 章回收，已逾期，建议尽快安排",
                    record.Content, *record.TargetChapter))
        }
    }

    return status
}

// 建议伏笔回收点
func (m *ForeshadowingManager) SuggestForeshadowingResolution(novelID uint, currentChapter int, content string) []ResolutionSuggestion {
    suggestions := []ResolutionSuggestion{}

    // 获取需要回收的伏笔
    unresolved := m.getUnresolvedForeshadowings(novelID)

    // 分析当前章节内容，寻找合适的回收点
    for _, foreshadowing := range unresolved {
        // 检查当前内容是否适合回收
        if m.isSuitableForResolution(content, foreshadowing) {
            suggestion := ResolutionSuggestion{
                ForeshadowingID: foreshadowing.ID,
                Location:        m.suggestLocation(content),
                HowToResolve:    m.suggestResolutionMethod(foreshadowing, content),
            }
            suggestions = append(suggestions, suggestion)
        }
    }

    return suggestions
}
```

---

## 🎨 风格一致性系统

### 1. 风格分析与约束

```go
// 风格配置
type StyleProfile struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`

    // 文风特征
    Tone        string `json:"tone"` // humorous, serious, romantic, dark, etc.
    Voice       string `json:"voice"` // first_person, third_person, omniscient
    Pacing      string `json:"pacing"` // fast, slow, variable

    // 语言风格
    Diction     string `json:"diction"` // formal, casual, poetic, gritty
    SentenceLength string `json:"sentence_length"` // short, medium, long, mixed

    // 叙述风格
    NarrativeTechnique string `json:"narrative_technique"` // linear, non-linear, multiple_pov

    // 禁止元素
    ForbiddenElements string `json:"forbidden_elements" gorm:"type:text"` // JSON

    CreatedAt   time.Time `json:"created_at"`
}

// 风格一致性检查器
type StyleConsistencyChecker struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 检查风格一致性
func (c *StyleConsistencyChecker) CheckStyleConsistency(novelID uint, chapterNo int, content string) *StyleConsistencyReport {
    report := &StyleConsistencyReport{
        Issues: []StyleIssue{},
    }

    // 1. 获取风格配置
    profile := c.getStyleProfile(novelID)

    // 2. 获取历史章节的风格
    historicalStyles := c.getHistoricalStyles(novelID, chapterNo, 5)

    // 3. 分析当前章节风格
    currentStyle := c.analyzeStyle(content)

    // 4. 对比检查
    styleDrift := c.calculateStyleDrift(currentStyle, historicalStyles)

    if styleDrift > 0.6 {
        report.Issues = append(report.Issues, StyleIssue{
            Type:        "style_drift",
            Severity:    "medium",
            Description: fmt.Sprintf("文风偏离度过高（%.2f）", styleDrift),
            Aspects:     c.identifyDriftAspects(currentStyle, historicalStyles),
            Suggestion:  "调整叙述方式，保持与前面章节的风格一致",
        })
    }

    // 5. 检查禁止元素
    forbiddenIssues := c.checkForbiddenElements(content, profile)
    report.Issues = append(report.Issues, forbiddenIssues...)

    return report
}

// 分析章节风格
func (c *StyleConsistencyChecker) analyzeStyle(content string) *StyleFeatures {
    features := &StyleFeatures{}

    // 句子长度分布
    sentences := c.splitSentences(content)
    features.SentenceLengthDistribution = c.calculateLengthDistribution(sentences)

    // 词汇复杂性
    tokens := c.tokenize(content)
    features.VocabularyComplexity = c.calculateComplexity(tokens)

    // 对话比例
    features.DialogueRatio = c.calculateDialogueRatio(content)

    // 描述密度
    features.DescriptionDensity = c.calculateDescriptionDensity(content)

    // 情感基调
    features.EmotionalTone = c.analyzeEmotionalTone(content)

    // 叙述视角
    features.NarrativePOV = c.detectNarrativePOV(content)

    return features
}

// 计算风格偏离度
func (c *StyleConsistencyChecker) calculateStyleDrift(current *StyleFeatures, historical []*StyleFeatures) float64 {
    if len(historical) == 0 {
        return 0
    }

    // 计算每个特征的偏离
    totalDrift := 0.0
    featureCount := 0

    // 句子长度
    avgHistoricalLength := c.average(historical, func(s *StyleFeatures) float64 {
        return s.SentenceLengthDistribution.Average
    })
    lengthDrift := math.Abs(current.SentenceLengthDistribution.Average - avgHistoricalLength) / avgHistoricalLength
    totalDrift += lengthDrift
    featureCount++

    // 对话比例
    avgDialogueRatio := c.average(historical, func(s *StyleFeatures) float64 {
        return s.DialogueRatio
    })
    dialogueDrift := math.Abs(current.DialogueRatio - avgDialogueRatio)
    totalDrift += dialogueDrift
    featureCount++

    // 描述密度
    avgDescDensity := c.average(historical, func(s *StyleFeatures) float64 {
        return s.DescriptionDensity
    })
    descDrift := math.Abs(current.DescriptionDensity - avgDescDensity) / avgDescDensity
    totalDrift += descDrift
    featureCount++

    return totalDrift / float64(featureCount)
}
```

---

## 🤖 AI 生成优化

### 1. 上下文记忆增强

```go
// 长期记忆管理器
type LongTermMemoryManager struct {
    db         *gorm.DB
    vectorStore VectorStore
    cache      *redis.Client
    maxRetrievalDistance int // 最大检索距离（章节数）
}

// 检索长期记忆
func (m *LongTermMemoryManager) RetrieveLongTermMemory(novelID uint, currentChapter int, query string) *MemoryRetrievalResult {
    result := &MemoryRetrievalResult{
        Memories: []MemoryItem{},
    }

    // 1. 向量化查询
    queryVector := m.vectorize(query)

    // 2. 从向量数据库检索
    similarMemories := m.vectorStore.Search(queryVector, 20)

    // 3. 过滤和排序
    for _, memory := range similarMemories {
        // 只检索相关小说的记忆
        if memory.NovelID != novelID {
            continue
        }

        // 只检索不太久远的记忆
        if currentChapter - memory.ChapterNo > m.maxRetrievalDistance {
            continue
        }

        // 计算时间衰减（越近的记忆权重越高）
        timeDecay := m.calculateTimeDecay(currentChapter, memory.ChapterNo)

        result.Memories = append(result.Memories, MemoryItem{
            Content:      memory.Content,
            ChapterNo:    memory.ChapterNo,
            Relevance:    memory.Similarity * timeDecay,
            Type:         memory.Type,
        })
    }

    // 按相关性排序
    sort.Slice(result.Memories, func(i, j int) bool {
        return result.Memories[i].Relevance > result.Memories[j].Relevance
    })

    // 返回前 10 个最相关的记忆
    if len(result.Memories) > 10 {
        result.Memories = result.Memories[:10]
    }

    return result
}

// 压缩长期记忆
func (m *LongTermMemoryManager) CompressLongTermMemory(novelID uint, checkpointChapter int) error {
    // 1. 获取所有记忆
    memories := m.getAllMemories(novelID, checkpointChapter)

    // 2. 生成记忆摘要
    summaries := make(map[int]string)
    for _, memory := range memories {
        chapter := memory.ChapterNo
        summaries[chapter] = m.generateChapterSummary(memories, chapter)
    }

    // 3. 将摘要存储为新记忆
    for chapterNo, summary := range summaries {
        compressedMemory := &KnowledgeBase{
            Type:    "compressed_memory",
            Title:   fmt.Sprintf("第 %d 章记忆摘要", chapterNo),
            Content: summary,
            NovelID: &novelID,
        }

        m.db.Create(compressedMemory)

        // 向量化
        vector := m.vectorize(summary)
        m.vectorStore.Store(vector, compressedMemory)
    }

    return nil
}

// 生成章节摘要
func (m *LongTermMemoryManager) generateChapterSummary(memories []KnowledgeBase, chapterNo int) string {
    // 使用 AI 生成摘要
    content := ""
    for _, memory := range memories {
        content += memory.Content + "\n"
    }

    summary := m.aiClient.SummarizeChapter(content, chapterNo)
    return summary
}
```

### 2. 智能提示词优化

```go
// 提示词优化器
type PromptOptimizer struct {
    aiClient   AIProvider
    vectorStore VectorStore
    config     *OptimizationConfig
}

// 优化生成提示词
func (o *PromptOptimizer) OptimizeGenerationPrompt(
    basePrompt string,
    context *GenerationContext,
    issues []QualityIssue,
) string {
    optimized := basePrompt

    // 1. 根据质量问题调整提示词
    for _, issue := range issues {
        switch issue.Type {
        case "character_consistency":
            optimized = o.addCharacterConstraints(optimized, context)
        case "repetition":
            optimized = o.addDiversityInstruction(optimized)
        case "description_quality":
            optimized = o.addSensoryInstruction(optimized)
        case "dialogue_quality":
            optimized = o.addDialogueStyleInstruction(optimized, context)
        case "style_consistency":
            optimized = o.addStyleConstraint(optimized, context)
        }
    }

    // 2. 添加参考示例
    if o.config.UseReferenceExamples {
        examples := o.retrieveReferenceExamples(context)
        optimized = o.addExamples(optimized, examples)
    }

    // 3. 添加负面约束
    if o.config.UseNegativeConstraints {
        negatives := o.getNegativeConstraints(context)
        optimized = o.addNegativeConstraints(optimized, negatives)
    }

    return optimized
}

// 添加角色约束
func (o *PromptOptimizer) addCharacterConstraints(prompt string, context *GenerationContext) string {
    constraints := ""

    for _, char := range context.ActiveCharacters {
        constraints += fmt.Sprintf("\n【角色 %s 约束】\n", char.Name)
        constraints += fmt.Sprintf("- 性格：%s\n", char.Personality)
        constraints += fmt.Sprintf("- 说话风格：%s\n", char.SpeechStyle)
        constraints += fmt.Sprintf("- 当前状态：%s\n", char.CurrentState)
        constraints += fmt.Sprintf("- 行为约束：%s\n", char.BehaviorConstraints)
    }

    return prompt + "\n" + constraints
}

// 添加多样性指令
func (o *PromptOptimizer) addDiversityInstruction(prompt string) string {
    instruction := `

【写作要求 - 避免重复】
1. 句式结构：使用多样化的句式，避免连续使用相同结构的句子
2. 词汇选择：避免重复使用相同的形容词和动词，使用同义词替换
3. 段落结构：交替使用长短段落，保持节奏变化
4. 动作描写：使用不同的动词表达相似的动作

【检查清单】
- 句式多样性：每个段落至少有 3 种不同的句式结构
- 词汇多样性：连续 500 字内重复词汇不超过 3 次
- 对话模式：每个角色的对话应该有其独特的特征
`
    return prompt + instruction
}

// 添加感官描述指令
func (o *PromptOptimizer) addSensoryInstruction(prompt string) string {
    instruction := `

【写作要求 - 感官描述】
1. 多感官描写：不仅使用视觉，还要融入听觉、触觉、嗅觉、味觉
2. 具体细节：避免笼统的描述，使用具体的细节
3. 意象运用：使用比喻、拟人等修辞增强画面感

【检查清单】
- 视觉：描述颜色、形状、光影等视觉元素
- 听觉：描述声音、音乐、环境音等听觉元素
- 触觉：描述温度、质感、触感等触觉元素
- 嗅觉：描述气味、香味、臭味等嗅觉元素
- 味觉：描述味道、口感等味觉元素
`
    return prompt + instruction
}
```

---

## 📝 人机协作工作流

### 1. 审核与修正系统

```go
// 审核任务
type ReviewTask struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    ChapterID   uint   `json:"chapter_id"`

    Type        string `json:"type"` // consistency, quality, logic, style
    Priority    string `json:"priority"` // high, medium, low

    Issues      string `json:"issues" gorm:"type:text"` // JSON
    Status      string `json:"status"` // pending, in_progress, completed

    AssignedTo  *uint  `json:"assigned_to,omitempty"`
    CompletedAt *time.Time `json:"completed_at,omitempty"`

    CreatedAt   time.Time `json:"created_at"`
}

// 审核管理器
type ReviewManager struct {
    db         *gorm.DB
    aiClient   AIProvider
    notifier   NotificationService
}

// 自动创建审核任务
func (m *ReviewManager) CreateReviewTask(novelID uint, chapterNo int, generatedContent string) {
    // 1. 执行所有质量检查
    report := m.runAllChecks(novelID, chapterNo, generatedContent)

    // 2. 根据问题严重性创建任务
    if report.HasHighPriorityIssues() {
        task := &ReviewTask{
            NovelID:    novelID,
            ChapterID:  uint(chapterNo),
            Type:       "consistency",
            Priority:   "high",
            Issues:     report.HighPriorityIssuesJSON(),
            Status:     "pending",
        }
        m.db.Create(task)

        // 发送通知
        m.notifier.NotifyHighPriorityIssues(task)
    }

    if report.HasMediumPriorityIssues() {
        task := &ReviewTask{
            NovelID:    novelID,
            ChapterID:  uint(chapterNo),
            Type:       "quality",
            Priority:   "medium",
            Issues:     report.MediumPriorityIssuesJSON(),
            Status:     "pending",
        }
        m.db.Create(task)
    }
}

// 执行所有检查
func (m *ReviewManager) runAllChecks(novelID uint, chapterNo int, content string) *ComprehensiveReport {
    report := &ComprehensiveReport{
        ConsistencyIssues: []CharacterIssue{},
        QualityIssues:     []QualityIssue{},
        LogicIssues:       []PlotLogicIssue{},
        StyleIssues:       []StyleIssue{},
    }

    // 1. 一致性检查
    consistencyChecker := NewCharacterConsistencyChecker(m.db, m.aiClient, nil)
    report.ConsistencyIssues = consistencyChecker.CheckConsistency(novelID, chapterNo, content).Issues

    // 2. 质量检查
    qualityAnalyzer := NewDescriptionQualityAnalyzer(m.aiClient)
    report.QualityIssues = qualityAnalyzer.AnalyzeDescriptionQuality(content).Issues

    // 3. 逻辑检查
    logicChecker := NewPlotLogicChecker(m.db, m.aiClient)
    report.LogicIssues = logicChecker.CheckPlotLogic(novelID, chapterNo, content).Issues

    // 4. 风格检查
    styleChecker := NewStyleConsistencyChecker(m.db, m.aiClient)
    report.StyleIssues = styleChecker.CheckStyleConsistency(novelID, chapterNo, content).Issues

    return report
}
```

### 2. 版本控制系统

```go
// 章节版本
type ChapterVersion struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    ChapterID   uint   `json:"chapter_id"`
    VersionNo   int    `json:"version_no"`

    Content     string `json:"content" gorm:"type:text"`

    // 变更记录
    ChangeType  string `json:"change_type"` // generation, manual_edit, ai_revision
    ChangeDescription string `json:"change_description"`
    ChangeAuthorID *uint `json:"change_author_id,omitempty"`

    // 质量指标
    QualityScore float64 `json:"quality_score"`
    ConsistencyScore float64 `json:"consistency_score"`

    CreatedAt   time.Time `json:"created_at"`
}

// 版本管理器
type VersionManager struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 创建新版本
func (m *VersionManager) CreateVersion(chapterID uint, content string, changeType string, description string, authorID *uint) error {
    // 1. 计算版本号
    latestVersion := m.getLatestVersion(chapterID)
    newVersionNo := latestVersion.VersionNo + 1

    // 2. 计算质量指标
    qualityScore := m.calculateQualityScore(content)
    consistencyScore := m.calculateConsistencyScore(content)

    // 3. 创建版本
    version := &ChapterVersion{
        ChapterID:           chapterID,
        VersionNo:           newVersionNo,
        Content:             content,
        ChangeType:          changeType,
        ChangeDescription:   description,
        ChangeAuthorID:      authorID,
        QualityScore:        qualityScore,
        ConsistencyScore:    consistencyScore,
        CreatedAt:           time.Now(),
    }

    m.db.Create(version)

    return nil
}

// 比较版本差异
func (m *VersionManager) CompareVersions(chapterID uint, versionNo1 int, versionNo2 int) *VersionDiff {
    v1 := m.getVersion(chapterID, versionNo1)
    v2 := m.getVersion(chapterID, versionNo2)

    // 计算文本差异
    diff := m.calculateTextDiff(v1.Content, v2.Content)

    return &VersionDiff{
        Version1:     v1,
        Version2:     v2,
        AddedLines:   diff.Added,
        RemovedLines: diff.Removed,
        ModifiedLines: diff.Modified,
        Summary:      m.generateDiffSummary(diff),
    }
}

// 回滚到指定版本
func (m *VersionManager) RollbackToVersion(chapterID uint, versionNo int, authorID uint) error {
    // 1. 获取目标版本
    targetVersion := m.getVersion(chapterID, versionNo)

    // 2. 创建当前版本的备份
    currentVersion := m.getLatestVersion(chapterID)
    m.CreateVersion(chapterID, currentVersion.Content, "backup", "回滚前备份", &authorID)

    // 3. 恢复内容
    chapter := m.getChapter(chapterID)
    chapter.Content = targetVersion.Content
    m.db.Save(chapter)

    // 4. 创建新版本记录回滚
    m.CreateVersion(chapterID, targetVersion.Content, "rollback",
        fmt.Sprintf("回滚到版本 %d", versionNo), &authorID)

    return nil
}
```

---

## 🚀 实施优先级与路线图

### 第一阶段：核心一致性保证（P0）

**时间：4 周**

- [ ] 角色状态快照系统
- [ ] 角色一致性检查器
- [ ] 世界观约束规则系统
- [ ] 世界观约束验证器
- [ ] 时间线追踪系统
- [ ] 时间线验证器

**验收标准：**
- 角色一致性问题检测率 > 85%
- 世界观违反检测率 > 90%
- 时间线冲突检测率 > 95%

### 第二阶段：内容质量控制（P1）

**时间：3 周**

- [ ] 重复性检测器
- [ ] 描述质量分析器
- [ ] 描述增强器
- [ ] 对话质量分析器
- [ ] 对话优化器
- [ ] 风格一致性检查器

**验收标准：**
- 重复内容检测准确率 > 80%
- 空洞描述识别率 > 75%
- 不自然对话检测率 > 70%

### 第三阶段：逻辑与剧情验证（P1）

**时间：3 周**

- [ ] 剧情逻辑检查器
- [ ] 伏笔追踪系统
- [ ] 力量体系验证器
- [ ] 因果关系检查器

**验收标准：**
- 逻辑漏洞检测率 > 75%
- 伏笔遗忘减少 > 80%
- 战力体系一致性问题减少 > 85%

### 第四阶段：AI 生成优化（P2）

**时间：2 周**

- [ ] 长期记忆管理器
- [ ] 提示词优化器
- [ ] 上下文压缩算法
- [ ] 智能参考检索

**验收标准：**
- 上下文记忆保持 > 95%
- 生成质量提升 > 30%
- 重复率降低 > 50%

### 第五阶段：人机协作系统（P2）

**时间：2 周**

- [ ] 审核任务管理器
- [ ] 版本控制系统
- [ ] 自动修复建议系统
- [ ] 审核工作流

**验收标准：**
- 审核效率提升 > 40%
- 版本回滚成功率 100%
- 人工干预减少 > 30%

---

## 📊 预期效果

### 问题解决率预估

| 问题类型 | 当前AI平均 | InkFrame目标 | 提升幅度 |
|---------|-----------|------------|---------|
| 角色不一致 | 60% | 90% | +50% |
| 世界观崩坏 | 55% | 88% | +60% |
| 时间线混乱 | 65% | 95% | +46% |
| 内容重复 | 70% | 92% | +31% |
| 对话不自然 | 65% | 85% | +31% |
| 描述空洞 | 60% | 80% | +33% |
| 剧情漏洞 | 70% | 90% | +29% |
| 伏笔遗忘 | 75% | 95% | +27% |

### 质量指标目标

- **一致性得分**：从 65% 提升到 90%
- **可读性得分**：从 70% 提升到 85%
- **逻辑性得分**：从 68% 提升到 88%
- **风格稳定性**：从 60% 提升到 85%
- **人工修正率**：从 40% 降低到 15%

---

## 🔄 持续改进机制

### 1. 反馈学习系统

```go
// 反馈记录
type FeedbackRecord struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    ChapterID   uint   `json:"chapter_id"`

    FeedbackType string `json:"feedback_type"` // consistency_issue, quality_issue, user_suggestion
    IssueType    string `json:"issue_type"`
    Description  string `json:"description" gorm:"type:text"`

    // 用户评价
    UserRating  *int   `json:"user_rating,omitempty"` // 1-5
    UserComment string `json:"user_comment" gorm:"type:text"`

    // AI 修正
    AIResponse  string `json:"ai_response" gorm:"type:text"`
    WasHelpful  *bool  `json:"was_helpful,omitempty"`

    CreatedAt   time.Time `json:"created_at"`
}

// 反馈学习器
type FeedbackLearner struct {
    db       *gorm.DB
    aiClient AIProvider
}

// 从反馈中学习
func (l *FeedbackLearner) LearnFromFeedback() error {
    // 1. 获取最近的反馈
    feedbacks := l.getRecentFeedbacks(100)

    // 2. 分析反馈模式
    patterns := l.analyzePatterns(feedbacks)

    // 3. 更新检测规则
    for _, pattern := range patterns {
        l.updateDetectionRules(pattern)
    }

    // 4. 优化提示词模板
    l.optimizePromptTemplates(patterns)

    // 5. 调整质量阈值
    l.adjustQualityThresholds(patterns)

    return nil
}
```

### 2. 自适应优化

```go
// 性能监控器
type PerformanceMonitor struct {
    db       *gorm.DB
    metrics  *MetricsCollector
}

// 监控生成质量趋势
func (m *PerformanceMonitor) MonitorQualityTrend(novelID uint) *QualityTrendReport {
    report := &QualityTrendReport{
        Chapters: []ChapterQuality{},
    }

    // 获取所有章节
    chapters := m.getAllChapters(novelID)

    for _, chapter := range chapters {
        // 计算质量指标
        quality := m.calculateChapterQuality(chapter)
        report.Chapters = append(report.Chapters, quality)
    }

    // 分析趋势
    report.Trend = m.analyzeTrend(report.Chapters)
    report.Recommendations = m.generateRecommendations(report.Trend)

    return report
}

// 自动调整生成参数
func (m *PerformanceMonitor) AutoAdjustParameters(novelID uint) error {
    // 1. 获取质量趋势
    trend := m.MonitorQualityTrend(novelID)

    // 2. 根据趋势调整参数
    novel := m.getNovel(novelID)

    if trend.IsDeclining() {
        // 如果质量下降，调整参数
        if trend.TemperatureTooHigh {
            novel.Temperature = math.Max(novel.Temperature*0.9, 0.5)
        }
        if trend.ConsistencyLow {
            novel.MaxTokens = int(float64(novel.MaxTokens) * 0.95)
        }
    } else if trend.IsStable() {
        // 如果质量稳定，尝试优化
        if trend.CanIncreaseCreativity {
            novel.Temperature = math.Min(novel.Temperature*1.05, 0.9)
        }
    }

    m.db.Save(novel)

    return nil
}
```

---

## 📚 总结

本质量控制方案针对 AI 生成小说的主要问题，提供了：

1. **全面的一致性保证**：角色、世界观、时间线三重保障
2. **细粒度的质量检测**：重复性、描述、对话、风格全方位检查
3. **智能的优化机制**：提示词优化、上下文记忆、参考检索
4. **高效的人机协作**：审核工作流、版本控制、自动修复建议
5. **持续的学习改进**：反馈学习、自适应优化

通过这套系统，InkFrame 能够大幅提升 AI 生成小说的质量和一致性，让用户能够更轻松地创作出高质量的作品。
