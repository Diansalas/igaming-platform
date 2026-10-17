//go:build integration

// F4 (platform-freeze work list 2026-10-16; A6 SR6 I-2 / residual 4): the WD-RG-1 withdrawal RG/risk gate re-run at
// payout dispatch (T1p ClaimForDispatch, the sweeper's T2 re-claim and T12 resend) and the migration 0169 backstop on
// the payout attempt's created|ambiguous -> submitting. MOCK providers only.
package payments

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate/standard"
)

type f4World struct {
	pool     *db.Pool
	f        payoutFixture
	provider *MockProvider
	orch     *Orchestrator
}

func newF4World(t *testing.T, providerID string) *f4World {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider(providerID, "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{providerID: provider}, MultiWebhookCredentialResolver{providerID: NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())
	return &f4World{pool: pool, f: f, provider: provider, orch: orch}
}

func (w *f4World) claim(t *testing.T, id uuid.UUID) (ClaimResult, error) {
	t.Helper()
	return w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, id, "bank_transfer", testSubmitActor())
}

func (w *f4World) tx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, fn); err != nil {
		t.Fatal(err)
	}
}

func (w *f4World) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) })
	return n
}

func (w *f4World) state(t *testing.T, id uuid.UUID) withdrawal.WithdrawalRequest {
	t.Helper()
	var wr withdrawal.WithdrawalRequest
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.GetByID(ctx, tx, id)
		return err
	})
	return wr
}

func (w *f4World) holdBalance(t *testing.T) int64 {
	t.Helper()
	var bal int64
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(sum(credit_total - debit_total), 0)::bigint FROM wallet_balance_projection
			WHERE wallet_id = $1 AND account_type = 'player_withdrawal_hold'`, w.f.walletID).Scan(&bal)
	})
	return bal
}

func (w *f4World) setAccount(t *testing.T, s identity.PlayerAccountStatus) {
	t.Helper()
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, w.f.playerAccountID, s)
	})
}

func (w *f4World) addRiskRule(t *testing.T, threshold int64, action risk.RuleAction) {
	t.Helper()
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &w.f.tenantID, Operation: risk.OperationWithdrawal, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
			Threshold: threshold, AssetCode: "EUR", RuleKind: risk.RuleHardLimit, Action: action,
			CreatedByActorType: "staff", CreatedByActorID: uuid.New(), Description: "f4 dispatch rule",
		})
		return err
	})
}

func (w *f4World) decisions(t *testing.T, id uuid.UUID, stage string, allowed bool) int {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = $2 AND allowed = $3`, id, stage, allowed)
}

