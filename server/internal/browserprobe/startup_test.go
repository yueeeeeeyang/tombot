package browserprobe

import (
	"context"
	"errors"
	"testing"
)

func TestStartupFailurePreservesSpecificStageAndCancellation(t *testing.T) {
	err := StartFailure("browser_start_directory_failed", context.Canceled)
	err = StartFailure("browser_start_protocol_failed", err)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "browser_start_directory_failed" || !errors.Is(err, context.Canceled) {
		t.Fatal("启动阶段或取消原因丢失", err)
	}
	if ValidFailure(failure.Code) || ValidStartFailure("browser_probe_sandbox_failed") || ValidStartFailure("unknown") {
		t.Fatal("启动分类与能力检查边界混用")
	}
}
