//go:build integration

// Stage 6 sportsbook vertical-slice integration suite: bet placement
// (stake locked, ledger balanced), insufficient balance (atomic no-op),
// odds-changed rejection, RG denial, risk denial, idempotency, and a
// genuine concurrency test using this codebase's OWN established
// deterministic technique (uncommitted-competing-row + pg_stat_activity
// poll - see waitForBlockedStatement's own doc comment for exactly where
// this precedent lives in the codebase today). Follows internal/casino's
// own orchestrator_integration_test.go fixture conventions (seedFixture/
// fundWallet/testPool) as closely as this package's narrower scope allows.
package sportsbook

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
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
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

type sbFixture struct {
	tenantID        uuid.UUID
	brandID         uuid.UUID
	playerAccountID uuid.UUID
	walletID        uuid.UUID
}

// seedFixture mirrors internal/casino's seedCasinoFixture exactly
// (tenant/person/brand/player/wallet).
func seedFixture(t *testing.T, pool *db.Pool) sbFixture {
	t.Helper()
	f := sbFixture{tenantID: uuid.New(), brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()

	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence', 'active')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Test Brand', 'active')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, f.tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			id, f.tenantID, f.brandID, f.playerAccountID); err != nil {
			return err
		}
		f.walletID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant rows: %v", err)
	}
	return f
}

// seedSecondPlayer adds a second player_account + wallet into f's SAME
// tenant/brand, returning a new sbFixture sharing tenantID/brandID but
// with its own playerAccountID/walletID - for tests that need two
// distinct players in one tenant (e.g. the cross-player idempotency-key
// regression below), as distinct from the cross-TENANT isolation already
// covered by the HTTP-layer flow tests.
func seedSecondPlayer(t *testing.T, pool *db.Pool, f sbFixture) sbFixture {
	t.Helper()
	second := sbFixture{tenantID: f.tenantID, brandID: f.brandID, playerAccountID: uuid.New()}
	personID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed second player's person row: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			second.playerAccountID, f.tenantID, f.brandID, personID, second.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			id, f.tenantID, f.brandID, second.playerAccountID); err != nil {
			return err
		}
		second.walletID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seed second player: %v", err)
	}
	return second
}

// fundWallet mirrors internal/casino's identical helper: a direct
// manual_adjustment credit to player_cash, simulating a completed deposit
// without depending on internal/payments.
func fundWallet(t *testing.T, pool *db.Pool, f sbFixture, amount int64) {
	t.Helper()
	reason := "test fixture funding"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adjustmentAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fund-" + uuid.New().String(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adjustmentAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fund wallet: %v", err)
	}
}

// seedSelectionParams lets each test control exactly the event/market/
// selection state it needs to exercise (odds, and each status dimension)
// without depending on MockSportsbookProvider's own fixed catalogue.
type seedSelectionParams struct {
	OddsNumerator   int64
	OddsDenominator int64
	EventStatus     EventStatus
	MarketStatus    MarketStatus
	SelectionStatus SelectionStatus
}

