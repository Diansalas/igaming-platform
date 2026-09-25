//go:build integration

// Stage 10 W1 (ADR 0088 §6.2, §14 items R1 and R4): cumulative sportsbook
// usage measured against the REAL postings of the whole bet lifecycle.
//
// Why a separate file in the EXTERNAL test package risk_test (and not an
// extension of cumulative_sportsbook_integration_test.go, which is
// package risk): internal/sportsbook imports internal/risk, so an
// internal (package risk) test importing internal/sportsbook would be an
// import cycle. An external test package is a distinct package that may
// import both - the go tool compiles internal/sportsbook against the
// test build of internal/risk, and nothing imports risk_test. The price
// is that only EXPORTED risk API is usable here; usage is therefore read
// through risk.Evaluate itself (see assertWindowUsage), which is the very
// code path PlaceBet consults - no test-only export was added.
//
// Every posting below is produced by sportsbook.PlaceBet and
// sportsbook.SimulateSettlementEvent - the in-house MOCK settlement
// driver (ADR 0088; NOT a real provider integration). Nothing in this
// file hand-builds a ledger posting.
package risk_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const (
	sbAsset = "EUR"
	// sbProbeThreshold is the rolling-hour hard cap used by the §6.2 rows.
	// Large enough never to bind a PlaceBet in these scenarios, so its
	// only job is to let assertWindowUsage read usage back exactly.
	sbProbeThreshold int64 = 1_000_000_000
	sbFunding        int64 = 10_000_000
)

type sbRiskFixture struct {
	tenantID uuid.UUID
	brandID  uuid.UUID
	playerID uuid.UUID
	walletID uuid.UUID
	staffID  uuid.UUID
	sel      sbSelection
}

type sbSelection struct {
	id       uuid.UUID
	oddsNum  int64
	oddsDen  int64
	marketID uuid.UUID
}

func sbConnect(t *testing.T, url string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func sbSharedPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return sbConnect(t, url)
}

// seedSBRiskFixture builds tenant/person/brand/player/wallet (the same
// chain internal/sportsbook's own seedFixture builds), funds the wallet,
// creates an ACTIVE risk_manager staff user (the only role granted
// PermSportsbookSettlementSimulate; SimulateSettlementEvent also checks
// the actor is an active staff user of this tenant), seeds one open
// selection, and arms one rolling-hour hard cumulative_amount rule for
// sportsbook_bet at threshold.
func seedSBRiskFixture(t *testing.T, pool *db.Pool, threshold int64) sbRiskFixture {
	t.Helper()
	ctx := context.Background()
	f := sbRiskFixture{tenantID: uuid.New(), brandID: uuid.New(), playerID: uuid.New(), walletID: uuid.New()}
	personID := uuid.New()

	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	reason := "test fixture funding"
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerID, f.tenantID, f.brandID, personID, f.playerID.String()+"@example.com"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			f.walletID, f.tenantID, f.brandID, f.playerID, sbAsset); err != nil {
			return err
		}
		// Funding is a manual_adjustment - not a sportsbook type, so it is
		// invisible to the sportsbook_bet measure by construction.
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, sbAsset)
		if err != nil {
			return err
		}
		adj, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, sbAsset)
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fund-" + uuid.New().String(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adj, Direction: ledger.Debit, Amount: sbFunding},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: sbFunding},
			},
		}); err != nil {
			return err
		}
		staff, err := identity.CreateStaffUser(ctx, tx, f.tenantID, "rm-"+uuid.NewString()[:8]+"@example.test", "x", identity.StaffRoleRiskManager, nil)
		if err != nil {
			return err
		}
		f.staffID = staff.ID
		_, err = risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &f.tenantID, Operation: risk.OperationSportsbookBet, LimitKind: risk.LimitCumulativeAmount,
			TimeWindow: risk.WindowRollingHour, AssetCode: sbAsset, Threshold: threshold, RuleKind: risk.RuleHardLimit,
			CreatedByActorType: "staff", CreatedByActorID: f.staffID,
		})
		return err
	}); err != nil {
		t.Fatalf("seed tenant rows: %v", err)
	}

	f.sel = seedSBSelection(t, pool)
	return f
}

