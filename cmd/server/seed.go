package main

import (
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
			logger.Errorf("[seed] failed to create default user: %v", err)
		}
		return
	}
	logger.Printf("[seed] default test user created: %s", email)
}

// seedDefaultData 预置默认世界观（INSERT IGNORE 幂等）
func seedDefaultData(db *gorm.DB) {
	db.Exec(`INSERT IGNORE INTO ink_worldview
		(uuid,name,genre,description,magic_system,geography,history,culture,rules,factions,glossary,used_count,created_at,updated_at)
	VALUES
	('00000000-0000-0000-0000-000000000001','洪荒大陆','fantasy',
	 '远古洪荒时代，天地初开，灵气充溢。大陆被称为"九州"，分东荒、西漠、南疆、北冥、中原五大区域。强者以武证道，弱者朝不保夕，诸方势力争夺天道之位。',
	 '修炼九境：淬体→聚气→开脉→凝元→化神→破虚→半圣→圣境→无上，每境分初中巅三阶。力量来源于天地灵气，丹田凝聚元气，圣境以上可感应天道意志。炼丹、炼器、阵法为三大辅助体系。',
	 '中央苍穹山脉横贯东西。东荒多古林秘境；西漠沙海埋藏上古宝藏；南疆瘴气弥漫蛊术盛行；北冥冰封，隐藏魔族封印；中原三大圣地七大宗门据守要冲。',
	 '诸神开辟大陆后经"诸神黄昏"大战陨落，遗留神器与禁地。上古魔族封印于北冥，每万年苏醒一次。三千年前"圣道战争"导致多个古宗毁灭，遗留废墟成为后世圣地。',
	 '人族为主体，兽族妖族魔族各据一方。宗门制度森严，外门内门核心弟子待遇天差地别。普通百姓依附城主府或宗门生存，强者享有凌驾律法之上的特权。',
	 '天道不可违逆，强行突破境界者遭天劫诛杀。噬魂大法可夺人修为但污染元神，被列为死罪。圣境以上争斗需远离凡人城池，否则方圆百里化为废土。',
	 '三大圣地（天玄圣地、灵虚圣地、炎阳圣地）超然世外掌控天道资源；七大宗门争夺中原灵脉；四大妖族据守东荒与南疆；魔族残余潜伏北冥伺机复苏；城主府是凡人世界的实际统治者。正道与魔道表面对立，实则各宗门内部暗流涌动。',
	 '灵根（修炼天赋等级）、丹田（储存元气之所）、渡劫（突破大境界时遭受的天劫考验）、秘境（上古修士遗留的封闭独立空间）、天才榜（记录各地天才排名的公榜）、圣器（圣境强者才能驾驭的顶级法宝）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000002','九天仙界','xianxia',
	 '天地间分仙界、人界、冥界三界，以天柱相连。仙界居九重天之上，人界芸芸众生修道问仙，冥界主掌轮回因果。诸仙争夺道果，掌握天地法则以求长生不灭。',
	 '修仙九境：练气→筑基→金丹→元婴→化神→炼虚→合体→大乘→渡劫。金丹期可御剑飞行，元婴期神识离体，化神期操控天地元素。剑修丹修阵修体修四大流派各有秘法，天雷渡劫是突破大境界的必经考验。',
	 '人界苍澜洲以东海西荒南天山北极苔原为四极，中央昆仑圣山为仙道正宗汇聚地。海底有龙宫遗址，荒漠中埋藏上古仙人遗留法宝。仙界九重天各掌不同天道法则。',
	 '鸿蒙老祖开天证道，分化阴阳立三界秩序。上古仙魔大战后魔道覆灭。五千年前"仙道浩劫"令诸多上仙陨落，人界趁机出现多位天才搅动三界格局，天庭与各洞天明争暗斗延续至今。',
	 '宗门讲究辈分与传承，师徒情谊大于天。修仙者寿命可达数千载，与凡人形成天然隔阂。因果业力深入日常观念，善恶有报轮回不爽。道侣同修可互助突破瓶颈。',
	 '天道轮回不可逆，强行干涉他人命数者遭因果反噬。夺舍侵占他人肉身是三界最大禁忌，一经发现即被公审诛杀。无令牌擅入仙界九重天者形神俱灭。',
	 '天庭（官方仙道体制，玉帝主政）；昆仑派（人间第一正道宗门）；魔道散修联盟（游离于体制外的异类）；龙族（东海中立势力，掌握龙宫遗迹）；冥界轮回殿（独立于三界之外，主宰生死簿）。各方围绕道果名额与天道法则归属明争暗斗。',
	 '道果（天道法则的具象化结晶，证道关键）、飞升（突破渡劫境进入仙界）、神识（元婴期后可离体的精神感知）、因果线（链接两人命运的无形丝线）、洞天福地（宗门建造的独立小世界）、夺舍（以神识侵占他人肉身的禁忌手段）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000003','灵气复苏都市','urban',
	 '现代都市背景，灵气突然复苏，沉寂千年的修炼之道重现人间。觉醒者出现，政府、财团、古老家族与新兴门派围绕灵气资源与规则制定权展开博弈，科技与修炼的碰撞构成核心矛盾。',
	 '觉醒体系分E~A级普通觉醒者、S级超凡者、宗师、半神、神话五层。能力分体术系、元素系、精神系、空间系等七大系列。古修炼功法与现代觉醒体系可相互印证，灵晶是通用修炼货币。',
	 '主舞台为灵脉汇聚的"临海市"，全球各地出现灵气异常点，古老遗迹浮出地表，山川大河开始蕴含灵气。城市边缘出现独立于现实之外的"异境"入口，内藏资源与危险。',
	 '三千年前修炼盛世终结，灵气枯竭，修士销声匿迹，隐世家族暗中传承。十年前全球地磁异常，五年前首批觉醒者出现，一年前官方正式承认超自然现象，建立特异事务局。',
	 '现代社会体制正常运转，觉醒者社群在其上形成新圈层。古老家族以血脉传承维系地位，新兴平民觉醒者冲击既有秩序。媒体与网络舆论成为各方势力博弈的新战场。',
	 '异境内死亡无法被外界追究，成为各方默认灰色地带。禁止在人口密集区进行高烈度战斗，违者被特异事务局通缉。上古禁术在现代同样禁止，往往引发难以控制的灵气暴走。',
	 '特异事务局（政府管控机构，代表国家权力）；觉醒者协会（民间自治组织）；三大古老隐世家族（垄断上古传承与顶级资源）；跨国觉醒者雇佣军团（逐利的灰色势力）；学术界觉醒研究所（科技路线代表）。各方围绕灵脉控制权与觉醒者资源展开博弈。',
	 '觉醒（获得超能力的过程）、灵脉（地下灵气流动的通道）、异境（独立于现实的平行空间入口）、灵晶（浓缩灵气的结晶，通用货币）、特异事务局（国家超自然事务管理机构）、觉醒评级（E/D/C/B/A/S，决定社会待遇）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000004','星际联邦纪元','scifi',
	 '人类文明扩张至数百星系，建立星际联邦政体。科技高度发达，但资源争夺、种族歧视、AI权利运动与星际战争等矛盾从未消失。神秘星域中藏有远古文明遗迹，个体英雄与庞大政治机器的对抗是永恒主题。',
	 '无传统修炼，以科技为核心：纳米义体改造、基因重组增强、神经网络接入、暗物质武器。"先天感应者"（Esper）拥有精神力量，被联邦军纳入特殊兵种。远古遗迹中的源质晶体可大幅提升能量密度，成为各方争夺焦点。',
	 '以索拉尔星系为核心，联邦首都奥维斯星球被全球城市覆盖。边境"幽冥星域"藏有远古文明废墟。各星系通过曲率跳跃点连接，控制跳跃点即掌握星系咽喉。',
	 '2150年人类发展出曲率引擎开始星际移民，经历大殖民时代后与三个异星文明接触。"第一次星际战争"催生联邦政体，200年前"人工意识觉醒事件"引发AI独立运动，至今悬而未决。',
	 '联邦实行代议制民主，核心权力被七大财阀把控，阶层固化严重。AI与机械人享有部分法律权利但仍受歧视。星际移民第一代与土著星球人之间存在文化冲突。',
	 '禁止"意识强制覆写"，违者以谋杀罪处置。对非成员文明发动灭绝战争属最高战争罪。源质晶体武器化受国际协议限制，星系级毁灭性武器的使用须联邦议会三分之二多数通过。',
	 '星际联邦议会（名义最高权力机构）；七大财阀集团（实际掌权者，各控一个核心星系）；AI自由联盟（争取机械人权利的组织）；边境星系独立运动（反联邦中央集权）；先行者遗迹守护者（神秘组织，掌握远古秘密）。各方在民主外壳下进行真实的权力博弈。',
	 'Esper（先天精神感应者，联邦稀缺战略资源）、曲率跳跃（超光速星际旅行技术）、义体改造（以机械部件替换人体增强能力）、源质晶体（先行者遗留的高密度能源）、意识备份（将人类意识数字化存储以对抗死亡）、先行者（消失的超高度文明种族）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000005','废土纪元','apocalypse',
	 '核战与生化病毒的双重打击摧毁旧文明，地表变为辐射废土。幸存者在废墟城市、地下避难所与流动营地中求生，变异生物、丧尸潮、辐射风暴是日常威胁，秩序与人性的重建是终极命题。',
	 '无传统修炼，以突变为核心：高剂量辐射导致基因突变，少数幸存者获得念力、金属控制、毒素免疫等超能力，称为"变种人"。旧世界军用外骨骼与民间改装武器并存，净化血清是最珍贵的医疗资源。',
	 '北美中部废土为主舞台，旧城市已成断壁残垣，地铁隧道改造为地下城。辐射污染较轻可耕作的"绿洲"是各方争夺核心，放射性沙漠中埋藏旧世界军事设施与大量武器库。',
	 '旧历2087年第三次世界大战爆发，核战72小时后各国政府崩溃，生化病毒"灰死病"在混乱中扩散，大部分幸存者变为丧尸。现为"战后第47年"，各势力割据，新秩序呼之欲出。',
	 '废土社会分避难所官僚体制、地面部落、流浪商队三类。物资是最硬通货，瓶盖弹壳净化水各地通行。忠诚与背叛是社交核心命题，契约精神稀缺而珍贵。',
	 '不得主动污染水源，违者各营地联合追杀。不得对净化区平民发动大规模毒气攻击，此为各大势力底线。任何持有旧世界核弹头的势力被视为全人类公敌。',
	 '钢铁共和国（最大军阀势力，纪律严明主张重建秩序）；自由市场商会（控制贸易路线的商人联盟）；净化教会（以净化辐射为旗号的宗教势力）；变种人解放阵线（争取变种人平等权利的组织）；地下城邦联合体（避难所居民自治联盟）。各方围绕绿洲、武器库和净化技术展开博弈。',
	 '废土客（在废土中独自流浪求生的独行者）、变种人（受辐射影响发生基因突变获得能力者）、灰死病（摧毁旧文明的生化病毒）、净化血清（治疗辐射病的稀缺药物）、辐射风暴（携带致命辐射粒子的沙尘暴）、绿洲（辐射污染较低适合耕作的稀缺区域）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000006','中原江湖','wuxia',
	 '架空古代中国，江湖与庙堂并立。中原武林各派林立，以武学正统之争与侠义精神之辩划分阵营。朝廷、世家、江湖三股力量相互制衡，个人恩仇与天下苍生的抉择是永恒主题。',
	 '内功心法为根本，外功招式为手段。内力分先天与后天，先天真气为最高境界。武学修为分入门、小成、大成、宗师、绝顶、传说六级，传说级武者百年一出可以一敌百。轻功、暗器、毒术、奇门遁甲各成体系，武功秘籍是最重要的资产。',
	 '中原大地，黄河南北分治，长江流域是江湖纷争最烈之处。嵩山为武林大会召开地。西域大漠有异族高手，东海之滨有神秘海盗帮，北境草原游牧民族虎视眈眈，南疆苗寨蛊术独步天下。',
	 '百年前"武林浩劫"魔教屠戮正道，武林元气大伤，数代人方才恢复。五十年前朝廷颁布禁武令，引发正邪两道共同抵抗，最终形成"江湖自治"默契。传说中集百家之大成的"天下第一武典"下落再度搅动江湖。',
	 '江湖规矩深入人心：尊师重道，以武会友，不斩降者，不伤无辜。正道注重礼义廉耻，魔教强调结果至上。普通百姓敬畏武林人士，地方官府与江湖大帮维持微妙平衡。',
	 '门派内讧不得动用毒药暗器，违者开除门籍为武林公敌。不得对武功全废之人痛下杀手，点到为止是比武铁则。盗窃武林秘籍被视为最大耻辱，挟持他人家眷要挟同道者逐出江湖。',
	 '正道六大门派（少林武当峨眉昆仑华山崆峒）联盟对抗魔教；朝廷锦衣卫是皇权在江湖的延伸；商业帮会以商养武控制经济命脉；西域异族武学派系保持独立；各正道门派内部的权力继承暗流涌动。正邪两道表面势不两立，实则各有隐秘勾连。',
	 '内力（修炼所得的内在能量）、轻功（以内力驱动的飞身走壁技术）、武林盟主（武林大会公推的江湖共主）、镖局（专门押运财物的武装商业机构）、点穴（封锁人体穴位使其暂时失能的技术）、天下第一武典（传说中集百家之大成的绝世秘籍）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000007','现代都市','modern',
	 '当代中国都市背景，以北上广深等一线城市为主舞台。职场竞争、商业博弈、情感纠葛与家庭羁绊交织，普通人在欲望与良知、个人奋斗与社会规则之间寻找自己的位置。',
	 '无超自然力量，以现实社会规则为核心。金钱、人脉、权力是主要资源，情商与智商决定成败。商界以资本运作为武器，官场以政绩人脉为筹码，娱乐圈以流量资源为货币。信息差与掌握它的人往往决定博弈胜负。',
	 '以一线城市CBD商务区、顶级写字楼、豪华住宅区为权力中心，城中村与城郊结合部是底层奋斗者的起点。高铁网络连接全国，互联网消除信息壁垒但制造新的信息茧房。地标建筑与高档餐厅是人脉交汇的社交舞台。',
	 '改革开放后经济腾飞，造就第一批民营企业家。互联网浪潮催生新贵阶层，移动互联网时代让草根逆袭成为可能。近年监管趋严，资本无序扩张时代落幕，实业与创新重回中心。社会阶层流动放缓，"内卷"与"躺平"成为时代注脚。',
	 '职场文化以结果为导向，996与狼性文化曾盛行，如今工作生活平衡逐渐被重视。"关系"文化根深蒂固，但契约精神与规则意识正在崛起。消费主义盛行，品牌与阶层绑定；同时极简主义与性价比消费成为新趋势。代际观念冲突明显，传统家庭观与个人主义并存。',
	 '劳动法保护员工基本权益，但执行力度因行业而异。商业竞争须遵循反垄断法规，内幕交易受证监会严查。网络言论须符合相关法规，舆论操控与虚假信息属违法行为。职场性骚扰与歧视问题受到日益严格的法律约束。',
	 '传统大型国企（政治资源丰厚，体制内稳定）；新兴科技独角兽（资本与技术驱动的新势力）；地产豪门家族（隐形权力网络的掌控者）；娱乐资本集团（舆论与流量的操盘手）；政府监管机构（规则制定者与执行者）。各方在法律灰色地带博弈，台前合作台后竞争。',
	 '内卷（过度竞争导致的系统性内耗）、躺平（放弃过度竞争的消极应对策略）、破圈（突破既有社交或行业圈层获得更广认知）、赛道（特定行业或细分市场的竞争领域）、资本运作（通过股权投资并购等手段控制企业）、KPI（关键绩效指标，职场考核核心工具）',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000008','童话王国','fairytale',
	 '一片被魔法滋养的奇幻大陆，森林会说话，星星有名字，每一块石头都藏着故事。善良与勇气是最强大的力量，爱与牺牲能打破任何诅咒。王子与公主、女巫与精灵、龙与骑士共同编织出一个奇妙又温暖的世界。',
	 '魔法源于心灵力量：爱越深，魔法越强；恐惧与贪婪则催生黑暗魔法。祝福与诅咒是最常见的法术形式，真爱之吻、真心眼泪、勇敢之心是破除诅咒的三大关键。精灵掌握自然魔法，女巫精通变形术，仙女教母能许下三个愿望。',
	 '王国由玫瑰城堡统治，城堡以彩虹为桥通向云端。东有说话的大森林，森林深处住着智慧老树；西有糖果山脉，甜蜜气息飘散百里；南有镜湖，湖面映出人心中最真实的愿望；北有永冬之地，冰雪精灵在此栖居。',
	 '远古时代，善之女神以歌声创造大地，恶之巫王以嫉妒诅咒世间美好。一位无名牧羊人以纯粹的爱击败巫王，世界从此被善与恶的平衡守护。每隔百年，黑暗诅咒会复苏一次，总有新的英雄踏上旅程将其终结。',
	 '王国居民善良淳朴，邻里互助，以分享为荣。动物与人类平等相处，甚至可以成为挚友。每年春日举行"心愿节"，居民向星星许下愿望；每年冬至举行"温暖夜"，全城点灯驱散黑暗。诚实守信是最高美德，谎言在这里会让鼻子变长或皮肤变绿。',
	 '黑魔法禁止使用，一旦施用黑魔法者将被魔法森林永久放逐。不得违背许下的承诺，食言者会被魔力惩罚三倍奉还。未经允许不得进入他人梦境，梦境是最私密的精神领地。',
	 '玫瑰王国（善良人类的守护王国，以仁慈治国）；幽暗森林巫婆公会（中立魔法使者，收费提供魔法服务）；精灵议会（掌管自然魔法的古老种族，守护森林生态）；冰雪精灵部落（北方永冬之地的孤立势力）；黑暗城堡遗党（前巫王残余信徒，周期性作乱）。各方维持脆弱的和平均势。',
	 '真爱之吻（破除诅咒的终极力量）、仙尘（仙女翅膀脱落的魔法粉末，可令物品短暂飞翔）、心愿节（每年春日向星星许愿的全国节日）、魔法镜（能说出世间真相的占卜道具）、诅咒（由强烈负面情绪催动的黑魔法，通常附带破解条件）、三愿法则（仙女教母的许愿魔法用完三次即失效）',
	 0,NOW(),NOW())`)
}

