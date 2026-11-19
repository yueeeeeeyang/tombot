package execution

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestChatFailureKeepsActionScope(t *testing.T) {
	if failureCode(computercontrol.ErrLeaseLost) != "COMPUTER_LEASE_LOST" {
		t.Fatal("单任务租约失效被误报为整机故障")
	}
	if failureCode(errors.Join(ErrUncertain, computercontrol.ErrNode)) != "EXTERNAL_RESULT_UNKNOWN" {
		t.Fatal("节点故障掩盖了外部动作结果未知")
	}
}

type stoppedComputerNode struct{ sessionNode }

func (*stoppedComputerNode) Open(context.Context, computer.SessionCommand) (computer.SessionObservation, error) {
	return computer.SessionObservation{}, &computer.Failure{Code: "session_computer_stopped"}
}

func TestStoppedComputerReasonReachesChat(t *testing.T) {
	f := newFixture(t, pgtest.Start(t))
	f.send(t, f.a, f.plainBot(t))
	l := f.claim(t)
	worker := f.workerWithComputers(t, callerFunc(func(context.Context, modelregistry.Configuration, []byte, modelgateway.Input, func(provider.Event) error) (modelgateway.Result, error) {
		t.Error("已停止的电脑不应执行模型")
		return modelgateway.Result{}, context.Canceled
	}), f.computers(t, &stoppedComputerNode{}, computerOptions()))
	run, err := worker.Process(t.Context(), l)
	if err != nil || run.ErrorCode != "COMPUTER_STOPPED" {
		t.Fatal(run, err)
	}
	var got string
	if err = f.pool.QueryRow(`SELECT payload->>'error_code' FROM tombot.user_events WHERE user_id=$1 AND resource_id=$2 AND kind='run.finished'`, l.UserID, l.RunID).Scan(&got); err != nil || got != run.ErrorCode {
		t.Fatal(got, err)
	}
}
