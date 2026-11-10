package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// executionClock 保留父级取消，按运行时间耗尽预算；暂停只用于明确的人机或子任务等待。
type executionClock struct {
	context.Context
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	timer      *time.Timer
	remaining  time.Duration
	started    time.Time
	pauses     int
	generation uint64
	closed     bool
}

func newExecutionClock(parent context.Context, limit time.Duration) *executionClock {
	ctx, cancel := context.WithCancelCause(parent)
	clock := &executionClock{Context: ctx, cancel: cancel, remaining: limit}
	clock.mu.Lock()
	clock.startLocked()
	clock.mu.Unlock()
	return clock
}

// Err 保留计时耗尽的语义，避免使用带原因取消时误报为用户主动取消。
func (c *executionClock) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func (c *executionClock) startLocked() {
	c.generation++
	generation := c.generation
	c.started = time.Now()
	c.timer = time.AfterFunc(c.remaining, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// Stop 不能阻止已经排队的回调；代次与暂停数共同避免旧回调取消已恢复的运行。
		if c.closed || c.pauses > 0 || c.generation != generation {
			return
		}
		c.remaining = 0
		c.cancel(context.DeadlineExceeded)
	})
}

// Pause 返回可重复调用的恢复函数；多个群成员同时提问时，只在最后一项结束后恢复共享时钟。
func (c *executionClock) Pause() func() {
	c.mu.Lock()
	if c.pauses == 0 && !c.closed {
		c.remaining -= time.Since(c.started)
		c.generation++
		c.timer.Stop()
		if c.remaining <= 0 {
			c.remaining = 0
			c.cancel(context.DeadlineExceeded)
		}
	}
	c.pauses++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pauses--
			if c.pauses == 0 && !c.closed && c.Context.Err() == nil {
				c.startLocked()
			}
		})
	}
}

func (c *executionClock) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.generation++
	c.timer.Stop()
	c.cancel(context.Canceled)
}
