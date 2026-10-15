//go:build integration

package rg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func figStr(t *testing.T, d Decision, k string) string {
	t.Helper()
	v, ok := d.Figures[k].(string)
	if !ok {
		t.Fatalf("figure %q missing or not a string: %v", k, d.Figures)
	}
	return v
}

// ---- the switch ---------------------------------------------------------------------------------

func s4_DisabledByDefault_IsEligibilityOnly(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"wager": {"mandatory": true}})
	// A mandatory wager kind with NO player value would be rg_limit_required if enforcement were on.
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1_000_000))
	wantAllowed(t, "enforcement off: no limit is read", d)
	if d.Figures != nil {
		t.Fatalf("an allowed decision carries no figures: %v", d.Figures)
	}
	// A jurisdiction with NO floor at all is still not rg_config_missing while the switch is off.
	g := w.newFix(t)
	wantAllowed(t, "no floor, switch off", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionDeposit, 5)))
	wantAllowed(t, "launch, switch off", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionCasinoLaunch, 0)))
	// A limit the player set is recorded but NOT enforced while the switch is off (the owner decision).
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("100"))
	w.stake(t, f.tenantA, f.acctA1, "500", time.Time{})
	wantAllowed(t, "recorded limit, switch off", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1000)))
	// Turning it on enforces the SAME recorded limit.
	w.activate(t, f, nil)
	wantDenied(t, "same limit, switch on", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1000)), CodeWagerLimitReached)
}

func s4_SelfExclusionAndTimeOut_DoNotNeedTheSwitch(t *testing.T, w *s1World) {
	f := w.newFix(t) // no floors, switch off
	insertRestrictionRow(t, w, f.personA, nil, RestrictionTimeOut, time.Hour)
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 5))
	wantDenied(t, "time-out with the switch off", d, CodeTimedOut)
	g := w.newFix(t)
	insertRestrictionRow(t, w, g.personA, nil, RestrictionSelfExclusion, time.Hour)
	wantDenied(t, "self-exclusion with the switch off", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 5)), CodeSelfExcluded)
}

// insertRestrictionRow inserts a platform-scope (tenant NULL) or tenant-scope restriction as the owner,
// started a moment ago and ending after d (d may be tiny for boundary tests).
func insertRestrictionRow(t *testing.T, w *s1World, person uuid.UUID, tenant *uuid.UUID, typ RestrictionType, d time.Duration) {
	t.Helper()
	insert := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO player_restrictions (id, person_id, tenant_id, restriction_type, starts_at, ends_at, source, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, clock_timestamp() - interval '1 minute', clock_timestamp() + $5 * interval '1 microsecond', 'staff', 'staff', $6)`,
			uuid.New(), person, tenant, string(typ), d.Microseconds(), uuid.New())
		return err
	}
	if tenant != nil {
		wantOK(t, "insert tenant restriction", w.owner.WithTenant(context.Background(), *tenant, insert))
		return
	}
	wantOK(t, "insert platform restriction", w.owner.WithoutTenant(context.Background(), insert))
}

func s4_ActivationControls(t *testing.T, w *s1World) {
	f := w.newFix(t)
	l := NewLimits(LimitsConfig{})
	in := ActivateEnforcementInput{TenantID: f.tenantA, StaffID: f.staffA, ReasonCode: "s4_test", AttestationRef: rgfix.TestAttestation}

	var created bool
	wantOK(t, "first activation", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		_, created, e = l.ActivateEnforcement(ctx, tx, in)
		return e
	}))
	if !created {
		t.Fatal("the first activation must create the row")
	}
	wantOK(t, "second activation is idempotent", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		_, created, e = l.ActivateEnforcement(ctx, tx, in)
		return e
	}))
	if created {
		t.Fatal("a repeated activation must not create a second row")
	}
	var n int
	wantOK(t, "count", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		list, e := ListEnforcementActivations(ctx, tx)
		n = len(list)
		return e
	}))
	if n != 1 {
		t.Fatalf("want exactly 1 activation row, got %d", n)
	}

	// Only an active staff principal of the SAME tenant may write the row.
	for name, s := range map[string]sess{
		"platform principal":            platformSess(w.ops.Requester),
		"player":                        playerSess(f.tenantA, f.acctA1),
		"staff of another tenant":       staffSess(f.tenantB, f.staffB),
		"no principal (tenant only)":    {tenant: f.tenantA.String()},
		"tenant staff, other tenant id": staffSess(f.tenantA, f.staffB),
	} {
		in2 := in
		in2.TenantID = f.tenantA
		in2.BrandID = &f.brandA2 // a scope that does not exist yet, so the unique key cannot mask the refusal
		err := do(w.rt, s, func(ctx context.Context, tx pgx.Tx) error {
			_, _, e := l.ActivateEnforcement(ctx, tx, in2)
			return e
		})
		if err == nil {
			t.Fatalf("%s must not be able to activate enforcement", name)
		}
	}

	// Tenant B sees none of A's rows, and A's activation does not enable B.
	wantOK(t, "tenant B list", do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
		list, e := ListEnforcementActivations(ctx, tx)
		n = len(list)
		return e
	}))
	if n != 0 {
		t.Fatalf("tenant B must not see tenant A's activation, saw %d", n)
	}
	wantOK(t, "tenant B not enabled", do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
		on, e := EnforcementEnabled(ctx, tx, f.tenantB, f.brandB)
		if e == nil && on {
			t.Fatal("tenant A's activation must not enable tenant B")
		}
		return e
	}))

	// Append-only, one way. Two independent controls: (1) the runtime role has no UPDATE / DELETE / TRUNCATE
	// privilege at all; (2) the table has no UPDATE or DELETE policy, so under FORCE RLS even the owner sees no row
	// to change, and the immutability trigger (RL009) refuses the statement when RLS is lifted (a scratch-database
	// owner step, rolled back) and on TRUNCATE (a statement trigger, which RLS does not hide).
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE rg_enforcement_activations SET reason_code = 'x'`,
		`DELETE FROM rg_enforcement_activations`,
		`TRUNCATE rg_enforcement_activations`,
	} {
		if _, err := w.rt.Raw().Exec(ctx, stmt); err == nil {
			t.Fatalf("%s must be refused to the runtime role", stmt)
		}
		// owner, RLS in force: nothing changes
		var tag string
		err := w.owner.WithTenant(ctx, f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
			ct, e := tx.Exec(ctx, stmt)
			tag = ct.String()
			return e
		})
		if !strings.HasPrefix(stmt, "TRUNCATE") && (err != nil || !strings.HasSuffix(tag, " 0")) {
			t.Fatalf("%s with RLS forced must change no row, got tag %q err %v", stmt, tag, err)
		}
		if strings.HasPrefix(stmt, "TRUNCATE") && SQLStateOf(err) != "RL009" {
			t.Fatalf("%s: want RL009, got %v", stmt, err)
		}
		// owner, RLS lifted inside a rolled-back transaction: the trigger refuses
		tx, err := w.owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE rg_enforcement_activations NO FORCE ROW LEVEL SECURITY`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, stmt)
		_ = tx.Rollback(ctx)
		if SQLStateOf(err) != "RL009" {
			t.Fatalf("%s with RLS lifted: want RL009, got %v", stmt, err)
		}
	}
	var still int
	wantOK(t, "row survives", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM rg_enforcement_activations`).Scan(&still)
	}))
	if still != 1 {
		t.Fatalf("the activation row must survive every mutation attempt, count=%d", still)
	}

	// The read is part of the evaluation: a session that is not the request tenant's is an ERROR (never "off").
	err := do(w.rt, staffSess(f.tenantB, f.staffB), func(ctx context.Context, tx pgx.Tx) error {
		_, e := EnforcementEnabled(ctx, tx, f.tenantA, f.brandA1)
		return e
	})
	if !errors.Is(err, ErrUsageScope) {
		t.Fatalf("a foreign-tenant session must be ErrUsageScope, got %v", err)
	}
	_, err = w.eval(f, f.tenantB, f.action(f.acctA1, ActionDeposit, 1)) // tenant A's account in tenant B's session
	if err == nil {
		t.Fatal("evaluating tenant A's account in tenant B's session must fail closed")
	}
}

