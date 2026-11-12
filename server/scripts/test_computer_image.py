"""校验镜像构建边界与桌面启动参数，无需启动 Docker。"""

import importlib.util
import argparse
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as XML

from build_computer_image import ENVIRONMENT_FILE, IMAGE_FILES, ROOT, image_tag


specification = importlib.util.spec_from_file_location("computer_desktop", ROOT / "deploy/computer/desktop.py")
desktop = importlib.util.module_from_spec(specification)
specification.loader.exec_module(desktop)
installation = importlib.util.spec_from_file_location("install_computer_tools", ROOT / "deploy/computer/install_tools.py")
installer = importlib.util.module_from_spec(installation)
installation.loader.exec_module(installer)


def prepare_identity(runtime):
    """只准备测试身份夹具，正式目录创建由代理资源组生命周期负责。"""
    uid, gid = os.getuid(), os.getgid()
    os.chown(runtime, uid, gid)
    for name, content in (("passwd", f"bot{uid}:x:{uid}:{gid}:Bot:/workspace:/bin/bash\n"),
                          ("group", f"bot:x:{gid}:\n")):
        path = runtime / name
        path.write_text(content)
        path.chmod(0o600)
        os.chown(path, uid, gid)
    for name in ("config", "cache", "state", "data", "tmp"):
        path = runtime / name
        path.mkdir(mode=0o700)
        os.chown(path, uid, gid)


