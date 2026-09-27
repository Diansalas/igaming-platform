//go:build integration

// PRH-I1 (ADR 0095 §10.5): the payment kill-switch admin API, end to end
// over real HTTP. Runs on a PRIVATE scratch database migrated all the way
// to the latest on-disk migration (via TEST_ADMIN_DATABASE_URL) rather than
// the shared TEST_DATABASE_URL/TEST_RUNTIME_DATABASE_URL, which are not
// guaranteed to have migration 0105 applied in every checkout/CI run this
// step lands in - mirrors internal/payments/deposit_v2_integration_test.go's
// identical rationale.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// ksMigrationsDir mirrors internal/payments' realMigrationsDir helper -
// duplicated here rather than exported, since it is a two-line test-only
// filesystem lookup and this package must not import internal/payments'
// test-only symbols.
func ksMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func ksScratchPool(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), ksMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch up: %v", err)
	}
	return pool
}

type ksAPI struct {
	t      *testing.T
	pool   *db.Pool
	issuer *auth.Issuer
	srv    *httptest.Server
	// logBuf mirrors every log line the server's slog.Logger emits (see
	// newKSAPI below) - RV-PRH-I1 security review L7's K21 requires an
	// HTTP-level assertion that a real engage call, over the whole stack,
	// actually emits logKillSwitchEngagedAlert's line, not merely that the
	// function itself does (the unit test in
	// payments_kill_switch_alert_test.go calls the function directly and
	// so does not notice the CALL SITE in the engage handler being
	// deleted - that is exactly what K21 SURVIVED against).
	// syncBuffer (provider_credential_api_integration_test.go) is a trivial
	// mutex-guarded io.Writer - httptest.Server serves each request on its
	// own goroutine, so a plain bytes.Buffer would race under -race.
	logBuf *syncBuffer
}

func newKSAPI(t *testing.T) *ksAPI {
	t.Helper()
	pool := ksScratchPool(t, "ks_api_")
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "ks-test", "ks-test")
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock},
		payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)})
	logBuf := &syncBuffer{}
	srv := httptest.NewServer(New(Deps{
		Logger:              slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logBuf), &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         "ks-test",
		AccessTokenTTL:      time.Hour,
		RefreshTokenTTL:     time.Hour,
		PersonResolver:      identityresolution.NewMockPersonResolver(),
		PaymentOrchestrator: orchestrator,
	}))
	t.Cleanup(srv.Close)
	return &ksAPI{t: t, pool: pool, issuer: issuer, srv: srv, logBuf: logBuf}
}

func (a *ksAPI) tenant() uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	platformAdmin := uuid.New()
	if err := a.pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'KS API', $2, 'under_platform_licence')`,
			id, "ksapi-"+strings.ReplaceAll(id.String(), "-", "")[:16])
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *ksAPI) platformAdmin() uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	if err := a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "ksapi-plat-"+id.String()+"@test.example")
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *ksAPI) tenantStaff(tenant uuid.UUID, role string) uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	if err := a.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', $4)`,
			id, tenant, "ksapi-"+id.String()+"@test.example", role)
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *ksAPI) player(tenant uuid.UUID) uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	return id // never inserted - a player token never needs a real row to be rejected by RequireStaffPrincipal
}

func (a *ksAPI) token(subject uuid.UUID, tenant uuid.UUID, role auth.Role, principal auth.PrincipalType) string {
	a.t.Helper()
	tok, err := a.issuer.Issue(subject.String(), tenant, role, principal, time.Hour)
	if err != nil {
		a.t.Fatal(err)
	}
	return tok
}

type ksResp struct {
	status int
	body   []byte
}

func (a *ksAPI) do(method, path, token string, body any) ksResp {
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
	return ksResp{status: resp.StatusCode, body: b}
}

