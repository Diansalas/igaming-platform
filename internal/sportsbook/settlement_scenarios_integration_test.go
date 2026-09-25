//go:build integration

// ADR 0088 §2.3's exact scenario matrix: every W1 lifecycle event, with
// exact end balances (CASH/LOCKED/HOUSE), bet status, history rows,
// ledger transaction types/keys/reverses_transaction_id, causation_id
// (§2.1), and audit records (§10).
package sportsbook

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

const (
	stdStake  int64 = 1000
	stdPayout int64 = 2000 // odds 200/100 (2.00x) * 1000 stake
	funded    int64 = 10_000
)

// newStdBet funds a fresh fixture, seeds a 2.00x-odds selection, places a
// 1000 stake (potential_return 2000) and returns everything a scenario
// test needs.
func newStdBet(t *testing.T, pool *db.Pool) (f sbFixture, actor, betID uuid.UUID) {
	t.Helper()
	f = seedFixture(t, pool)
	fundWallet(t, pool, f, funded)
	sel := seedSelection(t, pool, seedSelectionParams{})
	result, err := placeBet(t, pool, f, sel, stdStake, "std-bet-"+uuid.NewString())
	if err != nil || !result.Accepted {
		t.Fatalf("place bet: accepted=%v err=%v", result.Accepted, err)
	}
	actor = seedRiskManager(t, pool, f.tenantID)
	betID = result.Bet.ID
	return f, actor, betID
}

func assertNets(t *testing.T, pool *db.Pool, f sbFixture, wantCash, wantLocked, wantHouse int64) {
	t.Helper()
	if got := cashBalance(t, pool, f) - funded; got != wantCash {
		t.Errorf("CASH net = %d, want %d", got, wantCash)
	}
	if got := lockedCashBalance(t, pool, f); got != wantLocked {
		t.Errorf("LOCKED net = %d, want %d", got, wantLocked)
	}
	if got := houseBalance(t, pool, f); got != wantHouse {
		t.Errorf("HOUSE net = %d, want %d", got, wantHouse)
	}
}

func assertAuditReplayed(t *testing.T, recs []auditRow, wantReplayed bool) {
	t.Helper()
	if len(recs) == 0 {
		t.Fatalf("expected at least one audit record")
	}
	last := recs[len(recs)-1]
	if last.ActorType != string(audit.ActorStaff) {
		t.Errorf("actor_type = %q, want staff", last.ActorType)
	}
	if last.Outcome != string(audit.OutcomeSuccess) {
		t.Errorf("outcome = %q, want success", last.Outcome)
	}
	got, _ := last.Metadata["replayed"].(bool)
	if got != wantReplayed {
		t.Errorf("metadata.replayed = %v, want %v", got, wantReplayed)
	}
	if last.Metadata["driver"] != settlementDriver {
		t.Errorf("metadata.driver = %v, want %q", last.Metadata["driver"], settlementDriver)
	}
	if last.Metadata["mode"] != settlementMode {
		t.Errorf("metadata.mode = %v, want %q", last.Metadata["mode"], settlementMode)
	}
}

