//go:build integration

package licensingchange

// lic25 (migration 0187, F1 of lic14 / SN-3 of lic23): the responsible-gaming rulebook (floors, ceilings, cooling-off) is selected by
// the GOVERNING BASIS of the scope, the way the KYC rulebook is (internal/operatingmarket/rulebook.go), instead of by the tenant
// licence jurisdiction alone.
//
//   - exposure checks (EvaluateAction: deposit, wager, launch) select ONE jurisdiction from the player's verified residence country,
//     brand, operation and product, read in the same transaction; a basis tenant whose scope cannot be resolved is refused (error);
//   - limit setting, options and operator bounds have no operation or product: they use the STRICTEST FOLD of every jurisdiction the
//     tenant relies on (licence plus approved bases), the same fold rg_limit_config_compose applies to its own rows; a relied
//     jurisdiction without a floor fails closed for increases and removals, never for decreases, time-outs or cancels;
//   - a tenant with no approved basis keeps the pre-lic25 behaviour exactly.
//
// Every assertion about what the application can do runs as igaming_runtime; the owner pool provisions fixtures. No jurisdiction,
// country or value here is a statement about any real regime.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

const eur = "EUR"

func (w *world) jurCode(jid uuid.UUID) string {
	w.t.Helper()
	var code string
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT code FROM jurisdictions WHERE id = $1`, jid).Scan(&code)
	}); err != nil {
		w.t.Fatal(err)
	}
	return code
}

// rgFloors puts a floor for deposit, loss and wager (EUR) into the jurisdiction. ceilings maps a kind to a numeric ceiling ("" = none);
// cooldown is a postgres interval; reconfirm sets increase_requires_reconfirmation.
func (w *world) rgFloors(jid uuid.UUID, ceilings map[string]string, cooldown string, reconfirm bool) {
	w.t.Helper()
	w.rgFloorsTZ(jid, ceilings, cooldown, reconfirm, "UTC")
}

// rgFloorsTZ is rgFloors with the floors' calendar_tz (lic27a: relied-on jurisdictions that disagree on it cannot be folded).
func (w *world) rgFloorsTZ(jid uuid.UUID, ceilings map[string]string, cooldown string, reconfirm bool, tz string) {
	w.t.Helper()
	rgfix.EnsureTestEnvironment(w.t, w.ownerURL)
	code := w.jurCode(jid)
	for _, kind := range []string{"deposit", "loss", "wager"} {
		over := rgfix.Spec{"increase_cooldown": cooldown, "increase_requires_reconfirmation": reconfirm, "calendar_tz": tz}
		if c := ceilings[kind]; c != "" {
			over["ceiling"], over["ceiling_none"] = c, false
		}
		if kind == "deposit" {
			over["pending_intent_ttl"], over["pending_intent_ttl_none"] = nil, true // the only accepted shape (config validation)
		}
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertFloor(ctx, tx, rgfix.EnforcedFloor(code, kind, eur, over), rgfix.DefaultSteps())
			return err
		}); err != nil {
			w.t.Fatalf("floor %s in %s: %v", kind, code, err)
		}
	}
}

func (w *world) rgOn(tid uuid.UUID) {
	w.t.Helper()
	rgfix.EnsureTestEnvironment(w.t, w.ownerURL)
	rgfix.Activate(w.t, w.owner, tid, nil, rgfix.Staff(w.t, w.owner, tid))
}

func (w *world) rgEval(tid, brand uuid.UUID, p resident, a rg.Action, amount int64) (rg.Decision, error) {
	w.t.Helper()
	var d rg.Decision
	err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = rg.EvaluateAction(ctx, tx, rg.ActionParams{TenantID: tid, BrandID: brand, PlayerAccountID: p.id, Action: a, AssetCode: eur, Amount: amount})
		return e
	})
	return d, err
}

func (w *world) wantDenied(what string, d rg.Decision, err error, code, limit string) {
	w.t.Helper()
	if err != nil {
		w.t.Fatalf("%s: unexpected error %v", what, err)
	}
	if d.Allowed || d.Code != code {
		w.t.Fatalf("%s: want denied %q, got allowed=%v code=%q msg=%q", what, code, d.Allowed, d.Code, d.Message)
	}
	if limit != "" && d.Figures["limit"] != limit {
		w.t.Fatalf("%s: want limit %s, got %v", what, limit, d.Figures["limit"])
	}
}

func (w *world) wantAllowed(what string, d rg.Decision, err error) {
	w.t.Helper()
	if err != nil || !d.Allowed {
		w.t.Fatalf("%s: want allowed, got %+v / %v", what, d, err)
	}
}

func (w *world) wantUnavailable(what string, d rg.Decision, err error) {
	w.t.Helper()
	if !errors.Is(err, rg.ErrEvaluationUnavailable) || d.Allowed {
		w.t.Fatalf("%s: want ErrEvaluationUnavailable (deny), got allowed=%v code=%q err=%v", what, d.Allowed, d.Code, err)
	}
}

func wageringBasis(tid uuid.UUID, jid uuid.UUID, product string) OperatingBasisSetPayload {
	p := noLicence(tid, nil, jid, 30)
	p.OperationCode = "wagering"
	p.ProductCode = &product
	return p
}

// ---- exposure checks: one jurisdiction, selected by the scope -----------------------------------------------------------------

func TestLic25_Scoped_DepositAndWager_FollowTheGoverningBasis(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction()
	w.rgFloors(jl, map[string]string{"deposit": "1000", "wager": "1000"}, "00:00:02", false)
	w.rgFloors(jb, map[string]string{"deposit": "100", "wager": "50"}, "00:00:02", false)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")      // deposit, MT -> jb
	w.widen(KindOperatingBasisSet, wageringBasis(tid, jb, "casino"), "documented_basis") // casino wagering, MT -> jb
	w.rgOn(tid)
	mt, de := w.seedResident(tid, b, bCountry), w.seedResident(tid, b, "DE")

	// A player whose scope is governed by the basis gets the BASIS jurisdiction's ceiling, not the licence's.
	d, err := w.rgEval(tid, b, mt, rg.ActionDeposit, 150)
	w.wantDenied("basis scope deposit above the basis ceiling", d, err, rg.CodeDepositLimitReached, "100")
	d, err = w.rgEval(tid, b, mt, rg.ActionDeposit, 90)
	w.wantAllowed("basis scope deposit under the basis ceiling", d, err)
	d, err = w.rgEval(tid, b, mt, rg.ActionCasinoWager, 60)
	w.wantDenied("basis scope casino wager above the basis ceiling", d, err, rg.CodeWagerLimitReached, "50")
	d, err = w.rgEval(tid, b, mt, rg.ActionCasinoLaunch, 0)
	w.wantAllowed("basis scope launch (casino wagering)", d, err)

	// A scope with no basis row falls back to the tenant's own licence jurisdiction (unchanged behaviour for that scope).
	d, err = w.rgEval(tid, b, de, rg.ActionDeposit, 150)
	w.wantAllowed("licence scope deposit under the licence ceiling", d, err)
	d, err = w.rgEval(tid, b, de, rg.ActionDeposit, 1500)
	w.wantDenied("licence scope deposit above the licence ceiling", d, err, rg.CodeDepositLimitReached, "1000")
	// The basis covers casino only: a sportsbook wager of the same player has no row and falls back to the licence.
	d, err = w.rgEval(tid, b, mt, rg.ActionSportsbookWager, 60)
	w.wantAllowed("sportsbook (no basis row) falls back to the licence jurisdiction", d, err)
}

func TestLic25_Scoped_BasisOnlyTenant_UsesTheBasisJurisdiction_AndFailsClosedWhenUnresolvable(t *testing.T) {
	w := newWorld(t)
	jb := w.jurisdiction()
	w.rgFloors(jb, map[string]string{"deposit": "100", "wager": "50"}, "00:00:02", false)
	tid := w.tenant("under_platform_licence") // NO licence
	b := w.brand(tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis") // deposit, MT -> jb only
	w.rgOn(tid)
	mt := w.seedResident(tid, b, bCountry)

	d, err := w.rgEval(tid, b, mt, rg.ActionDeposit, 150)
	w.wantDenied("no licence: the basis jurisdiction governs", d, err, rg.CodeDepositLimitReached, "100")
	d, err = w.rgEval(tid, b, mt, rg.ActionDeposit, 90)
	w.wantAllowed("no licence: under the basis ceiling", d, err)

	// FAIL CLOSED (new exposure): no verified residence; a residence country without a row and without a licence; an operation or
	// product the basis does not cover. An error, never "no configuration".
	d, err = w.rgEval(tid, b, w.seedResident(tid, b, ""), rg.ActionDeposit, 1)
	w.wantUnavailable("no verified residence country", d, err)
	d, err = w.rgEval(tid, b, w.seedResident(tid, b, "DE"), rg.ActionDeposit, 1)
	w.wantUnavailable("residence country without a basis row and no licence", d, err)
	d, err = w.rgEval(tid, b, mt, rg.ActionCasinoWager, 1)
	w.wantUnavailable("wagering has no basis row and there is no licence", d, err)
	d, err = w.rgEval(tid, b, mt, rg.ActionCasinoLaunch, 0)
	w.wantUnavailable("launch has no basis row and there is no licence", d, err)

	w.widen(KindOperatingBasisSet, wageringBasis(tid, jb, "casino"), "documented_basis")
	d, err = w.rgEval(tid, b, mt, rg.ActionCasinoWager, 60)
	w.wantDenied("wagering basis added: the basis wager ceiling applies", d, err, rg.CodeWagerLimitReached, "50")

	// A withdrawn basis leaves no approved basis at all: the pre-lic25 path (no licence = no configuration) with the switch ON.
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: "wagering", ProductCode: sp("casino")}, "basis_withdrawn")
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	d, err = w.rgEval(tid, b, mt, rg.ActionDeposit, 1)
	w.wantDenied("no approved basis and no licence: rg_config_missing, exactly as before", d, err, rg.CodeConfigMissing, "")
}

func TestLic25_Scoped_NoBasis_Unchanged(t *testing.T) {
	w := newWorld(t)
	jl := w.jurisdiction()
	w.rgFloors(jl, map[string]string{"deposit": "100"}, "00:00:02", false)
	lic := w.tenant("under_platform_licence")
	lb := w.brand(lic)
	w.licence(jl, "platform", lic)
	w.rgOn(lic)
	// Licence only: the licence jurisdiction governs whatever the player's country (and even with no residence evidence at all).
	for _, c := range []string{bCountry, "DE", ""} {
		d, err := w.rgEval(lic, lb, w.seedResident(lic, lb, c), rg.ActionDeposit, 150)
		w.wantDenied("licence only, residence "+c, d, err, rg.CodeDepositLimitReached, "100")
	}
	// No licence, no basis, switch ON: rg_config_missing (a decision, not an error), as before.
	bare := w.tenant("under_platform_licence")
	bb := w.brand(bare)
	w.rgOn(bare)
	d, err := w.rgEval(bare, bb, w.seedResident(bare, bb, bCountry), rg.ActionDeposit, 1)
	w.wantDenied("no licence, no basis", d, err, rg.CodeConfigMissing, "")
}

// ---- limit setting: the strictest fold of every relied-on jurisdiction -----------------------------------------------------------

type rgPlayerCalls struct {
	w   *world
	tid uuid.UUID
	p   resident
}

func (c rgPlayerCalls) set(kind rg.ControlKind, period rg.PeriodKind, value *string) (rg.ChangeResult, error) {
	var res rg.ChangeResult
	a := eur
	err := c.w.rt.WithPlayerScope(context.Background(), c.tid, c.p.id, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		res, e = rg.NewLimits(rg.LimitsConfig{}).SetLimit(ctx, tx, rg.SetLimitInput{TenantID: c.tid, PlayerAccountID: c.p.id,
			Limit: rg.LimitInput{Kind: kind, AssetCode: &a, Period: &period, Value: value, IdempotencyKey: uuid.NewString()}})
		// Player-scope session shape: the player GUC must be restored after the tenant-scoped rulebook reads. (A refused change aborts
		// the transaction, which cannot be queried further; the refusal itself is asserted by the caller.)
		if e != nil {
			return e
		}
		var got string
		if qe := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.player_account_id', true), '')`).Scan(&got); qe != nil {
			return qe
		}
		if got != c.p.id.String() {
			c.w.t.Fatalf("the player scope was not restored after SetLimit: %q", got)
		}
		return e
	})
	return res, err
}

