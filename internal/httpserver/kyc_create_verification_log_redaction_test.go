//go:build integration

// N3/N-2 (RV-PRH-I2 KYC code re-review / security re-verification):
// kyc.RedactedProviderErrorDetail is unit-tested directly in
// internal/kyc, but the handler-level log line
// (create_verification_provider_unavailable, kyc_handlers.go) that
// actually USES it had no test of its own proving the raw provider error
// text never reaches the log line end to end over HTTP. This closes that
// gap with a captured-logger assertion.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

const createVerificationLogRedactionSentinel = "VENDOR-TRANSPORT-SECRET-xyz789"

// failingCreateVerificationAdapter wraps the MOCK but always fails
// CreateVerification with an error carrying a sentinel string - standing
// in for a real adapter's own transport error, which can embed a vendor
// response body, header, or a credential-bearing URL.
type failingCreateVerificationAdapter struct{ *kyc.MockKYCProvider }

func (a failingCreateVerificationAdapter) CreateVerification(ctx context.Context, in kyc.CreateVerificationInput) (kyc.ProviderResult, error) {
	return kyc.ProviderResult{}, errors.New("POST https://vendor.example/verify?token=" + createVerificationLogRedactionSentinel)
}

func newCreateVerificationLogRedactionServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, logger *slog.Logger) *httptest.Server {
	t.Helper()
	adapter := failingCreateVerificationAdapter{kyc.NewMockKYCProvider()}
	srv := httptest.NewServer(New(Deps{
		Logger:                 logger,
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": adapter}, kyc.NewMockWebhookCredentials(adapter.MockKYCProvider)),
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestKYC_CreateVerificationProviderFailureLog_NeverLeaksRawErrorText is
// N3/N-2's own required test: a CreateVerification failure carrying raw
// adapter error text must never reach the
// "create_verification_provider_unavailable" log line - only
// kyc.RedactedProviderErrorDetail's bounded classification.
func TestKYC_CreateVerificationProviderFailureLog_NeverLeaksRawErrorText(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	srv := newCreateVerificationLogRedactionServer(t, pool, issuer, logger)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a CreateVerification provider failure, got %d", resp.StatusCode)
	}

	var lines []capturedLogLine
	for _, l := range captured() {
		if l.msg == "create_verification_provider_unavailable" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 create_verification_provider_unavailable line, got %d: %+v", len(lines), lines)
	}
	for k, v := range lines[0].attrs {
		if s, ok := v.(string); ok && strings.Contains(s, createVerificationLogRedactionSentinel) {
			t.Fatalf("create_verification_provider_unavailable field %q leaked raw provider error text: %q", k, s)
		}
	}
	// The wrapping fmt.Errorf("%w: ... %v", ErrProviderUnavailable, err) in
	// CreateVerification means errors.Is(err, ErrProviderUnavailable) holds,
	// so RedactedProviderErrorDetail correctly classifies this as
	// "provider unavailable" (not the default "internal error (redacted)"
	// branch) - the point of this test is that the SENTINEL never leaks
	// (checked above), not which specific closed class applies.
	detail, _ := lines[0].attrs["detail"].(string)
	if detail != "provider unavailable" {
		t.Fatalf(`expected detail="provider unavailable", got %q`, detail)
	}
}
