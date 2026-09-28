//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103). Domain-level tests for
// Orchestrator.BootstrapLaunch, driven directly (VerifyCallback then
// BootstrapLaunch, exactly the two-phase shape the HTTP handler itself
// uses) rather than over HTTP - the "MOCK vendor goes over HTTP" property
// itself is proven separately, at the HTTP layer
// (internal/httpserver/casino_bootstrap_integration_test.go), which is the
// only place a real net/http round trip belongs; these tests exist to
// pin BootstrapLaunch's own contract precisely and to carry the mutation
// evidence.
package casino

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mintSessionForBootstrap creates a genuine 'active' casino_launch_sessions
// row (bypassing LaunchGame's own gates, exactly like migration_0108's own
// seedLaunchSessionAtStatus - the gates BootstrapLaunch itself re-checks
// are what these tests exercise, not LaunchGame's).
func mintSessionForBootstrap(t *testing.T, pool *db.Pool, f casinoFixture, game Game, mode GameMode, assetCode string, ttl time.Duration) (LaunchSession, string) {
	t.Helper()
	var session LaunchSession
	var token string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		session, token, err = CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
			AssetCode: assetCode, Mode: mode, TTL: ttl,
		})
		return err
	})
	if err != nil {
		t.Fatalf("mint session for bootstrap: %v", err)
	}
	return session, token
}

// bootstrapVerified runs phase 1 (VerifyCallback) for a MOCK bootstrap
// payload - the SAME two-phase entry the HTTP handler uses, just without
// the actual HTTP hop (that hop is what the httpserver-level tests
// exercise).
func bootstrapVerified(t *testing.T, orch *Orchestrator, pool *db.Pool, tenantID uuid.UUID, providerID string, in webhookauth.Inbound) *VerifiedCallback {
	t.Helper()
	v, err := orch.VerifyCallback(context.Background(), pool, tenantID, providerID, in)
	if err != nil {
		t.Fatalf("verify bootstrap payload: %v", err)
	}
	return v
}

func setupBootstrapFixture(t *testing.T) (*db.Pool, casinoFixture, Game, *MockCasinoProvider, *Orchestrator) {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 10000)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))
	return pool, f, game, provider, orch
}

// --- Success (fresh) ---

func TestBootstrapLaunch_FreshSuccess(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in := provider.BootstrapPayload(f.tenantID, token, "req-fresh-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)

	result, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("bootstrap launch: %v", err)
	}
	if result.Denied || result.Replayed {
		t.Fatalf("expected a fresh success, got %+v", result)
	}
	if result.SessionID != session.ID {
		t.Fatalf("expected session id %s, got %s", session.ID, result.SessionID)
	}
	if result.PlayerRef == uuid.Nil {
		t.Fatal("expected a non-nil player ref")
	}

	var resp map[string]any
	if err := json.Unmarshal(result.ResponseJSON, &resp); err != nil {
		t.Fatalf("parse response json: %v", err)
	}
	for _, key := range []string{"session_id", "player_ref", "provider_game_id", "asset_code", "mode"} {
		if _, ok := resp[key]; !ok {
			t.Fatalf("expected response to carry %q, got %s", key, result.ResponseJSON)
		}
	}

	var status LaunchSessionStatus
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, session.ID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != LaunchSessionConsumed {
		t.Fatalf("expected the session consumed, got %q", status)
	}

	if !auditActionExists(t, pool, f.tenantID, "casino.launch_bootstrapped") {
		t.Fatal("expected a casino.launch_bootstrapped audit record")
	}
}

// --- AU / secrecy: the raw token and its hash never appear in the audit row ---