func s4_ActivationScope_BrandVersusTenant(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"wager": {"mandatory": true}})
	w.activate(t, f, &f.brandA1) // brand A1 only
	wantDenied(t, "brand A1 is enforced", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 5)), CodeLimitRequired)
	wantAllowed(t, "brand A2 is not (no activation covers it)", w.mustEval(t, f, f.tenantA, f.action(f.acctA2, ActionCasinoWager, 5)))
	w.activate(t, f, nil) // now the whole tenant
	wantDenied(t, "brand A2 after the tenant-wide row", w.mustEval(t, f, f.tenantA, f.action(f.acctA2, ActionCasinoWager, 5)), CodeLimitRequired)
}

// ---- rg_config_missing (S1 LF 8: a jurisdiction rgfix has NOT seeded) ------------------------------------

func s4_ConfigMissing_FailsClosed(t *testing.T, w *s1World) {
	f := w.newFix(t) // fresh jurisdiction: no floor of any kind
	w.activate(t, f, nil)
	for _, a := range []Action{ActionDeposit, ActionCasinoWager, ActionSportsbookWager, ActionCasinoLaunch} {
		amount := int64(10)
		if a == ActionCasinoLaunch {
			amount = 0
		}
		d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, a, amount))
		wantDenied(t, string(a)+" with no floor", d, CodeConfigMissing)
		if figStr(t, d, "code") != CodeConfigMissing || figStr(t, d, "asset_code") != "USD" {
			t.Fatalf("the config-missing denial must say what was missing: %v", d.Figures)
		}
	}

	// One kind present is not enough: a wager needs BOTH wager and loss; a launch too.
	w.floor(t, rgfix.EnforcedFloor(f.jur, "wager", "USD", nil))
	wantDenied(t, "loss floor still missing", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 10)), CodeConfigMissing)
	wantDenied(t, "deposit floor still missing", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 10)), CodeConfigMissing)
	w.floor(t, rgfix.EnforcedFloor(f.jur, "loss", "USD", nil))
	wantAllowed(t, "wager and loss floors present", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 10)))
	wantAllowed(t, "launch with both", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoLaunch, 0)))
	// A floor for ANOTHER asset does not cover USD.
	w.floor(t, rgfix.EnforcedFloor(f.jur, "deposit", "EUR", nil))
	wantDenied(t, "a EUR floor does not cover USD", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 10)), CodeConfigMissing)

	// A tenant without a licence has no jurisdiction at all: fail closed.
	g := w.newFix(t)
	w.activate(t, g, nil)
	wantOK(t, "unlink the licence", w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, g.tenantA)
		return e
	}))
	wantDenied(t, "no licence", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionDeposit, 10)), CodeConfigMissing)
}

func s4_SyntheticFloorIsAbsentOutsideDevTest(t *testing.T, w *s1World) {
	f := w.newFix(t)
	w.activate(t, f, nil)
	// The scratch database is anchored 'production' (unset); record the TEST anchor so a synthetic row can be written at
	// all (the same write-once owner step rgfix.EnsureTestEnvironment performs for the shared test database).
	rgfix.EnsureTestEnvironment(t, w.ownerURL)
	// A SYNTHETIC-NOT-LEGAL permissive floor, written the only way the database allows (anchor and GUC 'test').
	for _, kind := range []string{"loss", "wager"} {
		kind := kind
		wantOK(t, "synthetic floor", do(w.rt, sess{env: "test"}, func(ctx context.Context, tx pgx.Tx) error {
			_, e := rgfix.InsertFloor(ctx, tx, rgfix.PermissiveFloor(f.jur, kind).Clone(rgfix.Spec{"asset_code": "USD"}), rgfix.DefaultSteps())
			return e
		}))
	}
	// Read in a connection whose app.environment is NOT dev/test (the production default): the floor is ABSENT.
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 10))
	wantDenied(t, "synthetic floor in a production-shaped session", d, CodeConfigMissing)
}

// ---- deposit limit -----------------------------------------------------------------------------------------

