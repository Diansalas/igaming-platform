//go:build integration

// PAY-RECEIPT-T4-DRAIN-TEST-1 (B2): the CALLBACK-path deferred-receipt drain
// at the tail of ApplyReceiptEvidence (receipt.go: fresh GetAttemptByID
// re-read, then ApplyDeferredReceiptsForAttempt) had no killing test (mutant
// D-DRAIN-4 survived, ADR 0095 receipt review). These tests are TEST-ONLY;
// they are read-only on receipt.go.
//
// Shape: a ref-less ambiguous deposit attempt A; a verified SUCCESS for
// reference X arrives first and is stored deferred_unresolved (webhook beats
// phase C); then a PENDING callback resolved BY MERCHANT REFERENCE binds X to
// A (T4). Only the callback-path drain can then converge the deferred
// success: the player was charged, so the deposit must post exactly once.
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type t4DrainEnv struct {
	pool     *db.Pool
	orch     *Orchestrator
	f        orchFixture
	attempt  PaymentAttempt
	intent   uuid.UUID
	provider string
}

func t4DrainSetup(t *testing.T, providerID, key string) t4DrainEnv {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	mp := NewMockProvider(providerID, "EUR")
	registerCapability(t, pool, f, mp, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{providerID: &refLessAmbiguousProvider{mp}},
		MultiWebhookCredentialResolver{providerID: NewMockWebhookCredentials(mp)})
	res := rvInit(t, pool, orch, f, MockAmountAmbiguous, key)
	if res.Attempt.State != AttemptAmbiguous || res.Attempt.ProviderReference != nil {
		t.Fatalf("setup: want ref-less ambiguous attempt, got state=%s ref=%v", res.Attempt.State, res.Attempt.ProviderReference)
	}
	return t4DrainEnv{pool: pool, orch: orch, f: f, attempt: res.Attempt, intent: res.Intent.ID, provider: providerID}
}

func (e t4DrainEnv) apply(ev ReceiptEvidence) (ReceiptDisposition, error) {
	return rvApplyReceipt(e.pool, e.orch, e.f.tenantID, e.provider, ev)
}

func (e t4DrainEnv) count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	return rvQueryInt(t, e.pool, e.f, sql, args...)
}

func t4AssertConverged(t *testing.T, env t4DrainEnv, ref string) {
	t.Helper()
	f := env.f
	final := mustGetAttempt(t, env.pool, f.tenantID, env.attempt.ID)
	if final.State != AttemptSucceeded || final.LedgerTransactionID == nil {
		t.Fatalf("T4 drain: deferred success must converge the attempt, got state=%s ledger=%v", final.State, final.LedgerTransactionID)
	}
	if final.ProviderReference == nil || *final.ProviderReference != ref {
		t.Fatalf("T4: attempt must have bound reference %q, got %v", ref, final.ProviderReference)
	}
	if n := env.count(t, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2 AND resolved_at IS NULL`, f.tenantID, ref); n != 0 {
		t.Fatalf("T4 drain: %d receipt(s) for %s still unresolved (resolved_at IS NULL) - the pay_unresolved precursor", n, ref)
	}
	if n := env.count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, f.tenantID); n != 1 {
		t.Fatalf("T4 drain: want exactly one deposit posting, got %d", n)
	}
	if bal := cashBalance(t, env.pool, f); bal != MockAmountAmbiguous {
		t.Fatalf("T4 drain: cash balance = %d, want %d", bal, MockAmountAmbiguous)
	}
	var status string
	if err := env.pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, env.intent).Scan(&status)
	}); err != nil {
		t.Fatal(err)
	}
	if status != string(DepositIntentSucceeded) {
		t.Fatalf("T4 drain: intent projection = %q, want succeeded", status)
	}
	assertLedgerBalanced(t, env.pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, env.pool, f.tenantID)
}

// Sequential: success deferred first, then the Pending-by-merchant-reference
// callback performs T4 and its own tail drain must apply the deferred success.
func TestReceiptT4Drain_PendingByMerchantRef_AppliesDeferredSuccess_ExactlyOnce(t *testing.T) {
	env := t4DrainSetup(t, "mock-t4drain-a", "t4drain-a")
	ref := "t4drain-ref-" + uuid.NewString()

	succ := ReceiptEvidence{EventType: string(CallbackEventDeposit), ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
	disp, err := env.apply(succ)
	if err != nil || disp != DispositionDeferredUnresolved {
		t.Fatalf("setup: success before binding must be deferred_unresolved, got %s err=%v", disp, err)
	}
	if bal := cashBalance(t, env.pool, env.f); bal != 0 {
		t.Fatalf("setup: deferred success must not post yet, balance=%d", bal)
	}

	pend := ReceiptEvidence{EventType: string(CallbackEventDeposit), ProviderReference: ref, MerchantReference: env.attempt.ID.String(), Outcome: OutcomePending, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
	if disp, err = env.apply(pend); err != nil {
		t.Fatalf("pending-by-merchant-ref callback: %v", err)
	}
	if disp != DispositionApplied {
		t.Fatalf("pending callback disposition = %s, want applied (T4)", disp)
	}
	t4AssertConverged(t, env, ref)

	// Duplicates: redelivering either event must not post again.
	if disp, err = env.apply(succ); err != nil || disp != DispositionDuplicateEffect {
		t.Fatalf("redelivered success: disp=%s err=%v, want duplicate_effect", disp, err)
	}
	if disp, err = env.apply(pend); err != nil || disp != DispositionDuplicateEffect {
		t.Fatalf("redelivered pending: disp=%s err=%v, want duplicate_effect", disp, err)
	}
	t4AssertConverged(t, env, ref)
}

// Interleaved: the verified success commits INSIDE the T4 callback's own
// READ COMMITTED gap (after merchant-reference resolution, before its locks),
// so only the callback's tail drain - which must read the fresh row carrying
// the newly bound reference - can apply it.
func TestReceiptT4Drain_SuccessLandsDuringT4Callback_AppliedByTailDrain(t *testing.T) {
	env := t4DrainSetup(t, "mock-t4drain-b", "t4drain-b")
	ref := "t4drain-ref-" + uuid.NewString()

	hookFired := false
	testHookBeforeReferenceConflictRecheck = func() {
		hookFired = true
		d, err := env.apply(ReceiptEvidence{
			EventType: string(CallbackEventDeposit), ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR",
		})
		if err != nil || d != DispositionDeferredUnresolved {
			t.Errorf("interleaved success: disp=%s err=%v, want deferred_unresolved", d, err)
		}
	}
	t.Cleanup(func() { testHookBeforeReferenceConflictRecheck = nil })

	disp, err := env.apply(ReceiptEvidence{EventType: string(CallbackEventDeposit), ProviderReference: ref, MerchantReference: env.attempt.ID.String(), Outcome: OutcomePending, Amount: MockAmountAmbiguous, AssetCode: "EUR"})
	if err != nil {
		t.Fatalf("T4 callback: %v", err)
	}
	if !hookFired {
		t.Fatal("setup: interleaving hook never fired")
	}
	if disp != DispositionApplied {
		t.Fatalf("T4 callback disposition = %s, want applied", disp)
	}
	t4AssertConverged(t, env, ref)
}