func (c rgPlayerCalls) resolve(id uuid.UUID, intent rg.ChangeKind) (rg.ChangeResult, error) {
	var res rg.ChangeResult
	err := c.w.rt.WithPlayerScope(context.Background(), c.tid, c.p.id, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		res, e = rg.NewLimits(rg.LimitsConfig{}).ResolvePending(ctx, tx, rg.ResolveInput{TenantID: c.tid, PlayerAccountID: c.p.id, ChangeID: id, Intent: intent, IdempotencyKey: uuid.NewString()})
		return e
	})
	return res, err
}

func (c rgPlayerCalls) timeOut(hours int) error {
	return c.w.rt.WithPlayerScope(context.Background(), c.tid, c.p.id, func(ctx context.Context, tx pgx.Tx) error {
		_, e := rg.NewLimits(rg.LimitsConfig{}).CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: c.tid, PlayerAccountID: c.p.id, DurationHours: hours, IdempotencyKey: uuid.NewString()})
		if e != nil {
			return e
		}
		var got string
		if qe := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.player_account_id', true), '')`).Scan(&got); qe != nil {
			return qe
		}
		if got != c.p.id.String() {
			c.w.t.Fatalf("the player scope was not restored after CreateTimeOut: %q", got)
		}
		return nil
	})
}

