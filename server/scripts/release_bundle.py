#!/usr/bin/env python3
"""从独立源码快照构建双架构发布包，或只读核验发布包的完整内容。"""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import signal
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile

from build_computer_image import ENVIRONMENT_FILE, IMAGE_FILES


ROOT = Path(__file__).resolve().parents[2]
ARCHITECTURES = ("amd64", "arm64")
HOST_PROGRAMS = ("tomd", "tomdb", "tomdocker-bootstrap", "tomdeploy-check", "tomdeploy-config", "tomimage-prepare", "tomnode",
                 "tomnode-register")
COMPUTER_PROGRAMS = ("computer-agent", "computer-task", "computer-probe", "bot-agent")
DEPLOY_FILES = ("tomd.service", "systemd/tombot-docker-bootstrap.service",
                "systemd/tombot-deployment-check.service",
                "systemd/tombot-image-prepare.service", "systemd/tombot-node.service",
                "node.example.json", "docker-install.schema.json", "computer-image.schema.json", "CONFIGURE.md")
VERIFICATION_FILES = ("release_bundle.py", "build_computer_image.py")
SOURCE_DIRECTORIES = ("server/cmd", "server/internal", "server/web", "server/deploy", "server/scripts", "admin", "ui")
SOURCE_SUFFIXES = {".go", ".sql", ".html", ".json", ".py", ".xml", ".service", ".md",
                   ".ts", ".tsx", ".js", ".mjs", ".css", ".png", ".svg", ".ico", ".woff2"}
SOURCE_NAMES = {"Dockerfile", "Caddyfile", "seccomp.LICENSE", ".prettierignore"}
EXCLUDED_DIRECTORIES = {"node_modules", "dist", "coverage", "test-results", "playwright-report", "__pycache__"}
ASSET_NAME = re.compile(r"admin/assets/[A-Za-z0-9_-]+\.(js|css|woff2|png|svg|ico)\Z")
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9][a-z0-9.-]{0,39})?\Z")
DIGEST = re.compile(r"[a-f0-9]{64}\Z")
MAX_FILE = 256 << 20
MAX_ARCHIVE = 1 << 30
MAX_CONTENT = 2 << 30
MAX_MANIFEST = 256 << 10
MAX_FILES = 400
CHUNK_SIZE = 1 << 20


class ReleaseError(Exception):
    """发布失败只报告固定阶段，不输出环境、私有软件源或文件内容。"""


def require(condition, message):
    if not condition:
        raise ReleaseError(message)


def release_version(value):
    require(isinstance(value, str) and len(value) <= 64 and VERSION.fullmatch(value), "发布版本无效")
    return value


def read_source(path, maximum=MAX_FILE):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as source:
        before = os.fstat(source.fileno())
        require(stat.S_ISREG(before.st_mode) and 0 <= before.st_size <= maximum, "发布输入不是有限普通文件")
        content = source.read(maximum + 1)
        after = os.fstat(source.fileno())
        require(len(content) <= maximum and file_identity(before) == file_identity(after), "发布输入读取期间发生变化")
        return content


def file_identity(value):
    return value.st_dev, value.st_ino, value.st_mode, value.st_size, value.st_mtime_ns, value.st_ctime_ns


def source_paths(root):
    paths = [root / "server/go.mod", root / "server/go.sum", root / "server/Makefile"]
    for relative in SOURCE_DIRECTORIES:
        base = root / relative
        require(base.is_dir() and not base.is_symlink(), "构建源码目录缺失或为链接")
        for directory, directories, files in os.walk(base, followlinks=False):
            directories[:] = sorted(name for name in directories if name not in EXCLUDED_DIRECTORIES)
            require(not any((Path(directory) / name).is_symlink() for name in directories), "构建源码包含目录链接")
            for name in sorted(files):
                if name == ".DS_Store" or name == ".env" or name.startswith(".env."):
                    continue
                path = Path(directory) / name
                require(path.suffix in SOURCE_SUFFIXES or name in SOURCE_NAMES, "构建源码出现未声明的文件类型")
                paths.append(path)
    return sorted(paths, key=lambda path: path.relative_to(root).as_posix())


def snapshot_sources(root, destination=None):
    digest = hashlib.sha256()
    if destination is not None:
        for relative in SOURCE_DIRECTORIES:
            (destination / relative).mkdir(parents=True, exist_ok=True)
    for path in source_paths(root):
        relative = path.relative_to(root).as_posix()
        content = read_source(path)
        digest.update(relative.encode("utf-8") + b"\0" + hashlib.sha256(content).digest())
        if destination is not None:
            target = destination / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open("xb") as output:
                output.write(content)
    return digest.hexdigest()


