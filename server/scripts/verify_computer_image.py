#!/usr/bin/env python3
"""使用全新受限容器验收工具、共享文件及两个独立的 1280×800 桌面。"""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import json
import hashlib
import os
from pathlib import Path
import shutil
import select
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time
import uuid


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def wait_for(check, message, seconds=40):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.1)
    raise TimeoutError(message)


def receive(connection, count):
    received = bytearray()
    while len(received) < count:
        chunk = connection.recv(count - len(received))
        require(bool(chunk), "远程桌面意外关闭连接")
        received.extend(chunk)
    return bytes(received)


def remote_desktop(path):
    """使用真实 RFB 握手验证对外尺寸；仅连接本次新建桌面的私有 socket。"""
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(5)
    try:
        connection.connect(str(path))
        require(receive(connection, 12) == b"RFB 003.008\n", "桌面协议版本不符")
        connection.sendall(b"RFB 003.008\n")
        count = receive(connection, 1)[0]
        require(count > 0 and 1 in receive(connection, count), "本地桌面握手失败")
        connection.sendall(b"\x01")
        require(receive(connection, 4) == bytes(4), "本地桌面认证失败")
        connection.sendall(b"\x01")
        header = receive(connection, 24)
        require(struct.unpack("!HH", header[:4]) == (1280, 800), "对外桌面不是 1280×800")
        length = struct.unpack("!I", header[20:24])[0]
        require(length <= 1024 and receive(connection, length) == b"Bot", "桌面名称不符")
        return connection
    except BaseException:
        connection.close()
        raise


class BotDesktop:
    def __init__(self, number, session=None):
        self.number = number
        self.uid, self.gid = 100000 + number, 20000
        self.runtime = Path("/run/bot-desktops") / f"{session or number:026d}"
        self.desktop_id = f"{100 + number:026d}"
        self.runtime.mkdir(mode=0o700)
        os.chown(self.runtime, self.uid, self.gid)
        # 镜像验收夹具模拟代理已准备的会话；Go 资源组集成测试另行验证正式目录创建与回收。
        for name, content in (("passwd", f"bot{self.uid}:x:{self.uid}:{self.gid}:Bot:/workspace:/bin/bash\n"),
                              ("group", f"bot:x:{self.gid}:\n")):
            path = self.runtime / name
            path.write_text(content)
            path.chmod(0o600)
            os.chown(path, self.uid, self.gid)
        for name in ("config", "cache", "state", "data", "tmp"):
            path = self.runtime / name
            path.mkdir(mode=0o700)
            os.chown(path, self.uid, self.gid)
        self.process = None
        self.environment = None
        self.applications = []

    def start(self):
        self.process = subprocess.Popen(["python3", "/usr/lib/tombot/desktop.py", "--runtime", str(self.runtime),
                                         "--display", str(self.number)], user=self.uid, group=self.gid,
                                        extra_groups=[], start_new_session=True)

    def ready(self):
        require(self.process.poll() is None, f"桌面 {self.number} 提前退出")
        return (self.runtime / "ready.json").is_file()

    def command(self, arguments, check=True):
        return subprocess.run(arguments, env=self.environment, cwd="/workspace", user=self.uid, group=self.gid,
                              extra_groups=[], capture_output=True, text=True, timeout=45, check=check)

    def application(self, arguments):
        process = subprocess.Popen(arguments, env=self.environment, cwd="/workspace", user=self.uid,
                                   group=self.gid, extra_groups=[], stdout=subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL, start_new_session=True)
        self.applications.append(process)
        return process

    def stop(self):
        if self.process is not None:
            if self.process.poll() is None:
                self.process.terminate()
            self.process.wait(timeout=20)
        for process in self.applications:
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=10)


