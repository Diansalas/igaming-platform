//go:build integration

// PRH-I1 payout dispatch tests (payout.go). Runs on a PRIVATE scratch
// database migrated through 0101 (payment_attempts) - the shared
// TEST_DATABASE_URL is not guaranteed to have 0101 applied in every
// checkout, exactly like deposit_v2_integration_test.go's identical
// rationale.
package payments

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// payoutFixture funds a wallet and (optionally) records a passed KYC
// verification, mirroring internal/withdrawal's own seedFixtureRaw
// exactly (per-package fixture duplication is this repo's convention).
type payoutFixture struct {
	orchFixture
	personID uuid.UUID
}

func seedPayoutFixture(t *testing.T, pool *db.Pool, initialBalance int64, verified bool) payoutFixture {
	t.Helper()
	f := seedOrchFixture(t, pool)
	pf := payoutFixture{orchFixture: f}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerAccountID).Scan(&pf.personID); err != nil {
			return err
		}
		if verified {
			if _, err := tx.Exec(ctx,
				`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
				 VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`,
				uuid.New(), f.tenantID, f.brandID, f.playerAccountID, pf.personID); err != nil {
				return err
			}
		}
		if initialBalance > 0 {
			cashID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
			if err != nil {
				return err
			}
			clearingID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
			if err != nil {
				return err
			}
			provider := "seed"
			providerTx := "seed-deposit-" + f.walletID.String()
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxDeposit,
				IdempotencyKey: providerTx, ProviderID: &provider, ProviderTxID: &providerTx,
				CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: clearingID, Direction: ledger.Debit, Amount: initialBalance},
					{LedgerAccountID: cashID, Direction: ledger.Credit, Amount: initialBalance},
				},
			})
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed payout fixture: %v", err)
	}
	return pf
}

// mustSetWithdrawalPolicy mirrors internal/withdrawal's own identical test
// helper: a high tenant-wide threshold so approvedWithdrawal's single
// automated approval suffices (defaultApprovalPolicy fails closed at
// threshold=0, requiring full four-eyes for ANY positive amount otherwise).
func mustSetWithdrawalPolicy(t *testing.T, pool *db.Pool, tenantID uuid.UUID, assetCode string, thresholdAmount int64, requiredApprovals int) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, $2, $3, $4, now() - interval '1 hour')`,
			tenantID, assetCode, thresholdAmount, requiredApprovals,
		)
		return err
	})
	if err != nil {
		t.Fatalf("set withdrawal policy: %v", err)
	}
}

// revokeVerification inserts a `rejected` kyc_verifications row that
// becomes the player's latest (most recent) verification - used to
// simulate a payout-dispatch-time KYC revocation after a request already
// passed pending_review/approval with a PASSED verification.
func revokeVerification(t *testing.T, pool *db.Pool, f payoutFixture) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, 'rejected', 'mock')`,
			uuid.New(), f.tenantID, f.brandID, f.playerAccountID, f.personID)
		return err
	})
	if err != nil {
		t.Fatalf("revoke verification: %v", err)
	}
}

// approvedWithdrawal drives a fresh WithdrawalRequest to `approved` state
// via a single automated (below-threshold) approval, mirroring
// internal/withdrawal's own approvedRequest test helper.
func approvedWithdrawal(t *testing.T, pool *db.Pool, f payoutFixture, amount int64, idemKey string) withdrawal.WithdrawalRequest {
	t.Helper()
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2)
	var wr withdrawal.WithdrawalRequest
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID,
			WalletID: f.walletID, AssetCode: "EUR", Amount: amount, IdempotencyKey: idemKey,
		})
		return err
	})
	if err != nil {
		t.Fatalf("request withdrawal: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.MoveToPendingReview(ctx, tx, wr.ID)
	})
	if err != nil {
		t.Fatalf("move to pending review: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := withdrawal.Approve(ctx, tx, wr.ID, uuid.New(), true, nil, nil)
		if err != nil {
			return err
		}
		if !approved {
			return fmt.Errorf("expected a single automated approval to approve the request")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	})
	if err != nil {
		t.Fatalf("reread approved request: %v", err)
	}
	return wr
}

// testSubmitActor is a fresh, valid-shaped SubmitActor for tests that don't
// care about the specific staff identity recorded (B6's own tests assert
// on the actor explicitly where it matters).
func testSubmitActor() SubmitActor {
	return SubmitActor{StaffID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "test-agent", RequestID: uuid.New().String()}
}

