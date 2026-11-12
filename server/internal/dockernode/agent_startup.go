package dockernode

import (
	"context"
	"errors"
	"time"

	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

var errAgentNotListening = errors.New("电脑代理尚未建立监听")

// AgentStartupOptions 只约束无副作用的控制入口检查，不用于重试启动、探针或网页动作。
type AgentStartupOptions struct {
	Timeout         time.Duration
	CallTimeout     time.Duration
	RetryDelay      time.Duration
	MaximumAttempts int
}

func (options AgentStartupOptions) Validate() error {
	if options.Timeout < time.Second || options.Timeout > time.Minute || options.CallTimeout < time.Second || options.CallTimeout > options.Timeout || options.RetryDelay < 10*time.Millisecond || options.RetryDelay > time.Second || options.MaximumAttempts < 1 || options.MaximumAttempts > 16 {
		return errors.New("电脑代理启动检查预算无效")
	}
	return nil
}

type agentAvailability interface {
	wait(context.Context, Resource, container) error
}

type agentStartup struct {
	node    *Runtime
	options AgentStartupOptions
	locate  func(context.Context, string, int) (processgroup.ContainerReference, error)
	check   func(context.Context, Resource, processgroup.ContainerReference) error
}

func newAgentStartup(node *Runtime) *agentStartup {
	startup := &agentStartup{node: node, options: node.options.AgentStartup, locate: processgroup.LocateContainer}
	startup.check = startup.call
	return startup
}

// wait 始终绑定原容器与内核父组；只在未建立连接时等待，其他身份、权限或回执错误直接失败。
func (startup *agentStartup) wait(ctx context.Context, resource Resource, current container) error {
	ctx, cancel := context.WithTimeout(ctx, startup.options.Timeout)
	defer cancel()
	expected, err := startup.node.serviceParent(ctx, resource, current.ID, startup.locate)
	if err != nil {
		return err
	}
	delay := startup.options.RetryDelay
	for attempt := 1; attempt <= startup.options.MaximumAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, err := startup.node.serviceParent(ctx, resource, current.ID, startup.locate)
		if err != nil {
			return err
		}
		if actual != expected {
			return failure("agent_startup_parent_changed")
		}
		call, stop := context.WithTimeout(ctx, startup.options.CallTimeout)
		err = startup.check(call, resource, expected)
		stop()
		if err == nil {
			actual, err = startup.node.serviceParent(ctx, resource, current.ID, startup.locate)
			if err != nil {
				return err
			}
			if actual != expected {
				return failure("agent_startup_parent_changed")
			}
			return ctx.Err()
		}
		if !isAgentNotListening(err) {
			return err
		}
		if attempt == startup.options.MaximumAttempts {
			return errors.Join(failure("agent_startup_attempts_exhausted"), err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		// 退避封顶为配置允许的最大间隔，总时间另受启动预算和开户租约共同约束。
		delay = min(delay*2, time.Second)
	}
	return failure("agent_startup_unconfirmed")
}

func isAgentNotListening(err error) bool {
	if err == errAgentNotListening {
		return true
	}
	connection, ok := err.(*computeragent.ConnectionError)
	return ok && connection.Unwrap() == errAgentNotListening
}

func (startup *agentStartup) call(ctx context.Context, resource Resource, expected processgroup.ContainerReference) error {
	client, err := startup.node.serviceClient(ctx, resource, expected, startup.locate, startup.options.CallTimeout)
	if err != nil {
		return err
	}
	return client.CheckControl(ctx, startup.node.agentConfiguration(resource).Identity)
}
