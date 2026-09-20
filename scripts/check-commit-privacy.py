#!/usr/bin/env python3
"""Reject non-noreply commit/tag emails without logging the detected identities."""
import argparse
import re
import subprocess
import sys

SAFE_EMAIL = re.compile(r'(?:[^\s<>@]+@users\.noreply\.github\.com|noreply@github\.com)', re.I)
OID = re.compile(r'[0-9a-f]{40}(?:[0-9a-f]{24})?')


def git(*args):
    return subprocess.check_output(['git', '--no-replace-objects', *args], text=True, stderr=subprocess.PIPE).strip()


def pushed_revisions(lines):
    revisions = []
    for line in lines:
        fields = line.split()
        if len(fields) != 4 or not OID.fullmatch(fields[1]):
            raise ValueError('Invalid pre-push input')
        if set(fields[1]) != {'0'}:
            revisions.append(fields[1])
    return revisions


def check(revisions):
    if revisions and git('rev-parse', '--is-shallow-repository') == 'true':
        raise ValueError('Full history is required for privacy checks')
    failed = False
    commits = set()
    tags = set()
    for revision in revisions:
        # Resolve options-safe object IDs before passing revisions to Git again.
        oid = git('rev-parse', '--verify', '--end-of-options', revision)
        kind = git('cat-file', '-t', oid)
        while kind == 'tag':
            tags.add(oid)
            tag = git('cat-file', 'tag', oid)
            headers = tag.split('\n\n', 1)[0]
            match = re.search(r'^tagger .* <([^<>]+)> \d+ [+-]\d{4}$', headers, re.M)
            if not match or not SAFE_EMAIL.fullmatch(match.group(1)):
                print(f'{oid[:12]}: tagger email is not a GitHub noreply address', file=sys.stderr)
                failed = True
            oid = re.search(r'^object ([0-9a-f]+)$', headers, re.M).group(1)
            kind = git('cat-file', '-t', oid)
        if kind != 'commit':
            print('Only commit histories and tags pointing to commits can be privacy-checked.', file=sys.stderr)
            failed = True
            continue
        for record in git('log', '--format=%H%x00%ae%x00%ce', oid, '--').splitlines():
            commit, author, committer = record.split('\0')
            if commit in commits:
                continue
            commits.add(commit)
            for field, value in [('author', author), ('committer', committer)]:
                if not SAFE_EMAIL.fullmatch(value):
                    print(f'{commit[:12]}: {field} email is not a GitHub noreply address', file=sys.stderr)
                    failed = True
    if failed:
        print('Push blocked. Use GitHub noreply identities and repair unpublished commit/tag metadata before retrying.', file=sys.stderr)
    else:
        print(f'Commit privacy passed ({len(commits)} commits, {len(tags)} annotated tags).')
    return 1 if failed else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pre-push', action='store_true')
    parser.add_argument('revisions', nargs='*')
    args = parser.parse_args()
    try:
        revisions = pushed_revisions(sys.stdin) if args.pre_push else (args.revisions or ['HEAD'])
        return check(revisions)
    except (ValueError, subprocess.CalledProcessError) as error:
        # Git stderr can contain user-supplied ref names; keep failure output generic.
        print(f'Cannot verify commit privacy ({type(error).__name__}); push blocked.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
