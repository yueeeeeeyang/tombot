package computeragent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
)

func TestControlHealthConnectionFailureRetainsCauseAndHidesDiagnostic(t *testing.T) {
	fixture := newFixture(t)
	cause := errors.New("测试底层诊断")
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) { return nil, cause }, func(agentchannel.Peer) error { return errors.New("连接失败后不应验证对端") }, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = client.CheckControl(t.Context(), fixture.guard.identity)
	var connection *ConnectionError
	if !errors.As(err, &connection) || !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatal("连接错误丢失分类或泄漏诊断", err)
	}
}
