package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestServiceHost(t *testing.T) {
	// 服务端不再限制平台，只要求专用非 root 身份。
	for _, fixture := range []struct {
		uid   int
		valid bool
	}{
		{20001, true}, {501, true}, {0, false}, {-1, false},
	} {
		if err := validateHost(fixture.uid); (err == nil) != fixture.valid {
			t.Errorf("%d: %v", fixture.uid, err)
		}
	}
}

func TestLegacyCommandsRejected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, command := range []string{"doctor", "backup", "restore", "unknown"} {
		if err := run(t.Context(), logger, []string{command}, ""); err == nil || !strings.Contains(err.Error(), "不接受子命令") {
			t.Fatalf("旧命令 %s 未明确拒绝: %v", command, err)
		}
	}
}
