package service

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

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
	Audios   []ccAudioMaterial `json:"audios"`
	Stickers []interface{}     `json:"stickers"`
	Texts    []ccTextMaterial  `json:"texts"`
	Videos   []ccVideoMaterial `json:"videos"`
}

type ccCanvasConfig struct {
	Height int    `json:"height"`
	Ratio  string `json:"ratio"`
	Width  int    `json:"width"`
}

type ccDraftContent struct {
	CanvasConfig ccCanvasConfig `json:"canvas_config"`
	CreateTime   int64          `json:"create_time"`
	Duration     int64          `json:"duration"`
	ID           string         `json:"id"`
	Keyframes    ccKeyframes    `json:"keyframes"`
	Materials    ccMaterials    `json:"materials"`
	Name         string         `json:"name"`
	Tracks       []ccTrack      `json:"tracks"`
	Version      string         `json:"version"`
}

type ccMetaInfo struct {
	DraftCover    string `json:"draft_cover"`
	DraftID       string `json:"draft_id"`
	DraftIsAI     bool   `json:"draft_is_ai_shorts"`
	DraftName     string `json:"draft_name"`
	TmDraftCreate int64  `json:"tm_draft_create"`
	TmDraftModify int64  `json:"tm_draft_modified"`
	TmDuration    int64  `json:"tm_duration"`
}

// CapCutExportResult ZIP 导出结果
type CapCutExportResult struct {
	Data     []byte
	Filename string
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
func (s *CapCutService) ExportCapCutDraft(video *model.Video, shots []*model.StoryboardShot, novel *model.Novel) (*CapCutExportResult, error) {
	now := time.Now().Unix()
	draftID := uuid.New().String()
	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

	subCfg := newSubtitleConfig(novel)
	width, height := aspectRatioDimensions(video.AspectRatio)
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

		vidFilename := fmt.Sprintf("shot_%03d%s", shot.ShotNo, ext)
		vidMatID := uuid.New().String()

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
			Path:           "media/" + vidFilename,
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
			audioFilename := fmt.Sprintf("shot_%03d_audio.mp3", shot.ShotNo)
			audMatID := uuid.New().String()

			// Bug3修复：用实际音频时长替代 shot.Duration。
			// CapCut 根据 Material.Duration 和 SourceTimerange.Duration 解析音频播放范围；
			// 若声称时长 > 文件实际时长，CapCut 可能拉伸音频，导致音画不同步。
			actualAudioDur := durationMicros // fallback：读取失败时用视频段时长
			if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
				ext := audioExtension(shot.AudioPath)
				audioFilename = fmt.Sprintf("shot_%03d_audio%s", shot.ShotNo, ext)
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
				FilePath:   "media/" + audioFilename,
				ID:         audMatID,
				Name:       fmt.Sprintf("shot_%03d_audio", shot.ShotNo),
				Type:       "extract_music",
			})

			audioSegments = append(audioSegments, ccSegment{
				Clip: ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
				},
				ID:              uuid.New().String(),
				MaterialID:      audMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: srcDur, Start: 0},    // 实际音频长度
				TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros}, // 时间轴位置与视频对齐
				Type:            "audio",
				Visible:         true,
				Volume:          1.0,
			})
		}

		// ── 3. 音效素材（SFX）────────────────────────────────────
		if shot.SFXURL != "" {
			sfxFilename := fmt.Sprintf("shot_%03d_sfx.mp3", shot.ShotNo)
			sfxMatID := uuid.New().String()

			actualSFXDur := durationMicros
			if data, err := readLocalOrRemoteFile(shot.SFXURL); err == nil && len(data) > 0 {
				ext := audioExtension(shot.SFXURL)
				sfxFilename = fmt.Sprintf("shot_%03d_sfx%s", shot.ShotNo, ext)
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
				FilePath:  "media/" + sfxFilename,
				ID:        sfxMatID,
				Name:      fmt.Sprintf("shot_%03d_sfx", shot.ShotNo),
				Type:      "extract_music",
			})
			sfxSegments = append(sfxSegments, ccSegment{
				Clip: ccClip{
					Alpha: 1.0,
					Scale: ccScale{X: 1.0, Y: 1.0},
				},
				ID:              uuid.New().String(),
				MaterialID:      sfxMatID,
				Speed:           1.0,
				SourceTimerange: ccTimeRange{Duration: sfxSrcDur, Start: 0},
				TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
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

	content := ccDraftContent{
		CanvasConfig: ccCanvasConfig{Height: height, Ratio: video.AspectRatio, Width: width},
		CreateTime:   now,
		Duration:     totalDuration,
		ID:           draftID,
		Keyframes:    ccKeyframes{Videos: allKFGroups},
		Materials: ccMaterials{
			Audios:   append(audioMaterials, sfxMaterials...),
			Stickers: []interface{}{},
			Texts:    textMaterials,
			Videos:   videoMaterials,
		},
		Name:    video.Title,
		Tracks:  tracks,
		Version: "3.0.0",
	}
	if content.Materials.Audios == nil {
		content.Materials.Audios = []ccAudioMaterial{}
	}
	if content.Materials.Texts == nil {
		content.Materials.Texts = []ccTextMaterial{}
	}

	meta := ccMetaInfo{
		DraftCover:    "cover.jpg",
		DraftID:       draftID,
		DraftIsAI:     false,
		DraftName:     video.Title,
		TmDraftCreate: now,
		TmDraftModify: now,
		TmDuration:    totalDuration,
	}

	contentJSON, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal draft content: %w", err)
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
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
	for _, mf := range mediaFiles {
		if err := writeZip(prefix+"media/"+mf.filename, mf.data); err != nil {
			return nil, fmt.Errorf("write media file %s: %w", mf.filename, err)
		}
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}

	return &CapCutExportResult{
		Data:     buf.Bytes(),
		Filename: projectName + ".zip",
	}, nil
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
