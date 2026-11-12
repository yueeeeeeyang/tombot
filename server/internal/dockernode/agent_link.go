package dockernode

import (
	"context"
	"errors"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

// sessionAuthority 在用户生命周期共享锁和桌面独占锁内调用，不得递归加锁；实现必须允许不同桌面并发。
type sessionAuthority interface {
	open(context.Context, computer.SessionCommand, processgroup.Reference, time.Time) error
	renew(context.Context, computer.SessionCommand, processgroup.Reference, time.Time) error
	close(context.Context, computer.SessionCommand, *processgroup.Reference) error
}

type agentLink struct {
	node    *Runtime
	groups  processgroup.Groups
	timeout time.Duration
}

func (link *agentLink) client(ctx context.Context, command computer.SessionCommand, expected *processgroup.Reference) (*computeragent.Client, error) {
	state, err := link.node.journal.read(command.Session.UserID)
	if err != nil {
		return nil, err
	}
	if state == nil || state.Specification.UserID != command.Session.UserID || state.Specification.ComputerID != command.Session.ComputerID || state.Specification.NodeID != command.Session.NodeID || state.Specification.Generation != command.Session.Generation || state.ContainerID != command.Session.ContainerID {
		return nil, failure("agent_instance_mismatch")
	}
	resource := link.node.resource(state.Specification)
	current, err := link.node.load(ctx, resource, state)
	if err != nil {
		return nil, err
	}
	if !current.hasContainer || !current.container.State.Running || current.container.State.Dead || current.container.State.Pid <= 1 {
		return nil, failure("agent_instance_stopped")
	}
	reference, err := link.groups.Locate(ctx, command.Session, current.container.State.Pid)
	if err != nil {
		return nil, err
	}
	if reference.Validate() != nil || reference.Session != command.Session || expected != nil && reference != *expected {
		return nil, failure("agent_parent_mismatch")
	}
	return link.node.agentClient(resource, func(peer agentchannel.Peer) error {
		if peer.PID <= 1 || peer.UID != 0 || peer.GID != 0 {
			return failure("agent_peer_invalid")
		}
		observed, err := link.groups.Locate(ctx, command.Session, int(peer.PID))
		if err != nil {
			return err
		}
		if observed != reference {
			return failure("agent_peer_container_mismatch")
		}
		return nil
	}, link.timeout)
}

func (link *agentLink) open(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, deadline time.Time) (failureResult error) {
	client, err := link.client(ctx, command, &reference)
	if err != nil {
		return err
	}
	files, err := link.groups.Controls(ctx, reference)
	if err != nil {
		return err
	}
	defer func() { failureResult = errors.Join(failureResult, agentchannel.CloseFiles(files)) }()
	stamp, err := agentchannel.FromDeadline(deadline)
	if err != nil {
		return err
	}
	_, err = client.Open(ctx, computeragent.Grant{Command: command, Group: reference, Deadline: stamp}, files)
	return err
}

func (link *agentLink) renew(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, deadline time.Time) error {
	client, err := link.client(ctx, command, &reference)
	if err != nil {
		return err
	}
	stamp, err := agentchannel.FromDeadline(deadline)
	if err != nil {
		return err
	}
	_, err = client.Renew(ctx, computeragent.Renewal{Command: command, Deadline: stamp})
	return err
}

func (link *agentLink) close(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) error {
	client, err := link.client(ctx, command, reference)
	if err != nil {
		return err
	}
	_, err = client.Close(ctx, command)
	return err
}
