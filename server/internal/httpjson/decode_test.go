package httpjson

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type brokenBody struct{ readError, closeError error }

func (body brokenBody) Read([]byte) (int, error) { return 0, body.readError }
func (body brokenBody) Close() error             { return body.closeError }

func TestDecodeInputBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		limit                   int64
		status                  int
	}{
		{"有效对象", `{"name":"机器人"}`, "application/json; charset=utf-8", 64, 0},
		{"未知字段", `{"role":"admin"}`, "application/json", 64, 400},
		{"多个对象", `{} {}`, "application/json", 64, 400},
		{"空值", `null`, "application/json", 64, 400},
		{"数组", `[]`, "application/json", 64, 400},
		{"无效 UTF-8", "{\"name\":\"\xff\"}", "application/json", 64, 400},
		{"长度边界", `{}`, "application/json", 2, 0},
		{"超长内容", `{} `, "application/json", 2, 400},
		{"非法时限", `{}`, "application/json", 0, 400},
		{"表单内容", `name=value`, "application/x-www-form-urlencoded", 64, 415},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "https://example.test", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			var target struct {
				Name string `json:"name"`
			}
			failure := Decode(request, &target, test.limit)
			if test.status == 0 {
				if failure != nil {
					t.Fatal("有效对象未通过", failure.Code)
				}
				return
			}
			if failure == nil || failure.Status != test.status {
				t.Fatal("未拒绝无效请求")
			}
		})
	}
	for _, body := range []io.ReadCloser{brokenBody{readError: errors.New("测试读取故障")}, brokenBody{readError: io.EOF, closeError: errors.New("测试关闭故障")}} {
		request := httptest.NewRequest("POST", "https://example.test", nil)
		request.Header.Set("Content-Type", "application/json")
		request.Body = body
		if Decode(request, new(any), 64) == nil {
			t.Fatal("读取或关闭错误被忽略")
		}
	}
}
