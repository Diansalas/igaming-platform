//go:build integration

// Stage 10.3 pre-fix evidence; inverted by the fix.
//
// This file records, against the CURRENT (pre-Item-1) code, the
// CAS-CAP-ROLLBACK-1 defects named in docs/plans/stage-10.3-planning/
// 02-casino-financial-analysis.md §0 (F1-F4) and §1 (E1/E3/E4 rows of the
// §1.3 table). Each test PASSES today because it demonstrates the
// defect; the Item 1 fix (orchestrator.go, capability.go) is expected to
// invert every one of these assertions. Do not "fix forward" these tests
// - they characterize what ships today, not what should ship.
//
// Run as the NOBYPASSRLS runtime role, per the binding test plan (04-
// review-qa.md §3, §1.9).
package httpserver

import (
	"context"
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

// TestCasCapRollback1_PreFix_DisabledCapabilityBlocksWinForPostedBet is E1:
// a verified win for an already-posted bet 503s while the tenant's whole
// casino_provider_capabilities row is disabled (F1+F2+F3 - the pre-
// dispatch capability/status check in ReceiveCallback runs identically
// for win as for bet, with no exemption for settling existing exposure).
// The stake taken by the original bet is left standing (never reversed,
// never credited back), and the win is never posted - the defect is that
// nothing settles, not that money is created or destroyed.
func TestCasCapRollback1_PreFix_DisabledCapabilityBlocksWinForPostedBet(t *testing.T) {
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
	if winResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PRE-FIX EVIDENCE: expected 503 for a verified win on an already-posted bet while the capability is disabled (F1-F3), got %d - if this changed, the fix landed; invert this test, do not widen it", winResp.StatusCode)
	}

	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, winTxID); got != 0 {
		t.Fatalf("expected the win to NOT be posted while disabled, got %d ledger_transactions rows", got)
	}
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, originalBetTxID); got != 1 {
		t.Fatalf("expected the original bet's single row to be untouched (un-reversed, not duplicated), got %d", got)
	}
	balanceAfterBlockedWin := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balanceAfterBlockedWin != balanceAfterBet {
		t.Fatalf("expected the stake to stay debited and the win withheld (balance still %d), got %d", balanceAfterBet, balanceAfterBlockedWin)
	}
}

// TestCasCapRollback1_PreFix_UnseenRollbackDisabledCapabilityNoTombstone is
// E2: a verified rollback of a NEVER-SEEN original, while the tenant's
// capability is disabled, writes NO tombstone (F1+F2+F3's last sentence).
// This breaks CLAUDE.md's late-arrival guarantee ("A rollback for a
// transaction never seen writes a tombstone so a late-arriving original
// is rejected") purely as a function of tenant configuration.
func TestCasCapRollback1_PreFix_UnseenRollbackDisabledCapabilityNoTombstone(t *testing.T) {
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

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PRE-FIX EVIDENCE: expected 503 for a verified rollback of an unseen original while the capability is disabled, got %d", resp.StatusCode)
	}

	if got := countCasinoTombstonesForTx(t, pool, tenant.ID, unseenOriginalTxID); got != 0 {
		t.Fatalf("PRE-FIX EVIDENCE: expected NO tombstone to be written (the late-arrival guarantee is broken by a disabled capability today), got %d", got)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
}

// TestCasCapRollback1_PreFix_LateOriginalAfterTombstoneIs500 is E3 (G-2/
// F8): a rollback of an unseen original writes its tombstone (capability
// enabled, the normal path), and the SAME original's bet then arrives
// late. F8 says this surfaces as an untyped error from the unique index,
// which the HTTP layer has no named mapping for and so falls through to
// the generic "failed to process callback" 500 branch
// (casino_handlers.go). This test confirms the ACTUAL status code
// observed today rather than assuming it.
func TestCasCapRollback1_PreFix_LateOriginalAfterTombstoneIs500(t *testing.T) {
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

	// Report the ACTUAL status observed, whatever it is, rather than
	// asserting 500 blindly - per the binding instruction. F8/the analysis
	// predicts 500 (an untyped ledger.Post conflict-path error falling
	// through to the generic CodeInternal branch).
	if lateResp.StatusCode != http.StatusInternalServerError {
		t.Logf("NOTE: analysis (G-2/F8) predicted 500 for a late original after its tombstone; ACTUAL observed status is %d - recording as-is, not massaging the test", lateResp.StatusCode)
	}
	t.Logf("PRE-FIX EVIDENCE: late original after tombstone -> HTTP %d", lateResp.StatusCode)
	if lateResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PRE-FIX EVIDENCE: expected the analysis-predicted 500 for a late original after its tombstone, ACTUAL was %d - see log line above for the recorded discrepancy", lateResp.StatusCode)
	}

	// Financial outcome is correct regardless of the error's HTTP class:
	// nothing NEW posts for the late original - the single row under this
	// provider_tx_id is still exactly the earlier tombstone (F7: a
	// tombstone's own provider_tx_id IS the original reference), not a
	// second/duplicate posting from the rejected late bet.
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, lateOriginalTxID); got != 1 {
		t.Fatalf("expected exactly 1 row (the tombstone only, no new posting) for the rejected late original, got %d", got)
	}
}

// TestCasCapRollback1_PreFix_BrandOnlyCapabilityRejectsCallbacks is E4
// (F4): a tenant with ONLY a brand-scoped casino_provider_capabilities row
// (no tenant-wide row) gets 503 on every callback, because
// ReceiveCallback resolves the capability with brandID = uuid.Nil
// unconditionally (orchestrator.go), while LaunchGame resolves with the
// real brand. LoadCapability's own query
// (`brand_id = $3 OR brand_id IS NULL`) can never match a brand-only row
// when $3 = uuid.Nil and the row's brand_id is a real, non-nil UUID.
// The admin HTTP API only ever writes tenant-wide rows today (F6), so
// this test constructs the brand-only row directly via
// casino.WriteCapability, standing in for "however a brand-scoped row
// might come to exist" (a future brand-level admin API, or a migration).
func TestCasCapRollback1_PreFix_BrandOnlyCapabilityRejectsCallbacks(t *testing.T) {
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
	// lookup genuinely finds nothing (F4's own precondition) - this is
	// what LoadCapability(ctx, tx, tenantID, uuid.Nil, providerID) does
	// inside ReceiveCallback.
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, found, lerr := casino.LoadCapability(ctx, tx, tenant.ID, uuid.Nil, "mock-casino")
		if lerr != nil {
			return lerr
		}
		if found {
			t.Fatalf("PRE-FIX EVIDENCE PRECONDITION FAILED: expected the tenant-wide (brand_id=uuid.Nil) lookup to find NOTHING when only a brand-scoped row exists, but it found one")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant-wide capability lookup: %v", err)
	}

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-e4-bet-1", "", "round-e4", "game-1",
		1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PRE-FIX EVIDENCE: expected 503 for every callback when only a brand-scoped capability row exists (F4), got %d", resp.StatusCode)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
}
