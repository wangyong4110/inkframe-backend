package ai

import (
	"context"
	"os"
	"testing"
	"time"
)

// ─── Pure function tests: parseSizeWH ──────────────────────────────────────

func TestParseSizeWH(t *testing.T) {
	cases := []struct {
		name       string
		size       string
		wantWidth  int
		wantHeight int
	}{
		{"empty defaults to 1328x1328", "", 1328, 1328},
		{"explicit WxH", "1024x1024", 1024, 1024},
		{"explicit non-square WxH", "1920x1080", 1920, 1080},
		{"aspect ratio 1:1", "1:1", 1328, 1328},
		{"aspect ratio 16:9 (wide)", "16:9", 1328, 1328 * 9 / 16},
		{"aspect ratio 9:16 (tall)", "9:16", 1328 * 9 / 16, 1328},
		{"aspect ratio 4:3", "4:3", 1328, 1328 * 3 / 4},
		{"invalid format falls back to default", "not-a-size", 1328, 1328},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := parseSizeWH(c.size)
			if w != c.wantWidth || h != c.wantHeight {
				t.Errorf("parseSizeWH(%q) = (%d,%d), want (%d,%d)", c.size, w, h, c.wantWidth, c.wantHeight)
			}
		})
	}
}

// ─── Pure function tests: pickSingleRef / pickMultiRef ─────────────────────

func TestPickSingleRef(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		image string
		want  string
	}{
		{"url present takes priority", "http://example.com/a.png", "base64data", "http://example.com/a.png"},
		{"url empty falls back to image", "", "base64data", "base64data"},
		{"both empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickSingleRef(c.url, c.image); got != c.want {
				t.Errorf("pickSingleRef(%q, %q) = %q, want %q", c.url, c.image, got, c.want)
			}
		})
	}
}

func TestPickMultiRef(t *testing.T) {
	urls := []string{"http://a.com/1.png", "http://a.com/2.png"}
	images := []string{"b64-1", "b64-2"}

	if got := pickMultiRef(urls, images); len(got) != 2 || got[0] != urls[0] {
		t.Errorf("pickMultiRef with non-empty urls = %v, want %v", got, urls)
	}
	if got := pickMultiRef(nil, images); len(got) != 2 || got[0] != images[0] {
		t.Errorf("pickMultiRef with empty urls should fall back to images, got %v, want %v", got, images)
	}
	if got := pickMultiRef(nil, nil); len(got) != 0 {
		t.Errorf("pickMultiRef with both empty = %v, want empty", got)
	}
}

// ─── setImageInput / setMultiImageInput ────────────────────────────────────

func TestVolcengineVisualProvider_SetImageInput(t *testing.T) {
	p := &VolcengineVisualProvider{}

	t.Run("empty image is a no-op", func(t *testing.T) {
		params := map[string]interface{}{}
		p.setImageInput(params, "", "image_urls", "binary_data_base64")
		if len(params) != 0 {
			t.Errorf("expected no params set, got %v", params)
		}
	})

	t.Run("http url goes to url field", func(t *testing.T) {
		params := map[string]interface{}{}
		p.setImageInput(params, "http://example.com/a.png", "image_urls", "binary_data_base64")
		urls, ok := params["image_urls"].([]string)
		if !ok || len(urls) != 1 || urls[0] != "http://example.com/a.png" {
			t.Errorf("expected image_urls=[http://example.com/a.png], got %v", params)
		}
		if _, ok := params["binary_data_base64"]; ok {
			t.Errorf("did not expect binary_data_base64 to be set, got %v", params)
		}
	})

	t.Run("https url goes to url field", func(t *testing.T) {
		params := map[string]interface{}{}
		p.setImageInput(params, "https://example.com/a.png", "image_urls", "binary_data_base64")
		urls, ok := params["image_urls"].([]string)
		if !ok || len(urls) != 1 {
			t.Errorf("expected image_urls set, got %v", params)
		}
	})

	t.Run("base64 goes to b64 field", func(t *testing.T) {
		params := map[string]interface{}{}
		p.setImageInput(params, "iVBORw0KGgoAAAANSUhEUgAA", "image_urls", "binary_data_base64")
		b64, ok := params["binary_data_base64"].([]string)
		if !ok || len(b64) != 1 || b64[0] != "iVBORw0KGgoAAAANSUhEUgAA" {
			t.Errorf("expected binary_data_base64 set, got %v", params)
		}
		if _, ok := params["image_urls"]; ok {
			t.Errorf("did not expect image_urls to be set, got %v", params)
		}
	})
}

