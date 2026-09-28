//go:build integration

// PRH-2 K1 (ADR 0099): the scoped financial capability grant admin API,
// end to end over real HTTP. Mirrors payments_kill_switch_api_integration_test.go's
// own harness shape exactly (a private scratch database migrated to the
// latest on-disk migration, a real httptest.Server, real JWTs).
//
// A-1 (the role x family x capability x action matrix, at HTTP and DB
// level): covers, for each capability-grant route, the eligible tenant
// role (tenant_admin), platform_admin, an ineligible role (support, which
// holds none of the capability_grant:* permissions), and the cross-tenant
// refusal (a different tenant's tenant_admin naming/reading a foreign
// tenant). This is a representative cut of the full role list (this
// codebase's rolePermissions map has ~10 roles), chosen because every
// OTHER role's outcome is already fully determined by
// internal/auth/permission.go's own static map (RequirePermission denies
// before this handler code ever runs) - internal/auth's own tests already
// prove RoleHasPermission is correct per-permission; what THIS suite adds
// is that the capability-grant routes are wired to the RIGHT permission,
// and that the two-layer design (permission AND DB invariant) actually
// holds end to end over HTTP, which no unit test can show.
//
// I-1 (architect ruling on K1 G-P1): a platform session naming a real
// tenant-scoped `finance` grantee is refused CG010/409, with zero request
// rows and zero success audit rows - proven here at the full HTTP level.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func cgMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

type cgAPI struct {
	t      *testing.T
	pool   *db.Pool
	issuer *auth.Issuer
	srv    *httptest.Server
}

func newCGAPI(t *testing.T) *cgAPI {
	t.Helper()
	url := scratchdb.New(t, "cg_api_")
	pool, err := db.Connect(context.Background(), url, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), cgMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch up: %v", err)
	}
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "cg-test", "cg-test")
	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              pool,
		AuthIssuer:      issuer,
		ServiceName:     "cg-test",
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return &cgAPI{t: t, pool: pool, issuer: issuer, srv: srv}
}

func (a *cgAPI) tenant() uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	if err := a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'CG API', $2, 'under_platform_licence')`,
			id, "cgapi-"+strings.ReplaceAll(id.String(), "-", "")[:16])
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

// staff inserts a staff_users row (person-linked) with the given
// tenant/role, and returns its id. tenantID == uuid.Nil means a
// platform-scoped row.
func (a *cgAPI) staff(tenantID uuid.UUID, role string) uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	person := uuid.New()
	insert := func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		var tid any
		if tenantID != uuid.Nil {
			tid = tenantID
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1, $2, $3, 'x', $4, 'active', $5)`,
			id, tid, "cgapi-"+id.String()+"@test.example", role, person)
		return err
	}
	var err error
	if tenantID == uuid.Nil {
		err = a.pool.WithoutTenant(context.Background(), insert)
	} else {
		err = a.pool.WithTenant(context.Background(), tenantID, insert)
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *cgAPI) token(subject, tenantID uuid.UUID, role auth.Role) string {
	a.t.Helper()
	tok, err := a.issuer.Issue(subject.String(), tenantID, role, auth.PrincipalStaff, time.Hour)
	if err != nil {
		a.t.Fatal(err)
	}
	return tok
}

type cgResp struct {
	status int
	body   []byte
}

func (a *cgAPI) do(method, path, token string, body any) cgResp {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	return cgResp{status: resp.StatusCode, body: b}
}

func (r cgResp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// requestCount reads via WithPlatformAdmin (not plain WithTenant): K1-C2's
// per-command RLS split requires app.principal_id to be set for the
// TENANT-scope SELECT policy, which plain WithTenant never sets - the
// platform-scope SELECT policy only requires the platform GUC, which
// WithPlatformAdmin does set, and reads across any tenant, so this stays
// correct as a pure test-assertion helper (never how a real caller reads).
func (a *cgAPI) requestCount(tenantID, granteeID uuid.UUID) int {
	a.t.Helper()
	var n int
	if err := a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grant_requests WHERE tenant_id = $1 AND grantee_staff_id = $2`, tenantID, granteeID).Scan(&n)
	}); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *cgAPI) successAuditCount(action string) int {
	a.t.Helper()
	var n int
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND outcome = 'success'`, action).Scan(&n)
	}); err != nil {
		a.t.Fatal(err)
	}
	return n
}

// --- I-1: G-P1 fail-closed, legible error, zero side effects ---

