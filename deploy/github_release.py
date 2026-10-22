#!/usr/bin/env python3
"""Fetch a release tag with conditional requests and a persisted API cooldown."""
import email.utils
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time


class Deferred(Exception):
    pass


def read_json(path):
    try:
        value = json.loads(path.read_text())
        return value if isinstance(value, dict) else {}
    except (OSError, ValueError):
        return {}


def save_json(path, value):
    with tempfile.NamedTemporaryFile(mode='w', dir=path.parent, delete=False) as file:
        json.dump(value, file)
        name = file.name
    os.replace(name, path)


def latest(repository, state):
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
        raise ValueError('invalid GitHub repository')
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    cooldown = state / 'github-cooldown.json'
    now = time.time()
    until = read_json(cooldown).get('until', 0)
    if isinstance(until, (float, int)) and until > now:
        raise Deferred(f'GitHub API check deferred for {int(until-now)+1}s')
    cache_file = state / (repository.replace('/', '-') + '.json')
    cache = read_json(cache_file)
    with tempfile.TemporaryDirectory() as directory:
        headers, body = [Path(directory) / name for name in ('headers', 'body')]
        cmd = ['curl', '--silent', '--show-error', '--connect-timeout', '10',
               '--max-time', '30', '--dump-header', str(headers), '--output', str(body),
               '--write-out', '%{http_code}']
        etag = cache.get('etag')
        if isinstance(etag, str) and '\n' not in etag and '\r' not in etag:
            cmd += ['--header', 'If-None-Match: ' + etag]
        cmd.append(f'https://api.github.com/repos/{repository}/releases/latest')
        response = subprocess.run(cmd, capture_output=True, text=True)
        if response.returncode:
            save_json(cooldown, {'until': now + 60})
            raise Deferred('GitHub API temporarily unreachable; retry on the next timer tick')
        status = int(response.stdout.strip())
        fields = {}
        for line in headers.read_text().splitlines():
            if line.startswith('HTTP/'):
                fields = {}
            elif ':' in line:
                k, v = line.split(':', 1)
                fields[k.lower()] = v.strip()
        if status == 429 or (status == 403 and (fields.get('x-ratelimit-remaining') == '0' or 'retry-after' in fields)):
            until = now + 60
            reset = fields.get('x-ratelimit-reset', '')
            if reset.isdigit():
                until = max(until, int(reset) + 5)
            retry = fields.get('retry-after', '')
            if retry.isdigit():
                until = max(until, now + int(retry))
            elif retry:
                try:
                    until = max(until, email.utils.parsedate_to_datetime(retry).timestamp())
                except (TypeError, ValueError):
                    pass
            save_json(cooldown, {'until': until})
            raise Deferred(f'GitHub API rate limited; retry after {time.strftime("%Y-%m-%d %H:%M:%S %Z", time.localtime(until))}')
        if 500 <= status <= 599:
            save_json(cooldown, {'until': now + 60})
            raise Deferred(f'GitHub API HTTP {status}; retry on the next timer tick')
        if status == 304:
            release = cache
        elif status == 200:
            release = json.loads(body.read_text())
            if not isinstance(release, dict):
                raise ValueError('GitHub returned an invalid release document')
            if release.get('draft') or release.get('prerelease'):
                raise ValueError('GitHub returned a non-stable release')
        else:
            raise ValueError(f'GitHub release check returned HTTP {status}')
        tag = release.get('tag_name', '')
        if not re.fullmatch(r'v\d+\.\d+\.\d+', tag):
            raise ValueError('GitHub returned an invalid stable release tag')
        if status == 200:
            save_json(cache_file, {'tag_name': tag, 'etag': fields.get('etag'), 'checked_at': now})
        return tag


if __name__ == '__main__':
    try:
        print(latest(sys.argv[1], Path(sys.argv[2])))
    except Deferred as error:
        print(str(error), file=sys.stderr)
        sys.exit(75)
    except (OSError, ValueError) as error:
        print(f'Release check failed: {error}', file=sys.stderr)
        sys.exit(1)
