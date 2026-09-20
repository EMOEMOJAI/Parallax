"""Exercise update orchestration without a host, network, root, or systemd."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'update.sh'

# Each command is a symlink to this fake executable. Artifact moves/copies and
# Bash error handling are real, so build/restart failures exercise rollback.
FAKE = '''#!/usr/bin/env python3
import os, pathlib, sys, subprocess, signal
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
if name == 'git' and args[:1] == ['-c']:
    args = args[2:]
with open(os.environ['COMMAND_LOG'], 'a') as out:
    out.write(name + ' ' + ' '.join(args) + '\\n')
if name == 'mv':
    subprocess.run(['/bin/mv', *args], check=True)
    if os.environ.get('FAIL') == 'signal' and args[-1].endswith('/previous-dist'):
        os.kill(os.getppid(), signal.SIGTERM)
elif name == 'find' and os.environ.get('FAIL') == 'unsafe-owner':
    print(os.environ['APP_DIR'])
elif name == 'systemctl':
    if args[0] == 'show':
        role = args[-1].removeprefix('looking-glass-').removesuffix('.service')
        print('loaded' if role in os.environ['ROLES'].split(',') else 'not-found')
    elif args[0] == 'is-active' and os.environ.get('STOPPED') == '1':
        sys.exit(3)
    elif args[0] == 'restart' and os.environ.get('FAIL') == 'restart':
        marker = pathlib.Path(os.environ['APP_DIR']) / 'failed-once'
        if not marker.exists():
            marker.touch()
            sys.exit(1)
elif name == 'git':
    if args[0] == 'status' and os.environ.get('FAIL') == 'dirty':
        print(' M local.go')
    if args[:2] == ['remote', 'get-url']:
        print(os.environ.get('ORIGIN', 'git@github.com:EMOEMOJAI/Parallax.git'))
    if args[0] == 'fetch' and os.environ.get('FAIL') == 'fetch':
        sys.exit(1)
    if args[0] == 'merge-base' and os.environ.get('FAIL') == 'ahead':
        sys.exit(1)
    if args[0] == 'merge' and os.environ.get('FAIL') == 'diverged':
        sys.exit(1)
    if args[0] == 'describe':
        print('test-version')
elif name == 'node':
    print('20.19.0' if os.environ.get('FAIL') == 'old-node' else '24.21.0')
elif name == 'go':
    if args[0] == 'version':
        print('go version go1.24.4 linux/amd64' if os.environ.get('FAIL') == 'old-go' else 'go version go1.27.1 linux/amd64')
        sys.exit(0)
    if os.environ.get('FAIL') == 'build':
        sys.exit(1)
    pathlib.Path(args[args.index('-o') + 1]).write_text('new-binary')
elif name == 'npm' and args == ['run', 'build']:
    pathlib.Path('dist').mkdir(exist_ok=True)
    pathlib.Path('dist/index.html').write_text('new-frontend')
'''


class UpdateTests(unittest.TestCase):
    def run_update(self, roles, fail='', origin=None, saved_key=False, stopped=False):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name).resolve()
        app = root / 'app'
        for name in ('backend', 'agent', 'frontend', 'deploy'):
            (app / 'repo' / name).mkdir(parents=True)
        if saved_key:
            (app / '.deploy-key-path').write_text(str(root / 'missing-key'))
        (app / 'repo/.git').mkdir()
        (app / 'repo/deploy/update.sh').write_text('#!/bin/bash\n# next updater\n')
        (app / 'frontend/dist').mkdir(parents=True)
        (app / 'frontend/dist/index.html').write_text('old-frontend')
        for role in roles.split(','):
            if role:
                (app / ('looking-glass-' + role)).write_text('old-binary')
        fake_dir = root / 'bin'
        fake_dir.mkdir()
        fake = fake_dir / 'fake'
        fake.write_text(FAKE)
        fake.chmod(0o755)
        for cmd in ('git', 'go', 'npm', 'systemctl', 'chown', 'chmod', 'sleep', 'flock', 'find', 'mv', 'node'):
            (fake_dir / cmd).symlink_to(fake)
        # The production script prepends Linux Go paths. Remove that line only
        # in this test copy so a developer's Go installation cannot run here.
        script = root / 'update.sh'
        script.write_text(SCRIPT.read_text().replace(
            'export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"', ''))
        log = root / 'commands'
        env = dict(os.environ, PATH=str(fake_dir) + os.pathsep + os.environ['PATH'],
                   APP_DIR=str(app), COMMAND_LOG=str(log), ROLES=roles, FAIL=fail, STOPPED='1' if stopped else '0')
        if origin:
            env['ORIGIN'] = origin
        result = subprocess.run(['bash', str(script)], env=env,
                                capture_output=True, text=True)
        return app, log.read_text(), result

    def test_agent_only_has_no_server_or_frontend_work(self):
        app, log, result = self.run_update('agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('npm ', log)
        self.assertNotIn('restart looking-glass-server', log)
        self.assertIn('remote set-url origin git@github.com:EMOEMOJAI/Parallax.git', log)
        self.assertEqual((app / 'looking-glass-agent').read_text(), 'new-binary')
        self.assertEqual((app / 'frontend/dist/index.html').read_text(), 'old-frontend')
        self.assertEqual(next(app.glob('rollback.*/looking-glass-agent')).read_text(),
                         'old-binary')

    def test_server_and_combined(self):
        for roles in ('server', 'server,agent'):
            with self.subTest(roles=roles):
                app, log, result = self.run_update(roles)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((app / 'frontend/dist/index.html').read_text(), 'new-frontend')
                for role in roles.split(','):
                    self.assertEqual((app / ('looking-glass-' + role)).read_text(), 'new-binary')
                    self.assertIn('restart looking-glass-' + role, log)

    def test_failure_before_swap_preserves_running_artifacts(self):
        for fail in ('dirty', 'fetch', 'build', 'diverged', 'ahead', 'unsafe-owner', 'old-go', 'old-node'):
            with self.subTest(fail=fail):
                app, log, result = self.run_update('server,agent', fail)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('systemctl restart', log)
                self.assertEqual((app / 'looking-glass-server').read_text(), 'old-binary')
                self.assertEqual((app / 'looking-glass-agent').read_text(), 'old-binary')
                self.assertEqual((app / 'frontend/dist/index.html').read_text(), 'old-frontend')

    def test_restart_failure_restores_both_binaries_and_frontend(self):
        app, log, result = self.run_update('server,agent', 'restart')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('restoring artifacts', result.stderr)
        for role in ('server', 'agent'):
            self.assertEqual((app / ('looking-glass-' + role)).read_text(), 'old-binary')
        self.assertEqual((app / 'frontend/dist/index.html').read_text(), 'old-frontend')

    def test_fork_origin_is_preserved(self):
        _, log, result = self.run_update('agent', origin='git@github.com:someone/fork.git')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('remote set-url origin git@github.com:someone/fork.git', log)

    def test_interrupt_between_frontend_moves_restores_release(self):
        app, log, result = self.run_update('server,agent', 'signal')
        self.assertEqual(result.returncode, 143, result.stderr)
        self.assertIn('restoring artifacts', result.stderr)
        self.assertEqual((app / 'frontend/dist/index.html').read_text(), 'old-frontend')
        self.assertEqual((app / 'looking-glass-server').read_text(), 'old-binary')
        self.assertEqual((app / 'looking-glass-agent').read_text(), 'old-binary')

    def test_https_update_ignores_retired_key(self):
        app, log, result = self.run_update('agent', origin='https://github.com/example/fork.git', saved_key=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((app / 'update.sh').read_text(), '#!/bin/bash\n# next updater\n')

    def test_stopped_service_is_not_started(self):
        _, log, result = self.run_update('agent', stopped=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('systemctl restart', log)

    def test_relative_app_directory_is_rejected(self):
        env = dict(os.environ, APP_DIR='relative/install')
        result = subprocess.run(['bash', str(SCRIPT)], env=env, capture_output=True, text=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('absolute path', result.stderr)

    def test_no_installed_services_fails_before_git(self):
        _, log, result = self.run_update('')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('git ', log)


if __name__ == '__main__':
    unittest.main()
