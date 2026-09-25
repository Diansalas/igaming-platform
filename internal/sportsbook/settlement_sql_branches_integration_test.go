//go:build integration

// SQL branch-coverage checklist (ADR 0088 §14's explicit substitute for a
// Go mutation tool over SQL, docs/governance/stage-10-w1-code-review.md
// finding 2 / B-2): every CHECK constraint, every partial unique index and
// every T-1/T-2 IF/RAISE branch added by migration 0091 is exercised on
// BOTH sides (the RAISE/violation fires, and it does not). Branches already
// pinned elsewhere (T-1's payout/generation/wrong-type/wrong-correlation/
// player-scope/composed-void-causation checks in
// settlement_db_constraints_integration_test.go; T-2's disallowed-
// transition/history-mismatch checks in the same file; migration 0091's
// down-migration refusals in settlement_migration_0091_integration_test.go)
// are NOT duplicated here - see
// docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md for the
// full branch -> test mapping, including the "false" (happy-path) side of
// every branch below, most of which is exercised by the existing scenario
// suite (settlement_scenarios_integration_test.go) rather than repeated
// here.
package sportsbook

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// --- column CHECK constraints -----------------------------------------

// TestSQLBranch_ColumnCheck_EventKindInvalid: event_kind must be one of the
// four admitted values.
func TestSQLBranch_ColumnCheck_EventKindInvalid(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "chk-eventkind-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'bogus', 1, 'won', $3, 'EUR', $4)`,
		f.tenantID, betID, stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected the event_kind CHECK to reject an unrecognised event_kind")
	}
}

// TestSQLBranch_ColumnCheck_PayoutNegativeRejected: payout_amount >= 0.
// Isolated on a 'tombstone' row (T-1 never inspects payout_amount for that
// kind), so the column CHECK - not T-1 - is what fires.
func TestSQLBranch_ColumnCheck_PayoutNegativeRejected(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxTombstone, betID, "chk-payoutneg-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, payout_amount, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'tombstone', 1, -1, 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected the payout_amount >= 0 CHECK to reject a negative payout_amount")
	}
}

// TestSQLBranch_ColumnCheck_VoidReasonInvalid: void_reason must be one of
// the three admitted values (player_self_exclusion is deliberately absent,
// ADR 0034 §14.7).
func TestSQLBranch_ColumnCheck_VoidReasonInvalid(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookVoid, betID, "chk-voidreason-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, void_reason, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'void', 'player_self_exclusion', 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected the void_reason CHECK to reject player_self_exclusion (not yet an admitted value)")
	}
}

// --- shape CHECK constraints (NOT NULL-iff rules, ADR 0088 §3.2) -------

// TestSQLBranch_ShapeCheck_SettlementRequiresOutcome: a 'settlement' row
// with outcome NULL is rejected by sportsbook_bet_settlements_settlement_
// shape, not by T-1 (T-1's own outcome checks are no-ops when outcome IS
// NULL, since 'won'/'lost' comparisons against NULL are never true).
func TestSQLBranch_ShapeCheck_SettlementRequiresOutcome(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "shape-settle-outcome-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, payout_amount, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'settlement', 1, 0, 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected sportsbook_bet_settlements_settlement_shape to reject outcome IS NULL")
	}
}

// TestSQLBranch_ShapeCheck_RollbackRejectsOutcome: a 'rollback' row must
// carry outcome/payout_amount/void_reason NULL. reverses_settlement_id is
// set to a REAL, latest settlement so T-1's own rollback checks pass and
// the shape CHECK is what fires.
func TestSQLBranch_ShapeCheck_RollbackRejectsOutcome(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	settlementID := res.SettlementRecordIDs[0]
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookRollback, betID, "shape-rollback-outcome-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, outcome, reverses_settlement_id, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'rollback', 1, 'won', $3, 'EUR', $4)`,
		f.tenantID, betID, settlementID, ltxID)
	if err == nil {
		t.Fatalf("expected sportsbook_bet_settlements_rollback_shape to reject a non-NULL outcome")
	}
}

