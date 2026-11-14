package deployment

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/dockerbootstrap"
)

// inspectInstallationHost 仅采集本机公开资源，不查询公网 IP；多网卡由安装者确认可达地址。
func inspectInstallationHost(ctx context.Context) (installationHost, error) {
	var host installationHost
	if err := ctx.Err(); err != nil {
		return host, err
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil || dockerbootstrap.ValidateHostRelease(release) != nil {
		return host, errors.New("自动安装目前支持 Ubuntu 24.04")
	}
	host.Hostname, err = os.Hostname()
	if err != nil {
		return host, errors.New("无法读取节点名称")
	}
	if len(host.Hostname) > 100 {
		host.Hostname = host.Hostname[:100]
	}
	host.CPUMillis = int64(runtime.NumCPU()) * 1000
	memory, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return host, errors.New("无法读取宿主内存")
	}
	for _, line := range strings.Split(string(memory), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			size, err := strconv.ParseUint(fields[1], 10, 54)
			if err != nil {
				return host, errors.New("宿主内存容量无效")
			}
			host.MemoryBytes = size * 1024
		}
	}
	if host.MemoryBytes == 0 {
		return host, errors.New("宿主内存容量未确定")
	}
	var disk unix.Statfs_t
	if unix.Statfs("/var/lib", &disk) != nil {
		return host, errors.New("无法读取节点存储空间")
	}
	host.DiskBytes = uint64(disk.Bavail) * uint64(disk.Bsize)
	interfaces, err := net.Interfaces()
	if err != nil {
		return host, errors.New("无法读取本机网卡")
	}
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagLoopback != 0 || strings.HasPrefix(network.Name, "docker") || strings.HasPrefix(network.Name, "veth") || strings.HasPrefix(network.Name, "br-") {
			continue
		}
		addresses, err := network.Addrs()
		if err != nil {
			return host, errors.New("无法读取网卡地址")
		}
		for _, value := range addresses {
			prefix, err := netip.ParsePrefix(value.String())
			if err == nil && prefix.Addr().IsGlobalUnicast() && !prefix.Addr().IsLoopback() {
				host.Addresses = append(host.Addresses, prefix.Addr())
			}
		}
	}
	return host, nil
}