def verify_programs(bot):
    versions = json.loads(bot.command(["python3", "/usr/lib/tombot/verify_tools.py", "--desktop"]).stdout)
    bot.command(["python3", "-c", "import sys; assert sys.version_info[:2] == (3, 13)"])
    bot.command(["python3", "-m", "venv", "/workspace/python-venv"])
    bot.command(["/workspace/python-venv/bin/pip", "--version"])
    bot.command(["uv", "venv", "--offline", "--python", "/usr/bin/python3", "/workspace/uv-venv"])
    bot.command(["node", "-e", "if (process.versions.node.split('.')[0] !== '24') process.exit(1)"])
    bot.command(["bun", "-e", "if (2 + 3 !== 5) process.exit(1)"])
    bot.command(["bash", "-c", "printf 'package main\nfunc main() { println(42) }\n' > /workspace/hello.go"])
    require(bot.command(["go", "run", "/workspace/hello.go"]).stderr.strip() == "42", "Go 实际运行失败")
    bot.command(["cargo", "new", "--vcs", "none", "/workspace/rust-example"])
    bot.command(["cargo", "build", "--offline", "--manifest-path", "/workspace/rust-example/Cargo.toml"])
    require(bot.command(["/workspace/rust-example/target/debug/rust-example"]).stdout.strip() == "Hello, world!",
            "Rust 实际运行失败")
    bot.command(["bash", "-c", "printf 'int main(void) { return 0; }\n' > /workspace/hello.c; gcc /workspace/hello.c -o /workspace/hello"])
    bot.command(["/workspace/hello"])
    bot.command(["bash", "-c", "printf 'Bot PDF verification\n' > /workspace/readme.txt"])
    bot.command(["libreoffice", "--headless", f"-env:UserInstallation=file://{bot.runtime}/office-check",
                 "--convert-to", "pdf", "--outdir", "/workspace", "/workspace/readme.txt"])
    require("Bot PDF verification" in bot.command(["pdftotext", "/workspace/readme.pdf", "-"]).stdout,
            "LibreOffice 转 PDF 或 pdftotext 读取失败")
    Path("/verification-output/tools-versions.json").write_text(json.dumps(versions, ensure_ascii=False, indent=2))


def verify_shared_files(first, second):
    first.command(["bash", "-c", "umask 0007; mkdir -p /workspace/tools/bin; printf 'shared-a' > /workspace/shared.txt; "
                   "printf '#!/bin/sh\nprintf shared-tool\n' > /workspace/tools/bin/shared-tool; chmod 0770 /workspace/tools/bin/shared-tool"])
    require(second.command(["cat", "/workspace/shared.txt"]).stdout == "shared-a", "业务文件未共享")
    require(second.command(["shared-tool"]).stdout == "shared-tool", "已安装工具未共享")
    second.command(["bash", "-c", "printf 'shared-b' > /workspace/shared.txt"])
    require(first.command(["cat", "/workspace/shared.txt"]).stdout == "shared-b", "共享文件不可互相修改")
    profile = "/var/lib/tombot-browser"
    marker = Path(profile) / "retained"
    marker.write_text("private-layout")
    marker.chmod(0o600)
    for bot in (first, second):
        for command in (["cat", profile + "/retained"], ["ls", profile], ["touch", profile + "/foreign"],
                        ["mv", profile, profile + "-replaced"]):
            require(bot.command(command, check=False).returncode != 0, "Bot 越权访问浏览器配置目录")


def verify_browser(bot):
    """只通过私有管道控制测试浏览器，验证真实渲染和命名空间沙箱。"""
    with (bot.runtime / "chrome.log").open("wb") as log:
        browser = subprocess.Popen([
            "/bin/sh", "-c", 'exec 3<&0 4>&1; exec "$@"', "chrome-pipe", "/usr/bin/google-chrome", "--headless",
            "--no-first-run", "--no-default-browser-check", "--disable-background-networking",
            "--disable-component-update", "--disable-gpu", "--remote-debugging-pipe",
            "--user-data-dir=" + str(bot.runtime / "chrome-check"), "about:blank",
        ], env=bot.environment, user=bot.uid, group=bot.gid, extra_groups=[], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=log, start_new_session=True)
    pending, sequence = b"", 0

    def call(method, parameters=None, session=None):
        nonlocal pending, sequence
        sequence += 1
        command = {"id": sequence, "method": method, "params": parameters or {}}
        if session:
            command["sessionId"] = session
        browser.stdin.write(json.dumps(command).encode() + b"\x00")
        browser.stdin.flush()
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if b"\x00" not in pending:
                if not select.select([browser.stdout], [], [], max(0, deadline - time.monotonic()))[0]:
                    break
                chunk = os.read(browser.stdout.fileno(), 65536)
                require(bool(chunk), "Chrome 关闭控制管道")
                pending += chunk
                require(len(pending) <= 1024 * 1024, "Chrome 控制响应超过限制")
            while b"\x00" in pending:
                frame, pending = pending.split(b"\x00", 1)
                message = json.loads(frame)
                if message.get("id") == sequence:
                    require("error" not in message, "Chrome 控制调用失败：" + method)
                    return message.get("result", {})
        raise TimeoutError("Chrome 控制调用超时：" + method)

    try:
        target = call("Target.createTarget", {"url": "about:blank"})["targetId"]
        session = call("Target.attachToTarget", {"targetId": target, "flatten": True})["sessionId"]
        result = call("Runtime.evaluate", {"expression": "1+2", "returnByValue": True}, session)
        require(result["result"].get("value") == 3, "Chrome 页面脚本未执行")
        call("Page.navigate", {"url": "chrome://sandbox"}, session)
        sandbox = ""

        def sandbox_ready():
            nonlocal sandbox
            response = call("Runtime.evaluate", {"expression": "document.body.innerText", "returnByValue": True}, session)
            sandbox = response["result"].get("value", "")
            # 页面标题先出现，状态表由浏览器异步填充；必须等待最终结论再读取各项。
            return "sandboxed." in sandbox

        wait_for(sandbox_ready, "Chrome 未返回沙箱状态", seconds=15)
        for expected in ("Layer 1 Sandbox\tNamespace", "PID namespaces\tYes", "Network namespaces\tYes",
                         "Seccomp-BPF sandbox\tYes", "You are adequately sandboxed."):
            require(expected in sandbox, "Chrome 沙箱没有完整启用：" + expected + "；实际状态：" + sandbox)
        Path("/verification-output/chrome-sandbox.txt").write_text(sandbox)
        require(bot.command(["unshare", "--mount", "true"], check=False).returncode != 0,
                "普通桌面身份不应具备外层挂载命名空间权限")
        call("Browser.close")
        require(browser.wait(timeout=10) == 0, "Chrome 未正常退出")
    finally:
        if browser.poll() is None:
            os.killpg(browser.pid, signal.SIGTERM)
        try:
            browser.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(browser.pid, signal.SIGKILL)
            browser.wait(timeout=5)
        browser.stdin.close()
        browser.stdout.close()


