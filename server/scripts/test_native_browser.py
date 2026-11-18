"""原生浏览器归属、桌面输入和启动边界的回归测试。"""

import importlib.util
from pathlib import Path
import stat
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch


ROOT = Path(__file__).resolve().parents[1] / "deploy/computer"
spec = importlib.util.spec_from_file_location("native_browser_policy", ROOT / "native_browser_policy.py")
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
launcher_spec = importlib.util.spec_from_file_location("native_browser", ROOT / "native_browser.py")
launcher = importlib.util.module_from_spec(launcher_spec)
launcher_spec.loader.exec_module(launcher)


class WindowOwnershipTest(unittest.TestCase):
    def setUp(self):
        self.owners = policy.WindowOwners()
        self.owners.bind(101, "first")
        self.owners.bind(202, "second")

    def test_windows_and_popups_never_cross_desktops(self):
        self.assertTrue(self.owners.allows(101, "first"))
        self.assertFalse(self.owners.allows(101, "second"))
        self.assertFalse(self.owners.allows(303, "first"))
        self.assertFalse(self.owners.allows(303, None))
        self.assertTrue(self.owners.inherit(303, 101))
        self.assertTrue(self.owners.allows(303, "first"))
        self.assertFalse(self.owners.inherit(404, 999))
        with self.assertRaises(ValueError):
            self.owners.bind(101, "second")
        self.owners.remove(303)
        self.assertFalse(self.owners.allows(303, "first"))

    def test_keyboard_pointer_and_window_operations_require_ownership(self):
        for kind in ("keyboard-event", "window-map", "window-close", "window-refresh", "window-focus"):
            self.assertTrue(self.owners.accepts("first", [kind, 101]))
            self.assertFalse(self.owners.accepts("first", [kind, 202]))
            self.assertFalse(self.owners.accepts("first", [kind, 999]))
        for kind in ("pointer-motion", "pointer-button", "pointer"):
            self.assertTrue(self.owners.accepts("first", [kind, -1, 1, 101]))
            self.assertFalse(self.owners.accepts("first", [kind, -1, 1, 202]))
        self.assertTrue(self.owners.accepts("first", ["window-draw-ack", 1, 101]))
        self.assertFalse(self.owners.accepts("first", ["window-draw-ack", 1, 202]))
        self.assertFalse(self.owners.accepts("first", ["window-configure", 101, {"pointer": {"wid": 202}}]))

    def test_global_command_and_data_channels_are_denied(self):
        for kind in ("shutdown-server", "exit-server", "start-command", "command-request", "info-request",
                     "clipboard-token", "send-file", "send-file-chunk", "print", "window-signal", "window-action"):
            self.assertFalse(self.owners.accepts("first", [kind, 101]))
        self.assertFalse(self.owners.accepts("first", ["pointer-button"]))
        self.assertFalse(self.owners.accepts("first", ["window-focus", "101"]))
        self.assertTrue(self.owners.accepts("first", ["ping", 1]))


class BrowserStartupEnvironmentTest(unittest.TestCase):
    def test_nss_preload_is_limited_to_xpra_and_xauth(self):
        xpra, chrome = MagicMock(), MagicMock(returncode=0)
        chrome.poll.return_value = 0
        inherited = {
            "LD_PRELOAD": "/test/inherited.so",
            "NSS_WRAPPER_PASSWD": "/test/passwd",
            "NSS_WRAPPER_GROUP": "/test/group",
        }
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory)
            (runtime / "xpra.sock").touch()
            root = MagicMock(spec=Path)
            root.is_absolute.return_value = True
            root.resolve.return_value = root
            root.lstat.return_value = SimpleNamespace(st_uid=30000, st_mode=stat.S_IFDIR | 0o700)
            root.__truediv__.side_effect = lambda name: runtime / name
            with (
                patch.object(launcher, "ROOT", root),
                patch.object(launcher.os, "getuid", return_value=30000),
                patch.object(launcher.os, "getgid", return_value=20000),
                patch.dict(launcher.os.environ, inherited),
                patch.object(launcher.os, "close"),
                patch.object(launcher.signal, "signal"),
                patch.object(launcher, "stop_process"),
                patch.object(launcher.subprocess, "run") as xauth,
                patch.object(launcher.subprocess, "Popen", side_effect=[xpra, chrome]) as start,
            ):
                launcher.supervise(["/usr/bin/google-chrome", "--remote-debugging-pipe"])
                self.assertEqual(launcher.os.environ["LD_PRELOAD"], inherited["LD_PRELOAD"])
            xpra_call, chrome_call = start.call_args_list
            self.assertEqual(xpra_call.args[0][6], "serve")
            self.assertEqual(chrome_call.args[0][6], "chrome")
            xpra_environment = xpra_call.kwargs["env"]
            self.assertEqual(xauth.call_args.kwargs["env"], xpra_environment)
            self.assertEqual(xpra_environment["LD_PRELOAD"], "/usr/lib/tombot-nss-wrapper.so")
            chrome_environment = chrome_call.kwargs["env"]
            self.assertEqual(chrome_environment["DISPLAY"], xpra_environment["DISPLAY"])
            self.assertEqual(chrome_environment["XAUTHORITY"], xpra_environment["XAUTHORITY"])
            self.assertTrue(inherited.keys().isdisjoint(chrome_environment))
            self.assertEqual(chrome_call.kwargs["pass_fds"], (3, 4))


if __name__ == "__main__":
    unittest.main()
