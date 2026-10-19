#!/usr/bin/env python3
"""Disposable benchmark project. No production compose files are modified.

Copy this file and a static connectivity-bench binary to an empty private
directory on the Docker host. Run `python3 harness.py setup`, then sample/run.
All files containing connection material are owner-only. No host ports open.
"""
import argparse
import json
import os
import secrets
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent
PROJECT = 'node-list-cbench'
NETWORK = PROJECT + '_default'
IMAGE = 'ghcr.io/cytusc/node_list:recovery-tuning-20261018-r1'


def docker(*args, capture=False):
    return subprocess.run(['docker', *args], check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def private(name, value):
    path = ROOT / name
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as f:
        f.write(value)


def compose(*args):
    docker('compose', '-p', PROJECT, '-f', str(ROOT / 'compose.json'), *args)


def setup():
    os.umask(0o077)
    secret = secrets.token_hex(24)
    private('kernel.yaml', 'mixed-port: 7890\nallow-lan: true\n'
            'bind-address: "*"\nexternal-controller: 0.0.0.0:9090\n'
            f'secret: {secret}\nmode: rule\nlog-level: silent\n'
            'ipv6: false\nproxies: []\nrules: ["MATCH,REJECT"]\n')
    normal = [f'cb-normal-{i:02}' for i in range(1, 21)]
    residential = [f'cb-residential-{i:02}' for i in range(1, 3)]
    services = {name: {
        'image': 'metacubex/mihomo:v1.19.27',
        'command': ['-f', '/seed/kernel.yaml', '-d', '/work'],
        'volumes': [str(ROOT / 'kernel.yaml') + ':/seed/kernel.yaml:ro'],
        'tmpfs': ['/work:size=64m'],
        'mem_limit': '512m', 'cpus': 2, 'pids_limit': 256,
        'restart': 'no', 'logging': {'driver': 'none'},
    } for name in normal + residential}
    private('compose.json', json.dumps({'services': services}, indent=2))
    def instance(name):
        return {'Controller': f'http://{name}:9090',
                'ProxyURL': f'http://{name}:7890', 'Secret': secret}
    for name, nodes in [('baseline', normal), ('batch', normal[:4])]:
        private(name + '.json', json.dumps({
            'normal': list(map(instance, nodes)),
            'residential': list(map(instance, residential))}))
    compose('up', '-d')


def sample(output, due):
    # Read only environment keys needed by libpq. Never echo values or copy the
    # application's full environment. This file is absent from probe containers.
    info = json.loads(docker('inspect', 'node_list-node-explorer-1', capture=True))[0]
    env = [s for s in info['Config']['Env'] if s.split('=', 1)[0] in
           ('PGHOST', 'PGPORT', 'PGDATABASE', 'PGUSER', 'PGPASSWORD', 'PGSSLMODE')]
    networks = list(info['NetworkSettings']['Networks'])
    # The app's network already resolves its configured PGHOST.
    envpath = ROOT / 'db.env'
    private('db.env', '\n'.join(env) + '\n')
    try:
        args = ['run', '--rm', '--no-healthcheck', '--user', '0:0', '--name', PROJECT + '-sample', '--network', networks[0],
                '--env-file', str(envpath), '--read-only', '--cap-drop', 'ALL',
                '--security-opt', 'no-new-privileges',
                '-v', str(ROOT) + ':/benchdata', '--entrypoint', '/benchdata/connectivity-bench',
                IMAGE, '--mode', 'sample', '--output', '/benchdata/' + output]
        if due:
            args.append('--due')
        docker(*args)
    finally:
        envpath.unlink(missing_ok=True)


def run(args):
    # No DB network and no DB credentials in the probe container.
    docker('run', '--rm', '--no-healthcheck', '--user', '0:0', '--name', PROJECT + '-run', '--network', NETWORK,
           '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
           '--memory', '1g', '--cpus', '2', '--pids-limit', '512',
           '-v', str(ROOT) + ':/benchdata', '-v', '/proc:/hostproc:ro',
           '--entrypoint', '/benchdata/connectivity-bench', IMAGE,
           '--sample', '/benchdata/' + args.sample,
           '--instances', '/benchdata/' + ('baseline.json' if args.batch == 1 else 'batch.json'),
           '--concurrency', str(args.concurrency), '--batch', str(args.batch),
           '--output', '/benchdata/' + args.output)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    sub.add_parser('setup')
    s = sub.add_parser('sample')
    s.add_argument('--output', required=True)
    s.add_argument('--due', action='store_true')
    r = sub.add_parser('run')
    r.add_argument('--sample', default='sample.json')
    r.add_argument('--output', required=True)
    r.add_argument('--concurrency', type=int, required=True)
    r.add_argument('--batch', type=int, default=128)
    sub.add_parser('down')
    opts = parser.parse_args()
    if opts.command == 'setup':
        setup()
    elif opts.command == 'sample':
        sample(opts.output, opts.due)
    elif opts.command == 'run':
        run(opts)
    else:
        compose('down', '--remove-orphans')
