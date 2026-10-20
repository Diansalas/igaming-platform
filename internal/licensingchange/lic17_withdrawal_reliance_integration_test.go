//go:build integration

// lic17: the withdrawal restriction policy evaluates EVERY licence the tenant relies on (security review F-1 of lic11): the tenant's
// own licence and the licences named by its approved, active, in-force operating bases (tenant-wide or of the brand). Also the
// unknown-licence-state reconciliation (F-4 / M-2) and the KYC rulebook resolution under a player-scoped session (L-3). Every
// application-level assertion runs as igaming_runtime; the owner pool provisions fixtures. No jurisdiction, country or arrangement
// here is a statement about any real regime. No real money, no real provider.
package licensingchange

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// evalWR runs the restriction evaluation as the runtime role in the tenant's own session.
func (w *world) evalWR(tid, brand uuid.UUID) withdrawal.RestrictionDecision {
	w.t.Helper()
	var d withdrawal.RestrictionDecision
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = withdrawal.EvaluateRestriction(ctx, tx, tid, brand, withdrawal.RestrictionStageRequest)
		return err
	}); err != nil {
		w.t.Fatalf("evaluate restriction: %v", err)
	}
	return d
}

// wrPolicy writes a policy row through the OWNER path (the fixture path of the 0182 guard; the governed path is tested in
// internal/withdrawalpolicy).
func (w *world) wrPolicy(tid uuid.UUID, cause withdrawal.RestrictionCause, action withdrawal.RestrictionAction) {
	w.t.Helper()
	w.ownerExec(`INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, $2, $3, 'lic17_fixture')`,
		tid, string(cause), string(action))
}

func (w *world) setLicenceStatus(lid uuid.UUID, status string) {
	w.t.Helper()
	w.narrow(KindLicenceStatusChange, LicenceStatusPayload{LicenceID: lid, Status: status}, "lic17_"+status)
}

func wantWR(t *testing.T, what string, d withdrawal.RestrictionDecision, action withdrawal.RestrictionAction, cause withdrawal.RestrictionCause, source string) {
	t.Helper()
	if d.Action != action || d.DecidingCause != cause || d.Source != source {
		t.Fatalf("%s: got action %s cause %q source %s (causes %v, licence state %s), want %s / %q / %s",
			what, d.Action, d.DecidingCause, d.Source, d.Causes, d.LicenceState, action, cause, source)
	}
	if slices.Contains(d.Causes, withdrawal.CauseLicenceMissing) && cause != withdrawal.CauseLicenceMissing {
		t.Fatalf("%s: licence_missing must not be derived for a tenant that relies on a basis, causes %v", what, d.Causes)
	}
}

// F-1 (a): platform_licence basis, NULL tenants.licence_id, the platform licence SUSPENDED. Before lic17 the evaluation read only
// tenants.licence_id and derived licence_missing; now the suspension of the licence relied on is the cause, with and without a
// configured block.
func TestLic17_F1_PlatformLicenceBasis_SuspendedLicenceIsTheCause(t *testing.T) {
	w := newWorld(t)
	j := w.jurisdiction()
	for _, configured := range []bool{false, true} {
		tid := w.tenant("under_platform_licence")
		b := w.brand(tid)
		l := w.licence(j, "platform", uuid.Nil)
		w.setCeiling(l, bCountry)
		w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", tid, nil, l, bCountry), "platform_licence_basis")
		wantWR(t, "valid platform licence basis", w.evalWR(tid, b), withdrawal.RestrictionAllow, "", withdrawal.RestrictionSourceNone)
		w.setLicenceStatus(l, "suspended")
		if configured {
			w.wrPolicy(tid, withdrawal.CauseLicenceMissing, withdrawal.RestrictionAllow) // must not be the one consulted
			w.wrPolicy(tid, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionBlock)
			d := w.evalWR(tid, b)
			wantWR(t, "suspended platform licence, configured block", d, withdrawal.RestrictionBlock, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionSourceConfigured)
			if d.LicenceState != "suspended" || d.SetVersion == 0 {
				t.Fatalf("the licence state and set version are recorded: %+v", d)
			}
			continue
		}
		d := w.evalWR(tid, b)
		wantWR(t, "suspended platform licence, no policy", d, withdrawal.RestrictionAllow, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionSourceDefault)
		if d.LicenceState != "suspended" || d.LicenceUnknown {
			t.Fatalf("the cause is known, not unknown: %+v", d)
		}
	}
}

