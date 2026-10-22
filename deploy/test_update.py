"""Run the updater against disposable files and mocked external commands."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class UpdateTests(unittest.TestCase):
    def run_update(self, current, latest, status=200):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'deploy').mkdir()
            (root / 'bin').mkdir()
            for name in ['update.sh', 'github_release.py']:
                shutil.copy2(Path(__file__).with_name(name), root / 'deploy' / name)
            (root / '.env').write_text('NODE_EXPLORER_AUTO_UPDATE=true\n')
            (root / 'release.env').write_text(f'IMAGE_TAG={current}\nSUBS_CHECK_TAG=1.6.5\n')
            services = ['node-explorer', 'source-ingest', 'mihomo-manager',
                        'exit-provision', 'exit-attributes', 'recovery', 'node-cleanup']
            config = {'services': {name: {'image': 'ghcr.io/cytusc/node_list:' + current}
                                   for name in services}}
            config['services']['task-redis'] = {'image': 'redis:7-alpine'}
            (root / 'config.json').write_text(json.dumps(config))
            (root / 'deploy/compose.sh').write_text('''#!/usr/bin/env bash
set -eu
case "$1" in
  config) cat config.json ;;
  ps) echo test-container ;;
  *) printf '%s %s\\n' "${IMAGE_TAG:-}" "$*" >> actions.log ;;
esac
''')
            (root / 'bin/curl').write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
tag = 'v1.6.5' if 'beck-8/subs-check' in sys.argv[-1] else os.environ['TEST_LATEST_TAG']
Path(sys.argv[sys.argv.index('--output') + 1]).write_text(json.dumps({'tag_name': tag}))
status = os.environ['TEST_STATUS']
Path(sys.argv[sys.argv.index('--dump-header') + 1]).write_text('HTTP/2 '+status+'\\nETag: test-etag\\nX-RateLimit-Remaining: 0\\nRetry-After: 600\\n')
print(status)
''')
            (root / 'bin/docker').write_text('#!/bin/sh\necho healthy\n')
            for name in ['curl', 'docker']:
                (root / 'bin' / name).chmod(0o755)
            env = dict(os.environ, PATH=str(root / 'bin') + ':' + os.environ['PATH'],
                       TEST_LATEST_TAG=latest, TEST_STATUS=str(status))
            result = subprocess.run(['bash', 'deploy/update.sh'], cwd=root, env=env,
                                    text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            actions = root / 'actions.log'
            return ((actions.read_text() if actions.exists() else ''),
                    (root / 'release.env').read_text(), services)

    def test_rate_limited_check_keeps_current_deployment(self):
        actions, release, _ = self.run_update('v1.5.35', 'v1.5.36', status=403)
        self.assertEqual(actions, '')
        self.assertIn('IMAGE_TAG=v1.5.35', release)

    def test_new_release_updates_all_deployed_roles(self):
        actions, release, services = self.run_update('v1.5.28-probefix-r2', 'v1.5.29')
        self.assertIn('IMAGE_TAG=v1.5.29', release)
        for action in ['pull', 'up -d --no-deps --wait --wait-timeout 180']:
            self.assertIn('v1.5.29 ' + action + ' ' + ' '.join(services), actions)
        self.assertNotIn('task-redis', actions)
        self.assertNotIn('subs-check', actions)

    def test_older_release_cannot_downgrade_hotfix(self):
        actions, release, _ = self.run_update('v1.5.28-probefix-r2', 'v1.5.26')
        self.assertEqual(actions, '')
        self.assertIn('IMAGE_TAG=v1.5.28-probefix-r2', release)

    def test_version_comparison_is_numeric(self):
        actions, release, _ = self.run_update('v1.5.9', 'v1.5.29')
        self.assertIn('v1.5.29 pull', actions)
        self.assertIn('IMAGE_TAG=v1.5.29', release)

    def test_current_release_is_unchanged(self):
        actions, _, _ = self.run_update('v1.5.29', 'v1.5.29')
        self.assertEqual(actions, '')


class ReleaseLookupTests(unittest.TestCase):
    def test_rate_limit_cooldown_skips_requests_across_repositories(self):
        from github_release import latest, Deferred
        from unittest.mock import patch
        from types import SimpleNamespace
        import time
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            def response(cmd, **kwargs):
                Path(cmd[cmd.index('--dump-header')+1]).write_text(
                    'HTTP/2 403\nX-RateLimit-Remaining: 0\nX-RateLimit-Reset: '+str(int(time.time()+600))+'\n')
                Path(cmd[cmd.index('--output')+1]).write_text('rate limited')
                return SimpleNamespace(returncode=0, stdout='403')
            with patch('github_release.subprocess.run', side_effect=response) as request:
                for repo in ['cytusc/node_list', 'beck-8/subs-check', 'cytusc/node_list']:
                    with self.assertRaises(Deferred):
                        latest(repo, state)
                self.assertEqual(request.call_count, 1)

    def test_etag_reuses_valid_release_after_304(self):
        from github_release import latest
        from unittest.mock import patch
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as directory:
            state=Path(directory)
            calls=[]
            def response(cmd, **kwargs):
                calls.append(cmd)
                status='200' if len(calls)==1 else '304'
                Path(cmd[cmd.index('--dump-header')+1]).write_text('HTTP/2 '+status+'\nETag: "release"\n')
                Path(cmd[cmd.index('--output')+1]).write_text(json.dumps({'tag_name':'v1.5.35'}))
                return SimpleNamespace(returncode=0,stdout=status)
            with patch('github_release.subprocess.run',side_effect=response):
                self.assertEqual(latest('cytusc/node_list',state),'v1.5.35')
                self.assertEqual(latest('cytusc/node_list',state),'v1.5.35')
            self.assertIn('If-None-Match: "release"',calls[1])

    def test_bad_release_cannot_be_used_as_update_target(self):
        from github_release import latest
        from unittest.mock import patch
        from types import SimpleNamespace
        for body in ['not json', '{"tag_name":"v1.5.36-rc1"}', '{"tag_name":"v1.5.36","prerelease":true}']:
            with self.subTest(body=body), tempfile.TemporaryDirectory() as directory:
                def response(cmd, **kwargs):
                    Path(cmd[cmd.index('--dump-header')+1]).write_text('HTTP/2 200\n')
                    Path(cmd[cmd.index('--output')+1]).write_text(body)
                    return SimpleNamespace(returncode=0,stdout='200')
                with patch('github_release.subprocess.run',side_effect=response):
                    with self.assertRaises(ValueError):
                        latest('cytusc/node_list',Path(directory))


if __name__ == '__main__':
    unittest.main()
