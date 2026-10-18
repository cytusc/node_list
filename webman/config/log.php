<?php
/**
 * 日志配置。
 *
 * - 按天切割，保留 7 天
 * - 格式带时间与级别，方便与 1Panel 的容器日志对齐排查
 * - 级别默认 warning：访问日志走 info，需要时把 APP_LOG_LEVEL 调成 debug
 *
 * 生产注意：日志目录 runtime/logs 必须可写，且要纳入磁盘清理策略
 * （访问日志在高频轮询下增长较快，容量估算见 webman/README.md 的「部署」一节）。
 */

return [
    'default' => [
        'handlers' => [
            [
                'class' => Monolog\Handler\RotatingFileHandler::class,
                'constructor' => [
                    runtime_path() . '/logs/webman.log',
                    7,
                    Monolog\Logger::toMonologLevel(env('APP_LOG_LEVEL', 'info')),
                ],
                'formatter' => [
                    'class' => Monolog\Formatter\LineFormatter::class,
                    'constructor' => [null, 'Y-m-d H:i:s', true],
                ],
            ],
        ],
    ],
];
