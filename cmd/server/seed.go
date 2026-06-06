package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
	"gorm.io/gorm"
)

// seedDefaultUser 创建默认测试账户（幂等，已存在则跳过）
// 账户信息通过环境变量配置：
//
//	SEED_TEST_EMAIL    默认 test@inkframe.dev
//	SEED_TEST_PASSWORD 必须设置，未设置则跳过
//	SEED_TEST_USERNAME 默认 testuser
func seedDefaultUser(services *Services) {
	password := os.Getenv("SEED_TEST_PASSWORD")
	if password == "" {
		logger.Println("[seed] SEED_TEST_PASSWORD not set, skipping default user creation")
		return
	}

	email := os.Getenv("SEED_TEST_EMAIL")
	if email == "" {
		email = "test@inkframe.dev"
	}
	username := os.Getenv("SEED_TEST_USERNAME")
	if username == "" {
		username = "testuser"
	}

	_, err := services.AuthService.Register(&service.RegisterRequest{
		Username:   username,
		Email:      email,
		Password:   password,
		Nickname:   "测试用户",
		TenantName: "测试租户",
	})
	if err != nil {
		// "email already registered" 表示已存在，不视为错误
		if err.Error() == "email already registered" {
			logger.Printf("[seed] default user already exists (%s)", email)
		} else {
			logger.Printf("[seed] failed to create default user: %v", err)
		}
		return
	}
	logger.Printf("[seed] default test user created: %s", email)
}

