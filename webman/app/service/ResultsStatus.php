<?php
declare(strict_types=1);

namespace app\service;

use app\model\ServiceStatus;
use DateTimeImmutable;
use DateTimeZone;
use support\Db;
use support\Log;
use Throwable;

/** 附加在节点结果中的采集器状态与来源告警。 */
final class ResultsStatus
{
    private ReadCache $cache;

    /**
     * 容器用 `new ResultsStatus()` 构造，因此依赖有默认值；测试可注入替身。
     */
    public function __construct(?ReadCache $cache = null)
    {
        $this->cache = $cache ?? new ReadCache();
    }

    /**
     * 采集器状态（嵌在 /api/results 里）。
     *
     * 形状与 Go 的 api.CollectorStatus 一致，且带 omitempty 语义：
     *   {"enabled":true,"running":true,"queued":18,"lastRunAt":"...","error":"..."}
     * 读不到状态时降级为 {"enabled":true,"error":"服务状态暂不可用"}，
     * 不让整个结果接口跟着 503 —— 采集器健康度是附属信息。
     *
     * @return array<string, mixed>
     */
    public function collectorStatus(): array
    {
        try {
            return $this->cache->remember(
                'node-list:collector',
                (int)config('api.cache.collector_seconds', 5),
                function (): array {
                    $status = $this->collectorSnapshot();
                    $value = ['enabled' => true];
                    if ($status['healthy']) {
                        $value['running'] = true;
                    }
                    if ($status['queued'] !== 0) {
                        $value['queued'] = $status['queued'];
                    }
                    // Go 侧用 time.RFC3339（秒精度）。
                    $value['lastRunAt'] = $status['updatedAtRfc3339'];
                    if ($status['error'] !== '') {
                        $value['error'] = $status['error'];
                    }
                    return $value;
                }
            );
        } catch (Throwable $e) {
            Log::error('collector status query failed: ' . $e->getMessage());
            return ['enabled' => true, 'error' => '服务状态暂不可用'];
        }
    }

    /**
     * 上游文件读取告警。
     *
     * Go 版由 10s ticker 把 source-ingest 心跳里的 payload->>'sourceWarning'
     * 抄进内存；这里直接读同一行心跳，缓存窗口对齐 10s，语义等价且少一个后台协程。
     * 返回 null 表示「上游文件读取正常」（前端据此不显示告警条）。
     *
     * 用 Eloquent 而不是裸 SQL：单表按主键取一行，是 ORM 最合适的场景。
     *
     * @return string|null
     */
    public function sourceWarning(): ?string
    {
        try {
            return $this->cache->remember(
                'node-list:source-warning',
                (int)config('api.cache.warning_seconds', 10),
                static function (): ?string {
                    /** @var ServiceStatus|null $status */
                    $status = ServiceStatus::query()->find('source-ingest', ['service', 'payload']);
                    $warning = $status?->payload['sourceWarning'] ?? '';
                    return $warning === '' ? null : (string)$warning;
                }
            );
        } catch (Throwable $e) {
            Log::error('source warning query failed: ' . $e->getMessage());
            return null;
        }
    }

    /** 仅查询结果接口需要的 IP 服务心跳与到期队列。 */
    private function collectorSnapshot(): array
    {
        Heartbeat::beatIfDue('api');
        $hearts = Db::table('node_explorer_service_status')
            ->whereIn('service', ['exit-provision', 'exit-attributes'])->get();
        $healthy = count($hearts) === 2;
        $updated = '0001-01-01 00:00:00+00';
        $error = $healthy ? '' : '服务尚未启动';
        foreach ($hearts as $heart) {
            $payload = json_decode((string)$heart->payload, true) ?: [];
            $updated = max($updated, (string)$heart->updated_at);
            if (strtotime((string)$heart->updated_at) <= time() - 90) {
                $healthy = false;
                $error = '服务心跳已过期';
            } elseif (($payload['running'] ?? false) !== true || !empty($payload['error'])) {
                $healthy = false;
                $error = '服务正在准备或报告异常';
            }
        }
        return ['healthy' => $healthy, 'queued' => 0,
            'updatedAtRfc3339' => self::rfc3339($updated), 'error' => $error];
    }

    /** 采集器状态用：Go 的 time.RFC3339（秒精度，无小数）。 */
    public static function rfc3339(string $raw): string
    {
        return self::render($raw);
    }

    /** 按 Go 的秒精度时间格式渲染；无法解析时退回 Go 的零值字面量。 */
    private static function render(string $raw): string
    {
        $dt = self::parse($raw);
        if ($dt === null) {
            return '0001-01-01T00:00:00Z';
        }
        $dt = $dt->setTimezone(self::fixedLocalZone());
        // Go 的格式串是 Z07:00：偏移为 0 时输出 Z，而不是 +00:00。
        $offset = $dt->getOffset() === 0 ? 'Z' : $dt->format('P');
        return $dt->format('Y-m-d\TH:i:s') . $offset;
    }

    /**
     * 「本地时区当前偏移」的固定时区。
     *
     * 不能直接用 new DateTimeZone(date_default_timezone_get())：那会套用历史
     * 规则，年份 1 的时间会被渲染成 LMT（+08:05:43），与 Go 的输出对不上。
     */
    private static function fixedLocalZone(): DateTimeZone
    {
        $offset = (new DateTimeImmutable('now', new DateTimeZone(date_default_timezone_get())))->getOffset();
        if ($offset === 0) {
            return new DateTimeZone('UTC');
        }
        $abs = abs($offset);
        return new DateTimeZone(sprintf('%s%02d:%02d', $offset < 0 ? '-' : '+', intdiv($abs, 3600), intdiv($abs % 3600, 60)));
    }

    /**
     * 解析时间戳；解析不了返回 null（由调用方输出 Go 的零值字面量）。
     *
     * 这里**不做**「年份 < 100 视为零值」的特殊判断：PG 的 `coalesce(x,'0001-01-01')`
     * 会把 0001 年解析成会话时区下的某个瞬时，Go 侧拿到的是同一个瞬时并按同一个
     * 偏移渲染，照抄这个瞬时才是真正的对齐（见 render 的注释）。
     */
    private static function parse(string $raw): ?DateTimeImmutable
    {
        if ($raw === '') {
            return null;
        }
        try {
            return new DateTimeImmutable($raw, new DateTimeZone('UTC'));
        } catch (Throwable) {
            return null;
        }
    }

}
