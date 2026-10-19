#!/usr/bin/env python3
"""Host/container and production lane telemetry; never print node data."""
import json
import subprocess
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent
SQL = """SELECT kind,priority,state,count(*) AS count,
 coalesce(round(percentile_cont(0.95) WITHIN GROUP(ORDER BY extract(epoch FROM now()-due_at)) FILTER(WHERE state='queued' AND due_at<=now())::numeric,2),0) AS due_age_p95_s
 FROM node_explorer_work WHERE kind IN ('connectivity','recovery','normal-recovery')
 AND state<>'done' GROUP BY kind,priority,state ORDER BY kind,priority,state"""
PHP = """<?php
$pdo=new PDO(sprintf('pgsql:host=%s;port=%s;dbname=%s',getenv('PGHOST'),getenv('PGPORT')?:'5432',getenv('PGDATABASE')),getenv('PGUSER'),getenv('PGPASSWORD'),[PDO::ATTR_ERRMODE=>PDO::ERRMODE_EXCEPTION]);
$pdo->exec("SET default_transaction_read_only=on");
$pdo->exec("SET statement_timeout='5s'");
echo json_encode($pdo->query(__SQL__)->fetchAll(PDO::FETCH_ASSOC));
""".replace('__SQL__', json.dumps(SQL))


def run(args, **kwargs):
    return subprocess.run(args, capture_output=True, text=True, timeout=15, **kwargs)


with (ROOT / 'telemetry.jsonl').open('a') as log:
    while not (ROOT / 'stop-monitor').exists():
        record = {'at': datetime.now(timezone.utc).isoformat()}
        current = run(['docker', 'ps', '--format', '{{.Names}}'])
        names = [n for n in current.stdout.splitlines() if n.startswith('node-list-cbench-')
                 or n in ('node_list-connectivity-1', 'node_list-normal-recovery-1',
                          'node_list-residential-recovery-1', 'node_list-node-explorer-1')]
        stats = run(['docker', 'stats', '--no-stream', '--format', '{{json .}}', *names])
        record['containers'] = [json.loads(s) for s in stats.stdout.splitlines() if s.startswith('{')]
        query = run(['docker', 'exec', '-i', 'node_list-node-explorer-1', 'php'], input=PHP)
        try:
            record['production_queues'] = json.loads(query.stdout)
        except ValueError:
            record['queue_monitor_error'] = True
        record['host_sockets'] = run(['ss', '-s']).stdout
        record['host_load'] = Path('/proc/loadavg').read_text().strip()
        log.write(json.dumps(record) + '\n')
        log.flush()
        time.sleep(15)
