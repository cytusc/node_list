<?php
/**
 * 路由表。接口面是白名单，用 webman 的分组 + 路由中间件表达
 * 「这一组接口共用访问日志」：
 *
 *   /api/health    → 200 {"status":"ok"}
 *   /api/results   → 实时视图（live）
 *   /api/downloads → 404 「订阅下载暂不可用。」
 *   /api/download  → 404 「订阅下载暂不可用。」
 *   /api/*         → 404 「接口不存在。」
 */

use app\controller\DownloadController;
use app\controller\HealthController;
use app\controller\ResultsController;
use app\controller\StaticController;
use app\controller\ServicesController;
use app\middleware\AccessLog;
use app\service\JsonResponse;
use support\Request;
use support\Response;
use Webman\Route;

// 关闭默认路由（/{controller}/{action}）：接口面必须是白名单，
// 否则控制器里任何 public 方法都能被 URL 直接命中。
Route::disableDefaultRoute();

Route::group('/api', function () {
    Route::get('/health', [HealthController::class, 'index'])->name('api.health');
    Route::get('/services', [ServicesController::class, 'index'])->name('api.services');
    Route::get('/results', [ResultsController::class, 'index'])->name('api.results');

    // 订阅下载是显式 404 占位（"订阅下载暂不可用。"），语义是「功能未开」
    // 而不是「路径写错」，所以必须保留具名路由、不能交给兜底。
    Route::any('/downloads', [DownloadController::class, 'unavailable'])->name('api.downloads');
    Route::any('/download', [DownloadController::class, 'unavailable'])->name('api.download');

    // 其余 /api/* 一律「接口不存在。」
    Route::any('/[{path:.+}]', static fn (Request $request) => JsonResponse::error(404, '接口不存在。'))
        ->name('api.notfound');
})->middleware([AccessLog::class]);

// 前端入口：框架的静态处理只认「真实文件」，目录请求需要显式映射到 index.html，
// 否则 GET / 会落到下面的 fallback 变成 404（Go 的 http.FileServer 会自动补）。
Route::get('/', [StaticController::class, 'root'])->name('static.root');
Route::get('/services', [StaticController::class, 'root'])->name('static.services');

// 前端静态资源由 public/ + 框架的 findFile 先处理，未命中的路径落到这里。
// 不挂在 /api 组上，避免静态资源也走访问日志。
//
// 404 响应逐字对齐 Go 的 http.FileServer：body 是 "404 page not found\n"，
// Content-Type 带 charset，并补 nosniff（FileServer 会自己加）。前端只在
// 「打开一个不存在的静态路径」时看到它，改动没意义但差异也没必要留。
Route::fallback(function (Request $request) {
    if (str_starts_with($request->path(), '/api/')) {
        return JsonResponse::error(404, '接口不存在。');
    }
    return new Response(404, [
        'Content-Type' => 'text/plain; charset=utf-8',
        'X-Content-Type-Options' => 'nosniff',
    ], "404 page not found\n");
});
