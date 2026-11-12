package sharedbrowser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type authorizations struct {
	mu     sync.Mutex
	active map[string]computer.ExecutionSession
}

func TestStartupTimeoutClosesPrivatePipes(t *testing.T) {
	options, _, _ := testOptions()
	options.CallTimeout = 100 * time.Millisecond
	readResponse, writeResponse := io.Pipe()
	readCommand, writeCommand := io.Pipe()
	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, readCommand)
		drained <- err
	}()
	defer func() {
		if err := errors.Join(writeResponse.Close(), readCommand.Close()); err != nil {
			t.Error(err)
		}
	}()
	if _, err := New(context.Background(), readResponse, writeCommand, options); !errors.Is(err, ErrUnavailable) {
		t.Fatal("无响应浏览器没有明确启动失败", err)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("启动失败遗留命令管道")
	}
	if _, err := writeResponse.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("启动失败仍接收迟到事件", err)
	}
}

func TestEventOverflowAndWindowRecordLimits(t *testing.T) {
	options, _, sessions := testOptions()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{options: options, root: root, stop: cancel, events: make(chan any, 1), windows: map[string]*entry{}, targets: map[target.ID]*entry{}}
	for index := range options.MaximumWindowRecords {
		id := ids.New()
		item := &entry{session: sessions[0], window: Window{ID: id, State: "closed"}}
		service.windows[id] = item
		if index < options.MaximumPerSession {
			service.targets[target.ID(id)] = item
		}
	}
	if !service.full(sessions[0]) || !service.full(sessions[1]) {
		t.Fatal("关闭回执记录没有遵守容量上限")
	}
	service.windows = map[string]*entry{}
	if !service.full(sessions[0]) || service.full(sessions[1]) {
		t.Fatal("单桌面窗口额度影响其他桌面")
	}
	service.observe(&target.EventTargetDestroyed{})
	service.observe(&target.EventTargetDestroyed{})
	if root.Err() == nil || !service.failed {
		t.Fatal("事件溢出后仍继续使用不完整归属状态")
	}
}

func (access *authorizations) check(ctx context.Context, session computer.ExecutionSession) error {
	access.mu.Lock()
	defer access.mu.Unlock()
	if ctx.Err() != nil || access.active[session.SessionID] != session {
		return ErrDenied
	}
	return nil
}
func (access *authorizations) revoke(session computer.ExecutionSession) {
	access.mu.Lock()
	delete(access.active, session.SessionID)
	access.mu.Unlock()
}
func testOptions() (Options, *authorizations, []computer.ExecutionSession) {
	user, computerID, node := ids.New(), ids.New(), ids.New()
	access := &authorizations{active: map[string]computer.ExecutionSession{}}
	var sessions []computer.ExecutionSession
	for index := 1; index <= 2; index++ {
		session := computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: user, ComputerID: computerID, BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + index, GID: computer.DesktopSharedGID, DisplayNumber: index}, NodeID: node, ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
		access.active[session.SessionID] = session
		sessions = append(sessions, session)
	}
	options := Options{UserID: user, ComputerID: computerID, Generation: 1, Width: 960, Height: 640, MaximumWindows: 8, MaximumPerSession: 4, MaximumWindowRecords: 32, MaximumEvents: 128, CallTimeout: 5 * time.Second, DialogTimeout: 10 * time.Second, ShutdownTimeout: 10 * time.Second, Authorize: access.check, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return options, access, sessions
}

func TestBoundedPipeFrames(t *testing.T) {
	encoded := append(bytes.Repeat([]byte{'x'}, maximumFrameBytes), 0)
	reader := &frameReader{ReadCloser: io.NopCloser(bytes.NewReader(append(encoded, encoded...)))}
	if count, err := io.Copy(io.Discard, reader); err != nil || count != int64(len(encoded)*2) {
		t.Fatal("完整有界帧被拒绝", count, err)
	}
	reader = &frameReader{ReadCloser: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, maximumFrameBytes+1)))}
	if _, err := io.Copy(io.Discard, reader); !errors.Is(err, ErrUnavailable) {
		t.Fatal("超限帧未封闭", err)
	}
	writer := frameWriter{shortWrite{}}
	if _, err := writer.Write([]byte("message\x00")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("短写未失败", err)
	}
	if _, err := writer.Write(make([]byte, maximumCommandBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatal("超限命令未拒绝", err)
	}
}

type shortWrite struct{}

func (shortWrite) Write(value []byte) (int, error) { return len(value) - 1, nil }
func (shortWrite) Close() error                    { return nil }

