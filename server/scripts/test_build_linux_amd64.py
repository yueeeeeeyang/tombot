"""验证 Linux x64 打包编排，测试夹具不生成可部署的软件。"""

import os
from pathlib import Path
import stat
import struct
import sys
import tempfile
import unittest
from unittest.mock import patch

import build_linux_amd64 as packaging
from release_bundle import ReleaseError, snapshot_sources
from test_release_bundle import make_source


class LinuxAMD64PackagingTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.worktree = self.directory / "worktree"
        make_source(self.worktree)
        self.source_file = self.worktree / "server/cmd/tomd/main.go"
        self.source_file.parent.mkdir(parents=True)
        self.source_file.write_bytes(b"current worktree source")
        self.output_parent = self.directory / "releases"
        self.output_parent.mkdir()
        self.output = self.output_parent / "one"
        self.version = "0.2.0-test"

    def compile_fixture(self, source, artifacts, version, architectures):
        self.assertEqual(version, self.version)
        self.assertEqual(architectures, ("amd64",))
        self.assertNotEqual(source, self.worktree)
        relative = self.source_file.relative_to(self.worktree)
        self.assertEqual((source / relative).read_bytes(), b"current worktree source")
        image = artifacts / "linux-amd64/computer-image"
        image.mkdir(parents=True)
        (image / "Dockerfile").write_bytes(b"image context fixture")
        binary = artifacts / "linux-amd64/bin/tomd"
        binary.parent.mkdir()
        header = bytearray(64)
        header[:7] = b"\x7fELF\x02\x01\x01"
        struct.pack_into("<HH", header, 16, 2, 62)
        binary.write_bytes(header)

    def command_fixture(self, arguments, _directory, _environment, _timeout, _stage):
        if arguments[0] == sys.executable:
            archive = Path(arguments[arguments.index("--output") + 1])
            archive.write_bytes(b"package fixture")

    def test_packages_one_linux_architecture_from_one_snapshot(self):
        digest = snapshot_sources(self.worktree)
        with patch.object(packaging, "compile_artifacts", side_effect=self.compile_fixture) as compile_artifacts, \
                patch.object(packaging, "run_command", side_effect=self.command_fixture) as run, \
                patch.dict(os.environ, {"DOCKER_CONTEXT": "test-docker-context"}):
            receipt = packaging.build(self.worktree, self.output, self.version)

        run.assert_called_once()
        source, artifacts, version = compile_artifacts.call_args.args
        self.assertEqual(compile_artifacts.call_args.kwargs, {"architectures": ("amd64",)})
        args, directory, environment, _, _ = run.call_args.args
        self.assertEqual(args[:-1], [sys.executable, "scripts/node_release.py", "programs", "--bin-dir",
                                     str(artifacts / "linux-amd64/bin"), "--architecture", "amd64",
                                     "--label", self.version, "--output"])
        self.assertEqual(directory, source / "server")
        self.assertEqual(version, self.version)
        self.assertEqual(environment["GOFLAGS"], "-mod=readonly -buildvcs=false")
        self.assertEqual(environment["DOCKER_CONTEXT"], "test-docker-context")
        self.assertNotEqual(Path(args[-1]).parent, self.output)
        self.assertEqual(receipt["source_sha256"], digest)
        self.assertIsNone(receipt["image"])
        self.assertEqual(receipt["node_package"], str(self.output / packaging.NODE_ARCHIVE))
        self.assertEqual(set(p.name for p in self.output.iterdir()), {"tomd", packaging.NODE_ARCHIVE})
        self.assertEqual(stat.S_IMODE((self.output / "tomd").stat().st_mode), 0o755)
        self.assertEqual((self.output / packaging.NODE_ARCHIVE).read_bytes(), b"package fixture")
        self.assertEqual(list(self.output_parent.iterdir()), [self.output])

    def test_image_is_a_separate_optional_product_from_the_same_snapshot(self):
        with patch.object(packaging, "compile_artifacts", side_effect=self.compile_fixture) as compile_artifacts, \
                patch.object(packaging, "run_command", side_effect=self.command_fixture) as run:
            receipt = packaging.build(self.worktree, self.output, self.version, with_image=True)
        source, artifacts, _ = compile_artifacts.call_args.args
        programs, docker, export = [call.args for call in run.call_args_list]
        self.assertEqual(programs[0][2], "programs")
        self.assertEqual(docker[0], ["docker", "buildx", "build", "--load", "--platform", "linux/amd64",
                                     "--tag", receipt["image"], str(artifacts / "linux-amd64/computer-image")])
        self.assertEqual(export[0][2:5], ["image", "--image", receipt["image"]])
        self.assertEqual(export[1], source / "server")
        self.assertEqual(set(p.name for p in self.output.iterdir()),
                         {"tomd", packaging.NODE_ARCHIVE, "computer-image-linux-amd64.tar.gz"})

    def test_failed_image_or_node_packaging_leaves_no_products(self):
        for failing_program in ("docker", sys.executable):
            with self.subTest(failing_program=failing_program):
                def command(arguments, *options):
                    self.command_fixture(arguments, *options)
                    if arguments[0] == failing_program:
                        raise ReleaseError("外部构建失败")

                with patch.object(packaging, "compile_artifacts", side_effect=self.compile_fixture), \
                        patch.object(packaging, "run_command", side_effect=command), \
                        self.assertRaisesRegex(ReleaseError, "外部构建失败"):
                    packaging.build(self.worktree, self.output, self.version, with_image=True)
                self.assertEqual(list(self.output_parent.iterdir()), [])

    def test_existing_output_is_rejected_before_compiling(self):
        self.output.mkdir()
        previous = self.output / "tomd"
        previous.write_bytes(b"previous release")
        with patch.object(packaging, "compile_artifacts") as compile_artifacts, \
                self.assertRaisesRegex(ReleaseError, "输出目录须为新目录"):
            packaging.build(self.worktree, self.output, self.version)
        compile_artifacts.assert_not_called()
        self.assertEqual(previous.read_bytes(), b"previous release")

    def test_changed_snapshot_is_rejected_after_packaging(self):
        def command(arguments, directory, *options):
            self.command_fixture(arguments, directory, *options)
            if arguments[0] == sys.executable:
                (directory / "cmd/tomd/main.go").write_bytes(b"changed input")

        with patch.object(packaging, "compile_artifacts", side_effect=self.compile_fixture), \
                patch.object(packaging, "run_command", side_effect=command), \
                self.assertRaisesRegex(ReleaseError, "构建过程修改了已固定的源码输入"):
            packaging.build(self.worktree, self.output, self.version)
        self.assertEqual(list(self.output_parent.iterdir()), [])
        self.assertEqual(self.source_file.read_bytes(), b"current worktree source")
