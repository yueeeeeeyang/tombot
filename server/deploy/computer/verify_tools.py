#!/usr/bin/python3
"""验证镜像必装工具及版本，并输出可追溯的工具清单。"""

import json
import argparse
from pathlib import Path
import re
import shutil
import subprocess


COMMANDS = {
    "python": ["python3", "--version"], "pip": ["pip", "--version"], "uv": ["uv", "--version"],
    "node": ["node", "--version"], "npm": ["npm", "--version"], "pnpm": ["pnpm", "--version"],
    "bun": ["bun", "--version"], "go": ["go", "version"], "rustc": ["rustc", "--version"],
    "cargo": ["cargo", "--version"], "perl": ["perl", "-e", "print $^V"], "gcc": ["gcc", "--version"],
    "git": ["git", "--version"], "gh": ["gh", "--version"], "curl": ["curl", "--version"],
    "wget": ["wget", "--version"], "jq": ["jq", "--version"], "ripgrep": ["rg", "--version"],
    "tmux": ["tmux", "-V"], "ssh": ["ssh", "-V"], "openssl": ["openssl", "version"],
    "ffmpeg": ["ffmpeg", "-version"], "pdftotext": ["pdftotext", "-v"], "psql": ["psql", "--version"],
    "chrome": ["google-chrome", "--version"], "libreoffice": ["libreoffice", "--headless", "--version"],
}
GUI_COMMANDS = {"xfce": "xfce4-session", "xfwm4": "xfwm4", "terminal": "xfce4-terminal", "thunar": "thunar"}


def verify(desktop=False):
    versions = {}
    commands = dict(COMMANDS)
    for name, executable in GUI_COMMANDS.items():
        if shutil.which(executable) is None:
            raise RuntimeError(f"镜像缺少桌面程序：{executable}")
        # XFCE 的版本入口也会连接 X11；构建阶段查安装数据库，图形验收阶段执行真实程序。
        commands[name] = ([executable, "--version"] if desktop else
                          ["dpkg-query", "-W", "-f=${Package} ${Version}\n", executable])
    for name, command in commands.items():
        completed = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   timeout=30, check=True)
        output = completed.stdout.strip() or completed.stderr.strip()
        if not output:
            raise RuntimeError(f"镜像工具未返回版本：{name}")
        versions[name] = output.splitlines()[0]
    for command in ("scp", "lowriter", "localc", "loimpress", "lodraw"):
        if shutil.which(command) is None:
            raise RuntimeError(f"镜像缺少必装命令：{command}")
    for name, pattern in {"python": r"Python 3\.13\.", "node": r"v24\.", "go": r"go version go1\.27\."}.items():
        if not re.match(pattern, versions[name]):
            raise RuntimeError(f"镜像工具版本不符合要求：{name}")
    lock = json.loads(Path("/usr/share/tombot/tools.lock.json").read_text())
    for name in lock:
        expected = lock[name]["version"].removesuffix("-1")
        if not re.search(r"(?<![\d.])" + re.escape(expected) + r"(?![\d.])", versions[name]):
            raise RuntimeError(f"镜像工具版本与安装清单不一致：{name}")
    return versions


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--desktop", action="store_true", help="在已启动的桌面中执行图形软件版本检查")
    print(json.dumps(verify(parser.parse_args().desktop), ensure_ascii=False, indent=2))