// TestSQLBranch_ShapeCheck_VoidRejectsGeneration: a 'void' row must carry
// generation NULL. Fixture bet has no settlement/void history, so T-1's
// void-specific has_void/has_unreversed checks both pass and the shape
// CHECK is what fires.
func TestSQLBranch_ShapeCheck_VoidRejectsGeneration(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookVoid, betID, "shape-void-generation-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, void_reason, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'void', 1, 'market_cancelled', 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected sportsbook_bet_settlements_void_shape to reject a non-NULL generation")
	}
}

// TestSQLBranch_ShapeCheck_TombstoneRejectsPayout: a 'tombstone' row must
// carry payout_amount NULL. T-1 never inspects payout_amount for a
// tombstone, so the shape CHECK is what fires.
func TestSQLBranch_ShapeCheck_TombstoneRejectsPayout(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxTombstone, betID, "shape-tomb-payout-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, payout_amount, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'tombstone', 1, 0, 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected sportsbook_bet_settlements_tombstone_shape to reject a non-NULL payout_amount")
	}
}

// --- T-1 branches not already pinned by settlement_db_constraints_integration_test.go ---

// TestSQLBranch_T1_TenantAssetMismatch_RejectsWrongAsset: NEW.asset_code
// must equal the bet's own asset_code.
func TestSQLBranch_T1_TenantAssetMismatch_RejectsWrongAsset(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-asset-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'settlement', 1, 'won', $3, 'USD', $4)`,
		f.tenantID, betID, stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject asset_code != the bet's own asset_code")
	}
	if !strings.Contains(err.Error(), "tenant/asset must equal") {
		t.Fatalf("unexpected error (expected T-1's tenant/asset RAISE): %v", err)
	}
}

// TestSQLBranch_T1_HasVoid_RejectsSettlementOnVoidBet: a settlement/
// tombstone insert on an already-void bet is rejected (has_void), even
// though the Go decision table never reaches the database in this case -
// this is the DB-level backstop's own true branch.
func TestSQLBranch_T1_HasVoid_RejectsSettlementOnVoidBet(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-hasvoid-settle-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a settlement insert on a void (terminal) bet")
	}
	if !strings.Contains(err.Error(), "is void (terminal)") {
		t.Fatalf("unexpected error (expected T-1's has_void RAISE): %v", err)
	}
}

// TestSQLBranch_T1_HasUnreversed_RejectsSecondSettlementBeforeRollback: a
// settlement insert while the bet already has an un-reversed settlement is
// rejected (has_unreversed), independent of the generation-sequence check.
func TestSQLBranch_T1_HasUnreversed_RejectsSecondSettlementBeforeRollback(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-hasunrev-settle-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 2, "won", stdPayout, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a second settlement while generation 1 is un-reversed")
	}
	if !strings.Contains(err.Error(), "already has an un-reversed settlement") {
		t.Fatalf("unexpected error (expected T-1's has_unreversed RAISE): %v", err)
	}
}

// TestSQLBranch_T1_RollbackTarget_RejectsWrongEventKind: a rollback row's
// reverses_settlement_id must reference a 'settlement' row - citing a
// 'void' row (a real row of this same bet) is rejected.
func TestSQLBranch_T1_RollbackTarget_RejectsWrongEventKind(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	voidRowID := settlementHistory(t, pool, f.tenantID, betID)[0].ID
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookRollback, betID, "t1-rbtarget-wrongkind-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertRollbackSQL, f.tenantID, betID, 1, voidRowID, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a rollback citing a non-settlement row")
	}
	if !strings.Contains(err.Error(), "rollback target must be") {
		t.Fatalf("unexpected error (expected T-1's rollback-target RAISE): %v", err)
	}
}

// TestSQLBranch_T1_OnlyLatestSettlementCanBeRolledBack: after a
// re-settlement (generation 1 rolled back, generation 2 settled), a SECOND
// rollback citing generation 1's (already-superseded) settlement row is
// rejected, even though generation 1 has no un-reversed settlement of its
// own citing it again would otherwise pass the "target found" check.
func TestSQLBranch_T1_OnlyLatestSettlementCanBeRolledBack(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	res1 := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
	gen1SettlementID := res1.SettlementRecordIDs[0]

	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookRollback, betID, "t1-onlylatest-"+uuid.NewString())
	err := rawInsertSettlement(t, pool, f.tenantID, insertRollbackSQL, f.tenantID, betID, 1, gen1SettlementID, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a rollback of generation 1 after generation 2 has settled")
	}
	if !strings.Contains(err.Error(), "only the latest settlement can be rolled back") {
		t.Fatalf("unexpected error (expected T-1's only-latest RAISE): %v", err)
	}
}

// TestSQLBranch_T1_VoidHasVoid_RejectsSecondVoid: a second void row on an
// already-void bet is rejected by T-1's OWN has_void check for void kind
// (distinct from the settlement/tombstone has_void branch above, and from
// the sportsbook_bet_settlements_one_void_per_bet unique index, which only
// ever fires under a genuine race - see
// TestSQLBranch_UniqueIndex_OneVoidPerBet_BackstopsConcurrentVoids below).
func TestSQLBranch_T1_VoidHasVoid_RejectsSecondVoid(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookVoid, betID, "t1-voidhasvoid-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, void_reason, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'void', 'data_error', 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a second void row on an already-void bet")
	}
	if !strings.Contains(err.Error(), "is already void") {
		t.Fatalf("unexpected error (expected T-1's void has_void RAISE): %v", err)
	}
}

// TestSQLBranch_T1_VoidHasUnreversed_RejectsVoidWhileSettled: a standalone
// void row (no causation_record_id - the "roll back first" case, distinct
// from the Go service's own composed void-after-settlement, which always
// rolls back BEFORE inserting the void so it never hits this branch) is
// rejected while an un-reversed settlement exists.
func TestSQLBranch_T1_VoidHasUnreversed_RejectsVoidWhileSettled(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookVoid, betID, "t1-voidhasunrev-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, void_reason, asset_code, ledger_transaction_id)
		 VALUES ($1, $2, 'void', 'market_cancelled', 'EUR', $3)`,
		f.tenantID, betID, ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a standalone void while a settlement is un-reversed")
	}
	if !strings.Contains(err.Error(), "void requires no un-reversed settlement") {
		t.Fatalf("unexpected error (expected T-1's void has_unreversed RAISE): %v", err)
	}
}

