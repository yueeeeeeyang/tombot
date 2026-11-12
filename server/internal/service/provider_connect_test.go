package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/testkit"
)

const discoveredFixture = `{"object":"list","data":[{"id":"discovered-alpha","object":"model","name":"真实发现模型","context_window":16384,"max_output_tokens":2048,"input_modalities":["text","image"]},{"id":"new-model-not-in-code","object":"model"}]}`

type discoveryFixture struct {
	mu     sync.Mutex
	body   string
	status int
	hook   func()
	calls  int
}

func connectFixture(t *testing.T, db *sql.DB) (*Catalog, *secret.Manager, *discoveryFixture) {
	t.Helper()
	f := &discoveryFixture{body: discoveredFixture, status: 200}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, status, hook := f.body, f.status, f.hook
		f.calls++
		f.mu.Unlock()
		if r.URL.Path != "/models" || r.Method != "GET" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("发现调用缺少契约")
		}
		if hook != nil {
			hook()
		}
		w.WriteHeader(status)
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return NewCatalog(db, CatalogOptions{DeepSeekBaseURL: server.URL, DiscoveryTimeout: time.Second}), secret.New(db, make([]byte, 32)), f
}

func (f *discoveryFixture) change(body string, status int, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.status, f.hook = body, status, hook
}

func requireConnectionCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("期望错误 %s，实际 %v", code, err)
	}
}

