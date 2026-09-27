//go:build integration

// RV-PRH-I1 fix round: code review (rv-prh-i1-payout-code-review.md) +
// ledger-finance review (rv-prh-i1-payout-ledger.md) required tests.
// Runs on the same private scratch-DB harness as
// payout_dispatch_integration_test.go.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// --- balance helpers (concrete account assertions ledger-finance required) -

func heldForWithdrawal(t *testing.T, pool *db.Pool, f payoutFixture) int64 {
	t.Helper()
	var held int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetByID(ctx, tx, f.walletID)
		if err != nil {
			return err
		}
		summary, err := wallet.GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		held = summary.HeldForWithdrawal
		return nil
	})
	if err != nil {
		t.Fatalf("read held-for-withdrawal balance: %v", err)
	}
	return held
}

// clearingBalance reads the platform-wide psp_clearing account's net
// (credit-debit) balance for assetCode - the account withdrawal.Complete
// credits on settlement.
func clearingBalance(t *testing.T, pool *db.Pool, tenantID uuid.UUID, assetCode string) int64 {
	t.Helper()
	var bal int64
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		id, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, nil, ledger.AccountPSPClearing, assetCode)
		if err != nil {
			return err
		}
		b, err := ledger.GetProjectedBalance(ctx, tx, id)
		if err != nil {
			return err
		}
		bal = b.CreditTotal - b.DebitTotal
		return nil
	})
	if err != nil {
		t.Fatalf("read psp_clearing balance: %v", err)
	}
	return bal
}

// --- synchronous success/decline provider wrappers --------------------------

// syncSuccessWithdrawProvider makes Withdraw return a definite,
// synchronous success with a reference - the mock adapter's own default
// Withdraw behavior is always OutcomePending, so ledger-finance's required
// "drive T2/T12 to a definite success and assert balances" tests need this.
type syncSuccessWithdrawProvider struct {
	*MockProvider
	ref string
}

func (p *syncSuccessWithdrawProvider) Withdraw(_ context.Context, req WithdrawRequest) (WithdrawResult, error) {
	return WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: p.ref}, nil
}

func (p *syncSuccessWithdrawProvider) QueryStatus(_ context.Context, ref string) (StatusResult, error) {
	return StatusResult{ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}, nil
}

// syncDeclineWithdrawProvider makes Withdraw return a definite decline.
type syncDeclineWithdrawProvider struct {
	*MockProvider
	reason string
}

func (p *syncDeclineWithdrawProvider) Withdraw(_ context.Context, req WithdrawRequest) (WithdrawResult, error) {
	return WithdrawResult{Outcome: OutcomeDeclined, DeclineReason: p.reason}, nil
}

// ambiguousThenResult lets a test control exactly what Withdraw/QueryStatus
// return per call, in order - used to drive T12's poll-then-resend-or-
// escalate branches deterministically.
type scriptedWithdrawProvider struct {
	*MockProvider
	mu           sync.Mutex
	withdraws    []WithdrawResult
	withdrawErrs []error
	statuses     []StatusResult
	statusErrs   []error
	widx, sidx   int
	withdrawN    int
}

func (p *scriptedWithdrawProvider) Withdraw(_ context.Context, _ WithdrawRequest) (WithdrawResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.withdrawN++
	i := p.widx
	if i >= len(p.withdraws) {
		i = len(p.withdraws) - 1
	}
	var err error
	if i < len(p.withdrawErrs) {
		err = p.withdrawErrs[i]
	}
	p.widx++
	return p.withdraws[i], err
}

func (p *scriptedWithdrawProvider) QueryStatus(_ context.Context, ref string) (StatusResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.sidx
	if i >= len(p.statuses) {
		i = len(p.statuses) - 1
	}
	var err error
	if i < len(p.statusErrs) {
		err = p.statusErrs[i]
	}
	p.sidx++
	res := p.statuses[i]
	if res.ProviderReference == "" {
		res.ProviderReference = ref
	}
	return res, err
}

