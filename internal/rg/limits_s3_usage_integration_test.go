//go:build integration

package rg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func lockedCashCr(acct uuid.UUID, amt string) s3Leg {
	return s3Leg{acct, ledger.AccountPlayerLockedCash, "credit", amt}
}
func lockedCashDr(acct uuid.UUID, amt string) s3Leg {
	return s3Leg{acct, ledger.AccountPlayerLockedCash, "debit", amt}
}

// seedS3Scenario writes the hand-computed ledger of acct (USD) relative to s3T. The expected figures
// are in TestRGUsageS3_HandComputedLedger and were derived by hand from this list:
//
//	bet1   T-30m casino_bet 100            bet2   T-2h  casino_bet 250        win2  T-1h  casino_win 400 (round of bet2)
//	rb(w2) T-30m casino_rollback of win2   bet3   T-25h casino_bet 1000       rb(b3) T-1h casino_rollback of bet3
//	bet4   T-8d  casino_bet 5000           bet5   T-3d  casino_bet 600         bet6  T-15h casino_bet 40
//	sb1    T-3h  sportsbook_bet 70         set1   T-2h  settlement payout 150
//	sb2    T-4h  sportsbook_bet 30         void2  T-3h30 sportsbook_void 30    (no reverses_transaction_id)
//	sb3    T-26h sportsbook_bet 20         void3  T-1h  sportsbook_void 20
//	T-1h deposit 999 and bonus_conversion 50 (neither is in the wager / loss sets)
//	bonus round: bet T-90m Dr player_bonus 80 / Cr player_locked_bonus 80; win T-80m house 30 + lock release 80 to player_bonus
func (w *s1World) seedS3Scenario(t *testing.T, tenant, a uuid.UUID) {
	t.Helper()
	usd := "USD"
	bet := func(at time.Duration, amt string) uuid.UUID {
		return w.s3Post(t, tenant, s3Tx{typ: "casino_bet", at: ago(at), asset: usd, legs: []s3Leg{cashDr(a, amt), houseCr(amt)}})
	}
	bet(30*time.Minute, "100")
	c2 := uuid.New()
	w.s3Post(t, tenant, s3Tx{typ: "casino_bet", at: ago(2 * hr), corr: c2, asset: usd, legs: []s3Leg{cashDr(a, "250"), houseCr("250")}})
	win2 := w.s3Post(t, tenant, s3Tx{typ: "casino_win", at: ago(hr), corr: c2, asset: usd, legs: []s3Leg{houseDr("400"), cashCr(a, "400")}})
	w.s3Post(t, tenant, s3Tx{typ: "casino_rollback", at: ago(30 * time.Minute), corr: c2, reverses: &win2, asset: usd, legs: []s3Leg{cashDr(a, "400"), houseCr("400")}})
	bet3 := bet(25*hr, "1000")
	w.s3Post(t, tenant, s3Tx{typ: "casino_rollback", at: ago(hr), reverses: &bet3, asset: usd, legs: []s3Leg{cashCr(a, "1000"), houseDr("1000")}})
	bet(8*day, "5000")
	bet(3*day, "600")
	bet(15*hr, "40")

	s1 := uuid.New()
	w.s3Post(t, tenant, s3Tx{typ: "sportsbook_bet", at: ago(3 * hr), corr: s1, asset: usd, legs: []s3Leg{cashDr(a, "70"), lockedCashCr(a, "70")}})
	w.s3Post(t, tenant, s3Tx{typ: "sportsbook_settlement", at: ago(2 * hr), corr: s1, asset: usd, legs: []s3Leg{lockedCashDr(a, "70"), houseCr("70"), houseDr("150"), cashCr(a, "150")}})
	s2 := uuid.New()
	w.s3Post(t, tenant, s3Tx{typ: "sportsbook_bet", at: ago(4 * hr), corr: s2, asset: usd, legs: []s3Leg{cashDr(a, "30"), lockedCashCr(a, "30")}})
	w.s3Post(t, tenant, s3Tx{typ: "sportsbook_void", at: ago(3*hr + 30*time.Minute), corr: s2, asset: usd, legs: []s3Leg{lockedCashDr(a, "30"), cashCr(a, "30")}})
	s3 := uuid.New()
	w.s3Post(t, tenant, s3Tx{typ: "sportsbook_bet", at: ago(26 * hr), corr: s3, asset: usd, legs: []s3Leg{cashDr(a, "20"), lockedCashCr(a, "20")}})
	w.s3Post(t, tenant, s3Tx{typ: "sportsbook_void", at: ago(hr), corr: s3, asset: usd, legs: []s3Leg{lockedCashDr(a, "20"), cashCr(a, "20")}})

	w.s3Deposit(t, tenant, a, usd, "999", ago(hr))
	w.s3Post(t, tenant, s3Tx{typ: "bonus_conversion", at: ago(hr), asset: usd, legs: []s3Leg{
		{a, ledger.AccountPlayerBonus, "debit", "50"}, cashCr(a, "50")}})

	c7 := uuid.New()
	w.s3Post(t, tenant, s3Tx{typ: "casino_bet", at: ago(90 * time.Minute), corr: c7, asset: usd, legs: []s3Leg{
		{a, ledger.AccountPlayerBonus, "debit", "80"}, {a, ledger.AccountPlayerLockedBonus, "credit", "80"}}})
	w.s3Post(t, tenant, s3Tx{typ: "casino_win", at: ago(80 * time.Minute), corr: c7, asset: usd, legs: []s3Leg{
		houseDr("30"), {a, ledger.AccountPlayerBonus, "credit", "30"},
		{a, ledger.AccountPlayerLockedBonus, "debit", "80"}, {a, ledger.AccountPlayerBonus, "credit", "80"}}})
}

