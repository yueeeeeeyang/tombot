package provider

import (
	"encoding/json"
	"io.yueyang/tom/internal/domain"
	"testing"
)

func TestReasoningEffortRequestEncoding(t *testing.T) {
	for _, protocol := range []string{"responses", "openai_compat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			adapter := NewHTTP(domain.Provider{Kind: protocol, IsEnabled: true}, nil)
			for _, effort := range []string{"", "low", "high", "max"} {
				request := minimalRequest()
				request.ReasoningEffort = effort
				encoded, err := adapter.encode(request)
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]json.RawMessage
				if json.Unmarshal(encoded, &body) != nil {
					t.Fatal("请求格式错误")
				}
				field := "reasoning_effort"
				if protocol == "responses" {
					field = "reasoning"
				}
				if protocol == "anthropic" {
					field = "output_config"
				}
				raw, exists := body[field]
				if effort == "" {
					if exists {
						t.Fatal("缺省思考参数被擅自添加")
					}
					continue
				}
				var actual string
				if protocol == "openai_compat" {
					if json.Unmarshal(raw, &actual) != nil {
						t.Fatal("思考级别编码错误")
					}
				} else {
					var setting struct {
						Effort string `json:"effort"`
					}
					if json.Unmarshal(raw, &setting) != nil {
						t.Fatal("思考配置编码错误")
					}
					actual = setting.Effort
				}
				if actual != effort {
					t.Fatal("显式思考级别未传递")
				}
			}
		})
	}
}
