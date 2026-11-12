package service

import (
	"context"
	"encoding/json"
	"testing"

	"io.yueyang/tom/internal/testkit"
)

func value[T any](v T) *T { return &v }
func TestCatalogDefaultsReferencesAndPatch(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	catalog := NewCatalog(db)
	p, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "本地模型", BaseURL: "http://127.0.0.1:1234/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	input := ModelInput{ProviderID: p.ID, ModelKey: "model-a", DisplayName: "模型 A", ContextWindow: 8192, MaxOutput: 1024, IsDefault: true}
	first, err := catalog.CreateModel(ctx, input, "user")
	if err != nil || !first.IsPriceEstimated {
		t.Fatalf("未配置价格没有标估算: %v", err)
	}
	input.ModelKey = "model-b"
	input.InputPriceMicro = value[int64](0)
	input.OutputPriceMicro = value[int64](0)
	second, err := catalog.CreateModel(ctx, input, "user")
	if err != nil || second.IsPriceEstimated {
		t.Fatalf("免费价格不能被标估算: %v", err)
	}
	first, err = catalog.Model(ctx, first.ID)
	if err != nil || first.IsDefault {
		t.Fatal("默认模型不唯一")
	}
	if _, err = catalog.CreateModel(ctx, input, "user"); err == nil {
		t.Fatal("重复模型被接受")
	}
	second, err = catalog.Model(ctx, second.ID)
	if err != nil || !second.IsDefault {
		t.Fatal("失败事务破坏了默认模型")
	}
	if err = catalog.DeleteProvider(ctx, p.ID, "user"); err == nil {
		t.Fatal("引用供应商仍可删除")
	}
	updated, err := catalog.UpdateModel(ctx, second.ID, ModelPatch{InputPriceMicro: Field[*int64]{Set: true, Value: value[int64](10)}}, "user")
	if err != nil || updated.IsPriceEstimated || updated.OutputPriceMicro != 0 {
		t.Fatal("局部价格修改丢失原语义")
	}
	if _, err = catalog.UpdateProvider(ctx, p.ID, ProviderPatch{BaseURL: Field[string]{Set: true, Value: "http://public.example/v1"}}, "user"); err == nil {
		t.Fatal("公网明文被接受")
	}
	for _, address := range []string{"https://user@example.com/v1", "https://example.com/v1?credential=hidden", "file:///tmp/model", "http://example.com"} {
		if err = ValidateProviderURL(address); err == nil {
			t.Fatalf("不安全 URL 被接受: %s", address)
		}
	}
	var patch ProviderPatch
	if err = json.Unmarshal([]byte(`{"secret_id":null}`), &patch); err != nil || !patch.SecretID.Set || patch.SecretID.Value != nil {
		t.Fatal("清空字段语义丢失")
	}
}
func TestModelLimits(t *testing.T) {
	ctx := context.Background()
	catalog := NewCatalog(testkit.Database(t))
	for _, input := range []ModelInput{{}, {ProviderID: "p", ModelKey: "x", DisplayName: "模型", ContextWindow: 100, MaxOutput: 1}, {ProviderID: "p", ModelKey: "x", DisplayName: "模型", ContextWindow: 1000, MaxOutput: 1001}, {ProviderID: "p", ModelKey: "x", DisplayName: "模型", ContextWindow: 1000, MaxOutput: 100, InputPriceMicro: value[int64](-1)}} {
		if _, err := catalog.CreateModel(ctx, input, "user"); err == nil {
			t.Fatal("无效模型配置被接受")
		}
	}
}
