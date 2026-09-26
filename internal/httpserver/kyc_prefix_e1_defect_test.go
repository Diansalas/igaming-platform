//go:build integration

// Stage 10.2 pre-fix evidence; retired or inverted by the fix commit.
//
// KYC-WH-1 (design doc §0, §H E1): TestKYCWH1_PreFix_PlayerSelfApprovalForgeSucceeds
// reproduces the end-to-end exploit against CURRENT, UNMODIFIED production
// code at HEAD: a player creates their own verification, reads their own
// provider_reference from the ordinary player-facing response, forges a
// callback body with crypto/hmac using the KYC mock provider's own
// webhookSecret, and POSTs it to their OWN tenant's webhook route. This
// does not use, copy, or reference the literal compile-time constant at
// cmd/platform-api/main.go:43 (kycMockWebhookSecret) - that constant is
// never read or reproduced here. Instead this test generates its OWN
// runtime-random secret (crypto/rand) and wires it into a
// kyc.NewMockKYCProvider it builds itself, standing in for "the public
// compile-time constant" (any party with read access to the (public)
// source tree - including any ordinary player - already knows its exact
// value in the real deployment; the test cannot use that literal value,
// but the defect is that the SAME single secret is shared by the mock
// provider instance and is discoverable by construction rather than
// derived per tenant/caller, which a runtime-random stand-in exercises
// identically).
//
// Expected pre-fix (design §H, table row E1): 200, status "approved",
// one kyc.provider_callback audit row written - i.e. the forgery
// succeeds completely, using only information already available to the
// legitimate player who owns the verification.
package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// kycE1RandomSecret returns a fresh, runtime-random hex string standing in
// for "the mock provider's shared webhook secret" - never the repo's
// actual compile-time literal (which this file deliberately never reads).
func kycE1RandomSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("failed to generate runtime-random secret: %v", err)
	}
	return hex.EncodeToString(buf)
}

func TestKYCWH1_PreFix_PlayerSelfApprovalForgeSucceeds(t *testing.T) {
	pool, issuer := testEnv(t)

	// The "public constant" stand-in: generated once per test run, never
	// derived from or compared against cmd/platform-api/main.go's actual
	// literal. It plays the exact structural role that literal plays in
	// production - a single fixed value baked into the running mock
	// provider instance.
	secret := kycE1RandomSecret(t)
	mockProvider := kyc.NewMockKYCProvider(secret)

	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              pool,
		AuthIssuer:      issuer,
		ServiceName:     "platform-api-test",
		AccessTokenTTL:  5 * time.Minute,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(),
		KYCOrchestrator: kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mockProvider}),
	}))
	t.Cleanup(srv.Close)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	// Step 1-2: the player creates their own verification through the
	// ordinary player route and reads their own provider_reference back -
	// nothing here requires any privilege beyond "is an authenticated
	// player", exactly as a real attacker would have.
	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating own verification, got %d", resp.StatusCode)
	}
	var created verificationResponse
	decodeBody(t, resp, &created)
	if created.ProviderReference == "" {
		t.Fatal("player's own verification-creation response carried no provider_reference to forge with")
	}
	if created.Status != string(kyc.StatusPending) && created.Status != string(kyc.StatusUnverified) {
		t.Fatalf("unexpected initial verification status %q", created.Status)
	}

	// Step 3: the player HMACs {"provider_reference":...,"outcome":
	// "approved"} with the (stand-in) shared secret - exactly the
	// attacker action in design §0's exploit narrative. This uses only
	// crypto/hmac, the reference the player already legitimately holds,
	// and the shared secret every instance of the mock uses - never any
	// server-side introspection.
	forgedBody, _ := json.Marshal(map[string]string{
		"provider_reference": created.ProviderReference,
		"outcome":            "approved",
		"reason":             "self-approved-by-player",
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(forgedBody)
	signature := hex.EncodeToString(mac.Sum(nil))
	wireBody := kyc.MockSignedCallbackBody(forgedBody, signature)

	// Step 4: POST to their own tenant's webhook route - the real,
	// unauthenticated (no bearer token) provider-facing route. The route
	// resolves the TENANT by its own slug (identity.GetTenantBySlug,
	// internal/httpserver/kyc_admin_handlers.go), not the brand slug.
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/webhooks/kyc/%s/mock", srv.URL, tenant.Slug),
		bytes.NewReader(wireBody))
	if err != nil {
		t.Fatalf("failed to build forged webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	webhookResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forged webhook request failed: %v", err)
	}
	defer webhookResp.Body.Close()

	// Step 5: the pre-fix expectation (design §H E1): 200, approved.
	if webhookResp.StatusCode != http.StatusOK {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(webhookResp.Body)
		t.Fatalf("PRE-FIX EVIDENCE: expected the forged self-approval to succeed with 200 (design §H E1), got %d: %s",
			webhookResp.StatusCode, body.String())
	}
	var result verificationResponse
	decodeBody(t, webhookResp, &result)
	if result.Status != string(kyc.StatusApproved) {
		t.Fatalf("PRE-FIX EVIDENCE: expected forged callback to move verification to 'approved', got %q", result.Status)
	}
	t.Logf("PRE-FIX EVIDENCE: player-forged self-approval succeeded: verification %s status=%s", result.ID, result.Status)

	// Confirm the audit row design §0 says is written even for a forged,
	// self-approved callback (audit.Record runs before the terminal
	// check in internal/kyc/provider.go's ReceiveCallback).
	var auditCount int
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'kyc.provider_callback' AND target_id = $1`,
			result.ID).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("failed to query audit_log: %v", err)
	}
	if auditCount < 1 {
		t.Fatalf("PRE-FIX EVIDENCE: expected at least one kyc.provider_callback audit row for the forged approval, got %d", auditCount)
	}
	t.Logf("PRE-FIX EVIDENCE: %d kyc.provider_callback audit row(s) written for the forged approval", auditCount)
}