func wantRefusal(t *testing.T, what string, err error, code rg.RefusalCode) {
	t.Helper()
	rf, ok := rg.IsRefusal(err)
	if !ok || rf.Code != code {
		t.Fatalf("%s: want refusal %q, got %v", what, code, err)
	}
}

func str(s string) *string { return &s }

func TestLic25_LimitSetting_UsesTheStrictestFoldOfEveryReliedJurisdiction(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction()
	w.rgFloors(jl, map[string]string{"wager": "1000"}, "00:00:02", false)
	w.rgFloors(jb, map[string]string{"wager": "100"}, "00:00:10", false)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	c := rgPlayerCalls{w, tid, w.seedResident(tid, b, bCountry)}

	// The ceiling is the minimum over the licence and the basis jurisdiction (the licence alone would accept 500).
	_, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("500"))
	wantRefusal(t, "initial value above the strictest ceiling", err, rg.RefusedAboveCeiling)
	r, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("80"))
	if err != nil || r.Change.ChangeKind != rg.ChangeInitial {
		t.Fatalf("an initial value under the strictest ceiling applies at once: %+v / %v", r.Change, err)
	}
	if r, err = c.set(rg.ControlWager, rg.PeriodRolling24h, str("60")); err != nil || r.Change.ChangeKind != rg.ChangeDecrease {
		t.Fatalf("a decrease applies at once: %+v / %v", r.Change, err)
	}
	// The cooling-off is the LONGEST of the relied jurisdictions (10s, not the licence's 2s).
	inc, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("90"))
	if err != nil || !inc.Change.Pending() {
		t.Fatalf("an increase is pending: %+v / %v", inc.Change, err)
	}
	if got := inc.Change.EffectiveAt.Sub(inc.Change.RequestedAt); got < 9*time.Second {
		t.Fatalf("cooling-off %v: the strictest (10s) of the relied jurisdictions must apply, not the licence's 2s", got)
	}
	if _, err := c.resolve(inc.Change.ID, rg.ChangeCancelPending); err != nil {
		t.Fatalf("cancelling a pending increase is never blocked: %v", err)
	}
}

