package domain

import "time"

// MonthlyBudget 按 UTC 自然月控制新回合准入；空额度表示不限制。
type MonthlyBudget struct {
	LimitMicro  *int64 `json:"limit_micro"`
	WarnPercent int    `json:"warn_percent"`
	Action      string `json:"action"`
	Revision    int64  `json:"revision"`
}

type UsageTotals struct {
	Calls           int64 `json:"calls"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	CostMicro       int64 `json:"cost_micro"`
	EstimatedCalls  int64 `json:"estimated_calls"`
}

type UsageGroup struct {
	UsageTotals
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MonthlyUsage struct {
	Month string `json:"month"`
	UsageTotals
	Budget MonthlyBudget `json:"budget"`
	Status string        `json:"status"`
}

type UsageReport struct {
	From       time.Time    `json:"from"`
	To         time.Time    `json:"to"`
	GroupBy    string       `json:"group_by"`
	Total      UsageTotals  `json:"total"`
	Groups     []UsageGroup `json:"groups"`
	NextCursor string       `json:"next_cursor"`
	Monthly    MonthlyUsage `json:"monthly"`
}
