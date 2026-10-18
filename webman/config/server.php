<?php
/**
 * Workerman 进程配置。
 *
 * listen 决定 HTTP 监听地址，默认 127.0.0.1:3001（HOST + API_PORT）。
 * 生产走 1Panel 反代，只需监听本机回环或容器网段。
 */

return [
    'listen' => env('API_LISTEN', 'http://' . env('API_HOST', '127.0.0.1') . ':' . env('API_PORT', '3001')),
    'name' => env('API_WORKER_NAME', 'node-list-api'),
    // Windows 上 workerman 只支持单进程；生产（Linux）按 CPU 核数给 2~4 即可。
    'count' => (int)env('API_WORKERS', DIRECTORY_SEPARATOR === '\\' ? 1 : 4),
    'user' => '',
    'group' => '',
    'reusePort' => false,
    'event_loop' => '',
    'stop_timeout' => 2,
    'pid_file' => runtime_path() . '/webman.pid',
    'status_file' => runtime_path() . '/webman.status',
    'stdout_file' => runtime_path() . '/logs/stdout.log',
    'log_file' => runtime_path() . '/logs/workerman.log',
    'max_package_size' => 10 * 1024 * 1024,
];