// seedSBSelection mirrors internal/sportsbook's seedSelection (defaults:
// odds 200/100, scheduled event, open market, active selection).
func seedSBSelection(t *testing.T, pool *db.Pool) sbSelection {
	t.Helper()
	sel := sbSelection{oddsNum: 200, oddsDen: 100}
	ref := uuid.New().String()[:8]
	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		var sportID, compID, eventID uuid.UUID
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
			compID, "test-event-"+ref, time.Now().Add(48*time.Hour), string(sportsbook.EventScheduled)).Scan(&eventID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name, status) VALUES ($1, $2, 'Test Market', $3) RETURNING id`,
			eventID, "test-market-"+ref, string(sportsbook.MarketOpen)).Scan(&sel.marketID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator, status)
			 VALUES ($1, $2, 'Test Selection', $3, $4, $5) RETURNING id`,
			sel.marketID, "test-sel-"+ref, sel.oddsNum, sel.oddsDen, string(sportsbook.SelectionActive)).Scan(&sel.id)
	})
	if err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	return sel
}

// placeSBBet drives sportsbook.PlaceBet (which itself runs risk.Evaluate
// against the armed rule) and returns the result, accepted or not.
func placeSBBet(t *testing.T, pool *db.Pool, f sbRiskFixture, stake int64) sportsbook.PlaceBetResult {
	t.Helper()
	var res sportsbook.PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = sportsbook.PlaceBet(ctx, tx, sportsbook.PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, WalletID: f.walletID,
			SelectionID: f.sel.id, AssetCode: sbAsset, StakeAmount: stake,
			ExpectedOddsNumerator: f.sel.oddsNum, ExpectedOddsDenominator: f.sel.oddsDen,
			IdempotencyKey: "risk-cum-" + uuid.NewString(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("PlaceBet: %v", err)
	}
	return res
}

func mustPlaceSBBet(t *testing.T, pool *db.Pool, f sbRiskFixture, stake int64) sportsbook.Bet {
	t.Helper()
	res := placeSBBet(t, pool, f, stake)
	if !res.Accepted {
		t.Fatalf("expected PlaceBet(stake=%d) to be accepted, got %s/%s: %s", stake, res.RejectionCategory, res.RejectionCode, res.RejectionMessage)
	}
	return res.Bet
}

// sbStep is one simulated settlement event applied to the row's bet.
type sbStep struct {
	name       string
	event      sportsbook.SettlementEventType
	generation int
	outcome    string // settle only
}

func settleStep(g int, outcome string) sbStep {
	return sbStep{name: fmt.Sprintf("settle(g%d,%s)", g, outcome), event: sportsbook.SettlementEventSettle, generation: g, outcome: outcome}
}

func rollbackStep(g int) sbStep {
	return sbStep{name: fmt.Sprintf("rollback(g%d)", g), event: sportsbook.SettlementEventRollback, generation: g}
}

func voidStep() sbStep {
	return sbStep{name: "void", event: sportsbook.SettlementEventVoid}
}

func applySBStep(t *testing.T, pool *db.Pool, f sbRiskFixture, bet sportsbook.Bet, s sbStep) sportsbook.SettlementResult {
	t.Helper()
	ev := sportsbook.SettlementEvent{
		TenantID: f.tenantID, BetID: bet.ID, ActorStaffID: f.staffID, EventType: s.event,
		RequestID: "risk-cum-" + uuid.NewString(),
	}
	switch s.event {
	case sportsbook.SettlementEventSettle:
		ev.Generation, ev.Outcome, ev.ClaimAssetCode = s.generation, s.outcome, bet.AssetCode
		if s.outcome == sportsbook.SettlementOutcomeWon {
			ev.ClaimPayoutAmount = bet.PotentialReturn
		}
	case sportsbook.SettlementEventRollback:
		ev.Generation = s.generation
	case sportsbook.SettlementEventVoid:
		ev.VoidReason = "market_cancelled"
	}
	var res sportsbook.SettlementResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = sportsbook.SimulateSettlementEvent(ctx, tx, ev)
		return err
	})
	if err != nil {
		t.Fatalf("%s: SimulateSettlementEvent: %v", s.name, err)
	}
	if res.Rejected() || res.Result != sportsbook.SettlementResultApplied {
		t.Fatalf("%s: expected an applied event, got result=%q rejection=%q", s.name, res.Result, res.RejectionCode)
	}
	return res
}

