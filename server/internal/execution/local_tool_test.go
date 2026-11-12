package execution

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type localMetadata struct{}

func (localMetadata) Schema() provider.ToolSchema { return (testTool{}).Schema() }
func (localMetadata) UsesComputer() bool          { return true }
func (localMetadata) RequiresVision() bool        { return false }
func (localMetadata) RunsInAgent() bool           { return true }

type localWorkerExecutor struct {
	execute func(context.Context, botagent.ToolGrant, botagent.ToolCall) (botagent.ToolResult, error)
}

func (localWorkerExecutor) Supports(name string) bool    { return name == "file_read" }
func (localWorkerExecutor) Finish(context.Context) error { return nil }
func (executor localWorkerExecutor) Execute(ctx context.Context, grant botagent.ToolGrant, call botagent.ToolCall) (botagent.ToolResult, error) {
	return executor.execute(ctx, grant, call)
}

func TestWorkerLocalOperationPersistenceAndUnknownBarrier(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"正常", "未知", "权限撤销", "结果超限"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			fixture.toolsModel(t)
			fixture.send(t, fixture.a, fixture.first)
			lease := fixture.claim(t)
			models, actions := 0, 0
			worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				models++
				if models == 1 {
					return emitTool(emit, `{"path":"notes.txt"}`)
				}
				if input.Messages[2].Blocks[0].ToolResult.Content != "Local operation completed." {
					t.Error("本地结果未进入模型")
				}
				return emitText(emit, "已完成。")
			}), localMetadata{})
			worker.agents.(*workerAgents).local = localWorkerExecutor{execute: func(_ context.Context, grant botagent.ToolGrant, call botagent.ToolCall) (botagent.ToolResult, error) {
				actions++
				if grant.Validate() != nil || grant.Session.RunID != lease.RunID || grant.Session.BotID != lease.BotID || call.Name != "file_read" {
					t.Error("Agent 未收到完整本地执行身份")
				}
				var status string
				if err := fixture.pool.QueryRow(`SELECT status FROM tombot.run_operations WHERE id=$1 AND run_id=$2`, grant.OperationID, lease.RunID).Scan(&status); err != nil || status != "started" {
					t.Error("本地动作前没有持久记录", status, err)
				}
				if mode == "未知" {
					return botagent.ToolResult{}, errors.New("本地结果未确认")
				}
				if mode == "权限撤销" {
					execute(t, fixture.pool, `UPDATE tombot.computer_sessions SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID)
				}
				result := botagent.ToolResult{Content: "Local operation completed."}
				if mode == "结果超限" {
					result.Content = string(make([]byte, worker.options.MaxOutputBytes+1))
				}
				return result, nil
			}}
			run, err := worker.Process(t.Context(), lease)
			if err != nil {
				t.Fatal(err)
			}
			var status string
			if err := fixture.pool.QueryRow(`SELECT status FROM tombot.run_operations WHERE run_id=$1 AND kind='computer'`, lease.RunID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if mode == "正常" {
				if run.Status != "succeeded" || status != "succeeded" || models != 2 {
					t.Fatal(run.Status, status, models)
				}
			} else if run.Status == "succeeded" || status != "unknown" || models != 1 {
				t.Fatal("未知操作继续执行", run.Status, status, models)
			}
			if actions != 1 {
				t.Fatal("本地操作被重复执行", actions)
			}
		})
	}
}

func TestWorkerRejectsLocalToolOnServerPath(t *testing.T) {
	worker := &Worker{}
	if _, err := worker.callTool(t.Context(), Lease{}, computer.ExecutionSession{}, "standard", "tool.1.1", botagent.ToolCall{Name: "file_read"}, map[string]Tool{"file_read": localMetadata{}}, nil); !errors.Is(err, botagent.ErrModelOutput) {
		t.Fatal("服务端允许执行本地工具", err)
	}
}