func TestLic25_LimitSetting_RelianceWithoutAFloor_FailsClosedForIncreasesOnly(t *testing.T) {
	w := newWorld(t)
	jl, jc := w.jurisdiction(), w.jurisdiction() // jc will have NO floor
	w.rgFloors(jl, map[string]string{"wager": "1000"}, "00:00:02", true)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	c := rgPlayerCalls{w, tid, w.seedResident(tid, b, bCountry)}

	// Licence only: an increase is accepted (control) and left pending (a confirm is required).
	if _, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("50")); err != nil {
		t.Fatal(err)
	}
	pend, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("70"))
	if err != nil || !pend.Change.Pending() {
		t.Fatalf("control: %+v / %v", pend.Change, err)
	}
	time.Sleep(2200 * time.Millisecond) // the licence floor's 2s cooling-off

	// The tenant now relies on a jurisdiction that has no floor.
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jc, 30), "documented_basis")
	_, err = c.set(rg.ControlWager, rg.PeriodRolling24h, str("80"))
	wantRefusal(t, "increase while a relied jurisdiction has no floor", err, rg.RefusedConfigMissing)
	_, err = c.set(rg.ControlWager, rg.PeriodRolling24h, nil)
	wantRefusal(t, "removal while a relied jurisdiction has no floor", err, rg.RefusedConfigMissing)
	_, err = c.resolve(pend.Change.ID, rg.ChangeConfirm)
	wantRefusal(t, "confirming an increase while a relied jurisdiction has no floor", err, rg.RefusedConfigMissing)
	// Never blocked: a decrease, an initial value on another period, a cancel of the pending increase, a time-out.
	if r, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("40")); err != nil || r.Change.ChangeKind != rg.ChangeDecrease {
		t.Fatalf("a decrease must not be blocked: %+v / %v", r.Change, err)
	}
	if r, err := c.set(rg.ControlWager, rg.PeriodRolling7d, str("500")); err != nil || r.Change.ChangeKind != rg.ChangeInitial {
		t.Fatalf("an initial value (ceiling of the jurisdictions that have a floor still applies) must not be blocked: %+v / %v", r.Change, err)
	}
	if _, err := c.set(rg.ControlWager, rg.PeriodRolling30d, str("5000")); err == nil {
		t.Fatal("the ceiling of the jurisdictions that DO have a floor must keep applying")
	}
	if err := c.timeOut(24); err != nil {
		t.Fatalf("a time-out must not be blocked: %v", err)
	}
	// Withdrawing the basis releases the reliance: the licence-only rulebook governs again and the increase is accepted.
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	if r, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("45")); err != nil || !r.Change.Pending() {
		t.Fatalf("with the basis withdrawn the increase is accepted again: %+v / %v", r.Change, err)
	}
}

func TestLic25_BasisOnlyTenant_TimeOutAndLimitsWork_ReadOptionsStillFailClosed(t *testing.T) {
	w := newWorld(t)
	jb := w.jurisdiction() // no floor
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	c := rgPlayerCalls{w, tid, w.seedResident(tid, b, bCountry)}
	if err := c.timeOut(24); err != nil {
		t.Fatalf("a time-out is protective and must work for a basis tenant without a licence: %v", err)
	}
	if r, err := c.set(rg.ControlDeposit, rg.PeriodRolling24h, str("100")); err != nil || r.Change.ChangeKind != rg.ChangeInitial {
		t.Fatalf("an initial limit (exposure-reducing) must work: %+v / %v", r.Change, err)
	}
	_, err := c.set(rg.ControlDeposit, rg.PeriodRolling24h, str("200"))
	wantRefusal(t, "increase with no floor anywhere", err, rg.RefusedConfigMissing)
	// lic27a (S-1): the options are no longer refused as a whole. Every limit kind is marked unavailable (nothing is offered, never a silent
	// weaker option) and the protective options are returned.
	var unavail rg.Options
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		unavail, e = rg.ReadOptions(ctx, tx, tid, c.p.id, eur)
		return e
	}); err != nil {
		t.Fatalf("options of a basis tenant with a relied-on jurisdiction without a floor must still be served: %v", err)
	}
	wantUnavailableOptions(t, unavail, rg.OptionUnavailableMissingFloor, rg.ControlDeposit, rg.ControlLoss, rg.ControlWager)
	// With floors everywhere the options are served and show the strictest ceiling.
	w.rgFloors(jb, map[string]string{"deposit": "300"}, "00:00:02", false)
	var opts rg.Options
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		opts, e = rg.ReadOptions(ctx, tx, tid, c.p.id, eur)
		return e
	}); err != nil {
		t.Fatalf("options with a floor in every relied jurisdiction: %v", err)
	}
	for _, l := range opts.Limits {
		if l.Kind == rg.ControlDeposit && (!l.ConfigInForce || l.Ceiling == nil || *l.Ceiling != "300") {
			t.Fatalf("deposit option: %+v", l)
		}
	}
}

