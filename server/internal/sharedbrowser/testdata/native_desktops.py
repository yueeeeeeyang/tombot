"""仅供受限集成测试启动真实 X11 桌面；不连接宿主显示服务。"""

import json
import os
from pathlib import Path
import secrets
import select
import subprocess
import sys
import time


sessions = json.loads(sys.stdin.readline())
processes = []
try:
    for session in sessions:
        runtime = Path("/run/bot-desktops") / session["session_id"]
        runtime.mkdir(mode=0o700, parents=True)
        os.chown(runtime, session["linux_uid"], 20000)
        auth = runtime / "Xauthority"
        auth.touch(mode=0o600)
        subprocess.run(["/usr/bin/xauth", "-f", str(auth)],
                       input=f'add :{session["display_number"]} . {secrets.token_hex(16)}\n', text=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=5)
        os.chown(auth, session["linux_uid"], 20000)
        environment = dict(os.environ, DISPLAY=f':{session["display_number"]}', XAUTHORITY=str(auth), HOME=str(runtime))
        xserver = subprocess.Popen(["/usr/bin/Xvfb", environment["DISPLAY"], "-screen", "0", "1280x800x24", "-auth", str(auth), "-nolisten", "tcp"],
                                   user=session["linux_uid"], group=20000, extra_groups=[], env=environment,
                                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        processes.append(xserver)
        for _ in range(100):
            result = subprocess.run(["/usr/bin/xdpyinfo"], env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=1)
            if result.returncode == 0:
                break
            if xserver.poll() is not None:
                raise RuntimeError("测试 X11 桌面退出")
            time.sleep(0.02)
        else:
            raise RuntimeError("测试 X11 桌面启动超时")
        manager = subprocess.Popen(["/usr/bin/xfwm4", "--compositor=off"], user=session["linux_uid"], group=20000,
                                   extra_groups=[], env=environment, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        processes.append(manager)
    print("ready", flush=True)
    while True:
        if any(process.poll() is not None for process in processes):
            raise RuntimeError("测试桌面核心组件退出")
        ready, _, _ = select.select([sys.stdin], [], [], 0.1)
        if ready and not sys.stdin.read(1):
            break
finally:
    for process in reversed(processes):
        if process.poll() is None:
            process.terminate()
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=2)
