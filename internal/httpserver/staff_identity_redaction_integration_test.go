//go:build integration

// ADR 0114 slice S0 (H1): live HTTP proof, per route family, that a tenant reader never receives
// the staff id of a PLATFORM actor, still receives its own tenant's staff ids, and that a platform
// reader is unchanged. Each test also asserts the platform ids are absent from the whole response
// body (not just the named field), so a second leaking field would fail it.
package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/capability"
)

// sidAbsent fails if any of ids appears anywhere in body.
func sidAbsent(t *testing.T, what string, body []byte, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		if strings.Contains(string(body), id.String()) {
			t.Errorf("%s: a platform staff id %s leaked to a tenant reader: %s", what, id, body)
		}
	}
}

// sidPresent fails unless id appears in body.
func sidPresent(t *testing.T, what string, body []byte, id uuid.UUID) {
	t.Helper()
	if !strings.Contains(string(body), id.String()) {
		t.Errorf("%s: expected id %s to be visible: %s", what, id, body)
	}
}

func asMaps(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		var one map[string]any
		if err2 := json.Unmarshal(body, &one); err2 != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if items, ok := one["items"].([]any); ok {
			var out []map[string]any
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
		return []map[string]any{one}
	}
	return arr
}

// K1: capability grant requests and grants.
func TestStaffIdentityHTTP_K1_CapabilityGrants(t *testing.T) {
	w := newMAWorld(t) // ta requested f1/f2 grants (tenant actors); pa requested `acting` grants (platform actor, platform grantee)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/capability-grants"
	platformIDs := []uuid.UUID{w.pa, w.pb, w.grantApprover, w.acting}

	for _, route := range []string{base + "/requests", base} {
		// tenant reader (tenant_admin and compliance both hold capability_grant:read)
		for name, tok := range map[string]string{"tenant_admin": w.tok(w.ta), "compliance": w.tok(w.comp)} {
			res := w.a.do("GET", route, tok, nil)
			if res.status != http.StatusOK {
				t.Fatalf("%s GET %s: %d %s", name, route, res.status, res.body)
			}
			sidAbsent(t, name+" "+route, res.body, platformIDs...)
			sawPlatform, sawTenant := false, false
			for _, m := range asMaps(t, res.body) {
				if m["grantee_scope"] == "platform" {
					sawPlatform = true
					if v, ok := m["grantee_staff_id"]; !ok || v != nil {
						t.Errorf("%s: platform grantee must be null, got %v", name, v)
					}
				}
				if m["requested_by_scope"] == "platform" {
					if v, ok := m["requested_by"]; !ok || v != nil {
						t.Errorf("%s: platform requester must be null, got %v", name, v)
					}
				}
				if m["grantee_scope"] == "tenant" {
					sawTenant = true
					if m["grantee_staff_id"] != w.f1.String() && m["grantee_staff_id"] != w.f2.String() {
						t.Errorf("%s: own-tenant grantee must be visible, got %v", name, m["grantee_staff_id"])
					}
				}
				if m["requested_by_scope"] == "tenant" && m["requested_by"] != w.ta.String() {
					t.Errorf("%s: own-tenant requester must be visible, got %v", name, m["requested_by"])
				}
			}
			if !sawPlatform || !sawTenant {
				t.Errorf("%s %s: fixture must contain both platform and tenant rows (platform=%v tenant=%v)", name, route, sawPlatform, sawTenant)
			}
		}
		// platform reader: unchanged
		res := w.a.do("GET", route, w.tok(w.pa), nil)
		if res.status != http.StatusOK {
			t.Fatalf("platform GET %s: %d %s", route, res.status, res.body)
		}
		sidPresent(t, "platform reader "+route, res.body, w.acting)
		if route == base+"/requests" {
			sidPresent(t, "platform reader "+route, res.body, w.pa)
		}
		sidPresent(t, "platform reader "+route, res.body, w.f1)
	}
	_ = capability.CapabilityLedgerAdjustmentInitiate
}

