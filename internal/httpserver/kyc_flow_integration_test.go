//go:build integration

// Stage 4F HTTP-layer tests: email verification, password reset, KYC
// verification/document lifecycle, staff review authorization, provider
// callback authentication, tenant isolation. Follows casino_flow_
// integration_test.go/rg_flow_integration_test.go's own conventions
// exactly (fixtures via internal packages, bearer tokens minted through
// the real HTTP register/login flow).
package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func newKYCTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer) (*httptest.Server, *kyc.MockKYCProvider, *email.MockProvider) {
	t.Helper()
	// Stage 10.2 (ADR 0091, KYC-WH-1): NewMockKYCProvider takes no
	// argument (a per-process crypto/rand master, never an injectable
	// literal) - the resolver is the SAME kyc.NewMockWebhookCredentials
	// wiring cmd/platform-api/wiring.go's kycWebhookResolver uses when
	// test support is enabled.
	mockProvider := kyc.NewMockKYCProvider()
	mockEmail := email.NewMockProvider()
	srv := httptest.NewServer(New(Deps{
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                pool,
		AuthIssuer:        issuer,
		ServiceName:       "platform-api-test",
		AccessTokenTTL:    5 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		PersonResolver:    identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:   kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mockProvider}, kyc.NewMockWebhookCredentials(mockProvider)),
		KYCWebhookEnabled: true,
		DocumentStorage:   kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:    kyc.NewMockMalwareScanner(),
		EmailProvider:     mockEmail,
	}))
	t.Cleanup(srv.Close)
	return srv, mockProvider, mockEmail
}

// mustGetKYCProviderReference reads a verification's provider_reference
// directly, tenant-scoped - the K15 replacement for reading it off the
// player-facing JSON response, which no longer carries it (Stage 10.2
// design §B5: a player has no legitimate need to see or echo it, and it
// is no longer a bearer capability once the webhook is tenant-bound and
// signature-verified).
func mustGetKYCProviderReference(t *testing.T, pool *db.Pool, tenantID uuid.UUID, verificationID string) string {
	t.Helper()
	id, err := uuid.Parse(verificationID)
	if err != nil {
		t.Fatalf("invalid verification id %q: %v", verificationID, err)
	}
	var ref string
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, id).Scan(&ref)
	})
	if err != nil {
		t.Fatalf("failed to read provider_reference for verification %s: %v", verificationID, err)
	}
	return ref
}

// rawPostKYCCallback posts a webhookauth.Inbound's body to the KYC public
// webhook route with its own headers (X-KYC-Signature/X-KYC-Key-Id) - the
// KYC counterpart of financial_flow_integration_test.go's rawPostCallback.
func rawPostKYCCallback(t *testing.T, srv *httptest.Server, path string, in webhookauth.Inbound) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(in.Body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range in.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// tinyPNG is a minimal valid 1x1 PNG - real PNG magic bytes so
// http.DetectContentType sniffs it as image/png, exactly like a real
// uploaded photo would be.
var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

func postMultipartDocument(t *testing.T, srv *httptest.Server, bearerToken string, verificationID, documentType, filename string, content []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("verification_id", verificationID)
	_ = mw.WriteField("document_type", documentType)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("failed to write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/me/kyc/documents", &buf)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// --- 1. Email verification: request -> confirm -> account becomes active ---

func TestEmailVerification_RequestConfirmActivatesAccount(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/email-verification/request", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 requesting email verification, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sent := mockEmail.Sent()
	if len(sent) != 1 || sent[0].To != player.Email {
		t.Fatalf("expected exactly one email sent to %s, got %+v", player.Email, sent)
	}
	token := extractToken(t, sent[0].Body)

	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": token})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 confirming email verification, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me", player.Tokens.AccessToken)
	defer resp.Body.Close()
	var me map[string]any
	decodeBody(t, resp, &me)
	if me["status"] != "active" {
		t.Fatalf("expected status active after email verification, got %+v", me["status"])
	}
}

// --- 2. Confirming twice fails the second time (one-time use) ---

func TestEmailVerification_TokenIsOneTimeUse(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	postJSON(t, srv, "/v1/me/email-verification/request", player.Tokens.AccessToken, map[string]any{}).Body.Close()
	token := extractToken(t, mockEmail.Sent()[0].Body)

	first := postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": token})
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("expected first confirm to succeed, got %d", first.StatusCode)
	}
	first.Body.Close()

	second := postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": token})
	defer second.Body.Close()
	if second.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 reusing an already-consumed token, got %d", second.StatusCode)
	}
}

