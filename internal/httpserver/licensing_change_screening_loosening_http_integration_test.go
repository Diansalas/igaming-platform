//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// c4 (migration 0190): the governed screening policy loosening through the real HTTP stack as igaming_runtime. The two kinds ride the
// generic licensing change routes: nothing new is routed. TEST FIXTURE VALUES - NOT LEGAL THRESHOLDS.

func TestLicensingChangeHTTP_ScreeningLoosening_InertUntilConfigured_ThenTwoPersons(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	jid := w.jurisdiction(tenantID)
	// The AML officer controls apply to the requester and the approvers (c4 review fix): the three fixture operators hold platform grants.
	for _, op := range []uuid.UUID{w.ops.Requester, w.ops.Approver1, w.ops.Approver2} {
		amlfix.GrantOfficerAs(t, w.owner, tenantID, op, "platform")
	}
	// A first screening policy version (owner fixture; the guard is disabled for the one insert only).
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_policies DISABLE TRIGGER aml_screening_policies_10_guard`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds,
				effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES ($1, $2, '{pep,sanctions}', 7200, 3600, now() - interval '1 hour', 'TEST FIXTURE REF', 'fixture', $3, $4, 'platform_acting')`,
			tenantID, jid, w.ops.Requester, w.ops.Persons[0]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE aml_screening_policies ENABLE TRIGGER aml_screening_policies_10_guard`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	loosen := map[string]any{"tenant_id": tenantID, "jurisdiction_id": jid, "lists": []string{"pep", "sanctions"}, "rescreen_interval_seconds": 14400,
		"backfill_grace_seconds": 3600, "effective_offset_seconds": 0, "legal_review_reference": "TEST FIXTURE LEGAL REF", "evidence_reference": "EVID-HTTP-1"}
	file := func(tok, kind string, p map[string]any) cgResp {
		return w.do("POST", "/v1/admin/licensing-changes", tok, map[string]any{"kind": kind, "reason_code": "screening_loosening_decision", "payload": p})
	}

	// INERT: not configured => 409 LICENSING_CHANGE_GUARD_REFUSED with the one closed code; nothing is written.
	r := file(w.tokReq(), "aml_screening_policy_loosen", loosen)
	codes := r.checkCodes(t)
	if r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_GUARD_REFUSED" || len(codes) != 1 || codes[0] != "loosening_not_configured" {
		t.Fatalf("unconfigured: %d %s", r.status, r.body)
	}
	if _, ok := aml.RefusalCodes[codes[0]]; !ok {
		t.Fatalf("the code %q is not in the closed table", codes[0])
	}

	// Tenant staff: never (403 for the platform-only kinds), whatever their role.
	for _, role := range []identity.StaffRole{identity.StaffRoleTenantAdmin, identity.StaffRoleCompliance} {
		tok := w.tenantTok(tenantID, w.staff(tenantID, role), auth.Role(role))
		cfgBody := map[string]any{"tenant_id": tenantID, "enabled": true, "cooling_seconds": 0, "evidence_reference": "EVID-HTTP-T"}
		for kind, body := range map[string]map[string]any{"aml_screening_policy_loosen": loosen, "aml_screening_loosening_configure": cfgBody} {
			if r := file(tok, kind, body); r.status != http.StatusForbidden {
				t.Fatalf("tenant %s files %s: %d %s", role, kind, r.status, r.body)
			}
		}
	}

	// Configure: enabling is widening, two Persons; the requester and the same Person cannot supply the approvals.
	cfg := file(w.tokReq(), "aml_screening_loosening_configure", map[string]any{"tenant_id": tenantID, "enabled": true, "cooling_seconds": 0, "evidence_reference": "EVID-HTTP-CFG"})
	var filed licensingChangeResponseIT
	cfg.decode(t, &filed)
	if cfg.status != http.StatusCreated || filed.Executed || !filed.ChangeRequest.Widening || filed.ChangeRequest.RequiredApprovals != 2 {
		t.Fatalf("configure: %d %s", cfg.status, cfg.body)
	}
	if r := w.lcApprove(w.tokReq(), filed.ChangeRequest.ID); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_SELF_APPROVAL" {
		t.Fatalf("self approval: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.tokAp1(), filed.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("approval 1: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.lcTwinToken(), filed.ChangeRequest.ID); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_ALREADY_DECIDED" {
		t.Fatalf("same Person: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.tokAp2(), filed.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("approval 2: %d %s", r.status, r.body)
	}

	// Loosen: the tenant also cannot read either request; the final approval executes and returns the before/after outcome.
	lo := file(w.tokReq(), "aml_screening_policy_loosen", loosen)
	var lf licensingChangeResponseIT
	lo.decode(t, &lf)
	if lo.status != http.StatusCreated || lf.Executed || !lf.ChangeRequest.Widening {
		t.Fatalf("loosen: %d %s", lo.status, lo.body)
	}
	tenantTok := w.tenantTok(tenantID, w.staff(tenantID, identity.StaffRoleCompliance), auth.RoleCompliance)
	if r := w.do("GET", "/v1/admin/licensing-changes/"+lf.ChangeRequest.ID, tenantTok, nil); r.status != http.StatusNotFound {
		t.Fatalf("tenant reads the loosening request: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.tokAp1(), lf.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("approval 1: %d %s", r.status, r.body)
	}
	done := w.lcApprove(w.tokAp2(), lf.ChangeRequest.ID)
	var ex licensingChangeResponseIT
	done.decode(t, &ex)
	if done.status != http.StatusOK || !ex.Executed || ex.Outcome["policy_id"] == nil || ex.Outcome["previous_rescreen_interval_seconds"] != float64(7200) {
		t.Fatalf("execution: %d %s", done.status, done.body)
	}
	var n int
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_policies WHERE tenant_id = $1 AND created_by_scope = 'platform' AND change_request_id = $2`,
			tenantID, uuid.MustParse(lf.ChangeRequest.ID)).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("exactly one platform-authored version for the request: %d %v", n, err)
	}
	// A loosening that is not one is refused with its own closed code, and the tighten-only route stays the way to tighten.
	same := map[string]any{}
	for k, v := range loosen {
		same[k] = v
	}
	same["rescreen_interval_seconds"] = 14400
	same["backfill_grace_seconds"] = 3600
	if r := file(w.tokReq(), "aml_screening_policy_loosen", same); r.status != http.StatusConflict || len(r.checkCodes(t)) != 1 || r.checkCodes(t)[0] != "not_a_loosening" {
		t.Fatalf("not a loosening: %d %s", r.status, r.body)
	}
}
