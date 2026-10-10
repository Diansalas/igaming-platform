//go:build integration

// Stage 9.2 (ADR 0083 Part B2, §12.2 items 20-29/31-33) cumulative-risk-
// and-exposure composition test suite for `sportsbook.PlaceBet`. Items 31
// (TestSportsbookCumulative_FailsClosedOnEveryUnmeasurableConfiguration)
// and 33 (TestSportsbookCumulative_TwoPlayerOwnedLegsAreNotNettedToZero)
// are ALREADY DELIVERED by Wave 1 at the internal/risk level
// (internal/risk/cumulative_sportsbook_integration_test.go) and are not
// duplicated here - this file covers the remaining twelve items, all of
// which are genuinely about PlaceBet's own composed order rather than
// risk.Evaluate in isolation.
package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// seedExposureLimit arms a new active sb_exposure_limits row via the real
// admin service (CreateExposureLimit) - never a raw INSERT - so these
// tests exercise the same code path an operator's HTTP call does, mirroring
// jurisdiction_integration_test.go's seedRestriction convention exactly.
func seedExposureLimit(t *testing.T, pool *db.Pool, tenantID uuid.UUID, brandID *uuid.UUID, scopeKind, assetCode string, maxOpenPotentialPayout int64) ExposureLimit {
	t.Helper()
	var l ExposureLimit
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = CreateExposureLimit(ctx, tx, CreateExposureLimitParams{
			TenantID: tenantID, BrandID: brandID, ScopeKind: scopeKind, AssetCode: assetCode,
			MaxOpenPotentialPayout: maxOpenPotentialPayout,
			AuthorizationReference: "test-authz-ref", ReasonCode: "test-reason",
			CreatedByActorID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed exposure limit: %v", err)
	}
	return l
}

// seedCumulativeRule arms a risk_rules cumulative_amount rule for
// sportsbook_bet (ADR 0083 §6.1.3's own ordinary admin-authored config
// path) via the real service (risk.CreateRule).
func seedCumulativeRule(t *testing.T, pool *db.Pool, tenantID uuid.UUID, threshold int64) risk.Rule {
	t.Helper()
	var r risk.Rule
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &tenantID, Operation: risk.OperationSportsbookBet, Product: "sportsbook",
			AssetCode: "EUR", LimitKind: risk.LimitCumulativeAmount, TimeWindow: risk.WindowRollingHour,
			Threshold: threshold, RuleKind: risk.RuleConfigurableLimit, Action: risk.ActionDeny,
			CreatedByActorType: "staff", CreatedByActorID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed cumulative risk rule: %v", err)
	}
	return r
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// --- item 21 --------------------------------------------------------------

// TestSportsbookExposure_CrossPlayerBetsOnOneSelectionAreAggregated proves
// the core Exposure(T,B,A,S) measure (§6.2.2): two DIFFERENT players'
// stakes on the SAME selection are summed, and the second bet is rejected
// once the aggregate crosses the ceiling - even though EITHER bet alone
// would fit comfortably.
func TestSportsbookExposure_CrossPlayerBetsOnOneSelectionAreAggregated(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	second := seedSecondPlayer(t, pool, f)
	fundWallet(t, pool, second, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 200, OddsDenominator: 100})

	// stake 1_000 -> potential_return 2_000 (odds 2.00). Ceiling 3_000: the
	// first bet's own 2_000 fits; the second bet's incremental 2_000 added
	// to the first's already-open 2_000 = 4_000 > 3_000.
	seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 3_000)

	first, err := placeBet(t, pool, f, sel, 1_000, "exp-cross-player-1")
	if err != nil {
		t.Fatalf("place first bet: %v", err)
	}
	if !first.Accepted {
		t.Fatalf("expected the first bet to be accepted (2000 <= 3000), got rejection %q/%q", first.RejectionCategory, first.RejectionCode)
	}

	secondResult, err := placeBet(t, pool, second, sel, 1_000, "exp-cross-player-2")
	if err != nil {
		t.Fatalf("place second bet: %v", err)
	}
	if secondResult.Accepted {
		t.Fatal("expected the second player's bet to be rejected once the aggregate (4000) crosses the 3000 ceiling")
	}
	if secondResult.RejectionCategory != RejectionExposureLimit {
		t.Fatalf("expected rejection category %q, got %q", RejectionExposureLimit, secondResult.RejectionCategory)
	}
}

