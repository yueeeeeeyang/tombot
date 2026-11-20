#!/usr/bin/python3
"""只在一次性验收容器中模拟服务转换和一次确定的启动失败。"""
import sys
import subprocess
from pathlib import Path

root = Path("/work")
active = root / "active"
action = sys.argv[1]
with (root / "calls").open("a") as log:
    log.write(action + "\n")
if action == "show":
    print("LoadState=loaded\nResult=success")
    print("ActiveState=active\nMainPID=1" if active.exists() else "ActiveState=inactive\nMainPID=0")
elif action == "enable":
    fault = root / "reject-once"
    if fault.exists():
        fault.unlink()
        sys.exit(1)
    subprocess.run(['/usr/lib/tombot/tomnode', 'check'], check=True, timeout=30, stdout=subprocess.DEVNULL)
    active.touch()
elif action == "stop":
    active.unlink(missing_ok=True)
elif action == "is-active":
    sys.exit(0 if active.exists() else 3)
elif action != "daemon-reload":
    sys.exit(1)
