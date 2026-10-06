//go:build integration

// PAY-CALLBACK-MISMATCH-BIND-1 (Class B, B3; ADR 0095 §35.4): a callback
// T10 (callback_amount_asset_mismatch) and the multiple-success T10
// (multiple_success_for_intent) bind the provider-reported reference to the
// parked attempt BEFORE the dispute transition, when (1) the attempt holds
// no reference, (2) the value passes providerref.ValidatePaymentReference and
// (3) no other attempt/intent of the same tenant holds it. Otherwise the
// park stays reference-less, exactly as before, with no error.
package payments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// refLessLive drives a real deposit to a LIVE attempt that holds no
// provider_reference (the adapter answered ambiguous with an empty reference),
// i.e. exactly the attempt a callback resolves by MERCHANT reference.
func (e *depRefEnv) refLessLive(t *testing.T, f orchFixture, key string) PaymentAttempt {
	t.Helper()
	e.p.setScript(scriptOutcome(OutcomeAmbiguous, ""))
	res := rvInit(t, e.pool, e.orch, f, 5000, key)
	a := mustGetAttempt(t, e.pool, f.tenantID, res.Attempt.ID)
	if a.ProviderReference != nil || (a.State != AttemptAmbiguous && a.State != AttemptSubmitting && a.State != AttemptPending) {
		t.Fatalf("setup: state=%s ref=%v, want a live reference-less attempt", a.State, a.ProviderReference)
	}
	return a
}

func b3Mismatch(a PaymentAttempt, ref string) ReceiptEvidence {
	return ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"}
}

func b3Apply(t *testing.T, e *depRefEnv, f orchFixture, ev ReceiptEvidence) (ReceiptDisposition, error) {
	t.Helper()
	return rvApplyReceipt(e.pool, e.orch, f.tenantID, e.id, ev)
}

func b3AssertParked(t *testing.T, e *depRefEnv, f orchFixture, id uuid.UUID, reason string, wantRef *string) PaymentAttempt {
	t.Helper()
	got := mustGetAttempt(t, e.pool, f.tenantID, id)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != reason {
		t.Fatalf("attempt state=%s reason=%v, want disputed/%s", got.State, got.TerminalReason, reason)
	}
	switch {
	case wantRef == nil && got.ProviderReference != nil:
		t.Fatalf("park must stay reference-less, bound %q", *got.ProviderReference)
	case wantRef != nil && (got.ProviderReference == nil || *got.ProviderReference != *wantRef):
		t.Fatalf("park reference = %v, want %q", got.ProviderReference, *wantRef)
	}
	if got.LedgerTransactionID != nil {
		t.Fatalf("a parked attempt must carry no ledger link")
	}
	return got
}

func b3NoMoney(t *testing.T, e *depRefEnv, f orchFixture) {
	t.Helper()
	if e.depositTxCount(t) != 0 || cashBalance(t, e.pool, f) != 0 {
		t.Fatalf("a callback mismatch must never post")
	}
	assertLedgerBalanced(t, e.pool, f.tenantID)
}

