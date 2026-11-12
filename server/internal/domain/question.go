package domain

import "context"

type UserQuestion struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}
type QuestionAnswer struct {
	Choice *string `json:"choice"`
	Text   string  `json:"text"`
}

// UserWaiter 由当前运行提供等待边界，释放并发槽并暂停人工等待期间的执行计时。
type UserWaiter interface {
	WaitForUser(context.Context, func(context.Context) (QuestionAnswer, error)) (QuestionAnswer, error)
}