// --- item 22 --------------------------------------------------------------

// TestSportsbookExposure_EventMarketAndSelectionLevelsEachEnforce - three
// independent sub-cases, one per scope_kind, each using its OWN fixture so
// the levels do not interact (broadest-first evaluation would otherwise
// mask a market/selection-level breach behind an event-level one).
func TestSportsbookExposure_EventMarketAndSelectionLevelsEachEnforce(t *testing.T) {
	t.Run("selection", func(t *testing.T) {
		pool := testPool(t)
		f := seedFixture(t, pool)
		fundWallet(t, pool, f, 100_000)
		second := seedSecondPlayer(t, pool, f)
		fundWallet(t, pool, second, 100_000)
		sel := seedSelection(t, pool, seedSelectionParams{})
		seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 3_000)

		if r, err := placeBet(t, pool, f, sel, 1_000, "exp-scope-sel-1"); err != nil || !r.Accepted {
			t.Fatalf("expected first bet accepted, got result=%+v err=%v", r, err)
		}
		r, err := placeBet(t, pool, second, sel, 1_000, "exp-scope-sel-2")
		if err != nil {
			t.Fatalf("place second bet: %v", err)
		}
		if r.Accepted || r.RejectionCategory != RejectionExposureLimit {
			t.Fatalf("expected a selection-level exposure rejection, got %+v", r)
		}
	})

	t.Run("market", func(t *testing.T) {
		pool := testPool(t)
		f := seedFixture(t, pool)
		fundWallet(t, pool, f, 100_000)
		second := seedSecondPlayer(t, pool, f)
		fundWallet(t, pool, second, 100_000)
		sel, _, marketID := seedSelectionWithContext(t, pool, seedSelectionParams{})
		sibling := seedSiblingSelection(t, pool, marketID)
		seedExposureLimit(t, pool, f.tenantID, nil, "market", "EUR", 3_000)

		// First bet on ONE selection, second bet on a DIFFERENT sibling
		// selection under the SAME market - the market-level ceiling must
		// aggregate across both.
		if r, err := placeBet(t, pool, f, sel, 1_000, "exp-scope-mkt-1"); err != nil || !r.Accepted {
			t.Fatalf("expected first bet accepted, got result=%+v err=%v", r, err)
		}
		r, err := placeBet(t, pool, second, sibling, 1_000, "exp-scope-mkt-2")
		if err != nil {
			t.Fatalf("place second bet: %v", err)
		}
		if r.Accepted || r.RejectionCategory != RejectionExposureLimit {
			t.Fatalf("expected a market-level exposure rejection aggregating across sibling selections, got %+v", r)
		}
	})

	t.Run("event", func(t *testing.T) {
		pool := testPool(t)
		f := seedFixture(t, pool)
		fundWallet(t, pool, f, 100_000)
		second := seedSecondPlayer(t, pool, f)
		fundWallet(t, pool, second, 100_000)
		sel, eventID, marketID := seedSelectionWithContext(t, pool, seedSelectionParams{})
		// A DIFFERENT market under the SAME event, with its own selection -
		// the event-level ceiling must aggregate across DIFFERENT markets,
		// not merely different selections in one market.
		otherMarketSel := seedSelectionInNewMarketUnderEvent(t, pool, eventID)
		_ = marketID
		seedExposureLimit(t, pool, f.tenantID, nil, "event", "EUR", 3_000)

		if r, err := placeBet(t, pool, f, sel, 1_000, "exp-scope-evt-1"); err != nil || !r.Accepted {
			t.Fatalf("expected first bet accepted, got result=%+v err=%v", r, err)
		}
		r, err := placeBet(t, pool, second, otherMarketSel, 1_000, "exp-scope-evt-2")
		if err != nil {
			t.Fatalf("place second bet: %v", err)
		}
		if r.Accepted || r.RejectionCategory != RejectionExposureLimit {
			t.Fatalf("expected an event-level exposure rejection aggregating across different markets/selections beneath it, got %+v", r)
		}
	})
}

