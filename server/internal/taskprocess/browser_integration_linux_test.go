//go:build linux && taskintegration && !cgo

package taskprocess

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

func TestKernelBrowserAdmissionRejects(t *testing.T) {
	requireIsolationContainer(t)
	if err := os.Mkdir(computer.BrowserRuntimeRoot, 0711); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	for _, test := range []struct{ phase, want string }{
		{"membership", "入组描述符不是受管内核入口"},
		{"revoked", "执行授权已撤销"},
		{"expired", "期限"},
		{"pipes", "协议入口不是受信管道"},
		{"permissions", "目录所属身份或权限无效"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			request := browserRequest()
			var err error
			request.Deadline, err = agentchannel.FromDeadline(time.Now().Add(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if test.phase == "expired" {
				request.Deadline.ExpiresNS = 1
			}
			if err := os.Mkdir(request.RuntimeDirectory(), 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(request.RuntimeDirectory()); err != nil {
					t.Error(err)
				}
			})
			if err := os.Chown(request.RuntimeDirectory(), computer.BrowserUID, computer.DesktopSharedGID); err != nil {
				t.Fatal(err)
			}
			if test.phase == "permissions" {
				if err := os.Chmod(request.RuntimeDirectory(), 0770); err != nil {
					t.Fatal(err)
				}
			}
			sealed, err := SealBrowser(request)
			if err != nil {
				t.Fatal(err)
			}
			membership, err := os.CreateTemp(t.TempDir(), "not-cgroup")
			if err != nil {
				t.Fatal(err)
			}
			var files []*os.File
			files = append(files, sealed, membership)
			t.Cleanup(func() {
				if err := agentchannel.CloseFiles(files); err != nil {
					t.Error(err)
				}
			})
			revokeRead, revokeWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, revokeRead)
			if test.phase == "revoked" {
				if err := revokeWrite.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				files = append(files, revokeWrite)
			}
			commands, send, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			responses, receive, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, commands, send, responses, receive)
			commandPipe := commands
			if test.phase == "pipes" {
				commandPipe = membership
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/usr/lib/tombot/computer-task", "--browser")
			command.Env = []string{"PATH=/usr/bin:/bin"}
			command.ExtraFiles = []*os.File{sealed, membership, revokeRead, commandPipe, receive}
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 125 || !strings.Contains(string(output), test.want) {
				t.Fatalf("浏览器未在预期边界拒绝：%v，%s", err, output)
			}
			if metadata, err := membership.Stat(); err != nil || metadata.Size() != 0 {
				t.Fatal("浏览器修改了伪造内核入口", err)
			}
		})
	}
}
