//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// --- test fixtures, created directly via internal/identity rather than
// HTTP, so each test's setup doesn't depend on the very endpoints other
// tests are exercising ---

func mustCreateTenant(t *testing.T, pool *db.Pool) identity.Tenant {
	t.Helper()
	suffix := uuid.NewString()
	var tenant identity.Tenant
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tenant, err = identity.CreateTenant(ctx, tx, "Test Tenant", "t-"+suffix, "under_platform_licence")
		return err
	})
	if err != nil {
		t.Fatalf("failed to create tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant.ID)
			return err
		})
	})
	return tenant
}

func mustCreateBrand(t *testing.T, pool *db.Pool, tenant identity.Tenant) identity.Brand {
	t.Helper()
	suffix := uuid.NewString()
	var brand identity.Brand
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		brand, err = identity.CreateBrand(ctx, tx, tenant.ID, "Test Brand", "b-"+suffix)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create brand: %v", err)
	}
	return brand
}

func mustCreateStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID, role identity.StaffRole, password string) identity.StaffUser {
	t.Helper()
	suffix := uuid.NewString()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	var staff identity.StaffUser
	scope := pool.WithoutTenant
	if tenantID != uuid.Nil {
		scope = func(ctx context.Context, fn db.TxFunc) error { return pool.WithTenant(ctx, tenantID, fn) }
	}
	err = scope(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staff, err = identity.CreateStaffUser(ctx, tx, tenantID, fmt.Sprintf("staff-%s@test.com", suffix), hash, role, nil)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create staff user: %v", err)
	}
	return staff
}

func postJSON(t *testing.T, srv *httptest.Server, path, bearerToken string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func getJSON(t *testing.T, srv *httptest.Server, path, bearerToken string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func deleteRequest(t *testing.T, srv *httptest.Server, path, bearerToken string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
}

// --- Full player lifecycle: register -> me -> login -> refresh rotation
// -> reuse detection -> logout ---

func TestPlayerLifecycle_EndToEnd(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	email := "player-" + uuid.NewString() + "@example.com"

	// Register
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 registering, got %d", resp.StatusCode)
	}
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatal("expected both access and refresh tokens on registration")
	}

	// Duplicate registration is rejected
	resp = postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 for duplicate registration, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET /v1/me with the access token
	resp = getJSON(t, srv, "/v1/me", tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /v1/me, got %d", resp.StatusCode)
	}
	var me meResponse
	decodeBody(t, resp, &me)
	if me.Email != email {
		t.Errorf("expected email %q, got %q", email, me.Email)
	}

	// Login with correct credentials
	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 logging in, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Login with wrong password
	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "wrong-password",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong password, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Refresh rotates the token
	resp = postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": tokens.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 refreshing, got %d", resp.StatusCode)
	}
	var rotated tokenPairResponse
	decodeBody(t, resp, &rotated)
	if rotated.RefreshToken == tokens.RefreshToken {
		t.Error("expected rotation to produce a NEW refresh token, got the same one back")
	}

	// Reusing the OLD (now-rotated) refresh token must be rejected
	resp = postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": tokens.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 reusing an already-rotated refresh token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Reuse detection must have revoked the WHOLE chain, including the
	// token issued by the rotation above.
	resp = postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": rotated.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 - reuse detection should have revoked the whole chain, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Logout on a fresh session, then confirm the refresh token no longer works.
	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	var loginTokens tokenPairResponse
	decodeBody(t, resp, &loginTokens)

	resp = postJSON(t, srv, "/v1/auth/logout", "", map[string]string{"refresh_token": loginTokens.RefreshToken})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 logging out, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": loginTokens.RefreshToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 refreshing a logged-out session, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLogin_LockoutAfterRepeatedFailures(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	email := "lockout-" + uuid.NewString() + "@example.com"

	// Register the account (so we have a real one to be locked out of).
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "correct-password-1",
	})
	resp.Body.Close()

	for i := 0; i < 5; i++ {
		resp := postJSON(t, srv, "/v1/auth/login", "", map[string]string{
			"brand_slug": brand.Slug, "email": email, "password": "wrong",
		})
		resp.Body.Close()
	}

	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "wrong",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 after threshold failures, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Even the CORRECT password must be refused while locked out.
	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "correct-password-1",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 for correct password while locked out, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Staff login and RBAC ---

