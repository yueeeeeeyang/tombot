package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestServiceHost(t *testing.T) {
	for _, fixture := range []struct {
		platform string
		uid      int
		valid    bool
	}{
		{"linux", 20001, true}, {"linux", 0, false}, {"linux", -1, false}, {"darwin", 501, false}, {"windows", 20001, false},
	} {
		if err := validateHost(fixture.platform, fixture.uid); (err == nil) != fixture.valid {
			t.Errorf("%s/%d: %v", fixture.platform, fixture.uid, err)
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
