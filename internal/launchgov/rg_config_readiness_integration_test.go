//go:build integration

package launchgov_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

// ADR 0115 section 6 / completion audit A6: the readiness check R-RG-CONFIG. A brand must not be activated
// while RG limit enforcement is off or while a compliance floor is missing for a currency the brand offers;
// a non-passing check cannot be ignored (the request must acknowledge the exact non-passing set).
func (w *world) rgCheck(sub launchgov.Subject, jid uuid.UUID) launchgov.Check {
	w.t.Helper()
	r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: sub, Action: launchgov.ActionActivate,
		Licensing: &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional,
			DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
		JurisdictionIDs: []uuid.UUID{jid}})
	if err != nil {
		w.t.Fatalf("readiness: %v", err)
	}
	for _, c := range r.Checks {
		if c.Code == launchgov.CheckRGConfig {
			return c
		}
	}
	w.t.Fatal("the readiness has no R-RG-CONFIG check")
	return launchgov.Check{}
}

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

func (w *world) floorFor(code, kind string, over rgfix.Spec) {
	w.t.Helper()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rgfix.InsertFloor(ctx, tx, rgfix.EnforcedFloor(code, kind, "EUR", over), rgfix.DefaultSteps())
		return err
	}); err != nil {
		w.t.Fatalf("floor %s/%s: %v", code, kind, err)
	}
}

func wantRGCheck(t *testing.T, what string, c launchgov.Check, result, detail string) {
	t.Helper()
	if c.Result != result || c.DetailCode != detail {
		t.Fatalf("%s: want %s/%s, got %s/%s", what, result, detail, c.Result, c.DetailCode)
	}
}

func TestReadiness_RGConfig_FailsClosedUntilEnforcementIsOnAndEveryFloorIsInForce(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	jid := w.jurisdiction(tid)                // allowed currency EUR
	w.licence(tid, jid, "platform", "active") // SR6R-1: enforcement reads the LICENCE jurisdiction
	code := w.jurCode(jid)
	tenantSub := launchgov.Subject{TenantID: tid}
	brandSub := launchgov.Subject{TenantID: tid, BrandID: &brand}

	wantRGCheck(t, "nothing configured", w.rgCheck(brandSub, jid), launchgov.ResultFail, "enforcement_not_enabled")

	// a subject referencing no jurisdiction cannot be evaluated as passing
	r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: brandSub, Action: launchgov.ActionActivate})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Checks {
		if c.Code == launchgov.CheckRGConfig {
			wantRGCheck(t, "no jurisdiction referenced", c, launchgov.ResultFail, "no_jurisdiction_referenced")
		}
	}

	// enforcement ON for the BRAND only: the brand subject now misses floors; the tenant subject is still off
	staff := rgfix.Staff(t, w.owner, tid)
	rgfix.Activate(t, w.owner, tid, &brand, staff)
	wantRGCheck(t, "on, no floors", w.rgCheck(brandSub, jid), launchgov.ResultFail, "floor_missing_deposit")
	wantRGCheck(t, "a brand-only activation does not satisfy a tenant subject", w.rgCheck(tenantSub, jid), launchgov.ResultFail, "enforcement_not_enabled")

	w.floorFor(code, "deposit", nil)
	wantRGCheck(t, "deposit floor only", w.rgCheck(brandSub, jid), launchgov.ResultFail, "floor_missing_loss")
	w.floorFor(code, "loss", nil)
	wantRGCheck(t, "deposit + loss", w.rgCheck(brandSub, jid), launchgov.ResultFail, "floor_missing_wager")
	w.floorFor(code, "wager", nil)
	wantRGCheck(t, "every floor in force", w.rgCheck(brandSub, jid), launchgov.ResultPass, "ok")

	// the tenant subject passes once a tenant-wide activation exists
	rgfix.Activate(t, w.owner, tid, nil, staff)
	wantRGCheck(t, "tenant-wide activation", w.rgCheck(tenantSub, jid), launchgov.ResultPass, "ok")

	// a session-duration control is NOT enforced by the platform: enabled or mandatory fails the check (honest, not a pass)
	w.floorFor(code, "session_duration", nil) // EnforcedFloor sets enabled=true
	wantRGCheck(t, "an enabled session control that is not enforced", w.rgCheck(brandSub, jid), launchgov.ResultFail, "session_limit_not_enforced")
	w.floorFor(code, "session_duration", rgfix.Spec{"scope": "tenant_person", "concurrent_chain_aggregation": true, "concurrent_chain_aggregation_none": false})
	wantRGCheck(t, "Person-scope chain aggregation cannot be measured", w.rgCheck(brandSub, jid), launchgov.ResultFail, "session_aggregation_unsupported")
}

// A currency the brand offers without a floor fails the check even when the other currency is fully configured.
func TestReadiness_RGConfig_EveryAllowedCurrencyNeedsFloors(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	jid := w.jurisdiction(tid)
	w.licence(tid, jid, "platform", "active")
	code := w.jurCode(jid)
	// allow a second currency in the tenant's jurisdiction configuration
	if err := w.owner.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenant_jurisdiction_configs SET allowed_currencies = '["EUR","USD"]' WHERE tenant_id = $1 AND jurisdiction_id = $2`, tid, jid)
		return err
	}); err != nil {
		// the table may be append-only: insert a later row instead
		if err2 := w.owner.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
				VALUES ($1, $2, 'kyc2', 'aml2', 'rg2', '["EUR","USD"]', '["card"]', now() - interval '1 minute')`, tid, jid)
			return e
		}); err2 != nil {
			t.Skipf("cannot widen the allowed currencies in this fixture: %v / %v", err, err2)
		}
	}
	brandSub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	rgfix.Activate(t, w.owner, tid, nil, rgfix.Staff(t, w.owner, tid))
	for _, k := range []string{"deposit", "loss", "wager"} {
		w.floorFor(code, k, nil) // EUR only
	}
	wantRGCheck(t, "USD has no floors", w.rgCheck(brandSub, jid), launchgov.ResultFail, "floor_missing_deposit")
}

// SR6R-1 (same gap as the activation guard): the check must also cover the LICENCE jurisdiction enforcement reads, which the
// request's JurisdictionIDs need not contain, and a tenant with no licence jurisdiction cannot pass.
func TestReadiness_RGConfig_CoversTheLicenceJurisdiction(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	jid := w.jurisdiction(tid) // the market jurisdiction named by the request: gets floors
	code := w.jurCode(jid)
	brandSub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	rgfix.Activate(t, w.owner, tid, nil, rgfix.Staff(t, w.owner, tid))
	for _, k := range []string{"deposit", "loss", "wager"} {
		w.floorFor(code, k, nil)
	}
	wantRGCheck(t, "no licence jurisdiction", w.rgCheck(brandSub, jid), launchgov.ResultFail, "no_licence_jurisdiction")

	// licensed in another jurisdiction that has NO floors: fails although the requested jurisdiction is fully configured
	other := w.jurisdiction(tid)
	w.licence(tid, other, "platform", "active")
	wantRGCheck(t, "licence jurisdiction without floors", w.rgCheck(brandSub, jid), launchgov.ResultFail, "floor_missing_deposit")

	// floors for the licence jurisdiction too: passes
	for _, k := range []string{"deposit", "loss", "wager"} {
		w.floorFor(w.jurCode(other), k, nil)
	}
	wantRGCheck(t, "both jurisdictions have floors", w.rgCheck(brandSub, jid), launchgov.ResultPass, "ok")
}
