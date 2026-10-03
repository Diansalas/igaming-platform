//go:build integration

// PRH-2 D: PAY-POLL-AMOUNT-1 + FH7-06 (the status-poll success branch compares
// amount, asset and echoed reference before it posts), with its ride-alongs
// PAY-F3SM-TEST-1 and PAY-SWEEP-CAS-NOISE-1, PAY-DEFERRED-RECEIPT-SYNC-1 (drain
// deferred receipts wherever a reference is bound or resolved), LF F-C4 (the
// binding pre-check also covers non-tombstone ledger transactions) and QA C3
// (fault injection on the park paths).
//
// Test rules (plan §5.0): no time.Sleep and no wall-clock assertion. Poll results
// are scripted through depRefProvider (QueryStatus overrides), a callback is
// interleaved INSIDE QueryStatus (phase B of a poll, no tx held) through the
// onQuery hook, and failures are injected with a table lock plus lock_timeout.
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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// --- helpers -----------------------------------------------------------------

func (e *depRefEnv) sweeper() *Sweeper {
	return NewSweeper(e.pool, e.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
}

// ambiguousBound drives a real deposit to 'ambiguous' with its provider
// reference bound (a sync success with no amount echo, LF F-C1): the attempt a
// poll can then resolve. Returns the attempt and the bound reference.
func (e *depRefEnv) ambiguousBound(t *testing.T, key string) (PaymentAttempt, string) {
	t.Helper()
	ref := "poll-ref-" + uuid.NewString()
	e.p.setScript(scriptSyncEcho(ref, 0, "EUR"))
	res := rvInit(t, e.pool, e.orch, e.f, 5000, key)
	a := mustGetAttempt(t, e.pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptAmbiguous || a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Fatalf("setup: state=%s ref=%v, want ambiguous with %q bound", a.State, a.ProviderReference, ref)
	}
	return a, ref
}

// poll scripts the status the provider reports for ref, makes the attempt due
// and runs one sweep.
func (e *depRefEnv) poll(t *testing.T, a PaymentAttempt, ref string, st StatusResult) SweepStats {
	t.Helper()
	e.p.setStatus(ref, st)
	setNextActionNow(t, e.pool, e.f.tenantID, a.ID)
	return e.sweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID})
}

func pollSuccess(ref string, amount int64, asset string) StatusResult {
	return StatusResult{ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: amount, AssetCode: asset}
}

func (e *depRefEnv) mustNoSweepErrors(t *testing.T, st SweepStats) {
	t.Helper()
	if len(st.Errors) != 0 {
		t.Fatalf("sweep errors: %v", st.Errors)
	}
}

func (e *depRefEnv) auditCount(t *testing.T, action string, attemptID uuid.UUID) int64 {
	t.Helper()
	return depScan[int64](t, e.pool, e.f.tenantID,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`, e.f.tenantID, action, attemptID.String())
}

func (e *depRefEnv) depositTxCount(t *testing.T) int64 {
	t.Helper()
	return depScan[int64](t, e.pool, e.f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, e.f.tenantID)
}

// deliverCallbackOnce interleaves a verified callback inside the poll's own
// QueryStatus (no transaction held there), exactly once.
func (e *depRefEnv) deliverCallbackOnce(t *testing.T, outcome Outcome, amount int64, declineReason string) {
	t.Helper()
	var once sync.Once
	e.p.setOnQuery(func(ref string) {
		once.Do(func() {
			if _, err := rvCallback(e.pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", outcome, amount, "EUR", declineReason, false)); err != nil {
				t.Errorf("interleaved callback: %v", err)
			}
		})
	})
}

// assertDeferredReceiptResolved: exactly one stored receipt names ref, it was
// stored deferred_unresolved (immutable) and has since been RESOLVED against the
// attempt (resolved_at set, resolution 'applied' = the duplicate-effect cell for
// an already-succeeded attempt, attempt_id set). Asserted, never logged (QA C
// re-review C1).
func assertDeferredReceiptResolved(t *testing.T, pool *db.Pool, tenantID uuid.UUID, ref string, attemptID uuid.UUID) {
	t.Helper()
	var total, unresolved int64
	var disposition, resolution string
	var linked uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*), count(*) FILTER (WHERE resolved_at IS NULL),
			        COALESCE(max(disposition_at_receipt), ''), COALESCE(max(resolution), ''), COALESCE(max(attempt_id::text)::uuid, '00000000-0000-0000-0000-000000000000')
			   FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference = $2`, tenantID, ref).
			Scan(&total, &unresolved, &disposition, &resolution, &linked)
	}); err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	if total != 1 || disposition != string(DispositionDeferredUnresolved) {
		t.Fatalf("setup: receipts=%d disposition_at_receipt=%q, want exactly one stored deferred_unresolved", total, disposition)
	}
	if unresolved != 0 {
		t.Fatalf("the deferred receipt is still unresolved (resolved_at IS NULL): it would age into pay_unresolved")
	}
	if resolution != string(ResolutionApplied) || linked != attemptID {
		t.Fatalf("receipt resolution=%q attempt_id=%s, want applied against attempt %s", resolution, linked, attemptID)
	}
}

