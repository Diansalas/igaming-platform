//go:build integration

package licensingchange

// lic23 N-7 / SN-3: the payout-verification freshness window is selected by the GOVERNING basis of the player's scope (the KYC
// rulebook selection, internal/operatingmarket/rulebook.go), not by the tenant licence alone. Every assertion runs as igaming_runtime.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func (w *world) freshness(tid, brand uuid.UUID, p resident) (uuid.UUID, bool, error) {
	w.t.Helper()
	var jid uuid.UUID
	var ok bool
	err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		jid, ok, err = payoutinstrument.FreshnessJurisdiction(ctx, tx, payoutinstrument.Instrument{TenantID: tid, BrandID: brand, PlayerAccountID: p.id})
		return err
	})
	return jid, ok, err
}

func withdrawalBasis(tid uuid.UUID, jid uuid.UUID) OperatingBasisSetPayload {
	p := noLicence(tid, nil, jid, 30)
	p.OperationCode = "withdrawal"
	return p
}

func TestLic23_N7_FreshnessWindowFollowsTheGoverningBasis(t *testing.T) {
	w := newWorld(t)
	jl, jb := w.jurisdiction(), w.jurisdiction()

	// No licence, no basis: no window configured (unchanged).
	bare := w.tenant("under_platform_licence")
	bb := w.brand(bare)
	if jid, ok, err := w.freshness(bare, bb, w.seedResident(bare, bb, bCountry)); err != nil || ok || jid != uuid.Nil {
		t.Fatalf("no licence, no basis: %v %v %v, want unresolved without error", jid, ok, err)
	}
	// Licence only: the licence jurisdiction (unchanged).
	lic := w.tenant("under_platform_licence")
	lb := w.brand(lic)
	w.licence(jl, "platform", lic)
	if jid, ok, err := w.freshness(lic, lb, w.seedResident(lic, lb, bCountry)); err != nil || !ok || jid != jl {
		t.Fatalf("licence only: %v %v %v, want %v", jid, ok, err, jl)
	}
	// Basis ONLY (no licence): the basis jurisdiction, no longer an error (N-7).
	only := w.tenant("under_platform_licence")
	ob := w.brand(only)
	w.widen(KindOperatingBasisSet, withdrawalBasis(only, jb), "documented_basis")
	if jid, ok, err := w.freshness(only, ob, w.seedResident(only, ob, bCountry)); err != nil || !ok || jid != jb {
		t.Fatalf("basis only: %v %v %v, want the basis jurisdiction %v", jid, ok, err, jb)
	}
	// Licence in A AND a withdrawal basis in B: the governing basis (B) selects the window, not the licence (SN-3).
	both := w.tenant("under_platform_licence")
	bb2 := w.brand(both)
	w.licence(jl, "platform", both)
	w.widen(KindOperatingBasisSet, withdrawalBasis(both, jb), "documented_basis")
	if jid, ok, err := w.freshness(both, bb2, w.seedResident(both, bb2, bCountry)); err != nil || !ok || jid != jb {
		t.Fatalf("licence + basis: %v %v %v, want the governing basis jurisdiction %v", jid, ok, err, jb)
	}
	// A basis tenant whose scope has no withdrawal row and no licence is unresolvable: fail closed.
	noRow := w.tenant("under_platform_licence")
	nb := w.brand(noRow)
	w.widen(KindOperatingBasisSet, noLicence(noRow, nil, jb, 30), "documented_basis") // a DEPOSIT basis only
	if _, _, err := w.freshness(noRow, nb, w.seedResident(noRow, nb, bCountry)); err == nil {
		t.Fatal("bases but no row for the withdrawal scope and no licence must fail closed")
	}
	// A basis tenant whose player has no verified residence country is unresolvable: fail closed.
	if _, _, err := w.freshness(only, ob, w.seedResident(only, ob, "")); err == nil {
		t.Fatal("bases but no verified residence country must fail closed")
	}
}

// SN-1: R-KYC-POLICY evaluates the licence jurisdiction AND the jurisdictions of approved operating bases.
func TestLic23_SN1_KYCPolicyReadiness_BasisJurisdictionWithoutAPolicyFails(t *testing.T) {
	w := newWorld(t)
	ja, jb := w.jurisdiction(), w.jurisdiction()
	w.depositPolicy(ja) // policies only in the LICENCE jurisdiction
	tid := w.tenant("under_platform_licence")
	w.licence(ja, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis") // an approved basis in B, which has no policy
	svc, err := launchgov.New(launchgov.Config{Pool: w.rt, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionActivate,
		Licensing:       &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional, DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
		JurisdictionIDs: []uuid.UUID{ja}})
	if err != nil {
		t.Fatal(err)
	}
	var c launchgov.Check
	for _, x := range r.Checks {
		if x.Code == launchgov.CheckKYCPolicy {
			c = x
		}
	}
	if c.Result != launchgov.ResultFail || c.DetailCode != "no_active_policy" {
		t.Fatalf("a basis jurisdiction with no active policy must fail the check, got %+v", c)
	}
	if got := r.Refs[launchgov.RefsKYCDormantJurisdictions]; len(got) != 1 || got[0] != jb.String() {
		t.Fatalf("the dormant jurisdiction must be reported by id: %v", got)
	}
	if r.Info[launchgov.InfoKYCJurisdictionsEvaluated] != 2 {
		t.Fatalf("jurisdictions evaluated = %d, want 2", r.Info[launchgov.InfoKYCJurisdictionsEvaluated])
	}
	// Once B has a policy too, the check passes.
	w.depositPolicy(jb)
	r2, err := svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionActivate,
		Licensing:       &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional, DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
		JurisdictionIDs: []uuid.UUID{ja}})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range r2.Checks {
		if x.Code == launchgov.CheckKYCPolicy && x.Result != launchgov.ResultPass {
			t.Fatalf("both jurisdictions have a policy: %+v", x)
		}
	}
}

