package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
)

// 同一等待快照之后续传，只应看到该调用后来提交的阶段；历史分页须返回相同的最终记录。
func checkToolTimeline(t *testing.T, s *Server, conversationID string, cursor int64, pending *domain.ToolActivity, decision string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	events, resync, err := s.Bus.Read(ctx, cursor)
	if err != nil || resync {
		t.Fatal("无法从审批快照的游标续传", err)
	}
	var phases []string
	var terminal domain.ToolActivity
	for _, ev := range events {
		if ev.ConversationID != conversationID || (ev.Kind != "tool.started" && ev.Kind != "tool.status" && ev.Kind != "tool.output") {
			continue
		}
		var item domain.ToolActivity
		if err = json.Unmarshal(ev.Payload, &item); err != nil {
			t.Fatal(err)
		}
		if item.ID != pending.ID || item.RunID != pending.RunID || item.Step != pending.Step {
			t.Fatal("恢复后的调用身份变化")
		}
		phases = append(phases, item.Status)
		terminal = item
	}
	want := []string{"pending", "running", "succeeded"}
	switch decision {
	case "deny":
		want = []string{"pending", "denied"}
	case "cancel_running":
		want = []string{"pending", "running", "canceled"}
		if terminal.ErrorCode != "TOOL_CANCELED" {
			t.Fatal("取消工具的错误码丢失", terminal.ErrorCode)
		}
	}
	if !slices.Equal(want, phases) || terminal.DurationMS == nil || *terminal.DurationMS < 0 {
		t.Fatal("工具事件阶段或耗时不符", phases, terminal.DurationMS)
	}
	snap, err := s.Conversations.Snapshot(ctx, conversationID)
	if err != nil || snap.ActiveTurnID != nil || len(snap.ActiveRuns) != 0 {
		t.Fatal("完成后快照仍有活跃步骤", err)
	}
	var found int
	for _, message := range snap.Messages {
		if len(message.ToolCalls) == 0 {
			continue
		}
		// 每页只取一条，分别读到原始调用和之后独立写入的工具回复。
		page, err := s.Conversations.Messages(ctx, conversationID, message.Seq+1, 1)
		if err != nil || len(page) != 1 || len(page[0].ToolCalls) != 1 {
			t.Fatal("单消息分页没有执行记录", err)
		}
		for _, item := range []domain.ToolActivity{message.ToolCalls[0], page[0].ToolCalls[0]} {
			if item.ID != terminal.ID || item.Status != terminal.Status || item.DurationMS == nil || *item.DurationMS != *terminal.DurationMS {
				t.Fatal("分页、快照与实时事件的最终状态不一致")
			}
		}
		found++
	}
	if found != 2 {
		t.Fatal("原始调用和结果没有同时关联记录", found)
	}
}

// cancelExecutingShell 在子进程已存在时通过用户接口取消，并同时核对系统进程与持久化执行状态。
func cancelExecutingShell(t *testing.T, s *Server, bot domain.Bot, sent agent.Submission, headers map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	path := filepath.Join(s.Config.WorkspaceDir, bot.WorkspaceDir, "notes", "child.pid")
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var pid int
	for pid == 0 {
		encoded, err := os.ReadFile(path)
		if err == nil {
			pid, err = strconv.Atoi(string(encoded))
			if err != nil || pid <= 1 {
				t.Fatal("命令进程标识无效", err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("已审批的命令没有启动", ctx.Err())
		case <-tick.C:
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("取消前命令子进程已经退出", err)
	}
	response := request(t, s.Handler(), http.MethodPost, "/api/v1/turns/"+sent.TurnID+"/cancel", nil, headers)
	if response.Code != http.StatusAccepted {
		t.Fatal("未受理回合取消", response.Code)
	}
	for {
		run, err := s.Runtime.Run(ctx, sent.RunID)
		if err != nil {
			t.Fatal(err)
		}
		processErr := syscall.Kill(pid, 0)
		if processErr != nil && !errors.Is(processErr, syscall.ESRCH) {
			t.Fatal(processErr)
		}
		if run.Status == "canceled" && errors.Is(processErr, syscall.ESRCH) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("回合或命令子进程仍未停止", run.Status, processErr)
		case <-tick.C:
		}
	}
}
