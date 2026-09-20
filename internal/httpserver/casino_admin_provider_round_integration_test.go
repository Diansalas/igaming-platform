//go:build integration

// Stage 8 (docs/decisions/0080-provider-integration-readiness-without-
// external-contracts.md, Decision 5) HTTP-layer tests for the Back
// Office's admin casino-rounds provider_round_id field, added to
// adminRoundResponse (casino_history_handlers.go) alongside
// casino.LookupProviderRoundIDsBySession (rounds.go). Mirrors
// casino_play_flow_integration_test.go's own established fixture/helper
// conventions exactly (testEnv/newCasinoTestServer/mustCreateTenant/
// mustCreateBrand/mustRegisterPlayer/mustActivatePlayer/fundWallet/
// mustSeedCasinoGame/mustEnableCasinoGameForTenant/mustEnableCasinoCapability/
// mustLaunchCasinoGame/mustCreateStaff/mustLoginStaff/getJSON/postJSON/
// decodeBody), plus internal/casino's own BindProviderRound (rounds.go) to
// seed a binding directly, since no code path this stage ever calls it
// except a test (no real provider posts callbacks yet).
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustBindProviderRound seeds a casino_provider_rounds row directly via
// internal/casino.BindProviderRound - the same function postBet itself
// calls, exercised here without going through a real provider callback
// (none exists this stage). BindProviderRound computes its own
// correlation_id internally (roundCorrelationID) - it is no longer a
// caller-supplied parameter - and providerSessionID is nil, since no
// separate "provider session id" concept exists on CallbackEvent yet.
func mustBindProviderRound(t *testing.T, pool *db.Pool, tenantID, brandID, playerAccountID, launchSessionID, gameID uuid.UUID, providerID, providerRoundID string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return casino.BindProviderRound(ctx, tx, tenantID, brandID, playerAccountID, launchSessionID, gameID,
			providerID, providerRoundID, nil)
	})
	if err != nil {
		t.Fatalf("bind provider round: %v", err)
	}
}

// TestAdminListCasinoRounds_ProviderRoundIDEmptyWhenNoBinding proves the
// common case ADR 0080 Decision 5 describes: a round with no
// casino_provider_rounds row at all renders provider_round_id as "" (never
// omitted, never null). This is deliberately a LAUNCHED-but-never-wagered
// round (RoundStatusLaunched): postBet itself binds a casino_provider_rounds
// row the first time a round is bet on (ADR 0080 Decision 1's own "Binding
// point" section - the play-simulation wager endpoint drives the same
// postBet path a real provider callback would), so a round with an actual
// bet posted is NOT the "no binding" case even though no real provider
// exists yet - only a round nobody has ever wagered on is.
func TestAdminListCasinoRounds_ProviderRoundIDEmptyWhenNoBinding(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	// Launch only - no wager, so postBet (and therefore BindProviderRound)
	// never runs for this round.
	mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/casino/rounds", tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminRoundResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 round, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ProviderRoundID != "" {
		t.Fatalf("expected provider_round_id=\"\" for an unbound round, got %q", page.Items[0].ProviderRoundID)
	}
}

// TestAdminListCasinoRounds_ProviderRoundIDPopulatedWhenBound proves a
// round WITH a casino_provider_rounds binding (seeded via
// casino.BindProviderRound, mirroring what a real provider's postBet call
// would eventually do) surfaces that binding's own provider_round_id
// through the admin endpoint.
func TestAdminListCasinoRounds_ProviderRoundIDPopulatedWhenBound(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	if resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the wager to succeed, got %d", resp.StatusCode)
	}

	sessionID := uuid.MustParse(launched.SessionID)
	mustBindProviderRound(t, pool, tenant.ID, brand.ID, player.ID, sessionID, game.ID, "mock-casino", "ext-round-77")

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/casino/rounds", tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminRoundResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 round, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ProviderRoundID != "ext-round-77" {
		t.Fatalf("expected provider_round_id=%q, got %q", "ext-round-77", page.Items[0].ProviderRoundID)
	}
}

// TestAdminListCasinoRounds_ProviderRoundIDCrossTenantIsolated proves an
// admin in tenant B never sees tenant A's provider_round_id - the batched
// casino.LookupProviderRoundIDsBySession lookup is explicitly scoped by
// tc.TenantID (in addition to casino_provider_rounds' own FORCE RLS), and
// this test exercises that boundary directly rather than assuming it from
// the pre-existing round-level cross-tenant isolation test alone.
func TestAdminListCasinoRounds_ProviderRoundIDCrossTenantIsolated(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)

	playerA := mustRegisterPlayer(t, srv, brandA.Slug)
	playerB := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, playerA.ID)
	mustActivatePlayer(t, pool, tenantB.ID, playerB.ID)
	fundWallet(t, pool, tenantA.ID, brandA.ID, playerA.ID, "EUR", 10_000)
	fundWallet(t, pool, tenantB.ID, brandB.ID, playerB.ID, "EUR", 10_000)

	gameA := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantA.ID, gameA.ID)
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	gameB := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantB.ID, gameB.ID)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	launchedA := mustLaunchCasinoGame(t, srv, playerA.Tokens.AccessToken, gameA.ID.String(), "EUR", "real")
	launchedB := mustLaunchCasinoGame(t, srv, playerB.Tokens.AccessToken, gameB.ID.String(), "EUR", "real")

	if resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedA.SessionID+"/wager", playerA.Tokens.AccessToken, wagerBody(500)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected player A's wager to succeed, got %d", resp.StatusCode)
	}
	if resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launchedB.SessionID+"/wager", playerB.Tokens.AccessToken, wagerBody(700)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected player B's wager to succeed, got %d", resp.StatusCode)
	}

	sessionIDA := uuid.MustParse(launchedA.SessionID)
	sessionIDB := uuid.MustParse(launchedB.SessionID)
	mustBindProviderRound(t, pool, tenantA.ID, brandA.ID, playerA.ID, sessionIDA, gameA.ID, "mock-casino", "tenant-a-round")
	mustBindProviderRound(t, pool, tenantB.ID, brandB.ID, playerB.ID, sessionIDB, gameB.ID, "mock-casino", "tenant-b-round")

	staffA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokensA := mustLoginStaff(t, srv, tenantA.Slug, staffA.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/casino/rounds", tokensA.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminRoundResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected tenant A's admin to see exactly its own tenant's 1 round, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ProviderRoundID != "tenant-a-round" {
		t.Fatalf("expected tenant A's admin to see ITS OWN round's provider_round_id %q, got %q", "tenant-a-round", page.Items[0].ProviderRoundID)
	}
}
