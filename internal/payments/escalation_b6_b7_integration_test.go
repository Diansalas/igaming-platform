//go:build integration

// B6 (PAY-DEPOSIT-ESCALATION-1) and B7 (PAY-H-FOLLOWUPS-1 (11)): T16 escalation for a live
// deposit past the manifest SettlementWindow (and for a reference-less submitting deposit past
// lease + window), the rate-bounded unconfirmed-amount poll audit, and the T6 deferred-receipt
// drain. An escalation is NEVER a state change: the attempt stays live and keeps polling (LF-C2
// rule 5, never auto-decline); it writes one audit row and one durable P1.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

const b6EscalationAudit = "payment.deposit_escalated"

// setWindow changes the SettlementWindow the provider's manifest reports (the sweeper reads it
// from the registered provider on every tick). first_submitted_at is immutable, so tests age an
// attempt by shrinking the window, never by back-dating the row.
func (e *depRefEnv) setWindow(w time.Duration) {
	e.p.SetManifest(OperationManifest{
		SupportsDeposit: true, StatusQuery: "by_provider_reference", IdempotentSubmission: true,
		SyncSuccessPossible: true, SettlementWindow: w,
	})
}

func (e *depRefEnv) escalationAlertCount(t *testing.T, reason string, attemptID uuid.UUID) int {
	t.Helper()
	want := "attempt:" + attemptID.String() + ":reason:" + reason
	n := 0
	for _, r := range iwAlerts(t, e) {
		if r.Kind == string(alerting.KindPaymentWebhookIntegrity) && r.Discriminator == want {
			n++
		}
	}
	return n
}

// b6AssertStillLiveNoMoney: the escalated attempt is neither terminal nor declined, nothing
// posted, the intent is not declined, no cascade child, the ledger balances and projection ==
// rebuild.
func b6AssertStillLiveNoMoney(t *testing.T, e *depRefEnv, a PaymentAttempt) PaymentAttempt {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if !attemptAwaitingEvidence(got.State) || got.TerminalReason != nil {
		t.Fatalf("an escalated attempt must stay live, state=%s reason=%v", got.State, got.TerminalReason)
	}
	if depNextActionNull(t, e, e.f, a.ID) {
		t.Fatalf("an escalated attempt must keep polling (next_action_at set)")
	}
	if s := depIntentStatus(t, e, e.f, *a.DepositIntentID); s == string(DepositIntentDeclined) {
		t.Fatalf("an escalation must never decline the intent")
	}
	if n := depAttemptCount(t, e, e.f, *a.DepositIntentID); n != 1 {
		t.Fatalf("attempts for the intent = %d, want 1 (no cascade)", n)
	}
	if e.depositTxCount(t) != 0 || cashBalance(t, e.pool, e.f) != 0 {
		t.Fatalf("an escalation must never post")
	}
	assertLedgerBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
	return got
}

func pendingPoll(ref string) StatusResult {
	return StatusResult{ProviderReference: ref, Outcome: OutcomePending}
}

// --- B6.1 -------------------------------------------------------------------------

// Normal + duplicate + retry: nothing before the window; one escalation (audit, P1, escalated_at)
// after it; later ticks do not escalate again, the attempt keeps polling, and a later success
// still posts exactly once.
func TestB6_Escalation_AfterWindow_ExactlyOnce_KeepsPolling_LaterSuccessPostsOnce(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-norm")
	a, ref := e.ambiguousBound(t, "b6-norm")

	// Inside the (default 24h) window: no escalation.
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.EscalatedAt != nil || e.auditCount(t, b6EscalationAudit, a.ID) != 0 {
		t.Fatalf("inside the window nothing may escalate: escalated_at=%v", got.EscalatedAt)
	}

	e.setWindow(time.Nanosecond)
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
	first := b6AssertStillLiveNoMoney(t, e, a)
	if first.EscalatedAt == nil {
		t.Fatalf("past the window the attempt must be escalated")
	}
	if n := e.auditCount(t, b6EscalationAudit, a.ID); n != 1 {
		t.Fatalf("escalation audits = %d, want 1", n)
	}
	if reason := depScan[string](t, pool, e.f.tenantID,
		`SELECT metadata->>'reason' FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`, e.f.tenantID, b6EscalationAudit, a.ID.String()); reason != alertReasonDepositSettlementWindowExceeded {
		t.Fatalf("audit reason = %q", reason)
	}
	r := iwParkAlert(t, e, a.ID, alertReasonDepositSettlementWindowExceeded, ref)
	if r.Severity != "p1" {
		t.Fatalf("severity %s", r.Severity)
	}

	// Duplicate ticks: no second escalation, audit or alert; the attempt is still polled.
	for i := 0; i < 3; i++ {
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
	}
	later := b6AssertStillLiveNoMoney(t, e, a)
	if later.EscalatedAt == nil || !later.EscalatedAt.Equal(*first.EscalatedAt) {
		t.Fatalf("escalated_at must not be rewritten: %v -> %v", first.EscalatedAt, later.EscalatedAt)
	}
	if later.PollCount <= first.PollCount {
		t.Fatalf("an escalated attempt must keep polling: poll_count %d -> %d", first.PollCount, later.PollCount)
	}
	if n := e.auditCount(t, b6EscalationAudit, a.ID); n != 1 {
		t.Fatalf("duplicate ticks wrote %d escalation audits, want 1", n)
	}
	if n := e.escalationAlertCount(t, alertReasonDepositSettlementWindowExceeded, a.ID); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}

	// Retry: a later success still posts exactly once.
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR")))
	done := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if done.State != AttemptSucceeded || done.EscalatedAt == nil {
		t.Fatalf("state=%s escalated_at=%v, want succeeded and still marked escalated", done.State, done.EscalatedAt)
	}
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
	e.mustNoSweepErrors(t, e.sweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID}))
	if e.depositTxCount(t) != 1 {
		t.Fatalf("a sweep after success must not post again")
	}
}

