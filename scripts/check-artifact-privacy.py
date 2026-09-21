#!/usr/bin/env python3
"""Inspect release bundles and image application payloads; never print matches."""
import argparse
import importlib.util
import json
from pathlib import Path, PurePosixPath
import struct
import subprocess
import tarfile
import tempfile

spec = importlib.util.spec_from_file_location('privacy', Path(__file__).with_name('check-privacy.py'))
privacy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(privacy)
TEXT = {'.js', '.json', '.html', '.css', '.md', '.txt', '.map', '.svg', '.yaml', '.yml', '.toml', '.ini', '.conf', '.csv', '.xml'}
LIMIT = 128 * 1024 * 1024


def forbidden_runtime_path(name):
    path = PurePosixPath(name)
    return any(part in {'.git', '.deployment', '.ssh', 'private'} for part in path.parts) or path.name == '.deploy-key-path' or path.name.startswith('schedules.json')


def image_metadata(data):
    """Allow rendering metadata, reject text, identity, location and timestamps."""
    if data.startswith(b'\x89PNG\r\n\x1a\n'):
        pos = 8
        while pos < len(data):
            if pos + 12 > len(data): return True
            size = struct.unpack('>I', data[pos:pos+4])[0]
            if pos + 12 + size > len(data): return True
            if data[pos+4:pos+8] in {b'tEXt', b'zTXt', b'iTXt', b'eXIf'}: return True
            pos += size + 12
    elif data.startswith(b'\xff\xd8'):
        pos = 2
        while pos < len(data):
            if pos+2 > len(data) or data[pos] != 255: return True
            # JPEG permits fill bytes before a marker.
            while pos+1 < len(data) and data[pos+1] == 255:
                pos += 1
            if pos+1 >= len(data): return True
            marker = data[pos+1]
            if marker == 0xd9: return pos+2 != len(data)
            if pos+4 > len(data): return True
            size = struct.unpack('>H', data[pos+2:pos+4])[0]
            if size < 2 or pos+2+size > len(data): return True
            payload = data[pos+4:pos+2+size]
            if marker == 0xfe: return True
            if marker == 0xe1:
                if not payload.startswith(b'Exif\0\0'): return True
                tiff = payload[6:]
                if len(tiff) < 8 or tiff[:2] not in {b'II', b'MM'}: return True
                endian = '<' if tiff[:2] == b'II' else '>'
                def value(fmt, offset): return struct.unpack_from(endian+fmt, tiff, offset)[0]
                try:
                    if value('H', 2) != 42: return True
                    pending = [value('I', 4)]; seen = set()
                    while pending:
                        offset = pending.pop()
                        if not offset: continue
                        if offset in seen: return True
                        seen.add(offset)
                        count = value('H', offset)
                        for index in range(count):
                            entry = offset + 2 + index*12
                            tag = value('H', entry)
                            if tag == 0x8769: pending.append(value('I', entry+8))
                            elif tag not in {0x100, 0x101, 0x112, 0x11a, 0x11b, 0x128, 0xa001, 0xa002, 0xa003}: return True
                            # Rendering fields must not smuggle ASCII/undefined data.
                            if value('H', entry+2) not in {3, 4, 5}: return True
                        pending.append(value('I', offset+2+count*12))
                except (struct.error, IndexError): return True
            if marker == 0xed:
                # Existing banner: empty IPTC resource plus its empty-content MD5.
                empty = b'Photoshop 3.0\0' + b'8BIM\x04\x04\0\0\0\0\0\0' + b'8BIM\x04\x25\0\0\0\0\0\x10' + bytes.fromhex('d41d8cd98f00b204e9800998ecf8427e')
                if payload != empty: return True
            pos += size + 2
            if marker == 0xda:
                # Entropy-coded scan bytes escape FF as FF00; restart markers
                # also belong to the scan. Inspect metadata between scans and
                # before EOI, including progressive JPEGs.
                while True:
                    pos = data.find(b'\xff', pos)
                    if pos < 0 or pos+1 >= len(data): return True
                    following = data[pos+1]
                    if following == 0 or 0xd0 <= following <= 0xd7:
                        pos += 2
                        continue
                    break
        return True  # JPEG ended without EOI.
    return False


def inspect_file(name, data):
    findings = set(privacy.inspect(name, ''))
    if forbidden_runtime_path(name): findings.add('repository or runtime state in artifact')
    decoded = data.decode('utf-8', errors='replace')
    if privacy.HOME.search(decoded): findings.add('personal build path')
    if privacy.PRIVATE_KEY.search(decoded): findings.add('private key material')
    try:
        text = data.decode('utf-16' if data.startswith((b'\xff\xfe', b'\xfe\xff')) else 'utf-8')
    except UnicodeError:
        text = None
    # Detect text by content too: LICENSE and other extensionless files ship in
    # release bundles, and config formats must not evade the text policy.
    if text is not None and '\0' not in text:
        findings.update(privacy.inspect('docs/' + name, text))
    elif PurePosixPath(name).suffix.lower() in TEXT:
        findings.update(privacy.inspect('docs/' + name, decoded))
    if image_metadata(data): findings.add('unapproved image metadata or malformed image')
    return findings


