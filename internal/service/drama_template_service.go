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
		// ── 扩展主流模板 ────────────────────────────────────────────────────────
		{
			Name:        "马甲大佬",
			Genre:       "都市",
			CoreHook:    "多重隐藏身份→逐层揭露→认知颠覆",
			Description: "主角同时拥有多个低调隐藏的顶级身份（大佬/天才/王者），在不同场景下被迫一层层揭开，每次揭露都是爽感高潮。",
			ThreeActBeats: `{"act1_setup":"主角以最普通身份示人，被周围人小瞧","act1_inciting":"第一个马甲被意外揭露，小范围震惊","act1_turn":"主角换场景重新低调，继续维持另一个马甲","act2_rising":"不同领域的人开始怀疑，多线追查主角真实身份","act2_midpoint":"两个马甲同时暴露，追查者迷茫，主角从容应对","act2_dark":"核心秘密即将全面曝光，有人利用此威胁主角","act3_climax":"主角主动亮出所有身份，全场降维碾压","act3_resolution":"各方势力臣服，最重要的人全面了解主角","act3_denouement":"无需伪装，以真实面貌站在最高处"}`,
			CharacterArchetypes: archetypesJSON("hidden_genius", "scheming_rival", "loyal_partner", "comic_pursuer"),
			EmotionCurveTemplate: emotionCurve([]float64{3, 5, 7, 6, 8, 6, 5, 9, 8, 10}),
			KeyTriggers: "马甲被扒,当场打脸,多重身份同时在线,全场震惊,从容亮出底牌,终极身份曝光",
			IsBuiltin:   true,
		},
		{
			Name:        "闪婚蜜爱",
			Genre:       "都市",
			CoreHook:    "陌生人→契约夫妻→真情萌生",
			Description: "男女主因各自目的闪婚，从利益结合到真情渐生。日常相处中的心动细节是核心看点，甜度持续攀升，中途一次高虐考验感情真实性。",
			ThreeActBeats: `{"act1_setup":"双方因各自目的签订婚姻契约，约定不干涉彼此","act1_inciting":"第一次意外肌肤之亲/共同危机，心跳产生","act1_turn":"双方都在否认感情，但行动出卖内心","act2_rising":"日常相处甜蜜升温，外人误解加速感情确认","act2_midpoint":"契约将到期或原始目的曝光，信任危机","act2_dark":"分离或冷战，双方意识到已离不开对方","act3_climax":"一方主动打破契约壁垒，真情告白","act3_resolution":"契约结束，真实婚姻开始","act3_denouement":"相互坦诚所有秘密，幸福收尾"}`,
			CharacterArchetypes: archetypesJSON("cold_ceo", "independent_heroine", "scheming_ex", "gossip_friend"),
			EmotionCurveTemplate: emotionCurve([]float64{4, 6, 7, 8, 9, 5, 3, 7, 9, 10}),
			KeyTriggers: "意外心动,吃醋名场面,误会吻戏,契约曝光,分离虐心,真情告白",
			IsBuiltin:   true,
		},
		{
			Name:        "豪门千金",
			Genre:       "都市",
			CoreHook:    "隐藏富贵→身份觉醒→阶层降维",
			Description: "女主看似普通甚至落魄，实则是隐藏的顶级豪门/权贵出身，在被人看不起的环境中逐步觉醒，用真实背景完成对所有人的碾压。",
			ThreeActBeats: `{"act1_setup":"女主身处弱势环境，被误解为普通人，遭受轻视","act1_inciting":"危机迫使女主动用部分家族资源，露出冰山一角","act1_turn":"部分人开始怀疑女主身份，女主继续低调","act2_rising":"女主逐步用身份/资源解决一个个问题，每次都更惊人","act2_midpoint":"有人找到家族线索，威胁女主，男主成为保护者","act2_dark":"家族陷入危机，女主被迫全面亮出身份","act3_climax":"豪门之力全面展示，碾压所有对手","act3_resolution":"回归家族，所有轻视者俯首","act3_denouement":"以真实身份与爱人平等相爱，开启新篇"}`,
			CharacterArchetypes: archetypesJSON("hidden_heiress", "scheming_socialite", "protective_ceo", "loyal_assistant"),
			EmotionCurveTemplate: emotionCurve([]float64{3, 4, 6, 7, 8, 5, 3, 9, 8, 10}),
			KeyTriggers: "身份初露,豪门气场,当众碾压,家族援助,终极亮身份,全场跪服",
			IsBuiltin:   true,
		},
		{
			Name:        "古风权谋",
			Genre:       "古风",
			CoreHook:    "步步为营→朝堂博弈→权情两得",
			Description: "架空古代背景，主角在权力斗争与感情纠葛中双线并进，智谋与情感同等重要。节奏紧凑，每集都有反转，权谋爽感与虐恋并重。",
			ThreeActBeats: `{"act1_setup":"主角身处危局，朝堂险象，感情线埋下伏笔","act1_inciting":"第一次展现谋略，成功化解第一个危机","act1_turn":"权贵注意到主角，势力角力开始，感情初动","act2_rising":"连续朝堂博弈，每次均以智取胜，感情线升温","act2_midpoint":"最强对手揭晓，情感线因误解/选择陷入危机","act2_dark":"局势最危险时刻，主角孤立无援，情感破裂","act3_climax":"最终决策，奇谋定胜负，同时完成情感和解","act3_resolution":"大局已定，奸佞伏法","act3_denouement":"权与情皆得，共治山河"}`,
			CharacterArchetypes: archetypesJSON("strategist_hero", "power_villain", "strong_female_lead", "loyal_subordinate"),
			EmotionCurveTemplate: emotionCurve([]float64{4, 6, 7, 6, 8, 4, 3, 9, 8, 10}),
			KeyTriggers: "奇谋破局,朝堂打脸,情感初动,误会决裂,孤注一掷,终局反转",
			IsBuiltin:   true,
		},
		{
			Name:        "系统金手指",
			Genre:       "都市/玄幻",
			CoreHook:    "挂机升级→碾压全场→人生开挂",
			Description: "主角获得系统/金手指/空间，从最底层开始利用外挂优势逐步碾压，每次使用都带来爽感反转。观众代入感极强，期待下一次爽点。",
			ThreeActBeats: `{"act1_setup":"主角在人生最低谷绑定系统，获得第一个隐秘优势","act1_inciting":"第一次偷偷使用金手指，解决危机，旁人不知原因","act1_turn":"主角开始主动利用系统积累资源，悄悄超越众人","act2_rising":"系统能力升级，主角在不同领域陆续碾压各路强者","act2_midpoint":"系统暴露风险或出现限制，主角面临选择","act2_dark":"强敌或危机超出系统能力，主角陷入绝境","act3_climax":"系统满级或主角自身超越系统，终极爽翻","act3_resolution":"所有挑战者臣服，开挂人生全面展开","act3_denouement":"主角已无需依赖外挂，实力即一切"}`,
			CharacterArchetypes: archetypesJSON("system_host", "jealous_rival", "supportive_partner", "system_ai"),
			EmotionCurveTemplate: emotionCurve([]float64{2, 5, 7, 8, 7, 5, 3, 8, 9, 10}),
			KeyTriggers: "系统觉醒,首次开挂,能力升级,全场震惊,系统危机,终极满级碾压",
			IsBuiltin:   true,
		},
		{
			Name:        "偏执霸总",
			Genre:       "都市",
			CoreHook:    "强占心动→偏执守护→爱恨交织",
			Description: "男主强势偏执，因女主触动内心封印而陷入执念，从单方面占有到真正理解。虐点来自男主的错误方式，甜点来自他的独家宠溺。",
			ThreeActBeats: `{"act1_setup":"女主误闯男主世界，男主被触动却以错误方式靠近","act1_inciting":"男主第一次护住女主，却用控制方式表达","act1_turn":"女主反抗，男主偏执升级，爱恨拉锯开始","act2_rising":"男主用偏执方式守护女主，同时伤害她","act2_midpoint":"女主了解男主创伤，心防松动，但仍受伤","act2_dark":"男主做出最伤女主的事，女主决心离开","act3_climax":"男主意识到错误，用行动而非控制证明爱意","act3_resolution":"女主接受改变后的男主，感情确立","act3_denouement":"男主学会正确表达爱，给予而非占有"}`,
			CharacterArchetypes: archetypesJSON("possessive_ceo", "scheming_third_party", "independent_heroine", "understanding_friend"),
			EmotionCurveTemplate: emotionCurve([]float64{4, 6, 5, 7, 8, 4, 2, 6, 8, 10}),
			KeyTriggers: "独家宠溺,偏执守护,错误伤害,女主反击,男主崩溃,正确告白",
			IsBuiltin:   true,
		},
		{
			Name:        "娱乐圈逆袭",
			Genre:       "都市",
			CoreHook:    "黑料压制→实力打脸→顶流蜕变",
			Description: "主角在娱乐圈从被黑、被压制、被质疑开始，凭借真实实力一步步逆袭成顶流，感情线与事业线双线并进，每个作品发布都是爽感节点。",
			ThreeActBeats: `{"act1_setup":"主角遭遇黑料/潜规则/被压，事业跌入谷底","act1_inciting":"一个意外机会让主角实力被小范围看见","act1_turn":"资本势力打压，主角决心用作品说话","act2_rising":"连续拿出令人震惊的作品/表演，路人转粉浪潮","act2_midpoint":"幕后黑手揭晓，感情线危机（被怀疑利用/欺骗）","act2_dark":"主角被全网黑到最低，连最亲近的人都怀疑","act3_climax":"终极舞台/作品发布，全面逆转舆论","act3_resolution":"幕后黑手伏法，实力获得全行业认可","act3_denouement":"携手爱人，成为真正的顶流"}`,
			CharacterArchetypes: archetypesJSON("rising_star", "industry_villain", "supportive_partner", "fan_friend"),
			EmotionCurveTemplate: emotionCurve([]float64{2, 4, 6, 7, 8, 5, 2, 8, 9, 10}),
			KeyTriggers: "实力震惊全场,路人转粉,黑料反转,幕后黑手揭晓,终极逆袭,全网爆款",
			IsBuiltin:   true,
		},
		{
			Name:        "军婚强宠",
			Genre:       "现代",
			CoreHook:    "硬汉柔情→护妻如命→热血爱情",
			Description: "冷峻军人与平凡/坚强女主的婚姻，表面冷漠内心守护，用行动而非言语表达爱意。热血场景与甜宠并重，危机时男主的守护是最大爽点。",
			ThreeActBeats: `{"act1_setup":"军婚开始，男主表面冷淡，女主努力适应","act1_inciting":"第一次危机男主毫不犹豫挺身守护","act1_turn":"女主被男主行动打动，男主开始主动靠近","act2_rising":"军人职责与婚姻生活矛盾，感情在磨合中升温","act2_midpoint":"执行任务与守护家人冲突，女主面临危险","act2_dark":"男主受伤/失联，女主独撑局面，感情遭受考验","act3_climax":"男主归来，全力保护女主完成终极守护","act3_resolution":"危机解除，男主用行动正式表达","act3_denouement":"并肩而立，成为彼此最坚强的后盾"}`,
			CharacterArchetypes: archetypesJSON("stoic_soldier", "scheming_villain", "strong_heroine", "comrade_friend"),
			EmotionCurveTemplate: emotionCurve([]float64{3, 5, 6, 7, 8, 5, 3, 8, 9, 10}),
			KeyTriggers: "硬汉守护,危险挺身,冷漠破功,任务冲突,生死危机,归来告白",
			IsBuiltin:   true,
		},
	}
}