// The trigger (migration 0187) decides on its own, whatever the application previewed.
func TestLic25_Trigger_SetAware_RawInsert(t *testing.T) {
	w := newWorld(t)
	jl, jb, jc := w.jurisdiction(), w.jurisdiction(), w.jurisdiction()
	w.rgFloors(jl, nil, "00:00:02", false)
	w.rgFloors(jb, nil, "00:00:10", false)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	p := w.seedResident(tid, b, bCountry)
	raw := func(value string) (kind string, requested, effective time.Time, err error) {
		err = w.rt.WithPlayerScope(context.Background(), tid, p.id, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO player_rg_limit_changes
				(tenant_id, player_account_id, scope, control_kind, asset_code, period_kind, value, source, reason_code, idempotency_key, request_hash)
				VALUES ($1, $2, 'account', 'deposit', 'EUR', 'rolling_24h', $3::numeric, 'player_self_service', 'lic25_test', $4, $5)
				RETURNING change_kind, requested_at, effective_at`, tid, p.id, value, uuid.NewString(), strings.Repeat("a", 64)).Scan(&kind, &requested, &effective)
		})
		return
	}
	if k, _, _, err := raw("50"); err != nil || k != "initial" {
		t.Fatalf("initial: %v %v", k, err)
	}
	k, rq, ef, err := raw("60")
	if err != nil || k != "increase" || ef.Sub(rq) < 9*time.Second {
		t.Fatalf("the trigger must apply the longest cooling-off of the relied jurisdictions: %v %v-%v %v", k, rq, ef, err)
	}
	// A relied jurisdiction without a floor: the trigger refuses an increase (RL023) and still admits a decrease.
	w.widen(KindOperatingBasisSet, func() OperatingBasisSetPayload { q := noLicence(tid, nil, jc, 30); q.CountryCode = "DE"; return q }(), "documented_basis")
	if _, _, _, err := raw("70"); sqlstateOf(err) != "RL023" {
		t.Fatalf("an increase with a relied jurisdiction lacking a floor must be RL023, got %v", err)
	}
	if k, _, _, err := raw("10"); err != nil || k != "decrease" {
		t.Fatalf("a decrease is never blocked: %v %v", k, err)
	}
}

// ---- isolation ------------------------------------------------------------------------------------------------------------------------

func TestLic25_Isolation_BasesOfOneTenantNeverReachAnother_AndTheSQLFunctionRefusesWrongScopes(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction()
	w.rgFloors(jl, map[string]string{"wager": "1000"}, "00:00:02", false) // jb has NO floor
	a, b2 := w.tenant("under_platform_licence"), w.tenant("under_platform_licence")
	ba, bb := w.brand(a), w.brand(b2)
	w.licence(jl, "platform", a)
	w.licence(jl, "platform", b2)
	w.widen(KindOperatingBasisSet, noLicence(a, nil, jb, 30), "documented_basis") // basis for A only
	ca, cb := rgPlayerCalls{w, a, w.seedResident(a, ba, bCountry)}, rgPlayerCalls{w, b2, w.seedResident(b2, bb, bCountry)}
	for _, c := range []rgPlayerCalls{ca, cb} {
		if _, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("50")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ca.set(rg.ControlWager, rg.PeriodRolling24h, str("60")); err == nil {
		t.Fatal("tenant A relies on jb (no floor): its increase is refused")
	}
	if r, err := cb.set(rg.ControlWager, rg.PeriodRolling24h, str("60")); err != nil || !r.Change.Pending() {
		t.Fatalf("tenant B has no basis and must be untouched by A's: %+v / %v", r.Change, err)
	}
	// The SQL function: another tenant's id, and a player-scoped session, are refused (never an empty "no basis" answer).
	if err := w.rt.WithTenant(context.Background(), b2, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT cardinality((rg_basis_jurisdictions($1, clock_timestamp())).codes)`, a).Scan(&n)
	}); sqlstateOf(err) != "RL026" {
		t.Fatalf("another tenant's id must be refused (RL026), got %v", err)
	}
	if err := w.rt.WithPlayerScope(context.Background(), a, ca.p.id, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT cardinality((rg_basis_jurisdictions($1, clock_timestamp())).codes)`, a).Scan(&n)
	}); sqlstateOf(err) != "RL026" {
		t.Fatalf("a player-scoped session must be refused (RL026), got %v", err)
	}
}

// ---- operator bounds under every relied jurisdiction -----------------------------------------------------------------------------------

func TestLic25_OperatorBounds_AreWrittenUnderEveryReliedJurisdiction(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction()
	w.rgFloors(jl, map[string]string{"deposit": "1000"}, "00:00:02", false)
	w.rgFloors(jb, map[string]string{"deposit": "800"}, "00:00:02", false)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	w.rgOn(tid)
	staff := rgfix.Staff(w.t, w.owner, tid)
	ceil := "300"
	write := func(ceiling string) error {
		return w.rt.WithPrincipalScope(context.Background(), tid, staff, func(ctx context.Context, tx pgx.Tx) error {
			cfg := rg.ConfigContent{ControlKind: rg.ControlDeposit, AssetCode: str(eur), Enabled: bp(true), Mandatory: bp(false),
				Ceiling: &rg.Attested[string]{Value: &ceiling}, IncreaseCooldownSeconds: i64(7200), IncreaseRequiresReconfirmation: bp(false), Scope: str("account"),
				AllowedPeriods: []string{"rolling_24h", "rolling_7d", "rolling_30d", "calendar_day", "calendar_week", "calendar_month"},
				CalendarTZ:     str("UTC"), WeekStart: ip(1), PendingIntentTTLSeconds: &rg.Attested[int64]{None: true},
				ReasonCode: "lic25_test", AttestationRef: rgfix.TestAttestation}
			_, e := rg.NewLimits(rg.LimitsConfig{}).WriteOperatorConfig(ctx, tx, rg.OperatorConfigInput{TenantID: tid, StaffID: staff, Content: cfg})
			return e
		})
	}
	if err := write(ceil); err != nil {
		t.Fatalf("an operator bound tighter than every relied floor is written under each: %v", err)
	}
	// Both relied jurisdictions now carry the operator row: players of either scope are bounded by it.
	p := w.seedResident(tid, b, bCountry)
	d, err := w.rgEval(tid, b, p, rg.ActionDeposit, 400)
	w.wantDenied("operator bound applies to the basis-governed scope", d, err, rg.CodeDepositLimitReached, "300")
	d, err = w.rgEval(tid, b, w.seedResident(tid, b, "DE"), rg.ActionDeposit, 400)
	w.wantDenied("operator bound applies to the licence-governed scope", d, err, rg.CodeDepositLimitReached, "300")
	// A bound that loosens ANY relied floor is refused, and nothing is written under the others (all or nothing).
	if err := write("900"); err == nil {
		t.Fatal("900 loosens the basis floor (800) and must be refused as a whole")
	}
	d, err = w.rgEval(tid, b, w.seedResident(tid, b, "DE"), rg.ActionDeposit, 400)
	w.wantDenied("the refused write changed nothing", d, err, rg.CodeDepositLimitReached, "300")
}

func bp(b bool) *bool    { return &b }
func ip(i int) *int      { return &i }
func i64(i int64) *int64 { return &i }

// ---- defence in depth of the SQL side ---------------------------------------------------------------------------------------------------

// An owner-path basis row (no executed change request) is NOT an approved basis: neither the Go selection nor the SQL function counts it,
// so it cannot make a tenant's increases fail closed (or open) differently in the trigger than in the preview.
func TestLic25_UnapprovedBasisRow_IsNotReliedOn_ByTheTriggerEither(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction() // jb has NO floor
	w.rgFloors(jl, nil, "00:00:02", false)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO operating_bases (tenant_id, country_code, operation_code, basis_kind, jurisdiction_id, determination_reference,
			determined_by_role, review_due_at, status, reason_code, policy_version)
			VALUES ($1, 'MT', 'deposit', 'no_licence_documented', $2, 'DET-owner-row', 'head_of_compliance', (now() + interval '30 days')::date, 'active', 'x', 'fixture')`, tid, jb)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := rgPlayerCalls{w, tid, w.seedResident(tid, b, bCountry)}
	if _, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("50")); err != nil {
		t.Fatal(err)
	}
	r, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("70"))
	if err != nil || !r.Change.Pending() {
		t.Fatalf("an unapproved (owner-path) basis row must not be relied on, so the licence-only increase is accepted: %+v / %v", r.Change, err)
	}
}

