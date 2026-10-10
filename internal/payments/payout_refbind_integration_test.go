//go:build integration

// PAY-PAYOUT-REFBIND-1 (LF-6 / F-C4 for payouts) and PAY-FPAY-HARDENING-1 F-L2
// integration tests: a valid-but-foreign provider reference parks the payout
// (T10, hold kept, one audit row, committed) instead of binding, settling or
// looping; every bind site is covered; a redelivery of the attempt's own
// reference is not a conflict.
package payments

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const rbConflictAudit = "payments.payout_parked_reference_conflict"

type rbEnv struct {
	pool *db.Pool
	f    payoutFixture
	orch *Orchestrator
	pid  string
}

func newRBEnv(t *testing.T, pid string) rbEnv {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, pid)
	return rbEnv{pool: pool, f: f, orch: orch, pid: pid}
}

// opts are the production payout options of the env's orchestrator (provider lookup + the declarations frozen at wiring, ADR 0111
// 24.7) with the shared destination service: an env without a provider lookup is untrusted and a success would be ambiguous.
func (e rbEnv) opts() []PayoutOption {
	return append(e.orch.PayoutOptions(), WithDestinations(pitest.Shared()))
}

// claim approves a fresh withdrawal and claims it (attempt in `submitting`).
func (e rbEnv) claim(t *testing.T, key string) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	t.Helper()
	wr := approvedWithdrawal(t, e.pool, e.f, 500, key)
	c, err := e.orch.ClaimForDispatch(context.Background(), e.pool, KYCEnforcementPayoutGate{}, e.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	return wr, c.Attempt
}

func rbResult(class ErrorClass, outcome Outcome, ref string) GateResult[WithdrawResult] {
	return GateResult[WithdrawResult]{Class: class, Value: WithdrawResult{Outcome: outcome, ProviderReference: ref, DeclineReason: "insufficient_funds"}}
}

func (e rbEnv) apply(t *testing.T, wr withdrawal.WithdrawalRequest, a PaymentAttempt, gr GateResult[WithdrawResult]) {
	t.Helper()
	if err := ApplyPayoutResult(context.Background(), e.pool, e.f.tenantID, wr.ID, a, gr, EvidenceSync, e.opts()...); err != nil {
		t.Fatalf("ApplyPayoutResult(%s, e.opts()): %v", gr.Class, err)
	}
}

