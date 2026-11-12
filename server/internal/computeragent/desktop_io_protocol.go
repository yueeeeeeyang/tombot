package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type desktopIOResult struct {
	Image   *computer.DesktopImage        `json:"image,omitempty"`
	Chunk   *computer.DesktopImageChunk   `json:"chunk,omitempty"`
	Receipt *computer.DesktopInputReceipt `json:"receipt,omitempty"`
	Error   string                        `json:"error,omitempty"`
}

func (client *Client) CaptureDesktop(ctx context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	if err := request.Validate(); err != nil {
		return computer.DesktopImage{}, err
	}
	var result desktopIOResult
	if err := client.exchange(ctx, "desktop_capture", request, nil, &result); err != nil {
		return computer.DesktopImage{}, err
	}
	if result.Error != "" || result.Image == nil || result.Chunk != nil || result.Receipt != nil || result.Image.Validate(request) != nil {
		return computer.DesktopImage{}, errors.New("电脑代理截图失败或响应无效")
	}
	return *result.Image, nil
}

func (client *Client) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	if err := request.Validate(); err != nil {
		return computer.DesktopImageChunk{}, err
	}
	var result desktopIOResult
	if err := client.exchange(ctx, "desktop_image_read", request, nil, &result); err != nil {
		return computer.DesktopImageChunk{}, err
	}
	if result.Error != "" || result.Chunk == nil || result.Image != nil || result.Receipt != nil || result.Chunk.Validate(request) != nil {
		return computer.DesktopImageChunk{}, errors.New("电脑代理截图读取失败或响应无效")
	}
	return *result.Chunk, nil
}

func (client *Client) InputDesktop(ctx context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	if err := request.Validate(); err != nil {
		return computer.DesktopInputReceipt{}, err
	}
	var result desktopIOResult
	if err := client.exchange(ctx, "desktop_input", request, nil, &result); err != nil {
		return computer.DesktopInputReceipt{}, err
	}
	if result.Error != "" || result.Receipt == nil || result.Image != nil || result.Chunk != nil || result.Receipt.Validate(request) != nil {
		return computer.DesktopInputReceipt{}, errors.New("电脑代理输入结果未确认或响应无效")
	}
	return *result.Receipt, nil
}

func (server *Server) serveDesktopIO(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files int) error {
	var result desktopIOResult
	var failure error
	if files != 0 {
		failure = errors.New("桌面输入输出协议不接受外部描述符")
	} else {
		switch request.Method {
		case "desktop_capture":
			var input computer.DesktopCapture
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var output computer.DesktopImage
				output, failure = server.controller.CaptureDesktop(ctx, input)
				result.Image = &output
			}
		case "desktop_image_read":
			var input computer.DesktopImageRead
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var output computer.DesktopImageChunk
				output, failure = server.controller.ReadDesktopImage(ctx, input)
				result.Chunk = &output
			}
		case "desktop_input":
			var input computer.DesktopInput
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var output computer.DesktopInputReceipt
				output, failure = server.controller.InputDesktop(ctx, input)
				result.Receipt = &output
			}
		default:
			failure = errors.New("桌面输入输出动作无效")
		}
	}
	if failure != nil {
		result = desktopIOResult{Error: "desktop_io_rejected"}
	}
	encoded, err := agentchannel.Encode(result)
	if err != nil {
		return err
	}
	return errors.Join(failure, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
