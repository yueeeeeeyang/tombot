package execution

import (
	"context"
	"io.yueyang/tom/internal/modelregistry"
	"testing"
)

func routingInt(value int) *int { return &value }

func (f fixture) routeBound(t *testing.T, lease Lease) {
	t.Helper()
	selection, err := f.service.BindModel(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.Route(context.Background(), selection, modelregistry.Requirements{}, ""); err != nil {
		t.Fatal(err)
	}
}