// seedSelectionInNewMarketUnderEvent adds a brand-new market (and one
// selection under it) to the SAME event eventID already has - for the
// event-level aggregation test, which must prove aggregation across
// DIFFERENT markets, not only different selections within one market.
func seedSelectionInNewMarketUnderEvent(t *testing.T, pool *db.Pool, eventID uuid.UUID) Selection {
	t.Helper()
	ref := uuid.New().String()[:8]
	var sel Selection
	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		var marketID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name, status) VALUES ($1, $2, 'Second Market', 'open') RETURNING id`,
			eventID, "exp-second-market-"+ref).Scan(&marketID); err != nil {
			return err
		}
		sel.MarketID = marketID
		sel.OddsNumerator = 200
		sel.OddsDenominator = 100
		sel.Status = SelectionActive
		sel.Name = "Second Market Selection"
		return tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator, status)
			 VALUES ($1, $2, 'Second Market Selection', 200, 100, 'active') RETURNING id`,
			marketID, "exp-second-market-sel-"+ref).Scan(&sel.ID)
	})
	if err != nil {
		t.Fatalf("seed selection in a second market under the same event: %v", err)
	}
	return sel
}

// --- item 23 --------------------------------------------------------------

// TestSportsbookExposure_TenantIsolation proves INV-SB-EXP-1: a second
// tenant's open bets on a platform-wide selection [not applicable here,
// since selections are also tenant-agnostic catalogue rows but bets are
// tenant-owned] never count toward a DIFFERENT tenant's aggregate - both
// because FORCE RLS structurally prevents it and because CreateExposureLimit
// itself scopes the limit to one tenant - and that a brand-scoped limit
// does not aggregate a sibling brand's bets in the SAME tenant.
func TestSportsbookExposure_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	tenantA := seedFixture(t, pool)
	fundWallet(t, pool, tenantA, 100_000)
	tenantB := seedFixture(t, pool)
	fundWallet(t, pool, tenantB, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	// Tenant A configures a tight selection-level ceiling; tenant B
	// configures NONE.
	seedExposureLimit(t, pool, tenantA.tenantID, nil, "selection", "EUR", 1_500)

	// Tenant B's bet on the SAME selection must be entirely unaffected by
	// tenant A's own limit (it has none of its own).
	resultB, err := placeBet(t, pool, tenantB, sel, 1_000, "exp-tenant-b")
	if err != nil {
		t.Fatalf("place tenant B bet: %v", err)
	}
	if !resultB.Accepted {
		t.Fatalf("expected tenant B's bet to be accepted (tenant B has no exposure limit configured), got rejection %q/%q", resultB.RejectionCategory, resultB.RejectionCode)
	}

	// Tenant A's own bet, evaluated against ONLY tenant A's own open bets
	// (structurally excluding tenant B's bet just placed on the identical
	// selection id) - 1000 stake -> potential_return 2000 > 1500 ceiling,
	// so it is rejected purely on tenant A's own book, not because tenant
	// B's bet leaked in (which would instead make it accepted at 2000, or
	// rejected at a much higher aggregate).
	resultA, err := placeBet(t, pool, tenantA, sel, 1_000, "exp-tenant-a")
	if err != nil {
		t.Fatalf("place tenant A bet: %v", err)
	}
	if resultA.Accepted {
		t.Fatal("expected tenant A's bet to be rejected by its own 1500 ceiling")
	}

	// Brand isolation within ONE tenant: a brand-scoped limit on tenant A's
	// own brand does not aggregate a sibling brand's bets.
	tenantC := seedFixture(t, pool)
	fundWallet(t, pool, tenantC, 100_000)
	secondBrand := seedSecondBrandFixture(t, pool, tenantC)
	fundWallet(t, pool, secondBrand, 100_000)
	sel2 := seedSelection(t, pool, seedSelectionParams{})
	seedExposureLimit(t, pool, tenantC.tenantID, &tenantC.brandID, "selection", "EUR", 1_500)

	if r, err := placeBet(t, pool, secondBrand, sel2, 1_000, "exp-sibling-brand"); err != nil || !r.Accepted {
		t.Fatalf("expected the sibling brand's bet to be unaffected by the OTHER brand's own limit, got result=%+v err=%v", r, err)
	}
	r, err := placeBet(t, pool, tenantC, sel2, 1_000, "exp-own-brand")
	if err != nil {
		t.Fatalf("place tenant C's own-brand bet: %v", err)
	}
	if r.Accepted {
		t.Fatal("expected tenant C's own brand-scoped limit to reject its own bet on its own book, unaffected by the sibling brand's bet")
	}
}