def scan_archive(path, image=False, allow_empty=False):
    failures = set(); count = 0; total = 0
    options = {'fileobj': path, 'mode': 'r|*'} if hasattr(path, 'read') else {'name': path}
    with tarfile.open(**options) as archive:
        for member in archive:
            name = str(PurePosixPath(member.name))
            if image and not (name == 'app' or name.startswith('app/')): continue
            count += 1; total += member.size
            if count > 10000 or total > 512*1024*1024 or member.size > LIMIT:
                raise ValueError('application payload exceeds inspection limits')
            if PurePosixPath(name).is_absolute() or '..' in PurePosixPath(name).parts:
                failures.add('unsafe archive path')
            if not image and (member.uname or member.gname or member.uid or member.gid):
                failures.add('archive owner metadata')
            if any(k not in {'path', 'size', 'mtime'} for k in member.pax_headers): failures.add('extended archive metadata')
            if not (member.isfile() or member.isdir()): failures.add('application link or special file')
            failures.update(privacy.inspect(name, ''))
            if forbidden_runtime_path(name): failures.add('repository or runtime state in artifact')
            if member.isfile(): failures.update(inspect_file(name, archive.extractfile(member).read(LIMIT+1)))
    if count == 0 and not allow_empty: failures.add('empty application payload')
    return failures


def check_context():
    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp); context = root/'context'; context.mkdir()
        (context/'.dockerignore').write_bytes(Path(__file__).resolve().parents[1].joinpath('.dockerignore').read_bytes())
        for name in ['.env', '.env.production', 'production.env', 'nested/staging.env', 'nested/id_ed25519', 'private/inventory.yml', 'nested/credential.key']:
            p = context/name; p.parent.mkdir(parents=True, exist_ok=True); p.write_text('synthetic-private-sentinel')
        (context/'safe.txt').write_text('public-fixture')
        (context/'Dockerfile').write_text('FROM scratch\nCOPY . /payload/\n')
        subprocess.run(['docker', 'build', '--quiet', '--output', 'type=local,dest='+str(root/'out'), str(context)], check=True, stdout=subprocess.DEVNULL)
        payload = root/'out/payload'
        assert (payload/'safe.txt').read_text() == 'public-fixture'
        assert not any(p.is_file() and b'synthetic-private-sentinel' in p.read_bytes() for p in payload.rglob('*')), 'Docker context included a forbidden fixture'
    print('Docker context exclusions verified with synthetic files.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive', action='append', default=[])
    parser.add_argument('--image', action='append', default=[])
    parser.add_argument('--check-context', action='store_true')
    args = parser.parse_args()
    if not (args.archive or args.image or args.check_context): parser.error('select an archive, image or context check')
    failures = set()
    for archive in args.archive: failures.update(scan_archive(archive))
    for tag in args.image:
        config = json.loads(subprocess.check_output(['docker', 'image', 'inspect', tag]))[0]['Config']
        config_text = json.dumps(config)
        failures.update(privacy.inspect('docs/image-config.json', config_text))
        for entry in config.get('Env') or []:
            key, _, value = entry.partition('=')
            if key in {'CLIENT_API_KEY', 'AGENT_API_KEY', 'METRICS_TOKEN', 'ALERT_WEBHOOK_URL'} and value:
                failures.add('credential embedded in image configuration')
        container = subprocess.check_output(['docker', 'create', '--entrypoint', '/bin/true', tag], text=True).strip()
        try:
            with tempfile.TemporaryDirectory() as temp:
                path = Path(temp)/'image.tar'
                subprocess.run(['docker', 'export', '-o', str(path), container], check=True)
                failures.update(scan_archive(path, image=True))
                # A deleted secret still ships in an earlier image layer.
                saved = Path(temp)/'layers.tar'
                subprocess.run(['docker', 'image', 'save', '-o', str(saved), tag], check=True)
                with tarfile.open(saved) as layers:
                    manifests = json.load(layers.extractfile('manifest.json'))
                    for manifest in manifests:
                        for name in manifest['Layers']:
                            with layers.extractfile(name) as layer:
                                failures.update(scan_archive(layer, image=True, allow_empty=True))
                history = subprocess.check_output(['docker', 'history', '--no-trunc', '--format', '{{json .CreatedBy}}', tag], text=True)
                failures.update(privacy.inspect('docs/image-history.txt', history))
        finally: subprocess.run(['docker', 'rm', '-f', container], check=True, stdout=subprocess.DEVNULL)
    if args.check_context: check_context()
    if failures:
        for finding in sorted(failures): print('Artifact privacy: ' + finding)
        return 1
    print('Artifact privacy checks passed (application files, image config and archive metadata).')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