// Normal: the callback resolved the attempt by merchant reference, so the park
// now holds R; the P1 is raised once and carries no raw reference.
func TestB3_CallbackMismatch_ByMerchantReference_BindsReportedReference(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-norm")
	a := e.refLessLive(t, e.f, "b3-norm")
	ref := "b3-ref-" + uuid.NewString()
	d, err := iwApplyReceiptInTx(t, e, b3Mismatch(a, ref))
	if err != nil || d != DispositionApplied {
		t.Fatalf("receipt: %v %v", d, err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	iwParkAlert(t, e, a.ID, TerminalReasonCallbackAmountAssetMismatch, ref)
	b3NoMoney(t, e, e.f)
}

// Duplicate/idempotency: a redelivered mismatch callback changes nothing.
func TestB3_CallbackMismatch_Redelivery_IsNoOp(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-dup")
	a := e.refLessLive(t, e.f, "b3-dup")
	ref := "b3-ref-" + uuid.NewString()
	ev := b3Mismatch(a, ref)
	if d, err := b3Apply(t, e, e.f, ev); err != nil || d != DispositionApplied {
		t.Fatalf("first: %v %v", d, err)
	}
	before := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	d, err := b3Apply(t, e, e.f, ev)
	if err != nil || d == DispositionApplied {
		t.Fatalf("redelivery must be a non-applied no-op, got %v %v", d, err)
	}
	after := b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("redelivery touched the parked attempt")
	}
	if n := e.auditCount(t, "payment.attempt_disputed", a.ID); n > 1 {
		t.Fatalf("redelivery duplicated the dispute audit: %d", n)
	}
	b3NoMoney(t, e, e.f)
}

// A rolled-back first attempt leaves neither bind nor dispute, and a retry
// converges to the same bound park: the bind is part of the dispute transaction.
func TestB3_CallbackMismatch_RollbackThenRetry_BindIsAtomicWithDispute(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-rb")
	a := e.refLessLive(t, e.f, "b3-rb")
	ref := "b3-ref-" + uuid.NewString()
	err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.id, b3Mismatch(a, ref)); err != nil {
			return err
		}
		return errors.New("injected: a later statement of the same transaction fails")
	})
	if err == nil {
		t.Fatal("setup: expected the injected failure")
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State == AttemptDisputed || got.ProviderReference != nil {
		t.Fatalf("rollback must undo the bind and the dispute together: state=%s ref=%v", got.State, got.ProviderReference)
	}
	if d, err := b3Apply(t, e, e.f, b3Mismatch(a, ref)); err != nil || d != DispositionApplied {
		t.Fatalf("retry: %v %v", d, err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	b3NoMoney(t, e, e.f)
}

// Rollback class: the alert statement fails deterministically (RaiseGuarded);
// the bind and the dispute still commit.
func TestB3_CallbackMismatch_AlertRaiseFails_BindAndDisputeStillCommit(t *testing.T) {
	for _, code := range []string{"P0001", "23514"} {
		t.Run(code, func(t *testing.T) {
			pool := testPool(t)
			e := newDepRefEnv(t, pool, "mock-b3-al"+strings.ToLower(code[:2]))
			a := e.refLessLive(t, e.f, "b3-al")
			ref := "b3-ref-" + uuid.NewString()
			alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, code)
			d, err := iwApplyReceiptInTx(t, e, b3Mismatch(a, ref))
			if err != nil || d != DispositionApplied {
				t.Fatalf("an alert failure must never fail the receipt: %v %v", d, err)
			}
			b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
			b3NoMoney(t, e, e.f)
		})
	}
}

// Partial failure: R is already bound to another attempt of this tenant (a
// payout, or a deposit). The receipt resolver already refuses to resolve the
// evidence (reference and merchant reference name different attempts): an
// anomaly, no error, nothing changes, the transaction commits and the callback
// never loops. The bind's own conflict check is proven directly below.
func TestB3_CallbackMismatch_ReferenceBoundElsewhere_AnomalyNoErrorLoop(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-cf1")
	a := e.refLessLive(t, e.f, "b3-cf1")
	ref := "b3-ref-" + uuid.NewString()
	payoutID := seedPayoutAttemptBoundTo(t, pool, e.f, e.id, ref)
	for i := 0; i < 2; i++ { // delivery and redelivery
		if d, err := b3Apply(t, e, e.f, b3Mismatch(a, ref)); err != nil || d != DispositionAnomaly {
			t.Fatalf("delivery %d must be a quiet anomaly, not an error: %v %v", i, d, err)
		}
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State == AttemptDisputed || got.ProviderReference != nil {
		t.Fatalf("an unresolved callback must not change the attempt: %+v", got)
	}
	if p := mustGetAttempt(t, pool, e.f.tenantID, payoutID); p.ProviderReference == nil || *p.ProviderReference != ref {
		t.Fatalf("the payout attempt changed: %+v", p)
	}
	b3NoMoney(t, e, e.f)
}

// The bind's own conflict check (LF-6 mirror): a reference held by another
// attempt of the tenant (payout or deposit) is NOT bound, with no error. Without
// the check the UPDATE would hit the per-tenant unique index and roll the whole
// evidence transaction back, forever.
func TestB3_BindParkReference_ConflictingReference_NotBound_NoError(t *testing.T) {
	t.Run("payout_attempt", func(t *testing.T) {
		pool := testPool(t)
		e := newDepRefEnv(t, pool, "mock-b3-bc1")
		a := e.refLessLive(t, e.f, "b3-bc1")
		ref := "b3-ref-" + uuid.NewString()
		seedPayoutAttemptBoundTo(t, pool, e.f, e.id, ref)
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return bindParkReference(ctx, tx, a, e.id, ref)
		}); err != nil {
			t.Fatalf("a conflicting reference must be skipped, not error: %v", err)
		}
		if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference != nil {
			t.Fatalf("conflicting reference bound: %q", *got.ProviderReference)
		}
	})
	t.Run("deposit_attempt", func(t *testing.T) {
		pool := testPool(t)
		e := newDepRefEnv(t, pool, "mock-b3-bc2")
		ref := "b3-ref-" + uuid.NewString()
		e.p.setScript(scriptOutcome(OutcomePending, ref))
		other := rvInit(t, pool, e.orch, e.f, 5000, "b3-bc2-other")
		a := e.refLessLive(t, e.f, "b3-bc2")
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return bindParkReference(ctx, tx, a, e.id, ref)
		}); err != nil {
			t.Fatalf("a conflicting reference must be skipped, not error: %v", err)
		}
		if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference != nil {
			t.Fatalf("conflicting reference bound: %q", *got.ProviderReference)
		}
		if o := mustGetAttempt(t, pool, e.f.tenantID, other.Attempt.ID); o.ProviderReference == nil || *o.ProviderReference != ref {
			t.Fatalf("the other attempt changed: %+v", o)
		}
	})
}

