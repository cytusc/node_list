<?php
declare(strict_types=1);

namespace app\service;

use support\Cache;
use Throwable;

/**
 * 读侧短 TTL 缓存（PSR-16 之上的薄封装）。
 *
 * 对应 Go 里 cmd/node-explorer 的 ttlCache，但按 PHP 侧的习惯做了调整：
 *
 *   1. 落在 webman 的缓存组件上（默认 file 驱动），**多个 worker 共享**，
 *      不像 Go 版那样每个进程各存一份。
 *   2. 仍然坚持「加载失败绝不写缓存」：上游抖动一次不该把错误状态钉住整个窗口。
 *   3. 缓存层自身故障（磁盘/权限）只降级为直查，不影响接口正确性 —— 但
 *      **加载函数的异常原样外传**，不会被误当成缓存故障而重试。
 *
 * 为什么内部再包一层 ['value' => ...]：
 * PSR-16 的 get() 用 null 表示 miss，而这里的合法值可能是 null
 * （sourceWarning 的「无告警」），包一层才能区分「没缓存」和「缓存了 null」。
 */
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
