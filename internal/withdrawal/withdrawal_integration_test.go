//go:build integration

package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixture is a tenant/brand/person/player_account/wallet chain, following
// the exact seeding pattern internal/ledger's own integration tests use
// (per-package fixture duplication is the repo convention, not shared
// test helper code across packages).
type fixture struct {
	tenantID        uuid.UUID
	brandID         uuid.UUID
	personID        uuid.UUID
	playerAccountID uuid.UUID
	walletID        uuid.UUID
	cashAccountID   uuid.UUID
	holdAccountID   uuid.UUID
	// instrumentID is the verified MOCK payout instrument every fixture binds (B13-B: since migration
	// 0126 a withdrawal cannot exist without one, in MOCK as well).
	instrumentID uuid.UUID
}

// seedFixture creates the fixture and, if initialBalance > 0, deposits
// that amount into player_cash first (a plain Flow 1 deposit posted
// directly via internal/ledger) so withdrawal tests have real funds to
// work against.
func seedFixture(t *testing.T, pool *db.Pool, initialBalance int64) fixture {
	t.Helper()
	return seedFixtureRaw(t, pool, initialBalance, true)
}

// seedFixtureRaw is seedFixture's implementation, parameterized on
// whether to seed a default APPROVED verification. withVerification=false
// is used by kyc_gate_integration_test.go's own tests, which need to
// control the verification state themselves rather than inherit the
// default "everyone passes KYC" fixture.
func seedFixtureRaw(t *testing.T, pool *db.Pool, initialBalance int64, withVerification bool) fixture {
	t.Helper()
	f := fixture{
		tenantID:        uuid.New(),
		brandID:         uuid.New(),
		playerAccountID: uuid.New(),
	}
	personID := uuid.New()
	f.personID = personID

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed platform-level rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, f.tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		// ADR 0096 §3.2 point 1 (PRH-I3): every withdrawal now requires the
		// player's current, latest verification to be `passed` and
		// unexpired. This fixture seeds one `approved`, non-expiring
		// verification (unless withVerification is false) so every OTHER
		// pre-existing withdrawal test in this package - none of which are
		// testing KYC - continues to exercise its own behavior rather than
		// universally hitting the new KYC gate. Tests that specifically
		// exercise the KYC gate use seedFixtureRaw(..., false) instead and
		// seed their own verification state (see
		// kyc_gate_integration_test.go).
		if withVerification {
			if _, err := tx.Exec(ctx,
				`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
				 VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`,
				uuid.New(), f.tenantID, f.brandID, f.playerAccountID, personID); err != nil {
				return err
			}
		}
		f.walletID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			f.walletID, f.tenantID, f.brandID, f.playerAccountID); err != nil {
			return err
		}

		var err error
		f.cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		f.holdAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		if err != nil {
			return err
		}
		clearingID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}

		if initialBalance > 0 {
			provider := "mockpsp"
			providerTx := "seed-deposit-" + f.walletID.String()
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID:        f.tenantID,
				TransactionType: ledger.TxDeposit,
				IdempotencyKey:  providerTx,
				ProviderID:      &provider,
				ProviderTxID:    &providerTx,
				CorrelationID:   uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: clearingID, Direction: ledger.Debit, Amount: initialBalance},
					{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: initialBalance},
				},
			})
		}
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant-scoped fixture: %v", err)
	}
	f.instrumentID = pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "EUR")
	return f
}

func runTx(pool *db.Pool, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pool.WithTenant(context.Background(), tenantID, fn)
}

func mustRunTx(t *testing.T, pool *db.Pool, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := runTx(pool, tenantID, fn); err != nil {
		t.Fatalf("tx failed: %v", err)
	}
}

// requestWithdrawal implements the LF-I3-3 sanctioned calling pattern
// (mirroring internal/httpserver/withdrawal_handlers.go's own handling):
// a *KYCDeniedError is caught INSIDE the closure and converted to a nil
// return so the transaction COMMITS the decision/audit rows
// RequestWithdrawal already wrote before returning it, rather than
// rolling them back. The caught error is still returned to this
// function's own caller unchanged, so every existing assertion on the
// returned error is unaffected - only the transaction's fate changes.
func requestWithdrawal(t *testing.T, pool *db.Pool, f fixture, amount int64, idemKey string) (WithdrawalRequest, error) {
	t.Helper()
	var wr WithdrawalRequest
	var kycDenied *KYCDeniedError
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = RequestWithdrawal(ctx, tx, RequestParams{
			TenantID:        f.tenantID,
			BrandID:         f.brandID,
			PlayerAccountID: f.playerAccountID, PersonID: f.personID,
			WalletID:           f.walletID,
			AssetCode:          "EUR",
			Amount:             amount,
			IdempotencyKey:     idemKey,
			PayoutInstrumentID: f.instrumentID, Destinations: pitest.Shared(),
		})
		if errors.As(err, &kycDenied) {
			return nil
		}
		return err
	})
	if kycDenied != nil {
		return wr, kycDenied
	}
	return wr, err
}