// --- 3. An expired token is rejected (forced directly at the DB layer,
// since the real TTL is 24h) ---

func TestEmailVerification_ExpiredTokenRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	postJSON(t, srv, "/v1/me/email-verification/request", player.Tokens.AccessToken, map[string]any{}).Body.Close()
	token := extractToken(t, mockEmail.Sent()[0].Body)

	err := pool.WithTenant(context.Background(), brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE player_credential_tokens SET expires_at = now() - interval '1 hour'`)
		return err
	})
	if err != nil {
		t.Fatalf("failed to force token expiry: %v", err)
	}

	resp := postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": token})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an expired token, got %d", resp.StatusCode)
	}
}

// --- 4. Rate limiting: resending beyond the limit is silently a no-op
// (never a distinguishable response - directive's anti-enumeration
// requirement) ---

func TestEmailVerification_ResendIsRateLimited(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	for i := 0; i < credentialTokenRateMaxCount+2; i++ {
		resp := postJSON(t, srv, "/v1/me/email-verification/request", player.Tokens.AccessToken, map[string]any{})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("iteration %d: expected 204 (rate limiting is silent), got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if len(mockEmail.Sent()) != credentialTokenRateMaxCount {
		t.Fatalf("expected exactly %d emails actually sent, got %d", credentialTokenRateMaxCount, len(mockEmail.Sent()))
	}
}

// --- 5. Password reset: request -> confirm -> new password works, old
// password rejected, all sessions revoked ---

func TestPasswordReset_RequestConfirmChangesPasswordAndRevokesSessions(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/auth/password-reset/request", "", map[string]any{"brand_slug": brand.Slug, "email": player.Email})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 requesting password reset, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	token := extractToken(t, mockEmail.Sent()[0].Body)
	const newPassword = "a-brand-new-password-1"
	resp = postJSON(t, srv, "/v1/auth/password-reset/confirm", "", map[string]any{"token": token, "new_password": newPassword})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 confirming password reset, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The OLD access token's underlying session is now revoked - refresh
	// must fail even though the access token itself hasn't expired yet.
	refreshResp := postJSON(t, srv, "/v1/auth/refresh", "", map[string]any{"refresh_token": player.Tokens.RefreshToken})
	defer refreshResp.Body.Close()
	if refreshResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 refreshing a session revoked by password reset, got %d", refreshResp.StatusCode)
	}

	// Logging in with the NEW password succeeds.
	loginResp := postJSON(t, srv, "/v1/auth/login", "", map[string]any{"brand_slug": brand.Slug, "email": player.Email, "password": newPassword})
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 logging in with the new password, got %d", loginResp.StatusCode)
	}
}

// --- 6. Request against a non-existent email returns the SAME response
// as a real one (anti-enumeration) ---

func TestPasswordReset_UnknownEmailGetsIdenticalResponse(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))

	resp := postJSON(t, srv, "/v1/auth/password-reset/request", "", map[string]any{"brand_slug": brand.Slug, "email": "no-such-player@example.com"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for an unknown email, got %d", resp.StatusCode)
	}
	if len(mockEmail.Sent()) != 0 {
		t.Fatalf("expected no email sent for an unknown email, got %+v", mockEmail.Sent())
	}
}

// --- 7. Reset token replay after use fails ---

func TestPasswordReset_TokenIsOneTimeUse(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	postJSON(t, srv, "/v1/auth/password-reset/request", "", map[string]any{"brand_slug": brand.Slug, "email": player.Email}).Body.Close()
	token := extractToken(t, mockEmail.Sent()[0].Body)

	first := postJSON(t, srv, "/v1/auth/password-reset/confirm", "", map[string]any{"token": token, "new_password": "first-new-password-1"})
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("expected first confirm to succeed, got %d", first.StatusCode)
	}
	first.Body.Close()

	second := postJSON(t, srv, "/v1/auth/password-reset/confirm", "", map[string]any{"token": token, "new_password": "second-new-password-1"})
	defer second.Body.Close()
	if second.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 reusing an already-consumed reset token, got %d", second.StatusCode)
	}
}

// --- 7a. Concurrent double-confirm of the SAME one-time token: the
// atomic conditional-UPDATE consumption (ValidateAndConsumeCredentialToken
// - WHERE consumed_at IS NULL AND expires_at > now(), checked via
// RowsAffected) must let exactly ONE of two simultaneous requests
// succeed, never both - a P0 finding from adversarial review, previously
// exercised only sequentially. Uses password reset since its side effect
// (password change + full session revocation) is unambiguous to verify
// exactly-once. ---

func TestPasswordReset_ConcurrentDoubleConfirmAppliesExactlyOnce(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, mockEmail := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	postJSON(t, srv, "/v1/auth/password-reset/request", "", map[string]any{"brand_slug": brand.Slug, "email": player.Email}).Body.Close()
	token := extractToken(t, mockEmail.Sent()[0].Body)

	const concurrency = 8
	statusCodes := make([]int, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			resp := postJSON(t, srv, "/v1/auth/password-reset/confirm", "", map[string]any{
				"token": token, "new_password": fmt.Sprintf("racer-new-password-%d", i),
			})
			statusCodes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, code := range statusCodes {
		if code == http.StatusNoContent {
			successes++
		} else if code != http.StatusBadRequest {
			t.Fatalf("expected every racing confirm to return 204 or 400, got %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent confirms to succeed, got %d", concurrency, successes)
	}

	// The token itself must show exactly one consumption, never two
	// concurrent UPDATEs both believing they won.
	var consumedCount int
	err := pool.WithTenant(context.Background(), brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM player_credential_tokens WHERE token_hash IS NOT NULL AND consumed_at IS NOT NULL AND player_account_id = $1`,
			player.ID).Scan(&consumedCount)
	})
	if err != nil {
		t.Fatalf("query consumed tokens: %v", err)
	}
	if consumedCount != 1 {
		t.Fatalf("expected exactly 1 consumed credential token row, got %d", consumedCount)
	}
}

