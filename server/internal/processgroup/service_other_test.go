//go:build !linux

package processgroup

import "testing"

func TestServicesUnsupportedOutsideLinux(t *testing.T) {
	if services, err := NewServices(Options{}, nil); services != nil || err == nil {
		t.Fatal("非 Linux 平台启用了服务资源组")
	}
	if ValidateServiceControls(serviceFixture(), nil) == nil {
		t.Fatal("非 Linux 平台接受资源组入口")
	}
}