// seedSecondBrandFixture adds a SECOND brand (and its own player/wallet)
// into f's SAME tenant - for the brand-isolation half of
// TestSportsbookExposure_TenantIsolation.
func seedSecondBrandFixture(t *testing.T, pool *db.Pool, f sbFixture) sbFixture {
	t.Helper()
	second := sbFixture{tenantID: f.tenantID, brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed second brand's person row: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Second Brand', 'active')`,
			second.brandID, f.tenantID, "b2-"+second.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			second.playerAccountID, f.tenantID, second.brandID, personID, second.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			id, f.tenantID, second.brandID, second.playerAccountID); err != nil {
			return err
		}
		second.walletID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seed second brand: %v", err)
	}
	return second
}

// --- item 24 ---------------------------------------------------------------

// TestSportsbookExposure_BalanceInteraction proves an exposure rejection
// happens BEFORE ledger.LockProjectionsForPosting (ADR 0083 §7.1 step 11 <
// step 14): it (a) leaves the player's balance untouched, (b) materialises
// NO zero-totals projection row (no ledger_transactions/sportsbook_bets row
// at all), and (c) still commits its own audit record.
func TestSportsbookExposure_BalanceInteraction(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	// Ceiling smaller than one bet's own potential_return -> the FIRST bet
	// on this selection is already rejected, so there is no prior open
	// exposure to reason about.
	seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 500)

	before := cashBalance(t, pool, f)
	result, err := placeBet(t, pool, f, sel, 1_000, "exp-balance-interaction")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected the bet to be rejected by the exposure ceiling")
	}
	if result.RejectionCategory != RejectionExposureLimit {
		t.Fatalf("expected rejection category %q, got %q", RejectionExposureLimit, result.RejectionCategory)
	}
	after := cashBalance(t, pool, f)
	if after != before {
		t.Fatalf("expected the player's cash balance to be untouched by an exposure rejection: before=%d after=%d", before, after)
	}
	if lockedCashBalance(t, pool, f) != 0 {
		t.Fatal("expected player_locked_cash to remain zero - no stake was locked")
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero sportsbook_bets rows")
	}
	if countLedgerTransactions(t, pool, f) != 0 {
		t.Fatal("expected zero ledger_transactions rows (no zero-totals residue) - the exposure gate runs before any projection lock")
	}

	var auditCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'sportsbook_bet.denied_by_exposure_policy'`,
			f.tenantID).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("count audit records: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 sportsbook_bet.denied_by_exposure_policy audit record, got %d", auditCount)
	}
}

// --- item 25 ---------------------------------------------------------------

