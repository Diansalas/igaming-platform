//go:build integration

// Code review B-3(b) (docs/governance/stage-10-w1-code-review.md, finding
// 3): the ADR 0088 §4.7 backstop - an ErrSettlementIntegrity abort gets its
// rejection audit written in a SEPARATE, already-committed tenant-scoped
// transaction, because the failed operation's own transaction rolled back
// - was untested at the HTTP layer. This reuses
// internal/sportsbook.TestSettlementFaultInjection_LedgerKeyBackstop's own
// fixture shape (a ledger transaction fabricated directly under the
// RESERVED settlement key, bypassing the settlement service entirely) but
// drives it through the real HTTP route, and asserts all three things the
// code review named: the 409 SETTLEMENT_INTEGRITY response, that nothing
// from the failed posting transaction committed, and that the rejection
// audit row DID commit (in its own transaction).
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// TestSettlementSimulate_IntegrityBackstop_409AndSeparateAuditCommit is
// B-3(b): fabricate a ledger transaction under the RESERVED
// "sportsbook_settlement:<bet>#1" key before any settlement is simulated,
// then settle(1) through the real HTTP route with a claim that matches the
// bet's own stored state exactly (so every §4.3/§2.4 check the service
// itself performs passes, and the call reaches the §4.7 ledger backstop).
func TestSettlementSimulate_IntegrityBackstop_409AndSeparateAuditCommit(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSettlementTestServer(t, pool, issuer, true)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, pool, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	token := mustRiskManagerToken(t, pool, srv, tenant)
	betID := uuid.MustParse(bet.ID)

	// Fabricate the fixture: a sportsbook_settlement ledger transaction
	// already occupying generation 1's key, with entries touching only
	// HOUSE/CASH and NO backing sportsbook_bet_settlements row - exactly
	// TestSettlementFaultInjection_LedgerKeyBackstop's shape
	// (internal/sportsbook/settlement_fault_injection_integration_test.go).
	reservedKey := "sportsbook_settlement:" + betID.String() + "#1"
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		wl, err := wallet.GetOrCreate(ctx, tx, tenant.ID, brand.ID, player.ID, "EUR")
		if err != nil {
			return err
		}
		house, err := ledger.GetOrCreateAccounts(ctx, tx, tenant.ID,
			ledger.AccountSpec{WalletID: nil, AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenant.ID, &wl.ID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: tenant.ID, TransactionType: ledger.TxSportsbookSettlement,
			IdempotencyKey: reservedKey, CorrelationID: betID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: house[0], Direction: ledger.Debit, Amount: 1},
				{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: 1},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fabricate the reserved-key ledger fixture: %v", err)
	}

	var sportsbookSettlementTxCountBefore int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_settlement'`,
			tenant.ID).Scan(&sportsbookSettlementTxCountBefore)
	}); err != nil {
		t.Fatalf("count fixture ledger rows: %v", err)
	}
	if sportsbookSettlementTxCountBefore != 1 {
		t.Fatalf("fixture precondition: expected exactly 1 sportsbook_settlement ledger row, got %d", sportsbookSettlementTxCountBefore)
	}

	resp := postJSON(t, srv, simulatePath(bet.ID), token, map[string]any{
		"event_type": "settle", "generation": 1, "outcome": "won", "asset_code": "EUR", "payout_amount": bet.PotentialReturn,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeSettlementIntegrity {
		t.Fatalf("expected code %q, got %q", apierror.CodeSettlementIntegrity, body.Code)
	}

	// Nothing from the failed posting transaction committed: no history
	// row, no second sportsbook_settlement ledger row under the same key
	// (still exactly the 1 fixture row), and the bet is still open.
	var histCount int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bet_settlements WHERE bet_id = $1`, betID).Scan(&histCount)
	}); err != nil {
		t.Fatalf("count history rows: %v", err)
	}
	if histCount != 0 {
		t.Fatalf("expected 0 settlement history rows after the integrity abort, got %d", histCount)
	}

	var sportsbookSettlementTxCountAfter int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_settlement'`,
			tenant.ID).Scan(&sportsbookSettlementTxCountAfter)
	}); err != nil {
		t.Fatalf("count ledger rows after the call: %v", err)
	}
	if sportsbookSettlementTxCountAfter != 1 {
		t.Fatalf("expected the call to add 0 new sportsbook_settlement ledger rows (still 1 fixture row), got %d", sportsbookSettlementTxCountAfter)
	}

	var betStatus string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM sportsbook_bets WHERE id = $1`, betID).Scan(&betStatus)
	}); err != nil {
		t.Fatalf("read bet status: %v", err)
	}
	if betStatus != "open" {
		t.Fatalf("expected the bet to remain open after the integrity abort, got %q", betStatus)
	}

	// The rejection audit DID commit - in its own, separate transaction
	// (the failed posting transaction rolled back and could not have
	// carried it).
	var count int
	var outcome, rejectionCode string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'sportsbook_bet.settlement_rejected' AND target_id = $2`,
			tenant.ID, bet.ID).Scan(&count); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT outcome, metadata->>'rejection_code' FROM audit_log
			  WHERE tenant_id = $1 AND action = 'sportsbook_bet.settlement_rejected' AND target_id = $2
			  ORDER BY created_at DESC LIMIT 1`,
			tenant.ID, bet.ID).Scan(&outcome, &rejectionCode)
	}); err != nil {
		t.Fatalf("read rejection audit row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 committed sportsbook_bet.settlement_rejected audit row, got %d", count)
	}
	if outcome != "failure" {
		t.Fatalf("rejection audit outcome = %q, want failure", outcome)
	}
	if rejectionCode != "SETTLEMENT_INTEGRITY" {
		t.Fatalf("rejection audit metadata.rejection_code = %q, want SETTLEMENT_INTEGRITY", rejectionCode)
	}
}
