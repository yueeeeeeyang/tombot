//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"io.yueyang/tom/internal/computer"
)

func (scope *kernelScope) Console(ctx context.Context, request computer.ConsoleRequest) (computer.ConsoleReceipt, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	receipt := computer.ConsoleReceipt{Session: request.Session, Revision: request.Revision, State: "paused"}
	if request.Validate() != nil || scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != request.Session || scope.desktop == nil || scope.desktop.snapshot().State != "ready" {
		return receipt, errors.New("桌面接管没有就绪的执行会话")
	}
	if request.Action == "pause" || request.Action == "resume" {
		if request.Revision == scope.consoleRevision && request.Action == scope.consoleAction && (request.Action == "resume" || request.ViewerID == scope.consoleViewer) {
			if scope.consoleFailure != nil {
				return receipt, scope.consoleFailure
			}
			if !scope.isPaused {
				receipt.State = "running"
			}
			return receipt, nil
		}
		if request.Revision <= scope.consoleRevision || scope.consoleFailure != nil {
			return receipt, errors.New("桌面接管控制已过期或前次未确认")
		}
		scope.consoleRevision, scope.consoleAction = request.Revision, request.Action
		paused := request.Action == "pause"
		// 撤回 RFB 输入先于解冻，按键释放完成后 Bot 才能继续。
		if !paused {
			for _, stream := range scope.streams {
				if err := stream.SetControl(false); err != nil && !errors.Is(err, net.ErrClosed) {
					scope.consoleFailure = err
					return receipt, err
				}
			}
		}
		scope.consoleFailure = scope.freezeTasks(ctx, paused)
		if scope.consoleFailure != nil {
			return receipt, scope.consoleFailure
		}
		scope.isPaused = paused
		scope.consoleViewer = request.ViewerID
		if paused {
			if stream := scope.streams[request.ViewerID]; stream != nil {
				if err := stream.SetControl(true); err != nil && !errors.Is(err, net.ErrClosed) {
					scope.consoleFailure = err
					return receipt, err
				}
			}
		} else {
			scope.consoleViewer = ""
			receipt.State = "running"
		}

		return receipt, nil
	}
	return receipt, errors.New("桌面接管动作无效")
}

// freezeTasks 只冻结任务子组；X11、桌面窗口和本桌面的 Chrome 仍然运行。确认失败不允许交接。
func (scope *kernelScope) freezeTasks(ctx context.Context, paused bool) error {
	value := "0"
	if paused {
		value = "1"
	}
	if len(scope.files) < 6 {
		return errors.New("桌面任务冻结控制缺失")
	}
	if count, err := scope.files[4].WriteAt([]byte(value), 0); err != nil || count != 1 {
		return errors.New("桌面任务冻结未发送")
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		encoded := make([]byte, 8193)
		count, err := scope.files[5].ReadAt(encoded, 0)
		if err != nil && !errors.Is(err, io.EOF) || count < 1 || count == len(encoded) {
			return errors.New("桌面任务冻结状态不可读")
		}
		state, err := frozenState(encoded[:count])
		if err != nil {
			return err
		}
		if state == paused {
			return nil
		}
		timer := time.NewTimer(scope.interval)
		select {
		case <-bounded.Done():
			timer.Stop()
			return errors.New("桌面任务冻结状态未确认")
		case <-timer.C:
		}
	}
}

func frozenState(encoded []byte) (bool, error) {
	found, paused := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return false, errors.New("资源组状态格式无效")
		}
		if fields[0] == "frozen" {
			if found || fields[1] != "0" && fields[1] != "1" {
				return false, errors.New("资源组冻结状态无效")
			}
			found, paused = true, fields[1] == "1"
		}
	}
	if !found {
		return false, errors.New("资源组缺少冻结状态")
	}
	return paused, nil
}
