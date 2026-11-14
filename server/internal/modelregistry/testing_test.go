package modelregistry

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestModelDiagnostics(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	for _, kind := range []string{"成功", "连接失败", "工具失败", "超时", "修改配置", "撤销凭据", "新增凭据", "撤销权限", "审计失败"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			if err := fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 1, false); err != nil {
				t.Fatal(err)
			}
			var borrowed []byte
			var calls []string
			fixture.verifier.probe = func(probeContext context.Context, configuration Configuration, credential []byte, probe string) error {
				calls = append(calls, probe)
				borrowed = credential
				if !configuration.SupportsTools || configuration.SupportsVision || configuration.OutputLimit > 1024 || !bytes.Equal(credential, fixture.plaintext) {
					t.Error("测试配置或凭据错误")
				}
				if kind == "连接失败" || kind == "工具失败" && probe == "tools" {
					return errors.New("不可回传的上游正文")
				}
				if kind == "超时" {
					<-probeContext.Done()
					return probeContext.Err()
				}
				if probe == "connectivity" {
					switch kind {
					case "修改配置":
						_, err := fixture.service.UpdateProvider(ctx, fixture.actor, ids.New(), fixture.provider.ID, 2, "变化后的名称", "openai_compat", fixture.provider.BaseURL, nil)
						if err != nil {
							t.Fatal(err)
						}
					case "撤销凭据":
						if err := fixture.service.RevokeCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID); err != nil {
							t.Fatal(err)
						}
					case "新增凭据":
						if _, err := fixture.service.AddCredential(ctx, fixture.actor, ids.New(), fixture.provider.ID, fixture.plaintext); err != nil {
							t.Fatal(err)
						}
					case "撤销权限":
						fixture.authorizer.denied.Store(true)
					}
				}
				return nil
			}
			if kind == "审计失败" {
				execute(t, fixture.pool, `CREATE FUNCTION public.reject_probe_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '测试审计失败'; END $$; CREATE TRIGGER reject_probe_audit BEFORE INSERT ON tombot.admin_audit_events FOR EACH ROW EXECUTE FUNCTION public.reject_probe_audit()`)
			}
			result, err := fixture.service.TestModel(ctx, fixture.actor, ids.New(), fixture.model.ID, 2, 1)
			if len(borrowed) == 0 || !bytes.Equal(borrowed, make([]byte, len(borrowed))) {
				t.Fatal("诊断凭据未清零")
			}
			switch kind {
			case "修改配置", "新增凭据":
				requireError(t, err, ErrConflict)
			case "撤销凭据":
				requireError(t, err, ErrCredential)
			case "撤销权限":
				requireError(t, err, identity.ErrForbidden)
			case "审计失败":
				if err == nil || result.ModelID != "" {
					t.Fatal("审计失败伪装成功")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(result.Connectivity.Message+result.Tools.Message, "上游正文") {
					t.Fatal("泄露上游错误")
				}
				if kind == "连接失败" || kind == "超时" {
					if result.Connectivity.Status != "failed" || result.Tools.Status != "skipped" || len(calls) != 1 {
						t.Fatal("连接失败后仍调用工具")
					}
				} else if result.Connectivity.Status != "succeeded" || len(calls) != 2 || calls[0] != "connectivity" || calls[1] != "tools" {
					t.Fatal("未执行独立的两项测试")
				}
				if kind == "工具失败" && result.Tools.Status != "failed" || kind == "成功" && result.Tools.Status != "succeeded" {
					t.Fatal("工具状态错误")
				}
				outcome := "succeeded"
				if kind != "成功" {
					outcome = "failed"
				}
				var count int
				if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND action='测试模型连通性和工具调用' AND result=$2`, fixture.model.ID, outcome).Scan(&count); err != nil || count != 1 {
					t.Fatal("缺失测试审计", err)
				}
			}
		})
	}
	t.Run("权限修订及取消边界不调用上游", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		_, err := fixture.service.TestModel(ctx, fixture.actor, ids.New(), fixture.model.ID, 2, 1)
		requireError(t, err, ErrConflict)
		_, err = fixture.service.TestModel(ctx, fixture.actor, ids.New(), fixture.model.ID, 1, 0)
		requireError(t, err, ErrInvalidInput)
		_, err = fixture.service.TestModel(ctx, fixture.actor, ids.New(), ids.New(), 1, 1)
		requireError(t, err, ErrNotFound)
		fixture.authorizer.denied.Store(true)
		_, err = fixture.service.TestModel(ctx, fixture.actor, ids.New(), fixture.model.ID, 1, 1)
		requireError(t, err, identity.ErrForbidden)
		fixture.authorizer.denied.Store(false)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = fixture.service.TestModel(canceled, fixture.actor, ids.New(), fixture.model.ID, 1, 1)
		requireError(t, err, context.Canceled)
		if fixture.verifier.calls.Load() != 0 {
			t.Fatal("无效请求调用了供应商")
		}
	})
}
