//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
)

const windowSocketName = "browser.sock"

type windowSession struct {
	session     computer.ExecutionSession
	instance    *browserInstance
	root        *os.Root
	socket      os.FileInfo
	options     Options
	logger      *slog.Logger
	onFatal     func()
	stop        context.CancelFunc
	life        context.Context
	authorize   sharedbrowser.Authorizer
	directMu    sync.Mutex
	directCalls sync.WaitGroup
	revoked     bool
	done        chan struct{}
	serveError  error
	retireOnce  sync.Once
	retired     chan struct{}
	retireError error
	gate        chan struct{}
	closed      bool
	closeError  error
	fatalOnce   sync.Once
}

// openWindowSession 仅在新会话目录准备后调用；监听器固定属于该会话，不接受业务请求选择路径或 Chrome。
func openWindowSession(reference processgroup.Reference, browsers *BrowserController, authorize sharedbrowser.Authorizer, options Options, logger *slog.Logger, onFatal func()) (WindowSession, error) {
	windows, err := openWindowSessionWithPeer(reference, browsers, authorize, options, logger, onFatal, checkWindowPeer)
	if err != nil {
		return nil, err
	}
	return windows, nil
}

func openWindowSessionWithPeer(reference processgroup.Reference, browsers *BrowserController, authorize sharedbrowser.Authorizer, options Options, logger *slog.Logger, onFatal func(), check func(context.Context, processgroup.Reference, sharedbrowser.Peer) error) (_ *windowSession, failure error) {
	if reference.Validate() != nil || browsers == nil || authorize == nil || options.validate() != nil || logger == nil || onFatal == nil || check == nil {
		return nil, errors.New("浏览器桌面入口配置无效")
	}
	session := reference.Session
	instance, err := browsers.windowSource(session)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(session.RuntimeDirectory(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("浏览器桌面目录不可用")
	}
	parent := os.NewFile(uintptr(fd), "window-directory")
	parentClosed := false
	defer func() {
		if !parentClosed {
			failure = errors.Join(failure, parent.Close())
		}
	}()
	var owner unix.Stat_t
	if unix.Fstat(fd, &owner) != nil || owner.Uid != uint32(session.UID) || owner.Gid != uint32(session.GID) || owner.Mode&07777 != 0700 {
		return nil, errors.New("浏览器桌面目录身份或权限不符")
	}
	root, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return nil, errors.New("固定浏览器桌面目录失败")
	}
	path := "/proc/self/fd/" + strconv.Itoa(fd) + "/" + windowSocketName
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.Join(errors.New("浏览器桌面入口已存在或不能创建"), root.Close())
	}
	listener.SetUnlinkOnClose(false)
	prepared := false
	defer func() {
		if !prepared {
			failure = errors.Join(failure, listener.Close(), root.Close())
		}
	}()
	if err := root.Chown(windowSocketName, 0, session.GID); err != nil {
		return nil, errors.New("设置浏览器桌面入口身份失败")
	}
	if err := root.Chmod(windowSocketName, 0660); err != nil {
		return nil, errors.New("设置浏览器桌面入口权限失败")
	}
	socket, err := root.Lstat(windowSocketName)
	if err != nil || socket.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("复验浏览器桌面入口失败")
	}
	limits := instance.grant.Limits
	// 每个窗口允许画面与动作各一条连接，总量受网关现有的 16 条连接硬上限约束。
	authorizeCurrent := func(ctx context.Context, current computer.ExecutionSession) error {
		active, err := browsers.windowSource(current)
		if current != session || err != nil || active != instance {
			return sharedbrowser.ErrDenied
		}
		return authorize(ctx, session)
	}
	gateway, err := sharedbrowser.NewGateway(instance.service, session, func(ctx context.Context, current computer.ExecutionSession, peer sharedbrowser.Peer) error {
		if err := check(ctx, reference, peer); err != nil {
			return err
		}
		return authorizeCurrent(ctx, current)
	}, sharedbrowser.GatewayOptions{Width: limits.Width, Height: limits.Height, MaximumConnections: min(2*limits.MaximumPerSession, 16), CallTimeout: max(time.Second, time.Duration(limits.CallTimeoutMS)*time.Millisecond), ShutdownTimeout: options.CleanupTimeout}, logger)
	if err != nil {
		return nil, err
	}
	parentClosed = true
	if err := parent.Close(); err != nil {
		return nil, errors.New("关闭浏览器桌面目录描述符失败")
	}
	life, stop := context.WithCancel(context.Background())
	windows := &windowSession{session: session, instance: instance, root: root, socket: socket, options: options, logger: logger, onFatal: onFatal, life: life, authorize: authorizeCurrent, stop: stop, done: make(chan struct{}), retired: make(chan struct{}), gate: make(chan struct{}, 1)}
	prepared = true
	go func() {
		windows.serveError = gateway.Serve(life, listener)
		if life.Err() == nil {
			windows.serveError = errors.Join(windows.serveError, errors.New("浏览器桌面入口意外停止"))
			windows.fail()
		}
		close(windows.done)
	}()
	logger.Info("浏览器桌面入口已绑定原共享实例", "user_id", session.UserID, "computer_id", session.ComputerID, "bot_id", session.BotID, "session_id", session.SessionID, "service_id", instance.grant.Request.Group.ID)
	return windows, nil
}

