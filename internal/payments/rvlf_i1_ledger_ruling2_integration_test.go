//go:build integration

// RV-PRH-I1 callback re-review 1 (ledger-finance) - N1/BDR permanent
// regression tests. TestRVLF2_Q2_* pins N1's fix (finalizeDeclined's
// no-op branch must report the intent's ACTUAL status, so a decline of a
// still-live sibling arriving after a T13 success never inserts a new
// cascade child - the FRZ/FRZP mutant). TestRVLF2_Q3_* pins F2/BDR on the
// callback path specifically (an oversized vendor decline_reason must
// never 500-loop a decline callback).
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Q2: after a T13 success, a decline callback for a still-LIVE sibling must
// not insert a new cascade child (finalizeDeclined's no-op must report the
// intent as succeeded to cascadeEligible).
func TestRVLF2_Q2_DeclineOfLiveSiblingAfterT13_NoNewCascadeChild(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("mock-q2-a", "EUR")
	pb := NewMockProvider("mock-q2-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-q2-a": pa, "mock-q2-b": pb},
		MultiWebhookCredentialResolver{"mock-q2-a": NewMockWebhookCredentials(pa), "mock-q2-b": NewMockWebhookCredentials(pb)})
	res := rvInit(t, pool, orch, f, 5000, "q2")
	if res.Attempt.ProviderID == nil || *res.Attempt.ProviderID != "mock-q2-a" {
		t.Fatalf("expected first attempt at mock-q2-a, got %v", res.Attempt.ProviderID)
	}
	refA := *res.Attempt.ProviderReference
	// A1 declines (cascadable) via callback -> A2 created.
	if _, err := rvCallback(pool, orch, f, "mock-q2-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, refA, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("decline A1: %v", err)
	}
	var a2 PaymentAttempt
	var intent DepositIntent
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 2`, res.Intent.ID).Scan(&id); err != nil {
			return err
		}
		var err error
		if a2, err = GetAttemptByID(ctx, tx, id); err != nil {
			return err
		}
		intent, err = GetDepositIntentByID(ctx, tx, res.Intent.ID)
		return err
	}); err != nil {
		t.Fatalf("load A2: %v", err)
	}
	// Drive A2 to mock-q2-b (pending, live).
	_, a2after, _, _, _, err := orch.driveCreatedAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, intent, a2, true)
	if err != nil {
		t.Fatalf("drive A2: %v", err)
	}
	if a2after.State != AttemptPending || a2after.ProviderReference == nil {
		t.Fatalf("A2 expected pending, got %s", a2after.State)
	}
	// Late success on A1 (T13) -> intent succeeded.
	if _, err := rvCallback(pool, orch, f, "mock-q2-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, refA, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("T13: %v", err)
	}
	// Now A2 declines, cascadable.
	if _, err := rvCallback(pool, orch, f, "mock-q2-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, *a2after.ProviderReference, "", OutcomeDeclined, 0, "", "provider_unavailable", true)); err != nil {
		t.Fatalf("decline A2: %v", err)
	}
	n := rvQueryInt(t, pool, f, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 3`, res.Intent.ID)
	var st string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce((SELECT state FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 3), '-')`, res.Intent.ID).Scan(&st)
	})
	t.Logf("after A2 decline post-T13: attempt_no=3 rows=%d state=%s balance=%d", n, st, cashBalance(t, pool, f))
	if n != 0 {
		t.Errorf("FINDING-Q2: a cascade child (attempt 3, state=%s) was inserted for an intent that already succeeded", st)
		var a3 PaymentAttempt
		var in2 DepositIntent
		_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id = $1 AND attempt_no = 3`, res.Intent.ID).Scan(&id); err != nil {
				return err
			}
			a3, _ = GetAttemptByID(ctx, tx, id)
			in2, _ = GetDepositIntentByID(ctx, tx, res.Intent.ID)
			return nil
		})
		claimErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
				return err
			}
			return ClaimCreatedForSubmission(ctx, tx, a3.ID, "mock-q2-a", uuid.New(), "rv", time.Now().Add(time.Minute))
		})
		t.Logf("direct T2 claim of A3 on a paid intent: err=%v (want CAS conflict)", claimErr)
		before := pa.AttemptCount() + pb.AttemptCount()
		_, a3after, _, _, _, derr := orch.driveCreatedAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, in2, a3, true)
		after := pa.AttemptCount() + pb.AttemptCount()
		t.Logf("sweeper-style drive of A3: err=%v state=%s provider Deposit calls %d->%d balance=%d", derr, a3after.State, before, after, cashBalance(t, pool, f))
	}
}

// Q3 (F2): an oversized vendor decline_reason on a decline CALLBACK must not
// 500-loop (the receipt row itself carries decline_reason under a 64-byte CHECK).
func TestRVLF2_Q3_OversizedDeclineReasonCallback(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-q3", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-q3": p}, MultiWebhookCredentialResolver{"mock-q3": NewMockWebhookCredentials(p)})
	res := rvInit(t, pool, orch, f, 5000, "q3")
	ref := *res.Attempt.ProviderReference
	long := "insufficient_funds_" + string(make([]byte, 0)) + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	_, err := rvCallback(pool, orch, f, "mock-q3", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", long, false))
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	t.Logf("len(reason)=%d err=%v attempt=%s", len(long), err, a.State)
	if err != nil {
		t.Errorf("FINDING-Q3: an oversized decline_reason on a decline callback still fails the whole transaction (500 loop): %v", err)
	}
}
