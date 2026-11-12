//go:build linux

package computerprobe

import (
	"context"
	"errors"
	"net"
)

const maximumProbeInterfaces = 64

func inspect(ctx context.Context) (Evidence, error) {
	if err := checkTerminal(ctx); err != nil {
		return Evidence{}, err
	}
	filesystem, size, err := checkFiles(ctx, "/workspace")
	if err != nil {
		return Evidence{}, err
	}
	if err = checkNetwork(ctx); err != nil {
		return Evidence{}, err
	}
	return Evidence{Terminal: true, Files: true, LocalNetwork: true, FilesystemType: filesystem, FilesystemSize: size}, nil
}

// checkNetwork 读取当前网络命名空间的实际接口和地址；代理连通性仍由节点另行核实。
func checkNetwork(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) == 0 || len(interfaces) > maximumProbeInterfaces {
		return errors.New("能力探针无法读取受限网络接口")
	}
	loopbacks := 0
	for _, network := range interfaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		addresses, err := network.Addrs()
		if err != nil {
			return errors.New("能力探针无法读取网络接口地址")
		}
		loopback, err := checkInterface(network, addresses)
		if err != nil {
			return err
		}
		if loopback {
			loopbacks++
		}
	}
	if loopbacks != 1 {
		return errors.New("能力探针没有唯一的环回网络接口")
	}
	return ctx.Err()
}

// checkInterface 允许内核预建但未启用且无地址的接口；任何可用的非环回接口都必须拒绝。
// 节点仍须核对 Docker 的 none 网络模式及最小权限，接口快照不能代替出网策略验收。
func checkInterface(network net.Interface, addresses []net.Addr) (bool, error) {
	if network.Name != "lo" {
		if network.Flags&(net.FlagUp|net.FlagRunning|net.FlagLoopback) != 0 || len(addresses) != 0 {
			return false, errors.New("能力探针发现非环回网络接口已启用或带有地址")
		}
		return false, nil
	}
	if network.Flags&net.FlagLoopback == 0 || network.Flags&net.FlagUp == 0 || len(addresses) == 0 || len(addresses) > 2 {
		return false, errors.New("能力探针无法确认环回网络状态")
	}
	for _, address := range addresses {
		if address == nil {
			return false, errors.New("能力探针无法确认环回网络地址")
		}
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || !ip.IsLoopback() {
			return false, errors.New("能力探针发现非环回网络地址")
		}
	}
	return true, nil
}
