"""验证发布集合、源码快照及损坏归档边界；编译夹具不代表实际程序或节点验收。"""

import contextlib
import copy
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import struct
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import release_bundle as release


def make_payload(directory):
    binaries = release.binary_paths()
    paths = release.required_paths() | {"admin/assets/index-test.js", "admin/assets/index-test.css"}
    for name in paths:
        path = directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        content = b"test artifact"
        if name in binaries:
            header = bytearray(64)
            header[:7] = b"\x7fELF\x02\x01\x01"
            struct.pack_into("<HH", header, 16, 2, {"amd64": 62, "arm64": 183}[binaries[name]])
            content = bytes(header)
        if name == "admin/index.html":
            content = b'<script src="/admin/assets/index-test.js"></script>'
        path.write_bytes(content)


def make_source(directory):
    for name in release.SOURCE_DIRECTORIES:
        (directory / name).mkdir(parents=True)
    for name in ("server/go.mod", "server/go.sum", "server/Makefile", "admin/package.json", "ui/theme.css"):
        (directory / name).write_text("source fixture")


def archive_entries(payload, manifest):
    entries = [("manifest.json", release.encode_manifest(manifest), 0o644)]
    entries.extend((name, (payload / name).read_bytes(), record["mode"]) for name, record in manifest["files"].items())
    return entries


def raw_archive(entries, change=None):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name, content, mode in entries:
            header = tarfile.TarInfo(name)
            header.mode, header.size = mode, len(content)
            if change is not None:
                change(header)
            archive.addfile(header, io.BytesIO(content))
    return output.getvalue()


class ReleaseBundleTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.payload = self.directory / "payload"
        make_payload(self.payload)
        self.manifest = release.payload_manifest(self.payload, "0.2.0-rc.1", "a" * 64)

    def test_complete_archive_is_deterministic_and_checked_without_extraction(self):
        paths = [self.directory / name for name in ("one.tar.gz", "two.tar.gz")]
        for path in paths:
            release.archive_payload(self.payload, self.manifest, path)
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            self.assertEqual(release.verify_archive(path, digest), self.manifest)
        self.assertEqual(paths[0].read_bytes(), paths[1].read_bytes())
        self.assertEqual({path.name for path in self.directory.iterdir()}, {"payload", "one.tar.gz", "two.tar.gz"})
        with self.assertRaises(release.ReleaseError):
            release.verify_archive(paths[0], "0" * 64)
        link = self.directory / "link.tar.gz"
        link.symlink_to(paths[0])
        with self.assertRaises(OSError):
            release.verify_archive(link, digest)

    def test_required_host_programs_image_context_and_frontend_cannot_be_missing(self):
        for name in ("linux-amd64/bin/tomd", "linux-arm64/bin/tomnode", "admin/index.html",
                     "linux-arm64/computer-image/bin/bot-agent", "linux-amd64/computer-image/Dockerfile",
                     "linux-arm64/computer-image/desktop-environment.json", "deploy/tomd.service",
                     "linux-amd64/bin/tomdeploy-check", "linux-arm64/bin/tomdeploy-check",
                     "linux-amd64/bin/tomdeploy-config", "linux-arm64/bin/tomdeploy-config",
                     "deploy/CONFIGURE.md",
                     "deploy/systemd/tombot-deployment-check.service"):
            with self.subTest(name=name):
                manifest = copy.deepcopy(self.manifest)
                del manifest["files"][name]
                with self.assertRaises(release.ReleaseError):
                    release.validate_manifest(manifest)

    def test_manifest_rejects_invalid_shapes_limits_and_paths(self):
        for scenario in ("version", "old-format", "platforms", "source", "extra", "boolean-size", "large-file", "mode", "digest", "traversal", "no-assets", "admin-limit"):
            with self.subTest(scenario=scenario):
                manifest = copy.deepcopy(self.manifest)
                file = manifest["files"]["README.md"]
                if scenario == "version":
                    manifest["version"] = "latest"
                elif scenario == "old-format":
                    manifest["format_version"] = 2
                elif scenario == "platforms":
                    manifest["platforms"] = ["linux/arm64"]
                elif scenario == "source":
                    manifest["source_sha256"] = "unknown"
                elif scenario == "extra":
                    manifest["other"] = True
                elif scenario == "boolean-size":
                    file["size"] = True
                elif scenario == "large-file":
                    file["size"] = release.MAX_FILE + 1
                elif scenario == "mode":
                    file["mode"] = 0o777
                elif scenario == "digest":
                    file["sha256"] = "invalid"
                elif scenario == "traversal":
                    manifest["files"]["../outside"] = file
                elif scenario == "no-assets":
                    manifest["files"] = {key: value for key, value in manifest["files"].items() if not key.startswith("admin/assets/")}
                elif scenario == "admin-limit":
                    manifest["files"]["admin/index.html"]["size"] = (64 << 10) + 1
                with self.assertRaises(release.ReleaseError):
                    release.validate_manifest(manifest)

    def test_archive_rejects_extra_duplicate_missing_and_changed_contents(self):
        for scenario in ("extra", "duplicate", "missing", "content", "duplicate-json", "wrong-arch", "wrong-admin-base"):
            with self.subTest(scenario=scenario):
                manifest = copy.deepcopy(self.manifest)
                entries = archive_entries(self.payload, manifest)
                if scenario == "extra":
                    entries.append(("unknown.txt", b"unknown", 0o644))
                elif scenario == "duplicate":
                    entries.append(entries[-1])
                elif scenario == "missing":
                    entries.pop()
                elif scenario == "content":
                    name, content, mode = entries[-1]
                    entries[-1] = name, content[:-1] + bytes([content[-1] ^ 1]), mode
                elif scenario == "duplicate-json":
                    name, content, mode = entries[0]
                    entries[0] = name, b'{"format_version":3,' + content[1:], mode
                else:
                    name = "linux-arm64/bin/tomd" if scenario == "wrong-arch" else "admin/index.html"
                    content = (self.payload / "linux-amd64/bin/tomd").read_bytes() if scenario == "wrong-arch" else b'<script src="/assets/index-test.js"></script>'
                    manifest["files"][name]["sha256"] = hashlib.sha256(content).hexdigest()
                    manifest["files"][name]["size"] = len(content)
                    entries = archive_entries(self.payload, manifest)
                    entries = [(key, content if key == name else body, mode) for key, body, mode in entries]
                with self.assertRaises(release.ReleaseError):
                    release.verify_stream(io.BytesIO(raw_archive(entries)))

    def test_archive_rejects_links_extensions_traversal_and_metadata(self):
        for scenario in ("symlink", "hardlink", "pax", "directory", "traversal", "owner", "time", "mode"):
            with self.subTest(scenario=scenario):
                def change(header):
                    if header.name != "README.md":
                        return
                    if scenario in ("symlink", "hardlink"):
                        header.type = tarfile.SYMTYPE if scenario == "symlink" else tarfile.LNKTYPE
                        header.linkname = "/outside"
                    elif scenario == "pax":
                        header.type = tarfile.XHDTYPE
                    elif scenario == "directory":
                        header.type = tarfile.DIRTYPE
                    elif scenario == "traversal":
                        header.name = "../outside"
                    elif scenario == "owner":
                        header.uid = 1000
                    elif scenario == "time":
                        header.mtime = 1
                    elif scenario == "mode":
                        header.mode = 0o666
                with self.assertRaises(release.ReleaseError):
                    release.verify_stream(io.BytesIO(raw_archive(archive_entries(self.payload, self.manifest), change)))

    def test_truncation_padding_and_trailing_archive_are_rejected(self):
        original = raw_archive(archive_entries(self.payload, self.manifest))
        cases = [original[:100], original[:1024], original + b"not an archive"]
        corrupted = bytearray(original)
        size = len(release.encode_manifest(self.manifest))
        self.assertNotEqual(size % 512, 0)
        corrupted[512 + size] = 1
        cases.append(bytes(corrupted))
        cases.append(original + bytes(tarfile.RECORDSIZE * 2))
        for content in cases:
            with self.subTest(size=len(content)), self.assertRaises(release.ReleaseError):
                release.verify_stream(io.BytesIO(content))
        compressed = self.directory / "truncated.tar.gz"
        compressed.write_bytes(gzip.compress(original)[:-5])
        with self.assertRaises((release.ReleaseError, EOFError)):
            release.verify_archive(compressed, hashlib.sha256(compressed.read_bytes()).hexdigest())

    def test_source_snapshot_ignores_generated_outputs_and_runtime_environment(self):
        source, snapshot = self.directory / "source", self.directory / "snapshot"
        make_source(source)
        (source / "admin/.env").write_text("excluded fixture")
        (source / "admin/dist").mkdir()
        (source / "admin/dist/old.txt").write_text("stale build")
        digest = release.snapshot_sources(source, snapshot)
        self.assertFalse((snapshot / "admin/.env").exists())
        self.assertFalse((snapshot / "admin/dist").exists())
        self.assertEqual(release.snapshot_sources(snapshot), digest)
        (source / "ui/theme.css").write_text("updated source")
        self.assertNotEqual(release.snapshot_sources(source), digest)
        self.assertEqual((snapshot / "ui/theme.css").read_text(), "source fixture")
        (source / "ui/linked.css").symlink_to(source / "ui/theme.css")
        with self.assertRaises(OSError):
            release.snapshot_sources(source)

    def test_atomic_build_never_reuses_existing_artifacts_or_publishes_on_failure(self):
        source = self.directory / "source"
        make_source(source)
        output = self.directory / "release.tar.gz"
        def compile_snapshot(snapshot, payload):
            self.assertNotEqual(snapshot, source)
            self.assertEqual((snapshot / "admin/package.json").read_text(), "source fixture")
            make_payload(payload)
        with patch.object(release, "compile_artifacts", side_effect=compile_snapshot):
            report = release.build_release(source, output, "0.2.0-test")
        self.assertEqual(release.verify_archive(output, report["sha256"])["version"], "0.2.0-test")
        before = output.read_bytes()
        with patch.object(release, "compile_artifacts") as compile_step, self.assertRaises(release.ReleaseError):
            release.build_release(source, output, "0.2.0-test")
        compile_step.assert_not_called()
        self.assertEqual(output.read_bytes(), before)
        other = self.directory / "failed.tar.gz"
        with patch.object(release, "compile_artifacts", side_effect=release.ReleaseError("测试编译失败")), self.assertRaises(release.ReleaseError):
            release.build_release(source, other, "0.2.0-test")
        self.assertFalse(other.exists())
        self.assertFalse(list(self.directory.glob(".tombot-release-*")))

    def test_build_rejects_mutated_snapshot_and_invalid_elf(self):
        source = self.directory / "source"
        make_source(source)
        for scenario in ("snapshot", "elf"):
            with self.subTest(scenario=scenario):
                def compile_snapshot(snapshot, payload):
                    make_payload(payload)
                    path = snapshot / "ui/theme.css" if scenario == "snapshot" else payload / "linux-arm64/bin/tomd"
                    path.write_text("changed")
                output = self.directory / (scenario + ".tar.gz")
                with patch.object(release, "compile_artifacts", side_effect=compile_snapshot), self.assertRaises(release.ReleaseError):
                    release.build_release(source, output, "0.2.0-test")
                self.assertFalse(output.exists())

    def test_tool_failure_and_timeout_are_explicit(self):
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(release.ReleaseError, "退出码 3"):
                release.run_command([sys.executable, "-c", "raise SystemExit(3)"], self.directory,
                                    release.build_environment(), 5, "测试工具")
            with self.assertRaisesRegex(release.ReleaseError, "超时"):
                release.run_command([sys.executable, "-c", "import time; time.sleep(30)"], self.directory,
                                    release.build_environment(), 0.05, "测试工具")
        with patch.dict(os.environ, {"GOFLAGS": "-mod=mod", "CGO_ENABLED": "1", "GOOS": "darwin", "VITE_EXAMPLE": "excluded"}):
            environment = release.build_environment()
        self.assertEqual(environment["CGO_ENABLED"], "0")
        self.assertEqual(environment["GOFLAGS"], "-mod=readonly -buildvcs=false")
        self.assertNotIn("VITE_EXAMPLE", environment)
        self.assertNotIn("GOOS", environment)


if __name__ == "__main__":
    unittest.main()