func (w *f4World) alerts(t *testing.T, discriminatorPrefix string) int {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM alerts WHERE kind = 'withdrawal.payout_gate_denied' AND discriminator LIKE $1`, discriminatorPrefix+"%")
}

func (w *f4World) reread(t *testing.T, id uuid.UUID) PaymentAttempt {
	t.Helper()
	var a PaymentAttempt
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = GetAttemptByID(ctx, tx, id)
		return err
	})
	return a
}

func (w *f4World) sweeper() *Sweeper {
	return &Sweeper{Pool: w.pool, Orchestrator: w.orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
}

func f4SQLState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// countingGate wraps a gate and counts evaluations; onEvaluate (optional) runs first.
type countingGate struct {
	inner      withdrawalgate.Gate
	calls      atomic.Int64
	onEvaluate func()
}

func (g *countingGate) Evaluate(ctx context.Context, tx pgx.Tx, in withdrawalgate.Input) (withdrawalgate.Decision, error) {
	g.calls.Add(1)
	if g.onEvaluate != nil {
		g.onEvaluate()
	}
	return g.inner.Evaluate(ctx, tx, in)
}

type f4FailingGate struct{}

func (f4FailingGate) Evaluate(context.Context, pgx.Tx, withdrawalgate.Input) (withdrawalgate.Decision, error) {
	return withdrawalgate.Decision{}, errors.New("injected gate outage")
}

// ALLOW: the claim records exactly one allowing dispatch decision (with the restriction in force, here none) in the
// claim transaction and transitions as before.
func TestF4_Claim_Allowed_RecordsTheDecisionAndSubmits(t *testing.T) {
	w := newF4World(t, "f4-allow")
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-allow")
	claim, err := w.claim(t, wr.ID)
	if err != nil || claim.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("an allowed claim submits: %v %+v", err, claim.Request)
	}
	if a, d := w.decisions(t, wr.ID, "dispatch", true), w.decisions(t, wr.ID, "dispatch", false); a != 1 || d != 0 {
		t.Fatalf("want exactly one allowing dispatch decision, got allow=%d deny=%d", a, d)
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
}

// Self-exclusion and time-out are ALLOWED at dispatch (identical to request time, ADR 0115); the decision row records
// the restriction that was in force.
func TestF4_Claim_SelfExcluded_IsAllowed_AndTheRestrictionIsRecorded(t *testing.T) {
	w := newF4World(t, "f4-se")
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-se")
	if err := w.pool.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: w.f.tenantID, PlayerAccountID: w.f.playerAccountID})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if claim, err := w.claim(t, wr.ID); err != nil || claim.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("a self-exclusion does not block a payout: %v", err)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND allowed AND restriction_in_force = 'self_exclusion'`, wr.ID); n != 1 {
		t.Fatalf("the allowing decision must record the self-exclusion in force, got %d rows", n)
	}
}

