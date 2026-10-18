<?php
declare(strict_types=1);

namespace app\service;

use app\model\ServiceStatus;
use DateTimeImmutable;
use DateTimeZone;
use stdClass;
use support\Db;
use support\Log;
use Throwable;

/**
 * 服务看板与采集器状态。
 *
 * 对应 Go 的 service.Repository.statuses()（Status/Statuses 已随接口层删除）：
 *
 *   GET /api/services                 → 6 个服务（10s 缓存，读失败 503）
 *   GET /api/results.ippureCollector  → 只看 ip-service（5s 缓存，读失败降级）
 *
 * api 自己的心跳由 Heartbeat 在 statuses() 入口惰性写入。
 *
 * 两个入口共用一条聚合 SQL：工作队列的统计（queued/scheduled/running/checked）
 * 需要 FILTER + 多表 LEFT JOIN，用 ORM 表达只会变成 N 次查询，所以保持原生 SQL；
 * 而单表按主键读 sourceWarning 这种简单场景交给 Eloquent（见 sourceWarning）。
 */
final class ServiceBoard
{
    /** 看板固定展示的服务与顺序。 */
    public const BOARD_SERVICES = [
        'api', 'source-ingest', 'connectivity', 'ip-service', 'residential-recovery', 'node-lifecycle',
    ];

    private ReadCache $cache;

    /**
     * 容器用 `new ServiceBoard()` 构造，因此依赖有默认值；测试可注入替身。
     */
    public function __construct(?ReadCache $cache = null)
    {
        $this->cache = $cache ?? new ReadCache();
    }

    /**
     * 状态聚合 SQL。
     *
     * 与 Go 的 statuses() 逐行对应，唯一的改动是给每列起了别名：
     * PDO 会把无别名的同名表达式列（多个 coalesce）互相覆盖，按列名取值才安全。
     */
    private const STATUS_SQL = <<<'SQL'
WITH requested(service) AS (SELECT unnest(ARRAY[%s]::text[])),
 kinds(service,kind) AS (VALUES ('connectivity','connectivity'),('ip-service','exit'),
 ('ip-service','attributes'),('ip-service','risk'),('residential-recovery','recovery'),('node-lifecycle','lifecycle')),
 work AS (
 SELECT k.service,count(*) FILTER(WHERE w.state='queued' AND w.due_at<=now()) AS queued,
 count(*) FILTER(WHERE w.state='queued' AND w.due_at>now()) AS scheduled,
 count(*) FILTER(WHERE w.state='running') AS running,
 min(w.due_at) FILTER(WHERE w.state='queued' AND w.due_at<=now()) AS oldest,
 count(*) FILTER(WHERE w.state='done' AND w.payload->>'recoveryResult' IN ('eligible','not-residential','probe-failed','exit-changed','timed-out')) AS checked,
 count(*) FILTER(WHERE w.state='done' AND w.payload->>'recoveryResult'='skipped-datacenter-asn') AS skipped
 FROM requested r JOIN kinds k USING(service) JOIN node_explorer_work w ON w.kind=k.kind
 WHERE w.state<>'done' OR w.kind='recovery' GROUP BY k.service
 )
 SELECT r.service AS service,
 coalesce(h.updated_at,'0001-01-01'::timestamptz) AS updated_at,
 coalesce(h.updated_at>now()-interval '90 seconds' AND coalesce(h.payload->>'error','')='',false) AS healthy,
 CASE WHEN h.updated_at IS NULL THEN '服务尚未启动' WHEN h.updated_at<=now()-interval '90 seconds' THEN '服务心跳已过期' ELSE coalesce(h.payload->>'error','') END AS error,
 coalesce(w.queued,0) AS queued,coalesce(w.scheduled,0) AS scheduled,coalesce(w.running,0) AS running,
 coalesce(greatest(0,extract(epoch FROM now()-w.oldest)),0)::double precision AS oldest_wait,
 CASE WHEN r.service='node-lifecycle' THEN (SELECT count(*) FROM node_explorer_lifecycle_events WHERE action='restore' AND NOT dry_run) ELSE 0 END AS restored,
 coalesce(w.checked,0) AS checked,coalesce(w.skipped,0) AS skipped
 FROM requested r LEFT JOIN node_explorer_service_status h USING(service) LEFT JOIN work w USING(service)
 ORDER BY r.service
SQL;

    /**
     * @return array{services: list<array<string, mixed>>}
     * @throws ApiError 503
     */
    public function services(): array
    {
        try {
            $services = $this->cache->remember(
                'node-list:services',
                (int)config('api.cache.services_seconds', 10),
                fn (): array => array_map(
                    self::present(...),
                    $this->statuses(self::BOARD_SERVICES)
                )
            );
        } catch (ApiError $e) {
            throw $e;
        } catch (Throwable $e) {
            Log::error('service board query failed: ' . $e->getMessage());
            throw ApiError::readFailed();
        }
        return ['services' => $services];
    }

