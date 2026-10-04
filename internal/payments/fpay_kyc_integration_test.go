//go:build integration

// PRH-2 F-pay tests: PAY-KYC-UNAVAIL-1 (security C-F1, ledger-finance F-2),
// DECISION-ROWS-1 (deposit gate, payout T1p allow, T2/T12) and
// PAY-PAYOUT-ERRREF-1 (payout adapter error path reference validation).
//
// Test rules (plan §5.0): no time.Sleep and no wall-clock assertion. A KYC
// store outage is injected deterministically: a second transaction holds
// LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE (the LOCK statement
// only returns once the lock is held), and the gate under test runs
// `SET LOCAL lock_timeout` first, so the evaluator's read fails promptly
// with 55P03 inside its savepoint - the same technique F-kyc used.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// lockTimeoutPayoutGate runs the REAL payout gate after setting a short
// lock_timeout on the caller's transaction (the test-only knob).
type lockTimeoutPayoutGate struct{}

func (lockTimeoutPayoutGate) EvaluatePayout(ctx context.Context, tx pgx.Tx, p kyc.EnforcementParams) (kyc.EnforcementDecision, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		return kyc.EnforcementDecision{}, err
	}
	return KYCEnforcementPayoutGate{}.EvaluatePayout(ctx, tx, p)
}

func fpCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func fpDecisions(t *testing.T, pool *db.Pool, f payoutFixture, op, outcome string) int {
	return fpCount(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND player_account_id=$2 AND operation=$3 AND outcome=$4`,
		f.tenantID, f.playerAccountID, op, outcome)
}

func fpLedgerTx(t *testing.T, pool *db.Pool, f payoutFixture) int {
	return fpCount(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, f.tenantID)
}

func fpSubmitAudits(t *testing.T, pool *db.Pool, tenantID, requestID uuid.UUID, outcome string) int {
	return fpCount(t, pool, tenantID,
		`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='withdrawal.submit.http' AND target_id=$2 AND outcome=$3`,
		tenantID, requestID.String(), outcome)
}

func fpDecisionAudits(t *testing.T, pool *db.Pool, f payoutFixture, action string) int {
	return fpCount(t, pool, f.tenantID,
		`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`,
		f.tenantID, action, f.playerAccountID.String())
}

func fpLockKYC(t *testing.T, pool *db.Pool, tenantID uuid.UUID) *loBlocker {
	return loHoldWith(t, pool, tenantID, "kyc-verifications-access-exclusive", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE`)
		return err
	})
}

func fpOrch(t *testing.T, pool *db.Pool, id string) (payoutFixture, *Orchestrator, *MockProvider) {
	t.Helper()
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider(id, "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{id: provider}, MultiWebhookCredentialResolver{id: NewMockWebhookCredentials(provider)})
	return f, orch, provider
}

func fpReqState(t *testing.T, pool *db.Pool, f payoutFixture, id uuid.UUID) withdrawal.WithdrawalRequest {
	t.Helper()
	var wr withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.GetByID(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatalf("reread request: %v", err)
	}
	return wr
}

// PAY-KYC-UNAVAIL-1 (a)+(c): an outage at staff submit is retryable
// (ErrPayoutKYCUnavailable, which the handler maps to 503), the request stays
// approved, exactly one `unavailable` decision row and one denied submit
// audit commit, and there is no attempt row and no ledger posting.
func TestClaimForDispatch_KYCStoreOutage_RetryableAndRecorded(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-fpay-a")
	wr := approvedWithdrawal(t, pool, f, 500, "fpay-t1p-outage")
	ledgerBefore := fpLedgerTx(t, pool, f)
	decisionsBefore := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND operation='withdrawal_payout'`, f.tenantID)

	blocker := fpLockKYC(t, pool, f.tenantID)
	claim, err := orch.ClaimForDispatch(context.Background(), pool, lockTimeoutPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	blocker.release()

	if !errors.Is(err, ErrPayoutKYCUnavailable) {
		t.Fatalf("expected ErrPayoutKYCUnavailable (retryable), got %v", err)
	}
	if errors.Is(err, withdrawal.ErrKYCUnavailable) {
		t.Fatalf("an outage must not surface as DenyForCompliance's ErrKYCUnavailable (the unhandled-500 path): %v", err)
	}
	if claim.Denied || claim.Attempt.ID != uuid.Nil {
		t.Fatalf("expected an empty ClaimResult, got %+v", claim)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateApproved || got.ReleaseLedgerTransactionID != nil {
		t.Fatalf("request must stay approved with the hold in place, got state=%s release=%v", got.State, got.ReleaseLedgerTransactionID)
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "unavailable"); n != 1 {
		t.Fatalf("expected exactly 1 unavailable decision row, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND operation='withdrawal_payout'`, f.tenantID); n != decisionsBefore+1 {
		t.Fatalf("expected exactly one new payout decision row, before=%d after=%d", decisionsBefore, n)
	}
	if n := fpSubmitAudits(t, pool, f.tenantID, wr.ID, "denied"); n != 1 {
		t.Fatalf("expected exactly 1 denied withdrawal.submit.http audit row, got %d", n)
	}
	if n := fpSubmitAudits(t, pool, f.tenantID, wr.ID, "success"); n != 0 {
		t.Fatalf("expected no success submit audit, got %d", n)
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
		t.Fatalf("expected 0 payment_attempts rows, got %d", n)
	}
	if n := fpLedgerTx(t, pool, f); n != ledgerBefore {
		t.Fatalf("expected no ledger posting, ledger_transactions %d -> %d", ledgerBefore, n)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)

	// Retry after the outage ends succeeds with the real gate: nothing was
	// stranded.
	retry, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil || retry.Denied || retry.Attempt.State != AttemptSubmitting {
		t.Fatalf("retry after the outage must claim cleanly, err=%v claim=%+v", err, retry)
	}
}

