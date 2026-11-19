package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/nodeenrollment"
)

// writeSharedBrowserFixture 还原旧安装器生成的完整限额及安装请求中的局部覆盖，保留原证书和节点 ID。
func writeSharedBrowserFixture(t *testing.T, i *nodeInstaller) ([]byte, nodeInstallRequest) {
	t.Helper()
	path := nodeInstallationPaths().Node
	body, err := os.ReadFile(i.fs.resolve(path))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields["browser"] = json.RawMessage(`{"call_timeout_ms":11000,"cleanup_timeout_ms":30000,"shutdown_timeout_ms":60000,"limits":{"width":1024,"height":800,"maximum_windows":32,"maximum_per_session":4,"maximum_window_records":128,"maximum_events":256,"call_timeout_ms":5000,"dialog_timeout_ms":300000,"shutdown_timeout_ms":5000}}`)
	body = encodeFixture(t, fields)
	if _, err := nodeapp.ParseConfig(body); err == nil {
		t.Fatal("节点运行入口接受了旧版浏览器配置")
	}
	writeInstallFixture(t, i.fs, path, body, 0600)
	record, err := i.installedNodeRecord()
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := parseNodeInstallRequest(record.Request)
	if err != nil {
		t.Fatal(err)
	}
	request.Node = json.RawMessage(`{"browser":{"call_timeout_ms":11000,"limits":{"width":1024}}}`)
	record.Files["node.json"] = contentDigest(body)
	record.Request = encodeFixture(t, request)
	_, record.RequestSHA256, err = parseNodeInstallRequest(record.Request)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, i.fs, deploy.NodeDirectory+"/installation.json", encodeFixture(t, record), 0600)
	return body, request
}

func checkSharedBrowserMigration(t *testing.T, i *nodeInstaller, original []byte, id string) {
	t.Helper()
	body, err := os.ReadFile(i.fs.resolve(nodeInstallationPaths().Node))
	if err != nil {
		t.Fatal(err)
	}
	node, err := nodeapp.ParseConfig(body)
	if err != nil || node.NodeID != id || node.Browser.CallTimeout != 11000 {
		t.Fatal("迁移后身份、浏览器调用时限或配置格式不符", err)
	}
	record, err := i.installedNodeRecord()
	if err != nil || bytes.Contains(record.Request, []byte(`"limits"`)) || record.Files["node.json"] != contentDigest(body) {
		t.Fatal("安装快照未迁移或配置摘要不匹配", err)
	}
	backups, err := filepath.Glob(i.fs.resolve(nodeRepairDirectory + "-*/before/node.json"))
	if err != nil || len(backups) != 1 {
		t.Fatal("缺少迁移前备份", err)
	}
	before, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(before, original) {
		t.Fatal("原始配置未完整备份", err)
	}
}

func TestUpgradeChecksOriginalConfigBeforeRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func([]byte, nodeInstallRequest) []byte
	}{
		{"摘要漂移", "文件摘要不符", func(b []byte, _ nodeInstallRequest) []byte { return append(b, '\n') }},
		{"身份冲突", "node_id 与安装请求快照不符", func(b []byte, r nodeInstallRequest) []byte {
			return bytes.Replace(b, []byte(r.NodeID), []byte(ids.New()), 1)
		}},
		{"未知字段", "node.json 配置无效", func(b []byte, _ nodeInstallRequest) []byte {
			return bytes.Replace(b, []byte(`"width"`), []byte(`"other"`), 1)
		}},
		{"重复字段", "node.json 配置无效", func(b []byte, _ nodeInstallRequest) []byte {
			return bytes.Replace(b, []byte(`"width":1024`), []byte(`"width":1280,"width":1024`), 1)
		}},
		{"时限无效", "node.json 配置无效", func(b []byte, _ nodeInstallRequest) []byte {
			return bytes.Replace(b, []byte(`"call_timeout_ms":11000`), []byte(`"call_timeout_ms":0`), 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, binary, _ := legacyRepairFixture(t)
			body, request := writeSharedBrowserFixture(t, i)
			body = tc.change(body, request)
			writeInstallFixture(t, i.fs, nodeInstallationPaths().Node, body, 0600)
			if tc.name != "摘要漂移" {
				record, err := i.installedNodeRecord()
				if err != nil {
					t.Fatal(err)
				}
				record.Files["node.json"] = contentDigest(body)
				writeInstallFixture(t, i.fs, deploy.NodeDirectory+"/installation.json", encodeFixture(t, record), 0600)
			}
			i.inspectRecovery = func(context.Context, nodeInstallRequest, string) (*nodeenrollment.RecoveryInspection, error) {
				t.Fatal("校验失败时仍然查询控制服务")
				return nil, nil
			}
			_, err := i.upgradeWithRecovery(t.Context(), "/source/tomnode", binary, NodeRepairOptions{Upgrade: true}, stoppedNode)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("预期 %s，得到 %v", tc.want, err)
			}
		})
	}
}
