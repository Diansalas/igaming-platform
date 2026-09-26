//go:build integration

// Stage 10.3 KYC-REASON-BOUND-1: E6 post-fix inversion.
//
// docs/plans/stage-10.3-planning/evidence/e6.txt records this file's
// PRE-FIX evidence run (test name
// TestKYCReasonBound_PreFix_UnboundedReasonStoredAndShownToPlayer, HEAD
// c90e591): a verified mock callback with an oversized, control/bidi-
// character-laden reason was stored VERBATIM and UNBOUNDED in
// kyc_verifications.reason and audit_log.metadata, and (per that run's
// own observation) the player-facing GET /v1/me/kyc/verifications
// response's 'reason' field DID expose it. That evidence file is left
// UNCHANGED (it is a historical record of the pre-fix run, not a live
// assertion) - this file inverts the CODE to prove the fix
// (NormalizeReason, the kyc_verifications.reason CHECK migration 0095,
// and playerVerificationResponse dropping reason/reason_code entirely
// per HD-10.3-3) actually closes both halves of the defect.
package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// TestKYCReasonBound_E6_NormalizedReasonBoundedAndNeverShownToPlayer is
// E6's post-fix assertion: the SAME oversized/control/bidi-character-laden
// reason this stage's pre-fix evidence run used is now (a) bounded to
// <=512 bytes and stripped of control/bidi characters wherever it is
// stored (kyc_verifications.reason, audit_log.metadata), and (b) never
// present, under any field name, in the player-facing
// GET /v1/me/kyc/verifications response - checked structurally (the raw
// JSON body is inspected for the substring), not merely "the typed
// struct has no Reason field", so a future field re-add under a
// different name would also be caught.
func TestKYCReasonBound_E6_NormalizedReasonBoundedAndNeverShownToPlayer(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mockProvider, _ := newKYCTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if verResp.StatusCode != 201 {
		t.Fatalf("expected 201 creating own verification, got %d", verResp.StatusCode)
	}
	var created playerVerificationResponse
	decodeBody(t, verResp, &created)
	providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, created.ID)

	// The EXACT E6 pre-fix evidence shape: oversized, control-character
	// (CR, ANSI escape) and bidi-override (U+202E) laden.
	controlLaden := "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text"
	hugeReason := controlLaden + strings.Repeat("A", 4096) + controlLaden

	callback := mockProvider.CallbackPayload(tenant.ID, providerReference, kyc.ProviderRejected, hugeReason)
	callbackResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", callback)
	if callbackResp.StatusCode != 204 {
		t.Fatalf("expected 204 for a validly-signed callback carrying an oversized/control-character reason, got %d", callbackResp.StatusCode)
	}
	_ = callbackResp.Body.Close()

	// --- Part (a): bounded, clean storage - verified directly against the DB ---
	var storedReason string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM kyc_verifications WHERE id = $1`, created.ID).Scan(&storedReason)
	})
	if err != nil {
		t.Fatalf("query kyc_verifications.reason: %v", err)
	}
	if storedReason == hugeReason {
		t.Fatal("POST-FIX REGRESSION: expected kyc_verifications.reason to be bounded/cleaned by NormalizeReason, got the raw, unbounded value stored verbatim")
	}
	if len(storedReason) > kyc.MaxReasonBytes {
		t.Fatalf("expected kyc_verifications.reason to be at most %d bytes, got %d", kyc.MaxReasonBytes, len(storedReason))
	}
	for _, bad := range []string{"\r", "\x1b", "\u202E"} {
		if strings.Contains(storedReason, bad) {
			t.Fatalf("expected control/bidi character %q stripped from the stored reason, got %q", bad, storedReason)
		}
	}

	var auditMetadataReason string
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata->>'reason' FROM audit_log WHERE action = 'kyc.provider_callback' AND target_id = $1 ORDER BY created_at DESC LIMIT 1`,
			created.ID).Scan(&auditMetadataReason)
	})
	if err != nil {
		t.Fatalf("query audit_log.metadata->>'reason': %v", err)
	}
	if auditMetadataReason != storedReason {
		t.Fatalf("expected audit_log.metadata to carry the SAME bounded/cleaned reason as the DB row, got %q vs stored %q", auditMetadataReason, storedReason)
	}

	// --- Part (b): the player-facing response, checked structurally at
	// the raw JSON level - never present under ANY field name (HD-10.3-3:
	// players see status only, no provider reason and no reason_code). ---
	listResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	rawBody, err := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if err != nil {
		t.Fatalf("read player list response body: %v", err)
	}
	var list []playerVerificationResponse
	if err := json.Unmarshal(rawBody, &list); err != nil {
		t.Fatalf("decode player list response: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 verification in the player's own list, got %d", len(list))
	}
	if list[0].Status != string(kyc.StatusRejected) {
		t.Fatalf("expected the player-visible status to reflect the callback's outcome (rejected), got %q", list[0].Status)
	}

	rawBodyStr := string(rawBody)
	if strings.Contains(rawBodyStr, "\"reason\"") || strings.Contains(rawBodyStr, "\"reason_code\"") {
		t.Fatalf("HD-10.3-3 VIOLATION: the player-facing response body declares a reason/reason_code field at all: %s", rawBodyStr)
	}
	// Belt and suspenders: even if some future field carried it under a
	// different name, the raw provider text itself must never appear
	// anywhere in the player-facing body.
	if strings.Contains(rawBodyStr, "FAKE ADMIN MESSAGE") || strings.Contains(rawBodyStr, "evil-reversed-text") {
		t.Fatal("HD-10.3-3 VIOLATION: the raw provider reason text leaked into the player-facing response body under some field")
	}
}
