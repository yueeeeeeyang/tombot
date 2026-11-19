#!/usr/bin/env python3
"""独立构建、核验节点程序包或固定摘要的电脑镜像包。"""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PROGRAMS = ('tomnode', 'computer-agent', 'computer-task', 'computer-probe', 'bot-agent')
PROGRAM_FILES = set(PROGRAMS) | {'program-release.json', 'PROGRAM-SHA256SUMS'}
IMAGE_FILES = {'computer-image.tar', 'node-release.json', 'SHA256SUMS'}
# 完整离线包可供已部署节点在首次切换前核验和回滚。
OFFLINE_FILES = IMAGE_FILES | {'tomnode', 'node-install.example.json', '安装说明.txt'}
MAX_IMAGE = 16 << 30
IMAGE_REFERENCE = r'[a-zA-Z0-9][a-zA-Z0-9./:_-]{0,220}@sha256:[0-9a-f]{64}'


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    with path.open('rb') as source:
        return digest_stream(source)


def digest_stream(source):
    checksum = hashlib.sha256()
    while block := source.read(1 << 20):
        checksum.update(block)
    return checksum.hexdigest()


def parse_json(encoded):
    def unique_fields(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, '清单包含重复字段')
            result[key] = value
        return result
    return json.loads(encoded, object_pairs_hook=unique_fields)


def command(arguments, timeout=300, **options):
    try:
        return subprocess.run(arguments, check=True, timeout=timeout, capture_output=True, **options).stdout
    except (subprocess.SubprocessError, OSError) as error:
        raise RuntimeError('离线安装包构建阶段失败：' + arguments[0]) from error


def inspect_image(image, architecture):
    description = json.loads(command(['docker', 'image', 'inspect', image]))[0]
    require(description['Os'] == 'linux' and description['Architecture'] == architecture, '电脑镜像架构不匹配')
    configuration = description['Config']
    require(configuration.get('Entrypoint') == ['/usr/lib/tombot/computer-agent'] and not configuration.get('Cmd') and configuration.get('WorkingDir') == '/workspace' and configuration.get('Labels', {}).get('io.tombot.computer.storage-layout') == '2' and configuration.get('Labels', {}).get('io.tombot.computer.external-programs') == '1', '电脑镜像入口或存储约定不匹配')
    references = description.get('RepoDigests', [])
    require(references and all(re.fullmatch(IMAGE_REFERENCE, item) for item in references), '镜像缺少可导出固定摘要，请使用支持 OCI 镜像存储的 Docker 构建')
    return description['Id'], sorted(references)[0]


def validate_runtime(manifest):
    require(set(manifest) == {'version', 'timeout_seconds', 'minimum_free_bytes'} and manifest['version'] == 2, '宿主安装清单必须使用版本 2，不能锁定系统补丁版本')
    require(type(manifest['timeout_seconds']) is int and type(manifest['minimum_free_bytes']) is int, '软件安装预算必须是整数')
    require(30 <= manifest['timeout_seconds'] <= 3600 and 1 << 30 <= manifest['minimum_free_bytes'] <= 100 << 30, '软件安装预算无效')


