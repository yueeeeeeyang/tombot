package dockernode

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computeragent"
)

type stoppedAgentStore interface {
	computeragent.StoreCatalog
	computeragent.BrowserStore
	computeragent.ProbeStore
}

// prepareRestart 使用与代理相同的控制目录独占锁，持锁复验原容器已退出后才可写入本地关闭屏障。
// 宿主旧会话、浏览器和探针已完成回收。释放目录独占锁后，调用方才能启动原容器。
func (startup *agentStartup) prepareRestart(ctx context.Context, resource Resource, state *record) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := computeragent.NewFileStore(resource.ControlPath)
	if err != nil {
		return errors.Join(failure("agent_recovery_store_unavailable"), err)
	}
	defer func() { result = errors.Join(result, store.Close()) }()
	node := startup.node
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return err
	}
	if state.ContainerID == "" || !current.hasContainer || !exitedComputer(current.container) {
		return failure("agent_recovery_container_not_exited")
	}
	return node.closeStoppedAgent(ctx, resource, state.ContainerID, store)
}

// closeStoppedAgent 以宿主关闭记录为依据封闭代理旧授权，保留已消费的版本、命令和资源组身份。
func (node *Runtime) closeStoppedAgent(ctx context.Context, resource Resource, containerID string, store stoppedAgentStore) error {
	identity := node.agentConfiguration(resource).Identity
	if err := store.Visit(ctx, func(observed computeragent.Snapshot) error {
		if observed.Phase == "closed" {
			return nil
		}
		session := observed.Command.Session
		if session.UserID != identity.UserID || session.ComputerID != identity.ComputerID || session.NodeID != identity.NodeID ||
			session.Generation != identity.Generation || session.ContainerID != containerID {
			return failure("agent_recovery_session_mismatch")
		}
		host, err := node.journal.readSession(sessionKeyOf(session))
		if err != nil {
			return err
		}
		if host == nil || host.Phase != "closed" || host.Command.Session != session || host.Command.Revision < observed.Command.Revision ||
			host.Group == nil || observed.Group == nil || *host.Group != *observed.Group {
			return failure("agent_recovery_session_unconfirmed")
		}
		desktop, err := store.Desktop(session.DesktopID)
		if err != nil {
			return err
		}
		observed.Phase = "closed"
		return desktop.Save(ctx, observed)
	}); err != nil {
		return err
	}

	browser, err := store.LoadBrowser(ctx)
	if err != nil {
		return err
	}
	if browser != nil && browser.Phase != "closed" {
		host, err := node.journal.readBrowser(identity.UserID)
		if err != nil {
			return err
		}
		grant := browser.Grant
		if grant.Identity != identity || grant.Request.Group.Container.ContainerID != containerID ||
			host == nil || host.Phase != "closed" || host.Grant.Identity != identity || host.Grant.Request.Group.Container.ContainerID != containerID ||
			host.Grant.Revision < grant.Revision || host.Grant.Revision == grant.Revision && host.Grant != grant {
			return failure("agent_recovery_browser_unconfirmed")
		}
		browser.Phase = "closed"
		if err := store.SaveBrowser(ctx, *browser); err != nil {
			return err
		}
	}

	probe, err := store.LoadProbe(ctx)
	if err != nil {
		return err
	}
	if probe != nil && probe.Phase != "closed" {
		host, err := node.journal.readProbe(identity.UserID)
		if err != nil {
			return err
		}
		grant := probe.Grant
		if grant.Identity != identity || grant.Request.Group.Container.ContainerID != containerID ||
			host == nil || host.Phase != "closed" || host.Grant.Identity != identity || host.Grant.Request.Group.Container.ContainerID != containerID ||
			host.Grant.Revision < grant.Revision || host.Grant.Revision == grant.Revision && host.Grant != grant {
			return failure("agent_recovery_probe_unconfirmed")
		}
		probe.Phase = "closed"
		return store.SaveProbe(ctx, *probe)
	}
	return ctx.Err()
}
