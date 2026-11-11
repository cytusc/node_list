<?php
declare(strict_types=1);
require __DIR__ . '/bootstrap.php';

use app\exception\ApiErrorHandler;
use app\middleware\OpenApiAuthenticate;
use app\service\{ApiError, OpenApiAuth, OpenApiSignature, OpenNodesQuery};
use support\{Db, Request, Response};

$passed = 0;
function check(bool $condition, string $message): void {
    global $passed;
    if (!$condition) throw new RuntimeException($message);
    $passed++;
}
function fails(callable $run, int $status, string $code): void {
    try { $run(); } catch (ApiError $e) {
        check($e->status === $status && $e->errorCode === $code, "Expected $status $code, got {$e->status} {$e->errorCode}");
        return;
    }
    throw new RuntimeException("Expected $status $code");
}
function configureOpenApi(string $id, string $secret): void {
    putenv('OPEN_API_APP_ID=' . $id);
    putenv('OPEN_API_SECRET=' . $secret);
    Webman\Config::load(dirname(__DIR__) . '/config', ['route']);
}

// 独立调用方签名：不使用服务端 message/parameters 实现来生成有效请求。
function signedRequest(string $path = '/api/open/v1/nodes', array $params = ['country' => 'HK'], array $headers = [], string $body = '', string $method = 'GET', ?int $signedTimestamp = null): Request {
    $timestamp = (string)($signedTimestamp ?? time());
    $nonce = bin2hex(random_bytes(16));
    ksort($params, SORT_STRING);
    $query = http_build_query($params, '', '&', PHP_QUERY_RFC3986);
    $message = implode("\n", [$method, $path, $query, hash('sha256', $body), $timestamp, $nonce]);
    $values = array_replace([
        'X-App-Id' => 'test-client', 'X-Timestamp' => $timestamp, 'X-Nonce' => $nonce,
        'X-Signature' => hash_hmac('sha256', $message, '0123456789abcdef0123456789abcdef'),
    ], $headers);
    $raw = "$method $path" . ($query === '' ? '' : '?' . $query) . " HTTP/1.1\r\nHost: localhost\r\nContent-Length: " . strlen($body) . "\r\n";
    foreach ($values as $key => $value) $raw .= "$key: $value\r\n";
    return new Request($raw . "\r\n" . $body);
}

$message = OpenApiSignature::message('GET', '/api/open/v1/nodes', ['page' => '1', 'mode' => 'standard', 'country' => 'HK'], '', '1700000000', '0123456789abcdef0123456789abcdef');
check(hash_hmac('sha256', $message, '0123456789abcdef0123456789abcdef') === '25e32218e7a5fc863eee92cbd06b91c3745503ed9d550ca15e01b7c9404b5d1c', 'Cross-language signing vector');
check(OpenApiSignature::canonicalQuery(['z' => 'a b', 'a' => '/香港']) === 'a=%2F%E9%A6%99%E6%B8%AF&z=a%20b', 'RFC3986 encoding and bytewise key order');
check(OpenApiSignature::parameters('country=%48K&page=1') === ['country' => 'HK', 'page' => '1'], 'Decode before canonicalization');
check(OpenApiSignature::parameters('q=a+b') === ['q' => 'a b'], 'URL form spaces canonicalize consistently');
foreach (['country=HK&country=US', 'country=HK&%63ountry=US', 'country[]=HK', 'c.ountry=HK', 'c+ountry=HK', 'country=%ZZ', 'country=%', 'country=%FF', 'country=HK%00', 'country=HK&&page=1', str_repeat('x', 4097), implode('&', array_map(fn ($i) => 'x' . $i . '=1', range(1, 17)))] as $query) {
    fails(fn () => OpenApiSignature::parameters($query), 400, 'INVALID_QUERY');
}
$q = OpenNodesQuery::fromParameters(['country' => 'hk']);
check($q->country === 'HK' && $q->page === 1 && OpenNodesQuery::PAGE_SIZE === 10, 'Country normalization, default page, fixed ten nodes');
foreach ([[], ['country' => 'HKG'], ['country' => 'ZZ'], ['country' => 'HK', 'page' => '0'], ['country' => 'HK', 'page' => '-1'], ['country' => 'HK', 'page' => '1.1'], ['country' => 'HK', 'page' => '01'], ['country' => 'HK', 'page' => '2147483648'], ['country' => 'HK', 'page' => '999999999999999999999999'], ['country' => 'HK', 'page_size' => '100'], ['country' => 'HK', 'limit' => '100']] as $params) {
    fails(fn () => OpenNodesQuery::fromParameters($params), 400, 'INVALID_QUERY');
}
fails(fn () => OpenNodesQuery::fromParameters(['country' => 'HK', 'mode' => 'speed']), 400, 'INVALID_MODE');

