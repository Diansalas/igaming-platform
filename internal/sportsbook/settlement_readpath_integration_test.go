//go:build integration

// ADR 0088 §3.4/§12: the read surfaces (GetBetByID, ListBetsForPlayer,
// ListBetsForTenant) must tolerate every one of the four BetStatus values -
// they select every column with no status filter, so a code revert that
// kept migration 0091 applied must still render non-open bets correctly.
package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReadPath_TolerantOfAllFourBetStatuses drives one bet through open,
// settled_won, open (rollback) and void, confirming GetBetByID,
// ListBetsForPlayer and ListBetsForTenant return it correctly at each
// step; a second, independent bet is separately parked at settled_lost -
// so all four BetStatus values are exercised across the two bets.
func TestReadPath_TolerantOfAllFourBetStatuses(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	assertReadable := func(id uuid.UUID, fx sbFixture, wantStatus BetStatus) {
		t.Helper()
		var got Bet
		err := pool.WithTenant(context.Background(), fx.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = GetBetByID(ctx, tx, id)
			return err
		})
		if err != nil {
			t.Fatalf("GetBetByID: %v", err)
		}
		if got.Status != wantStatus {
			t.Fatalf("GetBetByID status = %q, want %q", got.Status, wantStatus)
		}

		var playerBets []Bet
		err = pool.WithPlayerScope(context.Background(), fx.tenantID, fx.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			playerBets, _, err = ListBetsForPlayer(ctx, tx, fx.playerAccountID, 50, 0)
			return err
		})
		if err != nil {
			t.Fatalf("ListBetsForPlayer: %v", err)
		}
		if !containsBetWithStatus(playerBets, id, wantStatus) {
			t.Fatalf("ListBetsForPlayer did not return bet %s at status %q: %+v", id, wantStatus, playerBets)
		}

		var tenantBets []Bet
		err = pool.WithTenant(context.Background(), fx.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			tenantBets, _, err = ListBetsForTenant(ctx, tx, 50, 0)
			return err
		})
		if err != nil {
			t.Fatalf("ListBetsForTenant: %v", err)
		}
		if !containsBetWithStatus(tenantBets, id, wantStatus) {
			t.Fatalf("ListBetsForTenant did not return bet %s at status %q: %+v", id, wantStatus, tenantBets)
		}
	}

	assertReadable(betID, f, BetStatusOpen)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	assertReadable(betID, f, BetStatusSettledWon)
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	assertReadable(betID, f, BetStatusOpen)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	assertReadable(betID, f, BetStatusVoid)

	f2, actor2, betID2 := newStdBet(t, pool)
	mustSimulate(t, pool, f2.tenantID, settleEvent(betID2, actor2, 1, SettlementOutcomeLost, 0))
	assertReadable(betID2, f2, BetStatusSettledLost)
}

func containsBetWithStatus(bets []Bet, id uuid.UUID, want BetStatus) bool {
	for _, b := range bets {
		if b.ID == id {
			return b.Status == want
		}
	}
	return false
}
