package dockernode

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"io.yueyang/tom/internal/computeragent"
)

// agentClient 共用受保护目录和固定 socket 的连接规则，实例归属由调用方复验。
func (node *Runtime) agentClient(resource Resource, check computeragent.PeerCheck, timeout time.Duration) (*computeragent.Client, error) {
	return computeragent.NewClient(func(ctx context.Context) (connection *net.UnixConn, failureResult error) {
		defer func() {
			if failureResult != nil && connection != nil {
				failureResult = errors.Join(failureResult, connection.Close())
				connection = nil
			}
		}()
		directory, err := node.agentDirectory(resource, false)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err := directory.root.Close(); err != nil {
				failureResult = errors.Join(failureResult, err)
			}
		}()
		stat, err := directory.root.Lstat(computeragent.SocketName)
		if errors.Is(err, os.ErrNotExist) {
			return nil, errAgentNotListening
		}
		if err != nil {
			return nil, failure("agent_socket_missing")
		}
		owner, ok := stat.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || stat.Mode()&os.ModeSocket == 0 || stat.Mode().Perm() != 0600 {
			return nil, failure("agent_socket_unsafe")
		}
		parent, err := directory.root.Open(".")
		if err != nil {
			return nil, failure("agent_socket_directory")
		}
		defer func() {
			if err := parent.Close(); err != nil {
				failureResult = errors.Join(failureResult, err)
			}
		}()
		// 通过固定目录描述符寻址，避免 Unix 路径长度限制和根目录路径替换。
		path := "/proc/self/fd/" + strconv.Itoa(int(parent.Fd())) + "/" + computeragent.SocketName
		dialer := net.Dialer{}
		value, err := dialer.DialContext(ctx, "unixpacket", path)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errAgentNotListening
		}
		if err != nil {
			return nil, failure("agent_connect_failed")
		}
		result, ok := value.(*net.UnixConn)
		if !ok {
			return nil, errors.Join(failure("agent_connection_type"), value.Close())
		}
		connection = result
		return connection, nil
	}, check, timeout)
}
