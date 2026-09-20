//go:build integration

// Stage 8 (docs/decisions/0080-provider-integration-readiness-without-
// external-contracts.md, Decision 5) HTTP-layer tests for the Back
// Office's admin sportsbook-bets provider_id/provider_bet_reference fields,
// added to adminBetResponse (sportsbook_handlers.go), passed through
// directly from sportsbook.Bet's own additive, always-nil-today pointer
// fields (types.go, migration 0081). Mirrors sportsbook_flow_integration_
// test.go's own established fixture/helper conventions exactly
// (testEnv/newSportsbookTestServer/mustCreateTenant/mustCreateBrand/
// mustRegisterPlayer/mustActivatePlayer/fundWallet/mustSeedSportsbookSelection/
// placeBetRequestBody/mustCreateStaff/mustLoginStaff/getJSON/postJSON/
// decodeBody). No PlaceBet code path sets provider_id/provider_bet_reference
// this stage (internal/sportsbook.Provider deliberately has no bet-
// placement method yet - ADR 0080 Decision 2), so the "populated" case
// below sets them via direct SQL after a real bet is placed, exactly like
// internal/sportsbook's own provider_reference_integration_test.go does at
// the package level.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustSetBetProviderRefDirectSQL sets sportsbook_bets.provider_id/
// provider_bet_reference for an already-placed bet directly via SQL, since no
// HTTP endpoint or orchestrator code path sets either column this stage.
func mustSetBetProviderRefDirectSQL(t *testing.T, pool *db.Pool, tenantID, betID uuid.UUID, providerID, providerBetReference string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET provider_id = $1, provider_bet_reference = $2 WHERE id = $3`,
			providerID, providerBetReference, betID)
		return err
	})
	if err != nil {
		t.Fatalf("set bet provider reference via direct SQL: %v", err)
	}
}

// TestAdminListBets_ProviderFieldsEmptyWhenNull proves the common case ADR
// 0080 Decision 5 describes: a bet placed through the existing,
// unmodified PlaceBet flow (provider_id/provider_bet_reference always NULL -
// ADR 0080 Decision 2) renders both as "" (never omitted, never null)
// through the admin endpoint.
func TestAdminListBets_ProviderFieldsEmptyWhenNull(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100)
	if resp := postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 500, 200, 100, uuid.NewString())); resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected the bet to succeed, got %d", resp.StatusCode)
	}

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/sportsbook/bets", tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminBetResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 bet, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ProviderID != "" {
		t.Fatalf("expected provider_id=\"\" for a bet with no provider reference, got %q", page.Items[0].ProviderID)
	}
	if page.Items[0].ProviderBetReference != "" {
		t.Fatalf("expected provider_bet_reference=\"\" for a bet with no provider reference, got %q", page.Items[0].ProviderBetReference)
	}
}

// TestAdminListBets_ProviderFieldsPopulatedFromDirectSQL proves a bet
// WITH provider_id/provider_bet_reference set (via direct SQL, since no code
// path sets them this stage) surfaces both exact values through the admin
// endpoint.
func TestAdminListBets_ProviderFieldsPopulatedFromDirectSQL(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100)
	placeResp := postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 500, 200, 100, uuid.NewString()))
	if placeResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected the bet to succeed, got %d", placeResp.StatusCode)
	}
	var placed placeBetResponse
	decodeBody(t, placeResp, &placed)
	if !placed.Accepted || placed.Bet == nil {
		t.Fatalf("expected an accepted bet, got %+v", placed)
	}
	betID := uuid.MustParse(placed.Bet.ID)

	mustSetBetProviderRefDirectSQL(t, pool, tenant.ID, betID, "dummy-sportsbook", "dummy-ref-42")

	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	resp := getJSON(t, srv, "/v1/admin/sportsbook/bets", tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[adminBetResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 bet, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ProviderID != "dummy-sportsbook" {
		t.Fatalf("expected provider_id=%q, got %q", "dummy-sportsbook", page.Items[0].ProviderID)
	}
	if page.Items[0].ProviderBetReference != "dummy-ref-42" {
		t.Fatalf("expected provider_bet_reference=%q, got %q", "dummy-ref-42", page.Items[0].ProviderBetReference)
	}
}
