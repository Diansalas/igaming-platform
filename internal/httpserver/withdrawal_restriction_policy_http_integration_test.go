//go:build integration

package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/withdrawalpolicy"
)

// lic17 R-8 (security review of lic11): the eight withdrawal restriction policy routes through the real HTTP stack as
// igaming_runtime (the launch harness: owner-provisioned fixtures, three fixture platform operators with distinct Persons):
// authorization (permission and scope), the step-up class of each route, tenant isolation and request validation.

type wrpEffectiveIT struct {
	TenantID        string `json:"tenant_id"`
	SetVersion      int64  `json:"set_version"`
	Configured      bool   `json:"configured"`
	ActionSemantics []struct {
		Action  string `json:"action"`
		Meaning string `json:"meaning"`
	} `json:"action_semantics"`
	Parked struct {
		Visible bool   `json:"visible"`
		Count   int64  `json:"count"`
		Note    string `json:"note"`
	} `json:"parked"`
	Causes []struct {
		Cause        string `json:"cause"`
		Configurable bool   `json:"configurable"`
		Unconfigured bool   `json:"unconfigured"`
	} `json:"causes"`
	Disclaimer string `json:"disclaimer"`
}

type wrpChangeIT struct {
	Change struct {
		ID                string `json:"id"`
		TenantID          string `json:"tenant_id"`
		Status            string `json:"status"`
		Widening          bool   `json:"widening"`
		RequiredApprovals int    `json:"required_approvals"`
	} `json:"change"`
	Executed bool `json:"executed"`
	Policy   *struct {
		Action     string `json:"action"`
		SetVersion int64  `json:"set_version"`
	} `json:"policy"`
}

// wrpServe mounts a server whose Deps carry the withdrawal restriction policy service over the runtime pool.
func (w *lhWorld) wrpServe() *cgAPI {
	w.t.Helper()
	svc, err := withdrawalpolicy.New(withdrawalpolicy.Config{Pool: w.rt, LockTimeout: "2s"})
	if err != nil {
		w.t.Fatal(err)
	}
	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              w.rt,
		AuthIssuer:      w.issuer,
		StaffMFA:        testStaffMFA(),
		ServiceName:     "lh-test",
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(), MarketGate: allowAllMarketGate{},
		LaunchAuthorisation:         w.svc,
		WithdrawalRestrictionPolicy: svc,
	}))
	w.t.Cleanup(srv.Close)
	return &cgAPI{t: w.t, pool: w.rt, issuer: w.issuer, srv: srv}
}

func wantStatus(t *testing.T, what string, r cgResp, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status %d, want %d: %s", what, r.status, status, r.body)
	}
	if code != "" && r.code(t) != code {
		t.Fatalf("%s: code %q, want %q: %s", what, r.code(t), code, r.body)
	}
}