func seedSelection(t *testing.T, pool *db.Pool, p seedSelectionParams) Selection {
	t.Helper()
	if p.OddsNumerator == 0 {
		p.OddsNumerator = 200
	}
	if p.OddsDenominator == 0 {
		p.OddsDenominator = 100
	}
	if p.EventStatus == "" {
		p.EventStatus = EventScheduled
	}
	if p.MarketStatus == "" {
		p.MarketStatus = MarketOpen
	}
	if p.SelectionStatus == "" {
		p.SelectionStatus = SelectionActive
	}

	ref := uuid.New().String()[:8]
	var sel Selection
	sel.OddsNumerator = p.OddsNumerator
	sel.OddsDenominator = p.OddsDenominator
	sel.Status = p.SelectionStatus

	// Migration 0084 (ADR 0081): sb_* writes now require a genuinely
	// platform-service-scoped transaction (WithoutTenant is no longer
	// sufficient - see SyncCatalogue's own doc comment).
	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		var sportID, compID, eventID, marketID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $2, 'Test Sport') RETURNING id`,
			"test-sport-"+ref, "test-sport-"+ref).Scan(&sportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ($1, $2, 'Test Competition') RETURNING id`,
			sportID, "test-comp-"+ref).Scan(&compID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_events (competition_id, external_ref, name, start_time, status) VALUES ($1, $2, 'Test Event', $3, $4) RETURNING id`,
			compID, "test-event-"+ref, time.Now().Add(48*time.Hour), string(p.EventStatus)).Scan(&eventID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name, status) VALUES ($1, $2, 'Test Market', $3) RETURNING id`,
			eventID, "test-market-"+ref, string(p.MarketStatus)).Scan(&marketID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator, status)
			 VALUES ($1, $2, 'Test Selection', $3, $4, $5) RETURNING id`,
			marketID, "test-sel-"+ref, p.OddsNumerator, p.OddsDenominator, string(p.SelectionStatus)).Scan(&sel.ID); err != nil {
			return err
		}
		sel.MarketID = marketID
		sel.Name = "Test Selection"
		return nil
	})
	if err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	return sel
}

func placeBet(t *testing.T, pool *db.Pool, f sbFixture, sel Selection, stake int64, idempotencyKey string) (PlaceBetResult, error) {
	t.Helper()
	var result PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: stake,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: idempotencyKey,
		})
		return err
	})
	return result, err
}

func cashBalance(t *testing.T, pool *db.Pool, f sbFixture) int64 {
	t.Helper()
	var signed int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		b, err := ledger.GetProjectedBalance(ctx, tx, accountID)
		if err != nil {
			return err
		}
		signed = b.Signed()
		return nil
	})
	if err != nil {
		t.Fatalf("read cash balance: %v", err)
	}
	return signed
}

func lockedCashBalance(t *testing.T, pool *db.Pool, f sbFixture) int64 {
	t.Helper()
	var signed int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedCash, "EUR")
		if err != nil {
			return err
		}
		b, err := ledger.GetProjectedBalance(ctx, tx, accountID)
		if err != nil {
			return err
		}
		signed = b.Signed()
		return nil
	})
	if err != nil {
		t.Fatalf("read locked cash balance: %v", err)
	}
	return signed
}

func countLedgerTransactions(t *testing.T, pool *db.Pool, f sbFixture) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_bet'`, f.tenantID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	return count
}

func countBets(t *testing.T, pool *db.Pool, f sbFixture) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE tenant_id = $1`, f.tenantID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count bets: %v", err)
	}
	return count
}

func TestPlaceBet_SuccessLocksStakeAndBalancesLedger(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 250, OddsDenominator: 100})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-success-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected bet to be accepted, got rejection %q/%q: %s", result.RejectionCategory, result.RejectionCode, result.RejectionMessage)
	}
	if result.Bet.Status != BetStatusOpen {
		t.Fatalf("expected status open, got %q", result.Bet.Status)
	}
	// 1000 stake * 2.50 odds = 2500 potential return.
	if result.Bet.PotentialReturn != 2_500 {
		t.Fatalf("expected potential_return 2500, got %d", result.Bet.PotentialReturn)
	}

	if got := cashBalance(t, pool, f); got != 9_000 {
		t.Fatalf("expected player_cash 9000 after a 1000 stake on 10000, got %d", got)
	}
	if got := lockedCashBalance(t, pool, f); got != 1_000 {
		t.Fatalf("expected player_locked_cash 1000 after a 1000 stake, got %d", got)
	}

	// Ledger balance invariant: SUM(debits) == SUM(credits) for this
	// transaction's own entries.
	var debitTotal, creditTotal int64
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0), COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			 FROM ledger_entries WHERE ledger_transaction_id = $1`, result.Bet.LedgerTransactionID).Scan(&debitTotal, &creditTotal)
	})
	if err != nil {
		t.Fatalf("read ledger entries: %v", err)
	}
	if debitTotal != creditTotal {
		t.Fatalf("expected balanced entries, got debit=%d credit=%d", debitTotal, creditTotal)
	}
	if debitTotal != 1_000 {
		t.Fatalf("expected entries of 1000, got %d", debitTotal)
	}

	if countBets(t, pool, f) != 1 {
		t.Fatalf("expected exactly 1 bet row")
	}
}