// TestSQLBranch_T1_LedgerTransactionNotVisible_Rejected: ledger_
// transaction_id must reference a row visible to this connection.
func TestSQLBranch_T1_LedgerTransactionNotVisible_Rejected(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)

	err := rawInsertSettlement(t, pool, f.tenantID, insertSettlementSQL, f.tenantID, betID, 1, "won", stdPayout, uuid.New())
	if err == nil {
		t.Fatalf("expected T-1 to reject a ledger_transaction_id that does not exist")
	}
	if !strings.Contains(err.Error(), "is not visible") {
		t.Fatalf("unexpected error (expected T-1's ledger-transaction-visibility RAISE): %v", err)
	}
}

// TestSQLBranch_T1_LedgerTypeMismatch_RollbackKind: the expected_type CASE
// covers 'rollback' too (settlement/void kinds are already pinned by
// TestDBConstraints_T1_RejectsWrongLedgerTransactionType): a rollback row
// citing a sportsbook_settlement-typed ledger transaction is rejected.
func TestSQLBranch_T1_LedgerTypeMismatch_RollbackKind(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	// Wrong type: a fresh sportsbook_settlement-typed transaction, not the
	// sportsbook_rollback the rollback event_kind expects.
	wrongLtxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-rollbacktype-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID, insertRollbackSQL, f.tenantID, betID, 1, res.SettlementRecordIDs[0], wrongLtxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a rollback row whose ledger transaction is not sportsbook_rollback-typed")
	}
	if !strings.Contains(err.Error(), "does not match the event") {
		t.Fatalf("unexpected error (expected T-1's ledger type/correlation RAISE): %v", err)
	}
}

