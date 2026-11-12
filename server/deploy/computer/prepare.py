#!/usr/bin/python3
"""在接收用户任务之前准备共享业务目录与图形运行目录，不转换旧用户数据。"""

from contextlib import contextmanager
import itertools
import os
from pathlib import Path
import stat
import sys


SHARED_GID = 20000
SHARED_MODE = 0o2770
BROWSER_UID = 30000
BROWSER_DIRECTORY = "/var/lib/tombot-browser"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


@contextmanager
def directory(name, parent=None):
    """只打开真实目录并保持描述符，后续授权不跟随可替换的路径或链接。"""
    descriptor = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
    try:
        yield descriptor
    finally:
        os.close(descriptor)


def matches(descriptor, uid, gid, mode):
    identity = os.fstat(descriptor)
    return (stat.S_ISDIR(identity.st_mode) and identity.st_uid == uid and identity.st_gid == gid
            and stat.S_IMODE(identity.st_mode) == mode)


def entries(descriptor):
    """初始化只允许空目录或文件系统自带目录，最多读取两项即可判断，避免扫描用户文件。"""
    with os.scandir(descriptor) as iterator:
        return [entry.name for entry in itertools.islice(iterator, 2)]


def set_permissions(descriptor, uid, gid, mode):
    os.fchown(descriptor, uid, gid)
    os.fchmod(descriptor, mode)
    require(matches(descriptor, uid, gid, mode), "目录授权未保留要求的所有权或权限")


@contextmanager
def ensure_directory(parent, name, uid, root_gid, gid, mode):
    created = True
    try:
        os.mkdir(name, mode=0o700, dir_fd=parent)
    except FileExistsError:
        created = False
    with directory(name, parent) as descriptor:
        if created or not matches(descriptor, uid, gid, mode):
            identity = os.fstat(descriptor)
            # Linux 的 setgid 父目录会让新目录继承该位；700 与 2700 都可能是未完成授权的空共享目录。
            initial_modes = (0o700, 0o700 | (mode & stat.S_ISGID))
            require(identity.st_uid == uid and identity.st_gid in (root_gid, gid, os.getegid())
                    and stat.S_IMODE(identity.st_mode) in initial_modes and not entries(descriptor),
                    f"受管目录 {name} 已有不同权限或所属身份，拒绝修改")
            # 仅恢复创建后尚未完成授权的空目录；包含业务内容时不递归修复。
            set_permissions(descriptor, uid, gid, mode)
        yield descriptor


def prepare_layout(workspace, runtime, temporary, root_uid, root_gid, shared_gid):
    """只接受节点已准备的共享目录，不转换旧卷或修改业务文件的权限。"""
    with directory(workspace) as shared, directory(runtime) as run, directory(temporary) as tmp:
        require(matches(run, root_uid, root_gid, 0o755), "运行根目录权限不符合容器配置")
        require(matches(tmp, root_uid, root_gid, 0o1777), "临时根目录权限不符合容器配置")
        require(matches(shared, root_uid, shared_gid, SHARED_MODE), "共享目录必须由节点按当前布局预先准备")
        with ensure_directory(shared, "tools", root_uid, root_gid, shared_gid, SHARED_MODE) as tools:
            with ensure_directory(tools, "bin", root_uid, root_gid, shared_gid, SHARED_MODE):
                pass
        with ensure_directory(shared, ".tmp", root_uid, root_gid, shared_gid, SHARED_MODE):
            pass
        with ensure_directory(run, "bot-desktops", root_uid, root_gid, root_gid, 0o711):
            pass
        with ensure_directory(run, "tombot-browser", root_uid, root_gid, root_gid, 0o711):
            pass
        for name in (".X11-unix", ".ICE-unix"):
            with ensure_directory(tmp, name, root_uid, root_gid, root_gid, 0o1777):
                pass


def verify_browser_directory(browser, workspace, browser_uid, shared_gid):
    """浏览器配置由独立服务持有，但仍与业务目录使用同一块配额磁盘。"""
    with directory(browser) as profile, directory(workspace) as shared:
        require(matches(profile, browser_uid, shared_gid, 0o700), "浏览器持久目录所属身份或权限无效")
        profile_identity, shared_identity = os.fstat(profile), os.fstat(shared)
        require(profile_identity.st_dev == shared_identity.st_dev
                and profile_identity.st_ino != shared_identity.st_ino,
                "浏览器与业务目录必须来自同一磁盘的不同子目录")


def main():
    require(len(sys.argv) == 1, "运行空间初始化不接收路径或身份覆盖")
    require(os.getresuid() == (0, 0, 0) and os.getresgid() == (SHARED_GID,) * 3
            and not os.getgroups(), "运行空间初始化身份无效")
    status = dict(line.split(":", 1) for line in Path("/proc/self/status").read_text().splitlines() if ":" in line)
    require(status["NoNewPrivs"].strip() == "1" and status["Seccomp"].strip() == "2",
            "运行空间初始化缺少容器限制")
    require(all(os.path.ismount(path) for path in ("/workspace", BROWSER_DIRECTORY, "/run", "/tmp")),
            "运行空间必须使用独立挂载")
    verify_browser_directory(BROWSER_DIRECTORY, "/workspace", BROWSER_UID, SHARED_GID)
    prepare_layout("/workspace", "/run", "/tmp", 0, 0, SHARED_GID)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError) as error:
        reason = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(f"电脑运行空间准备失败：{reason}", file=sys.stderr)
        sys.exit(1)