// --- PAY-POLL-AMOUNT-1: amount and asset --------------------------------------

func TestPollAmount_Mismatch_ParksNoPosting(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name  string
		amt   int64
		asset string
	}{
		{"one under", 4999, "EUR"},
		{"one over", 5001, "EUR"},
		{"wrong asset", 5000, "USD"},
		{"negative", -5000, "EUR"},
		{"both wrong", 1, "USD"},
		// Partial evidence that already CONTRADICTS the record is a mismatch, never
		// Missing: on the poll path nothing decides after it (code review of C, F5).
		{"contradicting amount, asset omitted", 4999, ""},
		{"contradicting asset, amount omitted", 0, "USD"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-am-mm"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "d1-am-mm")
			st := e.poll(t, a, ref, pollSuccess(ref, c.amt, c.asset))
			e.mustNoSweepErrors(t, st)
			e.assertPollParked(t, a, ref, TerminalReasonPollAmountMismatch)
			if got := depScan[int64](t, pool, e.f.tenantID,
				`SELECT (metadata->>'provider_amount')::bigint FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				e.f.tenantID, a.ID.String()); got != c.amt {
				t.Errorf("audit provider_amount = %d, want %d", got, c.amt)
			}
		})
	}
}

// assertPollParked asserts the shared post-conditions of every poll T10: terminal
// 'disputed' with the reason, the reference still bound, exactly one dispute audit
// carrying adapter_outcome=succeeded, no ledger transaction of any kind, an
// untouched balance, a balanced ledger with a projection that matches its rebuild
// (drift zero), the intent ambiguous (never declined: funds may be captured),
// no cascade child, nothing left for the sweeper, and a sweeper pass that neither
// errors nor touches the parked attempt.
func (e *depRefEnv) assertPollParked(t *testing.T, a PaymentAttempt, ref, wantReason string) PaymentAttempt {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || depTerminalReason(got) != wantReason {
		t.Fatalf("attempt state=%s reason=%s, want disputed/%s", got.State, depTerminalReason(got), wantReason)
	}
	if got.ProviderReference == nil || *got.ProviderReference != ref {
		t.Errorf("the bound reference changed: %v, want %q", got.ProviderReference, ref)
	}
	if n := depDisputeAudits(t, e, e.f, a.ID); n != 1 {
		t.Errorf("payment.attempt_disputed audits = %d, want exactly 1", n)
	}
	if o := depScan[string](t, e.pool, e.f.tenantID,
		`SELECT metadata->>'adapter_outcome' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
		e.f.tenantID, a.ID.String()); o != string(OutcomeSucceeded) {
		t.Errorf("audit adapter_outcome=%q, want succeeded", o)
	}
	if n := depLedgerTxCount(t, e, e.f); n != 0 {
		t.Errorf("a T10 must never produce a ledger transaction, found %d", n)
	}
	if b := cashBalance(t, e.pool, e.f); b != 0 {
		t.Errorf("player balance = %d, want 0", b)
	}
	assertLedgerBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
	if s := depIntentStatus(t, e, e.f, *a.DepositIntentID); s != string(DepositIntentAmbiguous) {
		t.Errorf("intent status = %q, want ambiguous", s)
	}
	if n := depAttemptCount(t, e, e.f, *a.DepositIntentID); n != 1 {
		t.Errorf("attempts for the intent = %d, want 1 (a park must not cascade)", n)
	}
	if !depNextActionNull(t, e, e.f, a.ID) {
		t.Errorf("a parked attempt must have no next_action_at")
	}
	if st := e.sweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID}); len(st.Errors) != 0 {
		t.Errorf("sweeper errors after a park: %v", st.Errors)
	}
	if after := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); !after.UpdatedAt.Equal(got.UpdatedAt) {
		t.Errorf("the sweeper touched a parked attempt")
	}
	return got
}

// What "Missing" means on the poll path (ADR 0095 §35.2): a success with no usable
// amount evidence NEVER posts, is NOT a dispute, keeps the attempt live, and is
// audited on every poll so a provider that never echoes an amount is visible.
func TestPollAmount_Missing_NeverPosts_StaysLiveAndAudited(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name  string
		amt   int64
		asset string
	}{
		{"no amount", 0, "EUR"},
		{"no asset", 5000, ""},
		{"neither", 0, ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-am-miss"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "d1-am-miss")
			e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, c.amt, c.asset)))

			got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
			if got.State != AttemptAmbiguous {
				t.Fatalf("state=%s, want ambiguous (stays live)", got.State)
			}
			if got.TerminalReason != nil {
				t.Errorf("Missing is not a dispute, terminal_reason=%s", depTerminalReason(got))
			}
			if e.depositTxCount(t) != 0 || depLedgerTxCount(t, e, e.f) != 0 || cashBalance(t, pool, e.f) != 0 {
				t.Errorf("Missing amount evidence must never post")
			}
			if depDisputeAudits(t, e, e.f, a.ID) != 0 {
				t.Errorf("Missing must not write a dispute audit")
			}
			if n := e.auditCount(t, "payment.attempt_poll_amount_unconfirmed", a.ID); n != 1 {
				t.Errorf("unconfirmed-poll audits = %d, want 1", n)
			}
			if depNextActionNull(t, e, e.f, a.ID) {
				t.Errorf("a live attempt must stay scheduled")
			}
			// Liveness: the next poll that DOES carry evidence resolves it.
			e.mustNoSweepErrors(t, e.poll(t, got, ref, pollSuccess(ref, 5000, "EUR")))
			if f := mustGetAttempt(t, pool, e.f.tenantID, a.ID); f.State != AttemptSucceeded {
				t.Fatalf("after a poll with evidence: state=%s, want succeeded", f.State)
			}
			if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
				t.Errorf("deposit postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
			}
		})
	}
}