// --- 8. KYC verification + document lifecycle: create verification,
// upload a document, list, download own content ---

func TestKYC_VerificationAndDocumentLifecycle(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating a verification, got %d", resp.StatusCode)
	}
	var verification map[string]any
	decodeBody(t, resp, &verification)
	verificationID := verification["id"].(string)

	uploadResp := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "passport.png", tinyPNG)
	if uploadResp.StatusCode != http.StatusCreated {
		body := make([]byte, 2048)
		n, _ := uploadResp.Body.Read(body)
		t.Fatalf("expected 201 uploading a document, got %d: %s", uploadResp.StatusCode, body[:n])
	}
	var doc map[string]any
	decodeBody(t, uploadResp, &doc)
	if doc["status"] != "pending_review" || doc["content_type"] != "image/png" || doc["version"].(float64) != 1 {
		t.Fatalf("unexpected document response: %+v", doc)
	}
	docID := doc["id"].(string)

	listResp := getJSON(t, srv, "/v1/me/kyc/documents", player.Tokens.AccessToken)
	defer listResp.Body.Close()
	var docs []map[string]any
	decodeBody(t, listResp, &docs)
	if len(docs) != 1 {
		t.Fatalf("expected exactly one document, got %+v", docs)
	}

	contentResp := getJSON(t, srv, "/v1/me/kyc/documents/"+docID+"/content", player.Tokens.AccessToken)
	defer contentResp.Body.Close()
	if contentResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 downloading own document content, got %d", contentResp.StatusCode)
	}
	if ct := contentResp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("expected Content-Type image/png, got %q", ct)
	}
}

