<?php
use support\Db;
// Included by integration.php against its dedicated fixture database.
$monitor = new \app\service\ServiceMonitor();

$emptyMonitor = $monitor->query(24);
same(null, $emptyMonitor['current'], 'no sample is unknown, not zero');
same(true, $emptyMonitor['sampleStale'], 'empty telemetry is marked stale');
same(24, count($emptyMonitor['hourly']), 'hour range includes empty buckets');
same(null, $emptyMonitor['hourly'][0]['queues'], 'missing hours are gaps');
same(168, count($monitor->query(168)['hourly']), 'seven day range');
same(null, Db::selectOne("SELECT to_regclass('node_explorer_monitor_samples') AS name")->name, 'retired telemetry table is absent');
same(5, count($emptyMonitor['services']), 'only current services are listed');
