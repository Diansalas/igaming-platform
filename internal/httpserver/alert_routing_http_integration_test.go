//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
)

// ALERT-DELIVERY-1 routing readiness at the HTTP layer. Every test runs through
// the REAL RUNTIME ROLE (alertworld.World asserts it is neither superuser nor
// BYPASSRLS) against a throwaway database.

type arAPI struct {
	t      *testing.T
	w      *alertworld.World
	issuer *auth.Issuer
	srv    *httptest.Server
	disp   *alerting.Dispatcher
	admin  uuid.UUID
}

func newARAPI(t *testing.T, production bool) *arAPI {
	t.Helper()
	w := alertworld.NewWorld(t, "arh_")
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "ar-test", "ar-test")
	disp := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{}, alerting.LogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:                     w.Pool,
		AuthIssuer:             issuer,
		ServiceName:            "ar-test",
		AccessTokenTTL:         time.Hour,
		RefreshTokenTTL:        time.Hour,
		AlertRouting:           disp,
		AlertRoutingProduction: production,
	}))
	t.Cleanup(srv.Close)
	return &arAPI{t: t, w: w, issuer: issuer, srv: srv, disp: disp, admin: w.SeedAdmin()}
}

func (a *arAPI) token(subject, tenant uuid.UUID, role auth.Role) string {
	a.t.Helper()
	tok, err := a.issuer.Issue(subject.String(), tenant, role, auth.PrincipalStaff, time.Hour)
	if err != nil {
		a.t.Fatal(err)
	}
	return tok
}

func (a *arAPI) adminToken() string { return a.token(a.admin, uuid.Nil, auth.RolePlatformAdmin) }

type arResp struct {
	status int
	body   []byte
}

func (r arResp) json() map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(r.body, &m)
	return m
}

func (a *arAPI) do(method, path, token string, body any) arResp {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return arResp{status: resp.StatusCode, body: b}
}

