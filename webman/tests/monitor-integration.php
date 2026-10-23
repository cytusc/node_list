<?php
use support\Db;
// Included by integration.php against its dedicated fixture database.
$monitor = new \app\service\ServiceMonitor();
Db::statement('TRUNCATE node_explorer_monitor_samples');
$emptyMonitor = $monitor->query(24);
same(null, $emptyMonitor['current'], 'no sample is unknown, not zero');
same(true, $emptyMonitor['sampleStale'], 'empty telemetry is marked stale');
same(24, count($emptyMonitor['hourly']), 'hour range includes empty buckets');
same(null, $emptyMonitor['hourly'][0]['queues'], 'missing hours are gaps');
$base = "date_trunc('hour',now())-interval '2 hours'";
$payload = json_encode(['queues' => [['kind'=>'exit','due'=>10,'scheduled'=>20,'running'=>2]],'coverage'=>['total'=>100,'connectivity'=>50,'exit'=>30,'attributes'=>20], 'recovery'=>['active'=>0,'stalled'=>0]]);
Db::insert("INSERT INTO node_explorer_monitor_samples VALUES({$base}+interval '1 minute',?::jsonb)", [$payload]);
// A legitimate sample with no exit work must contribute zero to the hourly mean.
$second = json_encode(['queues'=>[], 'coverage'=>['total'=>100,'connectivity'=>60,'exit'=>40,'attributes'=>30], 'recovery'=>['active'=>0,'stalled'=>0]]);
Db::insert("INSERT INTO node_explorer_monitor_samples VALUES({$base}+interval '2 minutes',?::jsonb)", [$second]);
$history = $monitor->query(24);
$bucket = array_values(array_filter($history['hourly'], fn($row) => $row['samples'] > 0))[0];
same(2, $bucket['samples'], 'hourly sample count');
same(5.0, $bucket['queues']['exit']['due'], 'hour mean includes absence of a queue');
same(55.0, $bucket['coverage']['connectivity'], 'coverage average across real samples');
same(true, $history['sampleStale'], 'old sample is not current');
same(168, count($monitor->query(168)['hourly']), 'seven day range');
Db::statement('TRUNCATE node_explorer_monitor_samples');