func TestPlaceBet_InsufficientFundsNoLedgerEffectNoBetRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 500)
	sel := seedSelection(t, pool, seedSelectionParams{})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-insufficient-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be rejected for insufficient funds")
	}
	if result.RejectionCategory != RejectionInsufficientFunds {
		t.Fatalf("expected rejection category %q, got %q", RejectionInsufficientFunds, result.RejectionCategory)
	}

	// Atomically verify no ledger effect and no bet row - not merely "the
	// handler returned an error".
	if got := cashBalance(t, pool, f); got != 500 {
		t.Fatalf("expected player_cash unchanged at 500, got %d", got)
	}
	if got := lockedCashBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_locked_cash 0, got %d", got)
	}
	if countLedgerTransactions(t, pool, f) != 0 {
		t.Fatal("expected zero sportsbook_bet ledger transactions")
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero sportsbook_bets rows")
	}
}

func TestPlaceBet_OddsChangedRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 200, OddsDenominator: 100})

	var result PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_000,
			// Stale odds the client believes are still current.
			ExpectedOddsNumerator: 300, ExpectedOddsDenominator: 100,
			IdempotencyKey: "place-odds-changed-1",
		})
		return err
	})
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be rejected for odds changed")
	}
	if result.RejectionCategory != RejectionOddsChanged {
		t.Fatalf("expected rejection category %q, got %q", RejectionOddsChanged, result.RejectionCategory)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows on an odds-changed rejection")
	}
}

func TestPlaceBet_EventFinishedRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{EventStatus: EventFinished})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-event-finished-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be rejected for a finished event")
	}
	if result.RejectionCategory != RejectionEventNotOpen {
		t.Fatalf("expected rejection category %q, got %q", RejectionEventNotOpen, result.RejectionCategory)
	}
	if result.RejectionCode != string(EventFinished) {
		t.Fatalf("expected rejection code %q, got %q", EventFinished, result.RejectionCode)
	}
}

// TestPlaceBet_MarketNotOpenRejected is a Stage 6.1 QA-review regression
// test: this is a SIBLING branch to the event-finished check above, both
// sharing RejectionCategory=RejectionEventNotOpen but distinguished by
// RejectionCode - before this test existed, a refactor that deleted the
// market-status check entirely would have passed the full suite silently
// (only the event-status branch had a test).
func TestPlaceBet_MarketNotOpenRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{MarketStatus: MarketSuspended})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-market-suspended-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be rejected for a suspended market")
	}
	if result.RejectionCategory != RejectionEventNotOpen {
		t.Fatalf("expected rejection category %q, got %q", RejectionEventNotOpen, result.RejectionCategory)
	}
	if result.RejectionCode != string(MarketSuspended) {
		t.Fatalf("expected rejection code %q, got %q", MarketSuspended, result.RejectionCode)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows for a market-suspended rejection")
	}
}

// TestPlaceBet_SelectionNotActiveRejected is the third sibling branch -
// same rationale as TestPlaceBet_MarketNotOpenRejected above.
func TestPlaceBet_SelectionNotActiveRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{SelectionStatus: SelectionSuspended})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-selection-suspended-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be rejected for a suspended selection")
	}
	if result.RejectionCategory != RejectionEventNotOpen {
		t.Fatalf("expected rejection category %q, got %q", RejectionEventNotOpen, result.RejectionCategory)
	}
	if result.RejectionCode != string(SelectionSuspended) {
		t.Fatalf("expected rejection code %q, got %q", SelectionSuspended, result.RejectionCode)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows for a selection-suspended rejection")
	}
}

func TestPlaceBet_RGDeniedForSelfExcludedPlayer(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
		return err
	})
	if err != nil {
		t.Fatalf("self-exclude: %v", err)
	}

	result, err := placeBet(t, pool, f, sel, 1_000, "place-rg-denied-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be denied for a self-excluded player")
	}
	if result.RejectionCategory != RejectionRGDenied {
		t.Fatalf("expected rejection category %q, got %q", RejectionRGDenied, result.RejectionCategory)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows on an RG denial")
	}
	if got := cashBalance(t, pool, f); got != 10_000 {
		t.Fatalf("expected player_cash unchanged at 10000, got %d", got)
	}
}

