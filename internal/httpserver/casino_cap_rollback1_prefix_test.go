//go:build integration

// Stage 10.3 CAS-CAP-ROLLBACK-1: post-fix behaviour, inverted from the
// pre-fix evidence this file used to record.
//
// This file used to record, against the PRE-Item-1 code, the
// CAS-CAP-ROLLBACK-1 defects named in docs/plans/stage-10.3-planning/
// 02-casino-financial-analysis.md §0 (F1-F4) and §1 (E1/E3/E4 rows of the
// §1.3 table): a disabled/missing capability 503'd a verified win/
// rollback and skipped writing a rollback-of-unseen-original's tombstone,
// a late original after its own tombstone surfaced as an untyped 500,
// and a brand-only capability row rejected every callback (F4, the
// uuid.Nil tenant-wide lookup). The red run against pre-fix HEAD is
// preserved as evidence in docs/plans/stage-10.3-planning/evidence/ (see
// that directory's own files) - untouched by this inversion, per the
// binding instruction to keep evidence artifacts unchanged. Every
// assertion below is now inverted to the FIXED behaviour the §1.3
// contract table specifies; do not re-widen these back toward the
// pre-fix shape.
//
// Run as the NOBYPASSRLS runtime role, per the binding test plan (04-
// review-qa.md §3, §1.9).
package httpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"

	"net/http"
)

// countCasinoLedgerRows counts ledger_transactions rows for mock-casino
// matching an exact provider_tx_id, under tenant-staff scope.
func countCasinoLedgerRowsForTx(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerTxID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = $1`, providerTxID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger_transactions for provider_tx_id=%s: %v", providerTxID, err)
	}
	return count
}

// countCasinoAuditRowsForAction counts audit_log rows for a given action
// whose metadata->>'provider_tx_id' matches providerTxID - the shared
// helper the concurrency (C4) and per-flow audit tests use to assert
// "exactly one audit row" independent of the count of any OTHER audit
// action a scenario may also produce (e.g. casino.launched).
func countCasinoAuditRowsForAction(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, providerTxID string) int {
	t.Helper()
	return countCasinoAuditRowsForActionMetadataKey(t, pool, tenantID, action, "provider_tx_id", providerTxID)
}

// countCasinoAuditRowsForActionMetadataKey is countCasinoAuditRowsForAction
// generalized to an arbitrary metadata key - the rollback-tombstone audit
// record (casino_rollback.tombstoned) keys its OWN reference under
// "rollback_provider_tx_id", not "provider_tx_id" (orchestrator.go's own
// metadata shape for that action).
func countCasinoAuditRowsForActionMetadataKey(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, metadataKey, value string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND metadata->>$3 = $4`,
			tenantID, action, metadataKey, value).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count audit_log rows for action=%s %s=%s: %v", action, metadataKey, value, err)
	}
	return count
}

