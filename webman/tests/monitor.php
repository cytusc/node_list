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
ok(!isset(ServiceMonitor::SERVICES['mihomo-manager']), 'retired manager is absent from service catalog');
same('healthy', ServiceMonitor::service('node-liveness', $healthyRow, $now)['status'], 'independent liveness heartbeat is healthy');