func createSportsbookRiskRule(t *testing.T, pool *db.Pool, f sbFixture, params risk.CreateRuleParams) risk.Rule {
	t.Helper()
	params.TenantID = &f.tenantID
	if params.CreatedByActorType == "" {
		params.CreatedByActorType = "staff"
	}
	if params.AssetCode == "" && params.ThresholdExponent == nil {
		params.ThresholdExponent = risk.ThresholdExponentOf(2)
	}
	if params.CreatedByActorID == uuid.Nil {
		params.CreatedByActorID = uuid.New()
	}
	var r risk.Rule
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = risk.CreateRule(ctx, tx, params)
		return err
	})
	if err != nil {
		t.Fatalf("create risk rule: %v", err)
	}
	return r
}

func TestPlaceBet_RiskDeniedByMaxAmountRule(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	createSportsbookRiskRule(t, pool, f, risk.CreateRuleParams{
		Product: "sportsbook", Operation: risk.OperationSportsbookBet,
		LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
		Threshold: 500, RuleKind: risk.RuleHardLimit, Action: risk.ActionDeny,
	})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-risk-denied-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected bet to be denied by the max-amount risk rule")
	}
	if result.RejectionCategory != RejectionRiskDenied {
		t.Fatalf("expected rejection category %q, got %q", RejectionRiskDenied, result.RejectionCategory)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows on a risk denial")
	}
	if got := cashBalance(t, pool, f); got != 10_000 {
		t.Fatalf("expected player_cash unchanged at 10000, got %d", got)
	}
}

func TestPlaceBet_IdempotentRetrySameKeyOneEffect(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	first, err := placeBet(t, pool, f, sel, 1_000, "place-idempotent-1")
	if err != nil {
		t.Fatalf("place bet (first): %v", err)
	}
	if !first.Accepted {
		t.Fatalf("expected first attempt to be accepted, got %q", first.RejectionCategory)
	}

	second, err := placeBet(t, pool, f, sel, 1_000, "place-idempotent-1")
	if err != nil {
		t.Fatalf("place bet (retry): %v", err)
	}
	if !second.Accepted {
		t.Fatalf("expected retried attempt to be accepted (idempotent replay), got %q", second.RejectionCategory)
	}
	if second.Bet.ID != first.Bet.ID {
		t.Fatalf("expected the same bet id on retry, got %s vs %s", first.Bet.ID, second.Bet.ID)
	}

	if countBets(t, pool, f) != 1 {
		t.Fatal("expected exactly 1 bet row after a same-key retry")
	}
	if countLedgerTransactions(t, pool, f) != 1 {
		t.Fatal("expected exactly 1 ledger transaction after a same-key retry")
	}
	if got := cashBalance(t, pool, f); got != 9_000 {
		t.Fatalf("expected player_cash 9000 (stake deducted exactly once), got %d", got)
	}
}

