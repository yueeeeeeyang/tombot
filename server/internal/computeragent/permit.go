// Package computeragent 在用户容器内持有会话授权，并独立回收失去续约的执行环境。
package computeragent

import (
	"errors"
	"os"
	"sync"
	"time"
)

// permit 的撤销计时器不等待业务锁；即使启动操作或状态刷盘阻塞，也先封闭迟到引导。
type permit struct {
	mu             sync.Mutex
	deadline       time.Time
	revision       int64
	reader, writer *os.File
	timer          *time.Timer
	revoked        bool
	closeError     error
	readClosed     bool
	readCloseError error
	notify         chan<- struct{}
	onRevoke       func()
}

func newPermit(deadline time.Time, revision int64, notify chan<- struct{}, onRevoke func()) (*permit, error) {
	if revision < 1 || deadline.IsZero() || notify == nil || onRevoke == nil {
		return nil, errors.New("电脑执行撤销授权配置无效")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.New("创建电脑执行撤销通道失败")
	}
	value := &permit{deadline: deadline, revision: revision, reader: reader, writer: writer, notify: notify, onRevoke: onRevoke}
	value.mu.Lock()
	value.timer = time.AfterFunc(time.Until(deadline), value.expire)
	value.mu.Unlock()
	return value, nil
}

func (value *permit) expire() {
	value.mu.Lock()
	defer value.mu.Unlock()
	if value.revoked {
		return
	}
	if remaining := time.Until(value.deadline); remaining > 0 {
		value.timer.Reset(remaining)
		return
	}
	value.revokeLocked()
}

func (value *permit) revokeLocked() {
	if value.revoked {
		return
	}
	value.revoked = true
	value.timer.Stop()
	if value.writer.Close() != nil {
		value.closeError = errors.New("关闭电脑执行撤销通道失败")
	}
	// 回调只安排独立终止信号，不等待业务锁或磁盘刷盘。
	value.onRevoke()
	select {
	case value.notify <- struct{}{}:
	default:
	}
}

func (value *permit) active(revision int64) bool {
	value.mu.Lock()
	defer value.mu.Unlock()
	if !value.revoked && !time.Now().Before(value.deadline) {
		value.revokeLocked()
	}
	return !value.revoked && value.revision == revision
}

func (value *permit) renew(revision int64, deadline time.Time) error {
	value.mu.Lock()
	defer value.mu.Unlock()
	if !value.revoked && !time.Now().Before(value.deadline) {
		value.revokeLocked()
	}
	if value.revoked || revision <= value.revision || !time.Now().Before(deadline) {
		return errors.New("电脑执行授权已结束或续约版本无效")
	}
	value.deadline, value.revision = deadline, revision
	value.timer.Reset(time.Until(deadline))
	return nil
}

func (value *permit) revoke() error {
	value.mu.Lock()
	defer value.mu.Unlock()
	value.revokeLocked()
	return value.closeError
}

func (value *permit) close() error {
	result := value.revoke()
	value.mu.Lock()
	defer value.mu.Unlock()
	if !value.readClosed {
		value.readClosed = true
		if value.reader.Close() != nil {
			value.readCloseError = errors.New("关闭电脑执行撤销读端失败")
		}
	}
	return errors.Join(result, value.readCloseError)
}