// platformAudits returns the platform-scope (tenant_id NULL) audit rows of an action.
func (a *arAPI) platformAudits(action string) []map[string]any {
	a.t.Helper()
	var out []map[string]any
	if err := a.w.Pool.WithPlatformAdmin(context.Background(), a.admin, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT outcome, metadata, actor_id::text, COALESCE(ip_address::text,''), COALESCE(request_id,'') FROM audit_log WHERE action = $1 AND tenant_id IS NULL ORDER BY created_at, id`, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var outcome, actor, ip, rid string
			var raw []byte
			if err := rows.Scan(&outcome, &raw, &actor, &ip, &rid); err != nil {
				return err
			}
			m := map[string]any{}
			_ = json.Unmarshal(raw, &m)
			m["_outcome"], m["_actor"], m["_ip"], m["_rid"] = outcome, actor, ip, rid
			out = append(out, m)
		}
		return rows.Err()
	}); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func (a *arAPI) tenantAudits(tenant uuid.UUID, action string) int {
	a.t.Helper()
	var n int
	if err := a.w.Pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND tenant_id = $2`, action, tenant).Scan(&n)
	}); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *arAPI) routeCount() int {
	a.t.Helper()
	var n int
	if err := a.w.Pool.WithPlatformAdmin(context.Background(), a.admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_routes`).Scan(&n)
	}); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func routeBody(m map[string]any) map[string]any {
	b := map[string]any{"severity": "p2", "escalation_step": 0, "channel_kind": "mock", "enabled": false, "reason_code": "initial_setup"}
	for k, v := range m {
		b[k] = v
	}
	return b
}

func TestAlertRoutingAPI_CreateSupersedeList_AuditedWithoutTheRecipient(t *testing.T) {
	a := newARAPI(t, false)
	tok := a.adminToken()

	// Staged disabled route with no recipient.
	r := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(nil))
	if r.status != http.StatusCreated {
		t.Fatalf("create staged route: %d %s", r.status, r.body)
	}
	first := r.json()
	if first["enabled"] != false || first["human_notification"] != false {
		t.Fatalf("response: %v", first)
	}
	// A second create without supersedes_id conflicts and is audited as denied.
	if r := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(nil)); r.status != http.StatusConflict {
		t.Fatalf("duplicate current route: %d %s", r.status, r.body)
	}
	// Supersede with an enabled mock route naming a recipient.
	const recipient = "ops-primary-rota"
	r = a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{
		"enabled": true, "recipient_ref": recipient, "supersedes_id": first["id"], "reason_code": "on_call_change", "escalate_after_seconds": 300,
	}))
	if r.status != http.StatusCreated {
		t.Fatalf("supersede: %d %s", r.status, r.body)
	}
	second := r.json()
	if second["enabled"] != true || second["recipient_ref"] != recipient || second["escalate_after_seconds"].(float64) != 300 {
		t.Fatalf("second: %v", second)
	}
	// Listing: current only, then history.
	cur := a.do("GET", "/v1/admin/alerting/routes", tok, nil).json()["routes"].([]any)
	if len(cur) != 1 || cur[0].(map[string]any)["id"] != second["id"] {
		t.Fatalf("current routes: %v", cur)
	}
	hist := a.do("GET", "/v1/admin/alerting/routes?include_superseded=true", tok, nil).json()["routes"].([]any)
	if len(hist) != 2 {
		t.Fatalf("history: %v", hist)
	}

	// Audit: exactly one success per write, with before/after and reason code, never the recipient value.
	audits := a.platformAudits("alert_route.create")
	var ok, denied int
	for _, m := range audits {
		switch m["_outcome"] {
		case "success":
			ok++
			if m["_actor"] != a.admin.String() || m["_ip"] == "" || m["_rid"] == "" {
				t.Fatalf("success audit lacks actor/ip/request id: %v", m)
			}
		case "denied":
			denied++
		}
		if raw, _ := json.Marshal(m); strings.Contains(string(raw), recipient) {
			t.Fatalf("the recipient value must never be audited: %s", raw)
		}
	}
	if ok != 2 || denied != 1 {
		t.Fatalf("audit rows: %d success / %d denied, want 2 / 1: %v", ok, denied, audits)
	}
	last := audits[len(audits)-1]
	if last["reason_code"] != "on_call_change" || last["recipient_ref_present"] != true || last["before"] == nil || last["after"] == nil {
		t.Fatalf("audit metadata incomplete: %v", last)
	}
}

func TestAlertRoutingAPI_ValidationRefusalsAreAuditedAndChangeNothing(t *testing.T) {
	a := newARAPI(t, false)
	tok := a.adminToken()
	bad := []struct {
		name string
		body map[string]any
	}{
		{"unknown severity", routeBody(map[string]any{"severity": "p0"})},
		{"step out of range", routeBody(map[string]any{"escalation_step": 99})},
		{"unknown channel kind", routeBody(map[string]any{"channel_kind": "pagerduty"})},
		{"free-text reason", routeBody(map[string]any{"reason_code": "just because"})},
		{"enabled without recipient", routeBody(map[string]any{"enabled": true})},
		{"email recipient", routeBody(map[string]any{"recipient_ref": "ops@example.com"})},
		{"address recipient", routeBody(map[string]any{"recipient_ref": "169.254.169.254:80"})},
		{"key-shaped recipient", routeBody(map[string]any{"recipient_ref": "0123456789abcdef0123456789abcdef"})},
		{"negative escalate_after", routeBody(map[string]any{"escalate_after_seconds": -1})},
		{"supersedes a missing route", routeBody(map[string]any{"supersedes_id": uuid.NewString()})},
	}
	for _, c := range bad {
		r := a.do("POST", "/v1/admin/alerting/routes", tok, c.body)
		if r.status != http.StatusBadRequest && r.status != http.StatusNotFound {
			t.Errorf("%s: status %d (%s), want a refusal", c.name, r.status, r.body)
		}
	}
	if n := a.routeCount(); n != 0 {
		t.Fatalf("refused requests created %d route rows", n)
	}
	var denied int
	for _, m := range a.platformAudits("alert_route.create") {
		if m["_outcome"] == "denied" {
			denied++
		}
	}
	if denied != len(bad) {
		t.Fatalf("%d refusals audited, want %d", denied, len(bad))
	}
	// Unknown JSON fields are refused too (DisallowUnknownFields).
	if r := a.do("POST", "/v1/admin/alerting/routes", tok, map[string]any{"severity": "p1", "tenant_id": uuid.NewString()}); r.status == http.StatusCreated {
		t.Fatal("a client-supplied tenant_id must never be accepted")
	}
}

// Security M-3: in production a p1/p2 route on a channel that notifies nobody is refused.
func TestAlertRoutingAPI_ProductionRefusesNonHumanP1P2Enabled(t *testing.T) {
	a := newARAPI(t, true)
	tok := a.adminToken()
	if r := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{"severity": "p1", "enabled": true, "recipient_ref": "ops"})); r.status != http.StatusBadRequest {
		t.Fatalf("p1 enabled on mock in production: %d %s", r.status, r.body)
	}
	if r := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{"severity": "p1", "enabled": false})); r.status != http.StatusCreated {
		t.Fatalf("a DISABLED p1 route may be staged in production: %d %s", r.status, r.body)
	}
	if r := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(map[string]any{"severity": "p3", "enabled": true, "recipient_ref": "ops"})); r.status != http.StatusCreated {
		t.Fatalf("a p3 log/mock route stays allowed in production: %d %s", r.status, r.body)
	}
}

// Authorization: each layer pinned on its own; refusals are audited in the caller's own scope; tenants cannot reach anything.
func TestAlertRoutingAPI_Authorization_EachLayer_DeniedAuditedInCallerScope(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	other := a.w.SeedTenant(a.admin)

	tenantAdmin := a.token(uuid.New(), tenant, auth.RoleTenantAdmin)
	compliance := a.token(a.admin, uuid.Nil, auth.RoleCompliance)      // platform scope, wrong permission (layer 1)
	adminWithTenant := a.token(a.admin, other, auth.RolePlatformAdmin) // has the permission, tenant scoped (layer 2)

	// Layer 1 (permission) and layer 2 (platform scope) on the mutation.
	for name, tok := range map[string]string{"tenant admin": tenantAdmin, "platform user without alert:route_manage": compliance, "platform_admin presented with a tenant": adminWithTenant} {
		if r := a.do("POST", "/v1/admin/alerting/routes", tok, routeBody(nil)); r.status != http.StatusForbidden {
			t.Errorf("%s: POST status %d, want 403", name, r.status)
		}
	}
	if a.routeCount() != 0 {
		t.Fatal("a refused request created a route")
	}
	// Denied audit: the tenant admin's goes to its OWN tenant's audit; the platform user's to the platform audit; never a platform row from a tenant session.
	if n := a.tenantAudits(tenant, "alert_route.create"); n != 1 {
		t.Fatalf("tenant-scope denied audit rows = %d, want 1", n)
	}
	if n := a.tenantAudits(other, "alert_route.create"); n != 1 {
		t.Fatalf("the tenant-scoped platform_admin token is audited in that tenant's scope: %d", n)
	}
	var platformDenied int
	for _, m := range a.platformAudits("alert_route.create") {
		if m["_outcome"] == "denied" && m["denied"] == "permission" {
			platformDenied++
		}
	}
	if platformDenied != 1 {
		t.Fatalf("platform-scope permission denial rows = %d, want 1", platformDenied)
	}

	// Reads: GET routes needs alert:route_manage, status and list need alert:manage; tenants are refused everywhere.
	reads := []string{"/v1/admin/alerting/routes", "/v1/admin/alerting/status", "/v1/admin/alerts"}
	for _, p := range reads {
		if r := a.do("GET", p, tenantAdmin, nil); r.status != http.StatusForbidden {
			t.Errorf("tenant admin GET %s: %d, want 403", p, r.status)
		}
		if r := a.do("GET", p, adminWithTenant, nil); r.status != http.StatusForbidden {
			t.Errorf("tenant-scoped platform_admin GET %s: %d, want 403 (explicit scope check)", p, r.status)
		}
		if r := a.do("GET", p, "", nil); r.status != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: %d, want 401", p, r.status)
		}
	}
	if r := a.do("GET", "/v1/admin/alerting/routes", compliance, nil); r.status != http.StatusForbidden {
		t.Errorf("compliance GET routes: %d, want 403", r.status)
	}
}

// Tenant isolation at the data layer: a route exists, a tenant token cannot read or write it by any endpoint.
func TestAlertRoutingAPI_TenantTokenCannotSeeOrChangeRouting(t *testing.T) {
	a := newARAPI(t, false)
	tenantA := a.w.SeedTenant(a.admin)
	tenantB := a.w.SeedTenant(a.admin)
	a.w.AddRoute(a.admin, alerting.SeverityP2, 0, alerting.ChannelMock, "secret-rota", true)
	for _, ten := range []uuid.UUID{tenantA, tenantB} {
		tok := a.token(uuid.New(), ten, auth.RoleTenantAdmin)
		r := a.do("GET", "/v1/admin/alerting/routes", tok, nil)
		if r.status != http.StatusForbidden || strings.Contains(string(r.body), "secret-rota") {
			t.Fatalf("tenant %s read routes: %d %s", ten, r.status, r.body)
		}
	}
}

func TestAlertAdmin_RefusedAckAndResolveAreAudited_AckTakesAReasonCode(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	id := a.w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	tok := a.adminToken()
	ackPath := "/v1/admin/alerts/" + id.String() + "/ack"

	// Refused: wrong permission, tenant-scoped, unknown alert, invalid reason.
	if r := a.do("POST", ackPath, a.token(a.admin, uuid.Nil, auth.RoleCompliance), nil); r.status != http.StatusForbidden {
		t.Fatalf("permission: %d", r.status)
	}
	if r := a.do("POST", ackPath, a.token(a.admin, tenant, auth.RolePlatformAdmin), nil); r.status != http.StatusForbidden {
		t.Fatalf("scope: %d", r.status)
	}
	if r := a.do("POST", "/v1/admin/alerts/"+uuid.NewString()+"/ack", tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("not found: %d", r.status)
	}
	if r := a.do("POST", ackPath, tok, map[string]any{"reason_code": "NOT A TOKEN"}); r.status != http.StatusBadRequest {
		t.Fatalf("invalid reason: %d", r.status)
	}
	// Ack with a reason, then a second ack is an invalid transition (409), and resolve without reason is refused.
	if r := a.do("POST", ackPath, tok, map[string]any{"reason_code": "investigating"}); r.status != http.StatusOK {
		t.Fatalf("ack: %d %s", r.status, r.body)
	}
	if r := a.do("POST", ackPath, tok, nil); r.status != http.StatusConflict {
		t.Fatalf("second ack: %d", r.status)
	}
	if r := a.do("POST", "/v1/admin/alerts/"+id.String()+"/resolve", tok, map[string]any{}); r.status != http.StatusBadRequest {
		t.Fatalf("resolve without reason: %d", r.status)
	}

	var success, denied int
	reasons := map[string]int{}
	for _, m := range a.platformAudits("alerts.ack") {
		switch m["_outcome"] {
		case "success":
			success++
			if m["reason_code"] != "investigating" {
				t.Fatalf("ack reason not recorded: %v", m)
			}
		case "denied":
			denied++
			reasons[m["denied"].(string)]++
		}
	}
	if success != 1 || denied != 4 {
		t.Fatalf("ack audits: %d success, %d denied (%v), want 1 and 4", success, denied, reasons)
	}
	if reasons["permission"] != 1 || reasons["not_found"] != 1 || reasons["reason_code_invalid"] != 1 || reasons["invalid_transition"] != 1 {
		t.Fatalf("platform-scope denial reasons: %v", reasons)
	}
	if n := a.tenantAudits(tenant, "alerts.ack"); n != 1 {
		t.Fatalf("the tenant-scoped caller's refusal must be audited in that tenant's scope, got %d", n)
	}
	var resolveDenied int
	for _, m := range a.platformAudits("alerts.resolve") {
		if m["_outcome"] == "denied" && m["denied"] == "reason_code_required" {
			resolveDenied++
		}
	}
	if resolveDenied != 1 {
		t.Fatalf("resolve refusal audits = %d", resolveDenied)
	}
}

func TestAlertRoutingAPI_StatusReportsNotReadyWithExactMissingInput_ListNeverCallsNonHumanDelivered(t *testing.T) {
	a := newARAPI(t, false)
	tenant := a.w.SeedTenant(a.admin)
	tok := a.adminToken()

	// No dispatcher evaluation yet: every severity not ready, "not evaluated".
	st := a.do("GET", "/v1/admin/alerting/status", tok, nil).json()
	if st["ready"] != false {
		t.Fatalf("status before evaluation: %v", st)
	}

	// Enabled mock routes for p1/p2; deliver; evaluate.
	a.w.AddRoute(a.admin, alerting.SeverityP1, 0, alerting.ChannelMock, "rota", true)
	a.w.AddRoute(a.admin, alerting.SeverityP2, 0, alerting.ChannelMock, "rota", true)
	p2 := a.w.SeedAlert(tenant, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
	unrouted := a.w.SeedAlert(tenant, alerting.KindReconciliationLedgerProjectionDrift, "d:"+uuid.NewString()) // p1
	ch := alertingtest.NewRecordingChannel()

	// The server's own dispatcher (LogSink only) evaluates readiness; mock is not wired there.
	if err := a.disp.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	st = a.do("GET", "/v1/admin/alerting/status", tok, nil).json()
	if st["ready"] != false {
		t.Fatalf("readiness must be false today: %v", st)
	}
	for _, s := range st["severities"].([]any) {
		m := s.(map[string]any)
		if m["required_for_readiness"] == true {
			if m["ready"] != false || m["human_notification_routes"].(float64) != 0 || m["routes_enabled"].(float64) != 1 || m["routes_configured"].(float64) != 1 {
				t.Fatalf("severity %v: %v", m["severity"], m)
			}
			if m["reason"] == "" || m["missing_operator_input"] == "" {
				t.Fatalf("the exact missing operator input must be named: %v", m)
			}
		}
	}

	// The list shows the delivery state; a route to an unwired channel is a visible unrouted row (no_sink), not "delivered".
	list := a.do("GET", "/v1/admin/alerts?state=open", tok, nil).json()["alerts"].([]any)
	byID := map[string]map[string]any{}
	for _, it := range list {
		m := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	for _, id := range []uuid.UUID{p2, unrouted} {
		it := byID[id.String()]
		if it == nil {
			t.Fatalf("alert %s missing from the list", id)
		}
		if it["delivery_state"] != "unrouted" || it["unrouted_reason"] != "no_sink" || it["notified_a_person"] != false {
			t.Fatalf("%v", it)
		}
	}

	// Now wire the recording channel and deliver: recorded_non_human, never "delivered", notified_a_person=false.
	d2 := alerting.NewDispatcher(a.w.Pool, alerting.DispatcherConfig{}, ch)
	for i := 0; i < 3; i++ {
		if err := d2.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	list = a.do("GET", "/v1/admin/alerts?severity=p2&limit=50", tok, nil).json()["alerts"].([]any)
	var found bool
	for _, it := range list {
		m := it.(map[string]any)
		if m["id"] == p2.String() {
			found = true
			if m["delivery_state"] != "recorded_non_human" || m["notified_a_person"] != false {
				t.Fatalf("a mock delivery must never read as delivered: %v", m)
			}
		}
	}
	if !found {
		t.Fatal("p2 alert missing")
	}
	if r := a.do("GET", "/v1/admin/alerts?state=bogus", tok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad filter: %d", r.status)
	}
}
