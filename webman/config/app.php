<?php
/**
 * 应用级配置：只放 webman 框架需要的键。
 *
 * 业务参数（分页上限、判定版本）在 config/api.php，
 * 数据库在 config/database.php —— 一事一文件，
 * 这是 webman 的配置约定（Config::load 会把每个文件挂成一个顶层键）。
 */

return [
    'debug' => (bool)env('APP_DEBUG', false),
    'error_reporting' => E_ALL,
    // 时区决定时间戳的**渲染偏移**（存储与判定都在 UTC 侧完成）。
    // 默认 Asia/Shanghai（用户按北京时间看面板）；想覆盖就设 APP_TIMEZONE。
    // 注意：这不是 PG 的会话时区，库里的 timestamptz 与查询两侧都不经过它。
    'default_timezone' => env('APP_TIMEZONE', 'Asia/Shanghai'),
    'request_class' => support\Request::class,
    'public_path' => base_path() . DIRECTORY_SEPARATOR . 'public',
    'runtime_path' => base_path(false) . DIRECTORY_SEPARATOR . 'runtime',
    'controller_suffix' => 'Controller',
    // 本服务控制器无状态，关掉复用等价于「每请求一个控制器」，
    // 避免将来有人在控制器属性上缓存请求数据导致串请求。
    'controller_reuse' => false,
];
