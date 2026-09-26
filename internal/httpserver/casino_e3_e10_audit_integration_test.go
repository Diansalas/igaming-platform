//go:build integration

// Stage 10.3 gate 10.3-W1 ledger-finance condition C2: the E3
// `casino_bet.rejected_tombstoned` audit row was never asserted by any
// test, and no sequential E10 (win-after-tombstone) test existed outside
// the concurrency race. This file adds both, plus the six-point no-effect
// check on E3 (tolerating the ONE expected rejection audit row - the
// generic noeffect.AssertNoCasinoEffect helper requires zero audit delta,
// which is correct for every OTHER rejection path but not this one, so
// this file reimplements the same checklist inline with that one
// deliberate exception).
package httpserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"

	"net/http"
)

// assertNoCasinoEffectExceptAuditDelta is the Stage 10.1 six-point
// no-effect checklist, applied to a rejection path that itself is
// EXPECTED to write exactly wantAuditDelta new audit_log rows (E3's own
// `casino_bet.rejected_tombstoned` record) - every other dimension
// (ledger_transactions, ledger_entries, tombstone count, round count,
// debits==credits, projections) must still be completely unchanged.
func assertNoCasinoEffectExceptAuditDelta(t *testing.T, pool *db.Pool, tenantID uuid.UUID, before noeffect.CasinoSnapshot, wantAuditDelta int) {
	t.Helper()
	after := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenantID})
	if before.LedgerTransactionCount[tenantID] != after.LedgerTransactionCount[tenantID] {
		t.Errorf("expected ledger_transactions count unchanged, got %d -> %d", before.LedgerTransactionCount[tenantID], after.LedgerTransactionCount[tenantID])
	}
	if before.LedgerEntryCount[tenantID] != after.LedgerEntryCount[tenantID] {
		t.Errorf("expected ledger_entries count unchanged, got %d -> %d", before.LedgerEntryCount[tenantID], after.LedgerEntryCount[tenantID])
	}
	if before.TombstoneCount[tenantID] != after.TombstoneCount[tenantID] {
		t.Errorf("expected tombstone count unchanged, got %d -> %d", before.TombstoneCount[tenantID], after.TombstoneCount[tenantID])
	}
	if before.CasinoProviderRoundCount[tenantID] != after.CasinoProviderRoundCount[tenantID] {
		t.Errorf("expected casino_provider_rounds count unchanged, got %d -> %d", before.CasinoProviderRoundCount[tenantID], after.CasinoProviderRoundCount[tenantID])
	}
	if !after.DebitsEqualCredits[tenantID] {
		t.Errorf("expected SUM(debits) == SUM(credits) to still hold")
	}
	if before.ProjectionDebitTotal[tenantID] != after.ProjectionDebitTotal[tenantID] || before.ProjectionCreditTotal[tenantID] != after.ProjectionCreditTotal[tenantID] {
		t.Errorf("expected projections unchanged")
	}
	gotDelta := after.AuditLogCount[tenantID] - before.AuditLogCount[tenantID]
	if gotDelta != wantAuditDelta {
		t.Errorf("expected exactly %d new audit_log row(s), got %d", wantAuditDelta, gotDelta)
	}
}

