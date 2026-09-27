package httpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// TestGatedReader_RefusesReentrantAcquire is architect review AC2(b)'s
// explicit unit test: "gatedReader is non-reentrant: if txscope.Held(ctx)
// is true... it fails closed (503) instead of acquiring again, since
// nested acquire at per-key cap 2 is a hold-and-wait self-deadlock". db is
// deliberately nil - the whole point is that a held context must be
// refused BEFORE ever touching the pool, so a nil pool proves the guard
// fires first (a real call would nil-panic).
func TestGatedReader_RefusesReentrantAcquire(t *testing.T) {
	gate := admission.NewBulkhead(10)
	g := gatedReader{db: nil, gate: gate, key: "k", perKeyCap: 2, clock: admission.RealClock(), wait: time.Second}

	held := txscope.Mark(context.Background())
	err := g.WithTenantReadOnly(held, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		t.Fatal("must never reach the callback when txscope is already held")
		return nil
	})
	if !errors.Is(err, errDBGateUnavailable) {
		t.Fatalf("want errDBGateUnavailable, got %v", err)
	}
}

// TestGatedReader_RefusesNestedAcquireViaOwnMark proves the SECOND half
// of AC2(b): a context already marked by a PRIOR gatedReader acquisition
// in this same call chain (gatedReaderMarkKey, set by WithTenantReadOnly
// itself before delegating to the pool) is refused on a nested call -
// preventing a self-deadlock if code inside fn ever called back into the
// same reader. db is nil for the identical reason as above: the guard
// must fire before ever reaching the pool.
func TestGatedReader_RefusesNestedAcquireViaOwnMark(t *testing.T) {
	gate := admission.NewBulkhead(10)
	g := gatedReader{db: nil, gate: gate, key: "k", perKeyCap: 2, clock: admission.RealClock(), wait: time.Second}

	marked := context.WithValue(context.Background(), gatedReaderMarkKey{}, true)
	err := g.WithTenantReadOnly(marked, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		t.Fatal("must never reach the callback on a chain already marked by this reader")
		return nil
	})
	if !errors.Is(err, errDBGateUnavailable) {
		t.Fatalf("want errDBGateUnavailable, got %v", err)
	}
}
