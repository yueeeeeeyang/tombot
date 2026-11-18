"""监督非无头 Chrome，并把已授权原生窗口转发到各 Bot 的独立桌面。"""

import base64
import concurrent.futures
import ctypes
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import stat
import struct
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
from native_browser_policy import WindowOwners

DISPLAY_NUMBER = 5000
MAXIMUM_REQUEST = 32768
ROOT = Path(os.environ.get("XDG_RUNTIME_DIR", "/invalid"))
CLIENT_OPTIONS = ["--sharing=yes", "--clipboard=no", "--file-transfer=no", "--printing=no", "--webcam=no",
                  "--speaker=off", "--microphone=off", "--notifications=no", "--tray=no", "--opengl=no", "--reconnect=no", "--remote-logging=no", "--title=@title@"]


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def write_private(path, encoded):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "wb") as output:
        output.write(encoded)


def stop_process(process):
    if process.poll() is None:
        process.terminate()
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=2)


def child_arguments(mode, arguments):
    return ["/usr/bin/python3", "-I", "-B", __file__, "guarded", str(os.getpid()), mode, *arguments]


def guard_parent(parent):
    # 监督进程异常消失也必须关闭调试管道及原生窗口，不能依赖下一轮容器回收才撤销。
    libc = ctypes.CDLL(None, use_errno=True)
    require(libc.prctl(1, signal.SIGTERM, 0, 0, 0) == 0, "设置浏览器父进程监督失败")
    require(os.getppid() == parent, "浏览器父进程已退出")