func TestStaffLogin_PlatformAdminOmitsTenantSlug(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-password-1")

	resp := postJSON(t, srv, "/v1/staff/auth/login", "", map[string]string{
		"email": admin.Email, "password": "admin-password-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for platform admin login, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestStaffLogin_TenantScopedRequiresTenantSlug(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-password-1")

	// Without tenant_slug, staff login resolves to the platform-wide
	// (WithoutTenant) scope, where this tenant-scoped row is invisible.
	resp := postJSON(t, srv, "/v1/staff/auth/login", "", map[string]string{
		"email": staff.Email, "password": "ta-password-1",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without tenant_slug for a tenant-scoped staff user, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/staff/auth/login", "", map[string]string{
		"tenant_slug": tenant.Slug, "email": staff.Email, "password": "ta-password-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with the correct tenant_slug, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func mustLoginStaff(t *testing.T, srv *httptest.Server, tenantSlug, email, password string) tokenPairResponse {
	t.Helper()
	body := map[string]string{"email": email, "password": password}
	if tenantSlug != "" {
		body["tenant_slug"] = tenantSlug
	}
	resp := postJSON(t, srv, "/v1/staff/auth/login", "", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("staff login failed with status %d", resp.StatusCode)
	}
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)
	return tokens
}

// TestRBAC_RoleDistinctionEnforced is the end-to-end proof that
// permission-based RBAC actually gates the admin endpoints: support can
// read players but not suspend them; tenant_admin can do both;
// platform_admin (nil-tenant) is denied entirely by RequireTenantScope.
func TestRBAC_RoleDistinctionEnforced(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, "rbac-test@example.com", "hash")
		playerID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-pw-1")
	adminTokens := mustLoginStaff(t, srv, "", admin.Email, "admin-pw-1")

	support := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleSupport, "support-pw-1")
	supportTokens := mustLoginStaff(t, srv, tenant.Slug, support.Email, "support-pw-1")

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-pw-1")

	// platform_admin (nil tenant) is denied by RequireTenantScope, before
	// RequirePermission even runs - see docs/decisions/0011.
	resp := getJSON(t, srv, "/v1/admin/players", adminTokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for platform_admin on a tenant-scoped endpoint, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// support can read...
	resp = getJSON(t, srv, "/v1/admin/players", supportTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for support reading players, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// ...but not suspend.
	resp = postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/suspend", supportTokens.AccessToken, map[string]string{"reason": "test"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for support attempting to suspend, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// tenant_admin can suspend.
	resp = postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/suspend", tenantAdminTokens.AccessToken, map[string]string{"reason": "confirmed abuse"})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 for tenant_admin suspending, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/players/"+playerID.String(), tenantAdminTokens.AccessToken)
	var playerResp playerAccountResponse
	decodeBody(t, resp, &playerResp)
	if playerResp.Status != string(identity.PlayerStatusSuspended) {
		t.Errorf("expected player status 'suspended', got %q", playerResp.Status)
	}
}

func TestCrossTenantPlayerAccess_Denied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)

	var playerAID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandA, "cross-tenant@example.com", "hash")
		playerAID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	staffB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "staffb-pw-1")
	tokensB := mustLoginStaff(t, srv, tenantB.Slug, staffB.Email, "staffb-pw-1")

	// Tenant B's admin must not be able to read tenant A's player, even
	// with the exact id.
	resp := getJSON(t, srv, "/v1/admin/players/"+playerAID.String(), tokensB.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for cross-tenant player read (RLS-filtered, not a data leak), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Nor suspend it.
	resp = postJSON(t, srv, "/v1/admin/players/"+playerAID.String()+"/suspend", tokensB.AccessToken, map[string]string{"reason": "malicious attempt"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for cross-tenant suspend attempt, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Tenant/brand provisioning (platform_admin only) ---

func TestCreateTenantAndBrand_PlatformAdminOnly(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-pw-2")
	adminTokens := mustLoginStaff(t, srv, "", admin.Email, "admin-pw-2")

	tenant := mustCreateTenant(t, pool)
	staffOfTenant := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-pw-2")
	staffTokens := mustLoginStaff(t, srv, tenant.Slug, staffOfTenant.Email, "ta-pw-2")

	// tenant_admin does not have tenant:write.
	resp := postJSON(t, srv, "/v1/admin/tenants", staffTokens.AccessToken, map[string]string{
		"name": "Should Fail", "slug": "should-fail-" + uuid.NewString(), "licensing_model": "own_licence",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin creating a tenant, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// platform_admin can.
	slug := "new-tenant-" + uuid.NewString()
	resp = postJSON(t, srv, "/v1/admin/tenants", adminTokens.AccessToken, map[string]string{
		"name": "New Tenant", "slug": slug, "licensing_model": "own_licence",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for platform_admin creating a tenant, got %d", resp.StatusCode)
	}
	var newTenant tenantResponse
	decodeBody(t, resp, &newTenant)

	// tenant_admin of a DIFFERENT tenant cannot create a brand under this new tenant.
	resp = postJSON(t, srv, "/v1/admin/tenants/"+newTenant.ID+"/brands", staffTokens.AccessToken, map[string]string{
		"name": "Intruder Brand", "slug": "intruder-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a tenant_admin creating a brand under a different tenant, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// platform_admin can create a brand under any tenant.
	resp = postJSON(t, srv, "/v1/admin/tenants/"+newTenant.ID+"/brands", adminTokens.AccessToken, map[string]string{
		"name": "New Brand", "slug": "new-brand-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201 for platform_admin creating a brand, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Session self-service: listing and ownership-checked revocation ---

func TestSessionManagement_ListAndRevoke(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	// Player A registers (one active session) and logs in again (a
	// second active session on the same account).
	emailA := "session-a-" + uuid.NewString() + "@example.com"
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": emailA, "password": "a-decent-password-1",
	})
	var tokensA tokenPairResponse
	decodeBody(t, resp, &tokensA)

	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": emailA, "password": "a-decent-password-1",
	})
	var tokensA2 tokenPairResponse
	decodeBody(t, resp, &tokensA2)

	// Player B, a different account entirely (same tenant/brand), also
	// has an active session.
	emailB := "session-b-" + uuid.NewString() + "@example.com"
	resp = postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": emailB, "password": "b-decent-password-1",
	})
	var tokensB tokenPairResponse
	decodeBody(t, resp, &tokensB)

	// Player A lists their own sessions: exactly the two above, not B's.
	resp = getJSON(t, srv, "/v1/me/sessions", tokensA.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing sessions, got %d", resp.StatusCode)
	}
	var sessionsA []sessionResponse
	decodeBody(t, resp, &sessionsA)
	if len(sessionsA) != 2 {
		t.Fatalf("expected player A to see exactly 2 active sessions, got %d", len(sessionsA))
	}

	resp = getJSON(t, srv, "/v1/me/sessions", tokensB.AccessToken)
	var sessionsB []sessionResponse
	decodeBody(t, resp, &sessionsB)
	if len(sessionsB) != 1 {
		t.Fatalf("expected player B to see exactly 1 active session, got %d", len(sessionsB))
	}

	// Explicit cross-principal read check: none of A's session ids may
	// appear in B's list, and vice versa - not just a count coincidence.
	aIDs := map[string]bool{}
	for _, s := range sessionsA {
		aIDs[s.ID] = true
	}
	for _, s := range sessionsB {
		if aIDs[s.ID] {
			t.Errorf("player B's session list contained player A's session id %s", s.ID)
		}
	}

	// IDOR check: player B must NOT be able to revoke player A's session
	// by id, even though both are in the same tenant and B can legally
	// list their own sessions via the same endpoint shape.
	resp = deleteRequest(t, srv, "/v1/me/sessions/"+sessionsA[0].ID, tokensB.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for player B revoking player A's session (ownership must be enforced, not just tenant scope), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Confirm the IDOR attempt did not actually revoke it: A's refresh
	// token from that same session still works.
	resp = postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": tokensA.RefreshToken})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected player A's session to survive B's failed revoke attempt, got %d refreshing", resp.StatusCode)
	}
	resp.Body.Close()

	// Player A can revoke their OWN second session (ownership is checked
	// by principal, not by which of A's own tokens makes the request).
	resp = deleteRequest(t, srv, "/v1/me/sessions/"+sessionsA[1].ID, tokensA.AccessToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 for player A revoking their own session, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Revoking an already-revoked (or nonexistent) session id is a 404,
	// not a 500 or a 204 - it must not silently "succeed" twice.
	resp = deleteRequest(t, srv, "/v1/me/sessions/"+uuid.NewString(), tokensA.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 revoking a nonexistent session id, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Audit trail: mutating actions actually write a row, not just a log line ---

func TestAuditLog_RecordsSecurityEvents(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-audit-pw-1")
	adminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-audit-pw-1")

	email := "audit-test-" + uuid.NewString() + "@example.com"
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)

	resp = getJSON(t, srv, "/v1/me", tokens.AccessToken)
	var me meResponse
	decodeBody(t, resp, &me)

	// A failed login (wrong password) must also be audited.
	resp = postJSON(t, srv, "/v1/auth/login", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "wrong-password",
	})
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/admin/players/"+me.ID+"/suspend", adminTokens.AccessToken, map[string]string{"reason": "audit test"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 suspending player, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/audit-log", adminTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading audit log, got %d", resp.StatusCode)
	}
	var entries []auditEntryResponse
	decodeBody(t, resp, &entries)

	byAction := map[string][]auditEntryResponse{}
	for _, e := range entries {
		byAction[e.Action] = append(byAction[e.Action], e)
	}

	requireEntryFor := func(action string) auditEntryResponse {
		t.Helper()
		for _, e := range byAction[action] {
			if e.TargetID == me.ID {
				return e
			}
		}
		t.Fatalf("expected an audit_log entry for action %q targeting player %s, found none among %d entries", action, me.ID, len(entries))
		return auditEntryResponse{}
	}

	registered := requireEntryFor("player.registered")
	if registered.Outcome != "success" {
		t.Errorf("expected player.registered outcome success, got %q", registered.Outcome)
	}

	suspended := requireEntryFor("player.suspended")
	if suspended.Outcome != "success" {
		t.Errorf("expected player.suspended outcome success, got %q", suspended.Outcome)
	}
	if suspended.ActorID != tenantAdmin.ID.String() {
		t.Errorf("expected player.suspended actor to be the tenant_admin who performed it, got %q", suspended.ActorID)
	}

	found := false
	for _, e := range byAction["player.login_failed"] {
		found = true
		if e.Outcome != "failure" {
			t.Errorf("expected player.login_failed outcome failure, got %q", e.Outcome)
		}
	}
	if !found {
		t.Error("expected at least one player.login_failed audit entry for the wrong-password attempt")
	}
}

// --- Refresh rotation under concurrency: the reuse-detection race fix ---

// TestRefreshRotation_ConcurrentRequestsRaceSafely proves the RotateSession
// conditional-UPDATE fix: firing two concurrent refreshes of the exact same
// refresh token must result in exactly one success. Before the fix, both
// could observe "not yet replaced" and both succeed, producing two live
// chains from one token with reuse detection never triggering.
func TestRefreshRotation_ConcurrentRequestsRaceSafely(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	email := "race-" + uuid.NewString() + "@example.com"
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)

	const concurrency = 8
	var wg sync.WaitGroup
	statusCodes := make([]int, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": tokens.RefreshToken})
			statusCodes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, code := range statusCodes {
		if code == http.StatusOK {
			successCount++
		} else if code != http.StatusUnauthorized {
			t.Errorf("unexpected status code from concurrent refresh: %d", code)
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly 1 of %d concurrent refreshes of the same token to succeed, got %d", concurrency, successCount)
	}
}

// TestStaffLogin_LockoutHoldsAcrossEmailCaseVariants is the regression
// test for a Stage 2 security review finding: the lockout identifier was
// built from the raw request email while the actual lookup normalized it
// (lowercase + trim), so Admin@x.com / ADMIN@X.COM / " admin@x.com" each
// got their own login_attempts bucket - lockout never tripped no matter
// how many wrong-password attempts were made, as long as each used a
// different case/whitespace variant of the same address.
func TestStaffLogin_LockoutHoldsAcrossEmailCaseVariants(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "case-pw-1")

	variants := []string{
		staff.Email,
		strings.ToUpper(staff.Email),
		"  " + staff.Email + "  ",
		strings.ToUpper(staff.Email[:1]) + staff.Email[1:],
		staff.Email,
	}
	for _, email := range variants {
		resp := postJSON(t, srv, "/v1/staff/auth/login", "", map[string]string{
			"tenant_slug": tenant.Slug, "email": email, "password": "wrong",
		})
		resp.Body.Close()
	}

	// A 6th attempt, in yet another case variant, must already be locked
	// out - if the case/whitespace variants above each got their own
	// bucket, this would incorrectly return 401 (fresh bucket) instead of
	// 429 (shared bucket, threshold reached).
	resp := postJSON(t, srv, "/v1/staff/auth/login", "", map[string]string{
		"tenant_slug": tenant.Slug, "email": strings.ToUpper(staff.Email), "password": "wrong",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 - lockout must hold across case/whitespace variants of the same email, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Even the correct password, in the original casing, must be refused
	// while locked out.
	resp = postJSON(t, srv, "/v1/staff/auth/login", "", map[string]string{
		"tenant_slug": tenant.Slug, "email": staff.Email, "password": "case-pw-1",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 for correct password while locked out, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
