package metrics

import (
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

// ServerStartTime records when the process started, used to compute uptime.
var ServerStartTime = time.Now()

// ─── HTTP 请求指标 ────────────────────────────────────────────────────────────

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_http_requests_total",
		Help: "HTTP 请求总数（按方法、路由、状态码分组）",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkframe_http_request_duration_seconds",
		Help:    "HTTP 请求处理延迟分布",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "path", "status"})

	HTTPRequestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inkframe_http_requests_in_flight",
		Help: "当前正在处理的 HTTP 请求数",
	})

	HTTPPanicsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "inkframe_http_panics_total",
		Help: "HTTP handler panic 总次数",
	})

	HTTPRateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_http_rate_limited_total",
		Help: "被限流拦截的请求总数",
	}, []string{"type"}) // type: ip / auth
)

// ─── AI 调用指标 ──────────────────────────────────────────────────────────────

var (
	AIRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_ai_requests_total",
		Help: "AI 生成接口调用总数（按任务类型、提供商、结果状态分组）",
	}, []string{"task_type", "provider", "status"}) // status: success / error

	AIRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkframe_ai_request_duration_seconds",
		Help:    "AI 生成接口端到端延迟分布（秒）",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120},
	}, []string{"task_type", "provider"})

	AITokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_ai_tokens_total",
		Help: "AI 接口消耗 token 总数",
	}, []string{"task_type", "provider", "token_type"}) // token_type: prompt / completion

	// 当前正在进行 AI 调用的并发数（Gauge，便于检测积压）
	AIRequestsInFlight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "inkframe_ai_requests_in_flight",
		Help: "当前并发进行中的 AI 生成请求数",
	}, []string{"task_type", "provider"})
)

// ─── 数据库查询指标 ───────────────────────────────────────────────────────────

var (
	DBQueriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_db_queries_total",
		Help: "数据库查询总数（按表名、操作类型、是否出错分组）",
	}, []string{"table", "operation", "error"}) // operation: SELECT/INSERT/UPDATE/DELETE

	DBQueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkframe_db_query_duration_seconds",
		Help:    "数据库查询延迟分布（秒）",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
	}, []string{"table", "operation"})
)

// ─── 异步任务指标 ─────────────────────────────────────────────────────────────

var (
	TaskCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_task_created_total",
		Help: "异步任务创建总数（按类型分组）",
	}, []string{"type"})

	TaskCompletedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_task_completed_total",
		Help: "异步任务完成总数（按类型、结果状态分组）",
	}, []string{"type", "status"}) // status: success / error / cancelled
)

// ─── 业务指标：TTS 配音生成 ──────────────────────────────────────────────────

var (
	TTSGenerationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_tts_generation_total",
		Help: "TTS 配音生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error / skipped

	TTSGenerationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_tts_generation_duration_seconds",
		Help:    "TTS 单次配音生成耗时分布（秒）",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60},
	})
)

// ─── 业务指标：SFX 音效生成 ──────────────────────────────────────────────────

var (
	SFXGenerationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_sfx_generation_total",
		Help: "分镜 SFX 音效生成总数（按来源、结果状态分组）",
	}, []string{"source", "status"}) // source: ai/library/cache; status: success/error
)

// ─── 业务指标：平台发布 ──────────────────────────────────────────────────────

var (
	PublishTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_publish_total",
		Help: "视频发布到外部平台总数（按平台、结果状态分组）",
	}, []string{"platform", "status"}) // platform: bilibili/douyin/youtube; status: success/error

	PublishDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkframe_publish_duration_seconds",
		Help:    "视频发布到外部平台耗时分布（秒）",
		Buckets: []float64{5, 10, 30, 60, 120, 300, 600},
	}, []string{"platform"})
)

// ─── 业务指标：小说创建 & 大纲生成 ──────────────────────────────────────────

var (
	NovelCreationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_novel_creation_total",
		Help: "小说创建总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	OutlineGenerationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_outline_generation_total",
		Help: "小说大纲 AI 生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	OutlineGenerationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_outline_generation_duration_seconds",
		Help:    "小说大纲生成端到端耗时分布（秒）",
		Buckets: []float64{5, 10, 20, 30, 60, 120, 300},
	})
)

