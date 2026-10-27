<?php
declare(strict_types=1);

namespace app\service;

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

    public function query(): array
    {
        return Db::transaction(function (): array {
            // Read current heartbeats in one database transaction.
            Db::statement('SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY');
            Db::statement("SET LOCAL statement_timeout = '5s'");
            return $this->readCurrent();
        });
    }

    private function readCurrent(): array
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
        $safeLiveness['results'] = array_fill_keys(['reachable', 'unreachable', 'config_error', 'infrastructure'], 0);
        foreach (($liveness['counts'] ?? []) as $key => $count) {
            $status = explode(':', (string)$key, 2)[1] ?? '';
            if (array_key_exists($status, $safeLiveness['results'])) {
                $safeLiveness['results'][$status] += max(0, (int)$count);
            }
        }
        // Count each node once, in the same gate order as the publishing view.
        $safeLiveness['visibility'] = array_fill_keys(['published', 'unavailable', 'expired', 'unverified', 'unverifiedExit'], 0);
        foreach (Db::select(<<<'SQL'
SELECT reason,count(*) AS total FROM (
 SELECT CASE
 WHEN NOT n.available THEN 'unavailable'
 WHEN c.checked_at IS NULL OR c.checked_at<=now()-interval '6 hours' THEN 'expired'
 WHEN c.payload->>'reachable' IS DISTINCT FROM 'true'
   OR c.payload->>'incarnation' IS DISTINCT FROM n.incarnation
   OR c.payload->>'configDigest' IS DISTINCT FROM md5(n.node::text) THEN 'unverified'
 WHEN e.payload->>'verified' IS DISTINCT FROM 'true' THEN 'unverifiedExit'
 ELSE 'published' END AS reason
 FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_connectivity c USING(identity)
 LEFT JOIN node_explorer_exits e USING(identity)
) states GROUP BY reason
SQL) as $row) {
            $safeLiveness['visibility'][$row->reason] = (int)$row->total;
        }
        return ['generatedAt' => gmdate('c', $now), 'services' => $services, 'liveness' => $safeLiveness];
    }
}