func s4_DepositLimit_UsageWindowsAndFigures(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlDeposit, PeriodRolling24h, strp("1000"))
	now := time.Now()
	w.s3Deposit(t, f.tenantA, f.acctA1, "USD", "600", now.Add(-time.Hour))
	w.s3Deposit(t, f.tenantA, f.acctA1, "USD", "5000", now.Add(-25*time.Hour)) // outside the 24h window: must not count

	wantAllowed(t, "600 + 400 == 1000 (inclusive bound)", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 400)))
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 401))
	wantDenied(t, "600 + 401 > 1000", d, CodeDepositLimitReached)

	// The denial audit figures (S3 condition (e), ADR 0115 4): the COMPUTED numbers, not just query inputs.
	for k, want := range map[string]string{"limit": "1000", "usage": "600", "attempted_amount": "401", "period": "rolling_24h",
		"scope": "account", "kind": "deposit", "asset_code": "USD", "limit_source": "player_account", "action": "deposit", "code": CodeDepositLimitReached} {
		if got := figStr(t, d, k); got != want {
			t.Fatalf("figure %s = %q, want %q (%v)", k, got, want, d.Figures)
		}
	}
	ws, err := time.Parse(time.RFC3339Nano, figStr(t, d, "window_start"))
	asOf, err2 := time.Parse(time.RFC3339Nano, figStr(t, d, "as_of"))
	if err != nil || err2 != nil || asOf.Sub(ws) != 24*time.Hour {
		t.Fatalf("window_start / as_of must be 24h apart for rolling_24h: %v %v (%v %v)", ws, asOf, err, err2)
	}
	if ids, ok := d.Figures["config_version_ids"].([]string); !ok || len(ids) == 0 {
		t.Fatalf("the configuration versions in force must be recorded: %v", d.Figures["config_version_ids"])
	}
	if _, ok := d.Figures["usage_inputs"].(map[string]any); !ok {
		t.Fatalf("the usage query inputs must be recorded: %v", d.Figures)
	}

	// An in-flight (pending) intent counts at the requested amount; the CANDIDATE itself does not count twice.
	pending := w.s3Intent(t, f.tenantA, f.acctA1, "USD", "pending", "300", now, nil)
	wantAllowed(t, "600 + 300 pending + 100", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 100)))
	wantDenied(t, "600 + 300 pending + 101", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 101)), CodeDepositLimitReached)
	cand := f.action(f.acctA1, ActionDeposit, 300)
	cand.CandidateRef = &pending
	wantAllowed(t, "the candidate intent is excluded and added once: 600 + 300 == 900", w.mustEval(t, f, f.tenantA, cand))
	cand.Amount = 401
	wantDenied(t, "candidate excluded: 600 + 401 > 1000", w.mustEval(t, f, f.tenantA, cand), CodeDepositLimitReached)

	// A failed intent releases its usage; a pending intent of ANOTHER account never counts.
	w.s3Intent(t, f.tenantA, f.acctX, "USD", "pending", "9999", now, nil)
	w.s3Intent(t, f.tenantA, f.acctA1, "USD", "failed", "9999", now, nil)
	wantAllowed(t, "failed and foreign intents do not count", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 100)))
}

func s4_Ceiling_IsAHardCap_AndAClampOnAConfirm(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"wager": {"increase_requires_reconfirmation": true}})
	w.activate(t, f, nil)
	pending := w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("100"))
	if pending.Change.Pending() {
		t.Fatal("an initial value is immediate")
	}
	// Increase 100 -> 900: pending; needs the player's own confirm after the cooling-off period.
	inc := w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("900"))
	if !inc.Change.Pending() || !inc.Change.RequiresConfirmation {
		t.Fatalf("an increase must be pending and await a confirm: %+v", inc.Change)
	}
	// The operator/floor then LOWERS the ceiling to 400 (a new floor row, in force now).
	w.floor(t, rgfix.EnforcedFloor(f.jur, "wager", "USD", rgfix.Spec{"ceiling": "400", "ceiling_none": false, "increase_requires_reconfirmation": true}))
	time.Sleep(2200 * time.Millisecond) // the 2 s cooling-off period of the fixture floor
	wantOK(t, "confirm", do(w.rt, playerSess(f.tenantA, f.acctA1), func(ctx context.Context, tx pgx.Tx) error {
		_, e := NewLimits(LimitsConfig{}).ResolvePending(ctx, tx, ResolveInput{TenantID: f.tenantA, PlayerAccountID: f.acctA1,
			ChangeID: inc.Change.ID, Intent: ChangeConfirm, IdempotencyKey: uuid.NewString()})
		return e
	}))
	// The confirmed value (900) is REALIZED, but the lowered ceiling clamps it (S2 hard condition (i)).
	wantAllowed(t, "stake 400 == the ceiling", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 400)))
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 401))
	wantDenied(t, "stake 401 above the ceiling although 900 was realized", d, CodeWagerLimitReached)
	if figStr(t, d, "limit") != "400" || figStr(t, d, "limit_source") != "ceiling" {
		t.Fatalf("the ceiling must be the limit in force: %v", d.Figures)
	}

	// The ceiling is a hard cap even for a player who set NO value for the kind (every allowed period).
	g := w.s4Fix(t, map[string]rgfix.Spec{"deposit": {"ceiling": "2000", "ceiling_none": false}})
	w.activate(t, g, nil)
	wantAllowed(t, "2000 is within the ceiling", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionDeposit, 2000)))
	d = w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionDeposit, 2001))
	wantDenied(t, "2001 above the ceiling, no player value", d, CodeDepositLimitReached)
	if figStr(t, d, "limit_source") != "ceiling" {
		t.Fatalf("limit_source %v", d.Figures)
	}
}

func s4_Mandatory_RequiresAValue(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"deposit": {"mandatory": true}, "wager": {"mandatory": true}, "loss": {"mandatory": false}})
	w.activate(t, f, nil)
	for _, a := range []Action{ActionDeposit, ActionCasinoWager, ActionSportsbookWager, ActionCasinoLaunch} {
		amount := int64(1)
		if a == ActionCasinoLaunch {
			amount = 0
		}
		wantDenied(t, string(a)+" mandatory, no value", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, a, amount)), CodeLimitRequired)
	}
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("500"))
	wantAllowed(t, "wager value set", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 100)))
	wantDenied(t, "deposit still has no value", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 100)), CodeLimitRequired)
	// Removing a mandatory limit is an increase: it waits out the cooling-off period, the old value stays binding.
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, nil)
	wantDenied(t, "removal pending: the old limit binds", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 501)), CodeWagerLimitReached)
	time.Sleep(2200 * time.Millisecond)
	wantDenied(t, "removal realized: mandatory again with no value", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1)), CodeLimitRequired)
}

// The ceiling and the player's value share ONE (period, scope) key when the floor scope is the account scope: the
// MIN between the two sources must pick the smaller whichever is smaller.
func s4_Ceiling_SameScopeKey_MinOfPlayerValueAndCeiling(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"wager": {"scope": "account", "ceiling": "1000", "ceiling_none": false}})
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("800"))
	wantAllowed(t, "800 within the player value", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 800)))
	// The floor's ceiling is LOWERED below the player's value: the ceiling binds (smaller).
	w.floor(t, rgfix.EnforcedFloor(f.jur, "wager", "USD", rgfix.Spec{"scope": "account", "ceiling": "400", "ceiling_none": false}))
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 401))
	wantDenied(t, "401 above the lowered ceiling although the player value is 800", d, CodeWagerLimitReached)
	if figStr(t, d, "limit") != "400" || figStr(t, d, "limit_source") != "ceiling" {
		t.Fatalf("the ceiling must bind: %v", d.Figures)
	}
	// A player value BELOW the ceiling binds instead.
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("300")) // decrease: immediate
	d = w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 301))
	wantDenied(t, "301 above the player value 300 below the ceiling 400", d, CodeWagerLimitReached)
	if figStr(t, d, "limit") != "300" || figStr(t, d, "limit_source") != "player_account" {
		t.Fatalf("the player value must bind: %v", d.Figures)
	}
}

