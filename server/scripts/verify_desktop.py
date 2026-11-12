#!/usr/bin/env python3
"""在 macOS 上复验正式服务恢复、真实 Chromium、离线模型和原生客户端。"""
import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

from verify_restore import require


ROOT = Path(__file__).resolve().parents[1]
NATIVE = {
    "TestMessagingHTTPDelegationArtifactsAsyncAndSSE": "TOM_TEST_NATIVE_DM",
    "TestNativeWorkbenchSetup": "TOM_TEST_NATIVE_WORKBENCH",
    "TestNativeChatAttachment": "TOM_TEST_NATIVE_ATTACHMENT",
    "TestNativeAuditAndLargeDownload": "TOM_TEST_NATIVE_AUDIT",
    "TestNativeTerminalControl": "TOM_TEST_NATIVE_TERMINAL",
    "TestNativeBrowserRecovery": "TOM_TEST_NATIVE_BROWSER_RECOVERY",
    "TestLocalModelChatStreaming": "TOM_TEST_NATIVE_MODEL",
    "TestLocalModelGroupAssignment": "TOM_TEST_NATIVE_GROUP",
}
CHROMIUM = (
    "TestRealPersistentBrowser", "TestRealBrowserNavigationGuard",
    "TestRealBrowserDownloadsPreserveSessionCancelAndDoNotReplay", "TestRealBrowserScreenCaptureAndUserInput",
    "TestRealBrowserServicePersistenceAndActions", "TestRealChromiumScreenshotToVisionModel",
    "TestRealBrowserScreenWebSocketUserFlow", "TestRealBrowserToolApprovalAndBlockedRedirect",
)


