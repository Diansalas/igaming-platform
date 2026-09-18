package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// WageringProgress mirrors one bonus_wagering_progress row - the
// P_net/P_firm dual-measure's underlying contribution record
// (ledger-accounting-model.md §6.6.4, doc 10 §W2.8). One append-only row
// per (Grant, lock ledger transaction). This package stores the record
// only; the P_net/P_firm derivation itself (a read-only computation over
// this table plus ledger_transactions/ledger_entries) is Phase 3.
type WageringProgress struct {
	ID                      uuid.UUID
	TenantID                uuid.UUID
	GrantID                 uuid.UUID
	PlayerAccountID         uuid.UUID
	OfferVersionID          uuid.UUID
	LockLedgerTransactionID uuid.UUID
	CorrelationID           uuid.UUID
	AssetCode               string
	StakedBonusAmount       *big.Int // NUMERIC(38,0), > 0, read from the posted ledger entry
	ContributionWeightBP    int32    // basis points, 0-10000
	QualifyingScaled        *big.Int // NUMERIC(38,0), exact scaled integer, never rounded
	RoundingRuleID          *uuid.UUID
	CreatedAt               time.Time
}

const wageringProgressColumns = `
	id, tenant_id, grant_id, player_account_id, offer_version_id, lock_ledger_transaction_id, correlation_id,
	asset_code, staked_bonus_amount, contribution_weight_bp, qualifying_scaled, rounding_rule_id, created_at`

func scanWageringProgress(row rowScanner) (WageringProgress, error) {
	var (
		p          WageringProgress
		staked     pgtype.Numeric
		qualifying pgtype.Numeric
	)
	err := row.Scan(
		&p.ID, &p.TenantID, &p.GrantID, &p.PlayerAccountID, &p.OfferVersionID, &p.LockLedgerTransactionID, &p.CorrelationID,
		&p.AssetCode, &staked, &p.ContributionWeightBP, &qualifying, &p.RoundingRuleID, &p.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WageringProgress{}, ErrNotFound
	}
	if err != nil {
		return WageringProgress{}, fmt.Errorf("bonus: scan wagering progress: %w", err)
	}
	amt, err := numericToBigInt(staked)
	if err != nil {
		return WageringProgress{}, err
	}
	p.StakedBonusAmount = amt
	amt, err = numericToBigInt(qualifying)
	if err != nil {
		return WageringProgress{}, err
	}
	p.QualifyingScaled = amt
	return p, nil
}

// ErrWageringProgressAlreadyRecorded is returned by CreateWageringProgress
// when a row already exists for this (tenant_id, grant_id,
// lock_ledger_transaction_id) tuple (migration 0058's own UNIQUE
// constraint, HR-10) - the expected, graceful outcome for a redelivered/
// re-entrant caller, never a program error. In the one reachable
// production call path today (postBet's own pre-existing idempotency
// short-circuit, §7.18.3.5) this is never actually hit - it exists as
// defense-in-depth hardening for any future second caller that lacks an
// equivalent upstream guard (ledger-accounting-model.md §7.18.3.5,
// "bonus-engine should consider mirroring [AttributeGrantLedgerTransaction's]
// same pattern... as defense-in-depth hardening, not because the Wave-3
// call site needs it to be correct").
var ErrWageringProgressAlreadyRecorded = errors.New("bonus: a wagering progress row already exists for this (grant, lock transaction) pair")

// CreateWageringProgress inserts a new, append-only contribution row.
// Callers MUST write this in the same database transaction as the lock
// posting itself (ledger-accounting-model.md §6.6.4, HR-10) - this
// function does not enforce that, it is a caller discipline this
// package's own doc comment states rather than silently assumes.
// StakedBonusAmount must be read from the posted ledger entry, never
// supplied by an untrusted caller (§6.6.4's own stated rationale; §7.18.3.2's
// generalization - the funding account for a cash-funded wagering-
// contribution Grant is player_cash, not player_bonus/player_locked_bonus,
// but the "read from the ledger, never the caller" invariant is identical).
//
// ON CONFLICT (tenant_id, grant_id, lock_ledger_transaction_id) DO NOTHING
// (§7.18.3.5's named hardening item): a second insert attempt for the
// same tuple returns ErrWageringProgressAlreadyRecorded rather than a raw
// unique-violation error that would otherwise poison the caller's
// transaction - mirroring AttributeGrantLedgerTransaction's identical
// ON CONFLICT ... DO NOTHING / sentinel-error shape (attribution.go).
func CreateWageringProgress(ctx context.Context, tx pgx.Tx, p WageringProgress) (WageringProgress, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_wagering_progress (
			id, tenant_id, grant_id, player_account_id, offer_version_id, lock_ledger_transaction_id, correlation_id,
			asset_code, staked_bonus_amount, contribution_weight_bp, qualifying_scaled, rounding_rule_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant_id, grant_id, lock_ledger_transaction_id) DO NOTHING
		RETURNING `+wageringProgressColumns,
		p.ID, p.TenantID, p.GrantID, p.PlayerAccountID, p.OfferVersionID, p.LockLedgerTransactionID, p.CorrelationID,
		p.AssetCode, bigIntToNumeric(p.StakedBonusAmount), p.ContributionWeightBP, bigIntToNumeric(p.QualifyingScaled), p.RoundingRuleID,
	)
	wp, err := scanWageringProgress(row)
	if errors.Is(err, ErrNotFound) {
		// ErrNotFound means pgx.ErrNoRows here (scanWageringProgress's own
		// mapping) - i.e. the ON CONFLICT DO NOTHING fired, not that the
		// row genuinely does not exist (this was an INSERT, not a lookup).
		return WageringProgress{}, ErrWageringProgressAlreadyRecorded
	}
	return wp, err
}

// CreateWageringProgressIdempotent is CreateWageringProgress but treats
// ErrWageringProgressAlreadyRecorded as success, returning the
// already-recorded row instead - the shape a caller with no upstream
// idempotency guard of its own actually wants (mirroring
// AttributeGrantLedgerTransactionIdempotent's identical role).
func CreateWageringProgressIdempotent(ctx context.Context, tx pgx.Tx, p WageringProgress) (WageringProgress, bool, error) {
	wp, err := CreateWageringProgress(ctx, tx, p)
	if err == nil {
		return wp, true, nil
	}
	if !errors.Is(err, ErrWageringProgressAlreadyRecorded) {
		return WageringProgress{}, false, err
	}
	row := tx.QueryRow(ctx, `SELECT `+wageringProgressColumns+` FROM bonus_wagering_progress WHERE tenant_id = $1 AND grant_id = $2 AND lock_ledger_transaction_id = $3`,
		p.TenantID, p.GrantID, p.LockLedgerTransactionID,
	)
	existing, err := scanWageringProgress(row)
	if err != nil {
		return WageringProgress{}, false, err
	}
	return existing, false, nil
}

// ListWageringProgressByGrant returns every contribution row for a
// Grant, oldest first - the P_net/P_firm derivation's own read pattern.
func ListWageringProgressByGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) ([]WageringProgress, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+wageringProgressColumns+` FROM bonus_wagering_progress WHERE tenant_id = $1 AND grant_id = $2 ORDER BY created_at ASC`,
		tenantID, grantID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list wagering progress: %w", err)
	}
	defer rows.Close()
	var out []WageringProgress
	for rows.Next() {
		p, err := scanWageringProgress(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