// A risk rule that FLIPS between request and dispatch (added after the request was approved) is enforced at dispatch:
// WD051, the payout parks in `approved` with its hold (no attempt, no provider call), the denying decision row, the
// staff-attributed denial audit and the alert commit. A re-submit re-evaluates (and is refused again; the alert is one
// open alert with growing occurrences). REVIEW refuses like DENY.
func TestF4_Claim_RiskRuleFlipsBetweenRequestAndDispatch_Parks(t *testing.T) {
	for _, action := range []risk.RuleAction{risk.ActionDeny, risk.ActionReview} {
		w := newF4World(t, "f4-risk-"+string(action))
		wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-risk-"+string(action))
		holdBefore := w.holdBalance(t)
		w.addRiskRule(t, 400, action)
		for round := 1; round <= 2; round++ {
			_, err := w.claim(t, wr.ID)
			var denied *withdrawal.GateDeniedError
			if !errors.Is(err, ErrPayoutWithdrawalGateDenied) || !errors.As(err, &denied) || denied.GateCode != withdrawal.GateCodeRisk || denied.Reason != risk.CodeHardLimitBreach {
				t.Fatalf("%s round %d: want a WD051 dispatch denial, got %v", action, round, err)
			}
			if got := w.state(t, wr.ID); got.State != withdrawal.StateApproved || got.ProviderID != nil {
				t.Fatalf("%s: the payout must stay approved, got %s", action, got.State)
			}
			if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
				t.Fatalf("%s: no attempt may exist, got %d", action, n)
			}
			if h := w.holdBalance(t); h != holdBefore {
				t.Fatalf("%s: the hold must stay, before %d after %d", action, holdBefore, h)
			}
			if d := w.decisions(t, wr.ID, "dispatch", false); d != round {
				t.Fatalf("%s: want %d denying decisions, got %d", action, round, d)
			}
			if a := w.decisions(t, wr.ID, "dispatch", true); a != 0 {
				t.Fatalf("%s: no allowing decision may exist, got %d", action, a)
			}
			if n := w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1 AND outcome = 'denied'
				AND metadata->>'gate_code' = 'WD051' AND metadata->>'risk_outcome' = $2 AND metadata->>'stage' = 'dispatch' AND actor_type = 'staff'`,
				wr.ID.String(), string(action)); n != round {
				t.Fatalf("%s: want %d staff-attributed denial audits, got %d", action, round, n)
			}
			if n := w.alerts(t, "withdrawal:"+wr.ID.String()+":gate:WD051"); n != 1 {
				t.Fatalf("%s: want one open withdrawal.payout_gate_denied alert, got %d", action, n)
			}
		}
		if n := w.count(t, `SELECT count(*) FROM kyc_enforcement_decisions WHERE player_account_id = $1 AND operation = 'withdrawal_payout'`, w.f.playerAccountID); n != 0 {
			t.Fatalf("%s: the KYC gate must not run after a WD-RG-1 denial, %d decisions", action, n)
		}
		// The parked payout keeps its existing exits: a governed reject releases the hold (E5).
		w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := withdrawal.LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
				return err
			}
			decision := kyc.EnforcementDecision{Outcome: kyc.OutcomeFailed, Allowed: false, Code: "kyc_withdrawal_payout:failed", PolicyVersion: kyc.PolicyVersion}
			params := kyc.EnforcementParams{
				TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID,
				Operation: kyc.EnforcementWithdrawalPayout, AssetCode: "EUR", Amount: wr.Amount, CorrelationID: uuid.New(),
			}
			_, err := withdrawal.DenyForCompliance(ctx, tx, wr.ID, decision, params)
			return err
		})
		loAssertBalanced(t, w.pool, w.f.tenantID)
		loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
	}
}

// The RG half at dispatch: an account suspended after approval refuses WD050 (park, same shape as the risk denial).
func TestF4_Claim_AccountSuspendedAfterApproval_Parks(t *testing.T) {
	w := newF4World(t, "f4-susp")
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-susp")
	holdBefore := w.holdBalance(t)
	w.setAccount(t, identity.PlayerStatusSuspended)
	_, err := w.claim(t, wr.ID)
	var denied *withdrawal.GateDeniedError
	if !errors.As(err, &denied) || denied.GateCode != withdrawal.GateCodeRG || denied.Reason != rg.CodePlayerAccountNotActive {
		t.Fatalf("want a WD050 dispatch denial, got %v", err)
	}
	if got := w.state(t, wr.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("the payout must stay approved, got %s", got.State)
	}
	if w.decisions(t, wr.ID, "dispatch", false) != 1 || w.alerts(t, "withdrawal:"+wr.ID.String()+":gate:WD050") != 1 {
		t.Fatal("the denying decision and the alert must be committed")
	}
	// RF4-6: no money moved - the hold is unchanged, no attempt, the ledger balances.
	if h := w.holdBalance(t); h != holdBefore {
		t.Fatalf("the hold must stay, before %d after %d", holdBefore, h)
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
		t.Fatalf("no attempt may exist, got %d", n)
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
	// Reactivated: the re-submit is evaluated again and goes through.
	w.setAccount(t, identity.PlayerStatusActive)
	if claim, err := w.claim(t, wr.ID); err != nil || claim.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("after reactivation the re-submit goes through: %v", err)
	}
}

// An unavailable gate fails CLOSED and retryable: nothing is written in the claim (no decision, no attempt, no state
// change), only the detached "unavailable" audit label; a nil gate is the same.
func TestF4_Claim_GateUnavailable_FailsClosed(t *testing.T) {
	w := newF4World(t, "f4-unavail")
	for name, g := range map[string]withdrawalgate.Gate{"failing": f4FailingGate{}, "nil": nil} {
		wr := approvedWithdrawal(t, w.pool, w.f, 300, "f4-unavail-"+name)
		w.orch.WithWithdrawalGate(g)
		_, err := w.claim(t, wr.ID)
		if !errors.Is(err, ErrPayoutWithdrawalGateUnavailable) || !errors.Is(err, withdrawal.ErrGateUnavailable) {
			t.Fatalf("%s gate: want ErrPayoutWithdrawalGateUnavailable, got %v", name, err)
		}
		if got := w.state(t, wr.ID); got.State != withdrawal.StateApproved {
			t.Fatalf("%s gate: the request must stay approved, got %s", name, got.State)
		}
		if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 0 {
			t.Fatalf("%s gate: no attempt, got %d", name, n)
		}
		if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1`, wr.ID); n != 0 {
			t.Fatalf("%s gate: no decision row, got %d", name, n)
		}
		if n := w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1
			AND (metadata->>'denied_by_withdrawal_gate_unavailable')::boolean`, wr.ID.String()); n != 1 {
			t.Fatalf("%s gate: want the detached unavailable audit label, got %d", name, n)
		}
		// Retry once the gate is back: goes through.
		w.orch.WithWithdrawalGate(standard.New())
		if claim, err := w.claim(t, wr.ID); err != nil || claim.Request.State != withdrawal.StateSubmitted {
			t.Fatalf("%s gate: the retry goes through: %v", name, err)
		}
	}
}

// Replay / idempotency: a second claim of an already-submitted request (a staff double-submit, a client retry) is
// refused by the state check as before and NEVER re-evaluates the gate or writes a decision.
func TestF4_Claim_Replay_IsNotReEvaluated(t *testing.T) {
	w := newF4World(t, "f4-replay")
	spy := &countingGate{inner: standard.New()}
	w.orch.WithWithdrawalGate(spy)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-replay")
	if _, err := w.claim(t, wr.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.claim(t, wr.ID); !errors.Is(err, withdrawal.ErrStateConflict) {
			t.Fatalf("a replayed claim must be the state conflict, got %v", err)
		}
	}
	if c := spy.calls.Load(); c != 1 {
		t.Fatalf("the gate must be evaluated once (the first claim), got %d", c)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1`, wr.ID); n != 1 {
		t.Fatalf("one decision row, got %d", n)
	}
	// Tenant isolation: another tenant's claim of this id finds nothing and evaluates nothing.
	other := seedPayoutFixture(t, w.pool, 0, true)
	if _, err := w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, other.tenantID, wr.ID, "bank_transfer", testSubmitActor()); !errors.Is(err, withdrawal.ErrNotFound) {
		t.Fatalf("a foreign tenant's claim must be not found, got %v", err)
	}
	if c := spy.calls.Load(); c != 1 {
		t.Fatalf("a foreign tenant's claim must not evaluate the gate, got %d calls", c)
	}
}

