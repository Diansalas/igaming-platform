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

// HeldDispositionStatus is bonus_held_dispositions.status's closed set -
// EXACTLY ledger-accounting-model.md §7.7.2.5's frozen contract. No
// third economic outcome, no fourth status (§7.7.2.5's own binding
// statement).
type HeldDispositionStatus string

const (
	HeldDispositionHeld                HeldDispositionStatus = "held"
	HeldDispositionResolvedReforfeit   HeldDispositionStatus = "resolved_reforfeit"
	HeldDispositionResolvedRouteToCash HeldDispositionStatus = "resolved_route_to_cash"
	HeldDispositionVoidedByRollback    HeldDispositionStatus = "voided_by_rollback"
)

// HeldDisposition mirrors one bonus_held_dispositions row - the
// HeldDispositionRecord. THIS TYPE'S SHAPE IS FROZEN: it mirrors
// ledger-accounting-model.md §7.7.2.5 field-for-field. Do not add or
// rename a field here without updating that contract first.
type HeldDisposition struct {
	ID                            uuid.UUID
	TenantID                      uuid.UUID
	BrandID                       uuid.UUID
	WalletID                      uuid.UUID
	PlayerAccountID               uuid.UUID
	AssetCode                     string
	GrantID                       uuid.UUID
	CorrelationID                 uuid.UUID // audit trail only, NEVER the idempotency key
	SettlementLedgerTransactionID uuid.UUID // THE idempotency key
	PayoutAmount                  *big.Int  // NUMERIC(38,0), >= 0 (W)
	ReleasedLockAmount            *big.Int  // NUMERIC(38,0), >= 0, default 0 (X)
	Status                        HeldDispositionStatus
	CreatedAt                     time.Time
	ResolvedAt                    *time.Time
	ResolvedByActorID             *uuid.UUID
	ResolutionReasonCode          *string
	ResolutionLedgerTransactionID *uuid.UUID
}

const heldDispositionColumns = `
	id, tenant_id, brand_id, wallet_id, player_account_id, asset_code, grant_id, correlation_id, settlement_ledger_transaction_id,
	payout_amount, released_lock_amount, status, created_at, resolved_at, resolved_by_actor_id, resolution_reason_code, resolution_ledger_transaction_id`

func scanHeldDisposition(row rowScanner) (HeldDisposition, error) {
	var (
		d        HeldDisposition
		status   string
		payout   pgtype.Numeric
		released pgtype.Numeric
	)
	err := row.Scan(
		&d.ID, &d.TenantID, &d.BrandID, &d.WalletID, &d.PlayerAccountID, &d.AssetCode, &d.GrantID, &d.CorrelationID, &d.SettlementLedgerTransactionID,
		&payout, &released, &status, &d.CreatedAt, &d.ResolvedAt, &d.ResolvedByActorID, &d.ResolutionReasonCode, &d.ResolutionLedgerTransactionID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return HeldDisposition{}, ErrNotFound
	}
	if err != nil {
		return HeldDisposition{}, fmt.Errorf("bonus: scan held disposition: %w", err)
	}
	d.Status = HeldDispositionStatus(status)
	amt, err := numericToBigInt(payout)
	if err != nil {
		return HeldDisposition{}, err
	}
	d.PayoutAmount = amt
	amt, err = numericToBigInt(released)
	if err != nil {
		return HeldDisposition{}, err
	}
	d.ReleasedLockAmount = amt
	return d, nil
}

// CreateHeldDisposition inserts a new bonus_held_dispositions row in
// status 'held'. Per doc 10 N1.8.1/security-architecture.md
// REQ-SEP-BONUS-4, creation is TECHNICAL ("recording-and-parking a
// fact") and is NOT SEP-1/bonus_held_disposition:resolve-gated - only
// RESOLUTION is. Callers MUST write this in the same database
// transaction as the hold-capture posting itself
// (ledger-accounting-model.md §7.7.2.2), keyed by
// SettlementLedgerTransactionID as the idempotency key (§7.7.2.6, LF-22)
// - the UNIQUE (tenant_id, settlement_ledger_transaction_id) constraint
// is the actual enforcement, this function performs no
// check-then-insert.
func CreateHeldDisposition(ctx context.Context, tx pgx.Tx, d HeldDisposition) (HeldDisposition, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.Status == "" {
		d.Status = HeldDispositionHeld
	}
	if d.ReleasedLockAmount == nil {
		d.ReleasedLockAmount = big.NewInt(0)
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_held_dispositions (
			id, tenant_id, brand_id, wallet_id, player_account_id, asset_code, grant_id, correlation_id, settlement_ledger_transaction_id,
			payout_amount, released_lock_amount, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+heldDispositionColumns,
		d.ID, d.TenantID, d.BrandID, d.WalletID, d.PlayerAccountID, d.AssetCode, d.GrantID, d.CorrelationID, d.SettlementLedgerTransactionID,
		bigIntToNumeric(d.PayoutAmount), bigIntToNumeric(d.ReleasedLockAmount), string(d.Status),
	)
	return scanHeldDisposition(row)
}

// GetHeldDispositionByID looks up a HeldDisposition by id.
func GetHeldDispositionByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (HeldDisposition, error) {
	row := tx.QueryRow(ctx, `SELECT `+heldDispositionColumns+` FROM bonus_held_dispositions WHERE id = $1`, id)
	return scanHeldDisposition(row)
}

// GetHeldDispositionBySettlementTransaction resolves the idempotency key
// (§7.7.2.6): the settlement's own ledger transaction id, never
// correlation_id, never grant_id.
func GetHeldDispositionBySettlementTransaction(ctx context.Context, tx pgx.Tx, tenantID, settlementLedgerTransactionID uuid.UUID) (HeldDisposition, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+heldDispositionColumns+` FROM bonus_held_dispositions WHERE tenant_id = $1 AND settlement_ledger_transaction_id = $2`,
		tenantID, settlementLedgerTransactionID,
	)
	return scanHeldDisposition(row)
}

