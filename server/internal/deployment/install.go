package deployment

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/nodeenrollment"
)

const nodeServiceName = "tombot-node.service"

type nodeInstaller struct {
	checkListen         func(context.Context, string) error
	detectHost          func(context.Context) (installationHost, error)
	fetchBootstrap      func(context.Context, nodeenrollment.JoinCode) (nodeenrollment.BootstrapConfiguration, error)
	fs                  filesystem
	platform            string
	caller              uint32
	logger              *slog.Logger
	systemctl           func(context.Context, ...string) error
	readEnrollmentToken func(context.Context) ([]byte, error)
	prepareInstallation func(context.Context) (installationInput, error)
}

// InstallNode 安装当前程序、生成节点配置与证书并启动系统服务。
// 参数文件仅含公开部署参数；相同参数和程序可重试，冲突、取消和服务启动失败均返回错误并保留已发布身份。
func InstallNode(ctx context.Context, requestPath, executable string, caller uint32, logger *slog.Logger, readEnrollmentToken func(context.Context) ([]byte, error), confirmAddress func(context.Context, string) (string, error)) (NodeInstallationReport, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || logger == nil {
		return NodeInstallationReport{}, errors.New("节点安装必须在 Linux 宿主以 root 执行")
	}
	fs := filesystem{base: "/", owner: 0}
	if err := fs.directory("/run/systemd/system", account{}, false, false, false); err != nil {
		return NodeInstallationReport{}, errors.New("节点安装要求 systemd 系统服务环境")
	}
	info, err := fs.inspect(fileRule{path: "/usr/bin/systemctl", maximum: 64 << 20}, account{})
	if err != nil || info.Mode().Perm()&0111 == 0 {
		return NodeInstallationReport{}, errors.New("系统服务管理程序缺失或权限无效")
	}
	installer := nodeInstaller{fs: fs, platform: "linux/" + runtime.GOARCH, caller: caller, logger: logger, systemctl: runNodeSystemctl, readEnrollmentToken: readEnrollmentToken, checkListen: checkNodeListen}
	installer.prepareInstallation = func(ctx context.Context) (installationInput, error) {
		return installer.prepareAutomatic(ctx, requestPath, executable, readEnrollmentToken, confirmAddress)
	}
	return installer.install(ctx, requestPath, executable)
}

func runNodeSystemctl(parent context.Context, arguments ...string) error {
	budget := 30 * time.Second
	if len(arguments) > 0 && arguments[0] == "enable" {
		budget = 141 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", arguments...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "SYSTEMD_PAGER=cat"}
	command.WaitDelay = time.Second
	// 不回传 systemctl 原始输出，避免服务配置或私有路径进入安装回执。
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("节点系统服务操作取消或超时，保留安装文件并检查服务实际状态")
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return fmt.Errorf("systemctl %s 失败（退出码 %d），请运行 journalctl -u tombot-node -n 80 --no-pager 查看具体启动阶段", strings.Join(arguments, " "), exited.ExitCode())
		}
		return fmt.Errorf("无法执行 systemctl %s，请检查系统服务管理程序", strings.Join(arguments, " "))
	}
	return nil
}

