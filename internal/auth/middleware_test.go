package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddleware_RejectsMissingAuthHeader(t *testing.T) {
	issuer := testIssuer(t)
	handler := Middleware(issuer)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_RejectsMalformedScheme(t *testing.T) {
	issuer := testIssuer(t)
	handler := Middleware(issuer)(okHandler())

	token, err := issuer.Issue("player-1", uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	req.Header.Set("Authorization", "Basic "+token) // wrong scheme
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a non-Bearer scheme, got %d", rec.Code)
	}
}

func TestMiddleware_AcceptsCaseInsensitiveBearerScheme(t *testing.T) {
	issuer := testIssuer(t)
	tenantID := uuid.New()
	token, err := issuer.Issue("player-1", tenantID, RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	var gotTenantID uuid.UUID
	var gotPrincipalType string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			t.Fatalf("expected tenant context to be attached, got error: %v", err)
		}
		gotTenantID = tc.TenantID
		gotPrincipalType = tc.PrincipalType
		w.WriteHeader(http.StatusOK)
	})
	handler := Middleware(issuer)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	req.Header.Set("Authorization", "bearer "+token) // lowercase scheme, RFC 7235 case-insensitive
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotTenantID != tenantID {
		t.Errorf("expected tenant id %s attached to context, got %s", tenantID, gotTenantID)
	}
	if gotPrincipalType != string(PrincipalPlayer) {
		t.Errorf("expected principal type %q attached to context, got %q", PrincipalPlayer, gotPrincipalType)
	}
}

func TestMiddleware_AttachesNilTenantForPlatformScopedToken(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue("admin-1", uuid.Nil, RolePlatformAdmin, PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	var gotTenantID uuid.UUID
	var contextErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, err := tenant.FromContext(r.Context())
		contextErr = err
		gotTenantID = tc.TenantID
		w.WriteHeader(http.StatusOK)
	})
	handler := Middleware(issuer)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/thing", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if contextErr != nil {
		t.Fatalf("expected tenant context to be attached even for a nil-tenant token, got error: %v", contextErr)
	}
	if gotTenantID != uuid.Nil {
		t.Errorf("expected nil tenant id, got %s", gotTenantID)
	}
}

func TestMiddleware_RejectsInvalidToken(t *testing.T) {
	issuer := testIssuer(t)
	handler := Middleware(issuer)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for an invalid token, got %d", rec.Code)
	}
}

func TestRequirePermission_AllowsRoleWithPermission(t *testing.T) {
	handler := RequirePermission(PermPlayerRead)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/players", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RoleSupport),
		Subject:  "staff-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a role with the required permission, got %d", rec.Code)
	}
}

func TestRequirePermission_DeniesRoleWithoutPermission(t *testing.T) {
	handler := RequirePermission(PermPlayerSuspend)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/players/1/suspend", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RoleSupport), // support can read players but not suspend them
		Subject:  "staff-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a role missing the required permission, got %d", rec.Code)
	}
}

func TestRequirePermission_DeniesWhenNoTenantContext(t *testing.T) {
	handler := RequirePermission(PermPlayerRead)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/players", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when no tenant context is present, got %d", rec.Code)
	}
}

func TestRequirePermission_UnknownRoleIsDeniedByDefault(t *testing.T) {
	handler := RequirePermission(PermPlayerRead)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/players", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     "some_made_up_role",
		Subject:  "staff-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for an unrecognized role (fail closed), got %d", rec.Code)
	}
}

func TestRequireTenantScope_AllowsNonNilTenant(t *testing.T) {
	handler := RequireTenantScope(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RoleTenantAdmin),
		Subject:  "staff-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a tenant-scoped caller, got %d", rec.Code)
	}
}

func TestRequireTenantScope_DeniesNilTenant(t *testing.T) {
	handler := RequireTenantScope(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.Nil,
		Role:     string(RolePlatformAdmin),
		Subject:  "admin-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a platform-scoped (nil-tenant) caller, got %d", rec.Code)
	}
}

func TestRequireTenantScope_DeniesWhenNoTenantContext(t *testing.T) {
	handler := RequireTenantScope(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when no tenant context is present, got %d", rec.Code)
	}
}
