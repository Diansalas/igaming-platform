//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	tenant, err := identity.CreateTenant(context.Background(), pool, "Test Tenant", "t-"+suffix, "under_platform_licence")
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
		staff, err = identity.CreateStaffUser(ctx, tx, tenantID, fmt.Sprintf("staff-%s@test.com", suffix), hash, role)
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
