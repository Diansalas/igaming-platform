//go:build integration

// Code review B-3(a) (docs/governance/stage-10-w1-code-review.md, finding
// 3): the rejection audit (ADR 0088 §4.4) is DESIGNED to commit
// sportsbook_bet.settlement_rejected with no ledger write - but nothing
// pinned that a real rejection actually commits exactly one such row, with
// the required metadata, and posts nothing. A search of every _test.go for
// "settlement_rejected"/"RecordSettlementRejection" found no matches
// before this file. This is the service-level half (rejectSettlement,
// via a normal pool.WithTenant call that COMMITS); the HTTP/§4.7 half
// (a SEPARATE-transaction audit after an ErrSettlementIntegrity abort) is
// internal/httpserver/sportsbook_settlement_integrity_audit_integration_test.go.
package sportsbook

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// assertExactlyOneRejectionAudit is the shared shape every case below
// checks: exactly one sportsbook_bet.settlement_rejected row, outcome
// failure, and the §4.4 metadata set (rejection_code, event_type,
// generation, bet_status).
func assertExactlyOneRejectionAudit(t *testing.T, pool *db.Pool, f sbFixture, targetID string, wantCode string, wantEventType string, wantGeneration int, wantBetStatus string) {
	t.Helper()
	recs := auditRecordsFor(t, pool, f.tenantID, settlementAuditTargetType, targetID)
	var rejections []auditRow
	for _, r := range recs {
		if r.Action == settlementAuditActionRejected {
			rejections = append(rejections, r)
		}
	}
	if len(rejections) != 1 {
		t.Fatalf("expected exactly 1 %s audit row for target %s, got %d: %+v", settlementAuditActionRejected, targetID, len(rejections), rejections)
	}
	rec := rejections[0]
	if rec.Outcome != "failure" {
		t.Fatalf("rejection audit outcome = %q, want failure", rec.Outcome)
	}
	if rec.Metadata["rejection_code"] != wantCode {
		t.Fatalf("rejection audit metadata.rejection_code = %v, want %q", rec.Metadata["rejection_code"], wantCode)
	}
	if rec.Metadata["event_type"] != wantEventType {
		t.Fatalf("rejection audit metadata.event_type = %v, want %q", rec.Metadata["event_type"], wantEventType)
	}
	if wantGeneration != 0 {
		gotGen, ok := rec.Metadata["generation"].(float64) // JSON numbers decode as float64
		if !ok || int(gotGen) != wantGeneration {
			t.Fatalf("rejection audit metadata.generation = %v, want %d", rec.Metadata["generation"], wantGeneration)
		}
	}
	if rec.Metadata["bet_status"] != wantBetStatus {
		t.Fatalf("rejection audit metadata.bet_status = %v, want %q", rec.Metadata["bet_status"], wantBetStatus)
	}
}

// TestSettlementRejectionAudit_PayloadMismatch_CommitsOneAuditRowNoPosting
// (B-3(a), PAYLOAD_MISMATCH): a same-generation, different-claim redelivery
// commits exactly one rejection audit row and posts/inserts nothing.
func TestSettlementRejectionAudit_PayloadMismatch_CommitsOneAuditRowNoPosting(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	histBefore := len(settlementHistory(t, pool, f.tenantID, betID))
	ledgerBefore := countLedgerTransactions(t, pool, f)

	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	mustReject(t, res, err, SettlementRejectPayloadMismatch)

	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != histBefore {
		t.Fatalf("expected 0 new history rows, history grew %d -> %d", histBefore, got)
	}
	if got := countLedgerTransactions(t, pool, f); got != ledgerBefore {
		t.Fatalf("expected 0 new ledger rows, count grew %d -> %d", ledgerBefore, got)
	}
	assertExactlyOneRejectionAudit(t, pool, f, betID.String(), SettlementRejectPayloadMismatch,
		string(SettlementEventSettle), 1, string(BetStatusSettledWon))
}

// TestSettlementRejectionAudit_GenerationOutOfSequence_CommitsOneAuditRowNoPosting
// (B-3(a), GENERATION_OUT_OF_SEQUENCE): a generation gap on an open bet
// commits exactly one rejection audit row and posts nothing.
func TestSettlementRejectionAudit_GenerationOutOfSequence_CommitsOneAuditRowNoPosting(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	histBefore := len(settlementHistory(t, pool, f.tenantID, betID))
	ledgerBefore := countLedgerTransactions(t, pool, f)

	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
	mustReject(t, res, err, SettlementRejectGenerationSequence)

	if got := len(settlementHistory(t, pool, f.tenantID, betID)); got != histBefore {
		t.Fatalf("expected 0 new history rows, history grew %d -> %d", histBefore, got)
	}
	if got := countLedgerTransactions(t, pool, f); got != ledgerBefore {
		t.Fatalf("expected 0 new ledger rows, count grew %d -> %d", ledgerBefore, got)
	}
	assertExactlyOneRejectionAudit(t, pool, f, betID.String(), SettlementRejectGenerationSequence,
		string(SettlementEventSettle), 2, string(BetStatusOpen))
}

// TestSettlementRejectionAudit_NotFound_CommitsOneAuditRowNoPosting
// (B-3(a), NOT_FOUND): an unknown bet id commits exactly one rejection
// audit row (keyed by the REQUESTED id, since no bet was resolved) with an
// empty bet_status, and posts nothing.
func TestSettlementRejectionAudit_NotFound_CommitsOneAuditRowNoPosting(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	actor := seedRiskManager(t, pool, f.tenantID)
	unknown := uuid.New()

	res, err := simulateSettlement(t, pool, f.tenantID, settleEvent(unknown, actor, 1, SettlementOutcomeWon, stdPayout))
	mustReject(t, res, err, SettlementRejectBetNotFound)

	// A history row's bet_id FOREIGN KEY REFERENCES sportsbook_bets, so no
	// history row for an unknown bet id could ever exist - the interesting
	// assertion is the audit row alone.
	assertExactlyOneRejectionAudit(t, pool, f, unknown.String(), SettlementRejectBetNotFound,
		string(SettlementEventSettle), 1, "")
}
