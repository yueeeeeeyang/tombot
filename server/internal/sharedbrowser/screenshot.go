package sharedbrowser

import (
	"context"
	"errors"
	"time"

	"github.com/chromedp/cdproto"
)

const screenshotActivationAttempts = 5
const screenshotActivationBackoff = 25 * time.Millisecond

// captureActivePage 只处理导航提交后渲染页短暂无活动的明确错误；仅重取只读画面，不重发导航或输入。
// 每次重取重新校验授权，最多五次并指数退避；其他故障、取消和到期立即返回原失败。
func captureActivePage(ctx context.Context, authorize func() error, capture func() ([]byte, error)) ([]byte, error) {
	for attempt := 0; attempt < screenshotActivationAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := authorize(); err != nil {
			return nil, err
		}
		pixels, err := capture()
		if err == nil {
			return pixels, nil
		}
		var response *cdproto.Error
		if !errors.As(err, &response) || response.Code != -32000 || response.Message != "Not attached to an active page" || attempt == screenshotActivationAttempts-1 {
			return nil, err
		}
		timer := time.NewTimer(screenshotActivationBackoff * time.Duration(1<<attempt))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
	return nil, ErrUnavailable
}
