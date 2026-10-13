//go:build integration

package httpserver

// ADR 0115 slice S2: the staff surface of the RG limit APIs - authorization matrix, tenant isolation,
// staff-on-player-request decreases (and the step-up 403), self-target refusal, redaction.

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

func staffSet(kind, asset, period string, value any) rgBody {
	b := setLimit(kind, asset, period, value)
	b["reason_code"] = "player_request"
	b["evidence_ref"] = "ticket-4711"
	return b
}

func TestRGLimitsS2_Staff_DecreaseOnPlayerRequestAndRedaction(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	player := w.player(f, f.acctA1)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	path := "/v1/admin/players/" + f.acctA1.String() + "/rg/limits"
	w.call("POST", "/v1/me/rg/limits", player, "sp1", setLimit("deposit", "EUR", "rolling_24h", "1000")).want(t, 201, "")

	r := w.call("POST", path, comp, "st1", staffSet("deposit", "EUR", "rolling_24h", "400"))
	r.want(t, 201, "")
	o := r.obj(t)
	if o["change_kind"] != "decrease" || o["source"] != "staff_on_player_request" || o["value"] != "400" || o["evidence_ref"] != "ticket-4711" ||
		o["created_by"] != f.compA.String() || o["created_by_actor_type"] != "staff" || o["player_account_id"] != f.acctA1.String() {
		t.Fatalf("staff decrease: %v", o)
	}
	// the player's own view shows the change, its source and reason, and NO staff identity
	pv := w.call("GET", "/v1/me/rg/limits", player, "", nil)
	if v, _ := limitValue(t, pv, "deposit"); v != "400" {
		t.Fatalf("the staff decrease is in force: %q", v)
	}
	if !strings.Contains(string(pv.body), "staff_on_player_request") || strings.Contains(string(pv.body), f.compA.String()) ||
		strings.Contains(string(pv.body), "created_by") || strings.Contains(string(pv.body), "evidence_ref") {
		t.Fatalf("the player view must show the source but no staff identity / evidence: %s", pv.body)
	}
	// staff history: own tenant's staff id visible, the player's own rows carry created_by null
	hist := w.call("GET", "/v1/admin/players/"+f.acctA1.String()+"/rg/history", comp, "", nil)
	hist.want(t, 200, "")
	rows := hist.obj(t)["history"].([]any)
	if len(rows) != 2 {
		t.Fatalf("history rows: %d", len(rows))
	}
	for _, h := range rows {
		m := h.(map[string]any)
		switch m["created_by_actor_type"] {
		case "staff":
			if m["created_by"] != f.compA.String() {
				t.Errorf("staff row created_by: %v", m)
			}
		case "player":
			if m["created_by"] != nil {
				t.Errorf("a player's row must show no created_by: %v", m)
			}
		}
	}
	if lim := w.call("GET", path, w.staff(f.tenantA, f.adminA, auth.RoleTenantAdmin), "", nil); lim.status != 200 {
		t.Fatalf("tenant_admin may read: %s", lim.body)
	}
	// an initial value on behalf of the player is a tightening and is allowed
	w.call("POST", path, comp, "st2", staffSet("session_duration", "", "", "7200")).want(t, 201, "")
	// idempotent replay and mismatch
	rep := w.call("POST", path, comp, "st1", staffSet("deposit", "EUR", "rolling_24h", "400"))
	rep.want(t, 201, "")
	if rep.header.Get("Idempotent-Replayed") != "true" || rep.obj(t)["id"] != o["id"] {
		t.Fatal("staff replay returns the stored row")
	}
	w.call("POST", path, comp, "st1", staffSet("deposit", "EUR", "rolling_24h", "300")).want(t, 409, "RG_IDEMPOTENCY_MISMATCH")
	// a staff key does not collide with a player's key of the same text
	w.call("POST", "/v1/me/rg/limits", player, "st1", setLimit("deposit", "EUR", "rolling_24h", "350")).want(t, 201, "")
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", player, "", nil), "deposit"); v != "350" {
		t.Fatalf("player's own write: %q", v)
	}
	// audited with the staff actor and the source
	au := w.auditRows(f.tenantA, "rg.limit.change_requested")
	var staffRow map[string]any
	for _, a := range au {
		if a["source"] == "staff_on_player_request" && a["control_kind"] == "deposit" {
			staffRow = a
		}
	}
	if staffRow == nil || staffRow["_actor"] != f.compA.String() || staffRow["reason_code"] != "player_request" || staffRow["evidence_ref"] != "ticket-4711" {
		t.Fatalf("staff audit row: %v", staffRow)
	}
}

