package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// PromptFilter 在图像生成 API 调用前对 prompt 进行敏感词过滤。
// 采用两层规则：
//   - 内置基线规则（hardcoded，覆盖常见暴力/伤亡词汇）
//   - DB 可配置规则（管理员通过 API 增删改，Redis 缓存 60 秒）
type PromptFilter struct {
	repo  *repository.SensitiveWordRuleRepository
	mu    sync.RWMutex
	rules []model.SensitiveWordRule // 缓存的 DB 规则
	exp   time.Time                 // 缓存过期时间
}

func NewPromptFilter(repo *repository.SensitiveWordRuleRepository) *PromptFilter {
	return &PromptFilter{repo: repo}
}

// Apply 对 prompt 依次应用内置规则和 DB 规则，返回净化后的文本。
func (f *PromptFilter) Apply(prompt string) string {
	prompt = applyBuiltinRules(prompt)
	if f.repo != nil {
		prompt = f.applyDBRules(context.Background(), prompt)
	}
	return prompt
}

// HasSensitiveContent 快速检查 prompt 是否命中已知敏感词（不做替换）。
func (f *PromptFilter) HasSensitiveContent(prompt string) bool {
	return f.Apply(prompt) != prompt
}

// ─── 内置基线规则 ────────────────────────────────────────────────────────────

// builtinReplacer 是不可变的全局 replacer，进程启动时初始化一次。
var builtinReplacer = strings.NewReplacer(
	// ── 暴力/杀伤 ──
	"杀人", "战斗",
	"杀死", "击败",
	"杀掉", "击败",
	"杀害", "击败",
	"砍死", "击倒",
	"刺死", "刺击",
	"射杀", "射击",
	"暗杀", "偷袭",
	"屠杀", "激战",
	"虐杀", "战斗",
	"残杀", "击败",
	// ── 伤亡/血腥 ──
	"鲜血", "红色液体",
	"血腥", "激烈",
	"血流", "液体流淌",
	"血迹", "红色痕迹",
	"流血", "受伤",
	"伤口", "痕迹",
	"伤疤", "痕迹",
	"伤痕", "痕迹",
	"重伤", "受伤",
	"致命伤", "伤势",
	"断肢", "受伤",
	"残缺", "独特",
	"尸体", "倒地",
	"死尸", "倒地者",
	"遗体", "人物",
	"死亡", "消逝",
	"死去", "离开",
	"死了", "倒下",
	// ── 武器/凶器（单独出现时保留，作为组合词出现时替换） ──
	"凶器", "武器",
	"刺穿", "刺击",
	"砍断", "劈砍",
	"穿透", "穿过",
	"贯穿", "穿过",
	// ── 爆炸/破坏 ──
	"爆炸", "冲击波",
	"炸弹", "装置",
	"爆破", "冲击",
	"毒药", "液体",
	"投毒", "施毒",
	// ── 其他风险词 ──
	"残忍", "严肃",
	"暴力", "力量",
	"恐怖", "震撼",
	"血祭", "仪式",
	"人祭", "仪式",
	"活祭", "仪式",
)

func applyBuiltinRules(prompt string) string {
	return builtinReplacer.Replace(prompt)
}

// ─── DB 可配置规则 ────────────────────────────────────────────────────────────

const dbRulesCacheTTL = 60 * time.Second

func (f *PromptFilter) applyDBRules(ctx context.Context, prompt string) string {
	rules := f.getCachedRules(ctx)
	if len(rules) == 0 {
		return prompt
	}
	pairs := make([]string, 0, len(rules)*2)
	for _, r := range rules {
		if r.Word == "" {
			continue
		}
		repl := r.Replacement
		pairs = append(pairs, r.Word, repl)
	}
	if len(pairs) == 0 {
		return prompt
	}
	return strings.NewReplacer(pairs...).Replace(prompt)
}

func (f *PromptFilter) getCachedRules(ctx context.Context) []model.SensitiveWordRule {
	f.mu.RLock()
	if time.Now().Before(f.exp) {
		rules := f.rules
		f.mu.RUnlock()
		return rules
	}
	f.mu.RUnlock()

	f.mu.Lock()
	defer f.mu.Unlock()
	// double-check after write lock
	if time.Now().Before(f.exp) {
		return f.rules
	}
	rules, err := f.repo.ListEnabled(ctx)
	if err != nil {
		// 查询失败时保留旧缓存（不清空），等下次再试
		f.exp = time.Now().Add(5 * time.Second)
		return f.rules
	}
	f.rules = rules
	f.exp = time.Now().Add(dbRulesCacheTTL)
	return f.rules
}
