package httpserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// TestRLF4_AccessLogRedactsWebhookPath proves RL-F4 end to end through the
// REAL middleware chain New builds (requestIDMiddleware -> loggingMiddleware
// -> recoverMiddleware -> otelhttp -> mux), not just logPathFor in
// isolation - this is what actually proves the RequestState-pointer
// mechanism survives otelhttp's own request cloning (devops condition 3;
// see RequestState.LogPath's doc comment for why a naive r.Pattern read in
// loggingMiddleware would NOT have worked).
func TestRLF4_AccessLogRedactsWebhookPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/webhooks/payments/{tenantSlug}/{providerID}", func(w http.ResponseWriter, r *http.Request) {
		// Mirrors what the real handlers do: admitPreAuth is the first
		// call, and it marks RequestState.LogPath even when admission
		// itself is disabled (nil runtime).
		var rt *webhookAdmissionRuntime
		release, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { return true }, nil)
		if ok {
			release()
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	instrumented := otelhttp.NewHandler(mux, "test")
	handler := chain(instrumented, requestIDMiddleware, loggingMiddleware(logger), recoverMiddleware(logger))

	attackerSlug := "attacker-chosen-secret-looking-slug-" + strings.Repeat("x", 200)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/payments/"+attackerSlug+"/evilprovider", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var logged map[string]any
	// The LAST line is the access log (loggingMiddleware runs after the
	// handler returns).
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &logged); err != nil {
		t.Fatalf("log line not JSON: %v (%q)", err, buf.String())
	}
	path, _ := logged["path"].(string)
	if strings.Contains(path, attackerSlug) {
		t.Fatalf("access log leaked the attacker-chosen slug: %q", path)
	}
	if path != "POST /v1/webhooks/payments/{tenantSlug}/{providerID}" {
		t.Fatalf("access log path = %q, want the matched route pattern", path)
	}

	// A non-webhook route logs its real path unchanged.
	buf.Reset()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	var logged2 map[string]any
	lines2 := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if err := json.Unmarshal([]byte(lines2[len(lines2)-1]), &logged2); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if logged2["path"] != "/v1/me" {
		t.Fatalf("non-webhook route path = %v, want unchanged /v1/me", logged2["path"])
	}
}

// TestRLF4_PanicRecoveryRedactsWebhookPath is the panic-recovery half of
// devops condition 3.
func TestRLF4_PanicRecoveryRedactsWebhookPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/webhooks/casino/{tenantSlug}/{providerID}", func(w http.ResponseWriter, r *http.Request) {
		var rt *webhookAdmissionRuntime
		rt.admitPreAuth(w, r, domainCasino, 0, func(string) bool { return true }, nil)
		panic("boom")
	})

	instrumented := otelhttp.NewHandler(mux, "test")
	handler := chain(instrumented, requestIDMiddleware, loggingMiddleware(logger), recoverMiddleware(logger))

	attackerSlug := "panic-attacker-slug"
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/casino/"+attackerSlug+"/evilprovider", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"panic_recovered"`) {
		t.Fatalf("expected a panic_recovered log line, got %q", buf.String())
	}
	if strings.Contains(buf.String(), attackerSlug) {
		t.Fatalf("panic-recovery log leaked the attacker-chosen slug: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "POST /v1/webhooks/casino/{tenantSlug}/{providerID}") {
		t.Fatalf("panic-recovery log missing the matched route pattern: %q", buf.String())
	}
}
