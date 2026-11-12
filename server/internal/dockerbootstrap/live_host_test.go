//go:build linux && hostintegration

package dockerbootstrap

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestLiveHostPackages 只读验证真实 Ubuntu 包数据库、工具权限及启动，不安装包、创建磁盘或挂载文件系统。
func TestLiveHostPackages(t *testing.T) {
	if os.Getenv("TOM_HOST_TOOLS_TEST") != "1" || os.Geteuid() != 0 {
		t.Fatal("宿主工具验证必须显式启用，并在隔离 Linux root 环境执行")
	}
	manifest := Manifest{
		E2fsprogsPackageVersion: os.Getenv("TOM_TEST_E2FSPROGS_VERSION"),
		UtilLinuxPackageVersion: os.Getenv("TOM_TEST_UTIL_LINUX_VERSION"),
		MountPackageVersion:     os.Getenv("TOM_TEST_MOUNT_VERSION"),
	}
	if manifest.E2fsprogsPackageVersion == "" || manifest.UtilLinuxPackageVersion == "" || manifest.MountPackageVersion == "" {
		t.Fatal("真实宿主工具验证必须指定三个预期软件包版本")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	if err = ubuntuRelease(release); err != nil {
		t.Fatal(err)
	}
	installation := installer{host: realHost(), manifest: manifest}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	packages, err := installation.verifyHostPackages(ctx)
	if err != nil {
		t.Fatal("真实宿主包和工具核验失败", err)
	}
	if !reflect.DeepEqual(packages, installation.hostPackages()) {
		t.Fatal("返回了与预期版本不符的实际包证据")
	}
	for _, actual := range packages {
		t.Logf("已验证宿主软件包：%s %s %s", actual.Name, actual.Architecture, actual.Version)
	}
	installation.manifest.E2fsprogsPackageVersion = "0.invalid-test"
	if _, err = installation.verifyHostPackages(ctx); err == nil {
		t.Fatal("真实包版本不匹配仍通过验证")
	}
}
