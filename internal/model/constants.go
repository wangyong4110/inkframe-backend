package model

// Novel / Chapter status constants
const (
	StatusDraft      = "draft"
	StatusPlanning   = "planning"
	StatusGenerating = "generating"
	StatusCompleted  = "completed"
	StatusPending    = "pending"
	StatusFailed     = "failed"
	StatusRetry      = "retry"
	StatusPublished  = "published"

	// Video / Shot / Storyboard status
	StatusProcessing = "processing"
	StatusStoryboard = "storyboard"
	StatusDone       = "done"

	// TenantUser roles
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleViewer = "viewer"

	// AI task types (TaskModelConfig.TaskType)
	TaskTypeChapterGen  = "chapter_generation"
	TaskTypeOutlineGen  = "outline_generation"
	TaskTypeStoryboard  = "storyboard"
	TaskTypeCharProfile = "character_profile"
	TaskTypeQuality     = "quality_check"
	TaskTypeSummary     = "summary"

	// Default AI parameters
	DefaultTemperature = 0.7
	DefaultJSONTemp    = 0.1
	DefaultMaxTokens   = 4096
	DefaultWordCount   = 3000
	DefaultMaxRetries  = 2
)