// TestSettlementScenario_SettleWon covers ADR 0088 §2.3's "Settle won" row
// end-to-end: entries, end state, history row, ledger key/type, audit.
func TestSettlementScenario_SettleWon(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if res.Result != SettlementResultApplied {
		t.Fatalf("result = %q, want applied", res.Result)
	}
	if res.BetStatus != BetStatusSettledWon {
		t.Fatalf("bet status = %q, want settled_won", res.BetStatus)
	}

	assertNets(t, pool, f, stdPayout-stdStake, 0, stdStake-stdPayout)
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusSettledWon {
		t.Fatalf("stored bet status = %q, want settled_won", got)
	}

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 {
		t.Fatalf("expected 1 history row, got %d", len(hist))
	}
	rec := hist[0]
	if rec.EventKind != settlementHistoryKindSettlement || rec.Generation == nil || *rec.Generation != 1 ||
		rec.Outcome == nil || *rec.Outcome != SettlementOutcomeWon || rec.PayoutAmount == nil || *rec.PayoutAmount != stdPayout {
		t.Fatalf("unexpected settlement row: %+v", rec)
	}
	if rec.CausationRecordID != nil {
		t.Fatalf("first settlement must have nil causation_record_id, got %v", *rec.CausationRecordID)
	}
	if rec.ActorStaffAccountID == nil || *rec.ActorStaffAccountID != actor {
		t.Fatalf("actor_staff_account_id = %v, want %s", rec.ActorStaffAccountID, actor)
	}

	ltx := getLedgerTx(t, pool, f.tenantID, rec.LedgerTransactionID)
	if ltx.TransactionType != string(ledger.TxSportsbookSettlement) {
		t.Fatalf("transaction_type = %q, want sportsbook_settlement", ltx.TransactionType)
	}
	if ltx.IdempotencyKey != "sportsbook_settlement:"+betID.String()+"#1" {
		t.Fatalf("idempotency_key = %q", ltx.IdempotencyKey)
	}
	if ltx.CorrelationID != betID {
		t.Fatalf("correlation_id = %s, want bet id %s", ltx.CorrelationID, betID)
	}
	if ltx.CausationID != nil {
		t.Fatalf("causation_id must be nil for the first settlement, got %v", *ltx.CausationID)
	}
	if ltx.ProviderID != nil || ltx.ProviderTxID != nil || ltx.ReasonCode != nil {
		t.Fatalf("provider/reason fields must be nil: %+v", ltx)
	}

	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, betID.String())
	found := false
	for _, r := range recs {
		if r.Action == settlementAuditActionSettled {
			found = true
			if r.ActorID != actor {
				t.Errorf("audit actor id = %s, want %s", r.ActorID, actor)
			}
			if r.Metadata["reason_code"] != settlementAuditReasonCode {
				t.Errorf("reason_code = %v, want %q", r.Metadata["reason_code"], settlementAuditReasonCode)
			}
			replayed, _ := r.Metadata["replayed"].(bool)
			if replayed {
				t.Errorf("first settlement audit must have replayed=false")
			}
		}
	}
	if !found {
		t.Fatalf("no %s audit record found", settlementAuditActionSettled)
	}
}

// TestSettlementScenario_SettleLost covers the "Settle lost" row.
func TestSettlementScenario_SettleLost(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	if res.BetStatus != BetStatusSettledLost {
		t.Fatalf("bet status = %q, want settled_lost", res.BetStatus)
	}
	assertNets(t, pool, f, -stdStake, 0, stdStake)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 || hist[0].PayoutAmount == nil || *hist[0].PayoutAmount != 0 {
		t.Fatalf("unexpected history: %+v", hist)
	}
	ltx := getLedgerTx(t, pool, f.tenantID, hist[0].LedgerTransactionID)
	if ltx.TransactionType != string(ledger.TxSportsbookSettlement) {
		t.Fatalf("transaction_type = %q", ltx.TransactionType)
	}
}

// TestSettlementScenario_VoidBeforeSettlement covers the "Void before
// settlement" row.
func TestSettlementScenario_VoidBeforeSettlement(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	res := mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	if res.BetStatus != BetStatusVoid {
		t.Fatalf("bet status = %q, want void", res.BetStatus)
	}
	assertNets(t, pool, f, 0, 0, 0)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 || hist[0].EventKind != settlementHistoryKindVoid || hist[0].VoidReason == nil || *hist[0].VoidReason != "market_cancelled" {
		t.Fatalf("unexpected history: %+v", hist)
	}
	ltx := getLedgerTx(t, pool, f.tenantID, hist[0].LedgerTransactionID)
	if ltx.TransactionType != string(ledger.TxSportsbookVoid) {
		t.Fatalf("transaction_type = %q, want sportsbook_void", ltx.TransactionType)
	}
	if ltx.IdempotencyKey != "sportsbook_void:"+betID.String() {
		t.Fatalf("idempotency_key = %q", ltx.IdempotencyKey)
	}
	if ltx.CausationID != nil {
		t.Fatalf("void-before's causation_id must be nil")
	}

	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, betID.String())
	last := recs[len(recs)-1]
	if last.Metadata["void_reason"] != "market_cancelled" {
		t.Fatalf("void_reason metadata = %v", last.Metadata["void_reason"])
	}
	if _, ok := last.Metadata["reason_code"]; ok {
		t.Fatalf("void records must not carry reason_code (§10 reserves it for settle/rollback)")
	}
}

