package sharedbrowser

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestNativeScreenshotRequiresCompleteOwnedScreen(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1280, 800))); err != nil {
		t.Fatal(err)
	}
	if err := validateDesktopImage(encoded.Bytes(), 1280, 800); err != nil {
		t.Fatal(err)
	}
	for _, pixels := range [][]byte{nil, []byte("<svg/>"), encoded.Bytes()[:33], bytes.Repeat([]byte{0}, (4<<20)+1)} {
		if err := validateDesktopImage(pixels, 1280, 800); err == nil {
			t.Fatal("无效或超大原生画面被接受")
		}
	}
	if err := validateDesktopImage(encoded.Bytes(), 960, 640); err == nil {
		t.Fatal("其他屏幕尺寸被接受")
	}
}
