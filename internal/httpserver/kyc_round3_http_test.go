//go:build integration

// RV-PRH-I2 KYC code re-review 2 (`rv-prh-i2-kyc-code-review.md`, R2-2/R2-3,
// 2026-09-27): closes two Low findings the round-2 fix left open.
//
//   - R2-2: the upload handler's own "submit_verification_failed" log line
//     (kyc_handlers.go) uses kyc.RedactedProviderErrorDetail, exactly like
//     the create-verification path's own line, but had no test of its own
//     proving the raw error text never reaches it end to end over HTTP -
//     the create-path twin (kyc_create_verification_log_redaction_test.go)
//     does not exercise this second call site.
//   - R2-3: neither new 409 mapping added in round 2
//     (kyc.ErrVerificationStatusConflict in newReviewVerificationHandler,
//     kyc.ErrVerificationNotSubmitted in newUploadMyDocumentHandler) had an
//     HTTP-level test - removing either branch silently falls through to a
//     500, and no existing test would notice.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

const uploadLogRedactionSentinel = "VENDOR-SUBMIT-TRANSPORT-SECRET-qrs456"

// failingSubmitVerificationAdapter wraps the MOCK but always fails
// SubmitVerification with an error carrying a sentinel string - standing
// in for a real adapter's own transport error on the SUBMIT call
// (distinct from failingCreateVerificationAdapter's own CREATE failure),
// so the upload handler's own "submit_verification_failed" log line is
// what gets exercised, not the create-path's line.
type failingSubmitVerificationAdapter struct{ *kyc.MockKYCProvider }

func (a failingSubmitVerificationAdapter) SubmitVerification(ctx context.Context, providerReference string, documents []kyc.SubmittedDocument, call kyc.CallContext) (kyc.ProviderResult, error) {
	return kyc.ProviderResult{}, errors.New("POST https://vendor.example/submit?token=" + uploadLogRedactionSentinel)
}

func newUploadSubmitFailureLogRedactionServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, logger *slog.Logger) *httptest.Server {
	t.Helper()
	base := kyc.NewMockKYCProvider()
	adapter := failingSubmitVerificationAdapter{base}
	srv := httptest.NewServer(New(Deps{
		Logger:                 logger,
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": adapter}, kyc.NewMockWebhookCredentials(base)),
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestKYC_UploadSubmitFailureLog_NeverLeaksRawErrorText is R2-2's own
// required test: an upload that succeeds (phase A commits) but whose
// immediately-following SubmitVerification (phase B/C) fails must never
// leak the raw adapter error text into the "submit_verification_failed"
// log line - only kyc.RedactedProviderErrorDetail's bounded
// classification, mirroring the create-path's own already-pinned test.
func TestKYC_UploadSubmitFailureLog_NeverLeaksRawErrorText(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	srv := newUploadSubmitFailureLogRedactionServer(t, pool, issuer, logger)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if verResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating a verification, got %d", verResp.StatusCode)
	}
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	uploadResp := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "passport.png", tinyPNG)
	defer uploadResp.Body.Close()
	// The document upload itself (phase A) still succeeds - only the
	// FOLLOWING SubmitVerification call (phase B/C) fails, and that
	// failure never invalidates the already-committed upload (ADR 0095
	// §15.3).
	if uploadResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 uploading a document even though the following submit fails, got %d", uploadResp.StatusCode)
	}

	var lines []capturedLogLine
	for _, l := range captured() {
		if l.msg == "submit_verification_failed" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 submit_verification_failed line, got %d: %+v", len(lines), lines)
	}
	for k, v := range lines[0].attrs {
		if s, ok := v.(string); ok && strings.Contains(s, uploadLogRedactionSentinel) {
			t.Fatalf("submit_verification_failed field %q leaked raw provider error text: %q", k, s)
		}
	}
	// The wrapping fmt.Errorf("%w: submit verification to provider: %v",
	// ErrProviderUnavailable, err) in SubmitVerification means
	// errors.Is(err, ErrProviderUnavailable) holds, so
	// RedactedProviderErrorDetail correctly classifies this as "provider
	// unavailable" (not the default "internal error (redacted)" branch) -
	// exactly like the create-path's own already-pinned test. The point of
	// THIS test is that the SENTINEL never leaks (checked above), not
	// which specific closed class applies.
	detail, _ := lines[0].attrs["detail"].(string)
	if detail != "provider unavailable" {
		t.Fatalf(`expected detail="provider unavailable", got %q`, detail)
	}
}

// TestKYC_ReviewVerificationConflict_Returns409NotInternalError is R2-3's
// own required test for kyc.ErrVerificationStatusConflict: two genuinely
// concurrent HTTP review calls racing the SAME non-terminal verification
// to two DIFFERENT terminal statuses. Exactly one must win with 200; the
// loser must get 409 (apierror.CodeConflict) with the EXACT
// ErrVerificationStatusConflict message, never a 500 and never the
// pre-existing (already-covered) ErrInvalidTransition 409 - if the
// ErrVerificationStatusConflict branch in newReviewVerificationHandler is
// removed, this never observes that message and fails.
//
// Cross-package tests cannot reach kyc's own package-private
// reviewVerificationTestRaceHook (unlike internal/kyc's own mirror-race
// test, which injects the race deterministically), so genuinely
// overlapping read/write windows here depend on real goroutine and
// Postgres timing. This repeats the race against a FRESH verification
// each attempt, bounded, rather than asserting on a single (occasionally
// non-overlapping, and therefore inconclusive) pair - observed to succeed
// within the first few attempts locally.
func TestKYC_ReviewVerificationConflict_Returns409NotInternalError(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-r3-conflict-retry-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-r3-conflict-retry-pw-1")

	const maxAttempts = 40
	for attempt := 0; attempt < maxAttempts; attempt++ {
		player := mustRegisterPlayer(t, srv, brand.Slug)
		verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
		var verification map[string]any
		decodeBody(t, verResp, &verification)
		verResp.Body.Close()
		verificationID := verification["id"].(string)

		var wg sync.WaitGroup
		statusCodes := make([]int, 2)
		bodies := make([]string, 2)
		targets := []string{"approved", "rejected"}
		var start sync.WaitGroup
		start.Add(1)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start.Wait()
				resp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", complianceTokens.AccessToken,
					map[string]any{"status": targets[i], "reason": "concurrent_review_race"})
				defer resp.Body.Close()
				statusCodes[i] = resp.StatusCode
				var body map[string]any
				decodeBody(t, resp, &body)
				if msg, ok := body["message"].(string); ok {
					bodies[i] = msg
				}
			}(i)
		}
		start.Done()
		wg.Wait()

		for i, code := range statusCodes {
			if code != http.StatusOK && code != http.StatusConflict {
				t.Fatalf("R2-3: expected every concurrent review response to be 200 or 409, got %d (all: %v, bodies: %v)", code, statusCodes, bodies)
			}
			if code == http.StatusConflict && strings.Contains(bodies[i], "verification status changed") {
				// Genuine CAS conflict observed - if the
				// ErrVerificationStatusConflict branch were removed, this
				// exact message would never appear (either a 500, or the
				// generic "failed to review verification" message from the
				// fallback branch) and every attempt in this loop would be
				// exhausted without ever reaching this return.
				return
			}
		}
	}
	t.Fatalf("R2-3: did not observe a genuine ErrVerificationStatusConflict 409 (message containing \"verification status changed\") in %d attempts", maxAttempts)
}

