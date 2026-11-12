//go:build linux

package computeragent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sync/errgroup"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/egressproxy"
)

func egressBridge(store *FileStore, configuration Configuration, logger *slog.Logger) (*egressproxy.Bridge, net.Listener, error) {
	address, err := computer.EgressAddress()
	if err != nil {
		return nil, nil, err
	}
	bridge, err := egressproxy.NewBridge(configuration.Options.egressOptions(), func(ctx context.Context) (net.Conn, error) { return connectEgress(ctx, store) }, logger.With("user_id", configuration.Identity.UserID, "computer_id", configuration.Identity.ComputerID, "generation", configuration.Identity.Generation))
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, nil, errors.New("电脑环回出网端口无法监听")
	}
	return bridge, listener, nil
}

func connectEgress(ctx context.Context, store *FileStore) (connection net.Conn, failure error) {
	stat, err := store.root.Lstat(EgressSocketName)
	if err != nil {
		return nil, errors.New("电脑出网节点端点尚未提供")
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || owner.Nlink != 1 || stat.Mode()&os.ModeSocket == 0 || stat.Mode().Perm() != 0600 {
		return nil, errors.New("电脑出网节点端点未受保护")
	}
	parent, err := store.root.Open(".")
	if err != nil {
		return nil, errors.New("电脑控制目录不可读取")
	}
	defer func() {
		if err := parent.Close(); err != nil {
			failure = errors.Join(failure, errors.New("关闭电脑控制目录失败"))
		}
		if failure != nil && connection != nil {
			failure = errors.Join(failure, connection.Close())
			connection = nil
		}
	}()
	path := "/proc/self/fd/" + strconv.Itoa(int(parent.Fd())) + "/" + EgressSocketName
	value, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("电脑出网节点连接失败")
	}
	connection = value
	unix, ok := value.(*net.UnixConn)
	if !ok || egressproxy.RequireHostConnection(unix) != nil {
		return connection, errors.New("电脑出网节点身份错误")
	}
	return connection, nil
}

func serveWithEgress(ctx context.Context, serve, egress func(context.Context) error) error {
	group, running := errgroup.WithContext(ctx)
	for _, run := range []func(context.Context) error{serve, egress} {
		group.Go(func() error {
			err := run(running)
			if err == nil && running.Err() == nil {
				return errors.New("电脑代理网络服务提前结束")
			}
			return err
		})
	}
	return group.Wait()
}
