package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/serverapp"
)

type serverUpgrade struct {
	Version        int    `json:"version"`
	Candidate      string `json:"candidate_sha256"`
	Previous       string `json:"previous_sha256"`
	Backup         string `json:"backup"`
	Phase          string `json:"phase"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	RenewHTTPS     bool   `json:"renew_https,omitempty"`
}

type serverBackupFile struct {
	Source string `json:"source"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
}

func (installer serverInstaller) upgrade(ctx context.Context, program []byte, renewHTTPS bool) (report ServerReport, result error) {
	record, exists, err := installer.loadServerRecord()
	if err != nil {
		return report, err
	}
	if !exists {
		return report, errors.New("尚未安装，请先执行 tomd install")
	}
	api, err := installer.account(ctx, false)
	if err != nil || api.uid != record.UID || api.gid != record.GID {
		return report, errors.New("服务身份与原安装不一致")
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		return report, err
	}
	if err := installer.checkDatabase(ctx, configuration); err != nil {
		return report, err
	}
	journal, pending, err := installer.loadServerUpgrade()
	if err != nil {
		return report, err
	}
	digest := contentDigest(program)
	if pending && journal.Candidate != digest {
		return report, errors.New("升级尚未完成，请使用同一候选程序重试，未覆盖原备份")
	}
	if pending && renewHTTPS && !journal.RenewHTTPS {
		return report, errors.New("已有升级未计划重新签发 HTTPS 证书，请先完成原升级")
	}
	if pending {
		renewHTTPS = journal.RenewHTTPS
	}
	if err := installer.checkHTTPSUpgrade(configuration, api, renewHTTPS); err != nil {
		return report, err
	}
	if !pending && record.BinarySHA256 == digest && !renewHTTPS {
		return installer.status(ctx)
	}
	if _, err := installer.run(ctx, serverCommand{program: "/usr/bin/pg_dump", arguments: []string{"--version"}, timeout: 15 * time.Second}); err != nil {
		return report, errors.New("缺少可用 pg_dump，请安装与数据库匹配的 PostgreSQL 客户端后升级")
	}
	if !pending {
		for _, target := range []struct{ path, digest string }{{serverBinaryPath, record.BinarySHA256}, {serverServicePath, record.ServiceSHA256}} {
			content, err := installer.fs.readServerFile(fileRule{path: target.path, maximum: maximumNodeBinaryBytes}, account{})
			if err != nil || contentDigest(content) != target.digest {
				return report, errors.New("原程序或服务单元已被修改，未开始升级")
			}
		}
		if err := installer.fs.ensureInstallDirectory(serverBackupDirectory, 0700); err != nil {
			return report, err
		}
		directory, err := os.MkdirTemp(installer.fs.resolve(serverBackupDirectory), "upgrade-")
		if err != nil {
			return report, errors.New("无法创建升级备份目录")
		}
		journal = serverUpgrade{Version: 1, Candidate: digest, Previous: record.BinarySHA256, Backup: serverBackupDirectory + "/" + filepath.Base(directory), Phase: "preparing", RenewHTTPS: renewHTTPS}
		if err := installer.saveServerJSON(serverUpgradePath, journal); err != nil {
			return report, err
		}
	}
	report.Backup, report.URL = journal.Backup, configuration.AdminOrigin+"/admin/"
	defer func() {
		if result != nil {
			result = fmt.Errorf("升级未完成，备份保留在 %s；使用同一新版程序重试：%w", journal.Backup, result)
		}
	}()
	// 机器重启可能重新启动旧服务；恢复任何阶段都先停止它，不能让 enable --now 误把旧进程判为新版。
	if err := installer.stopServer(ctx); err != nil {
		return report, err
	}
	if journal.Phase == "preparing" {
		installer.logger.Info("正在停止服务并备份数据库与配置", "installation_id", record.ID)
		// 不覆盖可能完整的旧备份；每次备份尝试使用新目录，只有完整刷盘后才记录发布阶段。
		directory, err := os.MkdirTemp(installer.fs.resolve(journal.Backup), "snapshot-")
		if err != nil {
			return report, errors.New("无法创建升级快照目录")
		}
		snapshot := journal.Backup + "/" + filepath.Base(directory)
		if err := installer.backupServer(ctx, configuration, snapshot, api); err != nil {
			return report, err
		}
		manifest, err := installer.fs.readServerFile(fileRule{path: snapshot + "/files.json", maximum: 1 << 20, private: true}, account{})
		if err != nil {
			return report, err
		}
		journal.SnapshotSHA256 = contentDigest(manifest)
		journal.Backup, journal.Phase = snapshot, "backed_up"
		if err := installer.saveServerJSON(serverUpgradePath, journal); err != nil {
			return report, err
		}
		report.Backup = snapshot
	}
	if err := installer.verifyServerBackup(journal); err != nil {
		return report, err
	}
	if journal.RenewHTTPS {
		configuration, err = installer.renewServerHTTPS(ctx, configuration, api, journal)
		if err != nil {
			return report, err
		}
	}
	if journal.Phase == "backed_up" {
		// 崩溃可能发生在原子替换后、阶段落盘前；只接受原版或本次候选版。
		old, err := installer.fs.readServerFile(fileRule{path: serverBinaryPath, maximum: maximumNodeBinaryBytes}, account{})
		if err != nil || contentDigest(old) != journal.Previous && contentDigest(old) != journal.Candidate {
			return report, errors.New("程序不属于本次升级，未覆盖")
		}
		if err := installer.replaceServerFile(serverBinaryPath, program, 0755); err != nil {
			return report, err
		}
		if err := installer.replaceServerFile(serverServicePath, []byte(deploy.ServerService()), 0644); err != nil {
			return report, err
		}
		journal.Phase = "replaced"
		if err := installer.saveServerJSON(serverUpgradePath, journal); err != nil {
			return report, err
		}
	}
	for _, target := range []struct{ path, digest string }{{serverBinaryPath, digest}, {serverServicePath, contentDigest([]byte(deploy.ServerService()))}} {
		content, err := installer.fs.readServerFile(fileRule{path: target.path, maximum: maximumNodeBinaryBytes}, account{})
		if err != nil || contentDigest(content) != target.digest {
			return report, errors.New("升级发布文件与候选版本不一致，未启动")
		}
	}
	if err := installer.startServer(ctx); err != nil {
		return report, err
	}
	if err := installer.waitServer(ctx, configuration); err != nil {
		return report, err
	}
	record.BinarySHA256, record.ServiceSHA256 = digest, contentDigest([]byte(deploy.ServerService()))
	if journal.RenewHTTPS {
		record.InitialConfig, err = updateServerTLSPaths(record.InitialConfig, configuration.TLSCertificateFile, configuration.TLSPrivateKeyFile)
		if err != nil {
			return report, err
		}
	}
	if err := installer.saveServerJSON(serverRecordPath, record); err != nil {
		return report, err
	}
	if err := os.Remove(installer.fs.resolve(serverUpgradePath)); err != nil {
		return report, errors.New("删除已完成的升级记录失败")
	}
	if err := syncDirectory(installer.fs.resolve(serverDirectory)); err != nil {
		return report, err
	}
	checked, err := installer.status(ctx)
	checked.Backup = journal.Backup
	return checked, err
}

