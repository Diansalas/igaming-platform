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

// ADR 0088 §9.1/§14 (security review S5). RequireStaffPrincipal is the
// structural gate on the Stage 10 W1 sportsbook settlement-simulation
// route: it admits EXACTLY PrincipalStaff, mirroring
// TestRequirePlayerPrincipal_* in this package exactly, just for the
// opposite principal type.

func TestRequireStaffPrincipal_AllowsStaffToken(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue(uuid.NewString(), uuid.New(), RoleRiskManager, PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	handler := Middleware(issuer)(RequireStaffPrincipal(okHandler()))
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sportsbook/bets/"+uuid.NewString()+"/simulate-settlement-event", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a staff token, got %d", rec.Code)
	}
}

func TestRequireStaffPrincipal_DeniesPlayerPrincipal(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue(uuid.NewString(), uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	handler := Middleware(issuer)(RequireStaffPrincipal(next))

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sportsbook/bets/"+uuid.NewString()+"/simulate-settlement-event", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a player token, got %d", rec.Code)
	}
	if reached {
		t.Error("the staff handler must never run for a player token")
	}
	var body apierror.Error
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("expected the standard error envelope: %v", err)
	}
	if body.Code != apierror.CodeForbidden {
		t.Errorf("expected code %q, got %q", apierror.CodeForbidden, body.Code)
	}
}

func TestRequireStaffPrincipal_DeniesServicePrincipal(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue(uuid.NewString(), uuid.New(), RoleSupport, PrincipalService, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	handler := Middleware(issuer)(RequireStaffPrincipal(next))

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sportsbook/bets/"+uuid.NewString()+"/simulate-settlement-event", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a service-principal token, got %d", rec.Code)
	}
	if reached {
		t.Error("the staff handler must never run for a service-principal token")
	}
}

func TestRequireStaffPrincipal_DeniesWithoutAuthenticatedContext(t *testing.T) {
	var reached bool
	handler := RequireStaffPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sportsbook/bets/"+uuid.NewString()+"/simulate-settlement-event", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with no tenant context, got %d", rec.Code)
	}
	if reached {
		t.Error("the staff handler must never run without an authenticated context")
	}
}