func (e rbEnv) conflictAudits(t *testing.T) int {
	return fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2`, e.f.tenantID, rbConflictAudit)
}

// assertParked checks the full parked shape: attempt disputed with the reason,
// no reference bound anywhere, the withdrawal still submitted with its hold and
// no release/settlement posted, and the ledger unchanged.
func (e rbEnv) assertParked(t *testing.T, wr withdrawal.WithdrawalRequest, a PaymentAttempt, ledgerBefore int, foreignRef string) {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != "provider_reference_conflict" {
		t.Fatalf("expected disputed/provider_reference_conflict, got state=%s reason=%v", got.State, got.TerminalReason)
	}
	if got.ProviderReference != nil && *got.ProviderReference == foreignRef {
		t.Fatalf("the foreign reference was bound to the attempt")
	}
	w := fpReqState(t, e.pool, e.f, wr.ID)
	if w.State != withdrawal.StateSubmitted || w.ReleaseLedgerTransactionID != nil {
		t.Fatalf("the hold must be kept: state=%s release=%v", w.State, w.ReleaseLedgerTransactionID)
	}
	if w.ProviderReference != nil && *w.ProviderReference == foreignRef {
		t.Fatalf("the foreign reference was bound to the withdrawal")
	}
	if n := fpLedgerTx(t, e.pool, e.f); n != ledgerBefore {
		t.Fatalf("a parked conflict must post nothing: ledger tx %d -> %d", ledgerBefore, n)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

// seedBound binds ref to a first payout (attempt A pending) and returns A.
func (e rbEnv) seedBound(t *testing.T, key, ref string) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	t.Helper()
	wr, a := e.claim(t, key)
	e.apply(t, wr, a, rbResult(ErrorClassPending, OutcomePending, ref))
	return wr, mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
}

// Sync Pending: lookup 1 (another payment_attempts row).
func TestRefBind_Sync_Pending_ForeignAttemptReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-pending")
	_, aA := e.seedBound(t, "rb-p-a", "rb-shared-1")
	wrB, aB := e.claim(t, "rb-p-b")
	before := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wrB, aB, rbResult(ErrorClassPending, OutcomePending, "rb-shared-1"))

	e.assertParked(t, wrB, aB, before, "rb-shared-1")
	if e.conflictAudits(t) != 1 {
		t.Fatalf("expected exactly one conflict audit, got %d", e.conflictAudits(t))
	}
	if aGot := mustGetAttempt(t, e.pool, e.f.tenantID, aA.ID); aGot.State != AttemptPending || *aGot.ProviderReference != "rb-shared-1" {
		t.Fatalf("the original binder must be untouched: %+v", aGot)
	}
	// Retries do not loop: a redelivery of the same (now disputed) evidence
	// commits without error and writes no second audit.
	e.apply(t, wrB, aB, rbResult(ErrorClassPending, OutcomePending, "rb-shared-1"))
	if e.conflictAudits(t) != 1 {
		t.Fatalf("a retry must not re-park or re-audit, audits=%d", e.conflictAudits(t))
	}
}

// Sync Succeeded: parks BEFORE ApplySuccess/Complete; nothing posts.
func TestRefBind_Sync_Success_ForeignAttemptReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-success")
	e.seedBound(t, "rb-s-a", "rb-shared-2")
	wrB, aB := e.claim(t, "rb-s-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wrB, aB, rbResult(ErrorClassSucceeded, OutcomeSucceeded, "rb-shared-2"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-2")
	if e.conflictAudits(t) != 1 {
		t.Fatalf("expected one conflict audit, got %d", e.conflictAudits(t))
	}
}

// Sync definite decline: parks (hold kept) instead of binding and releasing.
func TestRefBind_Sync_Decline_ForeignReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-decline")
	e.seedBound(t, "rb-d-a", "rb-shared-3")
	wrB, aB := e.claim(t, "rb-d-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wrB, aB, rbResult(ErrorClassDefiniteDecline, OutcomeDeclined, "rb-shared-3"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-3")
	if e.conflictAudits(t) != 1 {
		t.Fatalf("expected one conflict audit, got %d", e.conflictAudits(t))
	}
}

// Sync Ambiguous (default branch) binds the reference too.
func TestRefBind_Sync_Ambiguous_ForeignReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-ambig")
	e.seedBound(t, "rb-a-a", "rb-shared-4")
	wrB, aB := e.claim(t, "rb-a-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wrB, aB, rbResult(ErrorClassAmbiguous, OutcomePending, "rb-shared-4"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-4")
}

// NotSent on a RESEND (ever_possibly_sent) routes to T6 and persists the
// reference, so it is guarded as well.
func TestRefBind_Sync_NotSentResend_ForeignReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-resend")
	e.seedBound(t, "rb-n-a", "rb-shared-5")
	wrB, aB := e.claim(t, "rb-n-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)
	resend := aB
	resend.EverPossiblySent = true

	e.apply(t, wrB, resend, rbResult(ErrorClassNotSent, OutcomePending, "rb-shared-5"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-5")
}

// Lookup 1 with a DEPOSIT attempt: the reference is held by a deposit attempt of
// the same provider (no withdrawal row carries it).
func TestRefBind_ForeignDepositAttemptReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-dep")
	dep := rvInit(t, e.pool, e.orch, e.f.orchFixture, 5000, "rb-dep-1")
	if dep.Attempt.ProviderReference == nil || *dep.Attempt.ProviderReference == "" {
		t.Fatalf("setup: the deposit attempt has no provider reference")
	}
	ref := *dep.Attempt.ProviderReference
	wrB, aB := e.claim(t, "rb-dep-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wrB, aB, rbResult(ErrorClassPending, OutcomePending, ref))

	e.assertParked(t, wrB, aB, ledgerBefore, ref)
	var boundTo string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'bound_to' FROM audit_log WHERE tenant_id=$1 AND action=$2`, e.f.tenantID, rbConflictAudit).Scan(&boundTo)
	}); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if boundTo != "deposit" {
		t.Fatalf("audit bound_to = %q, want deposit", boundTo)
	}
}

