<?php
declare(strict_types=1);
namespace app\controller;

use app\service\{ApiError, Heartbeat, JsonResponse, ServiceMonitor};
use support\{Log, Request, Response};
use Throwable;

final class ServicesController
{
    public function index(Request $request, ServiceMonitor $monitor): Response
    {
        Heartbeat::beatIfDue();
        try {
            return JsonResponse::payload($monitor->query());
        } catch (Throwable $e) {
            Log::warning('service monitor read failed: ' . $e->getMessage());
            throw new ApiError(503, '服务监控暂时无法读取，请稍后重试。', 'MONITOR_UNAVAILABLE');
        }
    }
}
