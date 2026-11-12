package dockernode

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
)

// Delegate 仅从当前活动会话产生代理授权，期限沿用已有节点期限；成功时调用方拥有四个描述符。
// 接收方必须是该用户容器的受信代理，不能把结果返回客户端或模型工具。
func (controller *SessionController) Delegate(ctx context.Context, session computer.ExecutionSession) (grant computeragent.Grant, files []*os.File, failure error) {
	failure = controller.withActiveSession(ctx, session, func(ctx context.Context, state *sessionRecord, lease sessionLease) error {
		var err error
		files, err = controller.groups.Controls(ctx, *state.Group)
		if err != nil {
			return err
		}
		if len(files) != agentchannel.MaximumFiles {
			return errors.New("节点代理控制描述符数量无效")
		}
		deadline, err := agentchannel.FromDeadline(lease.deadline)
		if err != nil {
			return err
		}
		grant = computeragent.Grant{Command: state.Command, Group: *state.Group, Deadline: deadline}
		return ctx.Err()
	})
	if failure != nil {
		failure = errors.Join(failure, agentchannel.CloseFiles(files))
		files = nil
		grant = computeragent.Grant{}
	}
	return grant, files, failure
}

// AgentRenewal 将现有节点期限同步给代理；获取或重发本身不会延长节点授权。
func (controller *SessionController) AgentRenewal(ctx context.Context, session computer.ExecutionSession) (renewal computeragent.Renewal, failure error) {
	failure = controller.withActiveSession(ctx, session, func(ctx context.Context, state *sessionRecord, lease sessionLease) error {
		deadline, err := agentchannel.FromDeadline(lease.deadline)
		if err != nil {
			return err
		}
		renewal = computeragent.Renewal{Command: state.Command, Deadline: deadline}
		return ctx.Err()
	})
	if failure != nil {
		renewal = computeragent.Renewal{}
	}
	return renewal, failure
}

// CheckAgentPeer 将 SO_PEERCRED 返回的宿主 PID 重新定位到固定容器父组，不单凭 root 身份信任对端。
func (controller *SessionController) CheckAgentPeer(ctx context.Context, session computer.ExecutionSession, peer agentchannel.Peer) error {
	if peer.PID <= 1 || peer.UID != 0 || peer.GID != 0 {
		return failure("agent_peer_invalid")
	}
	return controller.withActiveSession(ctx, session, func(ctx context.Context, state *sessionRecord, _ sessionLease) error {
		reference, err := controller.groups.Locate(ctx, session, int(peer.PID))
		if err != nil {
			return err
		}
		if reference != *state.Group {
			return failure("agent_peer_container_mismatch")
		}
		return nil
	})
}
