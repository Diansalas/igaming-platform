//go:build integration

// A8: the R-MARKET-CONFIG readiness check ("operating market configured"). A brand whose operating
// market is not configured is refused at registration, deposit and launch by the market gate
// (ADR 0045: a tenant must opt in), so an approver must see it before authorising a launch. The check
// resolves through operatingmarket.ResolveOperatingCountryPolicy itself.
package launchgov_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func (w *world) bindLicence(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	licenceID, jid := uuid.New(), uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Market Config Test')`, jid, "MC-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			licenceID, jid, "MCL-"+licenceID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, tenantID, licenceID)
		return err
	}); err != nil {
		w.t.Fatalf("bind licence: %v", err)
	}
	return licenceID
}

func (w *world) ceiling(licenceID uuid.UUID, cc string, state operatingmarket.State) {
	w.t.Helper()
	auth := ""
	if state == operatingmarket.StateEnabled {
		auth = "legal-ref"
	}
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: cc, State: state, Status: operatingmarket.StatusActive,
			AuthorizationReference: auth, Actor: operatingmarket.ActorContext{ActorID: w.ops.Requester, ReasonCode: "a8-readiness-test"},
		})
		return err
	}); err != nil {
		w.t.Fatalf("ceiling: %v", err)
	}
}

func (w *world) policy(tenantID uuid.UUID, p operatingmarket.CreateOperatingCountryPolicyVersionParams) {
	w.t.Helper()
	p.TenantID = tenantID
	p.Status = operatingmarket.StatusActive
	if p.State == operatingmarket.StateEnabled {
		p.AuthorizationReference = "compliance-ref"
	}
	p.Actor = operatingmarket.ActorContext{ActorID: w.ops.Requester, ReasonCode: "a8-readiness-test"}
	if err := w.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, p)
		return err
	}); err != nil {
		w.t.Fatalf("policy: %v", err)
	}
}

func (w *world) marketCheck(sub launchgov.Subject) launchgov.Check {
	w.t.Helper()
	r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: sub, Action: launchgov.ActionActivate})
	if err != nil {
		w.t.Fatalf("readiness: %v", err)
	}
	for _, c := range r.Checks {
		if c.Code == launchgov.CheckMarketConfig {
			return c
		}
	}
	w.t.Fatalf("readiness has no %s check: %+v", launchgov.CheckMarketConfig, r.Checks)
	return launchgov.Check{}
}

func wantCheck(t *testing.T, got launchgov.Check, result, detail string) {
	t.Helper()
	if got.Result != result || got.DetailCode != detail {
		t.Fatalf("R-MARKET-CONFIG: want %s/%s, got %s/%s", result, detail, got.Result, got.DetailCode)
	}
}

func TestReadiness_MarketConfig(t *testing.T) {
	w := newWorld(t)
	tenantID := w.activeTenant()
	brandID := w.activeBrand(tenantID)
	tenantSub := launchgov.Subject{TenantID: tenantID}
	brandSub := launchgov.Subject{TenantID: tenantID, BrandID: &brandID}

	// Nothing configured (not even a licence): fail, and never "pass" by absence.
	wantCheck(t, w.marketCheck(tenantSub), launchgov.ResultFail, "no_market_configured")
	wantCheck(t, w.marketCheck(brandSub), launchgov.ResultFail, "no_market_configured")

	// Ceiling + tenant opt-in: the market is configured (brand inherits).
	licence := w.bindLicence(tenantID)
	w.ceiling(licence, "MT", operatingmarket.StateEnabled)
	w.policy(tenantID, operatingmarket.CreateOperatingCountryPolicyVersionParams{Scope: operatingmarket.ScopeTenant, CountryCode: "MT", State: operatingmarket.StateEnabled})
	wantCheck(t, w.marketCheck(tenantSub), launchgov.ResultPass, "ok")
	wantCheck(t, w.marketCheck(brandSub), launchgov.ResultPass, "ok")

	// The tenant row alone is not enough: with the licence ceiling disabled the opt-in is orphaned
	// (configured, but no country permits anything).
	w.ceiling(licence, "MT", operatingmarket.StateDisabled)
	wantCheck(t, w.marketCheck(brandSub), launchgov.ResultFail, "no_country_permits_all_operations")
	w.ceiling(licence, "MT", operatingmarket.StateEnabled)
	wantCheck(t, w.marketCheck(brandSub), launchgov.ResultPass, "ok")

	// One gated action disabled for the only country: no country permits all three.
	w.policy(tenantID, operatingmarket.CreateOperatingCountryPolicyVersionParams{
		Scope: operatingmarket.ScopeOperation, BrandID: &brandID, OperationCode: "deposit", CountryCode: "MT", State: operatingmarket.StateDisabled,
	})
	wantCheck(t, w.marketCheck(brandSub), launchgov.ResultFail, "no_country_permits_all_operations")
	// ... the tenant-level subject (no brand rung) is not affected by the brand's operation row.
	wantCheck(t, w.marketCheck(tenantSub), launchgov.ResultPass, "ok")

	// A second country that IS open for all three satisfies the check again.
	w.ceiling(licence, "DE", operatingmarket.StateEnabled)
	w.policy(tenantID, operatingmarket.CreateOperatingCountryPolicyVersionParams{Scope: operatingmarket.ScopeTenant, CountryCode: "DE", State: operatingmarket.StateEnabled})
	wantCheck(t, w.marketCheck(brandSub), launchgov.ResultPass, "ok")

	// Tenant isolation: another tenant's configuration never counts.
	other := w.activeTenant()
	wantCheck(t, w.marketCheck(launchgov.Subject{TenantID: other}), launchgov.ResultFail, "no_market_configured")
}

func TestReadiness_MarketConfig_WithdrawnOrDisabledTenantRowIsNotConfigured(t *testing.T) {
	w := newWorld(t)
	tenantID := w.activeTenant()
	licence := w.bindLicence(tenantID)
	w.ceiling(licence, "MT", operatingmarket.StateEnabled)
	w.policy(tenantID, operatingmarket.CreateOperatingCountryPolicyVersionParams{Scope: operatingmarket.ScopeTenant, CountryCode: "MT", State: operatingmarket.StateEnabled})
	sub := launchgov.Subject{TenantID: tenantID}
	wantCheck(t, w.marketCheck(sub), launchgov.ResultPass, "ok")
	w.policy(tenantID, operatingmarket.CreateOperatingCountryPolicyVersionParams{Scope: operatingmarket.ScopeTenant, CountryCode: "MT", State: operatingmarket.StateDisabled})
	wantCheck(t, w.marketCheck(sub), launchgov.ResultFail, "no_market_configured")
}

// R-MARKET-CONFIG is a configuration check: evaluated for activate/reactivate/ratify, never for the
// funds-only actions (close/suspend).
func TestReadiness_MarketConfig_OnlyInTheConfigurationCheckSet(t *testing.T) {
	w := newWorld(t)
	tenantID := w.activeTenant()
	for _, tc := range []struct {
		action string
		want   bool
	}{{launchgov.ActionActivate, true}, {launchgov.ActionReactivate, true}, {launchgov.ActionRatify, true}, {launchgov.ActionClose, false}, {launchgov.ActionSuspend, false}} {
		r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tenantID}, Action: tc.action})
		if err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		has := false
		for _, c := range r.Checks {
			if c.Code == launchgov.CheckMarketConfig {
				has = true
			}
		}
		if has != tc.want {
			t.Fatalf("%s: R-MARKET-CONFIG present=%v, want %v", tc.action, has, tc.want)
		}
	}
}