func TestRGUsageS3(t *testing.T) {
	w := newS1World(t)

	t.Run("HandComputedLedger_WindowsReversalsBonusAndPerson", func(t *testing.T) {
		f := w.newFix(t)
		w.seedS3Scenario(t, f.tenantA, f.acctA1)
		// Other accounts: same Person other brand (A2), another Person (X), and the SAME Person in the OTHER
		// tenant (B). Only A2 may join A1's tenant_person total.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(hr), asset: "USD", legs: []s3Leg{cashDr(f.acctA2, "1000"), houseCr("1000")}})
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(hr), asset: "USD", legs: []s3Leg{cashDr(f.acctX, "7777"), houseCr("7777")}})
		w.s3Post(t, f.tenantB, s3Tx{typ: "casino_bet", at: ago(hr), asset: "USD", legs: []s3Leg{cashDr(f.acctB, "5555"), houseCr("5555")}})
		// Another asset on the same account must not leak in.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(hr), asset: "EUR", legs: []s3Leg{cashDr(f.acctA1, "12345"), houseCr("12345")}})

		at := s3T
		base := UsageRequest{TenantID: f.tenantA, PlayerAccountID: f.acctA1, Scope: LimitScopeAccount, AssetCode: "USD", Windows: s3Windows("UTC"), At: &at}

		// Cash only (count_bonus_funded=false for both kinds): the bonus round and the conversion change nothing.
		u := w.measureStake(t, f, f.tenantA, base)
		wantW := []string{"460", "1060", "6060", "420"}
		wantL := []string{"310", "910", "5910", "270"}
		for i, ws := range u.Windows {
			wantAmt(t, fmt.Sprintf("wager %s", ws.Window.Period), ws.Wager.Account, wantW[i])
			wantAmt(t, fmt.Sprintf("loss %s", ws.Window.Period), ws.Loss.Account, wantL[i])
			if ws.Wager.Person != nil || ws.Loss.Person != nil {
				t.Fatalf("account scope must not report a Person figure")
			}
		}
		if len(u.AccountIDs) != 1 || u.AccountIDs[0] != f.acctA1 {
			t.Fatalf("account scope must sum the anchor only: %v", u.AccountIDs)
		}
		if !u.AsOf.Equal(at) {
			t.Fatalf("as-of %s, want the replay instant %s", u.AsOf, at)
		}
		// Window starts are the SQL ones.
		if !u.Windows[0].Start.Equal(at.Add(-24*hr)) || !u.Windows[1].Start.Equal(at.Add(-168*hr)) ||
			!u.Windows[2].Start.Equal(at.Add(-720*hr)) || !u.Windows[3].Start.Equal(time.Date(2025, 3, 12, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("unexpected window starts: %v", u.Windows)
		}

		// Bonus-funded legs counted for both kinds: wager +80, loss 80 - 30 - 80 = -30 in every window
		// (the 50 conversion is not in the set).
		bonus := base
		bonus.WagerCountsBonus, bonus.LossCountsBonus = true, true
		ub := w.measureStake(t, f, f.tenantA, bonus)
		wantWB := []string{"540", "1140", "6140", "500"}
		wantLB := []string{"280", "880", "5880", "240"}
		for i, ws := range ub.Windows {
			wantAmt(t, fmt.Sprintf("bonus wager %s", ws.Window.Period), ws.Wager.Account, wantWB[i])
			wantAmt(t, fmt.Sprintf("bonus loss %s", ws.Window.Period), ws.Loss.Account, wantLB[i])
		}
		// The kinds carry independent flags.
		mixed := base
		mixed.WagerCountsBonus = true
		um := w.measureStake(t, f, f.tenantA, mixed)
		wantAmt(t, "wager with bonus, loss without (24h)", um.Windows[0].Wager.Account, "540")
		wantAmt(t, "loss without bonus (24h)", um.Windows[0].Loss.Account, "310")

		// tenant_person: A1 + A2 only. X (another Person) and B (another tenant) never join.
		pr := base
		pr.Scope = LimitScopeTenantPerson
		up := w.measureStake(t, f, f.tenantA, pr)
		wantAmt(t, "person wager 24h", up.Windows[0].Wager.Person, "1460")
		wantAmt(t, "person loss 24h", up.Windows[0].Loss.Person, "1310")
		wantAmt(t, "account wager 24h (person scope also reports the anchor)", up.Windows[0].Wager.Account, "460")
		wantAmt(t, "person wager 7d", up.Windows[1].Wager.Person, "2060")
		if len(up.AccountIDs) != 2 {
			t.Fatalf("tenant_person set must be A1+A2, got %v", up.AccountIDs)
		}
		for _, id := range up.AccountIDs {
			if id != f.acctA1 && id != f.acctA2 {
				t.Fatalf("foreign account %s in the Person set", id)
			}
		}
		// The same Person's account in tenant B stays tenant B's own: one account, never tenant A's.
		pb := UsageRequest{TenantID: f.tenantB, PlayerAccountID: f.acctB, Scope: LimitScopeTenantPerson, AssetCode: "USD", Windows: s3Windows("UTC"), At: &at}
		ubb := w.measureStake(t, f, f.tenantB, pb)
		wantAmt(t, "tenant B person wager", ubb.Windows[0].Wager.Person, "5555")
		if len(ubb.AccountIDs) != 1 || ubb.AccountIDs[0] != f.acctB {
			t.Fatalf("tenant B Person set must be B only: %v", ubb.AccountIDs)
		}

		// Per asset: EUR sees only the EUR bet.
		eu := base
		eu.AssetCode = "EUR"
		ue := w.measureStake(t, f, f.tenantA, eu)
		wantAmt(t, "EUR wager 24h", ue.Windows[0].Wager.Account, "12345")
		wantAmt(t, "EUR loss 30d", ue.Windows[2].Loss.Account, "12345")
	})

	t.Run("LossFloor_NetWinGivesNoHeadroomUnlessAttested", func(t *testing.T) {
		f := w.newFix(t)
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_win", at: ago(hr), asset: "USD", legs: []s3Leg{houseDr("500"), cashCr(f.acctX, "500")}})
		req := UsageRequest{TenantID: f.tenantA, PlayerAccountID: f.acctX, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T}
		u := w.measureStake(t, f, f.tenantA, req)
		wantAmt(t, "raw loss", u.Windows[0].LossRaw.Account, "-500")
		wantAmt(t, "floored loss", u.Windows[0].Loss.Account, "0")
		wantAmt(t, "wager", u.Windows[0].Wager.Account, "0")
		req.LossMayGoNegative = true
		u = w.measureStake(t, f, f.tenantA, req)
		wantAmt(t, "attested negative loss", u.Windows[0].Loss.Account, "-500")
	})

	t.Run("BoundaryInstantsAreInclusiveAtTheWindowStart", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		// Rolling 24h starts at T-24h exactly: a posting AT the start counts, one microsecond before does not.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: s3T.Add(-24 * hr), asset: "USD", legs: []s3Leg{cashDr(a, "1"), houseCr("1")}})
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: s3T.Add(-24*hr - time.Microsecond), asset: "USD", legs: []s3Leg{cashDr(a, "10"), houseCr("10")}})
		// Calendar day (UTC) starts 00:00 exactly.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: time.Date(2025, 3, 12, 0, 0, 0, 0, time.UTC), asset: "USD", legs: []s3Leg{cashDr(a, "100"), houseCr("100")}})
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: time.Date(2025, 3, 11, 23, 59, 59, 999999000, time.UTC), asset: "USD", legs: []s3Leg{cashDr(a, "1000"), houseCr("1000")}})
		// The longest window starts at T-720h exactly: a posting AT that start counts for it (and for no shorter window),
		// one microsecond before does not.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: s3T.Add(-720 * hr), asset: "USD", legs: []s3Leg{cashDr(a, "20000"), houseCr("20000")}})
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: s3T.Add(-720*hr - time.Microsecond), asset: "USD", legs: []s3Leg{cashDr(a, "300000"), houseCr("300000")}})
		// A posting AFTER the replay instant is not part of a replay.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: s3T.Add(time.Microsecond), asset: "USD", legs: []s3Leg{cashDr(a, "10000"), houseCr("10000")}})
		u := w.measureStake(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD",
			Windows: []WindowSpec{{Period: PeriodRolling24h}, {Period: PeriodCalendarDay, CalendarTZ: "UTC"}, {Period: PeriodRolling30d}}, At: &s3T})
		wantAmt(t, "rolling 24h", u.Windows[0].Wager.Account, "1101") // 1 + 100 + 1000 (the 23:59:59.999999 bet is inside the last 24h)
		wantAmt(t, "calendar day", u.Windows[1].Wager.Account, "100")
		wantAmt(t, "rolling 30d (the bet exactly at the start counts)", u.Windows[2].Wager.Account, "21111") // 1 + 10 + 100 + 1000 + 20000
	})

	t.Run("WindowRollover_CalendarDayResetsRollingDoesNot", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		ny := "America/New_York"
		// 2025-03-12 23:59:50 local (EDT, UTC-4) = 2025-03-13 03:59:50Z
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: time.Date(2025, 3, 13, 3, 59, 50, 0, time.UTC), asset: "USD", legs: []s3Leg{cashDr(a, "70"), houseCr("70")}})
		win := []WindowSpec{{Period: PeriodRolling24h}, {Period: PeriodCalendarDay, CalendarTZ: ny}}
		before := time.Date(2025, 3, 13, 3, 59, 59, 0, time.UTC) // still 12 March locally
		after := time.Date(2025, 3, 13, 4, 0, 1, 0, time.UTC)    // 13 March 00:00:01 locally
		ub := w.measureStake(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD", Windows: win, At: &before})
		ua := w.measureStake(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD", Windows: win, At: &after})
		wantAmt(t, "before midnight: calendar day", ub.Windows[1].Wager.Account, "70")
		wantAmt(t, "before midnight: rolling", ub.Windows[0].Wager.Account, "70")
		wantAmt(t, "after midnight: calendar day resets", ua.Windows[1].Wager.Account, "0")
		wantAmt(t, "after midnight: rolling still counts", ua.Windows[0].Wager.Account, "70")
	})

	t.Run("AmountsAboveInt64_NoInt64NoFloat", func(t *testing.T) {
		// No exponent-18 asset is registered in this schema; the magnitude (NUMERIC(38,0), 32 digits) is what
		// matters to the arithmetic, and BTC (exponent 8) carries it.
		f := w.newFix(t)
		asset := "BTC"
		a := f.acctA1
		big1, big2, bigWin := "12345678901234567890123456789012", "98765432109876543210987654321098", "11111111111111111111111111111111"
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(hr), asset: asset, legs: []s3Leg{cashDr(a, big1), houseCr(big1)}})
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(2 * hr), asset: asset, legs: []s3Leg{cashDr(a, big2), houseCr(big2)}})
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_win", at: ago(30 * time.Minute), asset: asset, legs: []s3Leg{houseDr(bigWin), cashCr(a, bigWin)}})
		u := w.measureStake(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: asset,
			Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T})
		// 12345678901234567890123456789012 + 98765432109876543210987654321098 = 111111111011111111101111111110110 (checked with python3 big ints)
		wantAmt(t, "wager above int64", u.Windows[0].Wager.Account, "111111111011111111101111111110110")
		// loss = wager - win = 111111111011111111101111111110110 - 11111111111111111111111111111111 = 99999999899999999989999999998999
		wantAmt(t, "loss above int64", u.Windows[0].Loss.Account, "99999999899999999989999999998999")
	})

	t.Run("FailClosed_UndeclaredLegAndUnresolvedReversal", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		req := UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T}
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(hr), asset: "USD", legs: []s3Leg{cashDr(a, "10"), houseCr("10")}})
		// A casino_win that touches a player-owned manual_adjustment leg: undeclared for the RG specs.
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_win", at: ago(hr), asset: "USD", legs: []s3Leg{houseDr("5"), {a, ledger.AccountManualAdjustment, "credit", "5"}}})
		err := do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureStakeUsage(ctx, tx, req)
			return err
		})
		if !errors.Is(err, ErrUnrecognizedUsageLeg) {
			t.Fatalf("an undeclared player-side leg must fail closed, got %v", err)
		}
		// A casino win that RELEASES a cash lock into player_cash (the locked-cash origin shape of ADR 0039 section 16.4,
		// not producible by today's postBet): its release credit would net the stake out of loss, so the shape is undeclared.
		h := w.newFix(t)
		w.s3Post(t, h.tenantA, s3Tx{typ: "casino_win", at: ago(hr), asset: "USD", legs: []s3Leg{
			houseDr("50"), cashCr(h.acctA1, "50"), lockedCashDr(h.acctA1, "20"), cashCr(h.acctA1, "20")}})
		err = do(w.rt, staffSess(h.tenantA, h.staffA), func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureStakeUsage(ctx, tx, UsageRequest{TenantID: h.tenantA, PlayerAccountID: h.acctA1, Scope: LimitScopeAccount, AssetCode: "USD",
				Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T})
			return err
		})
		if !errors.Is(err, ErrUnrecognizedUsageLeg) {
			t.Fatalf("a casino posting that locks cash must fail closed, got %v", err)
		}
		// A casino_rollback whose original cannot be resolved.
		g := w.newFix(t)
		w.s3Post(t, g.tenantA, s3Tx{typ: "casino_rollback", at: ago(hr), asset: "USD", legs: []s3Leg{cashCr(g.acctA1, "10"), houseDr("10")}})
		req2 := UsageRequest{TenantID: g.tenantA, PlayerAccountID: g.acctA1, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T}
		err = do(w.rt, staffSess(g.tenantA, g.staffA), func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureStakeUsage(ctx, tx, req2)
			return err
		})
		if !errors.Is(err, ErrUnresolvedReversal) {
			t.Fatalf("a reversal with no resolvable original must fail closed, got %v", err)
		}
	})

	t.Run("Guards_TenantIsolationScopeAndIsolationLevel", func(t *testing.T) {
		f := w.newFix(t)
		w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", at: ago(hr), asset: "USD", legs: []s3Leg{cashDr(f.acctA1, "10"), houseCr("10")}})
		req := UsageRequest{TenantID: f.tenantA, PlayerAccountID: f.acctA1, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T}
		run := func(s sess, r UsageRequest) error {
			return do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error { _, err := MeasureStakeUsage(ctx, tx, r); return err })
		}
		// Tenant B's session asking for tenant A's request: refused (RLS alone would answer zero usage = fail open).
		if err := run(staffSess(f.tenantB, f.staffB), req); !errors.Is(err, ErrUsageScope) {
			t.Fatalf("a request for another tenant must be refused, got %v", err)
		}
		// Tenant B's session naming tenant A's account under its OWN tenant id: not found, never zero.
		cross := req
		cross.TenantID = f.tenantB
		if err := run(staffSess(f.tenantB, f.staffB), cross); !errors.Is(err, ErrUsageAccountNotFound) {
			t.Fatalf("another tenant's account must be not found, got %v", err)
		}
		// A player-scoped session would see only its own ledger rows.
		if err := run(playerSess(f.tenantA, f.acctA1), req); !errors.Is(err, ErrUsageScope) {
			t.Fatalf("a player-scoped session must be refused, got %v", err)
		}
		for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
			s := staffSess(f.tenantA, f.staffA)
			s.iso = iso
			if err := run(s, req); !errors.Is(err, ErrUsageIsolation) {
				t.Fatalf("%s must be refused, got %v", iso, err)
			}
		}
		// Bad requests.
		bad := req
		bad.Scope = LimitScopePlatformPerson
		if err := run(staffSess(f.tenantA, f.staffA), bad); err == nil {
			t.Fatal("platform_person is not a usage scope")
		}
		bad = req
		bad.Windows = nil
		if err := run(staffSess(f.tenantA, f.staffA), bad); err == nil {
			t.Fatal("no windows must be refused")
		}
		bad = req
		bad.Windows = []WindowSpec{{Period: PeriodCalendarDay}}
		if err := run(staffSess(f.tenantA, f.staffA), bad); err == nil {
			t.Fatal("a calendar window without a timezone must be refused")
		}
		bad = req
		bad.Windows = []WindowSpec{{Period: PeriodCalendarDay, CalendarTZ: "Not/AZone"}}
		if err := run(staffSess(f.tenantA, f.staffA), bad); err == nil {
			t.Fatal("an unknown timezone must fail closed")
		}
	})

	t.Run("ConcurrentPostings_EveryReadIsOneConsistentSnapshot", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		const writers, perWriter = 4, 25
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < perWriter; j++ {
					// One transaction, three entries: 60 + 40 debit on player_cash, 100 credit to the house. A torn read
					// would show 60 or 40 and break the multiple-of-100 invariant.
					w.s3Post(t, f.tenantA, s3Tx{typ: "casino_bet", asset: "USD", legs: []s3Leg{cashDr(a, "60"), cashDr(a, "40"), houseCr("100")}})
				}
			}()
		}
		var readers sync.WaitGroup
		readErr := make(chan error, 1)
		reads := 0
		readers.Add(1)
		go func() {
			defer readers.Done()
			last := int64(-1)
			for {
				select {
				case <-stop:
					return
				default:
				}
				var u StakeUsage
				err := do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
					var err error
					u, err = MeasureStakeUsage(ctx, tx, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeTenantPerson, AssetCode: "USD",
						Windows: []WindowSpec{{Period: PeriodRolling24h}, {Period: PeriodRolling7d}}})
					return err
				})
				if err != nil {
					readErr <- err
					return
				}
				reads++
				wg0, lo0, wg1 := u.Windows[0].Wager.Account, u.Windows[0].Loss.Account, u.Windows[1].Wager.Account
				if wg0.Int64()%100 != 0 || wg0.Cmp(lo0) != 0 || wg0.Cmp(wg1) != 0 || wg0.Cmp(u.Windows[0].Wager.Person) != 0 {
					readErr <- fmt.Errorf("torn or inconsistent read: wager %s loss %s wager7d %s person %s", wg0, lo0, wg1, u.Windows[0].Wager.Person)
					return
				}
				if wg0.Int64() < last {
					readErr <- fmt.Errorf("usage went backwards: %d after %d", wg0.Int64(), last)
					return
				}
				last = wg0.Int64()
			}
		}()
		wg.Wait()
		close(stop)
		readers.Wait()
		select {
		case err := <-readErr:
			t.Fatal(err)
		default:
		}
		u := w.measureStake(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}})
		wantAmt(t, "final usage after all postings", u.Windows[0].Wager.Account, fmt.Sprint(writers*perWriter*100))
		if reads < 3 {
			t.Fatalf("only %d concurrent reads happened; the test did not overlap writers", reads)
		}
	})
}

// Compile-time use of time to keep the helper file's constants referenced when only some tests run.
var _ = time.Second
