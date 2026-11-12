// Package httpjson 统一业务接口的有界 JSON 输入校验，不记录请求正文。
package httpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"
)

// Failure 仅包含固定协议错误，可直接返回客户端，不携带原始解码异常。
type Failure struct {
	Status  int
	Code    string
	Message string
}

// Decode 要求单个 UTF-8 JSON 对象并拒绝未知字段；读取及关闭失败均明确返回错误。
func Decode(r *http.Request, target any, maxBytes int64) *Failure {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &Failure{415, "JSON_REQUIRED", "请使用 JSON 请求"}
	}
	if maxBytes < 1 || maxBytes > 1<<20 || r.Body == nil {
		return &Failure{400, "BODY_INVALID", "请求内容限制无效"}
	}
	encoded, readErr := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	closeErr := r.Body.Close()
	trimmed := bytes.TrimSpace(encoded)
	if readErr != nil || closeErr != nil || int64(len(encoded)) > maxBytes || !utf8.Valid(encoded) || len(trimmed) == 0 || trimmed[0] != '{' {
		return &Failure{400, "BODY_INVALID", "请求内容无效或超过限制"}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return &Failure{400, "BODY_INVALID", "请求内容格式无效"}
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return &Failure{400, "BODY_INVALID", "请求只能包含一个 JSON 对象"}
	}
	return nil
}