func TestCapabilityAPI_I1_PlatformSessionCannotRequestForTenantGrantee(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	financeGrantee := a.staff(tenantID, "finance")
	platformPrincipal := a.staff(uuid.Nil, "platform_admin")
	platformTok := a.token(platformPrincipal, uuid.Nil, auth.RolePlatformAdmin)

	before := a.successAuditCount("capability_grant.requested")

	resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", platformTok, map[string]any{
		"grantee_staff_id": financeGrantee.String(),
		"capability":       "ledger_adjustment:initiate",
		"reason_code":      "test",
	})
	if resp.status != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", resp.status, resp.body)
	}
	var body struct {
		Message string `json:"message"`
	}
	resp.decode(t, &body)
	if !strings.Contains(body.Message, "platform-originated grants for tenant staff are not supported") {
		t.Fatalf("expected the I-1 legible message, got %q", body.Message)
	}

	if n := a.requestCount(tenantID, financeGrantee); n != 0 {
		t.Fatalf("expected zero request rows, got %d", n)
	}
	if after := a.successAuditCount("capability_grant.requested"); after != before {
		t.Fatalf("expected zero new success audit rows, before=%d after=%d", before, after)
	}
}

// --- A-1: the role x route x scope matrix ---

type cgRouteCase struct {
	name   string
	method string
	path   func(tenantID uuid.UUID) string
	body   func() any
}

func cgRoutes(tenantID uuid.UUID, granteeID, requestID, grantID uuid.UUID) []cgRouteCase {
	base := "/v1/admin/tenants/" + tenantID.String() + "/capability-grants"
	return []cgRouteCase{
		{"create_request", http.MethodPost, func(uuid.UUID) string { return base + "/requests" }, func() any {
			return map[string]any{"grantee_staff_id": granteeID.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test"}
		}},
		{"list_requests", http.MethodGet, func(uuid.UUID) string { return base + "/requests" }, func() any { return nil }},
		{"cancel_request", http.MethodPost, func(uuid.UUID) string { return base + "/requests/" + requestID.String() + "/cancel" }, func() any { return nil }},
		{"approve_request", http.MethodPost, func(uuid.UUID) string { return base + "/requests/" + requestID.String() + "/approve" }, func() any {
			return map[string]any{"reason_code": "test"}
		}},
		{"reject_request", http.MethodPost, func(uuid.UUID) string { return base + "/requests/" + requestID.String() + "/reject" }, func() any {
			return map[string]any{"reason_code": "test"}
		}},
		{"list_grants", http.MethodGet, func(uuid.UUID) string { return base }, func() any { return nil }},
		{"revoke_grant", http.MethodPost, func(uuid.UUID) string { return base + "/" + grantID.String() + "/revoke" }, func() any {
			return map[string]any{"reason_code": "test"}
		}},
	}
}

// TestCapabilityAPI_A1_IneligibleRoleRefusedOnEveryRoute: support holds
// none of the four capability_grant:* permissions - every route must 403
// before any handler-specific logic runs.
func TestCapabilityAPI_A1_IneligibleRoleRefusedOnEveryRoute(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	supportStaff := a.staff(tenantID, "support")
	tok := a.token(supportStaff, tenantID, auth.RoleSupport)
	someID := uuid.New()

	for _, rc := range cgRoutes(tenantID, someID, someID, someID) {
		rc := rc
		t.Run(rc.name, func(t *testing.T) {
			resp := a.do(rc.method, rc.path(tenantID), tok, rc.body())
			if resp.status != http.StatusForbidden {
				t.Fatalf("%s: expected 403 for an ineligible role, got %d: %s", rc.name, resp.status, resp.body)
			}
		})
	}
}

// TestCapabilityAPI_A1_TenantAdmin_RequestCancelRevokeReadAllowed_ApproveRejectRefused
// is the G-T requester-side matrix: tenant_admin may request/cancel/
// revoke/read within its own tenant, and is refused (403, by the static
// permission map - it holds no capability_grant:approve) on approve/
// reject.
func TestCapabilityAPI_A1_TenantAdmin_RequestAllowed_ApproveRejectRefused(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	tenantAdmin := a.staff(tenantID, "tenant_admin")
	financeGrantee := a.staff(tenantID, "finance")
	tok := a.token(tenantAdmin, tenantID, auth.RoleTenantAdmin)

	// create_request: allowed (201).
	resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", tok, map[string]any{
		"grantee_staff_id": financeGrantee.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create_request: expected 201, got %d: %s", resp.status, resp.body)
	}
	var created struct {
		ID string `json:"id"`
	}
	resp.decode(t, &created)
	requestID, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatalf("parse created request id: %v", err)
	}

	// list_requests: allowed (200).
	if resp := a.do(http.MethodGet, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", tok, nil); resp.status != http.StatusOK {
		t.Fatalf("list_requests: expected 200, got %d: %s", resp.status, resp.body)
	}

	// approve_request: refused - tenant_admin holds no capability_grant:approve.
	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests/"+requestID.String()+"/approve", tok,
		map[string]any{"reason_code": "test"}); resp.status != http.StatusForbidden {
		t.Fatalf("approve_request: expected 403, got %d: %s", resp.status, resp.body)
	}
	// reject_request: refused, same reason.
	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests/"+requestID.String()+"/reject", tok,
		map[string]any{"reason_code": "test"}); resp.status != http.StatusForbidden {
		t.Fatalf("reject_request: expected 403, got %d: %s", resp.status, resp.body)
	}

	// cancel_request: allowed (204).
	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests/"+requestID.String()+"/cancel", tok, nil); resp.status != http.StatusNoContent {
		t.Fatalf("cancel_request: expected 204, got %d: %s", resp.status, resp.body)
	}
}

