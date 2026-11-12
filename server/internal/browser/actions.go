package browser

import (
	"context"
	"errors"
	"math"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"io.yueyang/tom/internal/domain"
)

const MaxInputBytes = 16 << 10
const MaxScreenshotBytes = 8 << 20

// Click 先滚动再按真实坐标点击；仅在鼠标尚未发出时回退 focus + Enter，避免重复提交。
func (m *Manager) Click(ctx context.Context, selected Selection, ref string) error {
	return m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		return m.click(ctx, tab, ref)
	})
}

func (m *Manager) click(ctx context.Context, tab *entry, ref string) error {
	node, err := findReference(tab, ref)
	if err != nil {
		return err
	}
	tab.refs = nil
	tab.Revision++
	defer m.notify()
	target := chromedp.FromContext(tab.ctx).Target
	_, scrollErr := cdp.Call(ctx, target, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{BackendNodeID: node.node})
	quads, quadErr := cdp.Call(ctx, target, dom.GetContentQuads, dom.GetContentQuadsParams{BackendNodeID: node.node})
	if scrollErr == nil && quadErr == nil {
		for _, quad := range quads.Quads {
			if len(quad) != 8 {
				continue
			}
			x, y := 0.0, 0.0
			for point := 0; point < 8; point += 2 {
				x += quad[point] / 4
				y += quad[point+1] / 4
			}
			if x >= 0 && y >= 0 && x < ViewportWidth && y < ViewportHeight {
				if err := hitTarget(ctx, target, node.node, x, y); err != nil {
					return err
				}
				return chromedp.Do(ctx, chromedp.MouseClickXY(x, y))
			}
		}
	}
	if _, err = cdp.Call(ctx, target, dom.Focus, dom.FocusParams{BackendNodeID: node.node}); err != nil {
		return errors.Join(domain.NewError(409, "BROWSER_REF_STALE", "The element is no longer interactable. Take a new snapshot."), err)
	}
	return chromedp.Do(ctx, chromedp.KeyEvent(kb.Enter))
}

// hitTarget 验证落点仍是目标或其后代，防止遮罩层在快照后出现导致误点其他控件。
func hitTarget(ctx context.Context, target *chromedp.Target, node cdp.BackendNodeID, x, y float64) (err error) {
	hit, err := cdp.Call(ctx, target, dom.GetNodeForLocation, dom.GetNodeForLocationParams{X: int64(x), Y: int64(y)})
	if err != nil {
		return err
	}
	if hit.BackendNodeID == node {
		return nil
	}
	const group = "tom-hit-test"
	defer func() {
		_, releaseErr := cdp.Call(ctx, target, runtime.ReleaseObjectGroup, runtime.ReleaseObjectGroupParams{ObjectGroup: group})
		err = errors.Join(err, releaseErr)
	}()
	expected, err := cdp.Call(ctx, target, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: node, ObjectGroup: group})
	if err != nil {
		return err
	}
	actual, err := cdp.Call(ctx, target, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: hit.BackendNodeID, ObjectGroup: group})
	if err != nil {
		return err
	}
	if expected.Object == nil || actual.Object == nil {
		return domain.NewError(409, "BROWSER_REF_STALE", "The target disappeared. Take a new snapshot.")
	}
	byValue := true
	result, err := cdp.Call(ctx, target, runtime.CallFunctionOn, runtime.CallFunctionOnParams{ObjectID: expected.Object.ObjectID, FunctionDeclaration: "function(other) { return this.isConnected && this.contains(other); }", Arguments: []*runtime.CallArgument{{ObjectID: actual.Object.ObjectID}}, ReturnByValue: &byValue})
	if err != nil {
		return err
	}
	if result.ExceptionDetails != nil || result.Result == nil || string(result.Result.Value) != "true" {
		return domain.NewError(409, "BROWSER_TARGET_OBSCURED", "Another element covers the target. Inspect a fresh snapshot or screenshot before interacting.")
	}
	return nil
}

// Type 使用协议文本字段而非拼接脚本；清空通过浏览器编辑命令完成，适用于中文输入。
func (m *Manager) Type(ctx context.Context, selected Selection, ref, text string, clear bool) error {
	if err := ValidateText(text); err != nil {
		return err
	}
	return m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		node, err := findReference(tab, ref)
		if err != nil {
			return err
		}
		if node.role != "textbox" && node.role != "searchbox" && node.role != "combobox" && node.role != "spinbutton" {
			return invalid("The reference is not a text input.")
		}
		tab.refs = nil
		tab.Revision++
		defer m.notify()
		target := chromedp.FromContext(tab.ctx).Target
		if _, err = cdp.Call(ctx, target, dom.Focus, dom.FocusParams{BackendNodeID: node.node}); err != nil {
			return err
		}
		if clear {
			if _, err = cdp.Call(ctx, target, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyDown, Key: "a", Code: "KeyA", Commands: []string{"selectAll"}}); err != nil {
				return err
			}
			if _, err = cdp.Call(ctx, target, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyUp, Key: "a", Code: "KeyA"}); err != nil {
				return err
			}
			if err = chromedp.Do(ctx, chromedp.KeyEvent(kb.Backspace)); err != nil {
				return err
			}
		}
		_, err = cdp.Call(ctx, target, input.InsertText, input.InsertTextParams{Text: text})
		return err
	})
}

