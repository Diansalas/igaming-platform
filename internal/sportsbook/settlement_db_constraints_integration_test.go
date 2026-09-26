//go:build integration

// DB-level enforcement (ADR 0088 §3.2, §3.3, §3.5): trigger T-1
// (sportsbook_bet_settlements_validate), trigger T-2
// (sportsbook_bets_status_transition), the deny triggers (append-only),
// and RLS (staff insert/select, player self-scope). All run against the
// SHARED already-migrated TEST_DATABASE_URL database, connected as its
// owning role - the same role every other integration test uses, which
// still holds UPDATE/DELETE/TRUNCATE privileges before any REVOKE, so a
// pass here proves the TRIGGERS bind regardless of privilege (§3.5's
// "deny triggers are the binding control").
package sportsbook

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// postRawLedgerTx posts an arbitrary, balanced ledger transaction directly
// (bypassing the settlement service) - a raw fixture builder for trigger
// tests that need a ledger_transactions row of a SPECIFIC type/correlation
// to hand to a raw INSERT into sportsbook_bet_settlements.
func postRawLedgerTx(t *testing.T, pool *db.Pool, f sbFixture, txType ledger.TransactionType, correlationID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		house, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: nil, AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: txType, IdempotencyKey: key, CorrelationID: correlationID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: house[0], Direction: ledger.Debit, Amount: 1},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1},
			},
		})
		id = res.TransactionID
		return err
	})
	if err != nil {
		t.Fatalf("post raw ledger transaction: %v", err)
	}
	return id
}

// rawInsertSettlement issues the raw INSERT statement T-1 validates,
// returning the Postgres error (nil on success) so tests can assert on its
// message.
func rawInsertSettlement(t *testing.T, pool *db.Pool, tenantID uuid.UUID, sql string, args ...any) error {
	t.Helper()
	return pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

const insertSettlementSQL = `INSERT INTO sportsbook_bet_settlements
	(tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, ledger_transaction_id)
	VALUES ($1, $2, 'settlement', $3, $4, $5, 'EUR', $6)`

// TestDBConstraints_T1_RejectsPayoutMismatch: T-1 rejects payout_amount !=
// bet.potential_return for a 'won' row, even though the caller is the
// table owner (not merely the runtime role).
func TestDBConstraints_T1_RejectsPayoutMismatch(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-payout-mismatch-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout+1, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject payout_amount != potential_return")
	}
	if !strings.Contains(err.Error(), "won payout must equal") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDBConstraints_T1_RejectsWonPayoutZero: a 'won' row with
// payout_amount = 0 is rejected too (V-3: a zero-payout win cannot post).
func TestDBConstraints_T1_RejectsWonPayoutZero(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-won-zero-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "won", 0, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a zero-payout won row")
	}
}

// TestDBConstraints_T1_RejectsLostPayoutNonzero: a 'lost' row must carry
// payout_amount = 0.
func TestDBConstraints_T1_RejectsLostPayoutNonzero(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-lost-nonzero-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "lost", 1, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a nonzero-payout lost row")
	}
}

// TestDBConstraints_T1_RejectsWrongGeneration: generation must be
// max(generation)+1 over settlement/tombstone rows.
func TestDBConstraints_T1_RejectsWrongGeneration(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-gen-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 2, "won", stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject generation 2 with no generation 1 row")
	}
	if !strings.Contains(err.Error(), "generation") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDBConstraints_T1_RejectsWrongLedgerTransactionType: the ledger
// transaction's type must match the event_kind (settlement ->
// sportsbook_settlement).
func TestDBConstraints_T1_RejectsWrongLedgerTransactionType(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	// Wrong type: this is a sportsbook_void transaction, not a settlement.
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookVoid, betID, "t1-wrongtype-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a ledger transaction of the wrong type")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDBConstraints_T1_RejectsWrongCorrelation: the ledger transaction's
// correlation_id must equal bet_id.
func TestDBConstraints_T1_RejectsWrongCorrelation(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, uuid.New(), "t1-wrongcorr-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a ledger transaction correlated to a different bet")
	}
}

