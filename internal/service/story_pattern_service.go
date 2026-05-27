package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StoryPattern is a reusable narrative structure template for Chinese web novels.
type StoryPattern struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Genres       []string `json:"genres"`       // applicable genres
	Archetype    string   `json:"archetype"`    // broad category
	Description  string   `json:"description"`
	Beats        []string `json:"beats"`        // ordered narrative beats
	EmotionalArc string   `json:"emotional_arc"`
	KeyElements  []string `json:"key_elements"` // must-have elements
	AvoidCliches []string `json:"avoid_cliches"`
	TensionCurve string   `json:"tension_curve"`
	PacingAdvice string   `json:"pacing_advice"`
}

// embeddedPatterns is the built-in story pattern library for Chinese web novels.
var embeddedPatterns = []StoryPattern{
	{
		ID:          "reversal_comeback",
		Name:        "绝境逆袭",
		Genres:      []string{"修仙", "玄幻", "武侠", "都市", "战争"},
		Archetype:   "逆袭",
		Description: "主角陷入看似无法逆转的危局，凭借意志力或隐藏实力实现反败为胜",
		Beats: []string{
			"1. 铺垫：多重困境叠加（伤势+敌强+援断），读者感受到绝望氛围",
			"2. 深渊：局势持续恶化，反派嘲讽或准备致命一击",
			"3. 触底：内心触底时刻，触发深层意志、隐藏天赋或秘密准备",
			"4. 反击：出人意料的手段，节奏骤然加速，动作描写密集",
			"5. 结果：逆转成功，但留有余韵——代价、伤痛或新的威胁",
		},
		EmotionalArc: "压抑↓→绝望↓↓→爆发↑↑→胜利（带代价）",
		KeyElements:  []string{"绝境铺垫充分有层次", "反击依据在前文埋伏", "胜利要有合理代价"},
		AvoidCliches: []string{"主角无故暴力碾压", "反派独白过长", "逆袭缺乏代价", "结局过于完美"},
		TensionCurve: "低→极低→骤升→峰值",
		PacingAdvice: "绝境阶段宜慢（渲染绝望），反击阶段节奏急促，结尾留白回味",
	},
	{
		ID:          "identity_reveal",
		Name:        "身份揭露",
		Genres:      []string{"修仙", "玄幻", "都市", "历史", "悬疑"},
		Archetype:   "揭秘",
		Description: "主角或关键人物的真实身份/过去在关键时刻被揭露，改变所有人的认知",
		Beats: []string{
			"1. 前置线索：散落的异常细节引发读者与配角的疑惑",
			"2. 压力触发：外部威胁或情感冲突迫使秘密无法继续隐藏",
			"3. 揭露时刻：通过物证/对话/回忆闪回多角度呈现，信息量集中爆发",
			"4. 冲击波：周围人的震惊反应放大戏剧效果，主角内心独白",
			"5. 格局重构：揭露后的关系重新洗牌，引出新的冲突",
		},
		EmotionalArc: "好奇→疑惑→震惊↑↑→理解→关系重构",
		KeyElements:  []string{"前期线索必须真实存在（不能事后补充）", "揭露方式有创意", "揭露后有实质性影响"},
		AvoidCliches: []string{"揭露后立刻化解一切矛盾", "身份揭露用途仅为炫耀", "前期零铺垫突兀揭露"},
		TensionCurve: "平缓→缓升→骤升（揭露点）→余震",
		PacingAdvice: "揭露前蓄势要足，揭露瞬间可短暂停顿，后续反应用较慢节奏展开",
	},
	{
		ID:          "talent_awakening",
		Name:        "天赋觉醒",
		Genres:      []string{"修仙", "玄幻", "异能", "校园"},
		Archetype:   "觉醒",
		Description: "主角潜藏的异禀天赋在特殊契机下觉醒，彻底改变其命运走向",
		Beats: []string{
			"1. 压抑期：主角被视为废材/普通人，积累了足够的不甘与渴望",
			"2. 契机：极端压力、生死边缘或情感冲击创造觉醒条件",
			"3. 觉醒过程：感官异变→能量涌动→天地变色，用感官细节描写",
			"4. 初展锋芒：新能力的第一次运用，结局出人意料",
			"5. 格局提升：他人对主角认知翻转，主角心态也随之改变",
		},
		EmotionalArc: "压抑→渴望→混乱（觉醒中）→震撼↑→自信觉醒",
		KeyElements:  []string{"压抑期铺垫足够长以使觉醒更有冲击感", "觉醒有独特视觉感", "觉醒后有学习成本"},
		AvoidCliches: []string{"觉醒即无敌", "觉醒原因牵强", "觉醒后性格突变"},
		TensionCurve: "平低→混乱波动→骤升→高位稳定",
		PacingAdvice: "觉醒过程可用慢镜头式细节描写，后续展示实力可节奏加快",
	},
	{
		ID:          "master_inheritance",
		Name:        "师承传授",
		Genres:      []string{"修仙", "武侠", "玄幻"},
		Archetype:   "传承",
		Description: "师父/前辈在关键时刻将毕生绝学或秘密传给主角，伴随深厚的情感联结",
		Beats: []string{
			"1. 缘起：师徒/前辈间建立信任关系，互相展示真实自我",
			"2. 危机来临：师父面临死亡、离别或无力保护弟子的困境",
			"3. 传承决定：师父权衡后做出传授决定，伴随深沉的情感",
			"4. 传授过程：技艺/秘密的传递，穿插回忆或解释，情感高潮",
			"5. 分离与继承：师父离去，主角带着传承独立前行",
		},
		EmotionalArc: "敬重→亲密→哀愁→感恩→使命感",
		KeyElements:  []string{"师徒感情须有前期积累", "传承内容对后续剧情有实际影响", "分离场景需有仪式感"},
		AvoidCliches: []string{"师父只为工具人角色", "传承后主角立刻掌握一切", "感情戏敷衍"},
		TensionCurve: "温和→缓升（危机）→情感高点→平缓（余韵）",
		PacingAdvice: "以慢为主，情感积累重于动作描写，对话要有层次感",
	},
	{
		ID:          "alliance_vs_enemy",
		Name:        "联手对敌",
		Genres:      []string{"修仙", "玄幻", "武侠", "战争"},
		Archetype:   "联合",
		Description: "主角与以往的竞争对手或陌生人因共同威胁而结成同盟，合作消灭强敌",
		Beats: []string{
			"1. 共同危机：出现远超双方单独应对能力的强敌",
			"2. 接触试探：双方互相试探、确认合作底线，建立初步信任",
			"3. 默契磨合：合作中的冲突与调整，展示两方的性格与实力",
			"4. 关键配合：在最危险的时刻，双方默契达成，发挥出1+1>2的效果",
			"5. 胜利与裂痕：共敌已去，同盟关系如何演变？保留张力",
		},
		EmotionalArc: "警惕→试探→摩擦→默契→复杂情感",
		KeyElements:  []string{"敌人足够强以迫使联手", "合作过程有摩擦才真实", "结盟后关系留有余地"},
		AvoidCliches: []string{"联手后感情突变成好友", "敌人立刻被消灭", "配合过程过于顺滑"},
		TensionCurve: "骤升→波动→峰值→缓降",
		PacingAdvice: "合作磨合阶段节奏适中，关键配合处快节奏，结尾刻意放慢以留余味",
	},
	{
		ID:          "hidden_guardian",
		Name:        "暗中守护",
		Genres:      []string{"修仙", "都市", "言情", "玄幻"},
		Archetype:   "守护",
		Description: "强大的保护者在暗中守护主角，适时出手化解危机，其身份最终揭晓",
		Beats: []string{
			"1. 危机接近：主角陷入无力抵御的险境，读者隐约感知有人在暗处",
			"2. 神秘出手：危机关头神秘力量介入，解围但不现身",
			"3. 线索积累：主角发现可疑迹象，开始追查守护者身份",
			"4. 揭晓时刻：守护者身份揭晓，与主角有深刻关联（血缘/承诺/爱意）",
			"5. 正面相遇：双方正式对话，情感关系升华或引发新冲突",
		},
		EmotionalArc: "安全感↑→好奇→感动→震撼→情感深化",
		KeyElements:  []string{"守护者行动必须留下可追溯的线索", "身份揭晓要有情感冲击力", "守护的动机要有说服力"},
		AvoidCliches: []string{"守护者全知全能", "守护动机过于随意", "揭晓后处理草率"},
		TensionCurve: "缓升（危机）→骤降（出手）→平稳（线索）→骤升（揭晓）",
		PacingAdvice: "出手场景快速，揭晓后的情感交流节奏放慢，留足空间给人物情绪",
	},
	{
		ID:          "breakthrough_bottleneck",
		Name:        "瓶颈突破",
		Genres:      []string{"修仙", "玄幻", "武侠"},
		Archetype:   "成长",
		Description: "主角在修炼瓶颈处长期停滞，通过顿悟/磨难/机缘完成重大突破",
		Beats: []string{
			"1. 停滞之痛：详细描写瓶颈期的压抑感，外界压力加剧焦虑",
			"2. 尝试失败：多次冲关失败，旁人的轻视与自我的怀疑",
			"3. 触发契机：生死边缘/情感冲击/顿悟/奇遇，触发突破条件",
			"4. 突破过程：体内变化的感官细节，天地异象呼应，渲染宏大感",
			"5. 新境界展示：以实战或能力展示新层次的强大，与旧我告别",
		},
		EmotionalArc: "焦虑→挣扎→顿悟（宁静一瞬）→爆发↑→喜悦+敬畏",
		KeyElements:  []string{"瓶颈期心理刻画要真实", "突破条件需与角色成长逻辑相符", "新境界需有具体化展示"},
		AvoidCliches: []string{"突破原因过于外部化（仅靠药/宝物）", "突破过程无内心挣扎", "突破后实力夸张失真"},
		TensionCurve: "低迷→波动（尝试）→骤升（突破）→高点稳定",
		PacingAdvice: "瓶颈期可用较长铺垫，突破过程快节奏+感官密集，后续实战展示中节奏",
	},
	{
		ID:          "revenge_moment",
		Name:        "复仇时刻",
		Genres:      []string{"修仙", "玄幻", "都市", "武侠", "悬疑"},
		Archetype:   "复仇",
		Description: "主角在积累足够实力后与仇人正面相对，完成心理与力量的双重清算",
		Beats: []string{
			"1. 仇恨积淀：回忆/闪回展示过去的伤害，激活读者的代入感",
			"2. 相遇时刻：仇人的第一次反应（轻蔑/震惊/恐惧），形成对比",
			"3. 心理博弈：复仇前的语言对决，主角保持控制感但内心涌动",
			"4. 复仇行动：主角展示碾压性优势，但不可过于轻松",
			"5. 了断或放下：复仇完成后的心境——解脱、空虚或新的决意",
		},
		EmotionalArc: "压抑已久→压迫感（对峙）→快意→解脱（可能伴随空虚）",
		KeyElements:  []string{"复仇前须展示充足实力差距", "复仇过程有心理层次", "结尾处理要有深度而非单纯爽感"},
		AvoidCliches: []string{"复仇过于草率（反派一击即倒）", "主角显得残忍无情", "复仇结束后无后续影响"},
		TensionCurve: "积压→骤升（相遇）→高位持续（博弈）→释放",
		PacingAdvice: "对峙阶段拉长节奏，心理博弈占比高，实际冲突可快速解决，结尾情绪处理慢",
	},
	{
		ID:          "sacrifice_price",
		Name:        "牺牲与代价",
		Genres:      []string{"修仙", "玄幻", "战争", "言情", "悬疑"},
		Archetype:   "牺牲",
		Description: "主角或重要配角为保护他人/完成目标付出巨大代价，引发深刻情感共鸣",
		Beats: []string{
			"1. 价值确立：清晰展示被守护者/目标对牺牲者的重要性",
			"2. 困境选择：牺牲的必要性被清楚呈现，无路可退",
			"3. 决定时刻：牺牲者的内心独白，告别或遗嘱",
			"4. 牺牲过程：以慢镜头式细节描写，渲染悲壮感",
			"5. 余波：幸存者的悲痛与成长，牺牲的影响延伸至后续章节",
		},
		EmotionalArc: "依恋→危机↑→悲壮↑→极度悲痛→升华",
		KeyElements:  []string{"牺牲必须是真实代价而非反转可撤销", "情感铺垫必须充分", "牺牲后对其他角色产生持续影响"},
		AvoidCliches: []string{"牺牲后立刻复活/反转", "牺牲感情过于煽情失真", "仅为工具性推进剧情"},
		TensionCurve: "缓升→骤升→极高点→缓降（余波持续）",
		PacingAdvice: "牺牲前情感铺垫节奏慢，牺牲过程中节奏，余波处理节奏极慢以留空间给悲痛",
	},
	{
		ID:          "rivals_acquainted",
		Name:        "对手相知",
		Genres:      []string{"修仙", "玄幻", "武侠", "都市"},
		Archetype:   "对手",
		Description: "主角与实力相当的对手在多次交手和共同经历中相互理解，形成独特的竞争情谊",
		Beats: []string{
			"1. 初次交锋：旗鼓相当，双方都感到压力，种下敬重种子",
			"2. 反复较量：多次交手，每次都有不同维度的胜负",
			"3. 意外共情：共同面对危险或个人困境，窥见对方真实内心",
			"4. 理解时刻：言语或行动中透露出对彼此的尊重与理解",
			"5. 关系升华：从单纯对手变为彼此激励、相互成就的存在",
		},
		EmotionalArc: "敌视→试探→尊重→共情→复杂情感（竞争+理解）",
		KeyElements:  []string{"双方实力真正均衡不可差距过大", "竞争动机要有层次", "情感转变要自然不突兀"},
		AvoidCliches: []string{"对手仅是工具人", "突然变成好友失去张力", "没有代价的轻松化解对立"},
		TensionCurve: "持续波动（反复交锋）→情感高点（共情）→稳定（新平衡）",
		PacingAdvice: "较量场景节奏快，情感交流场景节奏慢，两者交替形成节奏对比",
	},
	{
		ID:          "world_secret_reveal",
		Name:        "世界真相揭秘",
		Genres:      []string{"修仙", "玄幻", "科幻", "悬疑"},
		Archetype:   "揭秘",
		Description: "主角发现世界运行规则的重大秘密，颠覆此前所有认知，重新定义目标",
		Beats: []string{
			"1. 异常积累：前期埋下不合逻辑的细节，引发主角与读者的疑惑",
			"2. 触发探索：特殊事件促使主角深入调查禁区/禁忌领域",
			"3. 逐层揭露：真相不是一次性给出，而是层层剥开，每层都更震撼",
			"4. 核心真相：最终真相与最初设定完全相反或完全超出预期",
			"5. 认知重构：主角及世界观的重新建立，引出更宏大的故事格局",
		},
		EmotionalArc: "疑惑→震撼→迷失→接受→使命感升华",
		KeyElements:  []string{"真相线索须在前期真实存在", "真相规模要与故事格局匹配", "揭秘后世界不能恢复原状"},
		AvoidCliches: []string{"真相无前期伏笔纯靠强行解释", "揭秘后立刻解决所有问题", "真相仅服务于当前情节"},
		TensionCurve: "平缓→缓升→多次骤升（层层揭秘）→终极峰值",
		PacingAdvice: "探索阶段用悬疑慢节奏，每次揭秘用快节奏爆发，最终揭秘后放慢消化",
	},
	{
		ID:          "karmic_retribution",
		Name:        "因果清算",
		Genres:      []string{"修仙", "玄幻", "都市", "历史"},
		Archetype:   "因果",
		Description: "反派积累的恶行在因果到来时被一一清算，体现宿命感与道义感",
		Beats: []string{
			"1. 积罪铺垫：反派的历史恶行通过多种渠道展现，形成读者期待",
			"2. 高峰嚣张：反派在末日来临前处于权势顶峰，读者焦虑积累",
			"3. 因果触发：宿命相遇的那一刻，主角或其他力量成为因果执行者",
			"4. 逐一清算：每一项恶行都有对应的惩罚，有仪式感",
			"5. 尘埃落定：清算完成，被伤害者得到告慰，世界秩序恢复",
		},
		EmotionalArc: "压抑（观恶）→愤怒积累→期待↑↑→快意→安慰",
		KeyElements:  []string{"恶行与惩罚有对应关系", "清算过程不能过于草率", "结局有道义层面的升华"},
		AvoidCliches: []string{"清算过于简单无反转", "反派一夜间悔悟", "惩罚与罪行完全不对称"},
		TensionCurve: "低沉（积恶）→缓升→骤升（清算）→平稳（终局）",
		PacingAdvice: "清算前积压期宜慢以积累情绪，清算过程节奏适中有层次，结尾留白处理",
	},
	{
		ID:          "romance_confession",
		Name:        "情感表白",
		Genres:      []string{"言情", "都市", "修仙", "校园"},
		Archetype:   "情感",
		Description: "主角与重要情感对象在情感积累到极点后，通过直接或间接方式完成情感表达",
		Beats: []string{
			"1. 情感积累：日常细节中的心动时刻，两人距离在不知不觉中缩近",
			"2. 错失机会：多次本可表白却因外因错过，张力积累",
			"3. 临界时刻：危险/离别/误会将情感推到爆发点",
			"4. 表白时刻：真实情感迸发，方式与角色性格高度契合",
			"5. 情感定格：接受/拒绝/沉默，关系发生实质性改变",
		},
		EmotionalArc: "心动→不确定→错失→渴望→勇气→结果（各异）",
		KeyElements:  []string{"情感铺垫必须细腻真实", "表白方式符合角色性格", "结果要有后续影响力"},
		AvoidCliches: []string{"过于套路化的告白场景", "情感突然爆发无铺垫", "接受后立刻顺风顺水"},
		TensionCurve: "缓升→波动（错失）→骤升（临界）→情感峰值",
		PacingAdvice: "情感积累用慢节奏细节，临界时刻中节奏，表白瞬间用极慢笔墨放大细节",
	},
	{
		ID:          "betrayal_revelation",
		Name:        "背叛揭露",
		Genres:      []string{"修仙", "玄幻", "悬疑", "历史", "都市"},
		Archetype:   "背叛",
		Description: "主角发现信任之人是幕后黑手或出卖者，友情/亲情/信念被颠覆",
		Beats: []string{
			"1. 信任积累：明确展示主角对背叛者的信任深度",
			"2. 蛛丝马迹：细节处的异常开始积累，读者略有感知但主角尚未察觉",
			"3. 揭露时刻：事实以最具冲击力的方式呈现，无法否认",
			"4. 崩溃过程：主角的情感崩溃——愤怒、难以置信、悲痛",
			"5. 重建或报复：主角的选择——原谅/仇恨/报复，决定后续走向",
		},
		EmotionalArc: "信任→疑惑→崩溃↓↓→愤怒↑→决意",
		KeyElements:  []string{"背叛动机要充分可信", "主角的崩溃要真实", "背叛须对剧情有实质性影响"},
		AvoidCliches: []string{"背叛只是为了短暂制造波折", "背叛者最终被原谅且无后果", "背叛原因过于牵强"},
		TensionCurve: "平稳→缓升（疑惑）→骤升（揭露）→崩溃后低谷→决意上升",
		PacingAdvice: "揭露前蛛丝马迹用慢节奏埋伏，揭露瞬间快节奏，崩溃过程用慢节奏深挖心理",
	},
	{
		ID:          "resource_competition",
		Name:        "资源争夺",
		Genres:      []string{"修仙", "玄幻", "武侠"},
		Archetype:   "竞争",
		Description: "多方势力围绕稀缺资源展开博弈，主角在智谋与实力的双重角逐中脱颖而出",
		Beats: []string{
			"1. 资源登场：稀缺资源的重要性被充分渲染，各方势力的垂涎",
			"2. 格局建立：各派势力的实力与目标被快速勾勒，建立博弈棋盘",
			"3. 多线博弈：明争暗斗、联盟与背刺同步发生，节奏紧凑",
			"4. 关键转折：主角利用格局漏洞或出奇制胜的策略扭转局势",
			"5. 收尾清算：资源归属确定，各方势力关系重组",
		},
		EmotionalArc: "期待→紧张→刺激（博弈）→惊喜（转折）→满足",
		KeyElements:  []string{"资源价值要有充分说明", "多方势力的动机要清晰", "主角胜利要靠智谋+实力双重保障"},
		AvoidCliches: []string{"主角单纯靠运气得到资源", "其他势力成为纸老虎", "竞争过程过于简单"},
		TensionCurve: "缓升→高位波动（博弈）→骤升（转折）→稳定",
		PacingAdvice: "博弈阶段节奏快且信息密集，转折后快速推进结局，避免拖沓",
	},
}

