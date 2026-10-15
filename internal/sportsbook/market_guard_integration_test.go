//go:build integration

// A8 / SR8-1 / SR8-3: PlaceBetParams.MarketGuard. The decision logic is
// internal/marketgate's; these tests pin WHERE PlaceBet evaluates it (first,
// before the idempotency lookup, any catalogue read, any ledger lock or write)
// and what it does with each verdict: a refusal is a MarketDenied result with no
// stake locked, no ledger row and no bet row (replay included); an error fails
// closed; an EMPTY guard refuses.
package sportsbook

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

func placeWith(t *testing.T, _ any, f sbFixture, sel Selection, key string, guard marketgate.Guard) (PlaceBetResult, error) {
	t.Helper()
	p := testPool(t)
	var result PlaceBetResult
	err := p.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_000,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: key, MarketGuard: guard,
		})
		return err
	})
	return result, err
}

func deny() marketgate.Guard {
	return func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		return marketgate.Decision{Allowed: false, Code: marketgate.ReasonNotPermitted}, nil
	}
}

func TestPlaceBet_MarketGuardRefusalLocksNoStakeAndWritesNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 250, OddsDenominator: 100})

	res, err := placeWith(t, pool, f, sel, "mkt-deny-1", deny())
	if err != nil {
		t.Fatalf("a refusal is a result, not an error: %v", err)
	}
	if !res.MarketDenied || res.Accepted || res.RejectionCategory != "" || res.Bet.ID.String() != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("want a bare MarketDenied result, got %+v", res)
	}
	if bal := cashBalance(t, pool, f); bal != 10_000 {
		t.Fatalf("cash balance moved to %d", bal)
	}
	if locked := lockedCashBalance(t, pool, f); locked != 0 {
		t.Fatalf("a refused bet locked %d", locked)
	}
	if n := countLedgerTransactions(t, pool, f); n != 0 {
		t.Fatalf("a refused bet left %d sportsbook_bet ledger transactions", n)
	}
	if n := countBets(t, pool, f); n != 0 {
		t.Fatalf("a refused bet left %d bet rows", n)
	}
}

// The guard precedes the idempotency lookup: a replay of an accepted bet is refused too.
func TestPlaceBet_MarketGuardRefusesAReplayOfAnAcceptedBet(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	first, err := placeWith(t, pool, f, sel, "mkt-replay-1", marketgate.UngatedForTests)
	if err != nil || !first.Accepted {
		t.Fatalf("first placement: %+v %v", first, err)
	}
	replay, err := placeWith(t, pool, f, sel, "mkt-replay-1", deny())
	if err != nil || !replay.MarketDenied || replay.Accepted || replay.Bet.ID == first.Bet.ID {
		t.Fatalf("a refused replay must not return the bet, got %+v err=%v", replay, err)
	}
	if n := countBets(t, pool, f); n != 1 {
		t.Fatalf("want the original bet only, got %d", n)
	}
}

func TestPlaceBet_MarketGuardErrorFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	boom := errors.New("policy read failed")
	res, err := placeWith(t, pool, f, sel, "mkt-err-1", func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		return marketgate.Decision{Allowed: true}, boom
	})
	if !errors.Is(err, boom) || res.Accepted {
		t.Fatalf("a guard error must fail the placement, got %+v err=%v", res, err)
	}
	if countBets(t, pool, f) != 0 || countLedgerTransactions(t, pool, f) != 0 {
		t.Fatal("a failed gate wrote rows")
	}
}

func TestPlaceBet_EmptyMarketGuardRefuses(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	res, err := placeWith(t, pool, f, sel, "mkt-empty-1", nil)
	if !errors.Is(err, marketgate.ErrGuardRequired) || res.Accepted {
		t.Fatalf("an empty guard must refuse with ErrGuardRequired, got %+v err=%v", res, err)
	}
	if countBets(t, pool, f) != 0 || countLedgerTransactions(t, pool, f) != 0 {
		t.Fatal("an empty guard wrote rows")
	}
}

// The guard runs inside the caller's tenant transaction and BEFORE any bet row exists.
func TestPlaceBet_MarketGuardRunsFirstInsideTheTransaction(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	seen := -1
	res, err := placeWith(t, pool, f, sel, "mkt-first-1", func(ctx context.Context, tx pgx.Tx) (marketgate.Decision, error) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE tenant_id = $1`, f.tenantID).Scan(&seen); err != nil {
			return marketgate.Decision{}, err
		}
		return marketgate.Decision{Allowed: true}, nil
	})
	if err != nil || !res.Accepted {
		t.Fatalf("allowed placement: %+v %v", res, err)
	}
	if seen != 0 {
		t.Fatalf("the guard saw %d bet rows; it must run before the insert", seen)
	}
}