func TestBootstrapLaunch_AuditNeverCarriesTokenOrHash(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	tokenHash := hashLaunchToken(token)

	in := provider.BootstrapPayload(f.tenantID, token, "req-au-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v); err != nil {
		t.Fatalf("bootstrap launch: %v", err)
	}

	var metadataJSON []byte
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'casino.launch_bootstrapped' ORDER BY created_at DESC LIMIT 1`,
			f.tenantID).Scan(&metadataJSON)
	})
	if err != nil {
		t.Fatalf("read audit metadata: %v", err)
	}
	metadataStr := string(metadataJSON)
	if containsSubstring(metadataStr, token) || containsSubstring(metadataStr, tokenHash) {
		t.Fatalf("audit metadata leaked the token or its hash: %s", metadataStr)
	}
}

func containsSubstring(haystack, needle string) bool {
	return needle != "" && (len(haystack) >= len(needle)) && (indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// --- ADV: each returns the uniform refusal with the session unchanged and
// nothing written ---

func assertSessionUntouchedAndNoBootstrapRow(t *testing.T, pool *db.Pool, f casinoFixture, sessionID uuid.UUID, wantStatus LaunchSessionStatus) {
	t.Helper()
	var status LaunchSessionStatus
	var bootstrapCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, sessionID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("read session/bootstrap state: %v", err)
	}
	if status != wantStatus {
		t.Fatalf("expected session status %q (unchanged), got %q", wantStatus, status)
	}
	if bootstrapCount != 0 {
		t.Fatalf("expected no bootstrap row for session %s, got %d", sessionID, bootstrapCount)
	}
}

func TestBootstrapLaunch_ADV_CrossProvider(t *testing.T) {
	pool, f, game, _, _ := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	other := NewMockCasinoProvider("mock-casino-2", "EUR")
	registerCasinoCapability(t, pool, f, other, 100)
	orch2 := NewOrchestrator(map[string]CasinoProvider{"mock-casino-2": other}, NewMockWebhookCredentials(other))

	// Same token, but bootstrapped as if it were mock-casino-2's own -
	// resolves the session (token_hash is globally unique, no provider
	// filter at step 2b), then step 3's binding check refuses it.
	in := other.BootstrapPayload(f.tenantID, token, "req-xprov-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch2, pool, f.tenantID, "mock-casino-2", in)
	_, err := orch2.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino-2", v)
	requireBootstrapRefused(t, err, BootstrapRefusalBindingMismatch)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)
}

func requireBootstrapRefused(t *testing.T, err error, want BootstrapRefusalReason) {
	t.Helper()
	var refused *BootstrapRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected a *BootstrapRefusedError, got %T: %v", err, err)
	}
	if refused.Reason != want {
		t.Fatalf("expected refusal reason %q, got %q", want, refused.Reason)
	}
	if !errors.Is(err, ErrBootstrapRefused) {
		t.Fatal("expected errors.Is(err, ErrBootstrapRefused) to hold")
	}
}

func TestBootstrapLaunch_ADV_CrossTenant(t *testing.T) {
	pool, fA, gameA, providerA, orchA := setupBootstrapFixture(t)
	sessionA, tokenA := mintSessionForBootstrap(t, pool, fA, gameA, ModeReal, "EUR", DefaultLaunchTokenTTL)

	fB := seedCasinoFixture(t, pool)

	// Tenant B's own credential verifies (the MOCK resolver derives a key
	// per (tenant, provider), so this is a genuinely valid signature for
	// tenant B) - but tenant B's own bootstrap can never even SEE tenant
	// A's session (WHERE tenant_id = $t in the step 2b lookup).
	in := providerA.BootstrapPayload(fB.tenantID, tokenA, "req-xtenant-1", gameA.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orchA, pool, fB.tenantID, "mock-casino", in)
	_, err := orchA.BootstrapLaunch(context.Background(), pool, fB.tenantID, "mock-casino", v)
	requireBootstrapRefused(t, err, BootstrapRefusalNotFound)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, fA, sessionA.ID, LaunchSessionActive)
}

// mintAlreadyExpiredSessionForBootstrap inserts a session with expires_at
// already in the past, directly (bypassing CreateLaunchSession's own
// `ttl <= 0 -> DefaultLaunchTokenTTL` floor) - a fixture timestamp, never
// a sleep (T-1), and never an UPDATE afterwards (expires_at is immutable
// once inserted, migration 0036/0042's own column block).
func mintAlreadyExpiredSessionForBootstrap(t *testing.T, pool *db.Pool, f casinoFixture, game Game, mode GameMode, assetCode string) (LaunchSession, string) {
	t.Helper()
	token, err := generateLaunchToken()
	if err != nil {
		t.Fatalf("generate launch token: %v", err)
	}
	id := uuid.New()
	expiresAt := time.Now().UTC().Add(-time.Minute)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_launch_sessions
				(id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
				 asset_code, mode, token_hash, status, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'active', $12)`,
			id, f.tenantID, f.brandID, f.playerAccountID, f.walletID, game.ID, game.ProviderID, game.ProviderGameID,
			assetCode, string(mode), hashLaunchToken(token), expiresAt)
		return err
	})
	if err != nil {
		t.Fatalf("mint already-expired session: %v", err)
	}
	return LaunchSession{
		ID: id, TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
		AssetCode: assetCode, Mode: mode, Status: LaunchSessionActive, ExpiresAt: expiresAt,
	}, token
}

