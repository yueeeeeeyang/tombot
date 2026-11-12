package sharedbrowser

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/chromedp/chromedp"
)

const maximumFrameBytes = 8 << 20
const maximumCommandBytes = 256 << 10

// frameReader 限制单个零结尾 CDP 帧；超限使整条连接失败，不能跳过部分消息继续使用旧状态。
type frameReader struct {
	io.ReadCloser
	size int
}

func (reader *frameReader) Read(buffer []byte) (int, error) {
	count, err := reader.ReadCloser.Read(buffer)
	for _, value := range buffer[:count] {
		if value == 0 {
			reader.size = 0
		} else {
			reader.size++
			if reader.size > maximumFrameBytes {
				return 0, ErrUnavailable
			}
		}
	}
	return count, err
}

type frameWriter struct{ io.WriteCloser }

func (writer frameWriter) Write(buffer []byte) (int, error) {
	if len(buffer) > maximumCommandBytes {
		return 0, ErrInvalid
	}
	count, err := writer.WriteCloser.Write(buffer)
	if count != len(buffer) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return count, err
}

// pipeAllocator 仅连接已由受信监督器启动的 Chrome；不启动宿主程序或建立 TCP 调试监听。
type pipeAllocator struct {
	connection chromedp.Transport
	mu         sync.Mutex
	browser    *chromedp.Browser
}

func (*pipeAllocator) Attaches() bool { return true }
func (allocator *pipeAllocator) Allocate(ctx context.Context, options ...chromedp.BrowserOption) (*chromedp.Browser, error) {
	allocator.mu.Lock()
	defer allocator.mu.Unlock()
	if allocator.browser != nil {
		return nil, ErrConflict
	}
	browser, err := chromedp.NewBrowserTransport(ctx, allocator.connection, options...)
	if err == nil {
		allocator.browser = browser
	}
	return browser, err
}
func (allocator *pipeAllocator) Wait() {
	allocator.mu.Lock()
	browser := allocator.browser
	allocator.mu.Unlock()
	if browser != nil {
		<-browser.LostConnection
	}
}
