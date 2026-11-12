#!/usr/bin/python3
"""以已降权的 Bot 身份监督一个固定尺寸的 XFCE/X11 桌面。"""

import argparse
import fcntl
import json
import os
from pathlib import Path
import secrets
import signal
import stat
import subprocess
import sys
import time


WIDTH, HEIGHT = 1280, 800
START_TIMEOUT, STOP_TIMEOUT = 30, 5
ENVIRONMENT_FILE = Path("/usr/share/tombot/desktop-environment.json")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def private_directory(path):
    """拒绝链接或其他身份控制的目录，避免认证文件落入共享工作区。"""
    require(path.is_absolute() and path.resolve() == path, "桌面运行目录必须是无链接绝对路径")
    identity = path.lstat()
    require(stat.S_ISDIR(identity.st_mode) and identity.st_uid == os.getuid() and identity.st_gid == os.getgid()
            and stat.S_IMODE(identity.st_mode) == 0o700, "桌面运行目录所有者或权限错误")


def write_private(path, contents):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
        output.write(contents)


def write_ready(runtime, display):
    """原子发布完整就绪对象，避免代理观察到创建后尚未写完的 JSON。"""
    pending = runtime / "ready.pending"
    write_private(pending, json.dumps({"display": display, "width": WIDTH, "height": HEIGHT, "protocol": "x11"}))
    os.replace(pending, runtime / "ready.json")


