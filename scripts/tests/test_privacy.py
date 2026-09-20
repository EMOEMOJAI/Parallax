import importlib.util
from pathlib import Path
import unittest

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
        for path in ['.env.production', 'deploy/inventory.yml', 'agent/.ssh/config', 'private/state.json']:
            self.assertTrue(privacy.inspect(path, ''))
        self.assertTrue(privacy.inspect('docs/setup.md', 'node' + '.local'))
        self.assertEqual(privacy.inspect('.env.example', ''), [])


if __name__ == '__main__':
    unittest.main()
