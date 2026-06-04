package service

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	astisub "github.com/asticode/go-astisub"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// maxExportMediaBytes P2-4: skip local media embeds beyond this limit to prevent OOM.
const maxExportMediaBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

// CapCutService 剪映草稿导出服务
type CapCutService struct {
	segmentRepo   *repository.ShotVoiceSegmentRepository // P1-2: for including multi-segment audio in exports
	sfxItemRepo   *repository.ShotSFXItemRepository      // for including multi-item SFX in exports
	serverBaseURL string                                  // 服务器自身 base URL，用于解析 /uploads/... 和 /api/v1/media/... 相对路径
}

func NewCapCutService() *CapCutService {
	return &CapCutService{}
}

// WithSFXItemRepo 注入 ShotSFXItem 仓库，使导出时能包含多条音效（完整 StartOffset/Volume/Duration）。
func (s *CapCutService) WithSFXItemRepo(r *repository.ShotSFXItemRepository) *CapCutService {
	s.sfxItemRepo = r
	return s
}

// WithSegmentRepo 注入 VoiceSegment 仓库，使导出时能包含多段配音音频。
func (s *CapCutService) WithSegmentRepo(r *repository.ShotVoiceSegmentRepository) *CapCutService {
	s.segmentRepo = r
	return s
}

// WithServerBaseURL 注入服务器自身 base URL（如 "http://127.0.0.1:8080"）。
// 用于在本地/DB 存储模式下将相对 URL（/uploads/...、/api/v1/media/...）解析为可下载的完整 URL。
func (s *CapCutService) WithServerBaseURL(u string) *CapCutService {
	s.serverBaseURL = strings.TrimRight(u, "/")
	return s
}

// resolveMedia 统一媒体 URL 解析，支持所有存储后端：
//   - https://...     → CDN/OSS，直接下载
//   - file:///...     → 本地绝对路径
//   - /uploads/key    → 先尝试 ./uploads/key（本地 storage）；失败则走 serverBaseURL
//   - /api/v1/media/N → 通过 serverBaseURL 下载（DB storage）
//   - 裸相对/绝对路径   → os.ReadFile
func (s *CapCutService) resolveMedia(mediaURL string) ([]byte, error) {
	if mediaURL == "" {
		return nil, fmt.Errorf("empty URL")
	}
	if strings.HasPrefix(mediaURL, "http://") || strings.HasPrefix(mediaURL, "https://") {
		return downloadMediaFile(mediaURL)
	}
	if strings.HasPrefix(mediaURL, "file://") {
		return os.ReadFile(strings.TrimPrefix(mediaURL, "file://"))
	}
	// 相对 HTTP 路径（/uploads/... 或 /api/v1/media/...）
	if strings.HasPrefix(mediaURL, "/") {
		// 优先尝试：将 /uploads/key 映射到 ./uploads/key（服务器工作目录下的本地文件）
		if data, err := os.ReadFile("." + mediaURL); err == nil {
			return data, nil
		}
		// 降级：通过服务器自身 HTTP 接口下载（处理 /api/v1/media/{id} 等 DB 存储路径）
		if s.serverBaseURL != "" {
			return downloadMediaFile(s.serverBaseURL + mediaURL)
		}
		return nil, fmt.Errorf("cannot resolve relative URL %q: no server base URL configured", mediaURL)
	}
	// 裸本地路径
	return os.ReadFile(mediaURL)
}

// --- CapCut 草稿 JSON 结构体 ---

type ccFlip struct {
	Horizontal bool `json:"horizontal"`
	Vertical   bool `json:"vertical"`
}

type ccScale struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type ccTransform struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// --- 关键帧动画结构体（Ken Burns 运镜效果）---

type ccPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type ccKeyframe struct {
	CurveType    string    `json:"curve_type"`  // snake_case：CapCut 按此键解析曲线类型枚举，camelCase 会导致解析失败崩溃
	GraphID      string    `json:"graph_id"`    // snake_case：同上
	ID           string    `json:"id"`
	LeftControl  ccPoint   `json:"left_control"`
	RightControl ccPoint   `json:"right_control"`
	TimeOffset   int64     `json:"time_offset"`
	Values       []float64 `json:"values"`
}

// ccKeyframeGroup 一段素材上单个属性（ScaleX/ScaleY/PositionX/PositionY）的关键帧组
type ccKeyframeGroup struct {
	ID           string       `json:"id"`
	KeyframeList []ccKeyframe `json:"keyframe_list"`
	MaterialID   string       `json:"material_id"` // 引用 segment.ID
	PropertyType string       `json:"property_type"`
}

// ccKeyframes 关键帧集合。
// CapCut 会访问每一个子数组；若字段缺失或为 null，CapCut 迭代时崩溃 → 草稿点击无响应。
// 所有子数组必须序列化为 []（非 null）。
type ccKeyframes struct {
	Adjusts      []interface{}     `json:"adjusts"`
	Audios       []interface{}     `json:"audios"`
	ColorWheels  []interface{}     `json:"color_wheels"`
	Effects      []interface{}     `json:"effects"`       // CapCut International 遍历此数组，缺失即崩溃
	Filters      []interface{}     `json:"filters"`
	Handwrites   []interface{}     `json:"handwrites"`
	SpeedStickers []interface{}    `json:"speed_stickers"` // 同上
	Stickers     []interface{}     `json:"stickers"`
	Texts        []interface{}     `json:"texts"`
	Videos       []ccKeyframeGroup `json:"videos"`
	VocalSounds  []interface{}     `json:"vocal_sounds"`  // 同上
}

// --- 新增：Segment 辅助字段结构体 ---

type ccHdrSettings struct {
	Mode      int     `json:"mode"`
	Intensity float64 `json:"intensity"`
	Nits      int     `json:"nits"`
}

type ccUniformScale struct {
	On    bool    `json:"on"`
	Value float64 `json:"value"`
}

type ccResponsiveLayout struct {
	Enable              bool   `json:"enable"`
	TargetFollow        string `json:"target_follow"`
	SizeLayout          int    `json:"size_layout"`
	HorizontalPosLayout int    `json:"horizontal_pos_layout"`
	VerticalPosLayout   int    `json:"vertical_pos_layout"`
}

// --- 新增：伴生素材结构体 ---

// ccSpeedMaterial speed 伴生素材（每个 segment 必须有一个）
type ccSpeedMaterial struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`        // "speed"
	Mode       int         `json:"mode"`
	Speed      float64     `json:"speed"`
	CurveSpeed interface{} `json:"curve_speed"` // null
}

// ccPlaceholderInfo placeholder_info 伴生素材
type ccPlaceholderInfo struct {
	ID        string `json:"id"`
	Type      string `json:"type"`      // "placeholder_info"
	MetaType  string `json:"meta_type"` // "none"
	ResPath   string `json:"res_path"`
	ResText   string `json:"res_text"`
	ErrorPath string `json:"error_path"`
	ErrorText string `json:"error_text"`
}

// ccCanvasMaterial canvas_color 伴生素材（video segment 专用）
type ccCanvasMaterial struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`           // "canvas_color"
	Color          string  `json:"color"`
	Blur           float64 `json:"blur"`
	Image          string  `json:"image"`
	AlbumImage     string  `json:"album_image"`
	ImageID        string  `json:"image_id"`
	ImageName      string  `json:"image_name"`
	SourcePlatform int     `json:"source_platform"`
	TeamID         string  `json:"team_id"`
}

// ccSoundChannelMapping sound_channel_mapping 伴生素材
type ccSoundChannelMapping struct {
	ID                  string `json:"id"`
	Type                string `json:"type"`
	AudioChannelMapping int    `json:"audio_channel_mapping"`
	IsConfigOpen        bool   `json:"is_config_open"`
}

// ccMaterialColor material_colors 伴生素材（video segment 专用）
type ccMaterialColor struct {
	ID               string        `json:"id"`
	IsColorClip      bool          `json:"is_color_clip"`
	IsGradient       bool          `json:"is_gradient"`
	SolidColor       string        `json:"solid_color"`
	GradientColors   []interface{} `json:"gradient_colors"`
	GradientPercents []interface{} `json:"gradient_percents"`
	GradientAngle    float64       `json:"gradient_angle"`
	Width            float64       `json:"width"`
	Height           float64       `json:"height"`
}

// ccVocalSeparation vocal_separation 伴生素材
type ccVocalSeparation struct {
	ID             string        `json:"id"`
	Type           string        `json:"type"`           // "vocal_separation"
	Choice         int           `json:"choice"`
	RemovedSounds  []interface{} `json:"removed_sounds"`
	TimeRange      interface{}   `json:"time_range"`     // null
	ProductionPath string        `json:"production_path"`
	FinalAlgorithm string        `json:"final_algorithm"`
	EnterFrom      string        `json:"enter_from"`
}

// ccAIBeats AI 节拍信息
type ccAIBeats struct {
	MelodyURL      string        `json:"melody_url"`
	MelodyPath     string        `json:"melody_path"`
	BeatsURL       string        `json:"beats_url"`
	BeatsPath      string        `json:"beats_path"`
	BeatSpeedInfos []interface{} `json:"beat_speed_infos"`
}

// ccBeatsMaterial beats 伴生素材（audio segment 专用）
type ccBeatsMaterial struct {
	ID                string      `json:"id"`
	Type              string      `json:"type"`               // "beats"
	EnableAIBeats     bool        `json:"enable_ai_beats"`
	Gear              int         `json:"gear"`               // 404
	GearCount         int         `json:"gear_count"`
	Mode              int         `json:"mode"`               // 404
	UserBeats         []interface{} `json:"user_beats"`
	UserDeleteAIBeats interface{} `json:"user_delete_ai_beats"` // null
	AIBeats           ccAIBeats   `json:"ai_beats"`
}

type ccClip struct {
	Alpha     float64     `json:"alpha"`
	Flip      ccFlip      `json:"flip"`
	Rotation  float64     `json:"rotation"`
	Scale     ccScale     `json:"scale"`
	Transform ccTransform `json:"transform"`
}

type ccTimeRange struct {
	Duration int64 `json:"duration"`
	Start    int64 `json:"start"`
}

type ccSegment struct {
	Clip              *ccClip     `json:"clip"`                // 音频轨道必须为 null；视频/文本轨道必须为非 null 对象
	ID                string      `json:"id"`
	KeyframeRefs      []string    `json:"keyframe_refs"`       // 不用 omitempty：nil 时必须输出 [] 而非 null
	ExtraMaterialRefs []string    `json:"extra_material_refs"` // 同上，CapCut 迭代 null 会崩溃
	MaterialID        string      `json:"material_id"`
	Reverse           bool        `json:"reverse"`
	Speed             float64     `json:"speed"`
	SourceTimerange   ccTimeRange `json:"source_timerange"`
	TargetTimerange   ccTimeRange `json:"target_timerange"`
	Type              string      `json:"type"`
	Visible           bool        `json:"visible"`
	Volume            float64     `json:"volume"`

	// --- 以下字段真实草稿必须有，缺失会导致 CapCut 崩溃或行为异常 ---
	RenderTimerange       ccTimeRange        `json:"render_timerange"`
	RenderIndex           int                `json:"render_index"`
	TrackRenderIndex      int                `json:"track_render_index"`
	EnableLut             bool               `json:"enable_lut"`
	EnableAdjust          bool               `json:"enable_adjust"`
	EnableHsl             bool               `json:"enable_hsl"`
	EnableColorCurves     bool               `json:"enable_color_curves"`
	EnableHslCurves       bool               `json:"enable_hsl_curves"`
	EnableColorWheels     bool               `json:"enable_color_wheels"`
	HdrSettings           ccHdrSettings      `json:"hdr_settings"`
	TrackAttribute        int                `json:"track_attribute"`
	IsPlaceholder         bool               `json:"is_placeholder"`
	UniformScale          ccUniformScale     `json:"uniform_scale"`
	IsLoop                bool               `json:"is_loop"`
	IsToneModify          bool               `json:"is_tone_modify"`
	IntensifiesAudio      bool               `json:"intensifies_audio"`
	Cartoon               bool               `json:"cartoon"`
	LastNonzeroVolume     float64            `json:"last_nonzero_volume"`
	Desc                  string             `json:"desc"`
	State                 int                `json:"state"`
	GroupID               string             `json:"group_id"`
	CommonKeyframes       []interface{}      `json:"common_keyframes"`    // CapCut 迭代此数组，null 崩溃
	CaptionInfo           interface{}        `json:"caption_info"`
	ResponsiveLayout      ccResponsiveLayout `json:"responsive_layout"`
	EnableSmartColorAdjust bool              `json:"enable_smart_color_adjust"`
	Source                string             `json:"source"`
	TemplateID            string             `json:"template_id"`
	TemplateScene         string             `json:"template_scene"`
	RawSegmentID          string             `json:"raw_segment_id"`
	LyricKeyframes        []interface{}      `json:"lyric_keyframes"`     // CapCut 迭代此数组，null 崩溃
	EnableVideoMask       bool               `json:"enable_video_mask"`
}

// MarshalJSON 确保:
//   - KeyframeRefs / ExtraMaterialRefs 始终序列化为 [] 而非 null（CapCut 迭代 null 崩溃）
//   - CommonKeyframes / LyricKeyframes 始终序列化为 [] 而非 null（CapCut 迭代 null 崩溃）
//   - Clip 为 nil 时输出 "clip":null（音频轨道要求），非空时输出对象
func (s ccSegment) MarshalJSON() ([]byte, error) {
	kfRefs := s.KeyframeRefs
	if kfRefs == nil {
		kfRefs = []string{}
	}
	extRefs := s.ExtraMaterialRefs
	if extRefs == nil {
		extRefs = []string{}
	}
	commonKFs := s.CommonKeyframes
	if commonKFs == nil {
		commonKFs = []interface{}{}
	}
	lyricKFs := s.LyricKeyframes
	if lyricKFs == nil {
		lyricKFs = []interface{}{}
	}
	type seg struct {
		Clip                   *ccClip            `json:"clip"`
		ID                     string             `json:"id"`
		KeyframeRefs           []string           `json:"keyframe_refs"`
		ExtraMaterialRefs      []string           `json:"extra_material_refs"`
		MaterialID             string             `json:"material_id"`
		Reverse                bool               `json:"reverse"`
		Speed                  float64            `json:"speed"`
		SourceTimerange        ccTimeRange        `json:"source_timerange"`
		TargetTimerange        ccTimeRange        `json:"target_timerange"`
		Type                   string             `json:"type"`
		Visible                bool               `json:"visible"`
		Volume                 float64            `json:"volume"`
		RenderTimerange        ccTimeRange        `json:"render_timerange"`
		RenderIndex            int                `json:"render_index"`
		TrackRenderIndex       int                `json:"track_render_index"`
		EnableLut              bool               `json:"enable_lut"`
		EnableAdjust           bool               `json:"enable_adjust"`
		EnableHsl              bool               `json:"enable_hsl"`
		EnableColorCurves      bool               `json:"enable_color_curves"`
		EnableHslCurves        bool               `json:"enable_hsl_curves"`
		EnableColorWheels      bool               `json:"enable_color_wheels"`
		HdrSettings            ccHdrSettings      `json:"hdr_settings"`
		TrackAttribute         int                `json:"track_attribute"`
		IsPlaceholder          bool               `json:"is_placeholder"`
		UniformScale           ccUniformScale     `json:"uniform_scale"`
		IsLoop                 bool               `json:"is_loop"`
		IsToneModify           bool               `json:"is_tone_modify"`
		IntensifiesAudio       bool               `json:"intensifies_audio"`
		Cartoon                bool               `json:"cartoon"`
		LastNonzeroVolume      float64            `json:"last_nonzero_volume"`
		Desc                   string             `json:"desc"`
		State                  int                `json:"state"`
		GroupID                string             `json:"group_id"`
		CommonKeyframes        []interface{}      `json:"common_keyframes"`
		CaptionInfo            interface{}        `json:"caption_info"`
		ResponsiveLayout       ccResponsiveLayout `json:"responsive_layout"`
		EnableSmartColorAdjust bool               `json:"enable_smart_color_adjust"`
		Source                 string             `json:"source"`
		TemplateID             string             `json:"template_id"`
		TemplateScene          string             `json:"template_scene"`
		RawSegmentID           string             `json:"raw_segment_id"`
		LyricKeyframes         []interface{}      `json:"lyric_keyframes"`
		EnableVideoMask        bool               `json:"enable_video_mask"`
	}
	return json.Marshal(seg{
		Clip:                   s.Clip,
		ID:                     s.ID,
		KeyframeRefs:           kfRefs,
		ExtraMaterialRefs:      extRefs,
		MaterialID:             s.MaterialID,
		Reverse:                s.Reverse,
		Speed:                  s.Speed,
		SourceTimerange:        s.SourceTimerange,
		TargetTimerange:        s.TargetTimerange,
		Type:                   s.Type,
		Visible:                s.Visible,
		Volume:                 s.Volume,
		RenderTimerange:        s.RenderTimerange,
		RenderIndex:            s.RenderIndex,
		TrackRenderIndex:       s.TrackRenderIndex,
		EnableLut:              s.EnableLut,
		EnableAdjust:           s.EnableAdjust,
		EnableHsl:              s.EnableHsl,
		EnableColorCurves:      s.EnableColorCurves,
		EnableHslCurves:        s.EnableHslCurves,
		EnableColorWheels:      s.EnableColorWheels,
		HdrSettings:            s.HdrSettings,
		TrackAttribute:         s.TrackAttribute,
		IsPlaceholder:          s.IsPlaceholder,
		UniformScale:           s.UniformScale,
		IsLoop:                 s.IsLoop,
		IsToneModify:           s.IsToneModify,
		IntensifiesAudio:       s.IntensifiesAudio,
		Cartoon:                s.Cartoon,
		LastNonzeroVolume:      s.LastNonzeroVolume,
		Desc:                   s.Desc,
		State:                  s.State,
		GroupID:                s.GroupID,
		CommonKeyframes:        commonKFs,
		CaptionInfo:            s.CaptionInfo,
		ResponsiveLayout:       s.ResponsiveLayout,
		EnableSmartColorAdjust: s.EnableSmartColorAdjust,
		Source:                 s.Source,
		TemplateID:             s.TemplateID,
		TemplateScene:          s.TemplateScene,
		RawSegmentID:           s.RawSegmentID,
		LyricKeyframes:         lyricKFs,
		EnableVideoMask:        s.EnableVideoMask,
	})
}