class ComputerImageTest(unittest.TestCase):
    def test_ready_marker_is_published_as_a_complete_private_file(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory)
            desktop.write_ready(runtime, 7)
            self.assertEqual(json.loads((runtime / "ready.json").read_text()),
                             {"display": 7, "width": 1280, "height": 800, "protocol": "x11"})
            self.assertEqual((runtime / "ready.json").stat().st_mode & 0o777, 0o600)
            self.assertFalse((runtime / "ready.pending").exists())
            (runtime / "ready.pending").write_text("unfinished")
            with self.assertRaises(FileExistsError):
                desktop.write_ready(runtime, 8)
            self.assertEqual(json.loads((runtime / "ready.json").read_text())["display"], 7)

    def test_context_only_contains_image_inputs(self):
        self.assertTrue(ENVIRONMENT_FILE.is_file())
        for name in IMAGE_FILES:
            self.assertTrue((ROOT / "deploy/computer" / name).is_file())
        for name in ("latest", "../secret:tag", "image:tag --privileged", "image:tag\nother"):
            with self.assertRaises(argparse.ArgumentTypeError):
                image_tag(name)
        self.assertEqual(image_tag("tombot-computer:trixie"), "tombot-computer:trixie")

    def test_desktop_protocol_has_no_tcp_or_access_control_bypass(self):
        arguments = desktop.xserver_arguments(Path("/run/bot-desktops/a"), 7)
        for name, value in (("-geometry", "1280x800"), ("-rfbport", "-1"), ("-nolisten", "tcp"),
                            ("-rfbunixmode", "0600"), ("-auth", "/run/bot-desktops/a/Xauthority")):
            self.assertEqual(arguments[arguments.index(name) + 1], value)
        self.assertIn("-AcceptSetDesktopSize=0", arguments)
        self.assertNotIn("-ac", arguments)

    def test_shared_home_private_desktop_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory).resolve()
            prepare_identity(runtime)
            with patch.object(desktop, "ENVIRONMENT_FILE", ENVIRONMENT_FILE), patch.dict(os.environ, {"DBUS_SESSION_BUS_ADDRESS": "foreign", "DISPLAY": ":900"}):
                environment = desktop.session_environment(runtime, 7)
            self.assertEqual(environment["HOME"], "/workspace")
            self.assertEqual(environment["DISPLAY"], ":7")
            for path in ("/workspace/tools/go/bin", "/workspace/tools/cargo/bin", "/workspace/tools/pnpm/bin"):
                self.assertIn(path, environment["PATH"].split(":"))
            for name in ("XAUTHORITY", "ICEAUTHORITY", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME"):
                self.assertTrue(environment[name].startswith(str(runtime)))
            self.assertEqual(environment["DBUS_SESSION_BUS_ADDRESS"], f"unix:path={runtime}/bus")
            self.assertEqual((runtime / "passwd").stat().st_mode & 0o777, 0o600)
            self.assertEqual((runtime / "tmp").stat().st_mode & 0o777, 0o700)
            with patch.object(desktop, "ENVIRONMENT_FILE", ENVIRONMENT_FILE):
                self.assertEqual(desktop.session_environment(runtime, 7), environment)

    def test_private_directory_rejects_links_and_group_access(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            os.chown(root, os.getuid(), os.getgid())
            desktop.private_directory(root)
            link = root / "link"
            link.symlink_to(root, target_is_directory=True)
            with self.assertRaises(RuntimeError):
                desktop.private_directory(link)
            root.chmod(0o750)
            with self.assertRaises(RuntimeError):
                desktop.private_directory(root)

    def test_desktop_environment_rejects_missing_or_invalid_template(self):
        for contents in (None, "{", "null", "{}", '{"HOME":42}', '{"DISPLAY":"{{unknown}}"}'):
            with self.subTest(contents=contents), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                runtime = root / "runtime"
                runtime.mkdir(mode=0o700)
                prepare_identity(runtime)
                template = root / "environment.json"
                if contents is not None:
                    template.write_text(contents)
                with patch.object(desktop, "ENVIRONMENT_FILE", template), self.assertRaises(RuntimeError):
                    desktop.session_environment(runtime, 7)

    def test_desktop_requires_prepared_identity_without_repair(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory).resolve()
            with self.assertRaises(FileNotFoundError):
                desktop.session_environment(runtime, 7)
            prepare_identity(runtime)
            identity = runtime / "passwd"
            identity.chmod(0o640)
            with self.assertRaisesRegex(RuntimeError, "权限无效"):
                desktop.session_environment(runtime, 7)
            self.assertEqual(identity.stat().st_mode & 0o777, 0o640)
            identity.chmod(0o600)
            identity.write_text("foreign-example")
            with self.assertRaisesRegex(RuntimeError, "当前 Bot 不符"):
                desktop.session_environment(runtime, 7)
            identity.unlink()
            identity.symlink_to(runtime / "group")
            with self.assertRaises(OSError):
                desktop.session_environment(runtime, 7)

    def test_xfce_configuration_requires_window_manager(self):
        document = XML.parse(ROOT / "deploy/computer/xfce4-session.xml")
        self.assertEqual(document.find(".//property[@name='FailsafeSessionName']").get("value"), "Failsafe")
        self.assertEqual(document.find(".//property[@name='Client0_Command']/value").get("value"), "xfwm4")
        for name in ("xfwm4.xml", "xfce4-desktop.xml"):
            XML.parse(ROOT / "deploy/computer" / name)

    def test_tools_are_locked_and_downloads_are_verified(self):
        lock = json.loads((ROOT / "deploy/computer/tools.lock.json").read_text())
        self.assertTrue(lock["node"]["version"].startswith("24."))
        self.assertTrue(lock["go"]["version"].startswith("1.27."))
        for name in installer.HOSTS:
            for architecture in ("amd64", "arm64"):
                installer.validate_archive(name, lock[name][architecture])
        for address in ("http://nodejs.org/package", "https://example.com/package", "https://nodejs.org@other.test/file"):
            with self.assertRaises(ValueError):
                installer.validate_archive("node", {"url": address, "sha256": "0" * 64})
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "archive"
            archive.write_bytes(b"verified package")
            installer.verify_digest(archive, hashlib.sha256(archive.read_bytes()).hexdigest())
            with self.assertRaises(ValueError):
                installer.verify_digest(archive, "0" * 64)

    def test_browser_seccomp_only_adds_required_namespace_calls(self):
        profile = json.loads((ROOT / "deploy/computer/seccomp.json").read_text())
        source = json.loads((ROOT / "deploy/computer/seccomp-source.json").read_text())
        extra = profile["syscalls"].pop(0)
        self.assertEqual(extra["names"], source["addedSyscalls"])
        self.assertEqual(set(extra["names"]), {"clone", "setns", "unshare", "chroot"})
        self.assertEqual(extra["action"], "SCMP_ACT_ALLOW")
        self.assertEqual(profile["defaultAction"], "SCMP_ACT_ERRNO")
        for rule in profile["syscalls"]:
            rule.pop("comment", None)
        encoded = json.dumps(profile, sort_keys=True, separators=(",", ":")).encode()
        self.assertEqual(hashlib.sha256(encoded).hexdigest(), source["basePolicySha256"])


if __name__ == "__main__":
    unittest.main()
