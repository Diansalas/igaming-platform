//go:build integration

// ADR 0114 S6b: the REAL console release sequence over live HTTP. The defect this pins: the console located
// an open release request through the switch's release_request_id, but the database fills that column only
// at RELEASE time (migration 0106 forces NULL on insert and engage) and filing a request never touches the
// switch row. open_release_request_id is computed from the request table; these tests drive engage ->
// request (second person) -> GET shows the open request -> approve by a third principal -> released.
package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

type ksActors struct {
	a                *ksAPI
	tenant           uuid.UUID
	base             string
	t1, t2, t3       uuid.UUID
	tok1, tok2, tok3 string
	p1, p2           uuid.UUID
	ptok1, ptok2     string
}

func newKSActors(t *testing.T) *ksActors {
	a := newKSAPI(t)
	k := &ksActors{a: a, tenant: a.tenant()}
	k.base = "/v1/admin/tenants/" + k.tenant.String() + "/payments"
	k.t1, k.t2, k.t3 = a.tenantStaff(k.tenant, "tenant_admin"), a.tenantStaff(k.tenant, "tenant_admin"), a.tenantStaff(k.tenant, "tenant_admin")
	k.tok1 = a.token(k.t1, k.tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	k.tok2 = a.token(k.t2, k.tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	k.tok3 = a.token(k.t3, k.tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	k.p1, k.p2 = a.platformAdmin(), a.platformAdmin()
	k.ptok1 = a.token(k.p1, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	k.ptok2 = a.token(k.p2, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	return k
}

func (k *ksActors) engage(tok, op, reason string) killSwitchDTO {
	k.a.t.Helper()
	r := k.a.do("POST", k.base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": op, "reason_code": reason})
	if r.status != http.StatusOK {
		k.a.t.Fatalf("engage: %d %s", r.status, r.body)
	}
	var ks killSwitchDTO
	r.decode(k.a.t, &ks)
	return ks
}

func (k *ksActors) file(tok, ksID, reason string) ksResp {
	return k.a.do("POST", k.base+"/kill-switches/"+ksID+"/release-requests", tok, map[string]any{"reason_code": reason})
}

func (k *ksActors) get(tok, ksID string) killSwitchDTO {
	k.a.t.Helper()
	r := k.a.do("GET", k.base+"/kill-switches/"+ksID, tok, nil)
	if r.status != http.StatusOK {
		k.a.t.Fatalf("get switch: %d %s", r.status, r.body)
	}
	var ks killSwitchDTO
	r.decode(k.a.t, &ks)
	return ks
}

func (k *ksActors) listed(tok, ksID string) killSwitchDTO {
	k.a.t.Helper()
	r := k.a.do("GET", k.base+"/kill-switches", tok, nil)
	if r.status != http.StatusOK {
		k.a.t.Fatalf("list: %d %s", r.status, r.body)
	}
	var out struct {
		KillSwitches []killSwitchDTO `json:"kill_switches"`
	}
	r.decode(k.a.t, &out)
	for _, ks := range out.KillSwitches {
		if ks.ID == ksID {
			return ks
		}
	}
	k.a.t.Fatalf("switch %s not in list: %s", ksID, r.body)
	return killSwitchDTO{}
}

func openID(ks killSwitchDTO) string {
	if ks.OpenReleaseRequestID == nil {
		return ""
	}
	return *ks.OpenReleaseRequestID
}

func (k *ksActors) getReq(tok, id string) killSwitchReleaseRequestDTO {
	k.a.t.Helper()
	r := k.a.do("GET", k.base+"/kill-switch-release-requests/"+id, tok, nil)
	if r.status != http.StatusOK {
		k.a.t.Fatalf("get request: %d %s", r.status, r.body)
	}
	var q killSwitchReleaseRequestDTO
	r.decode(k.a.t, &q)
	return q
}

// The confirmed defect, reproduced as the console does it: file, then look the request up through the switch.
func TestKillSwitchOpenRequest_RealSequence_EngageFileGetApprove(t *testing.T) {
	k := newKSActors(t)
	ks := k.engage(k.tok1, "deposit", "incident_1")
	if ks.OpenReleaseRequestID != nil {
		t.Fatalf("a fresh engage has no open request, got %v", *ks.OpenReleaseRequestID)
	}
	// the key must be present (null), not omitted
	if raw := k.a.do("GET", k.base+"/kill-switches/"+ks.ID, k.tok2, nil); !strings.Contains(string(raw.body), `"open_release_request_id":null`) {
		t.Fatalf("open_release_request_id must be an explicit null: %s", raw.body)
	}

	rr := k.file(k.tok2, ks.ID, "resolved") // second person files
	if rr.status != http.StatusCreated {
		t.Fatalf("file: %d %s", rr.status, rr.body)
	}
	var req killSwitchReleaseRequestDTO
	rr.decode(t, &req)

	// the DB reality the UI was wrong about: the switch row's own release_request_id stays NULL while open
	got := k.get(k.tok3, ks.ID)
	if got.ReleaseRequestID != nil {
		t.Fatalf("release_request_id is filled only at release; got %v", *got.ReleaseRequestID)
	}
	if openID(got) != req.ID {
		t.Fatalf("GET switch open_release_request_id = %q, want filed request %q", openID(got), req.ID)
	}
	if l := k.listed(k.tok3, ks.ID); openID(l) != req.ID {
		t.Fatalf("list open_release_request_id = %q, want %q", openID(l), req.ID)
	}
	if q := k.getReq(k.tok3, openID(got)); q.Status != "open" || q.KillSwitchID != ks.ID {
		t.Fatalf("request reached through the switch is wrong: %+v", q)
	}

	// self-approval (the filer) is refused 409 and leaves the switch engaged and the request open
	if r := k.a.do("POST", k.base+"/kill-switch-release-requests/"+req.ID+"/approve", k.tok2, nil); r.status != http.StatusConflict {
		t.Fatalf("self-approval: %d %s", r.status, r.body)
	}
	if k.a.auditCountByOutcome(k.tenant, "payments_kill_switch.approve_release", "denied") != 1 {
		t.Error("self-approval must write exactly one denied audit row")
	}
	if after := k.get(k.tok3, ks.ID); !after.Engaged || openID(after) != req.ID {
		t.Fatalf("a refused approval must change nothing: %+v", after)
	}

	// a different principal approves: released in the same step; no open request remains
	ap := k.a.do("POST", k.base+"/kill-switch-release-requests/"+req.ID+"/approve", k.tok3, nil)
	if ap.status != http.StatusOK {
		t.Fatalf("approve: %d %s", ap.status, ap.body)
	}
	var released killSwitchDTO
	ap.decode(t, &released)
	if released.Engaged || released.OpenReleaseRequestID != nil || released.ReleaseRequestID == nil || *released.ReleaseRequestID != req.ID {
		t.Fatalf("released switch wrong: %+v", released)
	}
	final := k.get(k.tok1, ks.ID)
	if final.Engaged || final.OpenReleaseRequestID != nil {
		t.Fatalf("after release GET must show released with no open request: %+v", final)
	}
	if q := k.getReq(k.tok1, req.ID); q.Status != "approved" {
		t.Fatalf("request status after approval = %s", q.Status)
	}
	if l := k.listed(k.tok1, ks.ID); l.OpenReleaseRequestID != nil || l.Engaged {
		t.Fatalf("after release the LIST must show released with no open request (an approved request is not open): %+v", l)
	}
}

// Duplicate filing: the unique index (23505) used to be a 500 with no denied audit.
func TestKillSwitchOpenRequest_DuplicateFilingIs409WithDeniedAudit(t *testing.T) {
	k := newKSActors(t)
	ks := k.engage(k.tok1, "payout", "incident_2")
	first := k.file(k.tok2, ks.ID, "first")
	if first.status != http.StatusCreated {
		t.Fatalf("first: %d %s", first.status, first.body)
	}
	var req killSwitchReleaseRequestDTO
	first.decode(t, &req)

	dup := k.file(k.tok3, ks.ID, "second")
	if dup.status != http.StatusConflict {
		t.Fatalf("duplicate filing must be 409, got %d %s", dup.status, dup.body)
	}
	var env struct {
		Code string `json:"code"`
	}
	dup.decode(t, &env)
	if env.Code != "KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN" {
		t.Fatalf("stable code = %q", env.Code)
	}
	if n := k.a.auditCountByOutcome(k.tenant, "payments_kill_switch.request_release", "denied"); n != 1 {
		t.Fatalf("duplicate filing must write exactly one denied audit row, got %d", n)
	}
	m := k.a.auditMetadataByOutcome(k.tenant, "payments_kill_switch.request_release", "denied")
	if m["denied_class"] != "duplicate_open_request" {
		t.Fatalf("denied_class = %v", m["denied_class"])
	}
	if n := k.a.auditCountByOutcome(k.tenant, "payments_kill_switch.request_release", "success"); n != 1 {
		t.Fatalf("exactly one successful filing expected, got %d", n)
	}
	// the original request is untouched and still the open one
	if got := k.get(k.tok1, ks.ID); openID(got) != req.ID {
		t.Fatalf("the original request must stay the open one: %q vs %q", openID(got), req.ID)
	}
}

func TestKillSwitchOpenRequest_CancelThenRefile(t *testing.T) {
	k := newKSActors(t)
	ks := k.engage(k.tok1, "*", "incident_3")
	var r1 killSwitchReleaseRequestDTO
	k.file(k.tok2, ks.ID, "one").decode(t, &r1)
	if c := k.a.do("POST", k.base+"/kill-switch-release-requests/"+r1.ID+"/cancel", k.tok3, nil); c.status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", c.status, c.body)
	}
	got := k.get(k.tok1, ks.ID)
	if !got.Engaged || got.OpenReleaseRequestID != nil {
		t.Fatalf("after cancel the switch stays engaged with no open request: %+v", got)
	}
	if l := k.listed(k.tok1, ks.ID); l.OpenReleaseRequestID != nil {
		t.Fatalf("after cancel the LIST must not report the cancelled request as open: %v", *l.OpenReleaseRequestID)
	}
	if q := k.getReq(k.tok1, r1.ID); q.Status != "cancelled" {
		t.Fatalf("status = %s", q.Status)
	}
	var r2 killSwitchReleaseRequestDTO
	rr := k.file(k.tok2, ks.ID, "two")
	if rr.status != http.StatusCreated {
		t.Fatalf("refile after cancel: %d %s", rr.status, rr.body)
	}
	rr.decode(t, &r2)
	if r2.ID == r1.ID || openID(k.get(k.tok1, ks.ID)) != r2.ID {
		t.Fatalf("the new request must be the open one, not the cancelled one")
	}
	// cancelling twice is a refusal, not a 500
	if c := k.a.do("POST", k.base+"/kill-switch-release-requests/"+r1.ID+"/cancel", k.tok3, nil); c.status != http.StatusConflict {
		t.Fatalf("second cancel: %d %s", c.status, c.body)
	}
}

// reason_code is bounded in BYTES by the database CHECK; the handler must say 400, never leak a 23514 as 500.
func TestKillSwitchOpenRequest_ReasonCodeByteLength(t *testing.T) {
	k := newKSActors(t)
	post := func(tok, path string, reason string) ksResp {
		return k.a.do("POST", k.base+path, tok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": reason})
	}
	two32 := strings.Repeat("é", 32) // 32 characters, 64 bytes: valid
	two33 := strings.Repeat("é", 33) // 33 characters, 66 bytes: invalid, but only 33 runes
	for name, c := range map[string]struct {
		reason string
		want   int
	}{
		"64 ascii":    {strings.Repeat("a", 64), http.StatusOK},
		"65 ascii":    {strings.Repeat("a", 65), http.StatusBadRequest},
		"32 two-byte": {two32, http.StatusOK},
		"33 two-byte": {two33, http.StatusBadRequest},
		"empty":       {"", http.StatusBadRequest},
	} {
		if r := post(k.tok1, "/kill-switches", c.reason); r.status != c.want {
			t.Errorf("engage %s: %d (want %d) %s", name, r.status, c.want, r.body)
		}
	}
	ks := k.engage(k.tok1, "payout", "incident_4")
	for name, c := range map[string]struct {
		reason string
		want   int
	}{
		"65 ascii":    {strings.Repeat("a", 65), http.StatusBadRequest},
		"33 two-byte": {two33, http.StatusBadRequest},
		"empty":       {"", http.StatusBadRequest},
	} {
		if r := k.file(k.tok2, ks.ID, c.reason); r.status != c.want {
			t.Errorf("file %s: %d (want %d) %s", name, r.status, c.want, r.body)
		}
	}
	if got := k.get(k.tok1, ks.ID); got.OpenReleaseRequestID != nil {
		t.Fatal("a rejected filing must not leave an open request")
	}
	if r := k.file(k.tok2, ks.ID, two32); r.status != http.StatusCreated {
		t.Fatalf("64-byte multi-byte reason must be accepted: %d %s", r.status, r.body)
	}
	if n := k.a.auditCountByOutcome(k.tenant, "payments_kill_switch.request_release", "denied"); n != 0 {
		t.Errorf("a 400 is a validation failure, not a considered refusal: %d denied rows", n)
	}
}

// A tenant viewer of a platform-engaged switch with a platform-filed request sees the request id (it is
// the tenant's own data) but no platform staff id anywhere.
func TestKillSwitchOpenRequest_TenantViewerOfPlatformSwitchSeesNoPlatformIDs(t *testing.T) {
	k := newKSActors(t)
	ks := k.engage(k.ptok1, "deposit", "platform_incident")
	rr := k.file(k.ptok1, ks.ID, "platform_resolved")
	if rr.status != http.StatusCreated {
		t.Fatalf("platform file: %d %s", rr.status, rr.body)
	}
	var req killSwitchReleaseRequestDTO
	rr.decode(t, &req)

	g := k.a.do("GET", k.base+"/kill-switches/"+ks.ID, k.tok1, nil)
	var viaTenant killSwitchDTO
	g.decode(t, &viaTenant)
	if openID(viaTenant) != req.ID {
		t.Fatalf("tenant viewer must see the open request id: %s", g.body)
	}
	sidAbsent(t, "tenant GET switch", g.body, k.p1, k.p2)
	l := k.a.do("GET", k.base+"/kill-switches", k.tok1, nil)
	sidAbsent(t, "tenant list", l.body, k.p1, k.p2)
	if !strings.Contains(string(l.body), req.ID) {
		t.Fatalf("tenant list must carry the open request id: %s", l.body)
	}
	q := k.a.do("GET", k.base+"/kill-switch-release-requests/"+openID(viaTenant), k.tok1, nil)
	sidAbsent(t, "tenant GET request reached via the switch", q.body, k.p1, k.p2)
	var raw map[string]any
	q.decode(t, &raw)
	if v, ok := raw["requested_by"]; !ok || v != nil || raw["requested_by_scope"] != "platform" {
		t.Errorf("platform requested_by must be null with scope platform: %v", raw)
	}
	// the tenant cannot approve or hold the slot; the platform viewer sees ids
	if r := k.a.do("POST", k.base+"/kill-switch-release-requests/"+req.ID+"/approve", k.tok2, nil); r.status != http.StatusConflict {
		t.Fatalf("tenant approve of a platform request: %d %s", r.status, r.body)
	}
	pg := k.a.do("GET", k.base+"/kill-switches/"+ks.ID, k.ptok2, nil)
	sidPresent(t, "platform viewer", pg.body, k.p1)
	if r := k.a.do("POST", k.base+"/kill-switch-release-requests/"+req.ID+"/approve", k.ptok2, nil); r.status != http.StatusOK {
		t.Fatalf("platform approve: %d %s", r.status, r.body)
	}
	if after := k.get(k.tok1, ks.ID); after.Engaged || after.OpenReleaseRequestID != nil {
		t.Fatalf("released: %+v", after)
	}
}

// Exactly what a re-engage does to an open request (stated in the console notice and the OpenAPI text).
func TestKillSwitchOpenRequest_ReengageEffects(t *testing.T) {
	k := newKSActors(t)

	// (a) same-scope tenant re-engage: reason overwritten, version bumped, the request stays OPEN but stale.
	ks := k.engage(k.tok1, "deposit", "first_reason")
	var req killSwitchReleaseRequestDTO
	k.file(k.tok2, ks.ID, "r").decode(t, &req)
	re := k.engage(k.tok3, "deposit", "second_reason")
	if re.ID != ks.ID || re.ReasonCode != "second_reason" || re.Version != ks.Version+1 {
		t.Fatalf("re-engage must overwrite the reason and bump the version: before %+v after %+v", ks, re)
	}
	if openID(re) != req.ID {
		t.Fatalf("a tenant re-engage must leave the request open (engage response): %+v", re)
	}
	if openID(k.get(k.tok1, ks.ID)) != req.ID {
		t.Fatal("a tenant re-engage must leave the request open (GET)")
	}
	if q := k.getReq(k.tok1, req.ID); q.Status != "open" || q.ExpectedVersion != ks.Version {
		t.Fatalf("request stays open at its frozen version: %+v", q)
	}
	if r := k.a.do("POST", k.base+"/kill-switch-release-requests/"+req.ID+"/approve", k.tok3, nil); r.status != http.StatusConflict {
		t.Fatalf("approving a request made stale by the re-engage must be refused: %d %s", r.status, r.body)
	}
	// ...and while stale it still holds the one-open slot, so a new filing is 409 until it is cancelled
	if r := k.file(k.tok3, ks.ID, "again"); r.status != http.StatusConflict {
		t.Fatalf("stale open request still holds the slot: %d %s", r.status, r.body)
	}
	if c := k.a.do("POST", k.base+"/kill-switch-release-requests/"+req.ID+"/cancel", k.tok3, nil); c.status != http.StatusNoContent {
		t.Fatalf("cancel the stale request: %d %s", c.status, c.body)
	}
	if r := k.file(k.tok3, ks.ID, "again"); r.status != http.StatusCreated {
		t.Fatalf("refile after cancelling the stale one: %d %s", r.status, r.body)
	}

	// (b) platform re-engage of a TENANT-engaged switch cancels the open tenant request.
	ks2 := k.engage(k.tok1, "payout", "tenant_reason")
	var treq killSwitchReleaseRequestDTO
	k.file(k.tok2, ks2.ID, "r").decode(t, &treq)
	took := k.engage(k.ptok1, "payout", "platform_takeover")
	if took.OpenReleaseRequestID != nil || took.EngagedByScope == nil || *took.EngagedByScope != "platform" {
		t.Fatalf("a platform takeover leaves no open request: %+v", took)
	}
	if k.get(k.ptok2, ks2.ID).OpenReleaseRequestID != nil {
		t.Fatal("GET after takeover must show no open request")
	}
	if q := k.getReq(k.tok1, treq.ID); q.Status != "cancelled" {
		t.Fatalf("takeover must cancel the open tenant request, status = %s", q.Status)
	}
	// and a tenant can no longer touch the switch
	if r := k.a.do("POST", k.base+"/kill-switches", k.tok1, map[string]any{"provider_scope": "*", "operation_scope": "payout", "reason_code": "x"}); r.status != http.StatusConflict {
		t.Fatalf("tenant re-engage of a platform-engaged switch: %d %s", r.status, r.body)
	}
}

// Tenant isolation: another tenant's open request never appears, and a foreign-tenant path is refused.
func TestKillSwitchOpenRequest_TenantIsolation(t *testing.T) {
	k := newKSActors(t)
	ks := k.engage(k.tok1, "deposit", "iso")
	var req killSwitchReleaseRequestDTO
	k.file(k.tok2, ks.ID, "r").decode(t, &req)

	other := k.a.tenant()
	otherAdmin := k.a.tenantStaff(other, "tenant_admin")
	otherTok := k.a.token(otherAdmin, other, auth.RoleTenantAdmin, auth.PrincipalStaff)
	otherBase := "/v1/admin/tenants/" + other.String() + "/payments"

	// tenant B's own list: empty, and nothing of tenant A's
	l := k.a.do("GET", otherBase+"/kill-switches", otherTok, nil)
	if l.status != http.StatusOK || strings.Contains(string(l.body), req.ID) || strings.Contains(string(l.body), ks.ID) {
		t.Fatalf("tenant B list leaks tenant A: %d %s", l.status, l.body)
	}
	// tenant B's token on tenant A's switch and request: forbidden, no data
	for _, p := range []string{"/kill-switches/" + ks.ID, "/kill-switches", "/kill-switch-release-requests/" + req.ID} {
		r := k.a.do("GET", k.base+p, otherTok, nil)
		if r.status != http.StatusForbidden || strings.Contains(string(r.body), req.ID) {
			t.Errorf("B token on A path %s: %d %s", p, r.status, r.body)
		}
	}
	// tenant B naming A's switch id on B's own path: 404
	if r := k.a.do("GET", otherBase+"/kill-switches/"+ks.ID, otherTok, nil); r.status != http.StatusNotFound {
		t.Errorf("A's switch under B's path: %d %s", r.status, r.body)
	}
	// a platform caller listing tenant B (which has its own open request) sees only B's
	bks := func() killSwitchDTO {
		r := k.a.do("POST", otherBase+"/kill-switches", otherTok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "b"})
		var d killSwitchDTO
		r.decode(t, &d)
		return d
	}()
	if bks.OpenReleaseRequestID != nil {
		t.Fatalf("tenant B's fresh switch must not inherit tenant A's open request: %+v", bks)
	}
	pl := k.a.do("GET", otherBase+"/kill-switches", k.ptok1, nil)
	if strings.Contains(string(pl.body), req.ID) {
		t.Fatalf("platform list of tenant B shows tenant A's request: %s", pl.body)
	}
}