// --- 9. A reupload of the same document_type creates a NEW version, not
// an overwrite ---

func TestKYC_ReuploadCreatesNewVersion(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	first := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "p1.png", tinyPNG)
	var firstDoc map[string]any
	decodeBody(t, first, &firstDoc)

	second := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "p2.png", tinyPNG)
	defer second.Body.Close()
	var secondDoc map[string]any
	decodeBody(t, second, &secondDoc)

	if firstDoc["version"].(float64) != 1 || secondDoc["version"].(float64) != 2 {
		t.Fatalf("expected versions 1 then 2, got %v then %v", firstDoc["version"], secondDoc["version"])
	}
	if firstDoc["id"] == secondDoc["id"] {
		t.Fatal("expected the reupload to be a NEW row, not an edit of the first")
	}

	listResp := getJSON(t, srv, "/v1/me/kyc/documents", player.Tokens.AccessToken)
	defer listResp.Body.Close()
	var docs []map[string]any
	decodeBody(t, listResp, &docs)
	if len(docs) != 2 {
		t.Fatalf("expected BOTH historical versions to remain listed, got %+v", docs)
	}
}

// --- 10. Rejected uploads: wrong extension for sniffed content, and
// malware-scanner detection ---

func TestKYC_UploadRejectsMismatchedExtensionAndMalware(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	mismatched := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "passport.pdf", tinyPNG)
	defer mismatched.Body.Close()
	if mismatched.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for a .pdf filename containing PNG bytes, got %d", mismatched.StatusCode)
	}

	malwareContent := append([]byte("\x89PNG\r\n\x1a\n"), []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)...)
	malwareContent = append(malwareContent, tinyPNG...)
	infected := postMultipartDocument(t, srv, player.Tokens.AccessToken, verificationID, "passport", "passport.png", malwareContent)
	defer infected.Body.Close()
	if infected.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for malware-scanner-flagged content, got %d", infected.StatusCode)
	}
}

// --- 11. A player cannot access another player's document or verification ---

func TestKYC_PlayerCannotAccessAnotherPlayersDocument(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", playerA.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	uploadResp := postMultipartDocument(t, srv, playerA.Tokens.AccessToken, verificationID, "passport", "p.png", tinyPNG)
	var doc map[string]any
	decodeBody(t, uploadResp, &doc)
	docID := doc["id"].(string)

	// Player B attempts to download Player A's document content.
	resp := getJSON(t, srv, "/v1/me/kyc/documents/"+docID+"/content", playerB.Tokens.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-player document access, got %d", resp.StatusCode)
	}

	// Player B attempts to upload a document against Player A's verification.
	crossUpload := postMultipartDocument(t, srv, playerB.Tokens.AccessToken, verificationID, "selfie", "s.png", tinyPNG)
	defer crossUpload.Body.Close()
	if crossUpload.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 uploading against another player's verification, got %d", crossUpload.StatusCode)
	}
}

// --- 12. Compliance can review; tenant_admin can read but not review;
// finance/platform_admin/player are denied entirely ---

func TestKYC_ReviewAuthorization(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-kyc-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-kyc-pw-1")
	readResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+player.ID.String(), tenantAdminTokens.AccessToken)
	if readResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for tenant_admin reading verifications, got %d", readResp.StatusCode)
	}
	readResp.Body.Close()
	reviewByAdminResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", tenantAdminTokens.AccessToken, map[string]any{"status": "approved"})
	if reviewByAdminResp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin reviewing a verification, got %d", reviewByAdminResp.StatusCode)
	}
	reviewByAdminResp.Body.Close()

	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-kyc-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-kyc-pw-1")
	financeResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+player.ID.String(), financeTokens.AccessToken)
	defer financeResp.Body.Close()
	if financeResp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for finance reading verifications, got %d", financeResp.StatusCode)
	}

	playerAttempt := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", player.Tokens.AccessToken, map[string]any{"status": "approved"})
	defer playerAttempt.Body.Close()
	if playerAttempt.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token reviewing a verification, got %d", playerAttempt.StatusCode)
	}

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-kyc-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-kyc-pw-1")
	reviewResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", complianceTokens.AccessToken, map[string]any{"status": "approved", "reason": "docs_verified"})
	defer reviewResp.Body.Close()
	if reviewResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for compliance reviewing a verification, got %d", reviewResp.StatusCode)
	}
	var reviewed map[string]any
	decodeBody(t, reviewResp, &reviewed)
	if reviewed["status"] != "approved" {
		t.Fatalf("expected status approved, got %+v", reviewed["status"])
	}
}