func requireSecretValue(t *testing.T, secrets *secret.Manager, id, expected string) {
	t.Helper()
	if err := secrets.Use(context.Background(), id, "test", "", func(value []byte) error {
		if string(value) != expected {
			return errors.New("密钥未按事务生效")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectProviderCreatesAtomicCatalogAndBindsTom(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog, secrets, _ := connectFixture(t, db)
	bots := NewBots(db, bus.New(db), t.TempDir())
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	credential := testkit.Random(t)
	result, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: credential}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider.Preset != "deepseek" || len(result.Models) != 2 || result.Models[0].ModelKey != "discovered-alpha" || !result.Models[0].IsDefault || !result.Models[0].SupportsVision || !result.Models[0].SupportsTools || !result.Models[0].IsPriceEstimated {
		t.Fatal("真实模型规格或默认选择错误")
	}
	if result.Models[1].ContextWindow != 8192 || result.Models[1].MaxOutput != 4096 || !reflect.DeepEqual(result.ModelLimitDefaults, []string{"new-model-not-in-code"}) {
		t.Fatal("缺规格未采用并标记应用限额")
	}
	bot, err := bots.Get(ctx, "tom")
	if err != nil || bot.ModelID == nil || *bot.ModelID != result.Models[0].ID {
		t.Fatal("默认助手未原子绑定", err)
	}
	requireSecretValue(t, secrets, *result.Provider.SecretID, credential)
	providers, err := catalog.Providers(ctx, "", 10)
	if err != nil || len(providers) != 1 || providers[0].Preset != "deepseek" || !reflect.DeepEqual(providers[0].ModelLimitDefaults, result.ModelLimitDefaults) {
		t.Fatal("供应商未恢复规格来源", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), credential) {
		t.Fatal("接口泄露密钥")
	}
	var ciphertext []byte
	if err = db.QueryRow("SELECT ciphertext FROM secrets WHERE id=?", *result.Provider.SecretID).Scan(&ciphertext); err != nil || strings.Contains(string(ciphertext), credential) {
		t.Fatal("密钥未加密", err)
	}
}

func TestConnectProviderRepeatedSyncPreservesIDsPricesAndSelectedDefault(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog, secrets, _ := connectFixture(t, db)
	first, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: testkit.Random(t)}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	price := int64(123)
	_, err = catalog.UpdateModel(ctx, first.Models[1].ID, ModelPatch{IsDefault: Field[bool]{Set: true, Value: true}, InputPriceMicro: Field[*int64]{Set: true, Value: &price}, OutputPriceMicro: Field[*int64]{Set: true, Value: &price}, ContextWindow: Field[int]{Set: true, Value: 12000}, MaxOutput: Field[int]{Set: true, Value: 1500}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", ProviderID: first.Provider.ID}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if second.Provider.ID != first.Provider.ID || *second.Provider.SecretID != *first.Provider.SecretID || second.Models[0].ID != first.Models[0].ID || second.Models[0].IsDefault || !second.Models[1].IsDefault || second.Models[1].InputPriceMicro != 123 || second.Models[1].IsPriceEstimated || second.Models[1].ContextWindow != 12000 || second.Models[1].MaxOutput != 1500 || len(second.ModelLimitDefaults) != 0 {
		t.Fatal("重同步破坏模型标识、自定义价格或默认选择")
	}
	updated := testkit.Random(t)
	third, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: updated}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if third.Provider.ID != first.Provider.ID || third.Models[0].ID != first.Models[0].ID {
		t.Fatal("重复连接创建重复配置")
	}
	requireSecretValue(t, secrets, *third.Provider.SecretID, updated)
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM secrets").Scan(&count); err != nil || count != 1 {
		t.Fatal("轮换留下孤立密钥", err)
	}
}

func TestConnectProviderFailuresPreserveCatalogAndCredential(t *testing.T) {
	for _, failure := range []string{"认证失败", "空模型", "事务失败", "并发轮换"} {
		t.Run(failure, func(t *testing.T) {
			db := testkit.Database(t)
			ctx := context.Background()
			catalog, secrets, upstream := connectFixture(t, db)
			originalKey := testkit.Random(t)
			first, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: originalKey}, "user", secrets)
			if err != nil {
				t.Fatal(err)
			}
			expectedKey := originalKey
			input := ProviderConnectInput{Preset: "deepseek", ProviderID: first.Provider.ID, APIKey: testkit.Random(t)}
			switch failure {
			case "认证失败":
				upstream.change(input.APIKey, 401, nil)
			case "空模型":
				upstream.change(`{"object":"list","data":[]}`, 200, nil)
			case "事务失败":
				if _, err = db.Exec(`CREATE TRIGGER reject_model_sync BEFORE UPDATE ON models BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
					t.Fatal(err)
				}
			case "并发轮换":
				expectedKey = testkit.Random(t)
				upstream.change(discoveredFixture, 200, func() {
					if err := secrets.Rotate(ctx, *first.Provider.SecretID, expectedKey, "another-device"); err != nil {
						t.Error(err)
					}
				})
			}
			_, err = catalog.ConnectProvider(ctx, input, "user", secrets)
			if err == nil || strings.Contains(err.Error(), input.APIKey) {
				t.Fatal("失败接入成功或泄露密钥")
			}
			if failure == "并发轮换" {
				requireConnectionCode(t, err, "PROVIDER_CONNECTION_CHANGED")
			}
			requireSecretValue(t, secrets, *first.Provider.SecretID, expectedKey)
			models, err := catalog.Models(ctx, "", 10)
			if err != nil || len(models) != 2 {
				t.Fatal("失败改变模型数量", err)
			}
			for _, model := range models {
				if model.ID != first.Models[0].ID && model.ID != first.Models[1].ID {
					t.Fatal("失败事务替换模型标识")
				}
			}
			var count int
			if err = db.QueryRow("SELECT COUNT(*) FROM secrets").Scan(&count); err != nil || count != 1 {
				t.Fatal("失败留下额外密钥", err)
			}
		})
	}
}

func TestConnectProviderNewTransactionFailureLeavesNoConfiguration(t *testing.T) {
	db := testkit.Database(t)
	catalog, secrets, _ := connectFixture(t, db)
	if _, err := db.Exec(`CREATE TRIGGER reject_model_insert BEFORE INSERT ON models BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ConnectProvider(context.Background(), ProviderConnectInput{Preset: "deepseek", APIKey: testkit.Random(t)}, "user", secrets); err == nil {
		t.Fatal("模型插入失败仍成功")
	}
	for _, table := range []string{"providers", "models", "secrets"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("事务留下部分配置", table, err)
		}
	}
}

func TestConnectProviderProtectsReferencedRemovedModelsAndOtherDefaults(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog, secrets, upstream := connectFixture(t, db)
	bots := NewBots(db, bus.New(db), t.TempDir())
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: testkit.Random(t)}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	upstream.change(`{"object":"list","data":[{"id":"replacement","object":"model"}]}`, 200, nil)
	_, err = catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", ProviderID: first.Provider.ID}, "user", secrets)
	requireConnectionCode(t, err, "PROVIDER_MODEL_IN_USE")
	other, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "其他供应商", BaseURL: "https://other.example/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := catalog.CreateModel(ctx, ModelInput{ProviderID: other.ID, ModelKey: "chosen", DisplayName: "已有默认", ContextWindow: 4096, MaxOutput: 100, IsDefault: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bots.Update(ctx, "tom", BotPatch{ModelID: Field[*string]{Set: true, Value: &chosen.ID}}, "user"); err != nil {
		t.Fatal(err)
	}
	result, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", ProviderID: first.Provider.ID}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Models) != 1 || result.Models[0].IsDefault {
		t.Fatal("同步替换了其他供应商的全局默认")
	}
	models, err := catalog.Models(ctx, "", 10)
	if err != nil || len(models) != 2 {
		t.Fatal("未清理不再提供的无引用模型", err)
	}
	_, err = catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", ProviderID: other.ID, APIKey: testkit.Random(t)}, "user", secrets)
	requireConnectionCode(t, err, "PROVIDER_PRESET_MISMATCH")
}

