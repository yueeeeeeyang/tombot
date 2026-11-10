package provision

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// 依赖检查和真实渲染使用同一份字体标准，避免已有其它中文字体时跳过修复。
func supportsChineseFont(family string) bool {
	return strings.Contains(family, "PingFang") || strings.Contains(family, "Source Han") ||
		strings.Contains(family, "Noto") && (strings.Contains(family, "CJK") || strings.Contains(family, " Sans SC") || strings.Contains(family, " Serif SC"))
}

// CheckBrowser 验证真实截图与中文节点实际使用的字形；避免仅凭安装列表误报成功。
func CheckBrowser(parent context.Context, executable string) error {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options, chromedp.ExecPath(executable))
	if os.Geteuid() == 0 {
		options = append(options, chromedp.NoSandbox)
	}
	allocator, stop := chromedp.NewExecAllocator(ctx, options...)
	defer stop()
	browser, closeBrowser := chromedp.NewContext(allocator)
	defer closeBrowser()
	var screenshot []byte
	var fontNames []string
	err := chromedp.Do(browser,
		chromedp.Navigate("data:text/html;base64,"+base64.StdEncoding.EncodeToString([]byte(`<meta charset="utf-8"><style>body{background:white}#cjk{font:48px "Noto Sans CJK SC","PingFang SC",sans-serif;color:black}</style><span id="cjk">山巅中文测试</span>`))),
		chromedp.WaitVisible(chromedp.CSS("#cjk")),
		chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
			if _, err := cdp.Call(ctx, target, dom.Enable, dom.EnableParams{}); err != nil {
				return fmt.Errorf("启用 DOM: %w", err)
			}
			if _, err := cdp.Call(ctx, target, css.Enable, cdp.Empty{}); err != nil {
				return fmt.Errorf("启用样式: %w", err)
			}
			document, err := cdp.Call(ctx, target, dom.GetDocument, dom.GetDocumentParams{})
			if err != nil {
				return fmt.Errorf("读取文档: %w", err)
			}
			node, err := cdp.Call(ctx, target, dom.QuerySelector, dom.QuerySelectorParams{NodeID: document.Root.NodeID, Selector: "#cjk"})
			if err != nil {
				return fmt.Errorf("查找中文节点: %w", err)
			}
			fonts, err := cdp.Call(ctx, target, css.GetPlatformFontsForNode, css.GetPlatformFontsForNodeParams{NodeID: node.NodeID})
			if err != nil {
				return fmt.Errorf("查询字形: %w", err)
			}
			for _, font := range fonts.Fonts {
				if font.GlyphCount > 0 {
					fontNames = append(fontNames, font.FamilyName)
				}
			}
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("浏览器渲染自检失败: %w", err)
	}
	screenshot, err = chromedp.Run(browser, chromedp.CaptureScreenshot())
	if err != nil {
		return fmt.Errorf("浏览器截图失败: %w", err)
	}
	hasCJK := false
	for _, name := range fontNames {
		if supportsChineseFont(name) {
			hasCJK = true
		}
	}
	if !hasCJK {
		return fmt.Errorf("中文节点未使用受支持的中文字形，请安装 Noto CJK 字体")
	}
	picture, err := png.Decode(bytes.NewReader(screenshot))
	if err != nil {
		return err
	}
	bounds := picture.Bounds()
	if bounds.Dx() < 20 || bounds.Dy() < 20 {
		return fmt.Errorf("中文截图尺寸异常")
	}
	dark := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := picture.At(x, y).RGBA()
			if r < 32768 && g < 32768 && b < 32768 {
				dark++
			}
		}
	}
	if dark < 100 {
		return fmt.Errorf("中文截图为空白")
	}
	return nil
}
