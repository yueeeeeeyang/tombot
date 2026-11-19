package deployment

import (
	"context"
	"crypto/tls"
	"errors"
	"io.yueyang/tom/internal/noderelease"
	"os"
	"path/filepath"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/nodeenrollment"
)

// checkUpgradePackage 核对同包程序、发布清单和配套文件；大归档在发布到私有目录时流式校验。
func (installer nodeInstaller) checkUpgradePackage(directory string, program []byte) (string, error) {
	sums, err := installer.fs.readInstallationSource(filepath.Join(directory, "SHA256SUMS"), 64<<10, installer.caller)
	if err != nil {
		return "", errors.New("请从完整离线节点包执行 upgrade，缺少校验清单")
	}
	files, err := noderelease.ParseChecksums(sums)
	if err != nil {
		return "", err
	}
	if len(files) != 5 || files["tomnode"] == "" || files["computer-image.tar"] == "" || files["node-release.json"] == "" {
		return "", errors.New("请解压完整节点离线包后，从包含 computer-image.tar 和 node-release.json 的目录执行 ./tomnode upgrade")
	}
	if files["tomnode"] != contentDigest(program) {
		return "", errors.New("tomnode 与当前节点包的 SHA256SUMS 不匹配，请重新解压同一份完整节点离线包")
	}
	for _, name := range []string{"node-release.json", "node-install.example.json", "安装说明.txt"} {
		content, err := installer.fs.readInstallationSource(filepath.Join(directory, name), 64<<10, installer.caller)
		if err != nil || files[name] != contentDigest(content) {
			return "", errors.New("升级包文件缺失或摘要不符：" + name)
		}
		if name == "node-release.json" {
			var release nodeRelease
			if decodePreparation(content, &release) != nil || release.ArchiveSHA256 == "" || release.ArchiveSHA256 != files["computer-image.tar"] {
				return "", errors.New("升级包镜像摘要与发布清单不一致")
			}
		}
	}
	file, err := os.Lstat(installer.fs.resolve(filepath.Join(directory, "computer-image.tar")))
	if err != nil || !file.Mode().IsRegular() || file.Mode().Perm()&0022 != 0 || file.Size() < 1 || file.Size() > dockernode.MaximumImageArchive {
		return "", errors.New("升级包缺少安全的电脑镜像归档")
	}
	owner, ok := infoSysOwner(file)
	if !ok || owner.Nlink != 1 || (owner.Uid != installer.fs.owner && owner.Uid != installer.caller) {
		return "", errors.New("升级包镜像归档的归属或链接无效")
	}
	return contentDigest(sums), nil
}

func (installer nodeInstaller) maintainNode(ctx context.Context, request nodeInstallRequest, operation string, finish bool) error {
	if request.Server == nil {
		return errors.New("该节点没有已登记的控制服务")
	}
	pair, err := installer.fs.nodeKeyPair()
	if err != nil {
		return err
	}
	return nodeenrollment.Maintain(ctx, request.NodeID, enrollmentConfig(request), pair, nodeenrollment.MaintenanceRequest{OperationID: operation, Finish: finish})
}

func enrollmentConfig(request nodeInstallRequest) nodeenrollment.Config {
	return nodeenrollment.Config{Server: request.Server.runtime(), Registration: nodecatalog.Registration{Name: request.Name, Capacity: request.Capacity, ImageID: request.ImageID, PreviousImageID: request.PreviousImageID, ImageReference: request.Image.Reference}}
}

func (installer nodeInstaller) inspectNodeRecovery(ctx context.Context, request nodeInstallRequest, release string) (*nodeenrollment.RecoveryInspection, error) {
	pair, err := installer.fs.nodeKeyPair()
	if err != nil {
		return nil, err
	}
	return nodeenrollment.InspectRecovery(ctx, request.NodeID, enrollmentConfig(request), pair, release)
}

func (installer nodeInstaller) holdNodeRecovery(ctx context.Context, request nodeInstallRequest, operation string, target nodeenrollment.RecoveryTarget) error {
	pair, err := installer.fs.nodeKeyPair()
	if err != nil {
		return err
	}
	return nodeenrollment.Maintain(ctx, request.NodeID, enrollmentConfig(request), pair, nodeenrollment.MaintenanceRequest{OperationID: operation, Recovery: &target})
}

