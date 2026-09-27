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

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer")
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
}
