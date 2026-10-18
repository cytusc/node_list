<?php
declare(strict_types=1);

namespace app\middleware;

use support\Log;
use Webman\Http\Request;
use Webman\Http\Response;
use Webman\MiddlewareInterface;

/**
 * 访问日志（PSR-15 风格中间件）。
 *
 * 挂在 config/route.php 的 /api 路由组上，而不是全局中间件：
 * 静态资源（前端 dist）不该污染接口日志，也不该多一次时间戳采集。
 *
 * 记录一行机器可读日志，便于 grep 与日志采集：
 *   ip method path status cost_ms conn
 *
 * 只记 path 不记 query：本服务 query 里只有筛选条件，但「只记必要信息」
 * 是不用额外成本就能守住的习惯。
 */
final class AccessLog implements MiddlewareInterface
{
    public function process(Request $request, callable $handler): Response
    {
        $start = microtime(true);

        /** @var Response $response */
        $response = $handler($request);

        Log::info(sprintf(
            '%s %s %s %d %.1fms conn=%s',
            $request->getRealIp(),
            $request->method(),
            $request->path(),
            $response->getStatusCode(),
            (microtime(true) - $start) * 1000,
            (string)$request->connection->id
        ));

        return $response;
    }
}
