//go:build integration

// Stage 10.3 pre-fix evidence; inverted by the fix.
//
// Records KYC-REASON-BOUND-1 (docs/plans/stage-10.3-planning/
// 03-kyc-reason-bound-analysis.md): the verified sender's `reason` string
// is unbounded in length and charset today, flows verbatim into
// kyc_verifications.reason and audit_log.metadata, and (per that file's
// own reading of kyc_handlers.go:75,83) into the PLAYER-facing
// playerVerificationResponse.Reason field. This test PASSES today because
// it demonstrates BOTH halves of the defect against the CURRENT code:
// unbounded/control-character storage, and player exposure of the raw
// value. The NormalizeReason fix (W1d) and the reason_code split are
// expected to invert this test.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// TestKYCReasonBound_PreFix_UnboundedReasonStoredAndShownToPlayer is E6: a
// verified mock callback with a very long reason containing control and
// bidi characters is (a) stored unbounded/unstripped in
// kyc_verifications.reason and in audit_log.metadata, and (b) exposed
// verbatim to the player through GET /v1/me/kyc/verifications. Part (b)
// is checked, not assumed - if the player response does NOT carry the raw
// reason, this test records that precisely instead of asserting it does.
func TestKYCReasonBound_PreFix_UnboundedReasonStoredAndShownToPlayer(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mockProvider, _ := newKYCTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if verResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating own verification, got %d", verResp.StatusCode)
	}
	var created playerVerificationResponse
	decodeBody(t, verResp, &created)
	providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, created.ID)

	// Oversized (well past any plausible 512-byte bound) and laced with
	// control characters (CR, ANSI escape) and a bidi override character
	// (U+202E, RIGHT-TO-LEFT OVERRIDE) - exactly the class of input
	// NormalizeReason (not yet implemented) is meant to bound and clean.
	// A literal NUL byte is deliberately excluded: PostgreSQL's TEXT type
	// rejects 0x00 unconditionally regardless of any application-level
	// bound (SQLSTATE 22021), so it is not part of THIS defect's evidence
	// (it is already, incidentally, impossible to store one) - the
	// unit-level NormalizeReason tests (not this integration test) are
	// the right place to prove NUL-stripping in the in-memory string
	// before it would ever reach the database.
	controlLaden := "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m‮evil-reversed-text"
	hugeReason := controlLaden + strings.Repeat("A", 4096) + controlLaden

	callback := mockProvider.CallbackPayload(tenant.ID, providerReference, kyc.ProviderRejected, hugeReason)
	callbackResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", callback)
	if callbackResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for a validly-signed callback carrying an oversized/control-character reason, got %d", callbackResp.StatusCode)
	}
	callbackResp.Body.Close()

	// --- Part (a): unbounded storage, verified directly against the DB ---
	var storedReason string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason FROM kyc_verifications WHERE id = $1`, created.ID).Scan(&storedReason)
	})
	if err != nil {
		t.Fatalf("query kyc_verifications.reason: %v", err)
	}
	if storedReason != hugeReason {
		t.Fatalf("PRE-FIX EVIDENCE: expected kyc_verifications.reason to store the oversized/control-character reason VERBATIM and UNBOUNDED (no NormalizeReason exists yet), got a value that differs (len stored=%d, len sent=%d)", len(storedReason), len(hugeReason))
	}
	if len(storedReason) <= 512 {
		t.Fatalf("PRE-FIX EVIDENCE PRECONDITION FAILED: expected the stored reason to exceed the future 512-byte bound, got %d bytes", len(storedReason))
	}
	if !strings.Contains(storedReason, "\r") || !strings.Contains(storedReason, "\x1b[31m") || !strings.Contains(storedReason, "‮") {
		t.Fatalf("PRE-FIX EVIDENCE: expected the stored reason to retain its raw control/ANSI/bidi characters unmodified, got %q (truncated in log)", storedReason[:min(200, len(storedReason))])
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
	if auditMetadataReason != hugeReason {
		t.Fatalf("PRE-FIX EVIDENCE: expected audit_log.metadata to carry the SAME unbounded, unmodified reason, got a value that differs (len stored=%d, len sent=%d)", len(auditMetadataReason), len(hugeReason))
	}

	// --- Part (b): player-facing exposure, checked (not assumed) ---
	listResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	var list []playerVerificationResponse
	decodeBody(t, listResp, &list)
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 verification in the player's own list, got %d", len(list))
	}
	if list[0].Reason == hugeReason {
		t.Logf("PRE-FIX EVIDENCE: the player-facing GET /v1/me/kyc/verifications response DOES expose the raw, unbounded, control-character-laden reason string verbatim in its 'reason' field (kyc_handlers.go playerVerificationResponse.Reason) - the analysis's finding is confirmed as observed.")
	} else {
		t.Fatalf("RECORDING OBSERVED BEHAVIOUR (do not massage): the player-facing response's 'reason' field does NOT match the raw stored reason today. Observed player-facing reason=%q (len=%d); raw stored reason len=%d. This differs from the 02/03 analysis's stated finding (kyc_handlers.go:75,83) - re-verify the citation before relying on it.", list[0].Reason, len(list[0].Reason), len(hugeReason))
	}
}