// ccTransitionMaterial 转场特效素材
type ccTransitionMaterial struct {
	Category string `json:"category"` // "transition"
	Duration int64  `json:"duration"` // microseconds (500000 = 0.5s)
	Effect   string `json:"effect"`   // "dissolve" / "fade_to_black" / "fade_to_white" / "wipe_right" / "fade"
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // "transition"
}

type ccTrack struct {
	Attribute     int         `json:"attribute"`
	Flag          int         `json:"flag"`
	ID            string      `json:"id"`
	IsDefaultName bool        `json:"is_default_name"`
	Name          string      `json:"name"`
	Segments      []ccSegment `json:"segments"`
	Type          string      `json:"type"`
}

type ccCrop struct {
	LowerLeftX  float64 `json:"lower_left_x"`
	LowerLeftY  float64 `json:"lower_left_y"`
	LowerRightX float64 `json:"lower_right_x"`
	LowerRightY float64 `json:"lower_right_y"`
	UpperLeftX  float64 `json:"upper_left_x"`
	UpperLeftY  float64 `json:"upper_left_y"`
	UpperRightX float64 `json:"upper_right_x"`
	UpperRightY float64 `json:"upper_right_y"`
}

type ccVideoMaterial struct {
	CartoonPath       string  `json:"cartoon_path"`
	CheckFlag         int     `json:"check_flag"`
	CropScale         float64 `json:"crop_scale"`
	Crop              ccCrop  `json:"crop"`
	Duration          int64   `json:"duration"`
	ExtraInfo         string  `json:"extra_info"`
	HasAudio          bool    `json:"has_audio"`
	Height            int     `json:"height"`
	ID                string  `json:"id"`
	ImportTime        int64   `json:"import_time"`
	ImportTimeUs      int64   `json:"import_time_us"`      // 部分版本 CapCut 需要此字段，缺失时可能崩溃
	LocalMaterialPath string  `json:"local_material_path"` // CapCut 加载素材时访问此字段；缺失时行为未定义
	LocalVideoPath    string  `json:"local_video_path"`    // 同上；本地草稿留空字符串，CapCut 从 Path 查找
	MaterialID        string  `json:"material_id"`
	Path              string  `json:"path"`
	SourcePlatform    int     `json:"source_platform"`
	Type              string  `json:"type"` // "photo" / "video"
	Width             int     `json:"width"`
}

// ccAudioMaterial 音频素材（配音 / BGM）
type ccAudioMaterial struct {
	CheckFlag      int    `json:"check_flag"`
	Duration       int64  `json:"duration"`
	ExtraInfo      string `json:"extra_info"`
	FilePath       string `json:"file_Path"` // CapCut 格式：大写 P
	ID             string `json:"id"`
	MaterialID     string `json:"material_id"` // 必须与 ID 相同；CapCut 按此字段索引素材，缺失时 segment 找不到对应素材导致崩溃
	Name           string `json:"name"`
	SourcePlatform int    `json:"source_platform"`
	Type           string `json:"type"` // "extract_music" = 配音; "music" = BGM
}

// ccTextMaterial 字幕/文字素材；content 是 JSON 字符串
// 注意：剪映要求 id 与 material_id 同值，否则 segment.material_id 引用会失效导致字幕不显示。
type ccTextMaterial struct {
	CheckFlag  int    `json:"check_flag"`
	Content    string `json:"content"`
	ID         string `json:"id"`
	IsSubtitle bool   `json:"is_subtitle"`
	MaterialID string `json:"material_id"` // 必须与 ID 相同
	Name       string `json:"name"`
	Type       string `json:"type"` // "text"
}

type ccMaterials struct {
	AudioFades           []interface{}          `json:"audio_fades"`            // CapCut 6.x+ 必需字段
	Audios               []ccAudioMaterial      `json:"audios"`
	Beats                []ccBeatsMaterial      `json:"beats"`                  // 真实类型（audio segment 伴生素材）
	Canvases             []ccCanvasMaterial     `json:"canvases"`               // 真实类型（video segment 伴生素材）
	Chromas              []interface{}          `json:"chromas"`
	ColorCurves          []interface{}          `json:"color_curves"`
	Filters              []interface{}          `json:"filters"`
	GreenScreens         []interface{}          `json:"green_screens"`
	Masks                []interface{}          `json:"masks"`
	MaterialAnimations   []interface{}          `json:"material_animations"`
	MaterialColors       []ccMaterialColor      `json:"material_colors"`        // video segment 伴生素材
	PlaceholderInfos     []ccPlaceholderInfo    `json:"placeholder_infos"`      // 真实类型；CapCut 遍历此数组，null 崩溃
	Shapes               []interface{}          `json:"shapes"`
	SoundChannelMappings []ccSoundChannelMapping `json:"sound_channel_mappings"` // 真实类型
	Speeds               []ccSpeedMaterial      `json:"speeds"`                 // 真实字段名为 speeds（复数）
	Stickers             []interface{}          `json:"stickers"`
	Texts                []ccTextMaterial       `json:"texts"`
	TextTemplates        []interface{}          `json:"text_templates"`
	Transitions          []ccTransitionMaterial `json:"transitions"`
	VideoEffects         []interface{}          `json:"video_effects"`
	Videos               []ccVideoMaterial      `json:"videos"`
	VoiceEffects         []interface{}          `json:"voice_effects"`
	VocalSeparations     []ccVocalSeparation    `json:"vocal_separations"`      // 真实类型
}

type ccCanvasConfig struct {
	Height     int         `json:"height"`
	Ratio      string      `json:"ratio"`      // 字符串形式："16:9"/"9:16"/"1:1"/"4:5"（CapCut 6.x+ 规范要求）
	Width      int         `json:"width"`
	Background interface{} `json:"background"` // null（真实草稿要求此字段存在）
}

// ccPlatform 标识草稿所属平台（CapCut 国际版 app_source="cc"，剪映 app_source="lv"）。
// 必须为对象，不能是字符串——CapCut 解析时会断言类型，字符串值导致 JSON 反序列化失败。
type ccPlatform struct {
	AppSource  string `json:"app_source"`  // "cc" = CapCut 国际版; "lv" = 剪映
	AppVersion string `json:"app_version"` // 任意合法版本号，如 "5.0.0"
	AppID      int    `json:"app_id"`      // CapCut International = 359289
	OS         string `json:"os"`          // "mac" / "windows"
	OsVersion  string `json:"os_version"`  // 操作系统版本，如 "11.7.11"
	DeviceID   string `json:"device_id"`
	HardDiskID string `json:"hard_disk_id"`
	MacAddress string `json:"mac_address"`
}

type ccDraftContent struct {
	CanvasConfig         ccCanvasConfig `json:"canvas_config"`
	CreateTime           int64          `json:"create_time"`
	Duration             int64          `json:"duration"`
	FPS                  float64        `json:"fps"`
	ID                   string         `json:"id"`
	Keyframes            ccKeyframes    `json:"keyframes"`
	LastModifiedPlatform ccPlatform     `json:"last_modified_platform"` // 必须为对象（真实草稿），CapCut 做 object 类型断言，字符串会崩溃
	Materials            ccMaterials    `json:"materials"`
	Name                 string         `json:"name"`
	NewVersion           string         `json:"new_version"`
	Platform             ccPlatform     `json:"platform"` // 对象，非字符串
	Relationships        []interface{}  `json:"relationships"`
	Tracks               []ccTrack      `json:"tracks"`
	UpdateTime           int64          `json:"update_time"`
	Version              int            `json:"version"` // 整数（真实草稿 360000），CapCut 做 int 类型断言，字符串会崩溃
}

type ccMetaInfo struct {
	DraftCover               string        `json:"draft_cover"`
	DraftFoldPath            string        `json:"draft_fold_path"`
	DraftID                  string        `json:"draft_id"`
	DraftIsAI                bool          `json:"draft_is_ai_shorts"`
	DraftIsArticleVideo      bool          `json:"draft_is_article_video_draft"`
	DraftIsInvisible         bool          `json:"draft_is_invisible"`
	DraftMaterials           []interface{} `json:"draft_materials"`
	DraftName                string        `json:"draft_name"`
	DraftNewVersion          string        `json:"draft_new_version"`
	DraftRootPath            string        `json:"draft_root_path"`
	DraftSegmentExtraInfo    []interface{} `json:"draft_segment_extra_info"`
	DraftTimelineMaterialsV2 []interface{} `json:"draft_timeline_materialsv2"`
	TmDraftCreate            int64         `json:"tm_draft_create"`
	TmDraftModify            int64         `json:"tm_draft_modified"`
	TmDuration               int64         `json:"tm_duration"`
}

// ExportResult 导出结果（适用于所有格式）
type ExportResult struct {
	Data        []byte
	Filename    string
	ContentType string
}


// aspectRatioDimensions 根据宽高比返回 (width, height)
func aspectRatioDimensions(ratio string) (int, int) {
	switch ratio {
	case "9:16":
		return 1080, 1920
	case "1:1":
		return 1080, 1080
	case "4:3":
		return 1440, 1080
	default: // 16:9
		return 1920, 1080
	}
}

// aspectRatioString 返回 CapCut canvas_config.ratio 所需的字符串形式。
// CapCut 6.x+ 规范要求 ratio 为字符串标签（"16:9"），而非 float。
func aspectRatioString(ratio string) string {
	switch ratio {
	case "9:16", "1:1", "4:5":
		return ratio
	case "4:3":
		return "4:3"
	default:
		return "16:9"
	}
}

// utf16Len 返回字符串的 UTF-16 编码单元数。
// P2-3: CapCut 文本 content 的 range 字段使用 UTF-16 单元偏移，
// 对于辅助平面字符（emoji）一个码点占两个 UTF-16 单元，len([]rune) 会算错。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2 // 辅助平面字符（emoji 等）占两个 UTF-16 单元
		} else {
			n++
		}
	}
	return n
}

// subtitleConfig 解析小说级字幕配置，返回渲染所需参数（novel 为 nil 时全用默认值）
type subtitleConfig struct {
	enabled  bool
	position string // bottom/center/top
	fontSize int
	color    string // hex #RRGGBB
	bgStyle  string // none/shadow/box
}

func newSubtitleConfig(novel *model.Novel) subtitleConfig {
	cfg := subtitleConfig{
		// 导出草稿时始终生成字幕轨道；novel.SubtitleEnabled 仅控制应用内实时渲染，
		// 不影响导出——用户可在剪映中自行删除字幕轨道。
		enabled:  true,
		position: "bottom",
		fontSize: 48,
		color:    "#FFFFFF",
		bgStyle:  "shadow",
	}
	if novel == nil {
		return cfg
	}
	// 保留小说级样式配置（位置/字号/颜色/背景），但不读取 enabled 字段
	vc := novel.VideoConf()
	if vc.SubtitlePosition != "" {
		cfg.position = vc.SubtitlePosition
	}
	if vc.SubtitleFontSize > 0 {
		cfg.fontSize = vc.SubtitleFontSize
	}
	if vc.SubtitleColor != "" {
		cfg.color = vc.SubtitleColor
	}
	if vc.SubtitleBgStyle != "" {
		cfg.bgStyle = vc.SubtitleBgStyle
	}
	return cfg
}

// hexToRGBA 将 #RRGGBB 解析为 0-1 范围的 RGBA
func hexToRGBA(hex string) (r, g, b float64) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) < 6 {
		return 1, 1, 1
	}
	parse := func(s string) float64 {
		var v int
		fmt.Sscanf(s, "%02x", &v)
		return float64(v) / 255.0
	}
	return parse(hex[0:2]), parse(hex[2:4]), parse(hex[4:6])
}

// subtitleTransformY 将位置名转为剪映 Y 轴偏移（归一化，正值=向上）
func subtitleTransformY(position string) float64 {
	switch position {
	case "top":
		return 0.78
	case "center":
		return 0.0
	default: // bottom
		return -0.78
	}
}