func (installer serverInstaller) stopServer(ctx context.Context) error {
	before, err := installer.runSystemctl(ctx, 15*time.Second, "show", "tomd.service", "--property=ActiveState,Result,MainPID")
	if err != nil {
		return err
	}
	previous := parseServiceProperties(before)
	if _, err := installer.runSystemctl(ctx, 7*time.Minute, "stop", "tomd.service"); err != nil {
		return err
	}
	properties, err := installer.runSystemctl(ctx, 15*time.Second, "show", "tomd.service", "--property=ActiveState,Result,MainPID")
	if err != nil {
		return err
	}
	state := parseServiceProperties(properties)
	wasStopped := previous["ActiveState"] == "inactive" || previous["ActiveState"] == "failed"
	isStopped := (state["ActiveState"] == "inactive" || state["ActiveState"] == "failed") && state["MainPID"] == "0"
	if !isStopped || state["Result"] != "success" && !wasStopped {
		return errors.New("服务未正常退出，未替换程序；请检查退出日志")
	}
	if state["Result"] != "success" {
		installer.logger.Warn("原服务已经失败并停止，将保留备份后重试启动", "service", "tomd.service")
	}
	return nil
}

func (installer serverInstaller) loadServerUpgrade() (serverUpgrade, bool, error) {
	var journal serverUpgrade
	if _, err := os.Lstat(installer.fs.resolve(serverUpgradePath)); errors.Is(err, os.ErrNotExist) {
		return journal, false, nil
	} else if err != nil {
		return journal, false, errors.New("无法检查升级记录")
	}
	encoded, err := installer.fs.readServerFile(fileRule{path: serverUpgradePath, maximum: 16 << 10, private: true}, account{})
	if err != nil || decodePreparation(encoded, &journal) != nil || journal.Version != 1 || len(journal.Candidate) != 64 || len(journal.Previous) != 64 || !validPath(journal.Backup) || !strings.HasPrefix(journal.Backup, serverBackupDirectory+"/") {
		return journal, false, errors.New("升级记录无效，未修改安装")
	}
	if journal.Phase != "preparing" && journal.Phase != "backed_up" && journal.Phase != "replaced" {
		return journal, false, errors.New("升级阶段未知")
	}
	if journal.Phase != "preparing" && len(journal.SnapshotSHA256) != 64 {
		return journal, false, errors.New("升级记录缺少完整备份摘要")
	}
	if err := installer.fs.directory(journal.Backup, account{}, false, false, false); err != nil {
		return journal, false, err
	}
	return journal, true, nil
}

