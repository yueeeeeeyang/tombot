package nodecommand

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/dockernode"
)

type prepareImage func(context.Context, string, *slog.Logger) (dockernode.ImagePreparation, error)

// runPrepareImage 使用固定环境入口，失败没有成功回执；输出失败同样返回非零，调用方须按原身份重新核验。
func runPrepareImage(ctx context.Context, getenv func(string) string, output io.Writer, logger *slog.Logger, prepare prepareImage) int {
	path := getenv("TOM_IMAGE_PREPARATION_MANIFEST")
	if path == "" {
		logger.Error("镜像准备必须指定受保护清单")
		return 2
	}
	report, err := prepare(ctx, path, logger)
	if err != nil {
		reason := "image_preparation_failed"
		var failure *computer.Failure
		if errors.As(err, &failure) {
			reason = failure.Code
		} else if errors.Is(err, context.Canceled) {
			reason = "image_preparation_cancelled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = "image_preparation_timeout"
		}
		logger.Error("镜像准备未完成，需要保留原身份重新核验", "reason", reason)
		return 1
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		logger.Error("镜像准备回执输出失败")
		return 1
	}
	return 0
}
