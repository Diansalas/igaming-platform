package sportsbook

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SettlementHistoryEntry is SettlementRecord plus its own CreatedAt -
// exactly what ADR 0088 §3.4's read surfaces need (settled_at derives from
// a history row's created_at) that SettlementRecord itself does not carry
// (settlement.go's own struct and scan helpers are unchanged by this file -
// see internal/httpserver's read-surface handlers for how CreatedAt is
// used to derive settled_at/lifecycle ordering).
type SettlementHistoryEntry struct {
	SettlementRecord
	CreatedAt time.Time
}

// ListSettlementRecordsForBets batch-loads the settlement history for every
// bet id in betIDs, in insertion order per bet, with ONE query - the read
// surfaces this feeds (ADR 0088 §3.4: GET /v1/me/sportsbook/bets and
// GET /v1/admin/sportsbook/bets) render one page of bets at a time, and
// looking up each bet's history with its own query would be an N+1 query
// pattern. Returns a map keyed by bet id; a bet with no history rows
// (still open, never touched by the settlement route) is simply absent
// from the map, not present with an empty slice - callers must treat a
// missing key as "no history" rather than an error.
//
// Runs within the caller's own RLS scope (staff tenant scope or a player's
// own scope) exactly like ListSettlementRecords - no new authorization
// boundary is introduced here.
func ListSettlementRecordsForBets(ctx context.Context, tx pgx.Tx, betIDs []uuid.UUID) (map[uuid.UUID][]SettlementHistoryEntry, error) {
	out := make(map[uuid.UUID][]SettlementHistoryEntry, len(betIDs))
	if len(betIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT `+settlementRecordColumns+`, created_at FROM sportsbook_bet_settlements
		  WHERE bet_id = ANY($1) ORDER BY bet_id, created_at, generation NULLS LAST, id`,
		betIDs)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: list settlement records for bets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanSettlementHistoryEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("sportsbook: scan settlement record: %w", err)
		}
		out[e.BetID] = append(out[e.BetID], e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sportsbook: read settlement records for bets: %w", err)
	}
	return out, nil
}

func scanSettlementHistoryEntry(row pgx.Row) (SettlementHistoryEntry, error) {
	var e SettlementHistoryEntry
	var generation *int32
	err := row.Scan(&e.ID, &e.TenantID, &e.BetID, &e.EventKind, &generation, &e.Outcome, &e.PayoutAmount, &e.AssetCode,
		&e.VoidReason, &e.ReversesSettlementID, &e.CausationRecordID, &e.LedgerTransactionID, &e.ActorStaffAccountID, &e.RequestID,
		&e.CreatedAt)
	if err != nil {
		return SettlementHistoryEntry{}, err
	}
	if generation != nil {
		g := int(*generation)
		e.Generation = &g
	}
	return e, nil
}

// CurrentSettlement returns the latest un-reversed settlement entry among
// entries (nil if none) - the same "current settlement" concept
// settlementState.current uses internally, re-derived here from a plain
// slice for the read surface (which never runs inside a settlement
// operation's own settlementState).
func CurrentSettlement(entries []SettlementHistoryEntry) *SettlementHistoryEntry {
	reversed := make(map[uuid.UUID]bool)
	for i := range entries {
		if entries[i].EventKind == settlementHistoryKindRollback && entries[i].ReversesSettlementID != nil {
			reversed[*entries[i].ReversesSettlementID] = true
		}
	}
	var current *SettlementHistoryEntry
	for i := range entries {
		if entries[i].EventKind == settlementHistoryKindSettlement && !reversed[entries[i].ID] {
			current = &entries[i]
		}
	}
	return current
}

// VoidEntry returns the (at most one) void row among entries, if any.
func VoidEntry(entries []SettlementHistoryEntry) *SettlementHistoryEntry {
	for i := range entries {
		if entries[i].EventKind == settlementHistoryKindVoid {
			return &entries[i]
		}
	}
	return nil
}