// --- FH7-06: the echoed reference and the posting key -------------------------

func TestPollReference_EmptyOrMatchingEcho_PostsUnderTheBoundReference(t *testing.T) {
	pool := testPool(t)
	for _, c := range []struct {
		name string
		echo func(ref string) string
	}{
		{"empty echo", func(string) string { return "" }},
		{"matching echo", func(ref string) string { return ref }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-ref-ok-"+strings.ReplaceAll(c.name, " ", ""))
			a, ref := e.ambiguousBound(t, "d1-ref-ok")
			e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(c.echo(ref), 5000, "EUR")))
			got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
			if got.State != AttemptSucceeded || got.ProviderReference == nil || *got.ProviderReference != ref {
				t.Fatalf("state=%s ref=%v, want succeeded on %q", got.State, got.ProviderReference, ref)
			}
			// The posting key is the BOUND reference: never "", never the echo.
			var ptx, idem string
			if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT provider_tx_id, idempotency_key FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, e.f.tenantID).Scan(&ptx, &idem)
			}); err != nil {
				t.Fatalf("read the posting: %v", err)
			}
			if ptx != ref || idem != e.id+":"+ref {
				t.Errorf("posting provider_tx_id=%q idempotency_key=%q, want %q / %q", ptx, idem, ref, e.id+":"+ref)
			}
			if cashBalance(t, pool, e.f) != 5000 {
				t.Errorf("balance=%d, want 5000", cashBalance(t, pool, e.f))
			}
			assertLedgerBalanced(t, pool, e.f.tenantID)
		})
	}
}

func TestPollReference_DifferentNonEmptyEcho_ParksNoPosting(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name       string
		echo       string
		wantInMeta string // must appear in the audit metadata
		notInMeta  string // must NOT appear
	}{
		{"valid other reference", "other-ref-VALID1", "other-ref-VALID1", ""},
		{"control character", "ref\x07bellX", "echo_ref_len", "bellX"},
		{"oversize", strings.Repeat("z", providerrefMax+1), "echo_ref_len", strings.Repeat("z", 40)},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-ref-mm"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "d1-ref-mm")
			e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(c.echo, 5000, "EUR")))
			e.assertPollParked(t, a, ref, TerminalReasonPollReferenceMismatch)
			meta := depScan[string](t, pool, e.f.tenantID,
				`SELECT metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`, e.f.tenantID, a.ID.String())
			if !strings.Contains(meta, c.wantInMeta) {
				t.Errorf("audit metadata lacks %q: %s", c.wantInMeta, meta)
			}
			if c.notInMeta != "" && strings.Contains(meta, c.notInMeta) {
				t.Errorf("audit metadata leaks the invalid echo: %s", meta)
			}
		})
	}
}

// Order (§34.8 / plan D): the amount comparison decides before the reference one.
func TestPollOrder_AmountMismatchDecidesBeforeReferenceMismatch(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-d1-order")
	a, ref := e.ambiguousBound(t, "d1-order")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess("some-other-ref", 4999, "EUR")))
	e.assertPollParked(t, a, ref, TerminalReasonPollAmountMismatch)
}

// A reversal tombstone on the BOUND reference disputes the poll success. The
// lookup uses the bound reference, so an EMPTY echo cannot skip it (FH7-06).
func TestPollTombstone_OnBoundReference_DisputesEvenWithAnEmptyEcho(t *testing.T) {
	pool := testPool(t)
	for _, c := range []struct {
		name string
		echo func(ref string) string
	}{
		{"empty echo", func(string) string { return "" }},
		{"matching echo", func(ref string) string { return ref }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-tomb-"+strings.ReplaceAll(c.name, " ", ""))
			a, ref := e.ambiguousBound(t, "d1-tomb")
			if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := postDepositReversalTombstone(ctx, tx, e.f.tenantID, e.id, ref)
				return err
			}); err != nil {
				t.Fatalf("seed tombstone: %v", err)
			}
			e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(c.echo(ref), 5000, "EUR")))
			got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
			if got.State != AttemptDisputed || depTerminalReason(got) != TerminalReasonTombstonePrecedesSuccess {
				t.Fatalf("state=%s reason=%s, want disputed/reversal_tombstone_precedes_success", got.State, depTerminalReason(got))
			}
			if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
				t.Errorf("a tombstoned poll success must never post")
			}
			if n := depDisputeAudits(t, e, e.f, a.ID); n != 1 {
				t.Errorf("dispute audits = %d, want exactly 1 (unified through parkDepositAttempt)", n)
			}
			if s := depIntentStatus(t, e, e.f, *a.DepositIntentID); s != string(DepositIntentAmbiguous) {
				t.Errorf("intent status = %q, want ambiguous (recomputed)", s)
			}
			assertLedgerBalanced(t, pool, e.f.tenantID)
			loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
		})
	}
}

