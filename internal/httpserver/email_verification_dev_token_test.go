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

// --- Stage 9.3 account-activation dev-token test fixtures ---
//
// Sibling to payment_deposit_simulation_test.go's own fixtures - reuses
// mustCreateTenant/mustCreateBrand/mustRegisterPlayer/mustRegisterCapability/
// postJSON/getJSON/decodeBody/decodeAPIError from the existing test files in
// this package, and follows the same adversarial-suite shape: happy path,
// wrong-caller denial, disabled-outside-non-production, staff-token denial,
// and an already-confirmed/stale-token case.

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

// requestEmailVerification calls the existing, unmodified
// POST /v1/me/email-verification/request endpoint for player - the real
// entry point that mints a token today; nothing in this file touches it.
func requestEmailVerification(t *testing.T, srv *httptest.Server, accessToken string) {
	t.Helper()
	resp := postJSON(t, srv, "/v1/me/email-verification/request", accessToken, map[string]any{})
	if resp.StatusCode != 204 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 204 requesting email verification, got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()
}

type devTokenResponse struct {
	Token string `json:"token"`
}

// --- 1. Full happy path: register -> fetch dev token -> confirm (the
// REAL, unmodified confirm endpoint) -> status becomes active -> a
// previously RG-blocked financial action (deposit initiation) now
// succeeds end to end (bonus assertion). ---

func TestAccountActivationDevToken_HappyPath_RegisterFetchConfirmActivatesAndUnblocksDeposit(t *testing.T) {
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

	// Before requesting a token at all, there is nothing to fetch.
	resp = getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 fetching a dev-token before any was requested, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	requestEmailVerification(t, srv, player.Tokens.AccessToken)

	resp = getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	if resp.StatusCode != 200 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200 fetching the dev-token, got %d (%s)", resp.StatusCode, body.Message)
	}
	var tok devTokenResponse
	decodeBody(t, resp, &tok)
	if tok.Token == "" {
		t.Fatal("expected a non-empty raw token")
	}

	// Feed it into the REAL, UNMODIFIED confirm endpoint - this proves the
	// dev-token route produces a token the genuine flow actually accepts,
	// not a parallel/competing mechanism.
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": tok.Token})
	if resp.StatusCode != 204 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 204 confirming with the fetched dev-token, got %d (%s)", resp.StatusCode, body.Message)
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

// --- 2. A different player cannot fetch someone else's token - there is
// no id parameter to name another player's resource with at all, so this
// asserts the caller only ever sees THEIR OWN (empty) result. ---

func TestAccountActivationDevToken_WrongPlayerNeverSeesAnothersToken(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	victim := mustRegisterPlayer(t, srv, brand.Slug)
	attacker := mustRegisterPlayer(t, srv, brand.Slug)

	requestEmailVerification(t, srv, victim.Tokens.AccessToken)

	// The victim's own token is genuinely available to the victim.
	resp := getJSON(t, srv, "/v1/me/email-verification/dev-token", victim.Tokens.AccessToken)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for the victim fetching their own dev-token, got %d", resp.StatusCode)
	}
	var victimTok devTokenResponse
	decodeBody(t, resp, &victimTok)

	// The attacker, authenticated as themselves (there is no id parameter
	// to substitute the victim's identity into), never requested a token
	// of their own, so they see a clean 404 - never the victim's value.
	resp = getJSON(t, srv, "/v1/me/email-verification/dev-token", attacker.Tokens.AccessToken)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for the attacker (no token of their own requested), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Even once the attacker requests their OWN token, it is a DIFFERENT
	// value from the victim's.
	requestEmailVerification(t, srv, attacker.Tokens.AccessToken)
	resp = getJSON(t, srv, "/v1/me/email-verification/dev-token", attacker.Tokens.AccessToken)
	var attackerTok devTokenResponse
	decodeBody(t, resp, &attackerTok)
	if attackerTok.Token == victimTok.Token {
		t.Fatal("the attacker's own dev-token must never equal the victim's")
	}

	// The victim's own token, fed into the real confirm endpoint, still
	// only ever activates the VICTIM's account.
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": victimTok.Token})
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
}

// --- 3. Disabled outside non-production: the route does not exist at all
// when AccountActivationTestSupportEnabled is false. ---

func TestAccountActivationDevToken_DisabledWhenFlagOff(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, false)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	requestEmailVerification(t, srv, player.Tokens.AccessToken)

	resp := getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 (route not registered) when AccountActivationTestSupportEnabled is false, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 4. A STAFF bearer token must not reach this player-self-service
// route. ---

func TestAccountActivationDevToken_StaffTokenDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	requestEmailVerification(t, srv, player.Tokens.AccessToken)

	staffToken, err := issuer.Issue(uuid.NewString(), tenant.ID, auth.RoleTenantAdmin, auth.PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("issue staff token: %v", err)
	}
	resp := getJSON(t, srv, "/v1/me/email-verification/dev-token", staffToken)
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 for a staff bearer token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 5. Already-confirmed account: no stale/replayed-token issue. Once
// the real confirm endpoint has consumed the token, the dev-token route
// must not keep handing back a value that would only fail later - it
// re-verifies liveness against the database rather than trusting its own
// in-memory copy. ---

func TestAccountActivationDevToken_AlreadyConfirmedReturnsCleanNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	requestEmailVerification(t, srv, player.Tokens.AccessToken)

	resp := getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	var tok devTokenResponse
	decodeBody(t, resp, &tok)

	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": tok.Token})
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204 confirming, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The dev-token route must now report cleanly that nothing is
	// pending, rather than serving the stale (already-consumed) raw value.
	resp = getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for an already-confirmed account, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The already-consumed token must also not work a second time against
	// the real confirm endpoint (single-use, unweakened by any of this).
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": tok.Token})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 replaying an already-consumed token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 6. A player who requests a SECOND verification token must only ever
// be able to fetch the newest one - the superseded first token must not
// remain fetchable or usable, mirroring IssueCredentialToken's own
// "at most one live token per purpose" invariant. ---

func TestAccountActivationDevToken_OnlyNewestTokenIsFetchableAfterResend(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newAccountActivationTestServer(t, pool, issuer, orchestrator, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	requestEmailVerification(t, srv, player.Tokens.AccessToken)

	resp := getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	var first devTokenResponse
	decodeBody(t, resp, &first)

	requestEmailVerification(t, srv, player.Tokens.AccessToken)

	resp = getJSON(t, srv, "/v1/me/email-verification/dev-token", player.Tokens.AccessToken)
	var second devTokenResponse
	decodeBody(t, resp, &second)
	if second.Token == first.Token {
		t.Fatal("expected a resend to supersede the first token with a distinct value")
	}

	// The first (superseded) token must no longer be accepted by the real
	// confirm endpoint.
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": first.Token})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 confirming with the superseded first token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The current (second) token still works.
	resp = postJSON(t, srv, "/v1/auth/email-verification/confirm", "", map[string]any{"token": second.Token})
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204 confirming with the current second token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
