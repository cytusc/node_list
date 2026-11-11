<?php
declare(strict_types=1);

namespace app\controller;

use app\service\{Heartbeat, JsonResponse, LiveResults, ResultsQueryParser};
use support\{Request, Response};

/** GET /api/results：每次请求实时查询数据库中的可用节点。 */
final class ResultsController
{
    public function index(Request $request, LiveResults $live): Response
    {
        $query = ResultsQueryParser::fromQuery($request->get());
        Heartbeat::beatIfDue();
        return JsonResponse::payload($live->query($query));
    }
}
