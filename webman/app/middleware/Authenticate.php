<?php
declare(strict_types=1);
namespace app\middleware;

use app\service\{Auth, JsonResponse};
use Webman\Http\{Request, Response};
use Webman\MiddlewareInterface;

final class Authenticate implements MiddlewareInterface
{
    public function process(Request $request, callable $handler): Response
    {
        if (!Auth::valid($request)) {
            return JsonResponse::error(401, '请先登录。', 'UNAUTHENTICATED', true);
        }
        return $handler($request);
    }
}
