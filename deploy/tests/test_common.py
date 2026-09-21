"""Installer quoting, trust, migration and rollback regressions, without a host."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

COMMON = Path(__file__).resolve().parents[1] / 'common.sh'


class CommonTests(unittest.TestCase):
    def shell(self, code, *args, env=None):
        return subprocess.run(['bash', '-c', 'set -Eeuo pipefail; source "$1"; shift; ' + code,
                               'test', str(COMMON), *args], capture_output=True,
                              text=True, env=env)

    def test_argument_rejects_every_shell_representable_ascii_control(self):
        for char in [*range(1, 32), 127]:
            with self.subTest(char=char):
                result = self.shell('systemd_arg "$1"', 'label' + chr(char) + 'ExecStart=/bin/false')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('control characters', result.stderr)

    def test_argument_escapes_systemd_expansions_and_quotes(self):
        result = self.shell('systemd_arg "$1"', 'Name "quoted" $HOME %n \\ 🇭🇰')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '"Name \\"quoted\\" $$HOME %%n \\\\ 🇭🇰"')

    def test_trust_rejects_writable_existing_installation_before_git(self):
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp) / 'app'
            (app / 'repo').mkdir(parents=True)
            app.chmod(0o777)
            result = self.shell('APP_DIR=$1; check_install_trust; echo SHOULD_NOT_EXECUTE', str(app))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('Unsafe installation path', result.stderr)
            self.assertNotIn('SHOULD_NOT_EXECUTE', result.stdout)

    def test_legacy_environment_migration_preserves_other_values(self):
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp)
            env_file = app / '.env'
            for quote in ('', '"', "'"):
                with self.subTest(quote=quote):
                    env_file.write_text('CLIENT_API_KEY=private-value\nSCHEDULES_FILE=' + quote + str(app / 'schedules.json') + quote + '\n# operator comment\n')
                    env_file.chmod(0o640)
                    result = self.shell('APP_DIR=$1; migrate_legacy_schedule_env', str(app))
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, 'migrated\n')
                    self.assertEqual(env_file.read_text(), 'CLIENT_API_KEY=private-value\nSCHEDULES_FILE=' + str(app / 'data/schedules.json') + '\n# operator comment\n')
                    self.assertEqual(env_file.stat().st_mode & 0o777, 0o640)
            env_file.write_text('SCHEDULES_FILE=/custom/schedules.json\n')
            result = self.shell('APP_DIR=$1; migrate_legacy_schedule_env', str(app))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(env_file.read_text(), 'SCHEDULES_FILE=/custom/schedules.json\n')

    def test_schedule_migration_replaces_destination_symlink_without_touching_target(self):
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp)
            (app / 'data').mkdir()
            (app / 'schedules.json').write_text('[{"id":"fixture"}]')
            sentinel = app / 'sentinel'
            sentinel.write_text('keep this')
            sentinel.chmod(0o640)
            (app / 'data/schedules.json').symlink_to(sentinel)
            result = self.shell('APP_DIR=$1; migrate_legacy_schedule_data "$2" "$3"', str(app), str(os.getuid()), str(os.getgid()))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(sentinel.read_text(), 'keep this')
            self.assertEqual(sentinel.stat().st_mode & 0o777, 0o640)
            destination = app / 'data/schedules.json'
            self.assertFalse(destination.is_symlink())
            self.assertEqual(destination.read_text(), '[{"id":"fixture"}]')
            self.assertEqual(destination.stat().st_mode & 0o777, 0o600)
            self.assertEqual(list(app.glob('.schedules.migrate.*')), [])

    def test_schedule_migration_refuses_source_or_directory_symlinks(self):
        for which in ('source', 'directory'):
            with self.subTest(which=which), tempfile.TemporaryDirectory() as temp:
                app = Path(temp)
                target = app / 'outside'
                target.mkdir()
                (target / 'schedules.json').write_text('keep this')
                if which == 'source':
                    (app / 'data').mkdir()
                    (app / 'schedules.json').symlink_to(target / 'schedules.json')
                else:
                    (app / 'data').symlink_to(target, target_is_directory=True)
                    (app / 'schedules.json').write_text('[]')
                result = self.shell('APP_DIR=$1; migrate_legacy_schedule_data "$2" "$3"', str(app), str(os.getuid()), str(os.getgid()))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual((target / 'schedules.json').read_text(), 'keep this')

    def activation_case(self, trigger, initially_active=True):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        app = Path(temp.name) / 'app'
        unit_dir = Path(temp.name) / 'units'
        (app / 'frontend/dist').mkdir(parents=True)
        unit_dir.mkdir()
        paths = {
            app / 'looking-glass-server': 'old binary',
            app / '.env': 'old environment',
            app / 'update.sh': 'old updater',
            app / 'frontend/dist/index.html': 'old frontend',
            unit_dir / 'looking-glass-server.service': 'old unit',
        }
        for path, content in paths.items():
            path.write_text(content)
        log = Path(temp.name) / 'systemctl.log'
        code = '''APP_DIR=$1; SYSTEMD_DIR=$2; COMMAND_LOG=$3; INIT_ACTIVE=$4
systemctl() {
 printf '%s\\n' "$*" >> "$COMMAND_LOG"
 case "$1" in is-active|is-enabled) [ "$INIT_ACTIVE" = true ] ;; *) return 0 ;; esac
}
begin_activation looking-glass-server
printf new > "$APP_DIR/looking-glass-server"
printf new > "$APP_DIR/.env"
printf new > "$APP_DIR/update.sh"
printf new > "$APP_DIR/frontend/dist/index.html"
printf new > "$SYSTEMD_DIR/looking-glass-server.service"
''' + trigger
        result = self.shell(code, str(app), str(unit_dir), str(log), str(initially_active).lower())
        return paths, log.read_text(), result

    def test_failed_activation_restores_binary_frontend_config_and_unit(self):
        paths, log, result = self.activation_case('false')
        self.assertNotEqual(result.returncode, 0)
        for path, original in paths.items():
            self.assertEqual(path.read_text(), original)
        self.assertIn('restart looking-glass-server', log)

    def test_signal_activation_rolls_back(self):
        for sig, code in [('INT', 130), ('TERM', 143)]:
            with self.subTest(signal=sig):
                paths, _, result = self.activation_case('kill -' + sig + ' $$')
                self.assertEqual(result.returncode, code, result.stderr)
                for path, original in paths.items():
                    self.assertEqual(path.read_text(), original)

    def test_failed_reinstall_keeps_previously_stopped_service_stopped(self):
        paths, log, result = self.activation_case('false', initially_active=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('restart looking-glass-server', log)
        self.assertIn('disable looking-glass-server', log)
        for path, original in paths.items():
            self.assertEqual(path.read_text(), original)

    def test_failed_new_install_removes_created_artifacts_and_unit(self):
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp) / 'app'
            units = Path(temp) / 'units'
            app.mkdir(); units.mkdir()
            result = self.shell('''APP_DIR=$1; SYSTEMD_DIR=$2
systemctl() { case "$1" in is-active|is-enabled) return 1 ;; *) return 0 ;; esac; }
begin_activation looking-glass-server
mkdir -p "$APP_DIR/frontend/dist"
for path in "$APP_DIR/looking-glass-server" "$APP_DIR/.env" "$APP_DIR/update.sh" "$APP_DIR/frontend/dist/index.html" "$SYSTEMD_DIR/looking-glass-server.service"; do printf new > "$path"; done
false
''', str(app), str(units))
            self.assertNotEqual(result.returncode, 0, result.stderr)
            for path in (app / 'looking-glass-server', app / '.env', app / 'update.sh', app / 'frontend/dist', units / 'looking-glass-server.service'):
                self.assertFalse(path.exists(), str(path))

    def test_prepare_repo_rejects_commits_ahead_of_remote(self):
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp)
            (app / 'repo/.git').mkdir(parents=True)
            result = self.shell('''APP_DIR=$1; BRANCH=main; REPO_URL=https://github.com/example/project.git
git() {
 case " $* " in
  *" merge-base "*) return 1 ;;
  *" merge "*) echo UNSAFE_MERGE ;;
 esac
}
prepare_repo
''', str(app))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('commits outside the requested remote branch', result.stderr)
            self.assertNotIn('UNSAFE_MERGE', result.stdout)

    def test_patched_go_is_retained_and_old_or_unsupported_branch_is_replaced(self):
        for version, accepted in [('1.27.1', True), ('1.27.2', True), ('1.27.0', False), ('1.26.0', False)]:
            with self.subTest(version=version):
                result = self.shell('VERSION=$1; go() { echo "go version go$VERSION linux/amd64"; }; curl() { echo FETCH >&2; return 72; }; ensure_go', version)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
                self.assertEqual('FETCH' in result.stderr, not accepted)

    def test_only_patched_node_lts_is_retained(self):
        for version, accepted in [('24.21.0', True), ('24.22.0', True), ('24.20.0', False), ('22.12.0', False), ('23.0.0', False)]:
            with self.subTest(version=version):
                result = self.shell('VERSION=$1; node() { echo "$VERSION"; }; curl() { echo FETCH >&2; return 72; }; ensure_node', version)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
                self.assertEqual('FETCH' in result.stderr, not accepted)


if __name__ == '__main__':
    unittest.main()
