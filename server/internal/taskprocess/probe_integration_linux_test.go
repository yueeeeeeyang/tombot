//go:build linux && taskintegration && !cgo

package taskprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
)

// TestKernelProbeAdmissionRejects 验证固定探针拒绝伪造内核入口和迟到授权，不替代实际成功入组验收。
func TestKernelProbeAdmissionRejects(t *testing.T) {
	requireIsolationContainer(t)
	for _, test := range []struct {
		phase string
		want  string
	}{
		{"membership", "任务入组描述符不是受管内核入口"},
		{"revoked", "任务执行授权已撤销"},
		{"expired", "期限"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			request := probeRequest()
			var err error
			request.Deadline, err = agentchannel.FromDeadline(time.Now().Add(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if test.phase == "expired" {
				request.Deadline.ExpiresNS = 1
			}
			file, err := SealProbe(request)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			membership, err := os.CreateTemp(t.TempDir(), "not-a-cgroup")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := membership.Close(); err != nil {
					t.Error(err)
				}
			})
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			if test.phase == "revoked" {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Cleanup(func() {
					if err := writer.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			program, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, program, "-test.run=^TestProbeProcessChild$")
			command.Env = []string{"TOM_PROBE_CHILD=1"}
			command.ExtraFiles = []*os.File{file, membership, reader}
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(string(output), test.want) {
				t.Fatalf("探针未在预期边界拒绝：%v\n%s", err, output)
			}
			if stat, err := membership.Stat(); err != nil || stat.Size() != 0 {
				t.Fatal("探针修改了伪造入口", err)
			}
		})
	}
}

func TestProbeProcessChild(t *testing.T) {
	if os.Getenv("TOM_PROBE_CHILD") != "1" {
		return
	}
	fmt.Fprintln(os.Stderr, RunProbe())
	os.Exit(125)
}
