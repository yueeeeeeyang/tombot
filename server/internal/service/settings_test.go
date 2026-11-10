package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"io.yueyang/tom/internal/testkit"
)

func TestReviewerSettingsSelectionAndReferences(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	settings := NewSettings(db)
	catalog := NewCatalog(db)
	initial, err := settings.Get(ctx)
	if err != nil || initial.Reviewer.Enabled || initial.Reviewer.ModelID != nil {
		t.Fatal("默认审查配置错误", err)
	}
	p, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "模型服务", BaseURL: "http://127.0.0.1:1234"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	a, err := catalog.CreateModel(ctx, ModelInput{ProviderID: p.ID, ModelKey: "executor", DisplayName: "执行模型", ContextWindow: 8192, MaxOutput: 512}, "user")
	if err != nil {
		t.Fatal(err)
	}
	selected, _, self, err := catalog.ReviewModel(ctx, a.ID, nil)
	if err != nil || !self || selected.ID != a.ID {
		t.Fatal("单模型未回退自查", err)
	}
	b, err := catalog.CreateModel(ctx, ModelInput{ProviderID: p.ID, ModelKey: "reviewer", DisplayName: "审查模型", ContextWindow: 8192, MaxOutput: 512}, "user")
	if err != nil {
		t.Fatal(err)
	}
	selected, _, self, err = catalog.ReviewModel(ctx, a.ID, nil)
	if err != nil || self || selected.ID != b.ID {
		t.Fatal("未优先独立模型", err)
	}
	var workers sync.WaitGroup
	for _, patch := range []SettingsPatch{{Reviewer: &ReviewerPatch{Enabled: Field[*bool]{Set: true, Value: value(true)}}}, {Reviewer: &ReviewerPatch{ModelID: Field[*string]{Set: true, Value: &b.ID}}}} {
		workers.Go(func() {
			if _, err := settings.Update(ctx, patch, "user"); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	saved, err := settings.Get(ctx)
	if err != nil || !saved.Reviewer.Enabled || saved.Reviewer.ModelID == nil || *saved.Reviewer.ModelID != b.ID {
		t.Fatal("并发局部更新丢失", err)
	}
	if err = catalog.DeleteModel(ctx, b.ID, "user"); err == nil {
		t.Fatal("审查引用模型仍可删除")
	}
	if _, err = settings.Update(ctx, SettingsPatch{Reviewer: &ReviewerPatch{ModelID: Field[*string]{Set: true, Value: &a.ID}}}, "user"); err != nil {
		t.Fatal(err)
	}
	selected, _, self, err = catalog.ReviewModel(ctx, a.ID, &a.ID)
	if err != nil || !self || selected.ID != a.ID {
		t.Fatal("显式自查被覆盖", err)
	}
	if _, err = catalog.UpdateProvider(ctx, p.ID, ProviderPatch{IsEnabled: Field[bool]{Set: true, Value: false}}, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = settings.Update(ctx, SettingsPatch{Reviewer: &ReviewerPatch{Enabled: Field[*bool]{Set: true, Value: value(true)}}}, "user"); err == nil {
		t.Fatal("接受了不可用模型")
	}
	if _, err = settings.Update(ctx, SettingsPatch{Reviewer: &ReviewerPatch{Enabled: Field[*bool]{Set: true, Value: value(false)}}}, "user"); err != nil {
		t.Fatal("不能停用失效配置", err)
	}
	if _, err = settings.Update(ctx, SettingsPatch{Reviewer: &ReviewerPatch{ModelID: Field[*string]{Set: true}}}, "user"); err != nil {
		t.Fatal(err)
	}
	if err = catalog.DeleteModel(ctx, a.ID, "user"); err != nil {
		t.Fatal("清空配置后不能删除模型", err)
	}
	var audits int
	if err = db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='settings.update'").Scan(&audits); err != nil || audits != 5 {
		t.Fatal("配置审计不完整", audits, err)
	}
}

func TestReviewerSettingsRejectInvalidAndRollback(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	settings := NewSettings(db)
	for _, encoded := range []string{`{}`, `{"reviewer":null}`, `{"reviewer":{}}`, `{"reviewer":{"enabled":null}}`, `{"reviewer":{"model_id":""}}`, `{"reviewer":{"model_id":"missing"}}`} {
		var patch SettingsPatch
		if err := json.Unmarshal([]byte(encoded), &patch); err != nil {
			t.Fatal(err)
		}
		if _, err := settings.Update(ctx, patch, "user"); err == nil {
			t.Fatal("无效配置被接受", encoded)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_settings_audit BEFORE INSERT ON audit_log WHEN NEW.action='settings.update' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.Update(ctx, SettingsPatch{Reviewer: &ReviewerPatch{Enabled: Field[*bool]{Set: true, Value: value(true)}}}, "user"); err == nil {
		t.Fatal("审计失败未返回")
	}
	saved, err := settings.Get(ctx)
	if err != nil || saved.Reviewer.Enabled {
		t.Fatal("审计失败仍保存设置", err)
	}
	if _, err = db.Exec(`INSERT INTO settings(key,value_json,updated_at) VALUES('reviewer','invalid',0)`); err != nil {
		t.Fatal(err)
	}
	for _, encoded := range []string{"invalid", "null", "{}", `{"enabled":null}`, `{"enabled":false,"model_id":""}`, `{"enabled":false} {}`} {
		if _, err = db.Exec("UPDATE settings SET value_json=? WHERE key='reviewer'", encoded); err != nil {
			t.Fatal(err)
		}
		if _, err = settings.Get(ctx); err == nil {
			t.Fatal("配置损坏被默认为关闭", encoded)
		}
	}
}
