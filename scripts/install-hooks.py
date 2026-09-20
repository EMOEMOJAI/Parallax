#!/usr/bin/env python3
"""Install the privacy hook for this repository and all its linked worktrees."""
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[1]
common = Path(subprocess.check_output(['git', 'rev-parse', '--path-format=absolute', '--git-common-dir'], text=True, cwd=ROOT).strip())
hooks = common / 'hooks'
configured = subprocess.run(['git', 'config', '--get', 'core.hooksPath'], cwd=ROOT, capture_output=True, text=True)
if configured.returncode == 0 and Path(configured.stdout.strip()) != hooks:
    raise SystemExit('Existing custom hooksPath preserved. Integrate the privacy hook there before changing hook configuration.')
hook = hooks / 'pre-push'
if hook.exists() and 'Parallax commit privacy hook' not in hook.read_text():
    raise SystemExit('Existing pre-push hook preserved. Integrate its checks with the privacy hook manually.')
(hooks / 'parallax').mkdir(parents=True, exist_ok=True)
shutil.copyfile(ROOT / 'scripts/check-commit-privacy.py', hooks / 'parallax/check-commit-privacy.py')
shutil.copyfile(ROOT / '.githooks/pre-push', hook)
hook.chmod(0o755)
subprocess.run(['git', 'config', '--local', 'core.hooksPath', str(hooks)], cwd=ROOT, check=True)
print('Privacy hook installed for this repository and its linked worktrees. Re-run after updating hook source files.')
