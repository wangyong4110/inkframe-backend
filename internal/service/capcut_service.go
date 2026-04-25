package service

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	Type           string  `json:"type"` // "photo" for image, "video" for video
	Width          int     `json:"width"`
}

type ccMaterials struct {
	Audios   []interface{}     `json:"audios"`
	Stickers []interface{}     `json:"stickers"`
	Texts    []interface{}     `json:"texts"`
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

// ExportCapCutDraft 导出剪映草稿 ZIP
func (s *CapCutService) ExportCapCutDraft(video *model.Video, shots []*model.StoryboardShot) (*CapCutExportResult, error) {
	now := time.Now().Unix()
	draftID := uuid.New().String()
	projectName := sanitizeFilename(video.Title)
	if projectName == "" {
		projectName = fmt.Sprintf("video_%d", video.ID)
	}

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
	var videoMaterials []ccVideoMaterial
	var segments []ccSegment
	var totalDuration int64

	for _, shot := range shots {
		// 确定媒体 URL 和类型
		mediaURL := shot.ImageURL
		isVideo := false
		if shot.VideoURL != "" {
			mediaURL = shot.VideoURL
			isVideo = true
		}

		durationMicros := int64(shot.Duration * 1_000_000)
		if durationMicros <= 0 {
			durationMicros = 5_000_000 // 默认5秒
		}

		ext := ".jpg"
		matType := "photo"
		if isVideo {
			ext = ".mp4"
			matType = "video"
		}

		filename := fmt.Sprintf("shot_%03d%s", shot.ShotNo, ext)
		matID := uuid.New().String()

		// 下载媒体文件（失败不阻断整体导出）
		var fileData []byte
		if mediaURL != "" {
			data, err := downloadMediaFile(mediaURL)
			if err == nil {
				fileData = data
			}
		}
		if fileData != nil {
			mediaFiles = append(mediaFiles, mediaFile{filename: filename, data: fileData})
		}

		mat := ccVideoMaterial{
			CheckFlag:      63487,
			CropScale:      1,
			Crop:           defaultCrop,
			Duration:       durationMicros,
			HasAudio:       isVideo && shot.AudioPath != "",
			Height:         height,
			ID:             matID,
			ImportTime:     now,
			MaterialID:     matID,
			Path:           "media/" + filename,
			SourcePlatform: 0,
			Type:           matType,
			Width:          width,
		}
		videoMaterials = append(videoMaterials, mat)

		seg := ccSegment{
			Clip: ccClip{
				Alpha:     1.0,
				Flip:      ccFlip{},
				Rotation:  0,
				Scale:     ccScale{X: 1.0, Y: 1.0},
				Transform: ccTransform{},
			},
			ID:         uuid.New().String(),
			MaterialID: matID,
			Reverse:    false,
			Speed:      1.0,
			SourceTimerange: ccTimeRange{
				Duration: durationMicros,
				Start:    0,
			},
			TargetTimerange: ccTimeRange{
				Duration: durationMicros,
				Start:    totalDuration,
			},
			Type:    "video",
			Visible: true,
			Volume:  1.0,
		}
		segments = append(segments, seg)
		totalDuration += durationMicros
	}

	content := ccDraftContent{
		CanvasConfig: ccCanvasConfig{
			Height: height,
			Ratio:  video.AspectRatio,
			Width:  width,
		},
		CreateTime: now,
		Duration:   totalDuration,
		ID:         draftID,
		Materials: ccMaterials{
			Audios:   []interface{}{},
			Stickers: []interface{}{},
			Texts:    []interface{}{},
			Videos:   videoMaterials,
		},
		Name: video.Title,
		Tracks: []ccTrack{
			{
				Attribute:     0,
				Flag:          0,
				ID:            uuid.New().String(),
				IsDefaultName: true,
				Name:          "",
				Segments:      segments,
				Type:          "video",
			},
		},
		Version: "3.0.0",
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