// countCasinoTombstonesForTx counts tombstone rows keyed on a given
// original provider_tx_id.
func countCasinoTombstonesForTx(t *testing.T, pool *db.Pool, tenantID uuid.UUID, originalProviderTxID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = $1 AND transaction_type = 'tombstone'`, originalProviderTxID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count tombstones for original provider_tx_id=%s: %v", originalProviderTxID, err)
	}
	return count
}

// TestCasCapRollback1_DisabledCapabilityStillSettlesWinForPostedBet is E1
// (inverted from the pre-fix defect): a verified win for an already-
// posted bet now settles (200, the ledger row posts, the player's
// balance is credited) EVEN THOUGH the tenant's whole
// casino_provider_capabilities row is disabled. Capability/status now
// gate NEW BETS ONLY (§1.3) - a win never reads the capability at all.
func TestCasCapRollback1_DisabledCapabilityStillSettlesWinForPostedBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-e1-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-e1-pw-1")

	enableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling the mock-casino capability, got %d", enableResp.StatusCode)
	}
	enableResp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	const originalBetTxID = "cas-e1-original-bet"
	const roundID = "round-e1"
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, originalBetTxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the original bet while the capability is enabled, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, originalBetTxID); got != 1 {
		t.Fatalf("expected the original bet to have genuinely posted (1 ledger_transactions row), got %d", got)
	}
	balanceAfterBet := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterBet != 9000 {
		t.Fatalf("expected 10000-1000=9000 after the stake was taken, got %d", balanceAfterBet)
	}

	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 disabling the mock-casino capability, got %d", disableResp.StatusCode)
	}
	disableResp.Body.Close()

	const winTxID = "cas-e1-win"
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer winResp.Body.Close()
	if winResp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1: expected 200 for a verified win on an already-posted bet EVEN THOUGH the capability is disabled (settlement is never gated), got %d", winResp.StatusCode)
	}

	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 1 {
		t.Fatalf("expected the win to post exactly once while disabled, got %d ledger_transactions rows", got)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, originalBetTxID); got != 1 {
		t.Fatalf("expected the original bet's single row to be untouched (un-reversed, not duplicated), got %d", got)
	}
	balanceAfterWin := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterWin != balanceAfterBet+2500 {
		t.Fatalf("expected the win to credit the player (balance %d), got %d", balanceAfterBet+2500, balanceAfterWin)
	}
}

// TestCasCapRollback1_UnseenRollbackAlwaysTombstonesEvenWhenCapabilityDisabled
// is E8 (inverted from the pre-fix E2 defect): a verified rollback of a
// NEVER-SEEN original, while the tenant's capability is disabled, still
// writes its tombstone. CLAUDE.md's late-arrival guarantee ("A rollback
// for a transaction never seen writes a tombstone so a late-arriving
// original is rejected") no longer depends on tenant configuration.
func TestCasCapRollback1_UnseenRollbackAlwaysTombstonesEvenWhenCapabilityDisabled(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-e2-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-e2-pw-1")

	// Register the capability directly in the disabled state - no bet was
	// ever posted for this original reference under this tenant.
	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 registering the disabled capability, got %d", disableResp.StatusCode)
	}
	disableResp.Body.Close()

	const unseenOriginalTxID = "cas-e2-unseen-original"
	const rollbackTxID = "cas-e2-rollback"
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackTxID, unseenOriginalTxID, "round-e2", "game-1",
		0, "EUR", "", "", uuid.New(), uuid.Nil)

	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1: expected 200 (tombstoned) for a verified rollback of an unseen original even while the capability is disabled, got %d", resp.StatusCode)
	}

	if got := countCasinoTombstonesForTx(t, pool, tenant.ID, unseenOriginalTxID); got != 1 {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1: expected exactly 1 tombstone to be written (the late-arrival guarantee holds regardless of capability state), got %d", got)
	}
}

// TestCasCapRollback1_LateOriginalAfterTombstoneIsNamedRejectionNot500 is
// E3 (inverted from the pre-fix F8/G-2 defect): a rollback of an unseen
// original writes its tombstone (the normal path), and the SAME
// original's bet then arrives late. It is now a deterministic, named
// decline (postBet's own decline-without-error convention: 200,
// outcome=declined, decline_reason=original_rolled_back) instead of the
// untyped-error 500 that used to invite endless provider retries. Nothing
// NEW posts for the late original.
func TestCasCapRollback1_LateOriginalAfterTombstoneIsNamedRejectionNot500(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	const lateOriginalTxID = "cas-e3-late-original"
	const rollbackTxID = "cas-e3-early-rollback"
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackTxID, lateOriginalTxID, "round-e3", "game-1",
		0, "EUR", "", "", player.ID, uuid.Nil)
	rollbackResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	if rollbackResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the early rollback of an unseen original (writes a tombstone), got %d", rollbackResp.StatusCode)
	}
	rollbackResp.Body.Close()

	if got := countCasinoTombstonesForTx(t, pool, tenant.ID, lateOriginalTxID); got != 1 {
		t.Fatalf("expected exactly 1 tombstone for %s, got %d", lateOriginalTxID, got)
	}

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)
	lateOriginalPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, lateOriginalTxID, "", "round-e3", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	lateResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", lateOriginalPayload)
	defer lateResp.Body.Close()

	if lateResp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1: expected 200 (a named decline, not an error) for a late original after its own tombstone, got %d", lateResp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(lateResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode late-original response body: %v", err)
	}
	if body["outcome"] != string(casino.OutcomeDeclined) {
		t.Fatalf("expected outcome=declined for a late original after its tombstone, got %+v", body)
	}
	if body["decline_reason"] != "original_rolled_back" {
		t.Fatalf("expected decline_reason=original_rolled_back, got %+v", body)
	}

	// Financial outcome: nothing NEW posts for the late original - the
	// single row under this provider_tx_id is still exactly the earlier
	// tombstone (F7: a tombstone's own provider_tx_id IS the original
	// reference), not a second/duplicate posting from the declined late
	// bet.
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, lateOriginalTxID); got != 1 {
		t.Fatalf("expected exactly 1 row (the tombstone only, no new posting) for the declined late original, got %d", got)
	}
	balance := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balance != 10_000 {
		t.Fatalf("expected the wallet to remain untouched (10000), got %d", balance)
	}
}

// TestCasCapRollback1_BrandOnlyCapabilityNowResolvesAndAcceptsCallbacks is
// E4/F4 inverted: a tenant with ONLY a brand-scoped
// casino_provider_capabilities row (no tenant-wide row) now gets its bets
// accepted, because postBet resolves the capability for the SESSION's OWN
// brand (session.BrandID), exactly the way LaunchGame always did - F4's
// uuid.Nil tenant-wide-only lookup is gone from the callback path
// entirely.
func TestCasCapRollback1_BrandOnlyCapabilityNowResolvesAndAcceptsCallbacks(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	brandProvider := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, werr := casino.WriteCapability(ctx, tx, brandProvider, tenant.ID, &brand.ID, casino.CapabilityConfig{
			SupportsCatalogue: true, SupportsLaunch: true, SupportsBalance: true,
			SupportsBet: true, SupportsWin: true, SupportsRollback: true,
			SupportedAssets: []string{"EUR"}, SupportedGameTypes: []string{"slot", "table", "live"},
			Priority: 100, Status: casino.CapabilityActive,
		})
		return werr
	})
	if err != nil {
		t.Fatalf("failed to write a brand-only capability row directly: %v", err)
	}

	// Confirm, from the SAME connection's perspective, that a tenant-wide
	// lookup genuinely finds nothing (F4's own precondition) - the
	// callback path no longer performs this lookup at all (it is deleted
	// from ReceiveCallback), but this still documents why a brand-scoped
	// resolution is the only thing that CAN work here.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, found, lerr := casino.LoadCapability(ctx, tx, tenant.ID, uuid.Nil, "mock-casino")
		if lerr != nil {
			return lerr
		}
		if found {
			t.Fatalf("PRECONDITION FAILED: expected the tenant-wide (brand_id=uuid.Nil) lookup to find NOTHING when only a brand-scoped row exists, but it found one")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant-wide capability lookup: %v", err)
	}

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-e4-bet-1", "", "round-e4", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1 (F4): expected 200 for a bet when only a brand-scoped capability row exists and it matches the session's own brand, got %d", resp.StatusCode)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, "cas-e4-bet-1"); got != 1 {
		t.Fatalf("expected the bet to have posted (1 ledger_transactions row), got %d", got)
	}
}

// TestCasCapRollback1_NoBrandNoTenantCapabilityRow_StillRejectsNewBets
// confirms S-none from the §1.3 table did NOT silently become "allowed"
// by deleting the pre-dispatch check: once the capability row is removed
// entirely (not merely disabled), a NEW bet against a still-valid launch
// session still 503s (never 200) - the case-by-case decision is now made
// in postBet itself, not lost. The session is minted BEFORE the row is
// removed (launching itself still requires a capability), isolating this
// test to postBet's own S-none gate rather than session issuance.
func TestCasCapRollback1_NoBrandNoTenantCapabilityRow_StillRejectsNewBets(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	// Remove the capability row ENTIRELY (S-none), not merely disable it
	// (S-off, already covered by the E1/E2 tests above) - this is a
	// distinct capability state in the §1.3 table.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM casino_provider_capabilities WHERE tenant_id = $1`, tenant.ID)
		return err
	}); err != nil {
		t.Fatalf("delete capability row: %v", err)
	}

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-snone-bet-1", "", "round-snone", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("S-none: expected 503 for a new bet with no capability row configured at all, got %d", resp.StatusCode)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
}
