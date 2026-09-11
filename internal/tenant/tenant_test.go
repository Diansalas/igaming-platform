package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestFromContext_NoTenantAttached(t *testing.T) {
	_, err := FromContext(context.Background())
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("expected ErrNoTenant, got %v", err)
	}
}

func TestWithContext_RoundTrip(t *testing.T) {
	want := Context{
		TenantID: uuid.New(),
		Role:     "brand_operator",
		Subject:  "staff-42",
	}
	ctx := WithContext(context.Background(), want)

	got, err := FromContext(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("expected %+v, got %+v", want, got)
	}
}