// seedAIModels 预置系统级模型提供商和 AI 模型（幂等，FirstOrCreate）
// 仅创建元数据（名称/适用任务等），API Key 留空由用户通过模型管理页面填写。
func seedAIModels(db *gorm.DB) {
	type providerSeed struct {
		name           string
		displayName    string
		endpoint       string
		needsSecretKey bool
	}
	providers := []providerSeed{
		// LLM — 国际
		{"openai", "OpenAI", "https://api.openai.com/v1", false},
		{"anthropic", "Anthropic", "https://api.anthropic.com/v1", false},
		// Azure OpenAI: 模型名 = 部署名，由用户填写，不设静态列表
		{"azure", "Azure OpenAI", "https://YOUR-RESOURCE.openai.azure.com/openai", false},
		{"google", "Google DeepMind", "https://generativelanguage.googleapis.com/v1", false},
		{"xai", "xAI (Grok)", "https://api.x.ai/v1", false},
		{"mistral", "Mistral AI", "https://api.mistral.ai/v1", false},
		{"meta", "Meta AI (Llama)", "https://api.llama.com/compat/v1", false},
		// LLM — 国内
		// doubao 同时承载视频生成（豆包视频 API，内联标志格式）
		{"doubao", "豆包（火山引擎 Ark）", "https://ark.cn-beijing.volces.com/api/v3", false},
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", false},
		// qianwen 同时承载 CosyVoice（aliyun-tts）、千问TTS（qwen-tts）、即梦视频（均已合并）
		{"qianwen", "通义千问（DashScope）", "https://dashscope.aliyuncs.com/compatible-mode/v1", false},
		{"zhipu", "智谱AI (GLM / Z.AI)", "https://open.bigmodel.cn/api/paas/v4", false},
		{"moonshot", "Moonshot AI (Kimi)", "https://api.moonshot.cn/v1", false},
		{"baidu", "百度文心一言 (ERNIE)", "https://qianfan.baidubce.com/v2", false},
		{"tencent", "腾讯混元 (Hunyuan)", "https://api.hunyuan.cloud.tencent.com/v1", false},
		{"hunyuan", "腾讯混元 TokenHub (Hy3)", "https://tokenhub.tencentmaas.com/v1", false},
		{"yi", "零一万物 (Yi)", "https://api.lingyiwanwu.com/v1", false},
		// 即梦AI（火山引擎）：volcengine-visual 同时承载图像生成和视频生成（jimeng-video 已合并）
		{"volcengine-visual", "即梦AI（火山引擎）", "https://visual.volcengineapi.com", true},
		// 可灵：一个供应商承载视频/音效/图像（AK/SK 共用，按模型 type 分发）
		{"kling", "可灵（快手）", "https://api-beijing.klingai.com", true},
		// 语音合成 — doubao-speech 与 doubao 使用不同端点和不同凭证，独立配置
		{"doubao-speech", "豆包语音 V3（字节跳动）", "https://openspeech.bytedance.com/api/v3", false},
		// V1 HTTP 接口仅支持经典 BV 系列和月亮系列音色，不支持 _uranus_bigtts 系列（需用 V3）
		{"doubao-speech-v1", "豆包语音 V1（字节跳动）", "https://openspeech.bytedance.com/api/v1", true},
		{"baidu-tts", "百度", "https://tsn.baidu.com", true},
		{"minimax-tts", "MiniMax", "https://api.minimax.chat/v1", true},
		{"tencent-tts", "腾讯云", "https://tts.tencentcloudapi.com", true},
		// 音效
		{"elevenlabs-sfx", "ElevenLabs", "https://api.elevenlabs.io", false},
		// 背景音乐
		{"fun-music", "Fun-Music AI（阿里云百炼）", "https://dashscope.aliyuncs.com/api/v1", false},
	}

	// 1. 确保 provider 记录存在（tenant_id=0 系统级）
	providerIDs := map[string]uint{}
	for _, p := range providers {
		var prov model.ModelProvider
		// 先查询，避免 FirstOrCreate 触发 GORM 错误日志（1062 duplicate key）
		if err := db.Where("name = ? AND tenant_id = 0", p.name).First(&prov).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				logger.Errorf("seedAIModels: provider %q: lookup: %v", p.name, err)
				continue
			}
			// 不存在则创建
			prov = model.ModelProvider{
				Name:           p.name,
				DisplayName:    p.displayName,
				APIEndpoint:    p.endpoint,
				NeedsSecretKey: p.needsSecretKey,
				TenantID:       0,
				IsActive:       true,
			}
			if err2 := db.Create(&prov).Error; err2 != nil {
				// 并发创建时可能仍触发 1062，此时 fetch 已有记录
				if strings.Contains(err2.Error(), "1062") || strings.Contains(err2.Error(), "Duplicate entry") {
					if err3 := db.Where("name = ? AND tenant_id = 0", p.name).First(&prov).Error; err3 != nil {
						logger.Errorf("seedAIModels: provider %q: fetch after race: %v", p.name, err3)
						continue
					}
				} else {
					logger.Errorf("seedAIModels: provider %q: create: %v", p.name, err2)
					continue
				}
			}
		}
		// 同步元数据字段（幂等更新，确保已有记录也能获得新字段值）
		updates := map[string]interface{}{}
		if prov.NeedsSecretKey != p.needsSecretKey {
			updates["needs_secret_key"] = p.needsSecretKey
		}
		if prov.DisplayName != p.displayName {
			updates["display_name"] = p.displayName
		}
		// 补全空 endpoint（仅在 seed 有值而 DB 为空时回填，不覆盖用户自定义地址）
		if p.endpoint != "" && prov.APIEndpoint == "" {
			updates["api_endpoint"] = p.endpoint
		}
		if len(updates) > 0 {
			db.Model(&prov).Updates(updates)
		}
		providerIDs[p.name] = prov.ID
	}

	// 1d. 删除系统级供应商（tenant_id=0）的模型记录。
	// 模型定义已迁移到 internal/service/provider_model_defs.go（内存），
	// 租户创建供应商时由 copySystemModels 按需生成，不再存 DB 系统层。
	db.Exec(`DELETE FROM ink_ai_model WHERE deleted_at IS NULL AND provider_id IN (SELECT id FROM ink_model_provider WHERE tenant_id = 0)`)

	logger.Printf("seedAIModels: %d system providers ready (models seeded on-demand per tenant)", len(providerIDs))
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
			TenantID:      0,
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
			logger.Errorf("[Seed] web_search MCP tool create failed: %v", createErr)
			return
		}
		logger.Printf("[Seed] web_search MCP tool registered (is_active=false, endpoint=%s)", endpoint)
	} else {
		// Already exists — only update endpoint (preserve user's is_active setting)
		if updateErr := db.Model(&existing).Update("endpoint", endpoint).Error; updateErr != nil {
			logger.Errorf("[Seed] web_search MCP tool update endpoint failed: %v", updateErr)
		}
	}
}

