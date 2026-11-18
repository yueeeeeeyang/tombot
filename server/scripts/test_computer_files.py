"""验证远程文件工具的字节完整性、原子发布和目录隔离。"""
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("computer_files", Path(__file__).parents[1] / "deploy/computer/files.py")
FILES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FILES)


class ComputerFilesTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)

    def write(self, name, content):
        FILES.write_file(self.root, name, len(content), hashlib.sha256(content).hexdigest(), io.BytesIO(content))

    def test_binary_and_empty_round_trip(self):
        for content in (b"", bytes(range(256)) * 400, b"\xff\x00PDF\r\n"):
            name = f"Library/{len(content)}/文件.pdf"
            self.write(name, content)
            output = io.BytesIO()
            FILES.read_file(self.root, name, output)
            self.assertEqual(output.getvalue(), content)
        self.assertFalse(list(self.root.rglob(".tombot-import-*")))

    def test_atomic_publish_refuses_overwrite_and_removes_staging(self):
        self.write("report.docx", b"original")
        with self.assertRaises(FileExistsError):
            self.write("report.docx", b"replacement")
        self.assertEqual((self.root / "report.docx").read_bytes(), b"original")
        self.assertFalse(list(self.root.glob(".tombot-import-*")))

    def test_traversal_and_symlinks_are_rejected(self):
        (self.root / "real").mkdir()
        (self.root / "real/file").write_bytes(b"private")
        (self.root / "linked").symlink_to(self.root / "real", target_is_directory=True)
        (self.root / "file-link").symlink_to(self.root / "real/file")
        for name in ("../escape", "/absolute", "a/../escape", "a//b", "./file", "a\\b", "linked/file", "file-link", "incomplete.crdownload"):
            with self.subTest(name=name):
                with self.assertRaises((OSError, ValueError)):
                    self.write(name, b"new")
                with self.assertRaises((OSError, ValueError)):
                    FILES.read_file(self.root, name, io.BytesIO())
        self.assertEqual((self.root / "real/file").read_bytes(), b"private")

    def test_size_digest_and_non_regular_files(self):
        with self.assertRaises(ValueError):
            FILES.write_file(self.root, "wrong", 3, hashlib.sha256(b"abc").hexdigest(), io.BytesIO(b"def"))
        for size in (-1, FILES.MAXIMUM_BYTES + 1):
            with self.assertRaises(ValueError):
                FILES.write_file(self.root, "wrong", size, "", io.BytesIO())
        with (self.root / "large").open("wb") as stream:
            stream.truncate(FILES.MAXIMUM_BYTES + 1)
        os.mkfifo(self.root / "fifo")
        for name in ("large", "fifo"):
            with self.assertRaises(ValueError):
                FILES.read_file(self.root, name, io.BytesIO())
        self.assertFalse((self.root / "wrong").exists())

    def test_failed_publish_never_leaves_partial_destination(self):
        with patch.object(FILES.os, "link", side_effect=OSError("发布失败")):
            with self.assertRaises(OSError):
                self.write("report.pdf", b"content")
        self.assertEqual(list(self.root.iterdir()), [])

    def test_changed_file_is_not_delivered(self):
        self.write("changing", b"original")
        original_fstat = os.fstat
        calls = 0

        def changing_fstat(descriptor):
            nonlocal calls
            calls += 1
            if calls == 2:
                (self.root / "changing").write_bytes(b"changed-length")
            return original_fstat(descriptor)

        output = io.BytesIO()
        with patch.object(FILES.os, "fstat", side_effect=changing_fstat):
            with self.assertRaises(ValueError):
                FILES.read_file(self.root, "changing", output)
        self.assertEqual(output.getvalue(), b"")


if __name__ == "__main__":
    unittest.main()
