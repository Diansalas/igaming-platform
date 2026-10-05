//go:build integration

package kyc

// PRH-2 E1 mutation-driven additions (ADR 0106 section 10.9). Each test names
// the mutant it exists to kill:
//   - M7b: the txscope.Held refusal in callVendor (a ctx marked as holding a
//     pooled transaction never reaches the credential resolver or the adapter);
//   - M14b: the create idempotency key is validated by the guard TRIGGER (P0001),
//     not only by the table CHECK that would otherwise mask its removal.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

type countingOutboundResolver struct {
	inner OutboundCredentialResolver
	calls atomic.Int32
}

func (c *countingOutboundResolver) Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	c.calls.Add(1)
	return c.inner.Resolve(ctx, pool, tenantID, providerID)
}

// M7b: phase B refuses to run while the context says a pooled database
// transaction is held: no credential resolution, no adapter call, a not-sent
// outcome. The positive control proves the same row DOES reach the vendor on an
// unmarked context, so the refusal is the guard and not a missing fixture.
func TestOutbox_32b_CallVendorRefusesAHeldTransactionContext_M7b(t *testing.T) {
	r := newRig(t)
	v := r.create()
	resolver := &countingOutboundResolver{inner: NewMockOutboundResolver()}
	w := workerFor(r.pool, resolver, r.spy)
	row := claimedRow{
		ID: uuid.New(), TenantID: r.f.tenantID, VerificationID: v.ID, Operation: OpCreate,
		ProviderID: "mock", IdempotencyKey: "kv:" + v.ID.String(), ClaimToken: uuid.New(),
	}
	prep := &prepared{verification: v}

	held := w.callVendor(txscope.Mark(context.Background()), row, prep)
	if held.kind != outcomeNotSent {
		t.Fatalf("a held-transaction context must yield not-sent, got %v", held.kind)
	}
	if calls, _ := r.calls(); calls != 0 || resolver.calls.Load() != 0 {
		t.Fatalf("a held-transaction context must reach neither the resolver nor the adapter: adapter=%d resolver=%d", calls, resolver.calls.Load())
	}

	free := w.callVendor(context.Background(), row, prep)
	if free.kind != outcomeDefinitive {
		t.Fatalf("positive control: an unmarked context must reach the vendor, got %v", free.kind)
	}
	if calls, _ := r.calls(); calls != 1 || resolver.calls.Load() != 1 {
		t.Fatalf("positive control: expected exactly one adapter call and one resolution, got adapter=%d resolver=%d", calls, resolver.calls.Load())
	}
}

// M14b: a create row whose key is not kv:<verification id> is refused by the
// guard trigger itself (SQLSTATE P0001). The table CHECK would also refuse it
// (23514), but only AFTER the trigger, so the exact code proves the trigger
// layer is present.
func TestOutboxGuard_24b_CreateKeyMismatchRefusedByTheTrigger_M14b(t *testing.T) {
	r := newRig(t)
	orphan := rawOrphan(t, r.pool, seedSecondAccount(t, r.pool, r.f))
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		_, err := tx.Exec(ctx, insertCreateSQL, r.f.tenantID, orphan, "kv:"+uuid.NewString())
		if pgCode(err) != "P0001" {
			t.Errorf("the guard trigger must refuse a create with the wrong key (P0001), got %v", err)
		}
	})
}