func TestVolcengineVisualProvider_SetMultiImageInput(t *testing.T) {
	p := &VolcengineVisualProvider{}

	t.Run("mixed urls and base64 split correctly", func(t *testing.T) {
		params := map[string]interface{}{}
		images := []string{"http://example.com/1.png", "b64data1", "https://example.com/2.png", "", "b64data2"}
		p.setMultiImageInput(params, images, "image_urls", "binary_data_base64")

		urls, ok := params["image_urls"].([]string)
		if !ok || len(urls) != 2 {
			t.Fatalf("expected 2 image_urls, got %v", params["image_urls"])
		}
		b64s, ok := params["binary_data_base64"].([]string)
		if !ok || len(b64s) != 2 {
			t.Fatalf("expected 2 binary_data_base64, got %v", params["binary_data_base64"])
		}
	})

	t.Run("all empty produces no fields", func(t *testing.T) {
		params := map[string]interface{}{}
		p.setMultiImageInput(params, []string{"", ""}, "image_urls", "binary_data_base64")
		if len(params) != 0 {
			t.Errorf("expected no params set, got %v", params)
		}
	})

	t.Run("only urls", func(t *testing.T) {
		params := map[string]interface{}{}
		p.setMultiImageInput(params, []string{"http://a.com/1.png"}, "image_urls", "binary_data_base64")
		if _, ok := params["binary_data_base64"]; ok {
			t.Errorf("did not expect binary_data_base64 field, got %v", params)
		}
	})
}

// ─── buildSubmitParams (per-model request building; no network) ───────────

