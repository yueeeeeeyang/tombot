#!/usr/bin/env python3
"""在隔离的真实进程中验证中断、审批过期、例程离线补跑和私有数据边界。"""
import argparse
from collections import Counter
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import signal
import tempfile
import threading
import time
from urllib.parse import urlencode

from verify_restore import local_environment, request, require, server, setup_device


WRITE = "Prepare a file for approval."
STREAM = "Give me a progress report."
ROUTINE = "Confirm the scheduled check."
PARTIAL = "The first observation is recorded."


class ModelEndpoint(BaseHTTPRequestHandler):
    """控制上游返回的协议数据，让强制终止精确落在流式写入和工具等待期间。"""

    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, fmt, *args):
        # 标准访问日志可能包含请求目标；验收只保留经校验的各场景调用次数。
        return

    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        if self.path != "/v1/chat/completions" or not 0 < size < 1 << 20:
            self.send_error(400)
            return
        if self.headers.get("Authorization") != "Bearer " + self.server.credential:
            self.send_error(403)
            return
        prompt = json.loads(self.rfile.read(size))["messages"][-1]["content"]
        if prompt not in (WRITE, STREAM, ROUTINE):
            self.send_error(422)
            return
        with self.server.count_lock:
            self.server.calls[prompt] += 1
        delta, finish = {"content": "Scheduled check complete."}, "stop"
        if prompt == WRITE:
            args = {"path": "notes/pending.md", "content": "Approved output"}
            delta = {"tool_calls": [{"index": 0, "id": "recovery-write", "type": "function",
                                    "function": {"name": "file_write", "arguments": json.dumps(args)}}]}
            finish = "tool_calls"
        elif prompt == STREAM:
            delta, finish = {"content": PARTIAL}, None
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        chunk = {"choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
        if finish:
            chunk["usage"] = {"prompt_tokens": 10, "completion_tokens": 5}
        self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
        if finish:
            self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        if prompt == STREAM:
            require(self.server.interrupted.wait(90), "等待进程强制终止超时")


@contextmanager
def model_endpoint(credential):
    with ThreadingHTTPServer(("127.0.0.1", 0), ModelEndpoint) as upstream:
        upstream.credential = credential
        upstream.calls = Counter()
        upstream.count_lock = threading.Lock()
        upstream.interrupted = threading.Event()
        worker = threading.Thread(target=upstream.serve_forever)
        worker.start()
        try:
            yield upstream
        finally:
            upstream.interrupted.set()
            upstream.shutdown()
            worker.join(timeout=5)
            require(not worker.is_alive(), "模型验收服务未退出")


def until(read, description):
    end = time.monotonic() + 35
    while time.monotonic() < end:
        result = read()
        if result:
            return result
        time.sleep(0.2)
    raise TimeoutError(description)


def check_transcript(snapshot, text, status):
    return any(m["status"] == status and m["sender_kind"] == "bot" and
               any(b.get("text") == text for b in m["blocks"]) for m in snapshot["messages"])


def verify(binary, archive):
    credential = secrets.token_urlsafe(32)
    with tempfile.TemporaryDirectory(prefix="tom-active-recovery-") as scratch, \
            tempfile.TemporaryFile() as logs, model_endpoint(credential) as upstream:
        env = local_environment(archive)
        env.update(TOM_DATA_DIR=scratch + "/data", TOM_WORKSPACE_DIR=scratch + "/workspace")
        with server(binary, env, logs) as process:
            device = setup_device(env, process, secrets.token_urlsafe(32))

            def api(method, path, payload=None):
                response = request(env, method, path, payload, {"Authorization": "Bearer " + device["device_token"]})
                require(credential not in json.dumps(response), "接口响应泄漏了供应商密钥")
                return response

            secret = api("POST", "/secrets", {"name": "验收上游", "value": credential})
            provider = api("POST", "/providers", {
                "kind": "openai_compat", "display_name": "恢复验收上游", "secret_id": secret["id"],
                "base_url": "http://127.0.0.1:" + str(upstream.server_port) + "/v1"})
            api("POST", "/models", {"provider_id": provider["id"], "model_key": "recovery",
                "display_name": "恢复验收模型", "context_window": 8192, "max_output": 128,
                "supports_tools": True, "is_default": True})
            writer = api("POST", "/bots", {"template": "writer", "tool_policy": {
                "approval": "standard", "allowed_tools": ["file_write"]}})
            reader = api("POST", "/bots", {"template": "researcher"})
            records = [(writer["id"], "备课安排", "单独核对讲义场次。", "讲义场次"),
                       (reader["id"], "Publishing", "Running checks before publication.", "runs")]
            for owner, title, body, _ in records:
                api("POST", "/bots/" + owner + "/memory", {"title": title, "content": body, "importance": 3})
            writer_chat = "/conversations/" + writer["conversation_id"]
            reader_chat = "/conversations/" + reader["conversation_id"]
            pending = api("POST", writer_chat + "/messages", {"content": WRITE})
            approval = until(lambda: next((a for a in api("GET", "/approvals") if a["run_id"] == pending["run_id"]), None),
                             "文件写入没有进入审批")
            streaming = api("POST", reader_chat + "/messages", {"content": STREAM})
            until(lambda: check_transcript(api("GET", reader_chat), PARTIAL, "streaming"), "没有收到可恢复的流式正文")
            # 使用真实墙钟的下一次每日触发；后续重启和检查期间不会出现新的正常调度。
            due = (datetime.now(timezone.utc) + timedelta(seconds=85)).replace(second=0, microsecond=0)
            proposal = api("POST", "/routines", {
                "name": "离线恢复核查", "cron": f"{due.minute} {due.hour} * * *", "timezone": "UTC",
                "prompt": ROUTINE, "target": {"bot_id": reader["id"]}, "catch_up": "once"})
            routine_path = "/routines/" + proposal["routine_id"]
            api("POST", "/approvals/" + proposal["approval_id"] + "/decision", {"decision": "approve"})
            scheduled = api("GET", routine_path)["next_run_at"]
            require(datetime.fromisoformat(scheduled.replace("Z", "+00:00")) == due, "已批准的例程预定时间有误")
            require(not api("GET", routine_path + "/runs"), "停机前例程已执行")
            os.killpg(process.pid, signal.SIGKILL)
            require(process.wait(timeout=10) == -signal.SIGKILL, "验收实例未被强制终止")
            upstream.interrupted.set()
        delay = due.timestamp() - time.time()
        require(0 < delay <= 90, "强制终止未发生在预定触发之前")
        time.sleep(delay + 1)
        for _ in range(2):
            with server(binary, env, logs):
                for submitted in (pending, streaming):
                    detail = api("GET", "/runs/" + submitted["run_id"])
                    require(detail["run"]["status"] == "interrupted" and detail["run"]["error_code"] == "RUN_INTERRUPTED",
                            "执行未按同一标识标为中断")
                    if submitted == pending:
                        require([c["status"] for c in detail["tool_calls"]] == ["canceled"], "等待中的文件工具未取消")
                expired = api("GET", "/approvals?status=expired")
                require(any(a["id"] == approval["id"] for a in expired), "原审批未过期")
                require(not api("GET", "/approvals"), "过期动作仍可审批")
                write_snapshot, read_snapshot = api("GET", writer_chat), api("GET", reader_chat)
                require(not write_snapshot["active_turn_id"] and not read_snapshot["active_turn_id"], "旧会话仍活动")
                require(check_transcript(read_snapshot, PARTIAL, "interrupted"), "中断时已显示的正文或最终状态丢失")
                errors = [b["tool_result"] for m in write_snapshot["messages"] for b in m["blocks"] if b["kind"] == "tool_result"]
                require(len(errors) == 1 and errors[0]["is_error"] and "TOOL_CANCELED" in errors[0]["content"],
                        "文件工具没有唯一的失败回执")
                target = Path(env["TOM_WORKSPACE_DIR"]) / writer["workspace_dir"] / "notes/pending.md"
                require(not target.exists(), "未批准的文件写入已执行")
                history = until(lambda: [r for r in api("GET", routine_path + "/runs") if r.get("turn_status") == "succeeded"],
                                "没有收到成功的离线补跑结果")
                require(len(api("GET", routine_path + "/runs")) == len(history) == 1 and
                        history[0]["trigger_kind"] == "catch_up" and history[0]["scheduled_at"] == scheduled, "离线触发重复或时间失配")
                for owner, _, _, query in records:
                    for bot in (reader, writer):
                        found = api("GET", "/bots/" + bot["id"] + "/memory?" + urlencode({"query": query}))
                        require(len(found) == int(owner == bot["id"]), "中英文检索或私有记忆边界有误")
                api("GET", "/secrets")
                api("GET", "/audit/export")
        with upstream.count_lock:
            require(upstream.calls == {WRITE: 1, STREAM: 1, ROUTINE: 1}, "模型请求被重放或补跑次数有误")
        logs.seek(0)
        recorded = logs.read(4 << 20)
        require(recorded and not logs.read(1), "验收日志缺失或超过审阅上限")
        require(credential.encode() not in recorded, "进程日志泄漏了供应商密钥")
        print("真实进程验收通过：部分正文与过期审批恢复，文件零写入，离线补跑一次，两次重启无重放，私有检索和密钥脱敏通过。")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("browser_archive", type=Path)
    args = parser.parse_args()
    verify(args.binary.resolve(strict=True), args.browser_archive.resolve(strict=True))