func TestBootstrapLaunch_ADV_Expired(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintAlreadyExpiredSessionForBootstrap(t, pool, f, game, ModeReal, "EUR")

	in := provider.BootstrapPayload(f.tenantID, token, "req-expired-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	requireBootstrapRefused(t, err, BootstrapRefusalExpired)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)
}

func TestBootstrapLaunch_ADV_Revoked(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := RevokeLaunchSession(ctx, tx, session.ID)
		return err
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}

	in := provider.BootstrapPayload(f.tenantID, token, "req-revoked-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	_, err = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	requireBootstrapRefused(t, err, BootstrapRefusalNotActive)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionRevoked)
}

func TestBootstrapLaunch_ADV_ReplayAfterRevoke(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in := provider.BootstrapPayload(f.tenantID, token, "req-replay-revoke-1", game.ProviderGameID, "EUR", "real")
	v1 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v1); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// A's own revoke mechanism (migration 0108's consumed -> revoked).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		prior, revoked, err := RevokeLaunchSession(ctx, tx, session.ID)
		if err != nil {
			return err
		}
		if prior != LaunchSessionConsumed || !revoked {
			t.Fatalf("expected to revoke a consumed session, got prior=%q revoked=%v", prior, revoked)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("revoke consumed session: %v", err)
	}

	// The exact same signed request, replayed. Hash and digest both
	// match, but the session is no longer 'consumed'.
	v2 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	_, err = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v2)
	requireBootstrapRefused(t, err, BootstrapRefusalReplayRevoked)
}

func TestBootstrapLaunch_ADV_ReusedRequestIDDifferentTokenHash(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	sessionA, tokenA := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	sessionB, tokenB := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	inA := provider.BootstrapPayload(f.tenantID, tokenA, "req-shared-1", game.ProviderGameID, "EUR", "real")
	vA := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inA)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vA); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	inB := provider.BootstrapPayload(f.tenantID, tokenB, "req-shared-1", game.ProviderGameID, "EUR", "real")
	vB := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inB)
	_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vB)
	requireBootstrapRefused(t, err, BootstrapRefusalReplayMismatch)

	// Session A (the winner) stays consumed; session B is UNTOUCHED -
	// refusal-does-not-consume, even for the shape of refusal that only
	// surfaces after the idempotency-row lookup.
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, sessionB.ID, LaunchSessionActive)
	var statusA LaunchSessionStatus
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionA.ID).Scan(&statusA)
	})
	if err != nil {
		t.Fatalf("read session A status: %v", err)
	}
	if statusA != LaunchSessionConsumed {
		t.Fatalf("expected session A to remain consumed, got %q", statusA)
	}
}