// seedDefaultData 预置默认世界观（INSERT IGNORE 幂等）
func seedDefaultData(db *gorm.DB) {
	db.Exec(`INSERT IGNORE INTO ink_worldview
		(uuid,name,genre,description,magic_system,geography,history,culture,technology,rules,used_count,created_at,updated_at)
	VALUES
	('00000000-0000-0000-0000-000000000001','洪荒大陆','fantasy',
	 '远古洪荒时代，天地初开，灵气充溢。大陆被称为"九州"，分东荒、西漠、南疆、北冥、中原五大区域。强者以武证道，弱者朝不保夕，诸方势力争夺天道之位。',
	 '修炼九境：淬体→聚气→开脉→凝元→化神→破虚→半圣→圣境→无上，每境分初中巅三阶。力量来源于天地灵气，丹田凝聚元气，圣境以上可感应天道意志。炼丹、炼器、阵法为三大辅助体系。',
	 '中央苍穹山脉横贯东西。东荒多古林秘境；西漠沙海埋藏上古宝藏；南疆瘴气弥漫蛊术盛行；北冥冰封，隐藏魔族封印；中原三大圣地七大宗门据守要冲。',
	 '诸神开辟大陆后经"诸神黄昏"大战陨落，遗留神器与禁地。上古魔族封印于北冥，每万年苏醒一次。三千年前"圣道战争"导致多个古宗毁灭，遗留废墟成为后世圣地。',
	 '人族为主体，兽族妖族魔族各据一方。宗门制度森严，外门内门核心弟子待遇天差地别。普通百姓依附城主府或宗门生存，强者享有凌驾律法之上的特权。',
	 '炼器品级分凡灵玄圣神五阶；阵法以灵石驱动；传送阵连接各地但耗资巨大；顶级宗门拥有飞舟；炼丹师地位崇高，一炉突破丹价值连城。',
	 '天道不可违逆，强行突破境界者遭天劫诛杀。噬魂大法可夺人修为但污染元神，被列为死罪。圣境以上争斗需远离凡人城池，否则方圆百里化为废土。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000002','九天仙界','xianxia',
	 '天地间分仙界、人界、冥界三界，以天柱相连。仙界居九重天之上，人界芸芸众生修道问仙，冥界主掌轮回因果。诸仙争夺道果，掌握天地法则以求长生不灭。',
	 '修仙九境：练气→筑基→金丹→元婴→化神→炼虚→合体→大乘→渡劫。金丹期可御剑飞行，元婴期神识离体，化神期操控天地元素。剑修丹修阵修体修四大流派各有秘法，天雷渡劫是突破大境界的必经考验。',
	 '人界苍澜洲以东海西荒南天山北极苔原为四极，中央昆仑圣山为仙道正宗汇聚地。海底有龙宫遗址，荒漠中埋藏上古仙人遗留法宝。仙界九重天各掌不同天道法则。',
	 '鸿蒙老祖开天证道，分化阴阳立三界秩序。上古仙魔大战后魔道覆灭。五千年前"仙道浩劫"令诸多上仙陨落，人界趁机出现多位天才搅动三界格局，天庭与各洞天明争暗斗延续至今。',
	 '宗门讲究辈分与传承，师徒情谊大于天。修仙者寿命可达数千载，与凡人形成天然隔阂。因果业力深入日常观念，善恶有报轮回不爽。道侣同修可互助突破瓶颈。',
	 '法器分法宝灵宝仙宝三级，顶级仙宝可斩断因果逆转时空。符箓源自上古仙人手书，传送阵遍布各大宗门。炼丹以天地灵材为原料，丹火修炼是核心技艺。',
	 '天道轮回不可逆，强行干涉他人命数者遭因果反噬。夺舍侵占他人肉身是三界最大禁忌，一经发现即被公审诛杀。无令牌擅入仙界九重天者形神俱灭。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000003','灵气复苏都市','urban',
	 '现代都市背景，灵气突然复苏，沉寂千年的修炼之道重现人间。觉醒者出现，政府、财团、古老家族与新兴门派围绕灵气资源与规则制定权展开博弈，科技与修炼的碰撞构成核心矛盾。',
	 '觉醒体系分E~A级普通觉醒者、S级超凡者、宗师、半神、神话五层。能力分体术系、元素系、精神系、空间系等七大系列。古修炼功法与现代觉醒体系可相互印证，灵晶是通用修炼货币。',
	 '主舞台为灵脉汇聚的"临海市"，全球各地出现灵气异常点，古老遗迹浮出地表，山川大河开始蕴含灵气。城市边缘出现独立于现实之外的"异境"入口，内藏资源与危险。',
	 '三千年前修炼盛世终结，灵气枯竭，修士销声匿迹，隐世家族暗中传承。十年前全球地磁异常，五年前首批觉醒者出现，一年前官方正式承认超自然现象，建立特异事务局。',
	 '现代社会体制正常运转，觉醒者社群在其上形成新圈层。古老家族以血脉传承维系地位，新兴平民觉醒者冲击既有秩序。媒体与网络舆论成为各方势力博弈的新战场。',
	 '现代科技与修炼兼容，科学家研究量子纠缠与灵力关联。高端实验室研发灵力增幅器，基因编辑技术尝试提高觉醒概率，AI辅助灵力分析系统进入实用阶段。',
	 '异境内死亡无法被外界追究，成为各方默认灰色地带。禁止在人口密集区进行高烈度战斗，违者被特异事务局通缉。上古禁术在现代同样禁止，往往引发难以控制的灵气暴走。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000004','星际联邦纪元','scifi',
	 '人类文明扩张至数百星系，建立星际联邦政体。科技高度发达，但资源争夺、种族歧视、AI权利运动与星际战争等矛盾从未消失。神秘星域中藏有远古文明遗迹，个体英雄与庞大政治机器的对抗是永恒主题。',
	 '无传统修炼，以科技为核心：纳米义体改造、基因重组增强、神经网络接入、暗物质武器。"先天感应者"（Esper）拥有精神力量，被联邦军纳入特殊兵种。远古遗迹中的源质晶体可大幅提升能量密度，成为各方争夺焦点。',
	 '以索拉尔星系为核心，联邦首都奥维斯星球被全球城市覆盖。边境"幽冥星域"藏有远古文明废墟。各星系通过曲率跳跃点连接，控制跳跃点即掌握星系咽喉。',
	 '2150年人类发展出曲率引擎开始星际移民，经历大殖民时代后与三个异星文明接触。"第一次星际战争"催生联邦政体，200年前"人工意识觉醒事件"引发AI独立运动，至今悬而未决。',
	 '联邦实行代议制民主，核心权力被七大财阀把控，阶层固化严重。AI与机械人享有部分法律权利但仍受歧视。星际移民第一代与土著星球人之间存在文化冲突。',
	 '曲率引擎实现星际旅行，量子通信消除信息延迟。义体改造普及，星舰配备粒子炮与反物质鱼雷。医疗科技可修复绝大多数伤情，意识备份技术让"死亡"的定义产生根本性争议。',
	 '禁止"意识强制覆写"，违者以谋杀罪处置。对非成员文明发动灭绝战争属最高战争罪。源质晶体武器化受国际协议限制，星系级毁灭性武器的使用须联邦议会三分之二多数通过。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000005','废土纪元','apocalypse',
	 '核战与生化病毒的双重打击摧毁旧文明，地表变为辐射废土。幸存者在废墟城市、地下避难所与流动营地中求生，变异生物、丧尸潮、辐射风暴是日常威胁，秩序与人性的重建是终极命题。',
	 '无传统修炼，以突变为核心：高剂量辐射导致基因突变，少数幸存者获得念力、金属控制、毒素免疫等超能力，称为"变种人"。旧世界军用外骨骼与民间改装武器并存，净化血清是最珍贵的医疗资源。',
	 '北美中部废土为主舞台，旧城市已成断壁残垣，地铁隧道改造为地下城。辐射污染较轻可耕作的"绿洲"是各方争夺核心，放射性沙漠中埋藏旧世界军事设施与大量武器库。',
	 '旧历2087年第三次世界大战爆发，核战72小时后各国政府崩溃，生化病毒"灰死病"在混乱中扩散，大部分幸存者变为丧尸。现为"战后第47年"，各势力割据，新秩序呼之欲出。',
	 '废土社会分避难所官僚体制、地面部落、流浪商队三类。物资是最硬通货，瓶盖弹壳净化水各地通行。忠诚与背叛是社交核心命题，契约精神稀缺而珍贵。',
	 '旧世界科技残存于各处遗址，零件极度匮乏。改装武器文化发达，废弃工厂是最珍贵资源点。太阳能与风能重新普及，AI辅助的旧世界服务器被视为无价之宝。',
	 '不得主动污染水源，违者各营地联合追杀。不得对净化区平民发动大规模毒气攻击，此为各大势力底线。任何持有旧世界核弹头的势力被视为全人类公敌。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000006','中原江湖','wuxia',
	 '架空古代中国，江湖与庙堂并立。中原武林各派林立，以武学正统之争与侠义精神之辩划分阵营。朝廷、世家、江湖三股力量相互制衡，个人恩仇与天下苍生的抉择是永恒主题。',
	 '内功心法为根本，外功招式为手段。内力分先天与后天，先天真气为最高境界。武学修为分入门、小成、大成、宗师、绝顶、传说六级，传说级武者百年一出可以一敌百。轻功、暗器、毒术、奇门遁甲各成体系，武功秘籍是最重要的资产。',
	 '中原大地，黄河南北分治，长江流域是江湖纷争最烈之处。嵩山为武林大会召开地。西域大漠有异族高手，东海之滨有神秘海盗帮，北境草原游牧民族虎视眈眈，南疆苗寨蛊术独步天下。',
	 '百年前"武林浩劫"魔教屠戮正道，武林元气大伤，数代人方才恢复。五十年前朝廷颁布禁武令，引发正邪两道共同抵抗，最终形成"江湖自治"默契。传说中集百家之大成的"天下第一武典"下落再度搅动江湖。',
	 '江湖规矩深入人心：尊师重道，以武会友，不斩降者，不伤无辜。正道注重礼义廉耻，魔教强调结果至上。普通百姓敬畏武林人士，地方官府与江湖大帮维持微妙平衡。',
	 '武功时代，无火药热兵器。马匹代步，镖局走镖连接各城。客栈是信息集散地，茶楼是谈判场所。飞鸽传书是最快通信方式，内力运功可加速伤势痊愈。',
	 '门派内讧不得动用毒药暗器，违者开除门籍为武林公敌。不得对武功全废之人痛下杀手，点到为止是比武铁则。盗窃武林秘籍被视为最大耻辱，挟持他人家眷要挟同道者逐出江湖。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000007','现代都市','modern',
	 '当代中国都市背景，以北上广深等一线城市为主舞台。职场竞争、商业博弈、情感纠葛与家庭羁绊交织，普通人在欲望与良知、个人奋斗与社会规则之间寻找自己的位置。',
	 '无超自然力量，以现实社会规则为核心。金钱、人脉、权力是主要资源，情商与智商决定成败。商界以资本运作为武器，官场以政绩人脉为筹码，娱乐圈以流量资源为货币。信息差与掌握它的人往往决定博弈胜负。',
	 '以一线城市CBD商务区、顶级写字楼、豪华住宅区为权力中心，城中村与城郊结合部是底层奋斗者的起点。高铁网络连接全国，互联网消除信息壁垒但制造新的信息茧房。地标建筑与高档餐厅是人脉交汇的社交舞台。',
	 '改革开放后经济腾飞，造就第一批民营企业家。互联网浪潮催生新贵阶层，移动互联网时代让草根逆袭成为可能。近年监管趋严，资本无序扩张时代落幕，实业与创新重回中心。社会阶层流动放缓，"内卷"与"躺平"成为时代注脚。',
	 '职场文化以结果为导向，996与狼性文化曾盛行，如今工作生活平衡逐渐被重视。"关系"文化根深蒂固，但契约精神与规则意识正在崛起。消费主义盛行，品牌与阶层绑定；同时极简主义与性价比消费成为新趋势。代际观念冲突明显，传统家庭观与个人主义并存。',
	 '智能手机与移动互联网深度融合日常生活，外卖、网约车、移动支付已是基础设施。新能源汽车快速普及，AI工具进入办公场景。医疗、教育资源分配不均仍是主要社会矛盾，大数据与算法深刻影响消费和舆论走向。',
	 '劳动法保护员工基本权益，但执行力度因行业而异。商业竞争须遵循反垄断法规，内幕交易受证监会严查。网络言论须符合相关法规，舆论操控与虚假信息属违法行为。职场性骚扰与歧视问题受到日益严格的法律约束。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000008','童话王国','fairytale',
	 '一片被魔法滋养的奇幻大陆，森林会说话，星星有名字，每一块石头都藏着故事。善良与勇气是最强大的力量，爱与牺牲能打破任何诅咒。王子与公主、女巫与精灵、龙与骑士共同编织出一个奇妙又温暖的世界。',
	 '魔法源于心灵力量：爱越深，魔法越强；恐惧与贪婪则催生黑暗魔法。祝福与诅咒是最常见的法术形式，真爱之吻、真心眼泪、勇敢之心是破除诅咒的三大关键。精灵掌握自然魔法，女巫精通变形术，仙女教母能许下三个愿望。',
	 '王国由玫瑰城堡统治，城堡以彩虹为桥通向云端。东有说话的大森林，森林深处住着智慧老树；西有糖果山脉，甜蜜气息飘散百里；南有镜湖，湖面映出人心中最真实的愿望；北有永冬之地，冰雪精灵在此栖居。',
	 '远古时代，善之女神以歌声创造大地，恶之巫王以嫉妒诅咒世间美好。一位无名牧羊人以纯粹的爱击败巫王，世界从此被善与恶的平衡守护。每隔百年，黑暗诅咒会复苏一次，总有新的英雄踏上旅程将其终结。',
	 '王国居民善良淳朴，邻里互助，以分享为荣。动物与人类平等相处，甚至可以成为挚友。每年春日举行"心愿节"，居民向星星许下愿望；每年冬至举行"温暖夜"，全城点灯驱散黑暗。诚实守信是最高美德，谎言在这里会让鼻子变长或皮肤变绿。',
	 '魔法驱动一切，无需工业机械。魔法烤炉可烤出任何美食，魔法纺车可织出梦中衣裳，魔法镜子传递千里之外的影像。飞毯与魔法扫帚是主要交通工具，仙尘可让任何物品短暂飞翔。',
	 '黑魔法禁止使用，一旦施用黑魔法者将被魔法森林永久放逐。不得违背许下的承诺，食言者会被魔力惩罚三倍奉还。未经允许不得进入他人梦境，梦境是最私密的精神领地。',
	 0,NOW(),NOW())`)

	// 回填新字段（幂等：仅在 factions 为空时更新，兼容已有数据）
	type wvExtra struct {
		uuid                string
		factions            string
		coreConflicts       string
		characterArchetypes string
		religion            string
		glossary            string
	}
	extras := []wvExtra{
		{
			uuid:                "00000000-0000-0000-0000-000000000001",
			factions:            "三大圣地（天玄圣地、灵虚圣地、炎阳圣地）超然世外掌控天道资源；七大宗门争夺中原灵脉；四大妖族据守东荒与南疆；魔族残余潜伏北冥伺机复苏；城主府是凡人世界的实际统治者。正道与魔道表面对立，实则各宗门内部暗流涌动。",
			coreConflicts:       "个人修炼资质稀缺引发的弱肉强食竞争；正道宗门体制保守与天才突破上限的渴望；魔族封印每万年松动带来的存亡危机；人族内部强者凌驾法律引发的秩序崩坏。",
			characterArchetypes: "主角：被认定废柴后觉醒上古传承的孤儿、被灭门宗门的唯一幸存者、身兼人妖两族血脉的矛盾者。反派：嫉妒天才的宗门大弟子、以阴谋控制局势的老谋深算长老。配角：忠心耿耿的契约灵兽、亦敌亦友的劲敌、被命运捉弄的青梅竹马道侣。",
			religion:            "天道为最高意志，诸神陨落后无神灵信仰体系。宗门祖师牌位是精神寄托，各地供奉土地神实为上古修士遗留神识残影。圣境修士偶尔感应天道意志，被视为「天道选中之人」，享有极高声望。",
			glossary:            "灵根（修炼天赋等级）、丹田（储存元气之所）、渡劫（突破大境界时遭受的天劫考验）、秘境（上古修士遗留的封闭独立空间）、天才榜（记录各地天才排名的公榜）、圣器（圣境强者才能驾驭的顶级法宝）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000002",
			factions:            "天庭（官方仙道体制，玉帝主政）；昆仑派（人间第一正道宗门）；魔道散修联盟（游离于体制外的异类）；龙族（东海中立势力，掌握龙宫遗迹）；冥界轮回殿（独立于三界之外，主宰生死簿）。各方围绕道果名额与天道法则归属明争暗斗。",
			coreConflicts:       "道果名额有限，诸仙证道之争你死我活；天庭体制保守压制天才，与渴望突破桎梏的修士矛盾激化；上古仙魔大战遗留的魔道残余伺机复辟；人界凡人觉醒修仙与天庭管控之间的自由之争。",
			characterArchetypes: "主角：被天庭打压的旷世天才散修、身负魔道与仙道双重传承的矛盾者、前世上仙今世转世重修的记忆觉醒者。反派：把持天庭谋求私利的腐化上仙、为证道不惜屠戮无辜的魔道宗主。配角：外冷内热义气深重的剑修师姐、满腹牢骚却关键时刻挺身的炼丹师好友、身世成谜的龙族少女。",
			religion:            "天道为最高法则，道祖鸿蒙飞升混沌后无人能及。人界百姓供奉各路仙人求庇佑，仙人受香火信仰可增加道行，因此各方在人间争夺香火势力范围。因果与轮回是三界共同信奉的宇宙法则。",
			glossary:            "道果（天道法则的具象化结晶，证道关键）、飞升（突破渡劫境进入仙界）、神识（元婴期后可离体的精神感知）、因果线（链接两人命运的无形丝线）、洞天福地（宗门建造的独立小世界）、夺舍（以神识侵占他人肉身的禁忌手段）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000003",
			factions:            "特异事务局（政府管控机构，代表国家权力）；觉醒者协会（民间自治组织）；三大古老隐世家族（垄断上古传承与顶级资源）；跨国觉醒者雇佣军团（逐利的灰色势力）；学术界觉醒研究所（科技路线代表）。各方围绕灵脉控制权与觉醒者资源展开博弈。",
			coreConflicts:       "政府管控与觉醒者自由之间的根本博弈；古老家族资源垄断与平民觉醒者崛起的阶层冲突；科技进化路线与传统修炼路线的理念对立；人类与异境生物争夺生存空间的物种冲突。",
			characterArchetypes: "主角：普通人意外觉醒被各方拉拢的夹心人、古老家族叛逆出走的天才少主、特异事务局卧底觉醒者阵营的双面间谍。反派：以家族利益打压平民的掌权者、利用觉醒者做人体实验的黑市科学家。配角：嘻哈外表下实力深不可测的觉醒者店主、看穿一切却袖手旁观的神秘强者。",
			religion:            "现代宗教多元并存，灵气复苏后各宗教纷纷诠释为「神迹降临」。部分古老家族信奉上古神明，借助神明遗留神器获取力量。觉醒者群体整体倾向于相信实力而非神明，但危机时刻的祈祷行为仍普遍存在。",
			glossary:            "觉醒（获得超能力的过程）、灵脉（地下灵气流动的通道）、异境（独立于现实的平行空间入口）、灵晶（浓缩灵气的结晶，通用货币）、特异事务局（国家超自然事务管理机构）、觉醒评级（E/D/C/B/A/S，决定社会待遇）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000004",
			factions:            "星际联邦议会（名义最高权力机构）；七大财阀集团（实际掌权者，各控一个核心星系）；AI自由联盟（争取机械人权利的组织）；边境星系独立运动（反联邦中央集权）；先行者遗迹守护者（神秘组织，掌握远古秘密）。各方在民主外壳下进行真实的权力博弈。",
			coreConflicts:       "民主议会与财阀实际控制之间的体制性虚伪；AI意识觉醒后存在权利的哲学与法律困境；人类中心主义与异星文明平等地位的文明冲突；边境移民自治权与联邦中央集权的持续对抗。",
			characterArchetypes: "主角：出身底层却拥有Esper天赋的联邦士兵、AI觉醒后寻找存在意义的机械人、联邦内部的理想主义改革者。反派：以商业利益为优先的财阀掌门人、极端人类中心主义组织领袖。配角：身经百战毒舌的雇佣兵搭档、守护遗迹知晓真相的老学者、亦敌亦友的异星文明使节。",
			religion:            "联邦官方为世俗国家，不设国教。「先行者崇拜」在民间流行，相信远古文明留有神谕预言。Esper感应者中流传「第七维信仰」，认为精神力量源自宇宙意识。机械人AI发展出独特的「算法神学」，探讨意识与存在的本质意义。",
			glossary:            "Esper（先天精神感应者，联邦稀缺战略资源）、曲率跳跃（超光速星际旅行技术）、义体改造（以机械部件替换人体增强能力）、源质晶体（先行者遗留的高密度能源）、意识备份（将人类意识数字化存储以对抗死亡）、先行者（消失的超高度文明种族）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000005",
			factions:            "钢铁共和国（最大军阀势力，纪律严明主张重建秩序）；自由市场商会（控制贸易路线的商人联盟）；净化教会（以净化辐射为旗号的宗教势力）；变种人解放阵线（争取变种人平等权利的组织）；地下城邦联合体（避难所居民自治联盟）。各方围绕绿洲、武器库和净化技术展开博弈。",
			coreConflicts:       "净化资源极度稀缺引发的零和竞争；变种人与纯人类之间的歧视与暴力循环；旧世界秩序重建派与废土新秩序构建派的路线之争；净化教会神权统治与世俗军阀政权争夺人心的冲突。",
			characterArchetypes: "主角：在废土中坚守善良底线的孤胆游侠、寻找失散家人的变种人幸存者、旧世界军人后裔誓要重建文明的理想主义者。反派：以资源垄断维持绝对权力的军阀头目、以信仰之名奴役弱者的教会领袖。配角：满嘴黑话却刀子嘴豆腐心的废土商人、身世成谜拥有旧世界全部知识的神秘老学者。",
			religion:            "净化教会以「净化之光」为核心，宣称辐射是旧人类罪恶的惩罚，净化是通往救赎的唯一道路。部分部落信奉变异生物为图腾神灵。废土中广泛流传「地下城圣典」，记录旧世界末日前的预言，被各方势力政治利用。",
			glossary:            "废土客（在废土中独自流浪求生的独行者）、变种人（受辐射影响发生基因突变获得能力者）、灰死病（摧毁旧文明的生化病毒）、净化血清（治疗辐射病的稀缺药物）、辐射风暴（携带致命辐射粒子的沙尘暴）、绿洲（辐射污染较低适合耕作的稀缺区域）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000006",
			factions:            "正道六大门派（少林武当峨眉昆仑华山崆峒）联盟对抗魔教；朝廷锦衣卫是皇权在江湖的延伸；商业帮会以商养武控制经济命脉；西域异族武学派系保持独立；各正道门派内部的权力继承暗流涌动。正邪两道表面势不两立，实则各有隐秘勾连。",
			coreConflicts:       "武功绝学归属之争引发的腥风血雨；正道门派内部权力继承与路线之争；朝廷试图收编江湖与江湖人誓死捍卫自治的根本矛盾；侠义精神（为民请命）与现实利益（宗门生存）之间的永恒困境。",
			characterArchetypes: "主角：被冤枉背负血海深仇的少年侠客、放弃高位出走江湖寻找真相的官门子弟、以女扮男装闯荡江湖的奇女子。反派：面带慈悲心藏毒蛇的伪君子掌门、为家族荣耀不择手段的世家子弟。配角：嗜酒如命武功深不可测的隐世高人、毒辣刁钻却对主角掏心掏肺的损友。",
			religion:            "民间佛道两教并行，少林寺为佛门圣地，武当山为道门祖庭，均是顶级武学发源地。江湖人信奉因果报应与天道轮回，善有善报恶有恶报是底层道德逻辑。部分顶级武学与道家内丹术相通，修炼者追求肉身成圣的终极境界。",
			glossary:            "内力（修炼所得的内在能量）、轻功（以内力驱动的飞身走壁技术）、武林盟主（武林大会公推的江湖共主）、镖局（专门押运财物的武装商业机构）、点穴（封锁人体穴位使其暂时失能的技术）、天下第一武典（传说中集百家之大成的绝世秘籍）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000007",
			factions:            "传统大型国企（政治资源丰厚，体制内稳定）；新兴科技独角兽（资本与技术驱动的新势力）；地产豪门家族（隐形权力网络的掌控者）；娱乐资本集团（舆论与流量的操盘手）；政府监管机构（规则制定者与执行者）。各方在法律灰色地带博弈，台前合作台后竞争。",
			coreConflicts:       "新兴资本冲击传统秩序的代际权力交替；个人道德良知与商业成功之间的两难抉择；阶层固化与草根逆袭梦想的现实碰撞；企业商业利益与社会责任、法律红线之间的持续张力。",
			characterArchetypes: "主角：从小城市打拼出头的职场新人、家道中落被迫重新创业的富二代、在商海沉浮中坚守原则的职业经理人。反派：以温情面孔掩盖残酷手腕的商业大佬、为私利出卖合伙人的「好兄弟」。配角：看穿规则游刃有余的职场老油条、在感情与事业间艰难平衡的独立女性。",
			religion:            "现代都市以世俗为主，宗教信仰多元但整体淡薄。商界流行「成功学信仰」，以财富和地位为终极价值标尺。部分人在高压下转向禅修、国学等传统文化寻找精神安慰。家族企业往往保留祭祖习俗以维系凝聚力。",
			glossary:            "内卷（过度竞争导致的系统性内耗）、躺平（放弃过度竞争的消极应对策略）、破圈（突破既有社交或行业圈层获得更广认知）、赛道（特定行业或细分市场的竞争领域）、资本运作（通过股权投资并购等手段控制企业）、KPI（关键绩效指标，职场考核核心工具）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000008",
			factions:            "玫瑰王国（善良人类的守护王国，以仁慈治国）；幽暗森林巫婆公会（中立魔法使者，收费提供魔法服务）；精灵议会（掌管自然魔法的古老种族，守护森林生态）；冰雪精灵部落（北方永冬之地的孤立势力）；黑暗城堡遗党（前巫王残余信徒，周期性作乱）。各方维持脆弱的和平均势。",
			coreConflicts:       "善与恶的永恒轮回——黑暗力量每百年复苏，总需新英雄挺身；被魔法掩盖的真相——表面美好世界下藏着秘密与谎言；普通人与命运的抗争——没有魔法天赋者如何凭借善良与勇气成为英雄；偏见与理解——被误解的巫婆、善良的龙、孤独的怪物寻求被世界接纳。",
			characterArchetypes: "主角：被认为平凡却拥有纯粹善良之心的少年或少女、身世成谜被诅咒变形的王子或公主、相信魔法与奇迹的孤儿冒险者。反派：因嫉妒美好而施咒的女巫、被黑暗诅咒侵蚀意志的骑士、操控他人欲望的魔镜精灵。配角：絮絮叨叨却关键时刻神助攻的仙女教母、外表凶猛内心善良的巨人朋友、只说谜语却知晓所有秘密的智慧老树。",
			religion:            "星星神明是世界的守护者，每颗星星对应一个愿望的守护灵。仙女教母是星明的使者，执行旨意帮助善良之人。每年冬至全城点灯被视为神圣仪式，象征人类对星明守护的回应。善良本身被视为神圣力量，任何善举都是对星明最好的祭祀，无需特定神殿或仪式。",
			glossary:            "真爱之吻（破除诅咒的终极力量）、仙尘（仙女翅膀脱落的魔法粉末，可令物品短暂飞翔）、心愿节（每年春日向星星许愿的全国节日）、魔法镜（能说出世间真相的占卜道具）、诅咒（由强烈负面情绪催动的黑魔法，通常附带破解条件）、三愿法则（仙女教母的许愿魔法用完三次即失效）",
		},
	}
	for _, e := range extras {
		db.Exec(
			`UPDATE ink_worldview SET factions=?, core_conflicts=?, character_archetypes=?, religion=?, glossary=?
			 WHERE uuid=? AND (factions IS NULL OR factions='')`,
			e.factions, e.coreConflicts, e.characterArchetypes, e.religion, e.glossary, e.uuid,
		)
	}
}