// sanitizeSubtitleText 清理字幕文本：
//   - 去除 C0 控制字符（\x00–\x1F，保留 \t \n），防止 CapCut JSON 解析异常
//   - 超过 500 字符截断，避免单字幕素材过大
func sanitizeSubtitleText(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= 500 {
			break
		}
		if r < 0x20 && r != '\t' && r != '\n' {
			continue // 丢弃控制字符
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// buildTextContent 构建剪映字幕素材 content JSON 字符串。
// 剪映要求 style 元素中必须包含 font / useLetterColor / strokes 字段；
// 缺少这些字段时剪映会静默跳过该文字素材，导致字幕不显示。
func buildTextContent(text string, cfg subtitleConfig) string {
	r, g, b := hexToRGBA(cfg.color)
	// 字体大小：剪映使用 pt 单位，约 px / 5
	sizePt := float64(cfg.fontSize) / 5.0

	style := map[string]interface{}{
		"range": []int{0, utf16Len(text)}, // P2-3: 使用 UTF-16 单元数（兼容 emoji 等辅助平面字符）
		"fill": map[string]interface{}{
			"alpha": 1.0,
			"content": map[string]interface{}{
				"render_index": 0,
				"solid": map[string]interface{}{
					"alpha": 1.0,
					"color": map[string]interface{}{"a": 1.0, "b": b, "g": g, "r": r},
				},
			},
			"type": "solid",
		},
		"size":           sizePt,
		"useLetterColor": false,
		"strokes":        []interface{}{},
		// font 为空时剪映使用默认字体，但字段本身必须存在
		"font": map[string]interface{}{
			"id":       "",
			"path":     "",
			"typeface": "",
		},
	}

	// 背景样式
	switch cfg.bgStyle {
	case "shadow":
		style["shadow"] = map[string]interface{}{
			"alpha": 0.7, "blur": 6.0, "distance": 3.0, "offset": 45.0,
			"color": map[string]interface{}{"a": 1.0, "b": 0.0, "g": 0.0, "r": 0.0},
		}
	case "box":
		style["background"] = map[string]interface{}{
			"alpha": 0.6,
			"color": map[string]interface{}{"a": 1.0, "b": 0.0, "g": 0.0, "r": 0.0},
			"round_radius": 4.0,
			"style":        2,
		}
	}

	obj := map[string]interface{}{
		"text":          text,
		"styles":        []interface{}{style},
		"textAlignType": "center",
		"typesetting":   0,
	}
	b2, _ := json.Marshal(obj)
	return string(b2)
}

// isHTTPURL 判断路径是否为 HTTP/HTTPS 在线 URL
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// capCutTransitionEffect 将分镜 Transition 值映射到剪映转场效果名称。
// 返回空字符串表示直切（无转场素材）。
func capCutTransitionEffect(t string) string {
	switch t {
	case "fade", "fade-in", "fade-out":
		return "fade"
	case "dissolve":
		return "dissolve"
	case "dip-black":
		return "fade_to_black"
	case "dip-white":
		return "fade_to_white"
	case "wipe":
		return "wipe_right"
	default:
		return "" // cut 及其他未知值 = 直切，无转场
	}
}

// ExportCapCutDraft 导出剪映草稿 ZIP（含视频/图片、配音、字幕轨道）
func (s *CapCutService) ExportCapCutDraft(video *model.Video, shots []*model.StoryboardShot, novel *model.Novel, bgmSegs []*model.VideoBGMSegment) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportCapCutDraft: videoID=%d title=%q shots=%d", video.ID, video.Title, len(shots))
	now := time.Now().Unix()
	draftID := uuid.New().String()
	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	subCfg := newSubtitleConfig(novel)
	ratio := video.AspectRatio
	if ratio == "" {
		ratio = "16:9"
	}
	width, height := aspectRatioDimensions(ratio)
	defaultCrop := ccCrop{
		LowerLeftX: 0, LowerLeftY: 1, LowerRightX: 1, LowerRightY: 1,
		UpperLeftX: 0, UpperLeftY: 0, UpperRightX: 1, UpperRightY: 0,
	}

	type mediaFile struct {
		filename string
		data     []byte
	}
	var mediaFiles []mediaFile

	// 四类轨道数据
	var videoMaterials []ccVideoMaterial
	videoSegments := []ccSegment{} // 直接初始化为非 nil 空切片：var 声明为 nil，拷贝进 track 后再赋值无法修正
	allKFGroups := []ccKeyframeGroup{} // 图片分镜的 Ken Burns 运镜关键帧

	var audioMaterials []ccAudioMaterial
	var audioSegments []ccSegment

	var sfxMaterials []ccAudioMaterial // 音效轨道（独立于配音轨道）
	var sfxSegments []ccSegment

	var textMaterials []ccTextMaterial
	var textSegments []ccSegment

	var transitionMaterials []ccTransitionMaterial // 转场素材

	// 伴生素材集合（每个 segment 对应若干条，CapCut 通过 extra_material_refs 引用）
	var speedsSlice        []ccSpeedMaterial
	var placeholderSlice   []ccPlaceholderInfo
	var canvasSlice        []ccCanvasMaterial
	var soundChannelSlice  []ccSoundChannelMapping
	var materialColorSlice []ccMaterialColor
	var vocalSepSlice      []ccVocalSeparation
	var beatsSlice         []ccBeatsMaterial

	// Bug2修复：按 shot_no 升序排列，确保视频/音频/字幕轨道顺序与分镜编号一致。
	// 数据库返回顺序不保证有序，直接遍历会导致配音顺序与画面顺序错位。
	sort.Slice(shots, func(i, j int) bool {
		return shots[i].ShotNo < shots[j].ShotNo
	})

	// P1-1: 优先使用项目封面，回退到第一张分镜图作为草稿封面缩略图
	var coverData []byte
	if novel != nil && novel.CoverImage != "" {
		coverData, _ = s.resolveMedia(novel.CoverImage)
	}
	if len(coverData) == 0 && len(shots) > 0 && shots[0].ImageURL != "" {
		coverData, _ = s.resolveMedia(shots[0].ImageURL)
	}

	// P2-4: track total loaded bytes; skip embeds beyond maxExportMediaBytes to avoid OOM
	var totalLoadedBytes int64

	var totalDuration int64

	for _, shot := range shots {
		durationMicros := int64(shot.Duration * 1_000_000)
		if durationMicros <= 0 {
			durationMicros = 5_000_000
		}
		startMicros := totalDuration

		// ── 1. 视频/图片素材 ──────────────────────────────────────
		mediaURL := shot.ImageURL
		isVideo := false
		if shot.VideoURL != "" {
			mediaURL = shot.VideoURL
			isVideo = true
		}

		ext := ".jpg"
		matType := "photo"
		if isVideo {
			ext = ".mp4"
			matType = "video"
		}

		vidMatID := uuid.New().String()
		vidFilename := fmt.Sprintf("%03d%s", shot.ShotNo, ext) // 数字序号命名，便于剪辑师识别

		// CapCut 的 path 字段只接受本地文件路径，不能写 HTTP URL（会导致崩溃）。
		// 下载媒体并嵌入 ZIP；path 只写文件名，CapCut 从草稿目录自动找到该文件。
		// 下载失败时 path 保持文件名，CapCut 显示"素材缺失"。
		vidPath := vidFilename
		if mediaURL != "" {
			if mediaData, mediaErr := s.resolveMedia(mediaURL); mediaErr == nil && len(mediaData) > 0 {
				if totalLoadedBytes+int64(len(mediaData)) > maxExportMediaBytes {
					logger.Printf("[ExportCapCutDraft] total media exceeds 2GiB, skipping embed for shot %d", shot.ShotNo)
				} else {
					totalLoadedBytes += int64(len(mediaData))
					mediaFiles = append(mediaFiles, mediaFile{filename: vidFilename, data: mediaData})
				}
			} else {
				logger.Printf("[ExportCapCutDraft] media load failed for shot %d url=%q (%v)", shot.ShotNo, mediaURL, mediaErr)
			}
		}

		matDuration := durationMicros
		if !isVideo {
			matDuration = 10_800_000_000 // 图片素材 duration 固定为 3 小时，CapCut 按 SourceTimerange 截取实际时长
		}
		videoMaterials = append(videoMaterials, ccVideoMaterial{
			CheckFlag:         63487,
			CropScale:         1,
			Crop:              defaultCrop,
			Duration:          matDuration,
			HasAudio:          isVideo && shot.AudioPath != "",
			Height:            height,
			ID:                vidMatID,
			ImportTime:        now,
			ImportTimeUs:      now,
			LocalMaterialPath: "",
			LocalVideoPath:    "",
			MaterialID:        vidMatID,
			Path:              vidPath,
			SourcePlatform:    0,
			Type:              matType,
			Width:             width,
		})

		segID := uuid.New().String()
		seg := ccSegment{
			Clip: &ccClip{Alpha: 1.0, Scale: ccScale{X: 1.0, Y: 1.0}},
			ID:              segID,
			MaterialID:      vidMatID,
			Speed:           1.0,
			SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
			TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
			Type:            "video",
			Visible:         true,
			Volume:          1.0,
		}
		// 填充真实草稿所需默认字段
		applyVideoSegmentDefaults(&seg)

		// 创建 video segment 伴生素材（6 个）并将 ID 写入 ExtraMaterialRefs
		// 顺序：speed, placeholder_info, canvas, sound_channel_mapping, material_color, vocal_separation
		spd := newSpeedMaterial()
		ph := newPlaceholderInfo()
		cv := newCanvasMaterial()
		sc := newSoundChannelMapping()
		mc := newMaterialColor()
		vs := newVocalSeparation()
		speedsSlice = append(speedsSlice, spd)
		placeholderSlice = append(placeholderSlice, ph)
		canvasSlice = append(canvasSlice, cv)
		soundChannelSlice = append(soundChannelSlice, sc)
		materialColorSlice = append(materialColorSlice, mc)
		vocalSepSlice = append(vocalSepSlice, vs)
		seg.ExtraMaterialRefs = append(seg.ExtraMaterialRefs, spd.ID, ph.ID, cv.ID, sc.ID, mc.ID, vs.ID)

		// P1-1: 仅对静态图片添加 Ken Burns 运镜关键帧。
		// 视频素材由 Kling/Seedance 已生成摄像机运动，叠加缩放/平移关键帧会产生双重抖动。
		if !isVideo {
			kfGroups := buildPhotoMotionKeyframes(segID, shot.CameraType, durationMicros, shot.ShotNo)
			for _, g := range kfGroups {
				seg.KeyframeRefs = append(seg.KeyframeRefs, g.ID)
			}
			allKFGroups = append(allKFGroups, kfGroups...)
		}
		// 转场特效：非直切时创建转场素材并挂载到当前 segment（追加到伴生素材引用末尾）
		if effect := capCutTransitionEffect(shot.Transition); effect != "" {
			transMatID := uuid.New().String()
			transitionMaterials = append(transitionMaterials, ccTransitionMaterial{
				Category: "transition",
				Duration: 500000, // 0.5s
				Effect:   effect,
				ID:       transMatID,
				Name:     effect,
				Type:     "transition",
			})
			seg.ExtraMaterialRefs = append(seg.ExtraMaterialRefs, transMatID)
		}
		videoSegments = append(videoSegments, seg)

		// ── 2. 配音音频素材 ───────────────────────────────────────
		// 优先 shot.AudioPath（合成后的整段音频）；
		// Bug-A修复：AudioPath 为空且有 VoiceSegments 时，逐段加入配音轨道（与 ExportBRollDraft 保持一致）。
		if shot.AudioPath != "" {
			audMatID := uuid.New().String()

			// Bug3修复：用实际音频时长替代 shot.Duration。
			// CapCut 根据 Material.Duration 和 SourceTimerange.Duration 解析音频播放范围；
			// 若声称时长 > 文件实际时长，CapCut 可能拉伸音频，导致音画不同步。
			actualAudioDur := durationMicros // fallback：读取失败时用视频段时长
			audPath := fmt.Sprintf("%03d_voice.mp3", shot.ShotNo)
			if data, err := s.resolveMedia(shot.AudioPath); err == nil && len(data) > 0 {
				ext := audioExtension(shot.AudioPath)
				audPath = fmt.Sprintf("%03d_voice%s", shot.ShotNo, ext)
				if totalLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					totalLoadedBytes += int64(len(data))
					mediaFiles = append(mediaFiles, mediaFile{filename: audPath, data: data})
				}
				if dur := parseAudioDurationMicros(data, ext); dur > 0 {
					actualAudioDur = dur
				}
			} else {
				logger.Printf("[ExportCapCutDraft] audio load failed shot %d url=%q: %v", shot.ShotNo, shot.AudioPath, err)
			}
			// SourceTimerange：告知剪映实际取用的音频长度，不超过视频段时长
			srcDur := actualAudioDur
			if srcDur > durationMicros {
				srcDur = durationMicros // 超出镜头时长的部分截断，避免溢出到下一镜头
			}

			audioMaterials = append(audioMaterials, ccAudioMaterial{
				CheckFlag:  1,
				Duration:   actualAudioDur, // 素材真实时长
				FilePath:   audPath,        // HTTP URL（CDN）或相对文件名（本地嵌入）
				ID:         audMatID,
				MaterialID: audMatID,
				Name:       fmt.Sprintf("shot_%03d_audio", shot.ShotNo),
				Type:       "extract_music",
			})

			// Bug修复：TargetTimerange.Duration 必须等于 SourceTimerange.Duration（srcDur）。
			// 若用 durationMicros（视频段时长）而 srcDur < durationMicros，
			// 剪映会将音频拉伸以填满时间槽，导致音频变慢、音画严重不同步。
			{
				// 创建 audio segment 伴生素材（5 个）：speed, placeholder, beats, sound_channel, vocal_sep
				audSpd := newSpeedMaterial()
				audPh := newPlaceholderInfo()
				audBts := newBeatsMaterial()
				audSc := newSoundChannelMapping()
				audVs := newVocalSeparation()
				speedsSlice = append(speedsSlice, audSpd)
				placeholderSlice = append(placeholderSlice, audPh)
				beatsSlice = append(beatsSlice, audBts)
				soundChannelSlice = append(soundChannelSlice, audSc)
				vocalSepSlice = append(vocalSepSlice, audVs)
				audSeg := ccSegment{
					Clip:            nil, // 音频轨道必须为 null
					ID:              uuid.New().String(),
					MaterialID:      audMatID,
					Speed:           1.0,
					SourceTimerange: ccTimeRange{Duration: srcDur, Start: 0},
					TargetTimerange: ccTimeRange{Duration: srcDur, Start: startMicros},
					Type:            "audio",
					Visible:         true,
					Volume:          1.0,
				}
				applyVideoSegmentDefaults(&audSeg)
				audSeg.ExtraMaterialRefs = append(audSeg.ExtraMaterialRefs, audSpd.ID, audPh.ID, audBts.ID, audSc.ID, audVs.ID)
				audioSegments = append(audioSegments, audSeg)
			}
		} else if s.segmentRepo != nil {
			// Bug-A：AudioPath 为空 → 回落到逐段 VoiceSegment，与 ExportBRollDraft 保持一致。
			// 多段 TTS 场景下 shot.AudioPath 通常为空（各段未合并），若此处不处理则配音轨道完全缺失。
			segs, segErr := s.segmentRepo.ListByShotID(shot.ID)
			if segErr == nil && len(segs) > 0 {
				segOffset := startMicros // 镜头内各段的时间轴起始偏移
				for _, seg := range segs {
					if seg.AudioPath == "" {
						continue
					}
					audMatID := uuid.New().String()
					actualSegDur := durationMicros // fallback：解析失败时用镜头时长
					audPath := fmt.Sprintf("%03d_seg%02d.mp3", shot.ShotNo, seg.SeqNo)
					if data, rdErr := s.resolveMedia(seg.AudioPath); rdErr == nil && len(data) > 0 {
						ext := audioExtension(seg.AudioPath)
						audPath = fmt.Sprintf("%03d_seg%02d%s", shot.ShotNo, seg.SeqNo, ext)
						if totalLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
							totalLoadedBytes += int64(len(data))
							mediaFiles = append(mediaFiles, mediaFile{filename: audPath, data: data})
						} else {
							logger.Printf("[ExportCapCutDraft] VoiceSegment: total media exceeds 2GiB, skipping shot %d seg %d", shot.ShotNo, seg.SeqNo)
						}
						if dur := parseAudioDurationMicros(data, ext); dur > 0 {
							actualSegDur = dur
						}
					}
					// 优先使用已存储的 DurationSecs（避免重复解析 PCM header）
					if seg.DurationSecs > 0 {
						actualSegDur = int64(seg.DurationSecs * 1_000_000)
					}
					// 不超出镜头剩余时长，避免溢出到下一镜头
					remaining := startMicros + durationMicros - segOffset
					srcDur := actualSegDur
					if srcDur > remaining {
						srcDur = remaining
					}
					if srcDur <= 0 {
						break // 镜头时长已用尽
					}
					audioMaterials = append(audioMaterials, ccAudioMaterial{
						CheckFlag:  1,
						Duration:   actualSegDur,
						FilePath:   audPath,
						ID:         audMatID,
						MaterialID: audMatID,
						Name:       fmt.Sprintf("shot_%03d_seg%02d", shot.ShotNo, seg.SeqNo),
						Type:       "extract_music",
					})
					// 创建 audio segment 伴生素材（5 个）
					vsSpd := newSpeedMaterial()
					vsPh := newPlaceholderInfo()
					vsBts := newBeatsMaterial()
					vsSc := newSoundChannelMapping()
					vsVs := newVocalSeparation()
					speedsSlice = append(speedsSlice, vsSpd)
					placeholderSlice = append(placeholderSlice, vsPh)
					beatsSlice = append(beatsSlice, vsBts)
					soundChannelSlice = append(soundChannelSlice, vsSc)
					vocalSepSlice = append(vocalSepSlice, vsVs)
					vsSegment := ccSegment{
						Clip:            nil, // 音频轨道必须为 null
						ID:              uuid.New().String(),
						MaterialID:      audMatID,
						Speed:           1.0,
						SourceTimerange: ccTimeRange{Duration: srcDur, Start: 0},
						TargetTimerange: ccTimeRange{Duration: srcDur, Start: segOffset},
						Type:            "audio",
						Visible:         true,
						Volume:          1.0,
					}
					applyVideoSegmentDefaults(&vsSegment)
					vsSegment.ExtraMaterialRefs = append(vsSegment.ExtraMaterialRefs, vsSpd.ID, vsPh.ID, vsBts.ID, vsSc.ID, vsVs.ID)
					audioSegments = append(audioSegments, vsSegment)
					segOffset += actualSegDur
				}
			}
		}

		// ── 3. 音效素材（SFX）────────────────────────────────────
		// 优先从 sfxItemRepo 读取多条 ShotSFXItem（含精确 StartOffset / DurationSecs / Volume）；
		// 若 repo 未注入或该镜头无条目，回退到 shot.SFXURL 单字段（向后兼容）。
		var sfxItems []*model.ShotSFXItem
		if s.sfxItemRepo != nil {
			if items, rerr := s.sfxItemRepo.ListByShotID(shot.ID); rerr == nil {
				for _, it := range items {
					if !it.Disabled && it.URL != "" {
						sfxItems = append(sfxItems, it)
					}
				}
			}
		}
		if len(sfxItems) == 0 && shot.SFXURL != "" {
			// 向后兼容：没有 item 记录时用 shot.SFXURL 构造一条虚拟 item
			sfxItems = []*model.ShotSFXItem{{URL: shot.SFXURL, Volume: shot.SFXVolume}}
		}
		for sfxIdx, sfxItem := range sfxItems {
			sfxMatID := uuid.New().String()
			sfxOffsetMicros := int64(sfxItem.StartOffset * 1_000_000) // 音效在镜头内的起始偏移
			sfxTimelineStart := startMicros + sfxOffsetMicros
			remaining := startMicros + durationMicros - sfxTimelineStart
			if remaining <= 0 {
				continue // 偏移已超出镜头结尾，跳过
			}

			actualSFXDur := remaining // fallback：未知时长用剩余镜头时间
			if sfxItem.DurationSecs > 0 {
				actualSFXDur = int64(sfxItem.DurationSecs * 1_000_000)
			}

			seqLabel := sfxIdx + 1
			sfxPath := fmt.Sprintf("%03d_sfx%02d.mp3", shot.ShotNo, seqLabel)
			if data, err := s.resolveMedia(sfxItem.URL); err == nil && len(data) > 0 {
				ext := audioExtension(sfxItem.URL)
				sfxPath = fmt.Sprintf("%03d_sfx%02d%s", shot.ShotNo, seqLabel, ext)
				if totalLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					totalLoadedBytes += int64(len(data))
					mediaFiles = append(mediaFiles, mediaFile{filename: sfxPath, data: data})
				} else {
					logger.Printf("[ExportCapCutDraft] SFX: total media exceeds 2GiB, skipping shot %d sfx %d", shot.ShotNo, seqLabel)
				}
				if parsed := parseAudioDurationMicros(data, ext); parsed > 0 {
					actualSFXDur = parsed
				}
			} else {
				logger.Printf("[ExportCapCutDraft] SFX load failed for shot %d sfx %d url=%q: %v", shot.ShotNo, seqLabel, sfxItem.URL, err)
			}

			sfxSrcDur := actualSFXDur
			if sfxSrcDur > remaining {
				sfxSrcDur = remaining // 不超出镜头末尾
			}

			sfxVol := sfxItem.Volume
			if sfxVol <= 0 {
				sfxVol = 0.4
				if shot.Dialogue != "" {
					sfxVol = 0.2
				} else if shot.AudioPath != "" {
					sfxVol = 0.3
				}
			}

			sfxMaterials = append(sfxMaterials, ccAudioMaterial{
				CheckFlag:  1,
				Duration:   actualSFXDur,
				FilePath:   sfxPath,
				ID:         sfxMatID,
				MaterialID: sfxMatID,
				Name:       fmt.Sprintf("shot_%03d_sfx%02d", shot.ShotNo, seqLabel),
				Type:       "audio_effect",
			})
			// SFX segment 伴生素材（5 个）：speed, placeholder, beats, sound_channel, vocal_sep
			sfxSpd := newSpeedMaterial()
			sfxPh := newPlaceholderInfo()
			sfxBts := newBeatsMaterial()
			sfxSc := newSoundChannelMapping()
			sfxVs := newVocalSeparation()
			speedsSlice = append(speedsSlice, sfxSpd)
			placeholderSlice = append(placeholderSlice, sfxPh)
			beatsSlice = append(beatsSlice, sfxBts)
			soundChannelSlice = append(soundChannelSlice, sfxSc)
			vocalSepSlice = append(vocalSepSlice, sfxVs)
			sfxSeg := ccSegment{
				Clip:            nil, // 音频轨道必须为 null
				ID:              uuid.New().String(),
				MaterialID:      sfxMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: sfxSrcDur, Start: 0},
				TargetTimerange: ccTimeRange{Duration: sfxSrcDur, Start: sfxTimelineStart},
				Type:            "audio",
				Visible:         true,
				Volume:          sfxVol,
			}
			applyVideoSegmentDefaults(&sfxSeg)
			sfxSeg.ExtraMaterialRefs = append(sfxSeg.ExtraMaterialRefs, sfxSpd.ID, sfxPh.ID, sfxBts.ID, sfxSc.ID, sfxVs.ID)
			sfxSegments = append(sfxSegments, sfxSeg)
		}

		// ── 4. 字幕文字素材 ───────────────────────────────────────
		// 文本优先级：角色台词 > 旁白文案 > 英文画面描述（仅兼容旧分镜数据）
		subtitleText := shot.Dialogue
		if subtitleText == "" {
			subtitleText = shot.Narration // 旁白文案（中文，新数据首选）
		}
		if subtitleText == "" {
			subtitleText = shot.Description // 兜底：旧数据中 Description 可能是中文描述
		}
		if subtitleText != "" && subCfg.enabled {
			txtMatID := uuid.New().String()
			subtitleText = sanitizeSubtitleText(subtitleText)

			textMaterials = append(textMaterials, ccTextMaterial{
				CheckFlag:  7,
				Content:    buildTextContent(subtitleText, subCfg),
				ID:         txtMatID,
				IsSubtitle: true,
				MaterialID: txtMatID, // 必须与 ID 相同，否则 segment.material_id 引用失效
				Name:       fmt.Sprintf("shot_%03d_subtitle", shot.ShotNo),
				Type:       "text",
			})

			// text segment 伴生素材（3 个）：speed, placeholder, sound_channel
			txtSpd := newSpeedMaterial()
			txtPh := newPlaceholderInfo()
			txtSc := newSoundChannelMapping()
			speedsSlice = append(speedsSlice, txtSpd)
			placeholderSlice = append(placeholderSlice, txtPh)
			soundChannelSlice = append(soundChannelSlice, txtSc)
			txtSeg := ccSegment{
				Clip: &ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
					Transform: ccTransform{Y: subtitleTransformY(subCfg.position)},
				},
				ID:              uuid.New().String(),
				MaterialID:      txtMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
				TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
				Type:            "text",
				Visible:         true,
				Volume:          0,
			}
			applyVideoSegmentDefaults(&txtSeg)
			txtSeg.ExtraMaterialRefs = append(txtSeg.ExtraMaterialRefs, txtSpd.ID, txtPh.ID, txtSc.ID)
			textSegments = append(textSegments, txtSeg)
		}

		totalDuration += durationMicros
	}

	// 构造轨道列表（视频轨道始终存在；音频/字幕轨道有数据才加入）
	tracks := []ccTrack{
		{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Segments:      videoSegments,
			Type:          "video",
		},
	}
	if len(audioSegments) > 0 {
		tracks = append(tracks, ccTrack{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Name:          "配音",
			Segments:      audioSegments,
			Type:          "audio",
		})
	}
	if len(sfxSegments) > 0 {
		tracks = append(tracks, ccTrack{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Name:          "音效",
			Segments:      sfxSegments,
			Type:          "audio",
		})
	}
	if len(textSegments) > 0 {
		tracks = append(tracks, ccTrack{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Name:          "字幕",
			Segments:      textSegments,
			Type:          "text",
		})
	}

	audios := append(audioMaterials, sfxMaterials...)

	// ── BGM 轨道 ──────────────────────────────────────────────────────────────
	if len(bgmSegs) > 0 {
		// shot 已按 shot_no 升序排列，构建两张快查表（O(1) 查找，避免 O(N²) 嵌套）
		shotStartMap := make(map[int]int64, len(shots))
		shotEndMap   := make(map[int]int64, len(shots))
		var acc int64
		for _, sh := range shots {
			shotStartMap[sh.ShotNo] = acc
			dur := int64(sh.Duration * 1_000_000)
			if dur <= 0 {
				dur = 5_000_000
			}
			shotEndMap[sh.ShotNo] = acc + dur
			acc += dur
		}

		var bgmSegments []ccSegment
		bgmIndex := 0 // BGM 文件序号（用于数字顺序命名）
		for _, bs := range bgmSegs {
			if bs.Disabled || bs.URL == "" {
				continue
			}
			startMicros, ok := shotStartMap[bs.StartShotNo]
			if !ok {
				continue
			}
			// BGM 结束时间 = EndShotNo 镜头的末尾时刻（O(1) 查表）
			endMicros := totalDuration
			if end, ok2 := shotEndMap[bs.EndShotNo]; ok2 {
				endMicros = end
			}
			segDur := endMicros - startMicros
			if segDur <= 0 {
				continue
			}

			bgmIndex++
			bgmMatID := uuid.New().String()
			bgmFilename := fmt.Sprintf("bgm_%03d.mp3", bgmIndex) // 数字序号命名
			bgmPath := bgmFilename
			bgmActualDur := segDur // fallback，无法 probe 时用时间轴跨度
			if data, err := s.resolveMedia(bs.URL); err == nil && len(data) > 0 {
				ext := audioExtension(bs.URL)
				bgmFilename = fmt.Sprintf("bgm_%03d%s", bgmIndex, ext)
				bgmPath = bgmFilename
				if totalLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					totalLoadedBytes += int64(len(data))
					mediaFiles = append(mediaFiles, mediaFile{filename: bgmFilename, data: data})
				} else {
					logger.Printf("[ExportCapCutDraft] BGM: total media exceeds 2GiB, skipping embed for shot %d", bs.StartShotNo)
				}
				if dur := parseAudioDurationMicros(data, ext); dur > 0 {
					bgmActualDur = dur
				}
			} else {
				logger.Printf("[ExportCapCutDraft] BGM load failed for shot %d url=%q: %v", bs.StartShotNo, bs.URL, err)
			}

			vol := bs.Volume
			if vol <= 0 {
				vol = 0.3
			}

			audios = append(audios, ccAudioMaterial{
				CheckFlag:  1,
				Duration:   bgmActualDur, // P1-2: 实际文件时长，而非时间轴跨度
				FilePath:   bgmPath,
				ID:         bgmMatID,
				MaterialID: bgmMatID,
				Name:       bs.TrackName,
				Type:       "music",
			})
			// P0-2: SourceTimerange 不能超出文件实际时长，否则 CapCut 读取 EOF 后行为未定义（静默/循环/崩溃）
			bgmSrcDur := segDur
			if bgmActualDur > 0 && bgmActualDur < segDur {
				bgmSrcDur = bgmActualDur
			}
			// BGM segment 伴生素材（5 个）：speed, placeholder, beats, sound_channel, vocal_sep
			bgmSpd := newSpeedMaterial()
			bgmPh := newPlaceholderInfo()
			bgmBts := newBeatsMaterial()
			bgmSc := newSoundChannelMapping()
			bgmVs := newVocalSeparation()
			speedsSlice = append(speedsSlice, bgmSpd)
			placeholderSlice = append(placeholderSlice, bgmPh)
			beatsSlice = append(beatsSlice, bgmBts)
			soundChannelSlice = append(soundChannelSlice, bgmSc)
			vocalSepSlice = append(vocalSepSlice, bgmVs)
			bgmSeg := ccSegment{
				Clip:            nil, // 音频轨道必须为 null
				ID:              uuid.New().String(),
				MaterialID:      bgmMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: bgmSrcDur, Start: 0},
				TargetTimerange: ccTimeRange{Duration: segDur, Start: startMicros},
				Type:            "audio",
				Visible:         true,
				Volume:          vol,
			}
			applyVideoSegmentDefaults(&bgmSeg)
			bgmSeg.ExtraMaterialRefs = append(bgmSeg.ExtraMaterialRefs, bgmSpd.ID, bgmPh.ID, bgmBts.ID, bgmSc.ID, bgmVs.ID)
			bgmSegments = append(bgmSegments, bgmSeg)
		}
		if len(bgmSegments) > 0 {
			tracks = append(tracks, ccTrack{
				Attribute:     0,
				Flag:          0,
				ID:            uuid.New().String(),
				IsDefaultName: true,
				Name:          "背景音乐",
				Segments:      bgmSegments,
				Type:          "audio",
			})
		}
	}
	if audios == nil {
		audios = []ccAudioMaterial{}
	}
	if textMaterials == nil {
		textMaterials = []ccTextMaterial{}
	}
	if transitionMaterials == nil {
		transitionMaterials = []ccTransitionMaterial{}
	}
	if videoMaterials == nil {
		videoMaterials = []ccVideoMaterial{}
	}
	// videoSegments nil → "segments":null → CapCut 迭代崩溃（与 keyframe_refs 同理）
	if videoSegments == nil {
		videoSegments = []ccSegment{}
	}
	// 确保伴生素材切片不为 nil（零条 segment 时切片可能为 nil）
	if speedsSlice == nil {
		speedsSlice = []ccSpeedMaterial{}
	}
	if placeholderSlice == nil {
		placeholderSlice = []ccPlaceholderInfo{}
	}
	if canvasSlice == nil {
		canvasSlice = []ccCanvasMaterial{}
	}
	if soundChannelSlice == nil {
		soundChannelSlice = []ccSoundChannelMapping{}
	}
	if materialColorSlice == nil {
		materialColorSlice = []ccMaterialColor{}
	}
	if vocalSepSlice == nil {
		vocalSepSlice = []ccVocalSeparation{}
	}
	if beatsSlice == nil {
		beatsSlice = []ccBeatsMaterial{}
	}

	content := ccDraftContent{
		CanvasConfig:         ccCanvasConfig{Height: height, Ratio: aspectRatioString(ratio), Width: width},
		CreateTime:           now,
		Duration:             totalDuration,
		FPS:                  24.0,
		ID:                   draftID,
		Keyframes:            ccKeyframes{Adjusts: []interface{}{}, Audios: []interface{}{}, ColorWheels: []interface{}{}, Effects: []interface{}{}, Filters: []interface{}{}, Handwrites: []interface{}{}, SpeedStickers: []interface{}{}, Stickers: []interface{}{}, Texts: []interface{}{}, Videos: allKFGroups, VocalSounds: []interface{}{}},
		LastModifiedPlatform: ccPlatform{AppSource: "cc", AppVersion: "5.0.0", OS: "mac"}, // 必须为对象（真实草稿）
		Materials: ccMaterials{
			AudioFades:           []interface{}{},
			Audios:               audios,
			Beats:                beatsSlice,
			Canvases:             canvasSlice,
			Chromas:              []interface{}{},
			ColorCurves:          []interface{}{},
			Filters:              []interface{}{},
			GreenScreens:         []interface{}{},
			Masks:                []interface{}{},
			MaterialAnimations:   []interface{}{},
			MaterialColors:       materialColorSlice,
			PlaceholderInfos:     placeholderSlice,
			Shapes:               []interface{}{},
			SoundChannelMappings: soundChannelSlice,
			Speeds:               speedsSlice,
			Stickers:             []interface{}{},
			Texts:                textMaterials,
			TextTemplates:        []interface{}{},
			Transitions:          transitionMaterials,
			VideoEffects:         []interface{}{},
			Videos:               videoMaterials,
			VoiceEffects:         []interface{}{},
			VocalSeparations:     vocalSepSlice,
		},
		Name:          video.Title,
		NewVersion:    "110.0.0",
		Platform:      ccPlatform{AppSource: "cc", AppVersion: "5.0.0", OS: "mac"},
		Relationships: []interface{}{},
		Tracks:        tracks,
		UpdateTime:    now,
		Version:       360000, // 整数（真实草稿），CapCut 做 int 类型断言
	}

	// P1-1: 有封面图时在 meta 中引用，否则留空（避免 CapCut 显示损坏图标）
	draftCoverName := ""
	if len(coverData) > 0 {
		draftCoverName = "cover.jpg"
	}
	meta := ccMetaInfo{
		DraftCover:               draftCoverName,
		DraftFoldPath:            "",
		DraftID:                  draftID,
		DraftIsAI:                false,
		DraftIsArticleVideo:      false,
		DraftIsInvisible:         false,
		DraftMaterials:           []interface{}{},
		DraftName:                video.Title,
		DraftNewVersion:          "110.0.0", // P2-2
		DraftRootPath:            "",
		DraftSegmentExtraInfo:    []interface{}{},
		DraftTimelineMaterialsV2: []interface{}{},
		TmDraftCreate:            now,
		TmDraftModify:            now,
		TmDuration:               totalDuration,
	}

	contentJSON, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		logger.Printf("[CapCutService] ExportCapCutDraft: marshal draft content failed: %v", err)
		return nil, fmt.Errorf("marshal draft content: %w", err)
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		logger.Printf("[CapCutService] ExportCapCutDraft: marshal draft meta failed: %v", err)
		return nil, fmt.Errorf("marshal draft meta: %w", err)
	}

	// 构建 ZIP（写入临时文件，避免所有媒体数据同时在内存中双份缓冲）
	zipFile, err := os.CreateTemp("", "inkframe-capcut-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp zip file: %w", err)
	}
	zipFilePath := zipFile.Name()
	defer os.Remove(zipFilePath)

	zw := zip.NewWriter(zipFile)
	writeZip := func(name string, data []byte) error {
		w, e := zw.Create(name)
		if e != nil {
			return e
		}
		_, e = w.Write(data)
		return e
	}

	prefix := projectName + "/"
	if err := writeZip(prefix+"draft_content.json", contentJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_content.json: %w", err)
	}
	// macOS CapCut International 读取 draft_info.json，Windows 读取 draft_content.json；两个文件内容相同
	if err := writeZip(prefix+"draft_info.json", contentJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_info.json: %w", err)
	}
	if err := writeZip(prefix+"draft_meta_info.json", metaJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_meta_info.json: %w", err)
	}

	// draft_virtual_store.json — 部分版本的剪映需要此文件（资源索引），缺失时草稿可能无法识别
	virtualStoreJSON, _ := json.Marshal(map[string]interface{}{"sub_store": map[string]interface{}{}})
	if err := writeZip(prefix+"draft_virtual_store.json", virtualStoreJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_virtual_store.json: %w", err)
	}

	// P1-1: 封面图（草稿列表缩略图）
	if len(coverData) > 0 {
		if err := writeZip(prefix+"cover.jpg", coverData); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write cover.jpg: %w", err)
		}
	}

	// 媒体文件放在草稿根目录（与剪映真实草稿格式一致），而非 media/ 子目录
	for _, mf := range mediaFiles {
		if err := writeZip(prefix+mf.filename, mf.data); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write media file %s: %w", mf.filename, err)
		}
	}

	// SRT 字幕文件（通用格式，可在剪映/其他播放器/字幕工具中独立使用）
	if srtContent := buildSRTSubtitles(shots); srtContent != "" {
		if err := writeZip(prefix+"subtitle.srt", []byte(srtContent)); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write subtitle.srt: %w", err)
		}
	}

	// README.txt — 导入说明（剪映不直接打开 ZIP，需手动放入草稿目录）
	if err := writeZip(prefix+"README.txt", []byte(buildCapCutReadme(projectName))); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write README.txt: %w", err)
	}

	if err := zw.Close(); err != nil {
		zipFile.Close()
		logger.Printf("[CapCutService] ExportCapCutDraft: close zip failed: %v", err)
		return nil, fmt.Errorf("close zip: %w", err)
	}
	if err := zipFile.Close(); err != nil {
		return nil, fmt.Errorf("close zip file: %w", err)
	}

	zipData, err := os.ReadFile(zipFilePath)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}

	const zipWarnThreshold = 500 * 1024 * 1024 // 500 MB
	if len(zipData) > zipWarnThreshold {
		logger.Printf("[CapCutService] ExportCapCutDraft WARNING: ZIP size %d bytes exceeds %d MB; consider reducing shot count or media quality", len(zipData), zipWarnThreshold/1024/1024)
	}
	result := &ExportResult{
		Data:        zipData,
		Filename:    projectName + ".zip",
		ContentType: "application/zip",
	}
	logger.Printf("[CapCutService] ExportCapCutDraft done: filename=%s size=%d", result.Filename, len(result.Data))
	return result, nil
}

