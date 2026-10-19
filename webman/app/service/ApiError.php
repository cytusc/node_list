<?php
declare(strict_types=1);

namespace app\service;

/**
 * 接口层可预期的失败。
 *
 * 对应 Go 里的 writeError / writeUnavailable：状态码 + 中文文案 + 可选业务码。
 * 前端只依赖 error（展示）与 code（判定分支，目前只有 NO_RESULTS / SNAPSHOT_NOT_FOUND /
 * READ_FAILED 三处），因此字段名与语义必须与 Go 完全一致。
 *
 * 该异常由 app\exception\ApiErrorHandler 统一转成 JSON，控制器里不需要 try/catch。
 */
final class ApiError extends \RuntimeException
{
    public function __construct(
        /** HTTP 状态码，例如 400 / 404 / 503 */
        public readonly int $status,
        string $message,
        /** 业务码，空串表示不输出 code 字段（与 Go 的 writeError 一致） */
        public readonly string $errorCode = '',
    ) {
        parent::__construct($message);
    }

    /** 400：分页/筛选/排序参数不合法。 */
    public static function invalidQuery(string $message): self
    {
        return new self(400, $message);
    }

    /** 503：数据库读失败（Go 的 writeUnavailable）。 */
    public static function readFailed(): self
    {
        return new self(503, '检测结果暂时无法读取，请稍后刷新重试。', 'READ_FAILED');
    }

    /** 404：路由不存在或资源不存在。 */
    public static function notFound(string $message, string $code = ''): self
    {
        return new self(404, $message, $code);
    }
}