// ─── 业务指标：镜头图片生成 ──────────────────────────────────────────────────

var (
	ShotImageGenerationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_shot_image_generation_total",
		Help: "分镜图片生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	ShotImageGenerationInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inkframe_shot_image_generation_in_flight",
		Help: "当前正在生成图片的分镜数",
	})
)

// ─── 业务指标：镜头 AI 视频提交 ──────────────────────────────────────────────

var (
	ShotVideoSubmissionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_shot_video_submission_total",
		Help: "分镜 AI 视频任务提交总数（按提供商、结果状态分组）",
	}, []string{"provider", "status"}) // status: success / error
)

// ─── 业务指标：章节质量检查 ──────────────────────────────────────────────────

var (
	QualityCheckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_quality_check_total",
		Help: "章节质量检查总数（按检查方式分组）",
	}, []string{"method"}) // method: ai / rule

	QualityScoreOverall = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_quality_score_overall",
		Help:    "章节综合质量分分布（0–1，1 为满分）",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0},
	})

	QualityScoreByDimension = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkframe_quality_score_dimension",
		Help:    "章节各维度质量分分布（0–1）",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0},
	}, []string{"dimension"}) // dimension: logic / consistency / quality / style / dramatic
)

// ─── 业务指标：章节生成 ──────────────────────────────────────────────────────

var (
	ChapterGenerationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_chapter_generation_total",
		Help: "章节 AI 生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error / conflict

	ChapterGenerationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_chapter_generation_duration_seconds",
		Help:    "章节 AI 生成端到端耗时分布（秒）",
		Buckets: []float64{5, 10, 20, 30, 60, 120, 180, 300, 600},
	})

	ChapterGenerationInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inkframe_chapter_generation_in_flight",
		Help: "当前正在生成中的章节数",
	})
)

// ─── 业务指标：分镜脚本生成 ──────────────────────────────────────────────────

var (
	StoryboardGenerationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_storyboard_generation_total",
		Help: "分镜脚本生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error / partial

	StoryboardGenerationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_storyboard_generation_duration_seconds",
		Help:    "分镜脚本生成端到端耗时分布（秒）",
		Buckets: []float64{5, 10, 20, 30, 60, 120, 300, 600},
	})

	StoryboardShotsGenerated = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_storyboard_shots_per_generation",
		Help:    "每次分镜生成产出的镜头数分布",
		Buckets: []float64{5, 10, 15, 20, 30, 40, 60, 80, 100},
	})

	StoryboardGenerationInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inkframe_storyboard_generation_in_flight",
		Help: "当前正在生成中的分镜脚本数",
	})
)

// ─── 业务指标：视频合成 ──────────────────────────────────────────────────────

var (
	VideoSynthesisTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_video_synthesis_total",
		Help: "视频合成任务总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	VideoSynthesisDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_video_synthesis_duration_seconds",
		Help:    "视频合成端到端耗时分布（秒）",
		Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600},
	})

	VideoSynthesisInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inkframe_video_synthesis_in_flight",
		Help: "当前正在进行视频合成的任务数",
	})
)

// ─── 业务指标：小说爬取 ──────────────────────────────────────────────────────

var (
	CrawlChaptersTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_crawl_chapters_total",
		Help: "章节爬取总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	CrawlJobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_crawl_jobs_total",
		Help: "小说爬取任务完成总数（按最终状态分组）",
	}, []string{"status"}) // status: completed / partial / failed

	CrawlJobsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inkframe_crawl_jobs_in_flight",
		Help: "当前正在进行的小说爬取任务数",
	})
)

// ─── 业务指标：异步后处理管线（摘要/标题/精修/弧摘要） ──────────────────────

var (
	ChapterSummaryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_chapter_summary_total",
		Help: "章节摘要 AI 生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	ChapterSummaryDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_chapter_summary_duration_seconds",
		Help:    "章节摘要生成耗时分布（秒）",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 60},
	})

	ChapterTitleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_chapter_title_total",
		Help: "章节标题 AI 生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	ChapterRefinementTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_chapter_refinement_total",
		Help: "章节精修总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error / skipped / rejected

	ArcSummaryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_arc_summary_total",
		Help: "弧摘要 AI 生成总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	ArcSummaryDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_arc_summary_duration_seconds",
		Help:    "弧摘要生成耗时分布（秒）",
		Buckets: []float64{5, 10, 20, 30, 60, 120, 300},
	})
)

