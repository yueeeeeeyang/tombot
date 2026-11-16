package deployment

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/nodeenrollment"
)

func TestNodeAddressRejectsServerEndpointBeforePublication(t *testing.T) {
	installer, _, _ := automaticFixture(t)
	installer.prepareInstallation = func(ctx context.Context) (installationInput, error) {
		return installer.prepareAutomatic(ctx, "", "/source/tomnode", func(context.Context) ([]byte, error) {
			token := generatedEnrollmentToken()
			defer clear(token)
			code, err := nodeenrollment.EncodeJoinCode(nodeenrollment.JoinCode{URL: "https://api.example.test", Fingerprints: []string{strings.Repeat("c", 64)}, Token: string(token)})
			return []byte(code), err
		}, func(context.Context, string) (string, error) { return "https://API.example.test:443", nil })
	}
	if _, err := installer.install(t.Context(), "", "/source/tomnode"); err == nil || !strings.Contains(err.Error(), "不能与服务端地址相同") {
		t.Fatal("没有拦截重复服务地址", err)
	}
	if _, err := os.Stat(installer.fs.resolve(deploy.NodeDirectory)); !os.IsNotExist(err) {
		t.Fatal("错误地址仍发布身份")
	}
}

func TestNodeListenDetectsOccupiedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := checkNodeListen(t.Context(), address); err == nil {
		t.Fatal("被占端口仍通过")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkNodeListen(t.Context(), address); err != nil {
		t.Fatal("空闲端口未通过", err)
	}
}