// Lock order (ADR 0082 A7-N1): the gate runs BEFORE the request's L1 row lock: with another transaction holding the
// withdrawal row FOR UPDATE, the gate is evaluated (and its L0.4 lock taken) while the claim then waits on L1.
func TestF4_Claim_GateRunsBeforeTheRowLock(t *testing.T) {
	w := newF4World(t, "f4-order")
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-order")
	evaluated := make(chan struct{}, 1)
	w.orch.WithWithdrawalGate(&countingGate{inner: standard.New(), onEvaluate: func() { evaluated <- struct{}{} }})
	blocker := a7HoldRow(t, w.pool, w.f.tenantID, "withdrawal_requests", wr.ID, "f4-l1-blocker")
	done := make(chan error, 1)
	go func() {
		_, err := w.claim(t, wr.ID)
		done <- err
	}()
	select {
	case <-evaluated:
	case <-time.After(20 * time.Second):
		blocker.release()
		t.Fatal("the gate must be evaluated while another transaction holds the request's L1 lock (gate before L1)")
	}
	select {
	case err := <-done:
		blocker.release()
		t.Fatalf("the claim must wait on L1 after the gate, it returned %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	blocker.release()
	if err := <-done; err != nil {
		t.Fatalf("the claim completes once L1 is released: %v", err)
	}
}

// Concurrency with request and approval on the SAME player: claims of approved requests race new requests (L0.4 then
// ledger locks) and approvals of other requests; no deadlock, every operation succeeds, the ledger balances.
func TestF4_Claim_ConcurrentWithRequestAndApproval_NoDeadlock(t *testing.T) {
	w := newF4World(t, "f4-conc")
	mustSetWithdrawalPolicy(t, w.pool, w.f.tenantID, "EUR", 1_000_000, 2)
	for i := 0; i < 8; i++ {
		toClaim := approvedWithdrawal(t, w.pool, w.f, 100, "f4-conc-claim-"+uuid.NewString())
		toApprove := approvedWithdrawalPendingOnly(t, w.pool, w.f, 100, "f4-conc-appr-"+uuid.NewString())
		var wg sync.WaitGroup
		errs := make([]error, 3)
		start := make(chan struct{})
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = w.claim(t, toClaim.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[1] = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
					TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID,
					WalletID: w.f.walletID, AssetCode: "EUR", Amount: 100, IdempotencyKey: "f4-conc-req-" + uuid.NewString(),
					PayoutInstrumentID: w.f.instrumentID, Destinations: pitest.Shared(), Gate: standard.New(),
				})
				return err
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[2] = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := withdrawal.Approve(ctx, tx, toApprove.ID, uuid.New(), true, nil, nil, auth.StepUpAssertion{})
				return err
			})
		}()
		close(start)
		wg.Wait()
		for j, e := range errs {
			if e != nil {
				t.Fatalf("round %d op %d: %v (SQLSTATE %q)", i, j, e, f4SQLState(e))
			}
		}
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
}

