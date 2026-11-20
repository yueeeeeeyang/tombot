package deployment

import (
	"errors"

	"io.yueyang/tom/internal/noderelease"
)

type upgradeFailure struct {
	step  string
	cause error
}

func (failure upgradeFailure) Error() string       { return failure.cause.Error() }
func (failure upgradeFailure) Unwrap() error       { return failure.cause }
func (failure upgradeFailure) UpgradeStep() string { return failure.step }

// FailedUpgradeStep 将本机或控制服务的已知阻塞步骤写入作业结果，不转发系统命令输出。
func FailedUpgradeStep(err error) string {
	var failure interface{ UpgradeStep() string }
	if errors.As(err, &failure) && noderelease.ValidUpgradeStep(failure.UpgradeStep()) {
		return failure.UpgradeStep()
	}
	return "preflight"
}
