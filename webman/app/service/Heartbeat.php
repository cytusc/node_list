<?php
declare(strict_types=1);

namespace app\service;

use support\Db;
use support\Log;
use Throwable;

/**
 * api 服务心跳。
 *
 * Go 版由 cmd/node-explorer 起一个 10s ticker 无条件写
 * node_explorer_service_status；PHP 版改用「惰性心跳」：跟着请求走，
 * 两次写入之间至少间隔 INTERVAL 秒。
 *
 * 为什么不是 Timer：workerman 的定时器在 Windows（无 pcntl）上不触发，
 * 而本地开发恰恰是主要调试场景；跟着请求走在 Linux/Windows 行为一致。
 * 语义上「有请求」本就说明服务活着——前端轮询 /api/services（15s）与
 * /api/results（5s）本身就是心跳的驱动力，页面关着时看板显示
 * 「服务心跳已过期」也符合事实。
 *
 * 写入内容与 Go 逐字节一致：payload 的键序按字母（Go 用 map 序列化），
 * {"error":"","running":true}。
 */
final class Heartbeat
{
    /** 与 Go 的 10s ticker 对齐的最小写入间隔（秒）。 */
    public const INTERVAL = 10.0;

    /** 本 worker 上次写入的时间戳（microtime(true)；0 = 从未写过）。 */
    private static float $last = 0.0;

    /**
     * 距上次写入已满一个间隔？供测试直接断言节流行为。
     */
    public static function shouldBeat(): bool
    {
        return self::$last === 0.0 || (microtime(true) - self::$last) >= self::INTERVAL;
    }

    /**
     * 满了间隔才写。由 ServiceBoard::statuses() 调用——服务的两个读入口
     * （/api/services 与 /api/results.ippureCollector）都经过它，且各自的
     * 缓存窗口（10s/5s）天然把频率限制在与 Go 同一量级。
     */
    public static function beatIfDue(string $service = 'api'): void
    {
        if (!self::shouldBeat()) {
            return;
        }
        self::beat($service);
    }

    /**
     * 立即写一次心跳。失败只记日志：心跳是附属信息，不能把主接口带崩
     * （Go 侧 ticker 里同样忽略写失败的返回值）。
     */
    public static function beat(string $service): void
    {
        self::$last = microtime(true);
        try {
            Db::statement(
                "INSERT INTO node_explorer_service_status(service,updated_at,payload)
                 VALUES(?,now(),?)
                 ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload",
                [$service, '{"error":"","running":true}']
            );
        } catch (Throwable $e) {
            Log::warning('heartbeat write failed: ' . $e->getMessage());
        }
    }

    /**
     * 清掉节流状态。仅测试使用——否则同一进程内先跑的用例会把后一个
     * 用例的写入节流掉，让断言随执行顺序飘。
     */
    public static function reset(): void
    {
        self::$last = 0.0;
    }
}
