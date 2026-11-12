import contextlib
import io
import json
import os
from pathlib import Path
import signal
import sys
import tempfile
import unittest
from unittest import mock

from verify_desktop import go_scenarios


class DesktopVerificationTest(unittest.TestCase):
    def test_only_completed_cases_count(self):
        with tempfile.TemporaryDirectory() as folder:
            fake_go = Path(folder) / "go"
            fake_go.write_text("#!" + sys.executable + "\nimport os\nprint(os.environ['REPORT'])\n")
            fake_go.chmod(0o700)
            for action in ("pass", "skip", "fail", "run"):
                with self.subTest(action=action):
                    env = {**os.environ, "PATH": folder + os.pathsep + os.defpath,
                           "REPORT": json.dumps({"Test": "TestDesktop", "Action": action})}
                    with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                        if action == "pass":
                            go_scenarios(("TestDesktop",), [], env, 1)
                        else:
                            with self.assertRaisesRegex(RuntimeError, "TestDesktop"):
                                go_scenarios(("TestDesktop",), [], env, 1)

    def test_reap_group_after_launcher_exits(self):
        # 超时的 Flutter 父进程可以先退出，此时测试应用仍属于本次创建的独立进程组。
        def launch(*args, stdout, **kwargs):
            self.assertTrue(kwargs["start_new_session"])
            stdout.write(b'{"Test":"TestDesktop","Action":"pass"}\n')
            return mock.Mock(pid=123, **{"wait.return_value": 0, "poll.return_value": 0})

        with mock.patch("verify_desktop.subprocess.Popen", side_effect=launch), \
             mock.patch("verify_desktop.os.killpg") as terminate, \
             contextlib.redirect_stdout(io.StringIO()):
            go_scenarios(("TestDesktop",), [], {}, 1)
            terminate.assert_called_once_with(123, signal.SIGKILL)
