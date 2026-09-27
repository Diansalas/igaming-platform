//go:build integration

// ADR 0088 §4.3's decision tables (settle/rollback/void), §2.4's payout
// validation (V-1..V-4), §9.2's field-matrix validation, and the staff
// actor gate (§9.1).
package sportsbook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// callSimulate runs SimulateSettlementEvent inside a transaction scoped to
// txTenant, WITHOUT overwriting ev.TenantID (unlike simulateSettlement) -
// needed for the validation/actor tests below, which deliberately
// construct events with a zero, missing or mismatched tenant id.
func callSimulate(t *testing.T, pool *db.Pool, txTenant uuid.UUID, ev SettlementEvent) (SettlementResult, error) {
	t.Helper()
	var res SettlementResult
	err := pool.WithTenant(context.Background(), txTenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = SimulateSettlementEvent(ctx, tx, ev)
		return err
	})
	return res, err
}

func mustReject(t *testing.T, res SettlementResult, err error, code string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.Rejected() {
		t.Fatalf("expected a rejection, got result %q", res.Result)
	}
	if res.RejectionCode != code {
		t.Fatalf("rejection code = %q, want %q", res.RejectionCode, code)
	}
}

// TestSettlementDecision_ExactReplay_Settle: an exact redelivery of
// settle(1) is replayed - no new ledger rows, no new history row, audit
// replayed=true.
func TestSettlementDecision_ExactReplay_Settle(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	first := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	before := countLedgerTransactions(t, pool, f)
	second := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	after := countLedgerTransactions(t, pool, f)

	if second.Result != SettlementResultReplayed {
		t.Fatalf("result = %q, want replayed", second.Result)
	}
	if len(second.SettlementRecordIDs) != 1 || second.SettlementRecordIDs[0] != first.SettlementRecordIDs[0] {
		t.Fatalf("replay must return the original settlement record id")
	}
	if after != before {
		t.Fatalf("replay must not post a new sportsbook_bet ledger transaction (count %d -> %d)", before, after)
	}
	hist := settlementHistory(t, pool, f.tenantID, betID)
	if len(hist) != 1 {
		t.Fatalf("replay must not insert a new history row, got %d rows", len(hist))
	}
	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, betID.String())
	assertAuditReplayed(t, recs, true)
}

// TestSettlementDecision_PayloadMismatch_Settle: same generation, a
// different claim -> SETTLEMENT_PAYLOAD_MISMATCH, nothing posted.
func TestSettlementDecision_PayloadMismatch_Settle(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	cases := []SettlementEvent{
		settleEvent(betID, actor, 1, SettlementOutcomeLost, 0),          // outcome differs
		settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout+1), // payout differs
	}
	mismatchedAsset := settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout)
	mismatchedAsset.ClaimAssetCode = "USD"
	cases = append(cases, mismatchedAsset)

	histBefore := len(settlementHistory(t, pool, f.tenantID, betID))
	for i, ev := range cases {
		res, err := simulateSettlement(t, pool, f.tenantID, ev)
		mustReject(t, res, err, SettlementRejectPayloadMismatch)
		if !res.Alert {
			t.Fatalf("case %d: payload mismatch must alert", i)
		}
	}
	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != histBefore {
		t.Fatalf("payload mismatch must not post any row, history grew %d -> %d", histBefore, got)
	}
}

// TestSettlementDecision_PayloadMismatch_Void mirrors the settle case for
// void_reason.
func TestSettlementDecision_PayloadMismatch_Void(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))

	res, err := simulateSettlement(t, pool, f.tenantID, voidEvent(betID, actor, "push"))
	mustReject(t, res, err, SettlementRejectPayloadMismatch)

	// Same reason: replayed.
	replay, err := simulateSettlement(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	if err != nil || replay.Result != SettlementResultReplayed {
		t.Fatalf("same void_reason redelivery: result=%q err=%v", replay.Result, err)
	}
}

// TestSettlementDecision_Tombstone_LateSettleRejected_ThenNextGenerationSucceeds
// (ADR 0088 §14): tombstone(1) -> late settle(1) rejected
// SETTLEMENT_TOMBSTONED -> settle(2) succeeds, causation = the tombstone.
func TestSettlementDecision_Tombstone_LateSettleRejected_ThenNextGenerationSucceeds(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	tomb := mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if tomb.Result != SettlementResultTombstoned {
		t.Fatalf("expected tombstoned, got %q", tomb.Result)
	}

	late, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustReject(t, late, err, SettlementRejectTombstoned)

	next := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
	if next.BetStatus != BetStatusSettledWon {
		t.Fatalf("settle(2) after tombstone: status = %q", next.BetStatus)
	}
	hist := settlementHistory(t, pool, f.tenantID, betID)
	gen2 := hist[len(hist)-1]
	if gen2.CausationRecordID == nil || *gen2.CausationRecordID != tomb.SettlementRecordIDs[0] {
		t.Fatalf("settle(2)'s causation must be the tombstone row")
	}
}

