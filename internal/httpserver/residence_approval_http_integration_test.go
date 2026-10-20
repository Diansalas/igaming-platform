//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// lic10 (migration 0181): HTTP-level tests of the residence maker-checker policy routes. NOT RUN by their author (database
// harness refused): the owner runs them. The determination routes are covered at service level
// (internal/kyc/residence_maker_checker_integration_test.go).

func TestResidenceApprovalPolicyAPI_DefaultNone_StricterAtOnce_LooserPending_Authorisation(t *testing.T) {
	ensureLicensingProofs(t)
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tnt := mustCreateTenant(t, pool)
	c1 := mustCreateStaff(t, pool, tnt.ID, identity.StaffRoleCompliance, "res-pw-1")
	c2 := mustCreateStaff(t, pool, tnt.ID, identity.StaffRoleCompliance, "res-pw-2")
	c3 := mustCreateStaff(t, pool, tnt.ID, identity.StaffRoleCompliance, "res-pw-3")
	ta := mustCreateStaff(t, pool, tnt.ID, identity.StaffRoleTenantAdmin, "res-pw-ta")
	t1 := mustLoginStaff(t, srv, tnt.Slug, c1.Email, "res-pw-1").AccessToken
	t2 := mustLoginStaff(t, srv, tnt.Slug, c2.Email, "res-pw-2").AccessToken
	t3 := mustLoginStaff(t, srv, tnt.Slug, c3.Email, "res-pw-3").AccessToken
	tTA := mustLoginStaff(t, srv, tnt.Slug, ta.Email, "res-pw-ta").AccessToken
	base := srv.URL + "/v1/admin/tenants/" + tnt.ID.String() + "/residence-approval-policy"
	do := func(tok, method, url string, body any) *http.Response {
		return sendAssetJSON(t, url, method, tok, body)
	}

	// Default: mode none, no versions (today's behaviour), readable by compliance and tenant_admin.
	for _, tok := range []string{t1, tTA} {
		var p residencePolicyResponse
		decodeBody(t, do(tok, http.MethodGet, base, nil), &p)
		if p.EffectiveMode != "none" || len(p.Versions) != 0 {
			t.Fatalf("default policy = %+v", p)
		}
	}
	// A tenant_admin may read but not file.
	if got := do(tTA, http.MethodPost, base, map[string]any{"mode": "all", "reason_code": "x"}).StatusCode; got != http.StatusForbidden {
		t.Fatalf("tenant_admin filing: %d, want 403", got)
	}
	// Unauthenticated.
	if got := do("", http.MethodGet, base, nil).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", got)
	}
	// Validation: closed modes, strict body.
	for _, body := range []map[string]any{{"mode": "some", "reason_code": "x"}, {"mode": "all", "reason_code": "not a code"}, {"mode": "all", "reason_code": "x", "extra": 1}} {
		if got := do(t1, http.MethodPost, base, body).StatusCode; got != http.StatusBadRequest {
			t.Fatalf("%v: %d, want 400", body, got)
		}
	}
	// Stricter: executed at once (200).
	resp := do(t1, http.MethodPost, base, map[string]any{"mode": "all", "reason_code": "tighten"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stricter mode: %d, want 200", resp.StatusCode)
	}
	var ch residencePolicyChangeResponse
	decodeBody(t, resp, &ch)
	if ch.Status != "active" || ch.RequiredApprovals != 0 || ch.PreviousMode != "none" {
		t.Fatalf("stricter change: %+v", ch)
	}
	// Same mode: 409.
	if got := do(t1, http.MethodPost, base, map[string]any{"mode": "all", "reason_code": "again"}).StatusCode; got != http.StatusConflict {
		t.Fatalf("same mode: %d, want 409", got)
	}
	// Less strict: filed pending (202); the requester cannot approve (403); two others do (200 then active).
	resp = do(t1, http.MethodPost, base, map[string]any{"mode": "none", "reason_code": "relax"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("less strict mode: %d, want 202", resp.StatusCode)
	}
	decodeBody(t, resp, &ch)
	if ch.Status != "pending" || ch.RequiredApprovals != 2 {
		t.Fatalf("less strict change: %+v", ch)
	}
	appr := base + "/" + ch.PolicyID + "/approvals"
	if got := do(t1, http.MethodPost, appr, map[string]any{"reason_code": "self"}).StatusCode; got != http.StatusForbidden {
		t.Fatalf("requester approval: %d, want 403", got)
	}
	var a residencePolicyApprovalResponse
	decodeBody(t, do(t2, http.MethodPost, appr, map[string]any{"reason_code": "one"}), &a)
	if a.Status != "pending" || a.Approvals != 1 {
		t.Fatalf("first approval: %+v", a)
	}
	if got := do(t2, http.MethodPost, appr, map[string]any{"reason_code": "dup"}).StatusCode; got != http.StatusConflict && got != http.StatusForbidden {
		t.Fatalf("duplicate approval: %d, want 409/403", got)
	}
	decodeBody(t, do(t3, http.MethodPost, appr, map[string]any{"reason_code": "two"}), &a)
	if a.Status != "active" {
		t.Fatalf("second approval: %+v", a)
	}
	var p residencePolicyResponse
	decodeBody(t, do(t1, http.MethodGet, base, nil), &p)
	if p.EffectiveMode != "none" || len(p.Versions) != 2 {
		t.Fatalf("policy after loosening: %+v", p)
	}
	// A foreign tenant is 404 for a tenant-scoped caller.
	other := mustCreateTenant(t, pool)
	if got := do(t1, http.MethodGet, srv.URL+"/v1/admin/tenants/"+other.ID.String()+"/residence-approval-policy", nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("foreign tenant: %d, want 404", got)
	}
	// A platform admin with no acting grant for the tenant is refused by the database (never 200).
	pa := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "res-pw-pa")
	tPA := mustLoginStaff(t, srv, "", pa.Email, "res-pw-pa").AccessToken
	if got := do(tPA, http.MethodPost, base, map[string]any{"mode": "all", "reason_code": "x"}).StatusCode; got != http.StatusForbidden {
		t.Fatalf("platform admin without an acting grant: %d, want 403", got)
	}
}
