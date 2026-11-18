#!/usr/bin/env python3
"""核验完整离线包并签发有序分片清单；私钥仅由发布者在本机持有。"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
import tarfile

from node_release import verify


def sign(archive, label, key, output):
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._+\-]{0,63}', label):
        raise ValueError('版本标识无效')
    before = archive.stat()
    release = verify(archive)
    with archive.open('rb') as source:
        chunks = [hashlib.sha256(part).hexdigest() for part in iter(lambda: source.read(4 << 20), b'')]
    with tarfile.open(archive, "r:gz") as bundle:
        checksums = hashlib.sha256(bundle.extractfile("SHA256SUMS").read(65537)).hexdigest()
    payload = json.dumps(dict(version=1, checksums=checksums, label=label, platform=release['platform'], size=archive.stat().st_size,
                              chunks=chunks), separators=(',', ':')).encode()
    with tempfile.NamedTemporaryFile() as source:
        source.write(payload)
        source.flush()
        signature = subprocess.run(['openssl', 'pkeyutl', '-sign', '-rawin', '-inkey', str(key), '-in', source.name],
                                   check=True, capture_output=True, timeout=30).stdout
    if len(signature) != 64:
        raise ValueError('需要 Ed25519 发布私钥')
    after = archive.stat()
    if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
        raise ValueError('核验或签名期间节点归档发生变化')
    with output.open('x') as destination:
        json.dump(dict(payload=base64.b64encode(payload).decode(), signature=base64.b64encode(signature).decode()), destination)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('--label', required=True)
    parser.add_argument('--key', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    sign(args.archive, args.label, args.key, args.output)
