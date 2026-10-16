package rg

// ADR 0115 SLICE S4-min: the per-tenant / per-brand ENABLE of limit enforcement (migration 0158).
//
// OFF by default, and a ONE-WAY RATCHET: there is no function, route or SQL path that disables it (the
// table is append-only and holds enabling rows only). Switching a player-protection control off is a
// recorded decision plus a migration, never a software operation. The row is written by an active staff
// principal of the same tenant (the operator-configuration rule of migration 0135); the actor, the time
// and the id are set by the database.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// EnforcementActivation is one stored enabling row.
type EnforcementActivation struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	BrandID        *uuid.UUID // nil = every brand of the tenant
	ReasonCode     string
	AttestationRef string
	CreatedByStaff uuid.UUID
	CreatedAt      time.Time
}

// ActivateEnforcementInput is one enable request. tx MUST come from db.Pool.WithPrincipalScope (tenant,
// staff). TenantID and StaffID are the authenticated session's. BrandID (optional) must be a brand of
// the tenant (the composite foreign key refuses any other).
type ActivateEnforcementInput struct {
	TenantID, StaffID uuid.UUID
	BrandID           *uuid.UUID
	ReasonCode        string
	AttestationRef    string
	Meta              RequestMeta
}

func (in ActivateEnforcementInput) validate() error {
	if in.TenantID == uuid.Nil || in.StaffID == uuid.Nil {
		return invalidf("tenant and staff are required")
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"reason_code", in.ReasonCode, 64}, {"attestation_ref", in.AttestationRef, 200}} {
		if f.v == "" || len(f.v) > f.max || !printableASCII(f.v) || strings.TrimSpace(f.v) == "" {
			return invalidf("%s must be 1 to %d printable ASCII characters with a non-space character", f.name, f.max)
		}
	}
	return nil
}

const activationColumns = `id, tenant_id, brand_id, reason_code, attestation_ref, created_by_actor_id, created_at`

func scanActivation(row pgx.Row) (EnforcementActivation, error) {
	var a EnforcementActivation
	err := row.Scan(&a.ID, &a.TenantID, &a.BrandID, &a.ReasonCode, &a.AttestationRef, &a.CreatedByStaff, &a.CreatedAt)
	return a, err
}

// ActivateEnforcement switches limit enforcement ON for the tenant (BrandID nil) or one brand. It is
// idempotent: an activation that already exists for the same scope is returned with created == false and
// writes no second row and no second audit record. The audit record commits or rolls back with the row.
func (l *Limits) ActivateEnforcement(ctx context.Context, tx pgx.Tx, in ActivateEnforcementInput) (EnforcementActivation, bool, error) {
	if err := in.validate(); err != nil {
		return EnforcementActivation{}, false, err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return EnforcementActivation{}, false, fmt.Errorf("rg: open savepoint: %w", err)
	}
	var id uuid.UUID
	err = sp.QueryRow(ctx, `INSERT INTO rg_enforcement_activations (tenant_id, brand_id, reason_code, attestation_ref)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		in.TenantID, in.BrandID, in.ReasonCode, in.AttestationRef).Scan(&id)
	if err != nil {
		_ = sp.Rollback(ctx)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			// already enabled for this scope: return the stored row
			existing, rerr := scanActivation(tx.QueryRow(ctx, `SELECT `+activationColumns+` FROM rg_enforcement_activations
				WHERE tenant_id = $1 AND brand_id IS NOT DISTINCT FROM $2`, in.TenantID, in.BrandID))
			if rerr != nil {
				return EnforcementActivation{}, false, fmt.Errorf("rg: read existing activation: %w", rerr)
			}
			return existing, false, nil
		}
		return EnforcementActivation{}, false, MapDBError(err)
	}
	if err := sp.Commit(ctx); err != nil {
		return EnforcementActivation{}, false, fmt.Errorf("rg: release savepoint: %w", err)
	}
	stored, err := scanActivation(tx.QueryRow(ctx, `SELECT `+activationColumns+` FROM rg_enforcement_activations WHERE id = $1`, id))
	if err != nil {
		return EnforcementActivation{}, false, fmt.Errorf("rg: read back activation: %w", err)
	}
	if err := l.cfg.Audit(ctx, tx, audit.Entry{
		TenantID: in.TenantID, ActorType: audit.ActorStaff, ActorID: in.StaffID, Action: "rg.enforcement.activated",
		TargetType: "rg_enforcement_activation", TargetID: id.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: in.Meta.IPAddress, UserAgent: in.Meta.UserAgent, RequestID: in.Meta.RequestID,
		Metadata: map[string]any{"brand_id": in.BrandID, "reason_code": in.ReasonCode, "attestation_ref": in.AttestationRef,
			"effective_at": stored.CreatedAt.UTC().Format(time.RFC3339Nano)},
	}); err != nil {
		return EnforcementActivation{}, false, fmt.Errorf("rg: audit enforcement activation: %w", err)
	}
	return stored, true, nil
}

// ListEnforcementActivations returns the session tenant's activations, oldest first (RLS bounds them).
func ListEnforcementActivations(ctx context.Context, tx pgx.Tx) ([]EnforcementActivation, error) {
	rows, err := tx.Query(ctx, `SELECT `+activationColumns+` FROM rg_enforcement_activations ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("rg: list enforcement activations: %w", err)
	}
	defer rows.Close()
	var out []EnforcementActivation
	for rows.Next() {
		a, err := scanActivation(rows)
		if err != nil {
			return nil, fmt.Errorf("rg: scan activation: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
