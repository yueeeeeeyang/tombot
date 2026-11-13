// Package deployment 在安装之前核对同一宿主上的配置关系，不发布节点能力或开户许可。
package deployment

import (
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/serverapp"
)

// Paths 只引用部署文档，不包含数据库连接或凭据原文。
type Paths struct {
	Server string
	Node   string
	Docker string
	Image  string
}

type configuration struct {
	server serverapp.Config
	node   nodeapp.Config
	image  dockernode.ImageManifest
}

// Report 仅证明此次只读配置检查通过，不能替代实际宿主、镜像和电脑验收。
type Report struct {
	Scope    string `json:"scope"`
	NodeID   string `json:"node_id"`
	ImageID  string `json:"image_id"`
	Platform string `json:"platform"`
	APIUID   uint32 `json:"api_uid"`
	APIGID   uint32 `json:"api_gid"`
}

type account struct {
	uid uint32
	gid uint32
}

func within(path, directory string) bool {
	return path == directory || strings.HasPrefix(path, directory+"/")
}

func validPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && len(path) > 1 && len(path) <= 4096 && !strings.ContainsAny(path, "\x00\r\n")
}

func (paths Paths) files() []string {
	return []string{paths.Server, paths.Node, paths.Docker, paths.Image}
}

func (value configuration) directories() []string {
	return []string{value.node.Runtime.StateDirectory, value.node.Runtime.StorageDirectory, value.node.Storage.StateDirectory, value.image.StateDirectory, dockerbootstrap.StateDirectory, dockerbootstrap.RuntimeDirectory, value.server.AdminDirectory}
}

// validateGraph 在各配置自身验证之后检查身份、期限和路径，不推断数据库或 Docker 状态。
func (value configuration) validateGraph(paths Paths, api account, platform string) error {
	server, node, image := value.server, value.node, value.image
	if api.uid == 0 || api.gid == 0 {
		return errors.New("API 系统账户无效")
	}
	if server.Node.ID != node.NodeID || node.NodeID != image.NodeID || server.Node.Verification.ImageID != image.ImageID {
		return errors.New("API、节点与镜像清单身份不一致")
	}
	// 跨主机后 API 与节点不再共享 socket 路径：只核对同端口与互相固定的证书指纹。
	nodePort := nodeAddressPort(server.Node.URL)
	listenPort := nodeListenPort(node.Listen)
	if nodePort == 0 || nodePort != listenPort {
		return errors.New("API 节点地址与节点监听端口不一致")
	}
	if node.Runtime.Socket != image.Socket || image.Socket != dockerbootstrap.SocketPath {
		return errors.New("节点、镜像与 Docker 引导的控制端点不一致")
	}
	if image.Platform != platform {
		return errors.New("镜像平台与当前宿主架构不一致")
	}
	maximumLease := time.Duration(node.Runtime.MaximumLease) * time.Millisecond
	if maximumLease < server.Provision.LeaseDuration.Value() || maximumLease < server.Node.Verification.LeaseDuration.Value() {
		return errors.New("节点接受的租约短于开户或节点验收租约")
	}
	if time.Duration(node.API.CallTimeout)*time.Millisecond < server.Node.CallTimeout.Value() {
		return errors.New("节点调用预算短于 API 节点请求预算")
	}
	if server.Provision.DesktopConcurrency > node.Agent.MaximumActive {
		return errors.New("用户桌面额度超过电脑代理的并发容量")
	}
	directories := value.directories()
	for index, directory := range directories {
		if !validPath(directory) {
			return errors.New("部署目录路径无效")
		}
		for _, other := range directories[index+1:] {
			if within(directory, other) || within(other, directory) {
				return errors.New("引导、镜像、节点、用户磁盘或管理静态目录重叠")
			}
		}
		// 控制证书同时属于部署凭据，统一由下方文件清单的重叠检查拒绝，这里只拦 Docker 引导入口。
		if within(image.Socket, directory) || within(directory, image.Socket) {
			return errors.New("控制端点不得放入部署数据或管理静态目录")
		}
	}
	files := append(paths.files(), value.referencePaths()...)
	seen := make(map[string]bool, len(files))
	for _, path := range files {
		if !validPath(path) || seen[path] || path == image.Socket {
			return errors.New("部署文件路径无效、复用或与控制端点冲突")
		}
		seen[path] = true
		for _, directory := range directories {
			if within(path, directory) || within(directory, path) {
				return errors.New("部署配置或凭据与数据或静态目录重叠")
			}
		}
	}
	return nil
}

// nodeAddressPort 返回 API 配置中节点 URL 的端口；解析失败返回 0 交给调用方拒绝。
func nodeAddressPort(value string) int {
	parsed, err := url.Parse(value)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return 0
	}
	return port
}

func nodeListenPort(value string) int {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return 0
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return number
}
