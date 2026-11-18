package execution

import (
	"context"
	"errors"
	"io"
	"io.yueyang/tom/internal/botagent"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// 只替代实际节点，用户认证、设备隔离、队列、电脑租约和接管结算均访问真实 PostgreSQL。
type consoleNode struct {
	*workerAgents
	*workerDesktop
	mu       sync.Mutex
	requests []computer.ConsoleRequest
	mode     string
}

func (node *consoleNode) Console(_ context.Context, request computer.ConsoleRequest) (computer.ConsoleReceipt, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	if request.Validate() != nil {
		return computer.ConsoleReceipt{}, ErrInvalidInput
	}
	node.requests = append(node.requests, request)
	receipt := computer.ConsoleReceipt{Session: request.Session, Revision: request.Revision, State: "paused"}
	switch request.Action {
	case "prepare":
		receipt.State = "ready"
	case "pause":
		if node.mode == "暂停失败" {
			return receipt, ErrUnavailable
		}
	case "resume":
		receipt.State = "running"
		if node.mode == "恢复失败" {
			return receipt, ErrUnavailable
		}
	}
	return receipt, nil
}
func (node *consoleNode) OpenDesktopStream(ctx context.Context, request computer.DesktopStream) (net.Conn, error) {
	if request.Validate() != nil {
		return nil, ErrInvalidInput
	}
	left, right := net.Pipe()
	go func() {
		defer right.Close()
		_, err := right.Write([]byte("RFB 003.008\n"))
		if err != nil {
			return
		}
		buffer := make([]byte, 1)
		_, err = right.Read(buffer)
		if err != nil {
			return
		}
	}()
	return left, nil
}
func waitConsoleState(t *testing.T, f fixture, id string, state string) Console {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		status, err := f.service.ConsoleStatus(t.Context(), f.a, id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == state {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("接管未进入 %s，实际 %s/%s", state, status.State, status.ErrorCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func TestDesktopConsoleLifecycle(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"正常", "过期", "暂停失败", "恢复失败"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, cluster)
			requestID := ids.New()
			opened, err := f.service.OpenConsole(t.Context(), f.a, f.first.ID, requestID)
			if err != nil {
				t.Fatal(err)
			}
			again, err := f.service.OpenConsole(t.Context(), f.a, f.first.ID, requestID)
			if err != nil || again.ID != opened.ID {
				t.Fatal("重复申请未幂等", err)
			}
			if _, err = f.service.OpenConsole(t.Context(), f.a, f.first.ID, ids.New()); !errors.Is(err, ErrConflict) {
				t.Fatal("同一电脑允许多个接管", err)
			}
			if _, err = f.service.ConsoleStatus(t.Context(), f.b, opened.ID); err == nil {
				t.Fatal("跨用户读取接管")
			}
			if _, err = f.service.OpenConsole(t.Context(), f.a, f.foreign.ID, ids.New()); !errors.Is(err, ErrNotFound) {
				t.Fatal("跨用户接管电脑", err)
			}
			lease := f.claim(t)
			worker := f.worker(t, callerFunc(func(context.Context, modelregistry.Configuration, []byte, modelgateway.Input, func(provider.Event) error) (modelgateway.Result, error) {
				t.Error("空闲电脑接管不应调用模型")
				return modelgateway.Result{}, ErrUnavailable
			}))
			node := &consoleNode{workerAgents: worker.agents.(*workerAgents), workerDesktop: &workerDesktop{pixels: screenshotPixels(t)}, mode: mode}
			worker.agents = node
			runtime, err := NewConsoleService(f.service, node, worker.logger)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := worker.Process(t.Context(), lease); done <- err }()
			waitConsoleState(t, f, opened.ID, "viewing")
			node.mu.Lock()
			before := len(node.requests)
			node.mu.Unlock()
			if before != 0 {
				t.Fatal("只读观看暂停了任务")
			}
			if _, err = runtime.Stream(t.Context(), f.b, opened.ID); err == nil {
				t.Fatal("跨用户读取画面")
			}
			stream, err := runtime.Stream(t.Context(), f.a, opened.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			banner := make([]byte, 12)
			if _, err = io.ReadFull(stream, banner); err != nil || string(banner) != "RFB 003.008\n" {
				t.Fatal("RFB 握手字节被改写", err)
			}
			if _, err = f.service.ControlConsole(t.Context(), f.a, opened.ID, true); err != nil {
				t.Fatal(err)
			}
			if mode != "暂停失败" {
				waitConsoleState(t, f, opened.ID, "active")

				// 接管期间仍可发送普通消息，不把消息混入无模型的接管运行。
				f.send(t, f.a, f.first)
				if mode == "过期" {
					execute(t, f.pool, `UPDATE tombot.desktop_consoles SET expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, f.a.UserID, opened.ID)
				} else {
					if _, err = f.service.CloseConsole(t.Context(), f.a, opened.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("接管工作器未结束")
			}
			state := "closed"
			if mode == "暂停失败" || mode == "恢复失败" {
				state = "failed"
			}
			waitConsoleState(t, f, opened.ID, state)
			if _, err = runtime.Stream(t.Context(), f.a, opened.ID); err == nil {
				t.Fatal("结束后仍可读取画面")
			}
			if err = stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
			if _, err = stream.Read(make([]byte, 1)); err == nil {
				t.Fatal("撤销后仍可读取 RFB")
			}

			if f.sessionState(t, lease) != "released" {
				t.Fatal("电脑接管未释放会话")
			}
			messages, err := f.service.Messages(t.Context(), f.a, f.first.ConversationID, 0, 50)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				if message.SenderKind == "bot" {
					t.Fatal("电脑接管伪造了聊天回复")
				}
			}
			node.mu.Lock()
			defer node.mu.Unlock()
			pauses, resumes := 0, 0
			for _, request := range node.requests {
				if request.Session.BotID != f.first.ID || request.Session.RunID != lease.RunID {
					t.Error("节点使用错误电脑身份")
				}
				if request.Action == "pause" {
					pauses++
				}
				if request.Action == "resume" {
					resumes++
					if request.Revision != 5 {
						t.Error("恢复未使用下一控制代次")
					}
				}
			}
			if pauses != 1 || mode != "暂停失败" && resumes != 1 {
				t.Fatal("暂停与恢复次数错误", pauses, resumes)
			}
		})
	}
}

// 接管复用正在执行的电脑；工具必须等用户交还后才继续，模型和工具都不能因交接被重放。
func TestDesktopConsoleResumesExistingRun(t *testing.T) {
	f := newFixture(t, pgtest.Start(t))
	f.toolsModel(t)
	f.send(t, f.a, f.first)
	lease := f.claim(t)
	modelEntered, continueModel := make(chan struct{}), make(chan struct{})
	var models, tools atomic.Int32
	worker := f.worker(t, callerFunc(func(ctx context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		if models.Add(1) == 1 {
			close(modelEntered)
			select {
			case <-continueModel:
			case <-ctx.Done():
				return modelgateway.Result{}, ctx.Err()
			}
			return emitTool(emit, `{"path":"/workspace/report.txt"}`)
		}
		return emitText(emit, "接管后继续完成任务")
	}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
		tools.Add(1)
		return botagent.ToolResult{Content: "File read complete."}, nil
	}})
	node := &consoleNode{workerAgents: worker.agents.(*workerAgents), workerDesktop: &workerDesktop{pixels: screenshotPixels(t)}}
	worker.agents = node
	done := make(chan error, 1)
	go func() {
		run, err := worker.Process(t.Context(), lease)
		if err == nil && run.Status != "succeeded" {
			err = errors.New("原任务未成功完成")
		}
		done <- err
	}()
	select {
	case <-modelEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("原任务未调用模型")
	}
	opened, err := f.service.OpenConsole(t.Context(), f.a, f.first.ID, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	otherDevice := f.a
	otherDevice.DeviceID, otherDevice.SessionID = ids.New(), ids.New()
	execute(t, f.pool, `INSERT INTO tombot.devices(user_id,id,name,platform) VALUES($1,$2,'另一设备','linux')`, otherDevice.UserID, otherDevice.DeviceID)
	execute(t, f.pool, `INSERT INTO tombot.user_sessions(user_id,id,device_id,auth_version,scope,expires_at) VALUES($1,$2,$3,1,'desktop',clock_timestamp()+interval '1 hour')`, otherDevice.UserID, otherDevice.SessionID, otherDevice.DeviceID)
	if _, err = f.service.ConsoleStatus(t.Context(), otherDevice, opened.ID); err == nil {
		t.Fatal("同用户另一设备读取接管状态")
	}
	if _, err = f.service.CloseConsole(t.Context(), otherDevice, opened.ID); err == nil {
		t.Fatal("同用户另一设备抢走控制")
	}
	var runID string
	if err = f.pool.QueryRow(`SELECT run_id FROM tombot.desktop_consoles WHERE user_id=$1 AND id=$2`, f.a.UserID, opened.ID).Scan(&runID); err != nil || runID != lease.RunID {
		t.Fatal("接管未复用原任务", err)
	}
	runtime, err := NewConsoleService(f.service, node, worker.logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Ready(t.Context(), f.a, opened.ID); err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, f, opened.ID, "viewing")
	if _, err = f.service.ControlConsole(t.Context(), f.a, opened.ID, true); err != nil {
		t.Fatal(err)
	}
	close(continueModel)
	waitConsoleState(t, f, opened.ID, "active")
	if tools.Load() != 0 || models.Load() != 1 {
		t.Fatal("接管期间任务继续操作")
	}
	if _, err = f.service.CloseConsole(t.Context(), f.a, opened.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("交还后任务没有恢复")
	}
	if tools.Load() != 1 || models.Load() != 2 || node.starts != 1 {
		t.Fatal("交接重放模型、工具或重启 Agent")
	}
	waitConsoleState(t, f, opened.ID, "closed")
}

func TestViewingDoesNotPauseActiveRun(t *testing.T) {
	f := newFixture(t, pgtest.Start(t))
	f.toolsModel(t)
	f.send(t, f.a, f.first)
	lease := f.claim(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	worker := f.worker(t, callerFunc(func(ctx context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		close(entered)
		select {
		case <-proceed:
		case <-ctx.Done():
			return modelgateway.Result{}, ctx.Err()
		}
		return emitText(emit, "观看期间照常完成")
	}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
		return botagent.ToolResult{}, errors.New("只读观看测试不应调用工具")
	}})
	node := &consoleNode{workerAgents: worker.agents.(*workerAgents), workerDesktop: &workerDesktop{pixels: screenshotPixels(t)}}
	worker.agents = node
	done := make(chan error, 1)
	go func() {
		run, err := worker.Process(t.Context(), lease)
		if err == nil && run.Status != "succeeded" {
			err = errors.New("观看阻止任务完成")
		}
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("模型启动前任务结束：%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("模型未启动")
	}
	opened, err := f.service.OpenConsole(t.Context(), f.a, f.first.ID, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewConsoleService(f.service, node, worker.logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Ready(t.Context(), f.a, opened.ID); err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, f, opened.ID, "viewing")
	close(proceed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("只读观看阻塞了原任务")
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, request := range node.requests {
		if request.Action == "pause" {
			t.Fatal("观看触发暂停")
		}
	}
	waitConsoleState(t, f, opened.ID, "closed")
}