// DECISION-ROWS-1: one decision row per evaluation at T1p allow and T1p deny.
func TestClaimForDispatch_OneDecisionRowPerEvaluation_AllowAndDeny(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-fpay-b")
	wr := approvedWithdrawal(t, pool, f, 500, "fpay-t1p-allow")
	before := fpDecisions(t, pool, f, "withdrawal_payout", "passed")
	if _, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "passed"); n != before+1 {
		t.Fatalf("T1p allow: expected exactly 1 new passed decision row, got %d", n-before)
	}
	if n := fpDecisionAudits(t, pool, f, "kyc.enforcement_allowed"); n < 1 {
		t.Fatalf("T1p allow: expected the decision audit row")
	}

	f2, orch2, _ := fpOrch(t, pool, "mock-fpay-b2")
	wr2 := approvedWithdrawal(t, pool, f2, 500, "fpay-t1p-deny")
	revokeVerification(t, pool, f2)
	claim, err := orch2.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f2.tenantID, wr2.ID, "bank_transfer", testSubmitActor())
	if err != nil || !claim.Denied {
		t.Fatalf("deny: err=%v claim=%+v", err, claim)
	}
	if n := fpDecisions(t, pool, f2, "withdrawal_payout", "failed"); n != 1 {
		t.Fatalf("T1p deny: expected exactly 1 failed decision row, got %d", n)
	}
	// Tenant isolation / RLS: f's tenant must not see f2's rows and vice versa.
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE player_account_id=$1`, f2.playerAccountID); n != 0 {
		t.Fatalf("RLS: tenant A read %d of tenant B's decision rows", n)
	}
	if n := fpCount(t, pool, f2.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE player_account_id=$1`, f.playerAccountID); n != 0 {
		t.Fatalf("RLS: tenant B read %d of tenant A's decision rows", n)
	}
}

// A kill-switch / routing rollback discards the allow row together with the
// claim (never a row for a claim that did not happen).
func TestClaimForDispatch_NoRoutableProvider_NoAllowDecisionRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true) // no capability registered
	orch := NewOrchestrator(map[string]PaymentProvider{}, MultiWebhookCredentialResolver{})
	wr := approvedWithdrawal(t, pool, f, 500, "fpay-t1p-noroute")
	if _, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err == nil {
		t.Fatalf("expected a routing error")
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "passed"); n != 0 {
		t.Fatalf("expected the allow row to roll back with the claim, got %d", n)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateApproved {
		t.Fatalf("expected approved, got %s", got.State)
	}
}

