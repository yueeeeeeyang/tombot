package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
)

const nodeRepairDirectory = "/etc/tombot/node-repair"

// NodeRepairOptions 允许修正端点和导入完整离线镜像包，不接受新节点身份或凭据。
type NodeRepairOptions struct {
	ImagePackage string `json:"image_package,omitempty"`
	Listen       string `json:"listen"`
	URL          string `json:"url"`
}

type nodeRepairFile struct {
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type nodeRepair struct {
	Version int               `json:"version"`
	Options NodeRepairOptions `json:"options"`
	Program string            `json:"program"`
	Files   []nodeRepairFile  `json:"files"`
}

func nodeRepairTargets() []struct {
	name, path string
	mode       os.FileMode
} {
	return []struct {
		name, path string
		mode       os.FileMode
	}{
		{"program", nodeBinaryPath, 0755}, {"service", nodeServicePath, 0644},
		{"node.json", deploy.NodeDirectory + "/node.json", 0600},
		{"docker-install.json", deploy.NodeDirectory + "/docker-install.json", 0600},
		{"computer-image.json", deploy.NodeDirectory + "/computer-image.json", 0600},
		{"installation.json", deploy.NodeDirectory + "/installation.json", 0600},
	}
}

// RepairNode 备份并迁移安装器管理的配置，再启动当前程序；保留节点身份、证书与授权；显式指定离线包时更换镜像。
// 只处理已停止的服务，未完成的修复必须使用同一程序和选项恢复，任何内容漂移都拒绝覆盖。
func RepairNode(ctx context.Context, executable string, caller uint32, options NodeRepairOptions, logger *slog.Logger) (NodeInstallationReport, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || logger == nil {
		return NodeInstallationReport{}, errors.New("节点修复需要在 Linux 以 root 执行")
	}
	fs := filesystem{base: "/", owner: 0}
	if err := fs.directory("/run/systemd/system", account{}, false, false, false); err != nil {
		return NodeInstallationReport{}, err
	}
	installer := nodeInstaller{fs: fs, caller: caller, platform: "linux/" + runtime.GOARCH, logger: logger, systemctl: runNodeSystemctl, checkListen: checkNodeListen}
	checkStopped := func(ctx context.Context) error {
		output, err := runServerCommand(ctx, serverCommand{program: "/usr/bin/systemctl", arguments: []string{"show", nodeServiceName, "--property=LoadState,ActiveState"}, timeout: 15 * time.Second})
		if err != nil {
			return err
		}
		state := parseServiceProperties(output)
		if state["LoadState"] != "loaded" || state["ActiveState"] != "inactive" && state["ActiveState"] != "failed" {
			return errors.New("修复前请先停止节点：systemctl stop tombot-node；有运行任务时须先排空")
		}
		return nil
	}
	return installer.repair(ctx, executable, options, checkStopped)
}

func (installer nodeInstaller) repair(ctx context.Context, executable string, options NodeRepairOptions, checkStopped func(context.Context) error) (report NodeInstallationReport, result error) {
	unlock, err := installer.fs.lockNodeInstallation()
	if err != nil {
		return report, err
	}
	defer func() { result = errors.Join(result, unlock()) }()
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
	if err := checkStopped(ctx); err != nil {
		return report, err
	}
	journalPath := nodeRepairDirectory + "/plan.json"
	var journal nodeRepair
	if _, err := os.Lstat(installer.fs.resolve(nodeRepairDirectory)); errors.Is(err, os.ErrNotExist) {
		if err := installer.prepareNodeRepair(ctx, program, options); err != nil {
			return report, err
		}
	} else if err != nil {
		return report, errors.New("无法读取节点修复目录")
	}
	encoded, err := installer.fs.readConfig(fileRule{path: journalPath, maximum: 64 << 10, private: true}, account{})
	if err != nil {
		return report, err
	}
	if decodePreparation(encoded, &journal) != nil || journal.Version != 1 || journal.Options != options || journal.Program != contentDigest(program) || len(journal.Files) != len(nodeRepairTargets()) {
		return report, errors.New("待恢复的节点修复与此次程序或参数不一致，未覆盖")
	}
	// 先核对所有备份、目标和安装状态，再开始替换；崩溃恢复仅接受计划中的前后两个值。
	after := map[string][]byte{}
	for index, target := range nodeRepairTargets() {
		item := journal.Files[index]
		if item.Name != target.name {
			return report, errors.New("节点修复文件集合无效")
		}
		for _, revision := range []struct{ directory, digest string }{{"before", item.Before}, {"after", item.After}} {
			content, err := installer.fs.readConfig(fileRule{path: nodeRepairDirectory + "/" + revision.directory + "/" + target.name, maximum: maximumNodeBinaryBytes, private: true}, account{})
			if err != nil || contentDigest(content) != revision.digest {
				return report, errors.New("节点修复备份摘要或权限不一致，未覆盖")
			}
			if revision.directory == "after" {
				after[target.name] = content
			}
		}
		current, err := installer.fs.readConfig(fileRule{path: target.path, maximum: maximumNodeBinaryBytes, private: target.mode == 0600}, account{})
		if err != nil || contentDigest(current) != item.Before && contentDigest(current) != item.After {
			return report, errors.New("节点修复目标已被其他操作改变，未覆盖")
		}
	}
	var record nodeInstallation
	if decodePreparation(after["installation.json"], &record) != nil {
		return report, errors.New("修复后的安装记录无效")
	}
	request, digest, err := parseNodeInstallRequest(record.Request)
	if err != nil || digest != record.RequestSHA256 {
		return report, errors.New("修复后的安装请求无效")
	}
	currentRecord := record
	currentRecord.Files = make(map[string]string, len(record.Files))
	for name, digest := range record.Files {
		currentRecord.Files[name] = digest
	}
	for _, target := range nodeRepairTargets() {
		if _, present := currentRecord.Files[target.name]; !present {
			continue
		}
		content, err := installer.fs.readConfig(fileRule{path: target.path, maximum: 256 << 10, private: true}, account{})
		if err != nil {
			return report, err
		}
		currentRecord.Files[target.name] = contentDigest(content)
	}
	currentImageBytes, err := installer.fs.readConfig(fileRule{path: deploy.NodeDirectory + "/computer-image.json", maximum: 64 << 10, private: true}, account{})
	if err != nil {
		return report, err
	}
	currentImage, err := dockernode.ParseImageManifest(currentImageBytes)
	if err != nil {
		return report, err
	}
	for name := range currentRecord.Files {
		if strings.HasSuffix(name, ".tar") {
			delete(currentRecord.Files, name)
		}
	}
	if currentImage.ArchiveFile != "" {
		currentRecord.Files[filepath.Base(currentImage.ArchiveFile)] = currentImage.ArchiveSHA256
	}
	if _, err := installer.verifyInstallation(ctx, currentRecord); err != nil {
		return report, err
	}
	if installer.checkListen != nil {
		if err := installer.checkListen(ctx, request.Control.Listen); err != nil {
			return report, err
		}
	}
	for _, target := range nodeRepairTargets() {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := (serverInstaller{fs: installer.fs}).replaceServerFile(target.path, after[target.name], target.mode); err != nil {
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
	backup := nodeRepairDirectory + "-" + ids.New()
	if err := publishPath(installer.fs.resolve(nodeRepairDirectory), installer.fs.resolve(backup)); err != nil {
		return report, errors.New("无法归档节点修复备份")
	}
	if err := syncDirectory(installer.fs.resolve("/etc/tombot")); err != nil {
		return report, err
	}
	installer.logger.Info("节点修复已保存备份，节点身份保持不变", "node_id", report.NodeID, "image_id", report.ImageID, "backup", backup)
	for _, arguments := range [][]string{{"daemon-reload"}, {"enable", "--now", nodeServiceName}, {"is-active", "--quiet", nodeServiceName}} {
		if err := ctx.Err(); err != nil {
			return NodeInstallationReport{}, err
		}
		if err := installer.systemctl(ctx, arguments...); err != nil {
			return NodeInstallationReport{}, err
		}
	}
	report.State = "control_started"
	return report, nil
}

func (installer nodeInstaller) prepareNodeRepair(ctx context.Context, program []byte, options NodeRepairOptions) (result error) {
	encoded, err := installer.fs.readConfig(fileRule{path: deploy.NodeDirectory + "/installation.json", maximum: 256 << 10, private: true}, account{})
	if err != nil {
		return err
	}
	var record nodeInstallation
	if decodePreparation(encoded, &record) != nil || record.Version != 2 {
		return errors.New("节点修复需要有效安装记录，未接管手工部署")
	}
	request, digest, err := parseNodeInstallRequest(record.Request)
	if err != nil || digest != record.RequestSHA256 {
		return errors.New("原节点安装请求已变化，未覆盖")
	}
	if _, err := installer.verifyInstallation(ctx, record); err != nil {
		return err
	}
	oldProgram, err := installer.fs.readConfig(fileRule{path: nodeBinaryPath, maximum: maximumNodeBinaryBytes}, account{})
	if err != nil || contentDigest(oldProgram) != record.BinarySHA256 {
		return errors.New("已安装节点程序与记录不一致，未覆盖")
	}
	service, err := installer.fs.readConfig(fileRule{path: nodeServicePath, maximum: 64 << 10}, account{})
	if err != nil || !bytes.Equal(service, []byte(deploy.NodeService())) {
		return errors.New("节点服务单元不属于安装器默认配置，未覆盖")
	}
	if options.Listen != "" {
		address, err := nodeapi.ParseListenAddress(options.Listen, true)
		if err != nil || address.Port() == 0 {
			return errors.New("修复监听地址无效")
		}
		request.Control.Listen, request.Control.AllowPublic = options.Listen, !address.Addr().IsPrivate()
	}
	if options.URL != "" {
		if request.Server == nil || nodeapi.ValidateURL(options.URL) != nil {
			return errors.New("修复节点 HTTPS 地址无效")
		}
		request.Server.NodeURL = options.URL
	}
	if request.Server != nil && sameNodeOrigin(request.Server.URL, request.Server.NodeURL) {
		return errors.New("节点和服务端地址相同，请同时用 --listen 与 --url 指定独立节点端口")
	}
	if installer.checkListen != nil {
		if err := installer.checkListen(ctx, request.Control.Listen); err != nil {
			return err
		}
	}
	if options.ImagePackage != "" {
		if err := installer.prepareImageUpgrade(ctx, &request, &record, options.ImagePackage); err != nil {
			return err
		}
	}
	request.Docker, err = json.Marshal(dockerbootstrap.DefaultManifest())
	if err != nil {
		return err
	}
	_, files, err := renderNodeInstallation(request, installer.platform)
	if err != nil {
		return err
	}
	after := map[string][]byte{"program": program, "service": []byte(deploy.NodeService())}
	for _, file := range files {
		after[file.name] = file.content
		record.Files[file.name] = contentDigest(file.content)
	}
	record.Request, err = json.Marshal(request)
	if err != nil {
		return err
	}
	_, record.RequestSHA256, err = parseNodeInstallRequest(record.Request)
	if err != nil {
		return err
	}
	record.BinarySHA256 = contentDigest(program)
	after["installation.json"], err = json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(installer.fs.resolve("/etc/tombot"), ".node-repair-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			result = errors.Join(result, os.RemoveAll(stage))
		}
	}()
	for _, directory := range []string{"before", "after"} {
		if err := os.Mkdir(filepath.Join(stage, directory), 0700); err != nil {
			return err
		}
	}
	journal := nodeRepair{Version: 1, Options: options, Program: contentDigest(program)}
	for _, target := range nodeRepairTargets() {
		before, err := installer.fs.readConfig(fileRule{path: target.path, maximum: maximumNodeBinaryBytes, private: target.mode == 0600}, account{})
		if err != nil {
			return err
		}
		if target.name == "program" && !bytes.Equal(before, oldProgram) {
			return errors.New("备份期间节点程序发生变化")
		}
		for _, revision := range []struct {
			directory string
			content   []byte
		}{{"before", before}, {"after", after[target.name]}} {
			if err := writeConfigurationFile(filepath.Join(stage, revision.directory, target.name), revision.content, 0600); err != nil {
				return err
			}
		}
		journal.Files = append(journal.Files, nodeRepairFile{Name: target.name, Before: contentDigest(before), After: contentDigest(after[target.name])})
	}
	plan, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if err := writeConfigurationFile(filepath.Join(stage, "plan.json"), plan, 0600); err != nil {
		return err
	}
	for _, directory := range []string{"before", "after", ""} {
		if err := syncDirectory(filepath.Join(stage, directory)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishPath(stage, installer.fs.resolve(nodeRepairDirectory)); err != nil {
		return err
	}
	published = true
	return syncDirectory(installer.fs.resolve("/etc/tombot"))
}

// prepareImageUpgrade 先发布按摘要命名的不可变归档；配置事务始终可恢复，旧归档留作回滚。
func (installer nodeInstaller) prepareImageUpgrade(ctx context.Context, request *nodeInstallRequest, record *nodeInstallation, directory string) (failure error) {
	if !validPath(directory) || request.Server == nil {
		return errors.New("镜像升级需要绝对安装包目录和已登记节点")
	}
	encoded, err := installer.fs.readInstallationSource(filepath.Join(directory, "node-release.json"), 64<<10, installer.caller)
	if err != nil {
		return err
	}
	var release nodeRelease
	if decodePreparation(encoded, &release) != nil || release.Version != 1 || release.Platform != installer.platform || computer.ValidateImageReference(release.ImageReference) != nil || len(release.ArchiveSHA256) != 64 || strings.Trim(release.ArchiveSHA256, "0123456789abcdef") != "" {
		return errors.New("离线升级清单无效")
	}
	if _, err := dockerbootstrap.ParseManifest(release.Docker); err != nil {
		return err
	}
	archive := filepath.Join(deploy.NodeDirectory, "computer-image-"+release.ArchiveSHA256+".tar")
	if _, err = os.Lstat(installer.fs.resolve(archive)); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(installer.fs.resolve(deploy.NodeDirectory), ".image-upgrade-")
		if err != nil {
			return err
		}
		defer func() { failure = errors.Join(failure, os.RemoveAll(stage)) }()
		source := filepath.Join(directory, "computer-image.tar")
		staged := filepath.Join(stage, "image.tar")
		if err = installer.fs.copyInstallationArchive(ctx, source, staged, release.ArchiveSHA256, installer.caller); err != nil {
			return err
		}
		if err = publishPath(staged, installer.fs.resolve(archive)); err != nil {
			return err
		}
		if err = syncDirectory(installer.fs.resolve(deploy.NodeDirectory)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err = installer.fs.verifyInstallationArchive(ctx, archive, release.ArchiveSHA256); err != nil {
		return err
	}
	if request.Image.Reference != release.ImageReference {
		request.PreviousImageID, request.ImageID = request.ImageID, ids.New()
	}
	delete(record.Files, filepath.Base(request.Image.ArchiveFile))
	request.Image.Reference, request.Image.ArchiveFile, request.Image.ArchiveSHA256 = release.ImageReference, archive, release.ArchiveSHA256
	record.Files[filepath.Base(archive)] = release.ArchiveSHA256
	return nil
}
