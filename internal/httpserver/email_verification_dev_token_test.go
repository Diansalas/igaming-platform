//go:build integration

package httpserver

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// --- Stage 9.4 account-activation test-support tests ---
//
// Stage 9.3 shipped a separate GET /v1/me/email-verification/dev-token
// route backed by an in-memory, per-process store. A Stage 9.3 security
// review diagnosed that store as not safe under a multi-replica
// deployment: a token recorded in one replica's memory is invisible to a
// request that lands on a different replica. Stage 9.4 replaces that
// design entirely - see newRequestEmailVerificationHandler's own doc
// comment (credential_handlers.go) and Deps.AccountActivationTestSupportEnabled's
// own doc comment (server.go). These tests replace the old dev-token
// route tests; the file keeps its original name for history/diff
// locality, but every test body below exercises the new, stateless shape.
//
// Sibling to payment_deposit_simulation_test.go's own fixtures - reuses
// mustCreateTenant/mustCreateBrand/mustRegisterPlayer/mustRegisterCapability/
// postJSON/getJSON/decodeBody/decodeAPIError from the existing test files in
// this package.

// newAccountActivationTestServer wires every Deps field
// TestAccountActivationDevToken_* needs: an EmailProvider (so
// POST /v1/me/email-verification/request actually issues a token instead of
// 503ing), AccountActivationTestSupportEnabled, and - for the bonus
// financial-unblock assertion - a PaymentOrchestrator plus
// PaymentsMockSettlementEnabled, mirroring newFinancialTestServerWithMockSettlement's
// identical "one simulation flag on top of the base test server" pattern.
func newAccountActivationTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *payments.Orchestrator, activationEnabled bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:                              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                                  pool,
		AuthIssuer:                          issuer,
		ServiceName:                         "platform-api-test",
		AccessTokenTTL:                      5 * time.Minute,
		RefreshTokenTTL:                     time.Hour,
		PersonResolver:                      identityresolution.NewMockPersonResolver(),
		EmailProvider:                       email.NewMockProvider(),
		AccountActivationTestSupportEnabled: activationEnabled,
		PaymentOrchestrator:                 orchestrator,
		PaymentsMockSettlementEnabled:       activationEnabled,
	}))
	t.Cleanup(srv.Close)
	return srv
}

type devTokenResponse struct {
	Token string `json:"token"`
}

// requestEmailVerificationAndCaptureToken calls the existing, unmodified
// POST /v1/me/email-verification/request endpoint but
// asserts the AccountActivationTestSupportEnabled=true shape: 200 with a
// non-empty token in the body.
func requestEmailVerificationAndCaptureToken(t *testing.T, srv *httptest.Server, accessToken string) string {
	t.Helper()
	resp := postJSON(t, srv, "/v1/me/email-verification/request", accessToken, map[string]any{})
	if resp.StatusCode != 200 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200 requesting email verification with test support enabled, got %d (%s)", resp.StatusCode, body.Message)
	}
	var tok devTokenResponse
	decodeBody(t, resp, &tok)
	if tok.Token == "" {
		t.Fatal("expected a non-empty raw token in the response body")
	}
	return tok.Token
}

// --- 1. Full happy path: register -> request (token in the SAME response)
// -> confirm (the REAL, unmodified confirm endpoint) -> status becomes
// active -> a previously RG-blocked financial action (deposit initiation)
// now succeeds end to end (bonus assertion). ---

