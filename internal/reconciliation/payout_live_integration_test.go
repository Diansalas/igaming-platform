//go:build integration

// RECON-PAYOUT-LIVE-TEST-1 (code-reviewer PRH-I5 FH-7 re-review N1; PRH-2 H DoD): the LIVE, attempt-based
// payout path reconciled against the WIRED MOCK payment statement source. Every other payout test in this
// package uses the synthetic payoutFixture with a fixed source; none would notice a change to MOCK payout
// rendering, or to how ApplyPayoutResult / PollPayoutStatus write provider_reference and the settlement
// posting, that recreated the F1 false-P1 class for payouts.
package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func TestPaymentStatement_LivePayoutPath_AgainstWiredMockSource_NoMismatches(t *testing.T) {
	w := newPayWorld(t)
	ctx := context.Background()
	var personID uuid.UUID
	if err := w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, w.f.playerAccountID).Scan(&personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`,
			uuid.New(), w.f.tenantID, w.f.brandID, w.f.playerAccountID, personID); err != nil {
			return err
		}
		// Fund the player's cash account.
		prov, ptx := "seed", "seed-live-payout-"+w.f.tenantID.String()
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: ptx, ProviderID: &prov, ProviderTxID: &ptx,
			CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.clearingID, Direction: ledger.Debit, Amount: 10_000},
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: 10_000},
			},
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`, w.f.tenantID)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wr withdrawal.WithdrawalRequest
	if err := w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: personID,
			WalletID: w.f.walletID, AssetCode: "EUR", Amount: 700, IdempotencyKey: "live-payout-" + uuid.NewString(),
		})
		if err != nil {
			return err
		}
		if err := withdrawal.MoveToPendingReview(ctx, tx, wr.ID); err != nil {
			return err
		}
		_, err = withdrawal.Approve(ctx, tx, wr.ID, uuid.New(), true, nil, nil)
		return err
	}); err != nil {
		t.Fatalf("request/approve: %v", err)
	}

	// ClaimForDispatch -> DispatchWithdraw -> ApplyPayoutResult (pending, reference bound) ...
	actor := payments.SubmitActor{StaffID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "test", RequestID: uuid.NewString()}
	claim, err := w.orch.ClaimForDispatch(ctx, w.pool, payments.KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", actor)
	if err != nil || claim.Denied {
		t.Fatalf("ClaimForDispatch: %v denied=%v", err, claim.Denied)
	}
	provider := *claim.Attempt.ProviderID
	mock := map[string]*payments.MockProvider{payProvA: w.mockA, payProvB: w.mockB}[provider]
	if mock == nil {
		t.Fatalf("payout routed to unexpected provider %q", provider)
	}
	gr := payments.DispatchWithdraw(ctx, w.pool, payments.MockCredentialResolver{}, mock, claim.Attempt)
	if err := payments.ApplyPayoutResult(ctx, w.pool, w.f.tenantID, wr.ID, claim.Attempt, gr, payments.EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	var attempt payments.PaymentAttempt
	if err := w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = payments.GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if attempt.State != payments.AttemptPending || attempt.ProviderReference == nil {
		t.Fatalf("test premise: pending payout with a reference, got %s", attempt.State)
	}

	// ... Resolve(succeeded) -> PollPayoutStatus (settlement posted, attempt succeeded) ...
	mock.Resolve(*attempt.ProviderReference, payments.OutcomeSucceeded, "", false)
	if err := payments.PollPayoutStatus(ctx, w.pool, w.orch, payments.MockCredentialResolver{}, w.f.tenantID, attempt, time.Now().Add(time.Hour), nil); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}
	if err := w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := payments.GetAttemptByID(ctx, tx, attempt.ID)
		if err != nil {
			return err
		}
		if a.State != payments.AttemptSucceeded {
			t.Fatalf("test premise: succeeded payout, got %s", a.State)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// ... reconcile against the wired MOCK source: 0 mismatches.
	src := w.srcA
	if provider == payProvB {
		src = w.srcB
	}
	w.mustClean(t, src, PaymentStatementOptions{})
}