// ExportBRollDraft 导出 B 剪草稿 ZIP（静态图片 + 配音轨 + 字幕轨 + 分镜注释轨，无 Ken Burns / 音效 / BGM）
// 用途：剪辑师二剪参考稿，镜头顶部叠字注明分镜编号和描述，方便人工替换素材。
func (s *CapCutService) ExportBRollDraft(video *model.Video, shots []*model.StoryboardShot, novel *model.Novel) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportBRollDraft: videoID=%d shots=%d", video.ID, len(shots))
	now := time.Now().Unix()
	draftID := uuid.New().String()
	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	subCfg := newSubtitleConfig(novel)
	annCfg := subtitleConfig{enabled: true, position: "top", fontSize: 32, color: "#AAAAAA", bgStyle: "none"}

	ratio := video.AspectRatio
	if ratio == "" {
		ratio = "16:9"
	}
	width, height := aspectRatioDimensions(ratio)
	defaultCrop := ccCrop{
		LowerLeftX: 0, LowerLeftY: 1, LowerRightX: 1, LowerRightY: 1,
		UpperLeftX: 0, UpperLeftY: 0, UpperRightX: 1, UpperRightY: 0,
	}

	type mediaFile struct {
		filename string
		data     []byte
	}
	var mediaFiles []mediaFile

	var videoMaterials []ccVideoMaterial
	videoSegments := []ccSegment{} // 直接初始化为非 nil 空切片

	var audioMaterials []ccAudioMaterial
	var audioSegments []ccSegment

	// P3-3: 字幕和注释拆分为两个独立轨道，避免同一轨道同时间点多文本遮挡问题
	var subtitleMaterials []ccTextMaterial
	var subtitleSegments []ccSegment
	var annMaterials []ccTextMaterial
	var annSegments []ccSegment

	// 伴生素材集合（ExportBRollDraft 与 ExportCapCutDraft 共用相同的伴生素材逻辑）
	var brollSpeedsSlice        []ccSpeedMaterial
	var brollPlaceholderSlice   []ccPlaceholderInfo
	var brollCanvasSlice        []ccCanvasMaterial
	var brollSoundChannelSlice  []ccSoundChannelMapping
	var brollMaterialColorSlice []ccMaterialColor
	var brollVocalSepSlice      []ccVocalSeparation
	var brollBeatsSlice         []ccBeatsMaterial

	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	// P1-1: 优先使用项目封面，回退到第一张分镜图作为草稿封面缩略图
	var coverData []byte
	if novel != nil && novel.CoverImage != "" {
		coverData, _ = s.resolveMedia(novel.CoverImage)
	}
	if len(coverData) == 0 && len(shots) > 0 && shots[0].ImageURL != "" {
		coverData, _ = s.resolveMedia(shots[0].ImageURL)
	}

	// P3-2: track total loaded bytes for 2GiB guard
	var totalLoadedBytes int64

	var totalDuration int64

	for _, shot := range shots {
		durationMicros := int64(shot.Duration * 1_000_000)
		if durationMicros <= 0 {
			durationMicros = 5_000_000
		}
		startMicros := totalDuration

		// ── 1. 图片素材（始终使用静态图，忽略 VideoURL）────────────────────
		mediaURL := shot.ImageURL
		if mediaURL == "" {
			mediaURL = shot.VideoURL // 兜底：无图时才用视频
		}

		vidMatID := uuid.New().String()
		vidFilename := fmt.Sprintf("%03d.jpg", shot.ShotNo)
		vidPath := vidFilename
		if mediaURL != "" {
			if mediaData, mediaErr := s.resolveMedia(mediaURL); mediaErr == nil && len(mediaData) > 0 {
				if totalLoadedBytes+int64(len(mediaData)) <= maxExportMediaBytes {
					totalLoadedBytes += int64(len(mediaData))
					mediaFiles = append(mediaFiles, mediaFile{filename: vidFilename, data: mediaData})
				} else {
					logger.Printf("[ExportBRollDraft] total media exceeds 2GiB, skipping image embed shot %d", shot.ShotNo)
				}
			} else {
				logger.Printf("[ExportBRollDraft] media load failed for shot %d (%v)", shot.ShotNo, mediaErr)
			}
		}

		videoMaterials = append(videoMaterials, ccVideoMaterial{
			CheckFlag:         63487,
			CropScale:         1,
			Crop:              defaultCrop,
			Duration:          10_800_000_000, // 图片素材固定 3 小时
			HasAudio:          false,
			Height:            height,
			ID:                vidMatID,
			ImportTime:        now,
			ImportTimeUs:      now,
			LocalMaterialPath: "",
			LocalVideoPath:    "",
			MaterialID:        vidMatID,
			Path:              vidPath,
			SourcePlatform:    0,
			Type:              "photo",
			Width:             width,
		})

		{
			brollSeg := ccSegment{
				Clip: &ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
				},
				ID:              uuid.New().String(),
				MaterialID:      vidMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
				TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
				Type:            "video",
				Visible:         true,
				Volume:          1.0,
			}
			applyVideoSegmentDefaults(&brollSeg)
			// 创建 video segment 伴生素材（6 个）
			bSpd := newSpeedMaterial()
			bPh := newPlaceholderInfo()
			bCv := newCanvasMaterial()
			bSc := newSoundChannelMapping()
			bMc := newMaterialColor()
			bVs := newVocalSeparation()
			brollSpeedsSlice = append(brollSpeedsSlice, bSpd)
			brollPlaceholderSlice = append(brollPlaceholderSlice, bPh)
			brollCanvasSlice = append(brollCanvasSlice, bCv)
			brollSoundChannelSlice = append(brollSoundChannelSlice, bSc)
			brollMaterialColorSlice = append(brollMaterialColorSlice, bMc)
			brollVocalSepSlice = append(brollVocalSepSlice, bVs)
			brollSeg.ExtraMaterialRefs = append(brollSeg.ExtraMaterialRefs, bSpd.ID, bPh.ID, bCv.ID, bSc.ID, bMc.ID, bVs.ID)
			videoSegments = append(videoSegments, brollSeg)
		}

		// ── 2. 配音音频素材 ────────────────────────────────────────────────
		// 优先 shot.AudioPath（合成后的整段音频）；
		// P2-3: 当 AudioPath 为空且有 VoiceSegments 时，逐段加入音频轨道并按 SeqNo 顺序排列，
		// 确保多段 TTS 全部保留，不再仅取首段。
		if shot.AudioPath != "" {
			audMatID := uuid.New().String()
			actualAudioDur := durationMicros
			audPath := fmt.Sprintf("%03d_voice.mp3", shot.ShotNo)
			if data, err := s.resolveMedia(shot.AudioPath); err == nil && len(data) > 0 {
				ext := audioExtension(shot.AudioPath)
				audPath = fmt.Sprintf("%03d_voice%s", shot.ShotNo, ext)
				if totalLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					totalLoadedBytes += int64(len(data))
					mediaFiles = append(mediaFiles, mediaFile{filename: audPath, data: data})
				} else {
					logger.Printf("[ExportBRollDraft] audio: total media exceeds 2GiB, skipping audio embed shot %d", shot.ShotNo)
				}
				if dur := parseAudioDurationMicros(data, ext); dur > 0 {
					actualAudioDur = dur
				}
			} else {
				logger.Printf("[ExportBRollDraft] audio load failed shot %d url=%q: %v", shot.ShotNo, shot.AudioPath, err)
			}
			srcDur := actualAudioDur
			if srcDur > durationMicros {
				srcDur = durationMicros
			}
			audioMaterials = append(audioMaterials, ccAudioMaterial{
				CheckFlag:  1,
				Duration:   actualAudioDur,
				FilePath:   audPath,
				ID:         audMatID,
				MaterialID: audMatID,
				Name:       fmt.Sprintf("shot_%03d_audio", shot.ShotNo),
				Type:       "extract_music",
			})
			{
				// BRoll audio segment 伴生素材（5 个）
				baSpd := newSpeedMaterial()
				baPh := newPlaceholderInfo()
				baBts := newBeatsMaterial()
				baSc := newSoundChannelMapping()
				baVs := newVocalSeparation()
				brollSpeedsSlice = append(brollSpeedsSlice, baSpd)
				brollPlaceholderSlice = append(brollPlaceholderSlice, baPh)
				brollBeatsSlice = append(brollBeatsSlice, baBts)
				brollSoundChannelSlice = append(brollSoundChannelSlice, baSc)
				brollVocalSepSlice = append(brollVocalSepSlice, baVs)
				baSeg := ccSegment{
					Clip:            nil, // 音频轨道必须为 null
					ID:              uuid.New().String(),
					MaterialID:      audMatID,
					Speed:           1.0,
					SourceTimerange: ccTimeRange{Duration: srcDur, Start: 0},
					TargetTimerange: ccTimeRange{Duration: srcDur, Start: startMicros},
					Type:            "audio",
					Visible:         true,
					Volume:          1.0,
				}
				applyVideoSegmentDefaults(&baSeg)
				baSeg.ExtraMaterialRefs = append(baSeg.ExtraMaterialRefs, baSpd.ID, baPh.ID, baBts.ID, baSc.ID, baVs.ID)
				audioSegments = append(audioSegments, baSeg)
			}
		} else if s.segmentRepo != nil {
			// P2-3: no merged audio — place each VoiceSegment individually at the correct timeline offset
			segs, segErr := s.segmentRepo.ListByShotID(shot.ID)
			if segErr == nil && len(segs) > 0 {
				segOffset := startMicros // running offset within the shot
				for _, seg := range segs {
					if seg.AudioPath == "" {
						continue
					}
					audMatID := uuid.New().String()
					actualSegDur := durationMicros // fallback
					audPath := fmt.Sprintf("%03d_seg%02d.mp3", shot.ShotNo, seg.SeqNo)
					if data, rdErr := s.resolveMedia(seg.AudioPath); rdErr == nil && len(data) > 0 {
						ext := audioExtension(seg.AudioPath)
						audPath = fmt.Sprintf("%03d_seg%02d%s", shot.ShotNo, seg.SeqNo, ext)
						if totalLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
							totalLoadedBytes += int64(len(data))
							mediaFiles = append(mediaFiles, mediaFile{filename: audPath, data: data})
						} else {
							logger.Printf("[ExportBRollDraft] P3-2: total media exceeds 2GiB, skipping VoiceSegment embed shot %d seg %d", shot.ShotNo, seg.SeqNo)
						}
						if dur := parseAudioDurationMicros(data, ext); dur > 0 {
							actualSegDur = dur
						}
					}
					// also use stored DurationSecs if available (avoids re-parsing)
					if seg.DurationSecs > 0 {
						actualSegDur = int64(seg.DurationSecs * 1_000_000)
					}
					// cap at remaining shot time to avoid overflow into next shot
					remaining := startMicros + durationMicros - segOffset
					srcDur := actualSegDur
					if srcDur > remaining {
						srcDur = remaining
					}
					if srcDur <= 0 {
						break // used up all shot time
					}
					audioMaterials = append(audioMaterials, ccAudioMaterial{
						CheckFlag:  1,
						Duration:   actualSegDur,
						FilePath:   audPath,
						ID:         audMatID,
						MaterialID: audMatID,
						Name:       fmt.Sprintf("shot_%03d_seg%02d", shot.ShotNo, seg.SeqNo),
						Type:       "extract_music",
					})
					// BRoll VoiceSegment 伴生素材（5 个）
					bvsSpd := newSpeedMaterial()
					bvsPh := newPlaceholderInfo()
					bvsBts := newBeatsMaterial()
					bvsSc := newSoundChannelMapping()
					bvsVs := newVocalSeparation()
					brollSpeedsSlice = append(brollSpeedsSlice, bvsSpd)
					brollPlaceholderSlice = append(brollPlaceholderSlice, bvsPh)
					brollBeatsSlice = append(brollBeatsSlice, bvsBts)
					brollSoundChannelSlice = append(brollSoundChannelSlice, bvsSc)
					brollVocalSepSlice = append(brollVocalSepSlice, bvsVs)
					bvsSeg := ccSegment{
						Clip:            nil, // 音频轨道必须为 null
						ID:              uuid.New().String(),
						MaterialID:      audMatID,
						Speed:           1.0,
						SourceTimerange: ccTimeRange{Duration: srcDur, Start: 0},
						TargetTimerange: ccTimeRange{Duration: srcDur, Start: segOffset},
						Type:            "audio",
						Visible:         true,
						Volume:          1.0,
					}
					applyVideoSegmentDefaults(&bvsSeg)
					bvsSeg.ExtraMaterialRefs = append(bvsSeg.ExtraMaterialRefs, bvsSpd.ID, bvsPh.ID, bvsBts.ID, bvsSc.ID, bvsVs.ID)
					audioSegments = append(audioSegments, bvsSeg)
					segOffset += actualSegDur
				}
			}
		}

		// ── 3. 字幕轨（底部旁白/台词） ────────────────────────────────────
		subtitleText := shot.Dialogue
		if subtitleText == "" {
			subtitleText = shot.Narration
		}
		if subtitleText == "" {
			subtitleText = shot.Description
		}
		if subtitleText != "" && subCfg.enabled {
			txtMatID := uuid.New().String()
			subtitleText = sanitizeSubtitleText(subtitleText)
			// P3-3: 字幕素材单独放入 subtitleMaterials
			subtitleMaterials = append(subtitleMaterials, ccTextMaterial{
				CheckFlag:  7,
				Content:    buildTextContent(subtitleText, subCfg),
				ID:         txtMatID,
				IsSubtitle: true,
				MaterialID: txtMatID,
				Name:       fmt.Sprintf("shot_%03d_subtitle", shot.ShotNo),
				Type:       "text",
			})
			// subtitle segment 伴生素材（3 个）
			bsubSpd := newSpeedMaterial()
			bsubPh := newPlaceholderInfo()
			bsubSc := newSoundChannelMapping()
			brollSpeedsSlice = append(brollSpeedsSlice, bsubSpd)
			brollPlaceholderSlice = append(brollPlaceholderSlice, bsubPh)
			brollSoundChannelSlice = append(brollSoundChannelSlice, bsubSc)
			bsubSeg := ccSegment{
				Clip: &ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
					Transform: ccTransform{Y: subtitleTransformY(subCfg.position)},
				},
				ID:              uuid.New().String(),
				MaterialID:      txtMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
				TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
				Type:            "text",
				Visible:         true,
				Volume:          0,
			}
			applyVideoSegmentDefaults(&bsubSeg)
			bsubSeg.ExtraMaterialRefs = append(bsubSeg.ExtraMaterialRefs, bsubSpd.ID, bsubPh.ID, bsubSc.ID)
			subtitleSegments = append(subtitleSegments, bsubSeg)
		}

		// ── 4. 注释轨（顶部分镜编号 + 描述，供剪辑师参考）────────────────
		// P3-3: 注释素材单独放入 annMaterials，与字幕轨分离
		annText := fmt.Sprintf("[镜%d]", shot.ShotNo)
		if shot.Description != "" {
			desc := shot.Description
			if len([]rune(desc)) > 30 {
				runes := []rune(desc)
				desc = string(runes[:30]) + "…"
			}
			annText = fmt.Sprintf("[镜%d] %s", shot.ShotNo, desc)
		}
		annMatID := uuid.New().String()
		annMaterials = append(annMaterials, ccTextMaterial{
			CheckFlag:  7,
			Content:    buildTextContent(annText, annCfg),
			ID:         annMatID,
			IsSubtitle: false,
			MaterialID: annMatID,
			Name:       fmt.Sprintf("shot_%03d_ann", shot.ShotNo),
			Type:       "text",
		})
		// annotation segment 伴生素材（3 个）
		bannSpd := newSpeedMaterial()
		bannPh := newPlaceholderInfo()
		bannSc := newSoundChannelMapping()
		brollSpeedsSlice = append(brollSpeedsSlice, bannSpd)
		brollPlaceholderSlice = append(brollPlaceholderSlice, bannPh)
		brollSoundChannelSlice = append(brollSoundChannelSlice, bannSc)
		bannSeg := ccSegment{
			Clip: &ccClip{
				Alpha: 1.0,
				Scale: ccScale{X: 1.0, Y: 1.0},
				Transform: ccTransform{Y: subtitleTransformY("top")},
			},
			ID:              uuid.New().String(),
			MaterialID:      annMatID,
			Speed:           1.0,
			SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
			TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
			Type:            "text",
			Visible:         true,
			Volume:          0,
		}
		applyVideoSegmentDefaults(&bannSeg)
		bannSeg.ExtraMaterialRefs = append(bannSeg.ExtraMaterialRefs, bannSpd.ID, bannPh.ID, bannSc.ID)
		annSegments = append(annSegments, bannSeg)

		totalDuration += durationMicros
	}

	tracks := []ccTrack{
		{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Segments:      videoSegments,
			Type:          "video",
		},
	}
	if len(audioSegments) > 0 {
		tracks = append(tracks, ccTrack{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Name:          "配音",
			Segments:      audioSegments,
			Type:          "audio",
		})
	}
	// P3-3: 字幕和注释分两条独立轨道，避免同一轨道同时间点多段文本发生遮挡
	if len(subtitleSegments) > 0 {
		tracks = append(tracks, ccTrack{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Name:          "字幕",
			Segments:      subtitleSegments,
			Type:          "text",
		})
	}
	if len(annSegments) > 0 {
		tracks = append(tracks, ccTrack{
			Attribute:     0,
			Flag:          0,
			ID:            uuid.New().String(),
			IsDefaultName: true,
			Name:          "注释",
			Segments:      annSegments,
			Type:          "text",
		})
	}

	if audioMaterials == nil {
		audioMaterials = []ccAudioMaterial{}
	}
	// Bug-E: videoMaterials nil → JSON null，防御性置空数组（0 分镜时触发）
	if videoMaterials == nil {
		videoMaterials = []ccVideoMaterial{}
	}
	if videoSegments == nil {
		videoSegments = []ccSegment{}
	}
	// P3-3: 合并两个文本素材列表用于 Materials.Texts
	allTextMaterials := append(subtitleMaterials, annMaterials...)
	if allTextMaterials == nil {
		allTextMaterials = []ccTextMaterial{}
	}

	// 确保 BRoll 伴生素材切片不为 nil
	if brollSpeedsSlice == nil {
		brollSpeedsSlice = []ccSpeedMaterial{}
	}
	if brollPlaceholderSlice == nil {
		brollPlaceholderSlice = []ccPlaceholderInfo{}
	}
	if brollCanvasSlice == nil {
		brollCanvasSlice = []ccCanvasMaterial{}
	}
	if brollSoundChannelSlice == nil {
		brollSoundChannelSlice = []ccSoundChannelMapping{}
	}
	if brollMaterialColorSlice == nil {
		brollMaterialColorSlice = []ccMaterialColor{}
	}
	if brollVocalSepSlice == nil {
		brollVocalSepSlice = []ccVocalSeparation{}
	}
	if brollBeatsSlice == nil {
		brollBeatsSlice = []ccBeatsMaterial{}
	}

	content := ccDraftContent{
		CanvasConfig:         ccCanvasConfig{Height: height, Ratio: aspectRatioString(ratio), Width: width},
		CreateTime:           now,
		Duration:             totalDuration,
		FPS:                  24.0,
		ID:                   draftID,
		Keyframes:            ccKeyframes{Adjusts: []interface{}{}, Audios: []interface{}{}, ColorWheels: []interface{}{}, Effects: []interface{}{}, Filters: []interface{}{}, Handwrites: []interface{}{}, SpeedStickers: []interface{}{}, Stickers: []interface{}{}, Texts: []interface{}{}, Videos: []ccKeyframeGroup{}, VocalSounds: []interface{}{}},
		LastModifiedPlatform: ccPlatform{AppSource: "cc", AppVersion: "5.0.0", OS: "mac"}, // 必须为对象（真实草稿）
		Materials: ccMaterials{
			AudioFades:           []interface{}{},
			Audios:               audioMaterials,
			Beats:                brollBeatsSlice,
			Canvases:             brollCanvasSlice,
			Chromas:              []interface{}{},
			ColorCurves:          []interface{}{},
			Filters:              []interface{}{},
			GreenScreens:         []interface{}{},
			Masks:                []interface{}{},
			MaterialAnimations:   []interface{}{},
			MaterialColors:       brollMaterialColorSlice,
			PlaceholderInfos:     brollPlaceholderSlice,
			Shapes:               []interface{}{},
			SoundChannelMappings: brollSoundChannelSlice,
			Speeds:               brollSpeedsSlice,
			Stickers:             []interface{}{},
			Texts:                allTextMaterials,
			TextTemplates:        []interface{}{},
			Transitions:          []ccTransitionMaterial{},
			VideoEffects:         []interface{}{},
			Videos:               videoMaterials,
			VoiceEffects:         []interface{}{},
			VocalSeparations:     brollVocalSepSlice,
		},
		Name:          video.Title + " (B剪)",
		NewVersion:    "110.0.0",
		Platform:      ccPlatform{AppSource: "cc", AppVersion: "5.0.0", OS: "mac"},
		Relationships: []interface{}{},
		Tracks:        tracks,
		UpdateTime:    now,
		Version:       360000, // 整数（真实草稿），CapCut 做 int 类型断言
	}

	// P1-1: 有封面图时在 meta 中引用
	brollCoverName := ""
	if len(coverData) > 0 {
		brollCoverName = "cover.jpg"
	}
	meta := ccMetaInfo{
		DraftCover:               brollCoverName,
		DraftFoldPath:            "",
		DraftID:                  draftID,
		DraftIsAI:                false,
		DraftIsArticleVideo:      false,
		DraftIsInvisible:         false,
		DraftMaterials:           []interface{}{},
		DraftName:                video.Title + " (B剪)",
		DraftNewVersion:          "110.0.0", // P2-2
		DraftRootPath:            "",
		DraftSegmentExtraInfo:    []interface{}{},
		DraftTimelineMaterialsV2: []interface{}{},
		TmDraftCreate:            now,
		TmDraftModify:            now,
		TmDuration:               totalDuration,
	}

	contentJSON, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal draft content: %w", err)
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal draft meta: %w", err)
	}

	zipFile, err := os.CreateTemp("", "inkframe-broll-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp zip file: %w", err)
	}
	zipFilePath := zipFile.Name()
	defer os.Remove(zipFilePath)

	zw := zip.NewWriter(zipFile)
	writeZip := func(name string, data []byte) error {
		w, e := zw.Create(name)
		if e != nil {
			return e
		}
		_, e = w.Write(data)
		return e
	}

	prefix := projectName + "_broll/"
	if err := writeZip(prefix+"draft_content.json", contentJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_content.json: %w", err)
	}
	// macOS CapCut International 读取 draft_info.json
	if err := writeZip(prefix+"draft_info.json", contentJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_info.json: %w", err)
	}
	if err := writeZip(prefix+"draft_meta_info.json", metaJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_meta_info.json: %w", err)
	}
	virtualStoreJSON, _ := json.Marshal(map[string]interface{}{"sub_store": map[string]interface{}{}})
	if err := writeZip(prefix+"draft_virtual_store.json", virtualStoreJSON); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write draft_virtual_store.json: %w", err)
	}
	// P1-1: 封面图（草稿列表缩略图）
	if len(coverData) > 0 {
		if err := writeZip(prefix+"cover.jpg", coverData); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write cover.jpg: %w", err)
		}
	}
	for _, mf := range mediaFiles {
		if err := writeZip(prefix+mf.filename, mf.data); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write media file %s: %w", mf.filename, err)
		}
	}
	if srtContent := buildSRTSubtitles(shots); srtContent != "" {
		if err := writeZip(prefix+"subtitle.srt", []byte(srtContent)); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write subtitle.srt: %w", err)
		}
	}
	if err := writeZip(prefix+"README.txt", []byte(buildCapCutReadme(projectName+"_broll"))); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write README.txt: %w", err)
	}
	if err := zw.Close(); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("close zip: %w", err)
	}
	if err := zipFile.Close(); err != nil {
		return nil, fmt.Errorf("close zip file: %w", err)
	}

	zipData, err := os.ReadFile(zipFilePath)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	result := &ExportResult{
		Data:        zipData,
		Filename:    projectName + "_broll.zip",
		ContentType: "application/zip",
	}
	logger.Printf("[CapCutService] ExportBRollDraft done: filename=%s size=%d", result.Filename, len(result.Data))
	return result, nil
}