// TestPlaceBet_SameIdempotencyKeyDifferentPlayersNeverCollide is a
// regression test for a real P1 an architect review caught before this
// stage's diff was ever committed: findBetByIdempotencyKey/the
// sportsbook_bets unique constraint originally scoped only by
// (tenant_id, idempotency_key), so two different players in the same
// tenant choosing the identical client-side idempotency key string would
// have one player silently receive the other's bet id/selection/stake/
// odds back as their own "accepted" response, with their own stake never
// charged. Fixed by scoping both the lookup and the unique constraint by
// (tenant_id, player_account_id, idempotency_key) - this proves that fix
// holds: same key, two different players, two independent bets, neither
// balance affected by the other's stake.
func TestPlaceBet_SameIdempotencyKeyDifferentPlayersNeverCollide(t *testing.T) {
	pool := testPool(t)
	playerA := seedFixture(t, pool)
	playerB := seedSecondPlayer(t, pool, playerA)
	fundWallet(t, pool, playerA, 10_000)
	fundWallet(t, pool, playerB, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	const sharedKey = "shared-idempotency-key"
	resultA, err := placeBet(t, pool, playerA, sel, 1_000, sharedKey)
	if err != nil {
		t.Fatalf("place bet (player A): %v", err)
	}
	if !resultA.Accepted {
		t.Fatalf("expected player A's bet to be accepted, got %q", resultA.RejectionCategory)
	}

	resultB, err := placeBet(t, pool, playerB, sel, 2_000, sharedKey)
	if err != nil {
		t.Fatalf("place bet (player B, same key, different stake): %v", err)
	}
	if !resultB.Accepted {
		t.Fatalf("expected player B's own bet to be accepted despite the shared key, got %q", resultB.RejectionCategory)
	}
	if resultB.Bet.ID == resultA.Bet.ID {
		t.Fatal("expected player B to get a DIFFERENT bet id than player A, not A's bet returned back to B")
	}
	if resultB.Bet.PlayerAccountID != playerB.playerAccountID {
		t.Fatalf("expected player B's bet to belong to player B, got player_account_id=%s", resultB.Bet.PlayerAccountID)
	}

	if got := cashBalance(t, pool, playerA); got != 9_000 {
		t.Fatalf("expected player A's cash 9000 (only A's own 1000 stake deducted), got %d", got)
	}
	if got := cashBalance(t, pool, playerB); got != 8_000 {
		t.Fatalf("expected player B's cash 8000 (only B's own 2000 stake deducted, never A's), got %d", got)
	}
}

// TestPlaceBet_LedgerIdempotencyKeyIsNamespacedByTypeAndPlayer is a Stage
// 6.1 hardening regression test: the ledger's own idempotency_key column
// is a FLAT namespace across (tenant_id, idempotency_key) shared by EVERY
// transaction type this tenant ever posts (deposits, withdrawals, casino,
// bonus, sportsbook) - see ledger.ErrIdempotencyKeyReused's own doc
// comment. Stage 6's fix prefixed the player's own id onto the raw client
// key (closing the cross-player collision found by architect review), but
// that alone still relied on every OTHER domain's key format happening to
// differ from sportsbook's "playerID:clientKey" shape rather than on an
// explicit, structural separation. This test locks in the stronger,
// self-evidently-scoped format
// "sportsbook_bet:<player_account_id>:<idempotency_key>" so a future edit
// that silently drops the transaction-type prefix fails this test, not
// just a production incident.
func TestPlaceBet_LedgerIdempotencyKeyIsNamespacedByTypeAndPlayer(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	const key = "ledger-namespace-check-1"
	result, err := placeBet(t, pool, f, sel, 1_000, key)
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected the bet to be accepted, got %q", result.RejectionCategory)
	}

	var storedKey string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT idempotency_key FROM ledger_transactions WHERE id = $1`, result.Bet.LedgerTransactionID).Scan(&storedKey)
	})
	if err != nil {
		t.Fatalf("read stored ledger idempotency_key: %v", err)
	}

	want := "sportsbook_bet:" + f.playerAccountID.String() + ":" + key
	if storedKey != want {
		t.Fatalf("expected ledger idempotency_key %q, got %q", want, storedKey)
	}
}

// waitForBlockedCount polls pg_stat_activity - fully deterministic, no
// timing assumption - for at least `want` backends genuinely blocked,
// directly or transitively, by the test's OWN blocker session
// (blockerPID, read with pg_backend_pid() inside the blocker
// transaction). Directive requirement: NEVER a bare sync.WaitGroup/
// time.Sleep barrier.
//
// Scoped to the blocker, not a database-wide count: `go test ./...` runs
// packages in parallel against the same test database, so a bare
// "wait_event_type = 'Lock'" count also sees OTHER packages' lock waits
// and can return before this test's own PlaceBet has reached the lock
// (Stage 10 W0: TestSportsbookJurisdiction_ConcurrentConfigurationRead
// IsConsistent then armed its restriction before PlaceBet's jurisdiction
// gate ran and failed intermittently). The same trap is documented in
// internal/ledger/lockorder_harness_test.go.
//
// Transitive on purpose: rg.EvaluateEligibility takes a transaction-
// scoped advisory lock (pg_advisory_xact_lock keyed on person_id), so of
// two concurrent PlaceBet calls only the FIRST to reach it waits on the
// blocker's wallet_balance_projection row lock - the SECOND waits on the
// first's advisory lock. pg_blocking_pids covers advisory locks, and the
// recursive walk counts both genuine waits.
func waitForBlockedCount(t *testing.T, pool *db.Pool, blockerPID int32, want int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				WITH RECURSIVE waiters(pid) AS (
					SELECT a.pid FROM pg_stat_activity a
					 WHERE $1::int = ANY(pg_blocking_pids(a.pid))
					UNION
					SELECT a.pid FROM pg_stat_activity a
					  JOIN waiters w ON w.pid = ANY(pg_blocking_pids(a.pid))
				)
				SELECT count(*) FROM waiters`, blockerPID).Scan(&count)
		})
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if count >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// backendPID returns the backend pid of the connection running tx, so a
// test can wait for sessions blocked by exactly that blocker. It returns
// an error rather than calling t.Fatalf because blockers usually run in
// their own goroutine.
func backendPID(ctx context.Context, tx pgx.Tx) (int32, error) {
	var pid int32
	err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	return pid, err
}