// mustSetWithdrawalPolicy writes a tenant-wide (brand_id/jurisdiction_code
// NULL), single-asset withdrawal_policies row so a test can control
// exactly which ApprovalPolicy Approve/Reject resolve, instead of the
// Stage 3C test/development default (policy.go's defaultApprovalPolicy).
// effectiveFrom lets a test install a SECOND, later-effective row to
// prove a policy change between two decisions on the same request is
// picked up per-call, never memoized (mirroring Stage 3B's original
// per-call thresholdAmount parameter this replaced).
func mustSetWithdrawalPolicy(t *testing.T, pool *db.Pool, tenantID uuid.UUID, assetCode string, thresholdAmount int64, requiredApprovals int, effectiveFrom time.Time) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, $2, $3, $4, $5)`,
			tenantID, assetCode, thresholdAmount, requiredApprovals, effectiveFrom,
		)
		return err
	})
	if err != nil {
		t.Fatalf("set withdrawal policy: %v", err)
	}
}

func mustRequestWithdrawal(t *testing.T, pool *db.Pool, f fixture, amount int64, idemKey string) WithdrawalRequest {
	t.Helper()
	wr, err := requestWithdrawal(t, pool, f, amount, idemKey)
	if err != nil {
		t.Fatalf("RequestWithdrawal failed: %v", err)
	}
	return wr
}

// mustCreateApprover inserts a real, linked, active staff_users row under
// tenantID and returns its id. Stage 3D's mandatory-Person-linkage policy
// (migration 0034) requires every non-automated Approve/Reject decision
// to resolve, at the DATABASE level, to a staff_users row with a non-NULL
// person_id and status = 'active' - a bare uuid.New() approver id, which
// sufficed for every pre-Stage-3D test in this package, is now rejected
// by the withdrawal_approvals_enforce_governance trigger the moment a
// decision actually reaches the INSERT, regardless of what a test's own
// ApproverEligibility closure (below) reports - that closure is this
// package's own Go-level check, entirely decoupled from what the trigger
// independently re-verifies from real table state (see
// ApproverEligibility's own doc comment in withdrawal.go). Every test
// call that expects an Approve/Reject decision to actually be recorded
// must use an id from this helper, not a bare uuid.New(); a call that is
// expected to fail BEFORE the INSERT (e.g. a self-approval or step-up
// refusal) does not need one.
func mustCreateApprover(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	personID := uuid.New()
	staffID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed approver person: %v", err)
	}
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			 VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`,
			staffID, tenantID, staffID.String()+"@example.com", personID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed approver staff user: %v", err)
	}
	return staffID
}

// alwaysEligible is a trivial ApproverEligibility for a test whose
// approver id was created via mustCreateApprover (real, linked, active),
// or for a test that never reaches the point Approve/Reject would
// actually invoke it (e.g. a self-approval or step-up refusal, both
// returned before the INSERT). See mustCreateApprover's own doc comment
// for why the database trigger, not this closure, is what actually
// enforces Stage 3D's policy once a decision is recorded.
func alwaysEligible(uuid.UUID) (linked, active bool, err error) {
	return true, true, nil
}

func cashBalance(t *testing.T, pool *db.Pool, f fixture) int64 {
	t.Helper()
	var signed int64
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := ledger.GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		signed = b.Signed()
		return nil
	})
	return signed
}

func holdBalance(t *testing.T, pool *db.Pool, f fixture) int64 {
	t.Helper()
	var signed int64
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := ledger.GetProjectedBalance(ctx, tx, f.holdAccountID)
		if err != nil {
			return err
		}
		signed = b.Signed()
		return nil
	})
	return signed
}

func TestRequestWithdrawal_PostsHoldAndDebitsCash(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 400, "wd-1")

	if wr.State != StateRequested {
		t.Fatalf("expected state %q, got %q", StateRequested, wr.State)
	}
	if wr.HoldLedgerTransactionID == nil {
		t.Fatal("expected hold_ledger_transaction_id to be set")
	}
	if wr.ReleaseLedgerTransactionID != nil {
		t.Fatal("expected release_ledger_transaction_id to be unset for a non-terminal state")
	}
	if got := cashBalance(t, pool, f); got != 600 {
		t.Fatalf("expected player_cash 600 after a 400 hold on 1000, got %d", got)
	}
	if got := holdBalance(t, pool, f); got != 400 {
		t.Fatalf("expected player_withdrawal_hold 400, got %d", got)
	}
}

