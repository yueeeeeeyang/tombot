//go:build linux

package computeragent

import (
	"bytes"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/processgroup"
)

func TestKernelProbeRejectsOrdinaryControls(t *testing.T) {
	fixture := newFixture(t)
	factory := &kernelScopes{interval: time.Millisecond}
	grant := probeGrant(fixture)
	files := probeFiles(t)
	t.Cleanup(func() {
		if err := agentchannel.CloseFiles(files); err != nil {
			t.Error(err)
		}
	})
	if _, err := factory.OpenProbe(grant.Request.Group, files); err == nil {
		t.Fatal("普通文件被当作真实探针组")
	}
	grant.Request.Group.Kind = processgroup.BrowserService
	if _, err := factory.OpenProbe(grant.Request.Group, files); err == nil {
		t.Fatal("探针接管浏览器服务")
	}
	for _, file := range files {
		if _, err := file.Stat(); err != nil {
			t.Fatal("失败接管关闭了调用方描述符", err)
		}
	}
}

func TestProbeOutputIsBoundedAndDrained(t *testing.T) {
	var output probeOutput
	content := bytes.Repeat([]byte("x"), maximumProbeOutputBytes+100)
	for range 3 {
		if count, err := output.Write(content); err != nil || count != len(content) {
			t.Fatal("超限输出未继续排空", err)
		}
	}
	if !output.exceeded || output.Len() != maximumProbeOutputBytes || !bytes.Equal(output.Bytes(), content[:maximumProbeOutputBytes]) {
		t.Fatal("探针输出未按上限保留")
	}
	var ordinary probeOutput
	if _, err := ordinary.Write([]byte("{}\n")); err != nil || ordinary.exceeded {
		t.Fatal("正常输出被拒绝", err)
	}
}

func TestKernelProbeScopeDoesNotCloseBeforeDrain(t *testing.T) {
	files := probeFiles(t)
	scope := &kernelServiceScope{files: files, interval: time.Millisecond}
	t.Cleanup(func() {
		if !scope.closed {
			if err := agentchannel.CloseFiles(files); err != nil {
				t.Error(err)
			}
		}
	})
	if err := scope.Close(); err == nil {
		t.Fatal("未回收探针丢弃控制入口")
	}
	if _, err := files[2].WriteAt([]byte("populated 0\n"), 0); err != nil {
		t.Fatal(err)
	}
	if err := scope.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal("重复关闭错误", err)
	}
	if _, err := files[0].Stat(); err == nil {
		t.Fatal("关闭后仍持有控制入口")
	}
}