// F-1 (b): a partner licence for ONE brand. That brand relies on it; another brand of the same tenant does not.
func TestLic17_F1_PartnerLicencePerBrand(t *testing.T) {
	w := newWorld(t)
	j1, j2 := w.jurisdiction(), w.jurisdiction()
	tid := w.tenant("under_platform_licence")
	own := w.licence(j2, "platform", tid)
	w.setCeiling(own, bCountry)
	brandA, brandB := w.brand(tid), w.brand(tid)
	partner := w.licence(j1, "tenant", uuid.Nil)
	w.setCeiling(partner, bCountry)
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", tid, &brandB, partner, bCountry), "partner_licence_basis")
	w.setLicenceStatus(partner, "suspended")
	w.wrPolicy(tid, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionBlock)
	wantWR(t, "brand B relies on the suspended partner licence", w.evalWR(tid, brandB), withdrawal.RestrictionBlock, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionSourceConfigured)
	wantWR(t, "brand A relies only on the tenant's valid licence", w.evalWR(tid, brandA), withdrawal.RestrictionAllow, "", withdrawal.RestrictionSourceNone)
	// No brand (a tenant-level evaluation) sees tenant-wide rows only: the brand's partner licence is not relied on.
	wantWR(t, "tenant level", w.evalWR(tid, uuid.Nil), withdrawal.RestrictionAllow, "", withdrawal.RestrictionSourceNone)
}

// F-1 (c): a documented no-licence basis only: no licence cause at all, and NOT licence_missing (even with a configured block for
// licence_missing).
func TestLic17_F1_DocumentedNoLicenceBasisOnly(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, w.jurisdiction(), 30), "documented_basis")
	w.wrPolicy(tid, withdrawal.CauseLicenceMissing, withdrawal.RestrictionBlock)
	d := w.evalWR(tid, b)
	wantWR(t, "documented basis only", d, withdrawal.RestrictionAllow, "", withdrawal.RestrictionSourceNone)
	if len(d.Causes) != 0 {
		t.Fatalf("a documented basis contributes no licence cause, got %v", d.Causes)
	}
}

// F-1 (d): strictest wins across the licences relied on: the tenant's own licence EXPIRED and a basis licence SUSPENDED; whichever
// cause carries the stricter configured action decides, in both orientations.
func TestLic17_F1_StrictestWinsAcrossReliedLicences(t *testing.T) {
	w := newWorld(t)
	j := w.jurisdiction()
	for _, tc := range []struct {
		expired, suspended withdrawal.RestrictionAction
		want               withdrawal.RestrictionCause
	}{
		{withdrawal.RestrictionHoldForReview, withdrawal.RestrictionBlock, withdrawal.CauseLicenceSuspended},
		{withdrawal.RestrictionBlock, withdrawal.RestrictionHoldForReview, withdrawal.CauseLicenceExpired},
	} {
		tid := w.tenant("under_platform_licence")
		b := w.brand(tid)
		own := w.licence(j, "platform", tid)
		w.setCeiling(own, bCountry)
		other := w.licence(j, "platform", uuid.Nil)
		w.setCeiling(other, bCountry)
		w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", tid, nil, other, bCountry), "platform_licence_basis")
		w.setLicenceStatus(own, "expired")
		w.setLicenceStatus(other, "suspended")
		w.wrPolicy(tid, withdrawal.CauseLicenceExpired, tc.expired)
		w.wrPolicy(tid, withdrawal.CauseLicenceSuspended, tc.suspended)
		d := w.evalWR(tid, b)
		wantWR(t, "strictest wins", d, withdrawal.RestrictionBlock, tc.want, withdrawal.RestrictionSourceConfigured)
		if !slices.Contains(d.Causes, withdrawal.CauseLicenceExpired) || !slices.Contains(d.Causes, withdrawal.CauseLicenceSuspended) {
			t.Fatalf("both causes are in force, got %v", d.Causes)
		}
	}
}