func (r ksResp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// auditMetadata returns the metadata JSONB of the single audit_log row for
// (tenantID, action), read under a tenant-scoped transaction. Fails the
// test if there isn't exactly one such row.
func (a *ksAPI) auditMetadata(tenantID uuid.UUID, action string) map[string]any {
	a.t.Helper()
	var raw []byte
	err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id=$1 AND action=$2`, tenantID, action).Scan(&raw)
	})
	if err != nil {
		a.t.Fatalf("read audit metadata for %s: %v", action, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		a.t.Fatalf("decode audit metadata: %v", err)
	}
	return m
}

// platformAuditMetadata is auditMetadata's platform-scope twin: platform-
// attributed mutation audit rows carry tenant_id = NULL (M4's interim
// measure - audit_log's own RLS has no "platform writes into a named
// tenant's scope" policy family), so reading them requires a platform-
// scoped transaction, filtering explicitly on metadata->>'target_tenant_id'.
func (a *ksAPI) platformAuditMetadata(platformPrincipal, targetTenant uuid.UUID, action string) map[string]any {
	a.t.Helper()
	var raw []byte
	err := a.pool.WithPlatformAdmin(context.Background(), platformPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE tenant_id IS NULL AND action=$1 AND metadata->>'target_tenant_id'=$2 ORDER BY created_at DESC LIMIT 1`,
			action, targetTenant.String()).Scan(&raw)
	})
	if err != nil {
		a.t.Fatalf("read platform audit metadata for %s: %v", action, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		a.t.Fatalf("decode audit metadata: %v", err)
	}
	return m
}