// The conflict check also covers a reference held at the deposit_intents level
// only (no attempt row holds it, so the attempt unique index would NOT stop the
// bind): binding it would leave two intents of the tenant claiming one reference.
func TestB3_BindParkReference_ReferenceHeldByAnotherIntent_NotBound(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-bi")
	other := e.refLessLive(t, e.f, "b3-bi-other")
	a := e.refLessLive(t, e.f, "b3-bi")
	if *other.DepositIntentID == *a.DepositIntentID {
		t.Fatal("setup: attempts must belong to different intents")
	}
	ref := "b3-ref-" + uuid.NewString()
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deposit_intents SET provider_id = $2, provider_reference = $3 WHERE id = $1`,
			*other.DepositIntentID, e.id, ref)
		return err
	}); err != nil {
		t.Fatalf("setup: bind the reference at intent level: %v", err)
	}
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return bindParkReference(ctx, tx, a, e.id, ref)
	}); err != nil {
		t.Fatalf("a reference held by another intent must be skipped, not error: %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference != nil {
		t.Fatalf("bound a reference another intent holds: %q", *got.ProviderReference)
	}
}

// bindParkReference itself (the validator is the guard whatever the caller
// does upstream): invalid, reserved-prefix and empty values never bind, and an
// existing reference is never replaced.
func TestB3_BindParkReference_ValidatorAndPreconditions(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-unit")
	cases := []struct {
		name string
		ref  string
		bind bool
	}{
		{"valid", "b3-unit-ok-" + uuid.NewString(), true},
		{"empty", "", false},
		{"control_char", "b3-unit\x01bad", false},
		{"oversize", strings.Repeat("r", 4096), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := e.refLessLive(t, e.f, "b3-unit-"+c.name)
			if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return bindParkReference(ctx, tx, a, e.id, c.ref)
			}); err != nil {
				t.Fatalf("bindParkReference must not error on a refused value: %v", err)
			}
			got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
			if c.bind != (got.ProviderReference != nil) {
				t.Fatalf("bound=%v, want %v (ref %v)", got.ProviderReference != nil, c.bind, got.ProviderReference)
			}
		})
	}
	t.Run("never_overwrites_existing_reference", func(t *testing.T) {
		a, ref := e.ambiguousBound(t, "b3-unit-has")
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return bindParkReference(ctx, tx, a, e.id, "b3-unit-other-"+uuid.NewString())
		}); err != nil {
			t.Fatal(err)
		}
		if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference == nil || *got.ProviderReference != ref {
			t.Fatalf("existing reference replaced: %v", got.ProviderReference)
		}
	})
}

// Tenant isolation: the same R string held by another tenant does not block
// the bind (the binding indexes are per tenant).
func TestB3_CallbackMismatch_SameReferenceInOtherTenant_StillBinds(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-tn")
	f2 := e.addTenant(t)
	ref := "b3-ref-" + uuid.NewString()
	e.p.setScript(scriptOutcome(OutcomePending, ref))
	other := rvInit(t, pool, e.orch, f2, 5000, "b3-tn-other")
	if other.Attempt.ProviderReference == nil || *other.Attempt.ProviderReference != ref {
		t.Fatalf("setup: other tenant attempt not bound to %q", ref)
	}
	a := e.refLessLive(t, e.f, "b3-tn")
	if d, err := b3Apply(t, e, e.f, b3Mismatch(a, ref)); err != nil || d != DispositionApplied {
		t.Fatalf("receipt: %v %v", d, err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	if o := mustGetAttempt(t, pool, f2.tenantID, other.Attempt.ID); o.State != AttemptPending {
		t.Fatalf("other tenant's attempt changed: %s", o.State)
	}
}

// Concurrency 1: the same mismatch callback delivered several times at once:
// one park, one bind, no error.
func TestB3_CallbackMismatch_ConcurrentSameCallback_OneBoundPark(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-cc1")
	a := e.refLessLive(t, e.f, "b3-cc1")
	ref := "b3-ref-" + uuid.NewString()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = rvApplyReceipt(pool, e.orch, e.f.tenantID, e.id, b3Mismatch(a, ref))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	if n := e.auditCount(t, "payment.attempt_disputed", a.ID); n > 1 {
		t.Fatalf("dispute audited %d times", n)
	}
	b3NoMoney(t, e, e.f)
}

// Concurrency 2: two different reference-less attempts of one tenant are told
// the SAME reference at once. Exactly one binds; the other never errors (the
// unique-index race is absorbed by the bind's savepoint, or the resolver sees
// the winner's reference and answers a quiet anomaly) and never holds it.
func TestB3_CallbackMismatch_TwoAttemptsRaceSameReference_OneBinds(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-cc2")
	atts := []PaymentAttempt{e.refLessLive(t, e.f, "b3-cc2-1"), e.refLessLive(t, e.f, "b3-cc2-2")}
	ref := "b3-ref-" + uuid.NewString()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range atts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = rvApplyReceipt(pool, e.orch, e.f.tenantID, e.id, b3Mismatch(atts[i], ref))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d must not error (no rollback loop): %v", i, err)
		}
	}
	bound := 0
	for _, a := range atts {
		got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
		if got.ProviderReference != nil {
			bound++
			if *got.ProviderReference != ref || got.State != AttemptDisputed {
				t.Fatalf("winner: state=%s ref=%q", got.State, *got.ProviderReference)
			}
		}
	}
	if bound != 1 {
		t.Fatalf("exactly one attempt may hold the reference, got %d", bound)
	}
	b3NoMoney(t, e, e.f)
}

// Deterministic form of the race: racer A binds R (uncommitted) and holds; racer
// B passes its pre-check (it cannot see A's uncommitted bind), then blocks on the
// per-tenant unique index. When A commits, B's UPDATE raises 23505; the savepoint
// absorbs it, B's transaction stays usable and commits with R unbound.
func TestB3_BindParkReference_ConcurrentUncommittedBind_LoserAbsorbed(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-det")
	a1 := e.refLessLive(t, e.f, "b3-det-1")
	a2 := e.refLessLive(t, e.f, "b3-det-2")
	ref := "b3-ref-" + uuid.NewString()
	bound, release := make(chan struct{}), make(chan struct{})
	racerA := loStartRacer(t, pool, e.f.tenantID, "A", func(ctx context.Context, tx pgx.Tx) error {
		if err := bindParkReference(ctx, tx, a1, e.id, ref); err != nil {
			return err
		}
		close(bound)
		<-release
		return nil
	})
	<-bound
	racerB := loStartRacer(t, pool, e.f.tenantID, "B", func(ctx context.Context, tx pgx.Tx) error {
		if err := bindParkReference(ctx, tx, a2, e.id, ref); err != nil {
			return err
		}
		// The transaction must still be usable after the absorbed violation.
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
	})
	if _, ok := loWaitBlocked(t, pool, racerB.pid, racerB.done); !ok {
		close(release)
		t.Fatalf("racer B never blocked on the uncommitted bind (err=%v)", racerB.wait())
	}
	close(release)
	if err := racerA.wait(); err != nil {
		t.Fatalf("racer A: %v", err)
	}
	if err := racerB.wait(); err != nil {
		t.Fatalf("racer B must absorb the unique violation, got %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a1.ID); got.ProviderReference == nil || *got.ProviderReference != ref {
		t.Fatalf("winner lost its bind: %v", got.ProviderReference)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a2.ID); got.ProviderReference != nil {
		t.Fatalf("loser bound a reference the winner holds: %q", *got.ProviderReference)
	}
	b3WantBindOutcomes(t, e, a1.ID, ref, "bound")
	b3WantBindOutcomes(t, e, a2.ID, ref, "lost_race")
}

// multiple_success_for_intent (T10 via the choke point): the intent is already
// financially resolved by another posting; a matching success callback by
// merchant reference parks the reference-less attempt WITH the reported reference.
func b3ResolvedIntentAndRefLessAttempt(t *testing.T, e *depRefEnv, key string) PaymentAttempt {
	t.Helper()
	a := e.refLessLive(t, e.f, key)
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intent, err := GetDepositIntentByID(ctx, tx, *a.DepositIntentID)
		if err != nil {
			return err
		}
		_, _, err = e.orch.postDepositSuccess(ctx, tx, intent, nil, e.id, "b3-other-posting-"+uuid.NewString(), 5000, "EUR")
		return err
	}); err != nil {
		t.Fatalf("setup: resolve the intent through another posting: %v", err)
	}
	return a
}

func b3MatchingSuccess(a PaymentAttempt, ref string) ReceiptEvidence {
	return ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: a.Amount, AssetCode: a.AssetCode}
}

func TestB3_MultipleSuccess_ByMerchantReference_BindsReportedReference(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-ms")
	a := b3ResolvedIntentAndRefLessAttempt(t, e, "b3-ms")
	ref := "b3-ref-" + uuid.NewString()
	d, err := b3Apply(t, e, e.f, b3MatchingSuccess(a, ref))
	if err != nil || d != DispositionAnomaly {
		t.Fatalf("receipt: %v %v", d, err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonMultipleSuccessForIntent, &ref)
	if n := e.auditCount(t, "payment.attempt_disputed", a.ID); n != 1 {
		t.Fatalf("dispute audits = %d, want 1", n)
	}
	// Redelivery: no-op, still one posting (the other one), still one audit.
	if _, err := b3Apply(t, e, e.f, b3MatchingSuccess(a, ref)); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonMultipleSuccessForIntent, &ref)
	if n := e.auditCount(t, "payment.attempt_disputed", a.ID); n != 1 {
		t.Fatalf("redelivery re-audited: %d", n)
	}
	if e.depositTxCount(t) != 1 {
		t.Fatalf("postings = %d, want only the pre-existing one", e.depositTxCount(t))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

func TestB3_MultipleSuccess_ReferenceBoundElsewhere_QuietAnomaly(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-msc")
	a := b3ResolvedIntentAndRefLessAttempt(t, e, "b3-msc")
	ref := "b3-ref-" + uuid.NewString()
	seedPayoutAttemptBoundTo(t, pool, e.f, e.id, ref)
	if d, err := b3Apply(t, e, e.f, b3MatchingSuccess(a, ref)); err != nil || d != DispositionAnomaly {
		t.Fatalf("a conflicting reference must be a quiet anomaly: %v %v", d, err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference != nil {
		t.Fatalf("conflicting reference bound: %q", *got.ProviderReference)
	}
}

// seedPayoutAttemptLiveNoRef creates a payout attempt in 'submitting' holding no
// provider reference (the payout twin of refLessLive).
func seedPayoutAttemptLiveNoRef(t *testing.T, e *depRefEnv) PaymentAttempt {
	t.Helper()
	attemptID := uuid.New()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		wr := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'submitted',$6)`, wr, e.f.tenantID, e.f.brandID, e.f.playerAccountID, e.f.walletID, "b3-payout-idem-"+wr.String()); err != nil {
			return err
		}
		if _, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: attemptID, TenantID: e.f.tenantID, Operation: AttemptOperationPayout,
			WithdrawalRequestID: &wr, AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		}); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, attemptID, e.id, uuid.New(), "b3-payout", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("seed payout attempt: %v", err)
	}
	a := mustGetAttempt(t, e.pool, e.f.tenantID, attemptID)
	if a.ProviderReference != nil || a.State != AttemptSubmitting {
		t.Fatalf("setup: state=%s ref=%v", a.State, a.ProviderReference)
	}
	return a
}