// Staff increases and removals are refused until ADR 0017 step-up ships; nothing is stored.
func TestRGLimitsS2_Staff_IncreaseAndRemovalReturn403StepUp(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	player := w.player(f, f.acctA1)
	comp := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	path := "/v1/admin/players/" + f.acctA1.String() + "/rg/limits"
	w.call("POST", "/v1/me/rg/limits", player, "si1", setLimit("deposit", "EUR", "rolling_24h", "300")).want(t, 201, "")
	before := w.changeCount(f.tenantA, f.acctA1)
	w.call("POST", path, comp, "si2", staffSet("deposit", "EUR", "rolling_24h", "900")).want(t, 403, "RG_STEP_UP_REQUIRED")
	w.call("POST", path, comp, "si3", staffSet("deposit", "EUR", "rolling_24h", nil)).want(t, 403, "RG_STEP_UP_REQUIRED")
	if n := w.changeCount(f.tenantA, f.acctA1); n != before {
		t.Fatalf("a refused staff increase / removal stores nothing: %d -> %d", before, n)
	}
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", player, "", nil), "deposit"); v != "300" {
		t.Fatalf("the limit is untouched: %q", v)
	}
	den := w.auditRows(f.tenantA, "rg.limit.change_denied")
	n := 0
	for _, d := range den {
		if d["code"] == "step_up_required" && d["_actor"] == f.compA.String() {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("both refusals are audited with the staff actor, got %d", n)
	}
	// the ceiling rules bind staff writes too
	w.call("POST", path, comp, "si4", staffSet("deposit", "EUR", "rolling_24h", "5001")).want(t, 422, "RG_LIMIT_ABOVE_CEILING")
	w.call("POST", path, comp, "si5", staffSet("deposit", "EUR", "rolling_7d", "1")).want(t, 422, "RG_PERIOD_NOT_ALLOWED")
	// reason and evidence are required
	for i, mutate := range []func(rgBody){func(b rgBody) { delete(b, "reason_code") }, func(b rgBody) { delete(b, "evidence_ref") },
		func(b rgBody) { b["reason_code"] = "" }, func(b rgBody) { b["evidence_ref"] = strings.Repeat("e", 201) }} {
		b := staffSet("deposit", "EUR", "rolling_24h", "100")
		mutate(b)
		w.call("POST", path, comp, "sr"+string(rune('a'+i)), b).want(t, 400, "validation_error")
	}
}

// Staff may not change their own Person's limits.
func TestRGLimitsS2_Staff_SelfTargetRefused(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	self := w.staff(f.tenantA, f.compSelf, auth.RoleCompliance) // staff of personA; acctA1 and acctA2 are personA's
	for _, acct := range []uuid.UUID{f.acctA1, f.acctA2} {
		w.call("POST", "/v1/admin/players/"+acct.String()+"/rg/limits", self, "self-"+acct.String(), staffSet("deposit", "EUR", "rolling_24h", "100")).want(t, 403, "RG_SELF_TARGET")
	}
	// the same staff member may act on another Person's account
	w.call("POST", "/v1/admin/players/"+f.acctX.String()+"/rg/limits", self, "self-x", staffSet("deposit", "EUR", "rolling_24h", "100")).want(t, 201, "")
	if n := w.changeCount(f.tenantA, f.acctA1) + w.changeCount(f.tenantA, f.acctA2); n != 0 {
		t.Fatalf("self-targeted writes store nothing, got %d", n)
	}
}

// Cross-tenant isolation (a request for tenant A's data with tenant B's token must fail), on every
// read and write that names a player or a change.
func TestRGLimitsS2_CrossTenantIsolation(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	w.floor(f.jur, "deposit", nil)
	pa := w.player(f, f.acctA1)
	compA := w.staff(f.tenantA, f.compA, auth.RoleCompliance)
	compB := w.staff(f.tenantB, f.compB, auth.RoleCompliance)
	inc := w.call("POST", "/v1/me/rg/limits", pa, "x1", setLimit("deposit", "EUR", "rolling_24h", "100"))
	inc.want(t, 201, "")
	pend := w.call("POST", "/v1/me/rg/limits", pa, "x2", setLimit("deposit", "EUR", "rolling_24h", "200"))
	pend.want(t, 202, "")
	pendID := pend.obj(t)["id"].(string)

	// tenant B staff naming tenant A's player: the same 404 as an unknown id, on every route
	a := f.acctA1.String()
	unknown := uuid.NewString()
	for _, route := range [][2]string{{"GET", "/rg/limits"}, {"GET", "/rg/history"}, {"POST", "/rg/limits"}} {
		var body any
		if route[0] == "POST" {
			body = staffSet("deposit", "EUR", "rolling_24h", "50")
		}
		other := w.call(route[0], "/v1/admin/players/"+a+route[1], compB, "xt-"+route[1], body)
		ghost := w.call(route[0], "/v1/admin/players/"+unknown+route[1], compB, "xt-"+route[1], body)
		other.want(t, 404, "not_found")
		ghost.want(t, 404, "not_found")
		if string(other.body[:strings.Index(string(other.body), "request_id")]) != string(ghost.body[:strings.Index(string(ghost.body), "request_id")]) {
			t.Fatalf("a foreign player must be indistinguishable from an unknown one: %s vs %s", other.body, ghost.body)
		}
	}
	// tenant A staff naming tenant B's player (the same Person, other tenant)
	w.call("GET", "/v1/admin/players/"+f.acctB.String()+"/rg/limits", compA, "", nil).want(t, 404, "not_found")
	// a player of tenant B cannot confirm / cancel tenant A's change
	pb := w.player(f, f.acctB)
	w.call("POST", "/v1/me/rg/limits/changes/"+pendID+"/cancel", pb, "xc1", nil).want(t, 404, "not_found")
	w.call("POST", "/v1/me/rg/limits/changes/"+pendID+"/confirm", pb, "xc2", nil).want(t, 404, "not_found")
	// another player of the SAME tenant (different Person) cannot either: account-scope rows are private
	px := w.player(f, f.acctX)
	w.call("POST", "/v1/me/rg/limits/changes/"+pendID+"/cancel", px, "xc3", nil).want(t, 404, "not_found")
	// a token for tenant A carrying tenant B's player id as subject does not become that player
	forged := w.tok(f.acctB, f.tenantA, auth.RolePlayer, auth.PrincipalPlayer)
	w.call("GET", "/v1/me/rg/limits", forged, "", nil).want(t, 404, "not_found")
	w.call("POST", "/v1/me/rg/limits", forged, "xf1", setLimit("deposit", "EUR", "rolling_24h", "10")).want(t, 404, "not_found")
	if n := w.changeCount(f.tenantB, f.acctB); n != 0 {
		t.Fatalf("tenant B's player has no rows, got %d", n)
	}
	// tenant A's pending change is intact
	if v, _ := limitValue(t, w.call("GET", "/v1/me/rg/limits", pa, "", nil), "deposit"); v != "100" {
		t.Fatalf("tenant A's limit is unchanged: %q", v)
	}
	// and a body-supplied tenant is rejected outright
	b := staffSet("deposit", "EUR", "rolling_24h", "50")
	b["tenant_id"] = f.tenantA.String()
	w.call("POST", "/v1/admin/players/"+f.acctX.String()+"/rg/limits", compA, "xb", b).want(t, 400, "validation_error")
}

