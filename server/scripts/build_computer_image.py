#!/usr/bin/env python3
"""在独立构建上下文中编译电脑程序并制作 Debian 13 专用镜像。"""

import argparse
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
ENVIRONMENT_FILE = ROOT / "internal/computer/desktop-environment.json"
IMAGE_FILES = ("Dockerfile", "desktop.py", "prepare.py", "xfce4-session.xml", "xfwm4.xml", "xfce4-desktop.xml",
               "install_tools.py", "tools.lock.json", "verify_tools.py",
               "seccomp.json", "seccomp-source.json", "seccomp.LICENSE")


def image_tag(value):
    if not re.fullmatch(r"[a-z0-9][a-z0-9._/-]*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", value):
        raise argparse.ArgumentTypeError("必须提供明确版本的本地镜像名称")
    return value


def build(tag, architecture):
    environment = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": architecture}
    with tempfile.TemporaryDirectory(prefix="tombot-computer-build-") as directory:
        context = Path(directory)
        (context / "bin").mkdir()
        for name in IMAGE_FILES:
            (context / name).parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / "deploy" / "computer" / name, context / name)
        shutil.copyfile(ENVIRONMENT_FILE, context / "desktop-environment.json")
        for name in ("computer-agent", "computer-task", "computer-probe", "bot-agent"):
            subprocess.run(["go", "build", "-trimpath", "-o", str(context / "bin" / name), f"./cmd/{name}"],
                           cwd=ROOT, env=environment, check=True, timeout=300)
        subprocess.run(["docker", "buildx", "build", "--load", "--platform", f"linux/{architecture}",
                        "--tag", tag, str(context)], check=True, timeout=1800)
    subprocess.run(["docker", "image", "inspect", "--format", "{{.Id}} {{.Architecture}}", tag],
                   check=True, timeout=30)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", type=image_tag, default="tombot-computer:trixie")
    parser.add_argument("--architecture", choices=("amd64", "arm64"),
                        default="arm64" if platform.machine() in ("arm64", "aarch64") else "amd64")
    arguments = parser.parse_args()
    build(arguments.tag, arguments.architecture)
