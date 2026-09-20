import os
from pathlib import Path
import subprocess
import shutil
import tempfile
import unittest

CHECKER = Path(__file__).resolve().parents[1] / 'check-commit-privacy.py'
SAFE = '123+contributor@users.noreply.github.com'
PRIVATE = 'person' + '@example.invalid'


class CommitPrivacyTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.cwd = Path(self.tmp.name)
        self.git('init', '-q')
        self.git('config', 'user.name', 'Contributor')
        self.git('config', 'user.email', SAFE)
        self.git('config', 'commit.gpgsign', 'false')
        self.git('config', 'tag.gpgsign', 'false')

    def git(self, *args, env=None):
        return subprocess.check_output(['git', *args], cwd=self.cwd, env=env, text=True, stderr=subprocess.DEVNULL).strip()

    def commit(self, author=SAFE, committer=SAFE):
        self.git('commit', '--allow-empty', '-qm', 'Synthetic fixture', env=dict(os.environ, GIT_AUTHOR_NAME='Contributor', GIT_COMMITTER_NAME='Contributor', GIT_AUTHOR_EMAIL=author, GIT_COMMITTER_EMAIL=committer))
        return self.git('rev-parse', 'HEAD')

    def check(self, *args, input=None):
        return subprocess.run(['python3', str(CHECKER), *args], cwd=self.cwd, input=input, text=True, capture_output=True)

    def test_safe_history(self):
        self.commit()
        self.assertEqual(self.check().returncode, 0)

    def test_hidden_ancestor_is_rejected_without_disclosing_email(self):
        self.commit(author=PRIVATE)
        self.commit()
        result = self.check()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)

    def test_private_committer(self):
        self.commit(committer=PRIVATE)
        self.assertNotEqual(self.check().returncode, 0)

    def test_annotated_tagger(self):
        self.commit()
        self.git('-c', 'user.email=' + PRIVATE, 'tag', '-a', 'v1.0.0', '-m', 'Fixture')
        self.assertNotEqual(self.check('v1.0.0').returncode, 0)

    def test_new_branch_and_deletion(self):
        commit = self.commit()
        zero = '0' * 40
        self.assertEqual(self.check('--pre-push', input=f'refs/heads/new {commit} refs/heads/new {zero}\n').returncode, 0)
        self.assertEqual(self.check('--pre-push', input=f'(delete) {zero} refs/heads/old {commit}\n').returncode, 0)

    def test_multiple_refs_and_malformed_input(self):
        safe = self.commit()
        private = self.commit(author=PRIVATE)
        zero = '0' * 40
        result = self.check('--pre-push', input=f'refs/heads/safe {safe} refs/heads/safe {zero}\nrefs/heads/private {private} refs/heads/private {zero}\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotEqual(self.check('--pre-push', input='bad input\n').returncode, 0)

    def test_installed_hook_blocks_a_real_push(self):
        source = CHECKER.parents[1]
        for name in ['scripts/check-commit-privacy.py', 'scripts/install-hooks.py', '.githooks/pre-push']:
            destination = self.cwd / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source / name, destination)
        subprocess.run(['python3', str(self.cwd / 'scripts/install-hooks.py')], cwd=self.cwd, check=True, capture_output=True)
        remote = self.cwd / 'remote.git'
        self.git('init', '--bare', '-q', str(remote))
        self.git('remote', 'add', 'fixture', str(remote))
        self.commit(author=PRIVATE)
        result = subprocess.run(['git', 'push', 'fixture', 'HEAD:refs/heads/main'], cwd=self.cwd, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Push blocked', result.stderr)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertEqual(self.git('--git-dir', str(remote), 'for-each-ref'), '')

    def test_shared_annotated_tag_is_checked_without_false_rejection(self):
        self.commit()
        self.git('tag', '-a', 'v1.0.0', '-m', 'Fixture')
        self.assertEqual(self.check('v1.0.0', 'v1.0.0').returncode, 0)
