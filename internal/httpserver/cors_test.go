package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newCORSTestHandler(allowed []string) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return corsMiddleware(allowed)(inner)
}

func TestCORSMiddleware_EmptyAllowlistIsNoOp(t *testing.T) {
	h := newCORSTestHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Origin", "https://staging.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("expected no CORS header with empty allowlist, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected request to pass through, got status %d", rec.Code)
	}
}

func TestCORSMiddleware_NoOriginHeaderIsPassthrough(t *testing.T) {
	h := newCORSTestHandler([]string{"https://staging.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("expected no CORS header for same-origin request, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected request to pass through, got status %d", rec.Code)
	}
}

func TestCORSMiddleware_AllowlistedOriginGetsHeader(t *testing.T) {
	h := newCORSTestHandler([]string{"https://staging.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Origin", "https://staging.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://staging.example.com" {
		t.Errorf("expected Access-Control-Allow-Origin to reflect the allowlisted origin, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("expected no Access-Control-Allow-Credentials header (Bearer-token-only auth), got %q", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected request to reach the inner handler, got status %d", rec.Code)
	}
}

func TestCORSMiddleware_NonAllowlistedOriginGetsNoHeaderButStillServed(t *testing.T) {
	h := newCORSTestHandler([]string{"https://staging.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no Access-Control-Allow-Origin for a non-allowlisted origin, got %q", got)
	}
	// This is not an authorization control: the inner handler (real
	// server-side auth) still runs. A browser enforces the block; a
	// non-browser caller ignores Origin/CORS entirely regardless.
	if rec.Code != http.StatusOK {
		t.Errorf("expected the request to still reach the inner handler (CORS is not an authz gate), got status %d", rec.Code)
	}
}

func TestCORSMiddleware_PreflightShortCircuits(t *testing.T) {
	h := newCORSTestHandler([]string{"https://staging.example.com"})

	req := httptest.NewRequest(http.MethodOptions, "/v1/auth/login", nil)
	req.Header.Set("Origin", "https://staging.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected preflight to short-circuit with 204, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty preflight body, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("expected Access-Control-Allow-Methods on preflight response")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("expected Access-Control-Allow-Headers on preflight response")
	}
}

func TestCORSMiddleware_VaryOriginAlwaysSetWhenConfigured(t *testing.T) {
	h := newCORSTestHandler([]string{"https://staging.example.com"})

	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	found := false
	for _, v := range rec.Header().Values("Vary") {
		if v == "Origin" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Vary: Origin even for a non-allowlisted origin, got %v", rec.Header().Values("Vary"))
	}
}
