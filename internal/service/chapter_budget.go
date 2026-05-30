package service

import (
	"fmt"
	"strings"
)

// ChapterBudget 章节叙事预算 — 根据章节在全书中的结构位置，限制每章可引入/解决的叙事元素量。
//
// 核心目的：防止 AI 在前期章节过度引入 MacGuffin、过早解决主要矛盾、不合时宜地引入导师等，
// 保证故事按三幕结构有序推进，使节奏张弛有据。
type ChapterBudget struct {
	// ── 结构位置 ──────────────────────────────────────────────────
	StoryPhase    string  // "setup" | "rising" | "climax" | "resolution"
	ActLabel      string  // "第一幕" | "第二幕前半" | "第二幕后半" | "第三幕"
	PositionRatio float64 // 0.0–1.0，当前章在全书中的相对位置

	// ── 新元素引入限制 ────────────────────────────────────────────
	MaxNewMacGuffins      int // 本章最多引入几个新"关键道具/地图/秘密信物"
	MaxNewNamedCharacters int // 本章最多引入几个新命名角色（不含纯路人）
	MaxNewSecrets         int // 本章最多设置几个新悬念/秘密（不含已有伏笔的延续）

	// ── 冲突解决限制 ──────────────────────────────────────────────
	MaxConflictsResolved        int  // 本章最多解决几个已有次要冲突
	AllowMainConflictResolution bool // 核心矛盾能否在本章内获得实质解决

	// ── 重要事件许可 ──────────────────────────────────────────────
	AllowMentorFirstAppear bool // 导师型人物能否在本章首次出场（前两章不允许）
	AllowAbilityGain       bool // 主角能否在本章获得重要新能力/神兵/技法
	AllowBigRevelation     bool // 能否出现颠覆性大揭秘（改变读者对故事认知的信息）

	// ── 结构使命 ──────────────────────────────────────────────────
	StructuralMission string // 本章在宏观结构中的核心任务（一句话说清）
	PacingGuidance    string // 节奏建议："缓慢建立" | "稳步推进" | "快节奏冲突" | "高潮爆发" | "收尾沉淀"
}

// computeChapterBudget 根据章节号、全书目标章节数和幕次，计算本章的叙事预算。
//
//   - chapterNo:      当前章节号（从1开始）
//   - targetChapters: 全书目标章节数（0表示未知，退化为宽松限制）
//   - actNo:          当前幕次（1/2/3，0表示未知）
func computeChapterBudget(chapterNo, targetChapters, actNo int) ChapterBudget {
	b := ChapterBudget{}

	// 计算相对位置（无法确定时按宽松处理）
	if targetChapters <= 0 {
		targetChapters = 100 // 默认100章
	}
	b.PositionRatio = float64(chapterNo) / float64(targetChapters)
	if b.PositionRatio > 1.0 {
		b.PositionRatio = 1.0
	}

	// 优先用 actNo 判断阶段，actNo 为0时退化为位置比例
	switch {
	case actNo == 1 || (actNo == 0 && b.PositionRatio < 0.25):
		// ── 第一幕：建立期（前25%）──────────────────────────────
		b.StoryPhase = "setup"
		b.ActLabel = "第一幕"
		b.MaxNewMacGuffins = 1      // 每章最多1个新线索道具（前两章甚至应更少）
		b.MaxNewNamedCharacters = 2 // 逐步引入配角，不超过2个
		b.MaxNewSecrets = 1         // 悬念要留到后面，不要全倒出去
		b.MaxConflictsResolved = 0  // 建立期不应解决任何冲突
		b.AllowMainConflictResolution = false
		b.AllowMentorFirstAppear = chapterNo > 2       // 前两章不允许导师出场
		b.AllowAbilityGain = false                     // 建立期主角尚未开始成长跃迁
		b.AllowBigRevelation = chapterNo > 5            // 至少第5章后才有大揭秘
		b.StructuralMission = "建立普通世界，展示主角的日常处境与性格弱点，引入破坏平衡的关键事件（核心矛盾的萌芽），不解决任何问题"
		b.PacingGuidance = "缓慢建立"

		// 前两章约束更严
		if chapterNo <= 2 {
			b.MaxNewMacGuffins = 1
			b.MaxNewNamedCharacters = 1
			b.AllowBigRevelation = false
		}

	case actNo == 2 || (actNo == 0 && b.PositionRatio < 0.75):
		// ── 第二幕：发展期（25%–75%）────────────────────────────
		if b.PositionRatio < 0.5 {
			b.StoryPhase = "rising"
			b.ActLabel = "第二幕前半"
			b.MaxNewMacGuffins = 2
			b.MaxNewNamedCharacters = 2
			b.MaxNewSecrets = 2
			b.MaxConflictsResolved = 1 // 允许解决小冲突，主冲突不得解决
			b.AllowMainConflictResolution = false
			b.AllowMentorFirstAppear = true
			b.AllowAbilityGain = true  // 可以开始获得小能力/资源
			b.AllowBigRevelation = b.PositionRatio > 0.4 // 中点前后可以有中揭秘
			b.StructuralMission = "主角行动受阻，代价上升，引入主要拮抗力量，主角尝试解决但暂时失败，赌注不断提高"
			b.PacingGuidance = "稳步推进"
		} else {
			b.StoryPhase = "climax"
			b.ActLabel = "第二幕后半"
			b.MaxNewMacGuffins = 1  // 高潮期减少新元素，聚焦已有线索
			b.MaxNewNamedCharacters = 1
			b.MaxNewSecrets = 1
			b.MaxConflictsResolved = 2
			b.AllowMainConflictResolution = b.PositionRatio > 0.70 // 接近高潮才能开始解决
			b.AllowMentorFirstAppear = true
			b.AllowAbilityGain = true
			b.AllowBigRevelation = true // 高潮期可以有大揭秘
			b.StructuralMission = "矛盾激化到顶点，最大危机爆发，主角面临最艰难的抉择，一切看似无望（暗最低点）"
			b.PacingGuidance = "快节奏冲突"
		}

	default:
		// ── 第三幕：收尾期（后25%）──────────────────────────────
		b.StoryPhase = "resolution"
		b.ActLabel = "第三幕"
		b.MaxNewMacGuffins = 0  // 收尾期不再引入新线索
		b.MaxNewNamedCharacters = 0
		b.MaxNewSecrets = 0
		b.MaxConflictsResolved = 3 // 需要逐步收束所有线索
		b.AllowMainConflictResolution = true
		b.AllowMentorFirstAppear = false // 收尾期不引入新人物
		b.AllowAbilityGain = false
		b.AllowBigRevelation = b.PositionRatio < 0.90 // 最后几章不要再有大揭秘
		b.StructuralMission = "收束所有冲突线索，主角完成内在蜕变，世界恢复新的平衡，给读者情感上的完整感"
		b.PacingGuidance = "收尾沉淀"
	}

	return b
}