// evaluateSB runs risk.Evaluate for a sportsbook_bet of amount - the same
// call PlaceBet makes before posting.
func evaluateSB(t *testing.T, pool *db.Pool, f sbRiskFixture, amount int64) risk.RiskDecision {
	t.Helper()
	var d risk.RiskDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = risk.Evaluate(ctx, tx, risk.RiskRequest{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID,
			Operation: risk.OperationSportsbookBet, AssetCode: sbAsset, Amount: amount,
			LicensingMode: "under_platform_licence",
		})
		return err
	})
	if err != nil {
		t.Fatalf("risk.Evaluate(amount=%d): %v", amount, err)
	}
	return d
}

// assertWindowUsage pins the rolling-hour cumulative usage to EXACTLY want,
// read through risk.Evaluate alone. The rule DENIES when usage + amount >
// threshold, so with the fixture's single armed rule at threshold:
// amount = threshold-want must ALLOW and threshold-want+1 must DENY - the
// only usage value consistent with both outcomes is want.
func assertWindowUsage(t *testing.T, pool *db.Pool, f sbRiskFixture, threshold, want int64, label string) {
	t.Helper()
	fits := threshold - want
	if fits < 1 {
		t.Fatalf("%s: probe threshold %d cannot reveal usage %d", label, threshold, want)
	}
	if d := evaluateSB(t, pool, f, fits); d.Outcome != risk.OutcomeAllow {
		t.Fatalf("%s: expected window usage %d (amount %d fits under %d), but Evaluate returned %s - usage is higher than %d",
			label, want, fits, threshold, d.Outcome, want)
	}
	if d := evaluateSB(t, pool, f, fits+1); d.Outcome != risk.OutcomeDeny {
		t.Fatalf("%s: expected window usage %d (amount %d exceeds %d), but Evaluate returned %s - usage is lower than %d",
			label, want, fits+1, threshold, d.Outcome, want)
	}
}