// SN-1: the dormancy report names a basis jurisdiction with a live tenant and no policy.
func TestLic23_SN1_DormancyReportCoversBasisJurisdictions(t *testing.T) {
	w := newWorld(t)
	ja, jb := w.jurisdiction(), w.jurisdiction()
	w.depositPolicy(ja)
	tid := w.tenant("under_platform_licence")
	w.licence(ja, "platform", tid)
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	var dormant []kyc.DormantJurisdictionTrigger
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dormant, err = kyc.ListDormantJurisdictionTriggers(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range dormant {
		if d.LicensingJurisdictionID == jb && d.TriggerType == "cumulative_deposit" {
			found = true
		}
		if d.LicensingJurisdictionID == ja && d.TriggerType == "cumulative_deposit" {
			t.Fatal("the licence jurisdiction has an active deposit policy")
		}
	}
	if !found {
		t.Fatal("the basis jurisdiction B has a live tenant and no deposit policy: it must be reported dormant")
	}
}

// SN-4: the exclusivity of a tenant licence must not depend on what the writing session can see. A platform session that also
// carries app.platform_service_id (operating_bases_platform_read excludes it) saw no basis row and the bind passed unchecked.
func TestLic23_SN4_LicenceExclusivityHoldsForABlindSession(t *testing.T) {
	w := newWorld(t)
	x, y := w.tenant("own_licence"), w.tenant("own_licence")
	jid := w.jurisdiction()
	lic := w.licence(jid, "tenant", uuid.Nil)
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", x, nil, lic, bCountry), "partner_licence_basis")
	for name, shape := range map[string]string{
		"platform service id present": `SELECT set_config('app.platform_service_id', '00000000-0000-0000-0000-000000000023', true)`,
		"acting tenant id present":    `SELECT set_config('app.acting_tenant_id', '00000000-0000-0000-0000-000000000023', true)`,
	} {
		err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, shape); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, y)
			return err
		})
		if pgCode(err) != "LC043" {
			t.Errorf("%s: binding a licence another tenant's basis used must be LC043 whatever the session can see, got %v", name, err)
		}
	}
	// The session settings are restored: a following statement in the same transaction still sees the shape it had.
	var svc string
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_service_id', '00000000-0000-0000-0000-000000000023', true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, y); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT current_setting('app.platform_service_id', true)`).Scan(&svc)
	}); err != nil {
		t.Fatalf("an unbind must work: %v", err)
	}
}

// SN-3: RG floors follow the LICENCE jurisdiction only; a tenant that also has approved operating bases gets an ACKNOWLEDGEABLE
// (non-passing) R-RG-CONFIG until RG selection follows the governing basis. A licence-only tenant is unchanged.
func TestLic23_SN3_RGConfigReadiness_BasesPresentIsAnAcknowledgeableNote(t *testing.T) {
	w := newWorld(t)
	ja, jb := w.jurisdiction(), w.jurisdiction()
	tid := w.tenant("under_platform_licence")
	w.licence(ja, "platform", tid)
	set := TenantJurisdictionConfigPayload{TenantID: tid, JurisdictionID: ja, Action: ConfigActionSet, KYCRulesetID: sp("kyc-1"),
		AMLRulesetID: sp("aml-1"), RGRulesetID: sp("rg-1"), AllowedCurrencies: []string{"EUR"}, AllowedPaymentMethods: []string{"card"}, GeoBlockList: []string{}}
	first := w.mustRequest(w.req, KindTenantJurisdictionConfigVersion, set, "configuration_initial")
	w.mustApprove(w.ap1, first.Request.ID)
	w.mustApprove(w.ap2, first.Request.ID)
	var code string
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT code FROM jurisdictions WHERE id = $1`, ja).Scan(&code)
	}); err != nil {
		t.Fatal(err)
	}
	rgfix.EnsureTestEnvironment(t, w.ownerURL)
	rgfix.Activate(t, w.owner, tid, nil, rgfix.Staff(t, w.owner, tid))
	for _, kind := range []string{"deposit", "loss", "wager"} {
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rgfix.InsertFloor(ctx, tx, rgfix.EnforcedFloor(code, kind, "EUR", nil), rgfix.DefaultSteps())
			return err
		}); err != nil {
			t.Fatalf("floor %s: %v", kind, err)
		}
	}
	svc, err := launchgov.New(launchgov.Config{Pool: w.rt, Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	rg := func() launchgov.Check {
		r, err := svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionActivate,
			Licensing:       &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional, DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
			JurisdictionIDs: []uuid.UUID{ja}})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Checks {
			if c.Code == launchgov.CheckRGConfig {
				return c
			}
		}
		t.Fatal("no R-RG-CONFIG check")
		return launchgov.Check{}
	}
	if c := rg(); c.Result != launchgov.ResultPass || c.DetailCode != "ok" {
		t.Fatalf("a licence-only tenant with floors in force passes: %+v", c)
	}
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jb, 30), "documented_basis")
	if c := rg(); c.Result != launchgov.ResultFail || c.DetailCode != "bases_present_licence_rulebook_only" {
		t.Fatalf("with an approved basis the check is an acknowledgeable non-pass: %+v", c)
	}
}
