package sharedbrowser

import (
	"context"
	"sync"
	"time"
)

// commandRun 分开累计实际执行与等待对话框的预算；多个连续对话框不能重新获得等待额度。
// 只有收到明确的等待回执后才脱离单次请求，仍始终受窗口寿命和会话撤销约束。
type commandRun struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	changed    chan struct{}
	mu         sync.Mutex
	timer      *time.Timer
	started    time.Time
	remaining  [2]time.Duration
	waiting    bool
	closed     bool
	generation uint64
	result     Window
	failure    error
}

func newCommandRun(parent context.Context, active, dialog time.Duration) *commandRun {
	ctx, cancel := context.WithCancel(parent)
	run := &commandRun{ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}, 1), remaining: [2]time.Duration{active, dialog}}
	run.mu.Lock()
	run.arm()
	run.mu.Unlock()
	return run
}

// arm 仅在预算锁内调用；代次防止已进入回调的旧计时器取消刚切换到对话框的动作。
func (run *commandRun) arm() {
	index := 0
	if run.waiting {
		index = 1
	}
	run.started = time.Now()
	run.generation++
	generation := run.generation
	run.timer = time.AfterFunc(run.remaining[index], func() {
		run.mu.Lock()
		defer run.mu.Unlock()
		if !run.closed && generation == run.generation {
			run.cancel()
		}
	})
}

func (run *commandRun) setWaiting(waiting bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || run.waiting == waiting || run.ctx.Err() != nil {
		return
	}
	index := 0
	if run.waiting {
		index = 1
	}
	run.remaining[index] -= time.Since(run.started)
	run.timer.Stop()
	run.generation++
	if run.remaining[index] <= 0 {
		run.cancel()
		return
	}
	run.waiting = waiting
	run.arm()
	select {
	case run.changed <- struct{}{}:
	default:
	}
}

func (run *commandRun) finish() {
	run.mu.Lock()
	run.closed = true
	run.generation++
	run.timer.Stop()
	run.cancel()
	run.mu.Unlock()
}