def verify_desktops(first, second):
    for bot, title in ((first, "机器人甲"), (second, "机器人乙")):
        bot.application(["xfce4-terminal", "--disable-server", "--title", title, "--geometry", "80x20+50+50",
                         "--command", "bash --noprofile --norc"])

        def activate():
            found = bot.command(["xdotool", "search", "--onlyvisible", "--name", title], check=False)
            if found.returncode != 0:
                return False
            bot.command(["xdotool", "windowactivate", "--sync", found.stdout.splitlines()[-1]])
            return True

        wait_for(activate, "终端窗口未就绪")

    def type_remote(bot, text):
        with remote_desktop(bot.runtime / "desktop.sock") as connection:
            for character in text:
                key = 0xff0d if character == "\n" else ord(character)
                connection.sendall(struct.pack("!BBHI", 4, 1, 0, key) + struct.pack("!BBHI", 4, 0, 0, key))
            connection.sendall(struct.pack("!BBHH", 5, 0, 100 * bot.number, 150 * bot.number))

    with ThreadPoolExecutor(max_workers=2) as pool:
        pending = [pool.submit(type_remote, bot, f"printf bot-{bot.number} > /workspace/input-{bot.number}\n")
                   for bot in (first, second)]
        for result in pending:
            result.result()
    for bot in (first, second):
        wait_for(lambda: Path(f"/workspace/input-{bot.number}").exists(), "远程键盘输入未执行")
        require(Path(f"/workspace/input-{bot.number}").read_text() == f"bot-{bot.number}", "键盘输入串到其他桌面")
        coordinates = bot.command(["xdotool", "getmouselocation", "--shell"]).stdout
        require(f"X={100 * bot.number}\n" in coordinates and f"Y={150 * bot.number}\n" in coordinates,
                "鼠标操作串到其他桌面")
    for own, foreign, foreign_title in ((first, second, "机器人乙"), (second, first, "机器人甲")):
        require(own.command(["xdotool", "search", "--onlyvisible", "--name", foreign_title], check=False).returncode != 0,
                "桌面出现其他 Bot 窗口")
        require(own.command(["env", f"DISPLAY=:{foreign.number}", "xdpyinfo"], check=False).returncode != 0,
                "跨桌面 X11 认证未被拒绝")
        require(own.command(["python3", "-c", "import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])",
                             str(foreign.runtime / "desktop.sock")], check=False).returncode != 0,
                "可以连接其他 Bot 的远程桌面")
        require(own.command(["cat", str(foreign.runtime / "Xauthority")], check=False).returncode != 0,
                "可以读取其他 Bot 的桌面认证")
        require(own.command(["xdpyinfo", "-display", f"127.0.0.1:{own.number}"], check=False).returncode != 0,
                "X11 意外监听 TCP")
        with socket.socket() as connection:
            connection.settimeout(1)
            require(connection.connect_ex(("127.0.0.1", 5900 + own.number)) != 0, "RFB 意外监听 TCP")
        with remote_desktop(own.runtime / "desktop.sock") as connection:
            connection.sendall(struct.pack("!BBHHBB", 251, 0, 1024, 768, 1, 0)
                               + struct.pack("!IHHHHI", 0, 0, 0, 1024, 768, 0))
        require("1280x800 pixels" in own.command(["xdpyinfo"]).stdout, "远端请求改变了固定桌面尺寸")
        screenshot = Path(f"/workspace/bot-{own.number}.png")
        own.command(["scrot", str(screenshot)])
        require(struct.unpack("!II", screenshot.read_bytes()[16:24]) == (1280, 800), "截图尺寸不符")
        shutil.copyfile(screenshot, f"/verification-output/bot-{own.number}.png")