// StoryPatternService provides story pattern queries for chapter generation.
type StoryPatternService struct{}

// NewStoryPatternService creates a StoryPatternService.
func NewStoryPatternService() *StoryPatternService {
	return &StoryPatternService{}
}

// Search returns patterns matching the given genre and/or archetype.
// If both are empty it returns the top maxResults patterns.
func (s *StoryPatternService) Search(genre, archetype string, maxResults int) []StoryPattern {
	if maxResults <= 0 {
		maxResults = 2
	}

	genre = strings.TrimSpace(genre)
	archetype = strings.TrimSpace(archetype)

	var scored []struct {
		score   int
		pattern StoryPattern
	}

	for _, p := range embeddedPatterns {
		score := 0
		if genre != "" {
			for _, g := range p.Genres {
				if strings.Contains(g, genre) || strings.Contains(genre, g) {
					score += 2
					break
				}
			}
		}
		if archetype != "" {
			if strings.Contains(p.Archetype, archetype) || strings.Contains(archetype, p.Archetype) ||
				strings.Contains(p.Name, archetype) || strings.Contains(archetype, p.Name) {
				score += 3
			}
		}
		if score > 0 || (genre == "" && archetype == "") {
			scored = append(scored, struct {
				score   int
				pattern StoryPattern
			}{score, p})
		}
	}

	// Sort by score descending (simple insertion-sort for small slice)
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].score > scored[j-1].score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}

	result := make([]StoryPattern, 0, maxResults)
	for i := 0; i < len(scored) && i < maxResults; i++ {
		result = append(result, scored[i].pattern)
	}
	return result
}