// The incomplete flag (a relied-on basis whose jurisdiction could not be resolved) is honoured by the single classification the trigger
// and the preview share.
func TestLic25_Classification_IncompleteRelianceRefusesIncreasesOnly(t *testing.T) {
	w := newWorld(t)
	jl := w.jurisdiction()
	w.rgFloors(jl, nil, "00:00:02", false)
	code := w.jurCode(jl)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	p := w.seedResident(tid, b, bCountry)
	c := rgPlayerCalls{w, tid, p}
	if _, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("50")); err != nil {
		t.Fatal(err)
	}
	classify := func(value string, incomplete bool) (rg.Classification, error) {
		var cl rg.Classification
		per := rg.PeriodRolling24h
		a := eur
		err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			cl, e = rg.ClassifyLimitChange(ctx, tx, rg.ClassifyParams{Key: rg.LimitKey{TenantID: tid, PlayerAccountID: p.id, PersonID: p.person, Scope: rg.LimitScopeAccount,
				Kind: rg.ControlWager, AssetCode: &a, Period: &per}, BrandID: b, JurisdictionCodes: []string{code}, JurisdictionIncomplete: incomplete,
				Value: str(value), Source: rg.ChangeSourcePlayerSelfService})
			return e
		})
		return cl, err
	}
	if cl, err := classify("70", false); err != nil || cl.ChangeKind != rg.ChangeIncrease {
		t.Fatalf("control: %+v / %v", cl, err)
	}
	if _, err := classify("70", true); rg.SQLStateOf(err) != "RL023" {
		t.Fatalf("an increase with an unresolvable relied-on jurisdiction must be RL023, got %v", err)
	}
	if cl, err := classify("10", true); err != nil || cl.ChangeKind != rg.ChangeDecrease {
		t.Fatalf("a decrease is never refused for it: %+v / %v", cl, err)
	}
}

// The basis is selected by BRAND: a brand-specific row governs that brand only; the other brand falls back to the licence.
func TestLic25_Scoped_BrandSpecificBasis(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction()
	w.rgFloors(jl, map[string]string{"deposit": "1000"}, "00:00:02", false)
	w.rgFloors(jb, map[string]string{"deposit": "100"}, "00:00:02", false)
	tid := w.tenant("under_platform_licence")
	brandA, brandB := w.brand(tid), w.brand(tid)
	w.licence(jl, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, &brandB, jb, 30), "documented_basis")
	w.rgOn(tid)
	d, err := w.rgEval(tid, brandB, w.seedResident(tid, brandB, bCountry), rg.ActionDeposit, 150)
	w.wantDenied("the brand with its own basis row", d, err, rg.CodeDepositLimitReached, "100")
	d, err = w.rgEval(tid, brandA, w.seedResident(tid, brandA, bCountry), rg.ActionDeposit, 150)
	w.wantAllowed("the other brand has no row and falls back to the tenant licence", d, err)
}

