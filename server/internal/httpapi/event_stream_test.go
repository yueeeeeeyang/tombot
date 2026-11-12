package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/testkit"
)

// TestLiveSSEForwarding 在环回网络对三个订阅同时发流，计时覆盖事件落库至客户端读到正文。
func TestLiveSSEForwarding(t *testing.T) {
	if os.Getenv("TOM_TEST_SSE_LATENCY") != "1" {
		t.Skip("未启用实时转发耗时验收")
	}
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session, err := server.Auth.Login(ctx, password, "实时转发验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(server.Handler())
	defer host.Close()
	const rounds = 32
	readers := make([]*bufio.Scanner, 3)
	for i := range readers {
		req, err := http.NewRequestWithContext(ctx, "GET", host.URL+"/api/v1/events?conversation_id="+strconv.Itoa(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+session.DeviceToken)
		resp, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		readers[i] = bufio.NewScanner(resp.Body)
		if resp.StatusCode != http.StatusOK || !readers[i].Scan() || readers[i].Text() != ": connected" {
			t.Fatal("会话事件订阅未就绪", i)
		}
	}
	samples := make(chan time.Duration, len(readers)*rounds)
	t.Run("同时转发", func(t *testing.T) {
		for i, scanner := range readers {
			t.Run("会话"+strconv.Itoa(i), func(t *testing.T) {
				t.Parallel()
				for n := range rounds {
					value := strconv.Itoa(n)
					start := time.Now()
					if _, err := server.Bus.Publish(ctx, "text.delta", strconv.Itoa(i), value); err != nil {
						t.Fatal(err)
					}
					for scanner.Scan() {
						if strings.HasPrefix(scanner.Text(), "data: ") {
							break
						}
					}
					if err := scanner.Err(); err != nil || scanner.Text() != "data: "+strconv.Quote(value) {
						t.Fatal("事件缺失或顺序错误", n, err)
					}
					samples <- time.Since(start)
				}
			})
		}
	})
	close(samples)
	durations := make([]time.Duration, 0, cap(samples))
	for elapsed := range samples {
		durations = append(durations, elapsed)
	}
	if len(durations) != cap(samples) {
		t.Fatal("未收到全部耗时样本")
	}
	slices.Sort(durations)
	maximum := durations[len(durations)-1]
	t.Logf("三路共 %d 个事件，中位数 %s，P95 %s，最长 %s", len(durations), durations[len(durations)/2], durations[(len(durations)*95-1)/100], maximum)
	if maximum >= 50*time.Millisecond {
		t.Errorf("本轮转发超过 50 毫秒，最长 %s", maximum)
	}
}

func TestWorkspaceEventSummaryReplayAndIsolation(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(t.Context(), password, "工作台事件验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	const statusPath = "/api/v1/notifications/status"
	if response := request(t, handler, "GET", statusPath, nil, nil); response.Code != 401 {
		t.Fatal("匿名读取通知", response.Code)
	}
	if response := request(t, handler, "GET", statusPath, nil, map[string]string{"Authorization": "Bearer " + session.DeviceToken}); response.Code != 200 || !strings.Contains(response.Body.String(), `"completed_turn":null`) {
		t.Fatal("已授权客户端无法读取通知快照", response.Code)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	publish := func(kind, conversation string) string {
		t.Helper()
		id, err := server.Bus.Publish(t.Context(), kind, conversation, map[string]string{"kind": kind})
		if err != nil {
			t.Fatal(err)
		}
		return strconv.FormatInt(id, 10)
	}
	start := publish("bot.updated", "")
	publish("terminal.updated", "")
	publish("text.delta", "one")
	publish("browser.updated", "")
	publish("browser.availability", "")
	memory := publish("memory.updated", "one")
	publish("routine.updated", "two")
	publish("turn.finished", "one")
	publish("tool.output", "two")
	publish("bootstrap.step", "")
	end := publish("conversation.updated", "one")

	for _, test := range []struct {
		name, conversation, after string
		expected                  []string
	}{
		{"全局包含面板摘要和浏览器能力", "", start, []string{"terminal.updated", "browser.updated", "browser.availability", "memory.updated", "routine.updated", "notifications.changed", "conversation.updated"}},
		{"从已处理摘要后续传", "", memory, []string{"routine.updated", "notifications.changed", "conversation.updated"}},
		{"会话只含自身事件", "one", start, []string{"text.delta", "memory.updated", "turn.finished", "conversation.updated"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, host.URL+"/api/v1/events?conversation_id="+test.conversation, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Last-Event-ID", test.after)
			req.Header.Set("Authorization", "Bearer "+session.DeviceToken)
			response, err := host.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			var kinds []string
			var id string
			scanner := bufio.NewScanner(response.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if value, ok := strings.CutPrefix(line, "id: "); ok {
					id = value
				}
				if kind, ok := strings.CutPrefix(line, "event: "); ok {
					kinds = append(kinds, kind)
				}
				if strings.HasPrefix(line, "data: ") && kinds[len(kinds)-1] == "notifications.changed" && line != "data: {}" {
					t.Fatal("全局通知含有回合内容")
				}
				if id == end && line == "" {
					break
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if id != end || !slices.Equal(kinds, test.expected) {
				t.Fatalf("末尾游标 %s，收到 %v，预期 %v", id, kinds, test.expected)
			}
		})
	}
}