def inspect_archive(archive):
    require(0 < archive.stat().st_size <= 17 << 30, '节点包大小无效')
    hashes, contents = {}, {}
    headers = {}
    with tarfile.open(archive, 'r:gz') as bundle:
        for member in bundle:
            require(member.name in PROGRAM_FILES | OFFLINE_FILES and member.name not in hashes and member.isfile(), '安装包存在未知文件、重复文件或链接')
            limit = MAX_IMAGE if member.name == 'computer-image.tar' else (256 << 20 if member.name in PROGRAMS else 64 << 10)
            require(0 < member.size <= limit and member.mode == (0o755 if member.name in PROGRAMS else 0o644), '安装包文件大小或权限无效')
            with bundle.extractfile(member) as source:
                if member.name in {'SHA256SUMS', 'PROGRAM-SHA256SUMS', 'program-release.json', 'node-release.json'}:
                    require(member.size <= 65536, '安装元数据过大')
                    content = source.read()
                    contents[member.name] = content
                    hashes[member.name] = hashlib.sha256(content).hexdigest()
                elif member.name in PROGRAMS:
                    executable_header = headers[member.name] = source.read(64)
                    checksum = hashlib.sha256(executable_header)
                    while block := source.read(1 << 20):
                        checksum.update(block)
                    hashes[member.name] = checksum.hexdigest()
                else:
                    hashes[member.name] = digest_stream(source)
        # 读至 gzip 校验尾部，截断或附加的非零 tar 内容不能登记为可信归档。
        tail = bundle.fileobj.read(65536)
        require(len(tail) < 65536 and not any(tail), '节点归档尾部内容无效')
    files = set(hashes)
    require(files in (PROGRAM_FILES, IMAGE_FILES, OFFLINE_FILES), '发布包缺件或混入其他类型的文件')
    programs = files == PROGRAM_FILES
    sums = 'PROGRAM-SHA256SUMS' if programs else 'SHA256SUMS'
    checksums = {}
    for line in contents[sums].decode().splitlines():
        checksum, name = line.split('  ', 1)
        require(name in files - {sums} and name not in checksums and re.fullmatch(r'[0-9a-f]{64}', checksum), '校验清单无效')
        checksums[name] = checksum
    require(checksums == {name: checksum for name, checksum in hashes.items() if name != sums}, '安装包摘要不匹配')
    release = parse_json(contents['program-release.json' if programs else 'node-release.json'])
    require(release.get('platform') in {'linux/amd64', 'linux/arm64'}, '发布架构无效')
    if programs or files == IMAGE_FILES or 'label' in release:
        require(isinstance(release.get('label'), str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._+-]{0,63}', release['label']), '版本标识无效')
    if programs:
        require(set(release) == {'version', 'platform', 'label'} and release['version'] == 2, '节点程序清单无效')
    else:
        require(set(release) - {'label'} == {'version', 'platform', 'image_reference', 'archive_sha256', 'docker'}, '离线发布清单字段无效')
        require(release['version'] == (2 if files == IMAGE_FILES else 1) and release['archive_sha256'] == hashes['computer-image.tar'], '镜像清单与归档不匹配')
        require(re.fullmatch(IMAGE_REFERENCE, release['image_reference']), '镜像引用必须固定摘要')
        validate_runtime(release['docker'])
    machine = 62 if release['platform'] == 'linux/amd64' else 183
    for header in headers.values():
        require(len(header) == 64 and header[:7] == bytes.fromhex('7f454c46020101') and int.from_bytes(header[16:18], 'little') in (2, 3) and int.from_bytes(header[18:20], 'little') == machine, '节点程序架构与发布清单不符')
    return release


def verify(archive):
    return inspect_archive(archive)


def write_package(inputs, profile, profile_name, sums_name, output):
    """先在输出目录核验候选归档，再独占发布，失败保留已有产物。"""
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=output.parent) as tmp:
        directory = Path(tmp)
        manifest = directory / profile_name
        manifest.write_text(json.dumps(profile, sort_keys=True) + '\n')
        files = {**inputs, profile_name: manifest}
        sums = directory / sums_name
        sums.write_text(''.join(digest(path) + '  ' + name + '\n' for name, path in sorted(files.items())))
        files[sums_name] = sums
        candidate = directory / 'release.tar.gz'
        with candidate.open('xb') as target, gzip.GzipFile(filename='', fileobj=target, mode='wb', compresslevel=1, mtime=0) as compressed, \
                tarfile.open(fileobj=compressed, mode='w') as bundle:
            for name, path in sorted(files.items()):
                header = tarfile.TarInfo(name)
                header.size = path.stat().st_size
                header.mode = 0o755 if name in PROGRAMS else 0o644
                with path.open('rb') as source:
                    bundle.addfile(header, source)
        verify(candidate)
        os.link(candidate, output)


def build_programs(directory, architecture, label, output):
    """程序均来自调用者固定的同一份编译快照，日常打包无需 Docker。"""
    files = {name: directory / name for name in PROGRAMS}
    write_package(files, {'version': 2, 'label': label, 'platform': 'linux/' + architecture},
                  'program-release.json', 'PROGRAM-SHA256SUMS', output)


def build_image(image, architecture, runtime, label, output):
    manifest = parse_json(runtime.read_bytes())
    validate_runtime(manifest)
    _, reference = inspect_image(image, architecture)
    with tempfile.TemporaryDirectory() as temporary:
        archive = Path(temporary) / 'computer-image.tar'
        command(['docker', 'image', 'save', '--output', str(archive), image], timeout=1200)
        profile = {'version': 2, 'label': label, 'platform': 'linux/' + architecture, 'image_reference': reference,
                   'archive_sha256': digest(archive), 'docker': manifest}
        write_package({'computer-image.tar': archive}, profile, 'node-release.json', 'SHA256SUMS', output)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest='action', required=True)
    for action in ('programs', 'image'):
        command_parser = actions.add_parser(action)
        command_parser.add_argument('--architecture', choices=('amd64', 'arm64'), required=True)
        command_parser.add_argument('--label', required=True)
        command_parser.add_argument('--output', type=Path, required=True)
        if action == 'programs':
            command_parser.add_argument('--bin-dir', type=Path, required=True)
        else:
            command_parser.add_argument('--image', required=True)
            command_parser.add_argument('--runtime', type=Path, required=True)
    actions.add_parser('verify').add_argument('archive', type=Path)
    args = parser.parse_args()
    if args.action == 'programs':
        build_programs(args.bin_dir, args.architecture, args.label, args.output)
    elif args.action == 'image':
        build_image(args.image, args.architecture, args.runtime, args.label, args.output)
    else:
        verify(args.archive)
        print('发布包核验通过')
