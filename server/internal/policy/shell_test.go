package policy

import "testing"

func TestShellDangerousPatternsAndOrdinaryCommands(t *testing.T) {
	for _, command := range []string{"sudo -n true", "/usr/bin/sudo true", "mkfs.ext4 /dev/example", "dd if=input of=/dev/example", "shutdown now", "rm -rf /", "rm -r /etc/example", "printf text > /etc/example", "printf text | tee /usr/example", "cp out/report /etc/example", "sed -i s/a/b/ /etc/example"} {
		if ShellClass(command) != "shell.privileged" {
			t.Errorf("危险命令未提升: %s", command)
		}
	}
	for _, command := range []string{"ls -la", "printf text > out/report.md", "rm notes/draft.md", "python3 -c 'print(1)'", "pwd"} {
		if ShellClass(command) != "shell.exec" {
			t.Errorf("普通命令归类错误: %s", command)
		}
	}
}