def run_command(arguments, directory, environment, timeout, stage):
    print("正在" + stage, flush=True)
    try:
        with subprocess.Popen(arguments, cwd=directory, env=environment, stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL, start_new_session=True) as process:
            try:
                result = process.wait(timeout=timeout)
            except BaseException:
                # 子进程尚未回收，进程组标识不会被复用；超时和取消同时终止 npm/go 派生进程。
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=10)
                raise
        require(result == 0, f"{stage}失败（退出码 {result}）")
    except subprocess.TimeoutExpired as error:
        raise ReleaseError(stage + "超时") from error
    except OSError as error:
        raise ReleaseError(stage + "无法执行") from error


def build_environment():
    names = ("PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOPROXY",
             "GOSUMDB", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR")
    environment = {name: os.environ[name] for name in names if name in os.environ}
    environment.update({"CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOENV": "off",
                        "GOFLAGS": "-mod=readonly -buildvcs=false", "LC_ALL": "C", "CI": "true"})
    return environment


def required_paths():
    paths = {"admin/index.html", "README.md"} | {"deploy/" + name for name in DEPLOY_FILES}
    paths.update("tools/" + name for name in VERIFICATION_FILES)
    for architecture in ARCHITECTURES:
        prefix = "linux-" + architecture
        paths.update(prefix + "/bin/" + name for name in HOST_PROGRAMS)
        paths.update(prefix + "/computer-image/bin/" + name for name in COMPUTER_PROGRAMS)
        paths.update(prefix + "/computer-image/" + name for name in IMAGE_FILES)
        paths.add(prefix + "/computer-image/desktop-environment.json")
    return paths


def artifact_mode(name):
    return 0o755 if name in binary_paths() else 0o644


def binary_paths():
    paths = {}
    for architecture in ARCHITECTURES:
        prefix = "linux-" + architecture
        for name in HOST_PROGRAMS:
            paths[prefix + "/bin/" + name] = architecture
        for name in COMPUTER_PROGRAMS:
            paths[prefix + "/computer-image/bin/" + name] = architecture
    return paths


def validate_elf(header, architecture):
    require(len(header) >= 64 and header[:7] == b"\x7fELF\x02\x01\x01", "发布程序不是 64 位 Linux ELF")
    kind, machine = struct.unpack_from("<HH", header, 16)
    require(kind in (2, 3) and machine == {"amd64": 62, "arm64": 183}[architecture], "发布程序架构不符")


def copy_artifact(source, target):
    content = read_source(source)
    require(len(content) > 0, "发布文件为空")
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("xb") as output:
        output.write(content)


def compile_artifacts(source, payload):
    environment = build_environment()
    run_command(["npm", "ci", "--no-audit", "--no-fund"], source / "admin", environment, 600, "安装锁定的管理平台依赖")
    run_command(["npm", "run", "build"], source / "admin", environment, 300, "构建管理平台")
    distribution = source / "admin/dist"
    require({path.name for path in distribution.iterdir()} == {"index.html", "assets"}, "管理平台包含未知产物")
    copy_artifact(distribution / "index.html", payload / "admin/index.html")
    require(not (distribution / "assets").is_symlink(), "管理平台资源目录不能为链接")
    assets = sorted((distribution / "assets").iterdir())
    require(0 < len(assets) <= 256, "管理平台资源数量无效")
    for asset in assets:
        name = "admin/assets/" + asset.name
        require(ASSET_NAME.fullmatch(name), "管理平台资源名称无效")
        copy_artifact(asset, payload / name)
    for architecture in ARCHITECTURES:
        environment["GOOS"], environment["GOARCH"] = "linux", architecture
        for name, target_architecture in binary_paths().items():
            if target_architecture != architecture:
                continue
            target = payload / name
            target.parent.mkdir(parents=True, exist_ok=True)
            run_command(["go", "build", "-trimpath", "-o", str(target), "./cmd/" + target.name],
                        source / "server", environment, 300, "编译 " + architecture + " " + target.name)
        for name in IMAGE_FILES:
            copy_artifact(source / "server/deploy/computer" / name,
                          payload / ("linux-" + architecture) / "computer-image" / name)
        copy_artifact(source / ENVIRONMENT_FILE.relative_to(ROOT),
                      payload / ("linux-" + architecture) / "computer-image/desktop-environment.json")
    for name in DEPLOY_FILES:
        copy_artifact(source / "server/deploy" / name, payload / "deploy" / name)
    for name in VERIFICATION_FILES:
        copy_artifact(source / "server/scripts" / name, payload / "tools" / name)
    copy_artifact(source / "server/deploy/RELEASE.md", payload / "README.md")


def payload_manifest(payload, version, source_digest):
    files = {}
    for directory, directories, names in os.walk(payload, followlinks=False):
        require(not any((Path(directory) / name).is_symlink() for name in directories), "发布目录包含链接")
        for name in names:
            path = Path(directory) / name
            relative = path.relative_to(payload).as_posix()
            content = read_source(path)
            if relative in binary_paths():
                validate_elf(content[:64], binary_paths()[relative])
            files[relative] = {"size": len(content), "sha256": hashlib.sha256(content).hexdigest(),
                               "mode": artifact_mode(relative)}
    manifest = {"format_version": 3, "version": release_version(version),
                "platforms": ["linux/" + architecture for architecture in ARCHITECTURES],
                "source_sha256": source_digest, "files": dict(sorted(files.items()))}
    validate_manifest(manifest)
    return manifest


def validate_manifest(manifest):
    require(isinstance(manifest, dict) and set(manifest) == {"format_version", "version", "platforms", "source_sha256", "files"}, "发布清单字段无效")
    require(type(manifest["format_version"]) is int and manifest["format_version"] == 3, "发布清单版本无效")
    release_version(manifest["version"])
    require(manifest["platforms"] == ["linux/" + value for value in ARCHITECTURES], "发布平台集合不完整")
    require(isinstance(manifest["source_sha256"], str) and DIGEST.fullmatch(manifest["source_sha256"]), "源码摘要无效")
    files = manifest["files"]
    require(isinstance(files, dict) and len(files) <= MAX_FILES and required_paths() <= set(files), "发布清单缺少必要组件或超过上限")
    total, admin_total, assets = 0, 0, 0
    for name, entry in files.items():
        require(name in required_paths() or ASSET_NAME.fullmatch(name), "发布清单含未知文件")
        require(isinstance(entry, dict) and set(entry) == {"size", "sha256", "mode"}, "发布文件清单字段无效")
        require(type(entry["size"]) is int and 0 < entry["size"] <= MAX_FILE, "发布文件大小无效")
        require(type(entry["mode"]) is int and entry["mode"] == artifact_mode(name), "发布文件权限无效")
        require(isinstance(entry["sha256"], str) and DIGEST.fullmatch(entry["sha256"]), "发布文件摘要无效")
        total += entry["size"]
        if name.startswith("admin/"):
            limit = 64 << 10 if name == "admin/index.html" else 8 << 20
            require(entry["size"] <= limit, "管理平台单文件超过上限")
            admin_total += entry["size"]
            assets += name != "admin/index.html"
    require(total <= MAX_CONTENT and admin_total <= 16 << 20 and 0 < assets <= 256, "发布总容量或管理平台数量无效")


def encode_manifest(manifest):
    encoded = (json.dumps(manifest, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    require(len(encoded) <= MAX_MANIFEST, "发布清单超过大小上限")
    return encoded


def archive_payload(payload, manifest, target):
    encoded = encode_manifest(manifest)
    with target.open("xb") as output:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.USTAR_FORMAT) as archive:
                for name in ("manifest.json", *manifest["files"]):
                    header = tarfile.TarInfo(name)
                    header.mode = 0o644 if name == "manifest.json" else manifest["files"][name]["mode"]
                    content = encoded if name == "manifest.json" else read_source(payload / name)
                    header.size = len(content)
                    archive.addfile(header, io.BytesIO(content))
        output.flush()
        os.fsync(output.fileno())


def unique_fields(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "发布清单存在重复字段")
        result[key] = value
    return result


def read_exact(stream, size):
    content = stream.read(size)
    require(len(content) == size, "发布包内容被截断")
    return content


def verify_stream(stream):
    manifest = None
    seen = set()
    total = 0
    while True:
        block = read_exact(stream, 512)
        total += len(block)
        if block == bytes(512):
            require(read_exact(stream, 512) == bytes(512), "发布包结束标记无效")
            # USTAR 的尾部只允许一个记录块内的零填充，不接受追加归档或无限压缩填充。
            tail = stream.read(tarfile.RECORDSIZE + 1)
            require(len(tail) <= tarfile.RECORDSIZE and not any(tail), "发布包尾部包含附加内容")
            break
        header = tarfile.TarInfo.frombuf(block, "utf-8", "strict")
        require(header.type == tarfile.REGTYPE and not header.linkname and not header.pax_headers,
                "发布包只允许普通文件，不接受链接或扩展头")
        require(header.uid == 0 and header.gid == 0 and header.uname == "" and header.gname == "" and header.mtime == 0,
                "发布包元数据不符合规范")
        name = header.name
        require(name not in seen and len(seen) <= MAX_FILES and str(PurePosixPath(name)) == name and not name.startswith("/"), "发布包路径重复或无效")
        seen.add(name)
        if manifest is None:
            require(name == "manifest.json" and header.mode == 0o644 and 0 < header.size <= MAX_MANIFEST, "发布包首项不是有效清单")
            content = read_exact(stream, header.size)
            manifest = json.loads(content, object_pairs_hook=unique_fields)
            validate_manifest(manifest)
        else:
            expected = manifest["files"].get(name)
            require(expected is not None and header.size == expected["size"] and header.mode == expected["mode"], "发布文件与清单不符")
            digest, remaining, prefix, index = hashlib.sha256(), header.size, b"", b""
            while remaining:
                chunk = read_exact(stream, min(remaining, CHUNK_SIZE))
                if not prefix:
                    prefix = chunk[:64]
                digest.update(chunk)
                if name == "admin/index.html":
                    index += chunk
                remaining -= len(chunk)
            require(digest.hexdigest() == expected["sha256"], "发布文件摘要不符")
            if name in binary_paths():
                validate_elf(prefix, binary_paths()[name])
            if name == "admin/index.html":
                require(b"/admin/assets/" in index, "管理平台入口不是 /admin/ 构建")
        padding = (-header.size) % 512
        require(not any(read_exact(stream, padding)), "发布文件填充无效")
        total += header.size + padding
        require(total <= MAX_CONTENT + MAX_MANIFEST + (MAX_FILES + 1) * 1024, "发布包展开内容超过上限")
    require(manifest is not None and seen == set(manifest["files"]) | {"manifest.json"}, "发布包未包含清单中的全部文件")
    return manifest


def verify_archive(path, expected_digest):
    require(isinstance(expected_digest, str) and DIGEST.fullmatch(expected_digest), "必须提供受信发布渠道取得的 SHA-256")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as archive:
        before = os.fstat(archive.fileno())
        require(stat.S_ISREG(before.st_mode) and 0 < before.st_size <= MAX_ARCHIVE, "发布包类型或大小无效")
        digest = hashlib.sha256()
        while chunk := archive.read(CHUNK_SIZE):
            digest.update(chunk)
        require(digest.hexdigest() == expected_digest, "发布包整体摘要不符")
        archive.seek(0)
        with gzip.GzipFile(fileobj=archive, mode="rb") as stream:
            manifest = verify_stream(stream)
        require(file_identity(os.fstat(archive.fileno())) == file_identity(before), "发布包核验期间发生变化")
        return manifest


def build_release(root, target, version):
    release_version(version)
    target = target.absolute()
    require(target.parent.is_dir() and not target.exists() and not target.is_symlink(), "发布目标父目录须存在且目标不能已存在")
    with tempfile.TemporaryDirectory(prefix=".tombot-release-", dir=target.parent) as temporary:
        stage = Path(temporary)
        source, payload = stage / "source", stage / "payload"
        source_digest = snapshot_sources(root, source)
        compile_artifacts(source, payload)
        require(snapshot_sources(source) == source_digest, "构建过程修改了已固定的源码输入")
        manifest = payload_manifest(payload, version, source_digest)
        archive = stage / "release.tar.gz"
        archive_payload(payload, manifest, archive)
        digest = hashlib.sha256()
        with archive.open("rb") as content:
            while chunk := content.read(CHUNK_SIZE):
                digest.update(chunk)
        digest = digest.hexdigest()
        verify_archive(archive, digest)
        archive.chmod(0o644)
        # 同文件系统硬链接提供不可覆盖的原子发布；构建或校验失败时只留下临时目录，不暴露半份成品。
        os.link(archive, target)
        descriptor = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        return {"scope": "release_artifacts", "version": version, "sha256": digest,
                "source_sha256": source_digest, "file_count": len(manifest["files"])}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build", help="从当前工作树快照构建完整双架构发布包")
    build.add_argument("--version", required=True)
    build.add_argument("--output", type=Path, required=True)
    verify = commands.add_parser("verify", help="只读校验，不解包、不安装或启动服务")
    verify.add_argument("--archive", type=Path, required=True)
    verify.add_argument("--sha256", required=True)
    arguments = parser.parse_args()
    try:
        if arguments.command == "build":
            report = build_release(ROOT, arguments.output, arguments.version)
        else:
            manifest = verify_archive(arguments.archive, arguments.sha256)
            report = {"scope": "release_artifacts", "version": manifest["version"], "file_count": len(manifest["files"])}
        print(json.dumps(report, ensure_ascii=False, sort_keys=True))
        return 0
    except (ReleaseError, OSError, ValueError, tarfile.TarError, EOFError) as error:
        message = str(error) if isinstance(error, ReleaseError) else "发布构建或核验失败，输入、归档或文件系统无效"
        print(message, file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("发布构建或核验已取消", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