// Lookup 2: a reference held only by another WITHDRAWAL request (no attempt
// row carries it).
func TestRefBind_ForeignWithdrawalReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-wd")
	wrA, _ := e.claim(t, "rb-w-a")
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.AttachProviderReference(ctx, tx, wrA.ID, "rb-shared-6")
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	wrB, aB := e.claim(t, "rb-w-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wrB, aB, rbResult(ErrorClassPending, OutcomePending, "rb-shared-6"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-6")
}

// Lookup 3 (the "reverse collision"): the payout's settlement key equals an
// existing DEPOSIT ledger key at the same provider. Without the pre-check
// withdrawal.Complete's ledger.Post fails with ErrIdempotencyPayloadMismatch and
// the phase-C transaction rolls back forever.
func TestRefBind_ReverseCollision_DepositLedgerKey_ParksOnSuccess(t *testing.T) {
	e := newRBEnv(t, "mock-rb-ledger")
	ref := "rb-dep-key-7"
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, &e.f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		clearing, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: e.f.tenantID, TransactionType: ledger.TxDeposit,
			IdempotencyKey: e.pid + ":" + ref, ProviderID: &e.pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: 1000},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1000},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("seed deposit ledger key: %v", err)
	}
	wr, a := e.claim(t, "rb-l-a")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, ref))

	e.assertParked(t, wr, a, ledgerBefore, ref)
	var boundTo string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'bound_to' FROM audit_log WHERE tenant_id=$1 AND action=$2`, e.f.tenantID, rbConflictAudit).Scan(&boundTo)
	}); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if boundTo != "ledger_deposit" {
		t.Fatalf("audit bound_to = %q, want ledger_deposit", boundTo)
	}
}

// Duplicate / retry: the attempt's OWN reference redelivered is never a
// conflict; success settles once; a replayed success is a benign no-op.
func TestRefBind_OwnReferenceRedelivery_NotAConflict_SettlesOnce(t *testing.T) {
	e := newRBEnv(t, "mock-rb-own")
	wr, a := e.seedBound(t, "rb-o-a", "rb-own-8")

	e.apply(t, wr, a, rbResult(ErrorClassPending, OutcomePending, "rb-own-8"))
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatalf("a same-reference redelivery must leave the attempt pending, got %s", got.State)
	}

	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, "rb-own-8"))
	if w := fpReqState(t, e.pool, e.f, wr.ID); w.State != withdrawal.StateCompleted {
		t.Fatalf("expected completed, got %s", w.State)
	}
	ledgerAfterFirst := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, "rb-own-8"))
	if n := fpLedgerTx(t, e.pool, e.f); n != ledgerAfterFirst {
		t.Fatalf("a replayed success must post nothing: %d -> %d", ledgerAfterFirst, n)
	}
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("a replay must not disturb the succeeded attempt, got %s", got.State)
	}
	if e.conflictAudits(t) != 0 {
		t.Fatalf("no conflict may be recorded for an own-reference redelivery, got %d", e.conflictAudits(t))
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

// Normal path: a fresh, unshared reference is unaffected.
func TestRefBind_FreshReference_Settles(t *testing.T) {
	e := newRBEnv(t, "mock-rb-fresh")
	wr, a := e.claim(t, "rb-f-a")
	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, "rb-fresh-9"))
	if w := fpReqState(t, e.pool, e.f, wr.ID); w.State != withdrawal.StateCompleted {
		t.Fatalf("expected completed, got %s", w.State)
	}
	if e.conflictAudits(t) != 0 {
		t.Fatalf("unexpected conflict audit")
	}
}

// Tenant isolation: the same string at the same provider id in ANOTHER tenant is
// neither visible nor a conflict.
func TestRefBind_OtherTenantReference_NotAConflict(t *testing.T) {
	e1 := newRBEnv(t, "mock-rb-tenant")
	e1.seedBound(t, "rb-t-a", "rb-xtenant-10")

	f2 := seedPayoutFixture(t, e1.pool, 10_000, true)
	mp := NewMockProvider(e1.pid, "EUR")
	registerCapability(t, e1.pool, f2.orchFixture, mp, 100)
	orch2 := NewOrchestrator(map[string]PaymentProvider{e1.pid: mp}, MultiWebhookCredentialResolver{e1.pid: NewMockWebhookCredentials(mp)}).WithPayoutDestinations(pitest.Shared())
	e2 := rbEnv{pool: e1.pool, f: f2, orch: orch2, pid: e1.pid}
	wr, a := e2.claim(t, "rb-t-b")

	e2.apply(t, wr, a, rbResult(ErrorClassPending, OutcomePending, "rb-xtenant-10"))

	if got := mustGetAttempt(t, e2.pool, f2.tenantID, a.ID); got.State != AttemptPending {
		t.Fatalf("another tenant's reference must not park this payout, got %s", got.State)
	}
	if e2.conflictAudits(t) != 0 {
		t.Fatalf("no conflict audit expected in the second tenant")
	}
}

// Concurrency: two payouts race to bind the same fresh reference. The unique
// indexes make exactly one bind. The loser either parks directly (it read after
// the winner committed) or its WHOLE transaction fails with a unique violation
// and rolls back with nothing changed. In production a synchronous Withdraw
// result is LOST on that rollback; the loser converges via the sweeper (attempt
// still `submitting`, no reference -> T6 ambiguous -> resend), and the resend's
// result then hits the guard, which parks. This test models the convergence step
// as the same ApplyPayoutResult call being applied again (the resend's result).
// It never loops and never double-binds.
func TestRefBind_Concurrent_SameFreshReference_OneBindsLoserConvergesToPark(t *testing.T) {
	e := newRBEnv(t, "mock-rb-conc")
	wrA, aA := e.claim(t, "rb-c-a")
	wrB, aB := e.claim(t, "rb-c-b")
	ref := "rb-race-11"
	type racer struct {
		wr withdrawal.WithdrawalRequest
		a  PaymentAttempt
	}
	racers := []racer{{wrA, aA}, {wrB, aB}}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, r := range racers {
		wg.Add(1)
		go func(i int, r racer) {
			defer wg.Done()
			<-start
			errs[i] = ApplyPayoutResult(context.Background(), e.pool, e.f.tenantID, r.wr.ID, r.a, rbResult(ErrorClassPending, OutcomePending, ref), EvidenceSync, WithDestinations(pitest.Shared()))
		}(i, r)
	}
	close(start)
	wg.Wait()

	// A failed racer must have rolled back completely, so a retry is the same call.
	for i, err := range errs {
		if err == nil {
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("racer %d: only a unique violation is an acceptable loser error, got %v", i, err)
		}
		if err := ApplyPayoutResult(context.Background(), e.pool, e.f.tenantID, racers[i].wr.ID, racers[i].a, rbResult(ErrorClassPending, OutcomePending, ref), EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("racer %d retry must park, not error again: %v", i, err)
		}
	}
	states := map[AttemptState]int{}
	states[mustGetAttempt(t, e.pool, e.f.tenantID, aA.ID).State]++
	states[mustGetAttempt(t, e.pool, e.f.tenantID, aB.ID).State]++
	if states[AttemptPending] != 1 || states[AttemptDisputed] != 1 {
		t.Fatalf("expected exactly one pending and one disputed, got %v", states)
	}
	if n := fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM payment_attempts WHERE tenant_id=$1 AND provider_reference=$2`, e.f.tenantID, ref); n != 1 {
		t.Fatalf("exactly one attempt may hold the reference, got %d", n)
	}
	if e.conflictAudits(t) != 1 {
		t.Fatalf("expected one conflict audit, got %d", e.conflictAudits(t))
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

func rbStatus(class ErrorClass, outcome Outcome, ref string) GateResult[StatusResult] {
	return GateResult[StatusResult]{Class: class, Value: StatusResult{Outcome: outcome, ProviderReference: ref}}
}

func (e rbEnv) applyStatus(t *testing.T, wr withdrawal.WithdrawalRequest, a PaymentAttempt, gr GateResult[StatusResult]) {
	t.Helper()
	if err := applyPayoutStatusEvidence(context.Background(), e.pool, e.f.tenantID, wr.ID, a, gr, EvidenceQueryStatus, time.Now().Add(time.Minute), nil, e.opts()...); err != nil {
		t.Fatalf("applyPayoutStatusEvidence(%s, e.opts()): %v", gr.Class, err)
	}
}

// Status path, submitting + Pending.
func TestRefBind_Status_SubmittingPending_ForeignReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-st1")
	e.seedBound(t, "rb-st1-a", "rb-shared-12")
	wrB, aB := e.claim(t, "rb-st1-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.applyStatus(t, wrB, aB, rbStatus(ErrorClassPending, OutcomePending, "rb-shared-12"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-12")
}