// TestSQLBranch_T1_Causation_RejectsRecordFromAnotherBet: a re-settlement's
// causation_record_id must reference THIS bet's generation g-1 row -
// citing a real rollback row that belongs to a DIFFERENT bet is rejected
// (distinct from TestDBConstraints_T1_CausationRules's "no id at all" and
// "id references nothing" cases: this pins the "cause.bet_id <> NEW.bet_id"
// arm specifically, with a genuinely existing row).
func TestSQLBranch_T1_Causation_RejectsRecordFromAnotherBet(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))

	// A second, unrelated bet with its own real generation-1 rollback row.
	sel2 := seedSelection(t, pool, seedSelectionParams{})
	otherBet, err := placeBet(t, pool, f, sel2, stdStake, "other-bet-"+uuid.NewString())
	if err != nil || !otherBet.Accepted {
		t.Fatalf("place second bet: accepted=%v err=%v", otherBet.Accepted, err)
	}
	mustSimulate(t, pool, f.tenantID, settleEvent(otherBet.Bet.ID, actor, 1, SettlementOutcomeLost, 0))
	otherRollback := mustSimulate(t, pool, f.tenantID, rollbackEvent(otherBet.Bet.ID, actor, 1))
	otherRollbackRowID := otherRollback.SettlementRecordIDs[0]

	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookSettlement, betID, "t1-causation-otherbet-"+uuid.NewString())
	err = rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements
			(tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, ledger_transaction_id, causation_record_id)
		 VALUES ($1, $2, 'settlement', 2, 'won', $3, 'EUR', $4, $5)`,
		f.tenantID, betID, stdPayout, ltxID, otherRollbackRowID)
	if err == nil {
		t.Fatalf("expected T-1 to reject a causation_record_id belonging to a different bet")
	}
	if !strings.Contains(err.Error(), "must cite this bet's generation") {
		t.Fatalf("unexpected error (expected T-1's causation RAISE): %v", err)
	}
}

// TestSQLBranch_T1_CausationNotPermitted_RejectsOnRollback: the final
// ELSIF branch - causation_record_id is not permitted outside a
// generation>1 settlement or a composed void - rejects a rollback row that
// sets it.
func TestSQLBranch_T1_CausationNotPermitted_RejectsOnRollback(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	ltxID := postRawLedgerTx(t, pool, f, ledger.TxSportsbookRollback, betID, "t1-causenotpermitted-"+uuid.NewString())

	err := rawInsertSettlement(t, pool, f.tenantID,
		`INSERT INTO sportsbook_bet_settlements
			(tenant_id, bet_id, event_kind, generation, reverses_settlement_id, asset_code, ledger_transaction_id, causation_record_id)
		 VALUES ($1, $2, 'rollback', 1, $3, 'EUR', $4, $3)`,
		f.tenantID, betID, res.SettlementRecordIDs[0], ltxID)
	if err == nil {
		t.Fatalf("expected T-1 to reject causation_record_id on a rollback row")
	}
	if !strings.Contains(err.Error(), "causation_record_id is not permitted for this event") {
		t.Fatalf("unexpected error (expected T-1's causation-not-permitted RAISE): %v", err)
	}
}

// --- T-2 branch not already pinned -------------------------------------

// TestSQLBranch_T2_InsertRejectsNonOpenStatus: a bet must always be
// INSERTed 'open' (T-2's TG_OP = 'INSERT' branch). Builds a real,
// FK-satisfying ledger_transaction fixture (Dr CASH / Cr LOCKED, the same
// shape PlaceBet posts) so the ONLY thing under test is the status value.
func TestSQLBranch_T2_InsertRejectsNonOpenStatus(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, funded)
	sel := seedSelection(t, pool, seedSelectionParams{})
	betID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		locked, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedCash, "EUR")
		if err != nil {
			return err
		}
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxSportsbookBet,
			IdempotencyKey: "t2-insert-nonopen-" + uuid.NewString(), CorrelationID: betID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cash, Direction: ledger.Debit, Amount: stdStake},
				{LedgerAccountID: locked, Direction: ledger.Credit, Amount: stdStake},
			},
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO sportsbook_bets
				(id, tenant_id, brand_id, player_account_id, wallet_id, selection_id, asset_code,
				 stake_amount, odds_numerator, odds_denominator, potential_return, status, idempotency_key, ledger_transaction_id)
			 VALUES ($1, $2, $3, $4, $5, $6, 'EUR', $7, $8, $9, $10, 'settled_won', $11, $12)`,
			betID, f.tenantID, f.brandID, f.playerAccountID, f.walletID, sel.ID,
			stdStake, sel.OddsNumerator, sel.OddsDenominator, stdPayout, "t2-insert-nonopen-key-"+uuid.NewString(), res.TransactionID)
		return err
	})
	if err == nil {
		t.Fatalf("expected T-2 to reject a bet INSERTed with a non-open status")
	}
	if !strings.Contains(err.Error(), "a bet is always inserted open") {
		t.Fatalf("unexpected error (expected T-2's insert-status RAISE): %v", err)
	}
}

