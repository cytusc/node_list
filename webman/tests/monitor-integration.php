<?php
use support\Db;
// Included by integration.php against its dedicated fixture database.
$monitor = new \app\service\ServiceMonitor();
$empty = $monitor->query();
same(5, count($empty['services']), 'only current services are listed');
same(null, $empty['liveness']['inFlight'], 'missing heartbeat is unknown');
ok(!array_key_exists('hourly', $empty) && !array_key_exists('kernel', $empty), 'no historical or retired kernel placeholders');
Db::statement(<<<'SQL'
INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('node-liveness',now(),'{"running":true,"inFlight":2,"xrayQueued":10,"mihomoQueued":3}') ON CONFLICT(service) DO UPDATE SET updated_at=excluded.updated_at,payload=excluded.payload
SQL);
same(2, $monitor->query()['liveness']['inFlight'], 'current heartbeat read immediately');
Db::statement(<<<'SQL'
UPDATE node_explorer_service_status SET payload=payload || '{"inFlight":7}'::jsonb WHERE service='node-liveness'
SQL);
same(7, $monitor->query()['liveness']['inFlight'], 'subsequent request observes update without TTL');
Db::statement(<<<'SQL'
DELETE FROM node_explorer_service_status WHERE service='node-liveness'
SQL);