// A mandatory kind whose floor carries a numeric ceiling is bounded by the ceiling: it does not demand a value.
func s4_MandatoryWithCeiling_UsesTheCeiling(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"deposit": {"mandatory": true, "ceiling": "700", "ceiling_none": false}})
	w.activate(t, f, nil)
	wantAllowed(t, "700 == ceiling", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 700)))
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 701))
	wantDenied(t, "701 above the ceiling", d, CodeDepositLimitReached)
	if figStr(t, d, "limit_source") != "ceiling" {
		t.Fatalf("limit_source %v", d.Figures)
	}
}

// A launch moves no money: it stays allowed when recorded usage already exceeds a (lowered) limit, while the next
// stake is refused.
func s4_Launch_AllowedWhenUsageExceedsLoweredLimit(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("500"))
	w.stake(t, f.tenantA, f.acctA1, "300", time.Time{})
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("200")) // decrease: immediate
	wantAllowed(t, "launch with usage 300 above the limit 200", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoLaunch, 0)))
	wantDenied(t, "a stake of 1 is refused", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1)), CodeWagerLimitReached)
}

// ---- wager / loss ---------------------------------------------------------------------------------------------

func s4_WagerAndLossLimits(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("500"))
	for i := 0; i < 3; i++ {
		w.stake(t, f.tenantA, f.acctA1, "100", time.Time{})
	}
	for _, a := range []Action{ActionCasinoWager, ActionSportsbookWager} {
		wantAllowed(t, string(a)+" 300 + 200 == 500", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, a, 200)))
		d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, a, 201))
		wantDenied(t, string(a)+" 300 + 201 > 500", d, CodeWagerLimitReached)
		if figStr(t, d, "usage") != "300" || figStr(t, d, "limit") != "500" || figStr(t, d, "kind") != "wager" {
			t.Fatalf("figures %v", d.Figures)
		}
	}
	// A launch moves no money and measures no usage: allowed even though the next stake would be refused.
	wantAllowed(t, "launch with usage near the limit", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoLaunch, 0)))

	// Loss is NET of wins: stake 300, win 250 -> loss 50 (the earlier wager stakes are a separate player: use acctX).
	w.setLimit(t, f.tenantA, f.acctX, ControlLoss, PeriodRolling24h, strp("100"))
	w.stakeWithWin(t, f.tenantA, f.acctX, "300", "250")
	wantAllowed(t, "net loss 50 + 50 == 100", w.mustEval(t, f, f.tenantA, f.action(f.acctX, ActionCasinoWager, 50)))
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctX, ActionCasinoWager, 51))
	wantDenied(t, "net loss 50 + 51 > 100", d, CodeLossLimitReached)
	if figStr(t, d, "usage") != "50" || figStr(t, d, "kind") != "loss" {
		t.Fatalf("figures %v", d.Figures)
	}
	if figStr(t, d, "loss_raw_usage") != "50" {
		t.Fatalf("the raw loss figure must be recorded: %v", d.Figures)
	}

	// Wager is checked before loss when both are breached.
	w.setLimit(t, f.tenantA, f.acctX, ControlWager, PeriodRolling24h, strp("350"))
	wantDenied(t, "both breached: wager first", w.mustEval(t, f, f.tenantA, f.action(f.acctX, ActionCasinoWager, 51)), CodeWagerLimitReached)
}

func s4_WindowBoundary_Rolling24h(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("100"))
	// 24h + 3s ago: outside; 24h - 3s ago: inside (bounded by the DB clock; the margins are generous).
	w.stake(t, f.tenantA, f.acctA1, "100", time.Now().Add(-24*time.Hour-3*time.Second))
	wantAllowed(t, "a stake just outside the window does not count", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 100)))
	w.stake(t, f.tenantA, f.acctA1, "100", time.Now().Add(-24*time.Hour+3*time.Second))
	wantDenied(t, "a stake just inside the window counts", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1)), CodeWagerLimitReached)
}

func s4_DecreaseImmediate_IncreaseAfterCoolingOff(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("1000"))
	wantAllowed(t, "limit 1000", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 900)))
	dec := w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("400"))
	if dec.Change.Pending() {
		t.Fatal("a decrease is immediate")
	}
	wantDenied(t, "decrease takes effect at once", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 401)), CodeWagerLimitReached)
	inc := w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("5000"))
	if !inc.Change.Pending() {
		t.Fatal("an increase is pending")
	}
	wantDenied(t, "increase not yet in force: 400 still binds", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 401)), CodeWagerLimitReached)
	time.Sleep(2200 * time.Millisecond)
	wantAllowed(t, "increase realized after the cooling-off period", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 4000)))
}

func s4_ScopeMIN_AccountAndTenantPerson(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil) // configured scope: account
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("1000")) // account scope row
	// The operator tightens the configuration scope to tenant_person (a stricter, later floor); the player then
	// sets a tenant_person limit. The legacy account-scope row must still bind (S2 hard condition (ii)).
	w.floor(t, rgfix.EnforcedFloor(f.jur, "wager", "USD", rgfix.Spec{"scope": "tenant_person"}))
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("500"))
	// Person usage across the two brands = 300 (A1) + 150 (A2) = 450; account usage of A1 = 300.
	w.stake(t, f.tenantA, f.acctA1, "300", time.Time{})
	w.stake(t, f.tenantA, f.acctA2, "150", time.Time{})
	w.stake(t, f.tenantA, f.acctX, "9999", time.Time{}) // another Person
	w.stake(t, f.tenantB, f.acctB, "9999", time.Time{}) // the same Person, ANOTHER tenant: never aggregated
	wantAllowed(t, "person 450 + 50 == 500", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 50)))
	d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 51))
	wantDenied(t, "person 450 + 51 > 500", d, CodeWagerLimitReached)
	if figStr(t, d, "scope") != "tenant_person" || figStr(t, d, "usage") != "450" || figStr(t, d, "limit_source") != "player_tenant_person" {
		t.Fatalf("figures %v", d.Figures)
	}
	// The same limit binds from the OTHER brand of the Person (multi-brand aggregation).
	wantDenied(t, "from brand A2 the person total is the same", w.mustEval(t, f, f.tenantA, f.action(f.acctA2, ActionCasinoWager, 51)), CodeWagerLimitReached)
	wantAllowed(t, "from brand A2 within the person limit", w.mustEval(t, f, f.tenantA, f.action(f.acctA2, ActionCasinoWager, 50)))
	// Another Person is unaffected.
	wantAllowed(t, "another Person has no limit", w.mustEval(t, f, f.tenantA, f.action(f.acctX, ActionCasinoWager, 100000)))
}

