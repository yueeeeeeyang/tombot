package system

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// Metrics 以可空字段表示不可用指标；探测失败不能被当成零负载。
type Metrics struct {
	CPUPercent   *float64  `json:"cpu_pct"`
	MemoryUsed   *uint64   `json:"mem_used"`
	MemoryTotal  *uint64   `json:"mem_total"`
	DiskUsed     *uint64   `json:"disk_used"`
	DiskTotal    *uint64   `json:"disk_total"`
	DiskFree     *uint64   `json:"disk_free"`
	DatabaseSize *int64    `json:"db_size"`
	CheckedAt    time.Time `json:"checked_at"`
	Unavailable  []string  `json:"unavailable"`
}

// Monitor 合并并发采样并缓存五秒，避免公开健康接口触发无界系统调用。
type Monitor struct {
	mu        sync.Mutex
	directory string
	latest    Metrics
}

func NewMonitor(directory string) *Monitor { return &Monitor{directory: directory} }

// Snapshot 返回最近的系统指标；所有外部命令继承短超时且不重试。
func (m *Monitor) Snapshot(parent context.Context) Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.latest.CheckedAt) < 5*time.Second {
		return m.latest
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	snapshot := Metrics{CheckedAt: time.Now().UTC(), Unavailable: []string{}}
	if values, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(values) > 0 {
		snapshot.CPUPercent = &values[0]
	} else {
		snapshot.Unavailable = append(snapshot.Unavailable, "cpu")
	}
	if memory, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		snapshot.MemoryUsed = &memory.Used
		snapshot.MemoryTotal = &memory.Total
	} else {
		snapshot.Unavailable = append(snapshot.Unavailable, "memory")
	}
	if storage, err := disk.UsageWithContext(ctx, m.directory); err == nil {
		snapshot.DiskUsed = &storage.Used
		snapshot.DiskTotal = &storage.Total
		snapshot.DiskFree = &storage.Free
	} else {
		snapshot.Unavailable = append(snapshot.Unavailable, "disk")
	}
	if stat, err := os.Stat(filepath.Join(m.directory, "tom.db")); err == nil {
		size := stat.Size()
		snapshot.DatabaseSize = &size
	} else {
		snapshot.Unavailable = append(snapshot.Unavailable, "database")
	}
	m.latest = snapshot
	return snapshot
}
