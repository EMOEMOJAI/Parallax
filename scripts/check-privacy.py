#!/usr/bin/env python3
"""Check tracked content without printing potentially sensitive matched values.

This guard complements secret scanning; it cannot prove a repository anonymous.
Use reserved example domains/addresses in documentation and synthetic test data.
"""
import ipaddress
import pathlib
import re
import subprocess
import sys

EMAIL = re.compile(r'(?<![\w.+-])[\w.+-]+@([\w.-]+\.[A-Za-z]{2,})(?![\w.-])')
HOME = re.compile(r'/(?:Users|home)/[A-Za-z0-9_.-]+/')
PRIVATE_KEY = re.compile(r'-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----')
IPV4 = re.compile(r'(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])')
PRIVATE_NETS = tuple(ipaddress.ip_network(n) for n in ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16'))
LOCAL_DOMAIN = re.compile(r'\b(?:[a-z0-9-]+\.)+(?:local|lan|internal|home\.arpa)\b', re.I)


def inspect(path, data):
    """Return rule names only, never the detected value."""
    findings = set()
    name = pathlib.PurePosixPath(path)
    if (name.name.startswith('.env') and name.name != '.env.example') or name.suffix in {'.pem', '.key', '.p12', '.pfx'} or any(p in {'.ssh', 'private'} for p in name.parts) or name.name in {'id_rsa', 'id_ed25519', 'parallax-deploy', 'looking-glass-deploy', 'inventory.ini', 'inventory.yml', 'inventory.yaml'}:
        findings.add('credential or inventory filename')
    if PRIVATE_KEY.search(data):
        findings.add('private key material')
    if HOME.search(data):
        findings.add('personal home directory')
    for match in EMAIL.finditer(data):
        domain = match.group(1).lower()
        # Git SSH URLs are not email addresses.
        if match.group(0) == 'git' + '@github.com' and data[match.end():match.end()+1] == ':':
            continue
        if domain not in {'example.com', 'example.org', 'example.net', 'users.noreply.github.com'} and not domain.endswith(('.example', '.test', '.invalid')):
            findings.add('non-example email address')
    # Network policy tests need private addresses, but deployment documentation
    # and executable setup scripts should use reserved documentation addresses.
    is_deployment = (path.startswith('docs/') or path.startswith('deploy/') or name.name in {'README.md', 'docker-compose.yml'}) and '/tests/' not in path
    if is_deployment:
        for match in IPV4.finditer(data):
            try:
                address = ipaddress.ip_address(match.group())
                if any(address in network for network in PRIVATE_NETS):
                    findings.add('private deployment address')
            except ValueError:
                pass
        if LOCAL_DOMAIN.search(data):
            findings.add('private deployment domain')
    return sorted(findings)


def main():
    paths = subprocess.check_output(['git', 'ls-files', '-z']).decode().split('\0')
    failed = False
    for path in filter(None, paths):
        file = pathlib.Path(path)
        if not file.exists():
            continue
        if file.is_symlink():
            print(f'{path}: symlink requires manual privacy review')
            failed = True
            continue
        data = file.read_bytes().decode('utf-8', errors='replace')
        for finding in inspect(path, data):
            print(f'{path}: {finding}')
            failed = True
    if failed:
        return 1
    print('Tracked-file privacy guard passed.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
