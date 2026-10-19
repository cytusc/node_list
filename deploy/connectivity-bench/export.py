#!/usr/bin/env python3
"""Emit one compact, credential-free benchmark artifact to stdout.

Run on the benchmark host after review. Excludes raw samples, kernel configs,
and the full per-node report copies. Keep the latter in private audit storage.
"""
import collections
import hashlib
import json
import re
from datetime import datetime, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parent
sample = json.loads((ROOT / 'sample.json').read_text())
out = {'sample': {'captured_at': sample['captured_at'],
                  'targets': len(sample['targets']),
                  'retired': sum(c['retired'] for c in sample['targets']),
                  'protocols': dict(collections.Counter(c['node'].get('type', '') for c in sample['targets'])),
                  'residential': len(sample['residential'])}, 'runs': {}}
for path in sorted(ROOT.glob('*.json')):
    if path.name.endswith('sample.json') or path.name in ('compose.json', 'baseline.json', 'batch.json'):
        continue
    report = json.loads(path.read_text())
    if isinstance(report, dict) and 'results' in report:
        out['runs'][path.name] = {k: v for k, v in report.items() if k != 'results'}
        out['runs'][path.name]['sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
        out['runs'][path.name]['outcomes'] = dict(collections.Counter(
            'reachable' if r['reachable'] else r.get('error', 'unknown') for r in report['results'] if r['lane'] == 'normal'))
        out['runs'][path.name]['attempt_outcomes'] = dict(collections.Counter(
            a['phase'] + ':' + a['outcome'] for r in report['results'] if r['lane'] == 'normal'
            for a in r.get('trace', [])))
for name in ('selection.json', 'bounded-selection.json', 'resilient-selection.json', 'adjudication.json'):
    if (ROOT / name).exists():
        out[name[:-5]] = json.loads((ROOT / name).read_text())
telemetry = []
if (ROOT / 'telemetry.jsonl').exists():
    telemetry = [json.loads(s) for s in (ROOT / 'telemetry.jsonl').read_text().splitlines()]
out['production_monitor'] = {'samples': len(telemetry),
                            'errors': sum(bool(r.get('queue_monitor_error')) for r in telemetry)}
if telemetry:
    out['production_monitor']['first_at'] = telemetry[0]['at']
    out['production_monitor']['last_at'] = telemetry[-1]['at']
    ages = [float(q['due_age_p95_s']) for r in telemetry for q in r.get('production_queues', [])
            if q['kind'] == 'connectivity' and q['priority'] > 0 and q['state'] == 'queued']
    out['production_monitor']['residential_due_p95_max_s'] = max(ages, default=0)
    def mib(value):
        match = re.match(r'([\d.]+)([KMG]iB|B)', value.split(' / ')[0])
        return float(match[1]) * {'B': 1 / 1048576, 'KiB': 1 / 1024, 'MiB': 1, 'GiB': 1024}[match[2]] if match else 0
    for report in out['runs'].values():
        start = datetime.fromisoformat(report['started_at'].replace('Z', '+00:00'))
        end = start + timedelta(seconds=max(report['normal']['elapsed_seconds'], report['residential']['elapsed_seconds']))
        rows = [r for r in telemetry if start <= datetime.fromisoformat(r['at']) <= end]
        report['external_samples'] = len(rows)
        report['benchmark_peak_total_mib'] = max((sum(mib(c['MemUsage']) for c in r['containers']
                                                    if c['Name'].startswith('node-list-cbench-')) for r in rows), default=0)
        report['benchmark_peak_docker_cpu_percent'] = max((sum(float(c['CPUPerc'].rstrip('%')) for c in r['containers']
                                                             if c['Name'].startswith('node-list-cbench-')) for r in rows), default=0)
        report['host_peak_established_tcp'] = max((int(m[1]) for r in rows
                                                 if (m := re.search(r'estab (\d+)', r['host_sockets']))), default=0)
print(json.dumps(out, indent=2))
