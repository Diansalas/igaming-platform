//go:build integration

package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// RF4-1 (ledger-finance): a PAYOUT attempt INSERTed directly in `submitting` (the T1p shape) outside a gated T1p
// transaction is refused WD053, in both probe shapes: (a) a `submitted` request with no attempt (E7 committed by an
// earlier transaction, so no decision of THIS transaction); (b) an `approved` request (no E7 at all, so the E7 guard
// never fires), with or without an allowing dispatch decision. WD052 on a suspended account. The governed T1p claim
// (decision, E7, INSERT in one transaction) still passes with the trigger.
func TestF4_DB_PayoutAttemptBornSubmitting_NeedsTheGatedT1p(t *testing.T) {
	w := newF4World(t, "f4-rf41")
	insert := func(ctx context.Context, tx pgx.Tx, wrID uuid.UUID) error {
		_, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: w.f.tenantID, Operation: AttemptOperationPayout, WithdrawalRequestID: &wrID,
			ProviderID: "f4-rf41", PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: 500,
			ClaimToken: uuid.New(), LeaseOwner: "f4-probe", LeaseUntil: time.Now().Add(time.Minute),
		})
		return err
	}
	inTx := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return w.pool.WithTenant(context.Background(), w.f.tenantID, fn)
	}

	// (a) submitted request, no attempt, no decision in this transaction.
	sub := approvedWithdrawal(t, w.pool, w.f, 500, "f4-rf41-a")
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error { return f4MarkSubmittedPending(ctx, tx, sub.ID, "f4-rf41") })
	if c := f4SQLState(inTx(func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, sub.ID) })); c != "WD053" {
		t.Fatalf("(a) submitted request, no decision in this transaction: want WD053, got %q", c)
	}

	// (b) approved request: without a decision, and with an allowing dispatch decision but no E7.
	appr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-rf41-b")
	for _, record := range []bool{false, true} {
		err := inTx(func(ctx context.Context, tx pgx.Tx) error {
			if record {
				if err := f4RecordDispatchAllow(ctx, tx, appr.ID); err != nil {
					return err
				}
			}
			return insert(ctx, tx, appr.ID)
		})
		if c := f4SQLState(err); c != "WD053" {
			t.Fatalf("(b) approved request (decision recorded=%v): want WD053, got %q (%v)", record, c, err)
		}
	}

	// WD052: account suspended (the status half is checked first).
	w.setAccount(t, identity.PlayerStatusSuspended)
	if c := f4SQLState(inTx(func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, sub.ID) })); c != "WD052" {
		t.Fatalf("suspended account: want WD052, got %q", c)
	}
	w.setAccount(t, identity.PlayerStatusActive)

	if n := countAttempts(t, w.pool, w.f.tenantID, sub.ID) + countAttempts(t, w.pool, w.f.tenantID, appr.ID); n != 0 {
		t.Fatalf("no attempt may exist, got %d", n)
	}
	// The governed T1p passes with the trigger.
	if claim, err := w.claim(t, appr.ID); err != nil || claim.Attempt.State != AttemptSubmitting {
		t.Fatalf("the T1p claim must pass: %v", err)
	}
}