@contextmanager
def browser_launcher(bot):
    """按正式目录身份启动镜像内的固定程序；只为本次验收准备独立目录。"""
    runtime = Path("/run/tombot-browser") / bot.runtime.name
    profile = Path("/var/lib/tombot-browser") / bot.desktop_id
    uid = 200000 + bot.number
    for directory, mode in ((runtime, 0o711), (profile, 0o700)):
        directory.mkdir(mode=mode, exist_ok=directory == profile)
        os.chown(directory, uid, bot.gid)
        directory.chmod(mode)
    process = subprocess.Popen(["/usr/bin/python3", "-I", "-B", "/usr/lib/tombot/open_browser.py",
                                "serve", bot.runtime.name, bot.desktop_id, str(bot.number)],
                               user=uid, group=bot.gid, extra_groups=[], start_new_session=True)
    try:
        wait_for(lambda: (runtime / "open.sock").is_socket(), "浏览器启动入口未就绪")
        yield profile
    finally:
        if process.poll() is None:
            process.terminate()
        require(process.wait(timeout=10) == 0, "桌面浏览器入口未正常退出")


def verify_desktop_browsers(first, second, restored=False):
    """同一测试站点使用持久 Cookie 表示登录，实际键鼠经各自 RFB 连接进入原生窗口。"""
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    from threading import Thread

    class Site(BaseHTTPRequestHandler):
        def do_GET(self):
            if not self.path.startswith("http://desktop-check.test/"):
                self.send_error(404)
                return
            content = b"""<!doctype html><style>input{position:fixed;inset:0;width:100%;height:100%}</style>
<input autofocus oninput="document.cookie='desk='+this.value+'; Max-Age=3600; Path=/'; document.title='typed-'+this.value; this.style.background=this.value.includes('1')?'#2456ab':'#b74828'">
<script>document.title='stored-'+(document.cookie.split('desk=')[1]||'empty')</script>"""
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(content)))
            self.end_headers()
            self.wfile.write(content)

        def do_CONNECT(self):
            # 只接收本地验收站的 HTTP。拒绝后台 HTTPS 时只发响应头，客户端可能已经退出。
            self.send_response_only(403)
            self.end_headers()

        def log_message(self, *_):
            return

    def run_both(action):
        with ThreadPoolExecutor(max_workers=2) as pool:
            pending = [pool.submit(action, bot) for bot in (first, second)]
            for future in pending:
                future.result()

    def visible(bot):
        return bot.command(["xdotool", "search", "--onlyvisible", "--class", "google-chrome"], check=False)

    def title(bot, prefix):
        def active_title():
            return bot.command(["xdotool", "getactivewindow", "getwindowname"], check=False).stdout
        try:
            wait_for(lambda: active_title().startswith(prefix), f"桌面 {bot.number} 未观察到预期网页状态 {prefix}", 15)
        except TimeoutError as err:
            actual = active_title()
            # 仅输出本地测试页生成的已知状态，窗口标题可能包含其他应用的内容。
            state = next((s for s in ("empty", "bot1", "bot2okok", "bot2ok", "bot2")
                          if actual.startswith("stored-" + s + " ")), "其他窗口")
            raise RuntimeError(f"桌面 {bot.number} 预期 {prefix}，观察到 {state}") from err


    def open_page(bot, stored):
        bot.command(["google-chrome"])
        bot.command(["xdotool", "key", "ctrl+l"])
        bot.command(["xdotool", "type", "http://desktop-check.test/"])
        bot.command(["xdotool", "key", "Return"])
        title(bot, "stored-" + stored)

    def type_remote(bot, text):
        with remote_desktop(bot.runtime / "desktop.sock") as connection:
            connection.sendall(struct.pack("!BBHH", 5, 1, 360, 340) + struct.pack("!BBHH", 5, 0, 360, 340))
            connection.sendall(struct.pack("!BBHI", 4, 1, 0, 0xff57) + struct.pack("!BBHI", 4, 0, 0, 0xff57))
            for character in text:
                key = ord(character)
                connection.sendall(struct.pack("!BBHI", 4, 1, 0, key) + struct.pack("!BBHI", 4, 0, 0, key))

    def screen_color(bot):
        """请求指定观看连接上的一个原始像素；从两个桌面读回的颜色必须与各自输入一致。"""
        with remote_desktop(bot.runtime / "desktop.sock") as stream:
            pixel_format = struct.pack("!BBBBHHHBBB3x", 32, 24, 0, 1, 255, 255, 255, 16, 8, 0)
            stream.sendall(bytes(4) + pixel_format + struct.pack("!BBHi", 2, 0, 1, 0))
            stream.sendall(struct.pack("!BBHHHH", 3, 0, 370, 345, 1, 1))
            require(receive(stream, 4) == bytes([0, 0, 0, 1]), "观看流未返回指定画面区域")
            require(struct.unpack("!HHHHi", receive(stream, 12)) == (370, 345, 1, 1, 0), "观看流画面格式无效")
            return int.from_bytes(receive(stream, 4), "little") & 0xffffff

    with ThreadingHTTPServer(("127.0.0.1", 3128), Site) as site, browser_launcher(first), browser_launcher(second):
        thread = Thread(target=site.serve_forever, daemon=True)
        thread.start()
        try:
            require(all(visible(bot).returncode == 1 for bot in (first, second)), "桌面启动时 Chrome 已打开")
            if restored:
                run_both(lambda bot: open_page(bot, "bot1" if bot == first else "bot2okok"))
                print("已通过重建容器与新执行会话按桌面 ID 恢复各自的网站登录", flush=True)
                return
            run_both(lambda bot: open_page(bot, "empty"))
            run_both(lambda bot: type_remote(bot, "bot" + str(bot.number)))
            for bot in (first, second):
                title(bot, "typed-bot" + str(bot.number))
                expected = 0x2456ab if bot == first else 0xb74828
                wait_for(lambda: screen_color(bot) == expected, "观看流的像素与桌面输入不符", 8)
                before = visible(bot).stdout
                bot.command(["google-chrome"])
                require(visible(bot).stdout == before, "重复打开创建了其他窗口")
                for owner in (first, second):
                    require(bot.command(["ls", "/var/lib/tombot-browser/" + owner.desktop_id], check=False).returncode != 0,
                            "桌面程序可直接读取浏览器持久配置")
            job = first.application(["sh", "-c", 'while [ ! -f "$XDG_RUNTIME_DIR/continue" ]; do sleep 0.2; done'])
            first.command(["xdotool", "key", "ctrl+shift+w"])
            wait_for(lambda: visible(first).returncode == 1, "第一个 Chrome 未关闭")
            first.command(["touch", str(first.runtime / "continue")])
            require(job.wait(timeout=5) == 0 and first.ready(), "关闭 Chrome 影响了原桌面任务")
            type_remote(second, "ok")
            title(second, "typed-bot2ok")
            open_page(first, "bot1")
            pid = first.command(["xdotool", "getactivewindow", "getwindowpid"]).stdout.strip()
            os.kill(int(pid), signal.SIGKILL)
            wait_for(lambda: visible(first).returncode == 1, "异常退出的 Chrome 仍有窗口")
            require(first.ready() and second.ready(), "浏览器退出影响了桌面")
            type_remote(second, "ok")
            title(second, "typed-bot2okok")
            with remote_desktop(first.runtime / "desktop.sock"):
                first.command(["true"])
            print("已通过双桌面浏览器并发输入、独立登录、按需启动、关闭和崩溃隔离", flush=True)
        finally:
            site.shutdown()
            thread.join(timeout=5)