func s4_AccountScopeLimit_IsPerAccount(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil) // scope account
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("100"))
	w.stake(t, f.tenantA, f.acctA2, "5000", time.Time{}) // same Person, other brand: not this account's usage
	wantAllowed(t, "account scope ignores the other brand", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 100)))
	wantDenied(t, "account scope own usage", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 101)), CodeWagerLimitReached)
}

// ---- time-out ------------------------------------------------------------------------------------------------------

func s4_TimeOut_Scopes(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	// A tenant-scope time-out (tenant_person reach) blocks every brand of the tenant, not the other tenant.
	tenant := f.tenantA
	insertRestrictionRow(t, w, f.personA, &tenant, RestrictionTimeOut, time.Hour)
	for _, a := range []Action{ActionDeposit, ActionCasinoWager, ActionSportsbookWager, ActionCasinoLaunch} {
		amount := int64(1)
		if a == ActionCasinoLaunch {
			amount = 0
		}
		wantDenied(t, string(a)+" brand A1", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, a, amount)), CodeTimedOut)
		wantDenied(t, string(a)+" brand A2", w.mustEval(t, f, f.tenantA, f.action(f.acctA2, a, amount)), CodeTimedOut)
	}
	b := f.action(f.acctB, ActionDeposit, 1)
	b.TenantID, b.BrandID = f.tenantB, f.brandB
	wantAllowed(t, "the same Person in tenant B is not blocked by a tenant-scope time-out", w.mustEval(t, f, f.tenantB, b))
	wantAllowed(t, "another Person is not blocked", w.mustEval(t, f, f.tenantA, f.action(f.acctX, ActionDeposit, 1)))

	// A platform-scope time-out (the protective default) blocks the Person in EVERY tenant.
	g := w.newFix(t)
	insertRestrictionRow(t, w, g.personA, nil, RestrictionTimeOut, time.Hour)
	wantDenied(t, "platform time-out, tenant A", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 1)), CodeTimedOut)
	b = g.action(g.acctB, ActionCasinoWager, 1)
	b.TenantID, b.BrandID = g.tenantB, g.brandB
	wantDenied(t, "platform time-out, tenant B", w.mustEval(t, g, g.tenantB, b), CodeTimedOut)

	// Self-exclusion is reported ahead of a time-out when both are in force.
	insertRestrictionRow(t, w, g.personA, nil, RestrictionSelfExclusion, time.Hour)
	wantDenied(t, "self-exclusion wins", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 1)), CodeSelfExcluded)
}

func s4_TimeOut_ExpiryBoundary_UsesTheDatabaseClock(t *testing.T, w *s1World) {
	f := w.newFix(t)
	insertRestrictionRow(t, w, f.personA, nil, RestrictionTimeOut, 1500*time.Millisecond)
	wantDenied(t, "inside the time-out", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 1)), CodeTimedOut)
	// Poll until the DB clock passes ends_at: the first allowed evaluation must come within a tight envelope, and
	// the answer must not flap back to denied afterwards.
	deadline := time.Now().Add(5 * time.Second)
	var allowedAt time.Time
	for time.Now().Before(deadline) {
		d := w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 1))
		if d.Allowed {
			allowedAt = time.Now()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if allowedAt.IsZero() {
		t.Fatal("the time-out never expired")
	}
	wantAllowed(t, "after expiry", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionDeposit, 1)))
	// ends_at is EXCLUSIVE: a time-out whose ends_at is already in the past or equal to now is over.
	g := w.newFix(t)
	insertRestrictionRow(t, w, g.personA, nil, RestrictionTimeOut, -1*time.Millisecond)
	wantAllowed(t, "ends_at in the past", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionDeposit, 1)))
}

