"""在独立临时目录验证镜像初始化，不启动 Docker、不接触宿主运行目录。"""

import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

from build_computer_image import ROOT


specification = importlib.util.spec_from_file_location("prepare_computer", ROOT / "deploy/computer/prepare.py")
preparation = importlib.util.module_from_spec(specification)
specification.loader.exec_module(preparation)


class PrepareComputerTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name).resolve()
        # macOS 会继承临时目录父级的组；显式准备测试归属，不能依赖 TMPDIR 恰好使用当前主组。
        os.chown(self.root, os.getuid(), os.getgid())
        self.workspace, self.runtime, self.temporary = [self.root / name for name in ("workspace", "run", "tmp")]
        for path, mode in ((self.workspace, 0o2770), (self.runtime, 0o755), (self.temporary, 0o1777)):
            path.mkdir()
            path.chmod(mode)

    def prepare(self):
        preparation.prepare_layout(self.workspace, self.runtime, self.temporary, os.getuid(), os.getgid(), os.getgid())

    def test_fresh_layout_and_restart_preserve_business_files(self):
        self.prepare()
        (self.workspace / "business.txt").write_text("共享产出")
        (self.workspace / "tools/bin/program").write_text("共享工具")
        self.prepare()
        self.assertEqual((self.workspace / "business.txt").read_text(), "共享产出")
        self.assertEqual((self.workspace / "tools/bin/program").read_text(), "共享工具")
        for path in (self.workspace, self.workspace / "tools", self.workspace / "tools/bin", self.workspace / ".tmp"):
            self.assertEqual(path.stat().st_mode & 0o7777, 0o2770)
        self.assertEqual((self.runtime / "bot-desktops").stat().st_mode & 0o7777, 0o711)
        self.assertEqual((self.runtime / "tombot-browser").stat().st_mode & 0o7777, 0o711)
        for name in (".X11-unix", ".ICE-unix"):
            self.assertEqual((self.temporary / name).stat().st_mode & 0o7777, 0o1777)

    def test_legacy_data_is_not_converted(self):
        self.workspace.chmod(0o755)
        original = self.workspace / "legacy.txt"
        original.write_text("不得修改")
        with self.assertRaisesRegex(RuntimeError, "当前布局"):
            self.prepare()
        self.assertEqual(original.read_text(), "不得修改")
        self.assertEqual(self.workspace.stat().st_mode & 0o7777, 0o755)
        self.assertFalse((self.workspace / "tools").exists())

    def test_browser_profile_identity_and_contents(self):
        browser = self.root / "browser"
        browser.mkdir(mode=0o700)
        contents = browser / "saved-state"
        contents.write_text("保留浏览器状态")
        preparation.verify_browser_directory(browser, self.workspace, os.getuid(), os.getgid())
        self.assertEqual(contents.read_text(), "保留浏览器状态")
        browser.chmod(0o770)
        with self.assertRaisesRegex(RuntimeError, "身份或权限"):
            preparation.verify_browser_directory(browser, self.workspace, os.getuid(), os.getgid())
        self.assertEqual(browser.stat().st_mode & 0o7777, 0o770)
        browser.chmod(0o700)
        linked = self.root / "linked-browser"
        linked.symlink_to(browser, target_is_directory=True)
        with self.assertRaises(OSError):
            preparation.verify_browser_directory(linked, self.workspace, os.getuid(), os.getgid())
        with self.assertRaisesRegex(RuntimeError, "不同子目录"):
            preparation.verify_browser_directory(browser, browser, os.getuid(), os.getgid())

    def test_filesystem_recovery_directory_is_preserved(self):
        recovery = self.workspace / "lost+found"
        recovery.mkdir(mode=0o700)
        self.prepare()
        self.assertEqual(recovery.stat().st_mode & 0o7777, 0o700)

    def test_partial_empty_initialization_can_resume(self):
        self.prepare()
        (self.workspace / ".tmp").chmod(0o700)
        self.prepare()
        self.assertEqual((self.workspace / ".tmp").stat().st_mode & 0o7777, 0o2770)
        (self.workspace / ".tmp").chmod(0o2700)
        self.prepare()
        self.assertEqual((self.workspace / ".tmp").stat().st_mode & 0o7777, 0o2770)
        (self.workspace / ".tmp").chmod(0o700)
        (self.workspace / ".tmp/file").write_text("保留")
        with self.assertRaises(RuntimeError):
            self.prepare()
        self.assertEqual((self.workspace / ".tmp/file").read_text(), "保留")

    def test_symlinks_and_permission_drift_are_rejected(self):
        outside = self.root / "outside"
        outside.mkdir(mode=0o700)
        self.prepare()
        tools = self.workspace / "tools"
        tools.rename(self.workspace / "original-tools")
        tools.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(OSError):
            self.prepare()
        self.assertEqual(outside.stat().st_mode & 0o7777, 0o700)
        tools.unlink()
        (self.workspace / "original-tools").rename(tools)
        tools.chmod(0o777)
        with self.assertRaises(RuntimeError):
            self.prepare()
        self.assertEqual(tools.stat().st_mode & 0o7777, 0o777)
        tools.chmod(0o2770)
        self.runtime.chmod(0o777)
        with self.assertRaisesRegex(RuntimeError, "运行根目录"):
            self.prepare()


if __name__ == "__main__":
    unittest.main()
