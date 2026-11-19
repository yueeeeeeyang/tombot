package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io.yueyang/tom/internal/noderelease"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/serverapp"
)

const (
	serverDirectory       = "/etc/tombot/server"
	serverConfigPath      = "/etc/tombot/server.json"
	serverBinaryPath      = "/usr/local/bin/tomd"
	serverServicePath     = "/etc/systemd/system/tomd.service"
	serverRecordPath      = serverDirectory + "/installation.json"
	serverUpgradePath     = serverDirectory + "/upgrade.json"
	serverBackupDirectory = "/var/lib/tombot-server/backups"
)

// ServerInstallOptions 只接受公开地址和文件引用；不接受命令行中的密码或密钥。
type ServerInstallOptions struct {
	URL         string
	Config      string
	Certificate string
	PrivateKey  string
	RenewHTTPS  bool
}

// ServerReport 描述实际检查结果；失败时仍返回可供定位的状态，不包含凭据。
type ServerReport struct {
	URL                string `json:"url,omitempty"`
	Service            string `json:"service"`
	Enabled            bool   `json:"enabled"`
	Healthy            bool   `json:"healthy"`
	SelfSigned         bool   `json:"self_signed"`
	CertificateExpires string `json:"certificate_expires,omitempty"`
	CertificateFile    string `json:"certificate_file,omitempty"`
	CertificateSHA256  string `json:"certificate_sha256,omitempty"`
	Backup             string `json:"backup,omitempty"`
}

type serverInstallation struct {
	Version         int             `json:"version"`
	ID              string          `json:"id"`
	UID             uint32          `json:"uid"`
	GID             uint32          `json:"gid"`
	ManagedDatabase bool            `json:"managed_database"`
	InitialConfig   json.RawMessage `json:"initial_config"`
	BinarySHA256    string          `json:"binary_sha256"`
	ServiceSHA256   string          `json:"service_sha256"`
}

type serverInstaller struct {
	fs              filesystem
	caller          uint32
	platform        string
	logger          *slog.Logger
	run             serverRunner
	account         func(context.Context, bool) (account, error)
	prepareDatabase func(context.Context, serverInstallation) error
	checkDatabase   func(context.Context, serverapp.Config) error
	backupDatabase  func(context.Context, serverapp.Config, string) error
	health          func(context.Context, serverapp.Config) error
	confirmURL      func(context.Context, string) (string, error)
}

// ManageServer 在 Linux systemd 宿主执行安装、升级或只读检查。
// install 可重复恢复；upgrade 使用当前执行的程序，失败保留备份及阶段记录。
func ManageServer(ctx context.Context, action string, options ServerInstallOptions, executable string, caller uint32, logger *slog.Logger, confirmURL func(context.Context, string) (string, error)) (ServerReport, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || logger == nil {
		return ServerReport{}, errors.New("服务端安装、升级和完整状态检查需要在 Linux 以 sudo 执行")
	}
	installer := serverInstaller{fs: filesystem{base: "/", owner: 0}, caller: caller, platform: "linux/" + runtime.GOARCH, logger: logger, run: runServerCommand, account: ensureServerAccount, confirmURL: confirmURL}
	installer.prepareDatabase = installer.ensureServerDatabase
	installer.checkDatabase = installer.checkServerDatabase
	installer.backupDatabase = installer.dumpServerDatabase
	installer.health = installer.checkServerHealth
	if err := installer.fs.directory("/run/systemd/system", account{}, false, false, false); err != nil {
		return ServerReport{}, errors.New("服务端部署需要 systemd 系统服务环境")
	}
	return installer.manage(ctx, action, options, executable)
}

func (installer serverInstaller) manage(ctx context.Context, action string, options ServerInstallOptions, executable string) (report ServerReport, result error) {
	if action != "install" && action != "upgrade" && action != "status" {
		return report, errors.New("未知服务端部署命令")
	}
	unlock, err := installer.fs.lockInstallation("/run/tombot-server-install.lock")
	if err != nil {
		return report, err
	}
	defer func() { result = errors.Join(result, unlock()) }()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if action == "status" {
		return installer.status(ctx)
	}
	program, err := installer.fs.readInstallationSource(executable, maximumNodeBinaryBytes, installer.caller)
	if err != nil {
		return report, err
	}
	if err := noderelease.ValidateExecutable(program, installer.platform); err != nil {
		return report, errors.New("服务端程序不是与宿主匹配的 Linux 可执行文件")
	}
	if action == "upgrade" {
		return installer.upgrade(ctx, program, options.RenewHTTPS)
	}
	return installer.installServer(ctx, options, program)
}

func (installer serverInstaller) loadServerRecord() (serverInstallation, bool, error) {
	var record serverInstallation
	if err := installer.fs.directory("/etc/tombot", account{}, false, false, true); err != nil {
		return record, false, err
	}
	if _, err := os.Lstat(installer.fs.resolve(serverDirectory)); errors.Is(err, os.ErrNotExist) {
		return record, false, nil
	} else if err != nil {
		return record, false, errors.New("无法检查服务端安装目录")
	}
	encoded, err := installer.fs.readServerFile(fileRule{path: serverRecordPath, maximum: 256 << 10, private: true}, account{})
	if err != nil || decodeServerJSON(encoded, &record) != nil || record.Version != 1 || len(record.ID) != 32 || len(record.BinarySHA256) != 64 || len(record.ServiceSHA256) != 64 {
		return record, false, errors.New("已有服务端目录缺少有效安装记录，未接管或覆盖")
	}
	if _, err := serverapp.ParseConfig(record.InitialConfig); err != nil {
		return record, false, err
	}
	record.InitialConfig, err = formatServerConfig(record.InitialConfig)
	if err != nil {
		return record, false, err
	}
	return record, true, nil
}