// --- H1/settle/release concrete balance assertions --------------------------

// TestPayoutDispatch_T1p_SyncSuccess_SettlesConcreteBalances proves the
// success path (T1p's own synchronous dispatch, not a resend) actually
// moves money: player_withdrawal_hold -> 0, psp_clearing credited by the
// withdrawn amount, and the withdrawal/attempt both reach a definite
// terminal state - not just "pending" (ledger-finance: "EndToEnd_Success
// currently asserts pending").
func TestPayoutDispatch_T1p_SyncSuccess_SettlesConcreteBalances(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-a", "EUR")
	provider := &syncSuccessWithdrawProvider{MockProvider: inner, ref: "sync-success-ref-1"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-a": provider}, MultiWebhookCredentialResolver{"mock-fix-a": NewMockWebhookCredentials(inner)})

	heldBefore := heldForWithdrawal(t, pool, f)
	clearingBefore := clearingBalance(t, pool, f.tenantID, "EUR")

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-sync-success")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var got withdrawal.WithdrawalRequest
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateCompleted {
		t.Fatalf("expected completed, got %s", got.State)
	}
	if attempt.State != AttemptSucceeded {
		t.Fatalf("expected attempt succeeded, got %s", attempt.State)
	}
	if held := heldForWithdrawal(t, pool, f); held != heldBefore {
		t.Fatalf("expected held-for-withdrawal to return to baseline (fully released/settled), before=%d after=%d", heldBefore, held)
	}
	if clearing := clearingBalance(t, pool, f.tenantID, "EUR"); clearing != clearingBefore+500 {
		t.Fatalf("expected psp_clearing to gain 500, before=%d after=%d", clearingBefore, clearing)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_T1p_SyncDecline_ReleasesConcreteBalances is the
// decline/release mirror.
func TestPayoutDispatch_T1p_SyncDecline_ReleasesConcreteBalances(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-b", "EUR")
	provider := &syncDeclineWithdrawProvider{MockProvider: inner, reason: "account_closed"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-b": provider}, MultiWebhookCredentialResolver{"mock-fix-b": NewMockWebhookCredentials(inner)})

	cashBefore := cashBalance(t, pool, f.orchFixture)
	heldBefore := heldForWithdrawal(t, pool, f)

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-sync-decline")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var got withdrawal.WithdrawalRequest
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateFailed {
		t.Fatalf("expected failed, got %s", got.State)
	}
	if attempt.State != AttemptDeclined {
		t.Fatalf("expected attempt declined, got %s", attempt.State)
	}
	if held := heldForWithdrawal(t, pool, f); held != heldBefore {
		t.Fatalf("expected held-for-withdrawal to return to baseline (fully released/settled), before=%d after=%d", heldBefore, held)
	}
	if cash := cashBalance(t, pool, f.orchFixture); cash != cashBefore {
		t.Fatalf("expected cash to be restored to baseline after release, before=%d after=%d", cashBefore, cash)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_T2Resend_SyncSuccess_SettlesConcreteBalances drives a
// NotSent -> T2 re-claim -> synchronous success and asserts the same
// concrete balances (M3/H1: a sweeper-driven resend must actually be able
// to reach `completed`, not roll back on the evidence-kind guard).
func TestPayoutDispatch_T2Resend_SyncSuccess_SettlesConcreteBalances(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-c", "EUR")
	provider := &syncSuccessWithdrawProvider{MockProvider: inner, ref: "sync-success-ref-t2"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-c": provider}, MultiWebhookCredentialResolver{"mock-fix-c": NewMockWebhookCredentials(inner)})

	wr, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "payout-fix-t2-success")

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("reclaimPayoutCreated: %v", err)
	}

	var got withdrawal.WithdrawalRequest
	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		after, err = GetAttemptByID(ctx, tx, attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateCompleted {
		t.Fatalf("expected completed after T2 resend success, got %s", got.State)
	}
	if after.State != AttemptSucceeded {
		t.Fatalf("expected attempt succeeded after T2 resend, got %s", after.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_T2Resend_SyncDecline_ReleasesConcreteBalances is H1's
// decline mirror: a T2 resend that declines must release the hold, not
// roll back and leave it stuck (the exact probe-B failure the ledger
// review reported).
func TestPayoutDispatch_T2Resend_SyncDecline_ReleasesConcreteBalances(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-d", "EUR")
	provider := &syncDeclineWithdrawProvider{MockProvider: inner, reason: "account_closed"}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-d": provider}, MultiWebhookCredentialResolver{"mock-fix-d": NewMockWebhookCredentials(inner)})

	wr, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "payout-fix-t2-decline")

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("reclaimPayoutCreated: %v", err)
	}

	var got withdrawal.WithdrawalRequest
	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		after, err = GetAttemptByID(ctx, tx, attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateFailed {
		t.Fatalf("expected failed after T2 resend decline (hold released), got %s", got.State)
	}
	if after.State != AttemptDeclined {
		t.Fatalf("expected attempt declined, got %s", after.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- C1/B1: idempotent vs non-idempotent T12 ---------------------------------

// TestSweeper_T12_NonIdempotentManifest_NeverResends is the CRITICAL C1/B1
// fix's core proof: an ambiguous payout on a non-idempotent manifest is
// polled once (still ambiguous) and then ESCALATED - Withdraw is called
// exactly once in total, no matter how many sweeper ticks pass.
func TestSweeper_T12_NonIdempotentManifest_NeverResends(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-fix-e", "EUR") // zero-value manifest: IdempotentSubmission=false
	spy := &withdrawCountingProvider{MockProvider: inner}
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-e": spy}, MultiWebhookCredentialResolver{"mock-fix-e": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "payout-fix-t12-nonidempotent")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, spy, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	if spy.count() != 1 {
		t.Fatalf("expected exactly 1 Withdraw call from T1p itself, got %d", spy.count())
	}

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	for i := 0; i < 6; i++ {
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return Touch(ctx, tx, claim.Attempt.ID)
		}); err != nil {
			t.Fatalf("touch: %v", err)
		}
		st := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
		if len(st.Errors) != 0 {
			t.Fatalf("sweep %d: unexpected errors: %v", i, st.Errors)
		}
	}

	if spy.count() != 1 {
		t.Fatalf("PROBE-A regression: %d Withdraw calls to a non-idempotent provider for one withdrawal (expected exactly 1)", spy.count())
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptAmbiguous {
		t.Fatalf("expected the attempt to remain ambiguous (escalated, not resent/resolved), got %s", attempt.State)
	}
	if attempt.EscalatedAt == nil {
		t.Fatalf("expected the attempt to be escalated")
	}
	if attempt.SubmitCount != 1 {
		t.Fatalf("expected submit_count to stay at 1 (no resend), got %d", attempt.SubmitCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestSweeper_T12_IdempotentManifest_StopsAtMaxResubmits proves the other
// half of C1/B1: an idempotent-submission provider IS resent, but never
// more than s.MaxResubmits times.
func TestSweeper_T12_IdempotentManifest_StopsAtMaxResubmits(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-fix-f", "EUR")
	spy := &withdrawCountingProvider{MockProvider: inner}
	// idempotentAmbiguousProvider always returns Ambiguous with a fresh
	// reference and declares IdempotentSubmission - so every resend is
	// "eligible" and the only thing that can stop it is the cap.
	idem := &idempotentAmbiguousProvider{withdrawCountingProvider: spy}
	registerCapability(t, pool, f.orchFixture, idem, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-f": idem}, MultiWebhookCredentialResolver{"mock-fix-f": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "payout-fix-t12-idempotent")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, idem, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, MaxResubmits: 2}
	for i := 0; i < 8; i++ {
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return Touch(ctx, tx, claim.Attempt.ID)
		}); err != nil {
			t.Fatalf("touch: %v", err)
		}
		sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	}

	// N5 (RV-PRH-I1 re-review): the CAS predicate is `submit_count <
	// maxSubmits`, evaluated BEFORE the increment, starting from
	// submit_count=1 after T1p - so submit_count can reach EXACTLY
	// MaxResubmits (not "1 + MaxResubmits"), via exactly
	// MaxResubmits-1 resends. With MaxResubmits=2: 1 initial Withdraw (T1p)
	// + 1 resend = 2 total, submit_count stops at 2. An earlier revision of
	// this test asserted only a loose upper bound ("<= 3"), which does not
	// distinguish a correct cap from a mutant that removes the cap check
	// entirely on a short run, or one that resends one time fewer/more than
	// the real formula (M3, RV-PRH-I1 code re-review: "a positive T12
	// resend on an idempotent manifest, with an EXACT count").
	if spy.count() != 2 {
		t.Fatalf("expected EXACTLY 2 total Withdraw calls (1 initial + MaxResubmits-1=1 resend), got %d", spy.count())
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.SubmitCount != 2 {
		t.Fatalf("expected submit_count to stop at EXACTLY 2 (cap reached), got %d", attempt.SubmitCount)
	}
	if attempt.EscalatedAt == nil {
		t.Fatalf("expected the attempt to be escalated once the cap is reached")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// idempotentAmbiguousProvider always answers Withdraw with Ambiguous plus
// a fresh reference and QueryStatus with Ambiguous too (never resolving),
// and declares IdempotentSubmission - the manifest-driven allow path.
type idempotentAmbiguousProvider struct {
	*withdrawCountingProvider
}

func (p *idempotentAmbiguousProvider) Capabilities() AdapterCapability {
	c := p.MockProvider.Capabilities()
	c.Manifest.IdempotentSubmission = true
	return c
}

func (p *idempotentAmbiguousProvider) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	res, err := p.withdrawCountingProvider.Withdraw(ctx, req)
	res.Outcome = OutcomeAmbiguous
	res.ProviderReference = "idem-amb-" + uuid.New().String()
	return res, err
}

func (p *idempotentAmbiguousProvider) QueryStatus(ctx context.Context, ref string) (StatusResult, error) {
	return StatusResult{ProviderReference: ref, Outcome: OutcomeAmbiguous}, nil
}

// --- H2/B3: amount/asset mismatch on QueryStatus -----------------------------

// TestPollPayoutStatus_AmountMismatch_Disputes proves H2/B3: a QueryStatus
// success with a mismatched amount (x100) never completes the withdrawal.
func TestPollPayoutStatus_AmountMismatch_Disputes(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-fix-g", "EUR")
	provider := &scriptedWithdrawProvider{
		MockProvider: inner,
		withdraws:    []WithdrawResult{{Outcome: OutcomePending, ProviderReference: "amt-mismatch-ref"}},
		statuses:     []StatusResult{{Outcome: OutcomeSucceeded, Amount: 50_000, AssetCode: "EUR"}}, // requested 500, x100
	}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-g": provider}, MultiWebhookCredentialResolver{"mock-fix-g": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-amount-mismatch")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}

	var got withdrawal.WithdrawalRequest
	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		after, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State == withdrawal.StateCompleted {
		t.Fatalf("PROBE-D regression: completed on a x100 amount mismatch (INV-IO-6)")
	}
	if got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected the withdrawal to remain submitted, got %s", got.State)
	}
	if after.State != AttemptDisputed {
		t.Fatalf("expected the attempt to be disputed (T10), got %s", after.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPollPayoutStatus_AssetMismatch_Disputes is the wrong-asset variant.
func TestPollPayoutStatus_AssetMismatch_Disputes(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-fix-h", "EUR")
	provider := &scriptedWithdrawProvider{
		MockProvider: inner,
		withdraws:    []WithdrawResult{{Outcome: OutcomePending, ProviderReference: "asset-mismatch-ref"}},
		statuses:     []StatusResult{{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "USD"}},
	}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-h": provider}, MultiWebhookCredentialResolver{"mock-fix-h": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-asset-mismatch")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}
	var got withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State == withdrawal.StateCompleted {
		t.Fatalf("completed on an asset mismatch (INV-IO-6)")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- B5: oversize provider reference through the REAL adapter-call path -----

// oversizeRefWithdrawProvider returns a definite success with an over-bound
// provider reference DIRECTLY from Withdraw - unlike
// TestPayoutDispatch_OversizeProviderReference_Parks (which hand-builds a
// GateResult and never exercises payoutAdapterCall/callProvider at all),
// this drives the oversize value through the REAL DispatchWithdraw path,
// so disabling providerref.Validate in payoutAdapterCall would be caught.
type oversizeRefWithdrawProvider struct{ *MockProvider }

func (p *oversizeRefWithdrawProvider) Withdraw(_ context.Context, _ WithdrawRequest) (WithdrawResult, error) {
	return WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: string(make([]byte, 400))}, nil
}

// TestDispatchWithdraw_OversizeReference_ParksThroughRealAdapterPath is
// B5's required end-to-end test.
func TestDispatchWithdraw_OversizeReference_ParksThroughRealAdapterPath(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-oversize", "EUR")
	provider := &oversizeRefWithdrawProvider{MockProvider: inner}
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-oversize": provider}, MultiWebhookCredentialResolver{"mock-fix-oversize": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-oversize-real")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	// The REAL phase B call - through payoutAdapterCall/callProvider, not
	// a hand-built GateResult.
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, provider, claim.Attempt)
	if gr.Class != ErrorClassProviderRefInvalid {
		t.Fatalf("expected ErrorClassProviderRefInvalid from the real adapter-call path, got %s (err=%v)", gr.Class, gr.Err)
	}
	if gr.Err == nil {
		t.Fatalf("expected a non-nil error from the gate")
	}

	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var got withdrawal.WithdrawalRequest
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected the withdrawal to remain submitted (hold never released), got %s", got.State)
	}
	if attempt.State != AttemptDisputed {
		t.Fatalf("expected the attempt to be parked as disputed via the real adapter-call path, got %s", attempt.State)
	}
	if attempt.TerminalReason == nil || !strings.HasPrefix(*attempt.TerminalReason, "invalid_provider_reference:") {
		t.Fatalf("expected a specific invalid_provider_reference:<reason> terminal_reason, got %v", attempt.TerminalReason)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- M1/B4: still-pending poll reschedules, no error ------------------------

// TestPollPayoutStatus_StillPending_Reschedules proves M1/B4: polling a
// `pending` attempt that is STILL pending is a no-op reschedule, never a
// CAS error.
func TestPollPayoutStatus_StillPending_Reschedules(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-fix-i", "EUR")
	// Default Withdraw -> Pending (with a reference), default QueryStatus
	// -> whatever the mock recorded, which is Pending too.
	registerCapability(t, pool, f.orchFixture, inner, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-i": inner}, MultiWebhookCredentialResolver{"mock-fix-i": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-still-pending")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, inner, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptPending {
		t.Fatalf("expected pending after the initial dispatch, got %s", attempt.State)
	}
	pollCountBefore := attempt.PollCount

	if err := PollPayoutStatus(context.Background(), pool, orch, MockCredentialResolver{}, f.tenantID, attempt, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("PROBE-C regression: still-pending poll errored instead of rescheduling: %v", err)
	}

	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if after.State != AttemptPending {
		t.Fatalf("expected the attempt to remain pending, got %s", after.State)
	}
	if after.PollCount <= pollCountBefore {
		t.Fatalf("expected poll_count to advance, before=%d after=%d", pollCountBefore, after.PollCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- H3/B2: crash-after-claim recovery ---------------------------------------

// TestClaimForDispatch_NextActionAtSet_CrashRecovery proves B2/H3: T1p
// commits next_action_at = lease_until unconditionally, so a "crash"
// (simulated by expiring the lease with no phase-B/C ever having run) is
// still visible to the sweeper's batch claim, which then resolves it via
// QueryStatus/merchant-reference convergence rather than being invisible
// forever.
func TestClaimForDispatch_NextActionAtSet_CrashRecovery(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	inner := NewMockProvider("mock-fix-j", "EUR")
	registerCapability(t, pool, f.orchFixture, inner, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-j": inner}, MultiWebhookCredentialResolver{"mock-fix-j": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-crash-recovery")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	var nextActionAtNull bool
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT next_action_at IS NULL FROM payment_attempts WHERE id = $1`, claim.Attempt.ID).Scan(&nextActionAtNull)
	}); err != nil {
		t.Fatalf("check next_action_at: %v", err)
	}
	if nextActionAtNull {
		t.Fatalf("PROBE-E/H3 regression: next_action_at is NULL immediately after T1p")
	}

	// Simulate a crash: phase B/C never ran; expire the lease so it's due.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET lease_until = now() - interval '1 hour', next_action_at = now() - interval '1 hour' WHERE id = $1`, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	st := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if st.Claimed == 0 {
		t.Fatalf("expected the sweeper to claim the crashed attempt, got 0")
	}
	if len(st.Errors) != 0 {
		t.Fatalf("unexpected sweep errors: %v", st.Errors)
	}

	// M7 (RV-PRH-I1 re-review: "the crash-recovery test only asserts
	// Claimed>0 and no errors, not the resulting state"): a lease-expired,
	// no-reference `submitting` attempt must specifically land on
	// `ambiguous` (T6) with ever_possibly_sent set, not merely "claimed and
	// no error" - a mutant that routed it to a plain reschedule instead
	// would still pass the assertions above.
	var recovered PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recovered, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if recovered.State != AttemptAmbiguous {
		t.Fatalf("M7 regression: expected the crashed, lease-expired attempt to land on ambiguous (T6), got %s", recovered.State)
	}
	if !recovered.EverPossiblySent {
		t.Fatalf("M7 regression: expected ever_possibly_sent to be set once T6 fires")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- CP-W4: real claim-vs-claim race (replaces the vacuous Reject test) -----

// TestConcurrentClaimForDispatch_ExactlyOneWithdraws is the ledger review's
// required CP-W4 test: N concurrent ClaimForDispatch calls for the SAME
// approved withdrawal, asserting exactly one wins the claim, exactly one
// payment_attempts row exists, and (once dispatched) exactly one Withdraw
// call happens, with the ledger balanced throughout - 50 reps.
func TestConcurrentClaimForDispatch_ExactlyOneWithdraws(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 1_000_000, true)

	const reps = 50
	for i := 0; i < reps; i++ {
		inner := NewMockProvider(fmt.Sprintf("mock-fix-cpw4-%d", i), "EUR")
		spy := &withdrawCountingProvider{MockProvider: inner}
		registerCapability(t, pool, f.orchFixture, spy, 100)
		orch := NewOrchestrator(map[string]PaymentProvider{spy.Capabilities().ProviderID: spy}, MultiWebhookCredentialResolver{spy.Capabilities().ProviderID: NewMockWebhookCredentials(inner)})

		wr := approvedWithdrawal(t, pool, f, 500, fmt.Sprintf("payout-cpw4-%d", i))

		const concurrency = 5
		results := make([]ClaimResult, concurrency)
		errs := make([]error, concurrency)
		var wg sync.WaitGroup
		for j := 0; j < concurrency; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				results[j], errs[j] = orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			}(j)
		}
		wg.Wait()

		wins := 0
		var winningAttempt PaymentAttempt
		for j := 0; j < concurrency; j++ {
			if errs[j] == nil {
				wins++
				winningAttempt = results[j].Attempt
			}
		}
		if wins != 1 {
			t.Fatalf("rep %d: expected exactly 1 winning claim, got %d", i, wins)
		}
		if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 1 {
			t.Fatalf("rep %d: expected exactly 1 payment_attempts row, got %d", i, n)
		}

		gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, spy, winningAttempt)
		if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, winningAttempt, gr, EvidenceSync); err != nil {
			t.Fatalf("rep %d: ApplyPayoutResult: %v", i, err)
		}
		if spy.count() != 1 {
			t.Fatalf("rep %d: expected exactly 1 Withdraw call, got %d", i, spy.count())
		}
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- lock-presence test (ledger review condition) ---------------------------

// TestClaimForDispatch_GateRunsUnderOuterLock pins the ledger review's
// lock ruling: the KYC gate must never be evaluated while a DIFFERENT
// transaction holds the withdrawal row's FOR UPDATE lock - it must wait
// for that lock first (A7/LF95-C10(a) ordering), never read a
// concurrently-mutating row.
func TestClaimForDispatch_GateRunsUnderOuterLock(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-fix-lock", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-lock": provider}, MultiWebhookCredentialResolver{"mock-fix-lock": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-lock-presence")

	// Hold the withdrawal row FOR UPDATE in a separate, long-running
	// transaction, releasing it only after a short delay.
	lockHeld := make(chan struct{})
	lockRelease := make(chan struct{})
	holderDone := make(chan struct{})
	var holderErr error
	go func() {
		defer close(holderDone)
		holderErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT id FROM withdrawal_requests WHERE id = $1 FOR UPDATE`, wr.ID); err != nil {
				return err
			}
			close(lockHeld)
			<-lockRelease
			return nil
		})
	}()
	<-lockHeld

	gateInvoked := make(chan time.Time, 1)
	spy := recordingPayoutKYCGate{real: KYCEnforcementPayoutGate{}, invoked: gateInvoked}

	claimDone := make(chan struct{})
	var claimErr error
	go func() {
		defer close(claimDone)
		_, claimErr = orch.ClaimForDispatch(context.Background(), pool, spy, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	}()

	select {
	case <-gateInvoked:
		// C-T3 (RV-PRH-I1 ledger re-review): release the lock holder BEFORE
		// t.Fatalf - Fatalf calls runtime.Goexit on this goroutine only, so
		// without this the holder goroutine (blocked on <-lockRelease) and
		// the claim goroutine (blocked waiting for the row lock) would both
		// hang for the rest of the process's life, and the test would only
		// fail once the whole `go test` run times out instead of failing
		// fast with a clear message.
		close(lockRelease)
		<-claimDone
		<-holderDone
		t.Fatalf("the KYC gate was invoked while a concurrent transaction still held the withdrawal row lock")
	case <-time.After(300 * time.Millisecond):
		// Expected: ClaimForDispatch is blocked waiting for the lock.
	}

	close(lockRelease)
	<-claimDone
	<-holderDone
	if holderErr != nil {
		t.Fatalf("lock holder: %v", holderErr)
	}
	if claimErr != nil {
		t.Fatalf("ClaimForDispatch: %v", claimErr)
	}
	select {
	case <-gateInvoked:
	default:
		t.Fatalf("expected the gate to have been invoked after the lock was released")
	}
}

// recordingPayoutKYCGate wraps a real PayoutKYCGate and signals invoked
// the instant EvaluatePayout is called.
type recordingPayoutKYCGate struct {
	real    PayoutKYCGate
	invoked chan time.Time
}

func (g recordingPayoutKYCGate) EvaluatePayout(ctx context.Context, tx pgx.Tx, params kyc.EnforcementParams) (kyc.EnforcementDecision, error) {
	select {
	case g.invoked <- time.Now():
	default:
	}
	return g.real.EvaluatePayout(ctx, tx, params)
}

// --- kill switch (migration 0105) fail-closed handling -----------------------

// killSwitchStaffPrincipal creates a real, active tenant_admin staff_users
// row - EngageKillSwitch's own session trigger (payment_kill_switch_session)
// requires a genuine tenant-scoped staff principal, not an arbitrary uuid.
func killSwitchStaffPrincipal(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'tenant_admin', 'active')`,
			id, tenantID, "ks-staff-"+id.String()+"@test.example")
		return err
	})
	if err != nil {
		t.Fatalf("create staff principal: %v", err)
	}
	return id
}

func engageKillSwitchForTest(t *testing.T, pool *db.Pool, f payoutFixture, providerID string) {
	t.Helper()
	principal := killSwitchStaffPrincipal(t, pool, f.tenantID)
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := EngageKillSwitch(ctx, tx, f.tenantID, providerID, KillSwitchOperationPayout, "fix-round-test")
		return err
	}); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}
}

// TestClaimForDispatch_KillSwitchEngaged_FailsClosedNoWithdraw proves the
// T1p kill-switch fail-closed path: no Withdraw call, the request left
// `approved`, no attempt row, and a clean retry once released.
func TestClaimForDispatch_KillSwitchEngaged_FailsClosedNoWithdraw(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-ks-t1p", "EUR")
	spy := &withdrawCountingProvider{MockProvider: inner}
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-ks-t1p": spy}, MultiWebhookCredentialResolver{"mock-fix-ks-t1p": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-fix-ks-t1p")
	engageKillSwitchForTest(t, pool, f, "mock-fix-ks-t1p")

	_, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if !errors.Is(err, ErrPayoutKillSwitchEngaged) {
		t.Fatalf("expected ErrPayoutKillSwitchEngaged, got %v", err)
	}
	if spy.count() != 0 {
		t.Fatalf("provider.Withdraw was called while the kill switch was engaged")
	}
	var got withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateApproved {
		t.Fatalf("expected the request to remain approved, got %s", got.State)
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
		t.Fatalf("expected 0 payment_attempts rows while blocked, got %d", n)
	}

	// Release (four-eyes: request and approve from two DISTINCT staff
	// principals, in two separate transactions) and retry - must succeed
	// cleanly.
	requester := killSwitchStaffPrincipal(t, pool, f.tenantID)
	approver := killSwitchStaffPrincipal(t, pool, f.tenantID)
	var ksID uuid.UUID
	var reqID uuid.UUID
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		ks, _, err := GetKillSwitch(ctx, tx, f.tenantID, "mock-fix-ks-t1p", KillSwitchOperationPayout)
		if err != nil {
			return err
		}
		ksID = ks.ID
		req, err := RequestKillSwitchRelease(ctx, tx, f.tenantID, ks.ID, ks.Version, "fix-round-test-release", time.Hour)
		if err != nil {
			return err
		}
		reqID = req.ID
		return nil
	}); err != nil {
		t.Fatalf("request kill switch release: %v", err)
	}
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApproveAndReleaseKillSwitch(ctx, tx, f.tenantID, ksID, reqID)
		return err
	}); err != nil {
		t.Fatalf("release kill switch: %v", err)
	}

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch after release: %v", err)
	}
	if claim.Denied {
		t.Fatalf("expected allow after release")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestSweeper_T2Reclaim_KillSwitchEngaged_ReschedulesNeverEscalates proves
// the T2 kill-switch path is a plain reschedule, never a permanent
// Escalate - distinct from a KYC deny.
func TestSweeper_T2Reclaim_KillSwitchEngaged_ReschedulesNeverEscalates(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-fix-ks-t2", "EUR")
	spy := &withdrawCountingProvider{MockProvider: inner}
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-fix-ks-t2": spy}, MultiWebhookCredentialResolver{"mock-fix-ks-t2": NewMockWebhookCredentials(inner)})

	_, attempt := notSentPayoutAttempt(t, pool, orch, f, 500, "payout-fix-ks-t2")
	engageKillSwitchForTest(t, pool, f, "mock-fix-ks-t2")

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("reclaimPayoutCreated: %v", err)
	}
	if spy.count() != 0 {
		t.Fatalf("provider.Withdraw was called while the kill switch was engaged")
	}
	var after PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = GetAttemptByID(ctx, tx, attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if after.State != AttemptCreated {
		t.Fatalf("expected the attempt to remain created, got %s", after.State)
	}
	if after.EscalatedAt != nil {
		t.Fatalf("expected NO escalation for a transient kill-switch block, got escalated_at=%v", after.EscalatedAt)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