// TestSettlementScenario_VoidAfterSettlement_Won: settle won, then void ->
// rollback (inverse of the won settlement) + before-settlement-shape void,
// one DB transaction, two history rows, composed causation.
func TestSettlementScenario_VoidAfterSettlement_Won(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	res := mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "data_error"))
	if res.BetStatus != BetStatusVoid {
		t.Fatalf("bet status = %q, want void", res.BetStatus)
	}
	if len(res.LedgerTransactionIDs) != 2 || len(res.SettlementRecordIDs) != 2 {
		t.Fatalf("expected 2 ledger tx ids and 2 settlement record ids, got %d/%d",
			len(res.LedgerTransactionIDs), len(res.SettlementRecordIDs))
	}
	assertNets(t, pool, f, 0, 0, 0)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 3 {
		t.Fatalf("expected 3 history rows (settlement, rollback, void), got %d", len(hist))
	}
	rollback, void := hist[1], hist[2]
	if rollback.EventKind != settlementHistoryKindRollback || rollback.ReversesSettlementID == nil || *rollback.ReversesSettlementID != hist[0].ID {
		t.Fatalf("unexpected rollback row: %+v", rollback)
	}
	if void.EventKind != settlementHistoryKindVoid || void.CausationRecordID == nil || *void.CausationRecordID != rollback.ID {
		t.Fatalf("composed void must cite the rollback row: %+v", void)
	}

	rollbackTx := getLedgerTx(t, pool, f.tenantID, rollback.LedgerTransactionID)
	voidTx := getLedgerTx(t, pool, f.tenantID, void.LedgerTransactionID)
	if rollbackTx.TransactionType != string(ledger.TxSportsbookRollback) {
		t.Fatalf("rollback tx type = %q", rollbackTx.TransactionType)
	}
	if rollbackTx.ReversesTransactionID == nil || *rollbackTx.ReversesTransactionID != hist[0].LedgerTransactionID {
		t.Fatalf("rollback reverses_transaction_id = %v, want %s", rollbackTx.ReversesTransactionID, hist[0].LedgerTransactionID)
	}
	if rollbackTx.CausationID != nil {
		t.Fatalf("the rollback leg of a composed void carries nil causation_id (§2.1)")
	}
	if voidTx.TransactionType != string(ledger.TxSportsbookVoid) {
		t.Fatalf("void tx type = %q", voidTx.TransactionType)
	}
	if voidTx.CausationID == nil || *voidTx.CausationID != rollbackTx.ID {
		t.Fatalf("composed void's causation_id = %v, want the rollback tx id %s", voidTx.CausationID, rollbackTx.ID)
	}

	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, betID.String())
	var sawRolledBack, sawVoided bool
	for _, r := range recs {
		if r.Action == settlementAuditActionRolledBack {
			sawRolledBack = true
		}
		if r.Action == settlementAuditActionVoided {
			sawVoided = true
		}
	}
	if !sawRolledBack || !sawVoided {
		t.Fatalf("expected both rolled_back and voided audit actions, got %+v", recs)
	}
}

// TestSettlementScenario_VoidAfterSettlement_Lost mirrors the won case with
// a lost settlement (no payout leg to reverse).
func TestSettlementScenario_VoidAfterSettlement_Lost(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))

	res := mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "push"))
	if res.BetStatus != BetStatusVoid {
		t.Fatalf("bet status = %q, want void", res.BetStatus)
	}
	assertNets(t, pool, f, 0, 0, 0)
}

// TestSettlementScenario_Rollback_Won: settle won then roll back -> bet
// reopens, NetLocked returns to S, HOUSE/CASH nets return to placement
// state.
func TestSettlementScenario_Rollback_Won(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	settleRes := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	res := mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if res.BetStatus != BetStatusOpen {
		t.Fatalf("bet status = %q, want open", res.BetStatus)
	}
	assertNets(t, pool, f, -stdStake, stdStake, 0)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 2 || hist[1].EventKind != settlementHistoryKindRollback {
		t.Fatalf("unexpected history: %+v", hist)
	}
	ltx := getLedgerTx(t, pool, f.tenantID, hist[1].LedgerTransactionID)
	if ltx.IdempotencyKey != "sportsbook_rollback:"+betID.String()+"#1" {
		t.Fatalf("idempotency_key = %q", ltx.IdempotencyKey)
	}
	if ltx.ReversesTransactionID == nil || *ltx.ReversesTransactionID != settleRes.LedgerTransactionIDs[0] {
		t.Fatalf("reverses_transaction_id mismatch")
	}
	if ltx.CausationID != nil {
		t.Fatalf("standalone rollback causation_id must be nil")
	}
}

// TestSettlementScenario_Rollback_Lost mirrors the won rollback case.
func TestSettlementScenario_Rollback_Lost(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	res := mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if res.BetStatus != BetStatusOpen {
		t.Fatalf("bet status = %q, want open", res.BetStatus)
	}
	assertNets(t, pool, f, -stdStake, stdStake, 0)
}