func fpAttempt(t *testing.T, pool *db.Pool, f payoutFixture, id uuid.UUID) PaymentAttempt {
	t.Helper()
	var a PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = GetAttemptByID(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	return a
}

// PAY-KYC-UNAVAIL-1 (b)+(c) at T2: an outage reschedules; never escalates,
// never resends, nothing terminal; one unavailable decision row.
func TestSweeper_T2Reclaim_KYCStoreOutage_ReschedulesNeverEscalates(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-fpay-c")
	wr, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "fpay-t2-outage")
	ledgerBefore := fpLedgerTx(t, pool, f)
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: lockTimeoutPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}

	blocker := fpLockKYC(t, pool, f.tenantID)
	err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt)
	blocker.release()
	if err != nil {
		t.Fatalf("an outage must reschedule, not error: %v", err)
	}
	after := fpAttempt(t, pool, f, attempt.ID)
	if after.State != AttemptCreated || after.EscalatedAt != nil || after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("expected created, un-escalated, no resend; got state=%s escalated=%v submit_count=%d", after.State, after.EscalatedAt, after.SubmitCount)
	}
	if after.NextActionAt == nil || attempt.NextActionAt == nil || !after.NextActionAt.After(*attempt.NextActionAt) {
		t.Fatalf("expected the attempt to be rescheduled (next_action_at pushed past the due time): before=%v after=%v", attempt.NextActionAt, after.NextActionAt)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected submitted, got %s", got.State)
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "unavailable"); n != 1 {
		t.Fatalf("expected exactly 1 unavailable decision row, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_reclaim_denied_by_kyc'`, f.tenantID); n != 0 {
		t.Fatalf("an outage must not write the deny audit, got %d", n)
	}
	if n := fpLedgerTx(t, pool, f); n != ledgerBefore {
		t.Fatalf("expected no ledger posting, %d -> %d", ledgerBefore, n)
	}
	// Recovery: the same attempt is re-claimed and dispatched once the store is back.
	healthy := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
	if err := healthy.reclaimPayoutCreated(context.Background(), f.tenantID, after); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rec := fpAttempt(t, pool, f, attempt.ID); rec.State == AttemptCreated {
		t.Fatalf("expected the attempt to progress after recovery")
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "passed"); n < 1 {
		t.Fatalf("expected the T2 allow decision row after recovery")
	}
}

// ... and at T12.
func TestSweeper_T12Resubmit_KYCStoreOutage_ReschedulesNeverEscalates(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, mp := fpOrch(t, pool, "mock-fpay-d")
	// T12 only reaches the KYC gate for a provider that declares idempotent
	// submission (otherwise it escalates earlier, for a different reason).
	manifest := mp.Capabilities().Manifest
	manifest.IdempotentSubmission = true
	mp.SetManifest(manifest)
	wr := approvedWithdrawal(t, pool, f, 500, "fpay-t12-outage")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (Ambiguous): %v", err)
	}
	attempt := fpAttempt(t, pool, f, claim.Attempt.ID)
	if attempt.State != AttemptAmbiguous {
		t.Fatalf("expected ambiguous, got %s", attempt.State)
	}
	ledgerBefore := fpLedgerTx(t, pool, f)
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: lockTimeoutPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}

	blocker := fpLockKYC(t, pool, f.tenantID)
	err = sweeper.resubmitPayoutAmbiguous(context.Background(), f.tenantID, attempt)
	blocker.release()
	if err != nil {
		t.Fatalf("an outage must reschedule, not error: %v", err)
	}
	after := fpAttempt(t, pool, f, attempt.ID)
	if after.State != AttemptAmbiguous || after.EscalatedAt != nil || after.SubmitCount != attempt.SubmitCount {
		t.Fatalf("expected ambiguous, un-escalated, no resend; got state=%s escalated=%v submit_count=%d", after.State, after.EscalatedAt, after.SubmitCount)
	}
	if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected submitted, got %s", got.State)
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "unavailable"); n != 1 {
		t.Fatalf("expected exactly 1 unavailable decision row, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_reclaim_denied_by_kyc'`, f.tenantID); n != 0 {
		t.Fatalf("an outage must not write the deny audit, got %d", n)
	}
	if n := fpLedgerTx(t, pool, f); n != ledgerBefore {
		t.Fatalf("expected no ledger posting, %d -> %d", ledgerBefore, n)
	}
}