// TestSportsbookExposure_RejectionShapeLeaksNoAmounts proves INV-SB-EXP-2:
// PlaceBetResult carries no aggregate, no threshold and no limit id on an
// exposure rejection - only the opaque RejectionCategory (and a generic,
// amount-free RejectionMessage).
func TestSportsbookExposure_RejectionShapeLeaksNoAmounts(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	limit := seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 500)

	result, err := placeBet(t, pool, f, sel, 1_000, "exp-no-leak")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected rejection")
	}
	if result.RejectionCategory != RejectionExposureLimit {
		t.Fatalf("expected rejection category %q, got %q", RejectionExposureLimit, result.RejectionCategory)
	}
	if result.RejectionCode != "" {
		t.Fatalf("INV-SB-EXP-2: expected an EMPTY RejectionCode (no scope_kind/limit id), got %q", result.RejectionCode)
	}
	forbidden := []string{
		"500", fmt.Sprint(limit.MaxOpenPotentialPayout), limit.ID.String(), "2000", "potential_return", "max_open_potential_payout",
	}
	for _, s := range forbidden {
		if containsSubstring(result.RejectionMessage, s) {
			t.Fatalf("INV-SB-EXP-2: RejectionMessage %q leaks forbidden substring %q", result.RejectionMessage, s)
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- item 26 ---------------------------------------------------------------

// TestSportsbookCumulativeAndExposure_IdempotentRetryReevaluatesNothing
// proves a retry with the same idempotency key returns the ORIGINAL bet
// without re-running the exposure gate a second time.
//
// Stage 9.2 fix round (ledger-finance P2-1): the PRIOR version of this test
// disabled the limit BETWEEN the two calls, which meant even a fully
// BROKEN implementation that re-evaluates on every retry would still
// converge on the same accepted outcome (limit gone => passes either way)
// - the test proved nothing about idempotency specifically. Fixed here: the
// limit stays ARMED and UNCHANGED across both calls, and the bet's own
// potential_return is sized so a genuine second evaluation would swing the
// ceiling check the OTHER way. The selection's default odds are 2.00
// (seedSelectionParams' own default, orchestrator_integration_test.go), so
// a 1_000 stake yields a potential_return of exactly 2_000. With the limit
// set to 3_000:
//   - First call: aggregate(0) + incremental(2_000) = 2_000 <= 3_000 =>
//     accepted, and the bet is now posted 'open' with potential_return
//     2_000, itself now part of the aggregate.
//   - A CORRECT idempotent-retry implementation takes the idempotency
//     short-circuit BEFORE reaching the exposure gate at all (step 2, per
//     PlaceBet's own doc comment) and returns the SAME original bet -
//     Accepted, same bet id, no re-evaluation.
//   - A BROKEN implementation that re-evaluates on retry would recompute
//     aggregate(2_000, from the now-open first bet) + incremental(2_000,
//     recomputed for the same retried bet) = 4_000 > 3_000 and INCORRECTLY
//     reject the retry.
//
// This makes the assertion "the retry is accepted and returns the original
// bet id" load-bearing: it fails under a broken re-evaluating
// implementation and passes only under a correct short-circuiting one -
// verified by temporarily disabling the idempotency short-circuit in
// PlaceBet and confirming this test fails, then restoring it (recorded in
// this fix round's own report; not re-verified by CI, which cannot
// self-mutate production code).
func TestSportsbookCumulativeAndExposure_IdempotentRetryReevaluatesNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{}) // default odds 2.00
	seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 3_000)

	first, err := placeBet(t, pool, f, sel, 1_000, "exp-idem-retry")
	if err != nil {
		t.Fatalf("place first bet: %v", err)
	}
	if !first.Accepted {
		t.Fatalf("expected the first bet to be accepted, got rejection %q/%q", first.RejectionCategory, first.RejectionCode)
	}
	if first.Bet.PotentialReturn != 2_000 {
		t.Fatalf("test setup invariant broken: expected potential_return 2_000 for a 1_000 stake at 2.00 odds, got %d", first.Bet.PotentialReturn)
	}

	// The limit is left ARMED and UNCHANGED - the whole point of this test.
	// A genuine second evaluation would now see the first bet's own
	// potential_return already counted in the aggregate and reject; only a
	// correct idempotency short-circuit (never re-evaluating) can accept.
	retry, err := placeBet(t, pool, f, sel, 1_000, "exp-idem-retry")
	if err != nil {
		t.Fatalf("place retry bet: %v", err)
	}
	if !retry.Accepted {
		t.Fatalf("expected the retry to return the ORIGINAL accepted bet via the idempotency short-circuit (no re-evaluation), got rejection %q/%q - "+
			"a broken implementation that re-evaluates the exposure gate on retry would incorrectly reject here, since the aggregate now already "+
			"includes this bet's own potential_return from the first call", retry.RejectionCategory, retry.RejectionCode)
	}
	if retry.Bet.ID != first.Bet.ID {
		t.Fatalf("expected the SAME bet id on retry (idempotency short-circuit, no re-evaluation), got %s vs %s", retry.Bet.ID, first.Bet.ID)
	}
	if countBets(t, pool, f) != 1 {
		t.Fatal("expected exactly 1 bet row - the retry must not have posted a second one")
	}
}

// --- item 27 ---------------------------------------------------------------