// rollbackCashDebits returns the total player_cash DEBIT posted by the
// bet's sportsbook_rollback transactions - the P that a won settlement's
// rollback takes back from the player, and that the measure must ignore.
func rollbackCashDebits(t *testing.T, pool *db.Pool, f sbRiskFixture, betID uuid.UUID) int64 {
	t.Helper()
	var total int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(le.amount), 0)::bigint
			 FROM ledger_entries le
			 JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id
			 JOIN ledger_accounts la ON la.id = le.ledger_account_id
			 WHERE lt.correlation_id = $1 AND lt.transaction_type = $2
			   AND la.account_type = $3 AND le.direction = $4`,
			betID, string(ledger.TxSportsbookRollback), string(ledger.AccountPlayerCash), string(ledger.Debit)).Scan(&total)
	})
	if err != nil {
		t.Fatalf("read rollback player_cash debits: %v", err)
	}
	return total
}

// TestSportsbookCumulative_EveryADR0088NettingRow is ADR 0088 §14 item R1:
// one sub-test per §6.2 row (plus the won/lost variants §6.2 folds into
// one row), each with its own tenant so usage starts at zero. Usage is
// asserted after placement and after EVERY subsequent event, so an
// intermediate step that leaked into the measure is caught even where
// the final value happens to coincide.
func TestSportsbookCumulative_EveryADR0088NettingRow(t *testing.T) {
	pool := sbSharedPool(t)
	const stake int64 = 1000
	const S = stake

	rows := []struct {
		name  string // §6.2 row
		steps []sbStep
		// want[i] is the usage after steps[i]; usage after placement is
		// always +S and asserted separately.
		want []int64
		// wonRollbackPosted marks rows where a WON settlement is rolled
		// back, so a Dr player_cash P rollback leg must exist in the ledger
		// for the exclusion assertion to mean anything.
		wonRollbackPosted bool
	}{
		{name: "place => +S", steps: nil, want: nil},
		{name: "place -> settle won => +S", steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeWon)}, want: []int64{S}},
		{name: "place -> settle lost => +S", steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeLost)}, want: []int64{S}},
		{name: "place -> void-before => 0", steps: []sbStep{voidStep()}, want: []int64{0}},
		{
			name:  "place -> settle won -> void-after (rollback + void) => 0",
			steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeWon), voidStep()}, want: []int64{S, 0},
			wonRollbackPosted: true,
		},
		{
			name:  "place -> settle lost -> void-after (rollback + void) => 0",
			steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeLost), voidStep()}, want: []int64{S, 0},
		},
		{
			name:  "place -> settle won -> rollback => +S",
			steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeWon), rollbackStep(1)}, want: []int64{S, S},
			wonRollbackPosted: true,
		},
		{
			name:  "place -> settle lost -> rollback => +S",
			steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeLost), rollbackStep(1)}, want: []int64{S, S},
		},
		{
			name:  "place -> settle won -> rollback -> void => 0",
			steps: []sbStep{settleStep(1, sportsbook.SettlementOutcomeWon), rollbackStep(1), voidStep()}, want: []int64{S, S, 0},
			wonRollbackPosted: true,
		},
		{
			name: "place -> settle won -> rollback -> re-settle lost => +S",
			steps: []sbStep{
				settleStep(1, sportsbook.SettlementOutcomeWon), rollbackStep(1), settleStep(2, sportsbook.SettlementOutcomeLost),
			},
			want:              []int64{S, S, S},
			wonRollbackPosted: true,
		},
		{
			name: "place -> settle lost -> rollback -> re-settle won => +S",
			steps: []sbStep{
				settleStep(1, sportsbook.SettlementOutcomeLost), rollbackStep(1), settleStep(2, sportsbook.SettlementOutcomeWon),
			},
			want: []int64{S, S, S},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := seedSBRiskFixture(t, pool, sbProbeThreshold)
			assertWindowUsage(t, pool, f, sbProbeThreshold, 0, "before placement")

			bet := mustPlaceSBBet(t, pool, f, stake)
			if bet.PotentialReturn <= stake {
				t.Fatalf("fixture odds must make a won payout P exceed S (P=%d, S=%d) so an unexcluded P could not coincide with a netting value", bet.PotentialReturn, stake)
			}
			assertWindowUsage(t, pool, f, sbProbeThreshold, S, "after place")

			for i, s := range row.steps {
				applySBStep(t, pool, f, bet, s)
				assertWindowUsage(t, pool, f, sbProbeThreshold, row.want[i], "after "+s.name)
			}

			if row.wonRollbackPosted {
				// Negative control: the rollback of the WON settlement really
				// did post Dr player_cash P. A measure that counted
				// sportsbook_rollback would have read S+P (or P) instead of
				// the values asserted above.
				if got := rollbackCashDebits(t, pool, f, bet.ID); got != bet.PotentialReturn {
					t.Fatalf("expected the won settlement's rollback to post Dr player_cash %d, found %d", bet.PotentialReturn, got)
				}
			}
		})
	}
}

// TestSportsbookCumulative_VoidInsideWindowOfBetPlacedBeforeWindow_PinsNetOutflowSemantics
// is ADR 0088 §14 item R4 (Orchestrator ruling R-3, OI-5 CLOSED as named
// debt): cumulative limits measure NET OUTFLOW BY POSTING TIME, not gross
// stakes placed in the window. A bet placed before the window start and
// voided inside it contributes -S to the window, so a player can stake
// 2S gross inside one window under a cap of S. This test PINS that
// behaviour; it is not an endorsement. If product/compliance rules that
// limits must be gross-by-placement this becomes P1 for casino and
// sportsbook (ADR 0088 §6.2) and this test must change together with the
// netting query - never alone.
//
// Placing a bet "before the window" needs a ledger posting whose
// created_at is more than one rolling hour old. ledger_entries/
// ledger_transactions are append-only (deny triggers), so rows cannot be
// backdated after the fact without disabling those triggers, and the
// evaluator's window start is the Go wall clock (no injectable clock).
// The least invasive deterministic fixture is therefore to run in a
// THROWAWAY, fully-migrated scratch database (internal/testsupport/
// scratchdb) owned by the test role, and - only for the one placement -
// shift the created_at column DEFAULT two hours back. No trigger is
// disabled, no row is ever UPDATEd (append-only holds throughout), the
// placement is still produced by sportsbook.PlaceBet unmodified, and the
// shared test database's schema is never touched. Skipped when
// TEST_ADMIN_DATABASE_URL is unset (scratchdb's own contract).
func TestSportsbookCumulative_VoidInsideWindowOfBetPlacedBeforeWindow_PinsNetOutflowSemantics(t *testing.T) {
	scratchURL := scratchdb.New(t, "risk_r4_")
	pool := sbConnect(t, scratchURL)
	if _, err := pool.MigrateUp(context.Background(), "../../migrations"); err != nil {
		t.Fatalf("migrate scratch database: %v", err)
	}

	const S int64 = 1000
	// The cap: S per rolling hour. Under a gross-by-placement measure
	// exactly one S-stake could be placed inside the window.
	f := seedSBRiskFixture(t, pool, S)

	setCreatedAtDefault := func(expr string) {
		t.Helper()
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			for _, table := range []string{"ledger_transactions", "ledger_entries"} {
				if _, err := tx.Exec(ctx, `ALTER TABLE `+table+` ALTER COLUMN created_at SET DEFAULT `+expr); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("set ledger created_at default to %s: %v", expr, err)
		}
	}

	// B1: placed "two hours ago" - outside the rolling hour.
	setCreatedAtDefault(`(now() - interval '2 hours')`)
	b1 := mustPlaceSBBet(t, pool, f, S)
	setCreatedAtDefault(`now()`)

	// Prove the fixture did what it claims: every ledger row of B1's
	// placement is older than the window, so the pin below cannot pass
	// for a trivial reason.
	var newest time.Time
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT MAX(le.created_at) FROM ledger_entries le
			 JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id
			 WHERE lt.id = $1`, b1.LedgerTransactionID).Scan(&newest)
	}); err != nil {
		t.Fatalf("read B1 placement created_at: %v", err)
	}
	if !newest.Before(time.Now().Add(-90 * time.Minute)) {
		t.Fatalf("fixture failure: B1's placement entries (newest %s) are not outside the rolling hour", newest)
	}
	assertWindowUsage(t, pool, f, S, 0, "B1 placed before the window")

	// Void B1 inside the window: the void's Cr player_cash S is posted now.
	applySBStep(t, pool, f, b1, voidStep())
	assertWindowUsage(t, pool, f, S, -S, "B1 voided inside the window")

	// Two fresh S stakes are both admitted by PlaceBet under a cap of S -
	// 2S gross placed inside one window. The second is the one a
	// gross-by-placement cap would refuse (window already holds S gross).
	mustPlaceSBBet(t, pool, f, S)
	assertWindowUsage(t, pool, f, S, 0, "B2 placed")
	mustPlaceSBBet(t, pool, f, S)

	// Net window usage is now exactly the cap (0 pinned above + B3's S),
	// which assertWindowUsage cannot probe (no positive amount fits), so
	// the cap is shown to still bind on net outflow through PlaceBet: a
	// further stake of 1 is refused.
	if res := placeSBBet(t, pool, f, 1); res.Accepted || res.RejectionCategory != sportsbook.RejectionRiskDenied {
		t.Fatalf("expected the next stake to be risk-denied once net window usage reaches the cap, got accepted=%v category=%q", res.Accepted, res.RejectionCategory)
	}
}