// F-1 (e): no basis anywhere: exactly the lic11 behaviour (no licence -> licence_missing; the tenant's own licence state decides).
func TestLic17_F1_NoBases_Unchanged(t *testing.T) {
	w := newWorld(t)
	j := w.jurisdiction()
	bare := w.tenant("under_platform_licence")
	wantWR(t, "no licence, no basis", w.evalWR(bare, w.brand(bare)), withdrawal.RestrictionAllow, withdrawal.CauseLicenceMissing, withdrawal.RestrictionSourceDefault)
	w.wrPolicy(bare, withdrawal.CauseLicenceMissing, withdrawal.RestrictionBlock)
	wantWR(t, "no licence, no basis, configured", w.evalWR(bare, w.brand(bare)), withdrawal.RestrictionBlock, withdrawal.CauseLicenceMissing, withdrawal.RestrictionSourceConfigured)
	lic := w.tenant("under_platform_licence")
	lb := w.brand(lic)
	l := w.licence(j, "platform", lic)
	wantWR(t, "valid own licence", w.evalWR(lic, lb), withdrawal.RestrictionAllow, "", withdrawal.RestrictionSourceNone)
	w.setLicenceStatus(l, "suspended")
	wantWR(t, "suspended own licence", w.evalWR(lic, lb), withdrawal.RestrictionAllow, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionSourceDefault)
}

// F-4 / M-2: a licence relied on that cannot be read (the tenant row not visible -> a LicensingInvalid chain; the basis licence not
// visible -> LicenceNotFound; a failing read) is UNKNOWN, never licence_missing. With NO policy row the decision is the interim
// default (allow) recorded with licence state `unknown`, not a 503; with a policy row it fails closed (ErrGateUnavailable). The
// read failures are induced by shadowing one table with an empty TEMP table in an OWNER session (the runtime role has no TEMP
// privilege; the 0182 / 0184 functions pin search_path with pg_temp last, so only the Go-side reads see the shadow).
func TestLic17_F4_M2_UnreadableLicence_UnknownWithoutPolicy_FailClosedWithPolicy(t *testing.T) {
	w := newWorld(t)
	j := w.jurisdiction()
	shadows := map[string]string{
		"tenant row not visible (LicensingInvalid)":   `CREATE TEMP TABLE tenants (id uuid, licence_id uuid) ON COMMIT DROP`,
		"basis licence not visible (LicenceNotFound)": `CREATE TEMP TABLE licences (id uuid, jurisdiction_id uuid, status text, issued_at date, expires_at date) ON COMMIT DROP`,
		"basis read fails":                            `CREATE TEMP TABLE operating_bases (unrelated int) ON COMMIT DROP`,
	}
	eval := func(tid, b uuid.UUID, shadow string) (withdrawal.RestrictionDecision, error) {
		var d withdrawal.RestrictionDecision
		// A fresh owner pool per evaluation: a statement prepared earlier on a pooled connection keeps its resolved relation
		// (creating a shadowing TEMP table does not invalidate a cached plan), which would hide the induced failure.
		fresh := connect(t, w.ownerURL)
		err := fresh.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, shadow); err != nil {
				return err
			}
			var err error
			d, err = withdrawal.EvaluateRestriction(ctx, tx, tid, b, withdrawal.RestrictionStageRequest)
			if err != nil {
				return err
			}
			// The caller's transaction is still usable (the reliance read ran in a savepoint).
			_, err = tx.Exec(ctx, `SELECT 1`)
			return err
		})
		return d, err
	}
	for name, shadow := range shadows {
		tid := w.tenant("under_platform_licence")
		b := w.brand(tid)
		l := w.licence(j, "platform", uuid.Nil)
		w.setCeiling(l, bCountry)
		w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", tid, nil, l, bCountry), "platform_licence_basis")
		d, err := eval(tid, b, shadow)
		if err != nil {
			t.Fatalf("%s, no policy: must not fail: %v", name, err)
		}
		if d.Action != withdrawal.RestrictionAllow || d.Source != withdrawal.RestrictionSourceDefault || !d.LicenceUnknown ||
			d.LicenceState != withdrawal.LicenceStateUnknown || d.DecidingCause != "" || d.SetVersion != 0 {
			t.Fatalf("%s, no policy: allow / default_unconfigured / unknown, got %+v", name, d)
		}
		if md := d.AuditMetadata(); md["restriction_licence_state"] != withdrawal.LicenceStateUnknown || md["restriction_cause"] != nil {
			t.Fatalf("%s: the audit records the unknown state and no cause, got %v", name, md)
		}
		w.wrPolicy(tid, withdrawal.CauseLicenceSuspended, withdrawal.RestrictionBlock)
		if _, err := eval(tid, b, shadow); !errors.Is(err, withdrawal.ErrGateUnavailable) {
			t.Fatalf("%s, policy configured: must fail closed, got %v", name, err)
		}
		// Control: without the shadow the same tenant evaluates normally.
		wantWR(t, name+" control", w.evalWR(tid, b), withdrawal.RestrictionAllow, "", withdrawal.RestrictionSourceNone)
	}
}

