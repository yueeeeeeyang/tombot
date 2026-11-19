package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodeenrollment"
	"io.yueyang/tom/internal/noderelease"
)

// NodeInstallOptions 仅描述本节点差异，省略项由探测与发布清单提供；不接受凭据。
type NodeInstallOptions struct {
	Name     string `json:"node_name,omitempty"`
	Capacity int    `json:"computer_capacity,omitempty"`
	URL      string `json:"node_url,omitempty"`
	Listen   string `json:"listen,omitempty"`
}

type nodeRelease = noderelease.Profile
type installationInput struct {
	request       nodeInstallRequest
	token         []byte
	archiveSource string
}
type installationHost struct {
	Hostname    string
	Addresses   []netip.Addr
	CPUMillis   int64
	MemoryBytes uint64
	DiskBytes   uint64
}

// prepareAutomatic 在安装锁内读取公开发布资料，只在首次安装询问接入码；重试使用已持久化快照。
func (installer nodeInstaller) prepareAutomatic(ctx context.Context, optionsPath, executable string, readCode func(context.Context) ([]byte, error), confirmAddress func(context.Context, string) (string, error)) (installationInput, error) {
	var options NodeInstallOptions
	if optionsPath != "" {
		encoded, err := installer.fs.readInstallationSource(optionsPath, 16<<10, installer.caller)
		if err != nil {
			return installationInput{}, err
		}
		if decodePreparation(encoded, &options) != nil {
			return installationInput{}, errors.New("高级安装参数无效，仅支持节点名称、容量、访问地址和监听地址")
		}
	}
	canonical, err := json.Marshal(options)
	if err != nil {
		return installationInput{}, errors.New("安装选项无法编码")
	}
	if _, err := os.Lstat(installer.fs.resolve(deploy.NodeDirectory)); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return installationInput{}, errors.New("已有安装无法读取")
		}
		encoded, err := installer.fs.readConfig(fileRule{path: filepath.Join(deploy.NodeDirectory, "installation.json"), maximum: 256 << 10, private: true}, account{})
		if err != nil {
			return installationInput{}, err
		}
		var record nodeInstallation
		if decodePreparation(encoded, &record) != nil || record.Version != 2 {
			return installationInput{}, errors.New("已有安装记录不属于此版本，未覆盖")
		}
		request, digest, err := parseNodeInstallRequest(record.Request)
		if err != nil || digest != record.RequestSHA256 || !bytes.Equal(request.InstallOptions, canonical) {
			return installationInput{}, errors.New("安装选项或保存的配置已变化，未覆盖原身份")
		}
		return installationInput{request: request}, nil
	}
	profileBytes, err := installer.fs.readInstallationSource(filepath.Join(filepath.Dir(executable), "node-release.json"), 64<<10, installer.caller)
	if err != nil {
		return installationInput{}, errors.New("缺少配套离线安装清单，请把程序包和电脑镜像包解压到同一目录后运行")
	}
	var release nodeRelease
	if decodePreparation(profileBytes, &release) != nil || (release.Version != 1 && release.Version != 2) || release.Platform != installer.platform || computer.ValidateImageReference(release.ImageReference) != nil || len(release.ArchiveSHA256) != 64 || strings.Trim(release.ArchiveSHA256, "0123456789abcdef") != "" {
		return installationInput{}, errors.New("离线发布清单或平台无效")
	}
	if _, err := dockerbootstrap.ParseManifest(release.Docker); err != nil {
		return installationInput{}, err
	}
	if release.Version == 2 {
		if _, err := installer.checkUpgradePackage(filepath.Dir(executable), nil); err != nil {
			return installationInput{}, err
		}
	} else if programs, err := installer.fs.hasReleaseManifest(filepath.Dir(executable), "program-release.json", noderelease.ProgramSums); err != nil {
		return installationInput{}, err
	} else if programs {
		return installationInput{}, errMixedNodePackages
	}
	discover := installer.detectHost
	if discover == nil {
		discover = inspectInstallationHost
	}
	host, err := discover(ctx)
	if err != nil {
		return installationInput{}, err
	}
	if readCode == nil || confirmAddress == nil {
		return installationInput{}, errors.New("首次安装需要交互终端")
	}
	encodedCode, err := readCode(ctx)
	if err != nil {
		return installationInput{}, err
	}
	defer clear(encodedCode)
	code, err := nodeenrollment.ParseJoinCode(encodedCode)
	if err != nil {
		return installationInput{}, err
	}
	fetch := installer.fetchBootstrap
	if fetch == nil {
		fetch = nodeenrollment.FetchBootstrap
	}
	bootstrap, err := fetch(ctx, code)
	if err != nil {
		return installationInput{}, err
	}
	if err := bootstrap.Validate(); err != nil {
		return installationInput{}, err
	}
	nodeID := ids.New()
	if options.Name == "" {
		options.Name = host.Hostname + "-" + strings.ToLower(nodeID[len(nodeID)-6:])
	}
	capacity := recommendedCapacity(host, bootstrap.VerificationResources)
	if options.Capacity == 0 {
		options.Capacity = capacity
	}
	if options.Capacity < 1 || options.Capacity > capacity {
		return installationInput{}, errors.New("节点资源不足或指定容量超过保留系统及验收资源后的可用容量")
	}
	listen := options.Listen
	if listen == "" && len(host.Addresses) == 1 {
		listen = net.JoinHostPort(host.Addresses[0].String(), "9444")
	}
	if listen == "" {
		listen, err = confirmAddress(ctx, "无法唯一确定网卡，请填写本机监听 IP:端口（建议 9444）：")
		if err != nil {
			return installationInput{}, err
		}
	}
	address, err := nodeapi.ParseListenAddress(listen, true)
	if err != nil || address.Port() == 0 || !isLocalListen(address.Addr(), host.Addresses) {
		return installationInput{}, errors.New("监听地址必须是本机有效网卡地址或明确指定的通配地址")
	}
	nodeURL := options.URL
	if nodeURL == "" {
		suggested := ""
		if !address.Addr().IsUnspecified() {
			suggested = "https://" + address.String()
		}
		prompt := "请确认服务端可访问的节点 HTTPS 地址"
		if suggested != "" {
			prompt += "（回车使用 " + suggested + "；NAT/端口映射请填写实际地址）"
		}
		nodeURL, err = confirmAddress(ctx, prompt+"：")
		if err != nil {
			return installationInput{}, err
		}
		if nodeURL == "" {
			nodeURL = suggested
		}
	}
	if nodeapi.ValidateURL(nodeURL) != nil {
		return installationInput{}, errors.New("节点访问地址必须是有效 HTTPS 来源")
	}
	if sameNodeOrigin(nodeURL, code.URL) {
		return installationInput{}, errors.New("节点地址不能与服务端地址相同；同机部署请使用独立端口，例如服务端 9443、节点 9444")
	}
	if installer.checkListen != nil {
		if err := installer.checkListen(ctx, listen); err != nil {
			return installationInput{}, err
		}
	}
	request := nodeInstallRequest{Version: 1, NodeID: nodeID, ImageID: ids.New(), Name: options.Name, Capacity: options.Capacity, InstallOptions: canonical,
		Server:  &nodeInstallServer{URL: code.URL, Fingerprints: code.Fingerprints, NodeURL: nodeURL},
		Control: nodeInstallControl{Listen: listen, AllowPublic: !address.Addr().IsPrivate(), ClientFingerprints: bootstrap.ClientFingerprints},
		Image:   imageSettings{Reference: release.ImageReference, ArchiveFile: filepath.Join(deploy.NodeDirectory, "computer-image.tar"), ArchiveSHA256: release.ArchiveSHA256, CallTimeoutSeconds: 30, TimeoutSeconds: 3600}, Docker: release.Docker}
	if release.Version == 2 {
		binary, err := installer.fs.readInstallationSource(executable, maximumNodeBinaryBytes, installer.caller)
		if err != nil {
			return installationInput{}, err
		}
		request.Programs, _, err = installer.stagePrograms(ctx, filepath.Dir(executable), binary)
		if err != nil {
			return installationInput{}, err
		}
	}
	if _, _, err := renderNodeInstallation(request, installer.platform); err != nil {
		return installationInput{}, err
	}
	return installationInput{request: request, token: []byte(code.Token), archiveSource: filepath.Join(filepath.Dir(executable), "computer-image.tar")}, nil
}

