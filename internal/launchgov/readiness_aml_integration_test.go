//go:build integration

package launchgov_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// ADR 0116 s6 / S7 (A7) against the real database: the AML readiness items follow the officer quorum, the screening policy
// per referenced jurisdiction and the registered screening provider; transaction monitoring is ALWAYS an explicit failing item.

func (w *world) amlJurisdiction(tenantID uuid.UUID) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	jid := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'aml readiness fixture')`, jid, "AR-"+jid.String()[:12])
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	if err := w.owner.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs
			(tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', '["EUR"]', '["card"]', now() - interval '1 hour')`, tenantID, jid)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return jid
}

func amlCheckMap(t *testing.T, r launchgov.Readiness) map[string]launchgov.Check {
	t.Helper()
	m := map[string]launchgov.Check{}
	for _, c := range r.Checks {
		m[c.Code] = c
	}
	for _, code := range []string{launchgov.CheckAMLQuorum, launchgov.CheckAMLScreening, launchgov.CheckAMLMonitoring} {
		if _, ok := m[code]; !ok {
			t.Fatalf("readiness lacks %s: %+v", code, r.Checks)
		}
	}
	return m
}

func TestReadiness_AML_FollowsQuorumPolicyAndProvider(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	tid := w.activeTenant()
	j := w.amlJurisdiction(tid)
	mock := aml.NewMockScreeningProvider()
	comps := []providerkind.Registration{{Domain: "aml", Name: "screening_provider", Component: mock}}
	eval := func(env string, comps []providerkind.Registration) map[string]launchgov.Check {
		t.Helper()
		svc := w.service(launchgov.Config{Environment: env, Components: comps})
		r, err := svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionActivate, JurisdictionIDs: []uuid.UUID{j}})
		if err != nil {
			t.Fatal(err)
		}
		return amlCheckMap(t, r)
	}

	// Nothing configured: quorum fails, screening fails (no policy), monitoring fails.
	m := eval("test", comps)
	if m[launchgov.CheckAMLQuorum].DetailCode != "quorum_not_met"+launchgov.AMLPlatformScopeSuffix || m[launchgov.CheckAMLScreening].DetailCode != "policy_missing_for_jurisdiction" ||
		m[launchgov.CheckAMLMonitoring].Result != launchgov.ResultFail {
		t.Fatalf("unconfigured: %+v", m)
	}
	// Officer quorum (tenant is platform-licensed, so a PLATFORM quorum) + a screening policy for the jurisdiction.
	q := amlfix.Bootstrap(t, w.owner, tid, 2)
	m = eval("test", comps)
	if m[launchgov.CheckAMLQuorum].Result != launchgov.ResultPass {
		t.Fatalf("two officers are a quorum: %+v", m[launchgov.CheckAMLQuorum])
	}
	svc := amlfix.Service(t, w.rt)
	if _, err := svc.SetScreeningPolicy(ctx, q.Officers[0].Actor, aml.SetScreeningPolicyInput{JurisdictionID: j, Lists: []aml.ListType{aml.ListSanctions},
		RescreenInterval: time.Hour, EffectiveFrom: time.Now().Add(-time.Hour), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	// MOCK provider + policy: configured outside production ...
	m = eval("test", comps)
	if m[launchgov.CheckAMLScreening].Result != launchgov.ResultPass || m[launchgov.CheckAMLScreening].DetailCode != "ok_mock_provider" {
		t.Fatalf("a mock counts as configured outside production: %+v", m[launchgov.CheckAMLScreening])
	}
	// ... and NOT in production.
	m = eval("production", comps)
	if m[launchgov.CheckAMLScreening].Result != launchgov.ResultFail || m[launchgov.CheckAMLScreening].DetailCode != "synthetic_provider_in_production" {
		t.Fatalf("a mock must not count in production: %+v", m[launchgov.CheckAMLScreening])
	}
	// No provider registered at all.
	m = eval("test", []providerkind.Registration{{Domain: "payments", Name: "provider", Component: mock}})
	if m[launchgov.CheckAMLScreening].DetailCode != "no_screening_provider" {
		t.Fatalf("no provider: %+v", m[launchgov.CheckAMLScreening])
	}
	// Monitoring stays failing with everything else configured.
	m = eval("test", comps)
	if m[launchgov.CheckAMLMonitoring].Result != launchgov.ResultFail || m[launchgov.CheckAMLMonitoring].DetailCode != "monitoring_not_implemented" {
		t.Fatalf("monitoring: %+v", m[launchgov.CheckAMLMonitoring])
	}
	// Losing the quorum makes the item fail again.
	amlfix.RevokeOfficer(t, w.owner, tid, q.Officers[1].StaffID)
	if got := eval("test", comps)[launchgov.CheckAMLQuorum]; got.Result != launchgov.ResultFail {
		t.Fatalf("a revoked officer leaves the quorum: %+v", got)
	}
}

// The close / suspend actions evaluate the funds check only: the AML configuration items are not asked of them.
func TestReadiness_AML_NotPartOfCloseOrSuspend(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	for _, action := range []string{launchgov.ActionClose, launchgov.ActionSuspend} {
		r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: action})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Checks {
			if c.Code == launchgov.CheckAMLQuorum || c.Code == launchgov.CheckAMLScreening || c.Code == launchgov.CheckAMLMonitoring {
				t.Errorf("%s must not carry %s", action, c.Code)
			}
		}
	}
}