// TestPlaceBet_ConcurrentPlacementsOnlyOneSucceeds proves two concurrent
// PlaceBet calls against a balance that can only cover one are correctly
// serialized by lockCashBalance's row lock on wallet_balance_projection -
// at most one succeeds, using an uncommitted competing row (a blocker
// transaction holding the SAME FOR UPDATE lock PlaceBet itself takes) to
// force both real calls to genuinely queue on the lock before releasing
// it, rather than relying on scheduler timing.
func TestPlaceBet_ConcurrentPlacementsOnlyOneSucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 1_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	var cashAccountID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("resolve cash account: %v", err)
	}

	var blockerPIDValue int32
	blockerReady := make(chan struct{})
	proceed := make(chan struct{})
	blockerErr := make(chan error, 1)
	go func() {
		blockerErr <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var d, c int64
			if err := tx.QueryRow(ctx,
				`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
				cashAccountID).Scan(&d, &c); err != nil {
				return err
			}
			var err error
			if blockerPIDValue, err = backendPID(ctx, tx); err != nil {
				return err
			}
			close(blockerReady)
			<-proceed
			return nil
		})
	}()
	select {
	case <-blockerReady:
	case err := <-blockerErr:
		t.Fatalf("blocker transaction failed before acquiring its lock: %v", err)
	}

	const n = 2
	results := make([]PlaceBetResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = placeBet(t, pool, f, sel, 1_000, fmt.Sprintf("place-concurrent-%d", i))
		}(i)
	}

	if !waitForBlockedCount(t, pool, blockerPIDValue, 2) {
		close(proceed)
		wg.Wait()
		t.Fatal("timed out waiting for both concurrent PlaceBet calls to block on the uncommitted blocker row (pg_stat_activity never reported 2 backends in a Lock wait)")
	}
	close(proceed)
	wg.Wait()

	if err := <-blockerErr; err != nil {
		t.Fatalf("blocker transaction: %v", err)
	}

	var accepted, rejected int
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if results[i].Accepted {
			accepted++
		} else {
			if results[i].RejectionCategory != RejectionInsufficientFunds {
				t.Fatalf("goroutine %d: expected insufficient_funds rejection, got %q", i, results[i].RejectionCategory)
			}
			rejected++
		}
	}
	if accepted != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent placements to succeed against a balance that covers only 1, got %d", n, accepted)
	}
	if rejected != n-1 {
		t.Fatalf("expected %d rejections, got %d", n-1, rejected)
	}
	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_cash 0 after exactly one 1000 stake on 1000, got %d", got)
	}
	if countBets(t, pool, f) != 1 {
		t.Fatal("expected exactly 1 bet row")
	}
}

func TestPlaceBet_SelectionNotFound(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)

	_, err := placeBet(t, pool, f, Selection{ID: uuid.New(), OddsNumerator: 200, OddsDenominator: 100}, 1_000, "place-not-found-1")
	if !errors.Is(err, ErrSelectionNotFound) {
		t.Fatalf("expected ErrSelectionNotFound, got %v", err)
	}
}
