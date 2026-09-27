package httpserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRLF4_OrchestratorDisabled_StillRedactsPath is the security review
// Low "RL-F4 leftover": a request that short-circuits BEFORE admitPreAuth
// ever runs (the "webhooks not enabled on this deployment" 503, checked
// first in every one of the three webhook handlers) must still have its
// access-log path redacted - this branch is reachable with arbitrary
// attacker-chosen path segments regardless of whether any orchestrator is
// wired up.
func TestRLF4_OrchestratorDisabled_StillRedactsPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	// New Deps has PaymentOrchestrator/CasinoOrchestrator both nil - every
	// one of the three webhook routes hits its own "not enabled" 503
	// before admitPreAuth is ever called. New(deps) ALREADY wraps the full
	// production middleware chain (requestIDMiddleware -> loggingMiddleware
	// -> recoverMiddleware -> otelhttp) - it must be used directly, not
	// wrapped a second time (a second requestIDMiddleware would attach a
	// SEPARATE RequestState that the inner handler never touches).
	handler := New(Deps{Logger: logger, ServiceName: "test"})

	attackerSlug := "attacker-chosen-slug-when-orchestrator-disabled"
	req := httptest.NewRequest("POST", "/v1/webhooks/payments/"+attackerSlug+"/evilprovider", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var logged map[string]any
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &logged); err != nil {
		t.Fatalf("log line not JSON: %v (%q)", err, buf.String())
	}
	path, _ := logged["path"].(string)
	if strings.Contains(path, attackerSlug) {
		t.Fatalf("access log leaked the attacker-chosen slug even though the orchestrator was disabled: %q", path)
	}
	if path != "POST /v1/webhooks/payments/{tenantSlug}/{providerID}" {
		t.Fatalf("access log path = %q, want the matched route pattern", path)
	}
}
