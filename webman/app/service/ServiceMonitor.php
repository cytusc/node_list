<?php
declare(strict_types=1);

namespace app\service;

use DateTimeImmutable;
use DateTimeZone;
use support\Db;

/** Read only aggregated telemetry. Never return worker payloads, node identities or errors verbatim. */
final class ServiceMonitor
{
    public const SERVICES = [
        'api' => ['节点 API', '提供节点列表与监控接口'],
        'source-ingest' => ['数据采集', '导入订阅与 Qilan 节点'],
        'exit-provision' => ['出口检测', '核验节点实际出口 IP'],
        'exit-attributes' => ['IP 属性', '更新 IP 分类与风险信息'],
        'node-liveness' => ['节点测活', '独立使用 Xray / Mihomo 更新节点可用状态'],
    ];

    public static function hours(mixed $value): int
    {
        if (!in_array($value, [null, '24', '168', 24, 168], true)) {
            throw ApiError::invalidQuery('时间范围仅支持 24 或 168 小时。');
        }
        return $value === null ? 24 : (int)$value;
    }

    public static function service(string $id, ?array $row, int $now): array
    {
        $payload = $row === null ? [] : (json_decode((string)$row['payload'], true) ?: []);
        $updated = $row === null ? false : strtotime((string)$row['updated_at']);
        $status = 'unknown';
        if ($updated !== false) {
            $status = $now - $updated > 90 ? 'stale' : 'healthy';
            if ($status === 'healthy' && (($payload['running'] ?? false) !== true || !empty($payload['error']) || !empty($payload['sourceWarning']))) {
                $status = 'degraded';
            }
        }
        return ['id' => $id, 'name' => self::SERVICES[$id][0], 'description' => self::SERVICES[$id][1],
            'status' => $status, 'updatedAt' => $updated === false ? null : gmdate('c', $updated),
            'message' => match ($status) {
                'unknown' => '尚未收到服务心跳', 'stale' => '超过 90 秒未收到心跳',
                'degraded' => '服务报告异常或正在准备，请检查运行日志', default => '心跳正常',
            }];
    }

    public function query(int $hours): array
    {
        return Db::transaction(function () use ($hours): array {
            // Read all service heartbeats from a consistent snapshot.
            Db::statement('SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY');
            Db::statement("SET LOCAL statement_timeout = '5s'");
            return $this->snapshot($hours);
        });
    }

    private function snapshot(int $hours): array
    {
        $now = (int)Db::selectOne('SELECT extract(epoch FROM now())::bigint AS stamp')->stamp;
        $rows = [];
        foreach (Db::select('SELECT service,updated_at,payload FROM node_explorer_service_status') as $row) {
            $rows[$row->service] = (array)$row;
        }
        $services = [];
        foreach (self::SERVICES as $id => $_) {
            $services[] = self::service($id, $rows[$id] ?? null, $now);
        }
        $liveness = json_decode((string)($rows['node-liveness']['payload'] ?? '{}'), true) ?: [];
        $safeLiveness = [];
        foreach (['xrayQueued', 'mihomoQueued', 'inFlight'] as $key) {
            $safeLiveness[$key] = isset($liveness[$key]) ? max(0, (int)$liveness[$key]) : null;
        }
        $stamp = (string)($liveness['lastCompletedAt'] ?? '');
        $safeLiveness['lastCompletedAt'] = $stamp !== '' && !str_starts_with($stamp, '0001-') && strtotime($stamp) !== false ? gmdate('c', strtotime($stamp)) : null;
        // Legacy response keys remain for the unchanged frontend. Shared kernel is retired.
        $kernel = [];
        $safeKernel = ['healthy' => ($kernel['healthy'] ?? false) === true];
        foreach (['loaded','quarantined','reloadMillis','reloadDrainMillis','reloadPauseMillis'] as $key) {
            $safeKernel[$key] = isset($kernel[$key]) ? max(0, (int)$kernel[$key]) : null;
        }
        $safeKernel['exitCapacity'] = null;

        // Historical sampling service is retired; unknown data stays null.
        $sample = null;
        $sampledAt = null;
        $start = (new DateTimeImmutable('@' . $now))->setTimezone(new DateTimeZone('UTC'))->setTime((int)gmdate('H', $now), 0)->modify('-' . ($hours - 1) . ' hours');
        $history = [];
        for ($i = 0; $i < $hours; $i++) {
            $stamp = $start->modify('+' . $i . ' hours')->format('c');
            $history[$stamp] = ['hour' => $stamp, 'samples' => 0, 'queues' => null, 'coverage' => null];
        }
        return ['generatedAt' => gmdate('c', $now), 'sampledAt' => $sampledAt,
            'sampleStale' => $sampledAt === null || $now - strtotime($sampledAt) > 180,
            'services' => $services, 'liveness' => $safeLiveness, 'kernel' => $safeKernel,
            'current' => $sample, 'hours' => $hours, 'hourly' => array_values($history)];
    }
}