func sameNodeOrigin(first, second string) bool {
	a, err := url.Parse(first)
	if err != nil {
		return false
	}
	b, err := url.Parse(second)
	if err != nil {
		return false
	}
	port := func(endpoint *url.URL) string {
		if endpoint.Port() == "" || endpoint.Port() == "443" {
			return "443"
		}
		return endpoint.Port()
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// checkNodeListen 提前报告端口占用；实际服务绑定仍是最终检查，探测不持有长期监听。
func checkNodeListen(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", address)
	if err != nil {
		return errors.New("节点监听地址不可用或端口已占用，请指定本机独立端口")
	}
	return listener.Close()
}

func isLocalListen(address netip.Addr, addresses []netip.Addr) bool {
	if address.IsUnspecified() {
		return true
	}
	for _, candidate := range addresses {
		if address == candidate {
			return true
		}
	}
	return false
}

// recommendedCapacity 为宿主及独立验收各预留资源，且不超过节点默认出网额度；建议值不取代实际验收。
func recommendedCapacity(host installationHost, probe computer.Resources) int {
	const gib = uint64(1 << 30)
	const cpuPerComputer int64 = 1000
	const memoryPerComputer uint64 = 2 * gib
	const diskPerComputer uint64 = 10 * gib
	reservedCPU := int64(1000) + int64(probe.CPUMillis)
	reservedMemory := 2*gib + uint64(probe.MemoryBytes)
	reservedDisk := 5*gib + uint64(probe.DiskBytes)
	if host.CPUMillis <= reservedCPU || host.MemoryBytes <= reservedMemory || host.DiskBytes <= reservedDisk {
		return 0
	}
	capacity := min(uint64((host.CPUMillis-reservedCPU)/cpuPerComputer), (host.MemoryBytes-reservedMemory)/memoryPerComputer, (host.DiskBytes-reservedDisk)/diskPerComputer, uint64(7))
	return int(capacity)
}