// TestSQLBranch_T2_PlayerScopedUpdateIsANoOpUnderNormalScope documents (like
// TestDBConstraints_RLS_UpdateAndDeleteAreNoOpsUnderNormalScope does for
// sportsbook_bet_settlements) why T-2's own player-scope RAISE cannot be
// reached through an ordinary player-scoped UPDATE: sportsbook_bets defines
// player_self_scope as FOR SELECT only (migration 0078) - there is no
// player-scoped UPDATE policy at all - so under FORCE ROW LEVEL SECURITY
// the UPDATE matches zero rows and the per-row trigger never fires. This is
// not a gap: the row is provably unchanged.
func TestSQLBranch_T2_PlayerScopedUpdateIsANoOpUnderNormalScope(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET status = 'open' WHERE id = $1`, betID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected 0 rows affected under RLS (no player-scoped UPDATE policy exists), got %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error (RLS should silently affect 0 rows, not error): %v", err)
	}
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusSettledWon {
		t.Fatalf("bet status changed despite 0 RowsAffected being reported: %q", got)
	}
}

// TestSQLBranch_T2_PlayerScopedStatusChangeRejected_WithPermissiveRLSPolicy
// is the isolated proof (scratch database, mirroring
// TestDBConstraints_DenyTriggers_BlockMutationEvenWithAPermissiveRLSPolicy)
// that T-2's OWN player-scope check independently rejects a status UPDATE,
// even if a future migration mistakenly added a permissive player-scoped
// UPDATE policy to sportsbook_bets.
func TestSQLBranch_T2_PlayerScopedStatusChangeRejected_WithPermissiveRLSPolicy(t *testing.T) {
	pool := scratchPoolMigratedUp(t, "sb0091t2playerscope_")
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE POLICY qa_test_permit_player_update ON sportsbook_bets
			FOR UPDATE USING (true) WITH CHECK (true)`)
		return err
	})
	if err != nil {
		t.Fatalf("add temporary permissive UPDATE policy: %v", err)
	}

	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET status = 'open' WHERE id = $1`, betID)
		return err
	})
	if err == nil {
		t.Fatalf("expected T-2 to reject a status UPDATE under a player-scoped connection even though RLS now permits it")
	}
	if !strings.Contains(err.Error(), "status is never changed under a player-scoped connection") {
		t.Fatalf("unexpected error (expected T-2's player-scope RAISE): %v", err)
	}
}

// --- partial unique indexes (concurrent-race backstops) -----------------
//
// T-1's has_void/has_unreversed/generation-sequence checks are a
// read-then-insert pattern (ADR 0088 §3.3's own doc comment: "soundness...
// rests on INV-LOCK-E4", i.e. on the CALLER already holding the bet's L1
// FOR UPDATE row lock - which the real service always does, but a raw
// insert bypassing insertSettlementRecord does not). The three partial
// unique indexes are the backstop for exactly that gap. Each test below
// forces the genuine race with an uncommitted first raw insert (mirroring
// settlement_concurrency_integration_test.go's own blocker/
// waitForBlockedCount technique): the second transaction's OWN T-1
// evaluation runs under READ COMMITTED against the not-yet-committed first
// row, so it does NOT see it and passes T-1 cleanly - proving the unique
// index, not T-1, is what ultimately rejects it.

// raceUniqueIndex runs insertFn(tx) in two concurrent transactions against
// betID. The first transaction is held open (its statement executed but
// not committed) until the second transaction's own INSERT has queued
// behind the resulting unique-index conflict, at which point the first is
// committed. Returns both transactions' errors.
func raceUniqueIndex(t *testing.T, pool *db.Pool, f sbFixture, betID uuid.UUID, insertFn func(ctx context.Context, tx pgx.Tx) error) (firstErr, secondErr error) {
	t.Helper()
	firstReady := make(chan int32, 1)
	firstProceed := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := insertFn(ctx, tx); err != nil {
				return err
			}
			pid, err := backendPID(ctx, tx)
			if err != nil {
				return err
			}
			firstReady <- pid
			<-firstProceed
			return nil
		})
	}()
	firstPID := waitOrFatal(t, firstReady, 5_000_000_000, "first transaction to insert and report its backend pid")

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return insertFn(ctx, tx)
		})
	}()

	if !waitForBlockedCount(t, pool, firstPID, 1) {
		t.Fatalf("second insert never queued behind the first's uncommitted unique-index entry (race did not materialise)")
	}
	close(firstProceed)
	firstErr = waitOrFatal(t, firstDone, 5_000_000_000, "first transaction to commit")
	secondErr = waitOrFatal(t, secondDone, 5_000_000_000, "second transaction to finish")
	return firstErr, secondErr
}

// TestSQLBranch_UniqueIndex_OnePerGeneration_BackstopsConcurrentTombstones:
// two concurrent rollback(1) calls on a bet with NO settlement history race
// to insert generation-1 tombstones. Both pass T-1 (neither sees the
// other's uncommitted row), so sportsbook_bet_settlements_one_per_
// generation is what rejects the loser.
func TestSQLBranch_UniqueIndex_OnePerGeneration_BackstopsConcurrentTombstones(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)

	insertFn := func(ctx context.Context, tx pgx.Tx) error {
		key := "t1-onepergen-race-" + uuid.NewString()
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxTombstone, IdempotencyKey: key, CorrelationID: betID,
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, asset_code, ledger_transaction_id)
			 VALUES ($1, $2, 'tombstone', 1, 'EUR', $3)`,
			f.tenantID, betID, res.TransactionID)
		return err
	}

	firstErr, secondErr := raceUniqueIndex(t, pool, f, betID, insertFn)
	if firstErr != nil {
		t.Fatalf("expected the first concurrent tombstone insert to succeed: %v", firstErr)
	}
	if secondErr == nil {
		t.Fatalf("expected the second concurrent tombstone insert to be rejected by the one_per_generation unique index")
	}
	if !strings.Contains(secondErr.Error(), "sportsbook_bet_settlements_one_per_generation") {
		t.Fatalf("expected the one_per_generation unique-index violation, got: %v", secondErr)
	}
}

