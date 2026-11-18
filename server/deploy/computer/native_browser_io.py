"""在指定 Bot 的 X11 桌面上操作转发的 Chrome 窗口，不调用 DOM 输入接口。"""

import base64
import io
import json
import subprocess
import sys
import time


def command(arguments, text=None):
    subprocess.run(["/usr/bin/xdotool", *arguments], input=text, text=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=8)


def find_window(display, xid):
    """受信客户端写入原生窗口号，避免按网页标题猜测。"""
    atom = display.intern_atom("TOMBOT_WINDOW_ID", only_if_exists=True)
    pending = [display.screen().root]
    count = 0
    while pending:
        window = pending.pop()
        count += 1
        if count > 4096:
            raise RuntimeError("桌面窗口数量超过限制")
        prop = window.get_full_property(atom, 0) if atom else None
        if prop and len(prop.value) == 1 and prop.value[0] == xid:
            return window
        pending.extend(window.query_tree().children)
    return None


def execute(request):
    from Xlib import display as xdisplay, X
    from PIL import Image
    display = xdisplay.Display()
    try:
        window = find_window(display, request["xid"])
        if window is None:
            return {"state": "starting"}
        action = request["action"]
        if action == "inspect":
            command(["windowmove", str(window.id), "20", "30"])
            return {"state": "ready"}
        if action == "position":
            position = display.screen().root.translate_coords(window, 0, 0)
            return {"state": "ready", "x": position.x, "y": position.y}
        command(["windowactivate", "--sync", str(window.id)])
        if action == "capture":
            # 捕获用户实际看到的整块桌面，输入坐标和画面使用同一个坐标系。
            time.sleep(0.08)
            root = display.screen().root
            bounds = root.get_geometry()
            if (bounds.width, bounds.height) != (1280, 800):
                raise RuntimeError("桌面尺寸不符合约定")
            frame = root.get_image(0, 0, 1280, 800, X.ZPixmap, 0xFFFFFFFF)
            image = Image.frombytes("RGB", (1280, 800), frame.data, "raw", "BGRX")
            encoded = io.BytesIO()
            image.save(encoded, format="PNG")
            return {"state": "ready", "png": base64.b64encode(encoded.getvalue()).decode("ascii")}
        if action != "input":
            raise ValueError("原生浏览器动作无效")
        operation = request["command"]
        kind = operation["kind"]
        if kind == "navigate":
            command(["key", "--clearmodifiers", "ctrl+l"])
            command(["type", "--clearmodifiers", "--delay", "0", "--file", "-"], operation["url"])
            command(["key", "--clearmodifiers", "Return"])
        elif kind in {"back", "forward", "reload"}:
            command(["key", "--clearmodifiers", {"back": "alt+Left", "forward": "alt+Right", "reload": "ctrl+r"}[kind]])
        elif kind == "type":
            if len(operation["text"]) > 256:
                raise ValueError("每次键入最多 256 个字符")
            # 保留正常键入间隔，使动态 Unicode 映射在按键到达前由桌面处理。
            command(["type", "--clearmodifiers", "--delay", "12", "--file", "-"], operation["text"])
        elif kind == "key":
            names = {"Enter": "Return", "Escape": "Escape", "Backspace": "BackSpace", "Delete": "Delete",
                     "ArrowLeft": "Left", "ArrowRight": "Right", "ArrowUp": "Up", "ArrowDown": "Down",
                     "PageUp": "Prior", "PageDown": "Next", "Tab": "Tab", "Home": "Home", "End": "End"}
            key = names.get(operation["key"], operation["key"].lower())
            modifiers = operation.get("modifiers", 0)
            prefix = [name for bit, name in ((1, "alt"), (2, "ctrl"), (4, "super"), (8, "shift")) if modifiers & bit]
            command(["key", "--clearmodifiers", "+".join([*prefix, key])])
        elif kind in {"move", "click", "double_click", "scroll"}:
            x, y = operation.get("x", 0), operation.get("y", 0)
            if not (0 <= x < 1280 and 0 <= y < 800):
                raise ValueError("桌面坐标无效")
            command(["mousemove", str(x), str(y)])
            if kind in {"click", "double_click"}:
                command(["click", "--repeat", "2" if kind == "double_click" else "1", "--delay", "100", "1"])
            elif kind == "scroll":
                for amount, positive, negative in ((operation.get("delta_y", 0), 5, 4), (operation.get("delta_x", 0), 7, 6)):
                    if abs(amount) > 2000:
                        raise ValueError("滚动距离超过限制")
                    if amount:
                        command(["click", "--repeat", str(max(1, (abs(amount) + 79) // 80)), "--delay", "10", str(positive if amount > 0 else negative)])
        else:
            raise ValueError("原生浏览器输入类型无效")
        return {"state": "ready"}
    finally:
        display.close()


if __name__ == "__main__":
    try:
        encoded = sys.stdin.buffer.read(32769)
        if len(encoded) > 32768:
            raise ValueError("输入超过限制")
        print(json.dumps(execute(json.loads(encoded))))
    except Exception:
        # 用户输入可能含登录信息，不输出异常原文或子进程命令。
        print("原生浏览器桌面操作失败", file=sys.stderr)
        sys.exit(1)
