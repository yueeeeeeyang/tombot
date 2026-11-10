"""验证关联域名同时传给原生签名与应用，并隔离和回收临时配置。"""
import argparse
from pathlib import Path
import unittest
from unittest.mock import patch
import build_ios_links


class BuildLinksTest(unittest.TestCase):
    def test_host_boundary(self):
        self.assertEqual(build_ios_links.domain("links.example.test"), "links.example.test")
        for invalid in ("", "local", "https://a.test", "a..test", "-a.test", "a.test:80", "a.test\nFOO=1"):
            with self.subTest(host=invalid), self.assertRaises(argparse.ArgumentTypeError):
                build_ios_links.domain(invalid)

    def test_build_inputs_and_cleanup_after_failure(self):
        paths = []
        def invoke(command, *, cwd, env, check, timeout):
            self.assertIn("--dart-define=TOM_LINK_HOST=links.example.test", command)
            self.assertIn("--no-codesign", command)
            path = Path(env["XCODE_XCCONFIG_FILE"])
            self.assertIn("TOM_LINK_HOST = links.example.test\n", path.read_text())
            self.assertIn("Runner/AssociatedDomains.entitlements", path.read_text())
            paths.append(path)
            raise TimeoutError()
        with patch.dict("os.environ", {}, clear=True), patch("subprocess.run", side_effect=invoke), self.assertRaises(TimeoutError):
            build_ios_links.build("links.example.test", "debug", False)
        self.assertFalse(paths[0].exists())
