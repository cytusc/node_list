<?php
declare(strict_types=1);

namespace app\service;

use support\Response;

/**
 * JSON 响应工厂。
 *
 * 目标是与 Go 的 writeJSON 逐字节一致：
 *   - Content-Type: application/json; charset=utf-8
 *   - 中文直出（Go 的 json.Encoder 不转义非 ASCII）
 *   - 错误体 {error}；带业务码时是 {"code":…,"error":…}（Go 用 map，键按字母序）
 *   - **末尾带一个换行**：Go 用的是 json.NewEncoder(w).Encode，它在 JSON 之后
 *     总会写一个 '\n'。少这个字节，逐字节比对就全红，虽然前端无所谓。
 *
 * 之所以不用 webman 的 json() 助手：它固定返回 200、Content-Type 无 charset、
 * 也不带结尾换行，三处都要二次处理，在这里集中写掉更不容易漏。
 */
final class JsonResponse
{
    public const JSON_FLAGS = JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE;

    /** 200 正常回包。$noStore=true 时加 Cache-Control: no-store（Go 对 /api/results 等如此）。 */
    public static function payload(array $data, bool $noStore = true): Response
    {
        return self::build(200, $data, $noStore);
    }

    /**
     * 对应 Go 的 writeError(w, status, message, code)。
     *
     * 带业务码时**必须把 code 排在 error 前面**：Go 那边错误体是个
     * `map[string]any`，encoding/json 对 map 按 key 排序输出，所以真实字节是
     * `{"code":"READ_FAILED","error":"…"}`。按插入顺序写成 error 在前，
     * 语义一样但字节不同 —— 前端无所谓，契约比对会红。
     */
    public static function error(int $status, string $message, string $code = '', bool $noStore = false): Response
    {
        $body = $code === ''
            ? ['error' => $message]
            : ['code' => $code, 'error' => $message];
        return self::build($status, $body, $noStore);
    }

    /** @param array<string, mixed> $body */
    private static function build(int $status, array $body, bool $noStore): Response
    {
        $headers = ['Content-Type' => 'application/json; charset=utf-8'];
        if ($noStore) {
            $headers['Cache-Control'] = 'no-store';
        }
        return new Response($status, $headers, json_encode($body, self::JSON_FLAGS) . "\n");
    }

    /**
     * 数字规范化：整数值的浮点按整数输出。
     *
     * Go 的 float64 走 encoding/json 时 443.0 会写成 443，而 PHP 的 json_encode
     * 会写成 443.0。前端 JSON.parse 结果相同，但接口字节与快照指纹要一致，
     * 所以所有数值出口都经过这里。
     */
    public static function number(float|int|string|null $value): int|float
    {
        if ($value === null || $value === '') {
            return 0;
        }
        if (is_string($value)) {
            $value = (float)$value;
        }
        if (is_float($value) && $value === floor($value) && abs($value) < 9.0e15) {
            return (int)$value;
        }
        return $value;
    }
}
