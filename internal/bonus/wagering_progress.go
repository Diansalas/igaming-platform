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

// CreateWageringProgress inserts a new, append-only contribution row.
// Callers MUST write this in the same database transaction as the lock
// posting itself (ledger-accounting-model.md §6.6.4, HR-10) - this
// function does not enforce that, it is a caller discipline this
// package's own doc comment states rather than silently assumes.
// StakedBonusAmount must be read from the posted ledger entry, never
// supplied by an untrusted caller (§6.6.4's own stated rationale).
func CreateWageringProgress(ctx context.Context, tx pgx.Tx, p WageringProgress) (WageringProgress, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_wagering_progress (
			id, tenant_id, grant_id, player_account_id, offer_version_id, lock_ledger_transaction_id, correlation_id,
			asset_code, staked_bonus_amount, contribution_weight_bp, qualifying_scaled, rounding_rule_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+wageringProgressColumns,
		p.ID, p.TenantID, p.GrantID, p.PlayerAccountID, p.OfferVersionID, p.LockLedgerTransactionID, p.CorrelationID,
		p.AssetCode, bigIntToNumeric(p.StakedBonusAmount), p.ContributionWeightBP, bigIntToNumeric(p.QualifyingScaled), p.RoundingRuleID,
	)
	return scanWageringProgress(row)
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
