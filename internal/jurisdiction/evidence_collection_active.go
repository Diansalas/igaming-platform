package jurisdiction

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// This file implements Stage 4I Phase B's activation boundary:
// jurisdiction_evidence_collection_active, a per-tenant, per-evidence-type
// switch (default OFF) that must be explicitly turned on before any
// declared-residence write or verified-residence determination is
// accepted. It mirrors resolution_active.go's own shape almost exactly,
// substituting EvidenceType (evidence_type) for OperationClass
// (operation_class) - see that file's own doc comment for the general
// pattern this follows. Unlike resolution_active.go's fact (an
// engineering precondition RISK consumes), this fact gates collection of
// privacy-sensitive personal data under a lawful-basis judgment
// (docs/decisions/0042-human-decision-response.md HDR-J-3e), which is why
// it is a SEPARATE table/permission rather than reusing operation_class's
// vocabulary or jurisdiction_resolution_active's own rows.

// EvidenceType identifies which kind of jurisdiction-relevant player
// evidence a jurisdiction_evidence_collection_active row gates.
type EvidenceType string

const (
	EvidenceDeclaredResidence EvidenceType = "declared_residence"
	EvidenceVerifiedResidence EvidenceType = "verified_residence"
	// EvidenceLocationSignal is reserved (HDR-J-3a) - no reader consumes
	// it in Phase B. It exists in this enum and in migration 0074's CHECK
	// constraint now so a future phase that does add a reader needs no
	// migration just to widen the CHECK.
	EvidenceLocationSignal EvidenceType = "location_signal"
)

func validEvidenceType(t EvidenceType) bool {
	switch t {
	case EvidenceDeclaredResidence, EvidenceVerifiedResidence, EvidenceLocationSignal:
		return true
	default:
		return false
	}
}

// EvidenceCollectionActiveRecord is one jurisdiction_evidence_collection_active row.
type EvidenceCollectionActiveRecord struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	EvidenceType EvidenceType
	Active       bool
}

// SetEvidenceCollectionActiveParams is SetEvidenceCollectionActive's input.
type SetEvidenceCollectionActiveParams struct {
	TenantID     uuid.UUID
	EvidenceType EvidenceType
	Active       bool
	// ActorType/ActorID identify who is flipping the fact - staff or
	// system only (migration 0074's own CHECK), mirroring
	// SetResolutionActive's identical discipline: this is a configuration
	// change, never a player action.
	ActorType ActorType
	ActorID   uuid.UUID
	// ReasonCode is REQUIRED on every write, and is recorded in the audit
	// entry alongside before/after - the same discipline
	// SetResolutionActive applies, and doubly warranted here since this
	// fact gates collection of privacy-sensitive personal data under a
	// lawful-basis judgment (HDR-J-3e).
	ReasonCode string
	IPAddress  string
	UserAgent  string
	RequestID  string
}

// SetEvidenceCollectionActive creates or updates the (tenant,
// evidence_type) row and writes an audit_log entry
// ("jurisdiction_evidence_collection_active.changed") in the SAME
// transaction, per CLAUDE.md's "every mutating administrative action
// writes an audit record" rule. tx must be a tenant-scoped,
// non-player-scoped transaction (db.Pool.WithTenant) - migration 0074's
// RLS policy rejects anything else.
func SetEvidenceCollectionActive(ctx context.Context, tx pgx.Tx, p SetEvidenceCollectionActiveParams) (EvidenceCollectionActiveRecord, error) {
	if p.TenantID == uuid.Nil {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !validEvidenceType(p.EvidenceType) {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("%w: unknown evidence_type %q", ErrInvalidInput, p.EvidenceType)
	}
	if p.ActorType != ActorStaff && p.ActorType != ActorSystem {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("%w: actor_type must be staff or system", ErrInvalidInput)
	}
	if p.ActorID == uuid.Nil {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("%w: actor_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(p.ReasonCode) == "" {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("%w: reason_code is required on every evidence-collection-active change", ErrInvalidInput)
	}

	var before *bool
	if err := tx.QueryRow(ctx,
		`SELECT active FROM jurisdiction_evidence_collection_active WHERE tenant_id = $1 AND evidence_type = $2`,
		p.TenantID, string(p.EvidenceType)).Scan(&before); err != nil && err != pgx.ErrNoRows {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("jurisdiction: read evidence-collection-active fact: %w", err)
	}

	var rec EvidenceCollectionActiveRecord
	var et string
	err := tx.QueryRow(ctx, `
		INSERT INTO jurisdiction_evidence_collection_active (tenant_id, evidence_type, active, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, evidence_type) DO UPDATE SET active = EXCLUDED.active
		RETURNING id, tenant_id, evidence_type, active`,
		p.TenantID, string(p.EvidenceType), p.Active, string(p.ActorType), p.ActorID,
	).Scan(&rec.ID, &rec.TenantID, &et, &rec.Active)
	if err != nil {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("jurisdiction: upsert evidence-collection-active fact: %w", err)
	}
	rec.EvidenceType = EvidenceType(et)

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: audit.ActorType(p.ActorType), ActorID: p.ActorID,
		Action: "jurisdiction_evidence_collection_active.changed", TargetType: "jurisdiction_evidence_collection_active",
		TargetID: rec.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: p.IPAddress, UserAgent: p.UserAgent, RequestID: p.RequestID,
		Metadata: map[string]any{
			"evidence_type": string(p.EvidenceType),
			"before_active": before,
			"after_active":  p.Active,
			"reason_code":   p.ReasonCode,
		},
	}); err != nil {
		return EvidenceCollectionActiveRecord{}, fmt.Errorf("jurisdiction: audit evidence-collection-active change: %w", err)
	}
	return rec, nil
}

// IsEvidenceCollectionActive is the narrow, READ-ONLY accessor a future
// declared-residence write path or verified-residence determination path
// consumes before accepting a write. It answers false for any (tenant,
// evidence_type) pair with no row at all - fail-closed, never "not
// configured means unrestricted". Takes a ReadOnlyQuerier, exactly like
// IsActive, for the same reason: a future consumer reads it without a
// write handle.
func IsEvidenceCollectionActive(ctx context.Context, q ReadOnlyQuerier, tenantID uuid.UUID, evidenceType EvidenceType) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	var active bool
	err := q.QueryRow(ctx,
		`SELECT active FROM jurisdiction_evidence_collection_active WHERE tenant_id = $1 AND evidence_type = $2`,
		tenantID, string(evidenceType)).Scan(&active)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("jurisdiction: read evidence-collection-active fact: %w", err)
	}
	return active, nil
}

// ListEvidenceCollectionActive lists every (evidence_type, active) fact
// for a tenant - used by an admin read surface / diagnostics, never by an
// enforcement path (enforcement uses IsEvidenceCollectionActive for
// exactly one pair).
func ListEvidenceCollectionActive(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]EvidenceCollectionActiveRecord, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, evidence_type, active FROM jurisdiction_evidence_collection_active WHERE tenant_id = $1 ORDER BY evidence_type`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list evidence-collection-active facts: %w", err)
	}
	defer rows.Close()

	var out []EvidenceCollectionActiveRecord
	for rows.Next() {
		var rec EvidenceCollectionActiveRecord
		var et string
		if err := rows.Scan(&rec.ID, &rec.TenantID, &et, &rec.Active); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan evidence-collection-active fact: %w", err)
		}
		rec.EvidenceType = EvidenceType(et)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jurisdiction: iterate evidence-collection-active facts: %w", err)
	}
	return out, nil
}
