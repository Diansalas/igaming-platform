//go:build integration

// Stage 10.2 pre-fix evidence; retired or inverted by the fix commit.
//
// KYC-WH-1 (design doc §0, §B3, §H E3):
// TestKYCWH1_PreFix_UngatedWithTestSupportOff shows the KYC webhook route
// is reachable (non-404) even when test support is off, against CURRENT,
// UNMODIFIED production code.
//
// What this test actually proves, precisely: cmd/platform-api/main.go's
// KYC wiring (`kyc.NewOrchestrator(...)` at main.go:271-273, passed into
// `httpserver.Deps.KYCOrchestrator`) is NOT conditioned on
// `cfg.TestSupportRoutesEnabled()` at all - unlike the four sibling
// simulation flags in the very same Deps literal
// (CasinoPlaySimulationEnabled, PaymentsMockSettlementEnabled,
// AccountActivationTestSupportEnabled, SportsbookSettlementSimulationEnabled),
// which ARE each explicitly gated by `cfg.TestSupportRoutesEnabled()`.
// Because that wiring lives inside main()'s own function body (not an
// exported, independently callable function - unlike
// Config.TestSupportRoutesEnabled itself, which this test calls
// directly), this test cannot invoke main()'s wiring in-process. Instead
// it reproduces main()'s KYCOrchestrator wiring byte-for-byte at the
// httpserver.Deps level (the same unconditional
// `kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock":
// kyc.NewMockKYCProvider(secret)})` shape, using a runtime-random
// secret, never the repo's literal), builds a real config.Config with
// test support OFF, confirms `cfg.TestSupportRoutesEnabled() == false`
// for that config, and then shows the route still accepts and processes
// a request (non-404) with no reference anywhere to that cfg value. This
// demonstrates the internal/httpserver route-registration and handler
// code (registerKYCRoutes, newKYCWebhookHandler) has no gate on
// TestSupportRoutesEnabled or on deps.KYCOrchestrator's origin - the
// ONLY existing check is `deps.KYCOrchestrator == nil` (503) - so the
// real production binary, which always populates KYCOrchestrator
// unconditionally, serves this route in every environment regardless of
// TEST_SUPPORT_ENDPOINTS_ENABLED.
package httpserver

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func TestKYCWH1_PreFix_UngatedWithTestSupportOff(t *testing.T) {
	pool, issuer := testEnv(t)

	// A real config.Config, test support OFF ("staging" is a valid
	// non-production Environment value; TestSupportEndpointsEnabled left
	// at its zero value, false).
	cfg := config.Config{Environment: "staging", TestSupportEndpointsEnabled: false}
	if cfg.TestSupportRoutesEnabled() {
		t.Fatal("test setup bug: expected TestSupportRoutesEnabled() == false for this config")
	}

	// main.go's own KYC wiring, reproduced exactly (unconditional,
	// no reference to cfg.TestSupportRoutesEnabled() anywhere - see this
	// file's header comment). Uses a runtime-random secret; never the
	// repo's compile-time literal.
	secretBuf := make([]byte, 32)
	if _, err := rand.Read(secretBuf); err != nil {
		t.Fatalf("failed to generate runtime-random secret: %v", err)
	}
	kycOrchestrator := kyc.NewOrchestrator(map[string]kyc.KYCProvider{
		"mock": kyc.NewMockKYCProvider(hex.EncodeToString(secretBuf)),
	})

	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              pool,
		AuthIssuer:      issuer,
		ServiceName:     "platform-api-test",
		AccessTokenTTL:  5 * time.Minute,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(),
		KYCOrchestrator: kycOrchestrator,
		// Every TestSupportRoutesEnabled-gated flag left at its honest,
		// off value - deliberately NOT set from cfg, to make the point
		// unmissable: KYCOrchestrator above carries no such gate to set
		// in the first place.
		CasinoPlaySimulationEnabled:           cfg.TestSupportRoutesEnabled(),
		PaymentsMockSettlementEnabled:         cfg.TestSupportRoutesEnabled(),
		AccountActivationTestSupportEnabled:   cfg.TestSupportRoutesEnabled(),
		SportsbookSettlementSimulationEnabled: cfg.TestSupportRoutesEnabled(),
	}))
	t.Cleanup(srv.Close)

	tenant := mustCreateTenant(t, pool)

	// A garbage, unsigned body against the real "mock" provider id -
	// enough to prove the ROUTE and its handler run (reach signature
	// verification and fail there), never enough to forge anything.
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

	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("PRE-FIX EVIDENCE FAILED TO REPRODUCE: expected a non-404 status (route reachable) with test support off, got 404")
	}
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)
	t.Logf("PRE-FIX EVIDENCE: KYC webhook route reachable with TestSupportRoutesEnabled()==false: status=%d body=%s",
		resp.StatusCode, body.String())
}