// --- LF F-C4: a non-tombstone ledger transaction already holds the key ---------

type fc4Env struct {
	*depRefEnv
	pf payoutFixture
}

func newFC4Env(t *testing.T, providerID string) *fc4Env {
	t.Helper()
	pool := testPool(t)
	pf := seedPayoutFixture(t, pool, 10_000, true)
	mp := NewMockProvider(providerID, "EUR")
	mp.SetManifest(OperationManifest{SupportsDeposit: true, StatusQuery: "by_provider_reference", IdempotentSubmission: true, SyncSuccessPossible: true})
	p := &depRefProvider{MockProvider: mp}
	registerCapability(t, pool, pf.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{providerID: p}, MultiWebhookCredentialResolver{providerID: NewMockWebhookCredentials(mp)})
	return &fc4Env{depRefEnv: &depRefEnv{pool: pool, f: pf.orchFixture, p: p, orch: orch, id: providerID}, pf: pf}
}

// completePayoutWithSettlementRef runs a real payout to Step B at the SAME PSP
// with the given settlement reference: withdrawal_completed with provider_tx_id =
// settleRef, a key bound to no payment_attempts row at all.
func (e *fc4Env) completePayoutWithSettlementRef(t *testing.T, idemKey, settleRef string) {
	t.Helper()
	wr := approvedWithdrawal(t, e.pool, e.pf, 500, idemKey)
	if _, err := e.orch.ClaimForDispatch(context.Background(), e.pool, KYCEnforcementPayoutGate{}, e.f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.Complete(ctx, tx, wr.ID, e.id, settleRef)
	}); err != nil {
		t.Fatalf("withdrawal.Complete (Step B): %v", err)
	}
	if n := depScan[int64](t, e.pool, e.f.tenantID,
		`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_completed' AND provider_id = $2 AND provider_tx_id = $3`,
		e.f.tenantID, e.id, settleRef); n != 1 {
		t.Fatalf("setup: Step B postings for %q = %d, want 1", settleRef, n)
	}
}