def session_environment(runtime, display):
    """消费代理已准备的身份和目录，终端与 XFCE 不能各自覆盖同一会话的状态。"""
    uid, gid = os.getuid(), os.getgid()
    name = f"bot{uid}"
    for filename, expected in (("passwd", f"{name}:x:{uid}:{gid}:Bot:/workspace:/bin/bash\n"),
                               ("group", f"bot:x:{gid}:\n")):
        descriptor = os.open(runtime / filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(descriptor, "r", encoding="utf-8") as source:
            identity = os.fstat(source.fileno())
            require(stat.S_ISREG(identity.st_mode) and identity.st_uid == uid and identity.st_gid == gid
                    and stat.S_IMODE(identity.st_mode) == 0o600 and identity.st_nlink == 1,
                    "桌面身份文件所有者、类型或权限无效")
            require(source.read(1024) == expected, "桌面身份文件与当前 Bot 不符")
    for directory in ("config", "cache", "state", "data", "tmp"):
        private_directory(runtime / directory)
    try:
        environment = json.loads(ENVIRONMENT_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise RuntimeError("桌面环境模板不可用") from None
    require(isinstance(environment, dict) and environment
            and all(isinstance(name, str) and isinstance(value, str) for name, value in environment.items()),
            "桌面环境模板格式无效")
    replacements = {"{{uid}}": str(uid), "{{display}}": str(display), "{{runtime}}": str(runtime)}
    for name, value in environment.items():
        for placeholder, replacement in replacements.items():
            value = value.replace(placeholder, replacement)
        require("{{" not in value, "桌面环境模板包含未知占位符")
        environment[name] = value
    return environment


def xserver_arguments(runtime, display):
    return [
        "/usr/bin/Xtigervnc", f":{display}", "-geometry", f"{WIDTH}x{HEIGHT}", "-depth", "24",
        "-auth", str(runtime / "Xauthority"), "-nolisten", "tcp", "-rfbport", "-1",
        "-rfbunixpath", str(runtime / "desktop.sock"), "-rfbunixmode", "0600",
        # RFB 只在 0700 目录内的 0600 socket 上提供服务，身份校验由文件权限和上游网关承担。
        "-SecurityTypes", "None", "-AlwaysShared", "-AcceptSetDesktopSize=0",
        "-AllowOverride", "", "-desktop", "Bot",
    ]


class Desktop:
    """监督核心进程；异常不重放动作，最终后代回收仍由节点的桌面 cgroup 保证。"""

    def __init__(self, runtime, display):
        self.runtime, self.display = runtime, display
        self.processes = []
        self.stopping = False

    def start(self, name, arguments, environment):
        with (self.runtime / f"{name}.log").open("xb") as output:
            process = subprocess.Popen(arguments, env=environment, cwd="/workspace", stdin=subprocess.DEVNULL,
                                       stdout=output, stderr=output, start_new_session=True)
        self.processes.append((name, process))

    def check(self):
        require(not self.stopping, "桌面启动已取消")
        for name, process in self.processes:
            require(process.poll() is None, f"桌面核心进程退出：{name}")

    def wait_for(self, check, stage):
        deadline = time.monotonic() + START_TIMEOUT
        while time.monotonic() < deadline:
            self.check()
            if check():
                return
            time.sleep(0.1)
        raise RuntimeError(f"桌面{stage}启动超过时限")

    def run(self):
        # 编译器需要执行临时产物；业务临时文件共享，控制目录仍保持不可执行和私有。
        Path("/workspace/.tmp").mkdir(mode=0o770, exist_ok=True)
        environment = session_environment(self.runtime, self.display)
        write_private(self.runtime / "environment.json", json.dumps(environment))
        # 随机认证材料只经标准输入交给 xauth，不能进入命令行或日志。
        write_private(self.runtime / "Xauthority", "")
        subprocess.run(["/usr/bin/xauth", "-f", environment["XAUTHORITY"]],
                       input=f"add :{self.display} . {secrets.token_hex(16)}\n", text=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, check=True)
        self.start("x11", xserver_arguments(self.runtime, self.display), environment)

        def display_ready():
            result = subprocess.run(["/usr/bin/xdpyinfo"], env=environment, capture_output=True, timeout=2)
            if result.returncode != 0:
                return False
            require(f"{WIDTH}x{HEIGHT} pixels".encode() in result.stdout, "桌面分辨率不符合要求")
            return True

        self.wait_for(display_ready, " X11 ")
        self.start("dbus", ["/usr/bin/dbus-daemon", "--session", "--nofork", "--nopidfile",
                            f"--address={environment['DBUS_SESSION_BUS_ADDRESS']}"], environment)
        self.wait_for(lambda: (self.runtime / "bus").is_socket(), "会话总线")
        self.start("xfce", ["/usr/bin/xfce4-session"], environment)

        def window_manager_ready():
            result = subprocess.run(["/usr/bin/xprop", "-root", "_NET_SUPPORTING_WM_CHECK"],
                                    env=environment, capture_output=True, text=True, timeout=2)
            if result.returncode != 0 or "window id # 0x" not in result.stdout:
                return False
            window = result.stdout.split("window id # ", 1)[1].strip()
            result = subprocess.run(["/usr/bin/xprop", "-id", window, "_NET_WM_NAME"],
                                    env=environment, capture_output=True, text=True, timeout=2)
            require(result.returncode == 0 and '"Xfwm4"' in result.stdout, "窗口管理器不是 xfwm4")
            return True

        self.wait_for(window_manager_ready, "窗口管理器")

        def components_ready():
            for name in ("xfce4-panel", "xfdesktop"):
                result = subprocess.run(["/usr/bin/pgrep", "-u", str(os.getuid()), "-x", name],
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=2)
                require(result.returncode in (0, 1), "检查桌面组件失败")
                if result.returncode != 0:
                    return False
            return True

        self.wait_for(components_ready, "面板与文件视图")
        identity = (self.runtime / "desktop.sock").lstat()
        require(stat.S_ISSOCK(identity.st_mode) and identity.st_uid == os.getuid()
                and stat.S_IMODE(identity.st_mode) == 0o600, "远程桌面套接字权限错误")
        write_ready(self.runtime, self.display)
        print(f"桌面已就绪：身份 {os.getuid()}，显示 {self.display}，{WIDTH}×{HEIGHT}", flush=True)
        while not self.stopping:
            self.check()
            time.sleep(0.2)

    def stop(self):
        ready = self.runtime / "ready.json"
        ready.unlink(missing_ok=True)
        for _, process in reversed(self.processes):
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass  # 进程组已经结束，仍须 wait 回收直接子进程。
        deadline = time.monotonic() + STOP_TIMEOUT
        for _, process in reversed(self.processes):
            try:
                process.wait(timeout=max(0.01, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=STOP_TIMEOUT)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", type=Path, required=True)
    parser.add_argument("--display", type=int, required=True)
    arguments = parser.parse_args()
    require(os.getuid() == 100000 + arguments.display and os.getgid() == 20000, "图形桌面必须使用已降权的独立 Bot 身份")
    status = dict(line.split(":", 1) for line in Path("/proc/self/status").read_text().splitlines() if ":" in line)
    require(all(int(status[name].strip(), 16) == 0 for name in ("CapPrm", "CapEff", "CapAmb"))
            and status["NoNewPrivs"].strip() == "1" and status["Seccomp"].strip() == "2",
            "图形桌面权限未清除或缺少运行限制")
    require(set(os.getgroups()).issubset({os.getgid()}), "图形桌面包含未授权的附加组")
    require(1 <= arguments.display <= 4095, "显示编号超出范围")
    require(str(arguments.runtime).startswith("/run/bot-desktops/")
            and len(str(arguments.runtime)) <= 75, "桌面运行目录必须位于受管区域且长度受限")
    private_directory(arguments.runtime)
    parent = Path("/run/bot-desktops").lstat()
    require(stat.S_ISDIR(parent.st_mode) and parent.st_uid == 0 and parent.st_gid == 0
            and stat.S_IMODE(parent.st_mode) == 0o711, "桌面父目录必须由控制程序管理")
    os.umask(0o007)  # 新业务文件允许同一用户的共享组读取和修改。
    lock = os.open(arguments.runtime / "session.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock, "w"):
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        desktop = Desktop(arguments.runtime, arguments.display)
        for event in (signal.SIGTERM, signal.SIGINT):
            signal.signal(event, lambda *_: setattr(desktop, "stopping", True))
        try:
            desktop.run()
        finally:
            desktop.stop()


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        reason = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(f"桌面运行失败：{reason}", file=sys.stderr)
        sys.exit(1)
