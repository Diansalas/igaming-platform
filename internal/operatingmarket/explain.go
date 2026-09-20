package operatingmarket

// This file implements the admin-only "explain why" diagnostic (ADR 0045
// §5.1). ExplainOperatingCountryPolicy re-runs the SAME resolution
// algorithm as ResolveOperatingCountryPolicy (resolve.go's own resolve()
// function) and additionally returns the full chain - it is a WINDOW onto
// the decision, never a second, driftable implementation of it.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// ChainStep records one rung's state as considered during resolution -
// licence (the ceiling), tenant, brand, operation, in that order.
type ChainStep struct {
	Scope                  Scope
	Present                bool
	State                  *State
	Status                 *Status
	VersionID              *uuid.UUID
	EffectiveFrom          *time.Time
	EffectiveTo            *time.Time
	ReasonCode             string
	AuthorizationReference string
	// Inherited is true when Present == false and this rung inherited from
	// the rung above it (brand/operation only - tenant absence is
	// terminal, never "inherited"); for an operation-rung candidate step
	// (Present == true), Inherited is instead true when the candidate is
	// withdrawn (a tombstone: never permits, never blocks, never masks)
	// and false when it is live (ADR 0045 §3.5-A AMENDMENT-1).
	Inherited bool
	// BrandID and ProductCode are populated ONLY on Scope==ScopeOperation
	// steps (ADR 0045 §3.5-A AMENDMENT-1's full-candidate-set chain
	// emission) - nil on every tenant/brand step.
	BrandID     *uuid.UUID
	ProductCode *string
}

// Explanation is ExplainOperatingCountryPolicy's result - STAFF/ADMIN
// ONLY. It carries every diagnostic field Result deliberately withholds.
type Explanation struct {
	Query                 Query
	LicenceID             *uuid.UUID
	LicenceStatus         string
	LicenceExpiresAt      *time.Time
	LicenceValidity       jurisdiction.LicenceValidity
	CeilingReason         LicenceCeilingReason
	Chain                 []ChainStep // ordered: licence, tenant, brand, operation
	Outcome               Outcome
	SourceScope           Scope
	SourceVersionID       *uuid.UUID
	BlockingScope         Scope // "" when permitted
	BlockingVersionID     *uuid.UUID
	BlockingEffectiveFrom *time.Time
	ReasonCode            string // the blocking version's own reason_code
	PolicyVersion         string
	AsOf                  time.Time
}

// ExplainOperatingCountryPolicy re-runs the SAME resolution algorithm and
// additionally returns the full chain. It is STAFF/ADMIN ONLY.
//
// tx MUST be a tenant-scoped, non-player-scoped transaction for
// p.TenantID (assertTenantScope, resolve()'s first statement). There is
// NO platform-admin path to a tenant's chain: a tenant's operating
// footprint is commercially sensitive and is not platform-readable (ADR
// 0045 §7.3).
//
// This function MUST NOT be called from, or reachable by, any
// player-facing handler. The route that eventually exposes it is gated by
// auth.PermOperatingMarketPolicyRead - no such route exists in this
// phase.
//
// INVARIANT (tested): ExplainOperatingCountryPolicy(...).Outcome always
// equals ResolveOperatingCountryPolicy(...).Outcome() for the same Query
// and the same row set - both are produced by the ONE internal resolve()
// that returns the chain; ResolveOperatingCountryPolicy discards it.
func ExplainOperatingCountryPolicy(ctx context.Context, tx pgx.Tx, p Query) (Explanation, error) {
	res, err := resolve(ctx, tx, p)
	if err != nil {
		return Explanation{}, err
	}

	exp := Explanation{
		Query:         p,
		CeilingReason: res.ceilingReason,
		Chain:         res.chain,
		Outcome:       res.outcome,
		SourceScope:   res.sourceScope,
		PolicyVersion: res.policyVersion,
		AsOf:          res.asOf,
	}
	if res.licenceID != uuid.Nil {
		id := res.licenceID
		exp.LicenceID = &id
		exp.LicenceValidity = res.licenceValidity
	}
	if res.sourceVersionID != uuid.Nil {
		id := res.sourceVersionID
		exp.SourceVersionID = &id
	}
	if res.blockingScope != "" {
		exp.BlockingScope = res.blockingScope
		id := res.blockingVersionID
		exp.BlockingVersionID = &id
		t := res.blockingEffectiveFrom
		exp.BlockingEffectiveFrom = &t
		exp.ReasonCode = res.blockingReasonCode
	}

	// LicenceStatus/LicenceExpiresAt are read separately from the raw
	// licences row - resolve() itself only needs EvaluateLicenceValidity's
	// classified outcome, never the raw status/expiry, to make its
	// decision. This is diagnostic-only, staff-gated content (never
	// exposed via Result), so a second, small read here does not
	// duplicate any DECISION logic - it only surfaces already-decided-upon
	// raw fields for a human to read.
	if exp.LicenceID != nil {
		var status string
		var expiresAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT status, expires_at FROM licences WHERE id = $1`, *exp.LicenceID).Scan(&status, &expiresAt); err != nil {
			return Explanation{}, fmt.Errorf("operatingmarket: read licence status for explanation: %w", err)
		}
		exp.LicenceStatus = status
		exp.LicenceExpiresAt = expiresAt
	}

	return exp, nil
}
