<?php
declare(strict_types=1);

namespace app\service;

use app\model\ServiceStatus;
use DateTimeImmutable;
use DateTimeZone;
use support\Db;
use support\Log;
use Throwable;

/**
 * Service snapshots: durable work, heartbeats, immutable assessment statistics,
 * and residential retry cooldowns. Cache the observation time with the data so
 * polling a cached response cannot make an old snapshot appear fresh.
 */
final class ServiceBoard
{
    /** 看板固定展示的服务与顺序。 */
    public const BOARD_SERVICES = [
        'mihomo-manager', 'api', 'source-ingest', 'connectivity', 'ip-service', 'residential-recovery', 'normal-recovery', 'node-cleanup', 'node-lifecycle',
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
     * work 队列的 kind → service 归属（对应旧 SQL 的 kinds VALUES）。
     * ip-service 的三个 kind（exit/attributes/risk）合并统计到同一服务。
     */
    private const KIND_SERVICES = [
        'fast-delay' => 'connectivity',
        'connectivity' => 'connectivity',
        'exit' => 'ip-service',
        'attributes' => 'ip-service',
        'risk' => 'ip-service',
        'recovery' => 'residential-recovery',
        'normal-recovery' => 'normal-recovery',
        'node-cleanup' => 'node-cleanup',
        'lifecycle' => 'node-lifecycle',
    ];

    /** 空的工作队列统计（服务没有对应 work 行时输出 0）。 */
    private const EMPTY_STATS = [
        'queued' => 0, 'scheduled' => 0, 'running' => 0,
        'checked' => 0, 'skipped' => 0, 'oldestWaitSeconds' => 0.0,
        'activeAssessments' => 0, 'checkedLastHour' => 0,
        'blocked' => 0, 'expired' => 0, 'cooldown' => 0,
    ];

    /**
     * @return array{services: list<array<string, mixed>>, observedAt: string}
     * @throws ApiError 503
     */
    public function services(): array
    {
        try {
            $snapshot = $this->cache->remember(
                'node-list:services:v2',
                (int)config('api.cache.services_seconds', 10),
                fn (): array => [
                    'services' => array_map(self::present(...), $this->statuses(self::BOARD_SERVICES)),
                    'observedAt' => self::goTime((string)Db::selectOne('SELECT now() AS stamp')->stamp),
                ]
            );
        } catch (ApiError $e) {
            throw $e;
        } catch (Throwable $e) {
            Log::error('service board query failed: ' . $e->getMessage());
            throw ApiError::readFailed();
        }
        return $snapshot;
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
                  'running', 'oldestWaitSeconds', 'restored', 'checked', 'skipped',
                  'activeAssessments', 'checkedLastHour', 'drainEtaSeconds',
                  'blocked', 'expired', 'cooldown', 'restoredNodes'] as $key) {
            if ($key === 'error' && ($status[$key] ?? '') === '') {
                continue; // Go: json:"error,omitempty"
            }
            $out[$key] = $status[$key] ?? null;
        }
        foreach (['fast', 'kernel'] as $key) {
            if (isset($status[$key])) { $out[$key] = $status[$key]; }
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
     * 输出按服务名字母序（与旧 SQL 的 ORDER BY r.service 一致，不是
     * BOARD_SERVICES 的传入顺序）。services() 之外的传入集合（如
     * collectorStatus 只传 ip-service）也按字母序返回。
     *
     * @param list<string> $services
     * @return list<array<string, mixed>>
     * @throws Throwable
     */
    public function statuses(array $services): array
    {
        Heartbeat::beatIfDue('api');

        $stats = $this->workStats();
        $hearts = $this->heartbeats($services);
        $restored = in_array('node-lifecycle', $services, true) ? $this->restoredCounts() : ['events' => 0, 'nodes' => 0];
        $assessments = $this->assessmentStats();
        $cooldown = in_array('residential-recovery', $services, true) ? $this->cooldownStats() : null;

        // kind 统计按 KIND_SERVICES 归并到服务（ip-service 三个 kind 相加）。
        $byService = [];
        foreach ($stats as $kind => $s) {
            $svc = self::KIND_SERVICES[$kind] ?? null;
            if ($svc === null || !in_array($svc, $services, true)) {
                continue;
            }
            if (!isset($byService[$svc])) {
                $byService[$svc] = self::EMPTY_STATS;
            }
            foreach (['queued', 'scheduled', 'running', 'checked', 'skipped', 'activeAssessments', 'checkedLastHour', 'blocked', 'expired', 'cooldown'] as $k) {
                $byService[$svc][$k] += $s[$k];
            }
            // oldest_wait 各 kind 基于 DB 同一 now() 计算，取最大即最旧。
            $byService[$svc]['oldestWaitSeconds'] = max($byService[$svc]['oldestWaitSeconds'], $s['oldestWaitSeconds']);
        }

        $out = [];
        $ordered = $services;
        sort($ordered);
        foreach ($ordered as $svc) {
            $w = $byService[$svc] ?? self::EMPTY_STATS;
            if ($svc === 'residential-recovery' && $cooldown !== null) {
                $w['scheduled'] += (int)$cooldown->scheduled;
                $w['queued'] += (int)$cooldown->queued;
                $w['cooldown'] = (int)$cooldown->scheduled;
                $w['blocked'] += (int)$cooldown->blocked;
                $w['oldestWaitSeconds'] = max($w['oldestWaitSeconds'], (float)$cooldown->oldest_wait);
            }
            $kind = match ($svc) {
                'residential-recovery' => 'recovery',
                'normal-recovery' => 'normal-recovery',
                default => null,
            };
            if ($kind !== null) {
                $w = array_replace($w, $assessments[$kind] ?? []);
            }
            $h = $hearts[$svc] ?? ['updated_at' => '0001-01-01 00:00:00+00', 'healthy' => false, 'error' => '服务尚未启动'];
            $out[] = array_merge($h['metrics'] ?? [], [
                'service' => $svc,
                'updatedAt' => self::goTime((string)$h['updated_at']),
                'updatedAtRfc3339' => self::rfc3339((string)$h['updated_at']),
                'healthy' => (bool)$h['healthy'],
                'error' => (string)$h['error'],
                'queued' => (int)$w['queued'],
                'scheduled' => (int)$w['scheduled'],
                'running' => (int)$w['running'],
                'oldestWaitSeconds' => (float)$w['oldestWaitSeconds'],
                'restored' => $svc === 'node-lifecycle' ? $restored['events'] : 0,
                'restoredNodes' => $svc === 'node-lifecycle' ? $restored['nodes'] : 0,
                'blocked' => (int)$w['blocked'],
                'expired' => (int)$w['expired'],
                'cooldown' => (int)$w['cooldown'],
                'checked' => (int)$w['checked'],
                'skipped' => (int)$w['skipped'],
                'activeAssessments' => (int)$w['activeAssessments'],
                'checkedLastHour' => (int)$w['checkedLastHour'],
                // This estimates only already-due work using the rolling hour,
                // not the next scheduled round or future arrivals.
                'drainEtaSeconds' => $w['checkedLastHour'] > 0
                    ? (int)ceil(($w['queued'] + $w['running']) * 3600 / $w['checkedLastHour']) : null,
            ]);
        }
        return $out;
    }

    /**
     * work 队列按 kind 聚合的统计。
     *
     * 仅统计当前任务；核验结果由 assessmentStats() 独立统计。
     * 活跃核验包含等待前置探测的会话，执行中仅含有效租约。
     *
     * @return array<string, array{queued:int,scheduled:int,running:int,checked:int,skipped:int,oldestWaitSeconds:float}>
     */
    private function workStats(): array
    {
        $rows = Db::table('node_explorer_work')
            ->select([
                'kind',
                Db::raw("count(*) FILTER (WHERE state='queued' AND due_at<=now()) AS queued"),
                Db::raw("count(*) FILTER (WHERE state='queued' AND due_at>now()) AS scheduled"),
                Db::raw("count(*) FILTER (WHERE state='running' AND lease_until>now()) AS running"),
                Db::raw("count(*) FILTER (WHERE kind IN ('recovery','normal-recovery') AND state<>'done' AND coalesce(payload->>'startedAt','')<>'' AND payload->>'startedAt' NOT LIKE '0001-%') AS active_assessments"),
                Db::raw("count(*) FILTER (WHERE state<>'done' AND (error LIKE 'node_config_rejected:%' OR payload->>'recoveryResult'='config-rejected')) AS blocked"),
                Db::raw("count(*) FILTER (WHERE state='running' AND (lease_until IS NULL OR lease_until<=now())) AS expired"),
                Db::raw("coalesce(greatest(0,extract(epoch FROM now()-min(due_at) FILTER (WHERE state='queued' AND due_at<=now())))::double precision,0) AS oldest_wait"),
            ])
            ->where(function ($q): void {
                $q->where('state', '!=', 'done')->orWhereIn('kind', ['recovery', 'normal-recovery']);
            })
            ->groupBy('kind')
            ->get()->all();

        $stats = [];
        foreach ($rows as $row) {
            $stats[$row->kind] = [
                'queued' => (int)$row->queued,
                'scheduled' => (int)$row->scheduled,
                'running' => (int)$row->running,
                'checked' => 0,
                'activeAssessments' => (int)$row->active_assessments,
                'checkedLastHour' => 0,
                'skipped' => 0,
                'blocked' => (int)$row->blocked,
                'expired' => (int)$row->expired,
                'cooldown' => 0,
                'oldestWaitSeconds' => (float)$row->oldest_wait,
            ];
        }
        return $stats;
    }

    /**
     * 心跳行（按服务取，healthy/错误文案在库端计算）。
     *
     * 对应旧聚合 SQL 的 h 列：90s 窗口与 now() 都在库端求值，PHP/DB 时钟
     * 漂移不影响判定；零值 updated_at 的 coalesce 也留在库里，与旧 SQL 相同。
     *
     * @param list<string> $services
     * @return array<string, array{updated_at:string,healthy:bool,error:string}>
     */
    private function heartbeats(array $services): array
    {
        if ($services === []) {
            return [];
        }
        $rows = Db::table('node_explorer_service_status')
            ->select([
                'service', 'payload',
                Db::raw("coalesce(updated_at,'0001-01-01'::timestamptz) AS updated_at"),
                Db::raw("coalesce(updated_at>now()-interval '90 seconds' AND coalesce(payload->>'error','')='',false) AS healthy"),
                Db::raw("CASE WHEN updated_at IS NULL THEN '服务尚未启动' WHEN updated_at<=now()-interval '90 seconds' THEN '服务心跳已过期' ELSE coalesce(payload->>'error','') END AS error"),
            ])
            ->whereIn('service', $services)
            ->get()->all();

        $out = [];
        foreach ($rows as $row) {
            $out[$row->service] = [
                'updated_at' => (string)$row->updated_at,
                'healthy' => self::toBool($row->healthy),
                'error' => (string)$row->error,
                'metrics' => array_intersect_key(json_decode((string)($row->payload ?? '{}'), true) ?? [], array_flip(['fast', 'kernel'])),
            ];
        }
        return $out;
    }

    /** Immutable observed outcomes, independent of a task's current state/result. */
    private function assessmentStats(): array
    {
        $rows = Db::select("SELECT t.kind,
            coalesce(sum(t.completed) FILTER (WHERE t.result<>'skipped-datacenter-asn'),0) AS checked,
            coalesce(sum(t.completed) FILTER (WHERE t.result='skipped-datacenter-asn'),0) AS skipped,
            (SELECT count(*) FROM node_explorer_recovery_events e WHERE e.kind=t.kind
                AND e.result<>'skipped-datacenter-asn'
                AND e.checked_at>now()-interval '1 hour' AND e.checked_at<=now()) AS checked_hour
            FROM node_explorer_recovery_totals t GROUP BY t.kind");
        $out = [];
        foreach ($rows as $row) {
            $out[$row->kind] = ['checked' => (int)$row->checked, 'checkedLastHour' => (int)$row->checked_hour, 'skipped' => (int)$row->skipped];
        }
        return $out;
    }

    /** Residential retries remain done during backoff; include them in waiting. */
    private function cooldownStats(): object
    {
        return Db::selectOne("WITH cooling AS (
            SELECT nullif(w.payload->>'checkedAt','')::timestamptz + CASE
                WHEN w.payload->>'recoveryResult'='config-rejected' THEN interval '6 hours'
                WHEN coalesce((w.payload->>'recoveryAttempts')::integer,0)<=1 THEN interval '5 minutes'
                WHEN (w.payload->>'recoveryAttempts')::integer=2 THEN interval '15 minutes'
                WHEN (w.payload->>'recoveryAttempts')::integer=3 THEN interval '1 hour'
                ELSE interval '6 hours' END AS next_at, w.payload->>'recoveryResult' AS result
            FROM node_explorer_work w
            JOIN node_explorer_node_registry n ON n.identity=w.payload->>'identity'
            JOIN node_explorer_retired_nodes t ON t.identity=n.identity
            WHERE w.kind='recovery' AND w.state='done' AND n.available AND n.historical_residential
              AND w.key=n.identity||':'||t.retired_at
              AND w.payload->>'retiredAt'=t.retired_at AND w.payload->>'reason'=t.reason
              AND (t.reason=? OR t.reason LIKE 'IPPure:%')
              AND w.payload->>'recoveryResult' IN ('probe-failed','exit-probe-failed','exit-changed','timed-out','classification-unconfirmed','config-rejected')
              AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)
        ) SELECT count(*) FILTER (WHERE next_at>now()) AS scheduled,
            count(*) FILTER (WHERE next_at<=now()) AS queued,
            count(*) FILTER (WHERE result='config-rejected') AS blocked,
            coalesce(greatest(0,extract(epoch FROM now()-min(next_at) FILTER (WHERE next_at<=now()))),0) AS oldest_wait
            FROM cooling", ['Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed']);
    }

    private function restoredCounts(): array
    {
        $row = Db::table('node_explorer_lifecycle_events')
            ->where('action', 'restore')->where('dry_run', false)
            ->selectRaw('count(*) AS events, count(DISTINCT identity) AS nodes')->first();
        return ['events' => (int)$row->events, 'nodes' => (int)$row->nodes];
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
