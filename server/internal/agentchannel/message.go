// Package agentchannel 为节点与用户电脑代理提供有界、带描述符的本机控制通道。
package agentchannel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

const MaximumBytes = 128 << 10
const MaximumFiles = 4

// Message 仅承载一个有界控制动作；文件描述符数量与帧一起校验，业务字段另由类型协议验证。
type Message struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Files   int             `json:"files"`
	Body    json.RawMessage `json:"body"`
}

func (message Message) validate() error {
	if message.Version != 1 || message.Files < 0 || message.Files > MaximumFiles {
		return errors.New("电脑代理控制帧版本或描述符数量无效")
	}
	if _, err := ulid.ParseStrict(message.ID); err != nil {
		return errors.New("电脑代理控制请求标识无效")
	}
	switch message.Method {
	case "open", "renew", "close", "start", "read", "write", "resize", "desktop_start", "desktop_status", "desktop_capture", "desktop_image_read", "desktop_input", "agent_start", "agent_write", "agent_read", "browser_start", "browser_status", "browser_close", "probe", "health", "node_control", "result":
	default:
		return errors.New("电脑代理控制动作无效")
	}
	if len(message.Body) == 0 || len(message.Body) > MaximumBytes || !utf8.Valid(message.Body) || !json.Valid(message.Body) {
		return errors.New("电脑代理控制内容无效或超过限制")
	}
	return nil
}

// Encode 只使用固定结构的规范 JSON；不把错误中的原始命令或内容带入日志。
func Encode(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaximumBytes {
		return nil, errors.New("电脑代理控制内容无法编码或超过限制")
	}
	return encoded, nil
}

// Decode 拒绝未知字段、重复键、大小写别名及多份内容；发送方必须使用 Encode。
func Decode(encoded []byte, target any) error {
	if len(encoded) == 0 || len(encoded) > MaximumBytes || !utf8.Valid(encoded) {
		return errors.New("电脑代理控制内容大小或编码无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("电脑代理控制内容格式无效")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("电脑代理控制内容必须为单个对象")
	}
	canonical, err := Encode(target)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return errors.New("电脑代理控制内容不是规范编码")
	}
	return nil
}

// CloseFiles 关闭一次接收所得的全部描述符；调用方必须将错误纳入本次控制结果。
func CloseFiles(files []*os.File) error {
	var result error
	for _, file := range files {
		if file != nil && file.Close() != nil {
			result = errors.Join(result, errors.New("关闭电脑代理控制描述符失败"))
		}
	}
	return result
}
