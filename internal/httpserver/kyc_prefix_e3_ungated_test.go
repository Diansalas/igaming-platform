//go:build integration

// Stage 10.2 KYC-WH-1 fix verification: K11 (test-support-off leg), direct
// descendant of the Stage 10.2 pre-fix evidence E3
// (TestKYCWH1_PreFix_UngatedWithTestSupportOff, retired by this inversion
// per design §H).
//
// TestKYCWebhook_TestSupportOff_404 proves the KYC webhook route is now
// ABSENT (a genuine 404 from the mux, not a 503 from inside a registered
// handler) whenever Deps.KYCWebhookEnabled is false - exactly the
// condition cmd/platform-api/wiring.go's mockProviderWiring ties to
// cfg.TestSupportRoutesEnabled() (K11's cmd/platform-api leg, in
// kyc_webhook_integration_test.go, proves main's ACTUAL wiring value
// follows that function; this test proves internal/httpserver's route
// registration honors whatever value it is given).
package httpserver

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func TestKYCWebhook_TestSupportOff_404(t *testing.T) {
	pool, issuer := testEnv(t)

	// A fully-wired KYCOrchestrator (mirrors what production actually
	// constructs when kycOrchestrator/mockProviderWiring decide test
	// support is off - see cmd/platform-api/wiring_test.go's K11 leg for
	// that decision itself) - deliberately NOT nil here, to isolate what
	// THIS test actually proves: KYCWebhookEnabled alone, independent of
	// whether an orchestrator happens to be wired, must gate the route.
	mockProvider := kyc.NewMockKYCProvider()
	srv := httptest.NewServer(New(Deps{
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                pool,
		AuthIssuer:        issuer,
		ServiceName:       "platform-api-test",
		AccessTokenTTL:    5 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		PersonResolver:    identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:   kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mockProvider}, kyc.NewMockWebhookCredentials(mockProvider)),
		KYCWebhookEnabled: false, // the fix: test support off means this route does not exist.
	}))
	t.Cleanup(srv.Close)

	tenant := mustCreateTenant(t, pool)

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/webhooks/kyc/%s/mock", srv.URL, tenant.Slug),
		bytes.NewReader([]byte(`not-a-signed-payload`)))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 (route absent) with KYCWebhookEnabled=false, got %d", resp.StatusCode)
	}
}

// TestKYCWebhook_TestSupportOff_NilOrchestrator_404 covers the OTHER half
// of the K11 matrix: KYCWebhookEnabled=false is exactly the case
// mockProviderWiring also leaves KYCOrchestrator nil for (kycOrchestrator
// returns a true nil) - registerKYCRoutes must still 404, not panic or
// register a handler that then immediately 503s.
func TestKYCWebhook_TestSupportOff_NilOrchestrator_404(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              pool,
		AuthIssuer:      issuer,
		ServiceName:     "platform-api-test",
		AccessTokenTTL:  5 * time.Minute,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(),
		// KYCOrchestrator and KYCWebhookEnabled both left at their zero
		// values, exactly like a real production Deps built with test
		// support off (main.go's kycOrchestrator(wiring, ...) returns nil).
	}))
	t.Cleanup(srv.Close)

	tenant := mustCreateTenant(t, pool)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/webhooks/kyc/%s/mock", srv.URL, tenant.Slug),
		bytes.NewReader([]byte(`not-a-signed-payload`)))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 (route absent) with a nil KYCOrchestrator/KYCWebhookEnabled, got %d", resp.StatusCode)
	}

	// K16 (identity-compliance ruling J13): player self-service KYC itself
	// is unavailable (503, not 404) when the orchestrator is nil.
	player := mustRegisterPlayer(t, srv, mustCreateBrand(t, pool, tenant).Slug)
	createResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("K16: expected 503 creating a verification with a nil KYCOrchestrator, got %d", createResp.StatusCode)
	}
}