// DECISION-ROWS-1 at T2: a genuine deny still escalates and records exactly
// one failed decision row (the pre-existing escalation behaviour is kept).
func TestSweeper_T2Reclaim_KYCDeny_RecordsOneDecisionRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-fpay-e")
	_, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "fpay-t2-deny")
	revokeVerification(t, pool, f)
	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("reclaimPayoutCreated: %v", err)
	}
	after := fpAttempt(t, pool, f, attempt.ID)
	if after.EscalatedAt == nil || after.State != AttemptCreated {
		t.Fatalf("a genuine deny must still escalate without resend, got state=%s escalated=%v", after.State, after.EscalatedAt)
	}
	if n := fpDecisions(t, pool, f, "withdrawal_payout", "failed"); n != 1 {
		t.Fatalf("expected exactly 1 failed decision row, got %d", n)
	}
}

// DECISION-ROWS-1 at the deposit gate (phase A and the cascade T2 are both
// served by KYCEnforcementDepositGate): one decision row per evaluation,
// committed with the attempt claim.
func TestDepositGate_RecordsOneDecisionRowPerEvaluation(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-fpay-dep", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fpay-dep": provider}, MultiWebhookCredentialResolver{"mock-fpay-dep": NewMockWebhookCredentials(provider)})
	cnt := func() int {
		return fpCount(t, pool, f.tenantID,
			`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND player_account_id=$2 AND operation='deposit'`, f.tenantID, f.playerAccountID)
	}
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, KYCEnforcementDepositGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "fpay-dep-1",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending {
		t.Fatalf("expected pending, got %s", res.Attempt.State)
	}
	if n := cnt(); n != 1 {
		t.Fatalf("expected exactly 1 deposit decision row for one evaluation, got %d", n)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND operation='deposit' AND allowed`, f.tenantID); n != 1 {
		t.Fatalf("expected the row to be an allow, got %d", n)
	}
	// The idempotent replay does not re-run phase A: still exactly one row.
	if _, err := orch.InitiateDepositAttempt(context.Background(), pool, KYCEnforcementDepositGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "fpay-dep-1",
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := cnt(); n != 1 {
		t.Fatalf("replay must not add a decision row, got %d", n)
	}
}

// Deposit gate under an outage: the evaluator's failure is contained by its
// savepoint, so the `unavailable` decision row is recorded in a still-usable
// transaction and the deposit is denied (fail closed).
func TestDepositGate_StoreOutage_RecordsUnavailableAndDenies(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	// Bind a licence (test fixture only; no KYC threshold or policy is
	// seeded) so the evaluation reads `licences`, then hold that table.
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		var jid, lid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO jurisdictions (code, name) VALUES ($1, 'Test Jurisdiction') RETURNING id`, "FP-"+uuid.NewString()[:8]).Scan(&jid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', 'FP-1') RETURNING id`, jid).Scan(&lid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, f.tenantID)
		return err
	}); err != nil {
		t.Fatalf("bind licence: %v", err)
	}
	blocker := loHoldWith(t, pool, f.tenantID, "licences-access-exclusive", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `LOCK TABLE licences IN ACCESS EXCLUSIVE MODE`)
		return err
	})
	var allowed bool
	var reason string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
			return err
		}
		var err error
		allowed, reason, err = KYCEnforcementDepositGate{}.EvaluateDeposit(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, uuid.New(), 5000, "EUR")
		return err
	})
	blocker.release()
	if err != nil {
		t.Fatalf("EvaluateDeposit under outage must not error (savepoint-contained): %v", err)
	}
	if allowed || !strings.HasPrefix(reason, "kyc_unavailable:") {
		t.Fatalf("expected a fail-closed unavailable deny, got allowed=%v reason=%q", allowed, reason)
	}
	if n := fpCount(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id=$1 AND operation='deposit' AND outcome='unavailable' AND NOT allowed`, f.tenantID); n != 1 {
		t.Fatalf("expected exactly 1 unavailable deposit decision row, got %d", n)
	}
}

// PAY-PAYOUT-ERRREF-1: an adapter error TOGETHER with a hostile reference
// must classify as ProviderRefInvalid, scrubbed, before the error return.
type errRefWithdrawProvider struct {
	*MockProvider
	ref string
	err error
	out Outcome
}

func (p *errRefWithdrawProvider) Withdraw(context.Context, WithdrawRequest) (WithdrawResult, error) {
	return WithdrawResult{Outcome: p.out, ProviderReference: p.ref, DeclineReason: "raw-vendor-text"}, p.err
}

func TestPayoutAdapterCall_ErrorPathReferenceValidation(t *testing.T) {
	long := strings.Repeat("a", providerref.MaxBytes+1)
	cases := []struct {
		name      string
		ref       string
		wantClass ErrorClass
	}{
		{"oversize", long, ErrorClassProviderRefInvalid},
		{"control_char", "ref\x00evil\n", ErrorClassProviderRefInvalid},
		{"valid", "psp-ok-123", ErrorClassAmbiguous},
		{"empty", "", ErrorClassAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &errRefWithdrawProvider{MockProvider: NewMockProvider("m", "EUR"), ref: tc.ref, err: errors.New("boom"), out: OutcomePending}
			res, class, err := payoutAdapterCall(p, PaymentAttempt{})(context.Background(), CallContext{})
			if class != tc.wantClass {
				t.Fatalf("class = %s, want %s", class, tc.wantClass)
			}
			if err == nil {
				t.Fatalf("an error must still be returned")
			}
			if tc.wantClass == ErrorClassProviderRefInvalid {
				if _, ok := providerref.AsError(err); !ok {
					t.Fatalf("expected a *providerref.Error, got %v", err)
				}
				if res.ProviderReference != "" || res.DeclineReason != "" {
					t.Fatalf("the invalid reference must be scrubbed from the result, got %+v", res)
				}
				if strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), long) {
					t.Fatalf("the raw value leaked into the error text: %v", err)
				}
			}
		})
	}
}

// End to end: error + hostile reference through DispatchWithdraw and phase C
// parks the attempt (T10), persists no raw value anywhere, and does not loop.
func TestPayoutDispatch_ErrorPlusHostileReference_ParksNoRawPersistence(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{
		{"oversize", strings.Repeat("z", providerref.MaxBytes+1)},
		{"control_char", "bad\x07ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			pid := "mock-fpay-ref-" + strings.ReplaceAll(tc.name, "_", "-")
			f, _, mp := fpOrch(t, pool, pid)
			prov := &errRefWithdrawProvider{MockProvider: mp, ref: tc.ref, err: errors.New("upstream 500"), out: OutcomePending}
			orch := NewOrchestrator(map[string]PaymentProvider{pid: prov}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(mp)})
			wr := approvedWithdrawal(t, pool, f, 500, "fpay-errref-"+tc.name)
			claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			if err != nil {
				t.Fatalf("ClaimForDispatch: %v", err)
			}
			gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, prov, claim.Attempt)
			if gr.Class != ErrorClassProviderRefInvalid {
				t.Fatalf("expected ProviderRefInvalid, got %s (err=%v)", gr.Class, gr.Err)
			}
			if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
				t.Fatalf("phase C must park, not error/loop: %v", err)
			}
			a := fpAttempt(t, pool, f, claim.Attempt.ID)
			if a.State != AttemptDisputed {
				t.Fatalf("expected the attempt parked as disputed, got %s", a.State)
			}
			if a.ProviderReference != nil && *a.ProviderReference != "" {
				t.Fatalf("raw reference persisted on the attempt")
			}
			got := fpReqState(t, pool, f, wr.ID)
			if got.State != withdrawal.StateSubmitted || (got.ProviderReference != nil && *got.ProviderReference != "") {
				t.Fatalf("expected submitted with no reference, got state=%s ref=%v", got.State, got.ProviderReference)
			}
			// No raw value in the audit trail.
			if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND metadata::text LIKE '%' || $2 || '%'`, f.tenantID, tc.ref); n != 0 {
				t.Fatalf("raw reference found in %d audit rows", n)
			}
			if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_parked_invalid_reference'`, f.tenantID); n != 1 {
				t.Fatalf("expected exactly one park audit, got %d", n)
			}
			loAssertBalanced(t, pool, f.tenantID)
		})
	}
}