// Every non-resolving poll outcome past the window is an escalation tick: transport failure,
// ambiguous, pending (T9), and a success with no amount (the "PSP never echoes amounts" shape).
func TestB6_Escalation_EveryNonResolvingPollOutcome(t *testing.T) {
	pool := testPool(t)
	for i, c := range []struct {
		name string
		st   func(ref string) StatusResult
	}{
		{"pending", func(ref string) StatusResult { return pendingPoll(ref) }},
		{"ambiguous", func(ref string) StatusResult { return StatusResult{ProviderReference: ref, Outcome: OutcomeAmbiguous} }},
		{"success-without-amount", func(ref string) StatusResult { return pollSuccess(ref, 0, "EUR") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-b6-out"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "b6-out")
			e.setWindow(time.Nanosecond)
			e.mustNoSweepErrors(t, e.poll(t, a, ref, c.st(ref)))
			if got := b6AssertStillLiveNoMoney(t, e, a); got.EscalatedAt == nil {
				t.Fatalf("%s past the window must escalate", c.name)
			}
			if e.auditCount(t, b6EscalationAudit, a.ID) != 1 || e.escalationAlertCount(t, alertReasonDepositSettlementWindowExceeded, a.ID) != 1 {
				t.Fatalf("want exactly one audit and one alert")
			}
		})
	}
}

// A successful, resolving poll is not an escalation tick, even past the window.
func TestB6_Escalation_ResolvingSuccessPastWindow_DoesNotEscalate(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-res")
	a, ref := e.ambiguousBound(t, "b6-res")
	e.setWindow(time.Nanosecond)
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR")))
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptSucceeded || got.EscalatedAt != nil || e.auditCount(t, b6EscalationAudit, a.ID) != 0 || len(iwAlerts(t, e)) != 0 {
		t.Fatalf("a resolving poll must not escalate: state=%s escalated_at=%v alerts=%d", got.State, got.EscalatedAt, len(iwAlerts(t, e)))
	}
}

// Concurrency: a callback succeeds inside the poll's own QueryStatus window (so before the
// evidence tx re-reads the attempt). The escalation tick must find the attempt terminal and
// write nothing: no escalated_at, no audit, no alert, no second posting.
func TestB6_Escalation_CallbackSucceedsDuringTick_NoEscalationNoTerminalWrite(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-cc")
	a, ref := e.ambiguousBound(t, "b6-cc")
	e.setWindow(time.Nanosecond)
	fired := e.deliverCallbackOnce(t, OutcomeSucceeded, 5000, "")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
	<-fired
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptSucceeded || got.EscalatedAt != nil {
		t.Fatalf("state=%s escalated_at=%v, want succeeded and never escalated", got.State, got.EscalatedAt)
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 0 || e.escalationAlertCount(t, alertReasonDepositSettlementWindowExceeded, a.ID) != 0 {
		t.Fatalf("a resolved attempt must not escalate")
	}
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// The escalation CAS itself, on stale in-memory snapshots: an attempt that resolved, or was
// already escalated, in the database is a conflict, reported as "did not escalate", with no
// audit, alert or row write.
func TestB6_Escalation_CASConflict_StaleSnapshot_WritesNothing(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-cas")
	e.setWindow(time.Nanosecond)

	run := func(stale PaymentAttempt) (bool, error) {
		var esc bool
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			intent, err := GetDepositIntentByID(ctx, tx, *stale.DepositIntentID)
			if err != nil {
				return err
			}
			esc, err = e.sweeper().escalateDepositIfDue(ctx, tx, intent, stale, alertReasonDepositSettlementWindowExceeded, 0)
			return err
		})
		if err == nil {
			pending.Flush(context.Background())
		}
		return esc, err
	}

	// (a) resolved in the database, stale snapshot still ambiguous.
	a, ref := e.ambiguousBound(t, "b6-cas-a")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR")))
	before := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if esc, err := run(a); esc || err != nil {
		t.Fatalf("stale snapshot of a succeeded attempt: escalated=%v err=%v, want false/nil", esc, err)
	}
	after := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.EscalatedAt != nil || after.State != AttemptSucceeded {
		t.Fatalf("the terminal row was written by a conflicted escalation")
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 0 || e.escalationAlertCount(t, alertReasonDepositSettlementWindowExceeded, a.ID) != 0 {
		t.Fatalf("a conflicted escalation must write no audit and raise no alert")
	}

	// (b) already escalated in the database, stale snapshot says it was not.
	b, refB := e.ambiguousBound(t, "b6-cas-b")
	e.mustNoSweepErrors(t, e.poll(t, b, refB, pendingPoll(refB)))
	if esc, err := run(b); esc || err != nil {
		t.Fatalf("stale snapshot of an escalated attempt: escalated=%v err=%v, want false/nil", esc, err)
	}
	if n := e.auditCount(t, b6EscalationAudit, b.ID); n != 1 {
		t.Fatalf("escalation audits = %d, want 1", n)
	}
	// The raw CAS: terminal and already-escalated rows both conflict.
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return Escalate(ctx, tx, id, time.Now().Add(time.Minute))
		}); !errors.Is(err, ErrAttemptStateConflict) {
			t.Fatalf("Escalate on %s: %v, want ErrAttemptStateConflict", id, err)
		}
	}
}

