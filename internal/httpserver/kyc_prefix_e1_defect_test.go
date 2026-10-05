//go:build integration

// Stage 10.2 KYC-WH-1 fix verification: K1, direct descendant of the
// Stage 10.2 pre-fix evidence E1
// (TestKYCWH1_PreFix_PlayerSelfApprovalForgeSucceeds, retired by this
// inversion per design §H).
//
// TestKYCWebhook_PlayerComputedSignature_Rejected reproduces the IDENTICAL
// exploit narrative against the FIXED code: a player creates their own
// verification, reads their own provider_reference (via a tenant-scoped DB
// helper now that the player-facing response no longer carries it - K10),
// forges a callback body/signature using ONLY information a player could
// ever have (crypto/hmac over the shared MOCK wire scheme with a
// runtime-random stand-in secret, never the real per-tenant derived key -
// which a player never has access to, unlike the pre-fix single global
// constant), and POSTs it to their OWN tenant's webhook route.
//
// Expected post-fix (design §H K1): 401 "callback rejected", the
// verification's status is UNCHANGED, and no kyc.provider_callback audit
// row is written for the forged attempt - the full no-effect checklist.
package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// kycE1RandomSecret returns a fresh, runtime-random hex string - a player
// forging a callback has no way to learn the real per-tenant derived key
// (webhookauth.DeriveMockKey, seeded from a per-process crypto/rand
// master never exposed outside the orchestrator/resolver), so this stands
// in for "whatever a determined attacker might guess or brute-force",
// which must still fail.
func kycE1RandomSecret(t *testing.T) []byte {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("failed to generate runtime-random secret: %v", err)
	}
	return buf
}

func TestKYCWebhook_PlayerComputedSignature_Rejected(t *testing.T) {
	pool, issuer := testEnv(t)
	mockProvider := kyc.NewMockKYCProvider()

	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        newKYCOrchestratorWithWorker(t, pool, map[string]kyc.KYCProvider{"mock": mockProvider}, kyc.NewMockWebhookCredentials(mockProvider)),
		KYCWebhookEnabled:      true,
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	// Step 1-2: the player creates their own verification through the
	// ordinary player route. K10: the response carries no
	// provider_reference at all now - the reference is read via a
	// tenant-scoped DB helper, standing in for "whatever a player might
	// otherwise have learned" (e.g. from an out-of-band document-upload
	// flow), never from the player-facing JSON.
	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating own verification, got %d", resp.StatusCode)
	}
	var created playerVerificationResponse
	decodeBody(t, resp, &created)
	if created.Status != string(kyc.StatusPending) && created.Status != string(kyc.StatusUnverified) {
		t.Fatalf("unexpected initial verification status %q", created.Status)
	}
	providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, created.ID)
	// PRH-2 E1: the create response is the phase-A orphan (unverified); the
	// outbox worker has since applied the vendor's result (read via the
	// helper above), so the "unchanged" baseline is read now, after it.
	baseResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	var baseList []playerVerificationResponse
	decodeBody(t, baseResp, &baseList)
	if len(baseList) != 1 {
		t.Fatalf("expected one verification, got %+v", baseList)
	}
	baselineStatus := baseList[0].Status

	// Step 3: the player HMACs a forged approval body with a secret they
	// could never actually derive (see kycE1RandomSecret's doc comment),
	// over the exact wire scheme the real mock uses - the pre-fix defect
	// was that the body alone (no tenant binding, one shared constant) was
	// sufficient; the fix requires a per-tenant derived key no player can
	// reach.
	forgedBody, _ := json.Marshal(map[string]string{
		"provider_reference": providerReference,
		"outcome":            "approved",
		"reason":             "self-approved-by-player",
	})
	scheme := webhookauth.KYCScheme()
	sigHex := scheme.Sign(kycE1RandomSecret(t), tenant.ID, "mock", webhookauth.MockKeyID, forgedBody)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/webhooks/kyc/%s/mock", srv.URL, tenant.Slug),
		bytes.NewReader(forgedBody))
	if err != nil {
		t.Fatalf("failed to build forged webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	scheme.SetHeaders(req.Header, webhookauth.MockKeyID, sigHex)
	webhookResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forged webhook request failed: %v", err)
	}
	defer webhookResp.Body.Close()

	// Step 4: the fixed expectation (design §H K1): 401, uniform "callback
	// rejected" body.
	if webhookResp.StatusCode != http.StatusUnauthorized {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(webhookResp.Body)
		t.Fatalf("expected the player-forged self-approval to be REJECTED with 401 (design §H K1), got %d: %s",
			webhookResp.StatusCode, body.String())
	}
	apiErr := decodeAPIError(t, webhookResp)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", apiErr.Message)
	}

	// No-effect checklist: the verification's status is unchanged, and no
	// kyc.provider_callback audit row was written for this forged attempt.
	listResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	var list []playerVerificationResponse
	decodeBody(t, listResp, &list)
	if len(list) != 1 || list[0].Status != baselineStatus {
		t.Fatalf("expected the verification's status to be unchanged by the forged callback, got %+v", list)
	}

	var auditCount int
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'kyc.provider_callback' AND target_id = $1`,
			created.ID).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("failed to query audit_log: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("expected NO kyc.provider_callback audit row for a rejected forged callback, got %d", auditCount)
	}
}
