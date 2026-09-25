package ledger

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Replay payload comparison (Stage 10 F-7 remediation, ADR 0020
// amendment 2026-09-25, docs/governance/stage-10-f7-ledger-replay-audit.md).
//
// When Post's ledger_transactions INSERT conflicts on
// (tenant_id, idempotency_key), the stored transaction is compared with
// the request before anything is reported as an idempotent replay. The
// comparison is over the CANONICAL payload: every field that is persisted
// on the ledger_transactions row or its ledger_entries, as Post itself
// would have written it. A difference in any of them means the caller is
// not retrying the same financial fact, and Post returns
// ErrIdempotencyPayloadMismatch instead of the original's id.
//
// Field classes named in the error (never values - no amounts, account
// ids or references reach the error string, so no HTTP mapper can leak
// them):
const (
	replayFieldEntries      = "entries"
	replayFieldReversalLink = "reversal_link"
	replayFieldProviderRef  = "provider_ref"
	replayFieldReasonCode   = "reason_code"
	replayFieldCausation    = "causation"
	replayFieldCorrelation  = "correlation"
)

// storedTransaction is the persisted header of the transaction that
// already owns an idempotency key.
type storedTransaction struct {
	ID                    uuid.UUID
	TransactionType       TransactionType
	ProviderID            *string
	ProviderTxID          *string
	CorrelationID         uuid.UUID
	CausationID           *uuid.UUID
	ReversesTransactionID *uuid.UUID
	ReasonCode            *string
}

func lookupByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, key string) (storedTransaction, error) {
	var s storedTransaction
	err := tx.QueryRow(ctx,
		`SELECT id, transaction_type, provider_id, provider_tx_id, correlation_id, causation_id,
		        reverses_transaction_id, reason_code
		   FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenantID, key,
	).Scan(&s.ID, &s.TransactionType, &s.ProviderID, &s.ProviderTxID, &s.CorrelationID, &s.CausationID,
		&s.ReversesTransactionID, &s.ReasonCode)
	return s, err
}

// replayPayloadDifferences returns the field classes in which the stored
// transaction differs from in, whose final entry set (caller entries plus
// Rule B2 mirror/recognition legs, exactly as prepareEntries built them)
// is entriesToPost. An empty result means the request is an exact replay.
//
// Entries are compared as a multiset of (ledger_account_id, direction,
// amount): order-insensitive (Post's own insertion order is an
// auditability property, not part of the fact), and never netted (a
// Dr A 5 / Cr A 5 pair is not the same fact as no entry at all). Asset is
// implied by the account. Amounts are compared as decimal strings, so a
// stored NUMERIC(38,0) value outside int64 can never compare equal by
// truncation.
//
// Rule B2 legs are safe to compare: applyBonusMirror reads no balance and
// is a pure function of the caller's entries, the resolved account types
// and BonusCost.Funding, so a legitimate retry with identical caller
// entries and BonusCost regenerates byte-identical legs, while a retry
// with a different Funding lands on a different recognition account and
// is (correctly) a mismatch. BonusCost.ProviderID is not persisted on the
// ledger and therefore cannot be compared here.
//
// Tombstone correlation exemption: for TxTombstone the correlation_id is
// NOT compared. A tombstone moves no value and has no entries; its whole
// meaning is "this key, and this (provider_id, provider_tx_id), are
// occupied", and both are still compared. The existing tombstone writers
// (casino postRollbackTombstone and payments postDepositReversalTombstone)
// mint CorrelationID with uuid.New() on every call, and rows already
// written that way exist, so a legitimate concurrent (casino) or
// sequential (payments) re-tombstone would otherwise be rejected against
// a historical row that no code change can make deterministic.
func replayPayloadDifferences(ctx context.Context, tx pgx.Tx, stored storedTransaction, in TransactionInput, entriesToPost []EntryInput) ([]string, error) {
	var diffs []string

	storedEntries, err := loadStoredEntryMultiset(ctx, tx, stored.ID)
	if err != nil {
		return nil, err
	}
	if !equalMultiset(storedEntries, requestedEntryMultiset(entriesToPost)) {
		diffs = append(diffs, replayFieldEntries)
	}
	if !equalUUIDPtr(stored.ReversesTransactionID, in.ReversesTransactionID) {
		diffs = append(diffs, replayFieldReversalLink)
	}
	if !equalStringPtr(stored.ProviderID, in.ProviderID) || !equalStringPtr(stored.ProviderTxID, in.ProviderTxID) {
		diffs = append(diffs, replayFieldProviderRef)
	}
	if !equalStringPtr(stored.ReasonCode, in.ReasonCode) {
		diffs = append(diffs, replayFieldReasonCode)
	}
	if !equalUUIDPtr(stored.CausationID, in.CausationID) {
		diffs = append(diffs, replayFieldCausation)
	}
	if in.TransactionType != TxTombstone && stored.CorrelationID != in.CorrelationID {
		diffs = append(diffs, replayFieldCorrelation)
	}
	return diffs, nil
}

func entryMultisetKey(accountID uuid.UUID, direction Direction, amount string) string {
	return accountID.String() + "|" + string(direction) + "|" + amount
}

func requestedEntryMultiset(entries []EntryInput) map[string]int {
	m := make(map[string]int, len(entries))
	for _, e := range entries {
		m[entryMultisetKey(e.LedgerAccountID, e.Direction, strconv.FormatInt(e.Amount, 10))]++
	}
	return m
}

func loadStoredEntryMultiset(ctx context.Context, tx pgx.Tx, transactionID uuid.UUID) (map[string]int, error) {
	rows, err := tx.Query(ctx,
		`SELECT ledger_account_id, direction, amount::text FROM ledger_entries WHERE ledger_transaction_id = $1`,
		transactionID,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: load existing entries for replay comparison: %w", err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var accountID uuid.UUID
		var direction Direction
		var amount string
		if err := rows.Scan(&accountID, &direction, &amount); err != nil {
			return nil, fmt.Errorf("ledger: scan existing entry for replay comparison: %w", err)
		}
		m[entryMultisetKey(accountID, direction, amount)]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: read existing entries for replay comparison: %w", err)
	}
	return m, nil
}

func equalMultiset(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	return true
}

func equalUUIDPtr(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// formatReplayDiffs renders the differing field classes deterministically.
func formatReplayDiffs(diffs []string) string {
	sorted := append([]string(nil), diffs...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}