// approvedWithdrawalPendingOnly is approvedWithdrawal stopped at pending_review (the racer approves it).
func approvedWithdrawalPendingOnly(t *testing.T, pool *db.Pool, f payoutFixture, amount int64, key string) withdrawal.WithdrawalRequest {
	t.Helper()
	var wr withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID,
			WalletID: f.walletID, AssetCode: "EUR", Amount: amount, IdempotencyKey: key,
			PayoutInstrumentID: f.instrumentID, Destinations: pitest.Shared(), Gate: standard.New(),
		})
		if err != nil {
			return err
		}
		return withdrawal.MoveToPendingReview(ctx, tx, wr.ID)
	}); err != nil {
		t.Fatal(err)
	}
	return wr
}

// T2 re-claim of a never-sent attempt: an account suspended after T1p refuses the re-claim (escalated, T16: still
// created, no resend, no hold release), with a denying re-dispatch decision, the system audit and the alert. A
// second tick on the escalated attempt is an idempotent reschedule (no new row).
func TestF4_Sweeper_T2_DenialEscalates_NeverResends(t *testing.T) {
	w := newF4World(t, "f4-t2")
	wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "f4-t2")
	w.setAccount(t, identity.PlayerStatusSuspended)
	if err := w.sweeper().reclaimPayoutCreated(context.Background(), w.f.tenantID, attempt); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	after := w.reread(t, attempt.ID)
	if after.State != AttemptCreated || after.EscalatedAt == nil || after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("want created + escalated + no resend, got %s escalated=%v submits %d->%d", after.State, after.EscalatedAt != nil, attempt.SubmitCount, after.SubmitCount)
	}
	if got := w.state(t, wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("the withdrawal stays submitted (no release), got %s", got.State)
	}
	if w.decisions(t, wr.ID, "redispatch", false) != 1 || w.alerts(t, "payout_attempt:"+attempt.ID.String()+":gate:WD050") != 1 {
		t.Fatal("the denying re-dispatch decision and the alert must be committed")
	}
	if n := w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'payments.payout_redispatch_denied_by_withdrawal_gate' AND target_id = $1`, attempt.ID.String()); n != 1 {
		t.Fatalf("one system denial audit, got %d", n)
	}
	if err := w.sweeper().reclaimPayoutCreated(context.Background(), w.f.tenantID, after); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if w.decisions(t, wr.ID, "redispatch", false) != 1 {
		t.Fatal("an already-escalated attempt is rescheduled, no new decision row")
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
}

// T2 allow: the re-claim records an allowing re-dispatch decision bound to the attempt and resends.
func TestF4_Sweeper_T2_Allow_RecordsTheDecision(t *testing.T) {
	w := newF4World(t, "f4-t2a")
	wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "f4-t2a")
	if err := w.sweeper().reclaimPayoutCreated(context.Background(), w.f.tenantID, attempt); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if after := w.reread(t, attempt.ID); after.State != AttemptPending {
		t.Fatalf("want the re-claimed attempt pending, got %s", after.State)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'redispatch' AND allowed AND payment_attempt_id = $2`, wr.ID, attempt.ID); n != 1 {
		t.Fatalf("one allowing re-dispatch decision bound to the attempt, got %d", n)
	}
}

