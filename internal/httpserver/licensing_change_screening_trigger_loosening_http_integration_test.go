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

// d1 (migration 0191, D4): the governed loosening of the screening TRIGGER policy through the real HTTP stack as igaming_runtime. The third kind
// rides the generic licensing change routes: nothing new is routed. TEST FIXTURE VALUES - NOT LEGAL THRESHOLDS.
func TestLicensingChangeHTTP_ScreeningTriggerLoosening_InertUntilConfigured_ThenTwoPersons_NeverEnablesAGate(t *testing.T) {
	w := newLHWorld(t)
	tenantID := w.activeTenant()
	jid := w.jurisdiction(tenantID)
	for _, op := range []uuid.UUID{w.ops.Requester, w.ops.Approver1, w.ops.Approver2} {
		amlfix.GrantOfficerAs(t, w.owner, tenantID, op, "platform")
	}
	// A screening policy (7200 s) and a GATING trigger policy (threshold 1800 s) as the owner fixture (the guards are disabled for the inserts only).
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{`ALTER TABLE aml_screening_policies DISABLE TRIGGER aml_screening_policies_10_guard`, `ALTER TABLE aml_screening_trigger_policies DISABLE TRIGGER aml_screening_trigger_policies_10_guard`} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds,
				effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES ($1, $2, '{pep,sanctions}', 7200, 3600, now() - interval '1 hour', 'TEST FIXTURE REF', 'fixture', $3, $4, 'platform_acting')`, tenantID, jid, w.ops.Requester, w.ops.Persons[0]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds,
				effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES ($1, $2, '{registration,scheduled}', true, 1800, now() - interval '1 hour', 'TEST FIXTURE REF', 'fixture', $3, $4, 'platform_acting')`, tenantID, jid, w.ops.Requester, w.ops.Persons[0]); err != nil {
			return err
		}
		for _, s := range []string{`ALTER TABLE aml_screening_policies ENABLE TRIGGER aml_screening_policies_10_guard`, `ALTER TABLE aml_screening_trigger_policies ENABLE TRIGGER aml_screening_trigger_policies_10_guard`} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gateOff := map[string]any{"tenant_id": tenantID, "jurisdiction_id": jid, "events": []string{"registration", "scheduled"}, "gate_withdrawal": false,
		"gate_alert_after_seconds": nil, "effective_offset_seconds": 0, "legal_review_reference": "TEST FIXTURE LEGAL REF", "evidence_reference": "EVID-HTTP-TG"}
	file := func(tok, kind string, p map[string]any) cgResp {
		return w.do("POST", "/v1/admin/licensing-changes", tok, map[string]any{"kind": kind, "reason_code": "screening_loosening_decision", "payload": p})
	}
	// INERT: not configured => 409 with the one closed code; nothing is written.
	r := file(w.tokReq(), "aml_screening_trigger_policy_loosen", gateOff)
	codes := r.checkCodes(t)
	if r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_GUARD_REFUSED" || len(codes) != 1 || codes[0] != "loosening_not_configured" {
		t.Fatalf("unconfigured: %d %s", r.status, r.body)
	}
	// Tenant staff: never (403 for the platform-only kind).
	for _, role := range []identity.StaffRole{identity.StaffRoleTenantAdmin, identity.StaffRoleCompliance} {
		if r := file(w.tenantTok(tenantID, w.staff(tenantID, role), auth.Role(role)), "aml_screening_trigger_policy_loosen", gateOff); r.status != http.StatusForbidden {
			t.Fatalf("tenant %s files the kind: %d %s", role, r.status, r.body)
		}
	}
	// Configure the SAME capability as the policy kind (two Persons).
	cfg := file(w.tokReq(), "aml_screening_loosening_configure", map[string]any{"tenant_id": tenantID, "enabled": true, "cooling_seconds": 0, "evidence_reference": "EVID-HTTP-CFG2"})
	var filed licensingChangeResponseIT
	cfg.decode(t, &filed)
	if r := w.lcApprove(w.tokAp1(), filed.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("approval 1: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.tokAp2(), filed.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("approval 2: %d %s", r.status, r.body)
	}
	// A loosening that would ENABLE a gate is refused with its own closed code (after the gate is off, below).
	lo := file(w.tokReq(), "aml_screening_trigger_policy_loosen", gateOff)
	var lf licensingChangeResponseIT
	lo.decode(t, &lf)
	if lo.status != http.StatusCreated || lf.Executed || !lf.ChangeRequest.Widening || lf.ChangeRequest.RequiredApprovals != 2 {
		t.Fatalf("the gate-off loosening is filed as widening with two approvals: %d %s", lo.status, lo.body)
	}
	if r := w.lcApprove(w.tokReq(), lf.ChangeRequest.ID); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_SELF_APPROVAL" {
		t.Fatalf("self approval: %d %s", r.status, r.body)
	}
	tenantTok := w.tenantTok(tenantID, w.staff(tenantID, identity.StaffRoleCompliance), auth.RoleCompliance)
	if r := w.do("GET", "/v1/admin/licensing-changes/"+lf.ChangeRequest.ID, tenantTok, nil); r.status != http.StatusNotFound {
		t.Fatalf("tenant reads the loosening request: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.tokAp1(), lf.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("approval 1: %d %s", r.status, r.body)
	}
	if r := w.lcApprove(w.lcTwinToken(), lf.ChangeRequest.ID); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_ALREADY_DECIDED" {
		t.Fatalf("the same Person as a second approver: %d %s", r.status, r.body)
	}
	done := w.lcApprove(w.tokAp2(), lf.ChangeRequest.ID)
	var ex licensingChangeResponseIT
	done.decode(t, &ex)
	if done.status != http.StatusOK || !ex.Executed || ex.Outcome["policy_id"] == nil || ex.Outcome["previous_gate_withdrawal"] != true || ex.Outcome["gate_withdrawal"] != false {
		t.Fatalf("execution: %d %s", done.status, done.body)
	}
	var n int
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id = $1 AND created_by_scope = 'platform' AND change_request_id = $2 AND NOT gate_withdrawal`,
			tenantID, uuid.MustParse(lf.ChangeRequest.ID)).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("exactly one platform-authored version for the request: %d %v", n, err)
	}
	// From the OFF state a payload that gates is refused with its own closed code; nothing can turn a gate back ON through the loosening.
	enable := map[string]any{}
	for k, v := range gateOff {
		enable[k] = v
	}
	enable["gate_withdrawal"], enable["gate_alert_after_seconds"] = true, 1800
	if r := file(w.tokReq(), "aml_screening_trigger_policy_loosen", enable); r.status != http.StatusConflict || len(r.checkCodes(t)) != 1 || r.checkCodes(t)[0] != "trigger_gate_enable_refused" {
		t.Fatalf("enabling a gate by loosening: %d %s", r.status, r.body)
	}
	if _, ok := aml.RefusalCodes["trigger_gate_enable_refused"]; !ok {
		t.Fatal("the code is in the closed table")
	}
}