// Status path, submitting + Ambiguous.
func TestRefBind_Status_SubmittingAmbiguous_ForeignReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-st2")
	e.seedBound(t, "rb-st2-a", "rb-shared-13")
	wrB, aB := e.claim(t, "rb-st2-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.applyStatus(t, wrB, aB, rbStatus(ErrorClassAmbiguous, OutcomePending, "rb-shared-13"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-13")
}

// Status path, ambiguous + Pending.
func TestRefBind_Status_AmbiguousPending_ForeignReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-st3")
	e.seedBound(t, "rb-st3-a", "rb-shared-14")
	wrB, aB := e.claim(t, "rb-st3-b")
	e.apply(t, wrB, aB, rbResult(ErrorClassAmbiguous, OutcomePending, "")) // ambiguous, no reference yet
	amb := mustGetAttempt(t, e.pool, e.f.tenantID, aB.ID)
	if amb.State != AttemptAmbiguous {
		t.Fatalf("setup: expected ambiguous, got %s", amb.State)
	}
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.applyStatus(t, wrB, amb, rbStatus(ErrorClassPending, OutcomePending, "rb-shared-14"))

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-14")
}

// Status path, success and decline go through applyPayoutSuccess/Decline.
func TestRefBind_Status_SuccessAndDecline_ForeignReference_Park(t *testing.T) {
	for _, tc := range []struct {
		name    string
		class   ErrorClass
		outcome Outcome
	}{{"success", ErrorClassSucceeded, OutcomeSucceeded}, {"decline", ErrorClassDefiniteDecline, OutcomeDeclined}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRBEnv(t, "mock-rb-st4-"+tc.name)
			e.seedBound(t, "rb-st4-a", "rb-shared-15")
			wrB, aB := e.claim(t, "rb-st4-b")
			ledgerBefore := fpLedgerTx(t, e.pool, e.f)

			gr := rbStatus(tc.class, tc.outcome, "rb-shared-15")
			gr.Value.DeclineReason = "insufficient_funds"
			gr.Value.Amount, gr.Value.AssetCode = wrB.Amount, wrB.AssetCode // an exact echo, so only the reference can park it
			e.applyStatus(t, wrB, aB, gr)

			e.assertParked(t, wrB, aB, ledgerBefore, "rb-shared-15")
		})
	}
}

