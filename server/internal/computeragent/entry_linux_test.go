//go:build linux

package computeragent

import (
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/agentchannel"
)

func TestKernelAgentConfigurationAndSocketProtection(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openFileStore(directory, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	fixture := newFixture(t)
	configuration := Configuration{Version: ConfigurationVersion, Identity: fixture.guard.identity, Options: RuntimeOptions{EgressConnectTimeoutMS: 1000, EgressIdleTimeoutMS: 1000, EgressLifetimeMS: 10000, EgressMaximumConnections: 8, EgressMaximumTransferBytes: 1 << 20, MaximumActive: 4, CallTimeoutMS: 1000, CleanupTimeoutMS: 1000, RetryDelayMS: 10, CleanupAttempts: 2, PollIntervalMS: 10, ShutdownTimeoutMS: 3000, MaximumConnections: 8}}
	encoded, err := agentchannel.Encode(configuration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ConfigName)
	for _, version := range []int{0, 1, 2, 3, ConfigurationVersion + 1} {
		invalid := configuration
		invalid.Version = version
		invalidEncoded, err := agentchannel.Encode(invalid)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, invalidEncoded, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = loadConfiguration(store); err == nil {
			t.Fatal("不支持的代理配置版本未拒绝", version)
		}
	}
	if err = os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if actual, err := loadConfiguration(store); err != nil || actual != configuration {
		t.Fatal(actual, err)
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfiguration(store); err == nil {
		t.Fatal("可写配置未拒绝")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfiguration(store); err == nil {
		t.Fatal("非规范配置未拒绝")
	}
	socket := filepath.Join(directory, SocketName)
	if err = os.WriteFile(socket, []byte("保留原文件"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = listenControl(store); err == nil {
		t.Fatal("监听入口覆盖普通文件")
	}
	if err = os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	listener, err := listenControl(store)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Lstat(socket)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("套接字权限错误", err)
	}
	// 模拟已持有独占锁的下一次启动清理遗留 socket；不删除关闭屏障。
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err = listenControl(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
}
