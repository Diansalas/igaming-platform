//go:build integration

// KS-CAS-DISCRIM-TEST-1 (code-reviewer kill-switch phase-2 FH-7 re-review N1;
// PRH-2 C / G2). drive.go's cascade T2 branch folds "kill switch engaged" and
// EVERY other zero-row cause of ClaimCreatedForSubmission into one
// ErrAttemptStateConflict, then separates them with the read-only
// KillSwitchEngaged classification: only an ENGAGED switch may be recorded as
// terminal_reason='kill_switch' with a payments.cascade_rejected_kill_switch
// audit. Mutant K1 (never return on !engaged) survived every earlier suite:
// a non-kill-switch conflict would have been recorded as a kill-switch decline
// with a false audit.
//
// The non-kill-switch cause forced here is the T2 sibling-succeeded guard: the
// intent is already credited by another attempt when a stray 'created' sibling
// reaches T2. No kill switch is engaged anywhere.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestKSCASDiscrim_NonKillSwitchT2ConflictIsNeverRecordedAsKillSwitch(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	mp := NewMockProvider("mock-ksd", "EUR")
	registerCapability(t, pool, f, mp, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-ksd": mp}, MultiWebhookCredentialResolver{"mock-ksd": NewMockWebhookCredentials(mp)})

	// A1 succeeds (verified callback): the intent is financially resolved.
	res := rvInit(t, pool, orch, f, 5000, "ksd")
	intentID := res.Intent.ID
	ref := *res.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-ksd", mp.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("setup: success callback: %v", err)
	}
	if cashBalance(t, pool, f) != 5000 {
		t.Fatalf("setup: expected the intent credited")
	}

	// A stray 'created' sibling of the SAME, already-succeeded intent reaches T2.
	var created PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &intentID, AttemptNo: 2, ExcludedProviderIDs: []string{},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		return err
	}); err != nil {
		t.Fatalf("setup: insert stray created sibling: %v", err)
	}

	// Precondition: no kill switch is engaged for this tenant/provider/operation.
	var engaged bool
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		engaged, err = KillSwitchEngaged(ctx, tx, f.tenantID, "mock-ksd", AttemptOperationDeposit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if engaged {
		t.Fatalf("precondition: no kill switch may be engaged")
	}

	var intent DepositIntent
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = GetDepositIntentByID(ctx, tx, intentID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, _, err := orch.driveCreatedAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, intent, created, false)

	// (1) The conflict surfaces as an error (the pre-existing behaviour); it is
	// never silently reinterpreted as a kill-switch decline.
	if !errors.Is(err, ErrAttemptStateConflict) {
		t.Fatalf("a non-kill-switch T2 CAS conflict must surface ErrAttemptStateConflict, got %v", err)
	}
	// (2) No false kill-switch evidence anywhere.
	stray := mustGetAttempt(t, pool, f.tenantID, created.ID)
	if stray.State != AttemptCreated {
		t.Errorf("the stray sibling must be left untouched (created), got %s", stray.State)
	}
	if stray.TerminalReason != nil {
		t.Errorf("no terminal_reason may be recorded, got %q", *stray.TerminalReason)
	}
	var ksAudits int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payments.cascade_rejected_kill_switch'`, f.tenantID).Scan(&ksAudits)
	}); err != nil {
		t.Fatal(err)
	}
	if ksAudits != 0 {
		t.Errorf("a false payments.cascade_rejected_kill_switch audit was written (%d)", ksAudits)
	}
	// (3) No money moved and nothing else changed.
	if b := cashBalance(t, pool, f); b != 5000 {
		t.Errorf("balance=%d, want 5000 (unchanged)", b)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
	if n := ledgerDepositTxCount(t, pool, f.tenantID, intentID); n != 1 {
		t.Errorf("deposit postings=%d, want exactly 1", n)
	}
}
