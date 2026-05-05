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
)

// CapCutService 剪映草稿导出服务
type CapCutService struct{}

func NewCapCutService() *CapCutService {
	return &CapCutService{}
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
	CurveType    string    `json:"curveType"`
	GraphID      string    `json:"graphID"`
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

type ccKeyframes struct {
	Videos []ccKeyframeGroup `json:"videos"`
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
	Clip            ccClip      `json:"clip"`
	ID              string      `json:"id"`
	KeyframeRefs    []string    `json:"keyframe_refs,omitempty"`
	MaterialID      string      `json:"material_id"`
	Reverse         bool        `json:"reverse"`
	Speed           float64     `json:"speed"`
	SourceTimerange ccTimeRange `json:"source_timerange"`
	TargetTimerange ccTimeRange `json:"target_timerange"`
	Type            string      `json:"type"`
	Visible         bool        `json:"visible"`
	Volume          float64     `json:"volume"`
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
	CartoonPath    string  `json:"cartoon_path"`
	CheckFlag      int     `json:"check_flag"`
	CropScale      float64 `json:"crop_scale"`
	Crop           ccCrop  `json:"crop"`
	Duration       int64   `json:"duration"`
	ExtraInfo      string  `json:"extra_info"`
	HasAudio       bool    `json:"has_audio"`
	Height         int     `json:"height"`
	ID             string  `json:"id"`
	ImportTime     int64   `json:"import_time"`
	MaterialID     string  `json:"material_id"`
	Path           string  `json:"path"`
	SourcePlatform int     `json:"source_platform"`
	Type           string  `json:"type"` // "photo" / "video"
	Width          int     `json:"width"`
}

// ccAudioMaterial 音频素材（配音 / BGM）
type ccAudioMaterial struct {
	CheckFlag      int    `json:"check_flag"`
	Duration       int64  `json:"duration"`
	ExtraInfo      string `json:"extra_info"`
	FilePath       string `json:"file_Path"` // CapCut 格式：大写 P
	ID             string `json:"id"`
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
	Audios             []ccAudioMaterial `json:"audios"`
	Beats              []interface{}     `json:"beats"`
	Canvases           []interface{}     `json:"canvases"`
	Chromas            []interface{}     `json:"chromas"`
	ColorCurves        []interface{}     `json:"color_curves"`
	Filters            []interface{}     `json:"filters"`
	GreenScreens       []interface{}     `json:"green_screens"`
	Masks              []interface{}     `json:"masks"`
	MaterialAnimations []interface{}     `json:"material_animations"`
	Shapes             []interface{}     `json:"shapes"`
	Speed              []interface{}     `json:"speed"`
	Stickers           []interface{}     `json:"stickers"`
	Texts              []ccTextMaterial  `json:"texts"`
	Transitions        []interface{}     `json:"transitions"`
	VideoEffects       []interface{}     `json:"video_effects"`
	Videos             []ccVideoMaterial `json:"videos"`
	VocalSeparations   []interface{}     `json:"vocal_separations"`
}

type ccCanvasConfig struct {
	Height int    `json:"height"`
	Ratio  string `json:"ratio"`
	Width  int    `json:"width"`
}

type ccDraftContent struct {
	CanvasConfig         ccCanvasConfig `json:"canvas_config"`
	CreateTime           int64          `json:"create_time"`
	Duration             int64          `json:"duration"`
	FPS                  float64        `json:"fps"`
	ID                   string         `json:"id"`
	Keyframes            ccKeyframes    `json:"keyframes"`
	LastModifiedPlatform string         `json:"last_modified_platform"`
	Materials            ccMaterials    `json:"materials"`
	Name                 string         `json:"name"`
	NewVersion           string         `json:"new_version"`
	Platform             string         `json:"platform"`
	Relationships        []interface{}  `json:"relationships"`
	Tracks               []ccTrack      `json:"tracks"`
	UpdateTime           int64          `json:"update_time"`
	Version              string         `json:"version"`
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
	if novel.SubtitlePosition != "" {
		cfg.position = novel.SubtitlePosition
	}
	if novel.SubtitleFontSize > 0 {
		cfg.fontSize = novel.SubtitleFontSize
	}
	if novel.SubtitleColor != "" {
		cfg.color = novel.SubtitleColor
	}
	if novel.SubtitleBgStyle != "" {
		cfg.bgStyle = novel.SubtitleBgStyle
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

// buildTextContent 构建剪映字幕素材 content JSON 字符串。
// 剪映要求 style 元素中必须包含 font / useLetterColor / strokes 字段；
// 缺少这些字段时剪映会静默跳过该文字素材，导致字幕不显示。
func buildTextContent(text string, cfg subtitleConfig) string {
	r, g, b := hexToRGBA(cfg.color)
	// 字体大小：剪映使用 pt 单位，约 px / 5
	sizePt := float64(cfg.fontSize) / 5.0

	style := map[string]interface{}{
		"range": []int{0, len([]rune(text))},
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

// ExportCapCutDraft 导出剪映草稿 ZIP（含视频/图片、配音、字幕轨道）
func (s *CapCutService) ExportCapCutDraft(video *model.Video, shots []*model.StoryboardShot, novel *model.Novel) (*ExportResult, error) {
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
	var videoSegments []ccSegment
	allKFGroups := []ccKeyframeGroup{} // 图片分镜的 Ken Burns 运镜关键帧

	var audioMaterials []ccAudioMaterial
	var audioSegments []ccSegment

	var sfxMaterials []ccAudioMaterial // 音效轨道（独立于配音轨道）
	var sfxSegments []ccSegment

	var textMaterials []ccTextMaterial
	var textSegments []ccSegment

	// Bug2修复：按 shot_no 升序排列，确保视频/音频/字幕轨道顺序与分镜编号一致。
	// 数据库返回顺序不保证有序，直接遍历会导致配音顺序与画面顺序错位。
	sort.Slice(shots, func(i, j int) bool {
		return shots[i].ShotNo < shots[j].ShotNo
	})

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

		// 使用 materialID 作为文件名（与剪映自身草稿格式一致），放在草稿根目录而非子目录
		vidMatID := uuid.New().String()
		vidFilename := strings.ReplaceAll(vidMatID, "-", "") + ext

		if mediaURL != "" {
			if data, err := downloadMediaFile(mediaURL); err == nil {
				mediaFiles = append(mediaFiles, mediaFile{filename: vidFilename, data: data})
			}
		}

		videoMaterials = append(videoMaterials, ccVideoMaterial{
			CheckFlag:      63487,
			CropScale:      1,
			Crop:           defaultCrop,
			Duration:       durationMicros,
			HasAudio:       isVideo && shot.AudioPath != "",
			Height:         height,
			ID:             vidMatID,
			ImportTime:     now,
			MaterialID:     vidMatID,
			Path:           vidFilename, // 剪映草稿中媒体文件路径相对于草稿根目录
			SourcePlatform: 0,
			Type:           matType,
			Width:          width,
		})

		segID := uuid.New().String()
		seg := ccSegment{
			Clip: ccClip{
				Alpha: 1.0,
				Scale: ccScale{X: 1.0, Y: 1.0},
			},
			ID:              segID,
			MaterialID:      vidMatID,
			Speed:           1.0,
			SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
			TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
			Type:            "video",
			Visible:         true,
			Volume:          1.0,
		}
		// 图片分镜（非视频）添加 Ken Burns 运镜关键帧
		if !isVideo {
			kfGroups := buildPhotoMotionKeyframes(segID, shot.CameraType, durationMicros, shot.ShotNo)
			for _, g := range kfGroups {
				seg.KeyframeRefs = append(seg.KeyframeRefs, g.ID)
			}
			allKFGroups = append(allKFGroups, kfGroups...)
		}
		videoSegments = append(videoSegments, seg)

		// ── 2. 配音音频素材 ───────────────────────────────────────
		if shot.AudioPath != "" {
			audMatID := uuid.New().String()
			audioFilename := strings.ReplaceAll(audMatID, "-", "") + ".mp3" // 默认扩展名，读取成功后更新

			// Bug3修复：用实际音频时长替代 shot.Duration。
			// CapCut 根据 Material.Duration 和 SourceTimerange.Duration 解析音频播放范围；
			// 若声称时长 > 文件实际时长，CapCut 可能拉伸音频，导致音画不同步。
			actualAudioDur := durationMicros // fallback：读取失败时用视频段时长
			if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
				ext := audioExtension(shot.AudioPath)
				audioFilename = strings.ReplaceAll(audMatID, "-", "") + ext // 使用 materialID 作为文件名
				mediaFiles = append(mediaFiles, mediaFile{filename: audioFilename, data: data})
				if dur := parseAudioDurationMicros(data, ext); dur > 0 {
					actualAudioDur = dur
				}
			}
			// SourceTimerange：告知剪映实际取用的音频长度，不超过视频段时长
			srcDur := actualAudioDur
			if srcDur > durationMicros {
				srcDur = durationMicros // 超出镜头时长的部分截断，避免溢出到下一镜头
			}

			audioMaterials = append(audioMaterials, ccAudioMaterial{
				CheckFlag:  1,
				Duration:   actualAudioDur, // 素材真实时长
				FilePath:   audioFilename,  // 相对于草稿根目录的文件名
				ID:         audMatID,
				Name:       fmt.Sprintf("shot_%03d_audio", shot.ShotNo),
				Type:       "extract_music",
			})

			// Bug修复：TargetTimerange.Duration 必须等于 SourceTimerange.Duration（srcDur）。
			// 若用 durationMicros（视频段时长）而 srcDur < durationMicros，
			// 剪映会将音频拉伸以填满时间槽，导致音频变慢、音画严重不同步。
			audioSegments = append(audioSegments, ccSegment{
				Clip: ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
				},
				ID:              uuid.New().String(),
				MaterialID:      audMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: srcDur, Start: 0},
				TargetTimerange: ccTimeRange{Duration: srcDur, Start: startMicros},
				Type:            "audio",
				Visible:         true,
				Volume:          1.0,
			})
		}

		// ── 3. 音效素材（SFX）────────────────────────────────────
		if shot.SFXURL != "" {
			sfxMatID := uuid.New().String()
			sfxFilename := strings.ReplaceAll(sfxMatID, "-", "") + ".mp3" // 默认扩展名

			actualSFXDur := durationMicros
			if data, err := readLocalOrRemoteFile(shot.SFXURL); err == nil && len(data) > 0 {
				ext := audioExtension(shot.SFXURL)
				sfxFilename = strings.ReplaceAll(sfxMatID, "-", "") + ext // 使用 materialID 作为文件名
				mediaFiles = append(mediaFiles, mediaFile{filename: sfxFilename, data: data})
				if dur := parseAudioDurationMicros(data, ext); dur > 0 {
					actualSFXDur = dur
				}
			}
			sfxSrcDur := actualSFXDur
			if sfxSrcDur > durationMicros {
				sfxSrcDur = durationMicros
			}

			// 混音音量：shot.SFXVolume>0 时使用该值，否则按是否有台词/配音自动估算
			sfxVol := shot.SFXVolume
			if sfxVol <= 0 {
				sfxVol = 0.4
				if shot.Dialogue != "" {
					sfxVol = 0.2
				} else if shot.AudioPath != "" {
					sfxVol = 0.3
				}
			}

			sfxMaterials = append(sfxMaterials, ccAudioMaterial{
				CheckFlag: 1,
				Duration:  actualSFXDur,
				FilePath:  sfxFilename, // 相对于草稿根目录的文件名
				ID:        sfxMatID,
				Name:      fmt.Sprintf("shot_%03d_sfx", shot.ShotNo),
				Type:      "extract_music",
			})
			// Bug修复：SFX TargetTimerange.Duration 必须等于 SourceTimerange.Duration（sfxSrcDur），
			// 否则剪映会拉伸音效填满 durationMicros，导致音效变速、时间线混乱。
			sfxSegments = append(sfxSegments, ccSegment{
				Clip: ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
				},
				ID:              uuid.New().String(),
				MaterialID:      sfxMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: sfxSrcDur, Start: 0},
				TargetTimerange: ccTimeRange{Duration: sfxSrcDur, Start: startMicros},
				Type:            "audio",
				Visible:         true,
				Volume:          sfxVol,
			})
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

			textMaterials = append(textMaterials, ccTextMaterial{
				CheckFlag:  7,
				Content:    buildTextContent(subtitleText, subCfg),
				ID:         txtMatID,
				IsSubtitle: true,
				MaterialID: txtMatID, // 必须与 ID 相同，否则 segment.material_id 引用失效
				Name:       fmt.Sprintf("shot_%03d_subtitle", shot.ShotNo),
				Type:       "text",
			})

			textSegments = append(textSegments, ccSegment{
				Clip: ccClip{
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
				Volume:          1.0,
			})
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
	if audios == nil {
		audios = []ccAudioMaterial{}
	}
	if textMaterials == nil {
		textMaterials = []ccTextMaterial{}
	}
	content := ccDraftContent{
		CanvasConfig:         ccCanvasConfig{Height: height, Ratio: ratio, Width: width},
		CreateTime:           now,
		Duration:             totalDuration,
		FPS:                  30.0,
		ID:                   draftID,
		Keyframes:            ccKeyframes{Videos: allKFGroups},
		LastModifiedPlatform: "mac",
		Materials: ccMaterials{
			Audios:             audios,
			Beats:              []interface{}{},
			Canvases:           []interface{}{},
			Chromas:            []interface{}{},
			ColorCurves:        []interface{}{},
			Filters:            []interface{}{},
			GreenScreens:       []interface{}{},
			Masks:              []interface{}{},
			MaterialAnimations: []interface{}{},
			Shapes:             []interface{}{},
			Speed:              []interface{}{},
			Stickers:           []interface{}{},
			Texts:              textMaterials,
			Transitions:        []interface{}{},
			VideoEffects:       []interface{}{},
			Videos:             videoMaterials,
			VocalSeparations:   []interface{}{},
		},
		Name:          video.Title,
		NewVersion:    "",
		Platform:      "mac",
		Relationships: []interface{}{},
		Tracks:        tracks,
		UpdateTime:    now,
		Version:       "3.0.0",
	}

	meta := ccMetaInfo{
		DraftCover:               "cover.jpg",
		DraftFoldPath:            "",
		DraftID:                  draftID,
		DraftIsAI:                false,
		DraftIsArticleVideo:      false,
		DraftIsInvisible:         false,
		DraftMaterials:           []interface{}{},
		DraftName:                video.Title,
		DraftNewVersion:          "",
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

	// 构建 ZIP
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

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
		return nil, fmt.Errorf("write draft_content.json: %w", err)
	}
	if err := writeZip(prefix+"draft_meta_info.json", metaJSON); err != nil {
		return nil, fmt.Errorf("write draft_meta_info.json: %w", err)
	}

	// draft_virtual_store.json — 部分版本的剪映需要此文件（资源索引），缺失时草稿可能无法识别
	virtualStoreJSON, _ := json.Marshal(map[string]interface{}{"sub_store": map[string]interface{}{}})
	if err := writeZip(prefix+"draft_virtual_store.json", virtualStoreJSON); err != nil {
		return nil, fmt.Errorf("write draft_virtual_store.json: %w", err)
	}

	// 媒体文件放在草稿根目录（与剪映真实草稿格式一致），而非 media/ 子目录
	for _, mf := range mediaFiles {
		if err := writeZip(prefix+mf.filename, mf.data); err != nil {
			return nil, fmt.Errorf("write media file %s: %w", mf.filename, err)
		}
	}

	// SRT 字幕文件（通用格式，可在剪映/其他播放器/字幕工具中独立使用）
	if srtContent := buildSRTSubtitles(shots); srtContent != "" {
		if err := writeZip(prefix+"subtitle.srt", []byte(srtContent)); err != nil {
			return nil, fmt.Errorf("write subtitle.srt: %w", err)
		}
	}

	if err := zw.Close(); err != nil {
		logger.Printf("[CapCutService] ExportCapCutDraft: close zip failed: %v", err)
		return nil, fmt.Errorf("close zip: %w", err)
	}

	result := &ExportResult{
		Data:        buf.Bytes(),
		Filename:    projectName + ".zip",
		ContentType: "application/zip",
	}
	logger.Printf("[CapCutService] ExportCapCutDraft done: filename=%s size=%d", result.Filename, len(result.Data))
	return result, nil
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
	sb.WriteString(fmt.Sprintf(`  <format id="r1" frameDuration="1/25s" width="%d" height="%d"/>`, width, height))
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

	// ── 构建 ZIP ─────────────────────────────────────────────────────────
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
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
		return nil, fmt.Errorf("write fcpxml: %w", err)
	}

	// 媒体文件打包到 media/（供离线重连）
	for _, a := range assets {
		if a.localClip != "" {
			// 本地 Ken Burns 文件（slideshow 模式）直接读取
			if data, err := os.ReadFile(a.localClip); err == nil {
				if e := writeZip(prefix+"media/"+a.filename, data); e != nil {
					return nil, fmt.Errorf("write media %s: %w", a.filename, e)
				}
			}
		} else if a.src != "" && (strings.HasPrefix(a.src, "http://") || strings.HasPrefix(a.src, "https://")) {
			if data, err := downloadMediaFile(a.src); err == nil {
				if e := writeZip(prefix+"media/"+a.filename, data); e != nil {
					return nil, fmt.Errorf("write media %s: %w", a.filename, e)
				}
			}
		}
		// 音频文件：本地或远程均打包进 media/
		if a.audioSrc != "" {
			if data, err := readLocalOrRemoteFile(a.audioSrc); err == nil && len(data) > 0 {
				if e := writeZip(prefix+"media/"+a.audioFile, data); e != nil {
					return nil, fmt.Errorf("write audio %s: %w", a.audioFile, e)
				}
			}
		}
	}

	// SRT 字幕
	if srtContent := buildSRTSubtitles(shots); srtContent != "" {
		if err := writeZip(prefix+"subtitle.srt", []byte(srtContent)); err != nil {
			return nil, fmt.Errorf("write subtitle.srt: %w", err)
		}
	}

	if err := zw.Close(); err != nil {
		logger.Printf("[CapCutService] ExportFCPXML: close zip failed: %v", err)
		return nil, fmt.Errorf("close zip: %w", err)
	}

	result := &ExportResult{
		Data:        buf.Bytes(),
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

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZip := func(name string, data []byte) error {
		w, e := zw.Create(name)
		if e != nil {
			return e
		}
		_, e = w.Write(data)
		return e
	}

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
		vidSrc, vidLocal := shotVideoSource(shot)
		isVideo := vidSrc != ""
		if isVideo {
			filename := fmt.Sprintf("%03d.mp4", shot.ShotNo)
			var data []byte
			var err error
			if vidLocal {
				data, err = os.ReadFile(vidSrc)
			} else {
				data, err = downloadMediaFile(vidSrc)
			}
			if err == nil {
				if e := writeZip("video/"+filename, data); e != nil {
					return nil, fmt.Errorf("write video/%s: %w", filename, e)
				}
				meta.VideoFile = "video/" + filename
			}
		} else if shot.ImageURL != "" {
			filename := fmt.Sprintf("%03d.jpg", shot.ShotNo)
			if data, err := downloadMediaFile(shot.ImageURL); err == nil {
				if e := writeZip("image/"+filename, data); e != nil {
					return nil, fmt.Errorf("write image/%s: %w", filename, e)
				}
				meta.ImageFile = "image/" + filename
			}
		}

		// 音频
		if shot.AudioPath != "" {
			if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
				ext := audioExtension(shot.AudioPath)
				filename := fmt.Sprintf("%03d%s", shot.ShotNo, ext)
				if e := writeZip("audio/"+filename, data); e != nil {
					return nil, fmt.Errorf("write audio/%s: %w", filename, e)
				}
				meta.AudioFile = "audio/" + filename
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
		logger.Printf("[CapCutService] ExportResourceZip: close zip failed: %v", err)
		return nil, fmt.Errorf("close zip: %w", err)
	}

	result := &ExportResult{
		Data:        buf.Bytes(),
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
	// 裸本地路径
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

// downloadMediaFile 从 URL 下载文件，超时 30s
func downloadMediaFile(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
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

// microsToEDLTimecode 将微秒转为 CMX3600 时间码 HH:MM:SS:FF（25fps）
func microsToEDLTimecode(micros int64) string {
	totalFrames := micros * 25 / 1_000_000
	ff := totalFrames % 25
	ss := totalFrames / 25 % 60
	mm := totalFrames / 25 / 60 % 60
	hh := totalFrames / 25 / 3600
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
	const fps = 25.0
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