func wantUnavailableOptions(t *testing.T, o rg.Options, reason string, kinds ...rg.ControlKind) {
	t.Helper()
	if len(o.TimeOutHours) == 0 || len(o.SelfExclusionDays) == 0 {
		t.Fatalf("time-out and self-exclusion options are protective and always returned: %+v", o)
	}
	for _, k := range kinds {
		found := false
		for _, l := range o.Limits {
			if l.Kind != k {
				continue
			}
			found = true
			if l.UnavailableReason != reason || !l.ProtectiveOnly || l.ConfigInForce || l.Ceiling != nil || l.IncreaseCooldownSeconds != nil {
				t.Fatalf("kind %s must be unavailable (%s), protective-only, with no ceiling or cooldown: %+v", k, reason, l)
			}
		}
		if !found {
			t.Fatalf("kind %s missing from the options", k)
		}
	}
}

// lic27a (S-1): relied-on jurisdictions that disagree on calendar_tz cannot be folded; the kind is marked unavailable (calendar_conflict),
// the other kinds and the protective options are served, and activation / readiness cannot pass.
func TestLic27a_CalendarConflict_OptionsAndReadiness(t *testing.T) {
	w := newWorld(t)
	ja, jb := w.jurisdiction(), w.jurisdiction()
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(ja, "platform", tid)
	set := TenantJurisdictionConfigPayload{TenantID: tid, JurisdictionID: ja, Action: ConfigActionSet, KYCRulesetID: sp("kyc-1"),
		AMLRulesetID: sp("aml-1"), RGRulesetID: sp("rg-1"), AllowedCurrencies: []string{eur}, AllowedPaymentMethods: []string{"card"}, GeoBlockList: []string{}}
	first := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, set, "configuration_initial")
	w.mustApprove(w.ap1, first.Request.ID)
	w.mustApprove(w.ap2, first.Request.ID)
	w.rgFloorsTZ(ja, nil, "00:00:02", false, "UTC")
	w.rgFloorsTZ(jb, nil, "00:00:02", false, "Etc/GMT+5")
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	p := w.seedResident(tid, b, bCountry)
	var o rg.Options
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		o, e = rg.ReadOptions(ctx, tx, tid, p.id, eur)
		return e
	}); err != nil {
		t.Fatalf("a calendar disagreement must not refuse the whole options read: %v", err)
	}
	wantUnavailableOptions(t, o, rg.OptionUnavailableCalendarConflict, rg.ControlDeposit, rg.ControlLoss, rg.ControlWager)
	rgfix.Activate(t, w.owner, tid, nil, rgfix.Staff(t, w.owner, tid))
	check := func() launchgov.Check {
		var c launchgov.Check
		if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			c = launchgov.RGActivationReadiness(ctx, tx, tid, nil, w.dbNow())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := check(); c.Result != launchgov.ResultFail || c.DetailCode != "calendar_conflict" {
		t.Fatalf("the activation guard must fail on a calendar disagreement: %+v", c)
	}
	// Agreement restored by a new floor row in jb: the guard passes.
	w.rgFloorsTZ(jb, nil, "00:00:02", false, "UTC")
	if c := check(); c.Result != launchgov.ResultPass {
		t.Fatalf("with agreeing calendars the guard passes: %+v", c)
	}
}