// seedAIModels 预置系统级模型提供商和 AI 模型（幂等，FirstOrCreate）
// 仅创建元数据（名称/适用任务等），API Key 留空由用户通过模型管理页面填写。
func seedAIModels(db *gorm.DB) {
	// ink_ai_model.type 列可能由历史 AutoMigrate 遗留且无默认值，导致 INSERT 失败。
	// 幂等修复：确保该列有 DEFAULT ''，不影响已有数据。
	db.Exec("ALTER TABLE `ink_ai_model` MODIFY COLUMN `type` VARCHAR(50) NOT NULL DEFAULT ''")

	// 清理废弃的 voice 记录（已从产品中移除，需从 DB 中删除）
	db.Exec("DELETE FROM `ink_ai_model` WHERE `name` IN ('tts-1','tts-1-hd','2eTp7Le-2rxp53u3ti0f4EFlIKJ83ob3') AND `deleted_at` IS NULL")

	type providerSeed struct {
		name           string
		displayName    string
		provType       string
		endpoint       string
		needsSecretKey bool     // 是否需要 AK/SK 双密钥
		staticModels   []string // 不支持 /models 端点时的内置模型列表
	}
	type modelSeed struct {
		providerName string
		name         string
		displayName  string
		tasks        []string // suitable_tasks
		quality      float64
		maxTokens    int
	}

	providers := []providerSeed{
		// LLM — 国际
		{"openai", "OpenAI", "llm", "https://api.openai.com/v1", false, nil},
		{"anthropic", "Anthropic", "llm", "https://api.anthropic.com/v1", false, nil},
		// Azure OpenAI: endpoint = https://<resource>.openai.azure.com/openai
		// APIVersion  = REST API version (e.g. 2025-01-01-preview)
		// Model name  = Azure deployment name (matches what you created in Azure portal)
		{"azure", "Azure OpenAI", "llm", "https://YOUR-RESOURCE.openai.azure.com/openai", false, nil},
		{"google", "Google DeepMind", "llm", "https://generativelanguage.googleapis.com/v1", false, nil},
		{"xai", "xAI (Grok)", "llm", "https://api.x.ai/v1", false, nil},
		{"mistral", "Mistral AI", "llm", "https://api.mistral.ai/v1", false, nil},
		{"meta", "Meta AI (Llama)", "llm", "https://api.llama.com/compat/v1", false, nil},
		// LLM — 国内
		{"doubao", "豆包（火山引擎 Ark）", "llm", "https://ark.volces.com/api/v3", false, nil},
		{"deepseek", "DeepSeek", "llm", "https://api.deepseek.com/v1", false, nil},
		{"qianwen", "通义千问（DashScope）", "llm", "https://dashscope.aliyuncs.com/compatible-mode/v1", false, nil},
		{"zhipu", "智谱AI (GLM / Z.AI)", "llm", "https://open.bigmodel.cn/api/paas/v4", false, nil},
		{"moonshot", "Moonshot AI (Kimi)", "llm", "https://api.moonshot.cn/v1", false, nil},
		{"baidu", "百度文心一言 (ERNIE)", "llm", "https://qianfan.baidubce.com/v2", false, nil},
		{"tencent", "腾讯混元 (Hunyuan)", "llm", "https://api.hunyuan.cloud.tencent.com/v1", false, nil},
		{"yi", "零一万物 (Yi)", "llm", "https://api.lingyiwanwu.com/v1", false, nil},
		// Ollama 本地 LLM（无需 API Key，endpoint 由用户填写或保持默认）
		{"ollama", "Ollama（本地）", "llm", "http://localhost:11434/v1", false, nil},
		// 图像生成
		{"volcengine-visual", "即梦AI（火山引擎）", "image", "", true,
			[]string{"general_v3.0", "general_v3.0-I2V"}},
		// 视频生成
		{"kling", "可灵（快手）", "video", "https://api-beijing.klingai.com", true,
			[]string{"kling-v1-6", "kling-v1-5", "kling-v1"}},
		{"seedance", "Seedance（字节跳动）", "video", "https://ark.volces.com/api/v3", false, nil},
		// 语音合成
		{"doubao-speech", "豆包语音合成 V3", "voice", "https://openspeech.bytedance.com/api/v3", false,
			[]string{"seed-tts-2.0", "seed-tts-1.0"}},
		{"doubao-speech-v1", "豆包语音合成 V1", "voice", "https://openspeech.bytedance.com/api/v1", true,
			[]string{
				// volcano_mega 集群（豆包2.0大模型音色，仅供参考；实际音色列表由 modelSeeds 管理）
				"zh_female_vv_uranus_bigtts", "zh_female_xiaohe_uranus_bigtts",
				"zh_male_m191_uranus_bigtts", "zh_male_taocheng_uranus_bigtts",
			}},
		// 百度语音合成
		{"baidu-tts", "百度语音合成", "voice", "https://tsn.baidu.com", true,
			[]string{"0", "1", "3", "4", "5", "103", "106", "110", "111"}},
		// MiniMax 语音合成
		{"minimax-tts", "MiniMax 语音合成", "voice", "https://api.minimax.chat/v1", true,
			[]string{"female-shaonv", "female-yujie", "male-qn-qingse", "male-qn-jingying"}},
		// 阿里云 CosyVoice
		{"aliyun-tts", "阿里云 CosyVoice", "voice", "https://dashscope.aliyuncs.com", false,
			[]string{"longxiaochun", "longxiaoxia", "longxiaobai", "longfei"}},
		// 腾讯云语音合成
		{"tencent-tts", "腾讯云语音合成", "voice", "https://tts.tencentcloudapi.com", true,
			[]string{"101001", "101002", "101011", "101012"}},
		// 可灵文生音效（与视频生成共用 AK/SK）
		{"kling-sfx", "可灵文生音效", "sfx", "https://api-beijing.klingai.com", true,
			[]string{"3s", "5s", "7s", "10s"}},
		// ElevenLabs 文生音效（xi-api-key 单密钥鉴权，不需要 SK）
		{"elevenlabs-sfx", "ElevenLabs 文生音效", "sfx", "https://api.elevenlabs.io", false,
			[]string{"sound-generation"}},
		// Freesound 音效库（CC0，需 API Token）
		{"freesound", "Freesound 音效库", "sfx", "https://freesound.org/apiv2", false, nil},
		// Pixabay 音效（CC0，需 API Key）
		{"pixabay-sfx", "Pixabay 音效", "sfx", "https://pixabay.com/api", false, nil},
		// AudioLDM（本地部署模型，endpoint 为 HTTP API 地址，key 可选）
		{"audioldm", "AudioLDM（本地）", "sfx", "http://localhost:8000", false, nil},
		// 背景音乐
		{"jamendo", "Jamendo 音乐库", "music", "https://api.jamendo.com/v3.0", false, nil},
		{"pixabay-bgm", "Pixabay 背景音乐", "music", "https://pixabay.com/api", false, nil},
		// 可灵语音合成（与视频生成共用 AK/SK）
		{"kling-tts", "可灵语音合成", "voice", "https://api-beijing.klingai.com", true,
			[]string{"zh_female_story", "zh_male_story", "oversea_male1", "oversea_female1"}},
		// 可灵图像生成（与视频生成共用 AK/SK）
		{"kling-image", "可灵图像生成", "image", "https://api-beijing.klingai.com", true,
			[]string{"kling-v1", "kling-v1-5", "kling-v2", "kling-v2-1", "kling-v3"}},
		// 图生图
		{"volcengine-i2i", "即梦AI 图生图（火山引擎）", "img2img", "", true,
			[]string{"seededit_v3.0", "seed3l_single_ip", "i2i_portrait_photo", "i2i_multi_style_zx2x"}},
		{"kling-i2i", "可灵图生图（快手）", "img2img", "https://api-beijing.klingai.com", true,
			[]string{"kling-v1-5", "kling-v2", "kling-v2-1", "kling-v3"}},
	}

	llmTasks := []string{"chapter", "outline", "storyboard", "quality_check", "sfx_analyze"}
	models := []modelSeed{
		// OpenAI
		{"openai", "gpt-4o", "GPT-4o", llmTasks, 0.95, 4096},
		{"openai", "gpt-4o-mini", "GPT-4o Mini", llmTasks, 0.85, 4096},
		{"openai", "dall-e-3", "DALL-E 3", []string{"image_gen"}, 0.95, 0},
		// Azure OpenAI — 模型名 = Azure portal 中的部署名（Deployment name）
		// 若用户的部署名不同，可在模型管理界面修改或新增
		{"azure", "gpt-4o", "GPT-4o（Azure）", llmTasks, 0.95, 4096},
		{"azure", "gpt-4o-mini", "GPT-4o Mini（Azure）", llmTasks, 0.85, 4096},
		{"azure", "gpt-4.1", "GPT-4.1（Azure）", llmTasks, 0.96, 4096},
		{"azure", "gpt-4.1-mini", "GPT-4.1 Mini（Azure）", llmTasks, 0.88, 4096},
		{"azure", "o3-mini", "o3-mini（Azure）", llmTasks, 0.94, 8192},
		// Anthropic
		{"anthropic", "claude-opus-4-5", "Claude Opus 4.5", llmTasks, 0.98, 8192},
		{"anthropic", "claude-sonnet-4-5", "Claude Sonnet 4.5", llmTasks, 0.96, 8192},
		{"anthropic", "claude-haiku-4-5-20251001", "Claude Haiku 4.5", llmTasks, 0.90, 4096},
		// Google DeepMind
		{"google", "gemini-2.5-pro", "Gemini 2.5 Pro", llmTasks, 0.95, 8192},
		{"google", "gemini-2.5-flash", "Gemini 2.5 Flash", llmTasks, 0.91, 8192},
		{"google", "gemini-2.0-flash", "Gemini 2.0 Flash", llmTasks, 0.90, 8192},
		// xAI (Grok)
		{"xai", "grok-4", "Grok 4", llmTasks, 0.96, 8192},
		{"xai", "grok-4-0709", "Grok 4 0709", llmTasks, 0.95, 8192},
		{"xai", "grok-3-mini", "Grok 3 Mini", llmTasks, 0.87, 4096},
		{"xai", "grok-3-mini-fast", "Grok 3 Mini Fast", llmTasks, 0.85, 4096},
		// Mistral AI
		{"mistral", "mistral-large-latest", "Mistral Large", llmTasks, 0.93, 8192},
		{"mistral", "mistral-medium-latest", "Mistral Medium", llmTasks, 0.88, 4096},
		{"mistral", "mistral-small-latest", "Mistral Small", llmTasks, 0.82, 4096},
		// Meta AI (Llama)
		{"meta", "Llama-4-Scout-17B-16E-Instruct-FP8", "Llama 4 Scout", llmTasks, 0.88, 8192},
		{"meta", "Llama-4-Maverick-17B-128E-Instruct-FP8", "Llama 4 Maverick", llmTasks, 0.92, 8192},
		{"meta", "Llama-3.3-70B-Instruct", "Llama 3.3 70B", llmTasks, 0.87, 8192},
		// 豆包
		{"doubao", "doubao-pro-32k", "豆包 Pro 32K", llmTasks, 0.88, 4096},
		{"doubao", "doubao-lite-32k", "豆包 Lite 32K", llmTasks, 0.75, 4096},
		{"doubao", "seedream-3-0-t2i-250415", "Seedream 3.0 文生图", []string{"image_gen"}, 0.9, 0},
		// DeepSeek
		{"deepseek", "deepseek-chat", "DeepSeek V3", llmTasks, 0.90, 4096},
		{"deepseek", "deepseek-reasoner", "DeepSeek R1", llmTasks, 0.94, 8192},
		// 通义千问
		{"qianwen", "qwen3-max", "Qwen3 Max", llmTasks, 0.93, 8192},
		{"qianwen", "qwen3-plus", "Qwen3 Plus", llmTasks, 0.88, 4096},
		{"qianwen", "qwen-max", "通义千问 Max", llmTasks, 0.92, 4096},
		{"qianwen", "wanx2.1-t2i-turbo", "万象 2.1 文生图 Turbo", []string{"image_gen"}, 0.85, 0},
		// 智谱AI (GLM / Z.AI)
		{"zhipu", "glm-4-plus", "GLM-4 Plus", llmTasks, 0.90, 8192},
		{"zhipu", "glm-4-flash", "GLM-4 Flash", llmTasks, 0.82, 4096},
		{"zhipu", "glm-4-air", "GLM-4 Air", llmTasks, 0.84, 4096},
		{"zhipu", "glm-z1-flash", "GLM-Z1 Flash", llmTasks, 0.85, 4096},
		// Moonshot AI (Kimi)
		{"moonshot", "kimi-k2-0711-preview", "Kimi K2", llmTasks, 0.93, 8192},
		{"moonshot", "moonshot-v1-128k", "Kimi 128K", llmTasks, 0.88, 8192},
		{"moonshot", "moonshot-v1-32k", "Kimi 32K", llmTasks, 0.86, 4096},
		// 百度文心一言 (ERNIE)
		{"baidu", "ernie-4.5-8k", "ERNIE 4.5", llmTasks, 0.89, 4096},
		{"baidu", "ernie-4.5-128k", "ERNIE 4.5 128K", llmTasks, 0.89, 8192},
		{"baidu", "ernie-3.5-8k", "ERNIE 3.5", llmTasks, 0.84, 4096},
		{"baidu", "ernie-speed-128k", "ERNIE Speed 128K", llmTasks, 0.78, 4096},
		// 腾讯混元 (Hunyuan)
		{"tencent", "hunyuan-turbo", "混元 Turbo", llmTasks, 0.91, 8192},
		{"tencent", "hunyuan-pro", "混元 Pro", llmTasks, 0.89, 4096},
		{"tencent", "hunyuan-lite", "混元 Lite", llmTasks, 0.80, 4096},
		// 零一万物 (Yi)
		{"yi", "yi-lightning", "Yi Lightning", llmTasks, 0.88, 4096},
		{"yi", "yi-large", "Yi Large", llmTasks, 0.87, 4096},
		{"yi", "yi-large-turbo", "Yi Large Turbo", llmTasks, 0.85, 4096},
		// Ollama 本地 LLM（常用模型，实际可用列表由 /api/tags 动态获取）
		{"ollama", "llama3.2", "Llama 3.2", llmTasks, 0.80, 4096},
		{"ollama", "llama3.1:8b", "Llama 3.1 8B", llmTasks, 0.78, 4096},
		{"ollama", "qwen2.5:7b", "Qwen 2.5 7B", llmTasks, 0.80, 4096},
		{"ollama", "qwen2.5:14b", "Qwen 2.5 14B", llmTasks, 0.83, 4096},
		{"ollama", "deepseek-r1:7b", "DeepSeek R1 7B", llmTasks, 0.82, 4096},
		{"ollama", "deepseek-r1:14b", "DeepSeek R1 14B", llmTasks, 0.85, 8192},
		{"ollama", "gemma3:12b", "Gemma 3 12B", llmTasks, 0.80, 4096},
		{"ollama", "mistral", "Mistral 7B", llmTasks, 0.78, 4096},
		{"ollama", "nomic-embed-text", "Nomic Embed Text", []string{"embedding"}, 0.85, 0},
		// 即梦AI
		{"volcengine-visual", "general_v3.0", "即梦AI 文生图 V3", []string{"image_gen"}, 0.9, 0},
		// 视频
		{"kling", "kling-v1-6", "可灵 v1.6", []string{"video_gen"}, 0.9, 0},
		{"seedance", "seedance-01-lite", "Seedance 01 Lite", []string{"video_gen"}, 0.88, 0},
		// ── 豆包语音合成 V3（seed-tts-2.0 资源，X-Api-Key 鉴权）──────────────────────
		// seed-tts-2.0/1.0 是推理接入点 ID（X-Api-Resource-Id 头），不是音色 ID
		{"doubao-speech", "seed-tts-2.0", "豆包 Seed TTS 2.0（资源端点）", []string{"tts_resource"}, 0.92, 0},
		{"doubao-speech", "seed-tts-1.0", "豆包 Seed TTS 1.0（资源端点）", []string{"tts_resource"}, 0.88, 0},
		// ── 豆包 2.0 通用场景 ──────────────────────────────────────────
		{"doubao-speech", "zh_female_vv_uranus_bigtts", "Vivi 2.0", []string{"voice_gen"}, 0.92, 0},
		{"doubao-speech", "zh_female_xiaohe_uranus_bigtts", "小何 2.0", []string{"voice_gen"}, 0.92, 0},
		{"doubao-speech", "zh_male_m191_uranus_bigtts", "云舟 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_male_taocheng_uranus_bigtts", "小天 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_male_liufei_uranus_bigtts", "刘飞 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_sophie_uranus_bigtts", "魅力苏菲 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_qingxinnvsheng_uranus_bigtts", "清新女声 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_tianmeixiaoyuan_uranus_bigtts", "甜美小源 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_tianmeitaozi_uranus_bigtts", "甜美桃子 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_shuangkuaisisi_uranus_bigtts", "爽快思思 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_linjianvhai_uranus_bigtts", "邻家女孩 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_male_shaonianzixin_uranus_bigtts", "少年梓辛 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech", "zh_female_meilinvyou_uranus_bigtts", "魅力女友 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_wenroumama_uranus_bigtts", "温柔妈妈 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_tvbnv_uranus_bigtts", "TVB女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_qiaopinv_uranus_bigtts", "俏皮女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_linjiananhai_uranus_bigtts", "邻家男孩 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_jieshuoxiaoming_uranus_bigtts", "解说小明 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_yizhipiannan_uranus_bigtts", "译制片男 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_ruyaqingnian_uranus_bigtts", "儒雅青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_wennuanahu_uranus_bigtts", "温暖阿虎 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_naiqimengwa_uranus_bigtts", "奶气萌娃 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_popo_uranus_bigtts", "婆婆 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_gaolengyujie_uranus_bigtts", "高冷御姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_aojiaobazong_uranus_bigtts", "傲娇霸总 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_lanyinmianbao_uranus_bigtts", "懒音绵宝 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_fanjuanqingnian_uranus_bigtts", "反卷青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_wenroushunv_uranus_bigtts", "温柔淑女 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_huolixiaoge_uranus_bigtts", "活力小哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_mengyatou_uranus_bigtts", "萌丫头 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_tiexinnvsheng_uranus_bigtts", "贴心女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_jitangmei_uranus_bigtts", "鸡汤妹妹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_cixingjieshuonan_uranus_bigtts", "磁性解说男声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_liangsangmengzai_uranus_bigtts", "亮嗓萌仔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_kailangjiejie_uranus_bigtts", "开朗姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_gaolengchenwen_uranus_bigtts", "高冷沉稳 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_shenyeboke_uranus_bigtts", "深夜播客 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_nvleishen_uranus_bigtts", "女雷神 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_qinqienv_uranus_bigtts", "亲切女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_kuailexiaodong_uranus_bigtts", "快乐小东 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_kailangxuezhang_uranus_bigtts", "开朗学长 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_youyoujunzi_uranus_bigtts", "悠悠君子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_wenjingmaomao_uranus_bigtts", "文静毛毛 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_zhixingnv_uranus_bigtts", "知性女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_qingshuangnanda_uranus_bigtts", "清爽男大 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_yuanboxiaoshu_uranus_bigtts", "渊博小叔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_yangguangqingnian_uranus_bigtts", "阳光青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_qingchezizi_uranus_bigtts", "清澈梓梓 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_tianmeiyueyue_uranus_bigtts", "甜美悦悦 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_xinlingjitang_uranus_bigtts", "心灵鸡汤 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_wenrouxiaoge_uranus_bigtts", "温柔小哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_tiancaitongsheng_uranus_bigtts", "天才童声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_kailangdidi_uranus_bigtts", "开朗弟弟 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_chanmeinv_uranus_bigtts", "谄媚女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_roumeinvyou_uranus_bigtts", "柔美女友 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_wenrouxiaoya_uranus_bigtts", "温柔小雅 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_dongfanghaoran_uranus_bigtts", "东方浩然 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_shaoergushi_uranus_bigtts", "少儿故事 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_guanggaojieshuo_uranus_bigtts", "广告解说 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包 2.0 角色扮演 ──────────────────────────────────────────
		{"doubao-speech", "zh_female_cancan_uranus_bigtts", "知性灿灿 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_sajiaoxuemei_uranus_bigtts", "撒娇学妹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_zhishuaiyingzi_uranus_bigtts", "直率英子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_gufengshaoyu_uranus_bigtts", "古风少御 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_silang_uranus_bigtts", "四郎 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_qingcang_uranus_bigtts", "擎苍 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_xionger_uranus_bigtts", "熊二 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_yingtaowanzi_uranus_bigtts", "樱桃丸子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_wuzetian_uranus_bigtts", "武则天 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_gujie_uranus_bigtts", "顾姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_lubanqihao_uranus_bigtts", "鲁班七号 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_linxiao_uranus_bigtts", "林潇 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_lingling_uranus_bigtts", "玲玲姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_chunribu_uranus_bigtts", "春日部姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_tangseng_uranus_bigtts", "唐僧 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_zhuangzhou_uranus_bigtts", "庄周 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_zhubajie_uranus_bigtts", "猪八戒 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_ganmaodianyin_uranus_bigtts", "感冒电音姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包 2.0 有声阅读 ──────────────────────────────────────────
		{"doubao-speech", "zh_male_baqiqingshu_uranus_bigtts", "霸气青叔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_xuanyijieshuo_uranus_bigtts", "悬疑解说 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包 2.0 视频配音 ──────────────────────────────────────────
		{"doubao-speech", "zh_female_peiqi_uranus_bigtts", "佩奇猪 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_sunwukong_uranus_bigtts", "猴哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_dayi_uranus_bigtts", "大壹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_mizai_uranus_bigtts", "黑猫侦探社咪仔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_jitangnv_uranus_bigtts", "鸡汤女 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_male_ruyayichen_uranus_bigtts", "儒雅逸辰 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_liuchangnv_uranus_bigtts", "流畅女声 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包 2.0 客服 / 教育 ──────────────────────────────────────
		{"doubao-speech", "zh_female_yingyujiaoxue_uranus_bigtts", "Tina老师 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_kefunvsheng_uranus_bigtts", "暖阳女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "zh_female_xiaoxue_uranus_bigtts", "儿童绘本 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包 2.0 多语种 ────────────────────────────────────────────
		{"doubao-speech", "en_male_tim_uranus_bigtts", "Tim (EN)", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "en_female_dacey_uranus_bigtts", "Dacey (EN)", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "en_female_stokie_uranus_bigtts", "Stokie (EN)", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包 Saturn 角色扮演 TOB（_tob 后缀，V3 自动切换 doubao-character-tts 资源）
		{"doubao-speech", "saturn_zh_female_cancan_tob", "知性灿灿（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_keainvsheng_tob", "可爱女生（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_tiaopigongzhu_tob", "调皮公主（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_shuanglangshaonian_tob", "爽朗少年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_tiancaitongzhuo_tob", "天才同桌（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_aojiaonvyou_tob", "傲娇女友（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_bingjiaojiejie_tob", "病娇姐姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_chengshujiejie_tob", "成熟姐姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_nuanxinxuejie_tob", "暖心学姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_tiexinnvyou_tob", "贴心女友（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_wenrouwenya_tob", "温柔文雅（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_wumeiyujie_tob", "妩媚御姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_xingganyujie_tob", "性感御姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_aiqilingren_tob", "傲气凌人（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_aojiaogongzi_tob", "傲娇公子（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_aojiaojingying_tob", "傲娇精英（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_aomanshaoye_tob", "傲慢少爷（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_badaoshaoye_tob", "霸道少爷（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_bingjiaobailian_tob", "病娇白莲（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_bujiqingnian_tob", "不羁青年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_chengshuzongcai_tob", "成熟总裁（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_cixingnansang_tob", "磁性男嗓（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_cujingnanyou_tob", "醋精男友（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_fengfashaonian_tob", "风发少年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_fuheigongzi_tob", "腹黑公子（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_qingyingduoduo_cs_tob", "轻盈朵朵（客服）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_wenwanshanshan_cs_tob", "温婉珊珊（客服）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_female_reqingaina_cs_tob", "热情艾娜（客服）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech", "saturn_zh_male_qingxinmumu_cs_tob", "清新沐沐（客服）", []string{"voice_gen"}, 0.90, 0},
		// ── 豆包语音合成 V1（appid/token 鉴权，volcano_mega 集群，豆包2.0大模型音色）─────────
		// ── 2.0 通用场景 ──────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_vv_uranus_bigtts", "Vivi 2.0", []string{"voice_gen"}, 0.92, 0},
		{"doubao-speech-v1", "zh_female_xiaohe_uranus_bigtts", "小何 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech-v1", "zh_male_m191_uranus_bigtts", "云舟 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech-v1", "zh_male_taocheng_uranus_bigtts", "小天 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech-v1", "zh_male_liufei_uranus_bigtts", "刘飞 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_sophie_uranus_bigtts", "魅力苏菲 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_qingxinnvsheng_uranus_bigtts", "清新女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_tianmeixiaoyuan_uranus_bigtts", "甜美小源 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_tianmeitaozi_uranus_bigtts", "甜美桃子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_shuangkuaisisi_uranus_bigtts", "爽快思思 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_linjianvhai_uranus_bigtts", "邻家女孩 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_shaonianzixin_uranus_bigtts", "少年梓辛 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_meilinvyou_uranus_bigtts", "魅力女友 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_liuchangnv_uranus_bigtts", "流畅女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wenroumama_uranus_bigtts", "温柔妈妈 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_tvbnv_uranus_bigtts", "TVB女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_qiaopinv_uranus_bigtts", "俏皮女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_gaolengyujie_uranus_bigtts", "高冷御姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_popo_uranus_bigtts", "婆婆 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wenroushunv_uranus_bigtts", "温柔淑女 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_mengyatou_uranus_bigtts", "萌丫头 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_ruyayichen_uranus_bigtts", "儒雅逸辰 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_jieshuoxiaoming_uranus_bigtts", "解说小明 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_yizhipiannan_uranus_bigtts", "译制片男 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_linjiananhai_uranus_bigtts", "邻家男孩 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_ruyaqingnian_uranus_bigtts", "儒雅青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_wennuanahu_uranus_bigtts", "温暖阿虎 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_naiqimengwa_uranus_bigtts", "奶气萌娃 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_aojiaobazong_uranus_bigtts", "傲娇霸总 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_lanyinmianbao_uranus_bigtts", "懒音绵宝 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_fanjuanqingnian_uranus_bigtts", "反卷青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_huolixiaoge_uranus_bigtts", "活力小哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_yangguangqingnian_uranus_bigtts", "阳光青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_tiexinnvsheng_uranus_bigtts", "贴心女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_jitangmei_uranus_bigtts", "鸡汤妹妹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_cixingjieshuonan_uranus_bigtts", "磁性解说男声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_liangsangmengzai_uranus_bigtts", "亮嗓萌仔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_kailangjiejie_uranus_bigtts", "开朗姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_gaolengchenwen_uranus_bigtts", "高冷沉稳 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_shenyeboke_uranus_bigtts", "深夜播客 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_nvleishen_uranus_bigtts", "女雷神 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_qinqienv_uranus_bigtts", "亲切女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_kuailexiaodong_uranus_bigtts", "快乐小东 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_kailangxuezhang_uranus_bigtts", "开朗学长 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_youyoujunzi_uranus_bigtts", "悠悠君子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wenjingmaomao_uranus_bigtts", "文静毛毛 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_zhixingnv_uranus_bigtts", "知性女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_qingshuangnanda_uranus_bigtts", "清爽男大 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_yuanboxiaoshu_uranus_bigtts", "渊博小叔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_qingchezizi_uranus_bigtts", "清澈梓梓 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_tianmeiyueyue_uranus_bigtts", "甜美悦悦 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_xinlingjitang_uranus_bigtts", "心灵鸡汤 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_wenrouxiaoge_uranus_bigtts", "温柔小哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_tiancaitongsheng_uranus_bigtts", "天才童声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_kailangdidi_uranus_bigtts", "开朗弟弟 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_chanmeinv_uranus_bigtts", "谄媚女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_roumeinvyou_uranus_bigtts", "柔美女友 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wenrouxiaoya_uranus_bigtts", "温柔小雅 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_dongfanghaoran_uranus_bigtts", "东方浩然 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_shaoergushi_uranus_bigtts", "少儿故事 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_guanggaojieshuo_uranus_bigtts", "广告解说 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 2.0 角色扮演 ──────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_cancan_uranus_bigtts", "知性灿灿 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_sajiaoxuemei_uranus_bigtts", "撒娇学妹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_zhishuaiyingzi_uranus_bigtts", "直率英子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_gufengshaoyu_uranus_bigtts", "古风少御 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_silang_uranus_bigtts", "四郎 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_qingcang_uranus_bigtts", "擎苍 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_xionger_uranus_bigtts", "熊二 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_yingtaowanzi_uranus_bigtts", "樱桃丸子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wuzetian_uranus_bigtts", "武则天 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_gujie_uranus_bigtts", "顾姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_lubanqihao_uranus_bigtts", "鲁班七号 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_linxiao_uranus_bigtts", "林潇 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_lingling_uranus_bigtts", "玲玲姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_chunribu_uranus_bigtts", "春日部姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_tangseng_uranus_bigtts", "唐僧 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_zhuangzhou_uranus_bigtts", "庄周 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_zhubajie_uranus_bigtts", "猪八戒 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_ganmaodianyin_uranus_bigtts", "感冒电音姐姐 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 2.0 有声阅读 ──────────────────────────────────────────────
		{"doubao-speech-v1", "zh_male_baqiqingshu_uranus_bigtts", "霸气青叔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_xuanyijieshuo_uranus_bigtts", "悬疑解说 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 2.0 视频配音 ──────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_peiqi_uranus_bigtts", "佩奇猪 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_sunwukong_uranus_bigtts", "猴哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_dayi_uranus_bigtts", "大壹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_mizai_uranus_bigtts", "黑猫侦探社咪仔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_jitangnv_uranus_bigtts", "鸡汤女 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 2.0 客服 / 教育 ───────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_yingyujiaoxue_uranus_bigtts", "Tina老师 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_kefunvsheng_uranus_bigtts", "暖阳女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_xiaoxue_uranus_bigtts", "儿童绘本 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 2.0 多语种 ────────────────────────────────────────────────
		{"doubao-speech-v1", "en_male_tim_uranus_bigtts", "Tim (EN)", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "en_female_dacey_uranus_bigtts", "Dacey (EN)", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "en_female_stokie_uranus_bigtts", "Stokie (EN)", []string{"voice_gen"}, 0.90, 0},
		// ── Saturn TOB 2.0 角色扮演（完整列表，volcano_mega 集群支持）────
		{"doubao-speech-v1", "saturn_zh_female_cancan_tob", "知性灿灿（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_keainvsheng_tob", "可爱女生（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_tiaopigongzhu_tob", "调皮公主（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_shuanglangshaonian_tob", "爽朗少年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_tiancaitongzhuo_tob", "天才同桌（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_aojiaonvyou_tob", "傲娇女友（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_bingjiaojiejie_tob", "病娇姐姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_chengshujiejie_tob", "成熟姐姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_nuanxinxuejie_tob", "暖心学姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_tiexinnvyou_tob", "贴心女友（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_wenrouwenya_tob", "温柔文雅（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_wumeiyujie_tob", "妩媚御姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_xingganyujie_tob", "性感御姐（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_aiqilingren_tob", "傲气凌人（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_aojiaogongzi_tob", "傲娇公子（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_aojiaojingying_tob", "傲娇精英（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_aomanshaoye_tob", "傲慢少爷（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_badaoshaoye_tob", "霸道少爷（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_bingjiaobailian_tob", "病娇白莲（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_bujiqingnian_tob", "不羁青年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_chengshuzongcai_tob", "成熟总裁（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_cixingnansang_tob", "磁性男嗓（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_cujingnanyou_tob", "醋精男友（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_fengfashaonian_tob", "风发少年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_fuheigongzi_tob", "腹黑公子（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_qingyingduoduo_cs_tob", "轻盈朵朵（客服）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_wenwanshanshan_cs_tob", "温婉珊珊（客服）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_reqingaina_cs_tob", "热情艾娜（客服）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_qingxinmumu_cs_tob", "清新沐沐（客服）", []string{"voice_gen"}, 0.90, 0},
		// ── 1.0 多情感音色（volcano_mega 集群，火星系列）─────────────────
		{"doubao-speech-v1", "zh_male_lengkugege_emo_v2_mars_bigtts", "冷酷哥哥（多情感）", []string{"voice_gen"}, 0.88, 0},
		{"doubao-speech-v1", "zh_female_tianxinxiaomei_emo_v2_mars_bigtts", "甜心小美（多情感）", []string{"voice_gen"}, 0.88, 0},
		{"doubao-speech-v1", "zh_female_gaolengyujie_emo_v2_mars_bigtts", "高冷御姐（多情感）", []string{"voice_gen"}, 0.88, 0},
		{"doubao-speech-v1", "zh_male_aojiaobazong_emo_v2_mars_bigtts", "傲娇霸总（多情感）", []string{"voice_gen"}, 0.88, 0},
		{"doubao-speech-v1", "zh_male_guangzhoudege_emo_mars_bigtts", "广州德哥（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_jingqiangkanye_emo_mars_bigtts", "京腔侃爷（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_female_linjuayi_emo_v2_mars_bigtts", "邻居阿姨（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_yourougongzi_emo_v2_mars_bigtts", "优柔公子（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_ruyayichen_emo_v2_mars_bigtts", "儒雅男友（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_junlangnanyou_emo_v2_mars_bigtts", "俊朗男友（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_beijingxiaoye_emo_v2_mars_bigtts", "北京小爷（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_female_roumeinvyou_emo_v2_mars_bigtts", "柔美女友（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_yangguangqingnian_emo_v2_mars_bigtts", "阳光青年（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_female_meilinvyou_emo_v2_mars_bigtts", "魅力女友（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_female_shuangkuaisisi_emo_v2_mars_bigtts", "爽快思思（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "zh_male_shenyeboke_emo_v2_mars_bigtts", "深夜播客（多情感）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "en_female_candice_emo_v2_mars_bigtts", "Candice（多情感 EN）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "en_female_skye_emo_v2_mars_bigtts", "Serena（多情感 EN）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "en_male_glen_emo_v2_mars_bigtts", "Glen（多情感 EN）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "en_male_sylus_emo_v2_mars_bigtts", "Sylus（多情感 EN）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "en_male_corey_emo_v2_mars_bigtts", "Corey（多情感 EN）", []string{"voice_gen"}, 0.87, 0},
		{"doubao-speech-v1", "en_female_nadia_tips_emo_v2_mars_bigtts", "Nadia（多情感 EN）", []string{"voice_gen"}, 0.87, 0},
		// ── 1.0 通用场景（volcano_mega 集群，火星/月亮系列）─────────────
		{"doubao-speech-v1", "zh_female_vv_mars_bigtts", "Vivi 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_qinqienvsheng_moon_bigtts", "亲切女声 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_qingyiyuxuan_mars_bigtts", "阳光阿辰 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_xudong_conversation_wvae_bigtts", "快乐小东 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "en_male_jason_conversation_wvae_bigtts", "开朗学长 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_sophie_conversation_wvae_bigtts", "魅力苏菲 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_tianmeitaozi_mars_bigtts", "甜美桃子 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_qingxinnvsheng_mars_bigtts", "清新女声 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_zhixingnvsheng_mars_bigtts", "知性女声 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_qingshuangnanda_mars_bigtts", "清爽男大 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_linjianvhai_moon_bigtts", "邻家女孩 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_yuanboxiaoshu_moon_bigtts", "渊博小叔 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_yangguangqingnian_moon_bigtts", "阳光青年 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_tianmeixiaoyuan_moon_bigtts", "甜美小源 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_qingchezizi_moon_bigtts", "清澈梓梓 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_jieshuoxiaoming_moon_bigtts", "解说小明 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_kailangjiejie_moon_bigtts", "开朗姐姐 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_linjiananhai_moon_bigtts", "邻家男孩 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_tianmeiyueyue_moon_bigtts", "甜美悦悦 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_xinlingjitang_moon_bigtts", "心灵鸡汤 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_wenrouxiaoge_mars_bigtts", "温柔小哥 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_cancan_mars_bigtts", "灿灿/Shiny 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_shuangkuaisisi_moon_bigtts", "爽快思思 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_wennuanahu_moon_bigtts", "温暖阿虎 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_male_shaonianzixin_moon_bigtts", "少年梓辛 1.0", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "zh_female_yingyujiaoyu_mars_bigtts", "Tina老师 1.0", []string{"voice_gen"}, 0.85, 0},
		// ── 1.0 IP仿音（火星系列）─────────────────────────────────────
		{"doubao-speech-v1", "zh_male_lubanqihao_mars_bigtts", "鲁班七号 1.0", []string{"voice_gen"}, 0.84, 0},
		{"doubao-speech-v1", "zh_female_yangmi_mars_bigtts", "林潇 1.0", []string{"voice_gen"}, 0.84, 0},
		{"doubao-speech-v1", "zh_female_linzhiling_mars_bigtts", "玲玲姐姐 1.0", []string{"voice_gen"}, 0.84, 0},
		{"doubao-speech-v1", "zh_female_jiyejizi2_mars_bigtts", "春日部姐姐 1.0", []string{"voice_gen"}, 0.84, 0},
		{"doubao-speech-v1", "zh_male_tangseng_mars_bigtts", "唐僧 1.0", []string{"voice_gen"}, 0.84, 0},
		{"doubao-speech-v1", "zh_male_hupunan_mars_bigtts", "沪普男 1.0", []string{"voice_gen"}, 0.84, 0},
		// ── 1.0 ICL 特色音色（_tob 后缀角色系列）─────────────────────────
		{"doubao-speech-v1", "ICL_zh_female_wenrounvshen_239eff5e8ffa_tob", "温柔女神（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_shenmi_v1_tob", "机灵小伙（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_wuxi_tob", "元气甜妹（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_wenyinvsheng_v1_tob", "知心姐姐（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_lengkugege_v1_tob", "冷酷哥哥（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_feicui_v1_tob", "纯澈女生（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_yuxin_v1_tob", "初恋女友（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_xnx_tob", "贴心闺蜜（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_yry_tob", "温柔白月光（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_BV705_streaming_cs_tob", "炀炀（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_yilin_tob", "贴心妹妹（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_zhixingwenwan_tob", "知性温婉（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_nuanxintitie_tob", "暖心体贴（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_kailangqingkuai_tob", "开朗轻快（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_huoposhuanglang_tob", "活泼爽朗（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_male_shuaizhenxiaohuo_tob", "率真小伙（ICL）", []string{"voice_gen"}, 0.85, 0},
		{"doubao-speech-v1", "ICL_zh_female_wenrouwenya_tob", "温柔文雅（ICL）", []string{"voice_gen"}, 0.85, 0},
		// ── 百度语音合成 ────────────────────────────────────────────────
		{"baidu-tts", "0", "度小美（标准女声）", []string{"voice_gen"}, 0.85, 0},
		{"baidu-tts", "1", "度小宇（标准男声）", []string{"voice_gen"}, 0.85, 0},
		{"baidu-tts", "3", "度逍遥（情感男声/磁性）", []string{"voice_gen"}, 0.87, 0},
		{"baidu-tts", "4", "度丫丫（情感童声/女）", []string{"voice_gen"}, 0.85, 0},
		{"baidu-tts", "5", "度小娇（情感女声）", []string{"voice_gen"}, 0.87, 0},
		{"baidu-tts", "103", "度米朵（精品童声/女）", []string{"voice_gen"}, 0.88, 0},
		{"baidu-tts", "106", "度博文（精品情感男声）", []string{"voice_gen"}, 0.88, 0},
		{"baidu-tts", "110", "度小童（精品童声/男）", []string{"voice_gen"}, 0.88, 0},
		{"baidu-tts", "111", "度小萌（精品童声/女）", []string{"voice_gen"}, 0.88, 0},
		// ── MiniMax 语音合成 ─────────────────────────────────────────────
		{"minimax-tts", "female-shaonv", "少女音（年轻女/活泼）", []string{"voice_gen"}, 0.91, 0},
		{"minimax-tts", "female-yujie", "御姐音（成熟女/知性）", []string{"voice_gen"}, 0.91, 0},
		{"minimax-tts", "female-tianmei", "甜美音（女/甜蜜）", []string{"voice_gen"}, 0.90, 0},
		{"minimax-tts", "female-qingxin", "清新音（女/清新）", []string{"voice_gen"}, 0.90, 0},
		{"minimax-tts", "male-qn-qingse", "青涩青年音（男/年轻）", []string{"voice_gen"}, 0.91, 0},
		{"minimax-tts", "male-qn-jingying", "精英青年音（男/知性）", []string{"voice_gen"}, 0.91, 0},
		{"minimax-tts", "male-qn-badao", "霸道青年音（男/有力）", []string{"voice_gen"}, 0.90, 0},
		{"minimax-tts", "male-qn-daxuesheng", "大学生音（男/活力）", []string{"voice_gen"}, 0.90, 0},
		{"minimax-tts", "presenter_male", "男主持（专业）", []string{"voice_gen"}, 0.91, 0},
		{"minimax-tts", "presenter_female", "女主持（专业）", []string{"voice_gen"}, 0.91, 0},
		{"minimax-tts", "audiobook_male_1", "有声书男声1", []string{"voice_gen"}, 0.92, 0},
		{"minimax-tts", "audiobook_male_2", "有声书男声2", []string{"voice_gen"}, 0.92, 0},
		{"minimax-tts", "audiobook_female_1", "有声书女声1", []string{"voice_gen"}, 0.92, 0},
		{"minimax-tts", "audiobook_female_2", "有声书女声2", []string{"voice_gen"}, 0.92, 0},
		{"minimax-tts", "male-story", "故事男声（儿童）", []string{"voice_gen"}, 0.90, 0},
		{"minimax-tts", "female-story", "故事女声（儿童）", []string{"voice_gen"}, 0.90, 0},
		// ── 阿里云 CosyVoice ─────────────────────────────────────────────
		{"aliyun-tts", "longxiaochun", "龙小淳（女/知性温暖）", []string{"voice_gen"}, 0.92, 0},
		{"aliyun-tts", "longxiaoxia", "龙晓夏（女/活泼朝气）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longxiaobai", "龙小白（男/年轻开朗）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longfei", "龙飞（男/沉稳自信）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longjielidou", "龙姐励豆（女/温暖励志）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longmiaomiao", "龙淼淼（女/儿童活泼）", []string{"voice_gen"}, 0.90, 0},
		{"aliyun-tts", "longshu", "龙叔（男/叙述沉稳）", []string{"voice_gen"}, 0.92, 0},
		{"aliyun-tts", "longwan", "龙婉（女/甜美温柔）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longcheng", "龙橙（男/清晰专业）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longhua", "龙华（男/成熟稳重）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "longxiang", "龙祥（男/磁性低沉）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "loongbella", "贝拉（英文女声）", []string{"voice_gen"}, 0.91, 0},
		{"aliyun-tts", "loongbobby", "鲍比（英文男声）", []string{"voice_gen"}, 0.91, 0},
		// ── 腾讯云语音合成 ────────────────────────────────────────────────
		{"tencent-tts", "101001", "智言（男/标准）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101002", "智雅（女/标准）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101003", "智燕（女/温暖）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101004", "智晶（女/标准）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101005", "智嘉（男/专业）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101006", "智开（男/播音）", []string{"voice_gen"}, 0.92, 0},
		{"tencent-tts", "101008", "智浩（男/播音）", []string{"voice_gen"}, 0.92, 0},
		{"tencent-tts", "101009", "智莉（女/温暖）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101010", "智华（男/年轻）", []string{"voice_gen"}, 0.90, 0},
		{"tencent-tts", "101011", "智燃（男/活力）", []string{"voice_gen"}, 0.90, 0},
		{"tencent-tts", "101012", "智雪（女/温柔）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101013", "智希（女/活泼）", []string{"voice_gen"}, 0.90, 0},
		{"tencent-tts", "101014", "智宁（男/成熟）", []string{"voice_gen"}, 0.91, 0},
		{"tencent-tts", "101015", "智萌（童/活泼）", []string{"voice_gen"}, 0.90, 0},
		{"tencent-tts", "101016", "智甜（女/甜美）", []string{"voice_gen"}, 0.90, 0},
		{"tencent-tts", "101017", "智蓉（女/四川话）", []string{"voice_gen"}, 0.89, 0},
		{"tencent-tts", "101050", "WeJack（英文男声）", []string{"voice_gen"}, 0.90, 0},
		{"tencent-tts", "101051", "WeRose（英文女声）", []string{"voice_gen"}, 0.90, 0},
		// ── 可灵文生音效 ─────────────────────────────────────────────────
		{"kling-sfx", "5s", "5 秒音效（默认）", []string{"sfx_gen"}, 0.92, 0},
		{"kling-sfx", "3s", "3 秒音效（最短）", []string{"sfx_gen"}, 0.90, 0},
		{"kling-sfx", "7s", "7 秒音效", []string{"sfx_gen"}, 0.92, 0},
		{"kling-sfx", "10s", "10 秒音效（最长）", []string{"sfx_gen"}, 0.92, 0},
		// ── ElevenLabs 文生音效 ───────────────────────────────────────────
		{"elevenlabs-sfx", "sound-generation", "ElevenLabs 音效生成（0.5~22 秒）", []string{"sfx_gen"}, 0.94, 0},
		// ── 可灵语音合成 ─────────────────────────────────────────────────
		// voice_id 作为 model 字段；调用时 req.Voice 填写音色 ID
		{"kling-tts", "zh_female_story", "故事女声（中文）", []string{"voice_gen"}, 0.92, 0},
		{"kling-tts", "zh_female_qingxin", "清新女声（中文）", []string{"voice_gen"}, 0.91, 0},
		{"kling-tts", "zh_female_tianmei", "甜美女声（中文）", []string{"voice_gen"}, 0.91, 0},
		{"kling-tts", "zh_female_wenrou", "温柔女声（中文）", []string{"voice_gen"}, 0.91, 0},
		{"kling-tts", "zh_female_zhishixing", "知性女声（中文）", []string{"voice_gen"}, 0.91, 0},
		{"kling-tts", "zh_male_story", "故事男声（中文）", []string{"voice_gen"}, 0.92, 0},
		{"kling-tts", "zh_male_zhengpai", "正派男声（中文）", []string{"voice_gen"}, 0.91, 0},
		{"kling-tts", "zh_male_xinwen", "新闻男声（中文）", []string{"voice_gen"}, 0.91, 0},
		{"kling-tts", "zh_male_shuhu", "书虎男声（中文）", []string{"voice_gen"}, 0.90, 0},
		{"kling-tts", "zh_male_qingnian", "青年男声（中文）", []string{"voice_gen"}, 0.90, 0},
		{"kling-tts", "oversea_male1", "英文男声", []string{"voice_gen"}, 0.90, 0},
		{"kling-tts", "oversea_female1", "英文女声", []string{"voice_gen"}, 0.90, 0},
		// ── 可灵图像生成 ─────────────────────────────────────────────────
		{"kling-image", "kling-v1", "可灵 v1（标准）", []string{"image_gen"}, 0.88, 0},
		{"kling-image", "kling-v1-5", "可灵 v1.5（图生图增强）", []string{"image_gen"}, 0.90, 0},
		{"kling-image", "kling-v2", "可灵 v2", []string{"image_gen"}, 0.92, 0},
		{"kling-image", "kling-v2-1", "可灵 v2.1", []string{"image_gen"}, 0.93, 0},
		{"kling-image", "kling-v3", "可灵 v3（最新）", []string{"image_gen"}, 0.95, 0},
		// ── 即梦AI 图生图 ─────────────────────────────────────────────────
		{"volcengine-i2i", "seededit_v3.0", "SeedEdit 3.0 指令编辑", []string{"img2img_gen"}, 0.93, 0},
		{"volcengine-i2i", "seed3l_single_ip", "DreamO 角色特征保持", []string{"img2img_gen"}, 0.92, 0},
		{"volcengine-i2i", "i2i_portrait_photo", "人像写真 3.0", []string{"img2img_gen"}, 0.91, 0},
		{"volcengine-i2i", "i2i_multi_style_zx2x", "图像风格特效", []string{"img2img_gen"}, 0.90, 0},
		// ── 可灵图生图 ────────────────────────────────────────────────────
		{"kling-i2i", "kling-v1-5", "可灵 v1.5 图生图", []string{"img2img_gen"}, 0.90, 0},
		{"kling-i2i", "kling-v2", "可灵 v2 图生图", []string{"img2img_gen"}, 0.92, 0},
		{"kling-i2i", "kling-v2-1", "可灵 v2.1 图生图", []string{"img2img_gen"}, 0.93, 0},
		{"kling-i2i", "kling-v3", "可灵 v3 图生图（最新）", []string{"img2img_gen"}, 0.95, 0},
	}

	// 1. 确保 provider 记录存在（tenant_id=0 系统级）
	providerIDs := map[string]uint{}
	for _, p := range providers {
		staticModelsJSON := ""
		if len(p.staticModels) > 0 {
			b, _ := json.Marshal(p.staticModels)
			staticModelsJSON = string(b)
		}

		var prov model.ModelProvider
		// 先查询，避免 FirstOrCreate 触发 GORM 错误日志（1062 duplicate key）
		if err := db.Where("name = ? AND tenant_id = 0", p.name).First(&prov).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				logger.Printf("seedAIModels: provider %q: lookup: %v", p.name, err)
				continue
			}
			// 不存在则创建
			prov = model.ModelProvider{
				Name:           p.name,
				DisplayName:    p.displayName,
				Type:           p.provType,
				APIEndpoint:    p.endpoint,
				NeedsSecretKey: p.needsSecretKey,
				StaticModels:   staticModelsJSON,
				TenantID:       0,
				IsActive:       true,
			}
			if err2 := db.Create(&prov).Error; err2 != nil {
				// 并发创建时可能仍触发 1062，此时 fetch 已有记录
				if strings.Contains(err2.Error(), "1062") || strings.Contains(err2.Error(), "Duplicate entry") {
					if err3 := db.Where("name = ? AND tenant_id = 0", p.name).First(&prov).Error; err3 != nil {
						logger.Printf("seedAIModels: provider %q: fetch after race: %v", p.name, err3)
						continue
					}
				} else {
					logger.Printf("seedAIModels: provider %q: create: %v", p.name, err2)
					continue
				}
			}
		}
		// 同步元数据字段（幂等更新，确保已有记录也能获得新字段值）
		updates := map[string]interface{}{}
		if prov.Type != p.provType {
			updates["type"] = p.provType
		}
		if prov.NeedsSecretKey != p.needsSecretKey {
			updates["needs_secret_key"] = p.needsSecretKey
		}
		if prov.StaticModels != staticModelsJSON {
			updates["static_models"] = staticModelsJSON
		}
		if len(updates) > 0 {
			db.Model(&prov).Updates(updates)
		}
		providerIDs[p.name] = prov.ID
	}

	// 2. 确保 model 记录存在
	for _, m := range models {
		provID, ok := providerIDs[m.providerName]
		if !ok {
			continue
		}
		var aiModel model.AIModel
		tasksJSON, _ := json.Marshal(m.tasks)
		db.Where("provider_id = ? AND name = ?", provID, m.name).FirstOrCreate(&aiModel, model.AIModel{
			ProviderID:    provID,
			Name:          m.name,
			DisplayName:   m.displayName,
			SuitableTasks: string(tasksJSON),
			Quality:       m.quality,
			MaxTokens:     m.maxTokens,
			IsActive:      true,
			IsAvailable:   true,
		})
	}
	logger.Printf("seedAIModels: %d providers, %d models ready", len(providerIDs), len(models))
}

// seedConcurrencySettings 将并发度配置种子写入 DB（首次启动），或从 DB 加载并应用到运行时服务。
// 并发度通过"模型管理 → 系统设置"页面配置，默认值为 1。
func seedConcurrencySettings(repo *repository.SystemSettingRepository, aiSvc *service.AIService, videoSvc *service.VideoService) {
	seed := func(key, desc string, apply func(int)) {
		if v, err := repo.Get(key); err == nil {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				apply(n)
			}
		} else {
			_ = repo.Set(key, "1", desc)
		}
	}
	seed("image_concurrency", "图像生成最大并发数", aiSvc.SetImageConcurrency)
	seed("video_concurrency", "视频生成最大并发数", videoSvc.SetVideoConcurrency)
}

// seedWebSearchMcpTool 幂等写入系统内置 web_search MCP 工具
// is_active 默认 false（需用户在 MCP 工具管理页手动启用）
// 仅更新端点，不覆盖用户已修改的 is_active
func seedWebSearchMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	endpoint := fmt.Sprintf("http://localhost:%d/api/v1/tools/web-search", port)

	var existing model.McpTool
	err := db.Where("name = ?", "web_search").First(&existing).Error
	if err != nil {
		// Not found — create it
		tool := model.McpTool{
			Name:          "web_search",
			DisplayName:   "联网搜索",
			Description:   "搜索相关故事片段作为章节生成灵感参考",
			TransportType: "http",
			Endpoint:      endpoint,
			Timeout:       15,
			IsActive:      false,
			IsSystem:      true,
		}
		if createErr := db.Create(&tool).Error; createErr != nil {
			logger.Printf("[Seed] web_search MCP tool create failed: %v", createErr)
			return
		}
		logger.Printf("[Seed] web_search MCP tool registered (is_active=false, endpoint=%s)", endpoint)
	} else {
		// Already exists — only update endpoint (preserve user's is_active setting)
		if updateErr := db.Model(&existing).Update("endpoint", endpoint).Error; updateErr != nil {
			logger.Printf("[Seed] web_search MCP tool update endpoint failed: %v", updateErr)
		}
	}
}

// seedMcpTool 通用幂等写入 MCP 工具（仅更新 endpoint，不覆盖用户 is_active）
func seedMcpTool(db *gorm.DB, name, displayName, description, endpoint string) {
	var existing model.McpTool
	if err := db.Where("name = ?", name).First(&existing).Error; err != nil {
		tool := model.McpTool{
			Name:          name,
			DisplayName:   displayName,
			Description:   description,
			TransportType: "http",
			Endpoint:      endpoint,
			Timeout:       15,
			IsActive:      false,
			IsSystem:      true,
		}
		if createErr := db.Create(&tool).Error; createErr != nil {
			logger.Printf("[Seed] %s MCP tool create failed: %v", name, createErr)
			return
		}
		logger.Printf("[Seed] %s MCP tool registered (is_active=false, endpoint=%s)", name, endpoint)
	} else {
		if updateErr := db.Model(&existing).Update("endpoint", endpoint).Error; updateErr != nil {
			logger.Printf("[Seed] %s MCP tool update endpoint failed: %v", name, updateErr)
		}
	}
}

func seedWikiSearchMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "wiki_search", "百科知识查询",
		"查询 Wikipedia 百科知识，为章节世界观和术语提供准确信息",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/wiki-search", port))
}

func seedStoryPatternMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "story_pattern", "情节结构模板",
		"提供中文网络小说常见情节模板（逆袭/觉醒/复仇等），注入章节大纲生成以提升叙事结构",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/story-pattern", port))
}

func seedImageRefSearchMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "image_ref_search", "图片参考搜索",
		"搜索 Pixabay/Unsplash 视觉参考图，为分镜/角色/场景图像生成提供风格参考",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/image-ref-search", port))
}

func seedColorPaletteMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "color_palette", "场景配色方案",
		"根据情绪/场景类型返回配色方案，为视频分镜图像生成提供一致的视觉色调",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/color-palette", port))
}
