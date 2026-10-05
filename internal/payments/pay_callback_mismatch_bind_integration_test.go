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
