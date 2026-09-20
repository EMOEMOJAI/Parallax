"""Keep public HTTPS usable without deployment credentials."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

COMMON = Path(__file__).resolve().parents[1] / 'common.sh'


class RepositoryTests(unittest.TestCase):
    def repo(self, extra_env=None, saved_key=False):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        app = root / 'app'; app.mkdir()
        if saved_key:
            (app / '.deploy-key-path').write_text(str(root / 'retired-key'))
        (root / 'key').write_text('test-only')
        bin_dir = root / 'bin'; bin_dir.mkdir()
        log = root / 'commands'
        fake = bin_dir / 'git'
        fake.write_text('''#!/usr/bin/env python3
import os, sys, pathlib
with open(os.environ['COMMAND_LOG'], 'a') as f:
    f.write(' '.join(sys.argv[1:]) + '\\n')
    f.write('SSH=' + os.environ.get('GIT_SSH_COMMAND', '') + '\\n')
if sys.argv[1] == 'clone':
    pathlib.Path(sys.argv[-1]).mkdir()
''')
        fake.chmod(0o755)
        chown = bin_dir / 'chown'; chown.write_text('#!/bin/sh\nexit 0\n'); chown.chmod(0o755)
        env = dict(os.environ, APP_DIR=str(app), BRANCH='main', COMMAND_LOG=str(log),
                   PATH=str(bin_dir) + os.pathsep + os.environ['PATH'])
        for key in ('REPO_URL', 'DEPLOY_KEY', 'GIT_SSH_COMMAND'):
            env.pop(key, None)
        if extra_env:
            env.update({k: v.replace('{key}', str(root / 'key')) for k, v in extra_env.items()})
        result = subprocess.run(['bash', '-euc', 'source "$1"; prepare_repo', 'bash', str(COMMON)],
                                env=env, capture_output=True, text=True)
        return app, log.read_text() if log.exists() else '', result

    def test_public_clone_does_not_require_a_key(self):
        _, log, result = self.repo()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('https://github.com/EMOEMOJAI/Parallax.git', log)
        self.assertIn('SSH=\n', log)

    def test_https_ignores_retired_saved_key(self):
        _, log, result = self.repo(saved_key=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('https://github.com/EMOEMOJAI/Parallax.git', log)

    def test_optional_private_key_selects_ssh(self):
        app, log, result = self.repo({'DEPLOY_KEY': '{key}'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('git@github.com:EMOEMOJAI/Parallax.git', log)
        self.assertIn('IdentitiesOnly=yes', log)
        self.assertTrue((app / '.deploy-key-path').exists())

    def test_fork_url_is_honored(self):
        _, log, result = self.repo({'REPO_URL': 'https://github.com/example/fork.git'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('https://github.com/example/fork.git', log)

    def test_explicit_missing_key_fails_before_clone(self):
        _, log, result = self.repo({'DEPLOY_KEY': '/missing/deploy-key'})
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(log, '')


if __name__ == '__main__':
    unittest.main()
