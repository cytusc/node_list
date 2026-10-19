<?php
declare(strict_types=1);

namespace app\controller;

use app\service\JsonResponse;
use support\Request;
use support\Response;

/**
 * GET /api/health —— 存活探针。
 *
 * 与 Go 的 api.health 一致：只回 {"status":"ok"}，不查库。
 * 刻意不探测数据库：这个端点被容器健康检查和反代使用，PG 抖动时
 * 不该把整个实例踢下线（可读性由 /api/services 的心跳体现）。
 */
final class HealthController
{
    public function index(Request $request): Response
    {
        return JsonResponse::payload(['status' => 'ok'], noStore: false);
    }
}