// ValidateText 在审批前校验 UTF-8 与字节上限；执行入口复用同一规则，非法输入返回公开参数错误。
func ValidateText(text string) error {
	if len(text) > MaxInputBytes || !utf8.ValidString(text) {
		return invalid("Text must be valid UTF-8 and no larger than 16 KiB.")
	}
	return nil
}

// ValidateKey 校验模型和用户提交的按键名称，拒绝允许列表之外的键。
func ValidateKey(name string) error { _, err := keyValue(name); return err }

func keyValue(name string) (string, error) {
	switch name {
	case "Enter":
		return kb.Enter, nil
	case "Tab":
		return kb.Tab, nil
	case "Escape":
		return kb.Escape, nil
	case "ArrowUp":
		return kb.ArrowUp, nil
	case "ArrowDown":
		return kb.ArrowDown, nil
	case "ArrowLeft":
		return kb.ArrowLeft, nil
	case "ArrowRight":
		return kb.ArrowRight, nil
	case "Backspace":
		return kb.Backspace, nil
	case "Delete":
		return kb.Delete, nil
	case "Home":
		return kb.Home, nil
	case "End":
		return kb.End, nil
	}
	return "", invalid("Unsupported browser key.")
}
func (m *Manager) Key(ctx context.Context, selected Selection, name string) error {
	key, err := keyValue(name)
	if err != nil {
		return err
	}
	return m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		tab.refs = nil
		tab.Revision++
		defer m.notify()
		return chromedp.Do(ctx, chromedp.KeyEvent(key))
	})
}

func (m *Manager) Scroll(ctx context.Context, selected Selection, x, y float64) error {
	if err := ValidateScroll(x, y); err != nil {
		return err
	}
	return m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		tab.refs = nil
		tab.Revision++
		defer m.notify()
		_, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, input.DispatchMouseEvent, input.DispatchMouseEventParams{Type: input.DispatchMouseEventTypeMouseWheel, X: ViewportWidth / 2, Y: ViewportHeight / 2, DeltaX: x, DeltaY: y})
		return err
	})
}

// ValidateScroll 要求有限且有界的横纵滚动距离，避免异常数值进入协议。
func ValidateScroll(x, y float64) error {
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) || math.Abs(x) > 4096 || math.Abs(y) > 4096 {
		return invalid("Scroll deltas must be finite and within 4096 pixels.")
	}
	return nil
}

// Screenshot 只截取固定视口，限制输出大小；quality 为 1–100，返回 JPEG 原始字节。
func (m *Manager) Screenshot(ctx context.Context, selected Selection, quality int) ([]byte, error) {
	frame, err := m.CaptureFrame(ctx, selected, quality)
	return frame.JPEG, err
}

// ScreenFrame 的图像和页面版本在同一命令锁中获取，防止用户对导航前的画面输入。
type ScreenFrame struct {
	Tab  Tab
	JPEG []byte
}

// CaptureFrame 返回有界 JPEG 与对应页面元数据；读取期间导航则拒绝返回不一致画面。
func (m *Manager) CaptureFrame(ctx context.Context, selected Selection, quality int) (ScreenFrame, error) {
	if quality < 1 || quality > 100 {
		return ScreenFrame{}, invalid("Screenshot quality must be between 1 and 100.")
	}
	var frame ScreenFrame
	err := m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		epoch := tab.epoch.Load()
		if err := m.metadata(ctx, tab); err != nil {
			return err
		}
		qualityValue := int64(quality)
		beyond := false
		result, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, page.CaptureScreenshot, page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatJpeg, Quality: &qualityValue, CaptureBeyondViewport: &beyond})
		if err != nil {
			return err
		}
		if len(result.Data) == 0 || len(result.Data) > MaxScreenshotBytes {
			return domain.NewError(502, "BROWSER_SCREENSHOT_INVALID", "The browser returned an invalid screenshot size.")
		}
		if tab.epoch.Load() != epoch {
			return domain.NewError(409, "BROWSER_DOCUMENT_CHANGED", "The document changed while capturing. Request a new frame.")
		}
		frame = ScreenFrame{Tab: tab.Tab, JPEG: result.Data}
		return nil
	})
	return frame, err
}

// ValidatePoint 只允许固定视口内的有限坐标，避免缩放或异常数值点击画面外区域。
func ValidatePoint(x, y float64) error {
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) || x < 0 || y < 0 || x >= ViewportWidth || y >= ViewportHeight {
		return invalid("Click coordinates must be finite and inside the viewport.")
	}
	return nil
}

// ClickPoint 用于用户接管后的画面坐标点击；无失败回退，避免重复副作用。
func (m *Manager) ClickPoint(ctx context.Context, selected Selection, x, y float64) error {
	if err := ValidatePoint(x, y); err != nil {
		return err
	}
	return m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		tab.refs = nil
		tab.Revision++
		defer m.notify()
		return chromedp.Do(ctx, chromedp.MouseClickXY(x, y))
	})
}

// InputText 向当前焦点插入用户文本，不拼接脚本、不自动清空，不重试可能已送达的输入。
func (m *Manager) InputText(ctx context.Context, selected Selection, text string) error {
	if err := ValidateText(text); err != nil {
		return err
	}
	return m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		tab.refs = nil
		tab.Revision++
		defer m.notify()
		_, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, input.InsertText, input.InsertTextParams{Text: text})
		return err
	})
}
