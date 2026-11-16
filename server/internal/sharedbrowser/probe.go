package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/browserprobe"
)

type probePage struct {
	id   target.ID
	tab  *chromedp.Target
	stop context.CancelFunc
}

// Probe 在同一共享浏览器中执行真实检查；入口仅供受信代理使用，不接受业务网址或脚本。
// 临时窗口不进入 Bot 窗口目录，回执必须等待测试数据及全部窗口确认清理。
func (service *Service) Probe(ctx context.Context, challenge string) (evidence browserprobe.Evidence, failure error) {
	if _, err := ulid.ParseStrict(challenge); err != nil {
		return evidence, ErrInvalid
	}
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	if err := acquire(bounded, service.probeGate); err != nil {
		return evidence, err
	}
	defer func() { <-service.probeGate }()
	pages := []*probePage{}
	storageTouched := false
	stage := "windows"
	key := "tombot_probe_" + challenge
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), service.options.ShutdownTimeout)
		defer stop()
		var cleanupError error
		if storageTouched {
			cleanupError = probeWait(cleanup, pages[0].tab, `(()=>{localStorage.removeItem('`+key+`');document.cookie='`+key+`=; Max-Age=0; path=/';return localStorage.getItem('`+key+`')===null && !document.cookie.split('; ').some(v=>v.startsWith('`+key+`='))})()`)
		}
		for _, item := range pages {
			cleanupError = errors.Join(cleanupError, service.closeTarget(cleanup, item.id))
			cleanupError = errors.Join(cleanupError, service.waitProbeRemoved(cleanup, item.id))
			if item.stop != nil {
				item.stop()
			}
		}
		if cleanupError != nil {
			service.fail()
			stage = "cleanup"
		}
		failure = errors.Join(failure, cleanupError, bounded.Err())
		if failure != nil {
			evidence = browserprobe.Evidence{}
			service.options.Logger.WarnContext(ctx, "浏览器能力检查未通过", "user_id", service.options.UserID, "computer_id", service.options.ComputerID, "challenge_id", challenge, "stage", stage)
			failure = &browserprobe.Failure{Code: "browser_probe_" + stage + "_failed"}
		} else {
			evidence.Cleanup = true
		}
	}()
	open := func() (*probePage, error) {
		if err := acquire(bounded, service.creation); err != nil {
			return nil, err
		}
		defer func() { <-service.creation }()
		positive := true
		created, err := cdp.Call(bounded, service.browser, target.CreateTarget, target.CreateTargetParams{URL: "about:blank", NewWindow: &positive, Background: &positive, Width: 640, Height: 480})
		if err != nil || created.TargetID == "" {
			service.fail()
			return nil, ErrUnknown
		}
		item := &probePage{id: created.TargetID}
		pages = append(pages, item)
		service.mu.Lock()
		service.probeTargets[item.id] = true
		service.mu.Unlock()
		pageContext, stop := chromedp.NewContext(service.root, chromedp.WithTargetID(item.id), chromedp.WithDetachOnCancel())
		item.stop = stop
		interrupt := context.AfterFunc(bounded, stop)
		err = chromedp.Do(pageContext)
		interrupt()
		if err != nil {
			return nil, err
		}
		item.tab = chromedp.FromContext(pageContext).Target
		_, err = cdp.Call(bounded, item.tab, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{Width: 640, Height: 480, DeviceScaleFactor: 1})
		return item, err
	}
	first, err := open()
	if err != nil {
		return evidence, err
	}
	url := browserprobe.Origin + "/" + challenge
	stage = "proxy"
	if err = probeNavigate(bounded, first.tab, url); err != nil {
		return evidence, err
	}
	if err = probeWait(bounded, first.tab, `document.body?.dataset.challenge==='`+challenge+`'`); err != nil {
		return evidence, err
	}
	evidence.ChallengeID, evidence.Proxy = challenge, true
	stage = "rendering"
	positive := true
	capture, err := cdp.Call(bounded, first.tab, page.CaptureScreenshot, page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng, FromSurface: &positive})
	if err != nil {
		return evidence, err
	}
	bitmap, err := png.Decode(bytes.NewReader(capture.Data))
	if err != nil || bitmap.Bounds().Dx() != 640 || bitmap.Bounds().Dy() != 480 {
		return evidence, errors.New("浏览器探针渲染尺寸不符")
	}
	r, g, b, _ := bitmap.At(320, 240).RGBA()
	if r>>8 != 23 || g>>8 != 137 || b>>8 != 211 {
		return evidence, errors.New("浏览器探针渲染内容不符")
	}
	evidence.Rendering = true
	stage = "input"
	for _, kind := range []input.DispatchMouseEventType{input.DispatchMouseEventTypeMousePressed, input.DispatchMouseEventTypeMouseReleased} {
		buttons := int64(0)
		if kind == input.DispatchMouseEventTypeMousePressed {
			buttons = 1
		}
		if _, err = cdp.Call(bounded, first.tab, input.DispatchMouseEvent, input.DispatchMouseEventParams{Type: kind, X: 50, Y: 40, Button: input.MouseButtonLeft, Buttons: buttons, ClickCount: 1}); err != nil {
			return evidence, err
		}
	}
	if _, err = cdp.Call(bounded, first.tab, input.InsertText, input.InsertTextParams{Text: challenge}); err != nil {
		return evidence, err
	}
	if err = probeWait(bounded, first.tab, `document.body.dataset.input==='`+challenge+`'`); err != nil {
		return evidence, err
	}
	evidence.Input = true
	stage = "storage"
	storageTouched = true
	if err = probeWait(bounded, first.tab, `(()=>{localStorage.setItem('`+key+`','`+challenge+`');document.cookie='`+key+`=`+challenge+`; SameSite=Strict; path=/';return true})()`); err != nil {
		return evidence, err
	}
	second, err := open()
	if err != nil {
		return evidence, err
	}
	if err = probeNavigate(bounded, second.tab, url); err != nil {
		return evidence, err
	}
	if err = probeWait(bounded, second.tab, `document.body?.dataset.challenge==='`+challenge+`' && localStorage.getItem('`+key+`')==='`+challenge+`' && document.cookie.split('; ').includes('`+key+`=`+challenge+`')`); err != nil {
		return evidence, err
	}
	evidence.Windows, evidence.SharedStorage = first.id != second.id, true
	stage = "sandbox"
	sandbox, err := open()
	if err != nil {
		return evidence, err
	}
	if err = probeNavigate(bounded, sandbox.tab, "chrome://sandbox"); err != nil {
		return evidence, err
	}
	if err = probeWait(bounded, sandbox.tab, `document.body && document.body.innerText.includes('sandboxed.')`); err != nil {
		return evidence, err
	}
	var report string
	if err = probeEvaluate(bounded, sandbox.tab, `document.body.innerText`, &report); err != nil {
		return evidence, err
	}
	if !sandboxEnabled(report) {
		return evidence, errors.New("浏览器命名空间或系统调用沙箱未启用")
	}
	evidence.Sandbox = true
	return evidence, nil
}

