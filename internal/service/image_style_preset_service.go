package service

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

type ImageStylePresetService struct {
	repo *repository.ImageStylePresetRepository
}

func NewImageStylePresetService(repo *repository.ImageStylePresetRepository) *ImageStylePresetService {
	return &ImageStylePresetService{repo: repo}
}

func (s *ImageStylePresetService) List() ([]*model.ImageStylePreset, error) {
	return s.repo.List()
}

func (s *ImageStylePresetService) GetByID(id uint) (*model.ImageStylePreset, error) {
	return s.repo.GetByID(id)
}

func (s *ImageStylePresetService) Create(p *model.ImageStylePreset) error {
	if err := s.repo.Create(p); err != nil {
		return err
	}
	refreshStylePromptCache(s.repo)
	return nil
}

func (s *ImageStylePresetService) Update(id uint, p *model.ImageStylePreset) error {
	existing, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	if existing.IsBuiltin && p.StyleID != existing.StyleID {
		return fmt.Errorf("内置预设的 style_id 不可修改")
	}
	p.ID = id
	p.IsBuiltin = existing.IsBuiltin
	p.CreatedAt = existing.CreatedAt
	if err := s.repo.Update(p); err != nil {
		return err
	}
	refreshStylePromptCache(s.repo)
	return nil
}

func (s *ImageStylePresetService) Delete(id uint) error {
	existing, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	if existing.IsBuiltin {
		return fmt.Errorf("内置预设不可删除")
	}
	if err := s.repo.Delete(id); err != nil {
		return err
	}
	refreshStylePromptCache(s.repo)
	return nil
}

// SeedBuiltinImageStylePresets writes the built-in image style presets to DB (idempotent, upsert by style_id).
// 同时清理不再属于当前内置目录的旧 builtin 预设（即"删除之前的画风，换成新目录"的语义），
// 管理员通过 /image-style-presets 手动新增的非 builtin 预设不受影响。
func SeedBuiltinImageStylePresets(repo *repository.ImageStylePresetRepository) {
	defs := builtinImageStylePresets()
	styleIDs := make([]string, 0, len(defs))
	for i, p := range defs {
		p.SortOrder = i
		p.IsBuiltin = true
		p.Enabled = true
		_ = repo.Upsert(p)
		styleIDs = append(styleIDs, p.StyleID)
	}
	_ = repo.DeleteBuiltinNotIn(styleIDs)
	refreshStylePromptCache(repo)
}

// cachedStylePreset 是风格库单条记录在进程内缓存里保留的字段子集。
type cachedStylePreset struct {
	prompt   string
	category string // PromptCategory：realistic/anime/classic_illustration/dark_stylized/pixel/render_3d
}

// stylePresetCache 是 style_id → {prompt, category} 的进程内缓存，供 character_service.go 里的
// resolveStyleIllustrationDesc()/resolveStyleCategory() 在渲染 image_prompt 时查询，
// 取代此前逐风格硬编码的 switch/map。
// 用 atomic.Pointer 是因为读多写少（每次生成图片都读，只有种子初始化/管理员增删改预设时才写），
// 且这两个函数是纯函数，不持有 repo，只能通过包级缓存打通这条链路。
var stylePresetCache atomic.Pointer[map[string]cachedStylePreset]

// refreshStylePromptCache 从数据库重新加载全部启用中的预设到缓存。
// repo 为 nil 或查询失败时保留旧缓存（不清空，避免瞬时抖动）。
func refreshStylePromptCache(repo *repository.ImageStylePresetRepository) {
	if repo == nil {
		return
	}
	presets, err := repo.List()
	if err != nil {
		return
	}
	m := make(map[string]cachedStylePreset, len(presets))
	for _, p := range presets {
		if !p.Enabled {
			continue
		}
		m[p.StyleID] = cachedStylePreset{prompt: p.Prompt, category: p.PromptCategory}
	}
	stylePresetCache.Store(&m)
}

