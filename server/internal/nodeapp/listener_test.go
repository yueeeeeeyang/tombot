package nodeapp

import (
	"errors"
	"net"
	"testing"
)

type failedCloseListener struct {
	net.Listener
	failure error
}

func (listener failedCloseListener) Close() error { return listener.failure }

func TestControlListenerCleanupAfterHTTPShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeControlListener(listener); err != nil {
			t.Error(err)
		}
	})
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closeControlListener(listener); err != nil {
		t.Fatal("HTTP 已关闭端点却被误报为节点收尾失败", err)
	}
	if err := closeControlListener(listener); err != nil {
		t.Fatal("重复收尾不幂等", err)
	}
	failure := errors.New("测试端点关闭失败")
	if err := closeControlListener(failedCloseListener{failure: failure}); !errors.Is(err, failure) {
		t.Fatal("未知关闭错误被忽略", err)
	}
}

func TestControlListenerCleanupBeforeServing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := closeControlListener(listener); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatal("未进入 Serve 的端点没有关闭", err)
	}
}