// waitProbeRemoved 等待归属事件流消费销毁事件，避免迟到的创建事件将已关闭探针误当作无主窗口。
func (service *Service) waitProbeRemoved(ctx context.Context, id target.ID) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		service.mu.Lock()
		known := service.probeTargets[id]
		service.mu.Unlock()
		if !known {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-service.root.Done():
			return ErrUnavailable
		case <-ticker.C:
		}
	}
}

func sandboxEnabled(report string) bool {
	for _, feature := range []string{"PID namespaces", "Network namespaces", "Seccomp-BPF sandbox"} {
		found := false
		for line := range strings.SplitSeq(report, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 && strings.Join(fields[:len(fields)-1], " ") == feature && fields[len(fields)-1] == "Yes" {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func probeNavigate(ctx context.Context, tab *chromedp.Target, url string) error {
	result, err := cdp.Call(ctx, tab, page.Navigate, page.NavigateParams{URL: url})
	if err != nil {
		return err
	}
	if result.ErrorText != "" {
		return fmt.Errorf("浏览器探针页面不可达：%s", result.ErrorText)
	}
	return nil
}

func probeEvaluate(ctx context.Context, tab *chromedp.Target, expression string, result any) error {
	positive := true
	value, err := cdp.Call(ctx, tab, runtime.Evaluate, runtime.EvaluateParams{Expression: expression, ReturnByValue: &positive})
	if err != nil {
		return err
	}
	if value.ExceptionDetails != nil || value.Result == nil {
		return errors.New("浏览器探针页面检查失败")
	}
	return json.Unmarshal(value.Result.Value, result)
}

func probeWait(ctx context.Context, tab *chromedp.Target, expression string) error {
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		var ready bool
		if err := probeEvaluate(ctx, tab, expression, &ready); err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
