package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"io.yueyang/tom/internal/domain"
)

const completionTimeout = 45 * time.Second

// Turn 是交给模型的一轮对话。回复引用的说明使用英文，因为这是模型上下文。
type Turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Completion 是一次非流式补全。密钥只放在请求头，不进入日志。
type Completion struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	TopP        float64
	MaxTokens   int
	Messages    []Turn
}

// Completer 调用已配置的 OpenAI 兼容接口。测试可以替换它。
type Completer interface {
	Complete(context.Context, Completion) (string, error)
}

// HTTPCompleter 使用带超时的 HTTP 客户端，不跟随超限响应。
type HTTPCompleter struct {
	client *http.Client
}

// NewHTTPCompleter 创建默认补全客户端。
func NewHTTPCompleter() *HTTPCompleter {
	return &HTTPCompleter{client: &http.Client{Timeout: completionTimeout + 5*time.Second}}
}

// Complete 请求 chat/completions。地址必须是不含账号的 HTTP(S)，失败时不返回部分正文冒充成功。
func (c *HTTPCompleter) Complete(ctx context.Context, request Completion) (string, error) {
	endpoint, err := completionEndpoint(request.BaseURL)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"model": request.Model, "temperature": request.Temperature, "top_p": request.TopP,
		"max_tokens": request.MaxTokens, "stream": false, "messages": request.Messages,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if request.APIKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+request.APIKey)
	}
	response, err := c.client.Do(httpRequest)
	if err != nil {
		return "", domain.NewError(502, "MODEL_UNAVAILABLE", "模型服务没有响应，请检查 API 地址后重试。")
	}
	limited := io.LimitReader(response.Body, 2<<20)
	raw, readErr := io.ReadAll(limited)
	closeErr := response.Body.Close()
	if readErr != nil {
		return "", domain.NewError(502, "MODEL_UNAVAILABLE", "读取模型响应失败。")
	}
	if closeErr != nil {
		return "", closeErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", domain.NewError(502, "MODEL_REJECTED", "模型服务拒绝了这次请求。")
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded.Choices) == 0 {
		return "", domain.NewError(502, "MODEL_REJECTED", "模型响应无法识别。")
	}
	content := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if content == "" {
		return "", domain.NewError(502, "MODEL_REJECTED", "模型没有返回正文。")
	}
	if utf16Len(content) > maxMessageLength {
		content = truncateUTF16(content, maxMessageLength)
	}
	return content, nil
}

func completionEndpoint(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", domain.NewError(400, "SETTINGS_INVALID", "请输入不含登录凭据的 HTTP 或 HTTPS 地址。")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/chat/completions") {
		path += "/chat/completions"
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func truncateUTF16(value string, limit int) string {
	encoded := utf16.Encode([]rune(value))
	if len(encoded) <= limit {
		return value
	}
	runes := utf16.Decode(encoded[:limit])
	return string(runes)
}

// transcript 把回复展开成模型能读的英文引用。界面仍只显示中文摘要。
func transcript(messages []ChatMessage) []Turn {
	byID := make(map[string]ChatMessage, len(messages))
	for _, message := range messages {
		byID[message.ID] = message
	}
	turns := make([]Turn, 0, len(messages))
	for _, message := range messages {
		content := message.Content
		if message.ReplyTo != nil {
			quoted := message.ReplyTo.Excerpt
			if original, ok := byID[message.ReplyTo.MessageID]; ok {
				quoted = original.Content
			}
			lead := "This assistant message replies to an earlier message."
			if message.Role == "user" {
				lead = "The user is replying to an earlier message."
			}
			content = strings.Join([]string{lead, "Quoted message:", quoted, "Reply:", message.Content}, "\n")
		}
		role := "user"
		if message.Role == "assistant" {
			role = "assistant"
		}
		turns = append(turns, Turn{Role: role, Content: content})
	}
	return turns
}
