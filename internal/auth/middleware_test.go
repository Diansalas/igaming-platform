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

// TestRequireAnyPermission table-driven suite (Gate 10.3-W2/W3 code review
// finding #8): RequireAnyPermission had zero unit-test coverage even though
// it guards the provider-credential request-read routes
// (internal/httpserver/provider_credential_handlers.go). PermWithdrawalApprove
// is granted to RoleFinance ONLY and PermProviderConfigWrite is granted to
// RoleTenantAdmin ONLY (see TestRoleHasPermission_Stage3BWithdrawalAndProvider
// ConfigPermissions above) - no role holds both, and RoleSupport holds
// neither - which is exactly the fixture needed to prove "any of N", not
// "the first one", and not "all of them".
func TestRequireAnyPermission(t *testing.T) {
	handler := RequireAnyPermission(PermWithdrawalApprove, PermProviderConfigWrite)(okHandler())

	tests := []struct {
		name       string
		ctx        *tenant.Context
		wantStatus int
	}{
		{
			name: "principal holding only the first permission passes",
			ctx: &tenant.Context{
				TenantID: uuid.New(),
				Role:     string(RoleFinance), // holds PermWithdrawalApprove only
				Subject:  "staff-1",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "principal holding only the second permission passes",
			ctx: &tenant.Context{
				TenantID: uuid.New(),
				Role:     string(RoleTenantAdmin), // holds PermProviderConfigWrite only
				Subject:  "staff-2",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "principal holding neither permission is denied",
			ctx: &tenant.Context{
				TenantID: uuid.New(),
				Role:     string(RoleSupport), // holds neither
				Subject:  "staff-3",
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "unauthenticated request is denied",
			ctx:        nil,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/admin/provider-credentials/requests", nil)
			if tt.ctx != nil {
				req = req.WithContext(tenant.WithContext(req.Context(), *tt.ctx))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d (body: %s)", tt.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRequireAnyPermission_DenialMatchesRequirePermission proves the "no
// permission -> same denial status/body as RequirePermission" half of
// finding #8: a caller with neither listed permission must get an
// indistinguishable 403 (same code/message) from a single-permission check,
// so a client can't fingerprint "any" vs "single" checks from the response.
func TestRequireAnyPermission_DenialMatchesRequirePermission(t *testing.T) {
	anyHandler := RequireAnyPermission(PermWithdrawalApprove, PermProviderConfigWrite)(okHandler())
	singleHandler := RequirePermission(PermWithdrawalApprove)(okHandler())

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/x", nil)
		return req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
			TenantID: uuid.New(),
			Role:     string(RoleSupport), // holds neither permission either handler checks
			Subject:  "staff-1",
		}))
	}

	anyRec := httptest.NewRecorder()
	anyHandler.ServeHTTP(anyRec, newReq())

	singleRec := httptest.NewRecorder()
	singleHandler.ServeHTTP(singleRec, newReq())

	if anyRec.Code != singleRec.Code {
		t.Errorf("expected matching status codes, got RequireAnyPermission=%d RequirePermission=%d", anyRec.Code, singleRec.Code)
	}
	if anyRec.Body.String() != singleRec.Body.String() {
		t.Errorf("expected matching denial bodies, got RequireAnyPermission=%q RequirePermission=%q", anyRec.Body.String(), singleRec.Body.String())
	}
}

// TestRequireAnyPermission_IsAnyOfNotAllOf proves the "any-of" semantics
// directly: if RequireAnyPermission were accidentally rewritten to require
// ALL listed permissions (an "all-of" bug), this test would fail, because
// RoleFinance holds PermWithdrawalApprove but not
// PermRGRestrictionWrite (a permission it never holds - see
// TestRoleHasPermission_Stage4DRGRestrictionPermissions).
func TestRequireAnyPermission_IsAnyOfNotAllOf(t *testing.T) {
	handler := RequireAnyPermission(PermWithdrawalApprove, PermRGRestrictionWrite)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/x", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RoleFinance),
		Subject:  "staff-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200: RoleFinance holds one of the two listed permissions, an any-of check must pass; got %d (an all-of rewrite would produce 403)", rec.Code)
	}
}

// TestRequireAnyPermission_EmptyListDeniesEverything documents and proves
// the fail-closed behaviour finding #8 called out: RequireAnyPermission()
// with zero permissions must deny every authenticated caller, including
// roles that hold every other permission in the system (RolePlatformAdmin).
// The implementation already achieves this structurally - the for-range
// loop over an empty perms slice never runs, so every request falls through
// to the terminal "insufficient permissions" response - this test pins that
// behaviour so a future refactor (e.g. "if len(perms) == 0 { allow }")
// cannot silently regress it.
func TestRequireAnyPermission_EmptyListDeniesEverything(t *testing.T) {
	handler := RequireAnyPermission()(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/x", nil)
	req = req.WithContext(tenant.WithContext(req.Context(), tenant.Context{
		TenantID: uuid.New(),
		Role:     string(RolePlatformAdmin),
		Subject:  "admin-1",
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for an empty permission list (fail closed), got %d", rec.Code)
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