// TestCasCapRollback1_E3_AuditRowAndBoundedEffect is condition C2's E3
// requirement: the late-original-after-tombstone decline (E3) writes
// exactly one `casino_bet.rejected_tombstoned` audit row, with the
// expected fields, and nothing else - no new ledger row, no new/changed
// casino_provider_rounds row, no extra tombstone, and debits/credits and
// projections stay balanced and unchanged.
func TestCasCapRollback1_E3_AuditRowAndBoundedEffect(t *testing.T) {
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

	const lateOriginalTxID = "cas-e3-audit-late-original"
	const rollbackTxID = "cas-e3-audit-early-rollback"
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackTxID, lateOriginalTxID, "round-e3-audit", "game-1",
		0, "EUR", "", "", player.ID, uuid.Nil)
	rollbackResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	if rollbackResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the early rollback (tombstones), got %d", rollbackResp.StatusCode)
	}
	rollbackResp.Body.Close()

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	lateOriginalPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, lateOriginalTxID, "", "round-e3-audit", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	lateResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", lateOriginalPayload)
	defer lateResp.Body.Close()
	if lateResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (named decline) for the late original, got %d", lateResp.StatusCode)
	}

	assertNoCasinoEffectExceptAuditDelta(t, pool, tenant.ID, before, 1)

	// The E3 audit row itself: action, outcome and a target that names the
	// PROVIDER's own reference (not a fabricated ledger_transaction id -
	// gate 10.3-W1 code review cosmetic finding, orchestrator.go's E3
	// branch), never a "ledger_transaction" TargetType, since nothing
	// posts on this path.
	var count int
	var targetType, outcome string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_bet.rejected_tombstoned' AND target_id = $2`,
			tenant.ID, "mock-casino:"+lateOriginalTxID).Scan(&count); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT target_type, outcome FROM audit_log WHERE tenant_id = $1 AND action = 'casino_bet.rejected_tombstoned' AND target_id = $2`,
			tenant.ID, "mock-casino:"+lateOriginalTxID).Scan(&targetType, &outcome)
	})
	if err != nil {
		t.Fatalf("query E3 audit row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 casino_bet.rejected_tombstoned audit row, got %d", count)
	}
	if targetType != "casino_provider_tx" {
		t.Fatalf("expected target_type=casino_provider_tx (not a fabricated ledger_transaction id), got %q", targetType)
	}
	if outcome != "failure" {
		t.Fatalf("expected outcome=failure for a rejection record, got %q", outcome)
	}
}

// TestCasinoWebhook_E10_SequentialWinAfterTombstone is condition C2's
// sequential E10 requirement: a rollback tombstones a win reference BEFORE
// that win is ever delivered (sequentially, not raced), and the later win
// delivery is a deterministic 409 (ErrOriginalTombstoned) with NO ledger,
// audit, tombstone, or round effect of its own - postWin's E10 branch
// returns a bare error (aborting its whole transaction), so this is one of
// the few rejection paths where the ordinary noeffect.AssertNoCasinoEffect
// (zero audit delta too) applies unmodified.
func TestCasinoWebhook_E10_SequentialWinAfterTombstone(t *testing.T) {
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

	const roundID = "round-e10-sequential"
	const betTxID = "cas-e10-seq-bet"
	const winTxID = "cas-e10-seq-win"
	const rollbackOfWinTxID = "cas-e10-seq-rollback"

	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betTxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the bet, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	// Tombstone the win's reference BEFORE the win ever arrives.
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, rollbackOfWinTxID, winTxID, roundID, game.ProviderGameID,
		0, "EUR", "", "", player.ID, uuid.Nil)
	rbResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	if rbResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 tombstoning the never-seen win reference, got %d", rbResp.StatusCode)
	}
	rbResp.Body.Close()

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winTxID, "", roundID, game.ProviderGameID,
		2500, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.Nil)
	winResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	defer winResp.Body.Close()
	if winResp.StatusCode != http.StatusConflict {
		t.Fatalf("Stage 10.3 E10 (sequential): expected 409 for a win whose own reference is already tombstoned, got %d", winResp.StatusCode)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)

	// The bet itself must remain untouched (un-reversed, single row) - the
	// rejected win produced no financial effect whatsoever.
	if got := countCasinoLedgerRowsForTx(t, pool, tenant.ID, betTxID); got != 1 {
		t.Fatalf("expected the original bet's single row untouched, got %d", got)
	}
	if balance := walletCashBalance(t, srv, player.Tokens.AccessToken); balance != 9000 {
		t.Fatalf("expected the wallet to reflect only the bet's stake taken (9000), got %d", balance)
	}
}
