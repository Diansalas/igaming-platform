package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
)

// Stage 9 §8. RequirePlayerPrincipal is the structural gate on the player
// self-service surface (/v1/me/..., /v1/bonus/...), which carries no
// RequirePermission gate by design. Before it existed, a STAFF bearer
// token reached every one of those handlers and was stopped only
// INCIDENTALLY, by each handler's own player_accounts lookup happening to
// miss. These tests pin the gate itself, so a future /v1/me handler that
// forgets to do that lookup is not silently exploitable.

func TestRequirePlayerPrincipal_AllowsPlayerToken(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue(uuid.NewString(), uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	handler := Middleware(issuer)(RequirePlayerPrincipal(okHandler()))
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a player token, got %d", rec.Code)
	}
}

// Every staff role, not just one: a role-by-role table makes it
// impossible to "fix" this by special-casing a single role later.
func TestRequirePlayerPrincipal_DeniesEveryStaffRole(t *testing.T) {
	issuer := testIssuer(t)
	roles := []Role{
		RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleCompliance,
		RoleFinance, RoleRiskManager, RolePromotionsManager, RoleBonusOperations,
	}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			tenantID := uuid.New()
			if role == RolePlatformAdmin {
				tenantID = uuid.Nil // platform-scoped principal, ADR 0011
			}
			token, err := issuer.Issue(uuid.NewString(), tenantID, role, PrincipalStaff, time.Hour)
			if err != nil {
				t.Fatalf("unexpected error issuing token: %v", err)
			}

			var reached bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})
			handler := Middleware(issuer)(RequirePlayerPrincipal(next))

			req := httptest.NewRequest(http.MethodGet, "/v1/me/wallets", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("expected 403 for a %s staff token, got %d", role, rec.Code)
			}
			if reached {
				t.Error("the player handler must never run for a staff token")
			}
			var body apierror.Error
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("expected the standard error envelope: %v", err)
			}
			if body.Code != apierror.CodeForbidden {
				t.Errorf("expected code %q, got %q", apierror.CodeForbidden, body.Code)
			}
		})
	}
}

// A service principal is not a player either. Nothing issues one today,
// which is exactly why this needs a test: the first service token minted
// must not silently inherit the player self-service surface.
func TestRequirePlayerPrincipal_DeniesServicePrincipal(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue(uuid.NewString(), uuid.New(), RoleSupport, PrincipalService, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	handler := Middleware(issuer)(RequirePlayerPrincipal(okHandler()))
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a service-principal token, got %d", rec.Code)
	}
}

// Fails closed with no authenticated context at all - i.e. if a future
// route registration ever forgets to put Middleware in front of it, the
// result is a 401, never a pass-through.
func TestRequirePlayerPrincipal_DeniesWithoutAuthenticatedContext(t *testing.T) {
	var reached bool
	handler := RequirePlayerPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with no tenant context, got %d", rec.Code)
	}
	if reached {
		t.Error("the player handler must never run without an authenticated context")
	}
}
