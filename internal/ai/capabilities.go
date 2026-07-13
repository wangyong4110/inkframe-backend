package ai

// VideoEngineTraits captures per-provider quirks used by shot-video generation
// that used to be scattered as `providerName == "..."` checks at call sites.
// Each provider declares its own traits via RegisterVideoEngineTraits from an
// init() in its own file; a provider with no registration gets the zero
// value, i.e. none of the special-cased behavior below.
type VideoEngineTraits struct {
	// SnapsFixedDuration: shot duration must be snapped to 5 or 10 seconds,
	// the only values this engine's API accepts.
	SnapsFixedDuration bool

	// ResolvesModelFromDB: the Model field must come from the tenant's active
	// DB-configured video model rather than a UI override.
	ResolvesModelFromDB bool

	// SupportsMultiImageReference: this engine accepts more than one
	// reference image (character/scene-anchor images appended alongside the
	// main one), not just a single ImageURL.
	SupportsMultiImageReference bool

	// NeedsPerImageAnnotation: the prompt must be prefixed with an explicit
	// "[Image N]为{name}" mapping so the model can tell reference images
	// apart.
	NeedsPerImageAnnotation bool

	// SupportsTemporalLinking: accepts the previous shot's rendered video as
	// a motion reference (VideoGenerateRequest.VideoURLs).
	SupportsTemporalLinking bool

	// SupportsExtendedVideoParams: the API accepts ReturnLastFrame /
	// GenerateAudio / Priority / WebSearchEnabled / SafetyIdentifier beyond
	// the generic request shape.
	SupportsExtendedVideoParams bool

	// DefaultResolution computes the resolution to submit when the caller
	// hasn't forced an explicit request-level override. hdEnabled reflects
	// the project's HD toggle; configuredResolution is the project's saved
	// render-config resolution (may be empty). Nil means "no engine-specific
	// default" — the caller keeps its own fallback (empty string).
	DefaultResolution func(hdEnabled bool, configuredResolution string) string
}

var videoEngineTraits = map[string]VideoEngineTraits{}

// RegisterVideoEngineTraits declares the traits for a video provider name.
// Call this from the owning provider's own file via init().
func RegisterVideoEngineTraits(providerName string, traits VideoEngineTraits) {
	videoEngineTraits[providerName] = traits
}

// VideoEngineTraitsFor returns the registered traits for providerName, or the
// zero value (no special-cased behavior) if none were registered.
func VideoEngineTraitsFor(providerName string) VideoEngineTraits {
	return videoEngineTraits[providerName]
}

// ImageEngineTraits captures per-provider quirks used by image generation
// that used to be scattered as `providerName == "..."` checks. Registered the
// same way as VideoEngineTraits, via each provider's own init().
type ImageEngineTraits struct {
	// Supports2KResolution: the provider has an explicit "2k" resolution mode
	// (passed via GenerateRequest.Extra) requested separately from
	// width/height.
	Supports2KResolution bool

	// SupportsReferenceImage: the provider's ImageGenerate implementation
	// actually forwards ReferenceImage to the API, rather than silently
	// ignoring it.
	SupportsReferenceImage bool

	// SelectModel, if set, picks which model variant to use for a given
	// entry/reference-image/style/consistency-weight combination. Providers
	// without provider-specific model selection leave this nil, and callers
	// keep entry.Model unchanged.
	SelectModel func(entry ImageProviderEntry, referenceImage, style string, consistencyWeight float64) string
}

var imageEngineTraits = map[string]ImageEngineTraits{}

// RegisterImageEngineTraits declares the traits for an image provider name.
// Call this from the owning provider's own file via init().
func RegisterImageEngineTraits(providerName string, traits ImageEngineTraits) {
	imageEngineTraits[providerName] = traits
}

// ImageEngineTraitsFor returns the registered traits for providerName, or the
// zero value (no special-cased behavior) if none were registered.
func ImageEngineTraitsFor(providerName string) ImageEngineTraits {
	return imageEngineTraits[providerName]
}
