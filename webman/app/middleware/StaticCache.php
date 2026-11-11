<?php
declare(strict_types=1);

namespace app\middleware;

use Webman\Http\Request;
use Webman\Http\Response;
use Webman\MiddlewareInterface;

/** Only content-hashed build assets may be cached across releases. */
final class StaticCache implements MiddlewareInterface
{
    public function process(Request $request, callable $handler): Response
    {
        $response = $handler($request);
        if (!in_array($request->method(), ['GET', 'HEAD'], true)
            || !in_array($response->getStatusCode(), [200, 206, 304], true)) {
            return $response;
        }
        $immutable = preg_match('~^/assets/[^/]+-[A-Za-z0-9_-]{8,}\.[a-zA-Z0-9]+$~D', $request->path()) === 1;
        return $response->withHeader('Cache-Control', $immutable
            ? 'public, max-age=31536000, immutable'
            : 'no-cache');
    }
}