// Rollback / partial failure: a deterministic alert failure never aborts the escalation. The
// in-tx raise is swallowed, the escalation commits, and the post-commit detached retry persists
// the P1 (in-tx-only injection); with a persistent failure the escalation still commits.
func TestB6_Escalation_AlertDeterministicFailure_DoesNotAbortEscalation(t *testing.T) {
	pool := testPool(t)
	t.Run("in-tx failure, detached retry persists the P1", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b6-al1")
		a, ref := e.ambiguousBound(t, "b6-al1")
		e.setWindow(time.Nanosecond)
		alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
		if got := b6AssertStillLiveNoMoney(t, e, a); got.EscalatedAt == nil {
			t.Fatalf("the escalation must commit despite the alert failure")
		}
		if e.auditCount(t, b6EscalationAudit, a.ID) != 1 {
			t.Fatalf("the escalation audit must commit")
		}
		iwParkAlert(t, e, a.ID, alertReasonDepositSettlementWindowExceeded, ref)
	})
	for _, code := range []string{"P0001", "23514"} {
		t.Run("persistent "+code, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-b6-al2"+strings.ToLower(code[:2]))
			a, ref := e.ambiguousBound(t, "b6-al2")
			e.setWindow(time.Nanosecond)
			alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, code)
			e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
			if got := b6AssertStillLiveNoMoney(t, e, a); got.EscalatedAt == nil || e.auditCount(t, b6EscalationAudit, a.ID) != 1 {
				t.Fatalf("the escalation and its audit must commit despite a persistent alert failure")
			}
		})
	}
}

// Tenant isolation: tenant B's attempts and alerts are untouched by tenant A's escalation.
func TestB6_Escalation_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-iso")
	a, ref := e.ambiguousBound(t, "b6-iso")
	f2 := e.addTenant(t)
	e.setWindow(time.Nanosecond)
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
	if mustGetAttempt(t, pool, e.f.tenantID, a.ID).EscalatedAt == nil {
		t.Fatalf("setup: tenant A must escalate")
	}
	if rows := alertinject.ForSubject(t, pool, f2.tenantID); len(rows) != 0 {
		t.Fatalf("tenant B saw tenant A's alert: %+v", rows)
	}
}

// --- B6.2 -------------------------------------------------------------------------

// Audit counts over N polls: inside the window the unconfirmed-amount audit is written only when
// poll_count is 0 or a power of two. ambiguousBound leaves poll_count = 1 (T6 bumps it), so 20
// polls see poll_count 1..20 and write rows at 1, 2, 4, 8, 16.
func TestB6_UnconfirmedPollAudit_PowerOfTwoGate(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-p2")
	a, ref := e.ambiguousBound(t, "b6-p2")
	if pc := mustGetAttempt(t, pool, e.f.tenantID, a.ID).PollCount; pc != 1 {
		t.Fatalf("setup: poll_count = %d, want 1", pc)
	}
	const n = 20
	for i := 0; i < n; i++ {
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR")))
	}
	if got := e.auditCount(t, "payment.attempt_poll_amount_unconfirmed", a.ID); got != 5 {
		t.Fatalf("unconfirmed audits over %d polls = %d, want 5 (poll_count 1,2,4,8,16)", n, got)
	}
	if mustGetAttempt(t, pool, e.f.tenantID, a.ID).EscalatedAt != nil {
		t.Fatalf("inside the window nothing escalates")
	}
	// Still no posting; a later poll WITH evidence resolves it.
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR")))
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
}