// TestCapabilityAPI_A1_PlatformAdmin_FullFlowAllowed is the G-T
// approver-side + revoke matrix: platform_admin may approve (and, per the
// static permission map, everything else too).
func TestCapabilityAPI_A1_PlatformAdmin_FullFlowAllowed(t *testing.T) {
	a := newCGAPI(t)
	tenantID := a.tenant()
	tenantAdmin := a.staff(tenantID, "tenant_admin")
	financeGrantee := a.staff(tenantID, "finance")
	platformPrincipal := a.staff(uuid.Nil, "platform_admin")
	tenantTok := a.token(tenantAdmin, tenantID, auth.RoleTenantAdmin)
	platformTok := a.token(platformPrincipal, uuid.Nil, auth.RolePlatformAdmin)

	resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests", tenantTok, map[string]any{
		"grantee_staff_id": financeGrantee.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create_request: expected 201, got %d: %s", resp.status, resp.body)
	}
	var created struct {
		ID string `json:"id"`
	}
	resp.decode(t, &created)
	requestID := created.ID

	// approve, as platform, on the platform path (tenant taken from path,
	// canActOnTenant admits any tenant for a platform-scoped caller).
	resp = a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/requests/"+requestID+"/approve", platformTok,
		map[string]any{"reason_code": "test"})
	if resp.status != http.StatusOK {
		t.Fatalf("approve_request: expected 200, got %d: %s", resp.status, resp.body)
	}
	var grant struct {
		ID string `json:"id"`
	}
	resp.decode(t, &grant)

	// list_grants: allowed for platform.
	if resp := a.do(http.MethodGet, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants", platformTok, nil); resp.status != http.StatusOK {
		t.Fatalf("list_grants: expected 200, got %d: %s", resp.status, resp.body)
	}

	// revoke, as platform.
	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantID.String()+"/capability-grants/"+grant.ID+"/revoke", platformTok,
		map[string]any{"reason_code": "test"}); resp.status != http.StatusNoContent {
		t.Fatalf("revoke_grant: expected 204, got %d: %s", resp.status, resp.body)
	}
}

// TestCapabilityAPI_A1_CrossTenantRefused: a tenant_admin of tenant A
// naming/reading tenant B's path is refused (403, no data) by
// canActOnTenant, before any DB call.
func TestCapabilityAPI_A1_CrossTenantRefused(t *testing.T) {
	a := newCGAPI(t)
	tenantA := a.tenant()
	tenantB := a.tenant()
	tenantAdminOfA := a.staff(tenantA, "tenant_admin")
	granteeInB := a.staff(tenantB, "finance")
	tok := a.token(tenantAdminOfA, tenantA, auth.RoleTenantAdmin)

	for _, rc := range cgRoutes(tenantB, granteeInB, uuid.New(), uuid.New()) {
		rc := rc
		t.Run(rc.name, func(t *testing.T) {
			resp := a.do(rc.method, rc.path(tenantB), tok, rc.body())
			if resp.status != http.StatusForbidden {
				t.Fatalf("%s: expected 403 for a cross-tenant attempt, got %d: %s", rc.name, resp.status, resp.body)
			}
		})
	}
}

