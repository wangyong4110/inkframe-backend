-- 导出各类服务 (AI Provider / Model / MCP Tool / Video) 的配置信息
-- 数据库：MySQL（GORM，见 internal/model/ai_model.go、video.go）
--
-- 说明：
--   ink_model_provider.api_key / api_secret_key 在应用层用 EncryptField 加密后落库
--   （见 ModelProvider.BeforeSave），SQL 直查只能拿到密文，这里进一步用
--   CASE 语句只导出"是否已配置"，不导出密钥本身，避免明文/密文外泄。
--   ink_mcp_tool.headers / env 是未加密的 JSON 文本，可能包含 Authorization/Token 等
--   敏感信息，同样只导出是否配置及字段名，不导出取值。

-- ============================================================
-- 1. AI 模型供应商配置 (ink_model_provider)
-- ============================================================
SELECT
    p.id,
    p.tenant_id,
    p.name,
    p.display_name,
    p.api_endpoint,
    CASE WHEN p.api_key <> '' THEN '已配置' ELSE '未配置' END        AS api_key_status,
    CASE WHEN p.api_secret_key <> '' THEN '已配置' ELSE '未配置' END AS api_secret_key_status,
    p.api_version,
    p.default_model,
    p.needs_secret_key,
    p.static_models,
    p.is_active,
    p.health_check,
    p.last_checked,
    p.created_at,
    p.updated_at
FROM ink_model_provider p
WHERE p.deleted_at IS NULL
ORDER BY p.tenant_id, p.name;

-- ============================================================
-- 2. AI 模型配置 (ink_ai_model)，关联供应商
-- ============================================================
SELECT
    m.id,
    p.name          AS provider_name,
    p.display_name  AS provider_display_name,
    m.name          AS model_name,
    m.display_name  AS model_display_name,
    m.type,
    m.max_tokens,
    m.quality,
    m.timeout,
    m.concurrency,
    m.rate_limit,
    m.is_active,
    m.created_at,
    m.updated_at
FROM ink_ai_model m
JOIN ink_model_provider p ON p.id = m.provider_id AND p.deleted_at IS NULL
WHERE m.deleted_at IS NULL
ORDER BY p.name, m.type, m.name;

-- ============================================================
-- 3. MCP 工具配置 (ink_mcp_tool)
--    headers/env 只导出是否配置，不导出取值（可能含密钥/Token）
-- ============================================================
SELECT
    t.id,
    t.tenant_id,
    t.name,
    t.display_name,
    t.transport_type,
    t.endpoint,
    CASE WHEN t.headers IS NOT NULL AND t.headers <> '' AND t.headers <> '{}'
         THEN '已配置' ELSE '未配置' END AS headers_status,
    CASE WHEN t.env IS NOT NULL AND t.env <> '' AND t.env <> '{}'
         THEN '已配置' ELSE '未配置' END AS env_status,
    t.timeout,
    t.is_active,
    t.is_system,
    t.created_at,
    t.updated_at
FROM ink_mcp_tool t
WHERE t.deleted_at IS NULL
ORDER BY t.tenant_id, t.name;

-- ============================================================
-- 4. 模型 <-> MCP 工具绑定 (ink_model_mcp_binding)
-- ============================================================
SELECT
    b.id,
    p.name         AS provider_name,
    am.name        AS model_name,
    mt.name        AS mcp_tool_name,
    mt.transport_type,
    b.enabled,
    b.created_at
FROM ink_model_mcp_binding b
JOIN ink_ai_model am       ON am.id = b.model_id AND am.deleted_at IS NULL
JOIN ink_model_provider p  ON p.id = am.provider_id AND p.deleted_at IS NULL
JOIN ink_mcp_tool mt       ON mt.id = b.tool_id AND mt.deleted_at IS NULL
ORDER BY p.name, am.name, mt.name;

-- ============================================================
-- 5. 系统功能 <-> MCP 工具绑定 (ink_mcp_feature_binding)
-- ============================================================
SELECT
    f.id,
    f.feature_key,
    mt.name AS mcp_tool_name,
    CASE WHEN f.mcp_tool_id IS NULL THEN '内置默认实现' ELSE mt.name END AS resolved_impl,
    f.enabled,
    f.note,
    f.created_at,
    f.updated_at
FROM ink_mcp_feature_binding f
LEFT JOIN ink_mcp_tool mt ON mt.id = f.mcp_tool_id AND mt.deleted_at IS NULL
ORDER BY f.feature_key;

-- ============================================================
-- 6. 小说视频生成配置 (ink_novel_video_config, config 为 JSON 列)
--    按需用 JSON_EXTRACT / JSON_TABLE 展开常用字段
-- ============================================================
SELECT
    c.id,
    c.novel_id,
    JSON_UNQUOTE(JSON_EXTRACT(c.config, '$.video_type'))          AS video_type,
    JSON_UNQUOTE(JSON_EXTRACT(c.config, '$.video_resolution'))    AS video_resolution,
    JSON_UNQUOTE(JSON_EXTRACT(c.config, '$.video_fps'))           AS video_fps,
    JSON_UNQUOTE(JSON_EXTRACT(c.config, '$.video_aspect_ratio'))  AS video_aspect_ratio,
    JSON_UNQUOTE(JSON_EXTRACT(c.config, '$.kling_model'))         AS kling_model,
    JSON_UNQUOTE(JSON_EXTRACT(c.config, '$.subtitle_enabled'))    AS subtitle_enabled,
    c.config                                                      AS config_raw_json,
    c.created_at,
    c.updated_at
FROM ink_novel_video_config c
ORDER BY c.novel_id;

-- ============================================================
-- 7. 汇总视图：各类服务配置一览（跨表 UNION，便于统一导出/巡检）
-- ============================================================
SELECT
    'ai_provider'                                   AS service_type,
    p.tenant_id                                     AS tenant_id,
    p.name                                           AS service_name,
    p.display_name                                   AS display_name,
    p.api_endpoint                                   AS endpoint,
    p.is_active                                      AS is_active,
    CONCAT('api_key=', CASE WHEN p.api_key <> '' THEN 'set' ELSE 'unset' END,
           ';health=', p.health_check)                AS extra_info,
    p.updated_at                                     AS updated_at
FROM ink_model_provider p
WHERE p.deleted_at IS NULL

UNION ALL

SELECT
    'mcp_tool'                                       AS service_type,
    t.tenant_id                                      AS tenant_id,
    t.name                                            AS service_name,
    t.display_name                                    AS display_name,
    t.endpoint                                        AS endpoint,
    t.is_active                                       AS is_active,
    CONCAT('transport=', t.transport_type, ';is_system=', t.is_system) AS extra_info,
    t.updated_at                                      AS updated_at
FROM ink_mcp_tool t
WHERE t.deleted_at IS NULL

ORDER BY service_type, tenant_id, service_name;