// ─── 业务指标：场景一致性与视频增强 ──────────────────────────────────────────

var (
	SceneConsistencyScoreHist = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_scene_consistency_score",
		Help:    "分镜场景一致性综合评分分布（0–1）",
		Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.75, 0.8, 0.85, 0.9, 1.0},
	})

	SceneConsistencyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_scene_consistency_total",
		Help: "分镜场景一致性评分总数（按结果分组）",
	}, []string{"outcome"}) // outcome: passed / retry / human / noref

	VideoEnhancementTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_video_enhancement_total",
		Help: "视频增强任务总数（按类型、结果分组）",
	}, []string{"type", "status"}) // type: frame_interpolation/super_resolution/color_grading/stabilize/style_transfer; status: completed/failed
)

// ─── 业务指标：知识库与导出 ──────────────────────────────────────────────────

var (
	KnowledgeSearchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_knowledge_search_total",
		Help: "知识库搜索总数（按检索方式分组）",
	}, []string{"method"}) // method: vector / keyword

	KnowledgeExtractTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_knowledge_extract_total",
		Help: "章节剧情点提取总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error / skipped

	ExportCapCutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_export_capcut_total",
		Help: "CapCut 草稿导出总数（按格式、结果状态分组）",
	}, []string{"format", "status"}) // format: capcut/broll/fcpxml/resource/srt/vtt/edl/otio/csv; status: success/error

	ExportCapCutDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkframe_export_capcut_duration_seconds",
		Help:    "CapCut/剪映草稿导出耗时分布（秒）",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"format"})
)

// ─── 业务指标：章节 AI 深度审查 ──────────────────────────────────────────────

var (
	ChapterDeepReviewTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_chapter_deep_review_total",
		Help: "章节 AI 深度审查（ReviewChapter）总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	ChapterDeepReviewDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "inkframe_chapter_deep_review_duration_seconds",
		Help:    "章节 AI 深度审查端到端耗时分布（秒）",
		Buckets: []float64{5, 10, 20, 30, 60, 120, 300},
	})

	ChapterApplyDiffsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_chapter_apply_diffs_total",
		Help: "ApplyDiffs 调用总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	ChapterDiffsApplied = promauto.NewCounter(prometheus.CounterOpts{
		Name: "inkframe_chapter_diffs_applied_total",
		Help: "ApplyDiffs 实际修改的段落总数",
	})
)

// ─── 业务指标：角色与快照 ────────────────────────────────────────────────────

var (
	CharacterSnapshotExtractionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_character_snapshot_extraction_total",
		Help: "每章角色状态提取任务总数（按结果状态分组）",
	}, []string{"status"}) // status: skipped / ai_error / parse_error / success

	CharacterSnapshotTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_character_snapshot_total",
		Help: "角色状态快照写入总数（按结果状态分组）",
	}, []string{"status"}) // status: success / error

	CharacterImageBatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkframe_character_image_batch_total",
		Help: "角色图片批量生成：按每个角色的结果（succeeded/failed）计数",
	}, []string{"status"}) // status: succeeded / failed
)

// ─── 连接池指标（自定义 Collector） ──────────────────────────────────────────

// DBStatsCollector 将 sql.DB 连接池统计暴露为 Prometheus Gauge。
// 每次 Scrape 时由 Prometheus 主动调用 Collect() 读取实时数据。
type DBStatsCollector struct {
	db             *sql.DB
	openConns      *prometheus.Desc
	inUseConns     *prometheus.Desc
	idleConns      *prometheus.Desc
	waitCount      *prometheus.Desc
	waitDuration   *prometheus.Desc
	maxIdleClosed  *prometheus.Desc
	maxLifetimeClosed *prometheus.Desc
}

