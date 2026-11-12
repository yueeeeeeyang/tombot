package provider

import "testing"

func TestReportedUsagePreservesUnknownFields(t *testing.T) {
	emit := func(Event) error { return nil }
	state := streamState{usage: Usage{Estimated: true}}
	if err := state.openAI(`{"choices":[],"usage":{"prompt_tokens":7}}`, emit); err != nil {
		t.Fatal(err)
	}
	if state.usage.Reported == nil || state.usage.Reported.InputTokens == nil || *state.usage.Reported.InputTokens != 7 || state.usage.Reported.OutputTokens != nil || state.usage.Reported.CacheReadTokens != nil || !state.usage.Estimated {
		t.Fatal("缺失用量被补零或标为完整")
	}
	state = streamState{usage: Usage{Estimated: true}}
	if err := state.anthropic([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}}`), emit); err != nil {
		t.Fatal(err)
	}
	if state.usage.Reported.OutputTokens != nil {
		t.Fatal("尚未结算的输出被默认为零")
	}
	if err := state.anthropic([]byte(`{"type":"message_delta","usage":{"output_tokens":2}}`), emit); err != nil {
		t.Fatal(err)
	}
	usage := state.usage.Reported
	if state.usage.Estimated || usage.InputIncludesCache || *usage.InputTokens != 7 || *usage.OutputTokens != 2 || *usage.CacheReadTokens != 3 || *usage.CacheWriteTokens != 5 {
		t.Fatal("Anthropic 用量口径错误")
	}
	state = streamState{usage: Usage{Estimated: true}}
	if err := state.openAI(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, emit); err != nil {
		t.Fatal(err)
	}
	if state.usage.Reported.InputTokens == nil || state.usage.Reported.OutputTokens == nil || state.usage.Estimated {
		t.Fatal("明确的零用量被当成缺失")
	}
}

func TestReportedUsageRejectsImpossibleCounters(t *testing.T) {
	for _, payload := range []string{`{"usage":{"prompt_tokens":-1,"completion_tokens":2}}`, `{"usage":{"prompt_tokens":2,"completion_tokens":-1}}`, `{"usage":{"prompt_tokens":2,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":3}}}`, `{"usage":{"prompt_tokens":2,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":-1}}}`} {
		state := streamState{}
		if err := state.openAI(payload, func(Event) error { return nil }); err == nil {
			t.Fatal("无效上游计数被接受")
		}
	}
	for _, payload := range []string{`{"type":"message_start","message":{"usage":{"input_tokens":-1}}}`, `{"type":"message_start","message":{"usage":{"cache_creation_input_tokens":-1}}}`, `{"type":"message_delta","usage":{"output_tokens":-1}}`} {
		state := streamState{}
		if err := state.anthropic([]byte(payload), func(Event) error { return nil }); err == nil {
			t.Fatal("无效 Anthropic 计数被接受")
		}
	}
}