func (e *fc4Env) assertParkedOnLedgerKey(t *testing.T, attemptID, intentID uuid.UUID, depositTxsBefore int64) {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, attemptID)
	if got.State != AttemptDisputed || depTerminalReason(got) != TerminalReasonProviderReferenceConflict {
		t.Fatalf("attempt state=%s reason=%s, want disputed/provider_reference_conflict", got.State, depTerminalReason(got))
	}
	if op := depScan[string](t, e.pool, e.f.tenantID,
		`SELECT metadata->>'bound_to_operation' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
		e.f.tenantID, attemptID.String()); op != "ledger_withdrawal_completed" {
		t.Errorf("audit bound_to_operation=%q, want ledger_withdrawal_completed", op)
	}
	if n := ledgerDepositTxCount(t, e.pool, e.f.tenantID, intentID); n != 0 {
		t.Errorf("the parked intent has %d deposit postings, want 0", n)
	}
	if n := e.depositTxCount(t); n != depositTxsBefore {
		t.Errorf("deposit-type ledger transactions changed: %d, want %d", n, depositTxsBefore)
	}
	assertLedgerBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
}

func TestFC4_SyncSuccessWhoseReferenceIsAPayoutStepBKey_ParksNotErrorLoop(t *testing.T) {
	e := newFC4Env(t, "mock-d1-fc4-sync")
	settle := "settle-" + uuid.NewString()
	e.completePayoutWithSettlementRef(t, "d1-fc4-sync", settle)
	before := e.depositTxCount(t)
	e.p.setScript(scriptSyncEcho(settle, 5000, "EUR"))
	res := rvInit(t, e.pool, e.orch, e.f, 5000, "d1-fc4-sync-dep") // must not error
	e.assertParkedOnLedgerKey(t, res.Attempt.ID, res.Intent.ID, before)
	if res.RedirectURL != "" || res.HostedFieldToken != "" {
		t.Errorf("a parked attempt must return no redirect/token")
	}
}

func TestFC4_PollSuccessWhoseBoundReferenceBecameAPayoutStepBKey_ParksNotErrorLoop(t *testing.T) {
	e := newFC4Env(t, "mock-d1-fc4-poll")
	a, ref := e.ambiguousBound(t, "d1-fc4-poll-dep")
	// The payout's settlement reference equals the reference this deposit is bound to.
	e.completePayoutWithSettlementRef(t, "d1-fc4-poll", ref)
	before := e.depositTxCount(t)
	st := e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR"))
	e.mustNoSweepErrors(t, st) // before F-C4 this looped on ErrIdempotencyPayloadMismatch
	e.assertParkedOnLedgerKey(t, a.ID, *a.DepositIntentID, before)
}

// --- the poll racing a verified callback (run with -count=50 -race) -------------

func TestPollSuccess_RacingCallback_PostsExactlyOnce(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-d1-race")
	a, ref := e.ambiguousBound(t, "d1-race")
	e.p.setStatus(ref, pollSuccess(ref, 5000, "EUR"))
	setNextActionNow(t, pool, e.f.tenantID, a.ID)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var sweepStats SweepStats
	var cbErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		sweepStats = e.sweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, cbErr = rvCallback(pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
	}()
	close(start)
	wg.Wait()

	if cbErr != nil {
		t.Fatalf("callback: %v", cbErr)
	}
	e.mustNoSweepErrors(t, sweepStats)
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded", got.State)
	}
	if n := ledgerDepositTxCount(t, pool, e.f.tenantID, *a.DepositIntentID); n != 1 {
		t.Fatalf("deposit postings = %d, want exactly 1", n)
	}
	if b := cashBalance(t, pool, e.f); b != 5000 {
		t.Fatalf("balance=%d, want 5000", b)
	}
	if depDisputeAudits(t, e, e.f, a.ID) != 0 {
		t.Fatalf("a poll racing a matching callback must not dispute")
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
}

// --- PAY-F3SM-TEST-1 -----------------------------------------------------------

// A callback succeeds the attempt while the poll is in flight; the poll then
// reports a DIFFERENT amount. The sweeper maps it onto the succeeded x mismatched
// cell: an audit, no state change, no posting (mutant F3SM drops the audit).
func TestF3SM_MismatchedPollAgainstAnAlreadySucceededAttempt_AuditsTheContradiction(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-d1-f3sm")
	a, ref := e.ambiguousBound(t, "d1-f3sm")
	e.deliverCallbackOnce(t, OutcomeSucceeded, 5000, "")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 4999, "EUR")))

	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded (no state change)", got.State)
	}
	if n := e.auditCount(t, "payments.callback_amount_asset_mismatch_terminal", a.ID); n != 1 {
		t.Fatalf("terminal mismatch audits = %d, want exactly 1", n)
	}
	if got := depScan[int64](t, pool, e.f.tenantID,
		`SELECT (metadata->>'echoed_amount')::bigint FROM audit_log WHERE tenant_id = $1 AND action = 'payments.callback_amount_asset_mismatch_terminal' AND target_id = $2`,
		e.f.tenantID, a.ID.String()); got != 4999 {
		t.Errorf("audit echoed_amount=%d, want 4999", got)
	}
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Errorf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
}

// The same cell with an echo that merely omits the amount is not a contradiction.
func TestF3SM_PollWithoutAmountAgainstSucceededAttempt_IsNotAContradiction(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-d1-f3sm-miss")
	a, ref := e.ambiguousBound(t, "d1-f3sm-miss")
	e.deliverCallbackOnce(t, OutcomeSucceeded, 5000, "")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "")))
	if n := e.auditCount(t, "payments.callback_amount_asset_mismatch_terminal", a.ID); n != 0 {
		t.Fatalf("an echo without an amount was reported as a contradiction (%d audits)", n)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded", got.State)
	}
}

// declined (the T13 shape): a contradicting poll success is audited, not parked
// (there is no live->disputed transition from declined, §4.4); a matching one is
// the intent's first success and posts under the bound reference (T13).
func TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13(t *testing.T) {
	pool := testPool(t)
	t.Run("contradicting success is audited only", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-d1-decl-mm")
		a, ref := e.ambiguousBound(t, "d1-decl-mm")
		e.deliverCallbackOnce(t, OutcomeDeclined, 5000, "provider_unavailable")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 4999, "EUR")))
		got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
		if got.State != AttemptDeclined {
			t.Fatalf("state=%s, want declined (unchanged)", got.State)
		}
		if n := e.auditCount(t, "payments.poll_evidence_contradicts_terminal_attempt", a.ID); n != 1 {
			t.Errorf("terminal-contradiction audits = %d, want 1", n)
		}
		if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
			t.Errorf("a contradicting success must not post")
		}
	})
	t.Run("matching success posts under the bound reference", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-d1-decl-ok")
		a, ref := e.ambiguousBound(t, "d1-decl-ok")
		e.deliverCallbackOnce(t, OutcomeDeclined, 5000, "provider_unavailable")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess("", 5000, "EUR")))
		got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
		if got.State != AttemptSucceeded || got.ProviderReference == nil || *got.ProviderReference != ref {
			t.Fatalf("state=%s ref=%v, want succeeded on %q (T13)", got.State, got.ProviderReference, ref)
		}
		if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
			t.Errorf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
		}
	})
}

// --- PAY-SWEEP-CAS-NOISE-1 -----------------------------------------------------

// A callback succeeds the attempt while the poll is in flight. Every non-success
// poll result then maps to a no-op for the (now terminal) fresh state: the sweep
// must record NO error (no CAS conflict), leave the attempt succeeded and post once.
func TestSweepCASNoise_NonSuccessPollOnAnAttemptTerminalByCallback_IsANoOp(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name   string
		status func(ref string) (StatusResult, bool) // bool=false: no override, the MOCK errors (transport failure)
	}{
		{"pending", func(ref string) (StatusResult, bool) {
			return StatusResult{ProviderReference: ref, Outcome: OutcomePending}, true
		}},
		{"ambiguous", func(ref string) (StatusResult, bool) {
			return StatusResult{ProviderReference: ref, Outcome: OutcomeAmbiguous}, true
		}},
		{"declined", func(ref string) (StatusResult, bool) {
			return StatusResult{ProviderReference: ref, Outcome: OutcomeDeclined, DeclineReason: "provider_unavailable"}, true
		}},
		{"transport failure", func(string) (StatusResult, bool) { return StatusResult{}, false }},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-noise"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "d1-noise")
			e.deliverCallbackOnce(t, OutcomeSucceeded, 5000, "")
			st, override := c.status(ref)
			if override {
				e.p.setStatus(ref, st)
			}
			setNextActionNow(t, pool, e.f.tenantID, a.ID)
			stats := e.sweeper().RunOnce(context.Background(), []uuid.UUID{e.f.tenantID})
			e.mustNoSweepErrors(t, stats)
			if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
				t.Fatalf("state=%s, want succeeded", got.State)
			}
			if n := ledgerDepositTxCount(t, pool, e.f.tenantID, *a.DepositIntentID); n != 1 {
				t.Fatalf("deposit postings=%d, want exactly 1", n)
			}
			if depAttemptCount(t, e, e.f, *a.DepositIntentID) != 1 {
				t.Errorf("a stale decline poll must not cascade")
			}
			assertLedgerBalanced(t, pool, e.f.tenantID)
		})
	}
}

// --- PAY-DEFERRED-RECEIPT-SYNC-1 (widened by LF) ---------------------------------

// phaseBCallbackThenAmbiguous: a verified success callback arrives during phase B
// for a reference the attempt does not know yet (stored deferred_unresolved) and
// the adapter then answers Ambiguous WITH that reference, which T6 binds. The
// attempt is ambiguous, the reference is bound and one receipt is still waiting.
func (e *depRefEnv) phaseBCallbackThenAmbiguous(t *testing.T, key string) (PaymentAttempt, string) {
	t.Helper()
	ref := "defer-ref-" + uuid.NewString()
	var cbRes ReceiveCallbackResult
	var cbErr error
	e.p.setScript(func(req DepositRequest) DepositResult {
		cbRes, cbErr = rvCallback(e.pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false))
		return DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}
	})
	res := rvInit(t, e.pool, e.orch, e.f, 5000, key)
	if cbErr != nil || cbRes.Disposition != DispositionDeferredUnresolved {
		t.Fatalf("phase B callback: disposition=%s err=%v, want deferred_unresolved", cbRes.Disposition, cbErr)
	}
	a := mustGetAttempt(t, e.pool, e.f.tenantID, res.Attempt.ID)
	if a.State != AttemptAmbiguous || a.ProviderReference == nil || *a.ProviderReference != ref {
		t.Fatalf("setup: state=%s ref=%v, want ambiguous with %q bound", a.State, a.ProviderReference, ref)
	}
	return a, ref
}

// The sweeper's Succeeded branch on a T6-bound ambiguous attempt resolves the
// deferred receipt; the posting is the poll's, and there is no second one.
func TestDeferredReceipt_PollSuccessOnT6BoundAttempt_ResolvesReceiptNoSecondPosting(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-d1-dr-poll")
	a, ref := e.phaseBCallbackThenAmbiguous(t, "d1-dr-poll")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR")))
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded", got.State)
	}
	assertDeferredReceiptResolved(t, pool, e.f.tenantID, ref, a.ID)
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

// T9 (sweeper): a Pending poll on the T6-bound ambiguous attempt accepts it and
// drains the receipt, which applies the stored success exactly once.
func TestDeferredReceipt_T9PollPending_ResolvesReceiptAndPostsOnce(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-d1-dr-t9")
	a, ref := e.phaseBCallbackThenAmbiguous(t, "d1-dr-t9")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, StatusResult{ProviderReference: ref, Outcome: OutcomePending}))
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("state=%s, want succeeded (the drained success callback applied)", got.State)
	}
	assertDeferredReceiptResolved(t, pool, e.f.tenantID, ref, a.ID)
	if e.depositTxCount(t) != 1 || cashBalance(t, pool, e.f) != 5000 {
		t.Fatalf("postings=%d balance=%d, want exactly one posting of 5000", e.depositTxCount(t), cashBalance(t, pool, e.f))
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

// --- QA C3: fault injection on the park paths ----------------------------------
//
// An error is forced AFTER the dispute CAS and BEFORE commit with a table lock held
// by another transaction plus `SET LOCAL lock_timeout = '1ms'` in the victim: the
// victim's next write (the audit insert, or the intent projection update) cannot
// get its lock and fails with 55P03. Deterministic: the lock is held for the whole
// attempt, so the timeout always fires; there is no sleep. Then the test asserts
// the rollback was COMPLETE and that re-driving the same evidence parks cleanly.

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

func (e *depRefEnv) holdTableShare(t *testing.T, table string) *loBlocker {
	t.Helper()
	return loHoldWith(t, e.pool, e.f.tenantID, "share-"+table, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `LOCK TABLE `+table+` IN SHARE MODE`)
		return err
	})
}

type parkSnapshot struct {
	attemptState    AttemptState
	terminalReason  string
	attemptRef      string
	attemptNext     bool
	intentStatus    string
	intentRef       string
	disputeAudits   int64
	ledgerTxs       int64
	attemptUpdated  int64 // UnixNano
	intentUpdatedAt int64 // UnixNano
}

func (e *depRefEnv) snapshotPark(t *testing.T, attemptID, intentID uuid.UUID) parkSnapshot {
	t.Helper()
	a := mustGetAttempt(t, e.pool, e.f.tenantID, attemptID)
	s := parkSnapshot{
		attemptState: a.State, terminalReason: depTerminalReason(a), attemptNext: !depNextActionNull(t, e, e.f, attemptID), attemptUpdated: a.UpdatedAt.UnixNano(),
		intentStatus:  depIntentStatus(t, e, e.f, intentID),
		disputeAudits: depDisputeAudits(t, e, e.f, attemptID), ledgerTxs: depLedgerTxCount(t, e, e.f),
		intentUpdatedAt: depScan[time.Time](t, e.pool, e.f.tenantID, `SELECT updated_at FROM deposit_intents WHERE id = $1`, intentID).UnixNano(),
	}
	if a.ProviderReference != nil {
		s.attemptRef = *a.ProviderReference
	}
	if r := depIntentRef(t, e, e.f, intentID); r != nil {
		s.intentRef = *r
	}
	return s
}

func (e *depRefEnv) assertRolledBack(t *testing.T, what string, before, after parkSnapshot) {
	t.Helper()
	if before != after {
		t.Fatalf("%s: the failed park left partial state.\nbefore: %+v\nafter:  %+v", what, before, after)
	}
}

// pollVictim re-runs the sweeper's evidence application for one poll result in a
// test-owned transaction, so the test controls lock_timeout. Same code as RunOnce
// executes after the provider call: lock the intent, re-read, applyStatusEvidence.
func (e *depRefEnv) pollVictim(attemptID, intentID uuid.UUID, gr GateResult[StatusResult], lockTimeout bool) error {
	return e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if lockTimeout {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1ms'`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intentID); err != nil {
			return err
		}
		attempt, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, intentID)
		if err != nil {
			return err
		}
		return e.sweeper().applyStatusEvidence(ctx, tx, intent, attempt, gr)
	})
}

