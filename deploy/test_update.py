"""Run the updater against disposable files and mocked external commands."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class UpdateTests(unittest.TestCase):
    def run_update(self, current, latest):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'deploy').mkdir()
            (root / 'bin').mkdir()
            shutil.copy2(Path(__file__).with_name('update.sh'), root / 'deploy/update.sh')
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
tag = 'v1.6.5' if 'beck-8/subs-check' in sys.argv[-1] else os.environ['TEST_LATEST_TAG']
print(json.dumps({'tag_name': tag}))
''')
            (root / 'bin/docker').write_text('#!/bin/sh\necho healthy\n')
            for name in ['curl', 'docker']:
                (root / 'bin' / name).chmod(0o755)
            env = dict(os.environ, PATH=str(root / 'bin') + ':' + os.environ['PATH'],
                       TEST_LATEST_TAG=latest)
            result = subprocess.run(['bash', 'deploy/update.sh'], cwd=root, env=env,
                                    text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            actions = root / 'actions.log'
            return ((actions.read_text() if actions.exists() else ''),
                    (root / 'release.env').read_text(), services)

    def test_new_release_updates_all_deployed_roles(self):
        actions, release, services = self.run_update('v1.5.28-probefix-r2', 'v1.5.29')
        self.assertIn('IMAGE_TAG=v1.5.29', release)
        for action in ['pull', 'up -d --no-deps --wait --wait-timeout 120']:
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


if __name__ == '__main__':
    unittest.main()