// TestDBConstraints_T1_CausationRules: a re-settlement (generation 2) must
// cite this bet's generation-1 rollback/tombstone row; an arbitrary or
// missing causation_record_id is rejected.
func TestDBConstraints_T1_CausationRules(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))

	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-causation-"+uuid.NewString())

	// No causation_record_id at all for generation 2: rejected.
	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 2, "won", stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a generation-2 settlement with no causation_record_id")
	}

	// A causation_record_id pointing at an unrelated row: rejected.
	otherLtxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-causation-2-"+uuid.NewString())
	err = rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements
			(tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, ledger_transaction_id, causation_record_id)
		 VALUES ($1, $2, 'settlement', 2, 'won', $3, 'EUR', $4, $5)`,
		f.tenantID, betID, stdPayout, otherLtxID, uuid.New())
	if err == nil {
		t.Fatalf("expected T-1 to reject a causation_record_id that doesn't reference this bet's generation-1 rollback")
	}
}

// TestDBConstraints_T2_RejectsDisallowedTransition: a direct UPDATE to a
// status pair outside §3.1's allowed set is rejected (e.g. settled_won ->
// void directly, or void -> anything).
func TestDBConstraints_T2_RejectsDisallowedTransition(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET status = 'open' WHERE id = $1`, betID)
		return err
	})
	if err == nil {
		t.Fatalf("expected T-2 to reject void -> open (void is terminal)")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDBConstraints_T2_RejectsHistoryInconsistentStatus: even an allowed
// transition pair is rejected if it disagrees with the history-derived
// status (status is a cache, never an independent fact).
func TestDBConstraints_T2_RejectsHistoryInconsistentStatus(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	// History says settled_won (via the real service); try to force the
	// status to settled_lost directly - an allowed-shape UPDATE(open-
	// looking pair is not even reachable since OLD=open here) so use a bet
	// that is open in the DB but manufacture history disagreement by
	// settling then attempting an UPDATE mismatched with the actual outcome.
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	// Bet is 'open' again with a rolled-back settlement in history. Try to
	// jump straight to settled_lost with NO settlement-outcome-lost history
	// row to back it (the (open, settled_lost) pair is allowed in §3.1, but
	// history disagrees since there is no un-reversed 'lost' settlement).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET status = 'settled_lost' WHERE id = $1`, betID)
		return err
	})
	if err == nil {
		t.Fatalf("expected T-2 to reject a status update unsupported by history")
	}
	if !strings.Contains(err.Error(), "does not match the settlement history") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDBConstraints_ImmutableFieldsFrozenThroughLifecycle regression-tests
// that stake/odds/potential_return/references stay frozen through every
// settlement transition (sportsbook_bets_enforce_immutable_fields,
// migration 0087, unchanged by W1).
func TestDBConstraints_ImmutableFieldsFrozenThroughLifecycle(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET stake_amount = stake_amount + 1 WHERE id = $1`, betID)
		return err
	})
	if err == nil {
		t.Fatalf("expected the immutable-fields trigger to reject a stake_amount change after settlement lifecycle activity")
	}
}

// TestDBConstraints_RLS_UpdateAndDeleteAreNoOpsUnderNormalScope documents
// (rather than merely asserts) an important precondition for the deny-
// trigger test below: this migration defines ONLY tenant_staff_select,
// tenant_staff_insert and player_self_scope (all FOR SELECT/INSERT) - no
// UPDATE or DELETE policy exists at all. Under FORCE ROW LEVEL SECURITY
// with no applicable policy for a command, Postgres treats every row as
// invisible to THAT command, so an UPDATE/DELETE against an
// otherwise-real, tenant-scoped row silently affects ZERO rows and never
// even reaches the deny trigger (a per-ROW trigger only fires for rows
// that were actually matched). This is stronger than the trigger for the
// UPDATE/DELETE case in the current schema, but it also means a raw
// "owner still holds UPDATE/DELETE privilege" probe under ordinary tenant
// scope proves RLS, not the trigger - see
// TestDBConstraints_DenyTriggers_BlockMutationEvenWithAPermissiveRLSPolicy
// for the isolated proof the trigger itself independently blocks
// UPDATE/DELETE/TRUNCATE, exactly as ADR 0088 §14 (finding S3) asks for.
func TestDBConstraints_RLS_UpdateAndDeleteAreNoOpsUnderNormalScope(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	hist := settlementHistory(t, pool, f.tenantID, betID)
	rowID := hist[0].ID

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE sportsbook_bet_settlements SET outcome = 'lost' WHERE id = $1`, rowID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected 0 rows affected under RLS (no UPDATE policy exists), got %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error (RLS should silently affect 0 rows, not error): %v", err)
	}

	// The row must still be exactly as it was: RLS's silent no-op is not a
	// silent success at bypassing anything.
	after := settlementHistory(t, pool, f.tenantID, betID)
	if len(after) != 1 || after[0].Outcome == nil || *after[0].Outcome != SettlementOutcomeWon {
		t.Fatalf("row was mutated despite 0 RowsAffected being reported: %+v", after)
	}
}