func TestParkFaultInjection_PollPark_RollsBackCompletelyThenRedrivesCleanly(t *testing.T) {
	pool := testPool(t)
	cases := []struct {
		name   string
		status func(ref string) StatusResult
		reason string
	}{
		{"poll_amount_mismatch", func(ref string) StatusResult { return pollSuccess(ref, 4999, "EUR") }, TerminalReasonPollAmountMismatch},
		{"poll_reference_mismatch", func(ref string) StatusResult { return pollSuccess("some-other-ref", 5000, "EUR") }, TerminalReasonPollReferenceMismatch},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-d1-fi-poll"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "d1-fi-poll")
			intentID := *a.DepositIntentID
			gr := GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: c.status(ref)}
			before := e.snapshotPark(t, a.ID, intentID)

			// Injection: the audit insert (after the dispute CAS) cannot get its lock.
			blocker := e.holdTableShare(t, "audit_log")
			err := e.pollVictim(a.ID, intentID, gr, true)
			if !isLockNotAvailable(err) {
				t.Fatalf("injected failure: got %v, want SQLSTATE 55P03", err)
			}
			blocker.release()
			e.assertRolledBack(t, "after the audit insert failed", before, e.snapshotPark(t, a.ID, intentID))
			if cur := mustGetAttempt(t, pool, e.f.tenantID, a.ID); cur.State != AttemptAmbiguous {
				t.Fatalf("state=%s after the rollback, want ambiguous", cur.State)
			}

			// Clean re-drive of the same evidence: parks exactly once.
			if err := e.pollVictim(a.ID, intentID, gr, false); err != nil {
				t.Fatalf("re-drive: %v", err)
			}
			e.assertPollParked(t, a, ref, c.reason)
		})
	}
}

