<?php
declare(strict_types=1);
namespace app\service;

use Webman\Http\Request;

final class Auth
{
    public static function configured(): bool
    {
        return (string)env('AUTH_USERNAME', '') !== ''
            && (password_get_info((string)env('AUTH_PASSWORD_HASH', ''))['algo'] ?? null) !== null;
    }

    public static function valid(Request $request): bool
    {
        if (!self::configured() || !$request->cookie('node_explorer_session')) return false;
        $auth = $request->session()->get('auth', []);
        return ($auth['expires'] ?? 0) > time()
            && hash_equals(self::version(), (string)($auth['version'] ?? ''));
    }

    public static function version(): string
    {
        return hash('sha256', env('AUTH_USERNAME', '') . "\0" . env('AUTH_PASSWORD_HASH', ''));
    }

    // Fixed-window limits shared by all workers; never trust forwarded IP headers.
    public static function allowLogin(string $ip): bool
    {
        $dir = runtime_path('auth-limits');
        if (!is_dir($dir)) @mkdir($dir, 0700, true);
        $file = fopen($dir . '/' . hash('sha256', $ip), 'c+');
        if (!$file || !flock($file, LOCK_EX)) return false;
        try {
            $state = json_decode(stream_get_contents($file), true) ?: [];
            if (($state['until'] ?? 0) <= time()) $state = ['until' => time() + 900, 'count' => 0];
            $allowed = $state['count'] < 10;
            $state['count']++;
            rewind($file);
            ftruncate($file, 0);
            fwrite($file, json_encode($state));
            return $allowed;
        } finally {
            flock($file, LOCK_UN);
            fclose($file);
        }
    }
}