func TestLic17_R8_WithdrawalRestrictionPolicyHTTP_AuthzScopeValidation(t *testing.T) {
	w := newLHWorld(t)
	api := w.wrpServe()
	tA, tB := w.activeTenant(), w.activeTenant()
	taA := w.staff(tA, identity.StaffRoleTenantAdmin)
	coA := w.staff(tA, identity.StaffRoleCompliance)
	suA := w.staff(tA, identity.StaffRoleSupport)
	tokTA := api.token(taA, tA, auth.RoleTenantAdmin)
	tokCO := api.token(coA, tA, auth.RoleCompliance)
	tokSU := api.token(suA, tA, auth.RoleSupport)
	req, ap1, ap2 := api.token(w.ops.Requester, uuid.Nil, auth.RolePlatformAdmin), api.token(w.ops.Approver1, uuid.Nil, auth.RolePlatformAdmin),
		api.token(w.ops.Approver2, uuid.Nil, auth.RolePlatformAdmin)
	effective := func(tenant uuid.UUID) string {
		return "/v1/admin/tenants/" + tenant.String() + "/withdrawal-restriction-policy"
	}
	const changes = "/v1/admin/withdrawal-restriction-policy-changes"

	// ---- 1. GET effective --------------------------------------------------------------------------------------------
	wantStatus(t, "effective, no token", api.do("GET", effective(tA), "", nil), http.StatusUnauthorized, "")
	wantStatus(t, "effective, role without the read permission", api.do("GET", effective(tA), tokSU, nil), http.StatusForbidden, "")
	wantStatus(t, "effective, malformed tenant id", api.do("GET", "/v1/admin/tenants/not-a-uuid/withdrawal-restriction-policy", req, nil), http.StatusBadRequest, "")
	wantStatus(t, "effective, unknown tenant", api.do("GET", effective(uuid.New()), req, nil), http.StatusNotFound, "")
	wantStatus(t, "effective, another tenant's (tenant admin)", api.do("GET", effective(tB), tokTA, nil), http.StatusNotFound, "")
	var ev wrpEffectiveIT
	r := api.do("GET", effective(tA), tokCO, nil)
	wantStatus(t, "effective, own tenant (compliance)", r, http.StatusOK, "")
	r.decode(t, &ev)
	if ev.TenantID != tA.String() || ev.Configured || !ev.Parked.Visible || len(ev.ActionSemantics) != 3 || ev.Disclaimer != withdrawalpolicy.Disclaimer {
		t.Fatalf("tenant view: %s", r.body)
	}
	r = api.do("GET", effective(tA), req, nil)
	wantStatus(t, "effective, platform", r, http.StatusOK, "")
	r.decode(t, &ev)
	if ev.Parked.Visible || ev.Parked.Note != withdrawalpolicy.ParkedPlatformNote {
		t.Fatalf("platform view of parked: %s", r.body)
	}

	// ---- 5. POST create: permission, step-up, validation ------------------------------------------------------------
	body := func(tenant uuid.UUID, cause, action string) map[string]any {
		return map[string]any{"tenant_id": tenant, "cause": cause, "action": action, "reason_code": "lic17_r8"}
	}
	wantStatus(t, "create, tenant admin", api.do("POST", changes, tokTA, body(tA, "licence_suspended", "block")), http.StatusForbidden, "")
	stale := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, w.ops.Requester, uuid.Nil, auth.RolePlatformAdmin, 11*time.Minute)
	wantStatus(t, "create, stale second factor (critical)", api.do("POST", changes, stale, body(tA, "licence_suspended", "block")), http.StatusForbidden, "step_up_required")
	wantStatus(t, "create, unknown field", api.do("POST", changes, req, map[string]any{"tenant_id": tA, "cause": "licence_suspended", "action": "block", "reason_code": "x", "extra": 1}), http.StatusBadRequest, "")
	wantStatus(t, "create, malformed tenant", api.do("POST", changes, req, map[string]any{"tenant_id": "nope", "cause": "licence_suspended", "action": "block", "reason_code": "x"}), http.StatusBadRequest, "")
	wantStatus(t, "create, unknown action", api.do("POST", changes, req, body(tA, "licence_suspended", "deny")), http.StatusBadRequest, "")
	wantStatus(t, "create, free-text reason", api.do("POST", changes, req, map[string]any{"tenant_id": tA, "cause": "licence_suspended", "action": "block", "reason_code": "because I said so"}), http.StatusBadRequest, "")
	wantStatus(t, "create, owner-decided cause", api.do("POST", changes, req, body(tA, "tenant_suspended", "allow")), http.StatusConflict, "WITHDRAWAL_RESTRICTION_POLICY_NOT_CONFIGURABLE")
	wantStatus(t, "create, unknown tenant", api.do("POST", changes, req, body(uuid.New(), "licence_suspended", "block")), http.StatusNotFound, "")
	var tight wrpChangeIT
	r = api.do("POST", changes, req, body(tA, "licence_suspended", "block"))
	wantStatus(t, "create, tightening", r, http.StatusCreated, "")
	r.decode(t, &tight)
	if !tight.Executed || tight.Change.Widening || tight.Policy == nil || tight.Policy.SetVersion != 1 {
		t.Fatalf("a tightening executes at once: %s", r.body)
	}

	// ---- 6. POST approve: four-eyes, step-up ------------------------------------------------------------------------
	var loosen wrpChangeIT
	r = api.do("POST", changes, req, body(tA, "licence_suspended", "allow"))
	wantStatus(t, "create, loosening", r, http.StatusCreated, "")
	r.decode(t, &loosen)
	if loosen.Executed || !loosen.Change.Widening || loosen.Change.RequiredApprovals != 2 {
		t.Fatalf("a loosening waits: %s", r.body)
	}
	approve := func(tok, id string) cgResp {
		return api.do("POST", changes+"/"+id+"/approve", tok, map[string]any{"reason_code": "reviewed_ok"})
	}
	wantStatus(t, "approve, tenant role", approve(tokTA, loosen.Change.ID), http.StatusForbidden, "")
	staleAp1 := authtest.LiveStaffTokenAged(t, w.rt, testStaffMFAKeys(), w.issuer, w.ops.Approver1, uuid.Nil, auth.RolePlatformAdmin, 11*time.Minute)
	wantStatus(t, "approve, stale second factor (critical)", approve(staleAp1, loosen.Change.ID), http.StatusForbidden, "step_up_required")
	wantStatus(t, "approve, malformed id", approve(ap1, "nope"), http.StatusBadRequest, "")
	wantStatus(t, "approve, unknown change", approve(ap1, uuid.NewString()), http.StatusNotFound, "")
	wantStatus(t, "approve, self", approve(req, loosen.Change.ID), http.StatusConflict, "WITHDRAWAL_RESTRICTION_POLICY_SELF_APPROVAL")
	var a1 wrpChangeIT
	r = approve(ap1, loosen.Change.ID)
	wantStatus(t, "approve 1", r, http.StatusOK, "")
	r.decode(t, &a1)
	if a1.Executed {
		t.Fatalf("one approval does not execute: %s", r.body)
	}
	wantStatus(t, "approve, same Person second account", approve(w.lcTwinToken(), loosen.Change.ID), http.StatusConflict, "")
	var a2 wrpChangeIT
	r = approve(ap2, loosen.Change.ID)
	wantStatus(t, "approve 2", r, http.StatusOK, "")
	r.decode(t, &a2)
	if !a2.Executed || a2.Policy == nil || a2.Policy.Action != "allow" {
		t.Fatalf("the second distinct approver executes: %s", r.body)
	}
	wantStatus(t, "approve, replay", approve(ap1, loosen.Change.ID), http.StatusConflict, "WITHDRAWAL_RESTRICTION_POLICY_NOT_PENDING")

	// ---- 7. POST reject / 8. POST cancel: class none (a stale factor is not refused for step-up) --------------------------
	var open1, open2 wrpChangeIT
	r = api.do("POST", changes, req, body(tA, "licence_expired", "allow"))
	wantStatus(t, "create, loosening 2", r, http.StatusCreated, "")
	r.decode(t, &open1)
	r = api.do("POST", changes, req, body(tA, "licence_missing", "allow"))
	wantStatus(t, "create, loosening 3", r, http.StatusCreated, "")
	r.decode(t, &open2)
	wantStatus(t, "reject, tenant role", api.do("POST", changes+"/"+open1.Change.ID+"/reject", tokTA, map[string]any{"reason_code": "no"}), http.StatusForbidden, "")
	wantStatus(t, "reject, unknown field", api.do("POST", changes+"/"+open1.Change.ID+"/reject", ap1, map[string]any{"reason_code": "no", "x": 1}), http.StatusBadRequest, "")
	r = api.do("POST", changes+"/"+open1.Change.ID+"/reject", staleAp1, map[string]any{"reason_code": "not_justified"})
	wantStatus(t, "reject (stale factor: class none)", r, http.StatusOK, "")
	wantStatus(t, "reject, replay", api.do("POST", changes+"/"+open1.Change.ID+"/reject", ap1, map[string]any{"reason_code": "no"}), http.StatusConflict, "WITHDRAWAL_RESTRICTION_POLICY_NOT_PENDING")
	wantStatus(t, "cancel, tenant role", api.do("POST", changes+"/"+open2.Change.ID+"/cancel", tokTA, nil), http.StatusForbidden, "")
	wantStatus(t, "cancel, malformed id", api.do("POST", changes+"/nope/cancel", req, nil), http.StatusBadRequest, "")
	r = api.do("POST", changes+"/"+open2.Change.ID+"/cancel", stale, nil)
	wantStatus(t, "cancel (stale factor: class none)", r, http.StatusOK, "")
	var cancelled wrpChangeIT
	r.decode(t, &cancelled)
	if cancelled.Change.Status != "cancelled" {
		t.Fatalf("cancelled: %s", r.body)
	}

	// ---- 3. GET list / 4. GET one: platform records ----------------------------------------------------------------
	wantStatus(t, "list, tenant role", api.do("GET", changes, tokCO, nil), http.StatusForbidden, "")
	wantStatus(t, "list, bad limit", api.do("GET", changes+"?limit=0", req, nil), http.StatusBadRequest, "")
	wantStatus(t, "list, bad tenant filter", api.do("GET", changes+"?tenant_id=nope", req, nil), http.StatusBadRequest, "")
	wantStatus(t, "list, bad status filter", api.do("GET", changes+"?status=bogus", req, nil), http.StatusBadRequest, "")
	var list struct {
		Items []struct {
			TenantID string `json:"tenant_id"`
		} `json:"items"`
	}
	r = api.do("GET", changes+"?tenant_id="+tA.String(), req, nil)
	wantStatus(t, "list, platform", r, http.StatusOK, "")
	r.decode(t, &list)
	if len(list.Items) != 4 {
		t.Fatalf("four changes for tenant A, got %d: %s", len(list.Items), r.body)
	}
	r = api.do("GET", changes+"?tenant_id="+tB.String(), req, nil)
	r.decode(t, &list)
	if len(list.Items) != 0 {
		t.Fatalf("tenant B has no change, got %s", r.body)
	}
	wantStatus(t, "get, tenant role", api.do("GET", changes+"/"+loosen.Change.ID, tokCO, nil), http.StatusNotFound, "")
	wantStatus(t, "get, malformed id", api.do("GET", changes+"/nope", req, nil), http.StatusBadRequest, "")
	var got wrpChangeIT
	r = api.do("GET", changes+"/"+loosen.Change.ID, req, nil)
	wantStatus(t, "get, platform", r, http.StatusOK, "")
	r.decode(t, &got)
	if got.Change.Status != "executed" || !got.Executed {
		t.Fatalf("get: %s", r.body)
	}

	// ---- 2. GET versions: tenant isolation ---------------------------------------------------------------------------
	versions := func(tenant uuid.UUID) string { return effective(tenant) + "/versions" }
	var vs struct {
		Items []struct {
			SetVersion int64   `json:"set_version"`
			CreatedBy  *string `json:"created_by"`
		} `json:"items"`
	}
	r = api.do("GET", versions(tA), tokCO, nil)
	wantStatus(t, "versions, own tenant", r, http.StatusOK, "")
	r.decode(t, &vs)
	if len(vs.Items) != 2 || vs.Items[0].SetVersion != 2 || vs.Items[0].CreatedBy != nil {
		t.Fatalf("two versions, newest first, the platform author redacted for a tenant caller: %s", r.body)
	}
	r = api.do("GET", versions(tA), req, nil)
	r.decode(t, &vs)
	if len(vs.Items) != 2 || vs.Items[0].CreatedBy == nil {
		t.Fatalf("platform sees the author: %s", r.body)
	}
	wantStatus(t, "versions, another tenant's", api.do("GET", versions(tA), api.token(w.staff(tB, identity.StaffRoleTenantAdmin), tB, auth.RoleTenantAdmin), nil), http.StatusNotFound, "")
	wantStatus(t, "versions, role without read", api.do("GET", versions(tA), tokSU, nil), http.StatusForbidden, "")
	wantStatus(t, "versions, malformed tenant", api.do("GET", "/v1/admin/tenants/x/withdrawal-restriction-policy/versions", req, nil), http.StatusBadRequest, "")
	r = api.do("GET", versions(tB), req, nil)
	r.decode(t, &vs)
	if r.status != http.StatusOK || len(vs.Items) != 0 {
		t.Fatalf("tenant B has no version: %s", r.body)
	}

	// The effective view after the changes: configured, set version 2, tenant B untouched.
	r = api.do("GET", effective(tA), tokTA, nil)
	r.decode(t, &ev)
	if !ev.Configured || ev.SetVersion != 2 {
		t.Fatalf("tenant A configured at v2: %s", r.body)
	}
	r = api.do("GET", effective(tB), req, nil)
	r.decode(t, &ev)
	if ev.Configured || ev.SetVersion != 0 {
		t.Fatalf("tenant B untouched: %s", r.body)
	}
}
