package ai

import "testing"

func TestImageRefModeFor(t *testing.T) {
	cases := []struct {
		name         string
		providerName string
		model        string
		want         ImageRefMode
	}{
		{"non-volcengine provider always base64 only (doubao)", "doubao", "any-model", ImageRefModeBase64Only},
		{"non-volcengine provider always base64 only (kling-image)", ProviderNameKlingImage, "kling-v1", ImageRefModeBase64Only},
		{"non-volcengine provider always base64 only (hunyuan-image)", "hunyuan-image", hunyuanImageModelLite, ImageRefModeBase64Only},
		{"empty provider name defaults to base64 only", "", "", ImageRefModeBase64Only},
		{"volcengine DreamO prefers URL", ProviderNameVolcengineVisual, VolcModelDreamO, ImageRefModeURLPreferred},
		{"volcengine SeedEditV3 prefers URL", ProviderNameVolcengineVisual, VolcModelSeedEditV3, ImageRefModeURLPreferred},
		{"volcengine Text2ImgV3 URL only", ProviderNameVolcengineVisual, VolcModelText2ImgV3, ImageRefModeURLOnly},
		{"volcengine PortraitPhoto URL only", ProviderNameVolcengineVisual, VolcModelPortraitPhoto, ImageRefModeURLOnly},
		{"volcengine ImageEffect URL only", ProviderNameVolcengineVisual, VolcModelImageEffect, ImageRefModeURLOnly},
		{"volcengine JimengT2Iv40 URL only", ProviderNameVolcengineVisual, VolcModelJimengT2Iv40, ImageRefModeURLOnly},
		{"volcengine JimengSeedream46 URL only", ProviderNameVolcengineVisual, VolcModelJimengSeedream46, ImageRefModeURLOnly},
		{"volcengine JimengT2Iv30 URL only", ProviderNameVolcengineVisual, VolcModelJimengT2Iv30, ImageRefModeURLOnly},
		{"volcengine JimengT2Iv31 URL only", ProviderNameVolcengineVisual, VolcModelJimengT2Iv31, ImageRefModeURLOnly},
		{"volcengine JimengI2Iv30 URL only", ProviderNameVolcengineVisual, VolcModelJimengI2Iv30, ImageRefModeURLOnly},
		{"volcengine unknown model defaults to URL only", ProviderNameVolcengineVisual, "some-future-model", ImageRefModeURLOnly},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ImageRefModeFor(c.providerName, c.model); got != c.want {
				t.Errorf("ImageRefModeFor(%q, %q) = %v, want %v", c.providerName, c.model, got, c.want)
			}
		})
	}
}

func TestImageRefMode_Constants(t *testing.T) {
	// Guard the iota ordering since ImageRefModeFor callers may rely on the
	// zero value being Base64Only.
	if ImageRefModeBase64Only != 0 {
		t.Errorf("ImageRefModeBase64Only = %d, want 0 (zero value)", ImageRefModeBase64Only)
	}
	if ImageRefModeURLOnly == ImageRefModeBase64Only {
		t.Error("ImageRefModeURLOnly should differ from ImageRefModeBase64Only")
	}
	if ImageRefModeURLPreferred == ImageRefModeBase64Only || ImageRefModeURLPreferred == ImageRefModeURLOnly {
		t.Error("ImageRefModeURLPreferred should be distinct from the other two modes")
	}
}
