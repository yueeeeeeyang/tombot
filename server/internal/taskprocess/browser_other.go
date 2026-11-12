//go:build !linux

package taskprocess

import (
	"errors"
	"os"
)

func SealBrowser(BrowserRequest) (*os.File, error) {
	return nil, errors.New("共享浏览器授权封存仅支持 Linux")
}

func RunBrowser() error { return errors.New("共享浏览器引导仅支持受管 Linux 容器") }
