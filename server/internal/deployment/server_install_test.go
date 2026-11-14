package deployment

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/serverapp"
)

type serverTestHost struct {
	active       bool
	enabled      bool
	failDatabase bool
	failStart    bool
	failed       bool
	backups      int
	calls        []string
}

func serverInstallFixture(t *testing.T) (serverInstaller, *serverTestHost) {
	t.Helper()
	fs := filesystem{base: t.TempDir(), owner: uint32(os.Geteuid())}
	for _, path := range []string{"/run", "/source", "/etc"} {
		if err := os.Mkdir(fs.resolve(path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	program := make([]byte, 65)
	copy(program, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(program[16:18], 2)
	binary.LittleEndian.PutUint16(program[18:20], 62)
	writeInstallFixture(t, fs, "/source/tomd", program, 0755)
	host := &serverTestHost{}
	installer := serverInstaller{fs: fs, platform: "linux/amd64", caller: fs.owner, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), account: func(context.Context, bool) (account, error) {
		return account{uid: fs.owner, gid: uint32(os.Getegid())}, nil
	}}
	installer.run = func(_ context.Context, command serverCommand) ([]byte, error) {
		call := strings.Join(command.arguments, " ")
		host.calls = append(host.calls, call)
		if command.program != "/usr/bin/systemctl" {
			return nil, nil
		}
		switch command.arguments[0] {
		case "reset-failed":
			if !host.active && !host.failed {
				return nil, errors.New("单元尚未加载或已被 systemd 回收")
			}
			host.failed = false
		case "stop":
			host.active = false
		case "enable":
			if host.failStart {
				host.failed = true
				return nil, errors.New("模拟启动失败")
			}
			if host.failed {
				return nil, errors.New("失败单元尚未解除启动限流")
			}
			host.active = true
			host.enabled = true
		case "show":
			state := "inactive"
			if host.active {
				state = "active"
			} else if host.failed {
				state = "failed"
			}
			enabled := "disabled"
			if host.enabled {
				enabled = "enabled"
			}
			return []byte("LoadState=loaded\nActiveState=" + state + "\nUnitFileState=" + enabled + "\nResult=success\nMainPID=0\n"), nil
		}
		return nil, nil
	}
	installer.prepareDatabase = func(context.Context, serverInstallation) error {
		if host.failDatabase {
			return errors.New("模拟数据库准备失败")
		}
		return nil
	}
	installer.checkDatabase = func(context.Context, serverapp.Config) error { return nil }
	installer.backupDatabase = func(_ context.Context, _ serverapp.Config, path string) error {
		host.backups++
		return writeConfigurationFile(fs.resolve(path+"/database.dump"), []byte("PGDMP测试备份"), 0600)
	}
	installer.health = func(context.Context, serverapp.Config) error {
		if !host.active {
			return errors.New("服务未启动")
		}
		return nil
	}
	return installer, host
}

func installServerFixture(t *testing.T, installer serverInstaller) ServerReport {
	t.Helper()
	report, err := installer.manage(t.Context(), "install", ServerInstallOptions{URL: "https://192.0.2.20:9443"}, "/source/tomd")
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func readServerFixture(t *testing.T, installer serverInstaller, path string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(installer.fs.resolve(path))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestServerInstallCreatesIdentityAndResumesWithoutRotating(t *testing.T) {
	installer, host := serverInstallFixture(t)
	host.failDatabase = true
	if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{URL: "https://192.0.2.20:9443"}, "/source/tomd"); err == nil {
		t.Fatal("数据库准备失败仍成功")
	}
	paths := []string{serverConfigPath, serverRecordPath, serverDirectory + "/identity.key", serverDirectory + "/csrf.key", serverDirectory + "/model.key", serverDirectory + "/tls.key", serverDirectory + "/migration.connection"}
	before := map[string][]byte{}
	for _, path := range paths {
		before[path] = readServerFixture(t, installer, path)
	}
	if bytes.Equal(before[serverDirectory+"/identity.key"], before[serverDirectory+"/csrf.key"]) {
		t.Fatal("不同用途复用了密钥")
	}
	host.failDatabase = false
	report, err := installer.manage(t.Context(), "install", ServerInstallOptions{}, "/source/tomd")
	if err != nil || !report.Healthy || !report.Enabled || !report.SelfSigned || report.URL != "https://192.0.2.20:9443/admin/" {
		t.Fatal(report, err)
	}
	if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{}, "/source/tomd"); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !bytes.Equal(before[path], readServerFixture(t, installer, path)) {
			t.Fatal("续装改变身份或配置", path)
		}
	}
	for _, name := range []string{"identity.key", "csrf.key", "model.key", "tls.key", "api.key", "migration.connection", "identity.connection", "control.connection", "tenant.connection", "dispatch.connection"} {
		info, err := os.Stat(installer.fs.resolve(serverDirectory + "/" + name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("私有文件权限错误", name, err)
		}
	}
	if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{URL: "https://192.0.2.21:9443"}, "/source/tomd"); err == nil {
		t.Fatal("续装接受了不同参数")
	}
}

func TestServerInstallRejectsConflictsLinksCancellationAndLock(t *testing.T) {
	for _, path := range []string{serverConfigPath, serverBinaryPath, serverServicePath} {
		t.Run(path, func(t *testing.T) {
			installer, _ := serverInstallFixture(t)
			writeInstallFixture(t, installer.fs, path, []byte("已有文件"), 0644)
			if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{URL: "https://192.0.2.20:9443"}, "/source/tomd"); err == nil {
				t.Fatal("覆盖了已有部署")
			}
			if string(readServerFixture(t, installer, path)) != "已有文件" {
				t.Fatal("冲突文件被修改")
			}
		})
	}
	installer, _ := serverInstallFixture(t)
	unlock, err := installer.fs.lockInstallation("/run/tombot-server-install.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{}, "/source/tomd"); err == nil {
		t.Fatal("并发安装未被拒绝")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := installer.manage(ctx, "install", ServerInstallOptions{}, "/source/tomd"); err == nil {
		t.Fatal("取消后继续安装")
	}
	if err := os.Symlink("/source/tomd", installer.fs.resolve("/source/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{}, "/source/link"); err == nil {
		t.Fatal("接受符号链接程序")
	}
}

func TestServerUpgradeBacksUpPreservesConfigAndResumes(t *testing.T) {
	installer, host := serverInstallFixture(t)
	installServerFixture(t, installer)
	configuration := readServerFixture(t, installer, serverConfigPath)
	configuration = bytes.Replace(configuration, []byte(`"maximum_nodes": 128`), []byte(`"maximum_nodes": 64`), 1)
	writeInstallFixture(t, installer.fs, serverConfigPath, configuration, 0644)
	key := readServerFixture(t, installer, serverDirectory+"/identity.key")
	old := readServerFixture(t, installer, serverBinaryPath)
	candidate := bytes.Clone(old)
	candidate[len(candidate)-1] = 1
	writeInstallFixture(t, installer.fs, "/source/new-tomd", candidate, 0755)
	host.failStart = true
	report, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd")
	if err == nil || report.Backup == "" || host.backups != 1 {
		t.Fatal("启动失败未保留备份", report, err)
	}
	if _, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/tomd"); err == nil {
		t.Fatal("中断升级接受其他候选程序")
	}
	host.failStart = false
	report, err = installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd")
	if err != nil || !report.Healthy || host.backups != 1 {
		t.Fatal("升级恢复错误", report, err, host.backups)
	}
	if !bytes.Equal(configuration, readServerFixture(t, installer, serverConfigPath)) || !bytes.Equal(key, readServerFixture(t, installer, serverDirectory+"/identity.key")) {
		t.Fatal("升级覆盖配置或密钥")
	}
	if !bytes.Equal(candidate, readServerFixture(t, installer, serverBinaryPath)) {
		t.Fatal("未发布候选程序")
	}
	if _, err := os.Stat(installer.fs.resolve(report.Backup + "/database.dump")); err != nil {
		t.Fatal("缺少数据库备份", err)
	}
	if _, err := installer.manage(t.Context(), "status", ServerInstallOptions{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd"); err != nil || host.backups != 1 {
		t.Fatal("重复升级没有保持幂等", err)
	}
}

func TestServerUpgradeStopsBeforeReplacementAndRejectsDatabaseFailure(t *testing.T) {
	installer, host := serverInstallFixture(t)
	installServerFixture(t, installer)
	candidate := readServerFixture(t, installer, serverBinaryPath)
	candidate[len(candidate)-1] = 2
	writeInstallFixture(t, installer.fs, "/source/new-tomd", candidate, 0755)
	installer.checkDatabase = func(context.Context, serverapp.Config) error { return errors.New("模拟降级拒绝") }
	if _, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd"); err == nil || !host.active || host.backups != 0 {
		t.Fatal("预检失败仍影响在线服务", err)
	}
	installer.checkDatabase = func(context.Context, serverapp.Config) error { return nil }
	installer.backupDatabase = func(context.Context, serverapp.Config, string) error { return errors.New("模拟备份失败") }
	old := readServerFixture(t, installer, serverBinaryPath)
	if _, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd"); err == nil {
		t.Fatal("备份失败仍升级成功")
	}
	if !bytes.Equal(old, readServerFixture(t, installer, serverBinaryPath)) {
		t.Fatal("备份失败替换了程序")
	}
	if host.active {
		t.Fatal("未停服务即备份")
	}
}

func TestServerUpgradeResumeRejectsDamagedBackupAndStopsRebootedService(t *testing.T) {
	for _, damage := range []bool{false, true} {
		t.Run(strconv.FormatBool(damage), func(t *testing.T) {
			installer, host := serverInstallFixture(t)
			installServerFixture(t, installer)
			candidate := readServerFixture(t, installer, serverBinaryPath)
			candidate[len(candidate)-1] = 3
			writeInstallFixture(t, installer.fs, "/source/new-tomd", candidate, 0755)
			host.failStart = true
			report, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd")
			if err == nil {
				t.Fatal("应保留中断的升级")
			}
			if damage {
				writeInstallFixture(t, installer.fs, report.Backup+"/database.dump", []byte("损坏备份"), 0600)
			}
			host.failStart = false
			host.active = true
			host.calls = nil
			_, err = installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd")
			if (err != nil) != damage {
				t.Fatal("损坏备份的恢复结果错误", err)
			}
			stopped := false
			for _, call := range host.calls {
				if call == "stop tomd.service" {
					stopped = true
				}
				if strings.HasPrefix(call, "enable") && !stopped {
					t.Fatal("恢复启动前没有停止旧服务")
				}
			}
			if !stopped {
				t.Fatal("恢复升级未停止重启后的服务")
			}
			if host.backups != 1 {
				t.Fatal("恢复覆盖了升级前备份")
			}
		})
	}
}

func TestServerStatusReportsServiceAndHealthFailure(t *testing.T) {
	installer, host := serverInstallFixture(t)
	if report, err := installer.manage(t.Context(), "status", ServerInstallOptions{}, ""); err == nil || report.Service != "未安装" {
		t.Fatal("未安装状态不准确", report, err)
	}
	installServerFixture(t, installer)
	host.active = false
	if report, err := installer.manage(t.Context(), "status", ServerInstallOptions{}, ""); err == nil || report.Healthy || report.Service != "inactive" {
		t.Fatal("停止状态被当作成功", report, err)
	}
	host.active = true
	host.enabled = false
	if report, err := installer.manage(t.Context(), "status", ServerInstallOptions{}, ""); err == nil || report.Enabled {
		t.Fatal("未检测开机启动", report, err)
	}
}

func TestServerURLAndCertificates(t *testing.T) {
	for _, value := range []string{"http://example.test:9443", "https://user@example.test:9443", "https://example.test/admin/", "https://example.test:443", "https://0.0.0.0:9443", "https://example.test:9443?x=1", "https://example.test:9443/"} {
		if _, err := NormalizeServerURL(value); err == nil {
			t.Fatal("接受非法来源", value)
		}
	}
	for _, value := range []string{"https://example.test", "https://192.0.2.1:9443", "https://[2001:db8::1]:9443"} {
		if _, err := NormalizeServerURL(value); err != nil {
			t.Fatal(value, err)
		}
	}
	for _, host := range []string{"example.test", "192.0.2.1", "2001:db8::1"} {
		certificate, key, err := createServerCertificate(host, false, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := validateServerCertificate(certificate, key, host, false)
		if err != nil || time.Until(leaf.NotAfter) < 364*24*time.Hour {
			t.Fatal("证书身份或期限错误", err)
		}
		if _, err := validateServerCertificate(certificate, key, "other.test", false); err == nil {
			t.Fatal("错误域名未拒绝")
		}
		if _, err := validateServerCertificate(certificate, key, "", true); err == nil {
			t.Fatal("服务器证书被当作客户端身份")
		}
	}
	certificate, key, err := createServerCertificate("example.test", false, time.Now().AddDate(-2, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateServerCertificate(certificate, key, "example.test", false); err == nil {
		t.Fatal("过期证书未拒绝")
	}
}

func TestServerCertificatePreservesIssuanceFailure(t *testing.T) {
	// 不可编码的年份触发真实标准库签发错误，验证诊断不会再丢失底层原因。
	certificate, key, err := createServerCertificate("example.test", false, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	var encodingError asn1.StructuralError
	if !errors.As(err, &encodingError) || !strings.Contains(err.Error(), "签发服务证书失败") {
		t.Fatal("签发错误缺少阶段或底层原因", err)
	}
	if len(certificate) != 0 || len(key) != 0 {
		t.Fatal("签发失败不应返回证书或私钥")
	}
}

func TestServerHealthChecksActualTLSAndPayload(t *testing.T) {
	installer, _ := serverInstallFixture(t)
	installServerFixture(t, installer)
	api, err := installer.account(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(readServerFixture(t, installer, configuration.TLSCertificateFile), readServerFixture(t, installer, configuration.TLSPrivateKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	var status atomic.Int64
	status.Store(http.StatusOK)
	var payload atomic.Value
	payload.Store(`{"status":"ready","scope":"api"}`)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health/ready" {
			t.Error("错误探针路径")
		}
		response.WriteHeader(int(status.Load()))
		if _, err := io.WriteString(response, payload.Load().(string)); err != nil {
			t.Error(err)
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	configuration.Address = server.Listener.Addr().String()
	if err := installer.checkServerHealth(t.Context(), configuration); err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusServiceUnavailable)
	if err := installer.checkServerHealth(t.Context(), configuration); err == nil {
		t.Fatal("非就绪状态被接受")
	}
	status.Store(http.StatusOK)
	payload.Store(`{"status":"ready","scope":"other"}`)
	if err := installer.checkServerHealth(t.Context(), configuration); err == nil {
		t.Fatal("非 API 服务被接受")
	}
	configuration.AdminOrigin = "https://wrong.test:9443"
	if err := installer.checkServerHealth(t.Context(), configuration); err == nil {
		t.Fatal("未核验证书域名")
	}
}

func TestServerDatabasePasswordsAreNotInCommands(t *testing.T) {
	installer, _ := serverInstallFixture(t)
	installServerFixture(t, installer)
	record, _, err := installer.loadServerRecord()
	if err != nil {
		t.Fatal(err)
	}
	record.ManagedDatabase = false
	if err := installer.saveServerJSON(serverRecordPath, record); err != nil {
		t.Fatal(err)
	}
	api, err := installer.account(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		t.Fatal(err)
	}
	destination := "/backup"
	if err := os.Mkdir(installer.fs.resolve(destination), 0700); err != nil {
		t.Fatal(err)
	}
	installer.run = func(_ context.Context, command serverCommand) ([]byte, error) {
		if len(command.environment) != 1 || command.environment[0] != "PGPASSFILE=/backup/backup.pgpass" {
			t.Fatal("备份口令未通过私有文件传递")
		}
		for _, argument := range command.arguments {
			if strings.Contains(argument, "://tombot_migration_login:") {
				t.Fatal("密码进入了进程参数")
			}
		}
		info, err := os.Stat(installer.fs.resolve("/backup/backup.pgpass"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("备份认证文件不安全")
		}
		_, err = command.output.Write([]byte("PGDMP"))
		return nil, err
	}
	if err := installer.dumpServerDatabase(t.Context(), configuration, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installer.fs.resolve("/backup/backup.pgpass")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("临时认证材料未删除")
	}
}

func TestServerCommandFailureRedactsOutputAndTimesOut(t *testing.T) {
	// 子进程产生的任意原始输出不能进入错误；这里不是实际认证材料。
	_, err := runServerCommand(t.Context(), serverCommand{program: "/bin/sh", arguments: []string{"-c", "echo private-output >&2; exit 1"}, timeout: time.Second})
	if err == nil || strings.Contains(err.Error(), "private-output") {
		t.Fatal("进程原始输出泄露", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runServerCommand(ctx, serverCommand{program: "/bin/sleep", arguments: []string{"5"}, timeout: time.Second}); err == nil {
		t.Fatal("取消没有中止进程")
	}
}

func TestServerInstallCustomConfigDoesNotCreateDatabaseOrKeys(t *testing.T) {
	original, _ := serverInstallFixture(t)
	installServerFixture(t, original)
	configuration := readServerFixture(t, original, serverConfigPath)
	installer, _ := serverInstallFixture(t)
	api, err := installer.account(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, installer.fs, "/source/server.json", configuration, 0600)
	record, files, err := installer.prepareServerFiles(t.Context(), ServerInstallOptions{Config: "/source/server.json"}, api, readServerFixture(t, installer, "/source/tomd"))
	if err != nil || record.ManagedDatabase || len(files) != 0 || !bytes.Equal(record.InitialConfig, configuration) {
		t.Fatal("高级模式修改了数据库或身份", err)
	}
}
