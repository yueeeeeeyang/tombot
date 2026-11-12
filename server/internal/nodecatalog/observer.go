package nodecatalog

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/computer"
)

type ObserverOptions struct{ CallTimeout, Interval, Lifetime time.Duration }

// Validate 要求租约覆盖完整一轮观察和间隔，并保持失联检测有明确上限。
func (options ObserverOptions) Validate() error {
	if options.CallTimeout < time.Second || options.CallTimeout > time.Minute || options.Interval < time.Second || options.Interval > time.Minute || options.Lifetime < 3*options.CallTimeout+options.Interval || options.Lifetime > 5*time.Minute {
		return errors.New("节点心跳调用、间隔或租约预算无效")
	}
	return nil
}

// Observer 使用控制数据库与受保护节点通道持续观察，不持有 Docker 权限，也不授予节点 ready。
type Observer struct {
	service *Service
	node    computer.NodeRuntime
	options ObserverOptions
	logger  *slog.Logger
}

// NewObserver 固定目录身份、节点通道和日志；缺少依赖或有限预算时拒绝运行。
func NewObserver(service *Service, node computer.NodeRuntime, options ObserverOptions, logger *slog.Logger) (*Observer, error) {
	if service == nil || node == nil || options.Validate() != nil || logger == nil {
		return nil, errors.New("节点心跳监督配置无效")
	}
	return &Observer{service, node, options, logger}, nil
}

// Poll 分别限制取时、节点查询和保存三个步骤；未知提交不重发旧结果，下一次重新观察。
func (observer *Observer) Poll(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, observer.options.CallTimeout)
	started, err := observer.service.observationStart(ctx)
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(parent, observer.options.CallTimeout)
	heartbeat, err := observer.node.Heartbeat(ctx, computer.NodeQuery{NodeID: observer.service.nodeID})
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(parent, observer.options.CallTimeout)
	defer cancel()
	return observer.service.recordHeartbeat(ctx, started, heartbeat, observer.options.Lifetime)
}

// Run 每轮都是独立的只读节点观察；失败后保留旧租约到期，不关闭仍可用的登录和历史查询。
func (observer *Observer) Run(ctx context.Context) error {
	state := ""
	for ctx.Err() == nil {
		err := observer.Poll(ctx)
		if ctx.Err() != nil {
			break
		}
		current := "confirmed"
		if err != nil {
			current = "unconfirmed"
			if errors.Is(err, ErrNotFound) {
				current = "unregistered"
			}
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrStaleHeartbeat) {
				current = "stale_or_conflicting"
			}
		}
		if current != state {
			if err == nil {
				observer.logger.InfoContext(ctx, "节点控制心跳已确认，电脑就绪仍需能力检查", "node_id", observer.service.nodeID)
			} else {
				observer.logger.WarnContext(ctx, "节点控制心跳未确认，租约到期后禁止新增电脑", "node_id", observer.service.nodeID, "reason", current)
			}
			state = current
		}
		timer := time.NewTimer(observer.options.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