func (installer serverInstaller) saveServerJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("编码服务部署记录失败")
	}
	return installer.replaceServerFile(path, encoded, 0600)
}

func (installer serverInstaller) replaceServerFile(path string, content []byte, mode os.FileMode) (result error) {
	parent := filepath.Dir(path)
	if err := installer.fs.directory(parent, account{}, false, false, false); err != nil {
		return err
	}
	if _, err := os.Lstat(installer.fs.resolve(path)); err == nil {
		if _, err := installer.fs.inspect(fileRule{path: path, maximum: maximumNodeBinaryBytes, private: mode == 0600}, account{}); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("升级目标不可检查")
	}
	stage, err := os.MkdirTemp(installer.fs.resolve(parent), ".server-replace-")
	if err != nil {
		return errors.New("创建升级暂存目录失败")
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			result = errors.Join(result, errors.New("清理升级暂存目录失败"))
		}
	}()
	source := filepath.Join(stage, "content")
	if err := writeConfigurationFile(source, content, mode); err != nil {
		return err
	}
	if err := os.Rename(source, installer.fs.resolve(path)); err != nil {
		return errors.New("原子替换服务文件失败")
	}
	return syncDirectory(installer.fs.resolve(parent))
}

func (installer serverInstaller) backupServer(ctx context.Context, configuration serverapp.Config, destination string, api account) error {
	files := []serverBackupFile{}
	rules := append(serverReferences(configuration), fileRule{path: serverConfigPath, maximum: 128 << 10, api: true}, fileRule{path: serverBinaryPath, maximum: maximumNodeBinaryBytes}, fileRule{path: serverServicePath, maximum: 64 << 10}, fileRule{path: serverRecordPath, maximum: 256 << 10, private: true})
	seen := map[string]bool{}
	for _, rule := range rules {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[rule.path] {
			continue
		}
		seen[rule.path] = true
		info, err := installer.fs.inspect(rule, api)
		if err != nil {
			return err
		}
		owner, ok := infoSysOwner(info)
		if !ok {
			return errors.New("备份文件身份无法读取")
		}
		content, err := installer.fs.readInspectedFile(rule, info)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("file-%02d", len(files))
		files = append(files, serverBackupFile{rule.path, name, contentDigest(content), uint32(info.Mode().Perm()), owner.Uid, owner.Gid})
		err = writeConfigurationFile(installer.fs.resolve(filepath.Join(destination, name)), content, 0600)
		clear(content)
		if err != nil {
			return err
		}
	}
	if err := installer.backupDatabase(ctx, configuration, destination); err != nil {
		return err
	}
	digest, err := installer.hashBackupFile(destination + "/database.dump")
	if err != nil {
		return err
	}
	files = append(files, serverBackupFile{Source: "postgresql", File: "database.dump", SHA256: digest, Mode: 0600, UID: installer.fs.owner})
	manifest, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return errors.New("编码升级备份清单失败")
	}
	if err := writeConfigurationFile(installer.fs.resolve(filepath.Join(destination, "files.json")), manifest, 0600); err != nil {
		return err
	}
	return syncDirectory(installer.fs.resolve(destination))
}

func (installer serverInstaller) verifyServerBackup(journal serverUpgrade) error {
	encoded, err := installer.fs.readServerFile(fileRule{path: journal.Backup + "/files.json", maximum: 1 << 20, private: true}, account{})
	if err != nil || contentDigest(encoded) != journal.SnapshotSHA256 {
		return errors.New("升级备份清单缺失或已变化，未替换程序")
	}
	var files []serverBackupFile
	if json.Unmarshal(encoded, &files) != nil || len(files) == 0 || len(files) > 128 {
		return errors.New("升级备份清单无效")
	}
	hasDatabase := false
	for _, file := range files {
		if filepath.Base(file.File) != file.File || file.File == "." || file.File == ".." {
			return errors.New("升级备份文件名无效")
		}
		digest, err := installer.hashBackupFile(journal.Backup + "/" + file.File)
		if err != nil || digest != file.SHA256 {
			return errors.New("升级备份文件缺失或损坏，未替换程序")
		}
		hasDatabase = hasDatabase || file.File == "database.dump"
	}
	if !hasDatabase {
		return errors.New("升级备份缺少数据库")
	}
	return nil
}

func (installer serverInstaller) hashBackupFile(path string) (digest string, result error) {
	info, err := installer.fs.inspect(fileRule{path: path, maximum: 1 << 50, private: true}, account{})
	if err != nil {
		return "", err
	}
	file, err := os.Open(installer.fs.resolve(path))
	if err != nil {
		return "", errors.New("读取备份文件失败")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("备份文件读取前已改变")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, info.Size()+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || count != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return "", errors.New("备份文件读取失败或内容变化")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
