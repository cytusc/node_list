"""Check removal is limited to the current project's retired cleanup service."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class ComposeRetirementTests(unittest.TestCase):
    def invoke(self, *arguments):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'deploy').mkdir()
            (root / 'bin').mkdir()
            shutil.copy2(Path(__file__).with_name('compose.sh'), root / 'deploy/compose.sh')
            (root / '.env').write_text('')
            (root / 'release.env').write_text('')
            docker = root / 'bin/docker'
            docker.write_text('''#!/usr/bin/env bash
set -eu
printf '%s\\n' "$*" >> docker.log
if [[ "$1" == compose && "$*" == *'config --format json'* ]]; then
  echo '{"name":"test-project","services":{}}'
elif [[ "$1" == ps ]]; then
  [[ "$*" == *'label=com.docker.compose.project=test-project'* ]]
  [[ "$*" == *'label=com.docker.compose.service=node-cleanup'* || "$*" == *'label=com.docker.compose.service=recovery'* || "$*" == *'label=com.docker.compose.service=dead-verify'* ]]
  printf 'old-cleanup-a\\nold-cleanup-b\\n'
fi
''')
            docker.chmod(0o755)
            env = dict(os.environ, PATH=str(root / 'bin') + ':' + os.environ['PATH'])
            env.pop('COMPOSE_FILE', None)
            result = subprocess.run(['bash', 'deploy/compose.sh', *arguments], cwd=root,
                                    env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            return (root / 'docker.log').read_text()

    def test_up_removes_only_retired_service_before_start(self):
        actions = self.invoke('up', '-d', 'node-liveness')
        self.assertIn('rm -f old-cleanup-a\nrm -f old-cleanup-b\n', actions)
        self.assertLess(actions.index('rm -f'), actions.index('up -d node-liveness'))
        self.assertNotIn('--remove-orphans', actions)

    def test_read_only_config_never_removes_containers(self):
        actions = self.invoke('config', '--quiet')
        self.assertNotIn('rm -f', actions)
        self.assertNotIn('ps -aq', actions)


if __name__ == '__main__':
    unittest.main()