// Phase C park paths (all four C reasons): a failure after the dispute CAS rolls
// the whole phase C transaction back (the attempt is exactly as phase B left it:
// claimed 'submitting', nothing bound or audited, the intent unchanged), once at
// the audit insert and once at the intent projection update; re-driving phase C
// with the same adapter result then parks once.
func TestParkFaultInjection_PhaseC_RollsBackCompletelyThenRedrivesCleanly(t *testing.T) {
	pool := testPool(t)
	long := strings.Repeat("a", providerrefMax+1)
	invalidErr := providerref.Validate("deposit.provider_reference", long)
	if invalidErr == nil {
		t.Fatal("setup: expected an invalid reference error")
	}
	cases := []struct {
		name   string
		gr     func(e *depRefEnv) GateResult[DepositResult]
		reason string
	}{
		{"invalid reference", func(*depRefEnv) GateResult[DepositResult] {
			return GateResult[DepositResult]{Class: ErrorClassProviderRefInvalid, Err: invalidErr, Value: DepositResult{Outcome: OutcomeSucceeded}}
		}, TerminalReasonInvalidProviderReference + ":" + string(mustReason(t, invalidErr))},
		{"sync amount mismatch", func(*depRefEnv) GateResult[DepositResult] {
			return GateResult[DepositResult]{Class: ErrorClassSucceeded, Value: DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "fi-mm-ref", Amount: 4999, AssetCode: "EUR"}}
		}, TerminalReasonSyncAmountMismatch},
		{"provider reference conflict", func(e *depRefEnv) GateResult[DepositResult] {
			seedPayoutAttemptBoundTo(t, e.pool, e.f, e.id, "fi-conflict-ref")
			return GateResult[DepositResult]{Class: ErrorClassPending, Value: DepositResult{Outcome: OutcomePending, ProviderReference: "fi-conflict-ref"}}
		}, TerminalReasonProviderReferenceConflict},
		{"reversal tombstone", func(e *depRefEnv) GateResult[DepositResult] {
			if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := postDepositReversalTombstone(ctx, tx, e.f.tenantID, e.id, "fi-tomb-ref")
				return err
			}); err != nil {
				t.Fatalf("seed tombstone: %v", err)
			}
			return GateResult[DepositResult]{Class: ErrorClassSucceeded, Value: DepositResult{Outcome: OutcomeSucceeded, ProviderReference: "fi-tomb-ref", Amount: 5000, AssetCode: "EUR"}}
		}, TerminalReasonTombstonePrecedesSuccess},
	}
	for i, c := range cases {
		for _, inj := range []struct{ name, table string }{{"audit insert", "audit_log"}, {"intent projection update", "deposit_intents"}} {
			t.Run(c.name+"/"+inj.name, func(t *testing.T) {
				e := newDepRefEnv(t, pool, "mock-d1-fi-c"+string(rune('a'+i))+inj.table[:1])
				intentID := insertRawDepositIntent(t, pool, e.f, "pending")
				attemptID, claim := uuid.New(), uuid.New()
				if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
						ID: attemptID, TenantID: e.f.tenantID, Operation: AttemptOperationDeposit, DepositIntentID: &intentID,
						AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
					}); err != nil {
						return err
					}
					return ClaimCreatedForSubmission(ctx, tx, attemptID, e.id, claim, "d1-fi", time.Now().Add(time.Minute))
				}); err != nil {
					t.Fatalf("seed claimed attempt: %v", err)
				}
				gr := c.gr(e)
				before := e.snapshotPark(t, attemptID, intentID)
				if before.attemptState != AttemptSubmitting {
					t.Fatalf("setup: attempt state=%s, want submitting", before.attemptState)
				}

				blocker := e.holdTableShare(t, inj.table)
				err := e.phaseCVictim(attemptID, intentID, claim, gr, true)
				if !isLockNotAvailable(err) {
					t.Fatalf("injected failure at the %s: got %v, want SQLSTATE 55P03", inj.name, err)
				}
				blocker.release()
				e.assertRolledBack(t, "after the "+inj.name+" failed", before, e.snapshotPark(t, attemptID, intentID))

				// Clean re-drive of phase C with the same adapter result.
				if err := e.phaseCVictim(attemptID, intentID, claim, gr, false); err != nil {
					t.Fatalf("re-drive: %v", err)
				}
				got := mustGetAttempt(t, pool, e.f.tenantID, attemptID)
				if got.State != AttemptDisputed || depTerminalReason(got) != c.reason {
					t.Fatalf("re-driven state=%s reason=%s, want disputed/%s", got.State, depTerminalReason(got), c.reason)
				}
				if n := depDisputeAudits(t, e, e.f, attemptID); n != 1 {
					t.Errorf("dispute audits after the re-drive = %d, want exactly 1", n)
				}
				if s := depIntentStatus(t, e, e.f, intentID); s != string(DepositIntentAmbiguous) {
					t.Errorf("intent status=%q, want ambiguous", s)
				}
				if n := depLedgerTxCount(t, e, e.f); n != before.ledgerTxs {
					t.Errorf("ledger transactions changed by a park: %d -> %d", before.ledgerTxs, n)
				}
				assertLedgerBalanced(t, pool, e.f.tenantID)
				loAssertProjectionMatchesRebuild(t, pool, e.f.tenantID)
			})
		}
	}
}

func mustReason(t *testing.T, err error) providerref.Reason {
	t.Helper()
	perr, ok := providerref.AsError(err)
	if !ok {
		t.Fatalf("not a providerref error: %v", err)
	}
	return perr.Reason
}

// phaseCVictim runs phase C's evidence application (applyDepositCallResult) for
// an adapter result in a test-owned transaction, so the test controls lock_timeout.
func (e *depRefEnv) phaseCVictim(attemptID, intentID, claim uuid.UUID, gr GateResult[DepositResult], lockTimeout bool) error {
	return e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if lockTimeout {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1ms'`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intentID); err != nil {
			return err
		}
		attempt, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, intentID)
		if err != nil {
			return err
		}
		capability := ProviderCapability{AdapterCapability: AdapterCapability{ProviderID: e.id}}
		_, _, err = e.orch.applyDepositCallResult(ctx, tx, intent, attempt, capability, claim, gr, EvidenceSync, false)
		return err
	})
}
