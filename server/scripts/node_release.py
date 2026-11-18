#!/usr/bin/env python3
"""将当前节点程序、固定摘要电脑镜像及运行清单打包为离线安装包。"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
FILES = {'tomnode', 'computer-image.tar', 'node-release.json', 'node-install.example.json', '安装说明.txt', 'SHA256SUMS'}
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
    require(configuration.get('Entrypoint') == ['/usr/lib/tombot/computer-agent'] and not configuration.get('Cmd') and configuration.get('WorkingDir') == '/workspace' and configuration.get('Labels', {}).get('io.tombot.computer.storage-layout') == '2', '电脑镜像入口或存储约定不匹配')
    references = description.get('RepoDigests', [])
    require(references and all(re.fullmatch(IMAGE_REFERENCE, item) for item in references), '镜像缺少可导出固定摘要，请使用支持 OCI 镜像存储的 Docker 构建')
    return description['Id'], sorted(references)[0]


def validate_runtime(manifest):
    require(set(manifest) == {'version', 'timeout_seconds', 'minimum_free_bytes'} and manifest['version'] == 2, '宿主安装清单必须使用版本 2，不能锁定系统补丁版本')
    require(type(manifest['timeout_seconds']) is int and type(manifest['minimum_free_bytes']) is int, '软件安装预算必须是整数')
    require(30 <= manifest['timeout_seconds'] <= 3600 and 1 << 30 <= manifest['minimum_free_bytes'] <= 100 << 30, '软件安装预算无效')


def verify(archive):
    hashes, contents = {}, {}
    executable_header = b''
    with tarfile.open(archive, 'r:gz') as bundle:
        for member in bundle:
            require(member.name in FILES and member.name not in hashes and member.isfile(), '安装包存在未知文件、重复文件或链接')
            limit = MAX_IMAGE if member.name == 'computer-image.tar' else 256 << 20
            require(0 < member.size <= limit and member.mode == (0o755 if member.name == 'tomnode' else 0o644), '安装包文件大小或权限无效')
            with bundle.extractfile(member) as source:
                if member.name in {'SHA256SUMS', 'node-release.json'}:
                    require(member.size <= 65536, '安装元数据过大')
                    content = source.read()
                    contents[member.name] = content
                    hashes[member.name] = hashlib.sha256(content).hexdigest()
                elif member.name == 'tomnode':
                    executable_header = source.read(64)
                    checksum = hashlib.sha256(executable_header)
                    while block := source.read(1 << 20):
                        checksum.update(block)
                    hashes[member.name] = checksum.hexdigest()
                else:
                    hashes[member.name] = digest_stream(source)
    require(set(hashes) == FILES, '安装包缺少必要文件')
    checksums = {}
    for line in contents['SHA256SUMS'].decode().splitlines():
        checksum, name = line.split('  ', 1)
        require(name in FILES - {'SHA256SUMS'} and name not in checksums and re.fullmatch(r'[0-9a-f]{64}', checksum), '校验清单无效')
        checksums[name] = checksum
    require(checksums == {name: checksum for name, checksum in hashes.items() if name != 'SHA256SUMS'}, '安装包摘要不匹配')
    release = parse_json(contents['node-release.json'])
    require(set(release) == {'version', 'platform', 'image_reference', 'archive_sha256', 'docker'}, '离线发布清单字段无效')
    require(release['version'] == 1 and release['platform'] in {'linux/amd64', 'linux/arm64'} and release['archive_sha256'] == hashes['computer-image.tar'], '镜像清单与归档不匹配')
    require(re.fullmatch(IMAGE_REFERENCE, release['image_reference']), '镜像引用必须固定摘要')
    machine = 62 if release['platform'] == 'linux/amd64' else 183
    require(len(executable_header) == 64 and executable_header[:6] == b'\x7fELF\x02\x01' and int.from_bytes(executable_header[18:20], 'little') == machine, '节点程序架构与发布清单不符')
    validate_runtime(release['docker'])
    return release


def build(image, architecture, runtime, output):
    require(not output.exists(), '输出已存在，不覆盖历史安装包')
    manifest = parse_json(runtime.read_bytes())
    validate_runtime(manifest)
    image_id, reference = inspect_image(image, architecture)
    environment = {**os.environ, 'GOOS': 'linux', 'GOARCH': architecture, 'CGO_ENABLED': '0'}
    with tempfile.TemporaryDirectory(prefix='tombot-node-offline-') as temporary:
        stage = Path(temporary)
        payload = stage / 'payload'
        payload.mkdir()
        command(['go', 'build', '-trimpath', '-o', str(payload / 'tomnode'), './cmd/tomnode'], cwd=ROOT, env=environment)
        # 必须核对镜像内代理来自同一工作树，不能把陈旧镜像配给新节点程序。
        container = command(['docker', 'create', '--network', 'none', image_id]).decode().strip()
        try:
            for name in ('computer-agent', 'computer-task', 'computer-probe', 'bot-agent'):
                command(['go', 'build', '-trimpath', '-o', str(stage / name), './cmd/' + name], cwd=ROOT, env=environment)
                target = stage / (name + '.image')
                command(['docker', 'cp', container + ':/usr/lib/tombot/' + name, str(target)])
                require(digest(target) == digest(stage / name), '电脑镜像代理不是当前源码，请先重新构建镜像')
        finally:
            command(['docker', 'rm', container])
        print('正在导出离线电脑镜像', flush=True)
        command(['docker', 'image', 'save', '--output', str(payload / 'computer-image.tar'), image], timeout=1200)
        require((payload / 'computer-image.tar').stat().st_size <= MAX_IMAGE, '电脑镜像超过归档上限')
        release = {'version': 1, 'platform': 'linux/' + architecture, 'image_reference': reference, 'archive_sha256': digest(payload / 'computer-image.tar'), 'docker': manifest}
        (payload / 'node-release.json').write_text(json.dumps(release, ensure_ascii=False, indent=2) + '\n')
        shutil.copyfile(ROOT / 'deploy/node-install.example.json', payload / 'node-install.example.json')
        (payload / '安装说明.txt').write_text('完整解压到受保护目录（例如 /root/tombot），执行 sudo ./tomnode install。\n按提示粘贴管理平台新生成的接入码，再确认节点访问地址。默认无需填写 JSON。\n电脑镜像随包离线导入；Docker 和宿主依赖仍从官方软件源安装，需要联网。\n节点将保存身份、凭据与镜像，失败后使用同一程序重试；不要删除已生成身份。\n如需覆盖节点名称、容量、地址或监听网卡，填写可选示例并显式传入文件路径。\n支持 Debian/Ubuntu 与 RHEL/Rocky/AlmaLinux/CentOS Stream/Fedora；需要 systemd、cgroup v2 和 Docker API 1.47。节点默认端口 9444，服务端默认 9443。已有节点直接执行 sudo ./tomnode upgrade。需要远程更新时，首次将受信发布公钥 node-release.pub 放在本程序旁；后续在管理平台“电脑节点 → 远程升级”下发。节点能力以实际验收结果为准。\n')
        (payload / 'SHA256SUMS').write_text(''.join(digest(payload / name) + '  ' + name + '\n' for name in sorted(FILES - {'SHA256SUMS'})))
        candidate = stage / 'package.tar.gz'
        print('正在压缩并核验安装包', flush=True)
        with tarfile.open(candidate, 'w:gz', compresslevel=1) as bundle:
            for name in sorted(FILES):
                metadata = bundle.gettarinfo(str(payload / name), name)
                metadata.uid = metadata.gid = 0
                metadata.uname = metadata.gname = ''
                metadata.mode = 0o755 if name == 'tomnode' else 0o644
                with (payload / name).open('rb') as source:
                    bundle.addfile(metadata, source)
        verify(candidate)
        output.parent.mkdir(parents=True, exist_ok=True)
        with candidate.open('rb') as source, output.open('xb') as destination:
            shutil.copyfileobj(source, destination, 1 << 20)
    print(str(output), flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    subcommands = parser.add_subparsers(dest='action', required=True)
    builder = subcommands.add_parser('build')
    builder.add_argument('--image', required=True)
    builder.add_argument('--architecture', choices=('amd64', 'arm64'), required=True)
    builder.add_argument('--runtime', type=Path, required=True)
    builder.add_argument('--output', type=Path, required=True)
    checker = subcommands.add_parser('verify')
    checker.add_argument('archive', type=Path)
    arguments = parser.parse_args()
    if arguments.action == 'build':
        build(arguments.image, arguments.architecture, arguments.runtime, arguments.output)
    else:
        verify(arguments.archive)
        print('离线安装包核验通过')
