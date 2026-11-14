package modelregistry

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestCatalogDetails(t *testing.T) {
	fixture := newFixture(t, pgtest.Start(t))
	ctx := context.Background()
	policy, err := fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "详情策略")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, id string
		read     func(string) error
	}{
		{"供应商", fixture.provider.ID, func(id string) error {
			value, err := fixture.service.GetProvider(ctx, fixture.actor, id)
			if err == nil && (value.ID != id || value.Name != fixture.provider.Name || value.Revision != fixture.provider.Revision) {
				t.Fatal("供应商详情不一致")
			}
			return err
		}},
		{"策略", policy.ID, func(id string) error {
			value, err := fixture.service.GetPolicy(ctx, fixture.actor, id)
			if err == nil && (value.ID != id || value.Name != policy.Name || value.Revision != policy.Revision || value.CurrentVersion != nil) {
				t.Fatal("策略详情不一致")
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.read(test.id); err != nil {
				t.Fatal("详情读取失败", err)
			}
			requireError(t, test.read(ids.New()), ErrNotFound)
			requireError(t, test.read("invalid"), ErrInvalidInput)
			fixture.authorizer.denied.Store(true)
			requireError(t, test.read(test.id), identity.ErrForbidden)
			fixture.authorizer.denied.Store(false)
		})
	}
}