// buildCapCutReadme 生成剪映草稿导入说明文件内容
func buildCapCutReadme(projectName string) string {
	return fmt.Sprintf(`InkFrame 剪映草稿 — 导入说明
================================

草稿名称：%s

【版本兼容性】
- CapCut International（国际版）：完全支持
- 剪映 5.9 及以下：支持
- 剪映 6.0+（国内版）：不支持（6.0 起草稿文件加密，本导出格式为明文 JSON）
  推荐使用 CapCut International 导入

【重要】剪映/CapCut 无法直接打开 ZIP 文件，请按以下步骤导入：

CapCut International (macOS)
-----------------------------
1. 解压此 ZIP，得到文件夹「%s」
2. 将该文件夹移动到：
   ~/Movies/CapCut/User Data/Projects/com.lveditor.draft/
3. 重新启动 CapCut，草稿将出现在草稿列表

CapCut International (Windows)
--------------------------------
1. 解压此 ZIP，得到文件夹「%s」
2. 将该文件夹移动到：
   %%USERPROFILE%%\AppData\Local\CapCut\User Data\Projects\com.lveditor.draft\
3. 重新启动 CapCut，草稿将出现在草稿列表

剪映 5.9 及以下 (macOS)
------------------------
1. 解压此 ZIP，得到文件夹「%s」
2. 将该文件夹移动到：
   ~/Movies/JianyingPro/User Data/Projects/com.lveditor.draft/
3. 重新启动剪映，草稿将出现在草稿列表

【素材说明】
- 图片/视频/音效已打包到 ZIP 内（与草稿 JSON 文件放在同一目录）
- CapCut 打开草稿时会从草稿目录自动查找同名素材文件，无需联网
- 若提示"素材离线"或"无法找到文件"：
  确认已将整个文件夹（含图片/音频）完整移入草稿目录后重启 CapCut
- 若部分素材显示为红色占位（下载超时/超出 2GiB 限制）：
  在 CapCut 中右键该片段 → "重新链接素材" → 选择 ZIP 解压后的对应文件
`, projectName, projectName, projectName, projectName)
}

