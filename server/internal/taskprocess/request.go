// Package taskprocess 负责容器内用户程序的受信入组、身份降权和原位执行。
package taskprocess

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

const maximumRequestBytes = 96 << 10

// Request 只能由受信电脑代理产生；身份、资源组与路径不接受业务端覆盖。
// UID 与共享 GID 仅从已授权桌面身份取得，不能在启动请求中另行指定。
type Request struct {
	Version     int                    `json:"version"`
	OperationID string                 `json:"operation_id"`
	Group       processgroup.Reference `json:"group"`
	Role        string                 `json:"role"`
	computer.ProcessCommand
}

// Validate 限定协议、身份、绝对目录及程序参数总量；错误不包含原始用户内容。
func (request Request) Validate() error {
	if request.Version != 2 || request.Group.Validate() != nil {
		return errors.New("任务引导协议或资源组无效")
	}
	if _, err := ulid.ParseStrict(request.OperationID); err != nil {
		return errors.New("任务引导操作标识无效")
	}
	if request.Role != "shell" && request.Role != "browser" {
		return errors.New("任务引导资源类型无效")
	}
	return request.ProcessCommand.Validate()
}

// environment 不继承引导进程环境，避免将平台配置带入用户代码。
func (request Request) environment() ([]string, error) {
	values, err := request.Group.Session.DesktopEnvironment()
	if err != nil {
		return nil, err
	}
	for name, value := range request.Environment {
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result, nil
}

func encode(request Request) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > maximumRequestBytes {
		return nil, errors.New("任务引导内容无法编码或超过限制")
	}
	return encoded, nil
}

func decode(encoded []byte) (Request, error) {
	var request Request
	if len(encoded) == 0 || len(encoded) > maximumRequestBytes || !utf8.Valid(encoded) {
		return request, errors.New("任务引导内容无效或超过限制")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, errors.New("任务引导内容格式无效")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("任务引导内容必须为单个对象")
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	// 受信发送端只使用 Seal 的规范编码；拒绝重复键、大小写别名和未知的非规范表示。
	canonical, err := encode(request)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return Request{}, errors.New("任务引导内容不是受信规范编码")
	}
	return request, nil
}