// TestDBConstraints_DenyTriggers_BlockMutationEvenWithAPermissiveRLSPolicy
// is ADR 0088 §14's finding S3, proven on a scratch database: even if a
// future migration mistakenly added a permissive UPDATE/DELETE RLS policy
// on sportsbook_bet_settlements (making rows genuinely visible/writable to
// RLS), the deny triggers independently reject UPDATE, DELETE and
// TRUNCATE - for the table OWNER, a role that still holds those raw SQL
// privileges. This is deliberately run against a throwaway scratch
// database: adding a real (if temporary) permissive policy to the shared
// TEST_DATABASE_URL database would leave a dangerous policy behind for
// every other test in the suite if cleanup were ever skipped.
func TestDBConstraints_DenyTriggers_BlockMutationEvenWithAPermissiveRLSPolicy(t *testing.T) {
	pool := scratchPoolMigratedUp(t, "sb0091denytrig_")
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE POLICY qa_test_permit_write ON sportsbook_bet_settlements
			FOR UPDATE USING (true) WITH CHECK (true)`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `CREATE POLICY qa_test_permit_delete ON sportsbook_bet_settlements FOR DELETE USING (true)`)
		return err
	})
	if err != nil {
		t.Fatalf("add temporary permissive UPDATE/DELETE policies: %v", err)
	}

	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	hist := settlementHistory(t, pool, f.tenantID, betID)
	rowID := hist[0].ID

	t.Run("UPDATE", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE sportsbook_bet_settlements SET outcome = 'lost' WHERE id = $1`, rowID)
			return err
		})
		if err == nil {
			t.Fatalf("expected the deny trigger to reject UPDATE even though RLS now permits it")
		}
	})
	t.Run("DELETE", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM sportsbook_bet_settlements WHERE id = $1`, rowID)
			return err
		})
		if err == nil {
			t.Fatalf("expected the deny trigger to reject DELETE even though RLS now permits it")
		}
	})
	t.Run("TRUNCATE", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE sportsbook_bet_settlements`)
			return err
		})
		if err == nil {
			t.Fatalf("expected the deny trigger to reject TRUNCATE")
		}
	})
}

// TestDBConstraints_RLS_PlayerScopedInsertRejected: an INSERT under a
// player-scoped connection is rejected (T-1's own player-scope RAISE, plus
// the tenant_staff_insert policy's WITH CHECK). Security review finding
// P3-6 (docs/governance/stage-10-w1-security-review.md): the test must
// identify WHICH control rejected it, by asserting T-1's specific message
// substring, not merely "err != nil".
func TestDBConstraints_RLS_PlayerScopedInsertRejected(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "rls-player-insert-"+uuid.NewString())

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout, ltxID)
		return err
	})
	if err == nil {
		t.Fatalf("expected a player-scoped INSERT into sportsbook_bet_settlements to be rejected")
	}
	if !strings.Contains(err.Error(), "history rows are never written under a player-scoped connection") {
		t.Fatalf("expected T-1's player-scope RAISE message, got: %v", err)
	}
}

// TestDBConstraints_RLS_NoTenantContextInsertRejected: an INSERT with NO
// tenant context set at all (pool.WithoutTenant) is rejected by T-1's
// "parent bet is not visible" RAISE - distinct from the player-scoped
// case above, which fires T-1's FIRST check before the bet is even read.
// With no tenant context the player-scope check passes trivially (the
// setting is empty, not "player-scoped"), so this pins the SECOND
// failure mode: the parent bet lookup itself finds nothing, because
// sportsbook_bets' own RLS denies visibility with no tenant set (security
// review finding P3-6).
func TestDBConstraints_RLS_NoTenantContextInsertRejected(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "rls-no-tenant-insert-"+uuid.NewString())

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout, ltxID)
		return err
	})
	if err == nil {
		t.Fatalf("expected an INSERT with no tenant context to be rejected")
	}
	if !strings.Contains(err.Error(), "is not visible") {
		t.Fatalf("expected T-1's \"parent bet ... is not visible\" RAISE message, got: %v", err)
	}
}