func (fs filesystem) nodeKeyPair() (tls.Certificate, error) {
	read := func(name string) ([]byte, error) {
		return fs.readConfig(fileRule{path: filepath.Join(deploy.NodeDirectory, name), maximum: 64 << 10, private: true}, account{})
	}
	cert, err := read("certificate.pem")
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := read("private.key")
	if err != nil {
		return tls.Certificate{}, err
	}
	defer clear(key)
	return tls.X509KeyPair(cert, key)
}

func updateUnits() map[string]string {
	return map[string]string{"tombot-node-update.service": deploy.NodeUpdateService, "tombot-node-update.timer": deploy.NodeUpdateTimer}
}
func updateUnitDigests() map[string]string {
	result := map[string]string{}
	for name, body := range updateUnits() {
		result[name] = contentDigest([]byte(body))
	}
	return result
}

// verifyUpdateUnits 不接管手工改写的服务；恢复时只接受已记录的原值或当前包中的候选值。
func (installer nodeInstaller) verifyUpdateUnits(previous map[string]string) error {
	for name, body := range updateUnits() {
		path := "/etc/systemd/system/" + name
		if _, err := os.Lstat(installer.fs.resolve(path)); errors.Is(err, os.ErrNotExist) {
			if previous[name] == "" {
				continue
			}
			return errors.New("已安装的更新单元缺失")
		} else if err != nil {
			return err
		}
		b, err := installer.fs.readConfig(fileRule{path: path, maximum: 64 << 10}, account{})
		if err != nil {
			return err
		}
		if string(b) != body && contentDigest(b) != previous[name] {
			return errors.New("更新单元已被修改，未覆盖")
		}
	}
	return nil
}

// startNodeServices 先安装独立更新器，主服务停机不影响下载、执行和回执。
func (installer nodeInstaller) startNodeServices(ctx context.Context, registered bool, previous map[string]string) error {
	enable := []string{"enable", "--now", nodeServiceName}
	if registered {
		if err := installer.verifyUpdateUnits(previous); err != nil {
			return err
		}
		for name, body := range updateUnits() {
			path := "/etc/systemd/system/" + name
			if previous[name] != "" && previous[name] != contentDigest([]byte(body)) {
				if err := (serverInstaller{fs: installer.fs}).replaceServerFile(path, []byte(body), 0644); err != nil {
					return err
				}
				continue
			}
			if err := installer.fs.installExactFile(ctx, path, []byte(body), 0644); err != nil {
				return err
			}
		}
		enable = append(enable, "tombot-node-update.timer")
	}
	for _, args := range [][]string{{"daemon-reload"}, enable, {"is-active", "--quiet", nodeServiceName}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := installer.systemctl(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// stopNodeForUpgrade 区分原先已失败与本次停机异常；不能在 systemd 强杀旧进程后立即覆盖文件。
func stopNodeForUpgrade(ctx context.Context) error {
	read := func() (map[string]string, error) {
		raw, err := runServerCommand(ctx, serverCommand{program: "/usr/bin/systemctl", arguments: []string{"show", nodeServiceName, "--property=LoadState,ActiveState,MainPID,Result"}, timeout: 15 * time.Second})
		if err != nil {
			return nil, err
		}
		return parseServiceProperties(raw), nil
	}
	before, err := read()
	if err != nil {
		return err
	}
	if before["LoadState"] != "loaded" {
		return errors.New("节点系统服务未加载")
	}
	if err = runNodeSystemctl(ctx, "stop", nodeServiceName); err != nil {
		return err
	}
	after, err := read()
	if err != nil {
		return err
	}
	if after["MainPID"] != "0" || (after["ActiveState"] != "inactive" && after["ActiveState"] != "failed") || (after["Result"] != "success" && before["ActiveState"] != "failed" && before["ActiveState"] != "inactive") {
		return errors.New("节点服务未正常退出，未发布新程序，请检查系统日志")
	}
	return nil
}
