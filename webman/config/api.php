<?php
/**
 * 接口契约参数。
 *
 * 分页上限、排序键、判定版本号都收在这里：改契约不用翻代码。
 *
 * 注意两个「不做成配置」的东西，避免把契约改成可选：
 *   - 排序键的 SQL 表达式（写在 App\Service\Enum\SortKey 里，与枚举同源）
 *   - features 白名单（App\Service\Enum\Feature）
 */

return [
    // 只读实时视图。置 false 时 /api/results 直接回 503，
    // 用于灰度时「关掉读路径但不摘路由」，而不是 404 让前端以为接口没了。
    'live' => (bool)env('API_LIVE', true),

    // 分页：默认 50，上限 200（前端表格一次最多 200 行）
    'default_limit' => (int)env('API_DEFAULT_LIMIT', 50),
    'max_limit' => (int)env('API_MAX_LIMIT', 200),

    // ipcheck.VotingVersion：住宅判定版本。「优选」排序会校验它，
    // 版本不一致的历史证据视为过期（宁可排到后面，也不误报住宅）。
    'voting_version' => (int)env('IPCHECK_VOTING_VERSION', 8),

    // 读侧缓存 TTL（秒）。都远小于前端轮询间隔，避免界面拿到回退的旧状态。
    'cache' => [
        'collector_seconds' => (int)env('API_COLLECTOR_CACHE_SECONDS', 5),
        'services_seconds' => (int)env('API_SERVICES_CACHE_SECONDS', 10),
        'warning_seconds' => (int)env('API_WARNING_CACHE_SECONDS', 10),
    ],
];
