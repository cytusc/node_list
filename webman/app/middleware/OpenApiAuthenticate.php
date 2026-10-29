<?php
declare(strict_types=1);

namespace app\middleware;

use app\service\OpenApiAuth;
use Webman\Http\{Request, Response};
use Webman\MiddlewareInterface;

final class OpenApiAuthenticate implements MiddlewareInterface
{
    public function process(Request $request, callable $handler): Response
    {
        OpenApiAuth::authenticate($request);
        return $handler($request);
    }
}
