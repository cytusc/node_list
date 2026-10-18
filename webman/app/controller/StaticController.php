<?php
declare(strict_types=1);

namespace app\controller;

use app\service\JsonResponse;
use support\Response;

/**
 * 前端入口文件（public/index.html）。
 *
 * 为什么需要这个控制器：框架的静态文件处理只匹配「真实存在的文件路径」，
 * 不会把**目录请求**映射到该目录下的 index.html。于是 webman 默认情况下
 * `GET /` 会落到 fallback 返回 404 —— 打开站点首页直接白屏，属于用户可见差异。
 *
 * 这里只补目录请求语义里真正会被用到的行为：GET / → 200 index.html。
 *
 * 已知且有意不修的差异：
 *   - `GET /index.html`：静态处理在路由之前就命中真实文件，直接 200 返回，
 *     没有多余跳转。
 *   - 目录列举（public/ 里没有 index.html 时）不复刻：那是信息泄露，
 *     而且构建产物里永远有 index.html。
 */
final class StaticController
{
    public function root(): Response
    {
        $file = public_path() . '/index.html';
        if (!is_file($file)) {
            // 前端产物没同步进 public/（部署漏了一步）——明确报错好过返回空白页。
            return new Response(404, [
                'Content-Type' => 'text/plain; charset=utf-8',
                'X-Content-Type-Options' => 'nosniff',
            ], "404 page not found\n");
        }
        return new Response(200, ['Content-Type' => 'text/html; charset=utf-8'], (string)file_get_contents($file));
    }
}
