"""验证存储验收的权限边界及失败资源保留，不能自动升级验收权限。"""

import argparse
import contextlib
import io
import json
import unittest
from unittest.mock import patch

import verify_storage


class StorageVerificationTest(unittest.TestCase):
    def test_modes_have_explicit_privilege_boundary(self):
        regular = verify_storage.container_arguments("owned", "owned-volume", "fixed-image", "image")
        self.assertNotIn("--privileged", regular)
        self.assertEqual(regular[regular.index("--cap-drop") + 1], "ALL")
        self.assertEqual(regular[regular.index("--network") + 1], "none")
        self.assertIn("--read-only", regular)
        self.assertNotIn("--volume", regular)
        kernel = verify_storage.container_arguments("owned", "owned-volume", "fixed-image", "kernel")
        self.assertIn("--privileged", kernel)
        with self.assertRaises(ValueError):
            verify_storage.container_arguments("owned", "owned-volume", "fixed-image", "unknown")

    def test_kernel_mode_needs_explicit_authorization(self):
        options = argparse.Namespace(docker="/usr/bin/docker", socket="/run/docker.sock",
                                     image="example.test/image@sha256:" + "a" * 64,
                                     mode="kernel", allow_kernel=False)
        with patch("verify_storage.invoke") as invoke, self.assertRaises(ValueError):
            verify_storage.run(options)
        invoke.assert_not_called()

    def test_kernel_failure_preserves_images_and_control_records(self):
        for mode in ("image", "kernel"):
            with self.subTest(mode=mode):
                operations = []
                resources = {}

                def execute(command, **options):
                    if command[0] == "go":
                        return ""
                    arguments = command[5:]
                    operations.append(arguments)
                    if arguments[:2] == ["image", "inspect"]:
                        return "linux arm64\n"
                    if arguments[:2] == ["volume", "create"]:
                        resources["volume"] = arguments[-1]
                        return arguments[-1] + "\n"
                    if arguments[0] == "create":
                        resources["container"] = arguments[arguments.index("--name") + 1]
                        return "a" * 64 + "\n"
                    if arguments[0] == "exec":
                        raise RuntimeError("测试程序未完成回收")
                    if arguments[0] == "inspect" or arguments[:2] == ["volume", "inspect"]:
                        return json.dumps({"io.tombot.storage-test": resources["container"]})
                    return ""

                options = argparse.Namespace(docker="/usr/bin/docker", socket="/run/docker.sock",
                                             image="example.test/image@sha256:" + "a" * 64,
                                             mode=mode, allow_kernel=True)
                with patch("verify_storage.invoke", side_effect=execute), contextlib.redirect_stdout(io.StringIO()), self.assertRaises(RuntimeError):
                    verify_storage.run(options)
                self.assertTrue(any(operation[0] == "stop" for operation in operations))
                removed = any(operation[:2] == ["volume", "rm"] for operation in operations)
                self.assertEqual(removed, mode == "image")


if __name__ == "__main__":
    unittest.main()
