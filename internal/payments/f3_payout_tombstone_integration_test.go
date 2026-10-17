//go:build integration

package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// F3 (platform-freeze work list): SR5R-1 - a pre-existing tombstone on the payout's original reference.

// f3ForeignTombstone posts a deposit-style tombstone (a FRESH correlation id, the same idempotency key the A5
// branch would use: "tombstone:<provider>:<ref>") on the payout's reference, the precondition of SR5R-1.
func f3ForeignTombstone(t *testing.T, e rbEnv, ref string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Migration 0175: a deposit-style tombstone on a payment provider is the signed one (same key
		// "tombstone:<provider>:<ref>", fresh correlation); a raw one is refused (AP001). The fixture bypasses only the
		// real callback's not-a-deposit integrity check, which is what this backstop test needs.
		var err error
		id, err = PlantVerifiedDepositReversalTombstone(ctx, tx, e.f.tenantID, e.pid, ref)
		return err
	}); err != nil {
		t.Fatalf("seed the foreign tombstone: %v", err)
	}
	return id
}

// THE SR5R-1 PROBE (security re-check 12.2): a deposit-style tombstone on a pending payout's reference, then two
// grant holders decide. Before F3 the second vote returned tombstoned_hold_released with the request failed and ZERO
// request-correlated tombstones (the branch's ledger.Post was a replay, so WD027 never fired). Now every vote is
// refused with the typed ErrPayoutReversalReferenceTombstoned (an ErrPayoutReversalAttemptInconsistent, HTTP 409):
// nothing is recorded, the payout stays submitted with its hold, the attempt stays pending.
func TestF3_SR5R1_PreExistingTombstoneOnThePayoutReference_Refused(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	ref := *a.ProviderReference
	f3ForeignTombstone(t, e, ref)
	cashBefore, txBefore := e.a5Cash(t), fpLedgerTx(t, e.pool, e.f)
	p := e.a5Payload(ref)
	for _, staff := range []uuid.UUID{w.f1.ID, w.f2.ID} {
		_, err := e.a5Decide(staff, wr.ID, p)
		if !errors.Is(err, ErrPayoutReversalReferenceTombstoned) || !errors.Is(err, ErrPayoutReversalAttemptInconsistent) {
			t.Fatalf("a vote on a tombstoned reference must be refused with the typed error, got %v", err)
		}
	}
	if got := fpReqState(t, e.pool, e.f, wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		t.Fatalf("the payout must stay submitted with its hold: %s", got.State)
	}
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatalf("the attempt must not be parked: %s", got.State)
	}
	if d := e.a5Cash(t) - cashBefore; d != 0 || fpLedgerTx(t, e.pool, e.f) != txBefore {
		t.Fatalf("nothing may be posted (cash delta %d)", d)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id=$1 AND decision='reverse'`, wr.ID); n != 0 {
		t.Fatalf("a refused vote records nothing, got %d decisions", n)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1 AND transaction_type='tombstone' AND correlation_id=$2`, e.f.tenantID, wr.ID); n != 0 {
		t.Fatalf("no request-correlated tombstone, got %d", n)
	}
	w.assertInvariants()
}

// The backstop inside the branch itself: called past the vote-time check (as a race would), a post that returns
// AlreadyPosted is refused and the whole branch rolls back (park, E10 release and audit included).
func TestF3_SR5R1_BranchRefusesAnAlreadyPostedTombstone(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	ref := *a.ProviderReference
	f3ForeignTombstone(t, e, ref)
	cashBefore := e.a5Cash(t)
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := reversePayoutBeforeCompletion(ctx, tx, wr, &a, e.a5Payload(ref))
		return err
	})
	if !errors.Is(err, ErrPayoutReversalReferenceTombstoned) {
		t.Fatalf("the branch must refuse a replayed tombstone: %v", err)
	}
	if got := fpReqState(t, e.pool, e.f, wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("rolled back: the request is still submitted, got %s", got.State)
	}
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatalf("rolled back: the attempt is still pending, got %s", got.State)
	}
	if d := e.a5Cash(t) - cashBefore; d != 0 {
		t.Fatalf("rolled back: no hold release, cash delta %d", d)
	}
	if n := e.a5Count(t, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_reversal_tombstoned' AND target_id=$2`, e.f.tenantID, wr.ID.String()); n != 0 {
		t.Fatalf("no tombstone audit row survives, got %d", n)
	}
}

// Control: with no foreign tombstone the same two votes still take the branch (the refusal is specific).
func TestF3_SR5R1_NoForeignTombstone_BranchStillExecutes(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	p := e.a5Payload(*a.ProviderReference)
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
	e.a5MustDecide(t, w.f2.ID, wr.ID, p, PayoutReversalTombstoned)
	if n := e.a5Count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1 AND transaction_type='tombstone' AND correlation_id=$2`, e.f.tenantID, wr.ID); n != 1 {
		t.Fatalf("the branch writes its own request-correlated tombstone, got %d", n)
	}
}

// The refusal is per (provider, reference): a tombstone of ANOTHER provider on the same reference string does not
// block this provider's branch.
func TestF3_SR5R1_AnotherProvidersTombstoneOnTheSameStringDoesNotBlock(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	ref := *a.ProviderReference
	other := e
	other.pid = "f3-other-psp"
	f3ForeignTombstone(t, other, ref)
	p := e.a5Payload(ref)
	e.a5MustDecide(t, w.f1.ID, wr.ID, p, PayoutReversalRecorded)
	e.a5MustDecide(t, w.f2.ID, wr.ID, p, PayoutReversalTombstoned)
}