func (installer nodeInstaller) install(ctx context.Context, requestPath, executable string) (report NodeInstallationReport, result error) {
	if err := ctx.Err(); err != nil {
		return report, err
	}
	program, err := installer.fs.readInstallationSource(executable, maximumNodeBinaryBytes, installer.caller)
	if err != nil {
		return report, err
	}
	if err := validateNodeExecutable(program, installer.platform); err != nil {
		return report, err
	}
	unlock, err := installer.fs.lockNodeInstallation()
	if err != nil {
		return report, err
	}
	defer func() { result = errors.Join(result, unlock()) }()
	service := []byte(deploy.NodeService())
	for _, target := range []struct {
		path    string
		content []byte
		mode    os.FileMode
	}{{nodeBinaryPath, program, 0755}, {nodeServicePath, service, 0644}} {
		if _, err := installer.fs.existingInstallationFile(target.path, target.content, target.mode); err != nil {
			return report, err
		}
	}
	var input installationInput
	if installer.prepareInstallation != nil {
		input, err = installer.prepareInstallation(ctx)
	} else {
		var encoded []byte
		encoded, err = installer.fs.readInstallationSource(requestPath, 256<<10, installer.caller)
		if err == nil {
			input.request, _, err = parseNodeInstallRequest(encoded)
		}
	}
	defer clear(input.token)
	if err != nil {
		return report, err
	}
	request := input.request
	encoded, err := json.Marshal(request)
	if err != nil {
		return report, errors.New("安装参数无法编码")
	}
	request, requestDigest, err := parseNodeInstallRequest(encoded)
	if err != nil {
		return report, err
	}
	value, files, err := renderNodeInstallation(request, installer.platform)
	if err != nil {
		return report, err
	}
	if value.image.AuthenticationFile != "" {
		if _, err := installer.fs.inspect(fileRule{path: value.image.AuthenticationFile, maximum: 16 << 10, private: true}, account{}); err != nil {
			return report, err
		}
	}
	record, exists, err := installer.loadInstallation(requestDigest, contentDigest(program))
	if err != nil {
		return report, err
	}
	if !exists {
		if value.node.Enrollment != nil {
			token := input.token
			if token == nil {
				if installer.readEnrollmentToken == nil {
					return report, errors.New("首次安装需要从终端粘贴接入凭据码")
				}
				token, err = installer.readEnrollmentToken(ctx)
				defer clear(token)
				if err != nil {
					return report, err
				}
			}
			if nodeenrollment.ValidateToken(token) != nil {
				return report, errors.New("接入凭据码格式无效，请重新复制")
			}
			files = append(files, configurationFile{name: nodeEnrollmentFile, content: token, mode: 0600})
		}
		if value.image.ArchiveFile != "" {
			if value.image.ArchiveFile != filepath.Join(deploy.NodeDirectory, "computer-image.tar") || input.archiveSource == "" {
				return report, errors.New("离线镜像来源无效")
			}
			files = append(files, configurationFile{name: "computer-image.tar", source: input.archiveSource, digest: value.image.ArchiveSHA256, mode: 0600})
		}
		identity, err := createNodeCertificate(value.node.NodeID, time.Now().UTC())
		if err != nil {
			return report, err
		}
		files = append(files, identity...)
		defer clear(identity[1].content)
		record = nodeInstallation{Version: 2, Request: encoded, RequestSHA256: requestDigest, BinarySHA256: contentDigest(program), Files: map[string]string{}}
		for _, file := range files {
			if file.source != "" {
				record.Files[file.name] = file.digest
			} else {
				record.Files[file.name] = contentDigest(file.content)
			}
		}
		receipt, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return report, errors.New("生成节点安装记录失败")
		}
		files = append(files, configurationFile{name: "installation.json", content: receipt, mode: 0600})
		if err := installer.publishInstallation(ctx, files); err != nil {
			return report, err
		}
	}
	report, err = installer.verifyInstallation(ctx, record)
	if err != nil {
		return NodeInstallationReport{}, err
	}
	if _, err := checkNode(ctx, nodeInstallationPaths(), installer.platform, installer.fs); err != nil {
		return NodeInstallationReport{}, err
	}
	installer.logger.Info("节点配置与证书已核验", "node_id", report.NodeID)
	if err := installer.fs.installExactFile(ctx, nodeBinaryPath, program, 0755); err != nil {
		return NodeInstallationReport{}, err
	}
	if err := installer.fs.installExactFile(ctx, nodeServicePath, service, 0644); err != nil {
		return NodeInstallationReport{}, err
	}
	for _, arguments := range [][]string{{"daemon-reload"}, {"enable", "--now", nodeServiceName}, {"is-active", "--quiet", nodeServiceName}} {
		if err := ctx.Err(); err != nil {
			return NodeInstallationReport{}, err
		}
		if err := installer.systemctl(ctx, arguments...); err != nil {
			return NodeInstallationReport{}, err
		}
	}
	report.State = "control_started"
	installer.logger.Info("节点已安装并设置开机启动，电脑能力仍须独立验收", "node_id", report.NodeID)
	return report, nil
}

func validateNodeExecutable(content []byte, platform string) error {
	machine := map[string]uint16{"linux/amd64": 62, "linux/arm64": 183}[platform]
	if machine == 0 || len(content) < 64 || !bytes.Equal(content[:7], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1}) || binary.LittleEndian.Uint16(content[18:20]) != machine {
		return errors.New("节点安装程序必须是与宿主架构匹配的 Linux 二进制")
	}
	if kind := binary.LittleEndian.Uint16(content[16:18]); kind != 2 && kind != 3 {
		return errors.New("节点安装程序必须是可执行文件")
	}
	return nil
}