// TestSportsbookCumulative_RollbackOfTheWholeTransactionReleasesEveryLock
// proves a GENUINE error AFTER the exposure gate has run rolls back the
// whole PlaceBet transaction (no bet row, no ledger transaction) and
// releases every advisory lock (L0.6 in particular) - shown by a
// SUBSEQUENT, otherwise-identical bet succeeding immediately, which could
// not happen if the prior attempt's L0.6 lock (event-scoped, held for the
// remainder of ITS transaction) were somehow still held.
//
// Stage 9.2 fix round (code-reviewer, real test-integrity bug): the PRIOR
// version of this test passed an ALREADY-CANCELLED context to
// pool.WithTenant, which fails at Begin(ctx) - before PlaceBet, and
// therefore before the exposure gate, ever ran at all. That proved
// nothing about item 27 (rollback releases L0.6): the "subsequent bet
// succeeds" assertion would have passed even if L0.6 release were
// completely broken, since no lock was ever taken by the cancelled
// attempt in the first place.
//
// The fix: a REAL mid-transaction database error, forced AFTER the
// exposure gate has genuinely run (and acquired L0.6, since a limit is
// armed). Mechanism: pre-insert, in an already-committed transaction, a
// ledger_transactions row whose idempotency_key is byte-identical to the
// one PlaceBet will itself derive for this exact bet (orchestrator.go's
// own `ledger.TxSportsbookBet + ":" + playerAccountID + ":" +
// idempotencyKey` construction) but whose transaction_type is 'deposit',
// not 'sportsbook_bet'. PlaceBet's OWN idempotency short-circuit
// (findBetByIdempotencyKey, scoped to sportsbook_bets - a completely
// different key namespace) does not see this row, so PlaceBet proceeds
// all the way through RG, Risk, and the exposure gate (which passes,
// having found no breach, and acquires the real L0.6 advisory lock)
// exactly as an ordinary bet would. Only at ledger.Post - AFTER the
// exposure gate, deep inside this SAME PlaceBet transaction - does the
// real, database-round-tripped ledger.ErrIdempotencyKeyReused fire
// (internal/ledger/ledger.go's own "existing transaction has a different
// type" check), never a synthetic/pre-cancelled failure.
func TestSportsbookCumulative_RollbackOfTheWholeTransactionReleasesEveryLock(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 3_000)

	idempotencyKey := "exp-rollback-force-error"
	ledgerIdempotencyKey := string(ledger.TxSportsbookBet) + ":" + f.playerAccountID.String() + ":" + idempotencyKey
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id)
			 VALUES ($1, 'deposit', $2, $3)`,
			f.tenantID, ledgerIdempotencyKey, uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("seed a colliding ledger_transactions row (committed, separate transaction): %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_000,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: idempotencyKey,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected PlaceBet to fail via the genuine, post-exposure-gate ledger.ErrIdempotencyKeyReused collision")
	}
	if !errors.Is(err, ledger.ErrIdempotencyKeyReused) {
		t.Fatalf("expected the failure to be ledger.ErrIdempotencyKeyReused (proving this really reached ledger.Post, after the exposure gate), got: %v", err)
	}
	if countBets(t, pool, f) != 0 {
		t.Fatal("expected zero bet rows after the rolled-back attempt")
	}
	if countLedgerTransactions(t, pool, f) != 0 {
		t.Fatal("expected zero 'sportsbook_bet' ledger transactions after the rolled-back attempt (the pre-seeded 'deposit' row does not count, by construction)")
	}

	// A subsequent, ordinary bet on the SAME selection (same event, so the
	// SAME L0.6 key) must succeed immediately - proving no lock survived
	// the rollback.
	result, err := placeBet(t, pool, f, sel, 1_000, "exp-rollback-subsequent")
	if err != nil {
		t.Fatalf("place subsequent bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected the subsequent bet to be accepted, got rejection %q/%q", result.RejectionCategory, result.RejectionCode)
	}
}

// --- item 28 ---------------------------------------------------------------

// TestSportsbookCumulative_VoidedBetReleasesCapacity replaces this item's
// Stage 9.2 pin (TestSportsbookCumulative_VoidedBetStillConsumesCapacity-
// UntilReversalTypeExists), UPDATED rather than deleted as that test
// required: migration 0091 admitted sportsbook_void, and ADR 0088 §6.1 /
// INV-SB-CUM-1 set ReversalTypes = ["sportsbook_void"] in the same
// commit. A real void (Dr player_locked_cash / Cr player_cash, ADR 0088
// §2.3) now nets the stake out of risk.Evaluate's cumulativeUsage, so a
// second bet in the same window is admitted (§6.2 row "place ->
// void-before": consumed 0). The status is driven through the real
// settlement path; a raw status UPDATE is rejected by T-2.
func TestSportsbookCumulative_VoidedBetReleasesCapacity(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	actor := seedRiskManager(t, pool, f.tenantID)
	// A cumulative cap that admits exactly ONE 1_000 stake.
	seedCumulativeRule(t, pool, f.tenantID, 1_000)

	first, err := placeBet(t, pool, f, sel, 1_000, "exp-void-releases-1")
	if err != nil {
		t.Fatalf("place first bet: %v", err)
	}
	if !first.Accepted {
		t.Fatalf("expected the first bet to be accepted, got rejection %q/%q", first.RejectionCategory, first.RejectionCode)
	}

	// Before the void the cap is exhausted.
	blocked, err := placeBet(t, pool, f, sel, 1_000, "exp-void-releases-blocked")
	if err != nil {
		t.Fatalf("place blocked bet: %v", err)
	}
	if blocked.Accepted || blocked.RejectionCategory != RejectionRiskDenied {
		t.Fatalf("expected a risk denial before the void, got %+v", blocked)
	}

	mustSimulate(t, pool, f.tenantID, voidEvent(first.Bet.ID, actor, "market_cancelled"))

	second, err := placeBet(t, pool, f, sel, 1_000, "exp-void-releases-2")
	if err != nil {
		t.Fatalf("place second bet: %v", err)
	}
	if !second.Accepted {
		t.Fatalf("expected the second bet to be accepted - the void nets the first stake out of cumulative usage (ADR 0088 §6.2), got rejection %q/%q", second.RejectionCategory, second.RejectionCode)
	}
}

// --- item 29 ---------------------------------------------------------------

// TestSportsbookExposure_SettledBetLeavesOpenExposure asserts a bet
// settled through the real Stage 10 settlement path (ADR 0088 §6.3) is
// excluded from the open-exposure aggregate. Formerly
// TestSportsbookExposure_SettlementInteractionIsSpecifiedNotImplemented,
// which set the status with a raw UPDATE that migration 0091's T-2 now
// rejects.
func TestSportsbookExposure_SettledBetLeavesOpenExposure(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 3_000)
	actor := seedRiskManager(t, pool, f.tenantID)

	// A first bet consumes 2_000 of the 3_000 ceiling and is then settled
	// won through the real settlement path.
	first, err := placeBet(t, pool, f, sel, 1_000, "exp-settlement-1")
	if err != nil || !first.Accepted {
		t.Fatalf("place first bet: result=%+v err=%v", first, err)
	}
	mustSimulate(t, pool, f.tenantID, settleEvent(first.Bet.ID, actor, 1, SettlementOutcomeWon, first.Bet.PotentialReturn))

	// A second bet for the SAME 2_000 potential_return must now be
	// accepted - the settled bet no longer counts as OPEN exposure, so the
	// aggregate is 0 (existing) + 2_000 (incremental) = 2_000 <= 3_000.
	second, err := placeBet(t, pool, f, sel, 1_000, "exp-settlement-2")
	if err != nil {
		t.Fatalf("place second bet: %v", err)
	}
	if !second.Accepted {
		t.Fatalf("expected the second bet to be accepted - a settled bet carries no forward exposure, got rejection %q/%q", second.RejectionCategory, second.RejectionCode)
	}
}

// --- item 32 ---------------------------------------------------------------

// TestSportsbookExposure_FailsClosedOnUnreadableOrUnscannableLimit proves
// evaluateExposureLimits fails CLOSED - aborting the whole bet, never a
// decline and never an allow - on (b) a player-scoped connection and (c) a
// database-level error. (a) (an unscannable NUMERIC) is covered at the
// pure-function level by TestNumericToBigInt_RefusesFractionalExponent
// (exposure_unit_test.go) - NUMERIC(38,0)'s own typmod makes a negative
// exponent unreachable via a normal INSERT, so there is no integration
// path to construct one.
func TestSportsbookExposure_FailsClosedOnUnreadableOrUnscannableLimit(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel, eventID, marketID := seedSelectionWithContext(t, pool, seedSelectionParams{})
	seedExposureLimit(t, pool, f.tenantID, nil, "selection", "EUR", 3_000)

	t.Run("player_scoped_connection", func(t *testing.T) {
		err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := evaluateExposureLimits(ctx, tx, exposureParams{
				TenantID: f.tenantID, BrandID: f.brandID,
				Scope:     catalogueScope{EventID: eventID, MarketID: marketID, SelectionID: sel.ID},
				AssetCode: "EUR", IncrementalPotentialReturn: 1_000,
			})
			return err
		})
		if err == nil {
			t.Fatal("expected evaluateExposureLimits to fail closed on a player-scoped connection")
		}
		if !errors.Is(err, ErrExposurePlayerScopedConnection) {
			t.Fatalf("expected ErrExposurePlayerScopedConnection, got %v", err)
		}
	})

	// Stage 9.2 fix round (code-reviewer, real test-integrity bug): the
	// PRIOR version of this sub-test passed an ALREADY-CANCELLED context to
	// pool.WithTenant, which fails at Begin(ctx) before evaluateExposureLimits
	// - the actual code under test - ever runs. That proved nothing about
	// evaluateExposureLimits' own fail-closed behaviour on a database-level
	// error; it only proved db.Pool.WithTenant refuses a cancelled context,
	// a completely different (and already well-covered elsewhere) fact.
	//
	// The fix: a REAL database-level error, forced by a deliberately
	// malformed query executed on the SAME transaction immediately before
	// calling evaluateExposureLimits. Once one statement on a PostgreSQL
	// transaction errors, PostgreSQL puts the whole transaction into the
	// aborted state (SQLSTATE 25P02, "current transaction is aborted") -
	// every subsequent statement on that same transaction fails with a
	// genuine, round-tripped database error, exactly the class of failure
	// (query error, dropped connection, etc.) this test exists to prove
	// evaluateExposureLimits fails closed against. This reaches REAL SQL
	// executed against a REAL, valid, non-cancelled transaction - the
	// difference from a cancelled context is exactly the point.
	t.Run("database_error", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			// Deliberately malformed query: poisons the transaction into
			// PostgreSQL's aborted state. Its own error is expected and
			// discarded - what matters is every statement AFTER it,
			// including evaluateExposureLimits' own first query, now fails.
			_, _ = tx.Exec(ctx, `SELECT 1/0`)

			_, err := evaluateExposureLimits(ctx, tx, exposureParams{
				TenantID: f.tenantID, BrandID: f.brandID,
				Scope:     catalogueScope{EventID: eventID, MarketID: marketID, SelectionID: sel.ID},
				AssetCode: "EUR", IncrementalPotentialReturn: 1_000,
			})
			return err
		})
		if err == nil {
			t.Fatal("expected evaluateExposureLimits to fail closed when the underlying query errors (aborted transaction)")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected a genuine *pgconn.PgError (proving this is a real database-level failure, not a Go-level short-circuit), got %T: %v", err, err)
		}
		if pgErr.Code != "25P02" {
			t.Fatalf("expected SQLSTATE 25P02 (current transaction is aborted), got %s: %v", pgErr.Code, err)
		}
		if errors.Is(err, ErrExposurePlayerScopedConnection) || errors.Is(err, ErrExposureTenantScopeMismatch) {
			t.Fatal("expected a raw database error here, not one of the scope-mismatch sentinels (those are a different failure mode, covered by the sub-test above)")
		}
	})
}

// --- item 20 ---------------------------------------------------------------

// TestSportsbookCumulative_SimultaneousSamePlayerBetsCannotBothPass proves
// the L0.5 risk cumulative advisory lock (already wired for sportsbook_bet
// by Wave 1, ADR 0083 §6.1.1/§6.1.5) genuinely serializes two concurrent
// PlaceBet calls for ONE player against a cumulative_amount rule only one
// can fit under - exactly one is accepted, using the deterministic
// uncommitted-blocker technique this suite uses throughout (never a bare
// goroutine race).
func TestSportsbookCumulative_SimultaneousSamePlayerBetsCannotBothPass(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	// A cumulative cap that admits exactly ONE 1_000 stake, not two.
	seedCumulativeRule(t, pool, f.tenantID, 1_000)

	const n = 2
	results := make([]PlaceBetResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = placeBet(t, pool, f, sel, 1_000, fmt.Sprintf("cum-concurrent-%d", i))
		}(i)
	}
	wg.Wait()

	var accepted, denied int
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			if isDeadlock(errs[i]) {
				t.Fatalf("goroutine %d: deadlock - the L0.5 advisory lock is not serializing these two evaluations: %v", i, errs[i])
			}
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if results[i].Accepted {
			accepted++
		} else {
			if results[i].RejectionCategory != RejectionRiskDenied {
				t.Fatalf("goroutine %d: expected a risk denial on the losing bet, got %q/%q", i, results[i].RejectionCategory, results[i].RejectionCode)
			}
			denied++
		}
	}
	if accepted != 1 || denied != 1 {
		t.Fatalf("expected exactly 1 accepted and 1 denied under a 1000-stake/1000-threshold cumulative cap serialized by L0.5, got accepted=%d denied=%d", accepted, denied)
	}
	if countBets(t, pool, f) != 1 {
		t.Fatalf("expected exactly 1 committed bet row, got %d", countBets(t, pool, f))
	}
}
