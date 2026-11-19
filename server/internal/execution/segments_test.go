package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// 只关心消息合并的回归沿用这个视图；分段用量由完整 Worker 场景覆盖。
func (s *Service) mergeRunInputs(ctx context.Context, l Lease, key string, input modelgateway.Input) (modelgateway.Input, error) {
	merged, _, err := s.modelStepInput(ctx, l, key, input)
	return merged, err
}

// 强制每段只工作一轮，完整走消息、远程 Agent、工具记账、释放电脑、重领和取消入口。
func TestWorkerCheckpoints(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"完成", "取消", "无进展"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, cluster)
			f.toolsModel(t)
			f.send(t, f.a, f.first)
			l := f.claim(t)
			calls, actions := 0, 0
			worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				calls++
				switch calls {
				case 1:
					return emitTool(emit, `{"path":"first"}`)
				case 3:
					found := false
					for _, m := range input.Messages {
						for _, b := range m.Blocks {
							found = found || strings.Contains(b.Text, "check second file")
						}
					}
					if !found {
						t.Error("下一阶段缺少交接内容")
					}
					if mode == "完成" {
						return emitText(emit, "已完成全部核验。")
					}
					return emitTool(emit, `{"path":"first"}`)
				default:
					if len(input.Tools) != 1 || input.Tools[0].Name != "continue_work" {
						t.Fatal("未保留阶段收尾", input.Tools)
					}
					handoff := "check second file"
					if calls > 2 {
						handoff += " again"
					}
					return emitDesktopTool(emit, calls, "continue_work", `{"result":"已核验第一份文件。","handoff":"`+handoff+`"}`)
				}
			}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
				actions++
				return botagent.ToolResult{Content: "first file verified"}, nil
			}})
			worker.options.MaxModelSteps = 1
			if mode == "完成" {
				// 首批模型和工具确有进展时，单操作预算仍要能安全收尾并转入下一段。
				worker.options.MaxModelSteps = 8
				f.service.options.MaxOperations = 1
			}
			run, err := worker.Process(t.Context(), l)
			if err != nil || run.Status != "queued" || run.ID != l.RunID || actions != 1 {
				t.Fatal("阶段未成功交付", run, err, actions)
			}
			messages, err := f.service.Messages(t.Context(), f.a, l.ConversationID, 0, 20)
			if err != nil || len(messages) != 2 || messages[0].Blocks[0].Text != "已核验第一份文件。" {
				t.Fatal("阶段结果未出现在原会话", messages, err)
			}
			if mode == "取消" {
				run, err = f.service.Cancel(t.Context(), f.a, l.RunID)
				if err != nil || run.Status != "canceled" {
					t.Fatal(run, err)
				}
				return
			}
			next := f.claim(t)
			if next.RunID != l.RunID || next.FencingToken <= l.FencingToken {
				t.Fatal("下一段未绑定原任务的新租约")
			}
			run, err = worker.Process(t.Context(), next)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "完成" && (run.Status != "succeeded" || actions != 1 || calls != 3) {
				t.Fatal("运行未完成或重放已完成动作", run, calls, actions)
			}
			if mode == "无进展" && (run.ErrorCode != "NO_PROGRESS" || calls != 4) {
				t.Fatal("重复回执仍在空转", run, calls)
			}
		})
	}
}

func TestLongTaskContinuesWhileReceiptsAdvance(t *testing.T) {
	f := newFixture(t, pgtest.Start(t))
	f.toolsModel(t)
	f.send(t, f.a, f.first)
	const batches = 33
	batch, calls := 0, 0
	worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		calls++
		if len(input.Tools) == 1 && input.Tools[0].Name == "continue_work" {
			return emitDesktopTool(emit, calls, "continue_work", fmt.Sprintf(`{"result":"已检查第 %d 份文件。","handoff":"Check the next file in the queue."}`, batch))
		}
		if batch == batches {
			return emitText(emit, "全部文件均已检查。")
		}
		batch++
		return emitTool(emit, fmt.Sprintf(`{"path":"file-%d"}`, batch))
	}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
		return botagent.ToolResult{Content: "verified"}, nil
	}})
	worker.options.MaxModelSteps = 1
	var runID string
	for segment := 0; segment <= batches; segment++ {
		l := f.claim(t)
		if runID != "" && l.RunID != runID {
			t.Fatal("跨阶段改变了运行编号")
		}
		runID = l.RunID
		run, err := worker.Process(t.Context(), l)
		want := "queued"
		if segment == batches {
			want = "succeeded"
		}
		if err != nil || run.Status != want {
			t.Fatalf("第 %d 段状态 %s，原因 %s，错误 %v", segment, run.Status, run.ErrorCode, err)
		}
	}
	if calls != 2*batches+1 {
		t.Fatal("重复执行了已交付阶段", calls)
	}
}

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
