//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
)

// Migration 0177: the licensing change routes through the real HTTP stack as igaming_runtime (the launch harness: a runtime
// server, owner-provisioned fixtures, three fixture platform operators with three distinct Persons).

func (w *lhWorld) lcJurisdiction() uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lc http')`, id, "LCH-"+strings.ToUpper(id.String()[:10]))
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction: %v", err)
	}
	return id
}

// lcTwinToken is a platform_admin account of the SAME Person as fixture approver 1.
func (w *lhWorld) lcTwinToken() string {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, NULL, $2, '!x', 'platform_admin', $3)`,
			id, "lch-"+id.String()+"@fixture.invalid", w.ops.Persons[1])
		return err
	}); err != nil {
		w.t.Fatalf("seed twin: %v", err)
	}
	return w.api.token(id, uuid.Nil, auth.RolePlatformAdmin)
}

func (w *lhWorld) lcApprove(token, id string) cgResp {
	return w.do("POST", "/v1/admin/licensing-changes/"+id+"/approve", token, map[string]any{"reason_code": "reviewed_ok"})
}

func TestLicensingChangeHTTP_TwoPersonFlow_AuthzAndRedaction(t *testing.T) {
	w := newLHWorld(t)
	jid := w.lcJurisdiction()
	number := "LCH-" + uuid.NewString()[:8]
	create := w.do("POST", "/v1/admin/licensing-changes", w.tokReq(), map[string]any{
		"kind": "licence_create", "reason_code": "licence_granted",
		"payload": map[string]any{"jurisdiction_id": jid, "licensee": "platform", "licence_number": number, "permitted_products": []string{}, "permitted_markets": []string{}},
	})
	if create.status != http.StatusCreated {
		t.Fatalf("file: %d %s", create.status, create.body)
	}
	var filed licensingChangeResponseIT
	create.decode(t, &filed)
	if filed.Executed || !filed.ChangeRequest.Widening || filed.ChangeRequest.RequiredApprovals != 2 || filed.Disclaimer != licensingchange.Disclaimer {
		t.Fatalf("filed: %+v", filed)
	}
	id := filed.ChangeRequest.ID

	// A tenant role never approves (static permission: 403).
	tenantID := w.activeTenant()
	ta := w.staff(tenantID, identity.StaffRoleTenantAdmin)
	if r := w.lcApprove(w.tenantTok(tenantID, ta, auth.RoleTenantAdmin), id); r.status != http.StatusForbidden {
		t.Fatalf("tenant admin approves: %d %s", r.status, r.body)
	}
	// The requester cannot approve its own request.
	if r := w.lcApprove(w.tokReq(), id); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_SELF_APPROVAL" {
		t.Fatalf("self approval: %d %s", r.status, r.body)
	}
	// First approval: recorded, not executed.
	if r := w.lcApprove(w.tokAp1(), id); r.status != http.StatusOK {
		t.Fatalf("approval 1: %d %s", r.status, r.body)
	}
	// The same Person through a second staff account cannot supply the second approval.
	if r := w.lcApprove(w.lcTwinToken(), id); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_ALREADY_DECIDED" {
		t.Fatalf("same Person, second account: %d %s", r.status, r.body)
	}
	done := w.lcApprove(w.tokAp2(), id)
	var executed licensingChangeResponseIT
	done.decode(t, &executed)
	if done.status != http.StatusOK || !executed.Executed || executed.Outcome["licence_id"] == nil {
		t.Fatalf("approval 2 must execute: %d %s", done.status, done.body)
	}
	// Replays are stable 409s.
	if r := w.lcApprove(w.tokAp1(), id); r.status != http.StatusConflict || r.code(t) != "LICENSING_CHANGE_NOT_PENDING" {
		t.Fatalf("replay after execution: %d %s", r.status, r.body)
	}

	// Tenant compliance files a narrowing change of its OWN tenant; another tenant's id is a 404; a platform-only kind 403.
	co := w.staff(tenantID, identity.StaffRoleCompliance)
	coTok := w.tenantTok(tenantID, co, auth.RoleCompliance)
	own := w.do("POST", "/v1/admin/licensing-changes", coTok, map[string]any{"kind": "operating_policy_version", "reason_code": "market_exit",
		"payload": map[string]any{"tenant_id": tenantID, "scope": "tenant", "brand_id": nil, "operation_code": nil, "product_code": nil,
			"country_code": "MT", "state": "disabled", "status": "active", "authorization_reference": nil}})
	if own.status != http.StatusCreated {
		t.Fatalf("tenant files own narrowing change: %d %s", own.status, own.body)
	}
	var ownRes licensingChangeResponseIT
	own.decode(t, &ownRes)
	if ownRes.ChangeRequest.RequiredApprovals != 1 || ownRes.Executed || ownRes.ChangeRequest.RequestedBy == nil || *ownRes.ChangeRequest.RequestedBy != co.String() {
		t.Fatalf("tenant narrowing: %+v", ownRes)
	}
	other := w.activeTenant()
	foreign := w.do("POST", "/v1/admin/licensing-changes", coTok, map[string]any{"kind": "operating_policy_version", "reason_code": "market_exit",
		"payload": map[string]any{"tenant_id": other, "scope": "tenant", "brand_id": nil, "operation_code": nil, "product_code": nil,
			"country_code": "MT", "state": "disabled", "status": "active", "authorization_reference": nil}})
	if foreign.status != http.StatusNotFound {
		t.Fatalf("another tenant's subject: %d %s", foreign.status, foreign.body)
	}
	plat := w.do("POST", "/v1/admin/licensing-changes", coTok, map[string]any{"kind": "licence_create", "reason_code": "x",
		"payload": map[string]any{"jurisdiction_id": jid, "licensee": "platform", "licence_number": "X", "permitted_products": []string{}, "permitted_markets": []string{}}})
	if plat.status != http.StatusForbidden {
		t.Fatalf("tenant files a platform-only kind: %d %s", plat.status, plat.body)
	}

	// The platform approves the tenant's request; the tenant reads it back with the platform executor redacted.
	if r := w.lcApprove(w.tokAp1(), ownRes.ChangeRequest.ID); r.status != http.StatusOK {
		t.Fatalf("platform approves the tenant's request: %d %s", r.status, r.body)
	}
	got := w.do("GET", "/v1/admin/licensing-changes/"+ownRes.ChangeRequest.ID, coTok, nil)
	var view licensingChangeResponseIT
	got.decode(t, &view)
	if got.status != http.StatusOK || view.ChangeRequest.Status != "executed" || view.ChangeRequest.ExecutedBy != nil ||
		len(view.ChangeRequest.Approvals) != 1 || view.ChangeRequest.Approvals[0].ApproverID != nil {
		t.Fatalf("tenant view must show the outcome but never a platform staff id: %d %s", got.status, got.body)
	}
	// The tenant cannot read the platform's licence_create request, nor list it.
	if r := w.do("GET", "/v1/admin/licensing-changes/"+id, coTok, nil); r.status != http.StatusNotFound {
		t.Fatalf("tenant reads a platform request: %d %s", r.status, r.body)
	}
	if r := w.do("GET", "/v1/admin/licensing-changes", coTok, nil); r.status != http.StatusOK || strings.Contains(string(r.body), id) {
		t.Fatalf("tenant list leaked a platform request: %d", r.status)
	}
	// Unauthenticated: 401.
	if r := w.do("GET", "/v1/admin/licensing-changes", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", r.status)
	}
}

func TestLicensingChangeHTTP_NoProofIssuer_FailsClosed500(t *testing.T) {
	w := newLHWorld(t)
	jid := w.lcJurisdiction()
	prev := actorproof.Default()
	actorproof.SetDefault(nil)
	defer actorproof.SetDefault(prev)
	number := "LCH-NP-" + uuid.NewString()[:8]
	r := w.do("POST", "/v1/admin/licensing-changes", w.tokReq(), map[string]any{"kind": "licence_create", "reason_code": "licence_granted",
		"payload": map[string]any{"jurisdiction_id": jid, "licensee": "platform", "licence_number": number, "permitted_products": []string{}, "permitted_markets": []string{}}})
	if r.status != http.StatusInternalServerError || strings.Contains(string(r.body), "proof") {
		t.Fatalf("no issuer: %d %s", r.status, r.body)
	}
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licensing_change_requests WHERE payload->>'licence_number' = $1`, number).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("nothing may be recorded without a proof: %d %v", n, err)
	}
}
