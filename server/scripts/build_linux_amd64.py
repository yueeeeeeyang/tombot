#!/usr/bin/env python3
"""从同一份当前源码快照打包 Linux x64 服务端程序和节点离线安装包。"""

import argparse
import json
import os
from pathlib import Path
import sys
import tempfile
from uuid import uuid4

from release_bundle import (
    ROOT, ReleaseError, build_environment, compile_artifacts, release_version,
    require, run_command, snapshot_sources, validate_elf,
)


ARCHITECTURE = "amd64"
PLATFORM = "linux/amd64"
NODE_ARCHIVE = "tomnode-linux-amd64-offline.tar.gz"
DOCKER_ENVIRONMENT = ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY",
                      "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "BUILDX_CONFIG", "BUILDX_BUILDER")


def build(root, output, version):
    """以指定版本和新输出目录构建两件成品，失败时抛出异常。"""
    release_version(version)
    output = output.absolute()
    require(output.parent.is_dir() and not os.path.lexists(output), "输出目录的父目录须存在且输出目录须为新目录")
    environment = build_environment()
    environment.update({name: os.environ[name] for name in DOCKER_ENVIRONMENT if name in os.environ})
    environment["PYTHONDONTWRITEBYTECODE"] = "1"
    # 每次使用独立标签，避免并行打包时核对和导出的镜像被另一轮构建替换。
    image = "tombot-computer:release-amd64-" + uuid4().hex
    with tempfile.TemporaryDirectory(prefix=".tombot-linux-amd64-", dir=output.parent) as temporary:
        stage = Path(temporary)
        source, artifacts, products = stage / "source", stage / "artifacts", stage / "products"
        source_digest = snapshot_sources(root, source)
        compile_artifacts(source, artifacts, version, architectures=(ARCHITECTURE,))
        platform_artifacts = artifacts / "linux-amd64"
        server = source / "server"
        binary = platform_artifacts / "bin/tomd"
        with binary.open("rb") as executable:
            validate_elf(executable.read(64), ARCHITECTURE)
        products.mkdir(mode=0o700)
        binary.chmod(0o755)
        binary.rename(products / "tomd")
        run_command(["docker", "buildx", "build", "--load", "--platform", PLATFORM, "--tag", image,
                     str(platform_artifacts / "computer-image")], server, environment, 1800, "构建 Linux x64 电脑镜像")
        # 现有离线包构建器重新核对镜像内代理、文件散列及安装包格式。
        run_command([sys.executable, "scripts/node_release.py", "build", "--image", image,
                     "--architecture", ARCHITECTURE, "--runtime", "deploy/runtime/linux.json",
                     "--label", version, "--output", str(products / NODE_ARCHIVE)], server,
                    environment, 3600, "打包并核验节点离线安装包")
        require((products / NODE_ARCHIVE).stat().st_size > 0, "节点安装包为空")
        require(snapshot_sources(source) == source_digest, "构建过程修改了已固定的源码输入")
        # 独占目录名后一次发布两件已核验的成品，避免覆盖并发构建的结果。
        output.mkdir(mode=0o700)
        try:
            products.rename(output)
        except OSError:
            output.rmdir()
            raise
    return {"platform": PLATFORM, "version": version, "source_sha256": source_digest,
            "tomd": str(output / "tomd"), "node_package": str(output / NODE_ARCHIVE), "image": image}


def main():
    """解析打包参数，成功时输出成品路径及源码摘要。"""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="写入 tomd 的发布版本")
    parser.add_argument("--output-dir", type=Path, required=True, help="成品目录，父目录必须已存在")
    arguments = parser.parse_args()
    try:
        report = build(ROOT, arguments.output_dir, arguments.version)
    except (ReleaseError, ValueError, OSError) as error:
        message = str(error) if isinstance(error, (ReleaseError, ValueError)) else "Linux x64 打包失败，产物或输出文件系统无效"
        print(message, file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("Linux x64 打包已取消", file=sys.stderr)
        return 130
    print(json.dumps(report, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
