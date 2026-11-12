package sharedbrowser

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chromedp/cdproto"
)

func TestScreenshotActivationWaitIsReadOnlyBoundedAndRevocable(t *testing.T) {
	for _, mode := range []string{"激活后成功", "其他故障", "耗尽", "取消", "撤销"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls, checks := 0, 0
				began := time.Now()
				pixels, err := captureActivePage(ctx, func() error {
					checks++
					if mode == "撤销" && checks == 2 {
						return ErrDenied
					}
					return nil
				}, func() ([]byte, error) {
					calls++
					if mode == "取消" {
						cancel()
					}
					if mode == "其他故障" {
						return nil, &cdproto.Error{Code: -32000, Message: "Target closed"}
					}
					if mode == "激活后成功" && calls == 3 {
						return []byte("pixels"), nil
					}
					return nil, &cdproto.Error{Code: -32000, Message: "Not attached to an active page"}
				})
				switch mode {
				case "激活后成功":
					if err != nil || string(pixels) != "pixels" || calls != 3 || time.Since(began) != 75*time.Millisecond {
						t.Fatal(calls, time.Since(began), err)
					}
				case "耗尽":
					if err == nil || calls != 5 || time.Since(began) != 375*time.Millisecond {
						t.Fatal(calls, time.Since(began), err)
					}
				case "其他故障":
					if err == nil || calls != 1 {
						t.Fatal(calls, err)
					}
				case "取消":
					if !errors.Is(err, context.Canceled) || calls != 1 {
						t.Fatal(calls, err)
					}
				case "撤销":
					if !errors.Is(err, ErrDenied) || calls != 1 {
						t.Fatal(calls, err)
					}
				}
			})
		})
	}
}