// TestSQLBranch_UniqueIndex_OneVoidPerBet_BackstopsConcurrentVoids: two
// concurrent void raw inserts on a fresh open bet (no existing void) race;
// both pass T-1's has_void check (READ COMMITTED, neither sees the other's
// uncommitted row), so sportsbook_bet_settlements_one_void_per_bet is what
// rejects the loser.
//
// The fixture ledger_transactions row is inserted directly (bypassing
// ledger.Post), not via postRawLedgerTx/ledger.Post as elsewhere in this
// file: Post's own L3 pre-lock takes a FOR UPDATE on the bet's shared CASH/
// HOUSE wallet_balance_projection rows, which would itself serialise A and
// B on that lock and let A's void row COMMIT before B's has_void check ever
// runs - defeating the very race this test exists to force. A raw
// ledger_transactions row with no entries takes no such lock, so both
// transactions' T-1 evaluations genuinely race, exactly as they would for a
// caller that (bug notwithstanding) issued the settlements INSERT without
// holding the bet's L1 row lock.
func TestSQLBranch_UniqueIndex_OneVoidPerBet_BackstopsConcurrentVoids(t *testing.T) {
	pool := testPool(t)
	f, _, betID := newStdBet(t, pool)

	insertFn := func(ctx context.Context, tx pgx.Tx) error {
		key := "t1-onevoid-race-" + uuid.NewString()
		var ltxID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id)
			 VALUES ($1, 'sportsbook_void', $2, $3) RETURNING id`,
			f.tenantID, key, betID).Scan(&ltxID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, void_reason, asset_code, ledger_transaction_id)
			 VALUES ($1, $2, 'void', 'market_cancelled', 'EUR', $3)`,
			f.tenantID, betID, ltxID)
		return err
	}

	firstErr, secondErr := raceUniqueIndex(t, pool, f, betID, insertFn)
	if firstErr != nil {
		t.Fatalf("expected the first concurrent void insert to succeed: %v", firstErr)
	}
	if secondErr == nil {
		t.Fatalf("expected the second concurrent void insert to be rejected by the one_void_per_bet unique index")
	}
	if !strings.Contains(secondErr.Error(), "sportsbook_bet_settlements_one_void_per_bet") {
		t.Fatalf("expected the one_void_per_bet unique-index violation, got: %v", secondErr)
	}
}

