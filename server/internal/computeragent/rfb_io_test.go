package computeragent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/color"
	"image/png"
	"io"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func testFrameRectangle(x, y, width, height int, pixel [4]byte) []byte {
	encoded := make([]byte, 12+width*height*4)
	binary.BigEndian.PutUint16(encoded[:2], uint16(x))
	binary.BigEndian.PutUint16(encoded[2:4], uint16(y))
	binary.BigEndian.PutUint16(encoded[4:6], uint16(width))
	binary.BigEndian.PutUint16(encoded[6:8], uint16(height))
	for offset := 12; offset < len(encoded); offset += 4 {
		copy(encoded[offset:offset+4], pixel[:])
	}
	return encoded
}

func serveTestCapture(connection net.Conn, pixels [4]byte) (failure error) {
	defer func() { failure = errors.Join(failure, connection.Close()) }()
	if err := writeTestHandshake(connection, desktopHeader()); err != nil {
		return err
	}
	configuration := make([]byte, 38)
	if _, err := io.ReadFull(connection, configuration); err != nil {
		return err
	}
	if !bytes.Equal(configuration, []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0, 2, 0, 0, 1, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 5, 0, 3, 32}) {
		return errors.New("未按约定请求原始全帧像素")
	}
	encoded := append([]byte{0, 0, 0, 1}, testFrameRectangle(0, 0, 1280, 800, pixels)...)
	_, err := connection.Write(encoded)
	return err
}

func TestDesktopCaptureActualProtocol(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveTestCapture(server, [4]byte{42, 96, 201, 0}) }()
	encoded, err := captureDesktopFrame(context.Background(), client)
	if closeError := client.Close(); closeError != nil {
		t.Fatal(closeError)
	}
	if serverError := <-done; err != nil || serverError != nil {
		t.Fatal(err, serverError)
	}
	frame, err := png.Decode(bytes.NewReader(encoded))
	if err != nil || frame.Bounds().Dx() != 1280 || frame.Bounds().Dy() != 800 || color.RGBAModel.Convert(frame.At(1279, 799)) != (color.RGBA{42, 96, 201, 255}) {
		t.Fatal("实际 PNG 尺寸、颜色或不透明度不符", err)
	}
}

func TestDesktopFrameValidation(t *testing.T) {
	top := testFrameRectangle(0, 0, 1280, 400, [4]byte{1, 2, 3, 0})
	bottom := testFrameRectangle(0, 400, 1280, 400, [4]byte{9, 8, 7, 0})
	valid := append(append([]byte{2, 0, 0, 0, 2}, top...), bottom...)
	frame, err := readDesktopFrame(context.Background(), bytes.NewReader(valid))
	if err != nil || frame.RGBAAt(0, 0).R != 1 || frame.RGBAAt(0, 799).R != 9 {
		t.Fatal("多分块完整画面解析失败", err)
	}
	for name, encoded := range map[string][]byte{
		"缺失半帧":  append([]byte{0, 0, 0, 1}, top...),
		"重叠伪完整": append(append([]byte{0, 0, 0, 2}, top...), top...),
		"越界":    append([]byte{0, 0, 0, 1}, testFrameRectangle(1280, 0, 1, 1, [4]byte{})...),
		"空分块":   {0, 0, 0, 0}, "过多分块": {0, 0, 16, 1}, "未知消息": {255}, "消息洪流": bytes.Repeat([]byte{2}, 32),
		"超长剪贴板": {3, 0, 0, 0, 0, 1, 0, 1},
		"非原始编码": append([]byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 1}, 0),
	} {
		t.Run(name, func(t *testing.T) {
			if frame, err := readDesktopFrame(context.Background(), bytes.NewReader(encoded)); err == nil || frame != nil {
				t.Fatal("接受残缺或无效画面")
			}
		})
	}
	writer := &desktopImageWriter{ctx: context.Background()}
	if _, err := writer.Write(make([]byte, computer.MaximumDesktopImageBytes+1)); err == nil || writer.Len() != 0 {
		t.Fatal("编码未限制输出大小")
	}
}

func TestDesktopInputWireAndCancellation(t *testing.T) {
	session := newFixture(t).command.Session
	request := computer.DesktopInput{Session: session, Sequence: 1, Events: []computer.DesktopEvent{{Kind: "pointer", X: 1279, Y: 799, Buttons: 1}, {Kind: "pointer", X: 1279, Y: 799}, {Kind: "key", Key: 0x01004e2d, Down: true}, {Kind: "key", Key: 0x01004e2d}}}
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		failure := writeTestHandshake(server, desktopHeader())
		if failure == nil {
			encoded := make([]byte, 28)
			_, failure = io.ReadFull(server, encoded)
			if failure == nil && !bytes.Equal(encoded, []byte{5, 1, 4, 255, 3, 31, 5, 0, 4, 255, 3, 31, 4, 1, 0, 0, 1, 0, 78, 45, 4, 0, 0, 0, 1, 0, 78, 45}) {
				failure = errors.New("鼠标或中文按键报文不符")
			}
		}
		done <- errors.Join(failure, server.Close())
	}()
	err := sendDesktopInput(context.Background(), client, request)
	if closeError := client.Close(); closeError != nil {
		t.Fatal(closeError)
	}
	if serverError := <-done; err != nil || serverError != nil {
		t.Fatal(err, serverError)
	}
	client, server = net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, err := captureDesktopFrame(ctx, client); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消被当成完整截图")
		}
	case <-time.After(time.Second):
		t.Fatal("截图取消未唤醒阻塞连接")
	}
	if err := errors.Join(client.Close(), server.Close()); err != nil {
		t.Fatal(err)
	}
}