// TestSettlementDecision_DelayedSettleAfterRollbackAndResettle: a delayed
// duplicate of settle(1) arrives AFTER rollback(1) and settle(2) already
// applied - must be replayed (returns the ORIGINAL generation-1 row), never
// counted as a new generation (ADR 0038 §14.1's exact concern).
func TestSettlementDecision_DelayedSettleAfterRollbackAndResettle(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	first := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))

	delayed, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	if err != nil {
		t.Fatalf("delayed settle(1): %v", err)
	}
	if delayed.Result != SettlementResultReplayed {
		t.Fatalf("delayed settle(1) result = %q, want replayed", delayed.Result)
	}
	if len(delayed.SettlementRecordIDs) != 1 || delayed.SettlementRecordIDs[0] != first.SettlementRecordIDs[0] {
		t.Fatalf("delayed settle(1) must return the ORIGINAL generation-1 row")
	}
	// The bet must still be settled_won at generation 2 - the delayed
	// duplicate must not have disturbed current state.
	if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusSettledWon {
		t.Fatalf("bet status after delayed replay = %q, want settled_won", got)
	}
}

// TestSettlementDecision_RollbackRedeliveryAfterComposedVoid: after a
// void-after-settlement (rollback + void in one transaction), a later
// standalone rollback(g) redelivery resolves as replayed (same key).
func TestSettlementDecision_RollbackRedeliveryAfterComposedVoid(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "data_error"))

	res, err := simulateSettlement(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if err != nil {
		t.Fatalf("rollback redelivery: %v", err)
	}
	if res.Result != SettlementResultReplayed {
		t.Fatalf("result = %q, want replayed", res.Result)
	}
}

// TestSettlementDecision_GenerationOutOfSequence covers both the gap and
// stale variants.
func TestSettlementDecision_GenerationOutOfSequence(t *testing.T) {
	pool := testPool(t)

	t.Run("gap", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
		mustReject(t, res, err, SettlementRejectGenerationSequence)
		if res.Alert {
			t.Fatalf("generation-out-of-sequence is NOT in the alerted set (ADR 0088 §4.4)")
		}
	})

	t.Run("stale (open bet, non-adjacent generation)", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
		mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
		// Bet is open again, G=1 (from the reversed generation-1 settlement).
		// G+1=2; generation 3 is neither a replay of an existing row nor
		// G+1, so it must be rejected as out of sequence (not confused with
		// BET_ALREADY_SETTLED, which requires the bet to be settled).
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 3, SettlementOutcomeWon, stdPayout))
		mustReject(t, res, err, SettlementRejectGenerationSequence)
	})

	t.Run("settled bet, generation beyond current: BET_ALREADY_SETTLED not GENERATION_OUT_OF_SEQUENCE", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
		mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
		mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
		// The bet is now settled_won at generation 2; ANY other generation
		// (not a replay of 2) is BET_ALREADY_SETTLED, per §4.3's row order -
		// "bet settled_* (g > G)" is checked before the generic
		// out-of-sequence catch-all, and a rollback is required first.
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 4, SettlementOutcomeWon, stdPayout))
		mustReject(t, res, err, SettlementRejectBetAlreadySettled)
	})

	t.Run("rollback gap", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		// No settlement exists yet; rollback(2) is neither g=G+1=1 (tombstone
		// path) nor an existing row.
		res, err := simulateSettlement(t, pool, f.tenantID, rollbackEvent(betID, actor, 2))
		mustReject(t, res, err, SettlementRejectGenerationSequence)
	})
}

// TestSettlementDecision_BetVoided: settle/rollback against an already-void
// bet is rejected BET_VOIDED.
func TestSettlementDecision_BetVoided(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "market_cancelled"))

	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustReject(t, res, err, SettlementRejectBetVoided)

	res2, err2 := simulateSettlement(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	mustReject(t, res2, err2, SettlementRejectBetVoided)
}

// TestSettlementDecision_BetAlreadySettled: a second, different-generation
// settle against an already-settled bet is rejected (rollback required).
func TestSettlementDecision_BetAlreadySettled(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeLost, 0))
	mustReject(t, res, err, SettlementRejectBetAlreadySettled)
}

// TestSettlementDecision_UnknownBet_NotFound: an unknown bet id is 404-
// shaped, no posting, no tombstone, rejection audit with TargetID =
// requested id.
func TestSettlementDecision_UnknownBet_NotFound(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	actor := seedRiskManager(t, pool, f.tenantID)
	unknown := uuid.New()

	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(unknown, actor, 1, SettlementOutcomeWon, stdPayout))
	mustReject(t, res, err, SettlementRejectBetNotFound)

	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, unknown.String())
	if len(recs) != 1 || recs[0].Outcome != "failure" {
		t.Fatalf("expected exactly 1 failure audit record for the unknown id, got %+v", recs)
	}
}

