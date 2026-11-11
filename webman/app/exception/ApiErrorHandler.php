<?php
declare(strict_types=1);

namespace app\exception;

use app\service\ApiError;
use app\service\JsonResponse;
use support\exception\Handler;
use Throwable;
use Webman\Http\Request;
use Webman\Http\Response;

/**
 * 统一异常出口。
 *
 * 分两类：
 *   1. ApiError      —— 可预期的业务失败（400/404/503），按契约回 {"error","code"}
 *   2. 其它 Throwable —— 未预期故障，记 error 日志后回 500 的固定 JSON，
 *      绝不把堆栈/SQL 原文透给浏览器
 *
 * 4xx 不写 error 日志：参数校验失败是客户端问题，写日志只会污染告警。
 *
 * 关于 Cache-Control：/api/results 的 handler **入口**就设了
 * no-store，所以它们的失败响应（400/503）也带这个头。异常出口拿到的是
 * 「哪条路由抛的」之外的上下文，只能按请求路径补上，否则该端点的错误响应
 * 会缺一个头。
 */
final class ApiErrorHandler extends Handler
{
    /** @var string[] */
    public $dontReport = [
        ApiError::class,
    ];

    /** 这些端点的所有响应（含失败）都必须 no-store。 */
    private const NO_STORE_PATHS = ['/api/results', '/api/services'];

    public function render(Request $request, Throwable $exception): Response
    {
        $noStore = in_array($request->path(), self::NO_STORE_PATHS, true)
            || str_starts_with($request->path(), '/api/open/');

        if ($exception instanceof ApiError) {
            return JsonResponse::error($exception->status, $exception->getMessage(), $exception->errorCode, $noStore);
        }

        // 未预期异常交给父类记录（含 trace），但对外的响应固定为 JSON。
        $this->report($exception);
        $message = '服务暂时不可用，请稍后重试。';
        if (config('app.debug')) {
            // 仅调试模式附带异常摘要，方便本地定位；不含 trace。
            $message .= ' [' . $exception->getMessage() . ']';
        }
        return JsonResponse::error(500, $message, '', $noStore);
    }
}