// fcpXMLEscapeAttr 转义 XML 属性中的特殊字符
func fcpXMLEscapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// ExportFCPXML 导出 FCPXML 1.10 格式 ZIP（可在 DaVinci Resolve / Final Cut Pro 导入）
// src 使用原始 CDN URL，同时将媒体文件打包到 media/ 供离线重连。
func (s *CapCutService) ExportFCPXML(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportFCPXML: videoID=%d shots=%d", video.ID, len(shots))
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}
	ratio := video.AspectRatio
	if ratio == "" {
		ratio = "16:9"
	}
	width, height := aspectRatioDimensions(ratio)

	type assetInfo struct {
		id        string
		src       string
		duration  int64 // micros
		filename  string
		localClip string // 本地 Ken Burns 文件路径（slideshow 模式）
		audioID   string // empty = no audio
		audioSrc  string
		audioFile string
	}

	var assets []assetInfo
	var totalDuration int64
	// asset id counter: r1=format, r2..rN=video/image, r(N+1)..=audio
	nextID := 2

	// ── 第一遍：收集所有资产信息 ─────────────────────────────────────────
	for _, shot := range shots {
		dur := int64(shot.Duration * 1_000_000)
		if dur <= 0 {
			dur = 5_000_000
		}
		vidSrc, vidLocal := shotVideoSource(shot)
		mediaURL := shot.ImageURL
		ext := ".jpg"
		if vidSrc != "" {
			ext = ".mp4"
			if vidLocal {
				// 本地 Ken Burns 文件：src 用 media/ 相对路径（下面打包进 ZIP）
				mediaURL = "media/" + fmt.Sprintf("%03d.mp4", shot.ShotNo)
			} else {
				mediaURL = vidSrc
			}
		}
		vidID := fmt.Sprintf("r%d", nextID)
		nextID++
		vidFile := fmt.Sprintf("%03d%s", shot.ShotNo, ext)

		ai := assetInfo{id: vidID, src: mediaURL, duration: dur, filename: vidFile}
		// 本地剪辑文件保存到 ai.localClip 供后续打包
		if vidLocal && vidSrc != "" {
			ai.localClip = vidSrc
		}

		if shot.AudioPath != "" {
			audID := fmt.Sprintf("r%d", nextID)
			nextID++
			audExt := audioExtension(shot.AudioPath)
			audFile := fmt.Sprintf("%03d_audio%s", shot.ShotNo, audExt)
			ai.audioID = audID
			ai.audioSrc = shot.AudioPath
			ai.audioFile = audFile
		} else if s.segmentRepo != nil {
			// Bug-G: AudioPath 为空时回落到首个有效 VoiceSegment（FCPXML 单资产只引用一段音频）
			segs, segErr := s.segmentRepo.ListByShotID(shot.ID)
			if segErr == nil {
				for _, seg := range segs {
					if seg.AudioPath == "" {
						continue
					}
					audID := fmt.Sprintf("r%d", nextID)
					nextID++
					audExt := audioExtension(seg.AudioPath)
					audFile := fmt.Sprintf("%03d_seg01%s", shot.ShotNo, audExt)
					ai.audioID = audID
					ai.audioSrc = seg.AudioPath
					ai.audioFile = audFile
					break // 仅取首段
				}
			}
		}

		assets = append(assets, ai)
		totalDuration += dur
	}

	// ── 构建 FCPXML ──────────────────────────────────────────────────────
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString("\n<!DOCTYPE fcpxml>\n")
	sb.WriteString(`<fcpxml version="1.10">`)
	sb.WriteString("\n<resources>\n")
	// P0-1: frameDuration="1/24s" 与合成输出帧率一致（原为 1/25s 导致 FCP/DaVinci 项目帧率错误）
	// P2-3: 补充 colorSpace 避免 DaVinci Resolve 导入时弹出颜色空间选择对话框
	sb.WriteString(fmt.Sprintf(`  <format id="r1" frameDuration="1/24s" width="%d" height="%d" colorSpace="1-1-1 (Rec. 709)"/>`, width, height))
	sb.WriteString("\n")

	for _, a := range assets {
		durStr := fmt.Sprintf("%d/1000000s", a.duration)
		sb.WriteString(fmt.Sprintf(`  <asset id="%s" src="%s" duration="%s" hasVideo="1"/>`,
			a.id, fcpXMLEscapeAttr(a.src), durStr))
		sb.WriteString("\n")
		if a.audioID != "" {
			// 音频资产：src 优先使用 CDN URL；本地文件用 media/ 相对路径
			audioSrc := a.audioSrc
			if !strings.HasPrefix(audioSrc, "http://") && !strings.HasPrefix(audioSrc, "https://") {
				audioSrc = "media/" + a.audioFile
			}
			sb.WriteString(fmt.Sprintf(`  <asset id="%s" src="%s" duration="%s" hasAudio="1"/>`,
				a.audioID, fcpXMLEscapeAttr(audioSrc), durStr))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("</resources>\n")
	sb.WriteString(fmt.Sprintf(`<library><event name="%s"><project name="%s">`,
		fcpXMLEscapeAttr(video.Title), fcpXMLEscapeAttr(video.Title)))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf(`<sequence duration="%d/1000000s" format="r1">`, totalDuration))
	sb.WriteString("\n<spine>\n")

	var offset int64
	for _, a := range assets {
		durStr := fmt.Sprintf("%d/1000000s", a.duration)
		offStr := fmt.Sprintf("%d/1000000s", offset)
		if a.audioID == "" {
			// 纯视频/图片，无独立音频
			sb.WriteString(fmt.Sprintf(`  <asset-clip ref="%s" offset="%s" duration="%s" tcFormat="NDF"/>`,
				a.id, offStr, durStr))
			sb.WriteString("\n")
		} else {
			// 视频 + 连接音频（lane="-1" 为标准 FCPXML 旁白轨道）
			sb.WriteString(fmt.Sprintf(`  <asset-clip ref="%s" offset="%s" duration="%s" tcFormat="NDF">`,
				a.id, offStr, durStr))
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf(`    <audio ref="%s" lane="-1" offset="0s" duration="%s" role="dialogue"/>`,
				a.audioID, durStr))
			sb.WriteString("\n  </asset-clip>\n")
		}
		offset += a.duration
	}

	sb.WriteString("</spine>\n</sequence>\n</project></event></library>\n</fcpxml>\n")

	// ── 构建 ZIP（写入临时文件）────────────────────────────────────────────
	zipFile, err := os.CreateTemp("", "inkframe-fcpxml-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp zip file: %w", err)
	}
	zipFilePath := zipFile.Name()
	defer os.Remove(zipFilePath)

	zw := zip.NewWriter(zipFile)
	writeZip := func(name string, data []byte) error {
		w, e := zw.Create(name)
		if e != nil {
			return e
		}
		_, e = w.Write(data)
		return e
	}

	prefix := projectName + "_fcpxml/"
	if err := writeZip(prefix+projectName+".fcpxml", []byte(sb.String())); err != nil {
		zipFile.Close()
		return nil, fmt.Errorf("write fcpxml: %w", err)
	}

	// 媒体文件打包到 media/（供离线重连）
	// Bug-F: 加累积 2GiB 保护，防止大项目打包时 OOM
	var fcpLoadedBytes int64
	for _, a := range assets {
		if a.localClip != "" {
			// 本地 Ken Burns 文件（slideshow 模式）直接读取
			if data, err := os.ReadFile(a.localClip); err == nil {
				if fcpLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					fcpLoadedBytes += int64(len(data))
					if e := writeZip(prefix+"media/"+a.filename, data); e != nil {
						zipFile.Close()
						return nil, fmt.Errorf("write media %s: %w", a.filename, e)
					}
				} else {
					logger.Printf("[ExportFCPXML] total media exceeds 2GiB, skipping local clip %s", a.filename)
				}
			}
		} else if a.src != "" {
			if data, err := s.resolveMedia(a.src); err == nil && len(data) > 0 {
				if fcpLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					fcpLoadedBytes += int64(len(data))
					if e := writeZip(prefix+"media/"+a.filename, data); e != nil {
						zipFile.Close()
						return nil, fmt.Errorf("write media %s: %w", a.filename, e)
					}
				} else {
					logger.Printf("[ExportFCPXML] total media exceeds 2GiB, skipping remote media %s", a.filename)
				}
			}
		}
		// 音频文件：本地或远程均打包进 media/
		if a.audioSrc != "" {
			if data, err := s.resolveMedia(a.audioSrc); err == nil && len(data) > 0 {
				if fcpLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					fcpLoadedBytes += int64(len(data))
					if e := writeZip(prefix+"media/"+a.audioFile, data); e != nil {
						zipFile.Close()
						return nil, fmt.Errorf("write audio %s: %w", a.audioFile, e)
					}
				} else {
					logger.Printf("[ExportFCPXML] total media exceeds 2GiB, skipping audio %s", a.audioFile)
				}
			}
		}
	}

	// SRT 字幕
	if srtContent := buildSRTSubtitles(shots); srtContent != "" {
		if err := writeZip(prefix+"subtitle.srt", []byte(srtContent)); err != nil {
			zipFile.Close()
			return nil, fmt.Errorf("write subtitle.srt: %w", err)
		}
	}

	if err := zw.Close(); err != nil {
		zipFile.Close()
		logger.Printf("[CapCutService] ExportFCPXML: close zip failed: %v", err)
		return nil, fmt.Errorf("close zip: %w", err)
	}
	if err := zipFile.Close(); err != nil {
		return nil, fmt.Errorf("close zip file: %w", err)
	}

	zipData, err := os.ReadFile(zipFilePath)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	result := &ExportResult{
		Data:        zipData,
		Filename:    projectName + "_fcpxml.zip",
		ContentType: "application/zip",
	}
	logger.Printf("[CapCutService] ExportFCPXML done: filename=%s size=%d", result.Filename, len(result.Data))
	return result, nil
}

