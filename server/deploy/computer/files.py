#!/usr/bin/python3
"""在降权后的 Bot 身份内传输业务文件；不跟随链接，不覆盖已有产物。"""
import hashlib
import os
import stat
import sys
import uuid

MAXIMUM_BYTES = 16 << 20


def open_parent(root, relative, create=False):
    parts = relative.split("/")
    if not relative or relative.lower().endswith(".crdownload") or len(relative.encode()) > 4096 or any(
        part in ("", ".", "..") or any(c in part for c in "\x00\r\n\\") for part in parts
    ):
        raise ValueError("文件路径无效")
    parent = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in parts[:-1]:
            if create:
                try:
                    os.mkdir(part, 0o2770, dir_fd=parent)
                except FileExistsError:
                    if not stat.S_ISDIR(os.stat(part, dir_fd=parent, follow_symlinks=False).st_mode):
                        raise ValueError("文件父目录不是普通目录")
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
            os.close(parent)
            parent = child
        return parent, parts[-1]
    except BaseException:
        os.close(parent)
        raise


def read_file(root, relative, output):
    parent, name = open_parent(root, relative)
    try:
        descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        with os.fdopen(descriptor, "rb") as source:
            before = os.fstat(source.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_size > MAXIMUM_BYTES:
                raise ValueError("文件类型或大小不受支持")
            content = source.read(MAXIMUM_BYTES + 1)
            after = os.fstat(source.fileno())
            if len(content) != before.st_size or len(content) > MAXIMUM_BYTES or (
                before.st_size, before.st_mtime_ns, before.st_ctime_ns
            ) != (after.st_size, after.st_mtime_ns, after.st_ctime_ns):
                raise ValueError("文件在读取期间发生变化")
            output.write(content)
            output.flush()
    finally:
        os.close(parent)


def write_file(root, relative, expected_size, expected_digest, source):
    if expected_size < 0 or expected_size > MAXIMUM_BYTES:
        raise ValueError("文件大小超限")
    content = source.read(expected_size + 1)
    if len(content) != expected_size or hashlib.sha256(content).hexdigest() != expected_digest:
        raise ValueError("文件内容校验失败")
    parent, name = open_parent(root, relative, create=True)
    staging = ".tombot-import-" + uuid.uuid4().hex
    created = False
    try:
        descriptor = os.open(staging, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o660, dir_fd=parent)
        created = True
        with os.fdopen(descriptor, "wb") as destination:
            destination.write(content)
            destination.flush()
            os.fsync(destination.fileno())
        # 同目录硬链接提供不覆盖的原子发布，迟到请求不能改写用户已编辑的文件。
        os.link(staging, name, src_dir_fd=parent, dst_dir_fd=parent, follow_symlinks=False)
        os.unlink(staging, dir_fd=parent)
        created = False
        os.fsync(parent)
    finally:
        try:
            if created:
                os.unlink(staging, dir_fd=parent)
        finally:
            os.close(parent)


def main():
    if os.geteuid() == 0:
        raise ValueError("文件工具必须在 Bot 身份下执行")
    if len(sys.argv) == 3 and sys.argv[1] == "read":
        read_file("/workspace", sys.argv[2], sys.stdout.buffer)
    elif len(sys.argv) == 5 and sys.argv[1] == "write":
        write_file("/workspace", sys.argv[2], int(sys.argv[3]), sys.argv[4], sys.stdin.buffer)
    else:
        raise ValueError("文件工具参数无效")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError):
        print("电脑文件传输失败", file=sys.stderr)
        sys.exit(1)