func TestBootstrapLaunch_ADV_ReusedRequestIDSameTokenDifferentFields(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in1 := provider.BootstrapPayload(f.tenantID, token, "req-sb6-1", game.ProviderGameID, "EUR", "real")
	v1 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in1)
	if _, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v1); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// Same token, same request_id, but a DIFFERENT asset_code in the body
	// - the digest differs even though the hash matches (SB-6).
	in2 := provider.BootstrapPayload(f.tenantID, token, "req-sb6-1", game.ProviderGameID, "USD", "real")
	v2 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in2)
	_, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v2)
	requireBootstrapRefused(t, err, BootstrapRefusalReplayMismatch)

	// The original bootstrap (session now consumed) is unaffected.
	var status LaunchSessionStatus
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, session.ID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != LaunchSessionConsumed {
		t.Fatalf("expected the original session to remain consumed, got %q", status)
	}
}

func TestBootstrapLaunch_ADV_UnknownFieldInBody(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	raw, err := json.Marshal(map[string]any{
		"launch_token": token, "request_id": "req-unknown-field-1",
		"provider_game_id": game.ProviderGameID, "asset_code": "EUR", "mode": "real",
		"tenant_id": f.tenantID.String(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	in := provider.SignRawBody(f.tenantID, raw)
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	_, err = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	requireBootstrapRefused(t, err, BootstrapRefusalNotFound)
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)
}

// --- Gate denial (Q6) ---

func TestBootstrapLaunch_GateDenial_GameInactive(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
			Name: "Test Game", GameType: "slot", SupportedAssets: []string{"EUR"}, Status: GameStatusDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable game: %v", err)
	}

	in := provider.BootstrapPayload(f.tenantID, token, "req-gate-game-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	result, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("expected a denial result, not an error: %v", err)
	}
	if !result.Denied || result.DeniedReason != "game_inactive" {
		t.Fatalf("expected Denied=true reason=game_inactive, got %+v", result)
	}
	assertGateDenied(t, pool, f, session.ID, "game_inactive")
}

func TestBootstrapLaunch_GateDenial_CapabilityDenied(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		declared := provider.Capabilities()
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportsCatalogue: declared.SupportsCatalogue, SupportsLaunch: declared.SupportsLaunch, SupportsBalance: declared.SupportsBalance,
			SupportsBet: declared.SupportsBet, SupportsWin: declared.SupportsWin, SupportsRollback: declared.SupportsRollback,
			SupportedAssets: declared.SupportedAssets, SupportedGameTypes: declared.SupportedGameTypes,
			Priority: 100, Status: CapabilityDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable capability: %v", err)
	}

	in := provider.BootstrapPayload(f.tenantID, token, "req-gate-cap-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	result, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("expected a denial result, not an error: %v", err)
	}
	if !result.Denied || result.DeniedReason != "capability_denied" {
		t.Fatalf("expected Denied=true reason=capability_denied, got %+v", result)
	}
	assertGateDenied(t, pool, f, session.ID, "capability_denied")
}

func TestBootstrapLaunch_GateDenial_RGIneligible(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	suspendAccount(t, pool, f)

	in := provider.BootstrapPayload(f.tenantID, token, "req-gate-rg-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	result, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("expected a denial result, not an error: %v", err)
	}
	if !result.Denied || result.DeniedReason != "rg_ineligible" {
		t.Fatalf("expected Denied=true reason=rg_ineligible, got %+v", result)
	}
	assertGateDenied(t, pool, f, session.ID, "rg_ineligible")

	// RG's own denial audit is also written by evaluateAndAuditEligibility
	// (ADR 0103 §3.3).
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_bootstrap_denied_by_rg") {
		t.Fatal("expected RG's own denial audit record")
	}
}

