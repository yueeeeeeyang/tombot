package modelregistry

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestModelMetadataPersistsAndPinsReasoning(t *testing.T) {
	cluster := pgtest.Start(t)
	fixture := newFixture(t, cluster)
	ctx := context.Background()
	old := fixture.policy(t)
	metadata := ModelMetadata{InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}, Effort: &ModelEffort{SupportedLevels: []string{"low", "high", "max"}, DefaultLevel: "high"}}
	request := ModelRequest{ProviderID: fixture.provider.ID, ModelKey: "rich-model", DisplayName: "带规格模型", ContextLimit: 1048576, OutputLimit: 393216, SupportsTools: true, SupportsVision: true, Metadata: metadata, ReasoningEffort: "high"}
	model, err := fixture.service.CreateModel(ctx, fixture.actor, ids.New(), request)
	if err != nil {
		t.Fatal(err)
	}
	models, err := fixture.service.ListModels(ctx, fixture.actor, fixture.provider.ID, "", 10)
	if err != nil || len(models) != 2 || models[0].ID != model.ID || !reflect.DeepEqual(models[0].Metadata, metadata) || models[0].ReasoningEffort != "high" {
		t.Fatal("持久化后元数据丢失", err)
	}
	fixture.model = model
	fixture.verifier.verify = func(_ context.Context, configuration Configuration, _ []byte) error {
		if configuration.ReasoningEffort != "high" {
			t.Error("启用未携带思考级别")
		}
		return nil
	}
	version := fixture.policy(t)
	if version.Candidates[0].Configuration.ReasoningEffort != "high" {
		t.Fatal("策略未固定思考级别")
	}
	fetched, err := fixture.service.GetVersion(ctx, fixture.actor, version.PolicyID, version.Number)
	if err != nil || fetched.Candidates[0].Configuration.ReasoningEffort != "high" {
		t.Fatal("重新读取策略丢失思考级别", err)
	}
	if _, err = fixture.service.GetVersion(ctx, fixture.actor, old.PolicyID, old.Number); err != nil {
		t.Fatal("旧策略无思考参数时失效", err)
	}
	fixture.verifier.probe = func(_ context.Context, configuration Configuration, _ []byte, _ string) error {
		if configuration.ReasoningEffort != "high" {
			t.Error("模型测试未携带所选思考级别")
		}
		return nil
	}
	if _, err = fixture.service.TestModel(ctx, fixture.actor, ids.New(), model.ID, 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE tombot.models SET metadata='{}',revision=revision+1 WHERE id=$1`, `UPDATE tombot.models SET reasoning_effort='low',revision=revision+1 WHERE id=$1`} {
		if _, err = fixture.pool.Exec(query, model.ID); err == nil {
			t.Fatal("目录元数据或思考级别可被原地改写")
		}
	}
}

func TestModelMetadataBoundaries(t *testing.T) {
	for _, test := range []struct {
		encoded, effort string
		vision          bool
	}{
		{`{"input_modalities":["text"]}`, "", true},
		{`{"input_modalities":["audio"]}`, "", false},
		{`{"output_modalities":["image"]}`, "", false},
		{`{"effort":{"supported_levels":["low"],"default_level":"high"}}`, "", false},
		{`{"effort":{"supported_levels":["high","high"]}}`, "high", false},
		{`{"effort":{"supported_levels":["high"]}}`, "max", false},
		{`{}`, "high", false},
	} {
		var metadata ModelMetadata
		if err := json.Unmarshal([]byte(test.encoded), &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.accepts(test.effort, test.vision) {
			t.Fatal("矛盾或非法元数据被接受")
		}
	}
	if !(ModelMetadata{}).accepts("", false) {
		t.Fatal("未提供可选元数据的手动登记被阻止")
	}
}
