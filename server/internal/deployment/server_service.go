package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// runSystemctl 只接收安装器构造的固定命令参数，保留操作与服务名称，原始进程输出仍不外泄。
func (installer serverInstaller) runSystemctl(ctx context.Context, timeout time.Duration, arguments ...string) ([]byte, error) {
	output, err := installer.run(ctx, serverCommand{program: "/usr/bin/systemctl", arguments: arguments, timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("systemctl %s 失败：%w", strings.Join(arguments, " "), err)
	}
	return output, nil
}

// startServer 让首次安装与升级共用状态判断；未启动的单元可能被 systemd 回收，不能无条件 reset-failed。
func (installer serverInstaller) startServer(ctx context.Context) error {
	if _, err := installer.runSystemctl(ctx, 90*time.Second, "daemon-reload"); err != nil {
		return err
	}
	properties, err := installer.runSystemctl(ctx, 15*time.Second, "show", "tomd.service", "--property=LoadState,ActiveState")
	if err != nil {
		return err
	}
	state := parseServiceProperties(properties)
	if state["LoadState"] != "loaded" || state["ActiveState"] == "" {
		return errors.New("tomd.service 单元未正确加载，请检查服务单元状态")
	}
	// 失败单元会保留在 systemd 内存中；只对它清除失败和启动限流，实际失败仍向上传递。
	if state["ActiveState"] == "failed" {
		if _, err := installer.runSystemctl(ctx, 15*time.Second, "reset-failed", "tomd.service"); err != nil {
			return err
		}
	}
	_, err = installer.runSystemctl(ctx, 90*time.Second, "enable", "--now", "tomd.service")
	return err
}
