package jurisdiction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Resolve answers exactly one question (canonical-model §2.1): for this
// operation, of this class, by this subject, under this tenant and
// brand - which jurisdiction's rules govern, and on what basis? It does
// NOT decide whether the tenant may lawfully serve that jurisdiction,
// whether a limit is breached, whether an asset is authorized, or
// whether the player may gamble - those are separate checks in separate
// domains that CONSUME a Resolution, never reimplement one.
//
// q must be read-only capable code (see ReadOnlyQuerier's own doc
// comment). Resolve issues no Exec of any kind: the interface exposes
// only Query/QueryRow, so a write added to this function's body would
// not compile - the structural enforcement RISK H-2/canonical-model §6.4
// require ("the jurisdiction resolver is READ-ONLY on the evaluation
// path... enforced structurally, not by comment").
//
// STAGE 4I SCOPE, STATED PLAINLY (canonical-model §11.3, §3.1, §3.2):
// the ONLY basis this function can produce is tenant_licence, and ONLY
// for a tenant/brand-subject operation (p.PlayerAccountID == nil) - never
// for a player-scoped one. Every player-scoped operation resolves
// unresolved(no_signal): no player-side jurisdiction signal exists
// anywhere in this codebase (HDR-J-3 is unanswered), and this function
// must NEVER silently default to any other jurisdiction to paper over
// that gap. `platform_fallback` and `staff_supplied` are structurally
// unreachable return values - no branch of this function's logic ever
// assigns either to a Resolution's selected basis.
func Resolve(ctx context.Context, q ReadOnlyQuerier, p Params) (Resolution, error) {
	base := Resolution{
		asOf: time.Now().UTC(), policyVersion: PolicyVersion,
		tenantID: p.TenantID, brandID: p.BrandID, playerAcctID: p.PlayerAccountID,
		operationClass:       p.OperationClass,
		requestedByActorType: p.RequestedByActorType,
		requestedByActorID:   p.RequestedByActorID,
	}

	if p.TenantID == uuid.Nil {
		return refused(base, ReasonScopeMismatch), nil
	}
	if p.RequestedByActorType != "" && !validActorType(p.RequestedByActorType) {
		return Resolution{}, fmt.Errorf("%w: unknown requested_by_actor_type %q", ErrInvalidInput, p.RequestedByActorType)
	}
	if !validOperationClass(p.OperationClass) {
		return refused(base, ReasonUnsupportedOperationClass), nil
	}

	// §3.2: tenant_licence may be selected ONLY for a tenant/brand-subject
	// operation, never a player-scoped one - this IS the boundary that
	// keeps Stage 4I's one producible basis from becoming HDR-J-1's
	// forbidden fallback wearing a different name.
	if p.PlayerAccountID == nil {
		return resolveTenantLicence(ctx, q, base)
	}

	// Every player-scoped operation, Stage 4I: no player-side basis of
	// any kind exists in this codebase. This is the honest, disclosed
	// steady state (canonical-model §11.3) - NEVER a silent default to
	// any jurisdiction, and never `refused` (a data gap is not a
	// dependency failure - see Reason's own doc comment on the
	// unresolved/refused distinction, canonical-model §2.2).
	return unresolved(base, ReasonNoSignal), nil
}

// resolveTenantLicence implements the ONE basis Stage 4I can genuinely
// produce (canonical-model §11.1 B-4): tenants.licence_id ->
// licences.jurisdiction_id -> jurisdictions.code. This is the first
// production READ of that relationship anywhere in this platform's
// history (RECON §10) - the columns have existed, unread, since
// migration 0002.
func resolveTenantLicence(ctx context.Context, q ReadOnlyQuerier, base Resolution) (Resolution, error) {
	var licenceID *uuid.UUID
	err := q.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, base.tenantID).Scan(&licenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The tenant id itself does not resolve - a scope problem, not a
		// data gap (this can only happen if a caller passed a tenant id
		// that was never proven, which is Layer 2's job to have already
		// rejected upstream; Resolve fails closed anyway).
		return refused(base, ReasonScopeMismatch), nil
	}
	if err != nil {
		return Resolution{}, fmt.Errorf("jurisdiction: read tenant licence: %w", err)
	}
	if licenceID == nil {
		// No licence configured yet for this tenant - an ordinary,
		// expected "no signal available" case, not a dependency failure.
		return unresolved(base, ReasonNoSignal), nil
	}

	var jurisdictionID uuid.UUID
	var code string
	err = q.QueryRow(ctx, `
		SELECT j.id, j.code
		  FROM licences l
		  JOIN jurisdictions j ON j.id = l.jurisdiction_id
		 WHERE l.id = $1`, *licenceID).Scan(&jurisdictionID, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		// tenants.licence_id names a row that does not resolve to a
		// jurisdiction - a registry integrity problem, never silently
		// treated the same as "not configured yet" (that would make an
		// outage indistinguishable from a data gap, canonical-model §2.2).
		return refused(base, ReasonDependencyUnavailable), nil
	}
	if err != nil {
		return Resolution{}, fmt.Errorf("jurisdiction: read licence jurisdiction: %w", err)
	}

	res := base
	res.outcome = Resolved
	res.reason = ReasonDetermined
	res.code = code
	res.id = jurisdictionID
	res.selectedBasis = BasisTenantLicence
	res.confidenceClass = ConfidenceAuthoritative
	res.registryVersion = licenceID.String()
	res.consideredBases = []ConsideredBasis{{Basis: BasisTenantLicence, Status: StatusSelected}}
	return res, nil
}

func unresolved(base Resolution, reason Reason) Resolution {
	base.outcome = Unresolved
	base.reason = reason
	return base
}

func refused(base Resolution, reason Reason) Resolution {
	base.outcome = Refused
	base.reason = reason
	return base
}