// LockHeldDispositionForUpdate reads a HeldDisposition AND takes a row
// lock (SELECT ... FOR UPDATE) - the HR-25 "fourth participant" row lock
// ledger-accounting-model.md §7.7.2.9 specifies for the resolution path.
// This package does not itself compose the full HR-25 lock order (the
// (tenant_id, grant_id) advisory lock acquired first, doc10 §9) - that
// composition is Phase 3's job.
func LockHeldDispositionForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (HeldDisposition, error) {
	row := tx.QueryRow(ctx, `SELECT `+heldDispositionColumns+` FROM bonus_held_dispositions WHERE id = $1 FOR UPDATE`, id)
	return scanHeldDisposition(row)
}

// ListHeldDispositionsByGrant returns every HeldDisposition attributed
// to a Grant, oldest first.
func ListHeldDispositionsByGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) ([]HeldDisposition, error) {
	return queryHeldDispositions(ctx, tx, `SELECT `+heldDispositionColumns+` FROM bonus_held_dispositions WHERE tenant_id = $1 AND grant_id = $2 ORDER BY created_at ASC`, tenantID, grantID)
}

// ListHeldDispositionsByStatus returns every HeldDisposition in a given
// status - "held-disposition lookup by status for the aging/
// reconciliation sweep" (this dispatch's own named requirement; §7.7.2.8).
func ListHeldDispositionsByStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, status HeldDispositionStatus) ([]HeldDisposition, error) {
	return queryHeldDispositions(ctx, tx, `SELECT `+heldDispositionColumns+` FROM bonus_held_dispositions WHERE tenant_id = $1 AND status = $2 ORDER BY created_at ASC`, tenantID, string(status))
}

func queryHeldDispositions(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]HeldDisposition, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("bonus: query held dispositions: %w", err)
	}
	defer rows.Close()
	var out []HeldDisposition
	for rows.Next() {
		d, err := scanHeldDisposition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrHeldDispositionStateConflict is returned by ResolveHeldDisposition
// when the row is not (or is no longer) 'held' at update time.
var ErrHeldDispositionStateConflict = errors.New("bonus: held disposition is not in 'held' status")

// ResolveHeldDisposition applies a G-2 disposition answer
// (ACTION_REFORFEIT -> resolved_reforfeit, ACTION_ROUTE_TO_CASH ->
// resolved_route_to_cash) or the technical rollback-void transition
// (voided_by_rollback), via the exact compare-and-swap
// ledger-accounting-model.md §7.7.2.7 specifies:
// "UPDATE ... SET status = ... WHERE id = ? AND status = 'held'" - one
// atomic statement, zero rows affected is the loud failure signal, never
// check-then-update. This function performs NO SEP-1/four-eyes/
// permission enforcement (REQ-SEP-BONUS-4, bonus_held_disposition:resolve)
// and no posting of the resolution's own ledger transaction - callers
// (Phase 3) are responsible for both, and for holding HR-25's lock order
// (the (tenant_id, grant_id) advisory lock, then this row's own FOR
// UPDATE lock via LockHeldDispositionForUpdate) before calling this.
func ResolveHeldDisposition(
	ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID,
	newStatus HeldDispositionStatus, resolvedByActorID *uuid.UUID, reasonCode *string, resolutionLedgerTransactionID uuid.UUID, at time.Time,
) (HeldDisposition, error) {
	if newStatus == HeldDispositionHeld {
		return HeldDisposition{}, fmt.Errorf("bonus: resolution target status must not be 'held'")
	}
	row := tx.QueryRow(ctx, `
		UPDATE bonus_held_dispositions
		SET status = $3, resolved_at = $4, resolved_by_actor_id = $5, resolution_reason_code = $6, resolution_ledger_transaction_id = $7
		WHERE tenant_id = $1 AND id = $2 AND status = $8
		RETURNING `+heldDispositionColumns,
		tenantID, id, string(newStatus), at, resolvedByActorID, reasonCode, resolutionLedgerTransactionID, string(HeldDispositionHeld),
	)
	d, err := scanHeldDisposition(row)
	if errors.Is(err, ErrNotFound) {
		return HeldDisposition{}, ErrHeldDispositionStateConflict
	}
	return d, err
}