@contextmanager
def inference(binary, weights):
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    base = "http://127.0.0.1:" + str(port)
    alias = "tom-verification"
    model = subprocess.Popen([binary, "--model", weights, "--alias", alias, "--host", "127.0.0.1",
                              "--port", str(port), "--ctx-size", "4096", "--parallel", "1",
                              "--temp", "0", "--seed", "42", "--offline"], stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            require(model.poll() is None, "本地推理服务启动失败")
            try:
                with urllib.request.urlopen(base + "/health", timeout=2) as response:
                    if json.load(response).get("status") == "ok":
                        break
            except (urllib.error.URLError, TimeoutError) as err:
                if model.poll() is not None:
                    raise RuntimeError("本地推理服务在就绪前退出") from err
            time.sleep(.25)
        else:
            raise TimeoutError("本地模型未在时限内就绪")
        yield base + "/v1", alias
    finally:
        if model.poll() is None:
            model.terminate()
            try:
                model.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(model.pid, signal.SIGKILL)
                model.wait(timeout=5)


def run_isolated(args, cwd, env, timeout, stdout=None):
    job = subprocess.Popen(args, cwd=cwd, env=env, stdout=stdout, start_new_session=True)
    try:
        return job.wait(timeout=timeout)
    finally:
        # Flutter 工具在超时后可能留下同组的测试应用与调试进程。整个组仅由本次用例创建。
        try:
            os.killpg(job.pid, signal.SIGKILL)
        except ProcessLookupError:
            if job.poll() is None:
                raise
        job.wait(timeout=10)


def go_scenarios(names, packages, env, minutes):
    # go test 遇到显式跳过仍可能返回成功；交付验收要求每个指定用例确实完成。
    pattern = "^(" + "|".join(names) + ")$"
    passed, failed, locations = set(), set(), {}
    with tempfile.TemporaryFile() as report:
        code = run_isolated(["go", "test", "-race", "-count=1", "-json", "-p", "1", "-run", pattern,
                             "-timeout", f"{minutes}m", *packages], ROOT, env, minutes * 60 + 90, report)
        report.seek(0)
        for line in report:
            item = json.loads(line)
            name = item.get("Test")
            if name in names and item["Action"] == "pass":
                passed.add(name)
            if name in names and item["Action"] in ("fail", "skip"):
                failed.add(name)
            if name in names and item["Action"] == "output":
                # 只导出定位行号；原生测试输出可能包含消息内容，不复制到交付摘要。
                matches = re.findall(r"\b([A-Za-z_]+(?:_test\.go|\.dart):\d+(?::\d+)?)\b",
                                     item.get("Output", ""))
                if matches:
                    locations.setdefault(name, set()).update(matches)
        for name in sorted(failed):
            print(name, "、".join(sorted(locations.get(name, set()))), file=sys.stderr, flush=True)
    require(code == 0 and passed == set(names) and not failed,
            "验收失败或跳过：" + "、".join(sorted(failed | (set(names) - passed))))
    print(f"已通过 {len(passed)} 项：" + "、".join(sorted(passed)), flush=True)


def verify(archive, model_server, weights):
    require(sys.platform == "darwin", "桌面交付验收需要 macOS")
    env = {k: v for k, v in os.environ.items() if not k.startswith("TOM_")}
    env.update(TOM_TEST_BROWSER_ARCHIVE=str(archive), PYTHONDONTWRITEBYTECODE="1")
    with tempfile.TemporaryDirectory(prefix="tom-desktop-delivery-") as scratch:
        tomd = str(Path(scratch) / "tomd")
        subprocess.run(["go", "build", "-trimpath", "-o", tomd, "./cmd/tomd"], cwd=ROOT, env=env, timeout=180, check=True)
        for script in ("verify_restore.py", "verify_active_recovery.py"):
            subprocess.run([sys.executable, ROOT / "scripts" / script, tomd, archive], env=env, timeout=720, check=True)
        go_scenarios(CHROMIUM, ["./internal/browser", "./internal/service", "./internal/httpapi", "./internal/tools"], env, 6)
        try:
            for name, flag in NATIVE.items():
                print("正在运行 " + name, flush=True)
                options = {**env, flag: "widgets" if name == "TestNativeChatAttachment" else "1"}
                if name in ("TestLocalModelChatStreaming", "TestLocalModelGroupAssignment"):
                    # 保持单聊中的中止与下一个独立群任务之间的推理槽、缓存和显存生命周期隔离。
                    with inference(model_server, weights) as (address, alias):
                        options.update(TOM_TEST_LOCAL_MODEL_URL=address, TOM_TEST_LOCAL_MODEL=alias)
                        go_scenarios((name,), ["./internal/httpapi"], options, 6)
                else:
                    go_scenarios((name,), ["./internal/httpapi"], options, 6)
            for target in ("window_placement", "deep_link", "secure_session", "macos_notification_settings"):
                require(run_isolated(["flutter", "test", "--no-pub", "-d", "macos", f"integration_test/{target}_test.dart"],
                                     ROOT / "client", env, 240) == 0, f"桌面原生验收失败：{target}")
            subprocess.run(["flutter", "test", "--no-pub", "test/workbench_visual_test.dart"],
                           cwd=ROOT / "client", env=env, timeout=180, check=True)
            subprocess.run(["flutter", "build", "macos", "--release", "--no-pub"],
                           cwd=ROOT / "client", env=env, timeout=300, check=True)
            # 嵌入框架单独签名成功后，应用包仍必须覆盖本次实际嵌入的框架版本。
            subprocess.run(["codesign", "--verify", "--deep", "--strict",
                            ROOT / "client/build/macos/Build/Products/Release/tom.app"],
                           env=env, timeout=60, check=True)
        finally:
            # 原生集成测试共用 Debug 产物；交付时恢复用户实际启动的入口。
            subprocess.run(["flutter", "build", "macos", "--debug", "--no-pub"],
                           cwd=ROOT / "client", env=env, timeout=300, check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("browser_archive", type=Path)
    parser.add_argument("llama_server", type=Path)
    parser.add_argument("model_gguf", type=Path)
    options = parser.parse_args()
    verify(*(p.resolve(strict=True) for p in (options.browser_archive, options.llama_server, options.model_gguf)))
