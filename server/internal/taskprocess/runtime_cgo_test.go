//go:build linux && cgo

package taskprocess

import "testing"

func TestNativeThreadsRejectPrivilegeChanges(t *testing.T) {
	if err := Run(); err == nil {
		t.Fatal("含非 Go 线程的程序允许执行引导")
	}
	if err := execute(validRequest(), nil); err == nil {
		t.Fatal("含非 Go 线程的程序允许降权")
	}
}
