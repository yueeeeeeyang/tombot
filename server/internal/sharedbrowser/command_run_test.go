package sharedbrowser

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestCommandBudgetPausesExecutionAndAccumulatesDialogWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newCommandRun(t.Context(), time.Second, 3*time.Second)
		defer run.finish()
		time.Sleep(500 * time.Millisecond)
		run.setWaiting(true)
		time.Sleep(2 * time.Second)
		if run.ctx.Err() != nil {
			t.Fatal("等待对话框消耗了执行预算")
		}
		run.setWaiting(false)
		time.Sleep(100 * time.Millisecond)
		run.setWaiting(true)
		time.Sleep(time.Second)
		synctest.Wait()
		if run.ctx.Err() == nil {
			t.Fatal("连续对话框重新获得了等待预算")
		}
	})
}

func TestCommandBudgetResumesRemainingExecutionTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newCommandRun(t.Context(), time.Second, 10*time.Second)
		defer run.finish()
		time.Sleep(700 * time.Millisecond)
		run.setWaiting(true)
		time.Sleep(2 * time.Second)
		run.setWaiting(false)
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		if run.ctx.Err() == nil {
			t.Fatal("对话框答复重置了执行预算")
		}
	})
}

func TestCommandBudgetParentCancellationAndFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		run := newCommandRun(ctx, time.Second, time.Minute)
		run.setWaiting(true)
		cancel()
		if !errors.Is(run.ctx.Err(), context.Canceled) {
			t.Fatal("撤销未终止等待动作")
		}
		run.finish()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
	})
}

func TestCommandReceiptDoesNotReplayPendingOrUnknown(t *testing.T) {
	command := Command{Sequence: 7, Kind: "reload"}
	run := newCommandRun(t.Context(), time.Second, time.Minute)
	defer run.finish()
	item := &entry{life: t.Context(), last: command, active: run, window: Window{State: "ready", Sequence: 7, CommandState: "running"}}
	if receipt, err, handled := commandReceipt(item, command); !handled || err != nil || receipt.CommandState != "running" {
		t.Fatal("在途回执无法查询", err)
	}
	if _, err, _ := commandReceipt(item, Command{Sequence: 8, Kind: "reload"}); !errors.Is(err, ErrBusy) {
		t.Fatal("在途动作被覆盖", err)
	}
	if _, err, _ := commandReceipt(item, Command{Sequence: 7, Kind: "back"}); !errors.Is(err, ErrConflict) {
		t.Fatal("同序号更换动作未拒绝", err)
	}
	run.cancel()
	if _, err, _ := commandReceipt(item, command); !errors.Is(err, ErrUnknown) {
		t.Fatal("未知动作可以重放", err)
	}
	ended, revoke := context.WithCancel(t.Context())
	revoke()
	item.life, item.active, item.lastError = ended, nil, nil
	if _, err, _ := commandReceipt(item, command); !errors.Is(err, ErrDenied) {
		t.Fatal("已关闭页面仍返回成功回执", err)
	}
}

func TestDialogBudgetIsExplicitAndBounded(t *testing.T) {
	options, _, _ := testOptions()
	for _, timeout := range []time.Duration{0, -time.Second, time.Second - time.Nanosecond, 10*time.Minute + time.Nanosecond} {
		options.DialogTimeout = timeout
		if options.Validate() == nil {
			t.Fatal("无效对话框预算被接受", timeout)
		}
	}
	for _, timeout := range []time.Duration{time.Second, 10 * time.Minute} {
		options.DialogTimeout = timeout
		if err := options.Validate(); err != nil {
			t.Fatal("有效对话框边界被拒绝", err)
		}
	}
}
