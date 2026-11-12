// Package modelusage 管理新运行的真实用量证据、并发名额与月度额度。
package modelusage

import (
	"errors"
	"math"
	"math/big"

	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

var (
	ErrInvalidInput = errors.New("用量参数或供应商计数无效")
	ErrQuota        = errors.New("用户本月可用模型额度不足")
	ErrConcurrency  = errors.New("用户模型调用名额已占满")
	ErrConflict     = errors.New("用量证据与已有记录冲突")
	ErrNotFound     = errors.New("模型调用预留不存在")
)

// Observation 的 Final 表示供应商已完整结束；错误流的部分计数不能当作最终用量。
type Observation struct {
	Usage *provider.ReportedUsage `json:"usage"`
	Final bool                    `json:"is_final"`
}
type Measurement struct {
	Tokens       *int64
	CostMicroUSD *int64
	LowerBound   int64
}

// Measure 只使用明确报告的计数；缓存归属由协议固定，不通过字符估算补齐。
// 总 token 已知但缓存价格或拆分缺失时，费用仍可为未知。
func Measure(usage *provider.ReportedUsage, prices modelregistry.Prices, includesCache bool) (Measurement, error) {
	if usage == nil {
		return Measurement{}, nil
	}
	if usage.InputIncludesCache != includesCache {
		return Measurement{}, ErrInvalidInput
	}
	for _, count := range []*int{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens} {
		if count != nil && *count < 0 {
			return Measurement{}, ErrInvalidInput
		}
	}
	for _, price := range []*int64{prices.Input, prices.Output, prices.CacheRead, prices.CacheWrite} {
		if price != nil && (*price < 0 || *price > 1_000_000_000_000) {
			return Measurement{}, ErrInvalidInput
		}
	}
	value := func(count *int) int64 {
		if count == nil {
			return 0
		}
		return int64(*count)
	}
	input, output, cached, written := value(usage.InputTokens), value(usage.OutputTokens), value(usage.CacheReadTokens), value(usage.CacheWriteTokens)
	if includesCache && (usage.InputTokens != nil && cached > input || written > 0) {
		return Measurement{}, ErrInvalidInput
	}
	components := []int64{input, output}
	known := usage.InputTokens != nil && usage.OutputTokens != nil
	if includesCache {
		components[0] = max(input, cached)
	} else {
		components = append(components, cached, written)
		known = known && usage.CacheReadTokens != nil && usage.CacheWriteTokens != nil
	}
	measurement := Measurement{}
	for _, count := range components {
		if count > math.MaxInt64-measurement.LowerBound {
			return Measurement{}, ErrInvalidInput
		}
		measurement.LowerBound += count
	}
	if !known {
		return measurement, nil
	}
	total := measurement.LowerBound
	measurement.Tokens = &total
	if total == 0 {
		cost := int64(0)
		measurement.CostMicroUSD = &cost
		return measurement, nil
	}
	if includesCache && usage.CacheReadTokens == nil {
		if prices.Input == nil || prices.CacheRead == nil || *prices.Input != *prices.CacheRead {
			return measurement, nil
		}
		cached = 0
	}
	ordinary := input
	if includesCache {
		ordinary -= cached
	}
	amount := new(big.Int)
	for _, component := range []struct {
		count int64
		price *int64
	}{{ordinary, prices.Input}, {output, prices.Output}, {cached, prices.CacheRead}, {written, prices.CacheWrite}} {
		if component.count == 0 {
			continue
		}
		if component.price == nil {
			return measurement, nil
		}
		amount.Add(amount, new(big.Int).Mul(big.NewInt(component.count), big.NewInt(*component.price)))
	}
	amount.Add(amount, big.NewInt(500_000))
	amount.Quo(amount, big.NewInt(1_000_000))
	if !amount.IsInt64() {
		return Measurement{}, ErrInvalidInput
	}
	cost := amount.Int64()
	measurement.CostMicroUSD = &cost
	return measurement, nil
}
