<?php
declare(strict_types=1);

namespace app\controller;

use app\service\JsonResponse;
use app\service\ServiceBoard;
use support\Request;
use support\Response;

/**
 * GET /api/services —— 服务看板。
 *
 * 与 Go 的 api.WriteServices 一致：
 *   - 回 {"services":[...],"observedAt":"..."}，9 个固定服务按服务名排序
 *   - Cache-Control: no-store（前端 15s 轮询）
 *   - 读失败 → 503 READ_FAILED（不是降级为空列表，否则界面会误显示「全部健康」）
 */
final class ServicesController
{
    public function index(Request $request, ServiceBoard $board): Response
    {
        return JsonResponse::payload($board->services());
    }
}
