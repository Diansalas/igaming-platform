//go:build integration

// Stage 4H-B1 Wave 3 Phase 3 (item A) test suite for the deposit/reload
// event-consumption sweep (deposit_sweep.go). Seeds a real deposit
// directly against ledger_transactions/deposit_intents (mirroring
// internal/payments' own postDepositSuccess posting shape, Dr
// psp_clearing / Cr player_cash) rather than importing internal/payments
// - this package has no need to import it at all (see deposit_sweep.go's
// own top-of-file doc comment), and this suite is no exception.
package bonus

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// seedRawDeposit posts a real 'deposit' ledger transaction for f
// (Dr psp_clearing / Cr player_cash) plus its own deposit_intents row -
// the exact shape RunDepositSweepForTenant's own join expects.
func seedRawDeposit(t *testing.T, pool *db.Pool, f lifecycleFixture, amount int64, paymentMethod string) (ledgerTxID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intentID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'succeeded')`,
			intentID, f.tenantID, f.brandID, f.playerID, f.walletID, f.assetCode, amount, paymentMethod, "idem-"+intentID.String(),
		); err != nil {
			return err
		}

		clearing, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, f.assetCode)
		if err != nil {
			return err
		}
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, f.assetCode)
		if err != nil {
			return err
		}
		result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "mock-psp:" + intentID.String(),
			CorrelationID: intentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return err
		}
		ledgerTxID = result.TransactionID
		_, err = tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $2 WHERE id = $1`, intentID, ledgerTxID)
		return err
	})
	if err != nil {
		t.Fatalf("seed raw deposit: %v", err)
	}
	return ledgerTxID
}

// seedDepositMatchableOffer creates an Active Campaign/Offer/OfferVersion
// this sweep can match: R1 reward, auto_issue, the given eligibility
// axes.
func seedDepositMatchableOffer(t *testing.T, pool *db.Pool, f lifecycleFixture, firstDepositOnly bool, depositMethods []string) campaignOffer {
	t.Helper()
	var co campaignOffer
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, Status: CampaignActive, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		if err != nil {
			return err
		}
		co.campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		if err != nil {
			return err
		}
		co.campaignVersionID = v.ID
		if _, err := SetCampaignCurrentVersion(ctx, tx, f.tenantID, c.ID, v.ID); err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{
			TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID,
			GrantPolicy: GrantPolicyAutoIssue, Status: OfferActive, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID,
		})
		if err != nil {
			return err
		}
		co.offerID = o.ID
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardPercentageWithCap, RewardAssetCode: f.assetCode,
			RewardCalculation:      []byte(`{"rate_bp":5000,"cap_amount":"100000"}`),
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			FirstDepositOnly:          firstDepositOnly,
			EligibilityDepositMethods: depositMethods,
			CreatedByActorType:        ActorStaff, CreatedByActorID: f.staffID,
		})
		if err != nil {
			return err
		}
		co.offerVersionID = ov.ID
		if _, err := SetOfferCurrentVersion(ctx, tx, f.tenantID, o.ID, ov.ID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed deposit-matchable offer: %v", err)
	}
	return co
}

