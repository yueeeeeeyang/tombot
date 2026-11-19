#!/usr/bin/python3 -I
"""绑定当前桌面打开有头 Chrome；用户操作与模型观察均走原有桌面流。"""

import os
from pathlib import Path
import re
import signal
import socket
import struct
import subprocess
from urllib.parse import urlsplit
import sys
import time

ROOT = Path("/run/tombot-browser")
PROFILES = Path("/var/lib/tombot-browser")
TIMEOUT = 20
BINARY = "/opt/google/chrome/chrome"
IDENTIFIER = re.compile(r"[0-7][0-9A-HJKMNP-TV-Z]{25}")


def peer(connection):
    return struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))


def web_address(value):
    """桌面链接只能传递一个网页地址，不能变成 Chrome 的选项或本地程序指令。"""
    if len(value.encode("utf-8")) > 4096 or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise ValueError("网页地址无效")
    address = urlsplit(value)
    if value and (address.scheme not in ("http", "https") or not address.hostname):
        raise ValueError("仅允许 HTTP 或 HTTPS 地址")
    return value


def open_desktop(url=""):
    """客户端只选择已授权会话的入口；认证材料只在本机私有连接内传递。"""
    runtime = Path(os.environ["XDG_RUNTIME_DIR"])
    display = int(os.environ["DISPLAY"].removeprefix(":"))
    if runtime.parent != Path("/run/bot-desktops") or not IDENTIFIER.fullmatch(runtime.name) or os.getuid() != 100000 + display:
        raise ValueError("桌面身份无效")
    with (runtime / "Xauthority").open("rb") as source:
        authority = source.read(4097)
    if not 0 < len(authority) <= 4096:
        raise ValueError("桌面认证无效")
    with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
        connection.settimeout(TIMEOUT + 2)
        address = str(ROOT / runtime.name / "open.sock")
        deadline = time.monotonic() + 2
        while True:
            try:
                connection.connect(address)
                break
            except (FileNotFoundError, ConnectionRefusedError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.05)
        if peer(connection)[1] != 200000 + display:
            raise PermissionError("浏览器启动入口身份不符")
        connection.sendall(struct.pack("!H", len(authority)) + authority + web_address(url).encode("utf-8"))
        if connection.recv(16) != b"ready":
            raise RuntimeError("浏览器窗口未就绪")


def activate(process, environment):
    """只有本桌面上属于该 Chrome 主进程的可见窗口才构成启动成功的回执。"""
    deadline = time.monotonic() + TIMEOUT
    while process.poll() is None and time.monotonic() < deadline:
        found = subprocess.run(["/usr/bin/xdotool", "search", "--all", "--onlyvisible", "--pid", str(process.pid),
                                "--class", "google-chrome"], env=environment,
                               capture_output=True, timeout=2)
        if found.returncode == 0:
            window = found.stdout.splitlines()[-1]
            subprocess.run(["/usr/bin/xdotool", "windowactivate", window], env=environment,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=2)
            return
        if found.returncode != 1:
            raise RuntimeError("无法查询浏览器窗口")
        time.sleep(0.1)
    raise RuntimeError("Chrome 退出或窗口未就绪")


def finish_chrome(process, environment):
    """向 Chrome 窗口发送窗口管理器的正常关闭请求，使刚写入的 Cookie 随配置一并提交。"""
    from Xlib import X, Xatom, display
    from Xlib.protocol.event import ClientMessage

    os.environ["XAUTHORITY"] = environment["XAUTHORITY"]
    screen = display.Display(environment["DISPLAY"])
    try:
        root = screen.screen().root
        windows = root.get_full_property(screen.intern_atom("_NET_CLIENT_LIST"), Xatom.WINDOW)
        if windows:
            for identifier in windows.value:
                window = screen.create_resource_object("window", int(identifier))
                owner = window.get_full_property(screen.intern_atom("_NET_WM_PID"), Xatom.CARDINAL)
                if owner and len(owner.value) == 1 and owner.value[0] == process.pid:
                    event = ClientMessage(window=window, client_type=screen.intern_atom("WM_PROTOCOLS"),
                                          data=(32, [screen.intern_atom("WM_DELETE_WINDOW"), X.CurrentTime, 0, 0, 0]))
                    window.send_event(event)
        screen.flush()
    finally:
        screen.close()
    process.wait(timeout=2)


