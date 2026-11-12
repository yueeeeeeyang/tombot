//go:build linux

package dockernode

import (
	"context"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/infra/ids"
)

func TestNodeDelegatedAuthority(t *testing.T) {
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	groups := &fakeGroups{path: t.TempDir()}
	controller := fixture.sessionController(t, groups, sessionOptions())
	command := commandFor(session)
	if _, err := controller.Open(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	original, _ := controller.lease(sessionKeyOf(session))
	grant, files, err := controller.Delegate(context.Background(), session)
	if err != nil || len(files) != 4 || grant.Command != command || grant.Group.Session != session {
		t.Fatal("代理授权没有绑定当前节点会话", err)
	}
	if err := agentchannel.CloseFiles(files); err != nil {
		t.Fatal(err)
	}
	local, err := grant.Deadline.Local(command.Lifetime)
	if err != nil || local.After(original.deadline) {
		t.Fatal("委派延长了现有节点期限", err)
	}
	if err := controller.CheckAgentPeer(context.Background(), session, agentchannel.Peer{PID: 4242, UID: 10000, GID: 10000}); err == nil {
		t.Fatal("任务身份冒充代理")
	}
	if err := controller.CheckAgentPeer(context.Background(), session, agentchannel.Peer{PID: 4242}); err != nil {
		t.Fatal(err)
	}
	wrong := session
	wrong.UserID = ids.New()
	if _, files, err := controller.Delegate(context.Background(), wrong); err == nil || files != nil {
		t.Fatal("向另一用户委派资源组")
	}
	command.Revision++
	if _, err := controller.Renew(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	renewal, err := controller.AgentRenewal(context.Background(), session)
	if err != nil || renewal.Command != command {
		t.Fatal("代理续约没有使用最新节点版本", err)
	}
	controller.remember(sessionKeyOf(session), sessionLease{deadline: time.Now().Add(-time.Second)})
	if _, files, err := controller.Delegate(context.Background(), session); err == nil || files != nil {
		t.Fatal("过期节点仍委派描述符")
	}
	if _, err := controller.AgentRenewal(context.Background(), session); err == nil {
		t.Fatal("过期节点生成代理续约")
	}
}