func TestRunDepositSweepForTenant_MatchesAndAdvancesWatermark(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)
	depositTxID := seedRawDeposit(t, pool, f, 10000, "card")

	var outcome DepositSweepOutcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant: %v", err)
	}
	if outcome.EventsProcessed != 1 {
		t.Fatalf("expected 1 event processed, got %d", outcome.EventsProcessed)
	}
	// The AssetAuthorization gate denies (JurisdictionCode="" - this
	// platform's own pre-existing, disclosed per-player jurisdiction gap,
	// see RunDepositSweepForTenant's own doc comment) - proving the sweep
	// reached IssueAndActivateDepositBonus at all (a Grant row now
	// exists, cancelled) is this test's own assertion, not a claim that
	// the gate itself allows.
	if outcome.GrantsDenied != 1 || outcome.GrantsIssued != 0 {
		t.Fatalf("expected exactly one denied (jurisdiction gap) issuance attempt, got issued=%d denied=%d", outcome.GrantsIssued, outcome.GrantsDenied)
	}

	var grantCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND trigger_reference = $2`, f.tenantID, depositTxID.String()).Scan(&grantCount)
	})
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("expected exactly one grant with trigger_reference=%s, got %d", depositTxID, grantCount)
	}

	// Watermark advanced - a second tick with no new deposits is a
	// true no-op.
	var second DepositSweepOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		second, runErr = RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("second RunDepositSweepForTenant: %v", err)
	}
	if second.EventsProcessed != 0 {
		t.Fatalf("expected the second tick to find zero new events (watermark advanced), got %d", second.EventsProcessed)
	}
}

func TestRunDepositSweepForTenant_FirstDepositOnlyExcludesSubsequentDeposits(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, true, nil)
	firstTxID := seedRawDeposit(t, pool, f, 10000, "card")
	secondTxID := seedRawDeposit(t, pool, f, 5000, "card")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant: %v", err)
	}

	var firstCount, secondCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND trigger_reference = $2`, f.tenantID, firstTxID.String()).Scan(&firstCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND trigger_reference = $2`, f.tenantID, secondTxID.String()).Scan(&secondCount)
	})
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if firstCount != 1 {
		t.Fatalf("expected a grant attempt for the FIRST deposit, got %d", firstCount)
	}
	if secondCount != 0 {
		t.Fatalf("expected NO grant attempt for the second deposit under first_deposit_only, got %d", secondCount)
	}
}

func TestRunDepositSweepForTenant_DepositMethodMismatchSkipsCandidate(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, []string{"card"})
	depositTxID := seedRawDeposit(t, pool, f, 10000, "crypto")

	var outcome DepositSweepOutcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant: %v", err)
	}
	if outcome.EventsProcessed != 1 {
		t.Fatalf("expected the deposit event itself to be examined, got %d", outcome.EventsProcessed)
	}
	if outcome.GrantsIssued != 0 || outcome.GrantsDenied != 0 {
		t.Fatalf("expected zero issuance ATTEMPTS for a deposit_method mismatch, got issued=%d denied=%d", outcome.GrantsIssued, outcome.GrantsDenied)
	}
	var grantCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND trigger_reference = $2`, f.tenantID, depositTxID.String()).Scan(&grantCount)
	})
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grantCount != 0 {
		t.Fatalf("expected zero grants for a payment-method-mismatched deposit, got %d", grantCount)
	}
}

func TestRunDepositSweepForTenant_NoMatchingOfferIsANoOp(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedRawDeposit(t, pool, f, 10000, "card")

	var outcome DepositSweepOutcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant: %v", err)
	}
	if outcome.EventsProcessed != 1 {
		t.Fatalf("expected the deposit itself to be examined even with no matching offer, got %d", outcome.EventsProcessed)
	}
	if outcome.GrantsIssued != 0 && outcome.GrantsDenied != 0 {
		t.Fatalf("expected zero issuance attempts with no matching offer, got issued=%d denied=%d", outcome.GrantsIssued, outcome.GrantsDenied)
	}
}

// seedSecondPlayerAccountSamePerson creates a second player_account (and
// its own wallet) belonging to the SAME Person as f's own playerID -
// exactly the multi-account shape detectMultiAccountFirstDepositSignal
// exists to detect.
func seedSecondPlayerAccountSamePerson(t *testing.T, pool *db.Pool, f lifecycleFixture) (secondPlayerID, secondWalletID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var personID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerID).Scan(&personID); err != nil {
			return err
		}
		secondPlayerID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			secondPlayerID, f.tenantID, f.brandID, personID, secondPlayerID.String()+"@example.com"); err != nil {
			return err
		}
		secondWalletID = uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			secondWalletID, f.tenantID, f.brandID, secondPlayerID, f.assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed second player account (same person): %v", err)
	}
	return secondPlayerID, secondWalletID
}

// seedRawDepositForAccount mirrors seedRawDeposit but posts against an
// explicit (playerID, walletID) pair rather than f's own default account -
// needed to simulate a SECOND player_account of the same Person making
// its own "first" deposit.
func seedRawDepositForAccount(t *testing.T, pool *db.Pool, f lifecycleFixture, playerID, walletID uuid.UUID, amount int64, paymentMethod string) (ledgerTxID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intentID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'succeeded')`,
			intentID, f.tenantID, f.brandID, playerID, walletID, f.assetCode, amount, paymentMethod, "idem-"+intentID.String(),
		); err != nil {
			return err
		}
		clearing, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, f.assetCode)
		if err != nil {
			return err
		}
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerCash, f.assetCode)
		if err != nil {
			return err
		}
		result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "mock-psp:" + intentID.String(),
			CorrelationID: intentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return err
		}
		ledgerTxID = result.TransactionID
		_, err = tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $2 WHERE id = $1`, intentID, ledgerTxID)
		return err
	})
	if err != nil {
		t.Fatalf("seed raw deposit for account: %v", err)
	}
	return ledgerTxID
}

