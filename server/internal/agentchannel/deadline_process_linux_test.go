//go:build linux

package agentchannel

import (
	"math"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestClockOffsetsAndDeadlineTranslation(t *testing.T) {
	for _, test := range []struct {
		encoded string
		offset  int64
		valid   bool
	}{
		{"monotonic 0 0\nboottime 0 0\n", 0, true},
		{"monotonic 172800 7\nboottime 604800 0\n", 172800000000007, true},
		{"boottime 0 0\nmonotonic -2 500000000\n", -1500000000, true},
		{"", 0, false},
		{"monotonic 0 0", 0, false},
		{"monotonic 0 0\nmonotonic 1 0", 0, false},
		{"monotonic 0 0\nrealtime 0 0", 0, false},
		{"monotonic 0 -1\nboottime 0 0", 0, false},
		{"monotonic 0 1000000000\nboottime 0 0", 0, false},
		{"monotonic 9223372036 854775808\nboottime 0 0", 0, false},
		{"monotonic -9223372037 0\nboottime 0 0", 0, false},
		{"monotonic 0 0 trailing\nboottime 0 0", 0, false},
	} {
		offset, err := parseClockOffsets(test.encoded)
		if (err == nil) != test.valid || test.valid && offset != test.offset {
			t.Fatalf("偏移解析不符合预期：%q，结果 %d，错误 %v", test.encoded, offset, err)
		}
	}
	for _, test := range []struct {
		name            string
		expires, source int64
		target, want    int64
		valid           bool
	}{
		{"零偏移不同命名空间", 100, 0, 0, 100, true},
		{"正向偏移", 100, 20, 50, 130, true},
		{"负向偏移", 100, -20, -50, 70, true},
		{"跨正负偏移", 100, 30, -20, 50, true},
		{"源期限溢出", math.MaxInt64, -1, 0, 0, false},
		{"源偏移最小值", 1, math.MinInt64, 0, 0, false},
		{"目标期限溢出", math.MaxInt64, 0, 1, 0, false},
		{"目标非正值", 100, 0, -100, 0, false},
		{"目标偏移最小值", 100, 0, math.MinInt64, 0, false},
		{"初始期限非正值", 10, 20, 30, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stamp, err := translateDeadline(Deadline{ExpiresNS: test.expires, TimeNamespace: 1}, test.source, processClock{namespace: 2, offsetNS: test.target})
			if (err == nil) != test.valid || test.valid && (stamp.ExpiresNS != test.want || stamp.TimeNamespace != 2) {
				t.Fatal("期限换算错误", stamp, err)
			}
		})
	}
}

func TestKernelProcessClockPreservesDeadlineAndRejectsExitedProcess(t *testing.T) {
	original := time.Now().Add(time.Second)
	stamp, err := FromDeadlineForProcess(original, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	local, err := stamp.Local(2 * time.Second)
	if err != nil || local.After(original) {
		t.Fatal("换算延长原期限", local, original, err)
	}
	for _, pid := range []int{-1, 0, math.MaxInt32} {
		if _, err := FromDeadlineForProcess(original, pid); err == nil {
			t.Fatal("无效进程获得时钟授权", pid)
		}
	}
	child := exec.CommandContext(t.Context(), "/bin/sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	process, openErr := openProcessClock(child.Process.Pid)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("被终止的测试进程返回成功")
	}
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() {
		if err := process.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := readProcessClock(process); err == nil {
		t.Fatal("退出后的固定 proc 入口仍被当作有效目标")
	}
}
