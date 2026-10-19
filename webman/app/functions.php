<?php
declare(strict_types=1);

/**
 * 业务助手函数。
 *
 * webman 本身不提供 env()（Laravel 才有），而 config/*.php 里需要读 .env
 * 与环境变量，所以在这里补一个语义一致的实现：
 *   - 优先 $_ENV（Dotenv 的 createUnsafeImmutable 会写进 $_ENV 与 getenv）
 *   - 回落 getenv()（容器里直接注入的环境变量走这条）
 *   - 显式空值/假值按 Laravel 的约定归一，避免字符串 "false" 被当成 true
 *
 * 该文件由 composer.json 的 autoload.files 加载，CLI 与 Workerman 进程共用。
 */

if (!function_exists('env')) {
    function env(string $key, mixed $default = null): mixed
    {
        $value = $_ENV[$key] ?? null;
        if ($value === null) {
            $value = getenv($key);
        }
        if ($value === false || $value === null) {
            return $default;
        }
        if (!is_string($value)) {
            return $value;
        }
        return match (strtolower($value)) {
            'true', '(true)' => true,
            'false', '(false)' => false,
            'null', '(null)' => null,
            'empty', '(empty)' => '',
            default => $value,
        };
    }
}
