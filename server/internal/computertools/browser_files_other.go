//go:build !linux

package computertools

import (
	"context"
	"io.yueyang/tom/internal/sharedbrowser"
)

func readBrowserFiles(context.Context, []string) ([]sharedbrowser.UploadFile, error) {
	return nil, sharedbrowser.ErrUnavailable
}
