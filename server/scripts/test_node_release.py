"""离线包交付边界：不接受摘要错误、缺件、路径穿越、重复文件或架构混用。"""

import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

import node_release


class NodeReleaseTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.archive = Path(self.directory.name) / 'node.tar.gz'
        self.runtime = json.loads((node_release.ROOT / 'deploy/runtime/ubuntu-24.04-amd64.json').read_bytes())
        header = bytearray(64)
        header[:6] = b'\x7fELF\x02\x01'
        header[18:20] = (62).to_bytes(2, 'little')
        self.contents = {
            'tomnode': bytes(header), 'computer-image.tar': b'archive-fixture',
            'node-install.example.json': b'{}\n', '安装说明.txt': '测试安装说明'.encode(),
        }
        self.release = {'version': 1, 'platform': 'linux/amd64', 'image_reference': 'example.test/computer@sha256:' + hashlib.sha256(b'image-fixture').hexdigest(), 'archive_sha256': hashlib.sha256(self.contents['computer-image.tar']).hexdigest(), 'docker': self.runtime}

    def bundle(self, missing=None, extra=None, changed=None):
        self.contents['node-release.json'] = json.dumps(self.release).encode()
        self.contents['SHA256SUMS'] = ''.join(hashlib.sha256(content).hexdigest() + '  ' + name + '\n' for name, content in self.contents.items() if name != 'SHA256SUMS').encode()
        with tarfile.open(self.archive, 'w:gz') as bundle:
            for name, content in list(self.contents.items()) + ([extra] if extra else []):
                if name == missing:
                    continue
                if name == changed:
                    content += b'changed'
                member = tarfile.TarInfo(name)
                member.size = len(content)
                member.mode = 0o755 if name == 'tomnode' else 0o644
                bundle.addfile(member, io.BytesIO(content))

    def test_complete_bundle(self):
        self.bundle()
        self.assertEqual(node_release.verify(self.archive), self.release)

    def test_rejects_archive_structure(self):
        for options in ({'missing': 'tomnode'}, {'extra': ('../escape', b'x')}, {'extra': ('computer-image.tar', b'x')}, {'changed': 'computer-image.tar'}):
            with self.subTest(options=options):
                self.bundle(**options)
                with self.assertRaises(ValueError):
                    node_release.verify(self.archive)

    def test_rejects_link(self):
        with tarfile.open(self.archive, 'w:gz') as bundle:
            member = tarfile.TarInfo('tomnode')
            member.type = tarfile.SYMTYPE
            member.linkname = '/bin/sh'
            bundle.addfile(member)
        with self.assertRaises(ValueError):
            node_release.verify(self.archive)

    def test_rejects_manifest_mismatch(self):
        for key, value in (('platform', 'linux/arm64'), ('image_reference', 'computer:latest'), ('archive_sha256', '0' * 64), ('version', 2), ('unknown', True)):
            with self.subTest(key=key):
                original = dict(self.release)
                self.release[key] = value
                self.bundle()
                with self.assertRaises(ValueError):
                    node_release.verify(self.archive)
                self.release = original

    def test_runtime_and_duplicate_fields(self):
        for key, value in (('engine_package_version', 'latest'), ('allowed_engine_versions', ['0.0.0']), ('timeout_seconds', 0), ('repository_fingerprint', ''), ('unknown', 1)):
            with self.subTest(key=key), self.assertRaises(ValueError):
                node_release.validate_runtime({**self.runtime, key: value})
        with self.assertRaises(ValueError):
            node_release.parse_json(b'{"version":1,"version":1}')


if __name__ == '__main__':
    unittest.main()
