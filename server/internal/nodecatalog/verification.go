package nodecatalog

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/computer"
)

var ErrVerificationLease = errors.New("节点验收租约已经失效")

// VerificationOptions 固定独立验收电脑的镜像、预算与证据时限，不使用业务账户执行部署探针。
type VerificationOptions struct {
	ImageID       string
	Resources     computer.Resources
	CallTimeout   time.Duration
	LeaseDuration time.Duration
	Interval      time.Duration
	Lifetime      time.Duration
	RetryDelay    time.Duration
	MaxAttempts   int
}

// Validate 要求租约覆盖检查、清理、心跳和提交预算，拒绝未指定镜像、无界资源或无限核实。
func (options VerificationOptions) Validate() error {
	if !validID(options.ImageID) || options.Resources.Validate() != nil || options.CallTimeout < time.Second || options.CallTimeout > time.Minute ||
		options.LeaseDuration < 5*options.CallTimeout || options.LeaseDuration > 10*time.Minute || options.Interval < time.Minute || options.Interval > 30*time.Minute ||
		options.Lifetime < options.Interval+options.LeaseDuration || options.Lifetime > time.Hour || options.RetryDelay < time.Second || options.RetryDelay > time.Minute || options.MaxAttempts < 1 || options.MaxAttempts > 20 {
		return errors.New("节点验收镜像、资源或有限预算无效")
	}
	return nil
}

// VerificationRuntime 只组合已有受限电脑生命周期和控制心跳，不引入通用宿主命令能力。
type VerificationRuntime interface {
	computer.Runtime
	computer.NodeRuntime
}

// Verifier 复用正式供给与清理接口；只有临时电脑全能力通过且清理完成，才发布有时限的镜像准入。
type Verifier struct {
	service *Service
	node    VerificationRuntime
	options VerificationOptions
	logger  *slog.Logger
}

type verification struct {
	specification     computer.Specification
	imageID           string
	runtimeID         string
	runtimeGeneration int64
	isValid           bool
	state             string
	attempts          int
	capabilities      *computer.Capabilities
	verifiedAt        *time.Time
	errorCode         string
	started           time.Time
}

// NewVerifier 拒绝缺少真实节点通道或无界配置；资源身份由持久账本生成，不接受调用方指定用户电脑。
func NewVerifier(service *Service, node VerificationRuntime, options VerificationOptions, logger *slog.Logger) (*Verifier, error) {
	if service == nil || node == nil || logger == nil || options.Validate() != nil {
		return nil, errors.New("节点能力验收依赖无效")
	}
	return &Verifier{service, node, options, logger}, nil
}

// ProcessNext 先领取持久租约再核实同一资源；不确定结果先对账，不能另外创建替代电脑。
func (verifier *Verifier) ProcessNext(ctx context.Context) (bool, error) {
	current, err := verifier.claim(ctx)
	if err != nil || current == nil {
		return false, err
	}
	if current.state == "checking" && !current.isValid {
		return true, verifier.advance(ctx, current, "cleaning", "node_control_changed")
	}
	observation, err := verifier.call(ctx, current, verifier.node.Inspect)
	if err != nil {
		return true, verifier.uncertain(ctx, current, "inspection_unconfirmed")
	}
	if !observation.Matches(current.specification) || observation.State == "quarantined" {
		return true, verifier.advance(ctx, current, "quarantined", "resource_identity_unconfirmed")
	}
	if current.state == "cleaning" {
		// 物理资源不存在不代表本代次已经持久关闭；仍调用幂等 Remove 消费关闭屏障，才能安全进入下一代。
		observation, err = verifier.call(ctx, current, verifier.node.Remove)
		if err != nil {
			return true, verifier.uncertain(ctx, current, "cleanup_unconfirmed")
		}
		if !observation.Matches(current.specification) || observation.State != "absent" {
			return true, verifier.uncertain(ctx, current, "cleanup_unconfirmed")
		}
		return true, verifier.finish(ctx, current)
	}
	if observation.State != "ready" {
		observation, err = verifier.call(ctx, current, verifier.node.Ensure)
		if err != nil {
			var failure *computer.Failure
			if errors.As(err, &failure) && failure.IsPermanent {
				return true, verifier.advance(ctx, current, "cleaning", "capability_check_failed")
			}
			return true, verifier.uncertain(ctx, current, "creation_unconfirmed")
		}
	}
	if !observation.Matches(current.specification) {
		return true, verifier.advance(ctx, current, "quarantined", "resource_identity_unconfirmed")
	}
	if observation.State != "ready" || !observation.Capabilities.Ready() {
		return true, verifier.advance(ctx, current, "cleaning", "capability_check_failed")
	}
	current.capabilities = &observation.Capabilities
	current.verifiedAt = &current.started
	return true, verifier.advance(ctx, current, "cleaning", "")
}

func (verifier *Verifier) call(ctx context.Context, current *verification, invoke func(context.Context, computer.Specification) (computer.Observation, error)) (computer.Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, verifier.options.CallTimeout)
	defer cancel()
	return invoke(ctx, current.specification)
}

// uncertain 只安排有限次数的状态核实；达到上限后保留隔离记录，不遗忘可能仍存在的临时资源。
func (verifier *Verifier) uncertain(ctx context.Context, current *verification, code string) error {
	state := current.state
	if current.attempts >= verifier.options.MaxAttempts {
		state = "quarantined"
	}
	return verifier.advance(ctx, current, state, code)
}

// Run 独立于心跳监督；验收失败只阻止新增电脑，不关闭身份认证与历史查询。
func (verifier *Verifier) Run(ctx context.Context) error {
	previous := ""
	for ctx.Err() == nil {
		claimed, err := verifier.ProcessNext(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state := "observing"
		if err != nil {
			state = "unconfirmed"
		}
		if state != previous && err != nil {
			verifier.logger.WarnContext(ctx, "节点能力验收暂未确认，保留原记录并停止新增准入", "node_id", verifier.service.nodeID, "reason", state)
		}
		previous = state
		if claimed && err == nil {
			continue
		}
		timer := time.NewTimer(verifier.options.RetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
