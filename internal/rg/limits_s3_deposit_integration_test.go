//go:build integration

package rg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func TestRGDepositUsageS3(t *testing.T) {
	w := newS1World(t)

	t.Run("HandComputedIntentsAndLedger", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		usd := "USD"
		// In-flight: counted at the requested amount.
		p1 := w.s3Intent(t, f.tenantA, a, usd, "pending", "100", ago(hr), nil)
		w.s3Intent(t, f.tenantA, a, usd, "ambiguous", "50", ago(2*hr), nil)
		// Never money: declined / failed with no posting.
		w.s3Intent(t, f.tenantA, a, usd, "declined", "999", ago(hr), nil)
		w.s3Intent(t, f.tenantA, a, usd, "failed", "888", ago(hr), nil)
		// Succeeded at the credited amount (300 = 300; 500 requested but 450 credited).
		d300 := w.s3Deposit(t, f.tenantA, a, usd, "300", ago(3*hr))
		w.s3Intent(t, f.tenantA, a, usd, "succeeded", "300", ago(3*hr), &d300)
		d450 := w.s3Deposit(t, f.tenantA, a, usd, "450", ago(3*hr))
		w.s3Intent(t, f.tenantA, a, usd, "succeeded", "500", ago(3*hr), &d450)
		// An in-flight-looking intent that ALREADY has its ledger posting (33): it counts once, at the credited amount,
		// from the ledger side - never also as in-flight.
		d33 := w.s3Deposit(t, f.tenantA, a, usd, "33", ago(2*hr+30*time.Minute))
		w.s3Intent(t, f.tenantA, a, usd, "ambiguous", "33", ago(2*hr+30*time.Minute), &d33)
		// Inbound deposit with no intent.
		w.s3Deposit(t, f.tenantA, a, usd, "70", ago(4*hr))
		// A late credit on a failed intent: the money arrived, it counts once (ledger side only).
		d60 := w.s3Deposit(t, f.tenantA, a, usd, "60", ago(5*hr))
		w.s3Intent(t, f.tenantA, a, usd, "failed", "60", ago(6*hr), &d60)
		// A reversal does not restore capacity (a withdrawal is not a 'deposit' posting, so it cannot either).
		w.s3Post(t, f.tenantA, s3Tx{typ: "deposit_reversal", at: ago(30 * time.Minute), reverses: &d300, asset: usd, legs: []s3Leg{
			cashDr(a, "300"), {uuid.Nil, ledger.AccountPSPClearing, "credit", "300"}}})
		// 8 days old: only the 30-day window sees it. A 40-day-old pending intent is outside every window.
		w.s3Deposit(t, f.tenantA, a, usd, "1000", ago(8*day))
		w.s3Intent(t, f.tenantA, a, usd, "pending", "5", ago(40*day), nil)
		// Other accounts and assets: A2 (same Person), X (another Person), B (another tenant), EUR on A1.
		w.s3Intent(t, f.tenantA, f.acctA2, usd, "pending", "25", ago(hr), nil)
		w.s3Intent(t, f.tenantA, f.acctX, usd, "pending", "77", ago(hr), nil)
		w.s3Intent(t, f.tenantB, f.acctB, usd, "pending", "55", ago(hr), nil)
		w.s3Intent(t, f.tenantA, a, "EUR", "pending", "9", ago(hr), nil)

		at := s3T
		req := UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: usd, Windows: s3Windows("UTC"), At: &at}
		u := w.measureDeposit(t, f, f.tenantA, req)
		credited := []string{"913", "913", "1913", "913"}
		inflight := []string{"150", "150", "150", "150"}
		total := []string{"1063", "1063", "2063", "1063"}
		for i, ws := range u.Windows {
			wantAmt(t, "credited "+string(ws.Window.Period), ws.Credited.Account, credited[i])
			wantAmt(t, "in-flight "+string(ws.Window.Period), ws.InFlight.Account, inflight[i])
			wantAmt(t, "total "+string(ws.Window.Period), ws.Total.Account, total[i])
		}

		// The candidate intent is excluded (the caller adds it once as Amount).
		ex := req
		ex.ExcludeIntentID = &p1
		ue := w.measureDeposit(t, f, f.tenantA, ex)
		wantAmt(t, "in-flight without the candidate", ue.Windows[0].InFlight.Account, "50")
		wantAmt(t, "total without the candidate", ue.Windows[0].Total.Account, "963")

		// tenant_person: joined by person_id through the tenant's accounts: A1 + A2; X and B never.
		pr := req
		pr.Scope = LimitScopeTenantPerson
		up := w.measureDeposit(t, f, f.tenantA, pr)
		wantAmt(t, "person in-flight", up.Windows[0].InFlight.Person, "175")
		wantAmt(t, "person total", up.Windows[0].Total.Person, "1088")
		wantAmt(t, "anchor total unchanged", up.Windows[0].Total.Account, "1063")
		if len(up.AccountIDs) != 2 {
			t.Fatalf("Person set: %v", up.AccountIDs)
		}
		wantAmt(t, "person 30d total", up.Windows[2].Total.Person, "2088")

		// EUR is its own asset.
		eu := req
		eu.AssetCode = "EUR"
		ueu := w.measureDeposit(t, f, f.tenantA, eu)
		wantAmt(t, "EUR in-flight", ueu.Windows[0].InFlight.Account, "9")
		wantAmt(t, "EUR credited", ueu.Windows[0].Credited.Account, "0")

		// Tenant isolation: tenant B's session cannot read tenant A's account (not found, never zero).
		cross := req
		cross.TenantID = f.tenantB
		err := do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureDepositUsage(ctx, tx, cross)
			return err
		})
		if !errors.Is(err, ErrUsageAccountNotFound) {
			t.Fatalf("cross-tenant deposit usage must be not found, got %v", err)
		}
		// And the guards of the stake path apply here too.
		err = do(w.rt, playerSess(f.tenantA, a), func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureDepositUsage(ctx, tx, req)
			return err
		})
		if !errors.Is(err, ErrUsageScope) {
			t.Fatalf("player-scoped session: %v", err)
		}
		rr := staffSess(f.tenantA, f.staffA)
		rr.iso = pgx.RepeatableRead
		err = do(w.rt, rr, func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureDepositUsage(ctx, tx, req)
			return err
		})
		if !errors.Is(err, ErrUsageIsolation) {
			t.Fatalf("repeatable read: %v", err)
		}

		// The audit inputs carry what a replay needs.
		in := u.AuditInputs()
		for _, k := range []string{"as_of", "scope", "asset_code", "account_ids", "window_starts", "ledger_type", "in_flight_statuses"} {
			if _, ok := in[k]; !ok {
				t.Fatalf("audit inputs lack %s: %v", k, in)
			}
		}
	})

	t.Run("TTLSweepToFailedReleasesUsage_IsNotImplemented_StaleIntentsKeepCounting", func(t *testing.T) {
		// ADR 0115 1.4 / S3: a pending intent older than the configured TTL is moved to 'failed' by the payments
		// sweeper, which releases its usage. That sweep is NOT IMPLEMENTED in S3 (deviation recorded in the ADR);
		// the measurement is conservative: an old pending intent keeps counting inside its window. When a status
		// becomes 'failed' (by whatever component), its usage is released - pinned here.
		f := w.newFix(t)
		a := f.acctA1
		w.s3Intent(t, f.tenantA, a, "USD", "pending", "40", ago(23*hr), nil)
		req := UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T}
		wantAmt(t, "stale pending still counts", w.measureDeposit(t, f, f.tenantA, req).Windows[0].Total.Account, "40")
		wantOK(t, "intent fails", w.owner.WithTenant(context.Background(), f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE deposit_intents SET status = 'failed' WHERE player_account_id = $1 AND status = 'pending'`, a)
			return err
		}))
		wantAmt(t, "failed intent releases its usage", w.measureDeposit(t, f, f.tenantA, req).Windows[0].Total.Account, "0")
	})

	t.Run("UndeclaredDepositLegFailsClosed", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		w.s3Post(t, f.tenantA, s3Tx{typ: "deposit", at: ago(hr), asset: "USD", legs: []s3Leg{
			{uuid.Nil, ledger.AccountPSPClearing, "debit", "10"}, {a, ledger.AccountManualAdjustment, "credit", "10"}}})
		err := do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			_, err := MeasureDepositUsage(ctx, tx, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD",
				Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &s3T})
			return err
		})
		if !errors.Is(err, ErrUnrecognizedUsageLeg) {
			t.Fatalf("an undeclared deposit leg must fail closed: %v", err)
		}
	})

	t.Run("CalendarDayInTheHavanaAmbiguousHour_CountsThePostingOfTheFirstHour", func(t *testing.T) {
		// End to end through a ledger posting: Havana 2024-11-03 local 00:00 occurs twice (04:00Z and 05:00Z). A
		// deposit at 04:20Z (local 00:20, first occurrence) belongs to the local day; measured at 05:30Z (local 00:30,
		// second occurrence) the naive start (05:00Z) would miss it.
		f := w.newFix(t)
		a := f.acctA1
		w.s3Deposit(t, f.tenantA, a, "USD", "42", time.Date(2024, 11, 3, 4, 20, 0, 0, time.UTC))
		at := time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC)
		u := w.measureDeposit(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD",
			Windows: []WindowSpec{{Period: PeriodCalendarDay, CalendarTZ: "America/Havana"}}, At: &at})
		wantAmt(t, "calendar day in an ambiguous midnight", u.Windows[0].Credited.Account, "42")
		if !u.Windows[0].Start.Equal(time.Date(2024, 11, 3, 4, 0, 0, 0, time.UTC)) {
			t.Fatalf("start %s", u.Windows[0].Start)
		}
	})

	t.Run("DailyCreditCheck", func(t *testing.T) {
		f := w.newFix(t)
		a := f.acctA1
		usd := "USD"
		ok := w.s3Deposit(t, f.tenantA, a, usd, "300", ago(3*hr))
		okI := w.s3Intent(t, f.tenantA, a, usd, "succeeded", "300", ago(3*hr), &ok)
		d450 := w.s3Deposit(t, f.tenantA, a, usd, "450", ago(3*hr))
		amtI := w.s3Intent(t, f.tenantA, a, usd, "succeeded", "500", ago(3*hr), &d450)
		d60 := w.s3Deposit(t, f.tenantA, a, usd, "60", ago(5*hr))
		unsI := w.s3Intent(t, f.tenantA, a, usd, "failed", "60", ago(6*hr), &d60)
		noPostI := w.s3Intent(t, f.tenantA, a, usd, "succeeded", "111", ago(2*hr), nil)
		dOther := w.s3Deposit(t, f.tenantA, f.acctA2, usd, "70", ago(2*hr))
		acctI := w.s3Intent(t, f.tenantA, a, usd, "succeeded", "70", ago(2*hr), &dOther)
		dEUR := w.s3Deposit(t, f.tenantA, a, "EUR", "80", ago(2*hr))
		assetI := w.s3Intent(t, f.tenantA, a, usd, "succeeded", "80", ago(2*hr), &dEUR)
		w.s3Intent(t, f.tenantA, a, usd, "pending", "5", ago(hr), nil) // not a posting, not succeeded: nothing to compare

		var got []CreditMismatch
		wantOK(t, "credit check", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = CheckDepositCredits(ctx, tx, f.tenantA, ago(10*day), s3T.Add(hr), 0)
			return err
		}))
		want := map[uuid.UUID]CreditMismatchKind{
			amtI: MismatchAmount, unsI: MismatchPostingOnUnsettledIntent, noPostI: MismatchSucceededWithoutPosting,
			acctI: MismatchAccount, assetI: MismatchAsset,
		}
		if len(got) != len(want) {
			t.Fatalf("want %d mismatches, got %d: %+v", len(want), len(got), got)
		}
		for _, m := range got {
			if want[m.IntentID] != m.Kind {
				t.Fatalf("intent %s: want %q, got %q", m.IntentID, want[m.IntentID], m.Kind)
			}
			if m.IntentID == okI {
				t.Fatal("a matching intent must not be reported")
			}
		}
		for _, m := range got {
			if m.IntentID == amtI && (m.IntentAmount != "500" || m.CreditedAmount != "450") {
				t.Fatalf("amount mismatch must carry both figures: %+v", m)
			}
		}
		// Range and tenant scoping: another tenant sees none of it; an empty range is refused.
		wantOK(t, "other tenant", do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
			g, err := CheckDepositCredits(ctx, tx, f.tenantB, ago(10*day), s3T.Add(hr), 0)
			if err != nil || len(g) != 0 {
				t.Fatalf("tenant B: %v %v", g, err)
			}
			return nil
		}))
		wantOK(t, "range", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			g, err := CheckDepositCredits(ctx, tx, f.tenantA, ago(30*time.Minute), s3T.Add(hr), 0)
			if err != nil || len(g) != 0 {
				t.Fatalf("a range holding no intents: %v %v", g, err)
			}
			if _, err := CheckDepositCredits(ctx, tx, f.tenantA, s3T, s3T, 0); err == nil {
				t.Fatal("an empty range must be refused")
			}
			lim, err := CheckDepositCredits(ctx, tx, f.tenantA, ago(10*day), s3T.Add(hr), 2)
			if err != nil || len(lim) != 2 {
				t.Fatalf("limit: %v %v", lim, err)
			}
			return nil
		}))
	})
}

// TestRGSessionUsageS3 and TestRGEffectiveLimitInputsS3 share a world with the deposit tests' pattern.
func TestRGSessionUsageS3(t *testing.T) {
	w := newS1World(t)
	f := w.newFix(t)
	a := f.acctA1
	exp := s3T.Add(30 * day)
	s1, s2, s3, o1 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// Chain 1: s1 (root, T-5h) -> s2 (T-4h) -> s3 (T-1h, open leaf). Insert the leaf first (FK on replaced_by).
	w.s3Session(t, f.tenantA, a, s3, ago(hr), nil, nil, exp)
	w.s3Session(t, f.tenantA, a, s2, ago(4*hr), &s3, nil, exp)
	w.s3Session(t, f.tenantA, a, s1, ago(5*hr), &s2, nil, exp)
	// Chain 2: one open session started 9h ago (older root).
	w.s3Session(t, f.tenantA, a, o1, ago(9*hr), nil, nil, exp)
	// A revoked chain (older still) and an expired one must not count as open.
	rev := s3T.Add(-time.Hour)
	w.s3Session(t, f.tenantA, a, uuid.New(), ago(20*hr), nil, &rev, exp)
	w.s3Session(t, f.tenantA, a, uuid.New(), ago(30*hr), nil, nil, ago(10*hr))
	// The Person's other account has an OLDER open chain: principal_only coverage must not see it (RLS).
	w.s3Session(t, f.tenantA, f.acctA2, uuid.New(), ago(40*hr), nil, nil, exp)

	run := func(s sess, r SessionRequest) (SessionUsage, error) {
		var u SessionUsage
		err := do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			u, err = MeasureSessionUsage(ctx, tx, r)
			return err
		})
		return u, err
	}
	own := sess{tenant: f.tenantA.String(), principal: a.String()}
	at := s3T
	u, err := run(own, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, At: &at})
	wantOK(t, "session usage", err)
	if !u.ChainRootStartedAt.Equal(ago(5*hr)) || u.ElapsedSeconds != 5*3600 || u.Coverage != SessionCoveragePrincipalOnly {
		t.Fatalf("chain root / elapsed: %+v", u)
	}
	// Measured from the middle of the chain gives the same root.
	u2, err := run(own, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s2, At: &at})
	wantOK(t, "mid-chain", err)
	if !u2.ChainRootStartedAt.Equal(ago(5 * hr)) {
		t.Fatalf("mid-chain root %s", u2.ChainRootStartedAt)
	}
	// Concurrent chains: the earliest OPEN root is chain 2's (9h); revoked / expired / other-account chains do not count.
	ag, err := run(own, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, AggregateOpenChains: true, At: &at})
	wantOK(t, "aggregate", err)
	if ag.AggregatedRootStartedAt == nil || !ag.AggregatedRootStartedAt.Equal(ago(9*hr)) || *ag.AggregatedElapsed != 9*3600 || ag.OpenChains != 2 {
		t.Fatalf("aggregation: %+v", ag)
	}
	// Elapsed is floored to whole seconds.
	frac := s3T.Add(1500 * time.Millisecond)
	uf, err := run(own, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, At: &frac})
	wantOK(t, "floored", err)
	if uf.ElapsedSeconds != 5*3600+1 {
		t.Fatalf("floored seconds %d", uf.ElapsedSeconds)
	}
	// An instant before the root (clock skew / replay) is zero seconds, never negative.
	early := ago(6 * hr)
	ue2, err := run(own, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, At: &early})
	wantOK(t, "before the root", err)
	if ue2.ElapsedSeconds != 0 {
		t.Fatalf("elapsed before the root must be 0, got %d", ue2.ElapsedSeconds)
	}
	// Fail closed (never "zero elapsed"): another principal, the wrong account, an unknown session, another tenant.
	if _, err := run(sess{tenant: f.tenantA.String(), principal: f.acctA2.String()}, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, At: &at}); !errors.Is(err, ErrSessionChainNotFound) {
		t.Fatalf("a foreign principal must not read the chain: %v", err)
	}
	if _, err := run(sess{tenant: f.tenantA.String(), principal: f.acctA2.String()}, SessionRequest{TenantID: f.tenantA, PlayerAccountID: f.acctA2, SessionID: s3, At: &at}); !errors.Is(err, ErrSessionChainNotFound) {
		t.Fatalf("another account's session id: %v", err)
	}
	if _, err := run(own, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: uuid.New(), At: &at}); !errors.Is(err, ErrSessionChainNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := run(sess{tenant: f.tenantB.String(), principal: a.String()}, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, At: &at}); !errors.Is(err, ErrUsageScope) {
		t.Fatalf("another tenant's session scope: %v", err)
	}
	if _, err := run(sess{tenant: f.tenantA.String(), player: a.String(), principal: a.String()}, SessionRequest{TenantID: f.tenantA, PlayerAccountID: a, SessionID: s3, At: &at}); !errors.Is(err, ErrUsageScope) {
		t.Fatalf("a player-scoped session: %v", err)
	}
	if _, err := run(own, SessionRequest{}); err == nil {
		t.Fatal("an empty request must be refused")
	}
}

func TestRGEffectiveLimitInputsS3(t *testing.T) {
	w := newS1World(t)
	f := w.newFix(t)
	eur := strp("EUR")
	// A non-synthetic floor: tenant_person scope, Berlin calendar, week starting Wednesday, bonus counted, loss floor attested.
	w.floor(t, midFloor(f.jur, "wager").Clone(rgfix.Spec{"calendar_tz": "Europe/Berlin", "week_start": 3,
		"allowed_periods": []string{"rolling_24h", "calendar_day", "calendar_week"}, "count_bonus_funded": true, "increase_cooldown": "00:00:01",
		"increase_requires_reconfirmation": false}))
	w.floor(t, midFloor(f.jur, "loss").Clone(rgfix.Spec{"calendar_tz": "Europe/Berlin", "week_start": 3,
		"allowed_periods": []string{"rolling_24h"}, "loss_may_go_negative": true, "loss_may_go_negative_none": false}))
	w.floor(t, midFloor(f.jur, "deposit").Clone(rgfix.Spec{"calendar_tz": "Europe/Berlin", "pending_intent_ttl": "00:30:00", "pending_intent_ttl_none": false}))
	w.floor(t, midFloor(f.jur, "session_duration"))

	rolling, day := PeriodRolling24h, PeriodCalendarDay
	// Player limits in BOTH scopes and two periods (including one the config no longer treats as its scope).
	pa := playerSess(f.tenantA, f.acctA1)
	for _, c := range []chg{
		{scope: "account", period: strp(string(rolling)), value: strp("700")},
		{scope: "tenant_person", period: strp(string(rolling)), value: strp("900")},
		{scope: "tenant_person", period: strp(string(day)), value: strp("300")},
	} {
		c.tenant, c.acct, c.kind, c.asset = f.tenantA, f.acctA1, "wager", eur
		_, err := w.change(pa, c)
		wantOK(t, "player limit", err)
	}
	// Another kind, another asset: must not leak into the wager/EUR inputs.
	_, err := w.change(pa, chg{tenant: f.tenantA, acct: f.acctA1, scope: "account", kind: "wager", asset: strp("USD"), period: strp(string(rolling)), value: strp("1")})
	wantOK(t, "other asset", err)

	var in EffectiveLimitInputs
	wantOK(t, "read inputs", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, err = ReadEffectiveLimitInputs(ctx, tx, f.tenantA, f.acctA1, ControlWager, eur)
		return err
	}))
	c := in.Config
	if !c.Found || !c.Enabled || !c.Mandatory || c.Scope != LimitScopeTenantPerson || c.CalendarTZ == nil || *c.CalendarTZ != "Europe/Berlin" ||
		c.WeekStart == nil || *c.WeekStart != 3 || c.CountBonusFunded == nil || !*c.CountBonusFunded || c.Ceiling == nil || *c.Ceiling != "10000" || len(c.VersionIDs) == 0 {
		t.Fatalf("composed config: %+v", c)
	}
	if len(c.AllowedPeriods) != 3 {
		t.Fatalf("allowed periods: %v", c.AllowedPeriods)
	}
	ws := c.Windows()
	if len(ws) != 3 || ws[0].CalendarTZ != "Europe/Berlin" || ws[2].WeekStart != 3 {
		t.Fatalf("windows derived from the config: %+v", ws)
	}
	got := map[string]string{}
	for _, st := range in.Realized {
		if st.Kind != ControlWager || st.AssetCode == nil || *st.AssetCode != "EUR" || st.Period == nil {
			t.Fatalf("a foreign key leaked into the inputs: %+v", st)
		}
		v := "<removed>"
		if st.Value != nil {
			v = *st.Value
		}
		got[string(st.Scope)+"/"+string(*st.Period)] = v
	}
	want := map[string]string{"account/rolling_24h": "700", "tenant_person/rolling_24h": "900", "tenant_person/calendar_day": "300"}
	if len(got) != len(want) {
		t.Fatalf("realized inputs: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("realized %s: want %s, got %v", k, v, got)
		}
	}

	// loss: the attested negative-loss field; deposit: the TTL; session: break and aggregation.
	read := func(k ControlKind, asset *string) UsageConfig {
		var cfg UsageConfig
		wantOK(t, "config "+string(k), do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			cfg, err = ReadUsageConfig(ctx, tx, f.tenantA, f.brandA1, k, asset, nil)
			return err
		}))
		return cfg
	}
	if l := read(ControlLoss, eur); l.LossMayGoNegative == nil || !*l.LossMayGoNegative {
		t.Fatalf("loss config: %+v", l)
	}
	if d := read(ControlDeposit, eur); d.PendingIntentTTLSeconds == nil || *d.PendingIntentTTLSeconds != 1800 {
		t.Fatalf("deposit config: %+v", d)
	}
	if s := read(ControlSessionDuration, nil); s.PostSessionBreakSeconds == nil || *s.PostSessionBreakSeconds != 3600 || s.ConcurrentChainAggregation == nil || !*s.ConcurrentChainAggregation {
		t.Fatalf("session config: %+v", s)
	}
	// No floor for a jurisdiction: Found=false (rg_config_missing in S4), never a zero-value permissive config.
	g := w.newFix(t)
	wantOK(t, "no floor", do(w.rt, staffSess(g.tenantA, g.staffA), func(ctx context.Context, tx pgx.Tx) error {
		cfg, err := ReadUsageConfig(ctx, tx, g.tenantA, g.brandA1, ControlWager, eur, nil)
		if err != nil {
			return err
		}
		if cfg.Found || cfg.Enabled || len(cfg.AllowedPeriods) != 0 {
			t.Fatalf("an unconfigured jurisdiction must compose as not found: %+v", cfg)
		}
		return nil
	}))
	// The tenant is the session's: another tenant's account is not found.
	err = do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReadEffectiveLimitInputs(ctx, tx, f.tenantB, f.acctA1, ControlWager, eur)
		return err
	})
	if err == nil {
		t.Fatal("another tenant's account must not be readable")
	}
}
