package sharedbrowser

import (
	"bytes"
	"image/png"
)

// validateDesktopImage 仅接受所属固定屏幕的完整 PNG，不能以 CDP 页面截图代替实际桌面。
func validateDesktopImage(pixels []byte, width, height int) error {
	if len(pixels) == 0 || len(pixels) > 4<<20 {
		return ErrUnavailable
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(pixels))
	if err != nil || configuration.Width != width || configuration.Height != height {
		return ErrUnavailable
	}
	if _, err := png.Decode(bytes.NewReader(pixels)); err != nil {
		return ErrUnavailable
	}
	return nil
}
