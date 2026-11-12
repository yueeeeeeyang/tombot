//go:build dockerintegration

package dockerbootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveDockerEngine 只读验证实际引擎，不连接已有容器，不以此代替 Linux 安装和电脑探针验收。
func TestLiveDockerEngine(t *testing.T) {
	program, socket, version := os.Getenv("TOM_TEST_DOCKER_BIN"), os.Getenv("TOM_TEST_DOCKER_SOCKET"), os.Getenv("TOM_TEST_DOCKER_VERSION")
	if !filepath.IsAbs(program) || !filepath.IsAbs(socket) || version == "" {
		t.Fatal("真实引擎验证必须指定 Docker 程序、Unix 套接字及预期版本")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runCommand(ctx, program, "--config", t.TempDir(), "--host", "unix://"+socket, "info", "--format", "{{json .}}")
	if err != nil || result.exitCode != 0 {
		t.Fatal("读取真实 Docker 引擎状态失败", err)
	}
	report, err := verifyEngine(result.output, Manifest{AllowedEngineVersions: []string{version}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("真实 Docker 引擎已验证：版本 %s，系统 %s，cgroup %s", report.Version, report.OperatingSystem, report.CgroupVersion)
}