// shotJSONMeta 素材包 shots.json 中每镜元数据
type shotJSONMeta struct {
	ShotNo      int     `json:"shot_no"`
	Description string  `json:"description"`
	Dialogue    string  `json:"dialogue"`
	Narration   string  `json:"narration"`
	Duration    float64 `json:"duration"`
	VideoFile   string  `json:"video_file,omitempty"`
	ImageFile   string  `json:"image_file,omitempty"`
	AudioFile   string  `json:"audio_file,omitempty"`
}

// ExportResourceZip 导出素材包 ZIP（图片/视频 + 音频 + SRT + shots.json，可用于任意剪辑软件）
func (s *CapCutService) ExportResourceZip(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportResourceZip: videoID=%d shots=%d", video.ID, len(shots))
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	zipFile, err := os.CreateTemp("", "inkframe-assets-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp zip file: %w", err)
	}
	zipFilePath := zipFile.Name()
	defer os.Remove(zipFilePath)

	zw := zip.NewWriter(zipFile)
	writeZip := func(name string, data []byte) error {
		w, e := zw.Create(name)
		if e != nil {
			return e
		}
		_, e = w.Write(data)
		return e
	}

	// P2-4: total size guard for resource ZIP
	var zipLoadedBytes int64

	var metas []shotJSONMeta

	for _, shot := range shots {
		meta := shotJSONMeta{
			ShotNo:      shot.ShotNo,
			Description: shot.Description,
			Dialogue:    shot.Dialogue,
			Narration:   shot.Narration,
			Duration:    shot.Duration,
		}

		// 视频/图片
		vidSrc, _ := shotVideoSource(shot)
		isVideo := vidSrc != ""
		if isVideo {
			filename := fmt.Sprintf("%03d.mp4", shot.ShotNo)
			if data, err := s.resolveMedia(vidSrc); err == nil && len(data) > 0 {
				if zipLoadedBytes+int64(len(data)) > maxExportMediaBytes {
					logger.Printf("[ExportResourceZip] P2-4: total media exceeds 2GiB, skipping shot %d video", shot.ShotNo)
				} else {
					zipLoadedBytes += int64(len(data))
					if e := writeZip("video/"+filename, data); e != nil {
						return nil, fmt.Errorf("write video/%s: %w", filename, e)
					}
					meta.VideoFile = "video/" + filename
				}
			}
		} else if shot.ImageURL != "" {
			filename := fmt.Sprintf("%03d.jpg", shot.ShotNo)
			if data, err := s.resolveMedia(shot.ImageURL); err == nil && len(data) > 0 {
				if zipLoadedBytes+int64(len(data)) > maxExportMediaBytes {
					logger.Printf("[ExportResourceZip] P2-4: total media exceeds 2GiB, skipping shot %d image", shot.ShotNo)
				} else {
					zipLoadedBytes += int64(len(data))
					if e := writeZip("image/"+filename, data); e != nil {
						return nil, fmt.Errorf("write image/%s: %w", filename, e)
					}
					meta.ImageFile = "image/" + filename
				}
			}
		}

		// 音频：优先 shot.AudioPath，无则取 VoiceSegments（P1-2）
		if shot.AudioPath != "" {
			if data, err := s.resolveMedia(shot.AudioPath); err == nil && len(data) > 0 {
				if zipLoadedBytes+int64(len(data)) <= maxExportMediaBytes {
					zipLoadedBytes += int64(len(data))
					ext := audioExtension(shot.AudioPath)
					filename := fmt.Sprintf("%03d%s", shot.ShotNo, ext)
					if e := writeZip("audio/"+filename, data); e != nil {
						return nil, fmt.Errorf("write audio/%s: %w", filename, e)
					}
					meta.AudioFile = "audio/" + filename
				} else {
					logger.Printf("[ExportResourceZip] audio: total media exceeds 2GiB, skipping shot %d audio", shot.ShotNo)
				}
			}
		} else if s.segmentRepo != nil {
			segs, segErr := s.segmentRepo.ListByShotID(shot.ID)
			if segErr == nil {
				for _, seg := range segs {
					if seg.AudioPath == "" {
						continue
					}
					if data, rdErr := s.resolveMedia(seg.AudioPath); rdErr == nil && len(data) > 0 {
						if zipLoadedBytes+int64(len(data)) > maxExportMediaBytes {
							logger.Printf("[ExportResourceZip] VoiceSegment: total media exceeds 2GiB, skipping shot %d seg %d", shot.ShotNo, seg.SeqNo)
							break
						}
						zipLoadedBytes += int64(len(data))
						ext := audioExtension(seg.AudioPath)
						filename := fmt.Sprintf("%03d_seg%02d%s", shot.ShotNo, seg.SeqNo, ext)
						if e := writeZip("audio/"+filename, data); e == nil && meta.AudioFile == "" {
							meta.AudioFile = "audio/" + filename // first segment as primary ref
						}
					}
				}
			}
		}

		metas = append(metas, meta)
	}

	// shots.json
	shotsJSON, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal shots.json: %w", err)
	}
	if err := writeZip("shots.json", shotsJSON); err != nil {
		return nil, fmt.Errorf("write shots.json: %w", err)
	}

	// subtitle.srt
	if srtContent := buildSRTSubtitles(shots); srtContent != "" {
		if err := writeZip("subtitle.srt", []byte(srtContent)); err != nil {
			return nil, fmt.Errorf("write subtitle.srt: %w", err)
		}
	}

	if err := zw.Close(); err != nil {
		zipFile.Close()
		logger.Printf("[CapCutService] ExportResourceZip: close zip failed: %v", err)
		return nil, fmt.Errorf("close zip: %w", err)
	}
	if err := zipFile.Close(); err != nil {
		return nil, fmt.Errorf("close zip file: %w", err)
	}

	zipData, err := os.ReadFile(zipFilePath)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	result := &ExportResult{
		Data:        zipData,
		Filename:    projectName + "_assets.zip",
		ContentType: "application/zip",
	}
	logger.Printf("[CapCutService] ExportResourceZip done: filename=%s size=%d", result.Filename, len(result.Data))
	return result, nil
}

// ExportSRT 导出纯字幕 SRT 文件
func (s *CapCutService) ExportSRT(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportSRT: videoID=%d shots=%d", video.ID, len(shots))
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	return &ExportResult{
		Data:        []byte(buildSRTSubtitles(shots)),
		Filename:    projectName + ".srt",
		ContentType: "text/plain; charset=utf-8",
	}, nil
}

// shotVideoSource 返回镜头的有效视频来源。
// 优先级：ClipPath（本地 Ken Burns/静帧文件）> VideoURL（CDN）> ""（仅图片）
// isLocal=true 表示返回的是本地文件路径（已去除 file:// 前缀）。
func shotVideoSource(shot *model.StoryboardShot) (src string, isLocal bool) {
	if shot.ClipPath != "" {
		return strings.TrimPrefix(shot.ClipPath, "file://"), true
	}
	return shot.VideoURL, false
}

// readLocalOrRemoteFile 读取本地文件（支持 file:// 前缀）或远程 URL
func readLocalOrRemoteFile(path string) ([]byte, error) {
	if strings.HasPrefix(path, "file://") {
		return os.ReadFile(strings.TrimPrefix(path, "file://"))
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return downloadMediaFile(path)
	}
	// 相对 HTTP 路径（/uploads/...）→ 先尝试作为服务器工作目录下的本地文件
	if strings.HasPrefix(path, "/") {
		if data, err := os.ReadFile("." + path); err == nil {
			return data, nil
		}
		// 其余 /api/... 等路径无法在此处解析（需要 serverBaseURL），直接失败
		return nil, fmt.Errorf("cannot resolve relative URL %q without server base URL", path)
	}
	// 裸本地路径（相对或绝对路径）
	return os.ReadFile(path)
}

// audioExtension 从路径/URL 猜测音频后缀，默认 .mp3
func audioExtension(path string) string {
	lower := strings.ToLower(path)
	for _, ext := range []string{".wav", ".aac", ".ogg", ".m4a", ".flac", ".mp3"} {
		if strings.Contains(lower, ext) {
			return ext
		}
	}
	return ".mp3"
}

// downloadMediaFile 从 URL 下载文件，超时 2min，单文件最大 200MB。
// P2-1: 加 io.LimitReader 防止异常 CDN 大文件耗尽内存。
func downloadMediaFile(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxBytes = 200 << 20 // 200 MB per file
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("media file too large (>200MB)")
	}
	return data, nil
}

// parseAudioDurationMicros 解析音频文件的实际时长（微秒）。
// 支持 WAV（精确解析 RIFF 头）和 MP3（扫描首帧 bitrate 近似估算）。
// 其他格式或解析失败时返回 0（调用方应降级为 shot.Duration）。
func parseAudioDurationMicros(data []byte, ext string) int64 {
	switch strings.ToLower(ext) {
	case ".wav":
		return wavDurationMicros(data)
	case ".mp3":
		return mp3DurationMicros(data)
	default:
		return 0
	}
}

// wavDurationMicros 解析 WAV/RIFF 格式的精确时长（微秒）。
func wavDurationMicros(data []byte) int64 {
	if len(data) < 44 {
		return 0
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0
	}
	readU32LE := func(b []byte, off int) uint32 {
		return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
	}
	readU16LE := func(b []byte, off int) uint16 {
		return uint16(b[off]) | uint16(b[off+1])<<8
	}

	var byteRate uint32
	i := 12
	for i+8 <= len(data) {
		chunkID := string(data[i : i+4])
		chunkSize := int(readU32LE(data, i+4))
		// P0-3: 防止损坏的 WAV chunkSize 导致下一轮越界
		if chunkSize < 0 || i+8+chunkSize > len(data) {
			break
		}
		if chunkID == "fmt " && chunkSize >= 16 {
			// byteRate = sampleRate × numChannels × bitsPerSample/8
			sampleRate := readU32LE(data, i+8+4)
			numCh := readU16LE(data, i+8+2)
			bps := readU16LE(data, i+8+14)
			byteRate = sampleRate * uint32(numCh) * uint32(bps) / 8
		}
		if chunkID == "data" && byteRate > 0 {
			durationSec := float64(chunkSize) / float64(byteRate)
			return int64(durationSec * 1_000_000)
		}
		i += 8 + chunkSize
		if chunkSize%2 != 0 {
			i++ // RIFF chunks are word-aligned
		}
	}
	return 0
}

// mp3DurationMicros 通过扫描首个有效 MPEG-1 Layer3 帧获取 bitrate，
// 再用文件大小估算 MP3 时长（对 CBR MP3 准确，VBR 有偏差）。
func mp3DurationMicros(data []byte) int64 {
	// MPEG-1 Layer3 bitrate 表（kbps），索引 0 和 15 无效
	bitrateKbps := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}

	// 跳过 ID3v2 标签（常见于 TTS 输出文件头）
	offset := 0
	if len(data) >= 10 && string(data[0:3]) == "ID3" {
		syncsafe := func(b []byte) int {
			return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
		}
		offset = 10 + syncsafe(data[6:10])
	}

	// 扫描首个有效帧头（0xFF 0xFB / 0xFF 0xFA 等 MPEG-1 Layer3 同步字）
	for i := offset; i < len(data)-3; i++ {
		if data[i] != 0xFF || (data[i+1]&0xE0 != 0xE0) {
			continue
		}
		ver := (data[i+1] >> 3) & 0x03   // 11=MPEG1
		layer := (data[i+1] >> 1) & 0x03 // 01=Layer3
		if ver != 3 || layer != 1 {
			continue
		}
		brIdx := (data[i+2] >> 4) & 0x0F
		if brIdx == 0 || brIdx == 15 {
			continue
		}
		bitsPerSec := int64(bitrateKbps[brIdx]) * 1000
		if bitsPerSec <= 0 {
			continue
		}
		// 有效数据长度（去掉 ID3 标签）
		audioBytes := int64(len(data) - offset)
		durationSec := float64(audioBytes*8) / float64(bitsPerSec)
		return int64(durationSec * 1_000_000)
	}
	return 0
}

// --- 伴生素材创建辅助函数 ---

// newSpeedMaterial 创建 speed 伴生素材（每个 segment 必须有一个）
func newSpeedMaterial() ccSpeedMaterial {
	return ccSpeedMaterial{ID: uuid.New().String(), Type: "speed", Mode: 0, Speed: 1.0, CurveSpeed: nil}
}

// newPlaceholderInfo 创建 placeholder_info 伴生素材
func newPlaceholderInfo() ccPlaceholderInfo {
	return ccPlaceholderInfo{ID: uuid.New().String(), Type: "placeholder_info", MetaType: "none"}
}

// newCanvasMaterial 创建 canvas_color 伴生素材（video segment 专用）
func newCanvasMaterial() ccCanvasMaterial {
	return ccCanvasMaterial{ID: uuid.New().String(), Type: "canvas_color"}
}

// newSoundChannelMapping 创建 sound_channel_mapping 伴生素材
func newSoundChannelMapping() ccSoundChannelMapping {
	return ccSoundChannelMapping{ID: uuid.New().String()}
}

// newMaterialColor 创建 material_colors 伴生素材（video segment 专用）
func newMaterialColor() ccMaterialColor {
	return ccMaterialColor{
		ID:               uuid.New().String(),
		GradientColors:   []interface{}{},
		GradientPercents: []interface{}{},
		GradientAngle:    90.0,
	}
}

// newVocalSeparation 创建 vocal_separation 伴生素材
func newVocalSeparation() ccVocalSeparation {
	return ccVocalSeparation{ID: uuid.New().String(), Type: "vocal_separation", RemovedSounds: []interface{}{}}
}

// newBeatsMaterial 创建 beats 伴生素材（audio segment 专用）
func newBeatsMaterial() ccBeatsMaterial {
	return ccBeatsMaterial{
		ID:    uuid.New().String(),
		Type:  "beats",
		Gear:  404,
		Mode:  404,
		UserBeats: []interface{}{},
		AIBeats:   ccAIBeats{BeatSpeedInfos: []interface{}{}},
	}
}

// applyVideoSegmentDefaults 为 video segment 填充真实草稿所需的默认字段值
func applyVideoSegmentDefaults(seg *ccSegment) {
	seg.RenderTimerange = seg.TargetTimerange
	seg.EnableLut = true
	seg.EnableAdjust = true
	seg.EnableColorCurves = true
	seg.EnableHslCurves = true
	seg.EnableColorWheels = true
	seg.HdrSettings = ccHdrSettings{Mode: 1, Intensity: 1.0, Nits: 1000}
	seg.UniformScale = ccUniformScale{On: true, Value: 1.0}
	seg.LastNonzeroVolume = 1.0
	if seg.CommonKeyframes == nil {
		seg.CommonKeyframes = []interface{}{}
	}
	if seg.LyricKeyframes == nil {
		seg.LyricKeyframes = []interface{}{}
	}
	seg.EnableVideoMask = true
	seg.Source = "segmentsourcenormal"
}

