package service

import (
	"encoding/json"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

type DramaTemplateService struct {
	repo *repository.DramaTemplateRepository
}

func NewDramaTemplateService(repo *repository.DramaTemplateRepository) *DramaTemplateService {
	return &DramaTemplateService{repo: repo}
}

func (s *DramaTemplateService) List() ([]*model.DramaTemplate, error) {
	return s.repo.List()
}

func (s *DramaTemplateService) GetByID(id uint) (*model.DramaTemplate, error) {
	return s.repo.GetByID(id)
}

func (s *DramaTemplateService) Create(t *model.DramaTemplate) error {
	return s.repo.Create(t)
}

func (s *DramaTemplateService) Update(id uint, t *model.DramaTemplate) error {
	existing, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	if existing.IsBuiltin {
		return fmt.Errorf("内置模板不可修改")
	}
	t.ID = id
	return s.repo.Update(t)
}

func (s *DramaTemplateService) Delete(id uint) error {
	existing, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	if existing.IsBuiltin {
		return fmt.Errorf("内置模板不可删除")
	}
	return s.repo.Delete(id)
}

// BuildOutlineInjection returns a string block to inject into novel outline prompts.
func (s *DramaTemplateService) BuildOutlineInjection(templateID uint) (string, error) {
	t, err := s.repo.GetByID(templateID)
	if err != nil {
		return "", err
	}

	var acts map[string]interface{}
	if t.ThreeActBeats != "" {
		_ = json.Unmarshal([]byte(t.ThreeActBeats), &acts)
	}

	var archetypes map[string]string
	if t.CharacterArchetypes != "" {
		_ = json.Unmarshal([]byte(t.CharacterArchetypes), &archetypes)
	}

	injection := fmt.Sprintf(`
## 🎬 短剧类型模板：%s（%s）
**核心钩子**：%s
%s

**三幕六转折骨架（必须按此结构规划章节节点）**：
%s

**角色原型参考**：
`, t.Name, t.Genre, t.CoreHook, t.Description, t.ThreeActBeats)

	for role, desc := range archetypes {
		injection += fmt.Sprintf("- %s：%s\n", role, desc)
	}

	if t.KeyTriggers != "" {
		injection += fmt.Sprintf("\n**爆款关键场景节点**（必须在大纲中出现）：%s\n", t.KeyTriggers)
	}

	return injection, nil
}

// SeedBuiltinTemplates writes the 4 builtin drama templates to DB (idempotent).
func SeedBuiltinTemplates(repo *repository.DramaTemplateRepository) {
	templates := builtinTemplates()
	for _, t := range templates {
		_ = repo.Upsert(t)
	}
}

func builtinTemplates() []*model.DramaTemplate {
	archetypesJSON := func(p, a, l, s string) string {
		b, _ := json.Marshal(map[string]string{
			"protagonist":   p,
			"antagonist":    a,
			"love_interest": l,
			"sidekick":      s,
		})
		return string(b)
	}

	emotionCurve := func(vals []float64) string {
		b, _ := json.Marshal(vals)
		return string(b)
	}

	return []*model.DramaTemplate{
		{
			Name:        "霸总逆袭",
			Genre:       "都市",
			CoreHook:    "身份落差→认知反转→持续爽感",
			Description: "主角在最低点受尽屈辱，随着真实身份/能力逐渐揭露，完成一次次对施压者的降维打击。情绪节奏：压→爽→压→大爽。",
			ThreeActBeats: `{"act1_setup":"主角身处最低点，被误解/欺压，表现出隐忍","act1_inciting":"触发事件：意外展示真实身份或能力，引发第一次冲突","act1_turn":"敌人震惊，主角获得第一个胜利但危机未解","act2_rising":"连续打脸场景，每次升级对手等级，感情线同步推进","act2_midpoint":"背叛揭露或更强对手出现，主角面临最大危机","act2_dark":"主角被逼到绝境，情感与处境双重低谷","act3_climax":"终极对决，霸气反杀，身份彻底曝光","act3_resolution":"所有反派认输，感情线全面收线","act3_denouement":"新身份新生活，展现巅峰状态"}`,
			CharacterArchetypes: archetypesJSON("dominant_ceo", "scheming_rival", "pure_heroine", "loyal_friend"),
			EmotionCurveTemplate: emotionCurve([]float64{3, 4, 7, 5, 8, 5, 6, 9, 6, 10}),
			KeyTriggers: "真实身份揭露,当众打脸,降维打击,霸气发言,保护关键人物,终极翻盘",
			IsBuiltin:   true,
		},
		{
			Name:        "替嫁甜宠",
			Genre:       "古风/现代",
			CoreHook:    "错误身份→真情萌发→甜虐交替",
			Description: "女主阴差阳错嫁入/进入男主世界，在误会与真相交替中感情升温。节奏：甜→虐→更甜→高虐→超甜收尾。",
			ThreeActBeats: `{"act1_setup":"女主被迫替代他人进入男主生活，男主态度冷漠/敌意","act1_inciting":"意外接触产生第一次心动瞬间，双方各自否认","act1_turn":"误会加深，感情暗流涌动","act2_rising":"甜蜜互动积累，外部障碍出现（原配/家族/阴谋）","act2_midpoint":"甜蜜高峰后真相揭露（替嫁身份/秘密），关系危机","act2_dark":"男主误解女主，女主决定离开，高虐顶点","act3_climax":"男主意识到感情，用行动证明真心","act3_resolution":"误会解开，明确表白，障碍清除","act3_denouement":"正式在一起，甜蜜日常收尾"}`,
			CharacterArchetypes: archetypesJSON("pure_heroine", "scheming_rival", "dominant_ceo", "comic_relief"),
			EmotionCurveTemplate: emotionCurve([]float64{4, 6, 7, 8, 6, 4, 3, 7, 9, 10}),
			KeyTriggers: "意外心动,保护瞬间,误会吃醋,替嫁真相揭露,深夜告白,护短名场面",
			IsBuiltin:   true,
		},
		{
			Name:        "重生复仇",
			Genre:       "都市/古风/穿越",
			CoreHook:    "先知视角+爽感反击+因果报应",
			Description: "主角重生归来，手握前世记忆，提前布局逐步瓦解所有敌人。观众代入感极强，每次预判成真都是爽点。",
			ThreeActBeats: `{"act1_setup":"重生时刻，主角确认时间节点，立刻开始改变命运","act1_inciting":"第一个伏笔行动：阻止前世某个惨剧/危机","act1_turn":"前世仇人不知情地接近，主角表面配合暗中布局","act2_rising":"逐步积累资源/人脉，连续反制小反派","act2_midpoint":"大反派意识到异常，开始反击；主角情感线推进","act2_dark":"主角布局被部分识破，身处险境，情感线危机","act3_climax":"最终局揭晓，前世之仇一一清算","act3_resolution":"主谋伏法，所有冤屈昭雪","act3_denouement":"珍惜此生，与重要的人共建新生"}`,
			CharacterArchetypes: archetypesJSON("reborn_villain", "scheming_rival", "loyal_friend", "comic_relief"),
			EmotionCurveTemplate: emotionCurve([]float64{5, 7, 8, 7, 8, 5, 4, 9, 8, 10}),
			KeyTriggers: "重生觉醒,提前预知,反杀前世仇人,揭穿阴谋,保护珍视之人,终极清算",
			IsBuiltin:   true,
		},
		{
			Name:        "赘婿觉醒",
			Genre:       "都市",
			CoreHook:    "持续压迫→集中爆发→身份降维打击",
			Description: "主角以赘婿/上门女婿身份忍辱负重，在极度压迫后爆发，用真实身份完成对所有轻视者的压制。爽感来自压迫积累的释放。",
			ThreeActBeats: `{"act1_setup":"主角被丈母家/妻子全面看不起，承受各种羞辱","act1_inciting":"外部事件迫使主角不得不展示部分能力","act1_turn":"小范围翻身，但家人仍不信任，矛盾升级","act2_rising":"一边隐忍维持伪装，一边解决更大危机，多线并进","act2_midpoint":"身份即将被揭穿，情感线危机同步","act2_dark":"被家人当众羞辱至最低点，主角决定不再忍","act3_climax":"全面爆发，真实身份/能力彻底曝光","act3_resolution":"所有轻视者震惊认输，妻子理解/支持","act3_denouement":"平等相处，以真实面貌被接受"}`,
			CharacterArchetypes: archetypesJSON("dominant_ceo", "scheming_rival", "pure_heroine", "loyal_friend"),
			EmotionCurveTemplate: emotionCurve([]float64{2, 3, 5, 4, 6, 3, 2, 8, 9, 10}),
			KeyTriggers: "忍辱负重,当众羞辱,隐藏实力,意外出手,身份曝光,全场震惊",
			IsBuiltin:   true,
		},
	}
}
