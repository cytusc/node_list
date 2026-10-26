<?php
declare(strict_types=1);
require __DIR__ . '/bootstrap.php';

use app\controller\AuthController;
use app\middleware\Authenticate;
use app\service\Auth;
use support\Request;
use Workerman\Connection\TcpConnection;
use Workerman\Protocols\Http\Session;

// A connection shell lets us exercise the actual framework cookie/session code without a listener.
final class AuthTestConnection extends TcpConnection
{
    public function __construct() { $this->remoteAddress = 'auth-test-' . getmypid() . ':1234'; }
    public function __destruct() {}
}
$sessionDir = sys_get_temp_dir() . '/node-list-auth-test-' . getmypid();
Session::handlerClass(Workerman\Protocols\Http\Session\FileSessionHandler::class, ['save_path' => $sessionDir]);
Session::$name = 'node_explorer_session';
Session::$sameSite = 'Strict';
Session::$httpOnly = true;
Session::$secure = true;
Session::$lifetime = 28800;
$passed = 0;
function verify(bool $condition, string $message): void {
    global $passed;
    if (!$condition) throw new RuntimeException($message);
    $passed++;
}
function authRequest(string $path, array $body = [], ?string $sid = null, bool $trusted = true): Request {
    $json = json_encode($body);
    $raw = "POST $path HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: " . strlen($json) . "\r\n";
    if ($sid) $raw .= "Cookie: node_explorer_session=$sid\r\n";
    if ($trusted) $raw .= "X-Node-Explorer: 1\r\n";
    $request = new Request($raw . "\r\n" . $json);
    $request->connection = new AuthTestConnection();
    return $request;
}
try {
    putenv('AUTH_USERNAME=');
    putenv('AUTH_PASSWORD_HASH=');
    $controller = new AuthController();
    verify(!Auth::configured(), 'No credentials must fail closed');
    verify($controller->login(authRequest('/api/auth/login'))->getStatusCode() === 503, 'Unconfigured login denied');
    putenv('AUTH_USERNAME=admin');
    putenv('AUTH_PASSWORD_HASH=' . password_hash('test-password-12345', PASSWORD_DEFAULT));
    verify(Auth::configured(), 'Credentials recognized');
    verify($controller->login(authRequest('/api/auth/login', [], null, false))->getStatusCode() === 403, 'Cross-site login blocked');
    verify($controller->login(authRequest('/api/auth/login', ['username'=>'admin','password'=>'wrong']))->getStatusCode() === 401, 'Wrong password rejected');
    $login = authRequest('/api/auth/login', ['username'=>'admin','password'=>'test-password-12345']);
    $oldSid = $login->sessionId();
    verify($controller->login($login)->getStatusCode() === 200, 'Correct credentials accepted');
    verify($oldSid !== $login->sessionId(), 'Session id rotated');
    verify($login->session()->get('auth')['expires'] > time(), 'New session authenticated');
    $sid = $login->sessionId();
    $login->session()->save();
    $next = authRequest('/api/results', [], $sid);
    verify(Auth::valid($next), 'Session shared across requests');
    $middleware = new Authenticate();
    $called = false;
    $denied = $middleware->process(authRequest('/api/results'), function () use (&$called) { $called = true; return new support\Response(200); });
    verify($denied->getStatusCode() === 401 && !$called, 'Anonymous query denied before handler');
    verify($denied->getHeader('Cache-Control') === 'no-store', 'Auth failure cannot be cached');
    $next->session()->set('auth', ['expires' => time() - 1, 'version' => Auth::version()]);
    verify(!Auth::valid($next), 'Expired session denied');
    $next->session()->set('auth', ['expires' => time() + 28800, 'version' => Auth::version()]);
    putenv('AUTH_PASSWORD_HASH=' . password_hash('changed-password-12345', PASSWORD_DEFAULT));
    verify(!Auth::valid($next), 'Password rotation invalidates sessions');
    $next->session()->flush();
    $next->session()->save();
    $controller->logout($login);
    verify(!$login->session()->get('auth'), 'Logout clears auth');
    $login->session()->save();
    verify(!Auth::valid(authRequest('/api/results', [], $sid)), 'Logged out cookie cannot be replayed');
    $ip = 'auth-rate-test-' . getmypid();
    for ($i = 0; $i < 10; $i++) verify(Auth::allowLogin($ip), 'Initial login attempts allowed');
    verify(!Auth::allowLogin($ip), 'Further login attempts limited across requests');
    // In-memory database verifies exact rawLink retrieval, including config-only nodes.
    $capsule = new Webman\Database\Manager();
    $capsule->addConnection(['driver' => 'sqlite', 'database' => ':memory:', 'prefix' => '']);
    $capsule->setAsGlobal();
    support\Db::statement('CREATE TABLE node_explorer_live_facts (identity TEXT PRIMARY KEY, node TEXT)');
    $original = 'vless://stored@example.org:443?path=%2Fa#%E8%8A%82%E7%82%B9';
    support\Db::table('node_explorer_live_facts')->insert([
        ['identity' => 'with-link', 'node' => json_encode(['rawLink' => $original, 'rawConfig' => ['password' => 'never-export-this']])],
        ['identity' => 'config-only', 'node' => json_encode(['rawConfig' => ['type' => 'vless', 'uuid' => 'do-not-build-a-link']])],
    ]);
    $links = new app\controller\NodeLinkController();
    $copied = $links->index(authRequest('/api/nodes/link', ['identity' => 'with-link']));
    verify(json_decode($copied->rawBody(), true)['content'] === $original, 'Original database link returned byte for byte');
    verify(!str_contains($copied->rawBody(), 'never-export-this'), 'No rawConfig in link response');
    verify($copied->getHeader('Cache-Control') === 'no-store', 'Link response cannot be cached');
    verify($links->index(authRequest('/api/nodes/link', ['identity' => 'config-only']))->getStatusCode() === 422, 'Configuration-only node never produces a link');
    verify($links->index(authRequest('/api/nodes/link', ['identity' => 'absent']))->getStatusCode() === 404, 'Unavailable node cannot be copied');
    verify($links->index(authRequest('/api/nodes/link', ['identity' => []]))->getStatusCode() === 400, 'Invalid identifier rejected');
    verify($links->index(authRequest('/api/nodes/link', ['identity' => 'with-link'], null, false))->getStatusCode() === 403, 'Cross-site link request blocked');
    echo "OK $passed auth/link assertions\n";
} finally {
    foreach (glob($sessionDir . '/*') ?: [] as $file) unlink($file);
    @rmdir($sessionDir);
    foreach (['auth-test-' . getmypid(), 'auth-rate-test-' . getmypid()] as $ip) @unlink(runtime_path('auth-limits') . '/' . hash('sha256', $ip));
}