func TestVolcengineVisualProvider_BuildSubmitParams(t *testing.T) {
	p := &VolcengineVisualProvider{}

	t.Run("Text2ImgV3 basic fields", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt:         "a cat",
			NegativePrompt: "blurry",
			Size:           "1024x1024",
			CFGScale:       7.5,
			Seed:           42,
		}
		params := p.buildSubmitParams(VolcModelText2ImgV3, req)
		if params["req_key"] != VolcModelText2ImgV3 {
			t.Errorf("req_key = %v, want %v", params["req_key"], VolcModelText2ImgV3)
		}
		if params["prompt"] != "a cat" {
			t.Errorf("prompt = %v, want 'a cat'", params["prompt"])
		}
		if params["negative_prompt"] != "blurry" {
			t.Errorf("negative_prompt = %v, want 'blurry'", params["negative_prompt"])
		}
		if params["width"] != 1024 || params["height"] != 1024 {
			t.Errorf("width/height = %v/%v, want 1024/1024", params["width"], params["height"])
		}
		if params["scale"] != 7.5 {
			t.Errorf("scale = %v, want 7.5", params["scale"])
		}
		if params["seed"] != int64(42) {
			t.Errorf("seed = %v, want 42", params["seed"])
		}
	})

	t.Run("seed defaults to -1 when zero", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "x"}
		params := p.buildSubmitParams(VolcModelText2ImgV3, req)
		if params["seed"] != int64(-1) {
			t.Errorf("seed = %v, want -1", params["seed"])
		}
	})

	t.Run("JimengT2Iv30/v31 use_pre_llm defaults false and negative_prompt/size optional", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "a dog"}
		params := p.buildSubmitParams(VolcModelJimengT2Iv30, req)
		if params["use_pre_llm"] != false {
			t.Errorf("use_pre_llm = %v, want false", params["use_pre_llm"])
		}
		if _, ok := params["width"]; ok {
			t.Errorf("width should not be set when Size is empty, got %v", params)
		}
	})

	t.Run("JimengT2Iv31 use_pre_llm overridden via Extra", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt: "a dog",
			Extra:  map[string]interface{}{"use_pre_llm": true},
		}
		params := p.buildSubmitParams(VolcModelJimengT2Iv31, req)
		if params["use_pre_llm"] != true {
			t.Errorf("use_pre_llm = %v, want true (from Extra)", params["use_pre_llm"])
		}
	})

	t.Run("PortraitPhoto uses single ref via image_input", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt:        "portrait",
			ReferenceURL:  "http://example.com/ref.png",
			ReferenceImage: "b64fallback",
		}
		params := p.buildSubmitParams(VolcModelPortraitPhoto, req)
		if params["image_input"] != "http://example.com/ref.png" {
			t.Errorf("image_input = %v, want ReferenceURL to take priority", params["image_input"])
		}
	})

	t.Run("JimengI2Iv30 scale inverse mapping from CFGScale", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt:       "edit",
			ReferenceURL: "http://example.com/ref.png",
			CFGScale:     5.5, // (5.5-1)/9 = 0.5
		}
		params := p.buildSubmitParams(VolcModelJimengI2Iv30, req)
		scale, ok := params["scale"].(float64)
		if !ok {
			t.Fatalf("scale not set or wrong type: %v", params["scale"])
		}
		if scale < 0.49 || scale > 0.51 {
			t.Errorf("scale = %v, want ~0.5", scale)
		}
	})

	t.Run("JimengI2Iv30 scale clamps to [0,1]", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "edit", ReferenceURL: "http://x.com/r.png", CFGScale: 100}
		params := p.buildSubmitParams(VolcModelJimengI2Iv30, req)
		scale := params["scale"].(float64)
		if scale != 1.0 {
			t.Errorf("scale = %v, want clamped to 1.0", scale)
		}
	})

	t.Run("SeedEditV3 prefers multi-ref over single", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt:        "edit",
			ReferenceURLs: []string{"http://a.com/1.png", "http://a.com/2.png"},
			ReferenceURL:  "http://a.com/single.png",
		}
		params := p.buildSubmitParams(VolcModelSeedEditV3, req)
		urls, ok := params["image_urls"].([]string)
		if !ok || len(urls) != 2 {
			t.Errorf("expected 2 image_urls from ReferenceURLs, got %v", params["image_urls"])
		}
	})

	t.Run("SeedEditV3 falls back to single ref when no multi", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "edit", ReferenceURL: "http://a.com/single.png"}
		params := p.buildSubmitParams(VolcModelSeedEditV3, req)
		urls, ok := params["image_urls"].([]string)
		if !ok || len(urls) != 1 || urls[0] != "http://a.com/single.png" {
			t.Errorf("expected single image_urls entry, got %v", params["image_urls"])
		}
	})

	t.Run("DreamO width/height always set", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "character", Size: "512x768"}
		params := p.buildSubmitParams(VolcModelDreamO, req)
		if params["width"] != 512 || params["height"] != 768 {
			t.Errorf("width/height = %v/%v, want 512/768", params["width"], params["height"])
		}
	})

	t.Run("ImageEffect sets template_id from Style", func(t *testing.T) {
		req := &ImageGenerateRequest{
			ReferenceURL: "http://a.com/1.png",
			Style:        "template-123",
		}
		params := p.buildSubmitParams(VolcModelImageEffect, req)
		if params["template_id"] != "template-123" {
			t.Errorf("template_id = %v, want template-123", params["template_id"])
		}
		if params["image_input1"] != "http://a.com/1.png" {
			t.Errorf("image_input1 = %v, want http://a.com/1.png", params["image_input1"])
		}
	})

	t.Run("JimengT2Iv40 collects URLs from ReferenceURLs, dedups, caps at 10", func(t *testing.T) {
		var urls []string
		for i := 0; i < 15; i++ {
			urls = append(urls, "http://a.com/dup.png") // all duplicates
		}
		req := &ImageGenerateRequest{Prompt: "x", ReferenceURLs: urls}
		params := p.buildSubmitParams(VolcModelJimengT2Iv40, req)
		got, ok := params["image_urls"].([]string)
		if !ok || len(got) != 1 {
			t.Errorf("expected dedup to 1 url, got %v", params["image_urls"])
		}
	})

	t.Run("JimengT2Iv40 caps at 10 distinct urls", func(t *testing.T) {
		var urls []string
		for i := 0; i < 15; i++ {
			urls = append(urls, "http://a.com/img.png?"+string(rune('a'+i)))
		}
		req := &ImageGenerateRequest{Prompt: "x", ReferenceURLs: urls}
		params := p.buildSubmitParams(VolcModelJimengT2Iv40, req)
		got, ok := params["image_urls"].([]string)
		if !ok || len(got) != 10 {
			t.Errorf("expected cap at 10 urls, got %d", len(got))
		}
	})

	t.Run("JimengT2Iv40 falls back to single fields when ReferenceURLs empty, filters non-http", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt:          "x",
			ReferenceURL:    "http://a.com/1.png",
			ReferenceImage:  "/local/relative/path.png", // not http(s), should be filtered
			ReferenceImages: []string{"https://a.com/2.png"},
		}
		params := p.buildSubmitParams(VolcModelJimengT2Iv40, req)
		got, ok := params["image_urls"].([]string)
		if !ok || len(got) != 2 {
			t.Errorf("expected 2 http urls (local path filtered), got %v", params["image_urls"])
		}
	})

	t.Run("JimengT2Iv40 Extra passthrough for force_single/min_ratio/max_ratio", func(t *testing.T) {
		req := &ImageGenerateRequest{
			Prompt: "x",
			Extra: map[string]interface{}{
				"force_single": true,
				"min_ratio":    0.5,
				"max_ratio":    2.0,
			},
		}
		params := p.buildSubmitParams(VolcModelJimengT2Iv40, req)
		if params["force_single"] != true {
			t.Errorf("force_single = %v, want true", params["force_single"])
		}
		if params["min_ratio"] != 0.5 {
			t.Errorf("min_ratio = %v, want 0.5", params["min_ratio"])
		}
		if params["max_ratio"] != 2.0 {
			t.Errorf("max_ratio = %v, want 2.0", params["max_ratio"])
		}
	})

	t.Run("JimengSeedream46 scale int mapping from CFGScale", func(t *testing.T) {
		// cfgScale=7.5 -> (6.5/9)*99+1 ≈ 72.5 -> int() truncates to 72 -> +1(already added) recompute
		req := &ImageGenerateRequest{Prompt: "x", CFGScale: 7.5}
		params := p.buildSubmitParams(VolcModelJimengSeedream46, req)
		scale, ok := params["scale"].(int)
		if !ok {
			t.Fatalf("scale not int: %v (%T)", params["scale"], params["scale"])
		}
		if scale < 1 || scale > 100 {
			t.Errorf("scale = %d, want within [1,100]", scale)
		}
	})

	t.Run("JimengSeedream46 caps at 14 urls", func(t *testing.T) {
		var urls []string
		for i := 0; i < 20; i++ {
			urls = append(urls, "http://a.com/img.png?"+string(rune('a'+i)))
		}
		req := &ImageGenerateRequest{Prompt: "x", ReferenceURLs: urls}
		params := p.buildSubmitParams(VolcModelJimengSeedream46, req)
		got, ok := params["image_urls"].([]string)
		if !ok || len(got) != 14 {
			t.Errorf("expected cap at 14 urls, got %d", len(got))
		}
	})

	t.Run("JimengSeedream46 scale clamps to [1,100]", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "x", CFGScale: -5}
		params := p.buildSubmitParams(VolcModelJimengSeedream46, req)
		// CFGScale<=0 means the `if req.CFGScale > 0` branch is skipped entirely.
		if _, ok := params["scale"]; ok {
			t.Errorf("scale should not be set for non-positive CFGScale, got %v", params["scale"])
		}
	})

	t.Run("unknown model key returns only base fields", func(t *testing.T) {
		req := &ImageGenerateRequest{Prompt: "x"}
		params := p.buildSubmitParams("unknown-model-key", req)
		if params["req_key"] != "unknown-model-key" {
			t.Errorf("req_key = %v, want unknown-model-key", params["req_key"])
		}
		if _, ok := params["prompt"]; ok {
			t.Errorf("prompt should not be set for unrecognized model, got %v", params)
		}
	})
}