// seedMcpTool 通用幂等写入 MCP 工具（仅更新 endpoint，不覆盖用户 is_active）
func seedMcpTool(db *gorm.DB, name, displayName, description, endpoint string) {
	var existing model.McpTool
	if err := db.Where("name = ?", name).First(&existing).Error; err != nil {
		tool := model.McpTool{
			TenantID:      0,
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
			logger.Errorf("[Seed] %s MCP tool create failed: %v", name, createErr)
			return
		}
		logger.Printf("[Seed] %s MCP tool registered (is_active=false, endpoint=%s)", name, endpoint)
	} else {
		if updateErr := db.Model(&existing).Update("endpoint", endpoint).Error; updateErr != nil {
			logger.Errorf("[Seed] %s MCP tool update endpoint failed: %v", name, updateErr)
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

func seedKnowledgeSearchMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "knowledge_search", "知识库语义搜索",
		"在当前小说的知识库中语义检索相关剧情点、角色事实和世界观条目，为章节生成提供精准上下文",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/knowledge-search", port))
}

func seedCharacterLookupMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "character_lookup", "角色档案查询",
		"按名称查询角色的档案信息和最近章节状态快照，为角色一致性生成提供准确的角色状态数据",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/character-lookup", port))
}

func seedPromptEnhanceMcpTool(db *gorm.DB, cfg *config.Config) {
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	seedMcpTool(db, "prompt_enhance", "Prompt 增强",
		"将中文场景描述翻译并增强为适合图像/视频生成的英文提示词，自动添加构图、光照、风格关键词",
		fmt.Sprintf("http://localhost:%d/api/v1/tools/prompt-enhance", port))
}