// TestCapabilityAPI_K1C1_PlatformPathTenantMismatchGives404 (security
// review K1-C1 of 0f34d36 / F-1, F-5 of the code review): a platform
// token naming tenant A's path, acting on an id that actually belongs to
// tenant B, must 404 with no row change and no success audit - RLS alone
// (which admits every tenant to a platform session) is not the
// authorization boundary here; the id-plus-tenant filter in
// internal/capability's own queries is.
func TestCapabilityAPI_K1C1_PlatformPathTenantMismatchGives404(t *testing.T) {
	a := newCGAPI(t)
	tenantA := a.tenant()
	tenantB := a.tenant()
	tenantAdminOfB := a.staff(tenantB, "tenant_admin")
	financeInB := a.staff(tenantB, "finance")
	platformPrincipal := a.staff(uuid.Nil, "platform_admin")
	tenantBTok := a.token(tenantAdminOfB, tenantB, auth.RoleTenantAdmin)
	platformTok := a.token(platformPrincipal, uuid.Nil, auth.RolePlatformAdmin)

	// A real request that belongs to tenant B.
	resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantB.String()+"/capability-grants/requests", tenantBTok, map[string]any{
		"grantee_staff_id": financeInB.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("seed request: expected 201, got %d: %s", resp.status, resp.body)
	}
	var created struct {
		ID string `json:"id"`
	}
	resp.decode(t, &created)
	requestID := created.ID

	before := a.successAuditCount(decisionAuditAction["approve"])

	// A platform token names TENANT A's path, but the request id is B's -
	// the exact shape security's probe found gave 200 before the fix.
	resp = a.do(http.MethodPost, "/v1/admin/tenants/"+tenantA.String()+"/capability-grants/requests/"+requestID+"/approve", platformTok,
		map[string]any{"reason_code": "test"})
	if resp.status != http.StatusNotFound {
		t.Fatalf("approve via tenant A's path on tenant B's request: expected 404, got %d: %s", resp.status, resp.body)
	}
	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantA.String()+"/capability-grants/requests/"+requestID+"/reject", platformTok,
		map[string]any{"reason_code": "test"}); resp.status != http.StatusNotFound {
		t.Fatalf("reject via tenant A's path on tenant B's request: expected 404, got %d: %s", resp.status, resp.body)
	}
	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantA.String()+"/capability-grants/requests/"+requestID+"/cancel", platformTok, nil); resp.status != http.StatusNotFound {
		t.Fatalf("cancel via tenant A's path on tenant B's request: expected 404, got %d: %s", resp.status, resp.body)
	}

	// The request must still be pending and unrevoked in B, untouched by
	// any of the above.
	if n := a.requestCount(tenantB, financeInB); n != 1 {
		t.Fatalf("expected the original request to still be the only one in tenant B, got %d", n)
	}
	if after := a.successAuditCount(decisionAuditAction["approve"]); after != before {
		t.Fatalf("expected zero new success audit rows, before=%d after=%d", before, after)
	}

	// Now revoke: approve for real (via B's own path) to get a grant id,
	// then attempt to revoke it via A's path.
	resp = a.do(http.MethodPost, "/v1/admin/tenants/"+tenantB.String()+"/capability-grants/requests/"+requestID+"/approve", platformTok,
		map[string]any{"reason_code": "test"})
	if resp.status != http.StatusOK {
		t.Fatalf("approve via tenant B's own path: expected 200, got %d: %s", resp.status, resp.body)
	}
	var grant struct {
		ID string `json:"id"`
	}
	resp.decode(t, &grant)

	if resp := a.do(http.MethodPost, "/v1/admin/tenants/"+tenantA.String()+"/capability-grants/"+grant.ID+"/revoke", platformTok,
		map[string]any{"reason_code": "test"}); resp.status != http.StatusNotFound {
		t.Fatalf("revoke via tenant A's path on tenant B's grant: expected 404, got %d: %s", resp.status, resp.body)
	}

	// list_requests/list_grants via A's path must never include B's rows.
	resp = a.do(http.MethodGet, "/v1/admin/tenants/"+tenantA.String()+"/capability-grants/requests", platformTok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list_requests via A: expected 200, got %d: %s", resp.status, resp.body)
	}
	var reqList []map[string]any
	resp.decode(t, &reqList)
	if len(reqList) != 0 {
		t.Fatalf("expected zero requests listed under tenant A's path, got %d", len(reqList))
	}
	resp = a.do(http.MethodGet, "/v1/admin/tenants/"+tenantA.String()+"/capability-grants", platformTok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list_grants via A: expected 200, got %d: %s", resp.status, resp.body)
	}
	var grantList []map[string]any
	resp.decode(t, &grantList)
	if len(grantList) != 0 {
		t.Fatalf("expected zero grants listed under tenant A's path, got %d", len(grantList))
	}
}
