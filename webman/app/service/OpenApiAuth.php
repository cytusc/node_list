<?php
declare(strict_types=1);

namespace app\service;

use support\Db;
use support\Log;
use Throwable;
use Webman\Http\Request;

final class OpenApiAuth
{
    public const CLOCK_WINDOW = 300;
    // ±300 秒都包含边界，多保留 1 秒，避免未来时间戳在窗口末秒被重放。
    public const NONCE_TTL = self::CLOCK_WINDOW * 2 + 1;
    private static int $nextCleanup = 0;

    public static function authenticate(Request $request): void
    {
        $appId = (string)config('open_api.app_id', '');
        $secret = (string)config('open_api.secret', '');
        if (!preg_match('/^[a-zA-Z0-9_-]{1,64}$/D', $appId) || strlen($secret) < 32) {
            throw new ApiError(503, '对外接口未配置。', 'OPEN_API_DISABLED');
        }
        $id = (string)$request->header('x-app-id', '');
        $timestamp = (string)$request->header('x-timestamp', '');
        $nonce = (string)$request->header('x-nonce', '');
        $signature = (string)$request->header('x-signature', '');
        if (!hash_equals($appId, $id)
            || !preg_match('/^[1-9][0-9]{9}$/D', $timestamp)
            || !preg_match('/^[a-zA-Z0-9_-]{16,128}$/D', $nonce)
            || !preg_match('/^[0-9a-f]{64}$/D', $signature)) {
            throw new ApiError(401, '接口验签失败。', 'INVALID_SIGNATURE');
        }
        $now = time();
        if (abs($now - (int)$timestamp) > self::CLOCK_WINDOW) {
            throw new ApiError(401, '请求时间戳已过期或超前。', 'REQUEST_EXPIRED');
        }
        $params = OpenApiSignature::parameters($request->queryString());
        $message = OpenApiSignature::message($request->method(), $request->path(), $params, $request->rawBody(), $timestamp, $nonce);
        if (!hash_equals(hash_hmac('sha256', $message, $secret), $signature)) {
            throw new ApiError(401, '接口验签失败。', 'INVALID_SIGNATURE');
        }
        // 首版仅有 GET 接口，不接受 GET body，避免调用方和反代对其处理不一致。
        if ($request->method() !== 'GET' || $request->rawBody() !== '') {
            throw new ApiError(400, '接口只接受无请求体的 GET 请求。', 'INVALID_REQUEST');
        }
        self::claimNonce($id, $nonce, $now);
    }

    private static function claimNonce(string $appId, string $nonce, int $now): void
    {
        try {
            // 唯一键和条件 upsert 在多 worker / 多实例下也只允许一个请求成功。
            $claimed = Db::affectingStatement(
                'INSERT INTO node_explorer_open_api_nonces (app_id, nonce, expires_at) VALUES (?, ?, ?)
                 ON CONFLICT (app_id, nonce) DO UPDATE SET expires_at=excluded.expires_at
                 WHERE node_explorer_open_api_nonces.expires_at<=?',
                [$appId, $nonce, $now + self::NONCE_TTL, $now]
            );
        } catch (Throwable $e) {
            // 不记录 SQL、请求头、密钥或节点链接。
            Log::error('open api nonce storage unavailable');
            throw new ApiError(503, '接口验签服务暂不可用。', 'AUTH_UNAVAILABLE');
        }
        if ($claimed !== 1) {
            throw new ApiError(409, '请求已使用，请生成新的 nonce 和签名。', 'REPLAY_DETECTED');
        }
        if (self::$nextCleanup <= $now) {
            self::$nextCleanup = $now + 60;
            try {
                Db::delete('DELETE FROM node_explorer_open_api_nonces WHERE expires_at<=?', [$now]);
            } catch (Throwable $e) {
                // 清理失败不影响验签：过期 nonce 的回收仍由上面的条件 upsert 保证。
                Log::warning('open api expired nonce cleanup failed');
            }
        }
    }
}
