package term

import "unicode/utf8"

// outputBuffer 只保留固定容量的尾部；游标按规范化后的 UTF-8 字节累计，断线客户端可检测缺口。
type outputBuffer struct {
	bytes []byte
	end   int64
}

func newOutputBuffer(capacity int) outputBuffer {
	return outputBuffer{bytes: make([]byte, capacity)}
}

func (b *outputBuffer) append(text []byte) {
	start := b.end
	b.end += int64(len(text))
	if len(text) > len(b.bytes) {
		text = text[len(text)-len(b.bytes):]
		start = b.end - int64(len(text))
	}
	offset := int(start % int64(len(b.bytes)))
	copied := copy(b.bytes[offset:], text)
	copy(b.bytes, text[copied:])
}

func (b *outputBuffer) read(cursor int64, limit int) (text string, next int64, truncated bool) {
	start := max(int64(0), b.end-int64(len(b.bytes)))
	for start < b.end && !utf8.RuneStart(b.bytes[start%int64(len(b.bytes))]) {
		start++
	}
	truncated = cursor < start
	cursor = max(cursor, start)
	// 外部游标必须指向字符边界；中间字节向前跳过并明确要求客户端重置画面。
	for cursor < b.end && !utf8.RuneStart(b.bytes[cursor%int64(len(b.bytes))]) {
		cursor++
		truncated = true
	}
	next = min(b.end, cursor+int64(limit))
	for next < b.end && !utf8.RuneStart(b.bytes[next%int64(len(b.bytes))]) {
		next--
	}
	chunk := make([]byte, int(next-cursor))
	offset := int(cursor % int64(len(b.bytes)))
	copied := copy(chunk, b.bytes[offset:])
	copy(chunk[copied:], b.bytes)
	return string(chunk), next, truncated
}