// ledger-finance C1: a PAYOUT mismatch park stays reference-less (a bound payout
// park would be a standing pay_captured_unposted no deposit reversal can clear).
func TestB3_PayoutMismatchPark_StaysReferenceLess(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-po")
	a := seedPayoutAttemptLiveNoRef(t, e)
	ref := "b3-ref-" + uuid.NewString()
	t.Run("direct", func(t *testing.T) {
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return bindParkReference(ctx, tx, a, e.id, ref)
		}); err != nil {
			t.Fatal(err)
		}
		if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference != nil {
			t.Fatalf("a payout attempt must never be bound by the park helper: %q", *got.ProviderReference)
		}
	})
	t.Run("via_receipt", func(t *testing.T) {
		d, err := b3Apply(t, e, e.f, ReceiptEvidence{EventType: "payout", ProviderReference: ref, MerchantReference: a.MerchantReference,
			Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
		if err != nil || d != DispositionApplied {
			t.Fatalf("receipt: %v %v", d, err)
		}
		b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, nil)
		b3WantBindOutcomes(t, e, a.ID, ref, "skipped_non_deposit", "skipped_non_deposit") // direct + via_receipt
		// The withdrawal is untouched by the park (no completion, no release).
		var st string
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT w.state FROM withdrawal_requests w JOIN payment_attempts p ON p.withdrawal_request_id = w.id WHERE p.id = $1`, a.ID).Scan(&st)
		}); err != nil || st != "submitted" {
			t.Fatalf("withdrawal state = %q err=%v, want submitted", st, err)
		}
		b3NoMoney(t, e, e.f)
	})
}

// ledger-finance C2: the LEDGER leg of foreignReferenceBinding. An attempt-less
// legacy deposit posting keyed by (provider, R) exists (no attempt/intent holds
// R), so a park must NOT bind R.
func TestB3_BindParkReference_ReferenceHeldByAttemptlessLedgerPosting_NotBound(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-lg")
	other := e.refLessLive(t, e.f, "b3-lg-other")
	a := e.refLessLive(t, e.f, "b3-lg")
	ref := "b3-ref-" + uuid.NewString()
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intent, err := GetDepositIntentByID(ctx, tx, *other.DepositIntentID)
		if err != nil {
			return err
		}
		if _, _, err = e.orch.postDepositSuccess(ctx, tx, intent, nil, e.id, ref, 5000, "EUR"); err != nil {
			return err
		}
		// A legacy, attempt-less posting: no intent or attempt holds R, only the ledger does.
		_, err = tx.Exec(ctx, `UPDATE deposit_intents SET provider_reference = NULL WHERE id = $1`, intent.ID)
		return err
	}); err != nil {
		t.Fatalf("setup: attempt-less ledger posting on R: %v", err)
	}
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return bindParkReference(ctx, tx, a, e.id, ref)
	}); err != nil {
		t.Fatalf("must skip, not error: %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.ProviderReference != nil {
		t.Fatalf("bound a reference a ledger posting already holds: %q", *got.ProviderReference)
	}
	b3WantBindOutcomes(t, e, a.ID, ref, "conflict:ledger_deposit")
}

// N1: a deferred (unresolved) success receipt for R is waiting when the park
// binds R. The receipt path's own post-transition backstop (receipt.go, the
// changed branch) now finds it in the SAME transaction and replays it against
// the disputed, R-holding attempt: consumed as an anomaly no-op, nothing posts,
// the attempt stays disputed, and a later explicit replay finds nothing.
func TestB3_BoundPark_DeferredReceiptForR_ReplaysAsNoOpAgainstDisputedAttempt(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-df")
	a := e.refLessLive(t, e.f, "b3-df")
	ref := "b3-ref-" + uuid.NewString()
	// Deferred: names R only, no merchant reference, nothing holds R yet.
	if d, err := b3Apply(t, e, e.f, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}); err != nil || d != DispositionDeferredUnresolved {
		t.Fatalf("setup deferred: %v %v", d, err)
	}
	if d, err := b3Apply(t, e, e.f, b3Mismatch(a, ref)); err != nil || d != DispositionApplied {
		t.Fatalf("park: %v %v", d, err)
	}
	parked := b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	b3NoMoney(t, e, e.f)
	var unresolved int
	var resolutions []string
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT COALESCE(resolution, '') FROM payment_provider_events
			WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND resolved_at IS NOT NULL AND attempt_id = $4`,
			e.f.tenantID, e.id, ref, a.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			resolutions = append(resolutions, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3 AND resolved_at IS NULL`,
			e.f.tenantID, e.id, ref).Scan(&unresolved)
	}); err != nil {
		t.Fatal(err)
	}
	if unresolved != 0 {
		t.Fatalf("the park's own backstop must consume the deferred receipt, %d left open", unresolved)
	}
	sawAnomaly := false
	for _, r := range resolutions {
		if r == string(ResolutionAnomalyOther) {
			sawAnomaly = true
		}
		if r == string(ResolutionApplied) {
			t.Fatalf("a deferred success must never be APPLIED against a disputed attempt: %v", resolutions)
		}
	}
	if !sawAnomaly {
		t.Fatalf("expected the deferred receipt resolved as anomaly_other against the parked attempt, got %v", resolutions)
	}
	var applied int
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, e.orch, parked)
		return err
	}); err != nil || applied != 0 {
		t.Fatalf("a later replay must find nothing: %d %v", applied, err)
	}
	b3AssertParked(t, e, e.f, a.ID, TerminalReasonCallbackAmountAssetMismatch, &ref)
	b3NoMoney(t, e, e.f)
}

// b3WantBindOutcomes asserts the durable bind-outcome audit rows of one attempt
// (security S-1) and that none of them carries the raw reference.
func b3WantBindOutcomes(t *testing.T, e *depRefEnv, attemptID uuid.UUID, rawRef string, want ...string) {
	t.Helper()
	var got []string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata->>'bind_outcome', metadata::text FROM audit_log
			WHERE tenant_id = $1 AND action = $2 AND target_id = $3 ORDER BY created_at, id`,
			e.f.tenantID, ParkReferenceBindAuditAction, attemptID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o, raw string
			if err := rows.Scan(&o, &raw); err != nil {
				return err
			}
			if rawRef != "" && strings.Contains(raw, rawRef) {
				t.Fatalf("bind audit leaks the raw reference: %s", raw)
			}
			got = append(got, o)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("bind outcome audit for %s = %v, want %v", attemptID, got, want)
	}
}

// S-1: every bind decision is durable, with the closed outcome vocabulary.
func TestB3_BindOutcome_IsAudited_NoRawReference(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b3-au")
	bind := func(a PaymentAttempt, ref string) {
		t.Helper()
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return bindParkReference(ctx, tx, a, e.id, ref)
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("bound_via_receipt", func(t *testing.T) {
		a := e.refLessLive(t, e.f, "b3-au-1")
		ref := "b3-ref-" + uuid.NewString()
		if _, err := b3Apply(t, e, e.f, b3Mismatch(a, ref)); err != nil {
			t.Fatal(err)
		}
		b3WantBindOutcomes(t, e, a.ID, ref, "bound")
	})
	t.Run("already_bound", func(t *testing.T) {
		a, _ := e.ambiguousBound(t, "b3-au-2")
		bind(a, "b3-au-other-"+uuid.NewString())
		b3WantBindOutcomes(t, e, a.ID, "", "already_bound")
	})
	t.Run("invalid", func(t *testing.T) {
		a := e.refLessLive(t, e.f, "b3-au-3")
		bind(a, "b3-bad\x01"+uuid.NewString()[:8])
		b3WantBindOutcomes(t, e, a.ID, "", "invalid:control_char")
	})
	t.Run("conflict_payout", func(t *testing.T) {
		a := e.refLessLive(t, e.f, "b3-au-4")
		ref := "b3-ref-" + uuid.NewString()
		seedPayoutAttemptBoundTo(t, pool, e.f, e.id, ref)
		bind(a, ref)
		b3WantBindOutcomes(t, e, a.ID, ref, "conflict:payout")
	})
	t.Run("skipped_non_deposit", func(t *testing.T) {
		a := seedPayoutAttemptLiveNoRef(t, e)
		ref := "b3-ref-" + uuid.NewString()
		bind(a, ref)
		b3WantBindOutcomes(t, e, a.ID, ref, "skipped_non_deposit")
	})
	t.Run("nothing_reported_is_not_audited", func(t *testing.T) {
		a := e.refLessLive(t, e.f, "b3-au-6")
		bind(a, "")
		b3WantBindOutcomes(t, e, a.ID, "")
	})
}