// TestDBConstraints_RLS_PlayerSelfScope_SeesOnlyOwnBets: a player-scoped
// SELECT returns only rows belonging to that player's own bets, even
// though both players' history rows exist in the same tenant.
func TestDBConstraints_RLS_PlayerSelfScope_SeesOnlyOwnBets(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	second := seedSecondPlayer(t, pool, f)
	fundWallet(t, pool, second, funded)
	sel2 := seedSelection(t, pool, seedSelectionParams{})
	res2, err := placeBet(t, pool, second, sel2, stdStake, "second-player-bet-"+uuid.NewString())
	if err != nil || !res2.Accepted {
		t.Fatalf("place second player's bet: accepted=%v err=%v", res2.Accepted, err)
	}
	mustSimulate(t, pool, f.tenantID, settleEvent(res2.Bet.ID, actor, 1, SettlementOutcomeLost, 0))

	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT bet_id FROM sportsbook_bet_settlements`)
		if err != nil {
			return err
		}
		defer rows.Close()
		seenOther := false
		count := 0
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			count++
			if id == res2.Bet.ID {
				seenOther = true
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if seenOther {
			t.Fatalf("player-scoped SELECT leaked another player's settlement row")
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 own row visible, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scoped select: %v", err)
	}
}

// --- T-1 composed-void causation (code review finding 1 / B-1) -------------
//
// docs/governance/stage-10-w1-code-review.md finding 1: T-1's "only the
// composed void of a void-after-settlement may cite a rollback, and only
// one inserted by THIS SAME TRANSACTION" rule (migrations/0091…up.sql
// :244-255) is checked by comparing the candidate rollback row's xmin
// against pg_current_xact_id() - which is always the TOP-LEVEL
// transaction id. These three tests pin both directions plus the
// documented failure mode of that mechanism.

const insertRollbackSQL = `INSERT INTO sportsbook_bet_settlements
	(tenant_id, bet_id, event_kind, generation, asset_code, reverses_settlement_id, ledger_transaction_id)
	VALUES ($1, $2, 'rollback', $3, 'EUR', $4, $5) RETURNING id`

const insertVoidWithCausationSQL = `INSERT INTO sportsbook_bet_settlements
	(tenant_id, bet_id, event_kind, void_reason, asset_code, causation_record_id, ledger_transaction_id)
	VALUES ($1, $2, 'void', $3, 'EUR', $4, $5)`

// postRawLedgerTxOnTx is postRawLedgerTx's same balanced-fixture-posting
// shape (Dr HOUSE 1 / Cr CASH 1), except it runs on a tx the CALLER already
// holds open, rather than opening its own WithTenant transaction - required
// here because the whole point of these tests is to control exactly which
// statements land in which database transaction (and, for the savepoint
// case, which sub-transaction).
func postRawLedgerTxOnTx(ctx context.Context, tx pgx.Tx, tenantID, walletID uuid.UUID, txType ledger.TransactionType, correlationID uuid.UUID, key string) (uuid.UUID, error) {
	house, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: nil, AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
	if err != nil {
		return uuid.Nil, err
	}
	cash, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &walletID, ledger.AccountPlayerCash, "EUR")
	if err != nil {
		return uuid.Nil, err
	}
	res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: txType, IdempotencyKey: key, CorrelationID: correlationID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: house[0], Direction: ledger.Debit, Amount: 1},
			{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1},
		},
	})
	if err != nil {
		return uuid.Nil, err
	}
	return res.TransactionID, nil
}

// TestDBConstraints_T1_ComposedVoidCausation_RejectsEarlierTransactionRollback
// is B-1(a): a void row whose causation_record_id points at a rollback row
// that a DIFFERENT, EARLIER, already-committed transaction inserted (the
// real "rollback-then-void" shape TestSettlementScenario_RollbackThenVoid
// exercises through the service, which never even attempts to set
// causation_record_id there) must be rejected if something DOES try to set
// it - proving the reject branch is not merely "the Go code never asks for
// this", but that T-1 itself actively refuses it.
func TestDBConstraints_T1_ComposedVoidCausation_RejectsEarlierTransactionRollback(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	// Committed in ITS OWN transaction, well before the void attempt below.
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))

	hist := settlementHistory(t, pool, f.tenantID, betID)
	rollbackRow := hist[len(hist)-1]
	if rollbackRow.EventKind != settlementHistoryKindRollback {
		t.Fatalf("fixture precondition: expected the last history row to be the rollback, got %q", rollbackRow.EventKind)
	}

	voidLtxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookVoid, betID, "t1-composedvoid-early-"+uuid.NewString())
	err := rawInsertSettlement(t, pool, f.tenantID, insertVoidWithCausationSQL,
		f.tenantID, betID, "market_cancelled", rollbackRow.ID, voidLtxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a void citing a rollback committed by an earlier, separate transaction")
	}
	if !strings.Contains(err.Error(), "may cite only a rollback of this bet inserted by the same transaction") {
		t.Fatalf("unexpected error (expected T-1's same-transaction causation message): %v", err)
	}
}

// TestDBConstraints_T1_ComposedVoidCausation_AcceptsSameTransaction is
// B-1(b): a rollback row and a void row citing it, both inserted by ONE
// database transaction (the composed-void shape
// TestSettlementScenario_VoidAfterSettlement_Won exercises end-to-end
// through the service), must be ACCEPTED by T-1's xmin check - the direct,
// DB-level positive-branch pin the code review found missing.
func TestDBConstraints_T1_ComposedVoidCausation_AcceptsSameTransaction(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	settlementRowID := settlementHistory(t, pool, f.tenantID, betID)[0].ID

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rollbackTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookRollback, betID,
			"t1-composed-same-rollback-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post rollback ledger tx: %w", err)
		}
		var rollbackRowID uuid.UUID
		if err := tx.QueryRow(ctx, insertRollbackSQL, f.tenantID, betID, 1, settlementRowID, rollbackTxID).Scan(&rollbackRowID); err != nil {
			return fmt.Errorf("insert rollback row: %w", err)
		}

		voidTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookVoid, betID,
			"t1-composed-same-void-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post void ledger tx: %w", err)
		}
		_, err = tx.Exec(ctx, insertVoidWithCausationSQL, f.tenantID, betID, "data_error", rollbackRowID, voidTxID)
		return err
	})
	if err != nil {
		t.Fatalf("expected T-1 to accept a same-transaction rollback+composed-void, got: %v", err)
	}
}

// TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsAccepted is
// B-1(c), SB-T1-XMIN RESOLVED (migration 0093, ADR 0088 §3.3 follow-up):
// before 0093, T-1 compared a candidate causation row's xmin against
// pg_current_xact_id(), which is ALWAYS the top-level transaction id. A
// row inserted under a SAVEPOINT (even after RELEASE SAVEPOINT) keeps the
// subtransaction's own xid as its stored xmin, which never equals the
// top-level xid, so a rollback row inserted inside a savepoint, followed
// by a void citing it in the SAME top-level transaction, was wrongly
// rejected even though it is exactly the legitimate composed-void shape.
//
// Migration 0093 replaces the check with
// pg_xact_status(<reconstructed xid8>) IS NOT DISTINCT FROM 'in progress',
// which classifies a released-savepoint row (however nested) as belonging
// to the current transaction's own tree, and now ACCEPTS this shape. This
// test name and assertion were flipped accordingly; the previous
// "deliberately pinned as REJECTED" framing described a fail-closed
// availability defect (no money moved, HTTP 409 SETTLEMENT_INTEGRITY), not
// a desired outcome, and that defect is now fixed.
func TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsAccepted(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	settlementRowID := settlementHistory(t, pool, f.tenantID, betID)[0].ID

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rollbackTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookRollback, betID,
			"t1-composed-savepoint-rollback-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post rollback ledger tx: %w", err)
		}

		if _, err := tx.Exec(ctx, "SAVEPOINT sb_t1_xmin_probe"); err != nil {
			return fmt.Errorf("open savepoint: %w", err)
		}
		var rollbackRowID uuid.UUID
		if err := tx.QueryRow(ctx, insertRollbackSQL, f.tenantID, betID, 1, settlementRowID, rollbackTxID).Scan(&rollbackRowID); err != nil {
			return fmt.Errorf("insert rollback row under savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT sb_t1_xmin_probe"); err != nil {
			return fmt.Errorf("release savepoint: %w", err)
		}

		voidTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookVoid, betID,
			"t1-composed-savepoint-void-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post void ledger tx: %w", err)
		}
		_, err = tx.Exec(ctx, insertVoidWithCausationSQL, f.tenantID, betID, "data_error", rollbackRowID, voidTxID)
		return err
	})
	if err != nil {
		t.Fatalf("SB-T1-XMIN: expected T-1 to accept a composed void whose rollback row was inserted under a "+
			"released savepoint (migration 0093), got: %v", err)
	}
}

// TestDBConstraints_T1_ComposedVoidCausation_NestedSavepointIsAccepted
// pins the "however nested" part of the pg_xact_status fix: the rollback
// row is inserted under an INNER savepoint nested inside an OUTER
// savepoint, both released, before the composed void cites it on the
// outer (top-level) transaction. pg_xact_status classifies every xid in
// the current transaction's own tree - however deeply nested - as
// 'in progress', so this must also be accepted.
func TestDBConstraints_T1_ComposedVoidCausation_NestedSavepointIsAccepted(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	settlementRowID := settlementHistory(t, pool, f.tenantID, betID)[0].ID

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rollbackTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookRollback, betID,
			"t1-composed-nested-savepoint-rollback-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post rollback ledger tx: %w", err)
		}

		if _, err := tx.Exec(ctx, "SAVEPOINT sb_t1_xmin_outer"); err != nil {
			return fmt.Errorf("open outer savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT sb_t1_xmin_inner"); err != nil {
			return fmt.Errorf("open inner savepoint: %w", err)
		}
		var rollbackRowID uuid.UUID
		if err := tx.QueryRow(ctx, insertRollbackSQL, f.tenantID, betID, 1, settlementRowID, rollbackTxID).Scan(&rollbackRowID); err != nil {
			return fmt.Errorf("insert rollback row under nested savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT sb_t1_xmin_inner"); err != nil {
			return fmt.Errorf("release inner savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT sb_t1_xmin_outer"); err != nil {
			return fmt.Errorf("release outer savepoint: %w", err)
		}

		voidTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookVoid, betID,
			"t1-composed-nested-savepoint-void-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post void ledger tx: %w", err)
		}
		_, err = tx.Exec(ctx, insertVoidWithCausationSQL, f.tenantID, betID, "data_error", rollbackRowID, voidTxID)
		return err
	})
	if err != nil {
		t.Fatalf("SB-T1-XMIN: expected T-1 to accept a composed void whose rollback row was inserted under a "+
			"released, nested savepoint (migration 0093), got: %v", err)
	}
}

// TestDBConstraints_T1_ComposedVoidCausation_RollbackToSavepointFailsFK: a
// rollback row inserted under a savepoint that is then rolled back with
// ROLLBACK TO SAVEPOINT (not RELEASE) genuinely ceases to exist in the
// current transaction - Postgres discards it. Confirmed empirically at
// implementation time: because the rollback row is gone, the bet's
// settlement is once again "unreversed" from T-1's own point of view, so
// T-1's void-precondition check (event_kind = 'void' -> has_unreversed)
// fires FIRST and rejects the void before the causation branch's own
// `SELECT ... INTO cause` (which would otherwise hit the pre-existing
// `cause.id IS NULL` not-found branch) is ever reached. This asserts that
// actual, empirically-confirmed rejection shape - not the xmin-specific
// message - so this test cannot silently start asserting the wrong branch
// if T-1's check ordering changes in the future.
func TestDBConstraints_T1_ComposedVoidCausation_RollbackToSavepointFailsFK(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	settlementRowID := settlementHistory(t, pool, f.tenantID, betID)[0].ID

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rollbackTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookRollback, betID,
			"t1-composed-rollbacktosp-rollback-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post rollback ledger tx: %w", err)
		}

		if _, err := tx.Exec(ctx, "SAVEPOINT sb_t1_xmin_rb"); err != nil {
			return fmt.Errorf("open savepoint: %w", err)
		}
		var rollbackRowID uuid.UUID
		if err := tx.QueryRow(ctx, insertRollbackSQL, f.tenantID, betID, 1, settlementRowID, rollbackTxID).Scan(&rollbackRowID); err != nil {
			return fmt.Errorf("insert rollback row under savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sb_t1_xmin_rb"); err != nil {
			return fmt.Errorf("roll back to savepoint: %w", err)
		}

		voidTxID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookVoid, betID,
			"t1-composed-rollbacktosp-void-"+uuid.NewString())
		if err != nil {
			return fmt.Errorf("post void ledger tx: %w", err)
		}
		_, err = tx.Exec(ctx, insertVoidWithCausationSQL, f.tenantID, betID, "data_error", rollbackRowID, voidTxID)
		return err
	})
	if err == nil {
		t.Fatalf("expected T-1 to reject a void citing a rollback row that no longer exists " +
			"(ROLLBACK TO SAVEPOINT discarded it)")
	}
	if !strings.Contains(err.Error(), "void requires no un-reversed settlement") {
		t.Fatalf("unexpected error (expected T-1's has_unreversed void-precondition message, since the "+
			"discarded rollback leaves the settlement unreversed again): %v", err)
	}
}
