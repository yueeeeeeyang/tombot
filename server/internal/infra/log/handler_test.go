package log_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/testkit"
)

func TestRedactsMessagesGroupsAndBoundAttributes(t *testing.T) {
	var output bytes.Buffer
	credential := testkit.Random(t)
	device := "tomd_" + testkit.Random(t)
	logger := slog.New(safelog.NewHandler(slog.NewJSONHandler(&output, nil))).With("api_key", credential).WithGroup("request")
	logger.Info("测试 "+device, "authorization", "Bearer "+credential, "nested", slog.GroupValue(slog.String("password", credential)), "device_id", "public-id")
	encoded := output.String()
	if strings.Contains(encoded, credential) || strings.Contains(encoded, device) {
		t.Fatal("日志泄漏机密")
	}
	if !strings.Contains(encoded, "public-id") || !strings.Contains(encoded, "已脱敏") {
		t.Fatal("脱敏丢失业务标识")
	}
}

func TestAuditRedactionNormalizesFieldsAndProtectsOldDetails(t *testing.T) {
	value := testkit.Random(t)
	encoded, err := json.Marshal(map[string]any{
		"history":      []any{map[string]string{"apiKey": value, "PRIVATE-KEY": value, "银行卡": value, "note": "Bearer " + value}},
		"secret_count": 3, "filename": "sk-" + value,
	})
	if err != nil {
		t.Fatal(err)
	}
	safe, err := safelog.SafeDetails(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(safe, []byte(value)) || !bytes.Contains(safe, []byte(`"secret_count":3`)) {
		t.Fatal("敏感字段或合法计数处理错误")
	}
	if _, err = safelog.SafeDetails([]byte(strings.Repeat("[", 30) + "1" + strings.Repeat("]", 30))); err == nil {
		t.Fatal("过度嵌套被接受")
	}
	if _, err = safelog.SafeDetails([]byte("{")); err == nil {
		t.Fatal("损坏详情被接受")
	}
}