// The escalation tick always writes the audit, even when poll_count is not a power of two.
func TestB6_UnconfirmedPollAudit_AlwaysOnEscalationTick(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-esct")
	a, ref := e.ambiguousBound(t, "b6-esct")
	// poll_count 1, 2: both written (powers of two).
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR")))
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR")))
	if got := e.auditCount(t, "payment.attempt_poll_amount_unconfirmed", a.ID); got != 2 {
		t.Fatalf("audits = %d, want 2", got)
	}
	// poll_count 3 (not a power of two): not escalating -> no row.
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR")))
	if got := e.auditCount(t, "payment.attempt_poll_amount_unconfirmed", a.ID); got != 2 {
		t.Fatalf("poll_count 3 must not write: audits = %d, want 2", got)
	}
	// poll_count 4 is a power of two; make the escalation land on poll_count 5 instead.
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR"))) // pc 4 -> row (3 total)
	e.setWindow(time.Nanosecond)
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR"))) // pc 5, escalation tick -> row (4 total)
	if pc := mustGetAttempt(t, pool, e.f.tenantID, a.ID).PollCount; pc != 6 {
		t.Fatalf("setup: poll_count = %d, want 6", pc)
	}
	if got := e.auditCount(t, "payment.attempt_poll_amount_unconfirmed", a.ID); got != 4 {
		t.Fatalf("the escalation tick (poll_count 5) must write the audit: audits = %d, want 4", got)
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 1 {
		t.Fatalf("want one escalation audit")
	}
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR"))) // pc 6: not power of two, escalated -> no row
	if got := e.auditCount(t, "payment.attempt_poll_amount_unconfirmed", a.ID); got != 4 {
		t.Fatalf("after escalation, poll_count 6 must not write: audits = %d, want 4", got)
	}
}

func TestB6_PollAuditDue_Unit(t *testing.T) {
	for pc, want := range map[int]bool{-1: true, 0: true, 1: true, 2: true, 3: false, 4: true, 5: false, 6: false, 7: false, 8: true, 12: false, 16: true, 1023: false, 1024: true} {
		if got := pollAuditDue(pc); got != want {
			t.Errorf("pollAuditDue(%d) = %v, want %v", pc, got, want)
		}
	}
}

// --- B6.3 -------------------------------------------------------------------------

// T6 drain: a verified success callback that arrived during phase B (stored
// deferred_unresolved) is resolved at T6 by the same phase C that binds the reference. The
// attempt succeeds, the receipt is applied, exactly one posting, and no receipt is left to age
// into pay_unresolved.
func TestB6_T6BindingRef_DrainsDeferredReceipt_OnePostingNoUnresolved(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t6")
	ref := "defer-ref-" + uuid.NewString()
	var cbRes ReceiveCallbackResult
	var cbErr error
	e.p.setScript(func(req DepositRequest) DepositResult {
		cbRes, cbErr = rvCallback(e.pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
		return DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}
	})
	res := rvInit(t, pool, e.orch, e.f, 5000, "b6-t6")
	if cbErr != nil || cbRes.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("phase B callback: disposition=%s err=%v, want deferred_unresolved", cbRes.Disposition, cbErr)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if got.State != AttemptSucceeded || got.ProviderReference == nil || *got.ProviderReference != ref || got.LedgerTransactionID == nil {
		t.Fatalf("T6 must drain the deferred success: state=%s ref=%v ledger=%v", got.State, got.ProviderReference, got.LedgerTransactionID)
	}
	assertDeferredReceiptResolved(t, pool, e.f.tenantID, ref, got.ID)
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND resolved_at IS NULL`, e.f.tenantID); n != 0 {
		t.Fatalf("%d receipt(s) still unresolved: the pay_unresolved precursor", n)
	}
	if s := depIntentStatus(t, e, e.f, *res.Attempt.DepositIntentID); s != string(DepositIntentSucceeded) {
		t.Fatalf("intent status = %q, want succeeded", s)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
	// A later poll / redelivery posts nothing more.
	e.mustNoSweepErrors(t, e.sweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID}))
	if e.depositTxCount(t) != 1 {
		t.Fatalf("a later sweep must not post again")
	}
}

// T6 with no deferred receipt behaves as before: ambiguous, reference bound, nothing drained.
func TestB6_T6BindingRef_NoReceipt_StaysAmbiguous(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t6n")
	a, _ := e.ambiguousBound(t, "b6-t6n")
	if a.State != AttemptAmbiguous || e.depositTxCount(t) != 0 {
		t.Fatalf("state=%s postings=%d", a.State, e.depositTxCount(t))
	}
}

// T6 with a deferred DECLINE receipt: the drain applies it (the attempt declines once) and
// nothing posts.
func TestB6_T6BindingRef_DrainsDeferredDecline_NoPosting(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t6d")
	ref := "defer-ref-" + uuid.NewString()
	var cbErr error
	e.p.setScript(func(req DepositRequest) DepositResult {
		_, cbErr = rvCallback(e.pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 5000, "EUR", "insufficient_funds", false))
		return DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}
	})
	res := rvInit(t, pool, e.orch, e.f, 5000, "b6-t6d")
	if cbErr != nil {
		t.Fatalf("phase B callback: %v", cbErr)
	}
	if n := depScan[int64](t, pool, e.f.tenantID, `SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND resolved_at IS NULL`, e.f.tenantID); n != 0 {
		t.Fatalf("%d receipt(s) still unresolved after T6", n)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if got.State == AttemptSucceeded || e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("a deferred decline must never post: state=%s", got.State)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

// --- B7 ---------------------------------------------------------------------------

func (e *depRefEnv) unreferencedSweeper() *Sweeper {
	s := e.sweeper()
	s.Lease = time.Nanosecond
	return s
}

func (e *depRefEnv) sweepUnreferenced(t *testing.T, a PaymentAttempt) {
	t.Helper()
	if attemptAwaitingEvidence(mustGetAttempt(t, e.pool, e.f.tenantID, a.ID).State) {
		setNextActionNow(t, e.pool, e.f.tenantID, a.ID) // a terminal row has no next_action_at (CHECK)
	}
	e.mustNoSweepErrors(t, e.unreferencedSweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID}))
}

// Normal: a reference-less live deposit is rescheduled inside lease + window, escalated exactly
// once after it (reason deposit_unreferenced_submitting), never polled (no reference to query),
// never declined, and keeps being rescheduled.
func TestB7_UnreferencedDeposit_EscalatesOnce_KeepsRescheduling(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-norm")
	a := e.refLessLive(t, e.f, "b7-norm")

	// Inside the (default 24h) window: rescheduled, not escalated.
	e.sweepUnreferenced(t, a)
	inside := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if inside.EscalatedAt != nil || inside.PollCount <= a.PollCount || e.auditCount(t, b6EscalationAudit, a.ID) != 0 {
		t.Fatalf("inside the window: escalated_at=%v poll_count %d -> %d", inside.EscalatedAt, a.PollCount, inside.PollCount)
	}

	e.setWindow(time.Nanosecond)
	e.sweepUnreferenced(t, a)
	first := b6AssertStillLiveNoMoney(t, e, a)
	if first.EscalatedAt == nil || first.ProviderReference != nil {
		t.Fatalf("escalated_at=%v ref=%v, want escalated and still reference-less", first.EscalatedAt, first.ProviderReference)
	}
	if reason := depScan[string](t, pool, e.f.tenantID,
		`SELECT metadata->>'reason' FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`, e.f.tenantID, b6EscalationAudit, a.ID.String()); reason != alertReasonDepositUnreferencedSubmitting {
		t.Fatalf("audit reason = %q", reason)
	}
	iwParkAlert(t, e, a.ID, alertReasonDepositUnreferencedSubmitting)

	// Duplicate ticks: no re-escalation, still rescheduled, never queried.
	for i := 0; i < 3; i++ {
		e.sweepUnreferenced(t, a)
	}
	later := b6AssertStillLiveNoMoney(t, e, a)
	if !later.EscalatedAt.Equal(*first.EscalatedAt) || later.PollCount <= first.PollCount {
		t.Fatalf("escalated_at %v -> %v, poll_count %d -> %d", first.EscalatedAt, later.EscalatedAt, first.PollCount, later.PollCount)
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 1 || e.escalationAlertCount(t, alertReasonDepositUnreferencedSubmitting, a.ID) != 1 {
		t.Fatalf("want exactly one audit and one alert")
	}
	if q := e.p.queriedRefs(); len(q) != 0 {
		t.Fatalf("a reference-less attempt has nothing to query, queried %v", q)
	}
}

// Lease bound: the window alone is not enough, the attempt must be older than lease + window.
func TestB7_UnreferencedDeposit_NotEscalatedBeforeLeasePlusWindow(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-lease")
	a := e.refLessLive(t, e.f, "b7-lease")
	e.setWindow(time.Nanosecond)
	setNextActionNow(t, pool, e.f.tenantID, a.ID)
	s := e.sweeper()
	s.Lease = 24 * time.Hour
	e.mustNoSweepErrors(t, s.RunOnce(context.Background(), []uuid.UUID{e.f.tenantID}))
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.EscalatedAt != nil || len(iwAlerts(t, e)) != 0 {
		t.Fatalf("lease + window not yet elapsed: escalated_at=%v", got.EscalatedAt)
	}
}

// Concurrency, callback first: a callback by merchant reference resolves the attempt before the
// tick: no escalation, one posting.
func TestB7_UnreferencedDeposit_CallbackByMerchantReferenceBeforeEscalation(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-cb1")
	a := e.refLessLive(t, e.f, "b7-cb1")
	e.setWindow(time.Nanosecond)
	ref := "b7-ref-" + uuid.NewString()
	d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"})
	if err != nil || d != DispositionApplied {
		t.Fatalf("callback: %v %v", d, err)
	}
	e.sweepUnreferenced(t, a)
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptSucceeded || got.EscalatedAt != nil || e.auditCount(t, b6EscalationAudit, a.ID) != 0 || len(iwAlerts(t, e)) != 0 {
		t.Fatalf("state=%s escalated_at=%v: a resolved attempt must not escalate", got.State, got.EscalatedAt)
	}
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// Concurrency, escalation first: the callback by merchant reference still resolves the
// escalated attempt, with exactly one posting.
func TestB7_UnreferencedDeposit_CallbackByMerchantReferenceAfterEscalation(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-cb2")
	a := e.refLessLive(t, e.f, "b7-cb2")
	e.setWindow(time.Nanosecond)
	e.sweepUnreferenced(t, a)
	if mustGetAttempt(t, pool, e.f.tenantID, a.ID).EscalatedAt == nil {
		t.Fatalf("setup: must be escalated")
	}
	ref := "b7-ref-" + uuid.NewString()
	ev := ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}
	if d, err := iwApplyReceiptInTx(t, e, ev); err != nil || d != DispositionApplied {
		t.Fatalf("callback: %v %v", d, err)
	}
	if d, err := iwApplyReceiptInTx(t, e, ev); err != nil || d == DispositionApplied {
		t.Fatalf("redelivery must be a non-applied no-op: %v %v", d, err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptSucceeded || got.EscalatedAt == nil {
		t.Fatalf("state=%s escalated_at=%v", got.State, got.EscalatedAt)
	}
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
	e.sweepUnreferenced(t, a) // terminal now: nothing
	if e.depositTxCount(t) != 1 {
		t.Fatalf("a later tick must not post")
	}
}

// Partial failure: a deterministic alert failure never aborts the unreferenced escalation.
func TestB7_UnreferencedDeposit_AlertDeterministicFailure_DoesNotAbortEscalation(t *testing.T) {
	pool := testPool(t)
	t.Run("in-tx failure, detached retry persists the P1", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b7-al1")
		a := e.refLessLive(t, e.f, "b7-al1")
		e.setWindow(time.Nanosecond)
		alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
		e.sweepUnreferenced(t, a)
		if got := b6AssertStillLiveNoMoney(t, e, a); got.EscalatedAt == nil || e.auditCount(t, b6EscalationAudit, a.ID) != 1 {
			t.Fatalf("the escalation must commit despite the alert failure")
		}
		iwParkAlert(t, e, a.ID, alertReasonDepositUnreferencedSubmitting)
	})
	t.Run("persistent failure", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b7-al2")
		a := e.refLessLive(t, e.f, "b7-al2")
		e.setWindow(time.Nanosecond)
		alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, "23514")
		e.sweepUnreferenced(t, a)
		if got := b6AssertStillLiveNoMoney(t, e, a); got.EscalatedAt == nil || e.auditCount(t, b6EscalationAudit, a.ID) != 1 {
			t.Fatalf("the escalation must commit despite a persistent alert failure")
		}
	})
}

// applyEvidenceInTx runs applyStatusEvidence for the attempt exactly as
// processViaQueryStatus would (intent lock, fresh re-read, alerting.InTx, flush after commit).
func (e *depRefEnv) applyEvidenceInTx(t *testing.T, a PaymentAttempt, gr GateResult[StatusResult]) error {
	t.Helper()
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, *a.DepositIntentID); err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, *a.DepositIntentID)
		if err != nil {
			return err
		}
		fresh, err := GetAttemptByID(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		return e.sweeper().applyStatusEvidence(ctx, tx, intent, fresh, gr)
	})
	if err == nil {
		pending.Flush(context.Background())
	}
	return err
}

// A transport failure resolving the query itself (the PSP is down) is still an escalation tick:
// the P1 must not depend on the PSP answering. It reschedules, never declines.
func TestB6_Escalation_TransportFailurePastWindow(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-tf")
	a, _ := e.ambiguousBound(t, "b6-tf")
	down := GateResult[StatusResult]{Err: errors.New("psp unreachable"), Class: ErrorClassAmbiguous}
	if err := e.applyEvidenceInTx(t, a, down); err != nil {
		t.Fatalf("inside the window: %v", err)
	}
	if mustGetAttempt(t, pool, e.f.tenantID, a.ID).EscalatedAt != nil {
		t.Fatalf("inside the window nothing escalates")
	}
	e.setWindow(time.Nanosecond)
	if err := e.applyEvidenceInTx(t, a, down); err != nil {
		t.Fatalf("past the window: %v", err)
	}
	if got := b6AssertStillLiveNoMoney(t, e, a); got.EscalatedAt == nil {
		t.Fatalf("a transport failure past the window must escalate")
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 1 || e.escalationAlertCount(t, alertReasonDepositSettlementWindowExceeded, a.ID) != 1 {
		t.Fatalf("want exactly one audit and one alert")
	}
}

// B7 concurrency on a STALE snapshot: the callback resolved the attempt after the sweeper read it
// and before the escalation transaction took the intent lock. The transaction re-reads, finds it
// terminal, and writes nothing (no reschedule on a terminal row, no escalation, no error).
func TestB7_UnreferencedDeposit_StaleSnapshotResolvedByCallback_WritesNothing(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-stale")
	a := e.refLessLive(t, e.f, "b7-stale")
	e.setWindow(time.Nanosecond)
	ref := "b7-ref-" + uuid.NewString()
	if d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}); err != nil || d != DispositionApplied {
		t.Fatalf("callback: %v %v", d, err)
	}
	before := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if err := e.unreferencedSweeper().processUnreferenced(context.Background(), e.f.tenantID, a); err != nil {
		t.Fatalf("a stale snapshot of a resolved attempt must be a clean no-op: %v", err)
	}
	after := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.EscalatedAt != nil || after.State != AttemptSucceeded {
		t.Fatalf("the terminal row was written")
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 0 || len(iwAlerts(t, e)) != 0 {
		t.Fatalf("no audit or alert for a resolved attempt")
	}
}

// C1 (security review): the escalation runs LAST. A Pending poll past the window that resolves
// the attempt through the T9 drain (deferred success receipt) posts exactly once and must NOT
// raise a spurious "settlement window exceeded" P1, audit or escalated_at.
func TestB6_Escalation_PendingPollThatDrainsDeferredSuccess_NoEscalation(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-c1")
	a, ref := e.phaseBCallbackThenAmbiguous(t, "b6-c1")
	e.setWindow(time.Nanosecond)
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pendingPoll(ref)))
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptSucceeded || got.EscalatedAt != nil {
		t.Fatalf("state=%s escalated_at=%v, want succeeded and never escalated", got.State, got.EscalatedAt)
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 0 || len(iwAlerts(t, e)) != 0 {
		t.Fatalf("a poll that resolved the attempt must not audit or alert an escalation")
	}
	assertDeferredReceiptResolved(t, pool, e.f.tenantID, ref, a.ID)
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// receiptResolutions returns "resolution" of every stored receipt naming ref (and counts the
// unresolved ones).
func (e *depRefEnv) receiptResolutions(t *testing.T, ref string) (resolutions []string, unresolved int) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT COALESCE(resolution, ''), resolved_at IS NULL FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2 ORDER BY id`, e.f.tenantID, ref)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			var open bool
			if err := rows.Scan(&r, &open); err != nil {
				return err
			}
			resolutions = append(resolutions, r)
			if open {
				unresolved++
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	return resolutions, unresolved
}

// phaseBDeferred drives a deposit whose phase B receives a verified callback (stored
// deferred_unresolved) before the adapter answers Ambiguous WITH the callback's reference.
func (e *depRefEnv) phaseBDeferred(t *testing.T, key string, outcome Outcome, amount int64, declineReason string) (InitiateDepositAttemptResult, string) {
	t.Helper()
	ref := "defer-ref-" + uuid.NewString()
	var cbRes ReceiveCallbackResult
	var cbErr error
	e.p.setScript(func(req DepositRequest) DepositResult {
		cbRes, cbErr = rvCallback(e.pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", outcome, amount, "EUR", declineReason, false))
		return DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}
	})
	res := rvInit(t, e.pool, e.orch, e.f, 5000, key)
	if cbErr != nil || cbRes.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("phase B callback: disposition=%s err=%v, want deferred_unresolved", cbRes.Disposition, cbErr)
	}
	return res, ref
}