// --- 12a. Stage 4I Phase B: a review call that includes
// verified_residence_country succeeds once evidence collection is
// enabled, and the response body does not leak the raw country value
// anywhere it wasn't asked for. ---

func TestKYC_ReviewWithVerifiedResidenceDetermination(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-kyc-residence-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-kyc-residence-pw-1")

	// Enable jurisdiction_evidence_collection_active for verified_residence.
	enableResp := sendAssetJSON(t, srv.URL+"/v1/admin/jurisdiction-evidence-collection/verified_residence", http.MethodPut, complianceTokens.AccessToken,
		map[string]any{"active": true, "reason_code": "kyc-review-residence-test"})
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling verified_residence collection, got %d", enableResp.StatusCode)
	}
	enableResp.Body.Close()

	reviewResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", complianceTokens.AccessToken,
		map[string]any{"status": "approved", "reason": "docs_verified", "verified_residence_country": "US"})
	defer reviewResp.Body.Close()
	if reviewResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reviewing with a verified_residence_country, got %d", reviewResp.StatusCode)
	}
	var reviewed map[string]any
	decodeBody(t, reviewResp, &reviewed)
	if reviewed["status"] != "approved" {
		t.Fatalf("expected status approved, got %+v", reviewed["status"])
	}
	// The response shape (verificationResponse) carries no residence field
	// at all today - confirmed deliberately, not by accident: the reviewer
	// that just set the determination IS authorized to know it, but this
	// phase does not wire it into the review response, so no key named
	// (or resembling) verified_residence_country may appear here.
	for k := range reviewed {
		if k == "verified_residence_country" {
			t.Fatalf("did not expect the review response to echo verified_residence_country, got %+v", reviewed)
		}
	}
}

// --- 12b. Activation gate OFF: attempting a residence determination is a
// 403, and the status transition requested in the SAME call is NOT
// applied either (verified via a follow-up GET). ---

func TestKYC_ReviewWithVerifiedResidenceDetermination_ActivationGateOff(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)
	originalStatus := verification["status"]

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-kyc-residence-pw-2")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-kyc-residence-pw-2")

	// Deliberately NOT enabling verified_residence collection - default OFF.
	reviewResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", complianceTokens.AccessToken,
		map[string]any{"status": "approved", "reason": "docs_verified", "verified_residence_country": "US"})
	defer reviewResp.Body.Close()
	if reviewResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 attempting a residence determination while collection is inactive, got %d", reviewResp.StatusCode)
	}

	listResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+player.ID.String(), complianceTokens.AccessToken)
	defer listResp.Body.Close()
	var list []map[string]any
	decodeBody(t, listResp, &list)
	if len(list) != 1 || list[0]["status"] != originalStatus {
		t.Fatalf("expected the status transition to NOT have been applied (still %v), got %+v", originalStatus, list)
	}
}

// --- 12c. An invalid verified_residence_country is a 400. ---

func TestKYC_ReviewWithInvalidVerifiedResidenceCountry(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-kyc-residence-pw-3")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-kyc-residence-pw-3")

	reviewResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", complianceTokens.AccessToken,
		map[string]any{"status": "approved", "reason": "docs_verified", "verified_residence_country": "ZZ"})
	defer reviewResp.Body.Close()
	if reviewResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid verified_residence_country, got %d", reviewResp.StatusCode)
	}
}

// --- 13. Cross-tenant: a different tenant's compliance staff cannot
// read/review a verification belonging to another tenant's player ---