// T12 resend: a risk rule added after T1p refuses the resend (escalated, still ambiguous, submit_count unchanged).
func TestF4_Sweeper_T12_RiskDenialEscalates_NeverResends(t *testing.T) {
	w := newF4World(t, "f4-t12")
	// An idempotent manifest, so T12 reaches the resend gates (a non-idempotent one escalates before any gate).
	w.provider.SetManifest(OperationManifest{IdempotentSubmission: true})
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "f4-t12")
	claim, err := w.claim(t, wr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatal(err)
	}
	attempt := w.reread(t, claim.Attempt.ID)
	w.addRiskRule(t, 400, risk.ActionDeny)
	if err := w.sweeper().resubmitPayoutAmbiguous(context.Background(), w.f.tenantID, attempt); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	after := w.reread(t, attempt.ID)
	if after.State != AttemptAmbiguous || after.EscalatedAt == nil || after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("want ambiguous + escalated + no resend, got %s escalated=%v submits %d->%d", after.State, after.EscalatedAt != nil, attempt.SubmitCount, after.SubmitCount)
	}
	if w.decisions(t, wr.ID, "redispatch", false) != 1 || w.alerts(t, "payout_attempt:"+attempt.ID.String()+":gate:WD051") != 1 {
		t.Fatal("the denying re-dispatch decision and the alert must be committed")
	}
}

// The 0169 backstop on the payout attempt: created -> submitting (T2) without an allowing re-dispatch decision of the
// same transaction is refused WD053; with one but a suspended account WD052; with one and an active account it passes.
// A deposit attempt is not concerned (the WHEN clause).
func TestF4_DB_RedispatchBackstop(t *testing.T) {
	w := newF4World(t, "f4-dbre")
	wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "f4-dbre")
	reclaim := func(record bool) error {
		return w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if record {
				cur, err := withdrawal.GetByID(ctx, tx, wr.ID)
				if err != nil {
					return err
				}
				id := attempt.ID
				if err := withdrawal.RecordDispatchGateDecision(ctx, tx, cur, withdrawal.DispatchStageRedispatch, &id, withdrawalgate.Decision{Allowed: true, Outcome: "allow"}); err != nil {
					return err
				}
			}
			return ClaimCreatedForSubmission(ctx, tx, attempt.ID, *attempt.ProviderID, uuid.New(), "f4", time.Now().Add(time.Minute))
		})
	}
	if c := f4SQLState(reclaim(false)); c != "WD053" {
		t.Fatalf("T2 without a decision: want WD053, got %q", c)
	}
	w.setAccount(t, identity.PlayerStatusSuspended)
	if c := f4SQLState(reclaim(true)); c != "WD052" {
		t.Fatalf("T2 with a decision on a suspended account: want WD052, got %q", c)
	}
	w.setAccount(t, identity.PlayerStatusActive)
	if err := reclaim(true); err != nil {
		t.Fatalf("T2 with an allowing decision: %v", err)
	}
	if got := w.reread(t, attempt.ID); got.State != AttemptSubmitting {
		t.Fatalf("want submitting, got %s", got.State)
	}
}

