//go:build !linux

package main

import (
	"errors"
	"io"
)

func standardStream() (io.ReadWriteCloser, error) {
	return nil, errors.New("Bot Agent 只支持绑定的 Linux 电脑容器")
}
