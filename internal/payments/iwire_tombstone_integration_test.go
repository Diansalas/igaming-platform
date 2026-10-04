//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func iwSeedTombstone(t *testing.T, e *depRefEnv, ref string) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := postDepositReversalTombstone(ctx, tx, e.f.tenantID, e.id, ref)
		return err
	}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
}

// Tombstone-precedes-success, three evidence shapes, each a P1 with the closed
// reason reversal_tombstone_precedes_success (ADR 0102 Kind matrix).

// Poll path (live attempt -> T10 through parkDepositAttempt).
func TestIWire_Tombstone_PollPark_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-tomb-poll")
	a, ref := e.ambiguousBound(t, "iw-tomb-poll")
	iwSeedTombstone(t, e, ref)
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess("", 5000, "EUR")))
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptDisputed {
		t.Fatalf("state=%s", got.State)
	}
	iwParkAlert(t, e, a.ID, TerminalReasonTombstonePrecedesSuccess, ref)
}

// Receipt path, live attempt (non-terminal T10 from a callback).
func TestIWire_Tombstone_ReceiptLiveAttempt_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-tomb-live")
	a, ref := e.ambiguousBound(t, "iw-tomb-live")
	iwSeedTombstone(t, e, ref)
	if _, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || depTerminalReason(got) != TerminalReasonTombstonePrecedesSuccess {
		t.Fatalf("state=%s reason=%s", got.State, depTerminalReason(got))
	}
	if e.depositTxCount(t) != 0 {
		t.Fatal("a tombstoned success must never post")
	}
	iwParkAlert(t, e, a.ID, TerminalReasonTombstonePrecedesSuccess, ref)
}

// Receipt path, declined attempt (T13t).
func TestIWire_Tombstone_ReceiptDeclinedAttempt_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-tomb-decl")
	a, ref := e.ambiguousBound(t, "iw-tomb-decl")
	if _, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeDeclined, Amount: 5000, AssetCode: "EUR", DeclineReason: "provider_unavailable"}); err != nil {
		t.Fatalf("declined receipt: %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptDeclined {
		t.Fatalf("setup: state=%s, want declined", got.State)
	}
	iwSeedTombstone(t, e, ref)
	if _, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || depTerminalReason(got) != TerminalReasonTombstonePrecedesSuccess {
		t.Fatalf("state=%s reason=%s", got.State, depTerminalReason(got))
	}
	iwParkAlert(t, e, a.ID, TerminalReasonTombstonePrecedesSuccess, ref)
}