func NewDBStatsCollector(db *sql.DB) *DBStatsCollector {
	const ns = "inkframe_db_pool"
	return &DBStatsCollector{
		db:             db,
		openConns:      prometheus.NewDesc(ns+"_open_connections", "当前打开的数据库连接总数", nil, nil),
		inUseConns:     prometheus.NewDesc(ns+"_in_use_connections", "当前正在使用的连接数", nil, nil),
		idleConns:      prometheus.NewDesc(ns+"_idle_connections", "当前空闲连接数", nil, nil),
		waitCount:      prometheus.NewDesc(ns+"_wait_count_total", "等待新连接的累计次数", nil, nil),
		waitDuration:   prometheus.NewDesc(ns+"_wait_duration_seconds_total", "等待新连接的累计时长（秒）", nil, nil),
		maxIdleClosed:  prometheus.NewDesc(ns+"_max_idle_closed_total", "因超过 MaxIdleConns 而关闭的连接数", nil, nil),
		maxLifetimeClosed: prometheus.NewDesc(ns+"_max_lifetime_closed_total", "因超过 ConnMaxLifetime 而关闭的连接数", nil, nil),
	}
}

func (c *DBStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.openConns
	ch <- c.inUseConns
	ch <- c.idleConns
	ch <- c.waitCount
	ch <- c.waitDuration
	ch <- c.maxIdleClosed
	ch <- c.maxLifetimeClosed
}

func (c *DBStatsCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.db.Stats()
	ch <- prometheus.MustNewConstMetric(c.openConns, prometheus.GaugeValue, float64(s.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.inUseConns, prometheus.GaugeValue, float64(s.InUse))
	ch <- prometheus.MustNewConstMetric(c.idleConns, prometheus.GaugeValue, float64(s.Idle))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(s.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.waitDuration, prometheus.CounterValue, s.WaitDuration.Seconds())
	ch <- prometheus.MustNewConstMetric(c.maxIdleClosed, prometheus.CounterValue, float64(s.MaxIdleClosed))
	ch <- prometheus.MustNewConstMetric(c.maxLifetimeClosed, prometheus.CounterValue, float64(s.MaxLifetimeClosed))
}

// RedisStatsCollector 将 Redis 连接池统计暴露为 Prometheus Gauge。
type RedisStatsCollector struct {
	rdb          *redis.Client
	totalConns   *prometheus.Desc
	idleConns    *prometheus.Desc
	staleConns   *prometheus.Desc
	hits         *prometheus.Desc
	misses       *prometheus.Desc
	timeouts     *prometheus.Desc
}

func NewRedisStatsCollector(rdb *redis.Client) *RedisStatsCollector {
	const ns = "inkframe_redis_pool"
	return &RedisStatsCollector{
		rdb:        rdb,
		totalConns: prometheus.NewDesc(ns+"_total_connections", "Redis 连接池总连接数", nil, nil),
		idleConns:  prometheus.NewDesc(ns+"_idle_connections", "Redis 空闲连接数", nil, nil),
		staleConns: prometheus.NewDesc(ns+"_stale_connections", "Redis 过期连接数（待回收）", nil, nil),
		hits:       prometheus.NewDesc(ns+"_hits_total", "从连接池命中的累计次数", nil, nil),
		misses:     prometheus.NewDesc(ns+"_misses_total", "连接池未命中（新建连接）的累计次数", nil, nil),
		timeouts:   prometheus.NewDesc(ns+"_timeouts_total", "等待连接超时的累计次数", nil, nil),
	}
}

func (c *RedisStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalConns
	ch <- c.idleConns
	ch <- c.staleConns
	ch <- c.hits
	ch <- c.misses
	ch <- c.timeouts
}

func (c *RedisStatsCollector) Collect(ch chan<- prometheus.Metric) {
	if c.rdb == nil {
		return
	}
	s := c.rdb.PoolStats()
	ch <- prometheus.MustNewConstMetric(c.totalConns, prometheus.GaugeValue, float64(s.TotalConns))
	ch <- prometheus.MustNewConstMetric(c.idleConns, prometheus.GaugeValue, float64(s.IdleConns))
	ch <- prometheus.MustNewConstMetric(c.staleConns, prometheus.GaugeValue, float64(s.StaleConns))
	ch <- prometheus.MustNewConstMetric(c.hits, prometheus.CounterValue, float64(s.Hits))
	ch <- prometheus.MustNewConstMetric(c.misses, prometheus.CounterValue, float64(s.Misses))
	ch <- prometheus.MustNewConstMetric(c.timeouts, prometheus.CounterValue, float64(s.Timeouts))
}
