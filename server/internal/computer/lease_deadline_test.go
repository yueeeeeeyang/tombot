package computer

import (
	"context"
	"testing"
	"time"
)

func TestReceivedLeaseDoesNotGrow(t *testing.T) {
	start := time.Now()
	short := start.Add(40 * time.Millisecond)
	ctx := WithLeaseDeadline(context.Background(), short)
	time.Sleep(10 * time.Millisecond)
	if actual := LeaseDeadline(ctx, time.Minute); !actual.Equal(short) {
		t.Fatal("节点接收时重新延长租约", actual, short)
	}
	old := start.Add(-time.Second)
	if actual := LeaseDeadline(WithLeaseDeadline(context.Background(), old), time.Minute); !actual.Equal(old) {
		t.Fatal("过期授权被重新延长")
	}
	long := WithLeaseDeadline(context.Background(), start.Add(time.Hour))
	if actual := LeaseDeadline(long, time.Second); actual.After(time.Now().Add(time.Second)) {
		t.Fatal("远端可扩大指定租期")
	}
	if actual := LeaseDeadline(context.Background(), time.Second); actual.Before(start.Add(time.Second)) || actual.After(time.Now().Add(time.Second)) {
		t.Fatal("本机租期计算错误")
	}
}
