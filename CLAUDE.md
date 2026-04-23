# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

InkFrame Backend is an AI-powered intelligent novel auto-generation system written in Go 1.21+. It supports multi-AI-provider novel generation (OpenAI, Claude, Gemini, Doubao, Deepseek, Qianwen), character/worldview management, hierarchical narrative memory, quality control, and video generation from story content.

## Commands

```bash
# Setup
cp config.example.yaml config.yaml   # then fill in DB/API credentials
make deps                             # download and tidy Go modules

# Development
make run                              # build and run
make dev                              # hot-reload with reflex

# Testing
make test                             # run all tests with race detection
make test-coverage                    # generate coverage.html report
go test -v ./internal/service/... -run TestTemplateName   # run a single test

# Code quality
make fmt                              # gofmt -s -w
make vet                              # go vet
make lint                             # golangci-lint

# Build
make build                            # outputs to ./bin/inkframe-backend
make build-linux                      # cross-compile for Linux

# Database
make migrate-up                       # run migrations in ./migrations/
make migrate-down                     # rollback

# Docs
make docs                             # generate Swagger via swag
```

## Architecture

The application follows a strict layered architecture:

```
HTTP Request → Handler → Service → Repository → MySQL (GORM)
                                              → Redis (cache)
                                 → AI Module → OpenAI / Claude / Google / Doubao / Deepseek / Qianwen
                                 → Vector Store → Qdrant / Chroma
```

**`cmd/server/main.go`** — Entry point. Wires everything together: config → DB → Redis → AI providers → vector stores → repositories → services → handlers → router → HTTP server with graceful shutdown.

**`internal/config/`** — Viper-based YAML config (`config.yaml`). AI API keys are read from env vars in `main.go` via `getEnv()`: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GOOGLE_API_KEY`, `DOUBAO_API_KEY`, `DEEPSEEK_API_KEY`, `QIANWEN_API_KEY`, `KLING_API_KEY`, `SEEDANCE_API_KEY`, `QDRANT_ENDPOINT`, `QDRANT_API_KEY`, `CHROMA_ENDPOINT`. Server/DB/Redis config comes from `config.yaml`.

**`internal/model/`** — GORM models. All tables use the `ink_` prefix (e.g., `ink_novel`, `ink_chapter`, `ink_character`). Models are auto-migrated at startup. Key additions: `ArcSummary`, `KnowledgeBase`, `ModelProvider`, `AIModel`, `TaskModelConfig`, `McpTool`, `ModelMcpBinding`, `ModelComparisonExperiment`.

**`internal/repository/`** — Data access. Repositories receive `*gorm.DB` and `*redis.Client`. Novel and Chapter repos cache reads in Redis with a 30-minute TTL using keys like `novel:{id}`. `ArcSummaryRepository` provides `Create`, `Update`, `GetByNovelAndArcNo`, `ListByNovel` against the `ink_arc_summary` table (unique on `novel_id + arc_no`).

**`internal/service/`** — Business logic. Key services:

- `NovelService`, `ChapterService`, `CharacterService`, `WorldviewService` — core content management
- `NarrativeMemoryService` — hierarchical context for long-form novels (100+ chapters). Builds layered context: global summary → arc summaries (every 10 chapters) → recent detailed (last 2) → recent short (previous 8 truncated to 40 chars). Also handles `GenerateChapterSummary`, `GenerateChapterTitle`, `ExtractCharacterVoice`, `RefineChapterContent`, and async arc summary generation via `TriggerArcSummaryIfNeeded`.
- `ContinuityService` — validates chapter content for character consistency, worldview adherence, and plot continuity; produces typed `ContinuityReport` with severity levels.
- `KnowledgeService` — dual-layer storage (DB + vector embeddings) for novel-specific knowledge. `SearchKnowledge` uses semantic vector search (min_score=0.6) with graceful fallback to keyword search. `ExtractAndStorePlotPoints` uses AI to analyze chapters.
- `PromptService` — constructs context-aware prompts for AI generation by assembling worldview, character state snapshots, and recent chapter context.
- `QualityControlService` — AI-based scoring (Logic 30%, Consistency 25%, Quality 25%, Style 20%) with rule-based fallback (repeat word detection, dialogue ratio, sentence variance).
- `VideoService`, `VideoEnhancementService` — video generation via Kling or Seedance providers.
- `TemplateService` — `text/template`-based prompt rendering. Templates live in `internal/service/prompts/*.tmpl` (arc_summary, chapter_summary, chapter_title, character_voice, refinement_pass, chapter_from_outline, chapter_scene_outline).

**`internal/ai/`** — `AIProvider` interface covers `Generate`, `GenerateStream`, `Embed`, `ImageGenerate`, `AudioGenerate`. Concrete implementations: `openai.go`, `claude.go`, `doubao.go`, `deepseek.go`, `qianwen.go`. All providers are wrapped with `RetryProvider` (exponential backoff, 3 retries, 500ms base delay). `VideoProvider` interface (`kling_provider.go`, `seedance_provider.go`) handles video separately. `ModelManager` exposes `RegisterProvider`, `WrapWithRetry`, `SwitchProvider`.

**`internal/vector/`** — `VectorStore` interface with Qdrant and Chroma backends. Managed by `StoreManager`.

**`internal/handler/`** — Gin HTTP handlers (one file per domain). `response.go` provides shared helpers: `respondOK`, `respondCreated`, `respondBadRequest`, `respondErr`, and `parsePagination` (validates `page` ≥1, `page_size` 1–100, default 20).

**`internal/router/`** — All routes under `/api/v1/`. `GET /health` is the health check endpoint.

**`internal/middleware/`** — CORS (allow-all), structured logger, panic recovery. JWT auth middleware is applied to all `/api/v1/` routes except auth endpoints.

## Chapter Generation Pipeline

Three-step pipeline for high-quality chapter output:

1. **Scene outline** — `chapter_scene_outline.tmpl` breaks the chapter into 3–5 scenes (POV, goals, beats, tension per scene)
2. **Full text** — `chapter_from_outline.tmpl` generates 2000–3000 char prose from the scene breakdown
3. **Refinement** — `refinement_pass.tmpl` detects and fixes quality issues (e.g., "突然" appearing >5 times, 4+ consecutive paragraphs starting with 他/她)

Context injected at generation time comes from `NarrativeMemoryService.BuildHierarchicalContext()` which renders a markdown-formatted prompt section combining global, arc, and recent chapter summaries.

## Known TODOs

- Auth rate-limit middleware not implemented.
- Test coverage is sparse — only `internal/service/template_service_test.go` exists.
- `PromptService` repo dependency is currently `nil` (template DB not yet wired).
