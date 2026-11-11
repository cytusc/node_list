<?php
declare(strict_types=1);

namespace app\controller;

use app\service\{ApiError, JsonResponse, OpenApiSignature, OpenNodes, OpenNodesQuery};
use support\{Request, Response};

final class OpenApiController
{
    public function countries(Request $request, OpenNodes $nodes): Response
    {
        if (OpenApiSignature::parameters($request->queryString()) !== []) {
            throw new ApiError(400, '国家列表接口不接受查询参数。', 'INVALID_QUERY');
        }
        return JsonResponse::payload(['code' => 'OK', 'data' => $nodes->countries()]);
    }

    public function nodes(Request $request, OpenNodes $nodes): Response
    {
        $query = OpenNodesQuery::fromParameters(OpenApiSignature::parameters($request->queryString()));
        return JsonResponse::payload(['code' => 'OK', 'data' => $nodes->nodes($query)]);
    }
}