// ---------------------------------------------------------------------------
// PAY-FPAY-HARDENING-1 F-L2 (deposit): a KYC outage is not a KYC requirement.

type fixedDepositGate struct{ reason string }

func (g fixedDepositGate) EvaluateDeposit(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, string) (bool, string, error) {
	return false, g.reason, nil
}

func declineReasonOf(t *testing.T, pool *db.Pool, tenantID, intentID uuid.UUID) string {
	t.Helper()
	var reason string
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'decline_reason' FROM audit_log WHERE tenant_id=$1 AND action='deposit.declined' AND target_id=$2`, tenantID, intentID.String()).Scan(&reason)
	}); err != nil {
		t.Fatalf("read decline reason: %v", err)
	}
	return reason
}

// deposit_v2.go site: phase A.
func TestDepositKYC_PhaseA_OutageVsDeny_DistinctReasons(t *testing.T) {
	for _, tc := range []struct{ name, deny, want string }{
		{"unavailable", "kyc_unavailable:verification_lookup_failed", "kyc_unavailable:verification_lookup_failed"},
		{"deny", "kyc_test_deny", "kyc_required:kyc_test_deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedOrchFixture(t, pool)
			pid := "mock-l2a-" + tc.name
			mp := NewMockProvider(pid, "EUR")
			registerCapability(t, pool, f, mp, 100)
			orch := NewOrchestrator(map[string]PaymentProvider{pid: mp}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(mp)}).WithPayoutDestinations(pitest.Shared())
			res, err := orch.InitiateDepositAttempt(context.Background(), pool, fixedDepositGate{tc.deny}, MockCredentialResolver{}, InitiateDepositParams{
				Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
				AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "l2a-" + tc.name,
			})
			if err != nil {
				t.Fatalf("InitiateDepositAttempt: %v", err)
			}
			if res.Intent.Status != DepositIntentDeclined || res.AttemptCreated || mp.AttemptCount() != 0 {
				t.Fatalf("must stay fail-closed: status=%s created=%v calls=%d", res.Intent.Status, res.AttemptCreated, mp.AttemptCount())
			}
			if got := declineReasonOf(t, pool, f.tenantID, res.Intent.ID); got != tc.want {
				t.Fatalf("decline_reason = %q, want %q", got, tc.want)
			}
			// The denial audit keeps the gate's own reason code in both cases.
			if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.deposit_denied_by_kyc' AND metadata->>'reason_code'=$2`, f.tenantID, tc.deny); n != 1 {
				t.Fatalf("expected one deposit_denied_by_kyc audit with reason_code %q, got %d", tc.deny, n)
			}
			if cashBalance(t, pool, f) != 0 {
				t.Fatalf("no money may move")
			}
		})
	}
}

