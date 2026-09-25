//go:build integration

package sportsbook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// ledgerTxInfo is the subset of ledger_transactions this suite asserts on
// (ADR 0088 §2.1, §2.3, §4.2).
type ledgerTxInfo struct {
	ID                    uuid.UUID
	TransactionType       string
	IdempotencyKey        string
	CorrelationID         uuid.UUID
	CausationID           *uuid.UUID
	ReversesTransactionID *uuid.UUID
	ProviderID            *string
	ProviderTxID          *string
	ReasonCode            *string
}

func getLedgerTx(t *testing.T, pool *db.Pool, tenantID, id uuid.UUID) ledgerTxInfo {
	t.Helper()
	var info ledgerTxInfo
	info.ID = id
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT transaction_type, idempotency_key, correlation_id, causation_id, reverses_transaction_id,
			        provider_id, provider_tx_id, reason_code
			   FROM ledger_transactions WHERE id = $1`, id,
		).Scan(&info.TransactionType, &info.IdempotencyKey, &info.CorrelationID, &info.CausationID,
			&info.ReversesTransactionID, &info.ProviderID, &info.ProviderTxID, &info.ReasonCode)
	})
	if err != nil {
		t.Fatalf("read ledger transaction %s: %v", id, err)
	}
	return info
}

// settlementHistory returns a bet's sportsbook_bet_settlements rows in
// insertion order, under staff tenant scope.
func settlementHistory(t *testing.T, pool *db.Pool, tenantID, betID uuid.UUID) []SettlementRecord {
	t.Helper()
	var recs []SettlementRecord
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recs, err = ListSettlementRecords(ctx, tx, betID)
		return err
	})
	if err != nil {
		t.Fatalf("list settlement records for bet %s: %v", betID, err)
	}
	return recs
}

func betStatus(t *testing.T, pool *db.Pool, tenantID, betID uuid.UUID) BetStatus {
	t.Helper()
	var b Bet
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = GetBetByID(ctx, tx, betID)
		return err
	})
	if err != nil {
		t.Fatalf("get bet %s: %v", betID, err)
	}
	return b.Status
}

// houseBalance mirrors cashBalance/lockedCashBalance (orchestrator_integration_test.go)
// for the tenant's house_gaming/EUR account - fresh per test tenant, so its
// Signed() balance IS the bet's own HOUSE net (ADR 0088 §2.3's end-state
// table), no baseline subtraction needed.
func houseBalance(t *testing.T, pool *db.Pool, f sbFixture) int64 {
	t.Helper()
	var signed int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
		if err != nil {
			return err
		}
		b, err := ledger.GetProjectedBalance(ctx, tx, accountID)
		if err != nil {
			return err
		}
		signed = b.Signed()
		return nil
	})
	if err != nil {
		t.Fatalf("read house balance: %v", err)
	}
	return signed
}

// auditRow is one audit_log row this suite reads back.
type auditRow struct {
	ActorType string
	ActorID   uuid.UUID
	Action    string
	Outcome   string
	TargetID  string
	Metadata  map[string]any
}

// auditRecordsFor returns every audit_log row for (tenant, targetType,
// targetID), oldest first.
func auditRecordsFor(t *testing.T, pool *db.Pool, tenantID uuid.UUID, targetType, targetID string) []auditRow {
	t.Helper()
	var out []auditRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT actor_type, actor_id, action, outcome, target_id, metadata
			   FROM audit_log
			  WHERE tenant_id = $1 AND target_type = $2 AND target_id = $3
			  ORDER BY created_at, id`, tenantID, targetType, targetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r auditRow
			var actorID *uuid.UUID
			var metaBytes []byte
			if err := rows.Scan(&r.ActorType, &actorID, &r.Action, &r.Outcome, &r.TargetID, &metaBytes); err != nil {
				return err
			}
			if actorID != nil {
				r.ActorID = *actorID
			}
			if len(metaBytes) > 0 {
				if err := json.Unmarshal(metaBytes, &r.Metadata); err != nil {
					return err
				}
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit records for %s/%s: %v", targetType, targetID, err)
	}
	return out
}

// debitCash simulates the player spending or withdrawing cash already in
// player_cash (a manual_adjustment debit) - used by the OB-1 test to put
// real money "out the door" before a won settlement is rolled back.
func debitCash(t *testing.T, pool *db.Pool, f sbFixture, amount int64) {
	t.Helper()
	reason := "test fixture withdrawal simulation"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adjustmentAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "debit-" + uuid.New().String(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cashAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: adjustmentAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("debit cash: %v", err)
	}
}

// seedRiskManagerInOtherTenant creates a second, unrelated tenant with its
// own active risk_manager staff user - used by cross-tenant rejection
// tests (a staff actor and a bet must never straddle tenants).
func seedOtherTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	other := seedFixture(t, pool)
	return other.tenantID
}

// seedInactiveStaff creates a risk_manager staff user and immediately
// marks it inactive (ADR 0088 §9.1 "must be active").
func seedInactiveStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := seedRiskManager(t, pool, tenantID)
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, id)
		return err
	})
	if err != nil {
		t.Fatalf("suspend staff user: %v", err)
	}
	return id
}