$capsule = new Webman\Database\Manager();
$capsule->addConnection(['driver' => 'sqlite', 'database' => ':memory:', 'prefix' => '']);
$capsule->setAsGlobal();
Db::statement('CREATE TABLE node_explorer_open_api_nonces (app_id TEXT NOT NULL, nonce TEXT NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (app_id,nonce))');
configureOpenApi('', '');
fails(fn () => OpenApiAuth::authenticate(signedRequest()), 503, 'OPEN_API_DISABLED');
configureOpenApi('test-client', 'too-short');
fails(fn () => OpenApiAuth::authenticate(signedRequest()), 503, 'OPEN_API_DISABLED');
configureOpenApi('test-client', '0123456789abcdef0123456789abcdef');
fails(fn () => OpenApiAuth::authenticate(new Request("GET /api/open/v1/nodes HTTP/1.1\r\nCookie: node_explorer_session=anything\r\n\r\n")), 401, 'INVALID_SIGNATURE');
foreach ([['X-App-Id' => 'other'], ['X-Signature' => str_repeat('0', 64)], ['X-Signature' => str_repeat('A', 64)], ['X-Nonce' => 'short'], ['X-Nonce' => str_repeat('a', 129)], ['X-Timestamp' => '1e9'], ['X-Timestamp' => (string)(time() - 301)], ['X-Timestamp' => (string)(time() + 301)]] as $headers) {
    $expired = isset($headers['X-Timestamp']) && ctype_digit($headers['X-Timestamp']);
    fails(fn () => OpenApiAuth::authenticate(signedRequest(headers: $headers)), 401, $expired ? 'REQUEST_EXPIRED' : 'INVALID_SIGNATURE');
}
check(Db::table('node_explorer_open_api_nonces')->count() === 0, 'Failed signatures do not claim nonce');
$valid = signedRequest(params: ['page' => '1', 'country' => 'HK']);
$raw = $valid->rawBuffer();
foreach ([['country=HK', 'country=US'], ['/api/open/v1/nodes', '/api/open/v1/countries'], ['GET ', 'POST ']] as [$before, $after]) {
    fails(fn () => OpenApiAuth::authenticate(new Request(str_replace($before, $after, $raw))), 401, 'INVALID_SIGNATURE');
}
$bodyRequest = signedRequest(body: 'x');
fails(fn () => OpenApiAuth::authenticate(new Request(str_replace("\r\n\r\nx", "\r\n\r\ny", $bodyRequest->rawBuffer()))), 401, 'INVALID_SIGNATURE');
fails(fn () => OpenApiAuth::authenticate($bodyRequest), 400, 'INVALID_REQUEST');
fails(fn () => OpenApiAuth::authenticate(signedRequest(method: 'POST')), 400, 'INVALID_REQUEST');
$middleware = new OpenApiAuthenticate();
$called = false;
$response = $middleware->process($valid, function () use (&$called) { $called = true; return new Response(200); });
check($called && $response->getStatusCode() === 200, 'Signed request reaches handler without login');
check(Db::table('node_explorer_open_api_nonces')->count() === 1, 'Valid request claims nonce');
fails(fn () => $middleware->process($valid, fn () => new Response(200)), 409, 'REPLAY_DETECTED');
// 重新解析请求对象，模拟另一 worker，重复请求仍然失败。
fails(fn () => OpenApiAuth::authenticate(new Request($raw)), 409, 'REPLAY_DETECTED');
Db::table('node_explorer_open_api_nonces')->update(['expires_at' => time() - 1]);
OpenApiAuth::authenticate(new Request($raw));
check(Db::table('node_explorer_open_api_nonces')->count() === 1, 'Expired nonce reclaimed atomically');
check((int)Db::table('node_explorer_open_api_nonces')->value('expires_at') >= time() + 599, 'Nonce retained for full timestamp acceptance window');
$future = signedRequest(signedTimestamp: time() + 300);
OpenApiAuth::authenticate($future);
check((int)Db::table('node_explorer_open_api_nonces')->where('nonce', $future->header('x-nonce'))->value('expires_at') > (int)$future->header('x-timestamp') + 300, 'Future timestamp nonce outlives inclusive final acceptance second');
Db::statement('DROP TABLE node_explorer_open_api_nonces');
fails(fn () => OpenApiAuth::authenticate(signedRequest()), 503, 'AUTH_UNAVAILABLE');

$handler = new ApiErrorHandler(support\Log::channel(), false);
foreach ([new ApiError(400, 'invalid', 'INVALID_QUERY'), new ApiError(401, 'invalid', 'INVALID_SIGNATURE'), new ApiError(409, 'replay', 'REPLAY_DETECTED'), ApiError::readFailed()] as $error) {
    $response = $handler->render(signedRequest(), $error);
    check($response->getStatusCode() === $error->status && $response->getHeader('Cache-Control') === 'no-store', 'All open API errors are uncacheable');
}
echo "OK $passed open API assertions\n";
