import importlib.util
from pathlib import Path
import unittest
import subprocess
import tempfile

spec = importlib.util.spec_from_file_location('privacy', Path(__file__).resolve().parents[1] / 'check-privacy.py')
privacy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(privacy)


class PrivacyGuardTests(unittest.TestCase):
    def test_personal_markers_are_rejected(self):
        for text in ['owner' + '@mail.testdomain.org', '/' + 'Users/person/project/', '-----BEGIN ' + 'OPENSSH PRIVATE KEY-----']:
            self.assertTrue(privacy.inspect('README.md', text))

    def test_reserved_examples_and_git_remote_are_allowed(self):
        self.assertEqual(privacy.inspect('README.md', 'user@example.com 192.0.2.1 git@github.com:owner/repo.git'), [])

    def test_private_network_fixtures_do_not_become_deployment_inventory(self):
        address = '.'.join(['192', '168', '50', '20'])
        self.assertEqual(privacy.inspect('agent/policy_test.go', address), [])
        self.assertIn('private deployment address', privacy.inspect('docs/deployment.md', address))

    def test_sensitive_names_and_local_domains(self):
        for path in ['.env.production', 'frontend/public/production.env', 'deploy/inventory.yml', 'agent/.ssh/config', 'private/state.json']:
            self.assertTrue(privacy.inspect(path, ''))
        self.assertTrue(privacy.inspect('docs/setup.md', 'node' + '.local'))
        self.assertEqual(privacy.inspect('.env.example', ''), [])

    def test_private_ipv6_inventory_and_reserved_examples(self):
        for address in ['fd12:3456:789a::1', '[FE80::1%eth0]', '::ffff:c0a8:3201']:
            self.assertIn('private deployment address', privacy.inspect('docs/deployment.md', address))
            self.assertEqual(privacy.inspect('agent/policy_test.go', address), [])
        for address in ['2001:db8::1', '::1', 'https://example.com:8080', '12:30:00']:
            self.assertEqual(privacy.inspect('docs/deployment.md', address), [])

    def test_named_env_files_are_excluded_from_docker_context(self):
        rules = (Path(__file__).resolve().parents[2] / '.dockerignore').read_text().splitlines()
        self.assertIn('**/*.env', rules)

    def test_ipv6_in_prose_keeps_private_addresses_detectable(self):
        for address in ['fd12:3456:789a::1', 'fe80::1', '::ffff:192.168.50.1', '::ffff:c0a8:3201']:
            with self.subTest(address=address):
                self.assertIn('private deployment address', privacy.inspect('docs/deployment.md', f'Connect to {address}.'))
        for address in ['2001:db8::1', '::1', '::ffff:192.0.2.1']:
            self.assertEqual(privacy.inspect('docs/deployment.md', f'Example: {address}.'), [])


class IndexPrivacyTests(unittest.TestCase):
    def test_staged_private_content_survives_worktree_redaction_or_deletion(self):
        checker = Path(privacy.__file__).resolve()
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(['git', 'init', '-q', temp], check=True)
            file = root / 'README.md'
            secret = 'person' + '@mail.testdomain.org'
            file.write_text(secret)
            subprocess.run(['git', 'add', 'README.md'], cwd=root, check=True)
            for deleted in [False, True]:
                if deleted:
                    file.unlink()
                else:
                    file.write_text('safe example')
                result = subprocess.run(['python3', str(checker)], cwd=root, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(secret, result.stdout + result.stderr)
                self.assertIn('non-example email', result.stdout)

    def test_broken_symlink_is_not_skipped(self):
        checker = Path(privacy.__file__).resolve()
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            subprocess.run(['git', 'init', '-q', temp], check=True)
            (root / 'link').symlink_to('missing-target')
            subprocess.run(['git', 'add', 'link'], cwd=root, check=True)
            result = subprocess.run(['python3', str(checker)], cwd=root, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('symlink', result.stdout)


if __name__ == '__main__':
    unittest.main()
