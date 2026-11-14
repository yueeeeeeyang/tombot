package nodecommand

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestEnrollmentTerminalInput(t *testing.T) {
	for _, kind := range []string{"正常", "长接入码", "取消", "超长", "结束输入"} {
		t.Run(kind, func(t *testing.T) {
			master, terminal, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := master.Close(); err != nil {
					t.Error(err)
				}
				if err := terminal.Close(); err != nil {
					t.Error(err)
				}
			})
			original, err := getTerminalMode(int(terminal.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			length := 32
			if kind == "长接入码" {
				length = 1120
			}
			secret := make([]byte, length)
			if _, err := rand.Read(secret); err != nil {
				t.Fatal(err)
			}
			token := []byte(base64.RawURLEncoding.EncodeToString(secret))
			defer clear(secret)
			defer clear(token)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			type outcome struct {
				token []byte
				err   error
			}
			done := make(chan outcome, 1)
			go func() { value, err := readEnrollmentFromTerminal(ctx, terminal); done <- outcome{value, err} }()
			// 收到提示说明终端已关闭回显；读到换行则输入已清理并恢复。
			output := make(chan []byte, 1)
			prompt := make(chan struct{})
			go func() {
				var content []byte
				chunk := make([]byte, 512)
				notified := false
				for {
					n, err := master.Read(chunk)
					content = append(content, chunk[:n]...)
					if !notified && bytes.Contains(content, []byte("取消）：")) {
						close(prompt)
						notified = true
					}
					if err != nil || bytes.Contains(content, []byte("\n")) {
						output <- content
						return
					}
				}
			}()
			select {
			case <-prompt:
			case <-ctx.Done():
				t.Fatal("等待终端提示超时")
			}
			switch kind {
			case "正常", "长接入码":
				_, err = master.Write(append(bytes.Clone(token), '\n'))
			case "取消":
				cancel()
			case "超长":
				_, err = master.Write(append(bytes.Repeat([]byte("x"), maximumEnrollmentInput+1), '\n'))
			case "结束输入":
				_, err = master.Write([]byte{4})
			}
			if err != nil {
				t.Fatal(err)
			}
			var result outcome
			select {
			case result = <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("输入未及时结束")
			}
			defer clear(result.token)
			if kind == "正常" || kind == "长接入码" {
				if result.err != nil || !bytes.Equal(result.token, token) {
					t.Fatal("有效输入未读取", result.err)
				}
			} else if result.err == nil || len(result.token) != 0 {
				t.Fatal("失败输入返回凭据")
			}
			if kind == "取消" && !errors.Is(result.err, context.Canceled) {
				t.Fatal("取消未生效")
			}
			restored, err := getTerminalMode(int(terminal.Fd()))
			if err != nil || *original != *restored {
				t.Fatal("终端状态未恢复", err)
			}
			select {
			case content := <-output:
				if bytes.Contains(content, token) {
					t.Fatal("终端回显了接入码")
				}
			case <-time.After(time.Second):
				t.Fatal("终端未输出结束换行")
			}
		})
	}
}

func TestEnrollmentRejectsNonTerminal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if token, err := readEnrollmentFromTerminal(t.Context(), file); err == nil || len(token) != 0 {
		t.Fatal("接受了非交互输入")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readEnrollmentFromTerminal(ctx, file); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消仍尝试读取输入")
	}
}