// K2: manual adjustments (initiated_by).
func TestStaffIdentityHTTP_K2_ManualAdjustments(t *testing.T) {
	w := newMAWorld(t)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/manual-adjustments"
	stP, platformReq := w.submit(w.acting)
	stT, tenantReq := w.submit(w.f1)
	if stP != http.StatusCreated || stT != http.StatusCreated {
		t.Fatalf("seed: platform=%d tenant=%d", stP, stT)
	}
	// the platform caller's own create response is unchanged (it is the platform)
	// tenant reader: compliance (list + get)
	list := w.a.do("GET", base, w.tok(w.comp), nil)
	if list.status != http.StatusOK {
		t.Fatalf("list: %d %s", list.status, list.body)
	}
	sidAbsent(t, "K2 list", list.body, w.acting, w.pa)
	sawP, sawT := false, false
	var wrapper struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(list.body, &wrapper); err != nil || wrapper.Items == nil {
		wrapper.Items = asMaps(t, list.body)
	}
	for _, m := range wrapper.Items {
		switch m["initiated_by_scope"] {
		case "platform", "platform_acting":
			sawP = true
			if v, ok := m["initiated_by"]; !ok || v != nil {
				t.Errorf("platform initiator must be null, got %v", v)
			}
		case "tenant":
			sawT = true
			if m["initiated_by"] != w.f1.String() {
				t.Errorf("own-tenant initiator must be visible, got %v", m["initiated_by"])
			}
		}
	}
	if !sawP || !sawT {
		t.Fatalf("fixture must contain a platform and a tenant row (p=%v t=%v): %s", sawP, sawT, list.body)
	}
	get := w.a.do("GET", base+"/"+platformReq.ID, w.tok(w.comp), nil)
	sidAbsent(t, "K2 get (platform initiated)", get.body, w.acting, w.pa)
	get = w.a.do("GET", base+"/"+tenantReq.ID, w.tok(w.comp), nil)
	sidPresent(t, "K2 get (tenant initiated)", get.body, w.f1)
	// tenant approver (f2) approving a platform-initiated request: the decision response is a tenant read too.
	dec := w.a.do("POST", base+"/"+platformReq.ID+"/approve", w.tok(w.f2), map[string]any{"payload_hash": platformReq.PayloadHash, "reason_code": "x"})
	if dec.status == http.StatusOK {
		sidAbsent(t, "K2 approve response", dec.body, w.acting, w.pa)
	}
	// platform reader: unchanged
	pg := w.a.do("GET", base+"/"+platformReq.ID, w.tok(w.acting), nil)
	if pg.status != http.StatusOK {
		t.Fatalf("platform get: %d %s", pg.status, pg.body)
	}
	sidPresent(t, "K2 platform reader", pg.body, w.acting)
}

// K3: payment force-resolutions (requested_by) - and BLOCKER 2: no evidence_reference text for any reader.
func TestStaffIdentityHTTP_K3_ForceResolutions(t *testing.T) {
	w := newFRWorld(t, true)
	stP, bodyP, dtoP := w.request(w.acting, w.disputedDeposit(), nil)
	stT, bodyT, dtoT := w.request(w.f1, w.disputedDeposit(), nil)
	if stP != http.StatusCreated || stT != http.StatusCreated {
		t.Fatalf("seed: platform=%d %s tenant=%d %s", stP, bodyP, stT, bodyT)
	}
	list := w.a.do("GET", w.base(), w.tok(w.comp), nil)
	if list.status != http.StatusOK {
		t.Fatalf("list: %d %s", list.status, list.body)
	}
	sidAbsent(t, "K3 list", list.body, w.acting, w.pa)
	sawP, sawT := false, false
	for _, m := range asMaps(t, list.body) {
		switch m["requested_by_scope"] {
		case "platform", "platform_acting":
			sawP = true
			if v, ok := m["requested_by"]; !ok || v != nil {
				t.Errorf("platform requester must be null, got %v", v)
			}
		case "tenant":
			sawT = true
			if m["requested_by"] != w.f1.String() {
				t.Errorf("own-tenant requester must be visible, got %v", m["requested_by"])
			}
		}
		if _, leaked := m["evidence_reference"]; leaked {
			t.Errorf("evidence_reference text must never be returned: %v", m)
		}
	}
	if !sawP || !sawT {
		t.Fatalf("fixture must contain platform and tenant rows: %s", list.body)
	}
	get := w.a.do("GET", w.base()+"/"+dtoP.ID, w.tok(w.f2), nil) // a tenant finance APPROVER reads a platform-requested row
	sidAbsent(t, "K3 get as tenant approver", get.body, w.acting, w.pa)
	get = w.a.do("GET", w.base()+"/"+dtoT.ID, w.tok(w.comp), nil)
	sidPresent(t, "K3 get tenant-requested", get.body, w.f1)
	// platform reader unchanged
	pg := w.a.do("GET", w.base()+"/"+dtoP.ID, w.tok(w.acting), nil)
	if pg.status != http.StatusOK {
		t.Fatalf("platform get: %d %s", pg.status, pg.body)
	}
	sidPresent(t, "K3 platform reader", pg.body, w.acting)
	// the create response for a platform actor is rendered for the platform caller itself: unchanged.
	sidPresent(t, "K3 platform create response", bodyP, w.acting)
}

