<?php
declare(strict_types=1);

namespace app\service;

/** 签名和业务参数共用严格解析，避免 PHP 对重复键、数组和键名的隐式改写。 */
final class OpenApiSignature
{
    /** @return array<string, string> */
    public static function parameters(string $query): array
    {
        if ($query === '') return [];
        if (strlen($query) > 4096) self::invalid();
        $pairs = explode('&', $query);
        if (count($pairs) > 16) self::invalid();
        $params = [];
        foreach ($pairs as $pair) {
            if ($pair === '' || preg_match('/%(?![0-9a-fA-F]{2})/', $pair)) self::invalid();
            [$key, $value] = array_pad(explode('=', $pair, 2), 2, '');
            $key = urldecode($key);
            $value = urldecode($value);
            if (!preg_match('/^[a-zA-Z][a-zA-Z0-9_-]*$/D', $key)
                || array_key_exists($key, $params)
                || !preg_match('//u', $value)
                || preg_match('/[\x00-\x1f\x7f]/', $value)) self::invalid();
            $params[$key] = $value;
        }
        return $params;
    }

    /** @param array<string, string> $params */
    public static function canonicalQuery(array $params): string
    {
        ksort($params, SORT_STRING);
        return http_build_query($params, '', '&', PHP_QUERY_RFC3986);
    }

    /** 无末尾换行，时间戳和 nonce 使用请求头原文，默认参数不补入签名。 */
    public static function message(string $method, string $path, array $params, string $body, string $timestamp, string $nonce): string
    {
        return implode("\n", [strtoupper($method), $path, self::canonicalQuery($params), hash('sha256', $body), $timestamp, $nonce]);
    }

    private static function invalid(): never
    {
        throw new ApiError(400, '查询参数格式无效，不允许重复键或数组。', 'INVALID_QUERY');
    }
}
