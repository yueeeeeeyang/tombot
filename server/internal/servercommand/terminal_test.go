package servercommand

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestAddressConfirmationAcceptsInputAndRejectsCancellation(t *testing.T) {
	for _, fixture := range []struct {
		name, input, expected string
		cancel                bool
		invalid               bool
	}{
		{"默认地址", "\n", "https://192.0.2.1:9443", false, false},
		{"自定义地址", "https://example.test:9443\n", "https://example.test:9443", false, false},
		{"输入过长", strings.Repeat("a", 513) + "\n", "", false, true},
		{"取消", "", "", true, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := master.Close(); err != nil {
					t.Error(err)
				}
				if err := slave.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if fixture.cancel {
				cancel()
			} else {
				if _, err := master.WriteString(fixture.input); err != nil {
					t.Fatal(err)
				}
			}
			actual, err := readAddress(ctx, slave, "https://192.0.2.1:9443")
			if (err != nil) != fixture.invalid || actual != fixture.expected {
				t.Fatal(actual, err)
			}
		})
	}
}
