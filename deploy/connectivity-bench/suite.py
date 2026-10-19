#!/usr/bin/env python3
"""Run staged tiers, repeat candidates, and investigate every disagreement.

Requires setup + fixed sample + baseline16.json. Writes sanitized verdicts and
timing only to results. The private sample never belongs in a Git repository.
"""
import hashlib
import json
import os
import statistics
import subprocess
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
TIERS = [16, 64, 128, 256, 512]
PREFIX = os.getenv('BENCH_PREFIX', '')


def load(name):
    return json.loads((ROOT / name).read_text())


def save(name, value):
    path = ROOT / name
    with path.open('x') as f:
        json.dump(value, f, indent=2)
    path.chmod(0o600)


def run(name, tier, batch=128, sample='sample.json'):
    if not (ROOT / name).exists():
        print(f'Start {name}: concurrency={tier}, batch={batch}', flush=True)
        subprocess.run(['python3', str(ROOT / 'harness.py'), 'run', '--sample', sample,
                        '--output', name, '--concurrency', str(tier), '--batch', str(batch)], check=True)
    report = load(name)
    if report.get('aborted') or report['normal']['controller_errors']:
        raise RuntimeError('tier stopped; inspect sanitized result before continuing')
    return report


def acceptable(r):
    return (not r.get('aborted') and r['normal']['count'] == 512
            and r['residential']['count'] == 16
            and r['residential']['queue_p95_ms'] <= 5000
            and r['normal']['assessment_p99_ms'] <= 120000
            and r['host']['average_cpu_percent'] < 70
            and r['host']['min_available_mib'] >= 4096
            and not r['normal']['controller_errors'])


def speed(r):
    return r['normal']['count'] / r['normal']['elapsed_seconds']


def verdicts(r):
    # A target may appear in both lanes. Review residential regressions too,
    # without allowing the other lane's result to overwrite its evidence.
    return {x['lane'] + ':' + x['id']: (x['reachable'], x['confirmed_dead'], x.get('error', ''))
            for x in r['results']}


def main():
    # The first baseline may be running from an interactive SSH session.
    while not (ROOT / 'baseline16.json').exists():
        time.sleep(10)
    baseline = load('baseline16.json')
    reports = {tier: run(f'{PREFIX}batch{tier}.json', tier) for tier in TIERS}
    # A faster tier with weakened control evidence is not a valid winner.
    good_controls = lambda r: not any(x.get('error') == 'insufficient-controls'
        or any(a['outcome'] == 'http-429' for a in x.get('trace', [])) for x in r['results'])
    good = [tier for tier in TIERS if acceptable(reports[tier]) and good_controls(reports[tier])]
    if not good:
        raise RuntimeError('no tier meets resource/latency gates')
    repeated = {}
    while good:
        best = max(good, key=lambda t: speed(reports[t]))
        lower = max([t for t in good if t < best], default=best)
        for tier in sorted({best, lower}):
            if tier not in repeated:
                repeated[tier] = [reports[tier]] + [run(f'{PREFIX}batch{tier}-repeat{i}.json', tier) for i in (1, 2)]
        failed = [tier for tier in (best,lower) if not all(acceptable(r) and good_controls(r) for r in repeated[tier])]
        if not failed:
            break
        good = [tier for tier in good if tier not in failed]
    else:
        raise RuntimeError('repeated tiers failed resource/quality acceptance')
    stable = {tier: rs for tier,rs in repeated.items() if tier in good}
    means = {tier: statistics.mean(speed(r) for r in rs) for tier, rs in stable.items()}
    selected = best if best in means and (lower not in means or means[best] >= means[lower] * 1.10) else lower
    all_reports = {'baseline16.json': baseline}
    for path in ROOT.glob(PREFIX+'batch*.json'):
        # batch.json is endpoint configuration, not a report.
        value = load(path.name)
        if 'results' in value:
            all_reports[path.name] = value
    baseline_v = verdicts(baseline)
    diffs = {}
    for name, report in all_reports.items():
        current = verdicts(report)
        for identity, v in current.items():
            if v != baseline_v[identity]:
                diffs.setdefault(identity, {})[name] = v
    save(PREFIX+'disagreements.json', diffs)
    sample = load('sample.json')
    if diffs:
        def token(c):
            return hashlib.sha256(c['identity'].encode()).hexdigest()[:24]
        subset = dict(sample)
        subset['targets'] = [c for c in sample['targets'] if 'normal:' + token(c) in diffs]
        save(PREFIX+'disagreement-sample.json', subset)
        for i in (1, 2):
            run(f'{PREFIX}verify-baseline-{i}.json', 16, 1, PREFIX+'disagreement-sample.json')
            run(f'{PREFIX}verify-batch-{i}.json', selected, 128, PREFIX+'disagreement-sample.json')
        b = [verdicts(load(f'{PREFIX}verify-baseline-{i}.json')) for i in (1, 2)]
        p = [verdicts(load(f'{PREFIX}verify-batch-{i}.json')) for i in (1, 2)]
        persistent = [identity for identity in diffs if b[0][identity] == b[1][identity]
                      and p[0][identity] == p[1][identity] and b[0][identity] != p[0][identity]]
    else:
        persistent = []
    save(PREFIX+'selection.json', {'best': best, 'lower': lower, 'selected': selected,
                           'mean_nodes_per_second': means, 'disagreements': len(diffs),
                           'persistent_disagreements': persistent,
                           'quality_review_required': bool(diffs)})
    # Do not certify accuracy or start a full sweep solely from aggregate counts.
    # Human/agent review of the per-attempt diagnostic traces is required first.
    print(f'Selected={selected}; disagreement review={len(diffs)}; persistent={len(persistent)}', flush=True)
    if not diffs:
        subprocess.run(['python3', str(ROOT / 'harness.py'), 'sample', '--due',
                        '--output', PREFIX+'due-sample.json'], check=True)
        run(PREFIX+'due-sweep.json', selected, sample=PREFIX+'due-sample.json')


if __name__ == '__main__':
    main()