// L-3: the KYC rulebook resolution of a basis tenant from a PLAYER-scoped session. Before lic17 TenantHasApprovedBasis refused the
// player-scoped transaction and the deposit was `unavailable` (jurisdiction_unresolved); now the player GUC is cleared for the
// tenant-scoped reads and restored, and the decision equals the tenant-scoped one.
func TestLic17_L3_KYCEnforcementUnderPlayerScope(t *testing.T) {
	w := newWorld(t)
	j1 := w.jurisdiction()
	w.depositPolicy(j1)
	tid := w.tenant("under_platform_licence")
	b := w.brand(tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, j1, 30), "documented_basis")
	p := w.seedResident(tid, b, bCountry)
	wantKYC(t, "tenant scope", w.kycDeposit(tid, b, p), kyc.OutcomePassed, true)
	var d kyc.EnforcementDecision
	var after string
	if err := w.rt.WithPlayerScope(context.Background(), tid, p.id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = kyc.EvaluateEnforcement(ctx, tx, kyc.EnforcementParams{TenantID: tid, BrandID: b, PlayerAccountID: p.id, PersonID: p.person,
			Operation: kyc.EnforcementDeposit, AssetCode: "EUR", Amount: 1_000_000, CorrelationID: uuid.New()})
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT current_setting('app.player_account_id', true)`).Scan(&after)
	}); err != nil {
		t.Fatalf("evaluate kyc under player scope: %v", err)
	}
	wantKYC(t, "player scope", d, kyc.OutcomePassed, true)
	if after != p.id.String() {
		t.Fatalf("the player scope must be restored, got %q", after)
	}
	// Fail closed is unchanged: a player with no verified residence is unavailable under player scope too.
	noRes := w.seedResident(tid, b, "")
	if err := w.rt.WithPlayerScope(context.Background(), tid, noRes.id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = kyc.EvaluateEnforcement(ctx, tx, kyc.EnforcementParams{TenantID: tid, BrandID: b, PlayerAccountID: noRes.id, PersonID: noRes.person,
			Operation: kyc.EnforcementDeposit, AssetCode: "EUR", Amount: 1_000_000, CorrelationID: uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantKYC(t, "player scope, no residence", d, kyc.OutcomeUnavailable, false)
}
