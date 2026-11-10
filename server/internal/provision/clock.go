package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const clockTimeout = 5 * time.Second
const clockTolerance = 10 * time.Minute

// checkClock 仅读取受信端点的响应头，以请求中点比对服务端时间。超限或离线都记录为降级。
func checkClock(ctx context.Context, address string, transport http.RoundTripper, now func() time.Time) (result error) {
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, address, nil)
	if err != nil {
		return errors.New("时钟校验地址无效")
	}
	req.Header.Set("Cache-Control", "no-cache")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	before := now()
	response, err := client.Do(req)
	if err != nil {
		return errors.New("无法读取时间参照，请检查网络、系统时间或 TOM_CLOCK_URL")
	}
	defer func() { result = errors.Join(result, response.Body.Close()) }()
	midpoint := before.Add(now().Sub(before) / 2)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("时间参照返回 HTTP %d，请检查 TOM_CLOCK_URL", response.StatusCode)
	}
	reference, err := http.ParseTime(response.Header.Get("Date"))
	if err != nil {
		return errors.New("时间参照缺少有效的 HTTP Date 响应头")
	}
	if cached := response.Header.Get("Age"); cached != "" {
		age, err := strconv.ParseUint(cached, 10, 31)
		if err != nil {
			return errors.New("时间参照的缓存时长无效")
		}
		reference = reference.Add(time.Duration(age) * time.Second)
	}
	offset := midpoint.Sub(reference).Abs()
	if offset >= clockTolerance {
		return fmt.Errorf("系统时间与参照相差 %s，请校准主机时间以保证设备授权和例程调度准确", offset.Round(time.Second))
	}
	return nil
}
