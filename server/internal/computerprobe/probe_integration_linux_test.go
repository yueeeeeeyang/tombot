//go:build linux && probeintegration

package computerprobe

import (
	"context"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

// TestContainerProbeEvidence 只在无网络接口、独立 tmpfs 的专用非 root 容器执行。
// 它验证真实检查方法，不模拟成功入组，也不代表完整节点回收与电脑就绪验收。
func TestContainerProbeEvidence(t *testing.T) {
	if os.Getenv("TOM_PROBE_EVIDENCE_TEST") != "1" || os.Geteuid() != computer.ProbeUID || os.Getegid() != computer.DesktopSharedGID {
		t.Fatal("探针验收要求专用非 root 容器")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	evidence, err := inspect(ctx)
	if err != nil || !evidence.Terminal || !evidence.Files || !evidence.LocalNetwork || evidence.FilesystemSize != 32<<20 || evidence.ServiceID != "" {
		t.Fatal("真实容器探针证据错误", evidence, err)
	}
	entries, err := os.ReadDir("/workspace")
	if err != nil || len(entries) != 0 {
		t.Fatal("探针没有回收临时文件", err)
	}
}
