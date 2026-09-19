package jurisdiction

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// This file implements item B-6 (canonical-model §4.2, §11.1):
// jurisdiction_resolution_active, a resolver-owned, tenant-scoped fact
// recording whether jurisdiction resolution is genuinely active for a
// (tenant, operation_class) pair. It supplies the fact RISK §2.4b's
// future risk.CreateRule precondition (R-2b) reads via IsActive below -
// internal/risk defines no table, flag, or role of its own for this, and
// this package does not implement R-2b's precondition itself (that is
// risk's own later implementation phase, per the canonical model's
// explicit ownership split).

// ActiveRecord is one jurisdiction_resolution_active row.
type ActiveRecord struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	OperationClass OperationClass
	Active         bool
}

// SetResolutionActiveParams is SetResolutionActive's input.
type SetResolutionActiveParams struct {
	TenantID       uuid.UUID
	OperationClass OperationClass
	Active         bool
	// ActorType/ActorID identify who is flipping the fact - staff or
	// system only (migration 0071's own CHECK); a resolution-active
	// toggle is a configuration change, never a player action.
	ActorType ActorType
	ActorID   uuid.UUID
	IPAddress string
	UserAgent string
	RequestID string
}

// SetResolutionActive creates or updates the (tenant, operation_class)
// row and writes an audit_log entry ("jurisdiction_resolution_active.
// changed") in the SAME transaction (canonical-model §5.1's fourth
// audited event class), per CLAUDE.md's "every mutating administrative
// action writes an audit record" rule. tx must be a tenant-scoped,
// non-player-scoped transaction (db.Pool.WithTenant) - migration 0071's
// RLS policy rejects anything else.
func SetResolutionActive(ctx context.Context, tx pgx.Tx, p SetResolutionActiveParams) (ActiveRecord, error) {
	if p.TenantID == uuid.Nil {
		return ActiveRecord{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !validOperationClass(p.OperationClass) {
		return ActiveRecord{}, fmt.Errorf("%w: unknown operation_class %q", ErrInvalidInput, p.OperationClass)
	}
	if p.ActorType != ActorStaff && p.ActorType != ActorSystem {
		return ActiveRecord{}, fmt.Errorf("%w: actor_type must be staff or system", ErrInvalidInput)
	}
	if p.ActorID == uuid.Nil {
		return ActiveRecord{}, fmt.Errorf("%w: actor_id is required", ErrInvalidInput)
	}

	var before *bool
	if err := tx.QueryRow(ctx,
		`SELECT active FROM jurisdiction_resolution_active WHERE tenant_id = $1 AND operation_class = $2`,
		p.TenantID, string(p.OperationClass)).Scan(&before); err != nil && err != pgx.ErrNoRows {
		return ActiveRecord{}, fmt.Errorf("jurisdiction: read resolution-active fact: %w", err)
	}

	var rec ActiveRecord
	var oc string
	err := tx.QueryRow(ctx, `
		INSERT INTO jurisdiction_resolution_active (tenant_id, operation_class, active, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, operation_class) DO UPDATE SET active = EXCLUDED.active
		RETURNING id, tenant_id, operation_class, active`,
		p.TenantID, string(p.OperationClass), p.Active, string(p.ActorType), p.ActorID,
	).Scan(&rec.ID, &rec.TenantID, &oc, &rec.Active)
	if err != nil {
		return ActiveRecord{}, fmt.Errorf("jurisdiction: upsert resolution-active fact: %w", err)
	}
	rec.OperationClass = OperationClass(oc)

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: audit.ActorType(p.ActorType), ActorID: p.ActorID,
		Action: "jurisdiction_resolution_active.changed", TargetType: "jurisdiction_resolution_active",
		TargetID: rec.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: p.IPAddress, UserAgent: p.UserAgent, RequestID: p.RequestID,
		Metadata: map[string]any{
			"operation_class": string(p.OperationClass),
			"before_active":   before,
			"after_active":    p.Active,
		},
	}); err != nil {
		return ActiveRecord{}, fmt.Errorf("jurisdiction: audit resolution-active change: %w", err)
	}
	return rec, nil
}

// IsActive is the narrow, READ-ONLY accessor RISK §2.4b's future
// risk.CreateRule precondition (R-2b) is meant to consume
// (canonical-model §4.2: "Risk consumes it read-only through one narrow
// accessor exposed by the resolver package"). It answers false for any
// (tenant, operation_class) pair with no row at all - fail-closed, never
// "not configured means unrestricted".
func IsActive(ctx context.Context, q ReadOnlyQuerier, tenantID uuid.UUID, operationClass OperationClass) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	var active bool
	err := q.QueryRow(ctx,
		`SELECT active FROM jurisdiction_resolution_active WHERE tenant_id = $1 AND operation_class = $2`,
		tenantID, string(operationClass)).Scan(&active)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("jurisdiction: read resolution-active fact: %w", err)
	}
	return active, nil
}

// ListResolutionActive lists every (operation_class, active) fact for a
// tenant - used by an admin read surface / diagnostics, never by an
// enforcement path (enforcement uses IsActive for exactly one pair).
func ListResolutionActive(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]ActiveRecord, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, operation_class, active FROM jurisdiction_resolution_active WHERE tenant_id = $1 ORDER BY operation_class`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list resolution-active facts: %w", err)
	}
	defer rows.Close()

	var out []ActiveRecord
	for rows.Next() {
		var rec ActiveRecord
		var oc string
		if err := rows.Scan(&rec.ID, &rec.TenantID, &oc, &rec.Active); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan resolution-active fact: %w", err)
		}
		rec.OperationClass = OperationClass(oc)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jurisdiction: iterate resolution-active facts: %w", err)
	}
	return out, nil
}
