//go:build !linux

package taskprocess

import "testing"

func TestUnsupportedPlatformFailsClosed(t *testing.T) {
	if err := Run(); err == nil {
		t.Fatal("非 Linux 引导意外成功")
	}
	if file, err := Seal(validRequest()); file != nil || err == nil {
		t.Fatal("非 Linux 创建了替代封存内容")
	}
}