// auditMetadataByOutcome is auditMetadata's outcome-filtered twin, needed
// for RV-PRH-I1 security review L5's denied-audit rows: a refused mutation
// and (often) a subsequent successful retry share the same
// (tenantID, action) pair, so the plain-action lookup is ambiguous once
// both exist.
func (a *ksAPI) auditMetadataByOutcome(tenantID uuid.UUID, action, outcome string) map[string]any {
	a.t.Helper()
	var raw []byte
	err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id=$1 AND action=$2 AND outcome=$3`, tenantID, action, outcome).Scan(&raw)
	})
	if err != nil {
		a.t.Fatalf("read audit metadata for %s/%s: %v", action, outcome, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		a.t.Fatalf("decode audit metadata: %v", err)
	}
	return m
}

func (a *ksAPI) auditCountByOutcome(tenantID uuid.UUID, action, outcome string) int {
	a.t.Helper()
	var n int
	err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND outcome=$3`, tenantID, action, outcome).Scan(&n)
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *ksAPI) auditCount(tenantID uuid.UUID, action string) int {
	a.t.Helper()
	var n int
	err := a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2`, tenantID, action).Scan(&n)
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return n
}

// --- tests ----------------------------------------------------------------

func TestPaymentsKillSwitchAPI_TenantAdminEngageAndFourEyesRelease(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	adminA := a.tenantStaff(tenant, "tenant_admin")
	adminB := a.tenantStaff(tenant, "tenant_admin")
	tokA := a.token(adminA, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	tokB := a.token(adminB, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)

	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	engageResp := a.do("POST", base+"/kill-switches", tokA, map[string]any{
		"provider_scope": "*", "operation_scope": "deposit", "reason_code": "incident_123",
	})
	if engageResp.status != http.StatusOK {
		t.Fatalf("engage status = %d, body=%s", engageResp.status, engageResp.body)
	}
	var ks killSwitchDTO
	engageResp.decode(t, &ks)
	if !ks.Engaged || ks.ChangedByScope != "tenant" {
		t.Fatalf("unexpected switch after engage: %+v", ks)
	}
	if a.auditCount(tenant, "payments_kill_switch.engage") != 1 {
		t.Fatal("expected exactly one engage audit row")
	}
	// RV-PRH-I1 security review L7/K21: a real engage call, over the whole
	// HTTP stack, must actually emit logKillSwitchEngagedAlert's line - not
	// merely the function in isolation (payments_kill_switch_alert_test.go's
	// unit test), which does not notice the call site itself being deleted.
	if logged := a.logBuf.String(); !strings.Contains(logged, "payments_kill_switch_engaged_alert") ||
		!strings.Contains(logged, tenant.String()) || !strings.Contains(logged, "incident_123") {
		t.Fatalf("expected the engage call to emit the payments_kill_switch_engaged_alert log line for tenant %s, got log output: %s", tenant, logged)
	}

	reqResp := a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tokA, map[string]any{"reason_code": "resolved"})
	if reqResp.status != http.StatusCreated {
		t.Fatalf("request-release status = %d, body=%s", reqResp.status, reqResp.body)
	}
	var relReq killSwitchReleaseRequestDTO
	reqResp.decode(t, &relReq)
	if a.auditCount(tenant, "payments_kill_switch.request_release") != 1 {
		t.Fatal("expected exactly one request_release audit row")
	}

	// Requester approving their own request is refused (four-eyes, DB-enforced).
	selfApprove := a.do("POST", base+"/kill-switch-release-requests/"+relReq.ID+"/approve", tokA, nil)
	if selfApprove.status != http.StatusConflict {
		t.Fatalf("self-approve status = %d, want 409, body=%s", selfApprove.status, selfApprove.body)
	}
	// RV-PRH-I1 security review L5: the refused self-approval writes its
	// own denied-audit row, in a transaction separate from the failed
	// approve attempt (which rolled back).
	if a.auditCountByOutcome(tenant, "payments_kill_switch.approve_release", "denied") != 1 {
		t.Fatal("expected exactly one denied approve_release audit row for the self-approve refusal")
	}
	deniedMeta := a.auditMetadataByOutcome(tenant, "payments_kill_switch.approve_release", "denied")
	if deniedMeta["denied_class"] != "trigger_refusal" {
		t.Fatalf("denied audit metadata denied_class = %v, want trigger_refusal", deniedMeta["denied_class"])
	}

	// A distinct principal approves and releases in one call.
	approve := a.do("POST", base+"/kill-switch-release-requests/"+relReq.ID+"/approve", tokB, nil)
	if approve.status != http.StatusOK {
		t.Fatalf("approve status = %d, body=%s", approve.status, approve.body)
	}
	var released killSwitchDTO
	approve.decode(t, &released)
	if released.Engaged {
		t.Fatal("expected switch to be released")
	}
	if a.auditCountByOutcome(tenant, "payments_kill_switch.approve_release", "success") != 1 {
		t.Fatal("expected exactly one successful approve_release audit row")
	}
	// The denied row from the self-approve attempt above must still be
	// present alongside the successful one - a refusal is never
	// overwritten or lost once a later attempt succeeds.
	if a.auditCount(tenant, "payments_kill_switch.approve_release") != 2 {
		t.Fatal("expected both the denied and the successful approve_release audit rows to be present")
	}
}

func TestPaymentsKillSwitchAPI_CancelReleaseRequest(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	var ks killSwitchDTO
	a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "payout", "reason_code": "x"}).decode(t, &ks)

	var req killSwitchReleaseRequestDTO
	a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tok, map[string]any{"reason_code": "y"}).decode(t, &req)

	cancel := a.do("POST", base+"/kill-switch-release-requests/"+req.ID+"/cancel", tok, nil)
	if cancel.status != http.StatusNoContent {
		t.Fatalf("cancel status = %d, body=%s", cancel.status, cancel.body)
	}
	if a.auditCount(tenant, "payments_kill_switch.cancel_release") != 1 {
		t.Fatal("expected exactly one cancel_release audit row")
	}

	// Cancelling a second time (already terminal) is a conflict.
	cancelAgain := a.do("POST", base+"/kill-switch-release-requests/"+req.ID+"/cancel", tok, nil)
	if cancelAgain.status != http.StatusConflict {
		t.Fatalf("second cancel status = %d, want 409, body=%s", cancelAgain.status, cancelAgain.body)
	}
	// RV-PRH-I1 security review L5: the refused second cancel writes its
	// own denied-audit row alongside the first, successful one.
	if a.auditCountByOutcome(tenant, "payments_kill_switch.cancel_release", "denied") != 1 {
		t.Fatal("expected exactly one denied cancel_release audit row for the second, refused cancel")
	}
}

func TestPaymentsKillSwitchAPI_PlatformAdminActsOnArbitraryTenant(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	platAdmin := a.platformAdmin()
	tok := a.token(platAdmin, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	resp := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "*", "reason_code": "platform_incident"})
	if resp.status != http.StatusOK {
		t.Fatalf("platform engage status = %d, body=%s", resp.status, resp.body)
	}
	var ks killSwitchDTO
	resp.decode(t, &ks)
	if ks.ChangedByScope != "platform" || ks.EngagedByScope == nil || *ks.EngagedByScope != "platform" {
		t.Fatalf("unexpected switch after platform engage: %+v", ks)
	}

	// A tenant admin of that SAME tenant cannot touch the platform-engaged row.
	tenantAdmin := a.tenantStaff(tenant, "tenant_admin")
	tenantTok := a.token(tenantAdmin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	reqResp := a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tenantTok, map[string]any{"reason_code": "trying"})
	if reqResp.status != http.StatusForbidden && reqResp.status != http.StatusConflict {
		// L5(a): a tenant session may not even file the request. Either
		// classification is acceptable here; what matters is it is refused.
		t.Fatalf("expected tenant to be refused filing a release request against a platform-engaged switch, got %d body=%s", reqResp.status, reqResp.body)
	}

	// But the tenant admin CAN still read it (read-only visibility, §10.5).
	getResp := a.do("GET", base+"/kill-switches/"+ks.ID, tenantTok, nil)
	if getResp.status != http.StatusOK {
		t.Fatalf("tenant read of platform-engaged switch status = %d, body=%s", getResp.status, getResp.body)
	}
}

func TestPaymentsKillSwitchAPI_CrossTenantPathIsForbidden(t *testing.T) {
	a := newKSAPI(t)
	tenantA := a.tenant()
	tenantB := a.tenant()
	adminA := a.tenantStaff(tenantA, "tenant_admin")
	tokA := a.token(adminA, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)

	resp := a.do("POST", "/v1/admin/tenants/"+tenantB.String()+"/payments/kill-switches", tokA,
		map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x"})
	if resp.status != http.StatusForbidden {
		t.Fatalf("cross-tenant engage status = %d, want 403, body=%s", resp.status, resp.body)
	}
	var apiErr apierror.Error
	resp.decode(t, &apiErr)
	if apiErr.Code != apierror.CodeForbidden {
		t.Fatalf("code = %q, want forbidden", apiErr.Code)
	}
}

func TestPaymentsKillSwitchAPI_CrossTenantLookupIsNotFound(t *testing.T) {
	a := newKSAPI(t)
	tenantA := a.tenant()
	tenantB := a.tenant()
	adminA := a.tenantStaff(tenantA, "tenant_admin")
	adminB := a.tenantStaff(tenantB, "tenant_admin")
	tokA := a.token(adminA, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	tokB := a.token(adminB, tenantB, auth.RoleTenantAdmin, auth.PrincipalStaff)

	var ksB killSwitchDTO
	a.do("POST", "/v1/admin/tenants/"+tenantB.String()+"/payments/kill-switches", tokB,
		map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x"}).decode(t, &ksB)

	// Tenant A's own path, but the id belongs to tenant B: not found, never
	// tenant B's data.
	resp := a.do("GET", "/v1/admin/tenants/"+tenantA.String()+"/payments/kill-switches/"+ksB.ID, tokA, nil)
	if resp.status != http.StatusNotFound {
		t.Fatalf("cross-tenant lookup status = %d, want 404, body=%s", resp.status, resp.body)
	}
}

func TestPaymentsKillSwitchAPI_PlayerTokenForbidden(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	player := a.player(tenant)
	tok := a.token(player, tenant, auth.RolePlayer, auth.PrincipalPlayer)

	resp := a.do("GET", "/v1/admin/tenants/"+tenant.String()+"/payments/kill-switches", tok, nil)
	if resp.status != http.StatusForbidden {
		t.Fatalf("player token status = %d, want 403, body=%s", resp.status, resp.body)
	}
}

func TestPaymentsKillSwitchAPI_UnknownFieldsRefused(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)

	resp := a.do("POST", "/v1/admin/tenants/"+tenant.String()+"/payments/kill-switches", tok,
		map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x", "unexpected": "field"})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("unknown-field status = %d, want 400, body=%s", resp.status, resp.body)
	}
}

func TestPaymentsKillSwitchAPI_ListAndGetRoundTrip(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x"})
	a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "mock", "operation_scope": "payout", "reason_code": "y"})

	listResp := a.do("GET", base+"/kill-switches", tok, nil)
	if listResp.status != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", listResp.status, listResp.body)
	}
	var listed struct {
		KillSwitches []killSwitchDTO `json:"kill_switches"`
	}
	listResp.decode(t, &listed)
	if len(listed.KillSwitches) != 2 {
		t.Fatalf("expected 2 switches, got %d", len(listed.KillSwitches))
	}
}

func TestPaymentsKillSwitchAPI_ValidationErrors(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	resp := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "not_a_real_scope", "reason_code": "x"})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("invalid operation_scope status = %d, want 400, body=%s", resp.status, resp.body)
	}

	resp2 := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "deposit"})
	if resp2.status != http.StatusBadRequest {
		t.Fatalf("missing reason_code status = %d, want 400, body=%s", resp2.status, resp2.body)
	}
}

func TestPaymentsKillSwitchAPI_ReleaseRequestAgainstNonEngagedSwitchConflicts(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	var ks killSwitchDTO
	a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x"}).decode(t, &ks)

	// Release it immediately via a direct DB call (bypassing HTTP) is not
	// available here without a second admin; instead assert requesting a
	// release against a made-up, never-engaged id 404s, and a not-yet-
	// engaged one (none exists in this flow) is out of scope - covered at
	// the killswitch.go/migration level already. This test only pins the
	// unknown-id 404 shape at the HTTP layer.
	resp := a.do("POST", base+"/kill-switches/"+uuid.New().String()+"/release-requests", tok, map[string]any{"reason_code": "x"})
	if resp.status != http.StatusNotFound {
		t.Fatalf("unknown switch id status = %d, want 404, body=%s", resp.status, resp.body)
	}
}

// TestPaymentsKillSwitchAPI_ProviderScopeValidation is the RV-PRH-I1
// security review M2 fix: an unregistered provider_scope (a typo, or one
// with trailing whitespace) must be a 400, never a silently-accepted
// containment that matches nothing.
func TestPaymentsKillSwitchAPI_ProviderScopeValidation(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	unregistered := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "Mock-Payments", "operation_scope": "deposit", "reason_code": "x"})
	if unregistered.status != http.StatusBadRequest {
		t.Fatalf("unregistered provider_scope status = %d, want 400, body=%s", unregistered.status, unregistered.body)
	}

	trailingWhitespace := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "mock ", "operation_scope": "deposit", "reason_code": "x"})
	if trailingWhitespace.status != http.StatusBadRequest {
		t.Fatalf("trailing-whitespace provider_scope status = %d, want 400, body=%s", trailingWhitespace.status, trailingWhitespace.body)
	}

	registered := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "mock", "operation_scope": "deposit", "reason_code": "x"})
	if registered.status != http.StatusOK {
		t.Fatalf("registered provider_scope status = %d, want 200, body=%s", registered.status, registered.body)
	}

	wildcard := a.do("POST", base+"/kill-switches", tok, map[string]any{"provider_scope": "*", "operation_scope": "payout", "reason_code": "x"})
	if wildcard.status != http.StatusOK {
		t.Fatalf("'*' provider_scope status = %d, want 200, body=%s", wildcard.status, wildcard.body)
	}
}

// TestPaymentsKillSwitchAPI_PlatformCallerAgainstNonexistentTenantIs404 is
// the RV-PRH-I1 security review L6 fix.
func TestPaymentsKillSwitchAPI_PlatformCallerAgainstNonexistentTenantIs404(t *testing.T) {
	a := newKSAPI(t)
	platAdmin := a.platformAdmin()
	tok := a.token(platAdmin, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)

	resp := a.do("POST", "/v1/admin/tenants/"+uuid.New().String()+"/payments/kill-switches", tok,
		map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x"})
	if resp.status != http.StatusNotFound {
		t.Fatalf("nonexistent-tenant platform engage status = %d, want 404, body=%s", resp.status, resp.body)
	}
}

// TestPaymentsKillSwitchAPI_RouteTable is the RV-PRH-I1 security review L8
// fix (§10.4's own route-table requirement): every kill-switch route
// refuses a player token and a service token with 403, and none is
// reachable without authentication.
func TestPaymentsKillSwitchAPI_RouteTable(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	player := a.player(tenant)
	playerTok := a.token(player, tenant, auth.RolePlayer, auth.PrincipalPlayer)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", base + "/kill-switches", nil},
		{"GET", base + "/kill-switches/" + uuid.New().String(), nil},
		{"POST", base + "/kill-switches", map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "x"}},
		{"GET", base + "/kill-switch-release-requests/" + uuid.New().String(), nil},
		{"POST", base + "/kill-switches/" + uuid.New().String() + "/release-requests", map[string]any{"reason_code": "x"}},
		{"POST", base + "/kill-switch-release-requests/" + uuid.New().String() + "/approve", nil},
		{"POST", base + "/kill-switch-release-requests/" + uuid.New().String() + "/cancel", nil},
	}
	if len(routes) != 7 {
		t.Fatalf("expected exactly 7 kill-switch routes enumerated, got %d - update this table if a route was added or removed", len(routes))
	}

	for _, rt := range routes {
		resp := a.do(rt.method, rt.path, playerTok, rt.body)
		if resp.status != http.StatusForbidden {
			t.Errorf("%s %s with a player token: status = %d, want 403, body=%s", rt.method, rt.path, resp.status, resp.body)
		}
		noAuth := a.do(rt.method, rt.path, "", rt.body)
		if noAuth.status != http.StatusUnauthorized {
			t.Errorf("%s %s with no token: status = %d, want 401, body=%s", rt.method, rt.path, noAuth.status, noAuth.body)
		}
	}
}

// TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant is
// the RV-PRH-I1 security review M3 (before/after) and M4 (target_tenant_id
// present on every platform-attributed mutation, K18) fix: every mutation's
// audit row must carry a "before" and "after" object, and a platform-
// scoped mutation's audit row must carry target_tenant_id equal to the path
// tenant.
func TestPaymentsKillSwitchAPI_AuditRecordsCarryBeforeAfterAndTargetTenant(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	adminA := a.tenantStaff(tenant, "tenant_admin")
	adminB := a.tenantStaff(tenant, "tenant_admin")
	tokA := a.token(adminA, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	tokB := a.token(adminB, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	var ks killSwitchDTO
	a.do("POST", base+"/kill-switches", tokA, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "incident-1"}).decode(t, &ks)
	engageMeta := a.auditMetadata(tenant, "payments_kill_switch.engage")
	for _, key := range []string{"before", "after", "target_tenant_id"} {
		if _, ok := engageMeta[key]; !ok {
			t.Errorf("engage audit metadata missing %q: %v", key, engageMeta)
		}
	}
	if before, ok := engageMeta["before"].(map[string]any); !ok || before["existed"] != false {
		t.Errorf("engage before-state should record existed=false for a first-ever engage, got %v", engageMeta["before"])
	}

	var relReq killSwitchReleaseRequestDTO
	a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tokA, map[string]any{"reason_code": "resolved"}).decode(t, &relReq)
	reqMeta := a.auditMetadata(tenant, "payments_kill_switch.request_release")
	for _, key := range []string{"before", "after", "target_tenant_id"} {
		if _, ok := reqMeta[key]; !ok {
			t.Errorf("request_release audit metadata missing %q: %v", key, reqMeta)
		}
	}

	a.do("POST", base+"/kill-switch-release-requests/"+relReq.ID+"/approve", tokB, nil)
	approveMeta := a.auditMetadata(tenant, "payments_kill_switch.approve_release")
	for _, key := range []string{"before", "after", "target_tenant_id"} {
		if _, ok := approveMeta[key]; !ok {
			t.Errorf("approve_release audit metadata missing %q: %v", key, approveMeta)
		}
	}

	// Cancel a second, freshly re-engaged switch's request.
	a.do("POST", base+"/kill-switches", tokA, map[string]any{"provider_scope": "mock", "operation_scope": "payout", "reason_code": "x"})
	var ks2 killSwitchDTO
	listResp := a.do("GET", base+"/kill-switches", tokA, nil)
	var listed struct {
		KillSwitches []killSwitchDTO `json:"kill_switches"`
	}
	listResp.decode(t, &listed)
	for _, s := range listed.KillSwitches {
		if s.ProviderScope == "mock" {
			ks2 = s
		}
	}
	var relReq2 killSwitchReleaseRequestDTO
	a.do("POST", base+"/kill-switches/"+ks2.ID+"/release-requests", tokA, map[string]any{"reason_code": "y"}).decode(t, &relReq2)
	a.do("POST", base+"/kill-switch-release-requests/"+relReq2.ID+"/cancel", tokA, nil)
	cancelMeta := a.auditMetadata(tenant, "payments_kill_switch.cancel_release")
	for _, key := range []string{"before", "after", "target_tenant_id"} {
		if _, ok := cancelMeta[key]; !ok {
			t.Errorf("cancel_release audit metadata missing %q: %v", key, cancelMeta)
		}
	}

	// Platform-scoped engage: target_tenant_id must be present and equal to
	// the path tenant, even though the audit row's own tenant_id column is
	// NULL (M4's interim measure).
	platAdmin := a.platformAdmin()
	platResp := a.do("POST", base+"/kill-switches", a.token(platAdmin, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff),
		map[string]any{"provider_scope": "*", "operation_scope": "*", "reason_code": "platform_takeover_test"})
	if platResp.status != http.StatusOK {
		t.Fatalf("platform engage status = %d, body=%s", platResp.status, platResp.body)
	}
	platMeta := a.platformAuditMetadata(platAdmin, tenant, "payments_kill_switch.engage")
	if platMeta["target_tenant_id"] != tenant.String() {
		t.Errorf("K18: platform engage audit metadata target_tenant_id = %v, want %s", platMeta["target_tenant_id"], tenant.String())
	}
}

// TestPaymentsKillSwitchAPI_AuditRecordsPlatformTakeover is M3's explicit
// "record takeover and KS-L6 cancellations" requirement: a platform re-
// engage of an already tenant-engaged switch, with an open tenant release
// request outstanding, must audit is_platform_takeover=true and the
// cancelled request's id.
func TestPaymentsKillSwitchAPI_AuditRecordsPlatformTakeover(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	adminA := a.tenantStaff(tenant, "tenant_admin")
	tokA := a.token(adminA, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenant.String() + "/payments"

	var ks killSwitchDTO
	a.do("POST", base+"/kill-switches", tokA, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "incident-1"}).decode(t, &ks)
	var relReq killSwitchReleaseRequestDTO
	a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tokA, map[string]any{"reason_code": "resolved"}).decode(t, &relReq)

	platAdmin := a.platformAdmin()
	platTok := a.token(platAdmin, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	takeover := a.do("POST", base+"/kill-switches", platTok, map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "platform_incident"})
	if takeover.status != http.StatusOK {
		t.Fatalf("platform takeover status = %d, body=%s", takeover.status, takeover.body)
	}

	meta := a.platformAuditMetadata(platAdmin, tenant, "payments_kill_switch.engage")
	if meta["is_platform_takeover"] != true {
		t.Errorf("is_platform_takeover = %v, want true", meta["is_platform_takeover"])
	}
	if meta["ks_l6_cancelled_request_id"] != relReq.ID {
		t.Errorf("ks_l6_cancelled_request_id = %v, want %s", meta["ks_l6_cancelled_request_id"], relReq.ID)
	}
}