func formatServerConfig(encoded []byte) ([]byte, error) {
	var formatted bytes.Buffer
	if json.Indent(&formatted, bytes.TrimSpace(encoded), "", "  ") != nil {
		return nil, errors.New("服务配置格式无效")
	}
	return formatted.Bytes(), nil
}

// decodeServerJSON 保留服务配置允许的空列表；具体配置仍交给 serverapp 校验。
func decodeServerJSON(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("服务安装记录格式无效")
	}
	return nil
}

func (installer serverInstaller) installServer(ctx context.Context, options ServerInstallOptions, program []byte) (ServerReport, error) {
	record, exists, err := installer.loadServerRecord()
	if err != nil {
		return ServerReport{}, err
	}
	if _, err := os.Lstat(installer.fs.resolve(serverUpgradePath)); !errors.Is(err, os.ErrNotExist) {
		return ServerReport{}, errors.New("存在未完成的升级，请使用同一新版程序执行 tomd upgrade")
	}
	service := []byte(deploy.ServerService())
	if exists {
		if record.BinarySHA256 != contentDigest(program) || record.ServiceSHA256 != contentDigest(service) {
			return ServerReport{}, errors.New("已安装其他版本，请使用 tomd upgrade")
		}
		if options != (ServerInstallOptions{}) {
			return ServerReport{}, errors.New("续装使用原配置，请直接运行 tomd install，不再提供首次参数")
		}
	} else {
		for _, path := range []string{serverBinaryPath, serverServicePath, serverConfigPath} {
			if _, err := os.Lstat(installer.fs.resolve(path)); !errors.Is(err, os.ErrNotExist) {
				return ServerReport{}, fmt.Errorf("已有部署文件 %s，未覆盖；请在独立新环境安装", path)
			}
		}
		api, err := installer.account(ctx, true)
		if err != nil {
			return ServerReport{}, err
		}
		var files []configurationFile
		record, files, err = installer.prepareServerFiles(ctx, options, api, program)
		defer func() {
			for _, file := range files {
				clear(file.content)
			}
		}()
		if err != nil {
			return ServerReport{}, err
		}
		record.InitialConfig, err = formatServerConfig(record.InitialConfig)
		if err != nil {
			return ServerReport{}, err
		}
		if err := installer.publishServerFiles(ctx, record, files); err != nil {
			return ServerReport{}, err
		}
	}
	api, err := installer.account(ctx, false)
	if err != nil || api.uid != record.UID || api.gid != record.GID {
		return ServerReport{}, errors.New("服务账户与安装记录不一致，未更改文件归属")
	}
	if err := installer.fs.installExactFile(ctx, serverConfigPath, record.InitialConfig, 0644); err != nil {
		return ServerReport{}, err
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		return ServerReport{}, err
	}
	if record.ManagedDatabase {
		installer.logger.Info("正在准备本机 PostgreSQL", "installation_id", record.ID)
		if err := installer.prepareDatabase(ctx, record); err != nil {
			return ServerReport{}, err
		}
	}
	if err := installer.fs.ensureInstallDirectory("/opt/tombot", 0755); err != nil {
		return ServerReport{}, err
	}
	if err := installer.fs.installExactFile(ctx, serverBinaryPath, program, 0755); err != nil {
		return ServerReport{}, err
	}
	if err := installer.fs.installExactFile(ctx, serverServicePath, service, 0644); err != nil {
		return ServerReport{}, err
	}
	if err := installer.startServer(ctx); err != nil {
		return ServerReport{}, err
	}
	if err := installer.waitServer(ctx, configuration); err != nil {
		return ServerReport{}, err
	}
	return installer.status(ctx)
}

func (installer serverInstaller) waitServer(ctx context.Context, configuration serverapp.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for attempt := 0; attempt < 30; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.New("等待服务健康检查取消或超时，请查看 journalctl -u tomd 后重试")
		}
		if err := installer.health(ctx, configuration); err == nil {
			return nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return errors.New("服务健康检查未通过；保留配置和身份，请查看 journalctl -u tomd 后重试")
}

func (installer serverInstaller) publishServerFiles(ctx context.Context, record serverInstallation, files []configurationFile) (result error) {
	if err := installer.fs.ensureInstallDirectory("/etc/tombot", 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(installer.fs.resolve("/etc/tombot"), ".server-install-")
	if err != nil {
		return errors.New("无法创建服务端配置暂存目录")
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			result = errors.Join(result, errors.New("清理服务端暂存目录失败"))
		}
	}()
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(stage, file.name)
		if err := writeConfigurationFile(path, file.content, file.mode); err != nil {
			return err
		}
		if err := os.Chown(path, int(record.UID), int(record.GID)); err != nil {
			return errors.New("设置服务端文件归属失败")
		}
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return errors.New("编码服务端安装记录失败")
	}
	if err := writeConfigurationFile(filepath.Join(stage, "installation.json"), encoded, 0600); err != nil {
		return err
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return errors.New("设置服务端目录权限失败")
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishPath(stage, installer.fs.resolve(serverDirectory)); err != nil {
		return errors.New("服务端配置发布失败，未覆盖现有目录")
	}
	return syncDirectory(installer.fs.resolve("/etc/tombot"))
}
