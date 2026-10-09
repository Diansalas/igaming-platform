//go:build integration

// Ledger-finance review F3b (rv-fh3-ledger.md, 076e42e; registry
// FH3-FOLLOWUP-1): applyStatusEvidence (sweeper.go) must decide from the
// FRESH, under-lock re-read of the attempt, mapping it onto the §4.4
// matrix's own cells for that fresh state, exactly like
// applyResolvedReceiptEvidence (receipt.go) already does for the callback
// path - never blindly re-run the live-attempt flow and let its own CAS
// transitions (ApplySuccess's T7, applyMultipleSuccessDispute's T10)
// reject a state they were never meant to apply to. Mutant F3S (the
// sweeper's own re-read removed) previously survived the full suite
// because nothing exercised this exact gap: with the re-read intact but
// the DECISION still made from the stale, pre-lock snapshot, these two
// tests fail identically to F3S actually being applied.
package payments

import (
	"context"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// TestINVDEP1_F3b_SweeperEvidenceDecidesFromFreshState_SucceededXSucceededNoOp
// reproduces ledger-finance's own storm probe (Q2) race deterministically:
// a concurrent callback moves the attempt to 'succeeded' BEFORE the
// sweeper's own (now stale) pre-lock snapshot is handed to
// processViaQueryStatus - the exact window a real storm hits ~13 times per
// run. Calls processViaQueryStatus directly with that stale snapshot
// (never through RunOnce/claimBatch, so the race is deterministic, not
// timing-dependent) and asserts the fresh-state matrix's own
// succeeded x succeeded cell (a no-op) is taken, never a CAS-conflict
// error from re-attempting ApplySuccess's T7 transition on an
// already-succeeded row.
func TestINVDEP1_F3b_SweeperEvidenceDecidesFromFreshState_SucceededXSucceededNoOp(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-f3b-succ", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-f3b-succ": provider}, MultiWebhookCredentialResolver{"mock-f3b-succ": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "f3b-succ-noop",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending {
		t.Fatalf("expected pending, got %s", res.Attempt.State)
	}
	ref := *res.Attempt.ProviderReference

	// Snapshot the attempt BEFORE the concurrent success callback below -
	// the stale object a real sweeper race's claim/QueryStatus pipeline
	// would have been working from.
	stale := res.Attempt

	provider.Resolve(ref, OutcomeSucceeded, "", false)
	if _, err := rvCallback(pool, orch, f, "mock-f3b-succ", provider.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("concurrent success callback: %v", err)
	}
	succeededOnce := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if succeededOnce.State != AttemptSucceeded {
		t.Fatalf("setup: expected succeeded after the callback, got %s", succeededOnce.State)
	}

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	if err := sweeper.processViaQueryStatus(context.Background(), f.tenantID, stale); err != nil {
		t.Fatalf("F3b: succeeded x succeeded must be a no-op, not a CAS-conflict error: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptSucceeded {
		t.Fatalf("expected still succeeded, got %s", final.State)
	}
	if n := ledgerDepositTxCount(t, pool, f.tenantID, *final.DepositIntentID); n != 1 {
		t.Fatalf("expected exactly 1 deposit posting, got %d", n)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestINVDEP1_F3b_SweeperEvidenceDecidesFromFreshState_DisputedRecordedOnly
// is F3b's other cell: an attempt already disputed (multiple_success_for_
// intent, T13d) by a REAL, concurrent late success must record a
// redelivered/racing poll success as a no-op, never re-attempt
// applyMultipleSuccessDispute's own T10 CAS transition against an
// already-terminal row.
func TestINVDEP1_F3b_SweeperEvidenceDecidesFromFreshState_DisputedRecordedOnly(t *testing.T) {
	s := newInvDep1Setup(t, "f3b-disp-a", "f3b-disp-b")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "f3b-disp")
	refA := *res.Attempt.ProviderReference
	a2 := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "f3b-disp-a", s.pa, refA)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, a2)
	if child.State != AttemptPending || child.ProviderReference == nil {
		t.Fatalf("expected the cascade child pending, got %s", child.State)
	}
	refB := *child.ProviderReference

	// Snapshot A1 (declined) BEFORE its late success below moves it to
	// disputed - the stale object a real sweeper race would have been
	// working from.
	stale := mustGetAttempt(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if stale.State != AttemptDeclined {
		t.Fatalf("setup: expected A1 declined, got %s", stale.State)
	}

	s.pb.Resolve(refB, OutcomeSucceeded, "", false)
	if _, err := rvCallback(s.pool, s.orch, s.f, "f3b-disp-b", s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, refB, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("child success: %v", err)
	}
	s.pa.Resolve(refA, OutcomeSucceeded, "", false)
	if _, err := rvCallback(s.pool, s.orch, s.f, "f3b-disp-a", s.pa.CallbackPayload(s.f.tenantID, CallbackEventDeposit, refA, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("A1 late success (T13d): %v", err)
	}
	assertDisputedMultipleSuccess(t, s.pool, s.f.tenantID, res.Attempt.ID)

	sweeper := NewSweeper(s.pool, s.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	if err := sweeper.processViaQueryStatus(context.Background(), s.f.tenantID, stale); err != nil {
		t.Fatalf("F3b: disputed must be recorded-only, not a CAS-conflict error: %v", err)
	}

	assertDisputedMultipleSuccess(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if n := ledgerDepositTxCount(t, s.pool, s.f.tenantID, res.Intent.ID); n != 1 {
		t.Fatalf("expected exactly 1 deposit posting, got %d", n)
	}
	assertLedgerBalanced(t, s.pool, s.f.tenantID)
	loAssertProjectionMatchesRebuild(t, s.pool, s.f.tenantID)
}