def serve(session, desktop, display):
    """只在所属桌面 cgroup 内执行。普通关闭或浏览器崩溃不会终止桌面，下一次打开可以重新启动。"""
    if not IDENTIFIER.fullmatch(session) or not IDENTIFIER.fullmatch(desktop) or not 1 <= display <= 4095 or os.getuid() != 200000 + display:
        raise PermissionError("浏览器桌面身份不符")
    runtime, profile = ROOT / session, PROFILES / desktop
    private = runtime / "private"
    private.mkdir(mode=0o700)
    environment = {"PATH": "/usr/bin:/bin", "DISPLAY": f":{display}", "LANG": "zh_CN.UTF-8",
                   "HOME": str(profile), "XDG_RUNTIME_DIR": str(private), "TMPDIR": str(private),
                   "XAUTHORITY": str(private / "Xauthority")}
    arguments = [BINARY, "--no-first-run", "--no-default-browser-check", "--disable-background-mode",
                 "--disable-background-networking", "--disable-component-update", "--disable-quic",
                 "--proxy-server=http://127.0.0.1:3128", "--proxy-bypass-list=<-loopback>",
                 "--user-data-dir=" + str(profile)]
    options = dict(env=environment, cwd=profile, stdin=subprocess.DEVNULL,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    process = None
    def stop(_signal, _frame):
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, stop)
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as listener:
            address = runtime / "open.sock"
            listener.bind(str(address))
            address.chmod(0o660)
            listener.listen(4)
            while True:
                # 关闭最后一个窗口后及时回收 Chrome，空闲桌面可一直阻塞等待下一次打开。
                if process is not None and process.poll() is not None:
                    process = None
                listener.settimeout(1 if process is not None else None)
                try:
                    connection, _ = listener.accept()
                except socket.timeout:
                    continue
                with connection:
                    connection.settimeout(2)
                    try:
                        if peer(connection)[1:] != (100000 + display, 20000):
                            raise PermissionError("请求不属于当前桌面")
                        packet = connection.recv(8195)
                        length = int.from_bytes(packet[:2], "big")
                        if not 0 < length <= 4096 or not 2 + length <= len(packet) <= 8194:
                            raise ValueError("认证长度无效")
                        authority = packet[2:2 + length]
                        url = web_address(packet[2 + length:].decode("utf-8"))
                        if process is None or process.poll() is not None:
                            with (private / "Xauthority").open("wb") as target:
                                target.write(authority)
                            process = subprocess.Popen([*arguments, url or "about:blank"], **options)
                        elif url:
                            subprocess.run([*arguments, url], **options, check=True, timeout=5)
                        activate(process, environment)
                        response = b"ready"
                    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
                        print(f"桌面浏览器启动失败 desktop_id={desktop} 原因={type(error).__name__}", file=sys.stderr, flush=True)
                        response = b"failed"
                    try:
                        connection.sendall(response)
                    except OSError:
                        print(f"浏览器打开回执未送达 desktop_id={desktop}", file=sys.stderr, flush=True)
    finally:
        if process is not None and process.poll() is None:
            finish_chrome(process, environment)


if __name__ == "__main__":
    try:
        if len(sys.argv) == 1:
            open_desktop()
        elif len(sys.argv) == 2 and sys.argv[1] == "--version":
            os.execl("/usr/bin/google-chrome", "google-chrome", "--version")
        elif len(sys.argv) == 2:
            open_desktop(web_address(sys.argv[1]))
        elif len(sys.argv) == 5 and sys.argv[1] == "serve":
            serve(sys.argv[2], sys.argv[3], int(sys.argv[4]))
        else:
            raise ValueError("启动参数无效")
    except (KeyError, OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print("打开当前桌面浏览器失败：" + type(error).__name__, file=sys.stderr)
        sys.exit(1)