// f4ScratchClearGateDecisions is for a test that steps a THROWAWAY scratch database down from head to an older
// migration: every E7 since 0169 records a withdrawal_gate_decisions row and 0169's down refuses (WD095) while one
// exists, so the chain would stop at 0169 instead of reaching the down the test pins. The scratch owner removes the
// rows (the append-only trigger and FORCE RLS lifted for that one statement, both restored in the same transaction).
func f4ScratchClearGateDecisions(t *testing.T, pool *db.Pool) {
	t.Helper()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE withdrawal_gate_decisions DISABLE TRIGGER withdrawal_gate_decisions_immutable`,
			`ALTER TABLE withdrawal_gate_decisions NO FORCE ROW LEVEL SECURITY`,
			`DELETE FROM withdrawal_gate_decisions`,
			`ALTER TABLE withdrawal_gate_decisions FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE withdrawal_gate_decisions ENABLE TRIGGER withdrawal_gate_decisions_immutable`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scratch owner: clear the 0169 decision rows: %v", err)
	}
}

// f4AllowIfPayout records the fixture ALLOW (stage redispatch) when attemptID is a PAYOUT attempt; a deposit attempt
// is untouched (0169 binds payouts only).
func f4AllowIfPayout(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) error {
	var wr *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT withdrawal_request_id FROM payment_attempts WHERE id = $1`, attemptID).Scan(&wr); err != nil {
		return err
	}
	if wr == nil {
		return nil
	}
	return f4RecordRedispatchAllow(ctx, tx, *wr, attemptID)
}

// f4ClaimCreatedForSubmission / f4ResubmitAmbiguous are the FIXTURE forms of the T2 / T12 CAS for tests that drive a
// payout attempt directly (not through the sweeper): the fixture records the allowing re-dispatch decision migration
// 0169 requires (WD053), then runs the unchanged CAS. The gate itself is tested in this file.
func f4ClaimCreatedForSubmission(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, providerID string, claimToken uuid.UUID, leaseOwner string, leaseUntil time.Time) error {
	if err := f4AllowIfPayout(ctx, tx, attemptID); err != nil {
		return err
	}
	return ClaimCreatedForSubmission(ctx, tx, attemptID, providerID, claimToken, leaseOwner, leaseUntil)
}

func f4ResubmitAmbiguous(ctx context.Context, tx pgx.Tx, attemptID, claimToken uuid.UUID, leaseOwner string, leaseUntil time.Time, maxResubmits int) error {
	if err := f4AllowIfPayout(ctx, tx, attemptID); err != nil {
		return err
	}
	return ResubmitAmbiguous(ctx, tx, attemptID, claimToken, leaseOwner, leaseUntil, maxResubmits)
}

// f4MarkSubmittedPending is the FIXTURE form of E7 for tests that walk it directly: the allowing dispatch decision
// first (WD053), then the unchanged writer.
func f4MarkSubmittedPending(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, providerID string) error {
	if err := f4RecordDispatchAllow(ctx, tx, requestID); err != nil {
		return err
	}
	return withdrawal.MarkSubmittedPending(ctx, tx, requestID, providerID)
}

// f4RecordDispatchAllow is the FIXTURE step for a test that walks E7 directly (withdrawal.MarkSubmitted /
// MarkSubmittedPending, not ClaimForDispatch): it records the allowing dispatch decision migration 0169 requires in the
// transaction (WD053).
func f4RecordDispatchAllow(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) error {
	cur, err := withdrawal.GetByID(ctx, tx, requestID)
	if err != nil {
		return err
	}
	return withdrawal.RecordDispatchGateDecision(ctx, tx, cur, withdrawal.DispatchStageDispatch, nil, withdrawalgate.Decision{Allowed: true, Outcome: "allow"})
}

// f4RecordRedispatchAllow is the FIXTURE step for a test that drives a payout T2 / T12 directly (not through the
// sweeper): it records the allowing re-dispatch decision migration 0169 requires in the transaction (WD053).
func f4RecordRedispatchAllow(ctx context.Context, tx pgx.Tx, requestID, attemptID uuid.UUID) error {
	cur, err := withdrawal.GetByID(ctx, tx, requestID)
	if err != nil {
		return err
	}
	return withdrawal.RecordDispatchGateDecision(ctx, tx, cur, withdrawal.DispatchStageRedispatch, &attemptID, withdrawalgate.Decision{Allowed: true, Outcome: "allow"})
}
