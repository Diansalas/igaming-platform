//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103) HTTP-layer tests for architect review
// findings F-2 and F-6 (docs/plans/prh2-hardening-round/reviews/
// b-architect-qa-pop.md):
//
//   - F-2: "The HTTP mapping is untested: no test covers a step-3 refusal,
//     a replay mismatch, a wrong signature (the uniform 401 body, BS-8),
//     or the constant 403." Every one of those is exercised below, each
//     asserting the SAME uniform 401 body ("callback rejected") for the
//     three refusal shapes and a wrong signature, and the SAME constant
//     403 body ("launch not permitted") for a gate denial - never a
//     reason-specific message, per ADR 0103 §3.6/BS-8.
//   - F-6: "The C-103-5 secrecy scan covers only the success audit."
//     Every path below captures the server's own log output (the same
//     newCapturingLogger harness casino_webhook_auth_failure_logging_
//     integration_test.go uses) and scans every log line's message AND
//     every attribute value for the raw launch token and its sha256 hash,
//     not just the audit table.
package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// hashLaunchTokenForTest is this package's own copy of internal/casino's
// unexported hashLaunchToken - test helpers are not shared across package
// boundaries in this codebase (see extractLaunchToken's own doc comment).
func hashLaunchTokenForTest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// assertNoLogLineLeaksSecret scans every captured log line's message and
// every attribute's string form for needle (the raw token or its hash) -
// F-6's own scope: "every new path", not just the success audit.
func assertNoLogLineLeaksSecret(t *testing.T, lines []capturedLogLine, needle, label string) {
	t.Helper()
	if needle == "" {
		t.Fatalf("test bug: needle for %s must not be empty", label)
	}
	for _, l := range lines {
		if strings.Contains(l.msg, needle) {
			t.Fatalf("log line %q leaked the %s in its message", l.msg, label)
		}
		for k, v := range l.attrs {
			if s := fmt.Sprintf("%v", v); strings.Contains(s, needle) {
				t.Fatalf("log line %q attribute %q leaked the %s: %q", l.msg, k, label, s)
			}
		}
	}
}

// bootstrapLoggingFixture is the shared setup every sub-test below needs:
// a tenant/brand/player with a funded wallet and an enabled game, plus a
// capturing logger wired into the real HTTP server.
type bootstrapLoggingFixture struct {
	pool   *db.Pool
	srv    *httptest.Server
	tenant identity.Tenant
	brand  identity.Brand
	player registeredPlayer
	game   casino.Game
	mock   *casino.MockCasinoProvider
	logs   func() []capturedLogLine
}

func setupBootstrapLoggingFixture(t *testing.T) *bootstrapLoggingFixture {
	t.Helper()
	pool, issuer := testEnv(t)
	orch, mock := newMockCasinoOrchestrator()
	logger, captured := newCapturingLogger()
	srv := newCasinoTestServerWithLogger(t, pool, issuer, orch, logger)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10000)

	return &bootstrapLoggingFixture{
		pool: pool, srv: srv, tenant: tenant, brand: brand, player: player,
		game: game, mock: mock, logs: captured,
	}
}

func (f *bootstrapLoggingFixture) launch(t *testing.T) (sessionID uuid.UUID, token string) {
	t.Helper()
	launched := mustLaunchCasinoGame(t, f.srv, f.player.Tokens.AccessToken, f.game.ID.String(), "EUR", "real")
	id, err := uuid.Parse(launched.SessionID)
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	return id, extractLaunchToken(t, launched.LaunchURL)
}

func (f *bootstrapLoggingFixture) sessionStatus(t *testing.T, sessionID uuid.UUID) string {
	t.Helper()
	var status string
	err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	return status
}

