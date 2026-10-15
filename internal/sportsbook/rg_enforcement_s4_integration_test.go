//go:build integration

// ADR 0115 S4-min (completion audit A6) through the REAL sportsbook PlaceBet: wager / loss limits and the
// time-out refuse a NEW bet with a stable rejection code and a figures-bearing audit row, post nothing, and
// never gate settlement, void or rollback of bets already accepted. The stake of a sportsbook bet is its
// potential loss. Semantics are proven in internal/rg; this file proves the sportsbook WIRING.
package sportsbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func setSBLimit(t *testing.T, pool *db.Pool, f sbFixture, kind rg.ControlKind, period rg.PeriodKind, value string) {
	t.Helper()
	asset := "EUR"
	if err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.NewLimits(rg.LimitsConfig{}).SetLimit(ctx, tx, rg.SetLimitInput{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID,
			Limit: rg.LimitInput{Kind: kind, AssetCode: &asset, Period: &period, Value: &value, IdempotencyKey: uuid.NewString()}})
		return err
	}); err != nil {
		t.Fatalf("set %s limit: %v", kind, err)
	}
}

func TestRGEnforceS4_Sportsbook_WagerLimitRejectsAndNeverBlocksSettlementOrVoid(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 1_000_000)
	rgfix.Provision(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, "EUR", nil)
	setSBLimit(t, pool, f, rg.ControlWager, rg.PeriodRolling24h, "500")
	sel := seedSelection(t, pool, seedSelectionParams{})

	var accepted []uuid.UUID
	for i := 0; i < 5; i++ {
		r, err := placeBet(t, pool, f, sel, 100, fmt.Sprintf("rg-ok-%d-%s", i, uuid.NewString()))
		if err != nil || !r.Accepted {
			t.Fatalf("bet %d within the limit must be accepted: accepted=%v err=%v rejection=%s", i, r.Accepted, err, r.RejectionCode)
		}
		accepted = append(accepted, r.Bet.ID)
	}
	balBefore := cashBalance(t, pool, f)
	r, err := placeBet(t, pool, f, sel, 1, "rg-over-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if r.Accepted || r.RejectionCategory != RejectionRGDenied || r.RejectionCode != rg.CodeWagerLimitReached {
		t.Fatalf("a stake over the wager limit must be rejected rg_denied / %s, got %+v", rg.CodeWagerLimitReached, r)
	}
	if got := cashBalance(t, pool, f); got != balBefore {
		t.Fatalf("a rejected bet posts nothing: %d -> %d", balBefore, got)
	}
	var meta map[string]any
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'sportsbook_bet.denied_by_rg_policy' ORDER BY created_at DESC LIMIT 1`, f.tenantID).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &meta)
	}); err != nil {
		t.Fatalf("denial audit: %v", err)
	}
	for k, want := range map[string]string{"reason_code": rg.CodeWagerLimitReached, "limit": "500", "usage": "500", "attempted_amount": "1", "kind": "wager", "period": "rolling_24h"} {
		if meta[k] != want {
			t.Fatalf("denial audit %s = %v, want %q (%v)", k, meta[k], want, meta)
		}
	}

	// Settlement and void of bets ALREADY accepted are never gated, with the limit exhausted.
	actor := seedRiskManager(t, pool, f.tenantID)
	if res := mustSimulate(t, pool, f.tenantID, settleEvent(accepted[0], actor, 1, SettlementOutcomeWon, 200)); res.Result != SettlementResultApplied {
		t.Fatalf("a settlement must not be gated by a limit: %+v", res)
	}
	if res := mustSimulate(t, pool, f.tenantID, voidEvent(accepted[1], actor, "market_cancelled")); res.Result != SettlementResultApplied {
		t.Fatalf("a void must not be gated by a limit: %+v", res)
	}
}

func TestRGEnforceS4_Sportsbook_TimeOutRejects_ConfigMissingFailsClosed_SwitchOffIsEligibilityOnly(t *testing.T) {
	pool := testPool(t)
	sel := seedSelection(t, pool, seedSelectionParams{})

	// switch off: a recorded limit is not enforced
	off := seedFixture(t, pool)
	fundWallet(t, pool, off, 100_000)
	setSBLimit(t, pool, off, rg.ControlWager, rg.PeriodRolling24h, "100")
	if r, err := placeBet(t, pool, off, sel, 5000, "off-"+uuid.NewString()); err != nil || !r.Accepted {
		t.Fatalf("enforcement off: the bet is accepted, got %+v err=%v", r, err)
	}
	// ...but a time-out is enforced regardless
	if err := pool.WithPlayerScope(context.Background(), off.tenantID, off.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.NewLimits(rg.LimitsConfig{}).CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: off.tenantID, PlayerAccountID: off.playerAccountID,
			DurationHours: 24, IdempotencyKey: uuid.NewString()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, err := placeBet(t, pool, off, sel, 10, "to-"+uuid.NewString())
	if err != nil || r.Accepted || r.RejectionCategory != RejectionRGDenied || r.RejectionCode != rg.CodeTimedOut {
		t.Fatalf("a bet during a time-out must be rejected rg_denied / timed_out, got %+v err=%v", r, err)
	}

	// switch on, no floor: fail closed
	on := seedFixture(t, pool)
	fundWallet(t, pool, on, 100_000)
	staff := rgfix.Staff(t, launchfix.OwnerFor(t, pool), on.tenantID)
	rgfix.Activate(t, launchfix.OwnerFor(t, pool), on.tenantID, nil, staff)
	r, err = placeBet(t, pool, on, sel, 10, "cm-"+uuid.NewString())
	if err != nil || r.Accepted || r.RejectionCode != rg.CodeConfigMissing {
		t.Fatalf("enforcement on with no floor must reject rg_config_missing, got %+v err=%v", r, err)
	}
}

func TestRGEnforceS4_Sportsbook_EvaluationFailure_AbortsPlacement(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	rgfix.Provision(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, "EUR", nil)
	sel := seedSelection(t, pool, seedSelectionParams{})
	before := cashBalance(t, pool, f)
	// A REPEATABLE READ placement: the isolation guard of ADR 0115 3.1 refuses it (the post-lock usage read must see
	// every commit that preceded the lock), and a refusal to decide is an ERROR that aborts the placement.
	ctx := context.Background()
	tx, err := pool.Raw().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
		t.Fatal(err)
	}
	_, perr := PlaceBet(ctx, tx, PlaceBetParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 10,
		ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator, IdempotencyKey: "rr-" + uuid.NewString(),
	})
	if !errors.Is(perr, rg.ErrEvaluationUnavailable) || !errors.Is(perr, rg.ErrUsageIsolation) {
		t.Fatalf("a placement whose RG evaluation cannot decide must abort with ErrEvaluationUnavailable (isolation), got %v", perr)
	}
	_ = tx.Rollback(ctx)
	if got := cashBalance(t, pool, f); got != before {
		t.Fatalf("an aborted placement posts nothing: %d -> %d", before, got)
	}
}

func TestRGEnforceS4_Sportsbook_ConcurrentPlacementsRacingTheLimit_ExactlyTheAllowedOnesAreAccepted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 1_000_000)
	rgfix.Provision(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, "EUR", nil)
	setSBLimit(t, pool, f, rg.ControlWager, rg.PeriodRolling24h, "500")
	sel := seedSelection(t, pool, seedSelectionParams{})
	const n = 12
	var mu sync.Mutex
	accepted, rejected := 0, 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := placeBet(t, pool, f, sel, 100, fmt.Sprintf("race-%d-%s", i, uuid.NewString()))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("placement %d: %v", i, err)
			case r.Accepted:
				accepted++
			case r.RejectionCode == rg.CodeWagerLimitReached:
				rejected++
			default:
				t.Errorf("placement %d: unexpected %+v", i, r)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if accepted != 5 || rejected != n-5 {
		t.Fatalf("limit 500 / stake 100: exactly 5 of %d concurrent placements may be accepted, got %d accepted %d rejected", n, accepted, rejected)
	}
}