func (installer nodeInstaller) loadInstallation(requestDigest, programDigest string) (nodeInstallation, bool, error) {
	if err := installer.fs.directory(filepath.Dir(deploy.NodeDirectory), account{}, false, false, true); err != nil {
		return nodeInstallation{}, false, err
	}
	_, err := os.Lstat(installer.fs.resolve(deploy.NodeDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return nodeInstallation{}, false, nil
	}
	if err != nil {
		return nodeInstallation{}, false, errors.New("节点安装目录无法读取")
	}
	if err := installer.fs.directory(deploy.NodeDirectory, account{}, false, false, false); err != nil {
		return nodeInstallation{}, false, err
	}
	content, err := installer.fs.readConfig(fileRule{path: filepath.Join(deploy.NodeDirectory, "installation.json"), maximum: 256 << 10, private: true}, account{})
	if err != nil {
		return nodeInstallation{}, false, errors.New("已有节点目录缺少有效安装记录，未覆盖")
	}
	var record nodeInstallation
	if decodePreparation(content, &record) != nil || record.Version != 2 || record.RequestSHA256 != requestDigest || record.BinarySHA256 != programDigest {
		return record, false, errors.New("安装参数、程序版本或记录与原安装不一致，未覆盖节点身份")
	}
	return record, true, nil
}

func (installer nodeInstaller) publishInstallation(ctx context.Context, files []configurationFile) (result error) {
	parent := filepath.Dir(deploy.NodeDirectory)
	if err := installer.fs.ensureInstallDirectory(parent, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(installer.fs.resolve(parent), ".node-install-")
	if err != nil {
		return errors.New("无法创建节点配置暂存目录")
	}
	published := false
	defer func() {
		if !published && os.RemoveAll(stage) != nil {
			result = errors.Join(result, errors.New("节点配置暂存目录清理失败"))
		}
	}()
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if file.source != "" {
			err = installer.fs.copyInstallationArchive(ctx, file.source, filepath.Join(stage, file.name), file.digest, installer.caller)
		} else {
			err = writeConfigurationFile(filepath.Join(stage, file.name), file.content, file.mode)
		}
		if err != nil {
			return err
		}
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishPath(stage, installer.fs.resolve(deploy.NodeDirectory)); err != nil {
		return errors.New("节点配置发布失败，未覆盖既有目录")
	}
	published = true
	return syncDirectory(installer.fs.resolve(parent))
}

func (installer nodeInstaller) verifyInstallation(ctx context.Context, record nodeInstallation) (NodeInstallationReport, error) {
	names := []string{"node.json", "docker-install.json", "computer-image.json", "certificate.pem", "private.key"}
	contents := map[string][]byte{}
	defer func() { clear(contents["private.key"]) }()
	for _, name := range names {
		content, err := installer.fs.readConfig(fileRule{path: filepath.Join(deploy.NodeDirectory, name), maximum: 64 << 10, private: true}, account{})
		if err != nil {
			return NodeInstallationReport{}, err
		}
		contents[name] = content
		if record.Files[name] != contentDigest(content) {
			return NodeInstallationReport{}, errors.New("节点配置或证书与安装记录不一致，未重新生成或覆盖")
		}
	}
	node, err := nodeapp.ParseConfig(contents["node.json"])
	if err != nil {
		return NodeInstallationReport{}, err
	}
	if node.Enrollment != nil {
		if node.Enrollment.Server.TokenFile != filepath.Join(deploy.NodeDirectory, nodeEnrollmentFile) {
			return NodeInstallationReport{}, errors.New("安装器接入凭据引用无效")
		}
		token, err := installer.fs.readConfig(fileRule{path: node.Enrollment.Server.TokenFile, maximum: 128, private: true}, account{})
		defer clear(token)
		if err != nil {
			return NodeInstallationReport{}, err
		}
		if record.Files[nodeEnrollmentFile] != contentDigest(token) || nodeenrollment.ValidateToken(token) != nil {
			return NodeInstallationReport{}, errors.New("接入凭据与安装记录不一致，未覆盖")
		}
		names = append(names, nodeEnrollmentFile)
	}
	if node.Enrollment != nil && len(record.Request) == 0 {
		return NodeInstallationReport{}, errors.New("缺少安装请求快照")
	}
	var imageManifest dockernode.ImageManifest
	imageManifest, err = dockernode.ParseImageManifest(contents["computer-image.json"])
	if err != nil {
		return NodeInstallationReport{}, err
	}
	if imageManifest.ArchiveFile != "" {
		if !installationArchivePath(imageManifest.ArchiveFile, imageManifest.ArchiveSHA256) || record.Files[filepath.Base(imageManifest.ArchiveFile)] != imageManifest.ArchiveSHA256 {
			return NodeInstallationReport{}, errors.New("离线镜像记录不一致")
		}
		if _, err := installer.fs.inspect(fileRule{path: imageManifest.ArchiveFile, maximum: dockernode.MaximumImageArchive, private: true}, account{}); err != nil {
			return NodeInstallationReport{}, err
		}
		if err := installer.fs.verifyInstallationArchive(ctx, imageManifest.ArchiveFile, imageManifest.ArchiveSHA256); err != nil {
			return NodeInstallationReport{}, err
		}
		names = append(names, filepath.Base(imageManifest.ArchiveFile))
	}
	if len(record.Files) != len(names) {
		return NodeInstallationReport{}, errors.New("节点安装记录的文件集合无效")
	}
	image, err := dockernode.ParseImageManifest(contents["computer-image.json"])
	if err != nil {
		return NodeInstallationReport{}, err
	}
	return installationReport(node, image, contents["certificate.pem"], contents["private.key"])
}
