#!/usr/bin/env python3
"""在隔离目录用真实服务验证进程恢复和归档迁移。测试凭据仅在内存中传递。"""
import argparse
import base64
from contextlib import contextmanager
import json
import os
from pathlib import Path
import secrets
import select
import signal
import socket
import sys
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def request(env, method, path, payload=None, headers=None):
    body = None if payload is None else json.dumps(payload).encode()
    options = {"Content-Type": "application/json", **(headers or {})}
    url = "http://" + env["TOM_ADDR"] + "/api/v1" + path
    req = urllib.request.Request(url, body, options, method=method)
    with urllib.request.urlopen(req, timeout=10) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def ready(env, process):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        require(process.poll() is None, "服务在引导期间退出")
        try:
            state = request(env, "GET", "/bootstrap/status")
        except (ConnectionError, urllib.error.URLError, TimeoutError):
            time.sleep(0.2)
            continue
        if state["phase"] == "ready":
            return
        if state["phase"] == "failed":
            names = [s["id"] for s in state["steps"] if s["status"] == "failed"]
            raise RuntimeError("服务引导失败，失败步骤 " + ", ".join(names))
        time.sleep(0.2)
    raise TimeoutError("服务未在验收时限内就绪")


@contextmanager
def server(binary, env, stderr=subprocess.DEVNULL):
    process = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE,
                               stderr=stderr, start_new_session=True)
    try:
        ready(env, process)
        yield process
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                require(process.wait(timeout=100) == 0, "服务未正常关闭")
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=10)
                raise
        process.stdout.close()


def command(binary, args, env, success=True):
    result = subprocess.run([binary, *args], env=env, capture_output=True, timeout=300)
    if (result.returncode == 0) != success:
        raise RuntimeError(args[0] + " 结果异常 " + result.stderr.decode(errors="replace")[:300])


def verify_records(env, device, secret_id, content):
    headers = {"Authorization": "Bearer " + device["device_token"]}
    devices = request(env, "GET", "/devices", headers=headers)
    require(any(d["is_current"] and d["id"] == device["device_id"] for d in devices), "原设备失效")
    records = request(env, "GET", "/secrets", headers=headers)
    require([r["id"] for r in records] == [secret_id], "加密记录元数据不一致")
    file = request(env, "GET", "/fs/file?scope=shared&path=recovery.txt", headers=headers)
    require(file["content"] == content, "共享文件内容不一致")


def local_environment(browser_archive):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        addr = "127.0.0.1:" + str(listener.getsockname()[1])
    env = {k: v for k, v in os.environ.items() if not k.startswith("TOM_")}
    env.update(TOM_ADDR=addr, TOM_BROWSER_ARCHIVE=str(browser_archive),
               TOM_CLOCK_URL="https://127.0.0.1:1")
    return env


def setup_device(env, process, password):
    require(select.select([process.stdout], [], [], 5)[0], "未收到初始化码")
    code = process.stdout.readline().decode().strip().split("：", 1)[1]
    request(env, "POST", "/auth/setup", {"password": password}, {"X-Tom-Setup-Code": code})
    return request(env, "POST", "/auth/login", {
        "password": password, "device": {"name": "恢复验收", "platform": "macos" if sys.platform == "darwin" else "linux"}})


def verify(binary, browser_archive, environment_key=False):
    with tempfile.TemporaryDirectory(prefix="tom-recovery-") as parent:
        location = Path(parent)
        env = local_environment(browser_archive)
        if environment_key:
            env["TOM_MASTER_KEY"] = base64.b64encode(secrets.token_bytes(32)).decode()
        archive = location / "snapshot.tgz"
        target = location / "recovered"
        password = secrets.token_urlsafe(32)
        content = "进程恢复验收 " + secrets.token_hex(12)
        with tempfile.TemporaryDirectory(dir=location, prefix="source-") as source:
            env.update(TOM_DATA_DIR=source + "/data", TOM_WORKSPACE_DIR=source + "/workspace")
            with server(binary, env) as process:
                device = setup_device(env, process, password)
                headers = {"Authorization": "Bearer " + device["device_token"]}
                record = request(env, "POST", "/secrets", {
                    "name": "恢复验收", "value": secrets.token_urlsafe(32)}, headers)
                request(env, "POST", "/devices/push-token", {"push_token": secrets.token_urlsafe(32)}, headers)
                request(env, "PUT", "/fs/file", {
                    "scope": "shared", "path": "recovery.txt", "content": content}, headers)
                command(binary, ["backup", "-o", archive], env, success=False)
                os.killpg(process.pid, signal.SIGKILL)
                require(process.wait(timeout=10) == -signal.SIGKILL, "进程未被强制终止")
            with server(binary, env):
                verify_records(env, device, record["id"], content)
            command(binary, ["backup", "-o", archive], env)
        # 删除原实例后再恢复，重启时也不提供安装包，托管浏览器必须来自归档。
        del env["TOM_BROWSER_ARCHIVE"]
        if environment_key:
            wrong = dict(env, TOM_MASTER_KEY=base64.b64encode(secrets.token_bytes(32)).decode())
            command(binary, ["restore", "--input", archive, "--target", target], wrong, success=False)
            require(not target.exists(), "错误密钥留下了已发布目录")
        command(binary, ["restore", "--input", archive, "--target", target], env)
        env.update(TOM_DATA_DIR=str(target / "data"), TOM_WORKSPACE_DIR=str(target / "workspace"))
        with server(binary, env):
            verify_records(env, device, record["id"], content)
            session = request(env, "POST", "/auth/login", {
                "password": password, "device": {"name": "恢复后新设备", "platform": "macos" if sys.platform == "darwin" else "linux"}})
            require(session["device_id"] != device["device_id"], "新设备登录失败")
        print("真实服务验收通过：强制终止、重启、备份、清空原实例、还原、旧设备访问和新设备登录。")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path, help="待验收的 tomd 二进制")
    parser.add_argument("browser_archive", type=Path, help="与本机平台匹配的官方离线浏览器包")
    parser.add_argument("--environment-key", action="store_true", help="使用进程内随机环境主密钥")
    options = parser.parse_args()
    verify(options.binary.resolve(strict=True), options.browser_archive.resolve(strict=True),
           options.environment_key)