// ─── GetName / GetModels / constructor ─────────────────────────────────────

func TestNewVolcengineVisualProvider(t *testing.T) {
	p := NewVolcengineVisualProvider("ak", "sk")
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.svc == nil {
		t.Fatal("expected non-nil underlying SDK client")
	}
}

func TestVolcengineVisualProvider_GetName(t *testing.T) {
	p := NewVolcengineVisualProvider("ak", "sk")
	if got := p.GetName(); got != ProviderNameVolcengineVisual {
		t.Errorf("GetName() = %q, want %q", got, ProviderNameVolcengineVisual)
	}
}

func TestVolcengineVisualProvider_GetModels(t *testing.T) {
	p := NewVolcengineVisualProvider("ak", "sk")
	models := p.GetModels()
	if len(models) == 0 {
		t.Fatal("expected non-empty models list")
	}
	want := map[string]bool{
		VolcModelJimengSeedream46: true,
		VolcModelJimengT2Iv40:     true,
		VolcModelJimengI2Iv30:     true,
		VolcModelJimengT2Iv31:     true,
		VolcModelJimengT2Iv30:     true,
		VolcModelText2ImgV3:       true,
		VolcModelPortraitPhoto:    true,
		VolcModelSeedEditV3:       true,
		VolcModelDreamO:           true,
		VolcModelImageEffect:      true,
	}
	got := map[string]bool{}
	for _, m := range models {
		got[m] = true
	}
	for m := range want {
		if !got[m] {
			t.Errorf("GetModels() missing expected model %q", m)
		}
	}
}

