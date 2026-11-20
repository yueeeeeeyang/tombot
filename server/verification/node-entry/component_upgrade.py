"""在一次性 Linux 容器中以真实 ELF 和镜像归档演练升级事务；控制回执和 systemd 边界可注入故障。"""
import argparse
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import pty
import secrets
import select
import shutil
import signal
import ssl
import subprocess
import sys
import threading
import time


def digest(path):
    checksum = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            checksum.update(block)
    return checksum.hexdigest()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(*args, **kwargs):
    return subprocess.run(args, check=True, timeout=300, **kwargs)


def encode(value):
    return base64.urlsafe_b64encode(value).rstrip(b"=")


class Control(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # 测试日志只记录阶段，HTTP 头和接入资料留在进程内。

    def reply(self, body):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        require(self.path == "/api/v2/node-bootstrap", "引导端点错误")
        require(self.headers.get("Authorization") == "Bearer " + self.server.grant, "引导身份错误")
        self.reply({"client_fingerprints": [self.server.pin], "verification_resources": {
            "cpu_millis": 100, "memory_bytes": 128 << 20, "disk_bytes": 512 << 20, "process_limit": 32}})

    def do_POST(self):
        require(self.path == "/api/v2/node-enrollment", "登记端点错误")
        claim = json.loads(self.rfile.read(int(self.headers["Content-Length"])))["claim"]
        m = claim["maintenance"]
        result = {"node_id": claim["node_id"], "image_id": claim["registration"]["image_id"]}
        if m.get("inspect_release"):
            result.update(state="recovery_inspected", recovery={"state": "clear"})
            self.server.events.append("inspect")
        else:
            action = m.get("stage") or ("commit" if m["finish"] else "hold")
            op = m["operation_id"]
            self.server.events.append((op, action))
            result["state"] = "upgrade_progress" if m.get("stage") else ("upgrade_finished" if m["finish"] else "upgrade_held")
            if action == self.server.interrupt_stage:
                self.server.interrupt_stage = None
                os.kill(self.server.upgrader.pid, signal.SIGKILL)
                return
        self.reply(result)


def initial_install(binary, grant):
    pid, terminal = pty.fork()
    if pid == 0:
        os.execv(binary, [str(binary), "install", "/work/options.json"])
    output = b""
    sent = False
    try:
        deadline = time.monotonic() + 240
        while time.monotonic() < deadline:
            if not select.select([terminal], [], [], 1)[0]:
                continue
            try:
                block = os.read(terminal, 4096)
            except OSError as exc:
                if exc.errno != 5:
                    raise
                break
            if not block:
                break
            output = (output + block)[-16384:]
            if not sent and "Ctrl+C".encode() in output:
                os.write(terminal, grant + b"\n")
                sent = True
        else:
            os.kill(pid, signal.SIGKILL)
        _, status = os.waitpid(pid, 0)
        if status:
            raise RuntimeError("旧版安装失败 " + output.replace(grant, b"[redacted]").decode(errors="replace"))
    finally:
        os.close(terminal)


def isolated():
    work = Path("/work")
    require(Path("/.dockerenv").exists() and Path("/packages/old").is_file() and not Path("/etc/tombot/node").exists()
            and not Path("/var/run/docker.sock").exists() and os.geteuid() == 0 and not work.exists(), "需要全新专用容器")
    work.mkdir(mode=0o700)
    os.chdir(work)
    for name in ("old", "previous", "candidate"):
        dest = work / name
        dest.mkdir()
        run("tar", "--no-same-owner", "-xzf", "/packages/"+name, "-C", dest)
        run("sha256sum", "--status", "-c", "PROGRAM-SHA256SUMS", cwd=dest)
    for name in ("node-release.json", "SHA256SUMS", "computer-image.tar"):
        shutil.copyfile(Path("/packages/image")/name, work/"old"/name)
    Path("/run/systemd/system").mkdir(parents=True, exist_ok=True)
    shim = Path("/usr/bin/systemctl")
    shim.unlink(missing_ok=True)
    shutil.copyfile("/packages/systemctl", shim)
    shim.chmod(0o755)
    run("openssl", "req", "-x509", "-newkey", "ed25519", "-nodes", "-keyout", "tls.key",
        "-out", "tls.pem", "-subj", "/CN=node-test", "-days", "1", stderr=subprocess.DEVNULL)
    control = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Control)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain("tls.pem", "tls.key")
    control.socket = tls.wrap_socket(control.socket, server_side=True)
    control.pin = hashlib.sha256(ssl.PEM_cert_to_DER_cert(Path("tls.pem").read_text())).hexdigest()
    control.grant = encode(secrets.token_bytes(32)).decode()
    control.events = []
    control.interrupt_stage = None
    threading.Thread(target=control.serve_forever, daemon=True).start()
    code = {"url": "https://127.0.0.1:"+str(control.server_port), "fingerprints": [control.pin], "token": control.grant}
    Path("options.json").write_text(json.dumps({"listen": "0.0.0.0:9444", "node_url": "https://127.0.0.1:9444", "computer_capacity": 1}))
    initial_install(work/"old/tomnode", b"tomnode1." + encode(json.dumps(code, separators=(",", ":")).encode()))
    installed = Path("/usr/lib/tombot/tomnode")
    config = Path("/etc/tombot/node")
    identity = [digest(config/name) for name in ("node.json", "certificate.pem", "private.key", "computer-image.json")]
    require(digest(installed) == digest(work/"old/tomnode"), "首次安装的宿主程序不符")

    def upgrade(package, expected):
        control.upgrader = subprocess.Popen([str(work/package/"tomnode"), "upgrade"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            stdout, stderr = control.upgrader.communicate(timeout=300)
        except subprocess.TimeoutExpired:
            control.upgrader.kill()
            control.upgrader.wait()
            raise
        require(control.upgrader.returncode == expected, "升级结果异常 " + stderr.decode(errors="replace"))
        return json.loads(stdout) if not expected else {}

    upgrade("previous", 0)
    old_hash = digest(work/"previous/tomnode")
    require(digest(installed) == old_hash, "未安装第二份旧版本")
    # 进程主管拒绝一次候选启动；回滚必须恢复已验证的旧二进制和安装记录。
    Path("reject-once").touch()
    control.events.clear()
    control.interrupt_stage = "rolled_back"
    upgrade("candidate", -signal.SIGKILL)
    rollback_stops = Path("calls").read_text().splitlines().count("stop")
    upgrade("candidate", 1)
    rollback_ops = [op for op, step in control.events if step == "rolled_back"]
    require(len(rollback_ops)==2 and rollback_ops[0]==rollback_ops[1]
            and Path("calls").read_text().splitlines().count("stop")==rollback_stops, "回滚回执丢失后重复停机")
    require(digest(installed) == old_hash and json.loads((config/"installation.json").read_text())["binary_sha256"] == old_hash
            and control.events[-1][1] == "rolled_back" and Path("active").exists()
            and Path("/usr/lib/tombot/programs/bot").readlink().name == digest(work/"previous/bot-agent"), "未确认宿主回滚")
    print("旧版升级、候选启动失败自动回滚及回滚回执中断重试通过", flush=True)
    # 中央已经收到提交后中断本机，必须按同一操作重新交还窗口。
    control.events.clear()
    control.interrupt_stage = "commit"
    upgrade("candidate", -signal.SIGKILL)
    stops = Path("calls").read_text().splitlines().count("stop")
    report = upgrade("candidate", 0)
    commits = [op for op, stage in control.events if stage == "commit"]
    require(len(commits) == 2 and commits[0] == commits[1] and report["upgrade_scope"] in ("host","host_bot")
            and Path("calls").read_text().splitlines().count("stop") == stops
            and digest(installed) == digest(work/"candidate/tomnode"), "中断重试重复停机或丢失操作身份")
    require([digest(config/name) for name in ("node.json", "certificate.pem", "private.key", "computer-image.json")] == identity,
            "升级改变了身份或镜像配置")
    print("提交中断、原操作重试和宿主身份校验通过", flush=True)
    bot_source = Path("/packages/bot") if Path("/packages/bot").is_file() else work/"previous/bot-agent"
    require(digest(bot_source)!=digest(work/"candidate/bot-agent"), "本次热更新验收需要通过 --bot 指定不同版本的真实 Bot")
    botdir = work/"bot"
    shutil.copytree(work/"candidate", botdir)
    shutil.copyfile(bot_source, botdir/"bot-agent")
    require(digest(botdir/"bot-agent") != digest(work/"candidate/bot-agent"), "需提供与候选不同的 Bot 可执行文件")
    sums = botdir/"PROGRAM-SHA256SUMS"
    sums.write_text("".join(digest(botdir/name)+"  "+name+"\n" for name in (line.split()[1] for line in sums.read_text().splitlines())))
    calls = Path("calls").read_text()
    control.events.clear()
    bot_path=Path("/usr/lib/tombot/programs/bot/bot-agent")
    running=subprocess.Popen([bot_path],user=65534,group=65534,extra_groups=[],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,cwd="/")
    for package in ("bot", "candidate"):
        report = upgrade(package, 0)
        require(report["upgrade_scope"] == "bot" and report["state"] == "program_applied"
                and Path("/usr/lib/tombot/programs/bot").readlink().name == digest(work/package/"bot-agent"), "Bot 入口切换失败")
    require(running.poll() is None, "原 Bot 进程在热更新中退出")
    running.terminate()
    running.communicate(timeout=15)
    require(not control.events and Path("calls").read_text() == calls, "Bot 升级触发维护或停机")
    print("单独升级及回退 Bot 时 systemctl 调用和维护请求均为零", flush=True)
    control.shutdown()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("old", "previous", "candidate", "image", "linux-image"):
        parser.add_argument("--"+flag, required=True)
    parser.add_argument("--bot")
    args = parser.parse_args()
    base = Path(__file__).resolve().parent
    mounts = {"runner": Path(__file__).resolve(), "systemctl": base/"upgrade_systemctl.py",
              "old": args.old, "previous": args.previous, "candidate": args.candidate, "image": args.image}
    if args.bot:
        mounts["bot"] = args.bot
    docker = ["docker", "run", "--rm", "--init", "--pull", "never", "--platform", "linux/amd64", "--network", "none",
              "--user", "0:0", "--security-opt", "no-new-privileges", "--entrypoint", "/usr/bin/python3"]
    for name, path in mounts.items():
        docker += ["--mount", "type=bind,src="+str(Path(path).resolve(strict=True))+",dst=/packages/"+name+",readonly"]
    subprocess.run(docker + [args.linux_image, "/packages/runner", "--inside"], check=True, timeout=1200)


if __name__ == "__main__":
    if sys.argv[1:] == ["--inside"]:
        isolated()
    else:
        main()