// The authorization matrix: every route against every principal class. The generic gates answer
// 401 (no token) / 403 (wrong principal type, missing permission, wrong scope) before any handler logic.
func TestRGLimitsS2_AuthorizationMatrix(t *testing.T) {
	w := newRGW(t, rgWOpt{})
	f := w.fixture()
	id := uuid.NewString()
	tokens := map[string]string{
		"anon":     "",
		"player":   w.player(f, f.acctA1),
		"support":  w.staff(f.tenantA, f.supportA, auth.RoleSupport),
		"admin":    w.staff(f.tenantA, f.adminA, auth.RoleTenantAdmin),
		"comp":     w.staff(f.tenantA, f.compA, auth.RoleCompliance),
		"compB":    w.staff(f.tenantB, f.compB, auth.RoleCompliance),
		"platform": w.platform(w.ops.Requester),
	}
	routes := []struct {
		method, path string
		allowed      []string
	}{
		{"GET", "/v1/me/rg/limits", []string{"player"}},
		{"POST", "/v1/me/rg/limits", []string{"player"}},
		{"POST", "/v1/me/rg/limits/changes/" + id + "/confirm", []string{"player"}},
		{"POST", "/v1/me/rg/limits/changes/" + id + "/cancel", []string{"player"}},
		{"POST", "/v1/me/rg/time-out", []string{"player"}},
		{"GET", "/v1/admin/players/" + id + "/rg/limits", []string{"admin", "comp", "compB"}},
		{"GET", "/v1/admin/players/" + id + "/rg/history", []string{"admin", "comp", "compB"}},
		{"POST", "/v1/admin/players/" + id + "/rg/limits", []string{"comp", "compB"}},
		{"GET", "/v1/admin/rg/limit-configs", []string{"admin", "comp", "compB"}},
		{"POST", "/v1/admin/rg/limit-configs", []string{"comp", "compB"}},
		{"GET", "/v1/admin/platform/rg/limit-config-change-requests", []string{"platform"}},
		{"POST", "/v1/admin/platform/rg/limit-config-change-requests", []string{"platform"}},
		{"POST", "/v1/admin/platform/rg/limit-config-change-requests/" + id + "/approve", []string{"platform"}},
		{"POST", "/v1/admin/platform/rg/limit-config-change-requests/" + id + "/reject", []string{"platform"}},
		{"POST", "/v1/admin/platform/rg/limit-config-change-requests/" + id + "/cancel", []string{"platform"}},
	}
	for _, rt := range routes {
		allowed := map[string]bool{}
		for _, a := range rt.allowed {
			allowed[a] = true
		}
		for name, tk := range tokens {
			var body any
			if rt.method == "POST" {
				body = rgBody{}
			}
			r := w.call(rt.method, rt.path, tk, "authz-"+name, body)
			switch {
			case allowed[name]:
				if r.status == 401 || r.status == 403 || r.status >= 500 {
					t.Errorf("%s %s as %s must pass the gates, got %d %s", rt.method, rt.path, name, r.status, r.body)
				}
			case name == "anon":
				if r.status != 401 {
					t.Errorf("%s %s without a token must be 401, got %d", rt.method, rt.path, r.status)
				}
			default:
				if r.status != 403 {
					t.Errorf("%s %s as %s must be 403, got %d %s", rt.method, rt.path, name, r.status, r.body)
				}
			}
		}
	}
}