// TestSettlementScenario_Resettlement: rollback generation 1, settle
// generation 2 - causation cites generation 1's rollback row/ledger tx.
func TestSettlementScenario_Resettlement(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	rollback1 := mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))

	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
	if res.BetStatus != BetStatusSettledWon || res.Generation != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	assertNets(t, pool, f, stdPayout-stdStake, 0, stdStake-stdPayout)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	gen2 := hist[2]
	if gen2.CausationRecordID == nil || *gen2.CausationRecordID != hist[1].ID {
		t.Fatalf("generation 2 settlement must cite generation 1's rollback row, got %v want %s", gen2.CausationRecordID, hist[1].ID)
	}
	gen2Tx := getLedgerTx(t, pool, f.tenantID, gen2.LedgerTransactionID)
	if gen2Tx.CausationID == nil || *gen2Tx.CausationID != rollback1.LedgerTransactionIDs[0] {
		t.Fatalf("generation 2 ledger causation_id = %v, want rollback tx %s", gen2Tx.CausationID, rollback1.LedgerTransactionIDs[0])
	}
	if gen2Tx.IdempotencyKey != "sportsbook_settlement:"+betID.String()+"#2" {
		t.Fatalf("idempotency_key = %q", gen2Tx.IdempotencyKey)
	}
}

// TestSettlementScenario_RollbackThenVoid: standalone rollback (generation
// 1) followed later by a plain void - the void is "before-settlement
// shape" and carries NO causation_record_id (only the composed void of a
// SAME-transaction void-after-settlement may cite a rollback, §3.3).
func TestSettlementScenario_RollbackThenVoid(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))

	res := mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	if res.BetStatus != BetStatusVoid {
		t.Fatalf("bet status = %q, want void", res.BetStatus)
	}
	assertNets(t, pool, f, 0, 0, 0)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	voidRec := hist[len(hist)-1]
	if voidRec.EventKind != settlementHistoryKindVoid {
		t.Fatalf("expected the last row to be a void, got %q", voidRec.EventKind)
	}
	if voidRec.CausationRecordID != nil {
		// NOTE: this assertion pins the Go decision table (voidBet never
		// attempts to set causation_record_id for a standalone
		// rollback-then-void, because the rollback here is a separate,
		// already-committed operation, not part of a composed
		// void-after-settlement). It does NOT exercise T-1's own reject
		// branch: no causation_record_id is ever submitted for T-1 to
		// evaluate here, so a bug that made T-1 accept an earlier-
		// committed rollback's id would NOT be caught by this test. T-1's
		// reject branch (an earlier-committed rollback cited as causation)
		// and its accept branch (a same-transaction one) are pinned
		// directly, at the DB level, by
		// TestDBConstraints_T1_ComposedVoidCausation_RejectsEarlierTransactionRollback
		// and TestDBConstraints_T1_ComposedVoidCausation_AcceptsSameTransaction
		// in settlement_db_constraints_integration_test.go (code review
		// finding 1 / B-1).
		t.Fatalf("rollback-then-void's void must NOT cite the earlier, separately-committed rollback (voidBet never sets causation_record_id here)")
	}
	voidTx := getLedgerTx(t, pool, f.tenantID, voidRec.LedgerTransactionID)
	if voidTx.CausationID != nil {
		t.Fatalf("rollback-then-void's ledger causation_id must be nil")
	}
}

// TestSettlementScenario_TombstoneNeverSeen: rollback(1) with no prior
// settlement writes a tombstone occupying the settlement key; no entries,
// bet stays open, NetLocked unchanged.
func TestSettlementScenario_TombstoneNeverSeen(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	res := mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if res.Result != SettlementResultTombstoned {
		t.Fatalf("result = %q, want tombstoned", res.Result)
	}
	if res.BetStatus != BetStatusOpen {
		t.Fatalf("bet status = %q, want open", res.BetStatus)
	}
	assertNets(t, pool, f, -stdStake, stdStake, 0)

	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 || hist[0].EventKind != settlementHistoryKindTombstone {
		t.Fatalf("unexpected history: %+v", hist)
	}
	ltx := getLedgerTx(t, pool, f.tenantID, hist[0].LedgerTransactionID)
	if ltx.TransactionType != string(ledger.TxTombstone) {
		t.Fatalf("transaction_type = %q, want tombstone", ltx.TransactionType)
	}
	if ltx.IdempotencyKey != "sportsbook_settlement:"+betID.String()+"#1" {
		t.Fatalf("tombstone must occupy the settlement key, got %q", ltx.IdempotencyKey)
	}

	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, betID.String())
	last := recs[len(recs)-1]
	if last.Action != settlementAuditActionTombstoned {
		t.Fatalf("action = %q, want %q", last.Action, settlementAuditActionTombstoned)
	}
}