// lookupStylePresetFromCache 供 resolveStyleIllustrationDesc/resolveStyleCategory 查询进程内缓存，
// 找不到返回 (cachedStylePreset{}, false)。
func lookupStylePresetFromCache(styleID string) (cachedStylePreset, bool) {
	cache := stylePresetCache.Load()
	if cache == nil {
		return cachedStylePreset{}, false
	}
	p, ok := (*cache)[styleID]
	return p, ok
}

func builtinImageStylePresets() []*model.ImageStylePreset {
	tagsJSON := func(tags ...string) string {
		b, _ := json.Marshal(tags)
		return string(b)
	}
	colorsJSON := func(colors ...string) string {
		b, _ := json.Marshal(colors)
		return string(b)
	}

	type def struct {
		styleID, name, description, category, promptCategory, prompt string
		tags, colors                                                 []string
	}
	defs := []def{
		// ── AI真人剧 (live_action) ──────────────────────────────────────────
		{"modern_short_drama", "现代短剧", "真人电视剧风格，精品短剧画风，大师级构图，超写实真人电影质感,真实皮肤纹理与毛孔细节,电影级柔光与发丝光", "live_action", "realistic",
			"真人电视剧风格，精品短剧画风，大师级构图，超写实真人电影质感,真实皮肤纹理与毛孔细节,电影级柔光与发丝光",
			[]string{"逆袭", "复仇", "都市"}, []string{"#374151", "#4B5563", "#9CA3AF"}},
		{"beautiful_ancient_costume", "美型古装", "精品古装仙侠真人电视剧临江仙风格，美白滤镜，细腻真实的皮肤质感，精致打光，极致高清画质", "live_action", "realistic",
			"精品古装仙侠真人电视剧临江仙风格，美白滤镜，细腻真实的皮肤质感，精致打光，极致高清画质",
			[]string{"仙侠", "美颜", "东方"}, []string{"#A7C7E7", "#F5DEB3", "#C9A0DC"}},
		{"classic_ancient_costume", "经典古装", "精品古装真人短剧风格，专业打光，高质量电视剧质感", "live_action", "realistic",
			"精品古装真人短剧风格，专业打光，高质量电视剧质感",
			[]string{"古装", "宫廷", "历史"}, []string{"#8B0000", "#DAA520", "#2F2F2F"}},
		{"theatrical_blockbuster", "院线电影风", "参考院线电影，真人电影风格，达芬奇专业调色，大师级构图与，电影色调", "live_action", "realistic",
			"参考院线电影，真人电影风格，达芬奇专业调色，大师级构图与，电影色调",
			[]string{"电影", "质感", "好莱坞"}, []string{"#1C1C1C", "#B8860B", "#8B0000"}},
		{"zhang_yimou_style", "张艺谋风格", "参考张艺谋电影风格，极致用色， 强烈构图，仪式感叙事", "live_action", "realistic",
			"参考张艺谋电影风格，极致用色， 强烈构图，仪式感叙事",
			[]string{"历史", "大片", "仪式感"}, []string{"#C0392B", "#8B0000", "#FFD700"}},
		{"european_epic", "欧洲史诗风格", "斯巴达勇士风格，角斗士风格，古装史诗风格，史诗级大片质感，戏剧性的光线，浓重的明暗对比", "live_action", "realistic",
			"斯巴达勇士风格，角斗士风格，古装史诗风格，史诗级大片质感，戏剧性的光线，浓重的明暗对比",
			[]string{"史诗", "历史", "恢宏"}, []string{"#6B4226", "#8B7355", "#C0C0C0"}},
		{"blade_runner_style", "银翼杀手风格", "银翼杀手2049风格，（Minimalist Brutalist Cyberpunk），只用一种颜色来统治画面，粗野主义巨物建筑，气象级的环境粒子，留白。", "live_action", "realistic",
			"银翼杀手2049风格，（Minimalist Brutalist Cyberpunk），只用一种颜色来统治画面，粗野主义巨物建筑，气象级的环境粒子，留白。",
			[]string{"未来", "废墟", "孤独"}, []string{"#FF2D55", "#00CFFD", "#1A1A2E"}},
		{"game_of_thrones_style", "权力的游戏", "参考权力的游戏电视剧画风，冷色史诗写实 + 中世纪权谋氛围 + 粗粝真实质感 + 低饱和电影调色", "live_action", "realistic",
			"参考权力的游戏电视剧画风，冷色史诗写实 + 中世纪权谋氛围 + 粗粝真实质感 + 低饱和电影调色",
			[]string{"中世纪", "战争", "史诗", "暗黑"}, []string{"#2F2F2F", "#4A4A4A", "#8B0000"}},
		{"crime_drama_style", "剧情犯罪", "参考绝命毒师电视剧画风，犯罪题材美学，南美风格滤镜，真实质感滤镜", "live_action", "realistic",
			"参考绝命毒师电视剧画风，犯罪题材美学，南美风格滤镜，真实质感滤镜",
			[]string{"毒枭", "监狱", "剧情"}, []string{"#2C3E50", "#34495E", "#7F1D1D"}},
		{"modern_korean_drama", "现代韩剧风", "韩剧偶像剧风格，干净高级的商业影像 + 柔光美颜 + 偶像剧式浪漫氛围", "live_action", "realistic",
			"韩剧偶像剧风格，干净高级的商业影像 + 柔光美颜 + 偶像剧式浪漫氛围",
			[]string{"言情", "都市", "偶像"}, []string{"#FFC0CB", "#FFD1DC", "#E6E6FA"}},
		{"kurosawa_style", "黑泽明风格", "黑泽明风格，高对比黑白质感 + 强烈自然元素（风雨尘）+ 动态构图 + 戏剧化光影 + 人性史诗感", "live_action", "realistic",
			"黑泽明风格，高对比黑白质感 + 强烈自然元素（风雨尘）+ 动态构图 + 戏剧化光影 + 人性史诗感",
			[]string{"武士", "黑白", "磅礴"}, []string{"#1A1A1A", "#4A4A4A", "#D3D3D3"}},
		{"nolan_style", "诺兰风格", "诺兰风格，IMAX大画幅质感，冷蓝灰色调，极其锐利的画面，深沉严肃的氛围，精密的光线控制", "live_action", "realistic",
			"诺兰风格，IMAX大画幅质感，冷蓝灰色调，极其锐利的画面，深沉严肃的氛围，精密的光线控制",
			[]string{"烧脑", "冷调", "宏大", "严肃"}, []string{"#1F2937", "#374151", "#60A5FA"}},
		{"tarantino_style", "昆汀·塔伦蒂诺风格", "昆汀风格，高对比度，暴力美学，大胆的构图", "live_action", "realistic",
			"昆汀风格，高对比度，暴力美学，大胆的构图",
			[]string{"暴力", "复古", "cult"}, []string{"#B22222", "#FFD700", "#1C1C1C"}},
		{"david_lynch_style", "大卫·林奇风格", "大卫林奇风格，在看似平淡无奇的日常表象下，隐藏着极度诡异、荒诞、甚至令人毛骨悚然的超现实梦魇", "live_action", "realistic",
			"大卫林奇风格，在看似平淡无奇的日常表象下，隐藏着极度诡异、荒诞、甚至令人毛骨悚然的超现实梦魇",
			[]string{"诡异", "梦境", "暗黑"}, []string{"#2B0B3F", "#8B0000", "#000000"}},
		{"wes_anderson_style", "韦斯·安德森风格", "韦斯安德森风格，糖果色马卡龙配色", "live_action", "realistic",
			"韦斯安德森风格，糖果色马卡龙配色",
			[]string{"对称", "马卡龙色", "精致"}, []string{"#FADADD", "#FFDAB9", "#B5EAD7"}},
		{"wong_kar_wai_style", "王家卫风格", "王家卫风格，慵懒暧昧的氛围，颗粒感胶片，东方都市孤独美学", "live_action", "realistic",
			"王家卫风格，慵懒暧昧的氛围，颗粒感胶片，东方都市孤独美学",
			[]string{"暧昧", "霓虹", "孤独"}, []string{"#8B008B", "#FF1493", "#1A1A2E"}},
		{"shaw_brothers_wuxia", "邵氏武侠", "参考港式武侠电视剧风格，邵氏电影风格，电影感", "live_action", "realistic",
			"参考港式武侠电视剧风格，邵氏电影风格，电影感",
			[]string{"复古", "武侠", "酱板鸭"}, []string{"#B22222", "#DAA520", "#2F2F2F"}},
		{"cyberpunk_live_action", "赛博朋克", "参考真人赛博朋克电影，电影质感，极致高清画质", "live_action", "realistic",
			"参考真人赛博朋克电影，电影质感，极致高清画质",
			[]string{"赛博朋克", "真人电影", "高清"}, []string{"#6C63FF", "#FF2D55", "#00CFFD"}},

		// ── AI漫剧 (anime) ──────────────────────────────────────────────────
		{"universal_3d", "通用3D", "3D、游戏CG，影视级、虚幻引擎渲染", "anime", "render_3d",
			"3D、游戏CG，影视级、虚幻引擎渲染",
			[]string{"写实", "虚幻引擎"}, []string{"#3498DB", "#2C3E50", "#95A5A6"}},
		{"xianxia_fantasy_3d", "玄幻仙侠3D", "国风3D、影视级、虚幻引擎渲染", "anime", "render_3d",
			"国风3D、影视级、虚幻引擎渲染",
			[]string{"修仙", "斗破", "国风"}, []string{"#2ECC71", "#A8E6CF", "#F7E7CE"}},
		{"modern_urban_2d", "现代都市2D", "商业动画画风，柔和光影效果，轻柔的赛璐珞上色，柔和的漫射光线，清晰干净的细轮廓线条，参考京都动画作品，参考石立太一动画作品，2d动画", "anime", "anime",
			"商业动画画风，柔和光影效果，轻柔的赛璐珞上色，柔和的漫射光线，清晰干净的细轮廓线条，参考京都动画作品，参考石立太一动画作品，2d动画",
			[]string{"都市", "校园", "日常"}, []string{"#87CEEB", "#FFFFFF", "#F5F5F5"}},
		{"ancient_xianxia_2d", "古风仙侠2D", "商业动画画风，柔和光影效果，轻柔的赛璐珞上色，柔和的漫射光线，清晰干净的细轮廓线条，参考京都动画作品，参考石立太一动画作品，2d动画", "anime", "anime",
			"商业动画画风，柔和光影效果，轻柔的赛璐珞上色，柔和的漫射光线，清晰干净的细轮廓线条，参考京都动画作品，参考石立太一动画作品，2d动画",
			[]string{"古装", "修仙", "历史"}, []string{"#C9A0DC", "#F7E7CE", "#A8DADC"}},
		{"wasteland_game_cg", "废土游戏CG", "电影级CG渲染，废土写实主义，斑驳做旧质感，体积光与耶稣光，极致细节纹理，浅景深镜头，真实空气感介质", "anime", "render_3d",
			"电影级CG渲染，废土写实主义，斑驳做旧质感，体积光与耶稣光，极致细节纹理，浅景深镜头，真实空气感介质",
			[]string{"废土写实", "电影CG", "体积光"}, []string{"#8B7355", "#A0522D", "#2F2F2F"}},
		{"chibi_3d", "Q版3D", "Q版3D风格，精致3D CG，Q版可爱比例", "anime", "render_3d",
			"Q版3D风格，精致3D CG，Q版可爱比例",
			[]string{"Q版可爱", "精致3D", "角色萌系"}, []string{"#FFB6C1", "#FFD700", "#87CEFA"}},
		{"voxel_block_world", "方块世界", "方块世界风格，精致3D CG，复古像素艺术，高清体素方块，鲜艳高饱和色彩", "anime", "render_3d",
			"方块世界风格，精致3D CG，复古像素艺术，高清体素方块，鲜艳高饱和色彩",
			[]string{"方块世界", "体素3D", "像素艺术"}, []string{"#F39C12", "#3498DB", "#2ECC71"}},
		{"clay_stopmotion", "粘土玩具", "粘土玩具风格，粘土玩具定格感，写实质感，手工粘土材质，玩具手办质感，柔和清透色调，鲜艳高饱和色彩", "anime", "render_3d",
			"粘土玩具风格，粘土玩具定格感，写实质感，手工粘土材质，玩具手办质感，柔和清透色调，鲜艳高饱和色彩",
			[]string{"粘土玩具", "定格质感", "高饱和"}, []string{"#FF6F61", "#FFD166", "#06D6A0"}},
		{"western_3d_cg", "欧美3D CG", "欧美CG风格，电影级光影，写实质感，细腻皮肤与SSS，浅景深虚化，次世代3A渲染，精致3D CG", "anime", "render_3d",
			"欧美CG风格，电影级光影，写实质感，细腻皮肤与SSS，浅景深虚化，次世代3A渲染，精致3D CG",
			[]string{"欧美CG", "写实质感", "3A渲染"}, []string{"#34495E", "#2980B9", "#95A5A6"}},
		{"korean_webtoon_urban", "韩系都市条漫", "韩国条漫风格, 清晰干净的线稿, 赛璐璐阴影渲染, 现代都市美学, 二维数码插画, 柔和渐变光影, 时尚精细画风", "anime", "anime",
			"韩国条漫风格, 清晰干净的线稿, 赛璐璐阴影渲染, 现代都市美学, 二维数码插画, 柔和渐变光影, 时尚精细画风",
			[]string{"韩系条漫", "都市时尚", "柔和光影"}, []string{"#FFC2D4", "#A8D8EA", "#FF85A1"}},
		{"crime_city_illustration", "罪城游戏插画", "GTA风格插画, 赛璐璐矢量插画, 写实主义插画，清晰的黑色轮廓线, 戏剧性强光影", "anime", "dark_stylized",
			"GTA风格插画, 赛璐璐矢量插画, 写实主义插画，清晰的黑色轮廓线, 戏剧性强光影",
			[]string{"美式游戏", "赛璐珞矢量", "强光影"}, []string{"#E74C3C", "#F39C12", "#1C1C1C"}},
		{"sanhuer_3d2d", "三渲二", "风格化3D，赛璐璐风格 3D，三渲二，手绘线条笔触，柔和辉光，柔和清透色调，鲜艳高饱和色彩", "anime", "render_3d",
			"风格化3D，赛璐璐风格 3D，三渲二，手绘线条笔触，柔和辉光，柔和清透色调，鲜艳高饱和色彩",
			[]string{"三渲二", "赛璐珞3D", "高饱和"}, []string{"#FF6B9D", "#4FACFE", "#C44FDB"}},
		{"guofeng_2d_anime", "2D国风漫剧", "国风动漫美术风格，干净纤细线稿，赛璐珞扁平上色，低饱和度雅致色调，柔和漫反射光，温暖治愈氛围", "anime", "anime",
			"国风动漫美术风格，干净纤细线稿，赛璐珞扁平上色，低饱和度雅致色调，柔和漫反射光，温暖治愈氛围",
			[]string{"国风动漫", "赛璐珞", "治愈氛围"}, []string{"#2ECC71", "#F7E7CE", "#A8E6CF"}},
		{"otome_3d", "3D乙游", "3D写实二次元，乙女游戏CG画质，次世代3A渲染，细腻皮肤渲染，高精发丝细节，写实质感，清新明亮体积光", "anime", "render_3d",
			"3D写实二次元，乙女游戏CG画质，次世代3A渲染，细腻皮肤渲染，高精发丝细节，写实质感，清新明亮体积光",
			[]string{"乙女游戏", "3D二次元", "次世代渲染"}, []string{"#FFB3C6", "#C8A2C8", "#B5EAD7"}},
		{"guochuang_xianxia_3d", "3D国创仙侠", "国漫3D写实CG，东方玄幻仙侠美学，电影级真实材质，细腻发丝与皮肤质感，电影级体积光，朦胧空气感与烟雾", "anime", "render_3d",
			"国漫3D写实CG，东方玄幻仙侠美学，电影级真实材质，细腻发丝与皮肤质感，电影级体积光，朦胧空气感与烟雾",
			[]string{"仙侠玄幻", "国漫3D", "云雾氛围"}, []string{"#A8DADC", "#2ECC71", "#F7E7CE"}},
		{"pixel_farm", "像素农场", "16位像素艺术，星露谷物语风格，清晰的深色外轮廓，温馨明快的色彩组合，复古游戏美学，简化的平涂阴影，像素化肌理", "anime", "pixel",
			"16位像素艺术，星露谷物语风格，清晰的深色外轮廓，温馨明快的色彩组合，复古游戏美学，简化的平涂阴影，像素化肌理",
			[]string{"16位像素", "温馨农场", "复古游戏"}, []string{"#8BC34A", "#FFEB3B", "#795548"}},
		{"western_wild_game_cg", "西部荒野游戏CG", "Rockstar游戏引擎电影级渲染，体积光与丁达尔效应，尘土飞扬的大气氛围，粗粝饱经风霜的质感，温暖泥土色调，电影级胶片微粒，黄昏逆光与镜头光晕", "anime", "render_3d",
			"Rockstar游戏引擎电影级渲染，体积光与丁达尔效应，尘土飞扬的大气氛围，粗粝饱经风霜的质感，温暖泥土色调，电影级胶片微粒，黄昏逆光与镜头光晕",
			[]string{"西部荒野", "游戏CG", "黄昏逆光"}, []string{"#D2691E", "#FFD700", "#2F2F2F"}},
		{"dual_city_steampunk", "双城蒸汽朋克", "《双城之战》艺术风格，三维厚涂手绘风格，海克斯科技蒸汽朋克美学，戏剧性体积光，萤光霓虹微粒，斑驳铜锈金属质感，棱角分明的硬朗线条", "anime", "dark_stylized",
			"《双城之战》艺术风格，三维厚涂手绘风格，海克斯科技蒸汽朋克美学，戏剧性体积光，萤光霓虹微粒，斑驳铜锈金属质感，棱角分明的硬朗线条",
			[]string{"双城风格", "蒸汽朋克", "手绘3D"}, []string{"#8B6914", "#C5892A", "#4682B4"}},
		{"cartoon_figure_3d", "卡通小人3D", "皮克斯风格3D动画风格，超写实材质纹理，高保真织物与纤维细节，柔和容积工作室光影，微缩景深效果，细腻哑光搪胶皮肤", "anime", "render_3d",
			"皮克斯风格3D动画风格，超写实材质纹理，高保真织物与纤维细节，柔和容积工作室光影，微缩景深效果，细腻哑光搪胶皮肤",
			[]string{"3D动画", "卡通小人", "柔和棚拍"}, []string{"#87CEFA", "#FFB6C1", "#FFFFFF"}},
		{"impasto_retro_illustration", "颗粒复古手绘", "厚涂数字绘画, 不规则几何块面笔触, 戏剧性明暗对比，复古颗粒感，概念艺术插画，斑驳杂色质感", "anime", "classic_illustration",
			"厚涂数字绘画, 不规则几何块面笔触, 戏剧性明暗对比，复古颗粒感，概念艺术插画，斑驳杂色质感",
			[]string{"厚涂手绘", "复古颗粒", "概念插画"}, []string{"#C8A96E", "#8B7355", "#4A3728"}},
		{"yanyun_shiliuzhou", "燕云十六州", "国漫3D写实CG，东方武侠美学，电影级三维渲染, 细腻皮肤与发丝质感，超写实材质纹理，高保真织物与纤维细节，戏剧性光影与丁达尔效应，电影级景深微醺氛围", "anime", "render_3d",
			"国漫3D写实CG，东方武侠美学，电影级三维渲染, 细腻皮肤与发丝质感，超写实材质纹理，高保真织物与纤维细节，戏剧性光影与丁达尔效应，电影级景深微醺氛围",
			[]string{"武侠", "仙侠", "写实CG"}, []string{"#8B4513", "#CD853F", "#2F4F4F"}},
		{"oil_painting_arcane", "油画三渲二", "参考《双城之战》 (Fortiche / Arcane Style)画风", "anime", "classic_illustration",
			"参考《双城之战》 (Fortiche / Arcane Style)画风",
			[]string{"双城之战", "厚涂", "油画质感"}, []string{"#4A0E6B", "#C44FDB", "#1A1A2E"}},
		{"american_3d_animation", "美式3D", "美式3D动画电影风格", "anime", "render_3d",
			"美式3D动画电影风格",
			[]string{"美式动画", "3D电影", "合家欢"}, []string{"#FF7F50", "#4FACFE", "#FFD700"}},
		{"ink_wash_bw", "黑白水墨", "硬核传统2D水墨/(Hardcore Traditional Ink)，视觉特点： 保留生猛的毛笔枯笔笔触，张力拉满。参考《雾山五行》风格", "anime", "classic_illustration",
			"硬核传统2D水墨/(Hardcore Traditional Ink)，视觉特点： 保留生猛的毛笔枯笔笔触，张力拉满。参考《雾山五行》风格",
			[]string{"水墨", "黑白", "枯笔张力"}, []string{"#1A1A1A", "#4A4A4A", "#D3D3D3"}},
		{"ink_wash_color", "彩色水墨", "硬核传统2D水墨/剪纸 (Hardcore Traditional Ink)，视觉特点： 保留生猛的毛笔枯笔笔触，色彩借鉴中国传统重彩，战斗动作如中国武术般行云流水，张力拉满。参考《雾山五行》风格", "anime", "classic_illustration",
			"硬核传统2D水墨/剪纸 (Hardcore Traditional Ink)，视觉特点： 保留生猛的毛笔枯笔笔触，色彩借鉴中国传统重彩，战斗动作如中国武术般行云流水，张力拉满。参考《雾山五行》风格",
			[]string{"水墨", "重彩", "武术张力"}, []string{"#8B0000", "#DAA520", "#1A1A2E"}},
		{"wool_felt_style", "羊毛毡风格", "羊毛毡风格，定格动画，真实光影，极致细节，氛围感，故事感，大师级构图", "anime", "render_3d",
			"羊毛毡风格，定格动画，真实光影，极致细节，氛围感，故事感，大师级构图",
			[]string{"羊毛毡", "定格动画", "故事感"}, []string{"#D2691E", "#F5DEB3", "#8B7355"}},
		{"claymation_style", "黏土动画", "黏土动画风格,定格动画,真实光影,大师级构图", "anime", "render_3d",
			"黏土动画风格,定格动画,真实光影,大师级构图",
			[]string{"黏土", "定格动画", "真实光影"}, []string{"#FF6F61", "#FFD166", "#8B7355"}},
		{"horror_ghost_story", "惊悚怪谈风", "低饱和度色调，日式惊悚动画美学", "anime", "dark_stylized",
			"低饱和度色调，日式惊悚动画美学",
			[]string{"惊悚", "怪谈", "低饱和"}, []string{"#2C3E50", "#4A4A4A", "#1A1A1A"}},
		{"korean_manhwa_romance", "韩漫风格", "Korean webtoon style, semi-realistic anime, clean soft lineart, smooth gradient shading, glowing skin, pastel color palette, romantic lighting, close-up composition, emotional atmosphere, high detail character", "anime", "anime",
			"Korean webtoon style, semi-realistic anime, clean soft lineart, smooth gradient shading, glowing skin, pastel color palette, romantic lighting, close-up composition, emotional atmosphere, high detail character",
			[]string{"韩漫", "甜宠", "唯美"}, []string{"#FF85A1", "#FFC2D4", "#A8D8EA"}},
		{"next_gen_cel_shading", "次时代二渲三", "次世代高精三渲二 (Next-Gen Cel-Shading 3D) Zenless Zone Zero style，极致干净的赛璐璐线条，结合3D的平滑运镜。面部阴影经过极其严格的法线调整，保证任何角度都唯美", "anime", "render_3d",
			"次世代高精三渲二 (Next-Gen Cel-Shading 3D) Zenless Zone Zero style，极致干净的赛璐璐线条，结合3D的平滑运镜。面部阴影经过极其严格的法线调整，保证任何角度都唯美",
			[]string{"二渲三", "次时代", "赛璐璐"}, []string{"#00CFFD", "#FF2D55", "#1A1A2E"}},
		{"ghibli_animation", "宫崎骏动画", "参考吉卜力动画电影风格，宫崎骏动画风格", "anime", "anime",
			"参考吉卜力动画电影风格，宫崎骏动画风格",
			[]string{"吉卜力", "治愈", "手绘感"}, []string{"#87CEEB", "#A8E6CF", "#F7E7CE"}},
		{"shounen_battle_anime", "高燃战斗番", "参考《鬼灭之刃》画风、参考Ufotable飞碟社画风，粗描边", "anime", "anime",
			"参考《鬼灭之刃》画风、参考Ufotable飞碟社画风，粗描边",
			[]string{"战斗番", "粗描边", "燃"}, []string{"#E74C3C", "#1C1C1C", "#F39C12"}},
		{"cthulhu_gothic_horror", "克苏鲁风", "参考血源诅咒画风，克苏鲁风格、哥特、写实阴暗、 阴冷雾气、低饱和冷色调、虚幻引擎渲染", "anime", "dark_stylized",
			"参考血源诅咒画风，克苏鲁风格、哥特、写实阴暗、 阴冷雾气、低饱和冷色调、虚幻引擎渲染",
			[]string{"克苏鲁", "哥特", "阴冷"}, []string{"#1A0A2E", "#4A0E6B", "#2F2F2F"}},
		{"junji_ito_style", "伊藤润二风", "惊悚诡异风、线条锐利,参考伊藤润二动画(线条设计+色彩搭配+氛围营造),数字漫画笔触、轻微颗粒感、哑光质感,惊悚压抑、悬疑感", "anime", "dark_stylized",
			"惊悚诡异风、线条锐利,参考伊藤润二动画(线条设计+色彩搭配+氛围营造),数字漫画笔触、轻微颗粒感、哑光质感,惊悚压抑、悬疑感",
			[]string{"惊悚漫画", "锐利线条", "悬疑"}, []string{"#1C1C1C", "#7F8C8D", "#8B0000"}},
		{"retro_90s_anime", "日本复古动画", "参考渡边信一郎作品风格,参考神山健治作品,90年代日本复古动漫风格,上世纪九十年代日漫风格的动漫,层次感,线条清晰,迷人氛围", "anime", "anime",
			"参考渡边信一郎作品风格,参考神山健治作品,90年代日本复古动漫风格,上世纪九十年代日漫风格的动漫,层次感,线条清晰,迷人氛围",
			[]string{"复古动漫", "九十年代", "迷人氛围"}, []string{"#C8A96E", "#8B7355", "#4A3728"}},
	}

	presets := make([]*model.ImageStylePreset, 0, len(defs))
	for _, d := range defs {
		presets = append(presets, &model.ImageStylePreset{
			StyleID:        d.styleID,
			Name:           d.name,
			Description:    d.description,
			Tags:           tagsJSON(d.tags...),
			Category:       d.category,
			PromptCategory: d.promptCategory,
			PreviewColors:  colorsJSON(d.colors...),
			Prompt:         d.prompt,
		})
	}
	return presets
}