// ListAll returns all embedded patterns.
func (s *StoryPatternService) ListAll() []StoryPattern {
	return embeddedPatterns
}

// formatStoryPatterns formats patterns as a prompt-injectable string.
func formatStoryPatterns(patterns []StoryPattern) string {
	if len(patterns) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, p := range patterns {
		if i >= 2 {
			break
		}
		sb.WriteString(fmt.Sprintf("【情节模板：%s】\n", p.Name))
		sb.WriteString(fmt.Sprintf("情感弧线：%s\n", p.EmotionalArc))
		sb.WriteString("叙事节拍：\n")
		for _, beat := range p.Beats {
			sb.WriteString("  " + beat + "\n")
		}
		if len(p.KeyElements) > 0 {
			sb.WriteString("必备元素：" + strings.Join(p.KeyElements, "；") + "\n")
		}
		if len(p.AvoidCliches) > 0 {
			sb.WriteString("避免俗套：" + strings.Join(p.AvoidCliches, "；") + "\n")
		}
		sb.WriteString(fmt.Sprintf("节奏建议：%s\n", p.PacingAdvice))
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// parseStoryPatternOutput parses the output map from McpService.InvokeTool("story_pattern", …).
func parseStoryPatternOutput(output map[string]interface{}) string {
	if output == nil {
		return ""
	}
	raw, ok := output["patterns"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	var patterns []StoryPattern
	if err := json.Unmarshal(b, &patterns); err != nil {
		return ""
	}
	return formatStoryPatterns(patterns)
}
