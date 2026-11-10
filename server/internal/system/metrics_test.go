package system

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUnavailableMetricsAreExplicitAndCached(t *testing.T) {
	monitor := NewMonitor(filepath.Join(t.TempDir(), "missing"))
	first := monitor.Snapshot(context.Background())
	if first.DiskFree != nil || first.DatabaseSize != nil || len(first.Unavailable) < 2 {
		t.Fatal("不可用指标被伪装为零")
	}
	second := monitor.Snapshot(context.Background())
	if !first.CheckedAt.Equal(second.CheckedAt) {
		t.Fatal("指标缓存失效")
	}
}