func s4_TimeOut_NeverChangesWalletOrAccountStatus(t *testing.T, w *s1World) {
	f := w.newFix(t)
	wallet := w.wallet(t, f.tenantA, f.acctA1)
	insertRestrictionRow(t, w, f.personA, nil, RestrictionTimeOut, time.Hour)
	var status, wstatus string
	wantOK(t, "status", w.owner.WithTenant(context.Background(), f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `SELECT status FROM player_accounts WHERE id = $1`, f.acctA1).Scan(&status); e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT status FROM wallets WHERE id = $1`, wallet).Scan(&wstatus)
	}))
	if status != "active" || wstatus != "active" {
		t.Fatalf("a time-out must not change account or wallet status: %q %q", status, wstatus)
	}
	// The withdrawal-side RG evaluation allows it and reports the restriction (WD-RG-1 decision).
	var d Decision
	wantOK(t, "withdrawal eligibility", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = EvaluateWithdrawalEligibility(ctx, tx, EligibilityParams{TenantID: f.tenantA, BrandID: f.brandA1, PlayerAccountID: f.acctA1, WalletID: wallet})
		return e
	}))
	wantAllowed(t, "withdrawal during a time-out", d)
	if d.RestrictionInForce != string(RestrictionTimeOut) {
		t.Fatalf("the restriction in force must be reported, got %q", d.RestrictionInForce)
	}
}

// ---- fail closed -----------------------------------------------------------------------------------------------

func s4_FailClosed(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("1000"))
	for i := 0; i < 30; i++ { // enough rows that a 1 ms statement budget cannot be met
		w.stake(t, f.tenantA, f.acctA1, "1", time.Time{})
	}

	t.Run("RepeatableRead_IsRefused", func(t *testing.T) {
		err := do(w.rt, sess{tenant: f.tenantA.String(), principal: f.staffA.String(), iso: pgx.RepeatableRead}, func(ctx context.Context, tx pgx.Tx) error {
			_, e := EvaluateAction(ctx, tx, f.action(f.acctA1, ActionCasinoWager, 1))
			return e
		})
		if !errors.Is(err, ErrEvaluationUnavailable) || !errors.Is(err, ErrUsageIsolation) {
			t.Fatalf("a REPEATABLE READ caller must fail closed with ErrUsageIsolation, got %v", err)
		}
		err = do(w.rt, sess{tenant: f.tenantA.String(), principal: f.staffA.String(), iso: pgx.Serializable}, func(ctx context.Context, tx pgx.Tx) error {
			_, e := EvaluateAction(ctx, tx, f.action(f.acctA1, ActionCasinoWager, 1))
			return e
		})
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("a SERIALIZABLE caller must fail closed, got %v", err)
		}
	})

	t.Run("PlayerScopedSession_IsRefused", func(t *testing.T) {
		err := do(w.rt, playerSess(f.tenantA, f.acctA1), func(ctx context.Context, tx pgx.Tx) error {
			_, e := EvaluateAction(ctx, tx, f.action(f.acctA1, ActionCasinoWager, 1))
			return e
		})
		if !errors.Is(err, ErrEvaluationUnavailable) || !errors.Is(err, ErrUsageScope) {
			t.Fatalf("a player-scoped session would under-count a Person total: want ErrUsageScope, got %v", err)
		}
	})

	t.Run("StatementTimeout_FailsClosed_NeverZeroUsage", func(t *testing.T) {
		p := f.action(f.acctA1, ActionCasinoWager, 1)
		p.StatementTimeout = time.Millisecond
		_, err := w.eval(f, f.tenantA, p)
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("a statement timeout must fail closed, got %v", err)
		}
	})

	t.Run("LockTimeout_FailsClosed", func(t *testing.T) {
		ctx := context.Background()
		holder, err := w.rt.Raw().BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('player_restrictions'), hashtext($1::text))`, f.personA.String()); err != nil {
			t.Fatal(err)
		}
		p := f.action(f.acctA1, ActionCasinoWager, 1)
		p.LockTimeout = 150 * time.Millisecond
		started := time.Now()
		_, err = w.eval(f, f.tenantA, p)
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("a lock timeout must fail closed, got %v", err)
		}
		if time.Since(started) > 3*time.Second {
			t.Fatalf("the lock wait must be bounded by the lock timeout, took %s", time.Since(started))
		}
	})

	t.Run("UndeclaredLeg_FailsClosed", func(t *testing.T) {
		g := w.s4Fix(t, nil)
		w.activate(t, g, nil)
		w.setLimit(t, g.tenantA, g.acctA1, ControlWager, PeriodRolling24h, strp("1000"))
		// A casino posting that locks CASH: the shape the measurement refuses (S3), not producible by today's postBet.
		w.s3Post(t, g.tenantA, s3Tx{typ: "casino_bet", asset: "USD", legs: []s3Leg{cashDr(g.acctA1, "10"), lockedCashCr(g.acctA1, "10")}})
		_, err := w.eval(g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 1))
		if !errors.Is(err, ErrEvaluationUnavailable) || !errors.Is(err, ErrUnrecognizedUsageLeg) {
			t.Fatalf("an undeclared ledger leg must fail closed, got %v", err)
		}
	})

	t.Run("CountBonusFundedOnCasino_FailsClosed", func(t *testing.T) {
		g := w.s4Fix(t, map[string]rgfix.Spec{"wager": {"count_bonus_funded": true}})
		w.activate(t, g, nil)
		_, err := w.eval(g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 1))
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("count_bonus_funded on the casino path must fail closed (S3 condition (a)), got %v", err)
		}
		// The sportsbook path has no locked-bonus casino shape; it is not refused up front.
		if _, err := w.eval(g, g.tenantA, g.action(g.acctA1, ActionSportsbookWager, 1)); err != nil {
			t.Fatalf("count_bonus_funded on the sportsbook path: %v", err)
		}
	})

	t.Run("SessionAggregationAtPersonScope_FailsClosed", func(t *testing.T) {
		g := w.s4Fix(t, nil)
		w.activate(t, g, nil)
		w.floor(t, rgfix.EnforcedFloor(g.jur, "session_duration", "", rgfix.Spec{"scope": "tenant_person",
			"concurrent_chain_aggregation": true, "concurrent_chain_aggregation_none": false}))
		for _, a := range []Action{ActionDeposit, ActionCasinoWager, ActionSportsbookWager, ActionCasinoLaunch} {
			amount := int64(1)
			if a == ActionCasinoLaunch {
				amount = 0
			}
			_, err := w.eval(g, g.tenantA, g.action(g.acctA1, a, amount))
			if !errors.Is(err, ErrEvaluationUnavailable) {
				t.Fatalf("%s: concurrent_chain_aggregation at tenant_person scope cannot be measured and must fail closed (S3 condition (c)), got %v", a, err)
			}
		}
	})

	t.Run("TimeoutsAreRestored", func(t *testing.T) {
		wantOK(t, "restore", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, `SELECT set_config('lock_timeout', '7s', true), set_config('statement_timeout', '9s', true)`); e != nil {
				return e
			}
			if _, e := EvaluateAction(ctx, tx, f.action(f.acctA1, ActionCasinoWager, 1)); e != nil {
				return e
			}
			var l, s string
			if e := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&l, &s); e != nil {
				return e
			}
			if l != "7s" || s != "9s" {
				t.Fatalf("the caller's timeouts must be restored (the ledger post runs under them), got lock=%s statement=%s", l, s)
			}
			return nil
		}))
	})

	t.Run("InvalidInput", func(t *testing.T) {
		for name, p := range map[string]ActionParams{
			"zero amount":    f.action(f.acctA1, ActionCasinoWager, 0),
			"negative":       f.action(f.acctA1, ActionDeposit, -5),
			"launch amount":  f.action(f.acctA1, ActionCasinoLaunch, 5),
			"unknown action": f.action(f.acctA1, Action("nope"), 5),
			"no asset":       {TenantID: f.tenantA, BrandID: f.brandA1, PlayerAccountID: f.acctA1, Action: ActionDeposit, Amount: 1},
		} {
			if _, err := w.eval(f, f.tenantA, p); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s: want ErrInvalidInput, got %v", name, err)
			}
		}
	})
}

// ---- concurrency -------------------------------------------------------------------------------------------------

func s4_Concurrent_StakesRacingAWagerLimit_ExactlyTheAllowedOnesPost(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("500"))
	wallet := w.wallet(t, f.tenantA, f.acctA1)
	const n, stake = 12, 100
	ok, errs := parallel(n, func(i int) error {
		return do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			d, err := EvaluateAction(ctx, tx, f.action(f.acctA1, ActionCasinoWager, stake))
			if err != nil {
				return err
			}
			if !d.Allowed {
				return errRollback
			}
			return postStake(ctx, tx, f.tenantA, wallet, stake)
		})
	})
	for _, e := range errs {
		if !errors.Is(e, errRollback) {
			t.Fatalf("unexpected error (only RG declines are expected): %v", e)
		}
	}
	if ok != 5 {
		t.Fatalf("limit 500 / stake 100: exactly 5 of %d concurrent stakes may post, got %d", n, ok)
	}
	u := w.measureStake(t, f, f.tenantA, UsageRequest{TenantID: f.tenantA, PlayerAccountID: f.acctA1, Scope: LimitScopeAccount, AssetCode: "USD",
		Windows: []WindowSpec{{Period: PeriodRolling24h}}})
	wantAmt(t, "posted wager usage", u.Windows[0].Wager.Account, "500")
}