// TestSettlementDecision_CrossTenantBet_NotFound: a bet id that belongs to
// a DIFFERENT tenant resolves as not-found under RLS - identical shape to
// a genuinely nonexistent id, never a cross-tenant leak.
func TestSettlementDecision_CrossTenantBet_NotFound(t *testing.T) {
	pool := testPool(t)
	_, _, otherBetID := newStdBet(t, pool)

	f := seedFixture(t, pool)
	actor := seedRiskManager(t, pool, f.tenantID)
	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(otherBetID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustReject(t, res, err, SettlementRejectBetNotFound)
}

// TestSettlementDecision_PayoutValidation covers V-1..V-4.
func TestSettlementDecision_PayoutValidation(t *testing.T) {
	pool := testPool(t)

	t.Run("V-1 asset mismatch", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		ev := settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout)
		ev.ClaimAssetCode = "USD"
		res, err := simulateSettlement(t, pool, f.tenantID, ev)
		mustReject(t, res, err, SettlementRejectAssetMismatch)
	})

	t.Run("V-2 lost claims a payout", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 1))
		mustReject(t, res, err, SettlementRejectPayoutInvalid)
	})

	t.Run("V-4 won anti-minting (claim above potential_return)", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout+1))
		mustReject(t, res, err, SettlementRejectPayoutInvalid)
	})

	t.Run("V-4 won claims less than potential_return", func(t *testing.T) {
		f, actor, betID := newStdBet(t, pool)
		res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout-1))
		mustReject(t, res, err, SettlementRejectPayoutInvalid)
	})
}

// TestSettlementValidation_FieldMatrix covers ADR 0088 §9.2's field matrix
// at the service level (validateSettlementEvent), returning ErrInvalidInput.
func TestSettlementValidation_FieldMatrix(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	cases := map[string]SettlementEvent{
		"settle: generation < 1":        {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 0, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: stdPayout, ClaimAssetCode: "EUR"},
		"settle: bad outcome":           {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 1, Outcome: "draw", ClaimPayoutAmount: stdPayout, ClaimAssetCode: "EUR"},
		"settle: negative payout":       {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 1, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: -1, ClaimAssetCode: "EUR"},
		"settle: missing asset_code":    {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 1, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: stdPayout},
		"settle: void_reason forbidden": {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 1, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: stdPayout, ClaimAssetCode: "EUR", VoidReason: "push"},
		"rollback: generation < 1":      {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventRollback, Generation: 0},
		"rollback: extra outcome":       {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventRollback, Generation: 1, Outcome: SettlementOutcomeWon},
		"void: generation forbidden":    {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventVoid, Generation: 1, VoidReason: "push"},
		"void: bad void_reason":         {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventVoid, VoidReason: "because"},
		"void: missing void_reason":     {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventVoid},
		"unknown event_type":            {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: "cashout"},
		"missing tenant id":             {BetID: betID, ActorStaffID: actor, EventType: SettlementEventVoid, VoidReason: "push"},
		"missing bet id":                {TenantID: f.tenantID, ActorStaffID: actor, EventType: SettlementEventVoid, VoidReason: "push"},
		"missing actor id":              {TenantID: f.tenantID, BetID: betID, EventType: SettlementEventVoid, VoidReason: "push"},

		// PROVIDER-REF-BOUND-1: the claimed asset code is provider-supplied.
		"settle: asset_code over bound":   {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 1, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: stdPayout, ClaimAssetCode: strings.Repeat("E", 256)},
		"settle: asset_code control char": {TenantID: f.tenantID, BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: 1, Outcome: SettlementOutcomeWon, ClaimPayoutAmount: stdPayout, ClaimAssetCode: "EU\nR"},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := callSimulate(t, pool, f.tenantID, ev)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// TestSettlementActor_NotFoundOrInactiveOrCrossTenant covers ADR 0088 §9.1's
// staff-active gate.
func TestSettlementActor_NotFoundOrInactiveOrCrossTenant(t *testing.T) {
	pool := testPool(t)

	t.Run("unknown staff id", func(t *testing.T) {
		f, _, betID := newStdBet(t, pool)
		ev := voidEvent(betID, uuid.New(), "push")
		ev.TenantID = f.tenantID
		_, err := callSimulate(t, pool, f.tenantID, ev)
		if !errors.Is(err, ErrSettlementActorNotActive) {
			t.Fatalf("expected ErrSettlementActorNotActive, got %v", err)
		}
	})

	t.Run("inactive staff", func(t *testing.T) {
		f, _, betID := newStdBet(t, pool)
		inactive := seedInactiveStaff(t, pool, f.tenantID)
		ev := voidEvent(betID, inactive, "push")
		ev.TenantID = f.tenantID
		_, err := callSimulate(t, pool, f.tenantID, ev)
		if !errors.Is(err, ErrSettlementActorNotActive) {
			t.Fatalf("expected ErrSettlementActorNotActive, got %v", err)
		}
	})

	t.Run("staff belongs to another tenant", func(t *testing.T) {
		f, _, betID := newStdBet(t, pool)
		otherTenantStaff := seedRiskManager(t, pool, seedOtherTenant(t, pool))
		ev := voidEvent(betID, otherTenantStaff, "push")
		ev.TenantID = f.tenantID
		_, err := callSimulate(t, pool, f.tenantID, ev)
		if !errors.Is(err, ErrSettlementActorNotActive) {
			t.Fatalf("expected ErrSettlementActorNotActive, got %v", err)
		}
	})
}