func TestKYC_CrossTenantAccessDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", playerA.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	tenantB := mustCreateTenant(t, pool)
	complianceB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-kyc-crosstenant-pw-1")
	complianceBTokens := mustLoginStaff(t, srv, tenantB.Slug, complianceB.Email, "compliance-kyc-crosstenant-pw-1")

	// Tenant B's compliance staff cannot see tenant A's player's
	// verification at all (RLS-scoped list returns empty, not an error).
	listResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+playerA.ID.String(), complianceBTokens.AccessToken)
	defer listResp.Body.Close()
	var list []map[string]any
	decodeBody(t, listResp, &list)
	if len(list) != 0 {
		t.Fatalf("expected tenant B to see zero verifications for tenant A's player, got %+v", list)
	}

	reviewResp := postJSON(t, srv, "/v1/admin/kyc/verifications/"+verificationID+"/review", complianceBTokens.AccessToken, map[string]any{"status": "approved"})
	defer reviewResp.Body.Close()
	if reviewResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant B reviewing tenant A's verification, got %d", reviewResp.StatusCode)
	}
}

// K10 (staff leg): unlike the player-facing response (playerVerification
// Response, asserted absent in TestKYC_WebhookCallbackAuthentication and
// TestKYCWebhook_PlayerComputedSignature_Rejected), the STAFF-facing
// verificationResponse/kycCaseResponse shapes still carry
// provider_reference - design §B5: safe once the webhook is tenant-bound
// and signature-verified, and operationally needed for staff to correlate
// with a real vendor's own case/reference. Checked on BOTH staff-facing
// routes (the per-account list and the tenant-wide case queue), since
// they are two independently-maintained response types.
func TestKYC_StaffResponses_IncludeProviderReference(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var created map[string]any
	decodeBody(t, verResp, &created)
	verificationID := created["id"].(string)
	providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, verificationID)
	if providerReference == "" {
		t.Fatal("test setup: expected a non-empty provider_reference to have been minted")
	}

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-kyc-k10-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-kyc-k10-pw-1")

	listResp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+player.ID.String(), complianceTokens.AccessToken)
	defer listResp.Body.Close()
	var list []map[string]any
	decodeBody(t, listResp, &list)
	if len(list) != 1 || list[0]["provider_reference"] != providerReference {
		t.Fatalf("K10: expected the staff per-account list to carry provider_reference=%q, got %+v", providerReference, list)
	}

	queueResp := getJSON(t, srv, "/v1/admin/kyc/cases", complianceTokens.AccessToken)
	defer queueResp.Body.Close()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	decodeBody(t, queueResp, &page)
	if len(page.Items) != 1 || page.Items[0]["provider_reference"] != providerReference {
		t.Fatalf("K10: expected the staff tenant-wide queue to carry provider_reference=%q, got %+v", providerReference, page.Items)
	}
}

// --- 14. Provider callback: valid signature applies the outcome,
// invalid signature is rejected, unknown provider_reference 404s ---

func TestKYC_WebhookCallbackAuthentication(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mockProvider, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	if _, present := verification["provider_reference"]; present {
		t.Fatal("K10: player-facing verification response must never carry provider_reference")
	}
	verificationID := verification["id"].(string)
	providerReference := mustGetKYCProviderReference(t, pool, tenant.ID, verificationID)

	in := mockProvider.CallbackPayload(tenant.ID, providerReference, kyc.ProviderApproved, "auto_approved")
	resp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for a validly-signed callback, got %d", resp.StatusCode)
	}

	// K1/direct descendant of E1: a signature the caller (a player) could
	// compute on its own - a tampered/garbage signature - is rejected.
	tamperedIn := mockProvider.CallbackPayload(tenant.ID, providerReference, kyc.ProviderRejected, "tampered")
	tamperedIn.Header.Set(webhookauth.KYCSignatureHeader, "v1="+"0000000000000000000000000000000000000000000000000000000000000000"[:64])
	badSigResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", tamperedIn)
	defer badSigResp.Body.Close()
	if badSigResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an invalid signature, got %d", badSigResp.StatusCode)
	}

	// K8: a repeat of the ORIGINAL, validly-signed callback for an
	// already-terminal (approved) verification is a no-op (204), never
	// flips it to rejected, and does not resurrect it.
	repeat := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", in)
	defer repeat.Body.Close()
	if repeat.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 replaying an already-applied callback, got %d", repeat.StatusCode)
	}
	listResp := getJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken)
	var list []map[string]any
	decodeBody(t, listResp, &list)
	if len(list) != 1 || list[0]["status"] != "approved" {
		t.Fatalf("expected status to remain approved after a redelivered callback, got %+v", list)
	}

	unknownIn := mockProvider.CallbackPayload(tenant.ID, "no-such-reference", kyc.ProviderApproved, "x")
	unknownResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", unknownIn)
	defer unknownResp.Body.Close()
	if unknownResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown provider_reference, got %d", unknownResp.StatusCode)
	}
}