// Kill switch: the confirmed live defect. Platform engages; tenant_admin GETs.
func TestStaffIdentityHTTP_KillSwitch(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	pa1, pa2 := a.platformAdmin(), a.platformAdmin()
	ta1, ta2 := a.tenantStaff(tenant, "tenant_admin"), a.tenantStaff(tenant, "tenant_admin")
	pTok1, pTok2 := a.token(pa1, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff), a.token(pa2, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tTok1, tTok2 := a.token(ta1, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff), a.token(ta2, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	// platform engages one switch, the tenant another
	var ksP, ksT killSwitchDTO
	r := a.do("POST", base+"/kill-switches", pTok1, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "platform_incident"})
	if r.status != http.StatusOK {
		t.Fatalf("platform engage: %d %s", r.status, r.body)
	}
	r.decode(t, &ksP)
	sidPresent(t, "platform engage response (platform caller)", r.body, pa1)
	r = a.do("POST", base+"/kill-switches", tTok1, map[string]any{"provider_scope": "*", "operation_scope": "payout", "reason_code": "tenant_incident"})
	if r.status != http.StatusOK {
		t.Fatalf("tenant engage: %d %s", r.status, r.body)
	}
	r.decode(t, &ksT)

	// tenant reader: GET of the platform-engaged switch (the confirmed defect)
	get := a.do("GET", base+"/kill-switches/"+ksP.ID, tTok2, nil)
	if get.status != http.StatusOK {
		t.Fatalf("tenant get: %d %s", get.status, get.body)
	}
	sidAbsent(t, "kill switch GET", get.body, pa1, pa2)
	var raw map[string]any
	get.decode(t, &raw)
	if v, ok := raw["changed_by"]; !ok || v != nil || raw["changed_by_scope"] != "platform" {
		t.Errorf("platform changed_by must be null with scope platform, got %v / %v", raw["changed_by"], raw["changed_by_scope"])
	}
	// list: platform row hidden, tenant row visible
	list := a.do("GET", base+"/kill-switches", tTok2, nil)
	sidAbsent(t, "kill switch list", list.body, pa1, pa2)
	sidPresent(t, "kill switch list (tenant actor)", list.body, ta1)
	// platform reader unchanged
	pl := a.do("GET", base+"/kill-switches/"+ksP.ID, pTok2, nil)
	sidPresent(t, "kill switch GET by platform", pl.body, pa1)
	pl = a.do("GET", base+"/kill-switches/"+ksT.ID, pTok2, nil)
	sidPresent(t, "kill switch GET tenant-engaged by platform", pl.body, ta1)

	// release request filed and approved by PLATFORM on the platform-engaged switch; tenant reads it.
	rr := a.do("POST", base+"/kill-switches/"+ksP.ID+"/release-requests", pTok1, map[string]any{"reason_code": "resolved"})
	if rr.status != http.StatusCreated {
		t.Fatalf("platform release request: %d %s", rr.status, rr.body)
	}
	var req killSwitchReleaseRequestDTO
	rr.decode(t, &req)
	if ap := a.do("POST", base+"/kill-switch-release-requests/"+req.ID+"/approve", pTok2, nil); ap.status != http.StatusOK {
		t.Fatalf("platform approve: %d %s", ap.status, ap.body)
	}
	rg := a.do("GET", base+"/kill-switch-release-requests/"+req.ID, tTok1, nil)
	if rg.status != http.StatusOK {
		t.Fatalf("tenant get release request: %d %s", rg.status, rg.body)
	}
	sidAbsent(t, "release request GET", rg.body, pa1, pa2)
	var rraw map[string]any
	rg.decode(t, &rraw)
	if v, ok := rraw["requested_by"]; !ok || v != nil {
		t.Errorf("platform requested_by must be null, got %v", v)
	}
	if v, ok := rraw["approved_by"]; !ok || v != nil || rraw["approved_by_scope"] != "platform" {
		t.Errorf("platform approved_by must be null (present, redacted) with scope platform, got %v / %v", v, rraw["approved_by_scope"])
	}
	prg := a.do("GET", base+"/kill-switch-release-requests/"+req.ID, pTok1, nil)
	sidPresent(t, "release request by platform", prg.body, pa1)
	sidPresent(t, "release request by platform", prg.body, pa2)

	// tenant-only flow: the tenant reader sees both tenant ids
	var rt killSwitchReleaseRequestDTO
	a.do("POST", base+"/kill-switches/"+ksT.ID+"/release-requests", tTok1, map[string]any{"reason_code": "resolved"}).decode(t, &rt)
	if ap := a.do("POST", base+"/kill-switch-release-requests/"+rt.ID+"/approve", tTok2, nil); ap.status != http.StatusOK {
		t.Fatalf("tenant approve: %d %s", ap.status, ap.body)
	}
	tg := a.do("GET", base+"/kill-switch-release-requests/"+rt.ID, tTok1, nil)
	sidPresent(t, "tenant release request requested_by", tg.body, ta1)
	sidPresent(t, "tenant release request approved_by", tg.body, ta2)
}

// Provider credential handles: revoked_by has no scope column; a tenant reader sees the revoker only
// if it is staff of its own tenant.
func TestStaffIdentityHTTP_ProviderCredentialRevokedBy(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	b1, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	h1, _ := a.activate(tenant, b1)
	b2, _, _ := a.regBody(tenant, "acme2", "webhook_verify", "k1")
	h2, _ := a.activate(tenant, b2)
	ta := a.tenantStaff(tenant, "tenant_admin")
	adminTok := a.token(ta, tenant, auth.RoleTenantAdmin)
	if r := a.do("POST", base(tenant)+"/provider-credentials/"+h1+"/transitions", adminTok,
		map[string]any{"action": "revoke", "reason_code": "tenant_request"}); r.status != http.StatusOK {
		t.Fatalf("tenant revoke: %d %s", r.status, r.body)
	} else {
		sidPresent(t, "tenant revoke response (own id)", r.body, ta)
	}
	r := a.do("POST", base(tenant)+"/provider-credentials/"+h2+"/transitions", a.platformToken(a.approver),
		map[string]any{"action": "revoke", "reason_code": "suspected_compromise"})
	if r.status != http.StatusOK {
		t.Fatalf("platform revoke: %d %s", r.status, r.body)
	}
	sidPresent(t, "platform revoke response (platform caller)", r.body, a.approver)

	list := a.do("GET", base(tenant)+"/provider-credentials", adminTok, nil)
	if list.status != http.StatusOK {
		t.Fatalf("tenant list: %d %s", list.status, list.body)
	}
	sidAbsent(t, "handles list (tenant reader)", list.body, a.approver, a.requester)
	sidPresent(t, "handles list (own tenant revoker)", list.body, ta)
	var out struct {
		Handles []map[string]any `json:"handles"`
	}
	if err := json.Unmarshal(list.body, &out); err != nil {
		t.Fatal(err)
	}
	nullRevoker := 0
	for _, h := range out.Handles {
		if v, ok := h["revoked_by"]; ok && v == nil {
			nullRevoker++
		}
	}
	if nullRevoker != 1 {
		t.Errorf("exactly the platform-revoked handle must show revoked_by null, got %d: %s", nullRevoker, list.body)
	}
	pl := a.do("GET", base(tenant)+"/provider-credentials", a.platformToken(a.approver), nil)
	sidPresent(t, "handles list (platform reader)", pl.body, a.approver)
	sidPresent(t, "handles list (platform reader)", pl.body, ta)
}

// Generic tenant audit log: platform staff who acted inside the tenant are scope-only.
func TestStaffIdentityHTTP_TenantAuditLog_ActorID(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	pa := a.platformAdmin()
	ta := a.tenantStaff(tenant, "tenant_admin")
	pTok := a.token(pa, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tTok := a.token(ta, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	brandPath := "/v1/admin/tenants/" + tenant.String() + "/brands"
	if r := a.do("POST", brandPath, pTok, map[string]any{"name": "Platform Made", "slug": "plat-made"}); r.status != http.StatusCreated {
		t.Fatalf("platform create brand: %d %s", r.status, r.body)
	}
	if r := a.do("POST", brandPath, tTok, map[string]any{"name": "Tenant Made", "slug": "ten-made"}); r.status != http.StatusCreated {
		t.Fatalf("tenant create brand: %d %s", r.status, r.body)
	}
	res := a.do("GET", "/v1/admin/audit-log?action=brand.created", tTok, nil)
	if res.status != http.StatusOK {
		t.Fatalf("audit log: %d %s", res.status, res.body)
	}
	sidAbsent(t, "tenant audit-log", res.body, pa)
	sidPresent(t, "tenant audit-log (own staff)", res.body, ta)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	res.decode(t, &page)
	nulls := 0
	for _, it := range page.Items {
		if v, ok := it["actor_id"]; ok && v == nil {
			nulls++
		}
	}
	if len(page.Items) != 2 || nulls != 1 {
		t.Errorf("want 2 rows with exactly one null actor_id (the platform's), got %d rows / %d null: %s", len(page.Items), nulls, res.body)
	}
}