// buildPhotoMotionKeyframes 为静图分镜生成 Ken Burns 运镜关键帧组。
// 根据 shot.CameraType 映射到对应运镜预设，shotNo 用于交替平移方向。
// 返回的各组 ID 应填入 segment.KeyframeRefs，组本身追加到 content.Keyframes.Videos。
func buildPhotoMotionKeyframes(segID, cameraType string, durMicros int64, shotNo int) []ccKeyframeGroup {
	type kfProp struct {
		propType string
		kv       [][2]float64 // [][time_offset_micros, value]
	}

	d := float64(durMicros)
	dir := 1.0
	if shotNo%2 == 0 {
		dir = -1.0
	}

	var props []kfProp
	switch cameraType {
	case "pan":
		// 水平平移：缩放固定 1.1，X 轴左右交替
		props = []kfProp{
			{"KFTypeScaleX", [][2]float64{{0, 1.1}, {d, 1.1}}},
			{"KFTypeScaleY", [][2]float64{{0, 1.1}, {d, 1.1}}},
			{"KFTypePositionX", [][2]float64{{0, -0.06 * dir}, {d, 0.06 * dir}}},
		}
	case "zoom":
		// 推镜头：逐渐放大
		props = []kfProp{
			{"KFTypeScaleX", [][2]float64{{0, 1.0}, {d, 1.25}}},
			{"KFTypeScaleY", [][2]float64{{0, 1.0}, {d, 1.25}}},
		}
	case "dolly":
		// 拉镜头：逐渐缩小
		props = []kfProp{
			{"KFTypeScaleX", [][2]float64{{0, 1.25}, {d, 1.0}}},
			{"KFTypeScaleY", [][2]float64{{0, 1.25}, {d, 1.0}}},
		}
	case "tracking":
		// 跟拍：轻微放大 + X 轴平移
		props = []kfProp{
			{"KFTypeScaleX", [][2]float64{{0, 1.0}, {d, 1.15}}},
			{"KFTypeScaleY", [][2]float64{{0, 1.0}, {d, 1.15}}},
			{"KFTypePositionX", [][2]float64{{0, 0}, {d, 0.05 * dir}}},
		}
	case "crane":
		// 升降镜头：缩放固定 1.1，Y 轴上下交替
		props = []kfProp{
			{"KFTypeScaleX", [][2]float64{{0, 1.1}, {d, 1.1}}},
			{"KFTypeScaleY", [][2]float64{{0, 1.1}, {d, 1.1}}},
			{"KFTypePositionY", [][2]float64{{0, -0.06 * dir}, {d, 0.06 * dir}}},
		}
	default:
		// static 或未知：轻微推镜头，增加画面动感
		props = []kfProp{
			{"KFTypeScaleX", [][2]float64{{0, 1.0}, {d, 1.05}}},
			{"KFTypeScaleY", [][2]float64{{0, 1.0}, {d, 1.05}}},
		}
	}

	var groups []ccKeyframeGroup
	for _, p := range props {
		var kfs []ccKeyframe
		for _, tv := range p.kv {
			kfs = append(kfs, ccKeyframe{
				CurveType:    "Line",
				GraphID:      "",
				ID:           uuid.New().String(),
				LeftControl:  ccPoint{},
				RightControl: ccPoint{},
				TimeOffset:   int64(tv[0]),
				Values:       []float64{tv[1]},
			})
		}
		groups = append(groups, ccKeyframeGroup{
			ID:           uuid.New().String(),
			KeyframeList: kfs,
			MaterialID:   segID,
			PropertyType: p.propType,
		})
	}
	return groups
}

// stripDialogueSpeakerPrefix 去除台词字段中的"角色名："前缀，仅保留台词内容。
// 例："妈妈：你好吗？" → "你好吗？"
// Dialogue 字段保留完整格式供 TTS 音色解析，字幕显示时才调用此函数。
func stripDialogueSpeakerPrefix(text string) string {
	for _, colon := range []string{"：", ":"} {
		idx := strings.Index(text, colon)
		if idx <= 0 || idx > len(colon)*12 {
			continue
		}
		prefix := []rune(text[:idx])
		if len(prefix) < 1 || len(prefix) > 8 {
			continue
		}
		rest := strings.TrimSpace(text[idx+len(colon):])
		if rest != "" {
			return rest
		}
	}
	return text
}

// shotSubtitleText 返回镜头的有效字幕文本。
// 优先级：Subtitle 覆盖 > Dialogue（去角色名前缀）> Narration > Description。
func shotSubtitleText(shot *model.StoryboardShot) string {
	if shot.Subtitle != "" {
		return shot.Subtitle
	}
	if shot.Dialogue != "" {
		return stripDialogueSpeakerPrefix(shot.Dialogue)
	}
	if shot.Narration != "" {
		return shot.Narration
	}
	return shot.Description
}

// buildSRTSubtitles 生成 SRT 格式字幕内容（通用字幕标准，可在剪映/VLC/PotPlayer 等中导入）。
// 文本优先级：字幕覆盖 > 台词 > 旁白 > 画面描述；无文本的镜头跳过编号。
func buildSRTSubtitles(shots []*model.StoryboardShot) string {
	var sb strings.Builder
	var cursor int64 // 当前时间指针（微秒）
	idx := 1
	for _, shot := range shots {
		dur := int64(shot.Duration * 1_000_000)
		if dur <= 0 {
			dur = 5_000_000
		}
		end := cursor + dur

		if text := shotSubtitleText(shot); text != "" {
			fmt.Fprintf(&sb, "%d\n%s --> %s\n%s\n\n",
				idx, microsToSRTTime(cursor), microsToSRTTime(end), text)
			idx++
		}
		cursor = end
	}
	return sb.String()
}

// microsToSRTTime 将微秒转为 SRT 时间格式 HH:MM:SS,mmm
func microsToSRTTime(micros int64) string {
	ms := micros / 1000
	hh := ms / 3_600_000
	ms %= 3_600_000
	mm := ms / 60_000
	ms %= 60_000
	ss := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hh, mm, ss, ms)
}

// sanitizeFilename 清理文件名非法字符，限制长度为50
func sanitizeFilename(name string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
	)
	result := strings.TrimSpace(r.Replace(name))
	if len([]rune(result)) > 50 {
		runes := []rune(result)
		result = string(runes[:50])
	}
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// VTT — github.com/asticode/go-astisub（695 stars，MIT，支持 SRT/VTT/SSA/TTML）
// ─────────────────────────────────────────────────────────────────────────────

// ExportVTT 导出 WebVTT 字幕文件（浏览器原生、YouTube、Bilibili、各类播放器均支持）
func (s *CapCutService) ExportVTT(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportVTT: videoID=%d shots=%d", video.ID, len(shots))
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	subs := astisub.NewSubtitles()
	var cursor time.Duration

	for _, shot := range shots {
		dur := time.Duration(shot.Duration * float64(time.Second))
		if dur <= 0 {
			dur = 5 * time.Second
		}
		if text := shotSubtitleText(shot); text != "" {
			subs.Items = append(subs.Items, &astisub.Item{
				StartAt: cursor,
				EndAt:   cursor + dur,
				Lines:   []astisub.Line{{Items: []astisub.LineItem{{Text: text}}}},
			})
		}
		cursor += dur
	}

	var buf bytes.Buffer
	if err := subs.WriteToWebVTT(&buf); err != nil {
		logger.Printf("[CapCutService] ExportVTT: write vtt failed: %v", err)
		return nil, fmt.Errorf("write vtt: %w", err)
	}

	return &ExportResult{
		Data:        buf.Bytes(),
		Filename:    projectName + ".vtt",
		ContentType: "text/vtt; charset=utf-8",
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// EDL — CMX3600（DaVinci Resolve、Avid、Premiere Pro、Vegas Pro 均支持）
// Go 无成熟库，格式极简，手动生成。
// ─────────────────────────────────────────────────────────────────────────────

// microsToEDLTimecode 将微秒转为 CMX3600 时间码 HH:MM:SS:FF（24fps，与合成输出帧率一致）
func microsToEDLTimecode(micros int64) string {
	const fps int64 = 24
	totalFrames := micros * fps / 1_000_000
	ff := totalFrames % fps
	ss := totalFrames / fps % 60
	mm := totalFrames / fps / 60 % 60
	hh := totalFrames / fps / 3600
	return fmt.Sprintf("%02d:%02d:%02d:%02d", hh, mm, ss, ff)
}

// ExportEDL 导出 CMX3600 EDL 文件（可在几乎所有专业非线性编辑软件中导入）
func (s *CapCutService) ExportEDL(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportEDL: videoID=%d shots=%d", video.ID, len(shots))
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("TITLE: %s\n", projectName))
	sb.WriteString("FCM: NON-DROP FRAME\n\n")

	eventNo := 1
	var recordIn int64
	for _, shot := range shots {
		dur := int64(shot.Duration * 1_000_000)
		if dur <= 0 {
			dur = 5_000_000
		}
		recordOut := recordIn + dur

		vidSrc, _ := shotVideoSource(shot)
		ext := ".jpg"
		if vidSrc != "" {
			ext = ".mp4"
		}
		clipName := fmt.Sprintf("%03d%s", shot.ShotNo, ext)

		// 视频事件
		sb.WriteString(fmt.Sprintf("%03d  AX       V     C        %s %s %s %s\n",
			eventNo,
			microsToEDLTimecode(0),
			microsToEDLTimecode(dur),
			microsToEDLTimecode(recordIn),
			microsToEDLTimecode(recordOut),
		))
		sb.WriteString(fmt.Sprintf("* FROM CLIP NAME: %s\n", clipName))
		comment := shot.Narration
		if comment == "" {
			comment = shot.Description
		}
		if comment != "" {
			runes := []rune(comment)
			if len(runes) > 80 {
				comment = string(runes[:80])
			}
			sb.WriteString(fmt.Sprintf("* COMMENT: %s\n", comment))
		}
		sb.WriteString("\n")
		eventNo++

		// 音频事件（单独 A 事件，与视频同时间线位置，引用独立音频素材）
		if shot.AudioPath != "" {
			audExt := audioExtension(shot.AudioPath)
			audClipName := fmt.Sprintf("%03d_audio%s", shot.ShotNo, audExt)
			sb.WriteString(fmt.Sprintf("%03d  AX       A     C        %s %s %s %s\n",
				eventNo,
				microsToEDLTimecode(0),
				microsToEDLTimecode(dur),
				microsToEDLTimecode(recordIn),
				microsToEDLTimecode(recordOut),
			))
			sb.WriteString(fmt.Sprintf("* FROM CLIP NAME: %s\n", audClipName))
			sb.WriteString("\n")
			eventNo++
		}

		recordIn = recordOut
	}

	return &ExportResult{
		Data:        []byte(sb.String()),
		Filename:    projectName + ".edl",
		ContentType: "text/plain; charset=utf-8",
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// OpenTimelineIO (.otio) — Pixar 主导的开放时间线标准（JSON，Adobe/Apple/Avid 均支持）
// Go 无官方绑定，按规范手动构造 JSON。
// spec: github.com/OpenTimelineIO/OpenTimelineIO-Specification
// ─────────────────────────────────────────────────────────────────────────────

type otioRationalTime struct {
	Value float64 `json:"value"`
	Rate  float64 `json:"rate"`
}

type otioTimeRange struct {
	StartTime otioRationalTime `json:"start_time"`
	Duration  otioRationalTime `json:"duration"`
}

type otioExternalReference struct {
	OTIOSchema     string         `json:"OTIO_SCHEMA"`
	TargetURL      string         `json:"target_url"`
	Name           string         `json:"name,omitempty"`
	AvailableRange *otioTimeRange `json:"available_range,omitempty"`
}

type otioGap struct {
	OTIOSchema  string         `json:"OTIO_SCHEMA"`
	Name        string         `json:"name"`
	SourceRange *otioTimeRange `json:"source_range,omitempty"`
}

type otioClip struct {
	OTIOSchema              string                           `json:"OTIO_SCHEMA"`
	Name                    string                           `json:"name"`
	SourceRange             *otioTimeRange                   `json:"source_range,omitempty"`
	MediaReferences         map[string]otioExternalReference `json:"media_references"`
	ActiveMediaReferenceKey string                           `json:"active_media_reference_key"`
}

type otioTrack struct {
	OTIOSchema string `json:"OTIO_SCHEMA"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // "Video" | "Audio"
	Children   []any  `json:"children"`
}

type otioStack struct {
	OTIOSchema string `json:"OTIO_SCHEMA"`
	Name       string `json:"name"`
	Children   []any  `json:"children"`
}

type otioTimeline struct {
	OTIOSchema string    `json:"OTIO_SCHEMA"`
	Name       string    `json:"name"`
	Tracks     otioStack `json:"tracks"`
}

// ExportOTIO 导出 OpenTimelineIO .otio 文件（Pixar 开放标准，Premiere / FCP / DaVinci 均可导入）
func (s *CapCutService) ExportOTIO(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportOTIO: videoID=%d shots=%d", video.ID, len(shots))
	const fps = 24.0 // P1-3: match synthesis output fps; 25.0 caused per-second timeline drift in FCP/DaVinci
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	var videoClips []any
	var audioClips []any
	hasAnyAudio := false

	for _, shot := range shots {
		dur := shot.Duration
		if dur <= 0 {
			dur = 5.0
		}
		durFrames := dur * fps
		srcRange := &otioTimeRange{
			StartTime: otioRationalTime{Value: 0, Rate: fps},
			Duration:  otioRationalTime{Value: durFrames, Rate: fps},
		}

		// 视频 / 图片 clip
		vidSrc, vidLocal := shotVideoSource(shot)
		mediaURL := shot.ImageURL
		ext := ".jpg"
		if vidSrc != "" {
			ext = ".mp4"
			if vidLocal {
				mediaURL = "file://" + vidSrc
			} else {
				mediaURL = vidSrc
			}
		}
		clipName := fmt.Sprintf("%03d%s", shot.ShotNo, ext)
		videoClips = append(videoClips, otioClip{
			OTIOSchema:  "Clip.2",
			Name:        clipName,
			SourceRange: srcRange,
			MediaReferences: map[string]otioExternalReference{
				"DEFAULT_MEDIA": {
					OTIOSchema:     "ExternalReference.1",
					TargetURL:      mediaURL,
					Name:           clipName,
					AvailableRange: srcRange,
				},
			},
			ActiveMediaReferenceKey: "DEFAULT_MEDIA",
		})

		// 音频 clip（或 gap 占位）
		if shot.AudioPath != "" {
			hasAnyAudio = true
			audExt := audioExtension(shot.AudioPath)
			audName := fmt.Sprintf("%03d_audio%s", shot.ShotNo, audExt)
			audioClips = append(audioClips, otioClip{
				OTIOSchema:  "Clip.2",
				Name:        audName,
				SourceRange: srcRange,
				MediaReferences: map[string]otioExternalReference{
					"DEFAULT_MEDIA": {
						OTIOSchema:     "ExternalReference.1",
						TargetURL:      shot.AudioPath,
						Name:           audName,
						AvailableRange: srcRange,
					},
				},
				ActiveMediaReferenceKey: "DEFAULT_MEDIA",
			})
		} else {
			// 无音频的镜头用 Gap 占位，保持轨道对齐
			audioClips = append(audioClips, otioGap{
				OTIOSchema:  "Gap.1",
				Name:        "gap",
				SourceRange: srcRange,
			})
		}
	}

	trackChildren := []any{
		otioTrack{
			OTIOSchema: "Track.1",
			Name:       "Video",
			Kind:       "Video",
			Children:   videoClips,
		},
	}
	if hasAnyAudio {
		trackChildren = append(trackChildren, otioTrack{
			OTIOSchema: "Track.1",
			Name:       "Audio",
			Kind:       "Audio",
			Children:   audioClips,
		})
	}

	timeline := otioTimeline{
		OTIOSchema: "Timeline.1",
		Name:       video.Title,
		Tracks: otioStack{
			OTIOSchema: "Stack.1",
			Name:       "tracks",
			Children:   trackChildren,
		},
	}

	data, err := json.MarshalIndent(timeline, "", "  ")
	if err != nil {
		logger.Printf("[CapCutService] ExportOTIO: marshal failed: %v", err)
		return nil, fmt.Errorf("marshal otio: %w", err)
	}

	return &ExportResult{
		Data:        data,
		Filename:    projectName + ".otio",
		ContentType: "application/json",
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV 分镜表 — encoding/csv（标准库，Excel / Notion / 制片管理工具通用）
// ─────────────────────────────────────────────────────────────────────────────

// ExportCSV 导出分镜表 CSV（含全部元数据，可直接在 Excel / Notion 中打开）
func (s *CapCutService) ExportCSV(video *model.Video, shots []*model.StoryboardShot) (*ExportResult, error) {
	logger.Printf("[CapCutService] ExportCSV: videoID=%d shots=%d", video.ID, len(shots))
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	var buf bytes.Buffer
	// BOM：让 Excel 正确识别 UTF-8（尤其是中文内容）
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)

	_ = w.Write([]string{
		"shot_no", "description", "dialogue", "narration",
		"duration_s", "camera_type", "shot_size",
		"image_url", "video_url",
	})

	for _, shot := range shots {
		_ = w.Write([]string{
			strconv.Itoa(shot.ShotNo),
			shot.Description,
			shot.Dialogue,
			shot.Narration,
			strconv.FormatFloat(shot.Duration, 'f', 2, 64),
			shot.CameraType,
			shot.ShotSize,
			shot.ImageURL,
			shot.VideoURL,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		logger.Printf("[CapCutService] ExportCSV: flush failed: %v", err)
		return nil, fmt.Errorf("write csv: %w", err)
	}

	return &ExportResult{
		Data:        buf.Bytes(),
		Filename:    projectName + "_shots.csv",
		ContentType: "text/csv; charset=utf-8",
	}, nil
}
