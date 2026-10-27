<?php
declare(strict_types=1);

namespace app\service;

use support\Cache;
use Throwable;

/** 服务监控的短期缓存。节点查询不使用缓存。 */
final class ReadCache
{
    /**
     * @template T
     * @param callable():T $load 未命中时的加载函数；抛异常则异常外传且不写缓存
     * @return T
     */
    public function remember(string $key, int $ttlSeconds, callable $load): mixed
    {
        try {
            $hit = Cache::get($key);
            if (is_array($hit) && array_key_exists('value', $hit)) {
                return $hit['value'];
            }
        } catch (Throwable $e) {
            \support\Log::warning('cache read failed for ' . $key . ': ' . $e->getMessage());
        }

        $value = $load();

        if ($ttlSeconds > 0) {
            try {
                Cache::set($key, ['value' => $value], $ttlSeconds);
            } catch (Throwable $e) {
                \support\Log::warning('cache write failed for ' . $key . ': ' . $e->getMessage());
            }
        }
        return $value;
    }

    /** 主动失效（运维脚本/测试用）。 */
    public function forget(string $key): void
    {
        try {
            Cache::delete($key);
        } catch (Throwable) {
            // 失效失败不影响后续读路径：TTL 会兜住。
        }
    }
}