// lic27a (S-2): the SQL classification itself refuses a confirm while a relied-on jurisdiction has no floor (not only the Go layer).
func TestLic27a_ConfirmRefusedInSQL_WhileAReliedJurisdictionHasNoFloor(t *testing.T) {
	w := newWorld(t)
	jl, jc := w.jurisdiction(), w.jurisdiction() // jc: no floor
	w.rgFloors(jl, map[string]string{"wager": "1000"}, "00:00:02", true)
	build := func(withBasis bool) (uuid.UUID, resident, uuid.UUID) {
		tid := w.tenant("under_platform_licence")
		b := w.brand(tid)
		w.licence(jl, "platform", tid)
		p := w.seedResident(tid, b, bCountry)
		c := rgPlayerCalls{w, tid, p}
		if _, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("50")); err != nil {
			t.Fatal(err)
		}
		pend, err := c.set(rg.ControlWager, rg.PeriodRolling24h, str("70"))
		if err != nil || !pend.Change.Pending() {
			t.Fatalf("setup: %+v / %v", pend.Change, err)
		}
		if withBasis {
			w.widen(KindOperatingBasisSet, noLicence(tid, nil, jc, 30), "documented_basis")
		}
		return tid, p, pend.Change.ID
	}
	rawConfirm := func(tid uuid.UUID, p resident, pending uuid.UUID) (string, error) {
		var kind string
		err := w.rt.WithPlayerScope(context.Background(), tid, p.id, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO player_rg_limit_changes
				(tenant_id, player_account_id, scope, control_kind, asset_code, period_kind, change_kind, supersedes_change_id, source, reason_code, idempotency_key, request_hash)
				VALUES ($1, $2, 'account', 'wager', 'EUR', 'rolling_24h', 'confirm', $3, 'player_self_service', 'lic27a_test', $4, $5)
				RETURNING change_kind`, tid, p.id, pending, uuid.NewString(), strings.Repeat("b", 64)).Scan(&kind)
		})
		return kind, err
	}
	tOK, pOK, idOK := build(false)
	tBad, pBad, idBad := build(true)
	time.Sleep(2200 * time.Millisecond)
	if k, err := rawConfirm(tOK, pOK, idOK); err != nil || k != "confirm" {
		t.Fatalf("control: a tenant with no approved basis confirms exactly as before: %v %v", k, err)
	}
	if _, err := rawConfirm(tBad, pBad, idBad); sqlstateOf(err) != "RL023" {
		t.Fatalf("a confirm with a relied-on jurisdiction lacking a floor must be RL023 in SQL, got %v", err)
	}
}

func optionOf(t *testing.T, o rg.Options, k rg.ControlKind) rg.LimitOption {
	t.Helper()
	for _, l := range o.Limits {
		if l.Kind == k {
			return l
		}
	}
	t.Fatalf("kind %s missing", k)
	return rg.LimitOption{}
}

// lic28 (F-1): for an unavailable kind the options carry the PROTECTIVE periods (the union of the periods of the relied-on jurisdictions that
// have a floor), and the server accepts a first limit and a decrease for it while an increase, a removal and a confirm stay refused.
func TestLic28_ProtectiveOnly_FirstLimitAndDecreaseAccepted_IncreaseRemovalRefused(t *testing.T) {
	w := newWorld(t)
	jl, jc := w.jurisdiction(), w.jurisdiction() // jc: no floor
	w.rgFloors(jl, map[string]string{"wager": "1000"}, "00:00:02", false)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(jl, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jc, 30), "documented_basis")
	p := w.seedResident(tid, b, bCountry)
	c := rgPlayerCalls{w, tid, p}
	var o rg.Options
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		o, e = rg.ReadOptions(ctx, tx, tid, p.id, eur)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	wantUnavailableOptions(t, o, rg.OptionUnavailableMissingFloor, rg.ControlWager)
	w24 := optionOf(t, o, rg.ControlWager)
	if len(w24.AllowedPeriods) != len(rg.PeriodKinds) {
		t.Fatalf("the protective periods are those of the jurisdiction that has a floor: %v", w24.AllowedPeriods)
	}
	for _, k := range []rg.ControlKind{rg.ControlDeposit, rg.ControlLoss} {
		if l := optionOf(t, o, k); l.UnavailableReason == "" || len(l.AllowedPeriods) != len(rg.PeriodKinds) {
			t.Fatalf("%s: %+v", k, l)
		}
	}
	// First limit (initial) on an offered period, then a decrease: accepted at once. The ceiling of the floored jurisdiction still applies.
	if _, err := c.set(rg.ControlWager, w24.AllowedPeriods[0], str("5000")); err == nil {
		t.Fatal("the ceiling of the jurisdiction that has a floor keeps applying to a first limit")
	}
	if r, err := c.set(rg.ControlWager, w24.AllowedPeriods[0], str("500")); err != nil || r.Change.ChangeKind != rg.ChangeInitial {
		t.Fatalf("a first protective limit must be accepted: %+v / %v", r.Change, err)
	}
	if r, err := c.set(rg.ControlWager, w24.AllowedPeriods[0], str("300")); err != nil || r.Change.ChangeKind != rg.ChangeDecrease {
		t.Fatalf("a decrease must be accepted: %+v / %v", r.Change, err)
	}
	_, err := c.set(rg.ControlWager, w24.AllowedPeriods[0], str("400"))
	wantRefusal(t, "increase", err, rg.RefusedConfigMissing)
	_, err = c.set(rg.ControlWager, w24.AllowedPeriods[0], nil)
	wantRefusal(t, "removal", err, rg.RefusedConfigMissing)
}

// A calendar conflict also leaves the protective periods and the first-limit / decrease paths open.
func TestLic28_CalendarConflict_ProtectivePeriodsAndWritesStillWork(t *testing.T) {
	w := newWorld(t)
	ja, jb := w.jurisdiction(), w.jurisdiction()
	w.rgFloorsTZ(ja, nil, "00:00:02", false, "UTC")
	w.rgFloorsTZ(jb, nil, "00:00:02", false, "Etc/GMT+5")
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.licence(ja, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	p := w.seedResident(tid, b, bCountry)
	c := rgPlayerCalls{w, tid, p}
	var o rg.Options
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		o, e = rg.ReadOptions(ctx, tx, tid, p.id, eur)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	wantUnavailableOptions(t, o, rg.OptionUnavailableCalendarConflict, rg.ControlDeposit)
	l := optionOf(t, o, rg.ControlDeposit)
	if len(l.AllowedPeriods) != len(rg.PeriodKinds) {
		t.Fatalf("protective periods under a calendar conflict: %v", l.AllowedPeriods)
	}
	if _, err := c.set(rg.ControlDeposit, l.AllowedPeriods[0], str("50")); err != nil {
		t.Fatalf("a first limit is accepted under a calendar conflict: %v", err)
	}
	if r, err := c.set(rg.ControlDeposit, l.AllowedPeriods[0], str("40")); err != nil || r.Change.ChangeKind != rg.ChangeDecrease {
		t.Fatalf("a decrease is accepted under a calendar conflict: %+v / %v", r.Change, err)
	}
}
