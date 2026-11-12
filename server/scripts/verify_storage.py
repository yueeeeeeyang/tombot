"""在专属 Docker 测试卷中验证 Linux 存储；内核模式必须另行获得高权限执行授权。"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import uuid


def container_arguments(name, volume, image, mode):
    arguments = ["create", "--name", name, "--label", "io.tombot.storage-test=" + name,
                 "--network", "none", "--read-only", "--init", "--memory", "1g", "--pids-limit", "128",
                 "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=16m",
                 "--mount", "type=volume,source=" + volume + ",target=/quota-test",
                 "--entrypoint", "/bin/sleep"]
    if mode == "kernel":
        arguments.append("--privileged")
    elif mode == "image":
        arguments.extend(["--cap-drop", "ALL"])
    else:
        raise ValueError("未知存储验收模式")
    return arguments + [image, "600"]


def invoke(command, *, env=None, cwd=None, timeout=60, show_failure=False):
    try:
        result = subprocess.run(command, env=env, cwd=cwd, timeout=timeout, check=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired, OSError) as error:
        if show_failure and isinstance(error, subprocess.CalledProcessError) and error.stdout:
            # 仅展示本脚本新建容器中已知测试程序的诊断，不输出任意 Docker 对象或宿主环境。
            print(error.stdout[:1048576], end="")
        raise RuntimeError("存储验收命令执行失败，请核对测试环境和保留资源") from error
    return result.stdout


def run(options):
    if not Path(options.docker).is_absolute() or not Path(options.socket).is_absolute():
        raise ValueError("Docker 程序和套接字必须使用绝对路径")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9./:_-]*@sha256:[0-9a-f]{64}", options.image):
        raise ValueError("测试镜像必须使用本地已有的固定摘要")
    if options.mode == "kernel" and not options.allow_kernel:
        raise ValueError("内核验收需要明确授权后使用 --allow-kernel，不能自动提升权限")
    name = "tombot-storage-test-" + uuid.uuid4().hex
    volume = name + "-volume"
    server = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory(prefix="tombot-storage-") as folder:
        docker = [options.docker, "--config", folder, "--host", "unix://" + options.socket]
        platform = invoke(docker + ["image", "inspect", options.image, "--format", "{{.Os}} {{.Architecture}}" ]).strip()
        if platform not in ("linux amd64", "linux arm64"):
            raise ValueError("存储验收只支持本地 Linux amd64 或 arm64 镜像")
        binary = Path(folder) / "storage.test"
        environment = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": platform.split()[1]}
        invoke(["go", "test", "-c", "-tags=storageintegration", "-o", str(binary), "./internal/dockernode"],
               env=environment, cwd=server, timeout=180)
        volume_created = container_created = succeeded = False
        try:
            result = invoke(docker + ["volume", "create", "--label", "io.tombot.storage-test=" + name, volume]).strip()
            volume_created = True
            if result != volume:
                raise RuntimeError("测试卷创建响应与本次资源不符")
            result = invoke(docker + container_arguments(name, volume, options.image, options.mode)).strip()
            container_created = True
            if not re.fullmatch(r"[0-9a-f]{64}", result):
                raise RuntimeError("测试容器创建响应无效")
            invoke(docker + ["start", name])
            invoke(docker + ["cp", str(binary), name + ":/quota-test/storage.test"])
            test = "TestLinuxStorageImageNoMount" if options.mode == "image" else "TestLinuxStorageQuotaAndRecovery"
            arguments = ["exec", "--env", "TOM_TEST_STORAGE_ROOT=/quota-test"]
            if options.mode == "kernel":
                arguments += ["--env", "TOM_TEST_STORAGE_KERNEL=1"]
            output = invoke(docker + arguments + [name, "/quota-test/storage.test", "-test.v", "-test.run=^" + test + "$", "-test.timeout=150s"], timeout=180, show_failure=True)
            print(output, end="")
            if "--- PASS: " + test not in output:
                raise RuntimeError("未取得所要求验收测试的通过证据")
            succeeded = True
        finally:
            # 内核模式失败时可能仍有 loop 设备引用镜像，保留卷和控制记录，禁止用删卷掩盖失败。
            retain = options.mode == "kernel" and not succeeded
            if container_created:
                labels = json.loads(invoke(docker + ["inspect", name, "--format", "{{json .Config.Labels}}" ]))
                if labels.get("io.tombot.storage-test") != name:
                    raise RuntimeError("测试容器身份不符，拒绝清理")
                invoke(docker + ["stop", "--time", "10", name], timeout=30)
                if not retain:
                    invoke(docker + ["rm", name])
            if volume_created and not retain:
                labels = json.loads(invoke(docker + ["volume", "inspect", volume, "--format", "{{json .Labels}}" ]))
                if labels.get("io.tombot.storage-test") != name:
                    raise RuntimeError("测试卷身份不符，拒绝清理")
                invoke(docker + ["volume", "rm", volume])
            if retain:
                print("内核验收未通过，已保留测试资源供安全恢复：" + name + "，卷：" + volume)
        print("存储验收通过，本次容器和测试卷已回收")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docker", required=True)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--mode", choices=("image", "kernel"), default="image")
    parser.add_argument("--allow-kernel", action="store_true", help="仅在获得节点级设备权限授权后使用")
    run(parser.parse_args())


if __name__ == "__main__":
    main()
