<?php
declare(strict_types=1);

use app\middleware\StaticCache;
use Webman\Http\Request;
use Webman\Http\Response;

$cache = new StaticCache();
foreach ([
    ['/assets/index-Ab12_cd-.js', 200, 'public, max-age=31536000, immutable'],
    ['/assets/index-Ab12_cd-.js', 304, 'public, max-age=31536000, immutable'],
    ['/assets/flag-Ab12_cd-.svg', 206, 'public, max-age=31536000, immutable'],
    ['/assets/index.js', 200, 'no-cache'],
    ['/index.html', 200, 'no-cache'],
    ['/favicon.svg', 200, 'no-cache'],
    ['/assets/missing-Ab12_cd-.js', 404, null],
] as [$path, $status, $expected]) {
    $request = new Request("GET {$path} HTTP/1.1\r\nHost: localhost\r\n\r\n");
    $response = $cache->process($request, fn () => new Response($status));
    same($expected, $response->getHeader('Cache-Control'), "static cache {$path} {$status}");
}
