import importlib.util
import io
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('artifacts', Path(__file__).resolve().parents[1]/'check-artifact-privacy.py')
artifacts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(artifacts)


class ArtifactPrivacyTests(unittest.TestCase):
    def archive(self, name='app/readme.txt', data=b'public fixture', owner='', kind=None):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp)/'release.tar.gz'
            with tarfile.open(path, 'w:gz') as archive:
                member = tarfile.TarInfo(name)
                member.uname = owner
                if kind:
                    member.type = kind; member.linkname = '/etc/passwd'
                    archive.addfile(member)
                else:
                    member.size = len(data)
                    archive.addfile(member, io.BytesIO(data))
            return artifacts.scan_archive(path)

    def test_clean_bundle_and_embedded_leaks(self):
        self.assertEqual(self.archive(), set())
        self.assertIn('credential or inventory filename', self.archive('app/production.env'))
        self.assertIn('archive owner metadata', self.archive(owner='synthetic-owner'))
        self.assertIn('personal build path', self.archive('app/binary', b'\x7fELF/Users/'+b'fixture/build/main.go'))
        self.assertIn('private key material', self.archive(data=b'-----BEGIN '+b'OPENSSH PRIVATE KEY-----'))
        self.assertIn('non-example email address', self.archive(data=b'person'+b'@mail.testdomain.org'))
        for name in ['app/.git/config', 'app/data/schedules.json', 'app/.deploy-key-path']:
            self.assertIn('repository or runtime state in artifact', self.archive(name))

    def test_archive_paths_and_links(self):
        self.assertIn('unsafe archive path', self.archive('../escape.txt'))
        self.assertIn('application link or special file', self.archive(kind=tarfile.SYMTYPE))

    def test_streamed_image_layer_does_not_hide_an_earlier_secret(self):
        findings = set()
        for name in ['app/production.env', 'app/.wh.production.env']:
            layer = io.BytesIO()
            with tarfile.open(fileobj=layer, mode='w') as archive:
                member = tarfile.TarInfo(name)
                archive.addfile(member, io.BytesIO())
            layer.seek(0)
            findings.update(artifacts.scan_archive(layer, image=True, allow_empty=True))
        self.assertIn('credential or inventory filename', findings)

    def test_image_metadata(self):
        png = b'\x89PNG\r\n\x1a\n'+struct.pack('>I', 6)+b'tEXt'+b'Author'+b'\0'*4
        self.assertTrue(artifacts.image_metadata(png))
        self.assertTrue(artifacts.image_metadata(png[:-1]))
        # EXIF Artist is forbidden, even when its value is numeric/malformed.
        tiff = b'MM\0*\0\0\0\x08\0\x01'+struct.pack('>HHII', 0x13b, 3, 1, 0)+b'\0'*4
        payload = b'Exif\0\0'+tiff
        jpeg = b'\xff\xd8\xff\xe1'+struct.pack('>H', len(payload)+2)+payload+b'\xff\xd9\0\0'
        self.assertTrue(artifacts.image_metadata(jpeg))
        for path in (Path(__file__).resolve().parents[2]/'frontend/public').glob('*.png'):
            self.assertFalse(artifacts.image_metadata(path.read_bytes()))
