<?php
declare(strict_types=1);
namespace app\controller;

use app\service\{Auth, JsonResponse};
use support\{Request, Response};

final class AuthController
{
    public function status(Request $request): Response
    {
        return JsonResponse::payload(['authenticated' => Auth::valid($request), 'configured' => Auth::configured()]);
    }

    public function login(Request $request): Response
    {
        if ($request->header('x-node-explorer') !== '1') return JsonResponse::error(403, '请求来源无效。', '', true);
        if (!Auth::configured()) return JsonResponse::error(503, '管理员尚未配置登录账号，请完成服务端登录配置。', '', true);
        if (!Auth::allowLogin($request->getRemoteIp())) return JsonResponse::error(429, '登录尝试过于频繁，请 15 分钟后再试。', '', true);
        $username = $request->post('username');
        $password = $request->post('password');
        $validPassword = is_string($password) && strlen($password) <= 1024
            && password_verify($password, (string)env('AUTH_PASSWORD_HASH', ''));
        if (!is_string($username) || !hash_equals((string)env('AUTH_USERNAME', ''), $username) || !$validPassword) {
            return JsonResponse::error(401, '账号或密码错误。', '', true);
        }
        $request->sessionRegenerateId(true);
        $request->session()->set('auth', ['expires' => time() + 28800, 'version' => Auth::version()]);
        return JsonResponse::payload(['authenticated' => true]);
    }

    public function logout(Request $request): Response
    {
        if ($request->header('x-node-explorer') !== '1') return JsonResponse::error(403, '请求来源无效。', '', true);
        $request->session()->flush();
        return JsonResponse::payload(['authenticated' => false]);
    }
}
