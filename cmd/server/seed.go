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
		name                string
		displayName         string
		endpoint            string
		needsSecretKey      bool
		staticModelsByType  map[string][]string // type -> model list; nil = 运行时拉取（如 OpenAI）
	}
	providers := []providerSeed{
		// LLM — 国际
		// openai 支持标准 /models 端点，同时提供静态列表以便按类型过滤
		{"openai", "OpenAI", "https://api.openai.com/v1", false, map[string][]string{
			"llm":       {"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "o3", "o3-mini", "o1", "o1-mini"},
			"image":     {"dall-e-3", "dall-e-2", "gpt-image-1"},
			"embedding": {"text-embedding-3-large", "text-embedding-3-small", "text-embedding-ada-002"},
			"voice":     {"tts-1", "tts-1-hd"},
		}},
		{"anthropic", "Anthropic", "https://api.anthropic.com/v1", false, map[string][]string{
			"llm": {
				"claude-opus-4-7", "claude-opus-4-5",
				"claude-sonnet-4-6", "claude-sonnet-4-5",
				"claude-haiku-4-5-20251001",
				"claude-3-7-sonnet-20250219",
				"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022",
				"claude-3-opus-20240229",
			},
		}},
		// Azure OpenAI: 模型名 = 部署名，由用户填写，不设静态列表
		{"azure", "Azure OpenAI", "https://YOUR-RESOURCE.openai.azure.com/openai", false, nil},
		{"google", "Google DeepMind", "https://generativelanguage.googleapis.com/v1", false, map[string][]string{
			"llm": {
				"gemini-2.5-pro", "gemini-2.5-flash",
				"gemini-2.0-flash", "gemini-2.0-flash-lite",
				"gemini-1.5-pro", "gemini-1.5-flash",
			},
		}},
		{"xai", "xAI (Grok)", "https://api.x.ai/v1", false, map[string][]string{
			"llm": {"grok-3", "grok-3-mini", "grok-3-fast", "grok-2", "grok-2-vision"},
		}},
		{"mistral", "Mistral AI", "https://api.mistral.ai/v1", false, map[string][]string{
			"llm": {"mistral-large-latest", "mistral-small-latest", "codestral-latest", "open-mistral-nemo"},
		}},
		{"meta", "Meta AI (Llama)", "https://api.llama.com/compat/v1", false, map[string][]string{
			"llm": {"Llama-4-Scout-17B-16E-Instruct", "Llama-4-Maverick-17B-128E-Instruct", "Llama-3.3-70B-Instruct"},
		}},
		// LLM — 国内
		// doubao 同时承载视频生成（豆包视频 API，内联标志格式）
		{"doubao", "豆包（火山引擎 Ark）", "https://ark.cn-beijing.volces.com/api/v3", false, map[string][]string{
			"llm": {
				"doubao-pro-256k", "doubao-pro-128k", "doubao-pro-32k", "doubao-pro-4k",
				"doubao-lite-128k", "doubao-lite-32k",
				"doubao-seed-1-6", "doubao-seed-1-5",
			},
			"video": {"doubao-seaweed-241128", "doubao-seedance-1-0-lite-i2v-250528"},
		}},
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", false, map[string][]string{
			"llm": {"deepseek-chat", "deepseek-reasoner"},
		}},
		// qianwen 同时承载 CosyVoice（aliyun-tts）、千问TTS（qwen-tts）、即梦视频（均已合并）
		{"qianwen", "通义千问（DashScope）", "https://dashscope.aliyuncs.com/compatible-mode/v1", false, map[string][]string{
			"llm": {
				"qwen-max", "qwen-plus", "qwen-turbo", "qwen-long",
				"qwen3-235b-a22b", "qwen3-32b", "qwen3-14b", "qwen3-8b",
				"qwen2.5-72b-instruct", "qwen2.5-32b-instruct",
			},
			"image": {"wanx2.1-t2i-plus", "wanx2.1-t2i-turbo", "wanx-x-v1"},
			"video": {"wanx2.1-i2v-plus", "wanx2.1-i2v-turbo"},
			"voice": {"cosyvoice-v2-0.5b", "cosyvoice-v1-5b"},
		}},
		{"zhipu", "智谱AI (GLM / Z.AI)", "https://open.bigmodel.cn/api/paas/v4", false, map[string][]string{
			"llm": {"glm-4-plus", "glm-4-air", "glm-4-flash", "glm-z1-plus", "glm-z1-air"},
		}},
		{"moonshot", "Moonshot AI (Kimi)", "https://api.moonshot.cn/v1", false, map[string][]string{
			"llm": {"kimi-k2-0711-preview", "moonshot-v1-128k", "moonshot-v1-32k", "moonshot-v1-8k"},
		}},
		{"baidu", "百度文心一言 (ERNIE)", "https://qianfan.baidubce.com/v2", false, map[string][]string{
			"llm": {"ernie-4.5-turbo-128k", "ernie-4.5-8k", "ernie-3.5-128k", "ernie-speed-128k"},
		}},
		{"tencent", "腾讯混元 (Hunyuan)", "https://api.hunyuan.cloud.tencent.com/v1", false, map[string][]string{
			"llm": {"hunyuan-turbos-latest", "hunyuan-large", "hunyuan-standard-256k"},
		}},
		{"yi", "零一万物 (Yi)", "https://api.lingyiwanwu.com/v1", false, map[string][]string{
			"llm": {"yi-lightning", "yi-large", "yi-medium"},
		}},
		// 即梦AI（火山引擎）：volcengine-visual 同时承载图像生成和视频生成（jimeng-video 已合并）
		{"volcengine-visual", "即梦AI（火山引擎）", "https://visual.volcengineapi.com", true, map[string][]string{
			"image": {"general_v3.0", "general_v2.1", "general_v1.4"},
			"video": {"general_v3.0-I2V"},
		}},
		// 可灵：一个供应商承载视频/音效/图像（AK/SK 共用，按模型 type 分发）
		{"kling", "可灵（快手）", "https://api-beijing.klingai.com", true, map[string][]string{
			"video": {"kling-v1-6", "kling-v1-5", "kling-v1"},
			"image": {"kling-v1-6", "kling-v1-5", "kling-v1"},
			"sfx":   {"kling-v1"},
		}},
		// 语音合成 — doubao-speech 与 doubao 使用不同端点和不同凭证，独立配置
		{"doubao-speech", "豆包语音 V3（字节跳动）", "https://openspeech.bytedance.com/api/v3", false, map[string][]string{
			"voice": {"seed-tts-2.0", "seed-tts-1.0"},
		}},
		// V1 HTTP 接口仅支持经典 BV 系列和月亮系列音色，不支持 _uranus_bigtts 系列（需用 V3）
		{"doubao-speech-v1", "豆包语音 V1（字节跳动）", "https://openspeech.bytedance.com/api/v1", true, map[string][]string{
			"voice": {"BV001_streaming", "BV002_streaming"},
		}},
		{"baidu-tts", "百度", "https://tsn.baidu.com", true, map[string][]string{
			"voice": {"0", "1", "3", "4", "5", "103", "106", "110", "111"},
		}},
		{"minimax-tts", "MiniMax", "https://api.minimax.chat/v1", true, map[string][]string{
			"voice": {"female-shaonv", "female-yujie", "male-qn-qingse", "male-qn-jingying"},
		}},
		{"tencent-tts", "腾讯云", "https://tts.tencentcloudapi.com", true, map[string][]string{
			"voice": {"101001", "101002", "101011", "101012"},
		}},
		// 音效
		{"elevenlabs-sfx", "ElevenLabs", "https://api.elevenlabs.io", false, map[string][]string{
			"sfx": {"sound-generation"},
		}},
		// 背景音乐
		{"fun-music", "Fun-Music AI（阿里云百炼）", "https://dashscope.aliyuncs.com/api/v1", false, nil},
	}

	// 1. 确保 provider 记录存在（tenant_id=0 系统级）
	providerIDs := map[string]uint{}
	for _, p := range providers {
		staticModelsJSON := ""
		if len(p.staticModelsByType) > 0 {
			b, _ := json.Marshal(p.staticModelsByType)
			staticModelsJSON = string(b)
		}

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
				StaticModels:   staticModelsJSON,
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
		if prov.StaticModels != staticModelsJSON {
			updates["static_models"] = staticModelsJSON
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

// seedProviderVoices 将音色数据写入 ink_model_provider.voices_json（幂等）。
// 音色列表已从 ink_ai_model 迁移到 TTS 提供商的 voices_json 字段，以减少行数。
func seedProviderVoices(db *gorm.DB) {
	type voiceData struct {
		id       string
		name     string
		gender   string  // male / female / neutral（空=从名称推断）
		ageGroup string  // child / teen / adult / elder
		quality  float64
	}
	type providerVoices struct {
		providerName string
		voices       []voiceData
	}

	data := []providerVoices{
		// ── 豆包语音合成 V3 ──────────────────────────────────────────────
		{"doubao-speech", []voiceData{
			{"zh_female_vv_uranus_bigtts", "Vivi 2.0", "female", "adult", 0.92},
			{"zh_female_xiaohe_uranus_bigtts", "小何 2.0", "female", "adult", 0.92},
			{"zh_male_m191_uranus_bigtts", "云舟 2.0", "male", "adult", 0.91},
			{"zh_male_taocheng_uranus_bigtts", "小天 2.0", "male", "adult", 0.91},
			{"zh_male_liufei_uranus_bigtts", "刘飞 2.0", "male", "adult", 0.91},
			{"zh_female_sophie_uranus_bigtts", "魅力苏菲 2.0", "female", "adult", 0.91},
			{"zh_female_qingxinnvsheng_uranus_bigtts", "清新女声 2.0", "female", "adult", 0.91},
			{"zh_female_tianmeixiaoyuan_uranus_bigtts", "甜美小源 2.0", "female", "adult", 0.91},
			{"zh_female_tianmeitaozi_uranus_bigtts", "甜美桃子 2.0", "female", "adult", 0.91},
			{"zh_female_shuangkuaisisi_uranus_bigtts", "爽快思思 2.0", "female", "adult", 0.91},
			{"zh_female_linjianvhai_uranus_bigtts", "邻家女孩 2.0", "female", "adult", 0.91},
			{"zh_male_shaonianzixin_uranus_bigtts", "少年梓辛 2.0", "male", "teen", 0.91},
			{"zh_female_meilinvyou_uranus_bigtts", "魅力女友 2.0", "female", "adult", 0.90},
			{"zh_female_wenroumama_uranus_bigtts", "温柔妈妈 2.0", "female", "adult", 0.90},
			{"zh_female_tvbnv_uranus_bigtts", "TVB女声 2.0", "female", "adult", 0.90},
			{"zh_female_qiaopinv_uranus_bigtts", "俏皮女声 2.0", "female", "adult", 0.90},
			{"zh_male_linjiananhai_uranus_bigtts", "邻家男孩 2.0", "male", "adult", 0.90},
			{"zh_male_jieshuoxiaoming_uranus_bigtts", "解说小明 2.0", "male", "adult", 0.90},
			{"zh_male_yizhipiannan_uranus_bigtts", "译制片男 2.0", "male", "adult", 0.90},
			{"zh_male_ruyaqingnian_uranus_bigtts", "儒雅青年 2.0", "male", "adult", 0.90},
			{"zh_male_wennuanahu_uranus_bigtts", "温暖阿虎 2.0", "male", "adult", 0.90},
			{"zh_male_naiqimengwa_uranus_bigtts", "奶气萌娃 2.0", "male", "child", 0.90},
			{"zh_female_popo_uranus_bigtts", "婆婆 2.0", "female", "elder", 0.90},
			{"zh_female_gaolengyujie_uranus_bigtts", "高冷御姐 2.0", "female", "adult", 0.90},
			{"zh_male_aojiaobazong_uranus_bigtts", "傲娇霸总 2.0", "male", "adult", 0.90},
			{"zh_male_lanyinmianbao_uranus_bigtts", "懒音绵宝 2.0", "male", "adult", 0.90},
			{"zh_male_fanjuanqingnian_uranus_bigtts", "反卷青年 2.0", "male", "adult", 0.90},
			{"zh_female_wenroushunv_uranus_bigtts", "温柔淑女 2.0", "female", "adult", 0.90},
			{"zh_male_huolixiaoge_uranus_bigtts", "活力小哥 2.0", "male", "adult", 0.90},
			{"zh_female_mengyatou_uranus_bigtts", "萌丫头 2.0", "female", "teen", 0.90},
			{"zh_female_tiexinnvsheng_uranus_bigtts", "贴心女声 2.0", "female", "adult", 0.90},
			{"zh_female_jitangmei_uranus_bigtts", "鸡汤妹妹 2.0", "female", "adult", 0.90},
			{"zh_male_cixingjieshuonan_uranus_bigtts", "磁性解说男声 2.0", "male", "adult", 0.90},
			{"zh_male_liangsangmengzai_uranus_bigtts", "亮嗓萌仔 2.0", "male", "child", 0.90},
			{"zh_female_kailangjiejie_uranus_bigtts", "开朗姐姐 2.0", "female", "adult", 0.90},
			{"zh_male_gaolengchenwen_uranus_bigtts", "高冷沉稳 2.0", "male", "adult", 0.90},
			{"zh_male_shenyeboke_uranus_bigtts", "深夜播客 2.0", "male", "adult", 0.90},
			{"zh_female_nvleishen_uranus_bigtts", "女雷神 2.0", "female", "adult", 0.90},
			{"zh_female_qinqienv_uranus_bigtts", "亲切女声 2.0", "female", "adult", 0.90},
			{"zh_male_kuailexiaodong_uranus_bigtts", "快乐小东 2.0", "male", "adult", 0.90},
			{"zh_male_kailangxuezhang_uranus_bigtts", "开朗学长 2.0", "male", "adult", 0.90},
			{"zh_male_youyoujunzi_uranus_bigtts", "悠悠君子 2.0", "male", "adult", 0.90},
			{"zh_female_wenjingmaomao_uranus_bigtts", "文静毛毛 2.0", "female", "adult", 0.90},
			{"zh_female_zhixingnv_uranus_bigtts", "知性女声 2.0", "female", "adult", 0.90},
			{"zh_male_qingshuangnanda_uranus_bigtts", "清爽男大 2.0", "male", "adult", 0.90},
			{"zh_male_yuanboxiaoshu_uranus_bigtts", "渊博小叔 2.0", "male", "adult", 0.90},
			{"zh_male_yangguangqingnian_uranus_bigtts", "阳光青年 2.0", "male", "adult", 0.90},
			{"zh_female_qingchezizi_uranus_bigtts", "清澈梓梓 2.0", "female", "adult", 0.90},
			{"zh_female_tianmeiyueyue_uranus_bigtts", "甜美悦悦 2.0", "female", "adult", 0.90},
			{"zh_female_xinlingjitang_uranus_bigtts", "心灵鸡汤 2.0", "female", "adult", 0.90},
			{"zh_male_wenrouxiaoge_uranus_bigtts", "温柔小哥 2.0", "male", "adult", 0.90},
			{"zh_male_tiancaitongsheng_uranus_bigtts", "天才童声 2.0", "male", "child", 0.90},
			{"zh_male_kailangdidi_uranus_bigtts", "开朗弟弟 2.0", "male", "teen", 0.90},
			{"zh_female_chanmeinv_uranus_bigtts", "谄媚女声 2.0", "female", "adult", 0.90},
			{"zh_female_roumeinvyou_uranus_bigtts", "柔美女友 2.0", "female", "adult", 0.90},
			{"zh_female_wenrouxiaoya_uranus_bigtts", "温柔小雅 2.0", "female", "adult", 0.90},
			{"zh_male_dongfanghaoran_uranus_bigtts", "东方浩然 2.0", "male", "adult", 0.90},
			{"zh_female_shaoergushi_uranus_bigtts", "少儿故事 2.0", "female", "child", 0.90},
			{"zh_male_guanggaojieshuo_uranus_bigtts", "广告解说 2.0", "male", "adult", 0.90},
			{"zh_female_cancan_uranus_bigtts", "知性灿灿 2.0", "female", "adult", 0.90},
			{"zh_female_sajiaoxuemei_uranus_bigtts", "撒娇学妹 2.0", "female", "teen", 0.90},
			{"zh_female_zhishuaiyingzi_uranus_bigtts", "直率英子 2.0", "female", "adult", 0.90},
			{"zh_female_gufengshaoyu_uranus_bigtts", "古风少御 2.0", "female", "adult", 0.90},
			{"zh_male_silang_uranus_bigtts", "四郎 2.0", "male", "adult", 0.90},
			{"zh_male_qingcang_uranus_bigtts", "擎苍 2.0", "male", "adult", 0.90},
			{"zh_male_xionger_uranus_bigtts", "熊二 2.0", "male", "adult", 0.90},
			{"zh_female_yingtaowanzi_uranus_bigtts", "樱桃丸子 2.0", "female", "child", 0.90},
			{"zh_female_wuzetian_uranus_bigtts", "武则天 2.0", "female", "adult", 0.90},
			{"zh_female_gujie_uranus_bigtts", "顾姐 2.0", "female", "adult", 0.90},
			{"zh_male_lubanqihao_uranus_bigtts", "鲁班七号 2.0", "male", "adult", 0.90},
			{"zh_female_linxiao_uranus_bigtts", "林潇 2.0", "female", "adult", 0.90},
			{"zh_female_lingling_uranus_bigtts", "玲玲姐姐 2.0", "female", "adult", 0.90},
			{"zh_female_chunribu_uranus_bigtts", "春日部姐姐 2.0", "female", "adult", 0.90},
			{"zh_male_tangseng_uranus_bigtts", "唐僧 2.0", "male", "adult", 0.90},
			{"zh_male_zhuangzhou_uranus_bigtts", "庄周 2.0", "male", "adult", 0.90},
			{"zh_male_zhubajie_uranus_bigtts", "猪八戒 2.0", "male", "adult", 0.90},
			{"zh_female_ganmaodianyin_uranus_bigtts", "感冒电音姐姐 2.0", "female", "adult", 0.90},
			{"zh_male_baqiqingshu_uranus_bigtts", "霸气青叔 2.0", "male", "adult", 0.90},
			{"zh_female_liuchangnv_uranus_bigtts", "流畅女声 2.0", "female", "adult", 0.90},
			{"zh_male_ruyayichen_uranus_bigtts", "儒雅逸辰 2.0", "male", "adult", 0.90},
			{"zh_female_peiqi_uranus_bigtts", "佩奇猪 2.0", "female", "adult", 0.90},
			{"zh_male_sunwukong_uranus_bigtts", "猴哥 2.0", "male", "adult", 0.90},
			{"zh_female_yingyujiaoxue_uranus_bigtts", "Tina老师 2.0", "female", "adult", 0.90},
			{"zh_female_kefunvsheng_uranus_bigtts", "暖阳女声 2.0", "female", "adult", 0.90},
			{"zh_female_xiaoxue_uranus_bigtts", "儿童绘本 2.0", "female", "child", 0.90},
			{"zh_male_dayi_uranus_bigtts", "大壹 2.0", "male", "adult", 0.90},
			{"zh_female_mizai_uranus_bigtts", "黑猫侦探社咪仔 2.0", "female", "adult", 0.90},
			{"zh_female_jitangnv_uranus_bigtts", "鸡汤女 2.0", "female", "adult", 0.90},
			{"zh_male_xuanyijieshuo_uranus_bigtts", "悬疑解说 2.0", "male", "adult", 0.90},
			{"zh_female_jiaochuannv_uranus_bigtts", "娇喘女声 2.0", "female", "adult", 0.90},
			{"en_male_tim_uranus_bigtts", "Tim", "male", "adult", 0.90},
			{"en_female_dacey_uranus_bigtts", "Dacey", "female", "adult", 0.90},
			{"en_female_stokie_uranus_bigtts", "Stokie", "female", "adult", 0.90},
			// ICL 角色扮演系列（女）
			{"ICL_uranus_zh_female_aojiaonvyou_tob", "傲娇女友 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_aomanjiaosheng_tob", "傲慢娇声 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_xiemeinvwang_tob", "邪魅女王 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_bingjiaojiejie_tob", "病娇姐姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_bingjiaomengmei_tob", "病娇萌妹 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_bingruoshaonv_tob", "病弱少女 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_chengshuwenrou_tob", "成熟温柔 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_chengshujiejie_tob", "成熟姐姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_chunzhenshaonv_tob", "纯真少女 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_chunchenvsheng_tob", "纯澈女生 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_wumeikeren_tob", "妩媚可人 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_guaiqiaokeer_tob", "乖巧可儿 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_heainainai_tob", "和蔼奶奶 2.0", "female", "elder", 0.90},
			{"ICL_uranus_zh_female_huopodiaoman_tob", "活泼刁蛮 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_huoponvhai_tob", "活泼女孩 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_jiaohannvwang_tob", "娇憨女王 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_jiaoruoluoli_tob", "娇弱萝莉 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_jiaxiaozi_tob", "假小子 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_jinglingxiangdao_tob", "精灵向导 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_kailangtingting_tob", "开朗婷婷 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_kaixinxiaohong_tob", "开心小鸿 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_keainvsheng_tob", "可爱女生 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_lingdongxinxin_tob", "灵动欣欣 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_linjuayi_tob", "邻居阿姨 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tianmeijiaoqiao_tob", "甜美娇俏 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_qinglenggaoya_tob", "清冷高雅 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_lixingyuanzi_tob", "理性圆子 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_xingganmeihuo_tob", "性感魅惑 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_nuanxinqianqian_tob", "暖心茜茜 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_nuanxinxuejie_tob", "暖心学姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_qingtianmeimei_tob", "清甜莓莓 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_qingtiantaotao_tob", "清甜桃桃 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_qingxixiaoxue_tob", "清晰小雪 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_qingxinshaonv_tob", "倾心少女 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_rouguhunshi_tob", "柔骨魂师 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_ruanmengtangtang_tob", "软萌糖糖 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_ruanmengtuanzi_tob", "软萌团子 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tianmeihuopo_tob", "甜美活泼 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tianmeixiaoju_tob", "甜美小橘 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tianmeixiaoyu_tob", "甜美小雨 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tiaopigongzhu_tob", "调皮公主 2.0", "female", "teen", 0.90},
			{"ICL_uranus_zh_female_tiexinnvyou_tob", "贴心女友 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_wenrounvshen_tob", "温柔女神 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_wenrouwenya_tob", "温柔文雅 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_zhixinjiejie_tob", "知心姐姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_wumeiyujie_tob", "妩媚御姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_yuanqitianmei_tob", "元气甜妹 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_xiemeiyujie_tob", "邪魅御姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_xingganyujie_tob", "性感御姐 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_xiuliqianqian_tob", "秀丽倩倩 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tiexinguimi_tob", "贴心闺蜜 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_tiexinmeimei_tob", "贴心妹妹 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_wenroubaiyueguang_tob", "温柔白月光 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_chuliannvyou_tob", "初恋女友 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_zhixingwenwan_tob", "知性温婉 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_wenwanshanshan_tob", "温婉珊珊 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_reqingaina_tob", "热情艾娜 2.0", "female", "adult", 0.90},
			{"ICL_uranus_zh_female_qingyingduoduo_tob", "轻盈朵朵 2.0", "female", "adult", 0.90},
			// ICL 角色扮演系列（男）
			{"ICL_uranus_zh_male_aoqilingren_tob", "傲气凌人 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_anrenqinzhu_tob", "黯刃秦主 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_aojiaogongzi_tob", "傲娇公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_aojiaojingying_tob", "傲娇精英 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_aomanqingnian_tob", "傲慢青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_aomanshaoye_tob", "傲慢少爷 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zhenbiandiyu_tob", "枕边低语 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_badaoshaoye_tob", "霸道少爷 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_badaozongcai_tob", "霸道总裁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_bingjiaobailian_tob", "病娇白莲 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_bingjiaodidi_tob", "病娇弟弟 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_bingjiaogege_tob", "病娇哥哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_bingjiaonanyou_tob", "病娇男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_bingjiaoshaonian_tob", "病娇少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_bingruogongzi_tob", "病弱公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_bingruoshaonian_tob", "病弱少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_bujiqingnian_tob", "不羁青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_chunhoudiyin_tob", "醇厚低音 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_paoxiaoxiaoge_tob", "咆哮小哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_yangyang_tob", "炀炀 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_chanruoshaoye_tob", "孱弱少爷 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_chengshuzongcai_tob", "成熟总裁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_chenwenmingzai_tob", "沉稳明仔 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qingyisugan_tob", "清逸苏感 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_chunzhenxuedi_tob", "纯真学弟 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_cixingnansang_tob", "磁性男嗓 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_cujingnansheng_tob", "醋精男生 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_cujingnanyou_tob", "醋精男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_diyinchenyu_tob", "低音沉郁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_fengfashaonian_tob", "风发少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_ruyagongzi_tob", "儒雅公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_fuheigongzi_tob", "腹黑公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_ganjingshaonian_tob", "干净少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_gaolengzongcai_tob", "高冷总裁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_guaogongzi_tob", "孤傲公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_gugaogongzi_tob", "孤高公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_guiyishenmi_tob", "诡异神秘 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_guzhibingjiao_tob", "固执病娇 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_hanhoudunshi_tob", "憨厚敦实 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_huoliqingnian_tob", "活力青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_huoponanyou_tob", "活泼男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_huoposhuanglang_tob", "活泼爽朗 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_huzishushu_tob", "胡子叔叔 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_jijiazhineng_tob", "机甲智能 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_jingyingqingnian_tob", "精英青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_junyigongzi_tob", "俊逸公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_kailangqingkuai_tob", "开朗轻快 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_kailangqingnian_tob", "开朗青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lanyincaohunshi_tob", "蓝银草魂师 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengaozongcai_tob", "冷傲总裁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengdanshuli_tob", "冷淡疏离 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengjungaozhi_tob", "冷峻高智 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengjunshangsi_tob", "冷峻上司 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengkugege_tob", "冷酷哥哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lenglianxiongzhang_tob", "冷脸兄长 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lenglianxueba_tob", "冷脸学霸 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengmonanyou_tob", "冷漠男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lengmoxiongzhang_tob", "冷漠兄长 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lingyunqingnian_tob", "凌云青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qinglengjingui_tob", "清冷矜贵 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_lvchaxiaoge_tob", "绿茶小哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_mengdongqingnian_tob", "懵懂青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_menyoupingxiaoge_tob", "闷油瓶小哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_xiaozhangxiaoge_tob", "嚣张小哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_nianrennanyou_tob", "粘人男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_neiliancaijun_tob", "内敛才俊 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_nuanxintitie_tob", "暖心体贴 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_pianpiangongzi_tob", "翩翩公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_chenwenyouya_tob", "沉稳优雅 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qingsexiaosheng_tob", "青涩小生 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_qingseqingnian_tob", "青涩青年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_qingshuangshaonian_tob", "清爽少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_qingxinbobo_tob", "清新波波 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qinqieqingnian_tob", "亲切青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qinqiexiaozhuo_tob", "亲切小卓 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qinglangwenrun_tob", "清朗温润 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_rexueshaonian_tob", "热血少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_ruyacaijun_tob", "儒雅才俊 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_ruyajunzi_tob", "儒雅君子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_ruyazongcai_tob", "儒雅总裁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_sajiaonansheng_tob", "撒娇男生 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_sajiaonanyou_tob", "撒娇男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_sajiaonianren_tob", "撒娇粘人 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_satuoqingnian_tob", "洒脱青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_shaonianjiangjun_tob", "少年将军 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_shenchenzongcai_tob", "深沉总裁 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_jilingxiaohuo_tob", "机灵小伙 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_shenmifashi_tob", "神秘法师 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_shuaizhenxiaohuo_tob", "率真小伙 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_shuanglangxiaoyang_tob", "爽朗小阳 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_dichenqianquan_tob", "低沉缱绻 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_siwenqingnian_tob", "斯文青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_tianxinanyou_tob", "甜系男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_tiexinnanyou_tob", "贴心男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wenrounantongzhuo_tob", "温柔男同桌 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wenrounanyou_tob", "温柔男友 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wenrouxuezhang_tob", "温柔学长 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wenrunxuezhe_tob", "温润学者 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wenshunshaonian_tob", "温顺少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_guayanxiaoge_tob", "寡言小哥 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_xiaohouye_tob", "小侯爷 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_naiqixiaosheng_tob", "奶气小生 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_xiaosasuixing_tob", "潇洒随性 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wenrouneilian_tob", "温柔内敛 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_xuebanantongzhuo_tob", "学霸男同桌 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_xuebatongzhuo_tob", "学霸同桌 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_yangguangyangyang_tob", "阳光洋洋 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_wennuanshaonian_tob", "温暖少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_yiqishaonian_tob", "意气少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_younidashu_tob", "油腻大叔 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_youmodaye_tob", "幽默大爷 2.0", "male", "elder", 0.90},
			{"ICL_uranus_zh_male_youmoshushu_tob", "幽默叔叔 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_youroubangzhu_tob", "优柔帮主 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_yourougongzi_tob", "优柔公子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_yuanqishaonian_tob", "元气少年 2.0", "male", "teen", 0.90},
			{"ICL_uranus_zh_male_zhangjianjunzi_tob", "仗剑君子 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zhangjianxiake_tob", "仗剑侠客 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zhengzhiqingnian_tob", "正直青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zhishuaiqingnian_tob", "直率青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zhongerqingnian_tob", "中二青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zifuqingnian_tob", "自负青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_zixinqingnian_tob", "自信青年 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_tiancaitongzhuo_tob", "天才同桌 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_qingxinmumu_tob", "清新沐沐 2.0", "male", "adult", 0.90},
			{"ICL_uranus_zh_male_shuanglangshaonian_tob", "爽朗少年 2.0", "male", "teen", 0.90},
			// ICL 角色扮演系列（英文）
			{"ICL_uranus_en_female_charlie_tob", "Charlie 2.0", "female", "adult", 0.90},
			{"ICL_uranus_en_male_ethan_tob", "Ethan 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_alastor_tob", "Alastor 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_chucky_tob", "Chucky 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_noah_tob", "Noah 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_jigsaw_tob", "Jigsaw 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_clown_man_tob", "Clown Man 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_cartoon_chef_tob", "Cartoon Chef 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_frosty_man_tob", "Frosty Man 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_the_grinch_tob", "The Grinch 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_kevin_mccallister_tob", "Kevin McCallister 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_michael_tob", "Michael 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_big_boogie_tob", "Big Boogie 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_xavier_tob", "Xavier 2.0", "male", "adult", 0.90},
			{"ICL_uranus_en_male_zayne_tob", "Zayne 2.0", "male", "adult", 0.90},
		}},
		// ── 豆包语音合成 V1（仅支持 BV 经典系列和月亮系列，_uranus_bigtts 请用 V3）──────
		{"doubao-speech-v1", []voiceData{
			// 通用经典系列（volcano_tts 集群）
			{"BV001_streaming", "通用女声", "female", "adult", 0.82},
			{"BV002_streaming", "通用男声", "male", "adult", 0.82},
			{"BV005_streaming", "活泼女声", "female", "teen", 0.83},
			{"BV006_streaming", "沉稳男声", "male", "adult", 0.83},
			{"BV007_streaming", "新闻女声", "female", "adult", 0.84},
			{"BV033_streaming", "温柔小哥", "male", "adult", 0.84},
			{"BV034_streaming", "知性女声", "female", "adult", 0.84},
			// 月亮系列（volcano_mega 集群，大模型1.0音色）
			{"zh_female_shuangkuaisisi_moon_bigtts", "爽快思思", "female", "adult", 0.90},
			{"zh_male_jingqiangkanye_moon_bigtts", "精英男声", "male", "adult", 0.90},
			{"zh_female_linjingzhu_moon_bigtts", "甜美女声", "female", "adult", 0.90},
			{"zh_male_chunhou_moon_bigtts", "醇厚男声", "male", "adult", 0.90},
			{"zh_female_wanqingxiaochun_moon_bigtts", "温情晓春", "female", "adult", 0.90},
			{"zh_male_zhubo_moon_bigtts", "主播男声", "male", "adult", 0.90},
			// 英文系列
			{"en_female_sarah_stream", "Sarah（英文女声）", "female", "adult", 0.85},
			{"en_male_adam_stream", "Adam（英文男声）", "male", "adult", 0.85},
		}},
		// ── 百度语音合成 ──────────────────────────────────────────────────
		{"baidu-tts", []voiceData{
			{"0", "度小美（标准女声）", "female", "adult", 0.85},
			{"1", "度小宇（标准男声）", "male", "adult", 0.85},
			{"3", "度逍遥（情感男声/磁性）", "male", "adult", 0.87},
			{"4", "度丫丫（情感童声/女）", "female", "child", 0.85},
			{"5", "度小娇（情感女声）", "female", "adult", 0.87},
			{"103", "度米朵（精品童声/女）", "female", "child", 0.88},
			{"106", "度博文（精品情感男声）", "male", "adult", 0.88},
			{"110", "度小童（精品童声/男）", "male", "child", 0.88},
			{"111", "度小萌（精品童声/女）", "female", "child", 0.88},
		}},
		// ── MiniMax 语音合成 ──────────────────────────────────────────────
		{"minimax-tts", []voiceData{
			{"female-shaonv", "少女音（年轻女/活泼）", "female", "teen", 0.91},
			{"female-yujie", "御姐音（成熟女/知性）", "female", "adult", 0.91},
			{"female-tianmei", "甜美音（女/甜蜜）", "female", "adult", 0.90},
			{"female-qingxin", "清新音（女/清新）", "female", "adult", 0.90},
			{"male-qn-qingse", "青涩青年音（男/年轻）", "male", "teen", 0.91},
			{"male-qn-jingying", "精英青年音（男/知性）", "male", "adult", 0.91},
			{"male-qn-badao", "霸道青年音（男/有力）", "male", "adult", 0.90},
			{"male-qn-daxuesheng", "大学生音（男/活力）", "male", "teen", 0.90},
			{"presenter_male", "男主持（专业）", "male", "adult", 0.91},
			{"presenter_female", "女主持（专业）", "female", "adult", 0.91},
			{"audiobook_male_1", "有声书男声1", "male", "adult", 0.92},
			{"audiobook_male_2", "有声书男声2", "male", "adult", 0.92},
			{"audiobook_female_1", "有声书女声1", "female", "adult", 0.92},
			{"audiobook_female_2", "有声书女声2", "female", "adult", 0.92},
			{"male-story", "故事男声（儿童）", "male", "child", 0.90},
			{"female-story", "故事女声（儿童）", "female", "child", 0.90},
		}},
		// ── 阿里云 CosyVoice + 千问 TTS（均已合并至 qianwen） ─────────────
		// QianwenTTSRouter 按 voice ID 前缀分发：long*/loong* → CosyVoice，其余 → QwenTTS
		{"qianwen", []voiceData{
			// ── CosyVoice v3-flash ────────────────────────────────────────
			// 社交陪伴
			{"longanyang",      "龙安洋（男/阳光社交）", "male",   "adult", 0.94},
			{"longanhuan_v3",   "龙安欢V3（女/欢脱元气）", "female", "adult", 0.94},
			{"longanhuan",      "龙安欢（女/欢脱元气）",  "female", "adult", 0.93},
			// 童声
			{"longhuhu_v3",     "龙呼呼（女童/天真烂漫）", "female", "child", 0.93},
			{"longpaopao_v3",   "龙泡泡V3（飞天泡泡音）",  "neutral","child", 0.91},
			{"longjielidou_v3", "龙杰力豆V3（男童/阳光顽皮）","male","child", 0.91},
			{"longxian_v3",     "龙仙V3（女/豪放可爱）",   "female", "child", 0.91},
			{"longling_v3",     "龙铃V3（女/稚气呆板）",   "female", "child", 0.91},
			// 儿童有声书
			{"longshanshan_v3", "龙闪闪V3（戏剧化童声）",  "neutral","child", 0.91},
			{"longniuniu_v3",   "龙牛牛V3（阳光男童声）",  "male",   "child", 0.91},
			// 方言
			{"longjiaxin_v3",   "龙嘉欣V3（女/粤语优雅）", "female", "adult", 0.92},
			{"longjiayi_v3",    "龙嘉怡V3（女/知性粤语）", "female", "adult", 0.92},
			{"longanyue_v3",    "龙安粤V3（男/欢脱粤语）", "male",   "adult", 0.92},
			{"longlaotie_v3",   "龙老铁V3（男/东北直率）", "male",   "adult", 0.92},
			{"longshange_v3",   "龙陕哥V3（男/原味陕北）", "male",   "adult", 0.92},
			{"longanmin_v3",    "龙安闽V3（女/清纯闽南）", "female", "adult", 0.92},
			// 出海营销（日韩英等外语）
			{"loongkyong_v3",   "loongkyong V3（韩语女）",  "female", "adult", 0.90},
			{"loongriko_v3",    "Riko V3（二次元日语女）",  "female", "teen",  0.90},
			{"loongtomoka_v3",  "loongtomoka V3（日语女）", "female", "adult", 0.90},
			{"loongabby_v3",    "loongabby V3（美式英语女）","female","adult", 0.90},
			{"loongandy_v3",    "loongandy V3（美式英语男）","male",  "adult", 0.90},
			{"loongannie_v3",   "loongannie V3（美式英语女）","female","adult",0.90},
			{"loongava_v3",     "loongava V3（美式英语女）", "female","adult", 0.90},
			{"loongbeth_v3",    "loongbeth V3（美式英语女）","female","adult", 0.90},
			{"loongbetty_v3",   "loongbetty V3（美式英语女）","female","adult",0.90},
			{"loongcally_v3",   "loongcally V3（美式英语女）","female","adult",0.90},
			{"loongcindy_v3",   "loongcindy V3（美式英语女）","female","adult",0.90},
			{"loongdavid_v3",   "loongdavid V3（美式英语男）","male", "adult", 0.90},
			{"loongdonna_v3",   "loongdonna V3（美式英语女）","female","adult",0.90},
			{"loongemily_v3",   "loongemily V3（英式英语女）","female","adult",0.90},
			{"loongeric_v3",    "loongeric V3（英式英语男）", "male", "adult", 0.90},
			{"loongluna_v3",    "loongluna V3（英式英语女）", "female","adult",0.90},
			{"loongluca_v3",    "loongluca V3（英式英语男）", "male", "adult", 0.90},
			{"loongtomoya_v3",  "loongtomoya V3（日语男）",  "male",  "adult", 0.90},
			{"loongyuuna_v3",   "Yuuna V3（日语女）",         "female","teen",  0.90},
			{"loongyuuma_v3",   "Yuuma V3（日语男）",         "male",  "teen",  0.90},
			{"loongjihun_v3",   "Jihun V3（韩语男）",         "male",  "adult", 0.90},
			{"loongindah_v3",   "loongindah V3（印尼语女）",  "female","adult", 0.90},
			// 诗词朗诵
			{"longfei_v3",      "龙飞V3（男/热血磁性）",    "male",  "adult", 0.92},
			// 电话销售 / 客服
			{"longyingxiao_v3", "龙应笑V3（女/清甜推销）",  "female","adult", 0.91},
			{"longyingxun_v3",  "龙应询V3（男/年轻青涩）",  "male",  "adult", 0.91},
			{"longyingjing_v3", "龙应静V3（女/低调冷静）",  "female","adult", 0.91},
			{"longyingling_v3", "龙应聆V3（女/温和共情）",  "female","adult", 0.91},
			{"longyingtao_v3",  "龙应桃V3（女/温柔淡定）",  "female","adult", 0.91},
			// 语音助手
			{"longxiaochun_v3", "龙小淳V3（女/知性语音助手）","female","adult",0.93},
			{"longxiaoxia_v3",  "龙小夏V3（女/沉稳权威）",  "female","adult", 0.93},
			{"longyumi_v3",     "YUMI V3（女/正经青年）",    "female","adult", 0.93},
			{"longanyun_v3",    "龙安昀V3（男/居家暖男）",   "male",  "adult", 0.92},
			{"longanwen_v3",    "龙安温V3（女/优雅知性）",   "female","adult", 0.92},
			{"longanli_v3",     "龙安莉V3（女/利落从容）",   "female","adult", 0.92},
			{"longanlang_v3",   "龙安朗V3（男/清爽利落）",   "male",  "adult", 0.92},
			{"longyingmu_v3",   "龙应沐V3（女/优雅知性）",   "female","adult", 0.91},
			// 社交陪伴（更多）
			{"longantai_v3",    "龙安台V3（女/嗲甜台湾）",   "female","adult", 0.91},
			{"longhua_v3",      "龙华V3（女/元气甜美）",      "female","adult", 0.91},
			{"longcheng_v3",    "龙橙V3（男/智慧青年）",      "male",  "adult", 0.91},
			{"longze_v3",       "龙泽V3（男/温暖元气）",      "male",  "adult", 0.91},
			{"longzhe_v3",      "龙哲V3（男/呆板大暖男）",    "male",  "adult", 0.91},
			{"longyan_v3",      "龙颜V3（女/温暖春风）",      "female","adult", 0.91},
			{"longxing_v3",     "龙星V3（女/温婉邻家）",      "female","adult", 0.91},
			{"longtian_v3",     "龙天V3（男/磁性理智）",      "male",  "adult", 0.91},
			{"longwan_v3",      "龙婉V3（女/细腻柔声）",      "female","adult", 0.91},
			{"longqiang_v3",    "龙嫱V3（女/浪漫风情）",      "female","adult", 0.91},
			{"longfeifei_v3",   "龙菲菲V3（女/甜美娇气）",    "female","adult", 0.91},
			{"longhao_v3",      "龙浩V3（男/多情忧郁）",      "male",  "adult", 0.91},
			{"longanrou_v3",    "龙安柔V3（女/温柔闺蜜）",    "female","adult", 0.91},
			{"longhan_v3",      "龙寒V3（男/温暖痴情）",      "male",  "adult", 0.91},
			{"longanzhi_v3",    "龙安智V3（男/睿智轻熟）",    "male",  "adult", 0.91},
			{"longanling_v3",   "龙安灵V3（女/思维灵动）",    "female","adult", 0.91},
			{"longanya_v3",     "龙安雅V3（女/高雅气质）",    "female","adult", 0.91},
			{"longanqin_v3",    "龙安亲V3（女/亲和活泼）",    "female","adult", 0.91},
			// 有声书
			{"longmiao_v3",     "龙妙V3（女/抑扬顿挫）",     "female","adult", 0.93},
			{"longsanshu_v3",   "龙三叔V3（男/沉稳有声书）",  "male",  "adult", 0.93},
			{"longyuan_v3",     "龙媛V3（女/温暖治愈）",      "female","adult", 0.92},
			{"longyue_v3",      "龙悦V3（女/温暖磁性）",      "female","adult", 0.92},
			{"longxiu_v3",      "龙修V3（男/博才说书）",      "male",  "adult", 0.92},
			{"longnan_v3",      "龙楠V3（男/睿智青年）",      "male",  "adult", 0.92},
			{"longwanjun_v3",   "龙婉君V3（女/细腻柔声）",    "female","adult", 0.92},
			{"longyichen_v3",   "龙逸尘V3（男/洒脱活力）",    "male",  "adult", 0.92},
			{"longlaobo_v3",    "龙老伯V3（男/沧桑岁月）",    "male",  "elder", 0.91},
			{"longlaoyi_v3",    "龙老姨V3（女/烟火从容）",    "female","elder", 0.91},
			// 短视频配音
			{"longjiqi_v3",     "龙机器V3（呆萌机器人）",     "neutral","adult",0.91},
			{"longhouge_v3",    "龙猴哥V3（经典猴哥）",       "male",  "adult", 0.91},
			{"longdaiyu_v3",    "龙黛玉V3（娇率才女）",       "female","teen",  0.91},
			// 直播带货
			{"longanran_v3",    "龙安燃V3（女/活泼直播）",    "female","adult", 0.92},
			{"longanxuan_v3",   "龙安宣V3（女/经典直播）",    "female","adult", 0.92},
			// 新闻播报
			{"longshuo_v3",     "龙硕V3（男/干练播报）",      "male",  "adult", 0.93},
			{"longshu_v3",      "龙书V3（男/沉稳播报）",      "male",  "adult", 0.93},
			{"loongbella_v3",   "Bella 3.0（女/中英双语）",   "female","adult", 0.93},

			// ── CosyVoice v2 ─────────────────────────────────────────────
			{"longyingxiao",    "龙应笑（女/清甜推销）",      "female","adult", 0.91},
			{"longjiqi",        "龙机器（呆萌机器人）",        "neutral","adult",0.91},
			{"longhouge",       "龙猴哥（经典猴哥）",          "male",  "adult", 0.91},
			{"longjixin",       "龙机心（女/毒舌心机）",       "female","adult", 0.91},
			{"longanyue",       "龙安粤（男/欢脱粤语）",       "male",  "adult", 0.91},
			{"longshange",      "龙陕哥（男/原味陕北）",       "male",  "adult", 0.91},
			{"longanmin",       "龙安敏（女/甜美闽南）",       "female","adult", 0.91},
			{"longdaiyu",       "龙黛玉（娇率才女）",          "female","teen",  0.91},
			{"longgaoseng",     "龙高僧（男/得道高僧）",       "male",  "elder", 0.91},
			{"longanli",        "龙安莉（女/利落从容）",       "female","adult", 0.91},
			{"longanlang",      "龙安朗（男/清爽利落）",       "male",  "adult", 0.91},
			{"longanwen",       "龙安温（女/优雅知性）",       "female","adult", 0.91},
			{"longanyun",       "龙安昀（男/居家暖男）",       "male",  "adult", 0.91},
			{"longyumi_v2",     "YUMI V2（女/正经青年）",      "female","adult", 0.91},
			{"longxiaochun_v2", "龙小淳V2（女/知性积极）",     "female","adult", 0.91},
			{"longxiaoxia_v2",  "龙小夏V2（女/沉稳权威）",     "female","adult", 0.91},
			{"longyichen",      "龙逸尘（男/洒脱活力）",       "male",  "adult", 0.91},
			{"longwanjun",      "龙婉君（女/细腻柔声）",       "female","adult", 0.91},
			{"longlaobo",       "龙老伯（男/沧桑岁月）",       "male",  "elder", 0.91},
			{"longlaoyi",       "龙老姨（女/烟火从容）",       "female","elder", 0.91},
			{"longbaizhi",      "龙白芷（女/睿气旁白）",       "female","adult", 0.91},
			{"longsanshu",      "龙三叔（男/沉稳有声书）",     "male",  "adult", 0.91},
			{"longxiu_v2",      "龙修V2（男/博才说书）",       "male",  "adult", 0.91},
			{"longmiao_v2",     "龙妙V2（女/抑扬顿挫）",       "female","adult", 0.91},
			{"longyue_v2",      "龙悦V2（女/温暖磁性）",       "female","adult", 0.91},
			{"longnan_v2",      "龙楠V2（男/睿智青年）",       "male",  "adult", 0.91},
			{"longyuan_v2",     "龙媛V2（女/温暖治愈）",       "female","adult", 0.91},
			{"longanqin",       "龙安亲（女/亲和活泼）",       "female","adult", 0.91},
			{"longanya",        "龙安雅（女/高雅气质）",       "female","adult", 0.91},
			{"longanshuo",      "龙安朔（男/干净清爽）",       "male",  "adult", 0.91},
			{"longanling",      "龙安灵（女/思维灵动）",       "female","adult", 0.91},
			{"longanzhi",       "龙安智（男/睿智轻熟）",       "male",  "adult", 0.91},
			{"longanrou",       "龙安柔（女/温柔闺蜜）",       "female","adult", 0.91},
			{"longqiang_v2",    "龙嫱V2（女/浪漫风情）",       "female","adult", 0.91},
			{"longhan_v2",      "龙寒V2（男/温暖痴情）",       "male",  "adult", 0.91},
			{"longxing_v2",     "龙星V2（女/温婉邻家）",       "female","adult", 0.91},
			{"longhua_v2",      "龙华V2（女/元气甜美）",       "female","adult", 0.91},
			{"longwan_v2",      "龙婉V2（女/积极知性）",       "female","adult", 0.91},
			{"longcheng_v2",    "龙橙V2（男/智慧青年）",       "male",  "adult", 0.91},
			{"longfeifei_v2",   "龙菲菲V2（女/甜美娇气）",     "female","adult", 0.91},
			{"longxiaocheng_v2","龙小诚V2（男/磁性低音）",     "male",  "adult", 0.91},
			{"longzhe_v2",      "龙哲V2（男/呆板大暖男）",     "male",  "adult", 0.91},
			{"longyan_v2",      "龙颜V2（女/温暖春风）",       "female","adult", 0.91},
			{"longtian_v2",     "龙天V2（男/磁性理智）",       "male",  "adult", 0.91},
			{"longze_v2",       "龙泽V2（男/温暖元气）",       "male",  "adult", 0.91},
			{"longshao_v2",     "龙邵V2（男/积极向上）",       "male",  "adult", 0.91},
			{"longhao_v2",      "龙浩V2（男/多情忧郁）",       "male",  "adult", 0.91},
			{"kabuleshen_v2",   "龙深V2（男/实力歌手）",       "male",  "adult", 0.91},
			{"longhuhu",        "龙呼呼（女童/天真烂漫）",     "female","child", 0.91},
			{"longanpei",       "龙安培（女/青少年教师）",     "female","adult", 0.91},
			{"longwangwang",    "龙汪汪（台湾少年音）",        "neutral","child",0.90},
			{"longpaopao",      "龙泡泡（飞天泡泡音）",        "neutral","child",0.90},
			{"longshanshan",    "龙闪闪（戏剧化童声）",        "neutral","child",0.90},
			{"longniuniu",      "龙牛牛（阳光男童声）",        "male",  "child", 0.90},
			{"longyingmu",      "龙应沐（女/优雅知性）",       "female","adult", 0.91},
			{"longyingxun",     "龙应询（男/年轻青涩）",       "male",  "adult", 0.91},
			{"longyingcui",     "龙应催（男/严肃催收）",       "male",  "adult", 0.90},
			{"longyingda",      "龙应答（女/开朗高音）",       "female","adult", 0.90},
			{"longyingjing",    "龙应静（女/低调冷静）",       "female","adult", 0.91},
			{"longyingyan",     "龙应严（女/义正严辞）",       "female","adult", 0.90},
			{"longyingtian",    "龙应甜（女/温柔甜美）",       "female","adult", 0.91},
			{"longyingbing",    "龙应冰（女/尖锐强势）",       "female","adult", 0.90},
			{"longyingtao",     "龙应桃（女/温柔淡定）",       "female","adult", 0.91},
			{"longyingling",    "龙应聆（女/温和共情）",       "female","adult", 0.91},
			{"longanran",       "龙安燃（女/活泼直播）",       "female","adult", 0.91},
			{"longanxuan",      "龙安宣（女/经典直播）",       "female","adult", 0.91},
			{"longanchong",     "龙安冲（男/激情推销）",       "male",  "adult", 0.91},
			{"longanping",      "龙安萍（女/高亢直播）",       "female","adult", 0.90},
			{"longjielidou_v2", "龙杰力豆V2（男童/阳光顽皮）","male",  "child", 0.90},
			{"longling_v2",     "龙铃V2（女/稚气呆板）",       "female","child", 0.90},
			{"longke_v2",       "龙可V2（女/懵懂乖乖）",       "female","child", 0.90},
			{"longxian_v2",     "龙仙V2（女/豪放可爱）",       "female","child", 0.90},
			{"longlaotie_v2",   "龙老铁V2（男/东北直率）",     "male",  "adult", 0.91},
			{"longjiayi_v2",    "龙嘉怡V2（女/知性粤语）",     "female","adult", 0.91},
			{"longtao_v2",      "龙桃V2（女/积极粤语）",       "female","adult", 0.91},
			{"longfei_v2",      "龙飞V2（男/热血磁性）",       "male",  "adult", 0.91},
			{"libai_v2",        "李白V2（男/古代诗仙）",       "male",  "adult", 0.91},
			{"longjin_v2",      "龙津V2（男/优雅温润）",       "male",  "adult", 0.91},
			{"longshu_v2",      "龙书V2（男/沉稳播报）",       "male",  "adult", 0.91},
			{"loongbella_v2",   "Bella 2.0（女/精准干练）",    "female","adult", 0.91},
			{"longshuo_v2",     "龙硕V2（男/博才干练）",       "male",  "adult", 0.91},
			{"longxiaobai_v2",  "龙小白V2（女/沉稳播报）",     "female","adult", 0.91},
			{"longjing_v2",     "龙婧V2（女/典型播音）",       "female","adult", 0.91},
			{"loongstella_v2",  "Stella V2（女/飒爽利落）",    "female","adult", 0.91},
			{"loongyuuna_v2",   "loongyuuna V2（日语女）",     "female","adult", 0.90},
			{"loongyuuma_v2",   "loongyuuma V2（日语男）",     "male",  "adult", 0.90},
			{"loongjihun_v2",   "loongjihun V2（韩语男）",     "male",  "adult", 0.90},
			{"loongeva_v2",     "loongeva V2（英式英语女）",   "female","adult", 0.90},
			{"loongbrian_v2",   "loongbrian V2（英式英语男）", "male",  "adult", 0.90},
			{"loongluna_v2",    "loongluna V2（英式英语女）",  "female","adult", 0.90},
			{"loongluca_v2",    "loongluca V2（英式英语男）",  "male",  "adult", 0.90},
			{"loongemily_v2",   "loongemily V2（英式英语女）", "female","adult", 0.90},
			{"loongeric_v2",    "loongeric V2（英式英语男）",  "male",  "adult", 0.90},
			{"loongabby_v2",    "loongabby V2（美式英语女）",  "female","adult", 0.90},
			{"loongannie_v2",   "loongannie V2（美式英语女）", "female","adult", 0.90},
			{"loongandy_v2",    "loongandy V2（美式英语男）",  "male",  "adult", 0.90},
			{"loongava_v2",     "loongava V2（美式英语女）",   "female","adult", 0.90},
			{"loongbeth_v2",    "loongbeth V2（美式英语女）",  "female","adult", 0.90},
			{"loongbetty_v2",   "loongbetty V2（美式英语女）", "female","adult", 0.90},
			{"loongcindy_v2",   "loongcindy V2（美式英语女）", "female","adult", 0.90},
			{"loongcally_v2",   "loongcally V2（美式英语女）", "female","adult", 0.90},
			{"loongdavid_v2",   "loongdavid V2（美式英语男）", "male",  "adult", 0.90},
			{"loongdonna_v2",   "loongdonna V2（美式英语女）", "female","adult", 0.90},
			{"loongkyong_v2",   "loongkyong V2（韩语女）",     "female","adult", 0.90},
			{"loongtomoka_v2",  "loongtomoka V2（日语女）",    "female","adult", 0.90},
			{"loongtomoya_v2",  "loongtomoya V2（日语男）",    "male",  "adult", 0.90},

			// ── CosyVoice v1（经典音色）────────────────────────────────────
			{"longwan",         "龙婉（女/甜美温柔）",         "female","adult", 0.90},
			{"longcheng",       "龙橙（男/清晰专业）",         "male",  "adult", 0.90},
			{"longhua",         "龙华（男/成熟稳重）",         "male",  "adult", 0.90},
			{"longxiaochun",    "龙小淳（女/知性温暖）",       "female","adult", 0.90},
			{"longxiaoxia",     "龙晓夏（女/活泼朝气）",       "female","adult", 0.90},
			{"longxiaocheng",   "龙小诚（男/磁性低音）",       "male",  "adult", 0.90},
			{"longxiaobai",     "龙小白（男/年轻开朗）",       "male",  "adult", 0.90},
			{"longlaotie",      "龙老铁（男/东北话）",         "male",  "adult", 0.90},
			{"longshu",         "龙叔（男/叙述沉稳）",         "male",  "adult", 0.90},
			{"longshuo",        "龙硕（男/播报）",             "male",  "adult", 0.90},
			{"longjing",        "龙婧（女/播音）",             "female","adult", 0.90},
			{"longmiao",        "龙妙（女/有声书）",           "female","adult", 0.90},
			{"longyue",         "龙悦（女/温暖磁性）",         "female","adult", 0.90},
			{"longyuan",        "龙媛（女/有声书）",           "female","adult", 0.90},
			{"longfei",         "龙飞（男/沉稳自信）",         "male",  "adult", 0.90},
			{"longjielidou",    "龙杰力豆（儿童/新闻播报）",   "neutral","child",0.90},
			{"longtong",        "龙彤（女/导航播报）",         "female","adult", 0.90},
			{"longxiang",       "龙祥（男/磁性低沉）",         "male",  "adult", 0.90},
			{"loongstella",     "Stella（女/飒爽利落）",       "female","adult", 0.90},
			{"loongbella",      "Bella（女/精准干练）",        "female","adult", 0.90},
			// ── 千问 TTS 音色 ─────────────────────────────────────────────
			{"Cherry", "Cherry 芊悦（女/阳光亲切）", "female", "adult", 0.92},
			{"Serena", "Serena 苏瑶（女/温柔）", "female", "adult", 0.92},
			{"Ethan", "Ethan 晨煦（男/阳光活力）", "male", "adult", 0.91},
			{"Chelsie", "Chelsie 千雪（女/二次元虚拟女友）", "female", "teen", 0.91},
			{"Momo", "Momo 茉兔（女/撒娇搞怪）", "female", "teen", 0.91},
			{"Vivian", "Vivian 十三（女/拽拽可爱）", "female", "teen", 0.91},
			{"Moon", "Moon 月白（男/率性帅气）", "male", "teen", 0.91},
			{"Maia", "Maia 四月（女/知性温柔）", "female", "adult", 0.91},
			{"Kai", "Kai 凯（男/耳朵SPA）", "male", "adult", 0.91},
			{"Nofish", "Nofish 不吃鱼（男/无翘舌）", "male", "adult", 0.90},
			{"Bella", "Bella 萌宝（女童/小萝莉）", "female", "child", 0.91},
			{"Eldric Sage", "Eldric Sage 沧明子（男/沉稳睿智老者）", "male", "elder", 0.91},
			{"Mia", "Mia 乖小妹（女/温顺乖巧）", "female", "adult", 0.91},
			{"Mochi", "Mochi 沙小弥（男童/聪明早慧）", "male", "child", 0.91},
			{"Bellona", "Bellona 燕铮莺（女/洪亮江湖）", "female", "adult", 0.91},
			{"Vincent", "Vincent 田叔（男/沙哑烟嗓）", "male", "adult", 0.91},
			{"Bunny", "Bunny 萌小姬（女童/萌萝莉）", "female", "child", 0.91},
			{"Neil", "Neil 阿闻（男/新闻主持）", "male", "adult", 0.91},
			{"Elias", "Elias 墨讲师（女/知识讲解）", "female", "adult", 0.91},
			{"Arthur", "Arthur 徐大爷（男/质朴老者）", "male", "elder", 0.91},
			{"Nini", "Nini 邻家妹妹（女/甜糯少女）", "female", "teen", 0.91},
			{"Seren", "Seren 小婉（女/温和助眠）", "female", "adult", 0.91},
			{"Pip", "Pip 顽屁小孩（男童/调皮童真）", "male", "child", 0.91},
			{"Stella", "Stella 少女阿月（女/迷糊少女）", "female", "teen", 0.91},
			{"Jennifer", "Jennifer 詹妮弗（女/电影质感美语）", "female", "adult", 0.92},
			{"Ryan", "Ryan 甜茶（男/戏感炸裂）", "male", "adult", 0.91},
			{"Katerina", "Katerina 卡捷琳娜（女/御姐）", "female", "adult", 0.91},
			{"Aiden", "Aiden 艾登（男/美语大男孩）", "male", "adult", 0.91},
			{"Bodega", "Bodega 博德加（男/热情西班牙）", "male", "adult", 0.90},
			{"Sonrisa", "Sonrisa 索尼莎（女/热情拉美）", "female", "adult", 0.90},
			{"Alek", "Alek 阿列克（男/俄式冷暖）", "male", "adult", 0.90},
			{"Dolce", "Dolce 多尔切（男/慵懒意大利）", "male", "adult", 0.90},
			{"Sohee", "Sohee 素熙（女/温柔韩国欧尼）", "female", "adult", 0.90},
			{"Ono Anna", "Ono Anna 小野杏（女/鬼灵精怪日语）", "female", "teen", 0.90},
			{"Lenn", "Lenn 莱恩（男/德国叛逆青年）", "male", "teen", 0.90},
			{"Emilien", "Emilien 埃米尔安（男/浪漫法国）", "male", "adult", 0.90},
			{"Andre", "Andre 安德雷（男/磁性沉稳）", "male", "adult", 0.91},
			{"Radio Gol", "Radio Gol 拉迪奥·戈尔（男/足球解说）", "male", "adult", 0.90},
			{"Jada", "Jada 上海-阿珍（女/上海话）", "female", "adult", 0.90},
			{"Dylan", "Dylan 北京-晓东（男/北京话）", "male", "teen", 0.90},
			{"Li", "Li 南京-老李（男/南京话）", "male", "adult", 0.90},
			{"Marcus", "Marcus 陕西-秦川（男/陕西话）", "male", "adult", 0.90},
			{"Roy", "Roy 闽南-阿杰（男/闽南语）", "male", "adult", 0.90},
			{"Peter", "Peter 天津-李彼得（男/天津话）", "male", "adult", 0.90},
			{"Sunny", "Sunny 四川-晴儿（女/四川话）", "female", "teen", 0.90},
			{"Eric", "Eric 四川-程川（男/四川话）", "male", "adult", 0.90},
			{"Rocky", "Rocky 粤语-阿强（男/粤语）", "male", "adult", 0.90},
			{"Kiki", "Kiki 粤语-阿清（女/粤语）", "female", "adult", 0.90},
		}},
		// ── 腾讯云语音合成 ───────────────────────────────────────────────
		{"tencent-tts", []voiceData{
			{"101001", "智言（男/标准）", "male", "adult", 0.91},
			{"101002", "智雅（女/标准）", "female", "adult", 0.91},
			{"101003", "智燕（女/温暖）", "female", "adult", 0.91},
			{"101004", "智晶（女/标准）", "female", "adult", 0.91},
			{"101005", "智嘉（男/专业）", "male", "adult", 0.91},
			{"101006", "智开（男/播音）", "male", "adult", 0.92},
			{"101008", "智浩（男/播音）", "male", "adult", 0.92},
			{"101009", "智莉（女/温暖）", "female", "adult", 0.91},
			{"101010", "智华（男/年轻）", "male", "adult", 0.90},
			{"101011", "智燃（男/活力）", "male", "adult", 0.90},
			{"101012", "智雪（女/温柔）", "female", "adult", 0.91},
			{"101013", "智希（女/活泼）", "female", "adult", 0.90},
			{"101014", "智宁（男/成熟）", "male", "adult", 0.91},
			{"101015", "智萌（童/活泼）", "neutral", "child", 0.90},
			{"101016", "智甜（女/甜美）", "female", "adult", 0.90},
			{"101017", "智蓉（女/四川话）", "female", "adult", 0.89},
			{"101050", "WeJack（英文男声）", "male", "adult", 0.90},
			{"101051", "WeRose（英文女声）", "female", "adult", 0.90},
		}},
		// ── 可灵语音合成（已合并至 kling） ──────────────────────────────────
		{"kling", []voiceData{
			{"zh_female_story", "故事女声（中文）", "female", "adult", 0.92},
			{"zh_female_qingxin", "清新女声（中文）", "female", "adult", 0.91},
			{"zh_female_tianmei", "甜美女声（中文）", "female", "adult", 0.91},
			{"zh_female_wenrou", "温柔女声（中文）", "female", "adult", 0.91},
			{"zh_female_zhishixing", "知性女声（中文）", "female", "adult", 0.91},
			{"zh_male_story", "故事男声（中文）", "male", "adult", 0.92},
			{"zh_male_zhengpai", "正派男声（中文）", "male", "adult", 0.91},
			{"zh_male_xinwen", "新闻男声（中文）", "male", "adult", 0.91},
			{"zh_male_shuhu", "书虎男声（中文）", "male", "adult", 0.90},
			{"zh_male_qingnian", "青年男声（中文）", "male", "adult", 0.90},
			{"oversea_male1", "英文男声", "male", "adult", 0.90},
			{"oversea_female1", "英文女声", "female", "adult", 0.90},
		}},
	}

	updated := 0
	for _, pd := range data {
		var prov model.ModelProvider
		if err := db.Where("name = ? AND tenant_id = 0", pd.providerName).First(&prov).Error; err != nil {
			continue
		}
		voices := make([]model.VoiceEntry, 0, len(pd.voices))
		for _, v := range pd.voices {
			voices = append(voices, model.VoiceEntry{
				ID:       v.id,
				Name:     v.name,
				Gender:   v.gender,
				AgeGroup: v.ageGroup,
				Quality:  v.quality,
			})
		}
		b, err := json.Marshal(voices)
		if err != nil {
			logger.Errorf("seedProviderVoices: marshal %s: %v", pd.providerName, err)
			continue
		}
		if err := db.Model(&prov).Update("voices_json", string(b)).Error; err != nil {
			logger.Errorf("seedProviderVoices: update %s: %v", pd.providerName, err)
			continue
		}
		updated++
	}
	logger.Printf("seedProviderVoices: updated %d providers", updated)
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
