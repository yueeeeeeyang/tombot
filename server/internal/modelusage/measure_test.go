package modelusage

import (
	"errors"
	"math"
	"testing"

	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

func integer(value int) *int   { return &value }
func price(value int64) *int64 { return &value }
func same(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func TestReportedMeasurement(t *testing.T) {
	prices := modelregistry.Prices{Input: price(1_000_000), Output: price(2_000_000), CacheRead: price(500_000), CacheWrite: price(10_000_000)}
	for _, test := range []struct {
		name        string
		usage       *provider.ReportedUsage
		included    bool
		total, cost *int64
		lower       int64
	}{
		{"完全未知", nil, true, nil, nil, 0},
		{"缓存包含在输入", &provider.ReportedUsage{InputTokens: integer(6), OutputTokens: integer(2), CacheReadTokens: integer(2), InputIncludesCache: true}, true, price(8), price(9), 8},
		{"拆分未知只保留总量", &provider.ReportedUsage{InputTokens: integer(6), OutputTokens: integer(2), InputIncludesCache: true}, true, price(8), nil, 8},
		{"输出缺失", &provider.ReportedUsage{InputTokens: integer(6), InputIncludesCache: true}, true, nil, nil, 6},
		{"只报告缓存", &provider.ReportedUsage{CacheReadTokens: integer(3), InputIncludesCache: true}, true, nil, nil, 3},
		{"零值有别于未知", &provider.ReportedUsage{InputTokens: integer(0), OutputTokens: integer(0), InputIncludesCache: true}, true, price(0), price(0), 0},
		{"独立缓存计数", &provider.ReportedUsage{InputTokens: integer(7), OutputTokens: integer(2), CacheReadTokens: integer(3), CacheWriteTokens: integer(2)}, false, price(14), price(33), 14},
		{"独立缓存缺失", &provider.ReportedUsage{InputTokens: integer(7), OutputTokens: integer(2), CacheReadTokens: integer(3)}, false, nil, nil, 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			measurement, err := Measure(test.usage, prices, test.included)
			if err != nil || !same(measurement.Tokens, test.total) || !same(measurement.CostMicroUSD, test.cost) || measurement.LowerBound != test.lower {
				t.Fatalf("计量不符：%+v，%v", measurement, err)
			}
		})
	}
	usage := &provider.ReportedUsage{InputTokens: integer(1), OutputTokens: integer(0), CacheReadTokens: integer(0), InputIncludesCache: true}
	for _, unit := range []int64{499_999, 500_000} {
		measurement, err := Measure(usage, modelregistry.Prices{Input: price(unit)}, true)
		if err != nil || measurement.CostMicroUSD == nil || *measurement.CostMicroUSD != (unit+500_000)/1_000_000 {
			t.Fatal("总费用舍入错误", err)
		}
	}
	measurement, err := Measure(usage, modelregistry.Prices{}, true)
	if err != nil || measurement.Tokens == nil || measurement.CostMicroUSD != nil {
		t.Fatal("缺失价格被计作零", err)
	}
}

func TestMeasurementRejectsInvalidCounters(t *testing.T) {
	for _, usage := range []*provider.ReportedUsage{
		{InputTokens: integer(-1), InputIncludesCache: true},
		{InputTokens: integer(1), OutputTokens: integer(1), CacheReadTokens: integer(2), InputIncludesCache: true},
		{InputTokens: integer(1), OutputTokens: integer(1), CacheWriteTokens: integer(1), InputIncludesCache: true},
		{InputTokens: integer(math.MaxInt), OutputTokens: integer(1), InputIncludesCache: true},
		{InputTokens: integer(1)},
	} {
		if _, err := Measure(usage, modelregistry.Prices{}, true); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("非法计数未拒绝", err)
		}
	}
	if _, err := Measure(&provider.ReportedUsage{InputTokens: integer(math.MaxInt), OutputTokens: integer(0), CacheReadTokens: integer(0), InputIncludesCache: true}, modelregistry.Prices{Input: price(1_000_000_000_000)}, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("费用溢出未拒绝", err)
	}
}
