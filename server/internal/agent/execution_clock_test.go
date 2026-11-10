package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestExecutionClockNestedPauseRemainingBudgetAndCancellation(t *testing.T) {
	clock := newExecutionClock(context.Background(), 400*time.Millisecond)
	defer clock.Close()
	time.Sleep(100 * time.Millisecond)
	first, second := clock.Pause(), clock.Pause()
	clock.mu.Lock()
	remaining := clock.remaining
	clock.mu.Unlock()
	if remaining <= 0 || remaining >= 350*time.Millisecond {
		t.Fatal("暂停未扣除已执行时间", remaining)
	}
	first()
	first()
	select {
	case <-clock.Done():
		t.Fatal("嵌套等待提前耗尽时限")
	case <-time.After(450 * time.Millisecond):
	}
	second()
	select {
	case <-clock.Done():
		if !errors.Is(clock.Err(), context.DeadlineExceeded) {
			t.Fatal(clock.Err())
		}
	case <-time.After(remaining + 200*time.Millisecond):
		t.Fatal("恢复后没有耗尽剩余时限")
	}
	parent, cancel := context.WithCancel(context.Background())
	paused := newExecutionClock(parent, time.Hour)
	defer paused.Close()
	resume := paused.Pause()
	cancel()
	<-paused.Done()
	resume()
	if !errors.Is(paused.Err(), context.Canceled) {
		t.Fatal("人工等待屏蔽了父级取消", paused.Err())
	}
}

func TestExecutionClockConcurrentResumeAndClose(t *testing.T) {
	clock := newExecutionClock(context.Background(), time.Second)
	var workers sync.WaitGroup
	for range 32 {
		resume := clock.Pause()
		workers.Go(func() { resume(); resume() })
	}
	workers.Go(clock.Close)
	workers.Wait()
	clock.Close()
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if clock.pauses != 0 || !clock.closed || clock.Err() == nil {
		t.Fatal("并发恢复或关闭破坏了时钟状态")
	}
}
