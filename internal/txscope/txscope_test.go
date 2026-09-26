package txscope

import (
	"context"
	"testing"
)

func TestMarkHeld(t *testing.T) {
	ctx := context.Background()
	if Held(ctx) {
		t.Fatal("a fresh context must not be marked")
	}
	m := Mark(ctx)
	if !Held(m) {
		t.Fatal("Mark must mark")
	}
	child, cancel := context.WithCancel(m)
	defer cancel()
	if !Held(child) {
		t.Fatal("a child of a marked context must stay marked")
	}
	if Held(nil) { //nolint:staticcheck // nil context is handled defensively
		t.Fatal("nil context must not be marked")
	}
}