// drive.go site: the cascade T2 gate (an already-created attempt is driven).
func TestDepositKYC_CascadeT2_OutageVsDeny_DistinctReasons(t *testing.T) {
	for _, tc := range []struct{ name, deny, want string }{
		{"unavailable", "kyc_unavailable:verification_lookup_failed", "kyc_unavailable:verification_lookup_failed"},
		{"deny", "kyc_test_deny", "kyc_required:kyc_test_deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedOrchFixture(t, pool)
			pid := "mock-l2b-" + tc.name
			mp := NewMockProvider(pid, "EUR")
			registerCapability(t, pool, f, mp, 100)
			orch := NewOrchestrator(map[string]PaymentProvider{pid: mp}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(mp)}).WithPayoutDestinations(pitest.Shared())
			res := rvInit(t, pool, orch, f, 5000, "l2b-"+tc.name)
			intentID := res.Intent.ID

			var created PaymentAttempt
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				// One live attempt per intent: decline attempt 1 first (the intent
				// stays open, as in a cascade).
				cascadable := true
				if err := ApplyDecline(ctx, tx, res.Attempt.ID, DeclineEvidence{Evidence: EvidenceSync, Reason: "provider_declined", Stage: DeclineAtSubmission, Cascadable: &cascadable}); err != nil {
					return err
				}
				var err error
				created, err = InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
					ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
					DepositIntentID: &intentID, AttemptNo: 2, ExcludedProviderIDs: []string{},
					PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
				})
				return err
			}); err != nil {
				t.Fatalf("setup: %v", err)
			}
			var intent DepositIntent
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				intent, err = GetDepositIntentByID(ctx, tx, intentID)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			if _, _, _, _, _, err := orch.driveCreatedAttempt(context.Background(), pool, fixedDepositGate{tc.deny}, MockCredentialResolver{}, intent, created, false); err != nil {
				t.Fatalf("driveCreatedAttempt: %v", err)
			}
			got := mustGetAttempt(t, pool, f.tenantID, created.ID)
			if got.State != AttemptRejected || got.TerminalReason == nil || *got.TerminalReason != tc.want {
				t.Fatalf("expected rejected/%q, got state=%s reason=%v", tc.want, got.State, got.TerminalReason)
			}
			if r := declineReasonOf(t, pool, f.tenantID, intentID); r != tc.want {
				t.Fatalf("intent decline_reason = %q, want %q", r, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LF C1: for a PAYOUT a reversal tombstone on the key is a conflict.

func TestRefBind_Tombstone_PayoutSuccess_Parks_NoLoop(t *testing.T) {
	e := newRBEnv(t, "mock-rb-tomb")
	ref := "rb-tomb-ref-20"
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: e.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:" + e.pid + ":" + ref, ProviderID: &e.pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
		})
		return err
	}); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	wr, a := e.claim(t, "rb-tomb-a")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, ref))

	e.assertParked(t, wr, a, ledgerBefore, ref)
	var boundTo string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'bound_to' FROM audit_log WHERE tenant_id=$1 AND action=$2`, e.f.tenantID, rbConflictAudit).Scan(&boundTo)
	}); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if boundTo != "ledger_tombstone" {
		t.Fatalf("bound_to = %q, want ledger_tombstone", boundTo)
	}
	// A redelivery commits without error (no rollback loop) and adds no second park.
	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, ref))
	if e.conflictAudits(t) != 1 {
		t.Fatalf("redelivery must not re-park, audits=%d", e.conflictAudits(t))
	}
}

// ---------------------------------------------------------------------------
// Callback path (receipt.go) through ApplyReceiptEvidence.

func (e rbEnv) callback(t *testing.T, a PaymentAttempt, outcome Outcome, ref string) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
			EventType: "payout", ProviderReference: ref, MerchantReference: a.MerchantReference,
			Outcome: outcome, Amount: 500, AssetCode: "EUR", DeclineReason: "insufficient_funds",
		})
		return err
	}); err != nil {
		t.Fatalf("ApplyReceiptEvidence(%s): %v", outcome, err)
	}
}

func (e rbEnv) postDepositKey(t *testing.T, ref string) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, &e.f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		clearing, err := ledger.GetOrCreateAccount(ctx, tx, e.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: e.f.tenantID, TransactionType: ledger.TxDeposit,
			IdempotencyKey: e.pid + ":" + ref, ProviderID: &e.pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: 1000},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1000},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("seed ledger key: %v", err)
	}
}

// Pending callback: reference held ONLY by another withdrawal_requests row.
func TestRefBind_Callback_Pending_ForeignWithdrawalReference_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-cb1")
	wrA, _ := e.claim(t, "rb-cb1-a")
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.AttachProviderReference(ctx, tx, wrA.ID, "rb-cb-shared-21")
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	wrB, aB := e.claim(t, "rb-cb1-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.callback(t, aB, OutcomePending, "rb-cb-shared-21")

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-cb-shared-21")
	if e.conflictAudits(t) != 1 {
		t.Fatalf("expected one conflict audit, got %d", e.conflictAudits(t))
	}
}

// Pending callback: reference held ONLY by a ledger key.
func TestRefBind_Callback_Pending_ForeignLedgerKey_Parks(t *testing.T) {
	e := newRBEnv(t, "mock-rb-cb2")
	e.postDepositKey(t, "rb-cb-key-22")
	wrB, aB := e.claim(t, "rb-cb2-b")
	ledgerBefore := fpLedgerTx(t, e.pool, e.f)

	e.callback(t, aB, OutcomePending, "rb-cb-key-22")

	e.assertParked(t, wrB, aB, ledgerBefore, "rb-cb-key-22")
}

// Success and Decline callbacks carrying a reference held by ANOTHER ATTEMPT are
// refused earlier, at receipt resolution (anomaly receipt, no state change); the
// hold is kept and nothing binds or posts.
func TestRefBind_Callback_SuccessAndDecline_ForeignAttemptReference_AnomalyNoEffect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
	}{{"success", OutcomeSucceeded}, {"decline", OutcomeDeclined}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRBEnv(t, "mock-rb-cb3-"+tc.name)
			e.seedBound(t, "rb-cb3-a", "rb-cb-shared-23")
			wrB, aB := e.claim(t, "rb-cb3-b")
			ledgerBefore := fpLedgerTx(t, e.pool, e.f)

			e.callback(t, aB, tc.outcome, "rb-cb-shared-23")

			got := mustGetAttempt(t, e.pool, e.f.tenantID, aB.ID)
			if got.State != AttemptSubmitting || got.ProviderReference != nil {
				t.Fatalf("an anomaly receipt must change nothing, got state=%s ref=%v", got.State, got.ProviderReference)
			}
			w := fpReqState(t, e.pool, e.f, wrB.ID)
			if w.State != withdrawal.StateSubmitted || w.ProviderReference != nil || fpLedgerTx(t, e.pool, e.f) != ledgerBefore {
				t.Fatalf("hold/ledger/withdrawal changed: %+v", w)
			}
		})
	}
}

// Success and Decline callbacks carrying a reference held ONLY by a ledger key
// reach the guard (applyPayoutSuccess/Decline) and park, hold kept.
func TestRefBind_Callback_SuccessAndDecline_ForeignLedgerKey_Park(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
	}{{"success", OutcomeSucceeded}, {"decline", OutcomeDeclined}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRBEnv(t, "mock-rb-cb5-"+tc.name)
			e.postDepositKey(t, "rb-cb-key-25")
			wrB, aB := e.claim(t, "rb-cb5-b")
			ledgerBefore := fpLedgerTx(t, e.pool, e.f)

			e.callback(t, aB, tc.outcome, "rb-cb-key-25")

			e.assertParked(t, wrB, aB, ledgerBefore, "rb-cb-key-25")
		})
	}
}

// A Pending callback with a fresh reference still binds (normal path).
func TestRefBind_Callback_Pending_FreshReference_Binds(t *testing.T) {
	e := newRBEnv(t, "mock-rb-cb4")
	_, aB := e.claim(t, "rb-cb4-b")
	e.callback(t, aB, OutcomePending, "rb-cb-fresh-24")
	got := mustGetAttempt(t, e.pool, e.f.tenantID, aB.ID)
	if got.State != AttemptPending || got.ProviderReference == nil || *got.ProviderReference != "rb-cb-fresh-24" {
		t.Fatalf("expected pending/bound, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// M2 on a provider_reference_conflict payout is refused by the Go executor AND
// the DB allow-list (payment_m2_admits), for both kinds; the hold is untouched.

func TestRefBind_ConflictPark_M2Refused_GoAndDB(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.disputedPayout(100, "provider_reference_conflict")
	if M2ResolvableDispute(AttemptDisputed, a.TerminalReason) {
		t.Fatalf("Go allow-list admits provider_reference_conflict")
	}
	for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
		holdBefore := w.walletBalance("player_withdrawal_hold")
		_, err := w.request(w.f1, w.m2In(a.ID, kind))
		k3RequireCode(t, err, "MR012")
		if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted || w.walletBalance("player_withdrawal_hold") != holdBefore {
			t.Fatalf("%s: the hold or the withdrawal changed", kind)
		}
	}
}

func TestRefBind_ConflictPark_DBGuardRefuses(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, c := range []struct {
		kind     ResolutionKind
		newState string
	}{{ResolutionM2DeclarePaid, "succeeded"}, {ResolutionM2DeclareNotPaid, "declined"}} {
		_, a := w.ambiguousPayout(120)
		r := w.mustRequest(w.f1, w.m2In(a.ID, c.kind))
		reason := "provider_reference_conflict"
		if err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'disputed', $2, $3, $4, 'operator')`,
				a.ID, reason, *a.WithdrawalRequestID, c.newState).Scan(&got); err != nil {
				return err
			}
			if got {
				t.Errorf("%s: payment_m2_admits admits provider_reference_conflict", c.kind)
			}
			return nil
		}); err != nil {
			t.Fatalf("db guard probe: %v", err)
		}
	}
}