// LF-C1: ambiguous x deferred DECLINE (ADR 0095 4.4 T8) resolves at T6: the attempt is
// declined with its stage, the intent is declined, the receipt is applied, nothing posts.
func TestB6_T6Drain_DeferredDecline_DeclinesAttemptAndIntent_ReceiptApplied(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t6dd")
	res, ref := e.phaseBDeferred(t, "b6-t6dd", OutcomeDeclined, 5000, "insufficient_funds")
	got := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if got.State != AttemptDeclined || got.DeclineStage == nil || got.DeclineReason == nil {
		t.Fatalf("state=%s stage=%v reason=%v, want declined with its stage and reason", got.State, got.DeclineStage, got.DeclineReason)
	}
	if got.ProviderReference == nil || *got.ProviderReference != ref {
		t.Fatalf("the reference T6 bound must stay %q, got %v", ref, got.ProviderReference)
	}
	if s := depIntentStatus(t, e, e.f, *res.Attempt.DepositIntentID); s != string(DepositIntentDeclined) {
		t.Fatalf("intent status = %q, want declined", s)
	}
	if rs, open := e.receiptResolutions(t, ref); open != 0 || len(rs) != 1 || rs[0] != string(ResolutionApplied) {
		t.Fatalf("receipts = %v unresolved=%d, want one resolution 'applied'", rs, open)
	}
	if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("a decline must never post")
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// LF-C1: a deferred success whose amount mismatches the attempt is a T10 at T6: disputed
// callback_amount_asset_mismatch, one P1, no posting, the T6-bound reference left as bound.
func TestB6_T6Drain_DeferredMismatchedAmount_ParksOnceNoPosting(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t6mm")
	res, ref := e.phaseBDeferred(t, "b6-t6mm", OutcomeSucceeded, 4999, "")
	got := mustGetAttempt(t, pool, e.f.tenantID, res.Attempt.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != TerminalReasonCallbackAmountAssetMismatch {
		t.Fatalf("state=%s reason=%v, want disputed/%s", got.State, got.TerminalReason, TerminalReasonCallbackAmountAssetMismatch)
	}
	if got.ProviderReference == nil || *got.ProviderReference != ref {
		t.Fatalf("bound reference = %v, want %q (already bound by T6)", got.ProviderReference, ref)
	}
	iwParkAlert(t, e, got.ID, TerminalReasonCallbackAmountAssetMismatch, ref)
	if rs, open := e.receiptResolutions(t, ref); open != 0 || len(rs) != 1 {
		t.Fatalf("receipts = %v unresolved=%d, want one resolved", rs, open)
	}
	if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("a mismatch must never post")
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// NOTE (like the park tests' F2a note): "the fault fires after the posting" depends on
// ResolveReceipt being the drain's LAST write, after the posting and the intent projection
// update. If that order in ApplyDeferredReceiptsForAttempt ever changes, re-check the injection
// point (blocking payment_provider_events) so the rollback assertion does not become trivial.
//
// LF-C1: fault injection AFTER the T6 drain posted. The receipt resolution (the drain's last
// write) is made to fail with a lock timeout; the whole phase C transaction rolls back: the
// attempt is back at 'submitting' with no reference, no posting, the receipt still unresolved,
// the ledger balanced; re-driving the same adapter result then converges with one posting.
func TestB6_T6Drain_FaultAfterPosting_RollsBackCompletelyThenRedrives(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t6fi")
	ref := "defer-ref-" + uuid.NewString()
	intentID := insertRawDepositIntent(t, pool, e.f, "pending")
	attemptID, claim := uuid.New(), uuid.New()
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: attemptID, TenantID: e.f.tenantID, Operation: AttemptOperationDeposit, DepositIntentID: &intentID,
			AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		}); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, attemptID, e.id, claim, "b6-t6fi", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("seed claimed attempt: %v", err)
	}
	// The receipt must arrive AFTER the submission started (S95-C3: an earlier one predates it).
	cb, err := rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
	if err != nil || cb.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("seed deferred receipt: %v %v", cb.Disposition, err)
	}
	gr := GateResult[DepositResult]{Class: ErrorClassAmbiguous, Value: DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}}
	before := e.snapshotPark(t, attemptID, intentID)
	if before.attemptState != AttemptSubmitting {
		t.Fatalf("setup: state=%s", before.attemptState)
	}

	blocker := e.holdTableShare(t, "payment_provider_events")
	if err := e.phaseCVictim(attemptID, intentID, claim, gr, true); !isLockNotAvailable(err) {
		t.Fatalf("injected failure after the drain posted: got %v, want SQLSTATE 55P03", err)
	}
	blocker.release()
	e.assertRolledBack(t, "after the injected failure", before, e.snapshotPark(t, attemptID, intentID))
	if rs, open := e.receiptResolutions(t, ref); open != 1 || len(rs) != 1 {
		t.Fatalf("the receipt must stay unresolved after the rollback: %v open=%d", rs, open)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, attemptID); got.ProviderReference != nil || got.LedgerTransactionID != nil {
		t.Fatalf("rollback must unbind the reference and the ledger link: %v %v", got.ProviderReference, got.LedgerTransactionID)
	}
	if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("no posting may survive the rollback")
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)

	if err := e.phaseCVictim(attemptID, intentID, claim, gr, false); err != nil {
		t.Fatalf("re-drive: %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, attemptID); got.State != AttemptSucceeded {
		t.Fatalf("re-driven state=%s, want succeeded", got.State)
	}
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	if rs, open := e.receiptResolutions(t, ref); open != 0 || len(rs) != 1 || rs[0] != string(ResolutionApplied) {
		t.Fatalf("receipts after the re-drive: %v open=%d", rs, open)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// T12 re-submission keeps the bound reference X (ResubmitAmbiguous moves ambiguous -> submitting
// without touching it). If the adapter's next answer names a DIFFERENT reference Y, T6 keeps X
// (COALESCE) and the drain must run against X, never Y: the deferred success stored under Y is NOT
// applied, nothing posts, the ledger balances. (Reachable by a test, not by production today: only
// payout_sweep.go calls ResubmitAmbiguous. Any future deposit T12 caller must land with this test.)
func TestB6_T6Drain_AfterT12Resubmit_DrainsAgainstKeptReferenceNotAdapterReference(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-t12")
	a, refX := e.ambiguousBound(t, "b6-t12")
	claim := uuid.New()
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ResubmitAmbiguous(ctx, tx, a.ID, claim, "b6-t12", time.Now().Add(time.Minute), 0)
	}); err != nil {
		t.Fatalf("T12: %v", err)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSubmitting || got.ProviderReference == nil || *got.ProviderReference != refX {
		t.Fatalf("setup: state=%s ref=%v, want submitting keeping %q", got.State, got.ProviderReference, refX)
	}
	refY := "defer-ref-Y-" + uuid.NewString()
	cb, err := rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, refY, "", OutcomeSucceeded, 5000, "EUR", "", false))
	if err != nil || cb.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("seed deferred receipt under Y: %v %v", cb.Disposition, err)
	}
	gr := GateResult[DepositResult]{Class: ErrorClassAmbiguous, Value: DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: refY}}
	if err := e.phaseCVictim(a.ID, *a.DepositIntentID, claim, gr, false); err != nil {
		t.Fatalf("phase C: %v", err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptAmbiguous || got.ProviderReference == nil || *got.ProviderReference != refX {
		t.Fatalf("state=%s ref=%v, want ambiguous with X=%q kept", got.State, got.ProviderReference, refX)
	}
	if rs, open := e.receiptResolutions(t, refY); open != 1 || len(rs) != 1 {
		t.Fatalf("the Y receipt must stay unresolved (never applied against X's attempt): %v open=%d", rs, open)
	}
	if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("nothing may post")
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// LF-C2 (kills M5d): a callback by merchant reference can resolve a reference-less attempt
// WITHOUT binding a reference (the tombstone T10 path). A stale snapshot reaching
// processUnreferenced afterwards must be a clean no-op: no escalation, and no reschedule
// attempt on the terminal row.
func TestB7_UnreferencedDeposit_StaleSnapshot_TerminalWithoutReference_WritesNothing(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-tomb")
	a := e.refLessLive(t, e.f, "b7-tomb")
	e.setWindow(time.Nanosecond)
	ref := "b7-tomb-ref-" + uuid.NewString()
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := postDepositReversalTombstone(ctx, tx, e.f.tenantID, e.id, ref)
		return err
	}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	if d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: a.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}); err != nil || d != DispositionApplied {
		t.Fatalf("callback: %v %v", d, err)
	}
	before := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if before.State != AttemptDisputed || before.ProviderReference != nil {
		t.Fatalf("setup: state=%s ref=%v, want disputed and still reference-less", before.State, before.ProviderReference)
	}
	if err := e.unreferencedSweeper().processUnreferenced(context.Background(), e.f.tenantID, a); err != nil {
		t.Fatalf("a stale snapshot of a terminal reference-less attempt must be a clean no-op: %v", err)
	}
	after := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.EscalatedAt != nil {
		t.Fatalf("the terminal row was written")
	}
	if e.auditCount(t, b6EscalationAudit, a.ID) != 0 {
		t.Fatalf("no escalation audit for a resolved attempt")
	}
}

