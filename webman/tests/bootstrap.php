<?php
declare(strict_types=1);

/**
 * CLI 引导：让服务类在命令行下可直接使用，不启动 Workerman。
 *
 * 与 start.php 的差别只有两点：不加载路由（避免注册路由表）与不执行 App::run()。
 * 这样单元测试与集成脚本能复用与线上完全相同的 config/*.php 和服务代码。
 */

use Dotenv\Dotenv;
use Webman\Config;

$root = dirname(__DIR__);
require $root . '/vendor/autoload.php';

// .env 优先，其次用 .env.test（集成测试用）
$envFile = getenv('APP_ENV_FILE') ?: ($root . '/.env');
if (is_file($envFile) && class_exists(Dotenv::class)) {
    Dotenv::createUnsafeImmutable($root, basename($envFile))->load();
}

// 与 support\App::run() 一致，只排除 route/container（路由需要 Workerman 上下文）。
Config::load($root . '/config', ['route']);

// 时区也照线上取 config/app.php，而不是写死 —— 时间渲染是按「本地时区当前偏移」
// 做的（对齐 Go），测试必须跑到和线上同一条路径上。
date_default_timezone_set((string)config('app.default_timezone', 'Asia/Shanghai'));

// 初始化 illuminate 连接（等价于 webman/database 的 config 文件副作用）。
if (config('database.connections') !== null) {
    Webman\Database\Initializer::init(config('database'));
}

// runtime 目录（日志/缓存）在 CLI 下也要存在。
$runtime = $root . '/runtime/logs';
if (!is_dir($runtime)) {
    mkdir($runtime, 0777, true);
}
