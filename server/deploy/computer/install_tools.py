#!/usr/bin/python3
"""仅在镜像构建期间安装已固定版本并验证摘要的官方工具。"""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
from urllib.parse import urlsplit
import zipfile


HOSTS = {"node": "nodejs.org", "go": "go.dev", "uv": "github.com",
         "bun": "github.com", "chrome": "dl.google.com", "pnpm": "github.com"}


def validate_archive(name, record):
    address = urlsplit(record["url"])
    if address.scheme != "https" or address.hostname != HOSTS[name] or address.username or address.password:
        raise ValueError("工具下载地址不是已确认的官方 HTTPS 来源")
    if not re.fullmatch(r"[a-f0-9]{64}", record["sha256"]):
        raise ValueError("工具摘要格式错误")


def verify_digest(path, expected):
    digest = hashlib.sha256()
    with path.open("rb") as archive:
        while chunk := archive.read(1024 * 1024):
            digest.update(chunk)
    if digest.hexdigest() != expected:
        raise ValueError("工具归档摘要不匹配")


def install():
    lock = json.loads(Path("/usr/share/tombot/tools.lock.json").read_text())
    architecture = subprocess.check_output(["dpkg", "--print-architecture"], text=True, timeout=5).strip()
    if architecture not in ("amd64", "arm64"):
        raise ValueError("电脑镜像架构不受支持")
    with tempfile.TemporaryDirectory(prefix="computer-tools-") as directory:
        root = Path(directory)
        root.chmod(0o755)
        for name in HOSTS:
            record = lock[name][architecture]
            validate_archive(name, record)
            archive = root / Path(urlsplit(record["url"]).path).name
            print(f"正在安装 {name} {lock[name]['version']}", flush=True)
            subprocess.run(["curl", "--fail", "--location", "--proto", "=https", "--proto-redir", "=https",
                            "--connect-timeout", "15", "--max-time", "300", "--retry", "2",
                            "--retry-delay", "2", "--output", str(archive), record["url"]],
                           timeout=930, check=True)
            verify_digest(archive, record["sha256"])
            if name == "chrome":
                subprocess.run(["apt-get", "-o", "Acquire::Retries=3", "-o", "Acquire::http::Timeout=30",
                                "install", "-y", "--no-install-recommends", str(archive)],
                               env={**os.environ, "DEBIAN_FRONTEND": "noninteractive"}, timeout=600, check=True)
                continue
            destination = root / name
            destination.mkdir()
            if name == "bun":
                with zipfile.ZipFile(archive) as package:
                    entries = [entry for entry in package.infolist() if entry.filename.endswith("/bun")]
                    if len(entries) != 1:
                        raise ValueError("bun 归档结构不符")
                    with package.open(entries[0]) as source, Path("/usr/local/bin/bun").open("xb") as output:
                        shutil.copyfileobj(source, output)
                Path("/usr/local/bin/bun").chmod(0o755)
                Path("/usr/local/bin/bunx").symlink_to("bun")
            else:
                with tarfile.open(archive) as package:
                    package.extractall(destination, filter="data")
                if name == "pnpm":
                    executables = [path for path in destination.rglob("pnpm") if path.is_file() and not path.is_symlink()]
                    if len(executables) != 1:
                        raise ValueError("pnpm 归档结构不符")
                    executable = executables[0].relative_to(destination)
                    shutil.move(str(destination), "/usr/local/pnpm")
                    Path("/usr/local/bin/pnpm").symlink_to(Path("/usr/local/pnpm") / executable)
                    continue
                children = list(destination.iterdir())
                if len(children) != 1 or not children[0].is_dir():
                    raise ValueError("工具归档结构不符")
                if name in ("node", "go"):
                    shutil.move(str(children[0]), f"/usr/local/{name}")
                else:
                    for executable in ("uv", "uvx"):
                        shutil.copy2(children[0] / executable, f"/usr/local/bin/{executable}")


if __name__ == "__main__":
    install()