func assertGateDenied(t *testing.T, pool *db.Pool, f casinoFixture, sessionID uuid.UUID, wantReason string) {
	t.Helper()
	var status LaunchSessionStatus
	var bootstrapCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, sessionID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("read session/bootstrap state: %v", err)
	}
	if status != LaunchSessionRevoked {
		t.Fatalf("expected the session revoked, got %q", status)
	}
	if bootstrapCount != 0 {
		t.Fatalf("expected no bootstrap row for a denied session, got %d", bootstrapCount)
	}

	var metadataJSON []byte
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'casino.launch_bootstrap_denied' ORDER BY created_at DESC LIMIT 1`,
			f.tenantID).Scan(&metadataJSON)
	})
	if err != nil {
		t.Fatalf("read denial audit metadata: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
		t.Fatalf("parse denial audit metadata: %v (%s)", err, metadataJSON)
	}
	if metadata["prior_status"] != "active" {
		t.Fatalf(`expected prior_status="active" in the denial audit, got %s`, metadataJSON)
	}
	if metadata["reason"] != wantReason {
		t.Fatalf("expected reason=%q in the denial audit, got %s", wantReason, metadataJSON)
	}
}

// --- IDM: replay returns a byte-identical response and writes no new
// audit row ---

func TestBootstrapLaunch_Replay_ByteIdenticalNoNewAudit(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in := provider.BootstrapPayload(f.tenantID, token, "req-idm-1", game.ProviderGameID, "EUR", "real")
	v1 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	first, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v1)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	auditCountBefore := countAuditRows(t, pool, f.tenantID, "casino.launch_bootstrapped")

	v2 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	second, err := orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v2)
	if err != nil {
		t.Fatalf("replayed bootstrap: %v", err)
	}
	if !second.Replayed {
		t.Fatal("expected the second call to be a replay")
	}
	if string(first.ResponseJSON) != string(second.ResponseJSON) {
		t.Fatalf("expected byte-identical responses, got %s vs %s", first.ResponseJSON, second.ResponseJSON)
	}

	auditCountAfter := countAuditRows(t, pool, f.tenantID, "casino.launch_bootstrapped")
	if auditCountAfter != auditCountBefore {
		t.Fatalf("expected no new audit row on replay, before=%d after=%d", auditCountBefore, auditCountAfter)
	}

	var bootstrapCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, first.SessionID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("count bootstrap rows: %v", err)
	}
	if bootstrapCount != 1 {
		t.Fatalf("expected exactly one bootstrap row, got %d", bootstrapCount)
	}
}

func countAuditRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count audit rows for %s: %v", action, err)
	}
	return n
}

// --- TI: player_ref is provider-scoped (differs across providers for the
// SAME player) ---

func TestBootstrapLaunch_PlayerRefDiffersAcrossProviders(t *testing.T) {
	pool, f, game, providerA, orchA := setupBootstrapFixture(t)
	_, tokenA := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	providerB := NewMockCasinoProvider("mock-casino-b", "EUR")
	gameB := seedGame(t, pool, "mock-casino-b", "EUR")
	enableGameForTenant(t, pool, f, gameB.ID)
	registerCasinoCapability(t, pool, f, providerB, 100)
	orchB := NewOrchestrator(map[string]CasinoProvider{"mock-casino-b": providerB}, NewMockWebhookCredentials(providerB))
	_, tokenB := mintSessionForBootstrap(t, pool, f, gameB, ModeReal, "EUR", DefaultLaunchTokenTTL)

	inA := providerA.BootstrapPayload(f.tenantID, tokenA, "req-ti-a", game.ProviderGameID, "EUR", "real")
	vA := bootstrapVerified(t, orchA, pool, f.tenantID, "mock-casino", inA)
	resultA, err := orchA.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vA)
	if err != nil {
		t.Fatalf("bootstrap A: %v", err)
	}

	inB := providerB.BootstrapPayload(f.tenantID, tokenB, "req-ti-b", gameB.ProviderGameID, "EUR", "real")
	vB := bootstrapVerified(t, orchB, pool, f.tenantID, "mock-casino-b", inB)
	resultB, err := orchB.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino-b", vB)
	if err != nil {
		t.Fatalf("bootstrap B: %v", err)
	}

	if resultA.PlayerRef == resultB.PlayerRef {
		t.Fatalf("expected different player refs across providers, got the same value %s for both", resultA.PlayerRef)
	}
}

// --- Evaluation error: a genuine failure rolls back and returns a plain
// error, session untouched, never a denial ---

// bootstrapPoisonedTx wraps a real pgx.Tx and forces every QueryRow whose sql
// contains marker to fail - a minimal, self-contained fault injector for
// the one gate-evaluation-error case ADR 0103's own test list names,
// which this codebase has no other hook for.
type bootstrapPoisonedTx struct {
	pgx.Tx
	marker string
}

type bootstrapErrRow struct{ err error }

func (r bootstrapErrRow) Scan(dest ...any) error { return r.err }

func (p *bootstrapPoisonedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if containsSubstring(sql, p.marker) {
		return bootstrapErrRow{err: errors.New("injected evaluation error")}
	}
	return p.Tx.QueryRow(ctx, sql, args...)
}

// bootstrapPoisonedPool implements providercred.TenantTxRunner, opening a REAL
// transaction via the real pool and handing the caller a bootstrapPoisonedTx -
// commit/rollback is still driven by the real pool's own WithTenant.
type bootstrapPoisonedPool struct {
	real   *db.Pool
	marker string
}

func (p *bootstrapPoisonedPool) WithTenant(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error {
	return p.real.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, &bootstrapPoisonedTx{Tx: tx, marker: p.marker})
	})
}

func TestBootstrapLaunch_EvaluationError_RollsBackNeverDenies(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	in := provider.BootstrapPayload(f.tenantID, token, "req-eval-err-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)

	poisoned := &bootstrapPoisonedPool{real: pool, marker: "FROM casino_games"}
	_, err := orch.BootstrapLaunch(context.Background(), poisoned, f.tenantID, "mock-casino", v)
	if err == nil {
		t.Fatal("expected a genuine evaluation error")
	}
	var refused *BootstrapRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("expected a plain evaluation error, not a refusal: %v", err)
	}
	assertSessionUntouchedAndNoBootstrapRow(t, pool, f, session.ID, LaunchSessionActive)

	// L-1 (security review of e810ece, docs/plans/prh2-hardening-round/
	// reviews/b-security.md): this exact error VALUE is what the HTTP
	// handler's generic 5xx branch logs verbatim
	// (`logger.Error("casino_bootstrap_failed", "error", err, ...)`,
	// internal/httpserver/casino_bootstrap_handlers.go) - scanning its
	// string form here for the raw token and its hash is scanning the
	// SAME value that path would ever put in a log line. A genuinely
	// poisoned *db.Pool cannot be wired into the real HTTP server's Deps
	// (Deps.DB is a concrete *db.Pool, not an interface, so swapping it
	// for a test double is not possible without a production-code change
	// outside this fix's scope) - this is the closest equivalent-value
	// scan available, and is exact rather than approximate: err here is
	// literally the same error the HTTP handler would receive from
	// BootstrapLaunch on this path.
	tokenHash := hashLaunchToken(token)
	if containsSubstring(err.Error(), token) || containsSubstring(err.Error(), tokenHash) {
		t.Fatalf("the evaluation error (the exact value the HTTP 5xx path logs) leaked the token or its hash: %v", err)
	}
}

// --- CON: two genuinely concurrent goroutines, real Postgres row locking
// (not a forced interleaving harness - BootstrapLaunch manages its own
// transaction boundary via pool.WithTenant, so this package's tx-body-
// shaped lock harness, designed for code that runs INSIDE an
// already-open tx, does not fit here). Run with -race -count=50 per ADR
// 0103 §9. ---

func TestBootstrapLaunch_ConcurrentConsumes_DifferentRequestIDs(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	inA := provider.BootstrapPayload(f.tenantID, token, "req-con-diff-a", game.ProviderGameID, "EUR", "real")
	inB := provider.BootstrapPayload(f.tenantID, token, "req-con-diff-b", game.ProviderGameID, "EUR", "real")
	vA := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inA)
	vB := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", inB)

	var wg sync.WaitGroup
	var resultA, resultB BootstrapResult
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		resultA, errA = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vA)
	}()
	go func() {
		defer wg.Done()
		resultB, errB = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", vB)
	}()
	wg.Wait()

	succeeded := 0
	if errA == nil && !resultA.Denied {
		succeeded++
	}
	if errB == nil && !resultB.Denied {
		succeeded++
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly one success, got errA=%v resultA=%+v errB=%v resultB=%+v", errA, resultA, errB, resultB)
	}
	// The loser must be the uniform refusal (not_active - it lost the CAS
	// or found the session already bound to the other request_id), never
	// a partial write.
	loserErr := errA
	if succeeded == 1 && errA == nil {
		loserErr = errB
	}
	var refused *BootstrapRefusedError
	if !errors.As(loserErr, &refused) {
		t.Fatalf("expected the loser to be a *BootstrapRefusedError, got %v", loserErr)
	}

	var status LaunchSessionStatus
	var bootstrapCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var sessionID uuid.UUID
		if resultA.SessionID != uuid.Nil {
			sessionID = resultA.SessionID
		} else {
			sessionID = resultB.SessionID
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, sessionID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("read final state: %v", err)
	}
	if status != LaunchSessionConsumed {
		t.Fatalf("expected the session consumed exactly once, got %q", status)
	}
	if bootstrapCount != 1 {
		t.Fatalf("expected exactly one bootstrap row, got %d", bootstrapCount)
	}
}

func TestBootstrapLaunch_ConcurrentConsumes_SameRequestID(t *testing.T) {
	pool, f, game, provider, orch := setupBootstrapFixture(t)
	_, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	// The EXACT same signed payload, verified twice (two independent
	// phase-1 verifications of the same bytes, as two genuinely separate
	// incoming HTTP requests would each produce their own *VerifiedCallback).
	in := provider.BootstrapPayload(f.tenantID, token, "req-con-same-1", game.ProviderGameID, "EUR", "real")
	v1 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)
	v2 := bootstrapVerified(t, orch, pool, f.tenantID, "mock-casino", in)

	var wg sync.WaitGroup
	var result1, result2 BootstrapResult
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		result1, err1 = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v1)
	}()
	go func() {
		defer wg.Done()
		result2, err2 = orch.BootstrapLaunch(context.Background(), pool, f.tenantID, "mock-casino", v2)
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("expected caller 1 to succeed, got %v", err1)
	}
	if err2 != nil {
		t.Fatalf("expected caller 2 to succeed, got %v", err2)
	}
	if string(result1.ResponseJSON) != string(result2.ResponseJSON) {
		t.Fatalf("expected identical responses, got %s vs %s", result1.ResponseJSON, result2.ResponseJSON)
	}
	if result1.SessionID != result2.SessionID {
		t.Fatalf("expected the same session id, got %s vs %s", result1.SessionID, result2.SessionID)
	}

	var bootstrapCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, result1.SessionID).Scan(&bootstrapCount)
	})
	if err != nil {
		t.Fatalf("count bootstrap rows: %v", err)
	}
	if bootstrapCount != 1 {
		t.Fatalf("expected exactly one bootstrap row for the same request_id, got %d", bootstrapCount)
	}
}

// --- MUT support: casConsumeSessionForBootstrap's OWN binding predicate,
// tested directly rather than through the full BootstrapLaunch flow.
// Every one of provider_id/provider_game_id/asset_code/mode/status/
// expires_at is ALSO checked earlier, at step 3 (binding check) - which
// runs first and would refuse a genuine mismatch before the CAS is ever
// reached, since every one of these session columns is immutable after
// insert (migration 0036/0042) and the FOR UPDATE lock held since step 2b
// prevents any concurrent change for the lifetime of the transaction. A
// mutant that drops one of these from the CAS's own WHERE clause is
// therefore UNREACHABLE through BootstrapLaunch's own ADV tests (step 3
// already refused it) - these tests call casConsumeSessionForBootstrap
// directly, bypassing step 3 entirely, to pin the CAS's OWN redundant
// (defense-in-depth, ADR 0103 §3.2 step 5/SB-2) predicate. ---

func TestCasConsumeSessionForBootstrap_BindingPredicate(t *testing.T) {
	pool, f, game, _, _ := setupBootstrapFixture(t)
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	tokenHash := hashLaunchToken(token)

	cases := []struct {
		name           string
		providerID     string
		mode           GameMode
		assetCode      string
		providerGameID string
	}{
		{"wrong_provider_id", "some-other-provider", ModeReal, "EUR", game.ProviderGameID},
		{"wrong_mode", session.ProviderID, ModeDemo, "EUR", game.ProviderGameID},
		{"wrong_asset_code", session.ProviderID, ModeReal, "USD", game.ProviderGameID},
		{"wrong_provider_game_id", session.ProviderID, ModeReal, "EUR", "some-other-game"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				consumed, err := casConsumeSessionForBootstrap(ctx, tx, session.ID, f.tenantID, c.providerID, c.mode, c.assetCode, c.providerGameID, tokenHash)
				if err != nil {
					return err
				}
				if consumed != uuid.Nil {
					t.Fatalf("expected the CAS to refuse a mismatched %s, but it consumed the session", c.name)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("cas attempt (%s): %v", c.name, err)
			}
		})
	}

	// The correct values succeed - proves the mismatched cases above
	// failed on their OWN mismatched field, not on some other defect.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		consumed, err := casConsumeSessionForBootstrap(ctx, tx, session.ID, f.tenantID, session.ProviderID, ModeReal, "EUR", game.ProviderGameID, tokenHash)
		if err != nil {
			return err
		}
		if consumed != session.ID {
			t.Fatal("expected the CAS to succeed with every field correct")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cas correct attempt: %v", err)
	}
}

func TestCasConsumeSessionForBootstrap_RefusesNonActiveAndExpired(t *testing.T) {
	pool, f, game, _, _ := setupBootstrapFixture(t)

	t.Run("already_revoked", func(t *testing.T) {
		session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
		tokenHash := hashLaunchToken(token)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := RevokeLaunchSession(ctx, tx, session.ID); err != nil {
				return err
			}
			consumed, err := casConsumeSessionForBootstrap(ctx, tx, session.ID, f.tenantID, session.ProviderID, ModeReal, "EUR", game.ProviderGameID, tokenHash)
			if err != nil {
				return err
			}
			if consumed != uuid.Nil {
				t.Fatal("expected the CAS to refuse a revoked session")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("cas attempt: %v", err)
		}
	})

	t.Run("already_expired", func(t *testing.T) {
		session, token := mintAlreadyExpiredSessionForBootstrap(t, pool, f, game, ModeReal, "EUR")
		tokenHash := hashLaunchToken(token)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			consumed, err := casConsumeSessionForBootstrap(ctx, tx, session.ID, f.tenantID, session.ProviderID, ModeReal, "EUR", game.ProviderGameID, tokenHash)
			if err != nil {
				return err
			}
			if consumed != uuid.Nil {
				t.Fatal("expected the CAS to refuse an already-expired session")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("cas attempt: %v", err)
		}
	})
}
