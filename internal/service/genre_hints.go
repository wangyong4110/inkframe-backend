package service

import "strings"

// genreVisualHints 根据小说类型返回适配图像生成的视觉风格提示，注入角色 visual_prompt 模板。
// 返回值作为 {{ GenreVisualHints }} 变量传入模板，为空字符串时模板中该行不渲染任何内容。
func genreVisualHints(genre string) string {
	g := strings.ToLower(strings.TrimSpace(genre))
	switch {
	case contains(g, "修仙", "仙侠", "xianxia"):
		return "【修仙/仙侠】服饰以道袍、广袖长衫、玉带束腰为主，配色偏素雅（白、青、墨、银）；配饰常见玉佩、法印、储物戒、飞剑剑鞘（只写外观形制，不写战斗功能）；整体气质出尘飘逸，体态端正挺拔。"
	case contains(g, "玄幻", "奇幻", "fantasy", "magic"):
		return "【玄幻/奇幻】服饰融合东方古典与异世界风格，常见华贵长袍、铠甲护肩、斗篷；配色可用金、深紫、暗红等富有神秘感的色调；配饰常见魔法纹路宝石、神兵外鞘、图腾饰品（只写外观，不写能力）。"
	case contains(g, "武侠", "wuxia"):
		return "【武侠】服饰以武者便服、劲装、侠客袍为主，实用简洁；配色偏沉稳（深灰、赭石、靛蓝）；配饰常见刀鞘、剑穗、腰带、江湖令牌（只写外观形制，不写战斗用途）；体态矫健，肌肉线条内敛有力。"
	case contains(g, "历史", "古代", "宫廷", "历史", "historical"):
		return "【历史/古代】服饰严格参照朝代制度（汉服、唐制圆领袍、明制飞鱼服、清代旗袍等），等级配色遵循制度规范；配饰含品级腰带、玉佩、发冠、朝珠（只写形制材质，不写象征权力的措辞）。"
	case contains(g, "都市", "现代", "contemporary", "urban", "modern"):
		return "【都市/现代】服饰为当代时装，按人物身份选择（职场西装、休闲街头、高定礼服等）；配色贴近现实流行色系；配饰为现代饰品（手表、项链、耳饰）；整体造型符合当代审美。"
	case contains(g, "言情", "romance", "爱情"):
		return "【言情】服饰优雅精致，女性角色常见高定连衣裙、轻薄雪纺、刺绣旗袍；男性角色常见修身西装或古风长衫；配色柔和（粉、米、香槟、莫兰迪色系）；整体气质温柔或高冷。"
	case contains(g, "科幻", "sci-fi", "science fiction", "未来", "赛博", "cyberpunk"):
		return "【科幻/赛博朋克】服饰融合高科技材质（金属纤维、光感涂层、模块化护甲）；配色偏冷调（银白、深灰、电光蓝、霓虹色点缀）；配饰含全息投影装置外壳、机械义肢（只写外观）；整体风格硬朗未来感。"
	case contains(g, "末世", "末日", "apocalyptic", "灾难"):
		return "【末世/灾难】服饰以实用主义为主（改装皮甲、军用夹克、战术背心）；配色暗沉（卡其、深绿、锈棕）；整体造型带有使用痕迹（磨损、补丁），但只描述外观状态，不写伤亡相关词汇；配饰为生存工具的外壳或容器。"
	case contains(g, "悬疑", "推理", "犯罪", "惊悚", "mystery", "thriller"):
		return "【悬疑/推理】服饰为现代都市风格，偏低调内敛（深色系西装、风衣、休闲外套）；整体造型有内敛的精致感，细节可体现职业身份（探员徽章扣、律师袖扣、记者挎包）。"
	case contains(g, "游戏", "电竞", "网游", "game"):
		return "【游戏/网游】服饰依职业区分（法师长袍、战士铠甲、刺客轻甲），色彩鲜明饱和；配饰含职业徽记、技能道具外壳（只写外观造型，不写功能数值）；整体风格偏向游戏概念设计美学。"
	case contains(g, "童话", "寓言", "fairy", "fable"):
		return "【童话/寓言】服饰轻盈可爱，拟人化动物角色保留动物特征（耳朵、尾巴、羽毛）并搭配简洁服装；人类角色服饰偏向欧式童话风（斗篷、连衣裙、短裤背带）；配色鲜亮温暖（暖黄、草绿、天蓝、玫瑰红）；整体风格圆润可爱，线条柔和，无锐角感。"
	default:
		return ""
	}
}

func contains(s string, keywords ...string) bool {
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
