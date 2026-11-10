package log

import (
	"context"
	"log/slog"
	"regexp"
)

var devicePattern = regexp.MustCompile(`tomd_[A-Za-z0-9_-]{43}`)
var bearerPattern = regexp.MustCompile(`(?i)Bearer\s+[^\s,;"']+`)

// Handler 在日志出口统一屏蔽机密字段与设备凭据，防止调用方遗漏脱敏。
type Handler struct{ next slog.Handler }

func NewHandler(next slog.Handler) *Handler { return &Handler{next: next} }
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func redactText(value string) string {
	return bearerPattern.ReplaceAllString(devicePattern.ReplaceAllString(value, "[已脱敏]"), "Bearer [已脱敏]")
}
func redact(attribute slog.Attr) slog.Attr {
	if privateField(attribute.Key) {
		return slog.String(attribute.Key, "[已脱敏]")
	}
	attribute.Value = attribute.Value.Resolve()
	switch attribute.Value.Kind() {
	case slog.KindString:
		attribute.Value = slog.StringValue(SafeText(attribute.Value.String()))
	case slog.KindGroup:
		values := attribute.Value.Group()
		copied := make([]slog.Attr, len(values))
		for i, value := range values {
			copied[i] = redact(value)
		}
		attribute.Value = slog.GroupValue(copied...)
	}
	return attribute
}
func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, SafeText(record.Message), record.PC)
	record.Attrs(func(attribute slog.Attr) bool { clean.AddAttrs(redact(attribute)); return true })
	return h.next.Handle(ctx, clean)
}
func (h *Handler) WithAttrs(attributes []slog.Attr) slog.Handler {
	copied := make([]slog.Attr, len(attributes))
	for i, value := range attributes {
		copied[i] = redact(value)
	}
	return NewHandler(h.next.WithAttrs(copied))
}
func (h *Handler) WithGroup(name string) slog.Handler { return NewHandler(h.next.WithGroup(name)) }
