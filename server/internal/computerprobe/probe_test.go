package computerprobe

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/taskprocess"
)

func TestEvidenceRequiresCompleteMatchingInstance(t *testing.T) {
	id := ids.New()
	valid := Evidence{ServiceID: id, Terminal: true, Files: true, LocalNetwork: true, FilesystemType: 0xef53, FilesystemSize: 32 << 20}
	if err := valid.Validate(id); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Evidence){
		func(e *Evidence) { e.ServiceID = ids.New() },
		func(e *Evidence) { e.Terminal = false },
		func(e *Evidence) { e.Files = false },
		func(e *Evidence) { e.LocalNetwork = false },
		func(e *Evidence) { e.FilesystemType = 0 },
		func(e *Evidence) { e.FilesystemSize = 0 },
	} {
		invalid := valid
		change(&invalid)
		if invalid.Validate(id) == nil {
			t.Fatal("不完整或串实例证据被接受")
		}
	}
	if (Evidence{}).Validate("") == nil {
		t.Fatal("空证据被接受")
	}
}

func TestProbeRejectsUnauthorizedExecution(t *testing.T) {
	if evidence, err := Run(t.Context(), taskprocess.ProbeRequest{}); err == nil || evidence != (Evidence{}) {
		t.Fatal("没有服务授权仍输出探针证据", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if evidence, err := Run(ctx, taskprocess.ProbeRequest{}); !errors.Is(err, context.Canceled) || evidence != (Evidence{}) {
		t.Fatal("取消请求仍执行探针", err)
	}
}
