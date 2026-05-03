package service

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
type ccTextMaterial struct {
	CheckFlag int    `json:"check_flag"`
	Content   string `json:"content"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "text"
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
		enabled:  true,
		position: "bottom",
		fontSize: 48,
		color:    "#FFFFFF",
		bgStyle:  "shadow",
	}
	if novel == nil {
		return cfg
	}
	cfg.enabled = novel.SubtitleEnabled
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

// buildTextContent 构建剪映字幕素材 content JSON 字符串
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
		"size": sizePt,
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
			"style": 2,
		}
	}

	obj := map[string]interface{}{
		"styles": []interface{}{style},
		"text":   text,
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

	// 三类轨道数据
	var videoMaterials []ccVideoMaterial
	var videoSegments []ccSegment

	var audioMaterials []ccAudioMaterial
	var audioSegments []ccSegment

	var textMaterials []ccTextMaterial
	var textSegments []ccSegment

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

		videoSegments = append(videoSegments, ccSegment{
			Clip: ccClip{
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
		})

		// ── 2. 配音音频素材 ───────────────────────────────────────
		if shot.AudioPath != "" {
			audioFilename := fmt.Sprintf("shot_%03d_audio.mp3", shot.ShotNo)
			audMatID := uuid.New().String()

			if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
				// 根据实际内容决定后缀
				ext := audioExtension(shot.AudioPath)
				audioFilename = fmt.Sprintf("shot_%03d_audio%s", shot.ShotNo, ext)
				mediaFiles = append(mediaFiles, mediaFile{filename: audioFilename, data: data})
			}

			audioMaterials = append(audioMaterials, ccAudioMaterial{
				CheckFlag:  1,
				Duration:   durationMicros,
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
				SourceTimerange: ccTimeRange{Duration: durationMicros, Start: 0},
				TargetTimerange: ccTimeRange{Duration: durationMicros, Start: startMicros},
				Type:            "audio",
				Visible:         true,
				Volume:          1.0,
			})
		}

		// ── 3. 字幕文字素材 ───────────────────────────────────────
		subtitleText := shot.Dialogue
		if subtitleText == "" {
			subtitleText = shot.Description
		}
		if subtitleText != "" && subCfg.enabled {
			txtMatID := uuid.New().String()

			textMaterials = append(textMaterials, ccTextMaterial{
				CheckFlag: 7,
				Content:   buildTextContent(subtitleText, subCfg),
				ID:        txtMatID,
				Name:      fmt.Sprintf("shot_%03d_subtitle", shot.ShotNo),
				Type:      "text",
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
		Materials: ccMaterials{
			Audios:   audioMaterials,
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