func s4_Concurrent_TwoBrandsOfOnePerson_ShareOnePersonLimit(t *testing.T, w *s1World) {
	f := w.s4Fix(t, map[string]rgfix.Spec{"wager": {"scope": "tenant_person"}})
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("400"))
	wallets := map[uuid.UUID]uuid.UUID{f.acctA1: w.wallet(t, f.tenantA, f.acctA1), f.acctA2: w.wallet(t, f.tenantA, f.acctA2)}
	const n, stake = 12, 100
	ok, errs := parallel(n, func(i int) error {
		acct := f.acctA1
		if i%2 == 1 {
			acct = f.acctA2
		}
		return do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			d, err := EvaluateAction(ctx, tx, f.action(acct, ActionCasinoWager, stake))
			if err != nil {
				return err
			}
			if !d.Allowed {
				return errRollback
			}
			return postStake(ctx, tx, f.tenantA, wallets[acct], stake)
		})
	})
	for _, e := range errs {
		if !errors.Is(e, errRollback) {
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if ok != 4 {
		t.Fatalf("a tenant_person limit of 400 shared by two brands: exactly 4 stakes may post, got %d", ok)
	}
}

func s4_Concurrent_DepositsRacingADepositLimit_InFlightIntentsCount(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlDeposit, PeriodRolling24h, strp("500"))
	wallet := w.wallet(t, f.tenantA, f.acctA1)
	const n, amount = 8, 200
	ok, errs := parallel(n, func(i int) error {
		return do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			id := uuid.New()
			if _, e := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
				VALUES ($1, $2, $3, $4, $5, 'USD', $6, 'card', $7)`, id, f.tenantA, f.brandA1, f.acctA1, wallet, amount, "s4-"+id.String()); e != nil {
				return e
			}
			p := f.action(f.acctA1, ActionDeposit, amount)
			p.CandidateRef = &id
			d, err := EvaluateAction(ctx, tx, p)
			if err != nil {
				return err
			}
			if !d.Allowed {
				return errRollback
			}
			return nil // the intent stays pending (in flight): it counts for the next evaluation
		})
	})
	for _, e := range errs {
		if !errors.Is(e, errRollback) {
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if ok != 2 {
		t.Fatalf("limit 500 / deposit 200: exactly 2 of %d concurrent deposit intents may proceed, got %d", n, ok)
	}
}

func s4_Concurrent_DecreaseRacingAStake(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("1000"))
	wallet := w.wallet(t, f.tenantA, f.acctA1)
	// A stake of 600 racing a decrease to 400: the two serialise on L0.4. Either the stake commits first (usage 600,
	// then the decrease applies to the NEXT stake) or the decrease first (the stake is refused). Never a stake above
	// the limit in force at its own evaluation, and never an error.
	for round := 0; round < 6; round++ {
		g := w.s4Fix(t, nil)
		w.activate(t, g, nil)
		w.setLimit(t, g.tenantA, g.acctA1, ControlWager, PeriodRolling24h, strp("1000"))
		gw := w.wallet(t, g.tenantA, g.acctA1)
		var posted bool
		_, _ = parallel(2, func(i int) error {
			if i == 0 {
				return do(w.rt, staffSess(g.tenantA, g.staffA), func(ctx context.Context, tx pgx.Tx) error {
					d, err := EvaluateAction(ctx, tx, g.action(g.acctA1, ActionCasinoWager, 600))
					if err != nil {
						return err
					}
					if !d.Allowed {
						return errRollback
					}
					posted = true
					return postStake(ctx, tx, g.tenantA, gw, 600)
				})
			}
			w.setLimit(t, g.tenantA, g.acctA1, ControlWager, PeriodRolling24h, strp("400"))
			return nil
		})
		if posted {
			wantDenied(t, "after the race the decrease binds the next stake", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 1)), CodeWagerLimitReached)
		} else {
			wantDenied(t, "the stake was refused, so the decrease won", w.mustEval(t, g, g.tenantA, g.action(g.acctA1, ActionCasinoWager, 401)), CodeWagerLimitReached)
		}
	}
	_ = wallet
}

func s4_Concurrent_TimeOutRacingALaunch(t *testing.T, w *s1World) {
	for round := 0; round < 6; round++ {
		f := w.newFix(t)
		var launched bool
		_, _ = parallel(2, func(i int) error {
			if i == 0 {
				return do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
					d, err := EvaluateAction(ctx, tx, f.action(f.acctA1, ActionCasinoLaunch, 0))
					if err != nil {
						return err
					}
					launched = d.Allowed
					return nil
				})
			}
			return do(w.rt, playerSess(f.tenantA, f.acctA1), func(ctx context.Context, tx pgx.Tx) error {
				_, e := NewLimits(LimitsConfig{}).CreateTimeOut(ctx, tx, TimeOutInput{TenantID: f.tenantA, PlayerAccountID: f.acctA1, DurationHours: 1, IdempotencyKey: uuid.NewString()})
				return e
			})
		})
		// Whatever the order, once both committed the Person is timed out.
		_ = launched
		wantDenied(t, "after the race", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoLaunch, 0)), CodeTimedOut)
	}
}

// ---- relink --------------------------------------------------------------------------------------------------------

func s4_RelinkTakesBothPersonLocks_AndAnInFlightEvaluationFollowsTheMove(t *testing.T, w *s1World) {
	ctx := context.Background()

	t.Run("RelinkWaitsForAnInFlightEvaluation", func(t *testing.T) {
		f := w.newFix(t)
		other := uuid.New()
		wantOK(t, "other person", w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, other)
			return e
		}))
		// An evaluation holds the L0.4 lock of the account's Person for the rest of its transaction.
		evalTx, err := w.rt.Raw().BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = evalTx.Rollback(ctx) }()
		for _, kv := range [][2]string{{"app.tenant_id", f.tenantA.String()}, {"app.principal_id", f.staffA.String()}} {
			if _, err := evalTx.Exec(ctx, `SELECT set_config($1, $2, true)`, kv[0], kv[1]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := EvaluateEligibility(ctx, evalTx, EligibilityParams{TenantID: f.tenantA, BrandID: f.brandA1, PlayerAccountID: f.acctA1}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- w.owner.WithTenant(ctx, f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE player_accounts SET person_id = $2 WHERE id = $1`, f.acctA1, other)
				return e
			})
		}()
		select {
		case err := <-done:
			t.Fatalf("a re-link must wait for the in-flight evaluation of the old Person (it returned: %v)", err)
		case <-time.After(700 * time.Millisecond):
		}
		if err := evalTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("re-link after the evaluation ended: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the re-link never proceeded after the evaluation ended")
		}
	})

	t.Run("EvaluationFollowsAMoveCommittedBeforeItsLock", func(t *testing.T) {
		f := w.newFix(t)
		other := uuid.New()
		wantOK(t, "other person", w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, other)
			return e
		}))
		// The NEW Person holds an indefinite self-exclusion; the OLD one holds nothing.
		insertRestrictionRow(t, w, other, nil, RestrictionSelfExclusion, time.Hour)

		// A holder keeps the OLD Person's lock, so a re-link and an evaluation both queue behind it.
		holder, err := w.owner.Raw().BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('player_restrictions'), hashtext($1::text))`, f.personA.String()); err != nil {
			t.Fatal(err)
		}
		relinked := make(chan error, 1)
		go func() {
			relinked <- w.owner.WithTenant(ctx, f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE player_accounts SET person_id = $2 WHERE id = $1`, f.acctA1, other)
				return e
			})
		}()
		waitForAdvisoryWaiters(t, w, 1) // the re-link is queued first
		var dec Decision
		evalDone := make(chan error, 1)
		go func() {
			evalDone <- do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
				var e error
				dec, e = EvaluateEligibility(ctx, tx, EligibilityParams{TenantID: f.tenantA, BrandID: f.brandA1, PlayerAccountID: f.acctA1})
				return e
			})
		}()
		waitForAdvisoryWaiters(t, w, 2) // the evaluation read the account (OLD Person) and queued behind the re-link
		if err := holder.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-relinked; err != nil {
			t.Fatalf("re-link: %v", err)
		}
		if err := <-evalDone; err != nil {
			t.Fatalf("evaluation: %v", err)
		}
		// Without the post-lock Person re-read the evaluation would have used the OLD Person (no restriction) and allowed.
		if dec.Allowed || dec.Code != CodeSelfExcluded || dec.PersonID != other {
			t.Fatalf("the evaluation must follow the re-link to the new Person's self-exclusion, got %+v", dec)
		}
	})
}