// A reference bound after the sweeper read its snapshot (T4/T9 by a concurrent path): the stale
// reference-less snapshot must neither escalate nor reschedule; the next tick polls it normally.
func TestB7_UnreferencedDeposit_StaleSnapshot_ReferenceBoundSince_WritesNothing(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b7-bound")
	a := e.refLessLive(t, e.f, "b7-bound")
	e.setWindow(time.Nanosecond)
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ref := "b7-bound-" + uuid.NewString()
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET provider_reference = $2 WHERE id = $1 AND provider_reference IS NULL`, a.ID, ref)
		return err
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	before := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if err := e.unreferencedSweeper().processUnreferenced(context.Background(), e.f.tenantID, a); err != nil {
		t.Fatalf("processUnreferenced: %v", err)
	}
	after := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.EscalatedAt != nil || e.auditCount(t, b6EscalationAudit, a.ID) != 0 {
		t.Fatalf("a snapshot whose reference was bound since must write nothing")
	}
}

// A1: a manifest value above the ceiling (set after registration) is clamped, never used raw.
func TestA1_DepositSettlementWindow_ClampedAndDefaulted(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-a1-clamp")
	a, _ := e.ambiguousBound(t, "a1-clamp")
	e.setWindow(MaxSettlementWindow * 10)
	if w := e.sweeper().depositSettlementWindow(a); w != MaxSettlementWindow {
		t.Fatalf("window = %v, want clamp to %v", w, MaxSettlementWindow)
	}
	e.setWindow(0)
	if w := e.sweeper().depositSettlementWindow(a); w != DefaultSettlementWindow {
		t.Fatalf("window = %v, want default %v", w, DefaultSettlementWindow)
	}
}

// The legacy-shape helper binds the intent's reference too (C2).
func TestB6_LegacyShapeHelper_BindsIntentReference(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-c2")
	a, ref := e.phaseBCallbackThenAmbiguous(t, "b6-c2")
	if got := depScan[string](t, pool, e.f.tenantID, `SELECT COALESCE(provider_reference, '') FROM deposit_intents WHERE id = $1`, *a.DepositIntentID); got != ref {
		t.Fatalf("intent provider_reference = %q, want %q", got, ref)
	}
}

// The discriminator reason is closed: an unlisted reason never reaches an alert verbatim.
func TestB6B7_RaiseEscalationAlert_UnlistedReasonBecomesUnclassified(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b6-unc")
	a, _ := e.ambiguousBound(t, "b6-unc")
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		return raiseDepositEscalationAlert(ctx, tx, a, "provider said: card 4111")
	})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	pending.Flush(context.Background())
	iwParkAlert(t, e, a.ID, alertReasonUnclassified, "4111")
}