def verify_program_entrypoints():
    """通过镜像中的正式入口启动外置代理，核对真实进程版本、控制监听及降权 Bot 入口。"""
    control = Path("/run/tombot-control")
    revision = os.environ["TOM_TEST_PROGRAM_CORE"]
    (control / "program-core").write_text(revision + "\n")
    agent = "/usr/lib/tombot/computer-agent"
    require(subprocess.run([agent], capture_output=True, timeout=5).returncode != 0, "未配置的电脑代理不能报告成功")
    options = json.loads(os.environ["TOM_TEST_AGENT_OPTIONS"])
    identity = {name: str(number).zfill(26) for number, name in enumerate(("user_id", "computer_id", "node_id"), 1)}
    config = control / "config.json"
    config.write_text(json.dumps({"version": 4, "identity": {**identity, "generation": 1}, "options": options}, separators=(",", ":")))
    config.chmod(0o600)
    (control / "program-core").write_text("0" * 64 + "\n")
    missing = subprocess.run([agent], capture_output=True, timeout=5)
    require(missing.returncode == 127 and not (control / "agent.sock").exists(), "选定的程序缺失时仍启动了电脑代理")
    (control / "program-core").write_text(revision + "\n")
    with subprocess.Popen([agent], stdout=subprocess.DEVNULL) as process:
        try:
            wait_for(lambda: (control / "agent.sock").is_socket() or process.poll() is not None, "外置电脑代理启动超时", 12)
            require(process.poll() is None, "外置电脑代理在建立控制监听前退出")
            # 在限制进程窥探的正式 seccomp 配置下，使用内核返回的监听对端确认该启动进程已就绪。
            with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as client:
                client.settimeout(3)
                client.connect(str(control / "agent.sock"))
                peer_pid, _, _ = struct.unpack("3i", client.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
                require(peer_pid == process.pid, "监听入口的进程与节点启动的外置代理不一致")
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
                raise
        require(process.returncode == 0, "外置电脑代理未正常停止")
    task = subprocess.run(["/usr/lib/tombot/computer-task", "--invalid"], capture_output=True, timeout=5)
    require(task.returncode == 125 and "任务引导".encode() in task.stderr, "基础任务入口未运行所选程序")
    bot = subprocess.run(["/usr/lib/tombot/bot-agent", "--invalid"], user=100001, group=20000, extra_groups=[],
                         capture_output=True, timeout=5)
    require(bot.returncode == 125 and b"Bot Agent" in bot.stderr, "降权后的桌面无法执行当前 Bot 程序")
    return revision


def inside(restored=False):
    require(os.getuid() == 0 and Path("/etc/os-release").is_file(), "验收入口必须运行在测试容器内")
    release = Path("/etc/os-release").read_text()
    require('VERSION_ID="13"' in release and "VERSION_CODENAME=trixie" in release, "基础系统不是 Debian 13")
    require(hashlib.sha256(Path("/usr/share/tombot/seccomp.json").read_bytes()).hexdigest()
            == os.environ["TOM_TEST_SECCOMP_SHA256"], "运行配置与镜像内 seccomp 清单不同步")
    preparation = ["python3", "-I", "-B", "/usr/lib/tombot/prepare.py"]
    for _ in range(2):
        subprocess.run(preparation, user=0, group=20000, extra_groups=[], check=True, timeout=10)
    require(subprocess.run(preparation, user=100001, group=20000, extra_groups=[],
                           capture_output=True, timeout=5).returncode != 0, "普通 Bot 不得执行运行空间初始化")
    revision = verify_program_entrypoints()
    for uid in (0, 30001):
        probe_path = "/usr/lib/tombot/computer-probe" if uid == 0 else f"/usr/lib/tombot/programs/{revision}/computer-probe"
        probe = subprocess.run([probe_path], user=uid, group=0 if uid == 0 else 20000,
                               extra_groups=[], capture_output=True, timeout=5)
        require(probe.returncode == 125 and not probe.stdout,
                "没有封存授权和真实服务组的探针不得输出成功证据")
    require(subprocess.run(["python3", "/usr/lib/tombot/desktop.py", "--runtime", "/run/bot-desktops/root", "--display", "9"],
                           capture_output=True, timeout=5).returncode != 0, "不允许 root 图形桌面")
    bots = [BotDesktop(n, n + 10 if restored else n) for n in (1, 2)]
    try:
        for bot in bots:
            bot.start()
        for bot in bots:
            wait_for(bot.ready, "完整 XFCE 桌面启动超时")
            bot.environment = json.loads((bot.runtime / "environment.json").read_text())
        if restored:
            verify_desktop_browsers(*bots, restored=True)
            require(Path("/workspace/shared.txt").read_text() == "shared-b", "重建容器后共享文件未保留")
            return
        verify_shared_files(*bots)
        verify_desktops(*bots)
        verify_desktop_browsers(*bots)
        verify_programs(bots[1])
        verify_browser(bots[1])
        duplicate = bots[0].command(["python3", "/usr/lib/tombot/desktop.py", "--runtime", str(bots[0].runtime), "--display", "1"], check=False)
        require(duplicate.returncode != 0 and bots[0].ready(), "重复启动影响了活动桌面")
        # 主图形会话异常必须向监督者报错，不能把失败伪装为正常退出。
        bots[0].command(["pkill", "-u", str(bots[0].uid), "-x", "xfce4-session"])
        require(bots[0].process.wait(timeout=20) != 0, "图形会话异常被静默忽略")
        with remote_desktop(bots[1].runtime / "desktop.sock"):
            require(bots[1].ready(), "一个桌面异常影响了另一个")
        bots[1].process.terminate()
        require(bots[1].process.wait(timeout=20) == 0, "桌面未正常响应停止")
        require(not (bots[1].runtime / "ready.json").exists(), "桌面停止后残留就绪标记")
        print("已通过：工具与编译、PDF 转换、Chrome 沙箱、双桌面、远程输入、固定尺寸、共享文件、权限拒绝及定向关闭", flush=True)
    except BaseException:
        for bot in bots:
            for name in ("x11", "dbus", "xfce", "chrome"):
                log = bot.runtime / f"{name}.log"
                if log.is_file():
                    print(f"验收桌面 {bot.number} {name}：\n" + log.read_text(errors="replace")[-4000:], file=sys.stderr)
        raise
    finally:
        for bot in bots:
            bot.stop()


@contextmanager
def test_container(name, arguments):
    """只管理本次唯一命名的验收容器，任何启动或验收失败都显式回收。"""
    subprocess.run(["docker", "create", "--name", name, *arguments], check=True, timeout=30, capture_output=True)
    try:
        yield name
        state = json.loads(subprocess.run(["docker", "inspect", name], check=True, timeout=30, capture_output=True).stdout)[0]["State"]
        require(state["Status"] == "exited" and state["ExitCode"] == 0, "验收容器未正常完成")
    finally:
        subprocess.run(["docker", "rm", "--force", name], check=True, timeout=30)


@contextmanager
def test_volume(name, image):
    """使用全新临时卷模拟节点已准备的两个子目录，绝不挂载已有用户卷或宿主磁盘。"""
    subprocess.run(["docker", "volume", "create", name], check=True, timeout=30, capture_output=True)
    try:
        setup = """import os
os.chmod('/test-volume', 0o755)
for name, uid, mode in [('workspace', 0, 0o2770), ('desktops', 0, 0o711)]:
    path = '/test-volume/' + name
    os.mkdir(path, 0o700)
    os.chown(path, uid, 0 if name == 'desktops' else 20000)
    os.chmod(path, mode)
"""
        arguments = ["--network", "none", "--read-only", "--user", "0:20000", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER",
                     "--security-opt", "no-new-privileges:true", "--pids-limit", "16", "--memory", "64m",
                     "--mount", f"type=volume,src={name},dst=/test-volume,volume-nocopy",
                     "--entrypoint", "python3", image, "-c", setup]
        with test_container(name + "-setup", arguments) as container:
            subprocess.run(["docker", "start", "--attach", container], check=True, timeout=30)
        yield name
    finally:
        subprocess.run(["docker", "volume", "rm", name], check=True, timeout=30)


@contextmanager
def mounted_programs(architecture, package):
    """在宿主端准备只读挂载，验收真实的外置 ELF；不把测试程序写入镜像。"""
    from node_release import PROGRAMS, verify as verify_package
    import tarfile
    with tempfile.TemporaryDirectory(prefix="tombot-image-programs-") as path:
        root = Path(path).resolve()
        binaries = root / "bin"
        binaries.mkdir()
        if package:
            profile = verify_package(package)
            require(set(profile) == {"version", "label", "platform"} and profile["version"] == 2 and profile.get("platform") == "linux/" + architecture,
                    "程序包与待验收镜像架构不匹配")
            with tarfile.open(package, "r:gz") as archive:
                for name in PROGRAMS[1:]:
                    with archive.extractfile(name) as src, (binaries / name).open("xb") as dst:
                        shutil.copyfileobj(src, dst)
                    (binaries / name).chmod(0o755)
        else:
            environment = {**os.environ, "GOOS": "linux", "GOARCH": architecture, "CGO_ENABLED": "0"}
            for name in PROGRAMS[1:]:
                subprocess.run(["go", "build", "-trimpath", "-o", str(binaries / name), "./cmd/" + name],
                               cwd=Path(__file__).resolve().parents[1], env=environment, check=True, timeout=300)
        revision = hashlib.sha256("".join(n + " " + hashlib.sha256((binaries / n).read_bytes()).hexdigest() + "\n"
                                          for n in PROGRAMS[1:] if n != "bot-agent").encode()).hexdigest()
        programs = root / "programs"
        programs.mkdir()
        bot = hashlib.sha256((binaries / "bot-agent").read_bytes()).hexdigest()
        (programs / bot).mkdir()
        (binaries / "bot-agent").rename(programs / bot / "bot-agent")
        binaries.rename(programs / revision)
        (programs / "bot").symlink_to(bot)
        agent_options = json.loads((Path(__file__).resolve().parents[1] / "deploy/node.example.json").read_bytes())["agent"]
        yield ["--mount", f"type=bind,src={programs},dst=/usr/lib/tombot/programs,readonly",
               "--tmpfs", "/run/tombot-control:rw,nosuid,nodev,noexec,mode=0700,size=1048576",
               "--env", "TOM_TEST_AGENT_OPTIONS=" + json.dumps(agent_options), "--env", "TOM_TEST_PROGRAM_CORE=" + revision]


def verify(image, report, package=None):
    name = "tombot-image-check-" + uuid.uuid4().hex
    profile = Path(__file__).resolve().parents[1] / "deploy/computer/seccomp.json"
    inspected = json.loads(subprocess.run(["docker", "image", "inspect", image], check=True, timeout=30, capture_output=True).stdout)
    require(len(inspected) == 1 and inspected[0]["Config"]["Labels"].get("io.tombot.computer.storage-layout") == "2"
            and inspected[0]["Config"]["Labels"].get("io.tombot.computer.desktop-browser") == "1"
            and inspected[0]["Config"]["Labels"].get("io.tombot.computer.external-programs") == "1",
            "镜像持久盘布局版本不匹配")
    with tempfile.TemporaryDirectory(prefix="tombot-image-evidence-") as directory, test_volume(name + "-volume", image) as volume, \
            mounted_programs(inspected[0]["Architecture"], package) as mounts:
        command = [*mounts, "--init", "--hostname", "desktop-verification", "--network", "none", "--read-only",
                   "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--pids-limit", "768",
                   "--security-opt", f"seccomp={profile}",
                   "--env", "TOM_TEST_SECCOMP_SHA256=" + hashlib.sha256(profile.read_bytes()).hexdigest(),
                   "--memory", "3g", "--memory-swap", "3g", "--cpus", "2", "--shm-size", "256m",
                   "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,mode=1777,size=268435456",
                   "--tmpfs", "/run:rw,nosuid,nodev,noexec,mode=0755,size=134217728",
                   "--mount", f"type=volume,src={volume},dst=/workspace,volume-nocopy,volume-subpath=workspace",
                   "--mount", f"type=volume,src={volume},dst=/var/lib/tombot-browser,volume-nocopy,volume-subpath=desktops",
                   "--mount", f"type=bind,src={Path(__file__).resolve()},dst=/verify.py,readonly",
                   "--mount", f"type=bind,src={Path(directory).resolve()},dst=/verification-output",
                   "--entrypoint", "python3"]
        for capability in ("CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL"):
            command.extend(["--cap-add", capability])
        command.extend([image, "/verify.py", "--inside"])
        with test_container(name, command) as container:
            subprocess.run(["docker", "start", "--attach", container], check=True, timeout=360)
            state = json.loads(subprocess.run(["docker", "inspect", container], check=True, timeout=30, capture_output=True).stdout)[0]
            mounted = {item["Target"]: item for item in state["HostConfig"]["Mounts"] if item["Type"] == "volume"}
            for target, subpath in (("/workspace", "workspace"), ("/var/lib/tombot-browser", "desktops")):
                require(mounted[target]["Source"] == volume and mounted[target]["VolumeOptions"] == {"NoCopy": True, "Subpath": subpath},
                        "真实 Docker 子目录挂载与声明不符")
        with test_container(name + "-restored", [*command, "--restore-logins"]) as container:
            subprocess.run(["docker", "start", "--attach", container], check=True, timeout=150)
        shutil.copytree(directory, report)
    print(f"验收产物：{report}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="tombot-computer:trixie")
    parser.add_argument("--program-package", type=Path, help="与镜像配套的节点程序包")
    parser.add_argument("--report", type=Path)
    parser.add_argument("--inside", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--restore-logins", action="store_true", help=argparse.SUPPRESS)
    arguments = parser.parse_args()
    if arguments.inside:
        inside(arguments.restore_logins)
    else:
        target = arguments.report or Path(tempfile.gettempdir()) / ("tombot-image-report-" + uuid.uuid4().hex)
        require(not target.exists(), "验收产物目录已存在，不覆盖旧结果")
        verify(arguments.image, target.resolve(), arguments.program_package)
