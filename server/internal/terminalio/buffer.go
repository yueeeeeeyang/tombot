package terminalio

import "unicode/utf8"

// Buffer 只保留固定容量的尾部；游标按规范化后的 UTF-8 字节累计，断线客户端可检测缺口。
type Buffer struct {
	bytes []byte
	End   int64
}

func NewBuffer(capacity int) Buffer {
	return Buffer{bytes: make([]byte, capacity)}
}

func (b *Buffer) Append(text []byte) {
	start := b.End
	b.End += int64(len(text))
	if len(text) > len(b.bytes) {
		text = text[len(text)-len(b.bytes):]
		start = b.End - int64(len(text))
	}
	offset := int(start % int64(len(b.bytes)))
	copied := copy(b.bytes[offset:], text)
	copy(b.bytes, text[copied:])
}

func (b *Buffer) Read(cursor int64, limit int) (text string, next int64, truncated bool) {
	start := max(int64(0), b.End-int64(len(b.bytes)))
	for start < b.End && !utf8.RuneStart(b.bytes[start%int64(len(b.bytes))]) {
		start++
	}
	truncated = cursor < start
	cursor = max(cursor, start)
	// 外部游标必须指向字符边界；中间字节向前跳过并明确要求客户端重置画面。
	for cursor < b.End && !utf8.RuneStart(b.bytes[cursor%int64(len(b.bytes))]) {
		cursor++
		truncated = true
	}
	next = min(b.End, cursor+int64(limit))
	for next < b.End && !utf8.RuneStart(b.bytes[next%int64(len(b.bytes))]) {
		next--
	}
	chunk := make([]byte, int(next-cursor))
	offset := int(cursor % int64(len(b.bytes)))
	copied := copy(chunk, b.bytes[offset:])
	copy(chunk[copied:], b.bytes)
	return string(chunk), next, truncated
}