// TestSQLBranch_UniqueIndex_OneRollbackPerSettlement_BackstopsConcurrentRollbacks:
// two concurrent rollback raw inserts, both citing the SAME (latest, only)
// settlement row, race; both pass T-1's rollback-target/only-latest checks
// (READ COMMITTED, neither sees the other's uncommitted row), so
// sportsbook_bet_settlements_one_rollback_per_settlement is what rejects
// the loser.
func TestSQLBranch_UniqueIndex_OneRollbackPerSettlement_BackstopsConcurrentRollbacks(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	settlementID := res.SettlementRecordIDs[0]

	insertFn := func(ctx context.Context, tx pgx.Tx) error {
		key := "t1-onerollback-race-" + uuid.NewString()
		txID, err := postRawLedgerTxOnTx(ctx, tx, f.tenantID, f.walletID, ledger.TxSportsbookRollback, betID, key)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, insertRollbackSQL, f.tenantID, betID, 1, settlementID, txID)
		return err
	}

	firstErr, secondErr := raceUniqueIndex(t, pool, f, betID, insertFn)
	if firstErr != nil {
		t.Fatalf("expected the first concurrent rollback insert to succeed: %v", firstErr)
	}
	if secondErr == nil {
		t.Fatalf("expected the second concurrent rollback insert to be rejected by the one_rollback_per_settlement unique index")
	}
	if !strings.Contains(secondErr.Error(), "sportsbook_bet_settlements_one_rollback_per_settlement") {
		t.Fatalf("expected the one_rollback_per_settlement unique-index violation, got: %v", secondErr)
	}
}

// --- audit/history metadata population branches (Q2 mutation-pass survivors) ---
//
// gremlins found three LIVED mutants outside the §14-named scope (payout
// validation, decision tables, NetLocked, LockProjectionsForPostings, lock
// order) but still inside settlement.go: writeTransition's "include
// RequestID iff non-empty" (line 979) and settlementAuditMetadata/
// RecordSettlementRejection's "include generation iff non-zero" (lines
// 1104, 1141). These two tests pin both sides of each so no LIVED mutant
// in settlement.go goes unaddressed.

