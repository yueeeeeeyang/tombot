package computeragent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"time"

	"io.yueyang/tom/internal/computer"
)

const desktopIOTimeout = 5 * time.Second

// captureDesktopFrame 只接收固定尺寸、完整覆盖的原始像素；未知编码、超量更新和残帧均失败。
func captureDesktopFrame(ctx context.Context, connection net.Conn) (encoded []byte, failure error) {
	failure = withDesktopConnection(ctx, connection, desktopIOTimeout, func(ctx context.Context) error {
		if err := desktopHandshake(connection); err != nil {
			return err
		}
		// 使用小端 RGBA 字节顺序，避免依赖 X 服务端本机字节序或默认颜色位移。
		configuration := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0,
			2, 0, 0, 1, 0, 0, 0, 0,
			3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(configuration[34:36], computer.DesktopWidth)
		binary.BigEndian.PutUint16(configuration[36:38], computer.DesktopHeight)
		if count, err := connection.Write(configuration); err != nil || count != len(configuration) {
			return errors.New("发送桌面全帧请求失败")
		}
		frame, err := readDesktopFrame(ctx, connection)
		if err != nil {
			return err
		}
		writer := &desktopImageWriter{ctx: ctx}
		encoder := png.Encoder{CompressionLevel: png.BestSpeed}
		if err = encoder.Encode(writer, frame); err != nil {
			return errors.New("编码桌面画面失败")
		}
		encoded = writer.Bytes()
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return encoded, nil
}

func readDesktopFrame(ctx context.Context, reader io.Reader) (*image.RGBA, error) {
	frame := image.NewRGBA(image.Rect(0, 0, computer.DesktopWidth, computer.DesktopHeight))
	covered := make([]bool, computer.DesktopWidth*computer.DesktopHeight)
	remaining := len(covered)
	transferred, rectangles := 0, 0
	for messages := 0; messages < 32; messages++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var kind [1]byte
		if _, err := io.ReadFull(reader, kind[:]); err != nil {
			return nil, errors.New("读取桌面画面消息失败")
		}
		switch kind[0] {
		case 2: // 铃声不携带画面，仍占用消息预算。
			continue
		case 3:
			var header [7]byte
			if _, err := io.ReadFull(reader, header[:]); err != nil {
				return nil, errors.New("读取桌面剪贴板消息失败")
			}
			length := binary.BigEndian.Uint32(header[3:])
			if length > 64<<10 {
				return nil, errors.New("桌面剪贴板消息超限")
			}
			// 不向截图调用者返回剪贴板；消耗已声明的有界内容以保持协议对齐。
			if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
				return nil, errors.New("读取桌面剪贴板内容失败")
			}
			continue
		case 0:
		default:
			return nil, errors.New("桌面返回未授权的消息类型")
		}
		var header [3]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return nil, errors.New("读取桌面更新头失败")
		}
		count := int(binary.BigEndian.Uint16(header[1:]))
		rectangles += count
		if count == 0 || rectangles > 4096 {
			return nil, errors.New("桌面画面分块数量无效")
		}
		for range count {
			var block [12]byte
			if _, err := io.ReadFull(reader, block[:]); err != nil {
				return nil, errors.New("读取桌面画面分块头失败")
			}
			x, y := int(binary.BigEndian.Uint16(block[:2])), int(binary.BigEndian.Uint16(block[2:4]))
			width, height := int(binary.BigEndian.Uint16(block[4:6])), int(binary.BigEndian.Uint16(block[6:8]))
			if width == 0 || height == 0 || x+width > computer.DesktopWidth || y+height > computer.DesktopHeight || binary.BigEndian.Uint32(block[8:]) != 0 {
				return nil, errors.New("桌面画面分块越界或编码无效")
			}
			transferred += width * height * 4
			if transferred > computer.DesktopWidth*computer.DesktopHeight*8 {
				return nil, errors.New("桌面画面传输超限")
			}
			for row := y; row < y+height; row++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				start := frame.PixOffset(x, row)
				pixels := frame.Pix[start : start+width*4]
				if _, err := io.ReadFull(reader, pixels); err != nil {
					return nil, errors.New("读取桌面画面像素失败")
				}
				for column := range width {
					pixels[column*4+3] = 255
					index := row*computer.DesktopWidth + x + column
					if !covered[index] {
						covered[index] = true
						remaining--
					}
				}
			}
		}
		if remaining == 0 {
			return frame, nil
		}
	}
	return nil, errors.New("桌面画面未完整覆盖或消息超限")
}

type desktopImageWriter struct {
	bytes.Buffer
	ctx context.Context
}

func (writer *desktopImageWriter) Write(block []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	if writer.Len()+len(block) > computer.MaximumDesktopImageBytes {
		return 0, errors.New("桌面图像编码大小超限")
	}
	return writer.Buffer.Write(block)
}

// sendDesktopInput 不重试写入；任何中断都必须由上层保留 uncertain 回执。
func sendDesktopInput(ctx context.Context, connection net.Conn, request computer.DesktopInput) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return withDesktopConnection(ctx, connection, desktopIOTimeout, func(ctx context.Context) error {
		if err := desktopHandshake(connection); err != nil {
			return err
		}
		for _, event := range request.Events {
			if err := ctx.Err(); err != nil {
				return err
			}
			var message []byte
			if event.Kind == "key" {
				message = make([]byte, 8)
				message[0] = 4
				if event.Down {
					message[1] = 1
				}
				binary.BigEndian.PutUint32(message[4:], event.Key)
			} else {
				message = make([]byte, 6)
				message[0], message[1] = 5, event.Buttons
				binary.BigEndian.PutUint16(message[2:4], uint16(event.X))
				binary.BigEndian.PutUint16(message[4:], uint16(event.Y))
			}
			if count, err := connection.Write(message); err != nil || count != len(message) {
				return errors.New("桌面输入写入未确认")
			}
		}
		return nil
	})
}
