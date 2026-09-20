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

// assertTenantScope compares the tenant this resolution is being produced
// FOR against the transaction's own `app.tenant_id` GUC - canonical-model
// §4.4 Layer 2's first half ("the resolver asserts the transaction's
// app.tenant_id GUC against the tenant it resolves for, by the same query
// assertTenantScope uses"), deliberately written as a byte-for-byte
// behavioural mirror of internal/assetregistry's own assertTenantScope
// (authorization.go) so the two cannot drift.
//
// WHY THIS IS LOAD-BEARING AND NOT DEFENCE-IN-DEPTH (architect, Stage 4I
// final cross-domain certification; RESTATED by Stage 4I Phase E-SECURITY,
// migration 0077, which changed the premise but not the conclusion; FURTHER
// CORRECTED by this fix round's Fix 10, below): every table
// resolveTenantLicence reads - `tenants`, `licences`, `jurisdictions` - was,
// through Phase E, a platform-wide reference table with NO row-level
// security at all (verified against pg_class.relrowsecurity). Migration
// 0077 gave all three RLS: `tenants` and `jurisdictions` are DELIBERATELY
// read-open for every non-player scope (`USING (true)`) - this call site
// (identity.GetTenantBySlug and three WithoutTenant active-tenant sweeps
// need to read `tenants` from scopes with no tenant match to offer) is one
// of the reasons that read posture was chosen, per migration 0077's own
// header comment - but `licences` is NOT read-open: `licences_read` is
// narrowed to platform-admin, or the tenant whose own `tenants.licence_id`
// names the row.
//
// CORRECTED (this fix round, Fix 10): a prior version of this comment
// claimed RLS provides "ZERO READ isolation on this path" and that "without
// this assertion, Resolve would hand a transaction scoped to tenant A a
// fully Resolved jurisdiction belonging to tenant B" - both overstate the
// gap as it stands today. Because `licences_read` is narrow, a transaction
// scoped to tenant A cannot even READ tenant B's licence row: the
// `licences JOIN jurisdictions` query below would return zero rows for a
// foreign licence, and this function would return
// `refused(ReasonDependencyUnavailable)`, NOT a `Resolved` result for
// tenant B - even without this assertion. `tenants.licence_id` itself
// remains readable cross-tenant (only `licences`' content is narrowed), so
// the assertion is still necessary and load-bearing: without it, a
// mis-scoped call degrades only to that misleading fail-closed
// `refused(dependency_unavailable)` outcome instead of a diagnosable
// `ErrScopeMismatch` - a real difference for whoever has to debug it, even
// though neither outcome is a data leak. The assertion is therefore backed
// by, not substituted by, this second independent layer.
//
// Resolution.AssertScope cannot substitute for this: it compares the
// resolution's binding to the values the CALLER passes, which are the same
// values Resolve was given - the consuming gates' own comments in
// internal/casino and internal/bonus honestly record that this makes it
// tautological today. Only a comparison against the CONNECTION's proven
// scope is non-tautological, which is exactly why canonical-model §4.4
// specifies both halves and why CLAUDE.md requires tenant authority to come
// from server-side authenticated context rather than an argument.
//
// Note also that canonical-model §6.1's stated safety net - "a resolver
// query on a bare pool connection reads zero rows under FORCE RLS" - holds
// for the `licences` leg (narrow read policy) but not for the `tenants`
// leg (`tenants.licence_id` remains readable from a bare/tenant-scoped
// connection) - this function is what makes the intended behaviour real on
// the `tenants` leg instead of assumed.
func assertTenantScope(ctx context.Context, q ReadOnlyQuerier, tenantID uuid.UUID) (bool, error) {
	var scoped *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := q.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&scoped, &scopedPlayer); err != nil {
		return false, fmt.Errorf("jurisdiction: read tenant scope: %w", err)
	}
	if scoped == nil {
		// A caller mistake, not a data gap: resolving a tenant-subject
		// operation on a connection with no proven tenant scope is
		// unanswerable, and must never be reported as an ordinary
		// `unresolved` (canonical-model §6.1: "It must error, not return
		// unresolved").
		return false, fmt.Errorf("%w: transaction has no tenant scope (use db.Pool.WithTenant)", ErrScopeMismatch)
	}
	// Fix 6 (Stage 4I Phase E-SECURITY fix round): mirrors
	// internal/operatingmarket's own assertTenantScope exact pattern. This
	// branch is unreachable today - both production callers of
	// jurisdiction.Resolve always pass a non-nil PlayerAccountID, which
	// short-circuits Resolve before this code path is ever reached (see
	// Resolve's own doc comment) - but if that ever changes, a
	// player-scoped transaction reaching resolveTenantLicence must fail
	// with a diagnosable scope error, not a misleading
	// refused(dependency_unavailable) produced by licences_read's
	// player-exclusion conjunct silently returning zero rows.
	if scopedPlayer != nil {
		return false, fmt.Errorf("%w: transaction must not be player-scoped", ErrScopeMismatch)
	}
	return *scoped == tenantID, nil
}

// resolveTenantLicence implements the ONE basis Stage 4I can genuinely
// produce (canonical-model §11.1 B-4): tenants.licence_id ->
// licences.jurisdiction_id -> jurisdictions.code. This is the first
// production READ of that relationship anywhere in this platform's
// history (RECON §10) - the columns have existed, unread, since
// migration 0002.
func resolveTenantLicence(ctx context.Context, q ReadOnlyQuerier, base Resolution) (Resolution, error) {
	// canonical-model §4.4 Layer 2 - see assertTenantScope's own doc
	// comment for why this is the ONLY non-tautological tenant check on
	// this path. It is placed here, rather than at the top of Resolve,
	// deliberately: this is the one and only branch that can return a
	// `Resolved` outcome, and it is the one and only branch that queries
	// at all (the player-scoped branch returns unresolved(no_signal)
	// without touching the database, so it can leak nothing and must not
	// be made to require a connection it does not use).
	inScope, err := assertTenantScope(ctx, q, base.tenantID)
	if err != nil {
		return Resolution{}, err
	}
	if !inScope {
		return refused(base, ReasonScopeMismatch), nil
	}

	var licenceID *uuid.UUID
	err = q.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, base.tenantID).Scan(&licenceID)
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