// TestSQLBranch_AuditMetadata_RequestIDIncludedIffNonEmpty: a settle event
// WITH a RequestID gets a history row whose request_id is set; a void event
// with no RequestID gets a NULL request_id (writeTransition's own `if
// ev.RequestID != ""` branch, both sides).
func TestSQLBranch_AuditMetadata_RequestIDIncludedIffNonEmpty(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	withID := settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout)
	withID.RequestID = "req-" + uuid.NewString()
	res := mustSimulate(t, pool, f.tenantID, withID)
	hist := settlementHistory(t, pool, f.tenantID, betID)
	var settleRec *SettlementRecord
	for i := range hist {
		if hist[i].ID == res.SettlementRecordIDs[0] {
			settleRec = &hist[i]
		}
	}
	if settleRec == nil || settleRec.RequestID == nil || *settleRec.RequestID != withID.RequestID {
		t.Fatalf("expected the settlement row's request_id to be set to %q, got %+v", withID.RequestID, settleRec)
	}

	f2, actor2, betID2 := newStdBet(t, pool)
	noID := voidEvent(betID2, actor2, "market_cancelled")
	noID.RequestID = ""
	mustSimulate(t, pool, f2.tenantID, noID)
	hist2 := settlementHistory(t, pool, f2.tenantID, betID2)
	if len(hist2) != 1 || hist2[0].RequestID != nil {
		t.Fatalf("expected the void row's request_id to be NULL for an empty RequestID, got %+v", hist2)
	}
}

// TestSQLBranch_AuditMetadata_GenerationIncludedIffNonZero: the applied-path
// audit metadata (settlementAuditMetadata) and the rejection-path metadata
// (RecordSettlementRejection) both include "generation" only when
// ev.Generation != 0 - true for settle/rollback (generation >= 1), false
// for void (generation is always 0/absent).
func TestSQLBranch_AuditMetadata_GenerationIncludedIffNonZero(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, betID.String())
	var settledRow *auditRow
	for i := range recs {
		if recs[i].Action == settlementAuditActionSettled {
			settledRow = &recs[i]
		}
	}
	if settledRow == nil {
		t.Fatalf("expected a %s audit row, got %+v", settlementAuditActionSettled, recs)
	}
	if _, ok := settledRow.Metadata["generation"]; !ok {
		t.Fatalf("expected the settle audit row's metadata to include \"generation\" (event_type=settle), got %+v", settledRow.Metadata)
	}

	f2, actor2, betID2 := newStdBet(t, pool)
	mustSimulate(t, pool, f2.tenantID, voidEvent(betID2, actor2, "market_cancelled"))
	recs2 := auditRecordsFor(t, pool, f2.tenantID, settlementAuditTargetType, betID2.String())
	var voidedRow *auditRow
	for i := range recs2 {
		if recs2[i].Action == settlementAuditActionVoided {
			voidedRow = &recs2[i]
		}
	}
	if voidedRow == nil {
		t.Fatalf("expected a %s audit row, got %+v", settlementAuditActionVoided, recs2)
	}
	if _, ok := voidedRow.Metadata["generation"]; ok {
		t.Fatalf("expected the void audit row's metadata to OMIT \"generation\" (event_type=void, Generation=0), got %+v", voidedRow.Metadata)
	}

	// RecordSettlementRejection's own copy of the same "iff non-zero" rule
	// (line 1104): a rejected void (generation=0) - here a payload-mismatch
	// void_reason redelivery, so it goes through rejectSettlement rather
	// than the replay path - omits "generation" too.
	rejectRes, err := simulateSettlement(t, pool, f2.tenantID, voidEvent(betID2, actor2, "data_error"))
	if err != nil {
		t.Fatalf("unexpected error re-simulating the void with a different reason: %v", err)
	}
	if rejectRes.RejectionCode != SettlementRejectPayloadMismatch {
		t.Fatalf("expected a %s rejection for the void_reason mismatch, got result=%q code=%q", SettlementRejectPayloadMismatch, rejectRes.Result, rejectRes.RejectionCode)
	}
	rejRecs := auditRecordsFor(t, pool, f2.tenantID, settlementAuditTargetType, betID2.String())
	var rejectionRow *auditRow
	for i := range rejRecs {
		if rejRecs[i].Action == settlementAuditActionRejected {
			rejectionRow = &rejRecs[i]
		}
	}
	if rejectionRow == nil {
		t.Fatalf("expected a %s audit row", settlementAuditActionRejected)
	}
	if _, ok := rejectionRow.Metadata["generation"]; ok {
		t.Fatalf("expected the void rejection audit row's metadata to OMIT \"generation\", got %+v", rejectionRow.Metadata)
	}
}