// TestCasinoBootstrap_HTTP_WrongSignature_UniformUnauthorized is F-2's
// "a wrong signature" case: a genuinely-shaped, correctly-addressed
// request whose signature header is corrupted, as opposed to
// TestCasinoBootstrap_UnsignedRequest_UniformUnauthorized's "no signature
// at all" case.
func TestCasinoBootstrap_HTTP_WrongSignature_UniformUnauthorized(t *testing.T) {
	f := setupBootstrapLoggingFixture(t)
	sessionID, token := f.launch(t)
	in := f.mock.BootstrapPayload(f.tenant.ID, token, "req-f2-wrongsig-1", f.game.ProviderGameID, "EUR", "real")
	in.Header.Set(webhookauth.CasinoSignatureHeader, "v1="+"0000000000000000000000000000000000000000000000000000000000000000"[:64])

	resp := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", apiErr.Message)
	}
	tokenHash := hashLaunchTokenForTest(token)
	assertNoLogLineLeaksSecret(t, f.logs(), token, "raw launch token")
	assertNoLogLineLeaksSecret(t, f.logs(), tokenHash, "launch token hash")
	if f.sessionStatus(t, sessionID) != "active" {
		t.Fatalf("a wrong-signature request must never consume the session")
	}
}

// TestCasinoBootstrap_HTTP_Step3BindingMismatch_UniformUnauthorized is
// F-2's "a step-3 refusal" case: a genuinely signed, correctly verified
// request whose body names a provider_game_id that does not match the
// session's own binding - ADR 0103 §3.2 step 3's own refusal, which must
// produce the SAME uniform 401 as a pre-verification failure, never a
// distinguishing message or status.
func TestCasinoBootstrap_HTTP_Step3BindingMismatch_UniformUnauthorized(t *testing.T) {
	f := setupBootstrapLoggingFixture(t)
	sessionID, token := f.launch(t)
	in := f.mock.BootstrapPayload(f.tenant.ID, token, "req-f2-step3-1", "wrong-provider-game-id", "EUR", "real")

	resp := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", apiErr.Message)
	}
	tokenHash := hashLaunchTokenForTest(token)
	assertNoLogLineLeaksSecret(t, f.logs(), token, "raw launch token")
	assertNoLogLineLeaksSecret(t, f.logs(), tokenHash, "launch token hash")
	if f.sessionStatus(t, sessionID) != "active" {
		t.Fatalf("a step-3 binding mismatch must never consume the session")
	}
}

// TestCasinoBootstrap_HTTP_ReplayMismatch_UniformUnauthorized is F-2's
// "a replay mismatch" case: the SAME request_id reused against a
// DIFFERENT launch token (a different session), which must be refused
// with the same uniform 401 rather than treated as a replay of the first
// token's own response.
func TestCasinoBootstrap_HTTP_ReplayMismatch_UniformUnauthorized(t *testing.T) {
	f := setupBootstrapLoggingFixture(t)
	_, tokenA := f.launch(t)
	sessionB, tokenB := f.launch(t)

	const sharedRequestID = "req-f2-replaymismatch-1"
	inA := f.mock.BootstrapPayload(f.tenant.ID, tokenA, sharedRequestID, f.game.ProviderGameID, "EUR", "real")
	respA := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, inA)
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("expected the first bootstrap (token A) to succeed, got %d", respA.StatusCode)
	}

	inB := f.mock.BootstrapPayload(f.tenant.ID, tokenB, sharedRequestID, f.game.ProviderGameID, "EUR", "real")
	respB := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, inB)
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the second bootstrap (token B, same request_id) to be refused with 401, got %d", respB.StatusCode)
	}
	apiErr := decodeAPIError(t, respB)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", apiErr.Message)
	}

	tokenHashA := hashLaunchTokenForTest(tokenA)
	tokenHashB := hashLaunchTokenForTest(tokenB)
	for _, secret := range []struct{ needle, label string }{
		{tokenA, "raw launch token A"}, {tokenB, "raw launch token B"},
		{tokenHashA, "launch token A hash"}, {tokenHashB, "launch token B hash"},
	} {
		assertNoLogLineLeaksSecret(t, f.logs(), secret.needle, secret.label)
	}
	// Token B's own session must never have been consumed by the losing
	// attempt - this workstream's own "refusal never consumes" invariant,
	// now also pinned over HTTP for the replay-mismatch shape specifically.
	if f.sessionStatus(t, sessionB) != "active" {
		t.Fatalf("a replay-mismatch refusal must never consume the LOSING token's own session")
	}
}