func TestConnectProviderResyncKeepsExplicitlyClearedTomModel(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog, secrets, _ := connectFixture(t, db)
	bots := NewBots(db, bus.New(db), t.TempDir())
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: testkit.Random(t)}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bots.Update(ctx, "tom", BotPatch{ModelID: Field[*string]{Set: true, Value: nil}}, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", ProviderID: first.Provider.ID}, "user", secrets); err != nil {
		t.Fatal(err)
	}
	bot, err := bots.Get(ctx, "tom")
	if err != nil || bot.ModelID != nil {
		t.Fatal("刷新规格覆盖了用户清空的模型选择", err)
	}
}

func TestConnectProviderRuntimeLimitsAndSharedSecretIsolation(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog, secrets, upstream := connectFixture(t, db)
	catalog.deepSeekContextLimit, catalog.deepSeekOutputLimit = 4096, 512
	upstream.change(`{"object":"list","data":[{"id":"partial-limits","object":"model","max_output_tokens":100000}]}`, 200, nil)
	oldKey := testkit.Random(t)
	metadata, err := secrets.Create(ctx, "共享测试凭据", oldKey, "user")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "已配置 DeepSeek", BaseURL: catalog.deepSeekBaseURL, SecretID: &metadata.ID}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "使用同一凭据的供应商", BaseURL: "https://another.example", SecretID: &metadata.ID}, "user"); err != nil {
		t.Fatal(err)
	}
	newKey := testkit.Random(t)
	result, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", ProviderID: existing.ID, APIKey: newKey}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider.ID != existing.ID || result.Provider.DisplayName != existing.DisplayName || *result.Provider.SecretID == metadata.ID || result.Models[0].ContextWindow != 4096 || result.Models[0].MaxOutput != 512 || len(result.ModelLimitDefaults) != 1 {
		t.Fatal("已有供应商未复用、默认限额失效或错误轮换共享凭据")
	}
	requireSecretValue(t, secrets, metadata.ID, oldKey)
	requireSecretValue(t, secrets, *result.Provider.SecretID, newKey)
}

func TestConnectProviderRejectsBadInputBeforeContactingUpstream(t *testing.T) {
	db := testkit.Database(t)
	catalog, secrets, upstream := connectFixture(t, db)
	for _, input := range []ProviderConnectInput{{Preset: "unknown", APIKey: testkit.Random(t)}, {Preset: "deepseek"}, {Preset: "deepseek", APIKey: "short"}, {Preset: "deepseek", APIKey: testkit.Random(t) + "\ninvalid"}, {Preset: "deepseek", ProviderID: "missing", APIKey: testkit.Random(t)}} {
		if _, err := catalog.ConnectProvider(context.Background(), input, "user", secrets); err == nil {
			t.Fatal("无效输入仍接受")
		}
	}
	upstream.mu.Lock()
	calls := upstream.calls
	upstream.mu.Unlock()
	if calls != 0 {
		t.Fatal("无效输入仍访问外部服务")
	}
}

func TestConnectProviderPresetDoesNotSurviveGenericEndpointChange(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog, secrets, _ := connectFixture(t, db)
	result, err := catalog.ConnectProvider(ctx, ProviderConnectInput{Preset: "deepseek", APIKey: testkit.Random(t)}, "user", secrets)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := catalog.UpdateProvider(ctx, result.Provider.ID, ProviderPatch{BaseURL: Field[string]{Set: true, Value: "https://different.example/v1"}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Preset != "" || len(changed.ModelLimitDefaults) != 0 {
		t.Fatal("修改为通用供应商后仍应用 DeepSeek 专用协议")
	}
	reloaded, err := catalog.Provider(ctx, changed.ID)
	if err != nil || reloaded.Preset != "" {
		t.Fatal("清除预设没有持久化", err)
	}
}
