package computerdesktop

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func TestRejectMissingTransaction(t *testing.T) {
	if _, err := Register(context.Background(), nil, ids.New(), ids.New(), ids.New()); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("缺少事务仍接受登记", err)
	}
	if _, err := Lookup(context.Background(), nil, ids.New(), ids.New()); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("缺少事务仍接受读取", err)
	}
}
