<?php
/**
 * 缓存（PSR-16）。
 *
 * 本服务只用它做「读侧短 TTL 去抖」，不存业务状态，所以默认 file 驱动：
 *   - 多 worker 共享，5~10s 窗口内不会每个 worker 各查一遍库
 *   - 不引入 Redis 依赖（本机已有 Redis，但读侧缓存不值得多一个故障点）
 *
 * 若将来要求更低的读放大，把 default 改成 redis 并补 config/redis.php 即可，
 * 业务代码只依赖 support\Cache 的 get/set，不需要改。
 */

return [
    'default' => env('CACHE_DRIVER', 'file'),
    'stores' => [
        'file' => [
            'driver' => 'file',
            'path' => runtime_path('cache'),
        ],
        'array' => [
            'driver' => 'array',
        ],
        'redis' => [
            'driver' => 'redis',
            'connection' => 'default',
        ],
    ],
];
