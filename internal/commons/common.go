package commons

type ModelType string

const (
	SFX       ModelType = "sfx"
	Voice     ModelType = "voice"
	Image     ModelType = "image"
	Video     ModelType = "video"
	LLM       ModelType = "llm"
	Embedding ModelType = "embedding"
)
