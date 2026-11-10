package domain

// Error 向调用方提供稳定错误码；Message 可进入模型上下文，因此使用英文。
type Error struct {
	Code    string `json:"code"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NewError 构造公开错误；不得把底层响应、路径或凭据拼入 message。
func NewError(status int, code, message string) *Error {
	kind := "user_error"
	if status >= 500 {
		kind = "system_error"
	}
	return &Error{Code: code, Kind: kind, Message: message, Status: status}
}
