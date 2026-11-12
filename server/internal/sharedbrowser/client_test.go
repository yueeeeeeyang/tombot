package sharedbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

type clientTransport func(*http.Request) (*http.Response, error)

func (transport clientTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestLocalClientValidatesRepliesWithoutRetry(t *testing.T) {
	_, _, sessions := testOptions()
	session := sessions[0]
	for _, mode := range []string{"正确", "其他Bot", "重复窗口", "错误版本", "未知状态", "未知结果", "失败", "超限"} {
		t.Run(mode, func(t *testing.T) {
			client, err := NewClient(session)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			client.http.Transport = clientTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "browser.local" || request.URL.Path != "/v2/state" || request.Method != "GET" {
					t.Error("本地请求地址改变")
				}
				window := Window{ID: ids.New(), BotID: session.BotID, DesktopID: session.DesktopID, State: "ready"}
				if mode == "其他Bot" {
					window.BotID = sessions[1].BotID
				}
				if mode == "未知状态" {
					window.State = "success"
				}
				state := State{Version: 2, Width: 960, Height: 640, Windows: []Window{window}}
				if mode == "重复窗口" {
					state.Windows = append(state.Windows, window)
				}
				if mode == "错误版本" {
					state.Version = 1
				}
				encoded, err := json.Marshal(state)
				if err != nil {
					return nil, err
				}
				status := 200
				if mode == "未知结果" {
					status = 409
					encoded = []byte(`{"code":"unknown","message":"未确认"}`)
				}
				if mode == "失败" {
					return nil, errors.New("连接中断")
				}
				if mode == "超限" {
					encoded = []byte(strings.Repeat("x", (8<<20)+1))
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
			})
			_, err = client.State(context.Background())
			if (err == nil) != (mode == "正确") || calls != 1 {
				t.Fatal(mode, calls, err)
			}
			if mode == "未知结果" && !errors.Is(err, ErrUnknown) {
				t.Fatal("丢失未知状态", err)
			}
		})
	}
}
