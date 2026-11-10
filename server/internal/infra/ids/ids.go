package ids

import "github.com/oklog/ulid/v2"

// New 返回可排序的唯一业务标识，熵不足时由标准库终止进程。
func New() string { return ulid.Make().String() }