func countAttempts(t *testing.T, pool *db.Pool, tenantID uuid.UUID, withdrawalRequestID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, withdrawalRequestID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count payment_attempts: %v", err)
	}
	return n
}

// TestClaimForDispatch_KYCDeny_NoAttemptRowHoldReleased proves item (1)/(8):
// a payout KYC deny at T1p commits withdrawal.DenyForCompliance in the SAME
// transaction, with ZERO payment_attempts rows ever inserted and the hold
// released back to player_cash.
func TestClaimForDispatch_KYCDeny_NoAttemptRowHoldReleased(t *testing.T) {
	pool := depositV2ScratchPool(t)
	// Seeded WITH a passed verification so RequestWithdrawal/MoveToPendingReview/
	// Approve (which also gate on EnforcementWithdrawalHold) succeed - then
	// revoked before dispatch, so ONLY the payout-dispatch-time gate
	// (EnforcementWithdrawalPayout) denies, exactly like a real "approved,
	// then KYC lapses before the payout is sent" scenario.
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-a", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-a": provider}, MultiWebhookCredentialResolver{"mock-payout-a": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-kyc-deny")
	revokeVerification(t, pool, f)

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if !claim.Denied {
		t.Fatalf("expected the payout KYC gate to deny (no passed verification)")
	}
	if claim.Request.State != withdrawal.StateRejected {
		t.Fatalf("expected state rejected, got %s", claim.Request.State)
	}
	if claim.Request.ReleaseLedgerTransactionID == nil {
		t.Fatalf("expected the hold to be released")
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
		t.Fatalf("expected 0 payment_attempts rows after a KYC deny, got %d", n)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestClaimForDispatch_Allow_CommitsBeforeAnyProviderCall proves item (1):
// ClaimForDispatch commits approved->submitted plus a submitting attempt
// row with NO Withdraw call ever made (the mock provider is never invoked
// by ClaimForDispatch itself).
func TestClaimForDispatch_Allow_CommitsBeforeAnyProviderCall(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-b", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-b": provider}, MultiWebhookCredentialResolver{"mock-payout-b": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-allow-claim")

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if claim.Denied {
		t.Fatalf("expected allow, got denied")
	}
	if claim.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("expected state submitted, got %s", claim.Request.State)
	}
	// P95-C2: submitted with NO provider reference yet - the provider was
	// never called by ClaimForDispatch.
	if claim.Request.ProviderReference != nil {
		t.Fatalf("expected no provider reference yet (P95-C2), got %v", *claim.Request.ProviderReference)
	}
	if claim.Attempt.State != AttemptSubmitting {
		t.Fatalf("expected attempt state submitting, got %s", claim.Attempt.State)
	}
	if claim.Attempt.EverPossiblySent {
		t.Fatalf("expected ever_possibly_sent=false before any provider call")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_EndToEnd_Success drives the full T1p/B/C loop against
// the mock adapter's default Withdraw outcome (Pending - the mock has no
// dedicated synchronous-success amount for Withdraw) and asserts phase C
// attaches the provider reference and moves the attempt to pending,
// exactly matching P95-C2 (submitted may lack a reference until this
// point, then gains one without any premature ledger release).
func TestPayoutDispatch_EndToEnd_Success(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-c", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-c": provider}, MultiWebhookCredentialResolver{"mock-payout-c": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-e2e-success")

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	adapter, _ := orch.Provider(claim.Capability.ProviderID)
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, adapter, claim.Attempt)
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
		t.Fatalf("expected submitted (mock outcome=%v), got %s", gr.Value.Outcome, got.State)
	}
	if got.ProviderReference == nil || *got.ProviderReference == "" {
		t.Fatalf("expected phase C to attach the provider reference once accepted")
	}
	if attempt.State != AttemptPending {
		t.Fatalf("expected attempt state pending, got %s", attempt.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_OversizeProviderReference_Parks proves item (3):
// an over-bound provider reference PARKS the attempt (disputed) rather
// than rolling back to a retryable/resendable state, and never releases
// the hold.
func TestPayoutDispatch_OversizeProviderReference_Parks(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-d", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-d": provider}, MultiWebhookCredentialResolver{"mock-payout-d": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-oversize-ref")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	// Synthesize a GateResult as if the adapter returned an over-bound
	// reference (never actually constructible via a real adapter call in
	// this mock, so exercised directly at ApplyPayoutResult's boundary -
	// the exact shape payoutAdapterCall itself would have produced).
	over := make([]byte, providerref.MaxBytes+1)
	for i := range over {
		over[i] = 'a'
	}
	gr := GateResult[WithdrawResult]{
		Class: ErrorClassProviderRefInvalid,
		Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: string(over)},
		Err:   providerref.Validate("withdraw.provider_reference", string(over)),
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
		t.Fatalf("expected the attempt to be parked as disputed, got %s", attempt.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_NoDefiniteDeclineNeverFails proves item (4): an
// ambiguous/timeout outcome never transitions the withdrawal to `failed`.
func TestPayoutDispatch_NoDefiniteDeclineNeverFails(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-e", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-e": provider}, MultiWebhookCredentialResolver{"mock-payout-e": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-ambiguous")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := GateResult[WithdrawResult]{Class: ErrorClassAmbiguous, Value: WithdrawResult{}}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var got withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected the withdrawal to remain submitted on an ambiguous outcome, got %s", got.State)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutDispatch_NotSent_ReclaimNeverDoubleSends proves items (1)/(4)/
// double-payout impossibility: a NotSent phase-C outcome returns the
// attempt to `created` (re-claimable), and a subsequent T2 re-claim via
// ClaimCreatedForSubmission is the ONLY path back to `submitting` - never a
// second InsertSubmittingAttempt / second withdrawal-level T1p.
func TestPayoutDispatch_NotSent_ReclaimNeverDoubleSends(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-f", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-f": provider}, MultiWebhookCredentialResolver{"mock-payout-f": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-notsent")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	gr := GateResult[WithdrawResult]{Class: ErrorClassNotSent}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	if attempt.State != AttemptCreated {
		t.Fatalf("expected NotSent to return the attempt to created, got %s", attempt.State)
	}
	if attempt.EverPossiblySent {
		t.Fatalf("expected ever_possibly_sent still false after NotSent")
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 1 {
		t.Fatalf("expected exactly 1 payment_attempts row (no duplicate insert), got %d", n)
	}

	// Concurrent re-claims: only one may win T2.
	const attempts = 5
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return ClaimCreatedForSubmission(ctx, tx, attempt.ID, claim.Capability.ProviderID, uuid.New(), "sweeper", time.Now().Add(time.Minute))
			})
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, e := range results {
		if e == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 concurrent T2 re-claim to win, got %d", wins)
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 1 {
		t.Fatalf("expected still exactly 1 payment_attempts row after re-claim race, got %d", n)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestPayoutEnforcementParams_UsesWithdrawalPayoutOperation is a raw guard
// (item 9's spirit applied to the gate wiring itself): the payout gate
// MUST evaluate kyc.EnforcementWithdrawalPayout, never
// EnforcementWithdrawalHold - a mutant swapping the two would silently
// re-run the pending_review-time gate instead of the dispatch-time one.
func TestPayoutEnforcementParams_UsesWithdrawalPayoutOperation(t *testing.T) {
	wr := withdrawal.WithdrawalRequest{TenantID: uuid.New(), BrandID: uuid.New(), PlayerAccountID: uuid.New(), AssetCode: "EUR", Amount: 100}
	params := payoutEnforcementParams(wr, uuid.New())
	if params.Operation != kyc.EnforcementWithdrawalPayout {
		t.Fatalf("expected EnforcementWithdrawalPayout, got %s", params.Operation)
	}
}

// TestSweeper_T2Reclaim_KYCDeny_EscalatesNeverResends proves item (5): a
// NotSent payout attempt (returned to `created`) whose KYC re-check at T2
// denies is escalated (T16, no state change) - never resubmitted, and the
// withdrawal hold is never released (it stays `submitted`, not `rejected`
// - DenyForCompliance is illegal here since the request already left
// `approved`).
func TestSweeper_T2Reclaim_KYCDeny_EscalatesNeverResends(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-g", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-g": provider}, MultiWebhookCredentialResolver{"mock-payout-g": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-t2-deny")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	// NotSent -> submitting -> created.
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassNotSent}, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (NotSent): %v", err)
	}
	// Revoke KYC before the sweeper's T2 re-claim.
	revokeVerification(t, pool, f)

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
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
		after, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected the withdrawal to remain submitted (no hold release from a re-claim deny), got %s", got.State)
	}
	if after.State != AttemptCreated {
		t.Fatalf("expected the attempt to remain created (no resend), got %s", after.State)
	}
	if after.EscalatedAt == nil {
		t.Fatalf("expected the attempt to be escalated")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestSweeper_T2Reclaim_Allow_ResendsAndSucceeds proves the allow path of
// item (5): a NotSent attempt is re-claimed and driven through phase B/C
// again with a fresh claim token, reaching pending via the mock adapter's
// default Withdraw outcome.
func TestSweeper_T2Reclaim_Allow_ResendsAndSucceeds(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-h", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-h": provider}, MultiWebhookCredentialResolver{"mock-payout-h": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-t2-allow")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassNotSent}, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (NotSent): %v", err)
	}

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	if err := sweeper.reclaimPayoutCreated(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("reclaimPayoutCreated: %v", err)
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
		t.Fatalf("expected the re-claimed attempt to reach pending, got %s", after.State)
	}
	if after.SubmitCount < 2 {
		t.Fatalf("expected submit_count to reflect the resend, got %d", after.SubmitCount)
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 1 {
		t.Fatalf("expected exactly 1 payment_attempts row (re-claim, not a new insert), got %d", n)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// notSentPayoutAttempt drives a fresh approved WithdrawalRequest through
// ClaimForDispatch and a synthetic NotSent phase-C outcome, returning the
// resulting `created`, re-claimable attempt - the shared setup every
// sweeper-driven re-claim test in this file needs.
func notSentPayoutAttempt(t *testing.T, pool *db.Pool, orch *Orchestrator, f payoutFixture, amount int64, idemKey string) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	t.Helper()
	wr := approvedWithdrawal(t, pool, f, amount, idemKey)
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassNotSent}, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (NotSent): %v", err)
	}
	// MarkNotSent schedules next_action_at ~30s out (ApplyPayoutResult's
	// own payoutNextPollInterval); Touch (T17) pulls it back to now() so
	// the sweeper's claimBatch (next_action_at <= now()) picks it up
	// immediately, without a real-time sleep in the test.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Touch(ctx, tx, claim.Attempt.ID)
	}); err != nil {
		t.Fatalf("touch attempt: %v", err)
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	return wr, attempt
}

// TestSweeper_RunOnce_PayoutEndToEnd_BatchCapAndLease proves item (b): a
// full Sweeper.RunOnce pass claims a leased batch of due `created` payout
// attempts (respecting BatchPerTenant), drives each through phase B/C, and
// leaves everything else untouched - including the lease_owner evidence
// that only the claimed rows were ever touched by the batch claim/re-claim
// machinery.
func TestSweeper_RunOnce_PayoutEndToEnd_BatchCapAndLease(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-payout-i", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-i": provider}, MultiWebhookCredentialResolver{"mock-payout-i": NewMockWebhookCredentials(provider)})

	const total = 3
	var attempts [total]PaymentAttempt
	for i := 0; i < total; i++ {
		_, a := notSentPayoutAttempt(t, pool, orch, f, 500, fmt.Sprintf("payout-runonce-%d", i))
		attempts[i] = a
	}

	sweeper := &Sweeper{
		Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{},
		BatchPerTenant: 2, // per-tenant cap: strictly less than `total`.
	}
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("unexpected sweep errors: %v", stats.Errors)
	}
	if stats.Claimed != 2 {
		t.Fatalf("expected the per-tenant batch cap (2) to bound the claim, got %d claimed", stats.Claimed)
	}
	if stats.Processed != 2 {
		t.Fatalf("expected 2 processed, got %d", stats.Processed)
	}

	var createdCount, pendingCount int
	for _, a0 := range attempts {
		var cur PaymentAttempt
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			cur, err = GetAttemptByID(ctx, tx, a0.ID)
			return err
		}); err != nil {
			t.Fatalf("reread: %v", err)
		}
		switch cur.State {
		case AttemptCreated:
			createdCount++
			// Untouched by the sweeper: still carries the ORIGINAL T1p
			// lease_owner, never the batch-claim/re-claim one - direct
			// evidence this row was left exactly as leased (never
			// selected into the capped batch).
			if cur.LeaseOwner == nil || *cur.LeaseOwner != "payout-dispatch" {
				t.Fatalf("expected the untouched attempt to keep its original lease_owner, got %v", cur.LeaseOwner)
			}
		case AttemptPending:
			pendingCount++
			// Lease behaviour: the sweeper's own T2 re-claim
			// (reclaimPayoutCreated -> ClaimCreatedForSubmission) rewrote
			// lease_owner/lease_until as part of driving this row through
			// phase B/C.
			if cur.LeaseOwner == nil || *cur.LeaseOwner != "sweeper-payout-reclaim" {
				t.Fatalf("expected a claimed attempt's lease_owner to reflect the sweeper's re-claim, got %v", cur.LeaseOwner)
			}
			if cur.LeaseUntil == nil {
				t.Fatalf("expected a claimed attempt to carry a lease_until")
			}
		default:
			t.Fatalf("unexpected attempt state %s", cur.State)
		}
	}
	if createdCount != total-2 || pendingCount != 2 {
		t.Fatalf("expected %d untouched + 2 processed, got %d untouched + %d processed", total-2, createdCount, pendingCount)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestSweeper_T12Resubmit_KYCDeny_EscalatesNeverResends proves item (d): the
// T12 (ambiguous -> submitting resubmission) path re-runs the SAME shared
// gate T2 uses, and a DENY escalates with no resend and no hold release -
// the gate had previously only been exercised via T2; this is the
// dedicated T12 case.
func TestSweeper_T12Resubmit_KYCDeny_EscalatesNeverResends(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	provider := NewMockProvider("mock-payout-j", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-j": provider}, MultiWebhookCredentialResolver{"mock-payout-j": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-t12-deny")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	// Ambiguous -> submitting -> ambiguous (T6).
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous}, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult (Ambiguous): %v", err)
	}
	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread attempt: %v", err)
	}
	if attempt.State != AttemptAmbiguous {
		t.Fatalf("expected the attempt to be ambiguous before T12, got %s", attempt.State)
	}
	submitCountBefore := attempt.SubmitCount

	// Revoke KYC before the sweeper's T12 resubmission.
	revokeVerification(t, pool, f)

	sweeper := &Sweeper{Pool: pool, Orchestrator: orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}}
	if err := sweeper.resubmitPayoutAmbiguous(context.Background(), f.tenantID, attempt); err != nil {
		t.Fatalf("resubmitPayoutAmbiguous: %v", err)
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
	if got.State != withdrawal.StateSubmitted {
		t.Fatalf("expected the withdrawal to remain submitted (no hold release from a T12 deny), got %s", got.State)
	}
	if after.State != AttemptAmbiguous {
		t.Fatalf("expected the attempt to remain ambiguous (no resend), got %s", after.State)
	}
	if after.SubmitCount != submitCountBefore {
		t.Fatalf("expected submit_count unchanged (no resend), before=%d after=%d", submitCountBefore, after.SubmitCount)
	}
	if after.EscalatedAt == nil {
		t.Fatalf("expected the attempt to be escalated")
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 1 {
		t.Fatalf("expected exactly 1 payment_attempts row, got %d", n)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// withdrawCountingProvider wraps a *MockProvider and records how many times
// Withdraw was actually invoked - the crash-point-proxy assertion item (e)
// needs: ClaimForDispatch must NEVER call it, on any path (a pre-commit
// failure, a KYC-gate error, OR its own successful commit) - Withdraw is
// only ever reachable through a caller's SEPARATE, explicit
// DispatchWithdraw call after ClaimForDispatch has already returned.
type withdrawCountingProvider struct {
	*MockProvider
	mu    sync.Mutex
	calls int
}

func (p *withdrawCountingProvider) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.MockProvider.Withdraw(ctx, req)
}

func (p *withdrawCountingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// erroringPayoutKYCGate is a PayoutKYCGate stand-in that always fails
// evaluation itself (a DB/vendor-style outage), as opposed to a business
// DENY - the crash-point-proxy's second, gate-driven failure case.
type erroringPayoutKYCGate struct{}

func (erroringPayoutKYCGate) EvaluatePayout(context.Context, pgx.Tx, kyc.EnforcementParams) (kyc.EnforcementDecision, error) {
	return kyc.EnforcementDecision{}, fmt.Errorf("erroringPayoutKYCGate: simulated evaluation failure")
}

// commitVisibilitySpyProvider is TestClaimForDispatch_NeverCallsProviderBeforeCommit's
// falsifiable check (RV-PRH-I1 re-review: "ClaimForDispatch has no code
// path to Withdraw at all, so the old version of this test could not
// fail - it proved nothing about ordering"). At the moment Withdraw is
// called, it re-reads the attempt through the SAME pool but a brand-new
// connection/transaction - if ClaimForDispatch's own transaction had not
// ACTUALLY committed yet (e.g. a regression that called Withdraw from
// inside T1p's own transaction, or before it committed), this read would
// not see the row (MVCC visibility across sessions), not merely "the
// in-memory struct Go already happened to have".
type commitVisibilitySpyProvider struct {
	*MockProvider
	pool      *db.Pool
	tenantID  uuid.UUID
	attemptID uuid.UUID
	sawRow    bool
	readErr   error
	called    bool
}

func (p *commitVisibilitySpyProvider) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	p.called = true
	p.readErr = p.pool.WithTenant(ctx, p.tenantID, func(actx context.Context, tx pgx.Tx) error {
		_, err := GetAttemptByID(actx, tx, p.attemptID)
		return err
	})
	p.sawRow = p.readErr == nil
	return p.MockProvider.Withdraw(ctx, req)
}

// TestClaimForDispatch_NeverCallsProviderBeforeCommit proves item (e): no
// step ClaimForDispatch performs before its own commit - a cancelled
// context, or a hard KYC-gate evaluation error - ever reaches
// provider.Withdraw, and neither does ClaimForDispatch's own successful
// commit path (Withdraw is only ever called by a caller's later, separate
// DispatchWithdraw call). The successful-commit case is made falsifiable
// (RV-PRH-I1 re-review) via commitVisibilitySpyProvider: a mutant that
// called Withdraw from inside ClaimForDispatch's own still-open
// transaction, or before that transaction committed, would make the
// spy's separate-connection read fail to find the row - this is a real
// assertion about ORDERING, not just about which Go function called
// Withdraw.
func TestClaimForDispatch_NeverCallsProviderBeforeCommit(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	inner := NewMockProvider("mock-payout-k", "EUR")
	spy := &withdrawCountingProvider{MockProvider: inner}
	registerCapability(t, pool, f.orchFixture, spy, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-k": spy}, MultiWebhookCredentialResolver{"mock-payout-k": NewMockWebhookCredentials(inner)})

	wr := approvedWithdrawal(t, pool, f, 500, "payout-crashpoint")

	// Crash point 1: a context already cancelled before ClaimForDispatch
	// does any work at all.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := orch.ClaimForDispatch(cctx, pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err == nil {
		t.Fatalf("expected the cancelled-context claim to fail")
	}
	if spy.count() != 0 {
		t.Fatalf("provider.Withdraw was called on a pre-commit (cancelled-context) failure")
	}

	// Crash point 2: the payout KYC gate itself fails (not a business
	// DENY) - ClaimForDispatch must return the error before ever reaching
	// MarkSubmittedPending/InsertSubmittingAttempt.
	if _, err := orch.ClaimForDispatch(context.Background(), pool, erroringPayoutKYCGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); err == nil {
		t.Fatalf("expected the gate-evaluation-error claim to fail")
	}
	if spy.count() != 0 {
		t.Fatalf("provider.Withdraw was called on a pre-commit (gate error) failure")
	}

	// Both pre-commit failures above must have left the request untouched.
	var stillApproved withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stillApproved, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stillApproved.State != withdrawal.StateApproved {
		t.Fatalf("expected the request to remain approved after two pre-commit failures, got %s", stillApproved.State)
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
		t.Fatalf("expected 0 payment_attempts rows after two pre-commit failures, got %d", n)
	}

	// Crash point 3 (the successful commit path itself): ClaimForDispatch's
	// OWN commit must not call Withdraw either - only a caller's later,
	// separate DispatchWithdraw call may.
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	if claim.Denied {
		t.Fatalf("expected allow")
	}
	if spy.count() != 0 {
		t.Fatalf("provider.Withdraw was called by ClaimForDispatch's own successful commit")
	}

	// Falsifiable check (RV-PRH-I1 re-review): drive the ONLY legitimate
	// path to Withdraw (a caller's own, separate DispatchWithdraw call,
	// exactly like the real submit handler) through a spy that proves the
	// T1p row is visible from a DIFFERENT connection/transaction at the
	// moment Withdraw is invoked - i.e. the claim's commit had genuinely
	// already happened, not merely that Go had already returned from the
	// function call.
	visSpy := &commitVisibilitySpyProvider{MockProvider: inner, pool: pool, tenantID: f.tenantID, attemptID: claim.Attempt.ID}
	gr := DispatchWithdraw(context.Background(), MockCredentialResolver{}, visSpy, claim.Attempt)
	if !visSpy.called {
		t.Fatalf("test setup: the spy's Withdraw was never invoked")
	}
	if visSpy.readErr != nil {
		t.Fatalf("the attempt row committed by ClaimForDispatch was not visible from a separate connection at the moment Withdraw was called: %v", visSpy.readErr)
	}
	if !visSpy.sawRow {
		t.Fatalf("expected the committed attempt row to be visible from a separate connection before any Withdraw call")
	}
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestConcurrent_RejectVsClaimForDispatch_ExactlyOneWins was deleted
// (RV-PRH-I1 re-review, "delete the vacuous Reject-vs-claim test, or
// rename it and give it a real purpose"): withdrawal.Reject is illegal
// from `approved` regardless of timing (its own precondition requires
// `pending_review`), so this test would pass identically with NO locking
// at all - it exercised a business-rule precondition, not a race. The real
// claim-vs-claim race (two concurrent ClaimForDispatch calls on the SAME
// approved request, CP-W4) is covered by
// TestConcurrentClaimForDispatch_ExactlyOneWithdraws below, and the real
// compliance-vs-claim race is covered by
// TestConcurrent_DenyForComplianceVsClaimForDispatch_ExactlyOneWins, which
// remains.

// TestConcurrent_DenyForComplianceVsClaimForDispatch_ExactlyOneWins proves
// item (c)'s second pairing: an EXTERNAL, direct withdrawal.DenyForCompliance
// call (bypassing ClaimForDispatch's own internal gate re-check entirely -
// simulating a second, independent compliance-driven caller) racing
// ClaimForDispatch's own internal claim+gate sequence on the SAME approved
// request. The L1 row lock (withdrawal.LockApprovedForSubmission /
// lockRequestForUpdate) must serialize the two so that EXACTLY ONE commits
// (submitted+attempt, XOR rejected+hold-released) every rep, never both,
// with the ledger staying balanced throughout.
func TestConcurrent_DenyForComplianceVsClaimForDispatch_ExactlyOneWins(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 1_000_000, true)
	provider := NewMockProvider("mock-payout-m", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-payout-m": provider}, MultiWebhookCredentialResolver{"mock-payout-m": NewMockWebhookCredentials(provider)})

	const reps = 50
	var claimWins, denyWins int
	for i := 0; i < reps; i++ {
		wr := approvedWithdrawal(t, pool, f, 500, fmt.Sprintf("payout-race-deny-%d", i))

		var wg sync.WaitGroup
		var claimErr, denyErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, claimErr = orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
		}()
		go func() {
			defer wg.Done()
			denyErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				decision := kyc.EnforcementDecision{Outcome: kyc.OutcomeFailed, Allowed: false, Code: "kyc_withdrawal_payout:failed", PolicyVersion: kyc.PolicyVersion}
				kycParams := kyc.EnforcementParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID,
					Operation: kyc.EnforcementWithdrawalPayout, AssetCode: "EUR", Amount: 500, CorrelationID: uuid.New(),
				}
				_, err := withdrawal.DenyForCompliance(ctx, tx, wr.ID, decision, kycParams)
				return err
			})
		}()
		wg.Wait()

		claimed := claimErr == nil
		denied := denyErr == nil
		if claimed == denied {
			t.Fatalf("rep %d: expected exactly one of {ClaimForDispatch, DenyForCompliance} to win, got claimed=%v (err=%v) denied=%v (err=%v)",
				i, claimed, claimErr, denied, denyErr)
		}
		if claimed {
			claimWins++
		} else {
			denyWins++
		}

		var got withdrawal.WithdrawalRequest
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = withdrawal.GetByID(ctx, tx, wr.ID)
			return err
		}); err != nil {
			t.Fatalf("rep %d: reread: %v", i, err)
		}
		if claimed && got.State != withdrawal.StateSubmitted {
			t.Fatalf("rep %d: ClaimForDispatch won but state is %s, expected submitted", i, got.State)
		}
		if denied && got.State != withdrawal.StateRejected {
			t.Fatalf("rep %d: DenyForCompliance won but state is %s, expected rejected", i, got.State)
		}
	}
	if claimWins == 0 || denyWins == 0 {
		t.Logf("race distribution across %d reps: claim=%d deny=%d (both winning at least once would be stronger evidence of a genuine race, but is not required for correctness)", reps, claimWins, denyWins)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