func TestRequestWithdrawal_IsIdempotentOnRetry(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	first := mustRequestWithdrawal(t, pool, f, 400, "wd-retry")
	second := mustRequestWithdrawal(t, pool, f, 400, "wd-retry")

	if first.ID != second.ID {
		t.Fatalf("expected retried request to return the same id, got %s vs %s", first.ID, second.ID)
	}
	if got := cashBalance(t, pool, f); got != 600 {
		t.Fatalf("expected exactly one hold posted (cash=600), got %d", got)
	}
}

func TestRequestWithdrawal_InsufficientFundsRejectedAndAtomic(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 100)

	_, err := requestWithdrawal(t, pool, f, 500, "wd-too-much")
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds, got %v", err)
	}

	// The whole transaction (row insert included) must have rolled back -
	// there must never be a WithdrawalRequest row with no
	// hold_ledger_transaction_id (withdrawal-state-machine.md §4).
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE player_account_id = $1`, f.playerAccountID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected zero withdrawal_requests rows after a rejected request, got %d", count)
		}
		return nil
	})
	if got := cashBalance(t, pool, f); got != 100 {
		t.Fatalf("expected player_cash unchanged at 100, got %d", got)
	}
	if got := holdBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_withdrawal_hold unchanged at 0, got %d", got)
	}
}

func TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each request is for 700 - a wallet with only 1000 can fund
			// exactly one of these concurrently-issued requests, never two.
			_, err := requestWithdrawal(t, pool, f, 700, uuid.New().String())
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var succeeded, insufficientFunds int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInsufficientFunds):
			insufficientFunds++
		default:
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 goroutine to succeed, got %d (insufficientFunds=%d)", succeeded, insufficientFunds)
	}
	if insufficientFunds != n-1 {
		t.Fatalf("expected %d goroutines to see insufficient funds, got %d", n-1, insufficientFunds)
	}
	if got := cashBalance(t, pool, f); got != 300 {
		t.Fatalf("expected player_cash 300 after exactly one 700 hold on 1000, got %d", got)
	}
}

func TestFullHappyPath_RequestPendingReviewApproveSubmitComplete(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 300, "wd-happy")

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	// Explicit policy: the zero-config default requires 2 approvals for
	// ANY non-zero withdrawal (fail-closed - see policy.go's
	// defaultApprovalPolicy), so a single-approval happy path needs a
	// configured threshold above the request amount.
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))

	approverID := mustCreateApprover(t, pool, f.tenantID)
	var approved bool
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		// Below the configured policy threshold - a single approval
		// suffices.
		approved, err = Approve(ctx, tx, wr.ID, approverID, false, nil, alwaysEligible)
		return err
	})
	if !approved {
		t.Fatal("expected a single below-threshold approval to approve the request")
	}

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkSubmitted(ctx, tx, wr.ID, "mockpsp", "payout-ref-1")
	})

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Complete(ctx, tx, wr.ID, "mockpsp", "send-confirm-1")
	})

	var final WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	if final.State != StateCompleted {
		t.Fatalf("expected state %q, got %q", StateCompleted, final.State)
	}
	if final.ReleaseLedgerTransactionID == nil {
		t.Fatal("expected release_ledger_transaction_id to be set on completion")
	}

	if got := cashBalance(t, pool, f); got != 700 {
		t.Fatalf("expected player_cash 700 (1000-300) after full completion, got %d", got)
	}
	if got := holdBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_withdrawal_hold 0 after completion, got %d", got)
	}
}

func TestApprove_AboveThresholdRequiresTwoDistinctHumanApprovers(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)

	wr := mustRequestWithdrawal(t, pool, f, 100_000, "wd-above-threshold")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	// Below the request amount (100,000): the default policy's EUR
	// 1,000.00 (100,000 minor-unit) threshold is met via ">=", so this
	// still requires two humans, matching Stage 3B's original 50,000
	// test threshold's outcome exactly.

	// An automated approval never counts toward the two human approvals.
	serviceIdentity := uuid.New()
	var approved bool
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		approved, err = Approve(ctx, tx, wr.ID, serviceIdentity, true, nil, nil)
		return err
	})
	if approved {
		t.Fatal("an automated approval must never satisfy an above-threshold four-eyes requirement by itself")
	}

	firstApprover := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		approved, err = Approve(ctx, tx, wr.ID, firstApprover, false, nil, alwaysEligible)
		return err
	})
	if approved {
		t.Fatal("expected the first human approval to be insufficient above threshold")
	}

	// The same approver cannot supply the second approval.
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Approve(ctx, tx, wr.ID, firstApprover, false, nil, alwaysEligible)
		return err
	})
	if !errors.Is(err, ErrDuplicateApproval) {
		t.Fatalf("expected ErrDuplicateApproval for a second decision by the same approver, got %v", err)
	}

	// A second, DISTINCT human approver completes the four-eyes requirement.
	secondApprover := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		approved, err = Approve(ctx, tx, wr.ID, secondApprover, false, nil, alwaysEligible)
		return err
	})
	if !approved {
		t.Fatal("expected a second distinct human approver to satisfy the four-eyes requirement")
	}

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		if got.State != StateApproved {
			t.Fatalf("expected state %q, got %q", StateApproved, got.State)
		}
		return nil
	})
}

func TestApprove_BeneficiaryCannotApproveOwnWithdrawal(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-self-approve")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	approverIsBeneficiary := func(uuid.UUID) (bool, error) { return true, nil }

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Never reaches the INSERT (self-approval is refused before it) -
		// alwaysEligible only needs to satisfy Approve's own Go-level
		// fail-closed-on-nil contract, not a real staff_users row.
		_, err := Approve(ctx, tx, wr.ID, uuid.New(), false, approverIsBeneficiary, alwaysEligible)
		return err
	})
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval, got %v", err)
	}
}

func TestReject_RestoresPlayerCashBalance(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 400, "wd-reject")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	if got := cashBalance(t, pool, f); got != 600 {
		t.Fatalf("expected cash 600 after hold, got %d", got)
	}

	approverID := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Reject(ctx, tx, wr.ID, approverID, "failed_kyc_check", alwaysEligible)
	})

	var final WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	if final.State != StateRejected {
		t.Fatalf("expected state %q, got %q", StateRejected, final.State)
	}
	if final.ReleaseLedgerTransactionID == nil {
		t.Fatal("expected release_ledger_transaction_id to be set on rejection")
	}
	if got := cashBalance(t, pool, f); got != 1000 {
		t.Fatalf("expected player_cash restored to 1000 after rejection, got %d", got)
	}
	if got := holdBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_withdrawal_hold 0 after rejection, got %d", got)
	}
}

func TestFail_PostSubmissionFailureRestoresPlayerCashBalance(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 250, "wd-fail")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))
	approverID := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Approve(ctx, tx, wr.ID, approverID, false, nil, alwaysEligible)
		return err
	})
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkSubmitted(ctx, tx, wr.ID, "mockpsp", "payout-ref-fail")
	})

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Fail(ctx, tx, wr.ID, "psp_declined")
	})

	var final WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	if final.State != StateFailed {
		t.Fatalf("expected state %q, got %q", StateFailed, final.State)
	}
	if final.ReleaseLedgerTransactionID == nil {
		t.Fatal("expected release_ledger_transaction_id to be set on failure")
	}
	if got := cashBalance(t, pool, f); got != 1000 {
		t.Fatalf("expected player_cash restored to 1000 after post-submission failure, got %d", got)
	}
	if got := holdBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_withdrawal_hold 0 after failure, got %d", got)
	}
}

func TestCancel_OnlyValidFromRequestedState(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-cancel")

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Cancel(ctx, tx, wr.ID)
	})

	var final WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	if final.State != StateCancelled {
		t.Fatalf("expected state %q, got %q", StateCancelled, final.State)
	}
	if final.ReleaseLedgerTransactionID == nil {
		t.Fatal("expected release_ledger_transaction_id to be set on cancellation")
	}
	if got := cashBalance(t, pool, f); got != 1000 {
		t.Fatalf("expected player_cash restored to 1000 after cancellation, got %d", got)
	}

	// Cancelling again (already cancelled - a terminal state) must fail.
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Cancel(ctx, tx, wr.ID)
	})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("expected ErrStateConflict cancelling an already-cancelled request, got %v", err)
	}

	// A request that has moved past `requested` is no longer cancellable.
	wr2 := mustRequestWithdrawal(t, pool, f, 100, "wd-cancel-2")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr2.ID)
	})
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Cancel(ctx, tx, wr2.ID)
	})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("expected ErrStateConflict cancelling a request already in pending_review, got %v", err)
	}
}

func TestListForPlayer_ReturnsOnlyThatPlayersRequests(t *testing.T) {
	pool := testPool(t)
	f1 := seedFixture(t, pool, 1000)
	f2 := seedFixture(t, pool, 1000)

	mustRequestWithdrawal(t, pool, f1, 100, "wd-list-1")
	mustRequestWithdrawal(t, pool, f1, 200, "wd-list-2")
	mustRequestWithdrawal(t, pool, f2, 300, "wd-list-3")

	mustRunTx(t, pool, f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		list, err := ListForPlayer(ctx, tx, f1.playerAccountID)
		if err != nil {
			return err
		}
		if len(list) != 2 {
			t.Fatalf("expected 2 requests for player 1, got %d", len(list))
		}
		for _, wr := range list {
			if wr.PlayerAccountID != f1.playerAccountID {
				t.Fatalf("ListForPlayer leaked a row for a different player: %s", wr.PlayerAccountID)
			}
		}
		return nil
	})
}