// ─── Unsupported AIProvider methods ────────────────────────────────────────

func TestVolcengineVisualProvider_UnsupportedMethods(t *testing.T) {
	p := NewVolcengineVisualProvider("ak", "sk")
	ctx := context.Background()

	if _, err := p.Generate(ctx, &GenerateRequest{}); err == nil {
		t.Error("Generate() expected error, got nil")
	}
	if _, err := p.GenerateStream(ctx, &GenerateRequest{}); err == nil {
		t.Error("GenerateStream() expected error, got nil")
	}
	if _, err := p.Embed(ctx, "text"); err == nil {
		t.Error("Embed() expected error, got nil")
	}
	if _, err := p.AudioGenerate(ctx, &AudioGenerateRequest{}); err == nil {
		t.Error("AudioGenerate() expected error, got nil")
	}
}

// ─── Pure function tests: isRealisticStyle ─────────────────────────────────

func TestIsRealisticStyle(t *testing.T) {
	cases := []struct {
		name  string
		style string
		want  bool
	}{
		{"empty", "", false},
		{"realistic", "realistic", true},
		{"real_person", "real_person", true},
		{"photorealistic", "photorealistic", true},
		{"photography substring", "fashion photography", true},
		{"chinese 写实", "写实风格", true},
		{"chinese 真实", "真实感", true},
		{"chinese 摄影", "人像摄影", true},
		{"chinese 真人", "真人风格", true},
		{"anime", "anime", false},
		{"illustration", "illustration", false},
		{"case-insensitive", "REALISTIC", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRealisticStyle(c.style); got != c.want {
				t.Errorf("isRealisticStyle(%q) = %v, want %v", c.style, got, c.want)
			}
		})
	}
}

// ─── Pure function tests: selectVolcengineImageModel ───────────────────────

func TestSelectVolcengineImageModel_SingleRealisticRef_UsesPortraitPhoto(t *testing.T) {
	entry := ImageProviderEntry{ProviderName: ProviderNameVolcengineVisual, Model: ""}
	got := selectVolcengineImageModel(entry, 1, "realistic", 0.5)
	if got != VolcModelPortraitPhoto {
		t.Errorf("expected VolcModelPortraitPhoto for single realistic ref, got %s", got)
	}
}

func TestSelectVolcengineImageModel_MultiRealisticRef_AvoidsPortraitPhoto(t *testing.T) {
	// 两张参考图（如"角色+场景"分镜合成）+ 写实风格：PortraitPhoto 是单图模型，
	// 不能再选它（会静默丢弃第二张参考图），必须落到支持多图的 DreamO/SeedEditV3。
	entry := ImageProviderEntry{ProviderName: ProviderNameVolcengineVisual, Model: ""}

	gotHighWeight := selectVolcengineImageModel(entry, 2, "realistic", 0.8)
	if gotHighWeight != VolcModelDreamO {
		t.Errorf("expected VolcModelDreamO for 2 refs + high consistency weight, got %s", gotHighWeight)
	}

	gotLowWeight := selectVolcengineImageModel(entry, 2, "realistic", 0.4)
	if gotLowWeight != VolcModelSeedEditV3 {
		t.Errorf("expected VolcModelSeedEditV3 for 2 refs + low consistency weight, got %s", gotLowWeight)
	}

	if gotHighWeight == VolcModelPortraitPhoto || gotLowWeight == VolcModelPortraitPhoto {
		t.Fatal("multi-reference realistic-style call must never resolve to the single-image PortraitPhoto model")
	}
}

