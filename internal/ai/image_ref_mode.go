package ai

// ImageRefMode describes how a provider+model combination expects reference
// images to be delivered, replacing scattered `providerName == "..."` checks
// at call sites with a single declared property of the provider/model.
type ImageRefMode int

const (
	// ImageRefModeBase64Only: the provider cannot reach our (often private/signed)
	// storage URLs, so reference images must always be sent as base64.
	ImageRefModeBase64Only ImageRefMode = iota
	// ImageRefModeURLOnly: the provider/model only accepts public HTTP(S) image
	// URLs and does not support base64-encoded reference images.
	ImageRefModeURLOnly
	// ImageRefModeURLPreferred: the provider/model prefers a public HTTP(S) URL
	// when one is available, falling back to base64 otherwise.
	ImageRefModeURLPreferred
)

// ImageRefModeFor reports how the given provider+model wants reference images
// delivered:
//   - 非 volcengine-visual 提供商（doubao/kling-image 等）无法访问我们的私有/签名
//     URL，必须使用 base64。
//   - volcengine-visual 的 DreamO/SeedEditV3 优先使用可公开访问的 URL，没有 URL
//     时退回 base64。
//   - volcengine-visual 的其余模型（新一代 Jimeng 系列，如 T2Iv40/Seedream46）
//     只接受 image_urls，不支持 base64。
func ImageRefModeFor(providerName, model string) ImageRefMode {
	if providerName != ProviderNameVolcengineVisual {
		return ImageRefModeBase64Only
	}
	if model == VolcModelDreamO || model == VolcModelSeedEditV3 {
		return ImageRefModeURLPreferred
	}
	return ImageRefModeURLOnly
}