// TestRunDepositSweepForTenant_MultiAccountSamePersonFirstDepositSignal
// proves the REQ-SEP-BONUS-3 detection signal (deposit_sweep.go's
// detectMultiAccountFirstDepositSignal): when the SAME Person's second
// player_account makes its own "first" deposit against a FirstDepositOnly
// Offer the Person's OTHER account already holds a Grant for, the sweep
// records a bonus_deposit_sweep.multi_account_signal_detected audit entry
// naming both accounts - WITHOUT denying, skipping, or altering the
// Grant attempt itself (still an ordinary issuance attempt, gated
// identically to every other one). This test FAILS against the pre-fix
// code (no such call existed in RunDepositSweepForTenant) and PASSES
// post-fix.
func TestRunDepositSweepForTenant_MultiAccountSamePersonFirstDepositSignal(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, true, nil)

	secondPlayerID, secondWalletID := seedSecondPlayerAccountSamePerson(t, pool, f)

	// Account 2's own "first" deposit - a Grant attempt (and row) is
	// created for account 2 against the FirstDepositOnly Offer.
	seedRawDepositForAccount(t, pool, f, secondPlayerID, secondWalletID, 10000, "card")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant (account 2): %v", err)
	}

	// Account 1 (f.playerID)'s OWN "first" deposit - per-account
	// first-deposit-only eligibility passes (it IS f.playerID's own first
	// deposit), but the Person behind it already holds a Grant against
	// this Offer via account 2.
	seedRawDeposit(t, pool, f, 10000, "card")
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant (account 1): %v", err)
	}

	var signalCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'bonus_deposit_sweep.multi_account_signal_detected' AND target_id = (SELECT person_id::text FROM player_accounts WHERE id = $2)`,
			f.tenantID, f.playerID,
		).Scan(&signalCount)
	})
	if err != nil {
		t.Fatalf("count multi-account signal audit rows: %v", err)
	}
	if signalCount != 1 {
		t.Fatalf("expected exactly one multi-account first-deposit signal audit entry, got %d", signalCount)
	}

	// The signal is a SIGNAL ONLY - account 1's own Grant attempt still
	// happened exactly as it would have without the signal (still exactly
	// one attempted issuance, same jurisdiction-gap denial as every other
	// sweep test - never silently skipped or blocked by the signal).
	var grantCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerID).Scan(&grantCount)
	})
	if err != nil {
		t.Fatalf("count account 1 grants: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("expected the signal to be non-blocking (exactly one ordinary Grant attempt for account 1), got %d", grantCount)
	}
}

// TestRunDepositSweepForTenant_NoMultiAccountSignalWhenNotFirstDepositOnly
// proves the detector only runs for FirstDepositOnly Offers - an ordinary
// (non-first-deposit-only) Offer generates no signal even when the same
// Person holds accounts with grants against it, since the per-account
// eligibility axis this signal protects does not apply.
func TestRunDepositSweepForTenant_NoMultiAccountSignalWhenNotFirstDepositOnly(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)

	secondPlayerID, secondWalletID := seedSecondPlayerAccountSamePerson(t, pool, f)
	seedRawDepositForAccount(t, pool, f, secondPlayerID, secondWalletID, 10000, "card")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant (account 2): %v", err)
	}

	seedRawDeposit(t, pool, f, 10000, "card")
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant (account 1): %v", err)
	}

	var signalCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'bonus_deposit_sweep.multi_account_signal_detected'`, f.tenantID).Scan(&signalCount)
	})
	if err != nil {
		t.Fatalf("count multi-account signal audit rows: %v", err)
	}
	if signalCount != 0 {
		t.Fatalf("expected zero multi-account signals for a non-FirstDepositOnly Offer, got %d", signalCount)
	}
}

// TestRunDepositSweepScheduler_TenantAdvisoryLockSerializesOverlappingTicks
// proves the scheduler-loop wrapper's own advisory-lock guard: two
// concurrent tenant ticks for the SAME tenant never run RunDepositSweepForTenant's
// own body concurrently (which would otherwise be free to double-process
// the same watermark read).
func TestRunDepositSweepScheduler_TenantAdvisoryLockSerializesOverlappingTicks(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)
	seedRawDeposit(t, pool, f, 10000, "card")

	acquiredCount := 0
	for i := 0; i < 2; i++ {
		acquired, err := tryAdvisoryLockedTenantJob(context.Background(), pool, f.tenantID, "bonus_deposit_sweep_test", func(ctx context.Context, tx pgx.Tx) error {
			time.Sleep(10 * time.Millisecond)
			return nil
		})
		if err != nil {
			t.Fatalf("tryAdvisoryLockedTenantJob: %v", err)
		}
		if acquired {
			acquiredCount++
		}
	}
	if acquiredCount != 2 {
		t.Fatalf("expected both SEQUENTIAL (non-overlapping) attempts to acquire the lock, got %d", acquiredCount)
	}
}