func TestAccountActivationDevToken_HappyPath_RegisterRequestConfirmActivatesAndUnblocksDeposit(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)

	// Freshly registered: pending_verification, per
	// internal/identity/player_account.go's own default.
	resp := getJSON(t, srv, "/v1/me", player.Tokens.AccessToken)
	var me meResponse
	decodeBody(t, resp, &me)
	if me.Status != "pending_verification" {
		t.Fatalf("expected freshly registered status 'pending_verification', got %q", me.Status)
	}

	// The gate this whole gap is about: a deposit attempt right now is
	// RG-denied (declined, not activated) - proves the "before" state is
	// real, not merely asserted by the task description.
	resp = postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 5000, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 creating the pre-activation deposit intent, got %d", resp.StatusCode)
	}
	var preActivationIntent depositIntentResponse
	decodeBody(t, resp, &preActivationIntent)
	if preActivationIntent.Status != "declined" {
		t.Fatalf("expected the pre-activation deposit to be RG-declined, got status %q", preActivationIntent.Status)
	}

	// Request email verification: with test support enabled, the raw
	// token comes back in THIS SAME response - no separate fetch route
	// exists anymore.
	token := requestEmailVerificationAndCaptureToken(t, srv, player.Tokens.AccessToken)

	// Feed it into the REAL, UNMODIFIED confirm endpoint - this proves the
	// request endpoint's token response is accepted by the genuine flow,
	// not a parallel/competing mechanism.
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": token})
	if resp.StatusCode != 204 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 204 confirming with the returned token, got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me", player.Tokens.AccessToken)
	decodeBody(t, resp, &me)
	if me.Status != "active" {
		t.Fatalf("expected status 'active' after confirming, got %q", me.Status)
	}

	// Bonus assertion: the SAME financial action that was RG-declined
	// above now proceeds to a real, settleable pending deposit.
	resp = postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 6000, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 creating the post-activation deposit intent, got %d", resp.StatusCode)
	}
	var postActivationIntent depositIntentResponse
	decodeBody(t, resp, &postActivationIntent)
	if postActivationIntent.Status != "pending" {
		t.Fatalf("expected the post-activation deposit to be 'pending' (no longer RG-declined), got %q", postActivationIntent.Status)
	}

	resp = postJSON(t, srv, "/v1/me/deposits/"+postActivationIntent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 200 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200 settling the post-activation deposit, got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 reading the wallet after settlement, got %d", resp.StatusCode)
	}
	var wallet walletSummaryResponse
	decodeBody(t, resp, &wallet)
	if wallet.CashBalance != 6000 {
		t.Fatalf("expected a real credited cash balance of 6000 for the now-active player, got %d", wallet.CashBalance)
	}
}

// --- 2. The response body only ever reflects the CALLER's own
// newly-created token - never another player's. There is no longer a
// shared store to query cross-player at all (trivially true by
// construction), but this proves it end to end: two different players'
// own request calls return two different tokens, and each only ever
// activates its own account. ---

func TestAccountActivationDevToken_ResponseOnlyEverReflectsCallersOwnToken(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	victim := mustRegisterPlayer(t, srv, brand.Slug)
	attacker := mustRegisterPlayer(t, srv, brand.Slug)

	victimToken := requestEmailVerificationAndCaptureToken(t, srv, victim.Tokens.AccessToken)
	attackerToken := requestEmailVerificationAndCaptureToken(t, srv, attacker.Tokens.AccessToken)

	if attackerToken == victimToken {
		t.Fatal("the attacker's own token response must never equal the victim's")
	}

	// The victim's own token, fed into the real confirm endpoint, still
	// only ever activates the VICTIM's account.
	resp := postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": victimToken})
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204 confirming the victim's own token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	var me meResponse
	resp = getJSON(t, srv, "/v1/me", attacker.Tokens.AccessToken)
	decodeBody(t, resp, &me)
	if me.Status == "active" {
		t.Fatal("the victim's confirmed token must not have activated the attacker's account")
	}

	resp = getJSON(t, srv, "/v1/me", victim.Tokens.AccessToken)
	decodeBody(t, resp, &me)
	if me.Status != "active" {
		t.Fatal("the victim's own confirm must have activated the victim's own account")
	}
}

// --- 3. Flag-off behavior is byte-for-byte unchanged: 204, no body. This
// diffs the response shape explicitly rather than merely checking the
// status code, closing the directive's "verify this with a test that
// diffs the response shape" requirement. ---

func TestAccountActivationDevToken_FlagOffResponseUnchanged(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, false)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/email-verification/request", player.Tokens.AccessToken, map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204 with test support disabled, got %d", resp.StatusCode)
	}
	if cl := resp.ContentLength; cl > 0 {
		t.Fatalf("expected no body with test support disabled, got Content-Length %d", cl)
	}
	buf := make([]byte, 1)
	n, _ := resp.Body.Read(buf)
	if n != 0 {
		t.Fatalf("expected zero bytes of body with test support disabled, read %d", n)
	}
}

// --- 4. The old GET /v1/me/email-verification/dev-token route is gone
// entirely - never routed, regardless of the flag. ---

func TestAccountActivationDevToken_OldRouteNoLongerExists(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	requestEmailVerificationAndCaptureToken(t, srv, player.Tokens.AccessToken)

	resp := getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 - the old dev-token route must no longer be routed at all, got %d", resp.StatusCode)
	}
}

// --- 5. A resend (second request call) returns the NEW token, and only
// the new one still confirms - exercises the exact "only newest token
// works" property IssueCredentialToken's supersession invariant
// guarantees, now observed via the response body instead of a separate
// fetch route. ---