// TestKYC_UploadOrphanVerification_Returns409NotInternalError is R2-3's
// own required test for kyc.ErrVerificationNotSubmitted: a player who
// somehow knows an orphan verification's id (surfaced legitimately by
// their own GET /v1/me/kyc/verifications listing) must get 409 uploading
// to it, never 500 - if the ErrVerificationNotSubmitted branch in
// newUploadMyDocumentHandler is removed, this falls through to whatever
// the next matched branch is (or the generic 500), not the required 409.
func TestKYC_UploadOrphanVerification_Returns409NotInternalError(t *testing.T) {
	pool, issuer := testEnv(t)
	// A resolver that always fails credential resolution reproduces
	// CreateVerification's own documented phase-B failure mode (ADR 0095
	// §15.2), leaving a harmless orphan row (status=unverified,
	// provider_reference NULL) - exactly like a real vendor outage.
	base := kyc.NewMockKYCProvider()
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.DiscardHandler),
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": base}, kyc.NewMockWebhookCredentials(base)),
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: nil, // nil resolver -> CreateVerification's phase B fails closed, leaving an orphan.
	}))
	t.Cleanup(srv.Close)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	defer verResp.Body.Close()
	if verResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("test setup: expected 503 creating a verification with no outbound resolver (leaving an orphan), got %d", verResp.StatusCode)
	}

	listResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	defer listResp.Body.Close()
	var list []map[string]any
	decodeBody(t, listResp, &list)
	if len(list) != 1 || list[0]["status"] != "unverified" {
		t.Fatalf("test setup: expected exactly one orphan (status=unverified) verification, got %+v", list)
	}
	orphanID := list[0]["id"].(string)

	uploadResp := postMultipartDocument(t, srv, player.Tokens.AccessToken, orphanID, "passport", "passport.png", tinyPNG)
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusConflict {
		t.Fatalf("R2-3: expected 409 uploading to an orphan verification, got %d", uploadResp.StatusCode)
	}
}