// waitForAdvisoryWaiters polls pg_locks until n backends wait for an advisory lock.
func waitForAdvisoryWaiters(t *testing.T, w *s1World, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var c int
		if err := w.owner.Raw().QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE l.locktype = 'advisory' AND NOT l.granted AND a.datname = current_database()`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c >= n {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d advisory-lock waiters", n)
}

// ---- tenant isolation ----------------------------------------------------------------------------------------

func s4_TenantIsolation_LimitsAndFloorsAreNotShared(t *testing.T, w *s1World) {
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil) // tenant A only
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("1"))
	// Tenant B (the same jurisdiction, so the same floors) has NO activation: not enforced, however A is configured.
	b := f.action(f.acctB, ActionCasinoWager, 1_000_000)
	b.TenantID, b.BrandID = f.tenantB, f.brandB
	wantAllowed(t, "tenant B is not enforced by tenant A's activation", w.mustEval(t, f, f.tenantB, b))
	// Usage of the same Person in tenant B never counts against tenant A's limit.
	w.stake(t, f.tenantB, f.acctB, "9999", time.Time{})
	wantAllowed(t, "tenant A account unaffected by tenant B usage", w.mustEval(t, f, f.tenantA, f.action(f.acctA1, ActionCasinoWager, 1)))
	if strings.TrimSpace(f.jur) == "" {
		t.Fatal("fixture")
	}
}

// TestRGEnforceS4 runs every S4-min scenario against ONE scratch world (creating and migrating a scratch database
// per scenario would dominate the run time); each scenario builds its own isolated jurisdiction, tenants and accounts.
func TestRGEnforceS4(t *testing.T) {
	w := newS1World(t)
	for _, c := range []struct {
		name string
		fn   func(*testing.T, *s1World)
	}{
		{"DisabledByDefault_IsEligibilityOnly", s4_DisabledByDefault_IsEligibilityOnly},
		{"SelfExclusionAndTimeOut_DoNotNeedTheSwitch", s4_SelfExclusionAndTimeOut_DoNotNeedTheSwitch},
		{"ActivationControls", s4_ActivationControls},
		{"ActivationScope_BrandVersusTenant", s4_ActivationScope_BrandVersusTenant},
		{"ConfigMissing_FailsClosed", s4_ConfigMissing_FailsClosed},
		{"SyntheticFloorIsAbsentOutsideDevTest", s4_SyntheticFloorIsAbsentOutsideDevTest},
		{"DepositLimit_UsageWindowsAndFigures", s4_DepositLimit_UsageWindowsAndFigures},
		{"Ceiling_IsAHardCap_AndAClampOnAConfirm", s4_Ceiling_IsAHardCap_AndAClampOnAConfirm},
		{"Mandatory_RequiresAValue", s4_Mandatory_RequiresAValue},
		{"Ceiling_SameScopeKey_MinOfPlayerValueAndCeiling", s4_Ceiling_SameScopeKey_MinOfPlayerValueAndCeiling},
		{"MandatoryWithCeiling_UsesTheCeiling", s4_MandatoryWithCeiling_UsesTheCeiling},
		{"Launch_AllowedWhenUsageExceedsLoweredLimit", s4_Launch_AllowedWhenUsageExceedsLoweredLimit},
		{"WagerAndLossLimits", s4_WagerAndLossLimits},
		{"WindowBoundary_Rolling24h", s4_WindowBoundary_Rolling24h},
		{"DecreaseImmediate_IncreaseAfterCoolingOff", s4_DecreaseImmediate_IncreaseAfterCoolingOff},
		{"ScopeMIN_AccountAndTenantPerson", s4_ScopeMIN_AccountAndTenantPerson},
		{"AccountScopeLimit_IsPerAccount", s4_AccountScopeLimit_IsPerAccount},
		{"TimeOut_Scopes", s4_TimeOut_Scopes},
		{"TimeOut_ExpiryBoundary_UsesTheDatabaseClock", s4_TimeOut_ExpiryBoundary_UsesTheDatabaseClock},
		{"TimeOut_NeverChangesWalletOrAccountStatus", s4_TimeOut_NeverChangesWalletOrAccountStatus},
		{"FailClosed", s4_FailClosed},
		{"Concurrent_StakesRacingAWagerLimit_ExactlyTheAllowedOnesPost", s4_Concurrent_StakesRacingAWagerLimit_ExactlyTheAllowedOnesPost},
		{"Concurrent_TwoBrandsOfOnePerson_ShareOnePersonLimit", s4_Concurrent_TwoBrandsOfOnePerson_ShareOnePersonLimit},
		{"Concurrent_DepositsRacingADepositLimit_InFlightIntentsCount", s4_Concurrent_DepositsRacingADepositLimit_InFlightIntentsCount},
		{"Concurrent_DecreaseRacingAStake", s4_Concurrent_DecreaseRacingAStake},
		{"Concurrent_TimeOutRacingALaunch", s4_Concurrent_TimeOutRacingALaunch},
		{"RelinkTakesBothPersonLocks_AndAnInFlightEvaluationFollowsTheMove", s4_RelinkTakesBothPersonLocks_AndAnInFlightEvaluationFollowsTheMove},
		{"TenantIsolation_LimitsAndFloorsAreNotShared", s4_TenantIsolation_LimitsAndFloorsAreNotShared},
		{"HeavyPlayerMeasurement", s4_HeavyPlayerMeasurement},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, w) })
	}
}