func TestAccountActivationDevToken_ResendReturnsNewTokenOnlyNewOneConfirms(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)

	first := requestEmailVerificationAndCaptureToken(t, srv, player.Tokens.AccessToken)
	second := requestEmailVerificationAndCaptureToken(t, srv, player.Tokens.AccessToken)

	if second == first {
		t.Fatal("expected a resend to supersede the first token with a distinct value")
	}

	// The first (superseded) token must no longer be accepted by the real
	// confirm endpoint.
	resp := postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": first})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 confirming with the superseded first token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The current (second) token still works.
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": second})
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204 confirming with the current second token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 6. THE multi-replica test. Proves what the stateless redesign
// claims: two SEPARATE httpserver.New(...) instances (each its own fresh
// Deps, no shared in-process state whatsoever - the literal shape of two
// independent replica processes, sharing only the same Postgres) can
// split the request/confirm flow across them and it still works, because
// there is no longer a second request in the flow that could ever land on
// "the other" replica - the token travels in the SAME response as the
// request that minted it, and the confirm step's own correctness is
// already backed by the shared, replica-safe player_credential_tokens
// table.
//
// This is the literal "request 1 reaches replica A, request 2 reaches
// replica B" scenario the old in-memory devVerificationTokenStore could
// fail under (a token recorded in replica A's memory would 404 if the
// follow-up GET landed on replica B) - and it is now structurally
// impossible to reproduce that failure, because there is no follow-up GET
// at all. ---

func TestAccountActivationDevToken_MultiReplica_RequestOnReplicaA_ConfirmOnReplicaB(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()

	// Two INDEPENDENT httpserver.New(...) calls, each with its OWN zero
	// value / freshly-constructed Deps - simulating two separate replica
	// processes. Neither is built from the other, and neither shares any
	// Go-level state with the other; the only thing they have in common
	// is the same *db.Pool (the same Postgres, exactly as two real
	// replica processes would share the same database and nothing else).
	replicaA := httptest.NewServer(New(Deps{
		Logger:                              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                                  pool,
		AuthIssuer:                          issuer,
		ServiceName:                         "platform-api-test-replica-a",
		AccessTokenTTL:                      5 * time.Minute,
		RefreshTokenTTL:                     time.Hour,
		PersonResolver:                      identityresolution.NewMockPersonResolver(),
		EmailProvider:                       email.NewMockProvider(),
		AccountActivationTestSupportEnabled: true,
		PaymentOrchestrator:                 orchestrator,
	}))
	t.Cleanup(replicaA.Close)

	replicaB := httptest.NewServer(New(Deps{
		Logger:                              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                                  pool,
		AuthIssuer:                          issuer,
		ServiceName:                         "platform-api-test-replica-b",
		AccessTokenTTL:                      5 * time.Minute,
		RefreshTokenTTL:                     time.Hour,
		PersonResolver:                      identityresolution.NewMockPersonResolver(),
		EmailProvider:                       email.NewMockProvider(),
		AccountActivationTestSupportEnabled: true,
		PaymentOrchestrator:                 orchestrator,
	}))
	t.Cleanup(replicaB.Close)

	// Registration itself can happen on either replica - use replicaA for
	// everything up to and including the request call.
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, replicaA, brand.Slug)

	// Request 1 (the ONLY request that ever mints/exposes the token) is
	// handled by replica A.
	token := requestEmailVerificationAndCaptureToken(t, replicaA, player.Tokens.AccessToken)

	// Confirm is handled by replica B - a SEPARATE httpserver.New call,
	// proving no in-process state is shared. If any replica-local state
	// were involved in this flow, this call would have no way to succeed
	// (replica B never ran the request handler that minted this token).
	resp := postJSON(t, replicaB, "/v1/auth/email-verification/confirm", "", map[string]any{"token": token})
	if resp.StatusCode != 204 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 204 confirming on a DIFFERENT replica than the one that issued the request, got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()

	// The account is active - readable from EITHER replica, since both
	// only ever read the shared, authoritative Postgres state.
	var me meResponse
	resp = getJSON(t, replicaA, "/v1/me", player.Tokens.AccessToken)
	decodeBody(t, resp, &me)
	if me.Status != "active" {
		t.Fatalf("expected status 'active' reading from replica A after confirming on replica B, got %q", me.Status)
	}

	resp = getJSON(t, replicaB, "/v1/me", player.Tokens.AccessToken)
	decodeBody(t, resp, &me)
	if me.Status != "active" {
		t.Fatalf("expected status 'active' reading from replica B, got %q", me.Status)
	}
}
