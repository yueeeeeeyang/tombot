package agentchannel

import (
	"errors"
	"time"
)

// Deadline 绑定接收进程时间命名空间中的绝对单调时钟，避免排队或传输延迟延长执行权。
type Deadline struct {
	ExpiresNS     int64  `json:"expires_ns"`
	TimeNamespace uint64 `json:"time_namespace"`
}

// FromDeadline 在当前时间命名空间表示已有期限；先采样内核，再计算剩余时间，只会保守缩短。
// 向其他进程签发授权时使用 FromDeadlineForProcess，不能假定容器与宿主共用时钟。
func FromDeadline(deadline time.Time) (Deadline, error) {
	now, namespace, err := monotonicClock()
	if err != nil {
		return Deadline{}, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 5*time.Minute || now > 1<<63-1-int64(remaining) {
		return Deadline{}, errors.New("电脑代理共享时钟期限无效")
	}
	return Deadline{ExpiresNS: now + int64(remaining), TimeNamespace: namespace}, nil
}

// Local 拒绝不同时间命名空间和过期期限；不采用请求接收时重新计算完整租约的方式。
func (deadline Deadline) Local(maximum time.Duration) (time.Time, error) {
	anchor := time.Now()
	now, namespace, err := monotonicClock()
	if err != nil {
		return time.Time{}, err
	}
	if maximum < time.Second || maximum > 5*time.Minute || deadline.TimeNamespace != namespace || deadline.ExpiresNS <= now || deadline.ExpiresNS-now > int64(maximum) {
		return time.Time{}, errors.New("电脑代理共享时钟身份或执行期限无效")
	}
	return anchor.Add(time.Duration(deadline.ExpiresNS - now)), nil
}