func TestSelectVolcengineImageModel_NoRef_Unaffected(t *testing.T) {
	entry := ImageProviderEntry{ProviderName: ProviderNameVolcengineVisual, Model: ""}
	got := selectVolcengineImageModel(entry, 0, "realistic", 0.5)
	if got != VolcModelText2ImgV3 {
		t.Errorf("expected VolcModelText2ImgV3 for no-reference realistic call, got %s", got)
	}
}

func TestSelectVolcengineImageModel_NoRef_NonRealisticStyle(t *testing.T) {
	entry := ImageProviderEntry{ProviderName: ProviderNameVolcengineVisual, Model: ""}
	got := selectVolcengineImageModel(entry, 0, "anime", 0.5)
	if got != VolcModelJimengT2Iv31 {
		t.Errorf("expected VolcModelJimengT2Iv31 for no-reference non-realistic call, got %s", got)
	}
}

func TestSelectVolcengineImageModel_NewGenModelUnchanged(t *testing.T) {
	// 新一代即梦模型（4.0/4.6等）由用户显式配置，任何参考图数量下都不应被覆盖。
	entry := ImageProviderEntry{ProviderName: ProviderNameVolcengineVisual, Model: VolcModelJimengSeedream46}
	got := selectVolcengineImageModel(entry, 2, "realistic", 0.5)
	if got != VolcModelJimengSeedream46 {
		t.Errorf("expected new-gen model to remain unchanged regardless of ref count, got %s", got)
	}
}

// ─── ImageEngineTraits registration ────────────────────────────────────────

func TestVolcengineVisual_ImageEngineTraitsRegistered(t *testing.T) {
	traits := ImageEngineTraitsFor(ProviderNameVolcengineVisual)
	if !traits.SupportsReferenceImage {
		t.Error("expected SupportsReferenceImage=true for volcengine-visual")
	}
	// SelectModel must be registered: without it, a model tuned for single-image editing
	// (e.g. PortraitPhoto) could be selected for a multi-reference-image call and silently
	// drop every reference image past the first — this was a confirmed production bug.
	if traits.SelectModel == nil {
		t.Fatal("expected SelectModel to be registered for volcengine-visual")
	}
	got := traits.SelectModel(ImageProviderEntry{ProviderName: ProviderNameVolcengineVisual}, 2, "realistic", 0.8)
	if got != VolcModelDreamO {
		t.Errorf("traits.SelectModel(2 refs, realistic, 0.8) = %s, want %s", got, VolcModelDreamO)
	}
}

// ─── Real network call (env-gated) ─────────────────────────────────────────

// TestVolcengineVisualProvider_RealCall exercises ImageGenerate end-to-end
// against the live Volcengine API. Requires VOLCENGINE_ACCESS_KEY and
// VOLCENGINE_SECRET_KEY (or VOLC_ACCESS_KEY/VOLC_SECRET_KEY) to be set; skips
// otherwise.
func TestVolcengineVisualProvider_RealCall(t *testing.T) {
	ak := firstNonEmptyEnv("VOLCENGINE_ACCESS_KEY", "VOLC_ACCESS_KEY")
	sk := firstNonEmptyEnv("VOLCENGINE_SECRET_KEY", "VOLC_SECRET_KEY")
	if ak == "" || sk == "" {
		t.Skip("VOLCENGINE_ACCESS_KEY/VOLCENGINE_SECRET_KEY not set; skipping real API call")
	}

	p := NewVolcengineVisualProvider(ak, sk)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	resp, err := p.ImageGenerate(ctx, &ImageGenerateRequest{
		Model:  VolcModelText2ImgV3,
		Prompt: "a small red apple on a white table, studio lighting",
		Size:   "512x512",
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("ImageGenerate returned response-level error: %s", resp.Error)
	}
	if resp.URL == "" {
		t.Error("expected non-empty image URL in response")
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