// TestCasinoBootstrap_HTTP_GateDenial_ConstantForbidden is F-2's "the
// constant 403" case: an RG-ineligible (self-excluded) player produces the
// SAME 403 body ("launch not permitted") a game-inactive or
// capability-denied gate denial would, never a reason-specific message
// (ADR 0103 §3.3 - the reason is server-side audit/log only, RG status
// being player-sensitive).
func TestCasinoBootstrap_HTTP_GateDenial_ConstantForbidden(t *testing.T) {
	f := setupBootstrapLoggingFixture(t)
	sessionID, token := f.launch(t)

	selfExclResp := postJSON(t, f.srv, "/v1/me/rg/self-exclusion", f.player.Tokens.AccessToken, map[string]any{})
	defer selfExclResp.Body.Close()
	if selfExclResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 self-excluding the player, got %d", selfExclResp.StatusCode)
	}

	in := f.mock.BootstrapPayload(f.tenant.ID, token, "req-f2-denial-1", f.game.ProviderGameID, "EUR", "real")
	resp := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an RG-ineligible player, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message != "launch not permitted" {
		t.Fatalf("expected the constant 'launch not permitted' message, got %q", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, "rg") || strings.Contains(apiErr.Message, "exclu") {
		t.Fatalf("the 403 body must never disclose the RG reason: %q", apiErr.Message)
	}

	tokenHash := hashLaunchTokenForTest(token)
	assertNoLogLineLeaksSecret(t, f.logs(), token, "raw launch token")
	assertNoLogLineLeaksSecret(t, f.logs(), tokenHash, "launch token hash")
	if f.sessionStatus(t, sessionID) != "revoked" {
		t.Fatalf("expected a definitive gate denial to revoke the session, got %q", f.sessionStatus(t, sessionID))
	}
}

// TestCasinoBootstrap_HTTP_ReplaySuccess_NoSecretInLogs is L-1 (security
// review of e810ece, docs/plans/prh2-hardening-round/reviews/
// b-security.md): the earlier secrecy scan covered the FRESH success path
// (casino.launch_bootstrapped) but not the separate replay-success Info
// log line (`logger.Info("casino_bootstrap_replayed", ...)`,
// casino_bootstrap_handlers.go), which only fires on a genuine
// byte-identical replay, never a fresh bootstrap.
func TestCasinoBootstrap_HTTP_ReplaySuccess_NoSecretInLogs(t *testing.T) {
	f := setupBootstrapLoggingFixture(t)
	_, token := f.launch(t)

	const requestID = "req-l1-replay-1"
	in := f.mock.BootstrapPayload(f.tenant.ID, token, requestID, f.game.ProviderGameID, "EUR", "real")
	first := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, in)
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected the first bootstrap to succeed, got %d", first.StatusCode)
	}

	// The exact same request, byte-identical - the genuine replay.
	replayIn := f.mock.BootstrapPayload(f.tenant.ID, token, requestID, f.game.ProviderGameID, "EUR", "real")
	second := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, replayIn)
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected the replay to also return 200, got %d", second.StatusCode)
	}

	var found bool
	for _, l := range f.logs() {
		if l.msg == "casino_bootstrap_replayed" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a casino_bootstrap_replayed log line for the genuine replay")
	}

	tokenHash := hashLaunchTokenForTest(token)
	assertNoLogLineLeaksSecret(t, f.logs(), token, "raw launch token")
	assertNoLogLineLeaksSecret(t, f.logs(), tokenHash, "launch token hash")
}

// TestCasinoBootstrap_HTTP_Success_NoSecretInLogs closes F-6's own gap
// directly: the success path's log line(s), not merely the audit row
// TestBootstrapLaunch_AuditNeverCarriesTokenOrHash already covers at the
// domain layer.
func TestCasinoBootstrap_HTTP_Success_NoSecretInLogs(t *testing.T) {
	f := setupBootstrapLoggingFixture(t)
	_, token := f.launch(t)
	in := f.mock.BootstrapPayload(f.tenant.ID, token, "req-f6-success-1", f.game.ProviderGameID, "EUR", "real")
	resp := rawPostCasinoBootstrap(t, f.srv, f.tenant.Slug, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	tokenHash := hashLaunchTokenForTest(token)
	assertNoLogLineLeaksSecret(t, f.logs(), token, "raw launch token")
	assertNoLogLineLeaksSecret(t, f.logs(), tokenHash, "launch token hash")
}