    /**
     * 内部状态行 → /api/services 的公开字段。
     *
     * statuses() 返回的是「查询结果」，额外带了 updatedAtRfc3339（供采集器状态用
     * 秒精度时间）。Go 的 service.Status 结构里没有这个字段，且 Error 带
     * omitempty，所以对外输出必须过一遍投影，不能直接把内部行塞进响应——
     * 否则既多一个键、又多一个空字符串 error。
     *
     * @param array<string, mixed> $status
     * @return array<string, mixed>
     */
    public static function present(array $status): array
    {
        $out = [];
        foreach (['service', 'updatedAt', 'healthy', 'error', 'queued', 'scheduled',
                  'running', 'oldestWaitSeconds', 'restored', 'checked', 'skipped'] as $key) {
            if ($key === 'error' && ($status[$key] ?? '') === '') {
                continue; // Go: json:"error,omitempty"
            }
            $out[$key] = $status[$key] ?? null;
        }
        return $out;
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
                    $status = $this->statuses(['ip-service'])[0] ?? null;
                    if ($status === null) {
                        return ['enabled' => true, 'error' => '服务状态暂不可用'];
                    }
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

    /**
     * 查询一组服务的状态。
     *
     * 入口顺带写一次 api 心跳（满了 10s 间隔才写）：Go 版由 node-explorer 的
     * ticker 负责，PHP 版跟着请求走，见 Heartbeat 的说明。写在这条路径上
     * 是因为 services() 与 collectorStatus() 都经过它，一个接入点覆盖
     * 两个接口；失败不影响查询结果。
     *
     * @param list<string> $services
     * @return list<array<string, mixed>>
     * @throws Throwable
     */
    public function statuses(array $services): array
    {
        Heartbeat::beatIfDue('api');
        // unnest(ARRAY[?,?,...]) 与 Go 的 unnest($1::text[]) 等价，逐项绑定参数。
        $placeholders = implode(',', array_fill(0, count($services), '?'));
        $rows = Db::select(sprintf(self::STATUS_SQL, $placeholders), array_values($services));

        $out = [];
        foreach ($rows as $row) {
            if (!$row instanceof stdClass) {
                continue;
            }
            $out[] = [
                'service' => (string)$row->service,
                'updatedAt' => self::goTime((string)$row->updated_at),
                'updatedAtRfc3339' => self::rfc3339((string)$row->updated_at),
                'healthy' => self::toBool($row->healthy),
                'error' => (string)$row->error,
                'queued' => (int)$row->queued,
                'scheduled' => (int)$row->scheduled,
                'running' => (int)$row->running,
                'oldestWaitSeconds' => (float)$row->oldest_wait,
                'restored' => (int)$row->restored,
                'checked' => (int)$row->checked,
                'skipped' => (int)$row->skipped,
            ];
        }
        return $out;
    }

    /**
     * /api/services 输出用：Go 的 time.Time 走 encoding/json 是 RFC3339Nano
     * （微秒尾随零裁掉）。
     *
     * 时区处理与 Go 一致：把**时刻**渲染在「本地时区的当前偏移」上。
     * Go 那边 pgx 扫出的 time.Time 落在进程本地时区，`Format` 用的是该时区
     * 当下的偏移（Windows 的时区数据没有历史 LMT，所以零值时间会渲染成
     * `0000-12-31T23:54:17+08:00` 而不是 `…+08:05:43`）。这里用「当前偏移」
     * 构造一个固定偏移时区来对齐，不跟随历史规则。
     *
     * 这样在 UTC 部署下输出 `…Z`，在 +08 部署下输出 `+08:00`，与 Go 一致；
     * 前提是 PHP 的 app.default_timezone 与 Go 进程的本地时区相同
     * （容器化时两边都用 TZ=UTC，默认配置也按这个来）。
     */
    public static function goTime(string $raw): string
    {
        return self::render($raw, true);
    }

    /** 采集器状态用：Go 的 time.RFC3339（秒精度，无小数）。 */
    public static function rfc3339(string $raw): string
    {
        return self::render($raw, false);
    }

    /** 按 Go 的两种时间格式渲染；无法解析时退回 Go 的零值字面量。 */
    private static function render(string $raw, bool $withFraction): string
    {
        $dt = self::parse($raw);
        if ($dt === null) {
            return '0001-01-01T00:00:00Z';
        }
        $dt = $dt->setTimezone(self::fixedLocalZone());
        $fraction = $withFraction ? rtrim($dt->format('u'), '0') : '';
        // Go 的格式串是 Z07:00：偏移为 0 时输出 Z，而不是 +00:00。
        $offset = $dt->getOffset() === 0 ? 'Z' : $dt->format('P');
        return $dt->format('Y-m-d\TH:i:s') . ($fraction === '' ? '' : '.' . $fraction) . $offset;
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

    /** pdo_pgsql 的 bool 可能是 't'/'f'，直接 (bool)'f' 会得到 true。 */
    private static function toBool(mixed $value): bool
    {
        if (is_string($value)) {
            return $value !== '' && $value !== 'f' && $value !== 'false' && $value !== '0';
        }
        return (bool)$value;
    }
}
