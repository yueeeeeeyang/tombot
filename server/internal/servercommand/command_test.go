package servercommand

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/deployment"
)

func TestCommandsValidateBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{{}, {"doctor"}, {"install", "--password", "hidden"}, {"status", "--url", "https://example.test"}, {"upgrade", "candidate"}, {"install", "--url", "http://example.test"}, {"install", "--certificate", "certificate.pem"}, {"install", "--config", "server.json", "--url", "https://example.test"}} {
		called := false
		var output bytes.Buffer
		err := execute(t.Context(), args, &output, func(context.Context, string, deployment.ServerInstallOptions) (deployment.ServerReport, error) {
			called = true
			return deployment.ServerReport{}, nil
		})
		if err == nil || called {
			t.Fatal("无效参数触发部署", args, err)
		}
		if strings.Contains(err.Error(), "hidden") {
			t.Fatal("原始参数泄露")
		}
	}
}

func TestInstallUpgradeStatusAndHelp(t *testing.T) {
	for _, action := range []string{"install", "upgrade", "status"} {
		var output bytes.Buffer
		args := []string{action}
		if action == "install" {
			args = append(args, "--url", "https://example.test")
		}
		err := execute(t.Context(), args, &output, func(_ context.Context, actual string, options deployment.ServerInstallOptions) (deployment.ServerReport, error) {
			if actual != action {
				t.Fatal("命令路由错误")
			}
			if action == "install" && options.URL != "https://example.test:9443" {
				t.Fatal("默认端口未生成")
			}
			return deployment.ServerReport{Service: "active", Healthy: true, Enabled: true, URL: "https://example.test:9443/admin/", SelfSigned: true}, nil
		})
		if err != nil || !strings.Contains(output.String(), "管理平台：") || !strings.Contains(output.String(), "自签名") {
			t.Fatal(output.String(), err)
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"install", "--help"}} {
		var output bytes.Buffer
		if err := execute(t.Context(), args, &output, func(context.Context, string, deployment.ServerInstallOptions) (deployment.ServerReport, error) {
			t.Fatal("帮助触发部署")
			return deployment.ServerReport{}, nil
		}); err != nil || !strings.Contains(output.String(), "upgrade") {
			t.Fatal(err)
		}
	}
}

func TestStatusFailureStillPrintsActualState(t *testing.T) {
	var output bytes.Buffer
	err := execute(t.Context(), []string{"status"}, &output, func(context.Context, string, deployment.ServerInstallOptions) (deployment.ServerReport, error) {
		return deployment.ServerReport{Service: "inactive"}, errors.New("服务停止")
	})
	if err == nil || !strings.Contains(output.String(), "inactive") || strings.Contains(output.String(), "安装完成") {
		t.Fatal(output.String(), err)
	}
}

func TestHTTPSRenewalIsExplicitUpgradeOption(t *testing.T) {
	var output bytes.Buffer
	err := execute(t.Context(), []string{"upgrade", "--renew-https"}, &output, func(_ context.Context, action string, options deployment.ServerInstallOptions) (deployment.ServerReport, error) {
		if action != "upgrade" || !options.RenewHTTPS {
			t.Fatal("重新签发意图未传递")
		}
		return deployment.ServerReport{Service: "active", CertificateFile: "/etc/tombot/server/https/certificate.pem", CertificateSHA256: strings.Repeat("a", 64)}, nil
	})
	if err != nil || !strings.Contains(output.String(), "HTTPS 公共证书：") || !strings.Contains(output.String(), "证书 SHA-256 指纹：") {
		t.Fatal("没有提供重新信任证书所需信息", err)
	}
	for _, action := range []string{"install", "status"} {
		if err := execute(t.Context(), []string{action, "--renew-https"}, &output, func(context.Context, string, deployment.ServerInstallOptions) (deployment.ServerReport, error) {
			t.Fatal("非升级命令接受了重新签发参数")
			return deployment.ServerReport{}, nil
		}); err == nil {
			t.Fatal("无效参数未拒绝")
		}
	}
}
