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
	issuer := NewIssuer(testSecret, "platform-api-test")
	handler := Middleware(issuer)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_RejectsMalformedScheme(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	handler := Middleware(issuer)(okHandler())

	token, err := issuer.Issue("player-1", uuid.New(), RolePlayer, time.Hour)
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
	issuer := NewIssuer(testSecret, "platform-api-test")
	tenantID := uuid.New()
	token, err := issuer.Issue("player-1", tenantID, RolePlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	var gotTenantID uuid.UUID
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			t.Fatalf("expected tenant context to be attached, got error: %v", err)
		}
		gotTenantID = tc.TenantID
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
}

func TestMiddleware_RejectsInvalidToken(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	handler := Middleware(issuer)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant-config", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for an invalid token, got %d", rec.Code)
	}
}

func TestRequireRole_AllowsPermittedRole(t *testing.T) {
	handler := RequireRole(RolePlatformAdmin, RolePartnerAdmin)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/admin/thing", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RolePartnerAdmin),
		Subject:  "staff-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a permitted role, got %d", rec.Code)
	}
}

func TestRequireRole_DeniesUnpermittedRole(t *testing.T) {
	handler := RequireRole(RolePlatformAdmin)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/admin/thing", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RolePlayer),
		Subject:  "player-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for an unpermitted role, got %d", rec.Code)
	}
}

func TestRequireRole_DeniesWhenNoTenantContext(t *testing.T) {
	handler := RequireRole(RolePlayer)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/admin/thing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when no tenant context is present, got %d", rec.Code)
	}
}
