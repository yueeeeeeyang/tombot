"""共享 Chrome 的原生窗口归属；默认拒绝尚未由受信代理绑定的窗口。"""


class WindowOwners:
    """归属在原生窗口存活期间不可转移，子弹窗仅继承已知父窗口。"""

    def __init__(self):
        self.owners = {}

    def bind(self, xid, owner):
        if not isinstance(xid, int) or xid <= 0 or not owner:
            raise ValueError("窗口归属无效")
        if xid in self.owners and self.owners[xid] != owner:
            raise ValueError("不能转移已有窗口")
        self.owners[xid] = owner

    def allows(self, xid, owner):
        return bool(owner) and self.owners.get(xid) == owner

    def inherit(self, xid, parent):
        if parent in self.owners:
            self.bind(xid, self.owners[parent])
            return True
        return False

    def remove(self, xid):
        self.owners.pop(xid, None)

    def accepts(self, owner, packet):
        """固定 Xpra 6.5 协议；全局控制、命令、信息、文件和剪贴板入口不对客户端开放。"""
        kind = packet[0]
        # Xpra 6.5 客户端部分消息仍使用这些固定线协议名称。
        kind = {"map-window": "window-map", "unmap-window": "window-unmap", "close-window": "window-close",
                "focus": "window-focus", "damage-sequence": "window-draw-ack", "key-action": "keyboard-event",
                "pointer-position": "pointer-wheel", "button-action": "pointer-wheel",
                "buffer-refresh": "window-refresh", "pointer": "pointer-motion"}.get(kind, kind)
        if kind in {"ping", "ping_echo", "ping-echo", "connection-data", "disconnect",
                    "set_deflate", "encoding", "encodings", "desktop_size", "desktop-size",
                    "layout-changed", "keymap-changed", "keyboard-config", "keyboard-sync",
                    "set-keyboard-sync-enabled", "keyboard-sync-enabled-status"}:
            return True
        index = 1
        if kind in {"pointer-motion", "pointer-button"}:
            index = 3
        elif kind == "window-draw-ack":
            index = 2
        elif kind not in {"window-map", "window-unmap", "window-configure", "window-close",
                          "window-focus", "window-refresh", "keyboard-event", "key-repeat",
                          "pointer-wheel", "configure-window"}:
            return False
        if len(packet) <= index or not isinstance(packet[index], int):
            return False
        xid = packet[index]
        if kind == "window-focus" and xid == 0:
            return True
        if not self.allows(xid, owner):
            return False
        if kind == "configure-window" and len(packet) > 10 and packet[10] and not self.allows(packet[10], owner):
            return False
        if kind == "window-configure" and len(packet) > 2 and isinstance(packet[2], dict):
            pointer = packet[2].get("pointer")
            if pointer and not self.allows(pointer.get("wid"), owner):
                return False
        return True