func TestWindowListRefreshFailuresAndRevocation(t *testing.T) {
	for _, phase := range []string{"失败", "取消", "撤销", "缺失", "空目标", "重复目标", "错误类型", "标题超限", "地址超限", "并发关闭"} {
		t.Run(phase, func(t *testing.T) {
			options, access, sessions := testOptions()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			id := ids.New()
			item := &entry{session: sessions[0], target: target.ID(id), window: Window{ID: id, BotID: sessions[0].BotID, State: "ready", Title: "旧缓存"}}
			service := &Service{options: options, root: t.Context(), windows: map[string]*entry{id: item}}
			calls := 0
			service.listTargets = func(bounded context.Context) ([]*target.Info, error) {
				calls++
				info := &target.Info{TargetID: item.target, Type: "page", Title: "新标题"}
				switch phase {
				case "失败":
					return nil, errors.New("测试协议失败")
				case "取消":
					cancel()
					<-bounded.Done()
				case "撤销":
					access.revoke(sessions[0])
				case "缺失":
					return nil, nil
				case "空目标":
					return []*target.Info{nil}, nil
				case "重复目标":
					return []*target.Info{info, info}, nil
				case "错误类型":
					info.Type = "service_worker"
				case "标题超限":
					info.Title = strings.Repeat("x", 4097)
				case "地址超限":
					info.URL = strings.Repeat("x", 8193)
				case "并发关闭":
					service.mu.Lock()
					item.window.State = "closed"
					service.mu.Unlock()
				}
				return []*target.Info{info}, nil
			}
			windows, err := service.List(ctx, sessions[0])
			if phase == "并发关闭" {
				if err != nil || len(windows) != 0 {
					t.Fatal("关闭窗口被旧查询复活", windows, err)
				}
			} else if err == nil || windows != nil {
				t.Fatal("状态不完整或授权撤销后仍返回窗口缓存", windows, err)
			}
			if calls != 1 {
				t.Fatal("窗口列表没有使用单次有界查询", calls)
			}
		})
	}
}

func TestWindowAuthorizationAndInputBounds(t *testing.T) {
	options, access, sessions := testOptions()
	service := &Service{options: options, root: context.Background(), windows: map[string]*entry{}}
	var listed []*target.Info
	for _, session := range sessions {
		id := ids.New()
		service.windows[id] = &entry{session: session, target: target.ID(id), window: Window{ID: id, BotID: session.BotID, State: "ready"}}
		listed = append(listed, &target.Info{TargetID: target.ID(id), Type: "page", Title: "更新后的标题", URL: "https://example.test"})
	}
	service.listTargets = func(context.Context) ([]*target.Info, error) { return listed, nil }
	first, err := service.List(context.Background(), sessions[0])
	if err != nil || len(first) != 1 || first[0].BotID != sessions[0].BotID || first[0].Title != "更新后的标题" || first[0].URL != "https://example.test" {
		t.Fatal("窗口未按完整会话隔离", err)
	}
	if _, err := service.own(context.Background(), Address{Session: sessions[1], WindowID: first[0].ID}); !errors.Is(err, ErrDenied) {
		t.Fatal("跨 Bot 引用通过", err)
	}
	altered := sessions[0]
	altered.FencingToken++
	if _, err := service.List(context.Background(), altered); !errors.Is(err, ErrDenied) {
		t.Fatal("错误代次通过", err)
	}
	access.revoke(sessions[0])
	if _, err := service.List(context.Background(), sessions[0]); !errors.Is(err, ErrDenied) {
		t.Fatal("撤销后仍可枚举", err)
	}
	for _, command := range []Command{{Sequence: 1, Kind: "navigate", URL: "https://example.test"}, {Sequence: 2, Kind: "type", Text: "中文"}, {Sequence: 3, Kind: "click", X: 959, Y: 639}, {Sequence: 4, Kind: "key", Key: "Enter"}, {Sequence: 5, Kind: "scroll", DeltaY: 2000}} {
		if command.Validate(960, 640) != nil {
			t.Fatal("正常命令未通过", command.Kind)
		}
	}
	for _, command := range []Command{{Kind: "reload"}, {Sequence: 1, Kind: "navigate", URL: "file:///etc/private"}, {Sequence: 1, Kind: "navigate", URL: "https://name@example.test"}, {Sequence: 1, Kind: "type", Text: "\x00"}, {Sequence: 1, Kind: "click", X: 960}, {Sequence: 1, Kind: "move", Text: "extra"}, {Sequence: 1, Kind: "key", Key: "Enter", Modifiers: 16}, {Sequence: 1, Kind: "scroll"}, {Sequence: 1, Kind: "scroll", DeltaY: 2001}} {
		if command.Validate(960, 640) == nil {
			t.Fatal("非法命令通过", command.Kind)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	cancel()
	if err := acquire(ctx, gate); !errors.Is(err, context.Canceled) {
		t.Fatal("取消后仍等待门禁", err)
	}
}
