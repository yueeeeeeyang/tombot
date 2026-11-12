//go:build !linux

package computerprobe

import (
	"context"
	"errors"
)

func inspect(context.Context) (Evidence, error) {
	return Evidence{}, errors.New("实际电脑探针仅支持 Linux")
}
