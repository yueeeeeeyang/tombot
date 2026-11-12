package computerimage

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSecurityOptionsMatchImagePolicy(t *testing.T) {
	options := SecurityOptions()
	if len(options) != 2 || options[0] != "no-new-privileges:true" || !strings.HasPrefix(options[1], "seccomp=") {
		t.Fatal("缺少不可提权或专用 seccomp 策略")
	}
	var policy struct {
		DefaultAction string `json:"defaultAction"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(options[1], "seccomp=")), &policy); err != nil || policy.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatal("运行策略未保持默认拒绝", err)
	}
	encoded, err := os.ReadFile("seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected, actual any
	if err = json.Unmarshal(encoded, &expected); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(strings.TrimPrefix(options[1], "seccomp=")), &actual); err != nil {
		t.Fatal(err)
	}
	left, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(actual)
	if err != nil || string(left) != string(right) {
		t.Fatal("节点策略和镜像验收策略不一致", err)
	}
	options[1] = "seccomp=unconfined"
	if SecurityOptions()[1] == options[1] {
		t.Fatal("调用方修改污染运行策略")
	}
}