class NativeServer:
    """只有容器内 root 代理能建立归属；桌面客户端只持有自己的窗口视图。"""

    def __init__(self, server, glib):
        self.server, self.glib = server, glib
        self.owners = WindowOwners()
        self.bindings, self.clients = {}, {}
        self.paused = set()
        self.stopping = threading.Event()
        self.lock = threading.RLock()

    def main_thread(self, action):
        result = concurrent.futures.Future()

        def invoke():
            if not result.set_running_or_notify_cancel():
                return False
            try:
                with self.lock:
                    result.set_result(action())
            except Exception as error:
                result.set_exception(error)
            return False

        self.glib.idle_add(invoke)
        try:
            return result.result(timeout=8)
        except concurrent.futures.TimeoutError:
            result.cancel()
            raise RuntimeError("窗口服务响应超时") from None

    def bind(self, request):
        owner, window_id = request["owner"], request["window"]
        require(re.fullmatch(r"[0-9A-HJKMNP-TV-Z]{26}", owner) is not None, "窗口会话标识无效")
        require(isinstance(window_id, int) and window_id > 0, "浏览器窗口编号无效")
        if window_id in self.bindings:
            require(self.bindings[window_id][0] == owner, "浏览器窗口不属于当前会话")
            return self.bindings[window_id][1]
        bounds = request["bounds"]
        require(len(bounds) == 4 and all(type(value) is int for value in bounds), "原生窗口边界无效")
        matches = [window for window in self.server._window_to_id
                   if list(window.get_property("geometry")) == bounds
                   and window.get_property("xid") not in self.owners.owners]
        require(len(matches) <= 1, "无法唯一确定原生窗口归属")
        if not matches:
            return 0
        window = matches[0]
        xid = window.get_property("xid")
        self.owners.bind(xid, owner)
        self.bindings[window_id] = (owner, xid)
        self.server._send_new_window_packet(window)
        return xid

    def client(self, request):
        owner, display = request["owner"], request["display"]
        require(type(display) is int and 1 <= display <= 4095, "Bot 显示编号无效")
        authorization = base64.b64decode(request["authorization"], validate=True)
        require(0 < len(authorization) <= 4096, "桌面认证内容无效")
        existing = self.clients.get(owner)
        if existing:
            require(existing[0] == display and existing[2].poll() is None, "桌面转发已失效")
            return existing[1]
        require(len(self.clients) < 64, "桌面转发数量达到上限")
        auth_file = ROOT / ("client-" + owner + ".auth")
        write_private(auth_file, authorization)
        environment = dict(os.environ, DISPLAY=f":{display}", XAUTHORITY=str(auth_file), XPRA_USER_UUID=owner, XPRA_UNICODE_KEYNAMES="1")
        process = subprocess.Popen(child_arguments("client", ["attach", "socket:" + str(ROOT / "xpra.sock"), *CLIENT_OPTIONS]),
                                   env=environment, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.clients[owner] = (display, environment, process)
        return environment

    def operate(self, request):
        action = request.get("action")
        if action == "control":
            owner=request["owner"]
            require(owner in self.clients, "接管窗口尚未准备")
            if request.get("paused", False):
                self.paused.add(owner)
            else:
                self.paused.discard(owner)
            return {"state": "ready"}
        require(action == "release" or request.get("owner") not in self.paused, "用户接管期间拒绝自动化输入与截图")
        if action == "bind":
            xid = self.main_thread(lambda: self.bind(request))
            if not xid:
                return {"state": "starting"}
            environment = self.client(request)
            operation = {"action": "inspect", "xid": xid}
        elif action == "release":
            owner = request["owner"]

            def release():
                for window in self.server._window_to_id:
                    require(not self.owners.allows(window.get_property("xid"), owner), "所属窗口尚未关闭")
                self.bindings = {key: value for key, value in self.bindings.items() if value[0] != owner}

            self.main_thread(release)
            client = self.clients.pop(owner, None)
            self.paused.discard(owner)
            if client:
                stop_process(client[2])
                (ROOT / ("client-" + owner + ".auth")).unlink()
            return {"state": "closed"}
        else:
            require(action in {"input", "capture", "position"}, "原生浏览器动作无效")
            with self.lock:
                owner, xid = self.bindings[request["window"]]
                require(owner == request["owner"] and self.owners.allows(xid, owner), "原生窗口归属不符")
                client = self.clients[owner]
                require(client[2].poll() is None, "桌面窗口转发已退出")
                environment = client[1]
            operation = {"action": action, "xid": xid, "command": request.get("command")}
        result = subprocess.run(["/usr/bin/python3", "-I", "-B", str(Path(__file__).with_name("native_browser_io.py"))],
                                input=json.dumps(operation).encode(), env=environment, capture_output=True, timeout=12, check=True)
        require(len(result.stdout) <= 6 << 20, "原生截图超过限制")
        return json.loads(result.stdout)

    def serve(self):
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(str(ROOT / "native.sock"))
        os.chmod(ROOT / "native.sock", 0o600)
        listener.listen(8)
        listener.settimeout(0.2)
        try:
            while not self.stopping.is_set():
                try:
                    connection, _ = listener.accept()
                except TimeoutError:
                    continue
                with connection:
                    _, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
                    if uid != 0:
                        print("拒绝非代理的原生窗口控制请求", file=sys.stderr)
                        continue
                    connection.settimeout(15)
                    try:
                        with connection.makefile("rb") as source:
                            encoded = source.readline(MAXIMUM_REQUEST + 1)
                        require(encoded.endswith(b"\n") and len(encoded) <= MAXIMUM_REQUEST, "窗口请求超过限制")
                        result = self.operate(json.loads(encoded))
                    except Exception:
                        # 不回传异常原文，防止网站内容、登录输入或认证文件进入日志。
                        result = {"state": "failed"}
                        print("原生窗口控制请求失败", file=sys.stderr)
                    connection.sendall(json.dumps(result).encode() + b"\n")
        except Exception:
            print("原生窗口控制入口退出", file=sys.stderr)
            self.glib.idle_add(lambda: self.server.quit(False))
        finally:
            listener.close()
            for _, _, process in self.clients.values():
                stop_process(process)

    def refresh(self):
        with self.lock:
            alive = set()
            for window in tuple(self.server._window_to_id):
                xid = window.get_property("xid")
                alive.add(xid)
                if xid not in self.owners.owners and "transient-for" in window.get_property_names():
                    parent = window.get_property("transient-for")
                    if parent and self.owners.inherit(xid, parent):
                        self.server._send_new_window_packet(window)
            for xid in tuple(self.owners.owners):
                if xid not in alive:
                    self.owners.remove(xid)
        return not self.stopping.is_set()


def serve_xpra(arguments):
    from xpra.scripts import server as startup
    from xpra.server.source.window import WindowsConnection
    from xpra.os_util import gi_import
    glib = gi_import("GLib")
    original_make = startup.make_seamless_server

    def make_server(backend, clobber):
        server = original_make(backend, clobber)
        native = NativeServer(server, glib)
        original_can_send = WindowsConnection.can_send_window
        WindowsConnection.can_send_window = lambda source, window: native.owners.allows(window.get_property("xid"), source.uuid) and original_can_send(source, window)
        original_process = server.process_packet
        original_mouse = server.do_process_mouse_common

        def mouse_common(proto, device_id, wid, pointer, props=None):
            # 两个独立桌面的鼠标坐标可能相同；Xpra 默认只在坐标改变时抬升窗口，会把点击送给另一窗口。
            window = server.get_window(wid)
            if window is not None:
                from xpra.x11.error import xsync
                with xsync:
                    window.raise_window()
            return original_mouse(proto, device_id, wid, pointer, props)

        def process(proto, packet):
            source = server.get_server_source(proto)
            if source is not None and packet.get_type() == "info-request":
                # 客户端握手会查询服务摘要；仅返回当前视图的窗口号，不暴露全局页面与进程信息。
                with native.lock:
                    owned = [xid for xid, owner in native.owners.owners.items() if owner == source.uuid]
                source.send_info_response({"windows": owned})
                return
            if source is not None and not native.owners.accepts(source.uuid, packet):
                kind = packet.get_type()
                label = kind if isinstance(kind, str) and re.fullmatch(r"[a-z_-]{1,40}", kind) else "unknown"
                print("拒绝桌面窗口协议类型：" + label, file=sys.stderr)
                server.disconnect_client(proto, "窗口访问不属于当前桌面")
                return
            original_process(proto, packet)

        def property_changed(window, event):
            for source in server.window_sources():
                if source.can_send_window(window) and "x11-property" in getattr(source, "window_metadata_supported", ()):
                    source.send("window-metadata", server._window_to_id[window], {"x11-property": event})

        server.process_packet = process
        server.do_process_mouse_common = mouse_common
        server._x11_property_changed = property_changed
        glib.timeout_add(25, native.refresh)
        threading.Thread(target=native.serve, daemon=True).start()
        return server

    startup.make_seamless_server = make_server
    from xpra.scripts.main import main
    return main("xpra", arguments)


def attach_client(arguments):
    from xpra.scripts import main as startup
    original_make = startup.make_client

    def make_client(options):
        client = original_make(options)
        from xpra.client.gui.keyboard_helper import KeyboardHelper
        original_properties = KeyboardHelper.get_keymap_properties
        # 每个桌面的临时 Unicode 键码不同，按键名映射而不是覆盖共享 Chrome 的原生键码表。
        KeyboardHelper.get_keymap_properties = lambda helper, skip=(): original_properties(helper, (*skip, "query_struct", "x11_keycodes"))
        from xpra.client.gtk3.window.base import GTKClientWindowBase

        def set_native_id(window, value):
            # 固定版本把数值 XID 转成十进制字符串；显式转换后设置本服务的窗口标记。
            number = int(value, 16) if value.startswith("0x") else int(value, 10)
            require(0 < number < 2**32, "原生窗口标识无效")
            window.set_x11_property("TOMBOT_WINDOW_ID", "u32", number)

        GTKClientWindowBase.set_xid = set_native_id
        return client

    startup.make_client = make_client
    return startup.main("xpra", arguments)


def supervise(chrome_arguments):
    require(os.getuid() == 30000 and os.getgid() == 20000, "原生浏览器必须使用专用身份")
    identity = ROOT.lstat()
    require(ROOT.is_absolute() and ROOT.resolve() == ROOT and stat.S_ISDIR(identity.st_mode)
            and identity.st_uid == 30000 and stat.S_IMODE(identity.st_mode) == 0o700, "原生浏览器运行目录无效")
    require(chrome_arguments[0] == "/usr/bin/google-chrome" and not any(arg.startswith("--headless") for arg in chrome_arguments), "只能启动桌面 Chrome")
    write_private(ROOT / "passwd", b"browser:x:30000:20000:Browser:/var/lib/tombot-browser:/bin/false\n")
    write_private(ROOT / "group", b"browser:x:20000:\n")
    write_private(ROOT / "Xauthority", b"")
    chrome_environment = dict(os.environ, DISPLAY=f":{DISPLAY_NUMBER}", XAUTHORITY=str(ROOT / "Xauthority"),
                              USER="browser", LOGNAME="browser")
    # Xpra 需要虚拟用户数据库。预加载库传入 Chrome 会阻塞 Crashpad 的启动握手。
    for name in ("LD_PRELOAD", "NSS_WRAPPER_PASSWD", "NSS_WRAPPER_GROUP"):
        chrome_environment.pop(name, None)
    xpra_environment = dict(chrome_environment, LD_PRELOAD="/usr/lib/tombot-nss-wrapper.so",
                            NSS_WRAPPER_PASSWD=str(ROOT / "passwd"), NSS_WRAPPER_GROUP=str(ROOT / "group"))
    subprocess.run(["/usr/bin/xauth", "-f", str(ROOT / "Xauthority")], input=f"add :{DISPLAY_NUMBER} . {secrets.token_hex(16)}\n",
                   text=True, env=xpra_environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=5)
    processes = []
    stopping = threading.Event()
    for name in (signal.SIGTERM, signal.SIGINT):
        signal.signal(name, lambda *_: stopping.set())
    try:
        xpra = subprocess.Popen(child_arguments("serve", ["start", f":{DISPLAY_NUMBER}",
                                 "--daemon=no", "--bind=" + str(ROOT / "xpra.sock"), "--socket-dir=" + str(ROOT), "--socket-dirs=",
                                 "--sharing=yes", "--exit-with-client=no", "--mdns=no", "--clipboard=no", "--file-transfer=no",
                                 "--printing=no", "--webcam=no", "--speaker=off", "--microphone=off", "--notifications=no",
                                 "--dbus=no", "--system-tray=no", "--start-new-commands=no", "--html=off", "--remote-logging=no", "--resize-display=no",
                                 "--xvfb=Xvfb -screen 0 8192x8192x24 -nolisten tcp -noreset"]),
                                env=xpra_environment, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        processes.append(xpra)
        deadline = time.monotonic() + 15
        while not (ROOT / "xpra.sock").exists():
            require(xpra.poll() is None and time.monotonic() < deadline and not stopping.is_set(), "原生窗口服务启动失败")
            time.sleep(0.05)
        chrome = subprocess.Popen(child_arguments("chrome", chrome_arguments), env=chrome_environment, pass_fds=(3, 4), stdin=subprocess.DEVNULL,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        processes.append(chrome)
        os.close(3)
        os.close(4)
        while not stopping.wait(0.1):
            if chrome.poll() is not None:
                require(chrome.returncode == 0, "原生 Chrome 异常退出")
                return
            require(xpra.poll() is None, "原生窗口服务异常退出")
    finally:
        for process in reversed(processes):
            stop_process(process)


if __name__ == "__main__":
    try:
        if sys.argv[1] == "guarded":
            guard_parent(int(sys.argv[2]))
            sys.argv = [sys.argv[0], *sys.argv[3:]]
        if sys.argv[1] == "chrome":
            require(sys.argv[2] == "/usr/bin/google-chrome", "浏览器启动程序无效")
            # 官方启动脚本会创建日志 cat 子进程并继承调试管道；直接执行同一安装包的 Chrome ELF，避免退出后管道仍被占用。
            executable = Path(sys.argv[2]).resolve().with_name("chrome")
            identity = executable.stat()
            require(stat.S_ISREG(identity.st_mode) and identity.st_uid == 0 and not identity.st_mode & 0o022, "Chrome 程序身份无效")
            os.execv(str(executable), [str(executable), *sys.argv[3:]])
        if sys.argv[1] == "serve":
            sys.exit(serve_xpra(["xpra", *sys.argv[2:]]))
        if sys.argv[1] == "client":
            sys.exit(attach_client(["xpra", *sys.argv[2:]]))
        require(sys.argv[1] == "start", "原生浏览器启动模式无效")
        supervise(sys.argv[2:])
    except Exception:
        print("原生浏览器服务失败", file=sys.stderr)
        sys.exit(1)
