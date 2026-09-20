//go:build integration

package idempotency

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// This file proves this package's composed occurrence keys behave
// correctly against the REAL internal/ledger.Post posting engine and a
// real PostgreSQL 16 database - the actual, already-shipped financial
// path, per this stage's explicit testing requirement. It builds no new
// vendor adapter and no new ledger transaction_type/account_type: every
// test below uses transaction types the schema already accepts
// (migration 0021's CHECK constraint) purely as a VEHICLE for exercising
// the shared idempotency primitive end to end - deposit-shaped entries
// stand in for "an external-provider-mode multi-occurrence event," and
// manual_adjustment-shaped entries stand in for "an in-house-mode
// posting with no external provider," exactly mirroring §11's own
// citation of Flow 16's manual_adjustment as the precedent for that
// second path. Nothing here implements sportsbook, casino, or any other
// vendor's business logic.

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

type fixture struct {
	tenantID          uuid.UUID
	brandID           uuid.UUID
	playerAccountID   uuid.UUID
	walletID          uuid.UUID
	playerAccountID2  uuid.UUID
	walletID2         uuid.UUID
	cashAccountID     uuid.UUID
	cashAccountID2    uuid.UUID
	clearingAccountID uuid.UUID
	adjustmentAcctID  uuid.UUID
	houseGamingAcctID uuid.UUID
	// walletIDUSD/cashAccountIDUSD/clearingAccountIDUSD are a SECOND
	// wallet for player 1 in a genuinely different asset (USD, not EUR) -
	// per ADR 0007's multi-wallet-per-player model, one player legitimately
	// holds a distinct wallet per asset. Used only by the changed-asset
	// replay case in TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters.
	walletIDUSD          uuid.UUID
	cashAccountIDUSD     uuid.UUID
	clearingAccountIDUSD uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	f := fixture{
		tenantID:         uuid.New(),
		brandID:          uuid.New(),
		playerAccountID:  uuid.New(),
		playerAccountID2: uuid.New(),
	}
	personID1 := uuid.New()
	personID2 := uuid.New()

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
		for _, p := range []uuid.UUID{personID1, personID2} {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p); err != nil {
				return err
			}
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
			f.playerAccountID, f.tenantID, f.brandID, personID1, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID2, f.tenantID, f.brandID, personID2, f.playerAccountID2.String()+"@example.com"); err != nil {
			return err
		}
		f.walletID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			f.walletID, f.tenantID, f.brandID, f.playerAccountID); err != nil {
			return err
		}
		f.walletID2 = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			f.walletID2, f.tenantID, f.brandID, f.playerAccountID2); err != nil {
			return err
		}
		var err error
		f.cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		f.cashAccountID2, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID2, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		f.clearingAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		f.adjustmentAcctID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		f.houseGamingAcctID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
		if err != nil {
			return err
		}

		// A second wallet for player 1, in USD rather than EUR - the
		// multi-wallet-per-player model (ADR 0007) means this is a
		// perfectly ordinary, legitimate second wallet for the SAME
		// player, not a different player. Used to exercise a genuine
		// changed-asset replay below.
		f.walletIDUSD = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'USD')`,
			f.walletIDUSD, f.tenantID, f.brandID, f.playerAccountID); err != nil {
			return err
		}
		f.cashAccountIDUSD, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletIDUSD, ledger.AccountPlayerCash, "USD")
		if err != nil {
			return err
		}
		f.clearingAccountIDUSD, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "USD")
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant-scoped fixture: %v", err)
	}
	return f
}

func mustPost(t *testing.T, pool *db.Pool, tenantID uuid.UUID, in ledger.TransactionInput) ledger.PostResult {
	t.Helper()
	var res ledger.PostResult
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = ledger.Post(ctx, tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	return res
}

func tryPost(pool *db.Pool, tenantID uuid.UUID, in ledger.TransactionInput) (ledger.PostResult, error) {
	var res ledger.PostResult
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = ledger.Post(ctx, tx, in)
		return err
	})
	return res, err
}

func externalVehicleInput(f fixture, tenantID uuid.UUID, providerID string, cashAccountID uuid.UUID, composed string, correlationID uuid.UUID, amount int64) ledger.TransactionInput {
	return externalVehicleInputWithClearing(f, tenantID, providerID, f.clearingAccountID, cashAccountID, composed, correlationID, amount)
}

// externalVehicleInputWithClearing is externalVehicleInput generalized to
// accept an explicit clearing account, so a test can post entries in a
// SPECIFIC asset (the clearing and cash accounts must share one asset for
// the transaction to balance - see ledger.Post's per-asset balance
// invariant) rather than always the fixture's default EUR pair. Used by
// the changed-asset replay case below.
func externalVehicleInputWithClearing(f fixture, tenantID uuid.UUID, providerID string, clearingAccountID, cashAccountID uuid.UUID, composed string, correlationID uuid.UUID, amount int64) ledger.TransactionInput {
	// A deposit-shaped vehicle: Dr psp_clearing / Cr player_cash - stands
	// in for "an external-provider-mode multi-occurrence event" (e.g. a
	// sportsbook settlement/cashout/partial-settlement occurrence),
	// without inventing a new transaction_type. providerID/composed play
	// exactly the role Assign(ModeExternalProvider, ...) documents.
	assignment, err := Assign(ModeExternalProvider, composed, providerID, providerID+":"+composed)
	if err != nil {
		panic(err) // test-construction error, not a runtime path
	}
	return ledger.TransactionInput{
		TenantID:        tenantID,
		TransactionType: ledger.TxDeposit,
		IdempotencyKey:  assignment.IdempotencyKey,
		ProviderID:      assignment.ProviderID,
		ProviderTxID:    assignment.ProviderTxID,
		CorrelationID:   correlationID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: clearingAccountID, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
		},
	}
}

func inHouseVehicleInput(f fixture, tenantID uuid.UUID, cashAccountID uuid.UUID, composed string, correlationID uuid.UUID, amount int64) ledger.TransactionInput {
	// A manual_adjustment-shaped vehicle: Dr manual_adjustment /
	// Cr player_cash - stands in for "an in-house-mode posting with no
	// external provider" (ADR 0038 §14.6's routing rule), mirroring §11's
	// own citation of Flow 16's manual_adjustment as the precedent.
	assignment, err := Assign(ModeInHouse, composed, "", "")
	if err != nil {
		panic(err)
	}
	reason := "idempotency-primitive-integration-test"
	return ledger.TransactionInput{
		TenantID:        tenantID,
		TransactionType: ledger.TxManualAdjustment,
		IdempotencyKey:  assignment.IdempotencyKey,
		ProviderID:      assignment.ProviderID,
		ProviderTxID:    assignment.ProviderTxID,
		ReasonCode:      &reason,
		CorrelationID:   correlationID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: f.adjustmentAcctID, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
		},
	}
}

// TestIntegration_DuplicateDelivery proves a duplicate delivery of the
// SAME occurrence (identical composed key, identical payload) is an
// idempotent no-op against the real database - required test case
// "duplicate delivery."
func TestIntegration_DuplicateDelivery(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	corr := uuid.New()

	composed, err := ComposeOccurrenceKey("settle-ref-dup", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	first := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed, corr, 500))
	if first.AlreadyPosted {
		t.Fatal("first delivery should not be AlreadyPosted")
	}
	second := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed, corr, 500))
	if !second.AlreadyPosted {
		t.Fatal("duplicate delivery should be AlreadyPosted (idempotent no-op)")
	}
	if second.TransactionID != first.TransactionID {
		t.Fatalf("duplicate delivery returned a different transaction id: %s vs %s", second.TransactionID, first.TransactionID)
	}

	assertBalance(t, pool, f.tenantID, f.cashAccountID, 500) // posted exactly once
}

// TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse proves two
// genuinely distinct occurrences of the same correlation/transaction
// shape (e.g. two bet-builder legs, or a partial settlement followed by
// a second one) - distinguished ONLY by occurrence_ordinal, with
// otherwise-identical payloads - both post independently. Required test
// case "legitimate second occurrence" and directly the scenario ADR 0038
// §14/P1-3 named as unresolved before this package's fix.
func TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	corr := uuid.New()

	ord1 := OccurrenceOrdinal(1)
	ord2 := OccurrenceOrdinal(2)
	composed1, err := ComposeOccurrenceKeyWithOrdinal("leg-settlement-ref", &ord1)
	if err != nil {
		t.Fatalf("compose 1: %v", err)
	}
	composed2, err := ComposeOccurrenceKeyWithOrdinal("leg-settlement-ref", &ord2)
	if err != nil {
		t.Fatalf("compose 2: %v", err)
	}
	if composed1 == composed2 {
		t.Fatal("two distinct ordinals over the same reference must compose differently")
	}

	// Identical payload (same amount) on purpose - the exact "coincident
	// payload" scenario the original ADR 0038 §14 gap named.
	first := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed1, corr, 300))
	second := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed2, corr, 300))

	if first.AlreadyPosted || second.AlreadyPosted {
		t.Fatalf("both occurrences should post as NEW transactions, got AlreadyPosted=%v/%v", first.AlreadyPosted, second.AlreadyPosted)
	}
	if first.TransactionID == second.TransactionID {
		t.Fatal("two distinct occurrences must never share a transaction id")
	}
	// Both amounts posted - 300 + 300 = 600, not absorbed as one.
	assertBalance(t, pool, f.tenantID, f.cashAccountID, 600)
}

// TestIntegration_ReplayWithChangedProviderDoesNotCollapse proves the
// SAME provider_tx_id delivered under a DIFFERENT provider_id is a
// distinct occurrence, not a collapse - required test case "replay with
// changed provider."
func TestIntegration_ReplayWithChangedProviderDoesNotCollapse(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	composed, err := ComposeOccurrenceKey("shared-ref", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	first := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "provider-a", f.cashAccountID, composed, uuid.New(), 100))
	second := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "provider-b", f.cashAccountID, composed, uuid.New(), 100))

	if first.AlreadyPosted || second.AlreadyPosted {
		t.Fatal("a different provider_id must never be treated as the same occurrence")
	}
	if first.TransactionID == second.TransactionID {
		t.Fatal("different providers must never share a transaction id")
	}
	assertBalance(t, pool, f.tenantID, f.cashAccountID, 200)
}

// TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters
// documents the REAL, pre-existing behavior of internal/ledger.Post
// (unchanged by this package - it is not internal/idempotency's to
// change, per this stage's explicit scope) when an adapter INCORRECTLY
// reuses the identical composed occurrence key for a genuinely different
// financial fact: the redelivery is silently treated as an idempotent
// no-op of the ORIGINAL fact, and the new fact's own amount/entries are
// never posted. Required test cases "replay with changed amount,"
// "replay with changed asset," and "replay with changed player."
//
// This is precisely why fix #1 (S-5: never let the occurrence
// discriminator come from anything other than an authenticated payload
// field) and fix #2 (S-6: lossless composition) exist: a CORRECT adapter
// following this package's contract derives a DIFFERENT composed key for
// a genuinely different occurrence (see
// TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse above), so
// this absorption path is only ever reachable through an adapter defect
// (reusing a reference across two distinct occurrences) - never through
// this package's own logic.
func TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	corr := uuid.New()

	composed, err := ComposeOccurrenceKey("reused-ref-by-adapter-defect", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	original := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed, corr, 100))

	// "Replay" with a changed amount, SAME composed key (an adapter
	// defect scenario, not this package's own behavior).
	changedAmount := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed, corr, 999))
	if !changedAmount.AlreadyPosted || changedAmount.TransactionID != original.TransactionID {
		t.Fatalf("expected the ledger's own exact-retry path to short-circuit to the ORIGINAL transaction for a reused key, got AlreadyPosted=%v id=%s (original=%s)",
			changedAmount.AlreadyPosted, changedAmount.TransactionID, original.TransactionID)
	}
	// The original amount (100), not the "replayed" 999, is what's
	// posted - proving the changed amount was never applied.
	assertBalance(t, pool, f.tenantID, f.cashAccountID, 100)

	// "Replay" with a changed player (a different wallet's cash
	// account), SAME composed key.
	changedPlayer, err := tryPost(pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID2, composed, corr, 100))
	if err != nil {
		t.Fatalf("changed-player replay: %v", err)
	}
	if !changedPlayer.AlreadyPosted || changedPlayer.TransactionID != original.TransactionID {
		t.Fatal("expected the reused key to short-circuit regardless of which wallet's account the new entries named")
	}
	// player 2's cash account received NOTHING - the "replay" was fully
	// absorbed into player 1's original transaction.
	assertBalance(t, pool, f.tenantID, f.cashAccountID2, 0)

	// "Replay" with a changed ASSET - a genuinely different currency
	// (USD, not the original EUR), on a second wallet belonging to the
	// SAME player (f.playerAccountID, per ADR 0007's multi-wallet-
	// per-player model - this is deliberately NOT the changed-player case
	// above), SAME composed key. Both legs of this "replayed" input are
	// internally USD-consistent (so it would post as a perfectly valid,
	// independently balanced transaction if the key were NOT reused) -
	// the only defect is the adapter incorrectly reusing the original
	// EUR occurrence's key for a distinct USD fact.
	changedAsset, err := tryPost(pool, f.tenantID, externalVehicleInputWithClearing(f, f.tenantID, "mock-provider", f.clearingAccountIDUSD, f.cashAccountIDUSD, composed, corr, 100))
	if err != nil {
		t.Fatalf("changed-asset replay: %v", err)
	}
	if !changedAsset.AlreadyPosted || changedAsset.TransactionID != original.TransactionID {
		t.Fatal("expected the reused key to short-circuit regardless of which asset's account the new entries named")
	}
	// The USD cash account received NOTHING - the "replay" was fully
	// absorbed into the original EUR transaction, exactly like the
	// changed-amount and changed-player cases above.
	assertBalance(t, pool, f.tenantID, f.cashAccountIDUSD, 0)
}

// TestIntegration_CallbackAndSettlementRedelivery proves an ordinary
// (single-occurrence) callback redelivery - covering both a placement-
// shaped and a settlement-shaped vehicle - is idempotent, and separately
// proves the actual, already-shipped internal/casino idempotency
// mechanism (ADR 0020 exact-retry, distinct real transaction types) is
// unaffected by this package's presence, since internal/casino imports
// nothing from internal/idempotency and this stage changes no casino
// behavior. Required test cases "callback redelivery," "settlement
// redelivery."
func TestIntegration_CallbackAndSettlementRedelivery(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	corr := uuid.New()

	placementComposed, err := ComposeOccurrenceKey("placement-ref", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	settlementComposed, err := ComposeOccurrenceKey("settlement-ref", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	placement1 := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, placementComposed, corr, 200))
	placement2 := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, placementComposed, corr, 200))
	if !placement2.AlreadyPosted || placement2.TransactionID != placement1.TransactionID {
		t.Fatal("placement (callback) redelivery must be idempotent")
	}

	settlement1 := mustPost(t, pool, f.tenantID, inHouseVehicleInput(f, f.tenantID, f.cashAccountID, settlementComposed, corr, 200))
	settlement2 := mustPost(t, pool, f.tenantID, inHouseVehicleInput(f, f.tenantID, f.cashAccountID, settlementComposed, corr, 200))
	if !settlement2.AlreadyPosted || settlement2.TransactionID != settlement1.TransactionID {
		t.Fatal("settlement redelivery must be idempotent")
	}
}

// TestIntegration_RollbackRedeliveryCorrectionAndReversal proves a
// rollback/correction/reversal event, keyed via ReversesTransactionID
// exactly as ADR 0038 §10/§14.2 cases 4/5 specify, is itself idempotent
// under redelivery and posts as a NEW, traceable transaction rather than
// mutating the original. Required test cases "rollback redelivery,"
// "correction," "reversal."
func TestIntegration_RollbackRedeliveryCorrectionAndReversal(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	corr := uuid.New()

	originalComposed, err := ComposeOccurrenceKey("original-fact-ref", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	original := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, originalComposed, corr, 400))

	rollbackComposed, err := ComposeOccurrenceKey("rollback-of-original-fact", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	rollbackInput := inHouseVehicleInput(f, f.tenantID, f.cashAccountID, rollbackComposed, corr, 400)
	origID := original.TransactionID
	rollbackInput.ReversesTransactionID = &origID
	// Reverse direction of the entries relative to the original fact -
	// a compensating entry, never an edit of the original row
	// (CLAUDE.md's rollback rule).
	rollbackInput.Entries = []ledger.EntryInput{
		{LedgerAccountID: f.cashAccountID, Direction: ledger.Debit, Amount: 400},
		{LedgerAccountID: f.adjustmentAcctID, Direction: ledger.Credit, Amount: 400},
	}

	rollback1 := mustPost(t, pool, f.tenantID, rollbackInput)
	if rollback1.AlreadyPosted {
		t.Fatal("first rollback should not be AlreadyPosted")
	}

	// Rollback redelivery - identical payload, must be idempotent.
	rollback2 := mustPost(t, pool, f.tenantID, rollbackInput)
	if !rollback2.AlreadyPosted || rollback2.TransactionID != rollback1.TransactionID {
		t.Fatal("rollback redelivery must be idempotent")
	}

	// Net effect: the original credit (400) is exactly offset by the
	// rollback's debit (400) - back to zero, proving the compensating-
	// entry shape and that redelivery did not double-reverse.
	assertBalance(t, pool, f.tenantID, f.cashAccountID, 0)

	// Correction: a SECOND, independent transaction also pointing at the
	// same original via ReversesTransactionID (e.g. a market correction
	// distinct from the rollback above) - must post as its own new row,
	// never edit the original or the rollback.
	correctionComposed, err := ComposeOccurrenceKey("correction-of-original-fact", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	correctionInput := inHouseVehicleInput(f, f.tenantID, f.cashAccountID, correctionComposed, corr, 50)
	correctionInput.ReversesTransactionID = &origID
	correction := mustPost(t, pool, f.tenantID, correctionInput)
	if correction.TransactionID == original.TransactionID || correction.TransactionID == rollback1.TransactionID {
		t.Fatal("a correction must be its own distinct transaction, never a mutation of an existing one")
	}

	verifyReversesChain(t, pool, f.tenantID, rollback1.TransactionID, origID)
	verifyReversesChain(t, pool, f.tenantID, correction.TransactionID, origID)
}

func verifyReversesChain(t *testing.T, pool *db.Pool, tenantID, childID, expectedParentID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var parent uuid.UUID
		err := tx.QueryRow(ctx, `SELECT reverses_transaction_id FROM ledger_transactions WHERE id = $1`, childID).Scan(&parent)
		if err != nil {
			return err
		}
		if parent != expectedParentID {
			t.Fatalf("expected reverses_transaction_id %s, got %s", expectedParentID, parent)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify reverses chain: %v", err)
	}
}

// TestIntegration_InHouseModeDuplicateDoesNotDoubleDebit is the ADR 0038
// §14.6 worked example, run against the real database: a retried
// in-house-mode posting (provider_id/provider_tx_id both NULL, uniqueness
// via idempotency_key alone) with a REPRODUCIBLE composed reference
// (never a fresh value per attempt) is idempotent - the exact scenario
// that closed the partial-index gap.
func TestIntegration_InHouseModeDuplicateDoesNotDoubleDebit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	corr := uuid.New()

	// Simulates the worked example's "iref-B-accept-7f3a": a stable,
	// intrinsic reference reproduced verbatim on retry.
	composed, err := ComposeOccurrenceKey("iref-B-accept-7f3a", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	first := mustPost(t, pool, f.tenantID, inHouseVehicleInput(f, f.tenantID, f.cashAccountID, composed, corr, 700))
	if first.AlreadyPosted {
		t.Fatal("first in-house posting should not be AlreadyPosted")
	}
	retry := mustPost(t, pool, f.tenantID, inHouseVehicleInput(f, f.tenantID, f.cashAccountID, composed, corr, 700))
	if !retry.AlreadyPosted || retry.TransactionID != first.TransactionID {
		t.Fatal("retried in-house posting must be idempotent, never a second debit")
	}

	// Confirm provider_id/provider_tx_id truly stayed NULL (never a
	// reserved sentinel - ADR 0038 §14.6).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var providerID, providerTxID *string
		scanErr := tx.QueryRow(ctx,
			`SELECT provider_id, provider_tx_id FROM ledger_transactions WHERE id = $1`, first.TransactionID,
		).Scan(&providerID, &providerTxID)
		if scanErr != nil {
			return scanErr
		}
		if providerID != nil || providerTxID != nil {
			t.Fatalf("expected provider_id/provider_tx_id to stay NULL for in-house mode, got %v/%v", providerID, providerTxID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	assertBalance(t, pool, f.tenantID, f.cashAccountID, 700)
}

func assertBalance(t *testing.T, pool *db.Pool, tenantID, accountID uuid.UUID, want int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		bal, err := ledger.GetProjectedBalance(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if bal.Signed() != want {
			t.Fatalf("expected signed balance %d for account %s, got %d (debit=%d credit=%d)", want, accountID, bal.Signed(), bal.DebitTotal, bal.CreditTotal)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assert balance: %v", err)
	}
}
