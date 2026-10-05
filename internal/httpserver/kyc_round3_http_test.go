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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

const uploadLogRedactionSentinel = "VENDOR-SUBMIT-TRANSPORT-SECRET-qrs456"

// failingSubmitVerificationAdapter wraps the MOCK but always fails
// SubmitVerification with an error carrying a sentinel string - standing
// in for a real adapter's own transport error on the SUBMIT call. calls counts
// the vendor submit calls actually made.
type failingSubmitVerificationAdapter struct {
	*kyc.MockKYCProvider
	calls *atomic.Int32
}

func (a failingSubmitVerificationAdapter) SubmitVerification(ctx context.Context, providerReference string, documents []kyc.SubmittedDocument, call kyc.CallContext) (kyc.ProviderResult, error) {
	a.calls.Add(1)
	return kyc.ProviderResult{}, errors.New("POST https://vendor.example/submit?token=" + uploadLogRedactionSentinel)
}

func newUploadSubmitFailureLogRedactionServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, logger *slog.Logger) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	base := kyc.NewMockKYCProvider()
	adapter := failingSubmitVerificationAdapter{MockKYCProvider: base, calls: new(atomic.Int32)}
	orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": adapter}, kyc.NewMockWebhookCredentials(base))
	w := registerKYCOutboxWorker(t, pool, orch, kyc.NewMockOutboundResolver())
	w.Logger = logger
	srv := httptest.NewServer(New(Deps{
		Logger:                 logger,
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        orch,
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv, adapter.calls
}

// TestKYC_UploadSubmitFailureLog_NeverLeaksRawErrorText (R2-2, rewritten for
// PRH-2 E1): the upload handler commits the document and its submit outbox row
// and makes NO vendor call; the worker's later submit fails with raw adapter
// error text, which must never reach a worker log line.
func TestKYC_UploadSubmitFailureLog_NeverLeaksRawErrorText(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	srv, submitCalls := newUploadSubmitFailureLogRedactionServer(t, pool, issuer, logger)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if verResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating a verification, got %d", verResp.StatusCode)
	}
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)
	drainKYCOutbox(t) // the create is sent (the adapter only fails SUBMIT)

	uploadResp := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "passport.png", tinyPNG)
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 uploading a document, got %d", uploadResp.StatusCode)
	}
	if got := submitCalls.Load(); got != 0 {
		t.Fatalf("the HTTP upload path must make NO vendor call, got %d", got)
	}

	drainKYCOutbox(t)
	if got := submitCalls.Load(); got != 1 {
		t.Fatalf("the worker must make exactly one vendor submit call, got %d", got)
	}
	var lines []capturedLogLine
	for _, l := range captured() {
		for k, v := range l.attrs {
			if s, ok := v.(string); ok && strings.Contains(s, uploadLogRedactionSentinel) {
				t.Fatalf("log line %q field %q leaked raw provider error text: %q", l.msg, k, s)
			}
		}
		if l.msg == "kyc_outbox_provider_call_failed" && l.attrs["operation"] == "submit" {
			lines = append(lines, l)
		}
		if l.msg == "submit_verification_failed" {
			t.Fatal("the handler must not log a vendor submit failure: it makes no vendor call")
		}
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 worker submit failure line, got %d: %+v", len(lines), lines)
	}
	if class, _ := lines[0].attrs["class"].(string); class != string(kyc.ClassAmbiguous) {
		t.Fatalf(`expected class="ambiguous", got %q`, class)
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

// mismatchedKYCResolver hands back a credential for a DIFFERENT tenant: the
// worker's binding check ends the create failed_terminal, which leaves a
// harmless orphan with no live create (PRH-2 E1).
type mismatchedKYCResolver struct{}

func (mismatchedKYCResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, _ uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	return providercred.NewMockOutboundCredential(uuid.New(), "kyc", providerID), nil
}

// TestKYC_UploadOrphanVerification_Returns409NotInternalError is R2-3's own
// required test for kyc.ErrVerificationNotSubmitted: a player who somehow
// knows an orphan verification's id must get 409 uploading to it when the
// orphan has NO live create (its create ended failed_terminal), never 500.
// (An orphan whose create is still live accepts the upload, ADR 0106 section
// 2.6: covered in internal/kyc.)
func TestKYC_UploadOrphanVerification_Returns409NotInternalError(t *testing.T) {
	pool, issuer := testEnv(t)
	base := kyc.NewMockKYCProvider()
	orch := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": base}, kyc.NewMockWebhookCredentials(base))
	registerKYCOutboxWorker(t, pool, orch, mismatchedKYCResolver{})
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.DiscardHandler),
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        orch,
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	defer verResp.Body.Close()
	if verResp.StatusCode != http.StatusCreated {
		t.Fatalf("test setup: expected 201 creating a verification, got %d", verResp.StatusCode)
	}
	drainKYCOutbox(t) // credential binding mismatch: the create ends failed_terminal

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
		t.Fatalf("R2-3: expected 409 uploading to an orphan verification with no live create, got %d", uploadResp.StatusCode)
	}
}
