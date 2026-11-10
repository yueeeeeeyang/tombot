#!/usr/bin/env python3
"""使用同一个部署域名配置 iOS 签名能力和 Flutter 路由，临时配置随构建销毁。"""
import argparse
import os
from pathlib import Path
import re
import subprocess
import tempfile


def domain(value):
    """只接收小写 DNS 标签，防止额外路径或 Xcode 配置混入签名。"""
    labels = value.split(".")
    if (len(value) > 253 or len(labels) < 2 or
            any(not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in labels)):
        raise argparse.ArgumentTypeError("请填写不含协议、路径和端口的小写域名")
    return value


def build(host, mode, codesign):
    """使用原生构建入口，保留失败退出状态；不修改用户已有的全局 Xcode 配置。"""
    if os.environ.get("XCODE_XCCONFIG_FILE"):
        raise SystemExit("请在没有全局 XCODE_XCCONFIG_FILE 的环境中构建，以免覆盖已有签名配置")
    with tempfile.TemporaryDirectory(prefix="tom-links-") as directory:
        settings = Path(directory) / "Links.xcconfig"
        settings.write_text(f"TOM_LINK_HOST = {host}\nTOM_LINK_ENTITLEMENTS = Runner/AssociatedDomains.entitlements\n",
                            encoding="utf-8")
        command = ["flutter", "build", "ios", f"--{mode}", f"--dart-define=TOM_LINK_HOST={host}"]
        if not codesign:
            command.append("--no-codesign")
        subprocess.run(command, cwd=Path(__file__).resolve().parents[1] / "client",
                       env={**os.environ, "XCODE_XCCONFIG_FILE": str(settings)}, check=True, timeout=1200)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", type=domain)
    parser.add_argument("--mode", choices=("release", "debug", "profile"), default="release")
    parser.add_argument("--no-codesign", action="store_true")
    args = parser.parse_args()
    build(args.host, args.mode, not args.no_codesign)