// --- 15. RLS adversarial: direct SQL cross-tenant access to
// kyc_verifications/kyc_documents/player_credential_tokens is denied ---

func TestKYC_RLSAdversarial_CrossTenantDirectSQLDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	verResp := postJSON(t, srv, "/v1/me/kyc/verifications", playerA.Tokens.AccessToken, map[string]any{})
	var verification map[string]any
	decodeBody(t, verResp, &verification)
	verificationID := verification["id"].(string)

	tenantB := mustCreateTenant(t, pool)

	var count int
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("unexpected error querying kyc_verifications: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected tenant B to see zero rows for tenant A's verification, got %d", count)
	}

	err = pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'unverified', 'mock')`,
			tenantA.ID, brandA.ID, playerA.ID, uuid.New(),
		)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected a row-level-security violation (42501), got: %v", err)
	}
}

// --- 16. Stage 5 Back Office: GET /v1/admin/kyc/cases is a tenant-wide,
// paginated queue - authorized request succeeds with the paged envelope
// shape and both ?status= and ?player_account_id= filters narrow it. ---

func TestKYCCases_TenantWideQueueAuthorizedAndPaginated(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, mockProvider, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)

	verAResp := postJSON(t, srv, "/v1/me/kyc/verifications", playerA.Tokens.AccessToken, map[string]any{})
	var verA map[string]any
	decodeBody(t, verAResp, &verA)

	verBResp := postJSON(t, srv, "/v1/me/kyc/verifications", playerB.Tokens.AccessToken, map[string]any{})
	var verB map[string]any
	decodeBody(t, verBResp, &verB)
	providerReferenceB := mustGetKYCProviderReference(t, pool, tenant.ID, verB["id"].(string))

	// Move playerB's verification to approved via the (already-tested)
	// provider callback path, so the two cases have different statuses to
	// filter on.
	in := mockProvider.CallbackPayload(tenant.ID, providerReferenceB, kyc.ProviderApproved, "auto_approved")
	cbResp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", in)
	if cbResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 applying callback, got %d", cbResp.StatusCode)
	}
	cbResp.Body.Close()

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-kyc-queue-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-kyc-queue-pw-1")

	// Unfiltered: both cases visible, paginated envelope shape.
	resp := getJSON(t, srv, "/v1/admin/kyc/cases", complianceTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for compliance listing the tenant-wide KYC queue, got %d", resp.StatusCode)
	}
	var page struct {
		Items  []map[string]any `json:"items"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
		Total  int              `json:"total"`
	}
	decodeBody(t, resp, &page)
	if page.Total != 2 || len(page.Items) != 2 || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("expected a paginated envelope with 2 total/items, got %+v", page)
	}
	for _, item := range page.Items {
		if item["player_account_id"] != playerA.ID.String() && item["player_account_id"] != playerB.ID.String() {
			t.Fatalf("unexpected player_account_id in queue item: %+v", item)
		}
		if item["brand_id"] == nil || item["created_at"] == nil {
			t.Fatalf("expected brand_id/created_at to be populated, got %+v", item)
		}
	}

	// ?status=approved narrows to exactly playerB's case.
	statusResp := getJSON(t, srv, "/v1/admin/kyc/cases?status=approved", complianceTokens.AccessToken)
	defer statusResp.Body.Close()
	var statusPage struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	decodeBody(t, statusResp, &statusPage)
	if statusPage.Total != 1 || len(statusPage.Items) != 1 || statusPage.Items[0]["player_account_id"] != playerB.ID.String() {
		t.Fatalf("expected exactly playerB's approved case, got %+v", statusPage)
	}

	// ?player_account_id= narrows to exactly playerA's case.
	acctResp := getJSON(t, srv, "/v1/admin/kyc/cases?player_account_id="+playerA.ID.String(), complianceTokens.AccessToken)
	defer acctResp.Body.Close()
	var acctPage struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	decodeBody(t, acctResp, &acctPage)
	if acctPage.Total != 1 || len(acctPage.Items) != 1 || acctPage.Items[0]["player_account_id"] != playerA.ID.String() {
		t.Fatalf("expected exactly playerA's case, got %+v", acctPage)
	}

	// Pagination bounds: limit/offset are honored.
	limitedResp := getJSON(t, srv, "/v1/admin/kyc/cases?limit=1&offset=1", complianceTokens.AccessToken)
	defer limitedResp.Body.Close()
	var limitedPage struct {
		Items  []map[string]any `json:"items"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
		Total  int              `json:"total"`
	}
	decodeBody(t, limitedResp, &limitedPage)
	if limitedPage.Limit != 1 || limitedPage.Offset != 1 || limitedPage.Total != 2 || len(limitedPage.Items) != 1 {
		t.Fatalf("expected limit=1/offset=1/total=2/one item, got %+v", limitedPage)
	}

	// An invalid ?status= value is a 400, not silently ignored.
	badStatusResp := getJSON(t, srv, "/v1/admin/kyc/cases?status=not_a_real_status", complianceTokens.AccessToken)
	defer badStatusResp.Body.Close()
	if badStatusResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid status filter, got %d", badStatusResp.StatusCode)
	}
}

// --- 17. GET /v1/admin/kyc/cases requires PermVerificationRead - a
// finance/support/player token is denied ---

func TestKYCCases_UnauthorizedDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-kyc-queue-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-kyc-queue-pw-1")
	resp := getJSON(t, srv, "/v1/admin/kyc/cases", financeTokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for finance listing the KYC queue, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	support := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleSupport, "support-kyc-queue-pw-1")
	supportTokens := mustLoginStaff(t, srv, tenant.Slug, support.Email, "support-kyc-queue-pw-1")
	resp = getJSON(t, srv, "/v1/admin/kyc/cases", supportTokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for support listing the KYC queue, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/kyc/cases", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token listing the KYC queue, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 18. Cross-tenant: tenant B's compliance staff cannot see tenant A's
// KYC cases in the tenant-wide queue ---

func TestKYCCases_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, _, _ := newKYCTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	postJSON(t, srv, "/v1/me/kyc/verifications", playerA.Tokens.AccessToken, map[string]any{}).Body.Close()

	tenantB := mustCreateTenant(t, pool)
	complianceB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-kyc-queue-crosstenant-pw-1")
	complianceBTokens := mustLoginStaff(t, srv, tenantB.Slug, complianceB.Email, "compliance-kyc-queue-crosstenant-pw-1")

	resp := getJSON(t, srv, "/v1/admin/kyc/cases", complianceBTokens.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tenant B listing its own (empty) queue, got %d", resp.StatusCode)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	decodeBody(t, resp, &page)
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("expected tenant B to see zero KYC cases for tenant A's player, got %+v", page)
	}
}

// extractToken pulls the raw token out of a mock email body (the mock
// handlers embed it as "...: <token>" - see credential_handlers.go).
func extractToken(t *testing.T, body string) string {
	t.Helper()
	idx := bytes.LastIndexByte([]byte(body), ' ')
	if idx == -1 || idx == len(body)-1 {
		t.Fatalf("failed to extract token from email body: %q", body)
	}
	return body[idx+1:]
}
