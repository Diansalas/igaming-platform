//go:build integration

// Stage 10.3 gate 10.3-W1 code review finding #3: two W1c guards were
// untested - LaunchGame's real-mode `!SupportsBet` requirement
// (orchestrator.go's own K3-... style doc comment, "CAS-CAP-ROLLBACK-1
// §1.4 step 7, launch coherence") and the `supported_assets` narrowing
// check inside postBet (a capability that has narrowed since a session was
// launched must reject a bet on the now-unsupported asset, even though the
// session itself remains active).
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestLaunchGame_CodeReview3_RealModeRequiresSupportsBet proves LaunchGame
// itself refuses a REAL-mode launch under an S-nobet capability (active,
// supports_bet=false) - so no usable real-money session can ever exist in
// which every bet then 503s at postBet's own gate - while a DEMO-mode
// launch (no financial exposure to gate) is unaffected.
func TestLaunchGame_CodeReview3_RealModeRequiresSupportsBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-cr3-snobet-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-cr3-snobet-pw")

	snobetBody := validCasinoCapabilityBody()
	snobetBody["supports_bet"] = false
	capResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, snobetBody)
	if capResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 registering an S-nobet capability, got %d", capResp.StatusCode)
	}
	capResp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	realResp := postJSON(t, srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", player.Tokens.AccessToken, map[string]string{
		"asset_code": "EUR", "mode": "real",
	})
	defer realResp.Body.Close()
	if realResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a REAL-mode launch under S-nobet (LaunchGame's own supports_bet requirement), got %d", realResp.StatusCode)
	}

	demoResp := postJSON(t, srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", player.Tokens.AccessToken, map[string]string{
		"asset_code": "EUR", "mode": "demo",
	})
	defer demoResp.Body.Close()
	if demoResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for a DEMO-mode launch under S-nobet (no financial exposure to gate), got %d", demoResp.StatusCode)
	}
}

// TestPostBet_CodeReview3_SupportedAssetsNarrowedAfterLaunchRejectsBet
// launches a real-money EUR session while the capability supports
// {EUR, USD}, then narrows the capability to {USD} only - the session
// itself stays active - and proves postBet's OWN supported_assets
// recheck (not merely LaunchGame's, which only ran once at launch time)
// rejects a bet on the now-unsupported EUR asset.
func TestPostBet_CodeReview3_SupportedAssetsNarrowedAfterLaunchRejectsBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-cr3-narrow-pw")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-cr3-narrow-pw")

	enableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling the capability with EUR+USD support, got %d", enableResp.StatusCode)
	}
	enableResp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR", "USD")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	narrowedBody := validCasinoCapabilityBody()
	narrowedBody["supported_assets"] = []string{"USD"}
	narrowResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, narrowedBody)
	if narrowResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 narrowing supported_assets to USD only, got %d", narrowResp.StatusCode)
	}
	narrowResp.Body.Close()

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-cr3-narrow-bet", "", "round-cr3-narrow", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503: the EUR session's own asset is no longer in the capability's (narrowed) supported_assets, got %d", resp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, "cas-cr3-narrow-bet"); got != 0 {
		t.Fatalf("expected no ledger effect for the rejected bet, got %d rows", got)
	}
}