// formatBudgetForPrompt 将 ChapterBudget 格式化为可注入 prompt 的文本。
func formatBudgetForPrompt(b ChapterBudget) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("**结构位置**：第%.0f%%（%s / %s）\n",
		b.PositionRatio*100, b.StoryPhase, b.ActLabel))
	sb.WriteString(fmt.Sprintf("**节奏基调**：%s\n", b.PacingGuidance))
	sb.WriteString(fmt.Sprintf("**本章结构使命**：%s\n\n", b.StructuralMission))

	sb.WriteString("**元素引入上限**（超出即为违规）：\n")
	sb.WriteString(fmt.Sprintf("- 新关键道具/地图/信物/线索（MacGuffin）：≤ %d 个\n", b.MaxNewMacGuffins))
	sb.WriteString(fmt.Sprintf("- 新命名角色（有名字、有功能的配角）：≤ %d 个\n", b.MaxNewNamedCharacters))
	sb.WriteString(fmt.Sprintf("- 新悬念/秘密设置：≤ %d 个\n", b.MaxNewSecrets))

	sb.WriteString("\n**冲突解决约束**：\n")
	sb.WriteString(fmt.Sprintf("- 次要冲突最多解决 %d 个\n", b.MaxConflictsResolved))
	if b.AllowMainConflictResolution {
		sb.WriteString("- ✅ 本章**允许**对核心矛盾有实质性推进或解决\n")
	} else {
		sb.WriteString("- ❌ 本章**禁止**解决核心矛盾，主角必须在章末仍面临核心困境\n")
	}

	sb.WriteString("\n**重要事件许可**：\n")
	if b.AllowMentorFirstAppear {
		sb.WriteString("- ✅ 可以引入导师/高人型人物（首次出场）\n")
	} else {
		sb.WriteString("- ❌ 禁止导师/高人型人物首次出场（时机未到）\n")
	}
	if b.AllowAbilityGain {
		sb.WriteString("- ✅ 主角可以获得新能力/技法/重要资源（须有代价）\n")
	} else {
		sb.WriteString("- ❌ 本章主角不得获得新的重要能力或神兵（尚在建立期）\n")
	}
	if b.AllowBigRevelation {
		sb.WriteString("- ✅ 可以安排颠覆性大揭秘（改变读者认知的关键信息）\n")
	} else {
		sb.WriteString("- ❌ 本章禁止颠覆性大揭秘，请以局部悬念代替（时机未成熟）\n")
	}

	return sb.String()
}
