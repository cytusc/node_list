<?php
// Included by the offline suite; status tests must not require PostgreSQL.
use app\service\ServiceMonitor;
$now = time();
$healthyRow = ['updated_at' => gmdate('c', $now), 'payload' => '{"running":true,"error":""}'];
same('healthy', ServiceMonitor::service('source-ingest', $healthyRow, $now)['status'], 'fresh heartbeat is healthy');
same('stale', ServiceMonitor::service('source-ingest', $healthyRow, $now + 91)['status'], 'heartbeat expires at 90 seconds');
same('unknown', ServiceMonitor::service('source-ingest', null, $now)['status'], 'missing heartbeat is unknown, not healthy');
$warningRow = ['updated_at' => gmdate('c', $now), 'payload' => '{"running":true,"sourceWarning":"session=private-secret"}'];
$publicStatus = ServiceMonitor::service('source-ingest', $warningRow, $now);
same('degraded', $publicStatus['status'], 'source warning degrades status');
ok(!str_contains(json_encode($publicStatus), 'private-secret'), 'worker payload and raw warnings never escape to browser');
same('degraded', ServiceMonitor::service('mihomo-manager', $healthyRow, $now)['status'], 'manager requires healthy kernel');
same(24, ServiceMonitor::hours(null), 'default monitoring window');
same(168, ServiceMonitor::hours('168'), 'seven day monitoring window');
foreach (['0', '999', ['24'], '24 OR 1=1'] as $value) {
    try { ServiceMonitor::hours($value); ok(false, 'invalid monitor range accepted'); }
    catch (\app\service\ApiError $e) { same(400, $e->status, 'invalid monitoring range rejected'); }
}
