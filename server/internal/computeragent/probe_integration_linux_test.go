//go:build linux && agentintegration

package computeragent

import (
	"context"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/taskprocess"
)

func TestContainerProbeRunnerRejectsFakeMembership(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" || taskprocess.CheckController() != nil {
		t.Fatal("固定探针运行器验收要求专用受限容器")
	}
	if stat, err := os.Stat(taskprocess.ProbeProgram); err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0111 == 0 {
		t.Fatal("镜像缺少实际固定探针程序", err)
	}
	fixture := newFixture(t)
	grant := probeGrant(fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var err error
	grant.Request.Deadline, err = agentchannel.FromDeadline(time.Now().Add(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := os.CreateTemp(t.TempDir(), "not-a-cgroup")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := agentchannel.CloseFiles([]*os.File{membership, reader, writer}); err != nil {
			t.Error(err)
		}
	})
	proof, err := runProbeProcess(ctx, grant.Request, membership, reader)
	if err == nil || proof != (computerprobe.Evidence{}) || ctx.Err() != nil {
		t.Fatal("实际固定探针错误入口没有明确失败", proof, err)
	}
	if stat, err := membership.Stat(); err != nil || stat.Size() != 0 {
		t.Fatal("实际探针修改了伪造入口", err)
	}
}
