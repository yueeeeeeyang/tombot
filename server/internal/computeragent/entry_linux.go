//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

// Run 只供容器入口使用。任何失败都要求退出进程，以终止整个容器；不能嵌入宿主服务。
func Run(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		return errors.New("电脑代理日志未配置")
	}
	if err := taskprocess.CheckController(); err != nil {
		return err
	}
	store, err := NewFileStore(ControlDirectory)
	if err != nil {
		return err
	}
	// 初始化失败时没有用户执行；此后正常关闭由 supervisor 统一处理，致命故障靠入口退出释放。
	configuration, err := loadConfiguration(store)
	if err != nil {
		return errors.Join(err, store.Close())
	}
	options := configuration.Options
	if err = prepareFilesystem(ctx, time.Duration(options.CallTimeoutMS)*time.Millisecond); err != nil {
		logger.ErrorContext(ctx, "电脑代理运行空间准备失败", "user_id", configuration.Identity.UserID, "computer_id", configuration.Identity.ComputerID, "generation", configuration.Identity.Generation, "reason", err.Error())
		return errors.Join(err, store.Close())
	}
	var controller *Controller
	var browsers *BrowserController
	authorize := func(ctx context.Context, session computer.ExecutionSession) error {
		if controller == nil {
			return errors.New("电脑代理尚未完成桌面授权组合")
		}
		return controller.withScope(ctx, session, func(context.Context, *desktopGuard) error { return nil })
	}
	// 工厂只在全部监督器构造完成并开始接单后使用，窗口不能在恢复阶段发放执行权。
	factory, err := NewKernelScopes(time.Duration(options.PollIntervalMS)*time.Millisecond, func(reference processgroup.Reference) (WindowSession, error) {
		if controller == nil || browsers == nil {
			return nil, errors.New("电脑代理尚未完成窗口服务组合")
		}
		return openWindowSession(reference, browsers, authorize, options.guardOptions(), logger, controller.fail)
	})
	if err != nil {
		return errors.Join(err, store.Close())
	}
	controller, err = NewController(ctx, configuration.Identity, store, factory, options.guardOptions(), options.MaximumActive, logger)
	if err != nil {
		return errors.Join(err, store.Close())
	}
	probeFactory, ok := factory.(ProbeFactory)
	if !ok {
		return errors.Join(errors.New("电脑代理缺少固定探针执行后端"), store.Close())
	}
	probes, err := NewProbeController(ctx, configuration.Identity, store, probeFactory, options.guardOptions(), logger, controller.fail)
	if err != nil {
		logger.ErrorContext(ctx, "电脑代理探针状态恢复失败", "user_id", configuration.Identity.UserID, "computer_id", configuration.Identity.ComputerID, "generation", configuration.Identity.Generation, "reason", err.Error())
		return errors.Join(err, store.Close())
	}
	browserFactory, ok := factory.(BrowserFactory)
	if !ok {
		return errors.Join(errors.New("电脑代理缺少共享浏览器后端"), store.Close())
	}
	browsers, err = NewBrowserController(ctx, configuration.Identity, store, browserFactory, options.guardOptions(), authorize, logger, controller.fail)
	if err != nil {
		logger.ErrorContext(ctx, "电脑代理共享浏览器状态恢复失败", "user_id", configuration.Identity.UserID, "computer_id", configuration.Identity.ComputerID, "generation", configuration.Identity.Generation, "reason", err.Error())
		return errors.Join(err, store.Close())
	}
	server, err := NewServer(controller, probes, browsers, RequireHostPeer, time.Duration(options.CallTimeoutMS)*time.Millisecond, options.MaximumConnections, logger)
	if err != nil {
		return errors.Join(err, store.Close())
	}
	listener, err := listenControl(store)
	if err != nil {
		return errors.Join(err, store.Close())
	}
	bridge, egressListener, err := egressBridge(store, configuration, logger)
	if err != nil {
		return errors.Join(err, listener.Close(), store.Close())
	}
	logger.InfoContext(ctx, "电脑代理控制入口已启动，业务能力尚须节点探测", "user_id", configuration.Identity.UserID, "computer_id", configuration.Identity.ComputerID, "generation", configuration.Identity.Generation)
	return supervise(ctx, time.Duration(options.ShutdownTimeoutMS)*time.Millisecond,
		func(ctx context.Context) error {
			return serveWithEgress(ctx, func(ctx context.Context) error { return server.Serve(ctx, listener) }, func(ctx context.Context) error { return bridge.Serve(ctx, egressListener) })
		}, controller.Serve, func(ctx context.Context) error {
			return shutdownControllers(ctx, controller, probes, browsers)
		}, store.Close)
}

func loadConfiguration(store *FileStore) (configuration Configuration, failure error) {
	file, err := store.open(ConfigName, os.O_RDONLY, 0)
	if err != nil {
		return configuration, errors.New("电脑代理部署配置不可用")
	}
	defer func() { failure = errors.Join(failure, file.Close()) }()
	if err = store.check(file); err != nil {
		return configuration, err
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(encoded) > 4096 {
		return configuration, errors.New("电脑代理部署配置读取失败或超过限制")
	}
	if err = agentchannel.Decode(encoded, &configuration); err != nil {
		return configuration, err
	}
	return configuration, configuration.Validate()
}

// listenControl 只在已独占的持久目录内清理旧 socket，不删除普通文件或链接。
func listenControl(store *FileStore) (*net.UnixListener, error) {
	name := filepath.Join(store.root.Name(), SocketName)
	stat, err := os.Lstat(name)
	if err == nil {
		identity, ok := stat.Sys().(*syscall.Stat_t)
		if !ok || identity.Uid != store.owner || stat.Mode()&os.ModeSocket == 0 || stat.Mode().Perm() != 0600 {
			return nil, errors.New("电脑代理控制套接字被替换")
		}
		if err = store.root.Remove(SocketName); err != nil {
			return nil, errors.New("清理电脑代理旧套接字失败")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("检查电脑代理控制套接字失败")
	}
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: name, Net: "unixpacket"})
	if err != nil {
		return nil, errors.New("建立电脑代理控制监听失败")
	}
	if err = os.Chmod(name, 0600); err != nil {
		return nil, errors.Join(errors.New("设置电脑代理控制套接字权限失败"), listener.Close())
	}
	return listener, nil
}
