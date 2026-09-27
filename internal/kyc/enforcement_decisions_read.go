package kyc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnforcementDecisionRecord is one kyc_enforcement_decisions row, exactly
// the field set ADR 0096 §6 authorizes for staff exposure - never
// kyc_verifications.reason, person/document data, or an amount.
type EnforcementDecisionRecord struct {
	ID             uuid.UUID
	Operation      string
	Outcome        string
	MatchedTrigger string
	PolicyVersion  string
	DecidedAt      time.Time
}

// MaxEnforcementDecisionsPageSize is the server-enforced maximum page
// size (ADR 0096 §6 "never unbounded, never offset-based").
const MaxEnforcementDecisionsPageSize = 100

// DefaultEnforcementDecisionsPageSize is applied when the caller supplies
// none.
const DefaultEnforcementDecisionsPageSize = 25

// ListEnforcementDecisionsParams is ListEnforcementDecisionsForPlayer's
// input. TenantID MUST come from the authenticated staff context (never
// a query parameter); PlayerAccountID is a path/query value the caller
// supplies, subject to the tenant scope of tx (RLS on
// kyc_enforcement_decisions is tenant_isolation, migration 0100).
type ListEnforcementDecisionsParams struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID
	Limit           int
	// BeforeDecidedAt/BeforeID implement keyset pagination on
	// (decided_at DESC, id DESC) - both zero means "first page".
	BeforeDecidedAt *time.Time
	BeforeID        *uuid.UUID
}

// ListEnforcementDecisionsForPlayer returns a keyset page of
// kyc_enforcement_decisions for a player, most recent first. Runs inside
// tx, which MUST already be tenant-scoped (db.Pool.WithTenant) - this
// function performs no authorization of its own beyond relying on RLS
// and the explicit tenant_id filter below (belt-and-braces, matching this
// codebase's convention elsewhere).
func ListEnforcementDecisionsForPlayer(ctx context.Context, tx pgx.Tx, p ListEnforcementDecisionsParams) ([]EnforcementDecisionRecord, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultEnforcementDecisionsPageSize
	}
	if limit > MaxEnforcementDecisionsPageSize {
		limit = MaxEnforcementDecisionsPageSize
	}

	var rows pgx.Rows
	var err error
	if p.BeforeDecidedAt != nil && p.BeforeID != nil {
		rows, err = tx.Query(ctx, `
			SELECT id, operation, outcome, COALESCE(matched_trigger, ''), policy_version, decided_at
			  FROM kyc_enforcement_decisions
			 WHERE tenant_id = $1 AND player_account_id = $2
			   AND (decided_at, id) < ($3, $4)
			 ORDER BY decided_at DESC, id DESC
			 LIMIT $5`,
			p.TenantID, p.PlayerAccountID, *p.BeforeDecidedAt, *p.BeforeID, limit)
	} else {
		rows, err = tx.Query(ctx, `
			SELECT id, operation, outcome, COALESCE(matched_trigger, ''), policy_version, decided_at
			  FROM kyc_enforcement_decisions
			 WHERE tenant_id = $1 AND player_account_id = $2
			 ORDER BY decided_at DESC, id DESC
			 LIMIT $3`,
			p.TenantID, p.PlayerAccountID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("kyc: list enforcement decisions: %w", err)
	}
	defer rows.Close()

	var out []EnforcementDecisionRecord
	for rows.Next() {
		var r EnforcementDecisionRecord
		if err := rows.Scan(&r.ID, &r.Operation, &r.Outcome, &r.MatchedTrigger, &r.PolicyVersion, &r.DecidedAt); err != nil {
			return nil, fmt.Errorf("kyc: scan enforcement decision: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