// checkWindowPeer 只接受当前执行会话的 Agent 所在 shell 子组；其他 Bot 和旧运行不能操作所属页面。
func checkWindowPeer(ctx context.Context, reference processgroup.Reference, peer sharedbrowser.Peer) error {
	if ctx.Err() != nil || peer.PID <= 1 || peer.UID != uint32(reference.Session.UID) || peer.GID != uint32(reference.Session.GID) {
		return sharedbrowser.ErrDenied
	}
	file, err := os.Open("/proc/" + strconv.Itoa(peer.PID) + "/cgroup")
	if err != nil {
		return sharedbrowser.ErrDenied
	}
	encoded, readError := io.ReadAll(io.LimitReader(file, 16385))
	closeError := file.Close()
	if readError != nil || closeError != nil || len(encoded) > 16384 || strings.TrimSpace(string(encoded)) != "0::/"+reference.Name()+"/shell" || ctx.Err() != nil {
		return sharedbrowser.ErrDenied
	}
	return nil
}

func (windows *windowSession) fail() {
	windows.fatalOnce.Do(func() {
		windows.stop()
		windows.logger.Error("浏览器桌面入口或所属页面未确认回收，必须退出容器", "user_id", windows.session.UserID, "computer_id", windows.session.ComputerID, "bot_id", windows.session.BotID, "session_id", windows.session.SessionID)
		windows.onFatal()
	})
}

// Revoke 独立关闭页面，不能只断开输入而让页面脚本在租约结束后继续执行；不等待任务回收或状态刷盘。
func (windows *windowSession) Revoke() {
	windows.directMu.Lock()
	windows.revoked = true
	windows.directMu.Unlock()
	windows.stop()
	windows.retireOnce.Do(func() {
		go func() {
			defer close(windows.retired)
			ctx, cancel := context.WithTimeout(context.Background(), windows.options.CleanupTimeout)
			defer cancel()
			completed := make(chan error, 1)
			go func() {
				select {
				case <-windows.done:
					if windows.serveError != nil {
						completed <- windows.serveError
						return
					}
				case <-ctx.Done():
					completed <- ctx.Err()
					return
				}
				windows.directCalls.Wait()
				if ctx.Err() != nil {
					completed <- ctx.Err()
					return
				}
				completed <- windows.instance.service.ReleaseSession(ctx, windows.session)
			}()
			select {
			case err := <-completed:
				windows.retireError = errors.Join(err, ctx.Err())
			case <-ctx.Done():
				windows.retireError = ctx.Err()
			}
			if windows.retireError != nil {
				windows.fail()
			}
		}()
	})
}

// UseBrowser 供代理内通过会话验证的调用使用；不借用桌面客户端 UID，也不把全局 CDP 传给 Agent。
// 在途调用纳入会话撤销屏障，授权失效后的迟到结果必须丢弃，原实例关闭后不会重绑定。
func (windows *windowSession) UseBrowser(ctx context.Context, session computer.ExecutionSession, action func(context.Context, sharedbrowser.Browser) error) error {
	if action == nil || session != windows.session {
		return sharedbrowser.ErrDenied
	}
	windows.directMu.Lock()
	if windows.revoked || windows.life.Err() != nil {
		windows.directMu.Unlock()
		return sharedbrowser.ErrDenied
	}
	windows.directCalls.Add(1)
	windows.directMu.Unlock()
	defer windows.directCalls.Done()
	call, cancel := context.WithTimeout(ctx, windows.options.CallTimeout)
	defer cancel()
	stop := context.AfterFunc(windows.life, cancel)
	defer stop()
	if err := windows.authorize(call, session); err != nil {
		return err
	}
	if err := call.Err(); err != nil {
		return err
	}
	err := action(call, windows.instance.service)
	if call.Err() != nil || windows.life.Err() != nil {
		return sharedbrowser.ErrUnknown
	}
	// 网页动作可能已经生效；回执阶段失去授权不能报告为安全的拒绝并允许重试。
	verified := windows.authorize(call, session)
	if verified != nil || call.Err() != nil || windows.life.Err() != nil {
		return sharedbrowser.ErrUnknown
	}
	return err
}

func (scope *kernelScope) BrowserSession(session computer.ExecutionSession) (WindowSession, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != session || scope.windows == nil {
		return nil, sharedbrowser.ErrDenied
	}
	return scope.windows, nil
}

// Close 只消费一次原实例的会话回收；失败保留入口文件及错误，不向新实例重发关闭请求。
func (windows *windowSession) Close(ctx context.Context) (failure error) {
	windows.Revoke()
	ctx, cancel := context.WithTimeout(ctx, windows.options.CleanupTimeout)
	defer cancel()
	select {
	case windows.gate <- struct{}{}:
	case <-ctx.Done():
		windows.fail()
		return ctx.Err()
	}
	defer func() { <-windows.gate }()
	if windows.closed {
		return windows.closeError
	}
	defer func() {
		failure = errors.Join(failure, windows.root.Close())
		windows.closed, windows.closeError = true, failure
		if failure != nil {
			windows.fail()
		} else {
			windows.logger.InfoContext(ctx, "浏览器桌面入口与所属页面已回收", "user_id", windows.session.UserID, "computer_id", windows.session.ComputerID, "bot_id", windows.session.BotID, "session_id", windows.session.SessionID)
		}
	}()
	select {
	case <-windows.retired:
	case <-ctx.Done():
		return ctx.Err()
	}
	if windows.retireError != nil {
		return windows.retireError
	}
	actual, err := windows.root.Lstat(windowSocketName)
	if err != nil || !os.SameFile(actual, windows.socket) {
		return errors.New("浏览器桌面入口已变化，不能清理替换文件")
	}
	if err := windows.root.Remove(windowSocketName); err != nil {
		return errors.New("移除已关闭的浏览器桌面入口失败")
	}
	return ctx.Err()
}
