package agentchannel

import (
	"bytes"
	"encoding/json"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func testMessage() Message {
	return Message{Version: 1, ID: ids.New(), Method: "open", Files: 0, Body: json.RawMessage(`{"value":1}`)}
}

func TestSharedBrowserControlMethods(t *testing.T) {
	for _, method := range []string{"browser_start", "browser_status", "browser_close"} {
		message := testMessage()
		message.Method = method
		if err := message.validate(); err != nil {
			t.Fatal("共享浏览器固定控制动作被拒绝", method, err)
		}
	}
	message := testMessage()
	message.Method = "browser_execute"
	if message.validate() == nil {
		t.Fatal("通道接受任意浏览器执行动作")
	}
}

func TestControlFrameCanonicalValidation(t *testing.T) {
	message := testMessage()
	encoded, err := Encode(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Message
	if err = Decode(encoded, &decoded); err != nil || decoded.validate() != nil {
		t.Fatal("规范控制帧未往返", err)
	}
	for _, invalid := range [][]byte{append([]byte(" "), encoded...), append(append([]byte{}, encoded...), []byte("{}")...), bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":1,"extra":true`), 1), bytes.Replace(encoded, []byte(`"version":1`), []byte(`"Version":1`), 1), {0xff}} {
		if err := Decode(invalid, &Message{}); err == nil {
			t.Fatal("非规范控制帧被接受")
		}
	}
	for _, change := range []func(*Message){func(m *Message) { m.Version = 0 }, func(m *Message) { m.ID = "" }, func(m *Message) { m.Method = "arbitrary" }, func(m *Message) { m.Files = MaximumFiles + 1 }, func(m *Message) { m.Files = -1 }, func(m *Message) { m.Body = []byte("{") }} {
		value := testMessage()
		change(&value)
		if err := value.validate(); err == nil {
			t.Fatal("非法控制帧被接受")
		}
	}
}
