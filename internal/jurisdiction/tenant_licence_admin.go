package jurisdiction

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file implements Stage 4I Phase A
// (docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase A;
// docs/governance/stage-4i-canonical-model.md §11.1 item B-4): the
// write path for `tenants.licence_id`. resolveTenantLicence (resolver.go)
// has always been ABLE to read tenants.licence_id -> licences.
// jurisdiction_id -> jurisdictions.code, but before this file, nothing in
// the application ever wrote that column - it could only be set by direct
// database access. That made resolveTenantLicence's one producible basis
// permanently unreachable through any sanctioned code path.
//
// This deliberately does NOT re-implement the tenant/licence-model
// consistency check migration 0007 already enforces at the database level
// (the composite FK `tenants_licence_matches_model FOREIGN KEY
// (licence_id, expected_licensee) REFERENCES licences (id, licensee)`):
// this function lets that FK raise SQLSTATE 23503 and maps it, rather than
// pre-fetching the tenant's licensing_model and comparing it in Go, which
// would be a second, driftable copy of the same rule.
//
// STAGE 4I PHASE E-SECURITY (migration 0077) CHANGED THIS FUNCTION'S
// CONTRACT: `tenants` and `licences` now carry row-level security with NO
// tenant-scoped write policy of any kind - a tenant-scoped connection can
// no longer write its own `licence_id` at all (that write was the exact
// defect this migration closes: a tenant could otherwise repoint its own
// licence_id at another tenant's BYOL licence and forge a permitted
// resolution past a ceiling its real licence never granted). tx MUST
// therefore now be a genuinely PLATFORM-scoped transaction
// (db.Pool.WithPlatformAdmin), asserted in-function via assertPlatformScope
// as this function's FIRST statement, exactly like registry_admin.go's
// CreateJurisdiction/CreateLicence/ListJurisdictions/ListLicences and
// SetJurisdictionCountryCode. The platform-only permission check at the
// HTTP layer (auth.PermTenantLicenceAssign) remains, but is no longer the
// only control - migration 0077's tenants_platform_admin_update and
// licences_read policies enforce the identical predicate independently at
// the database.
//
// The audit write ALSO moves platform-scoped as a consequence
// (recordRegistryAudit(ctx, tx, uuid.Nil, ...), not p.TenantID): a
// WithPlatformAdmin transaction never sets app.tenant_id, and audit_log's
// dual-scope RLS WITH CHECK policy requires a non-NULL tenant_id to equal
// the connection's app.tenant_id setting exactly, so a tenant-scoped audit
// row could not be written from this scope even if it were still desired.
// This mirrors CreateJurisdiction/CreateLicence's own audit scope exactly
// - this operation's audit trail is now platform-visible only
// (task-registry item MKT-AUDIT-1 tracks the deferred question of whether
// the affected tenant should also see tenant-visible evidence of its own
// licence assignment).

// TenantLicenceState is the result of an AssignTenantLicence call.
type TenantLicenceState struct {
	TenantID  uuid.UUID
	LicenceID *uuid.UUID // nil means no licence assigned
}

// AssignTenantLicenceParams is AssignTenantLicence's input.
type AssignTenantLicenceParams struct {
	TenantID  uuid.UUID
	LicenceID *uuid.UUID // nil unassigns
	Actor     ActorContext
}

// AssignTenantLicence binds (or unassigns, when p.LicenceID is nil) the
// licence a tenant actually operates under. tx MUST now be a
// PLATFORM-scoped transaction (db.Pool.WithPlatformAdmin) - see this
// file's own header comment for why (Stage 4I Phase E-SECURITY, migration
// 0077: `tenants`/`licences` gained RLS with no tenant-scoped write
// policy, and the audit write moved platform-scoped as a consequence).
func AssignTenantLicence(ctx context.Context, tx pgx.Tx, p AssignTenantLicenceParams) (TenantLicenceState, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return TenantLicenceState{}, err
	}
	if err := p.Actor.validate(); err != nil {
		return TenantLicenceState{}, err
	}
	if p.TenantID == uuid.Nil {
		return TenantLicenceState{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}

	// A JSON `null` (unassign) never has a jurisdiction code to record.
	var licenceJurisdictionCode *string

	if p.LicenceID != nil {
		// Lock the licence row against a concurrent status change for the
		// duration of this transaction. This is a point-in-time check: no
		// licence status-transition operation exists yet
		// (registry_admin.go's CreateLicence doc comment), so the
		// status != "active" branch below is currently vacuous - but it
		// must not silently regress into a no-op once one is added.
		var status, code string
		err := tx.QueryRow(ctx, `
			SELECT l.status, j.code
			  FROM licences l
			  JOIN jurisdictions j ON j.id = l.jurisdiction_id
			 WHERE l.id = $1
			 FOR SHARE OF l`, *p.LicenceID,
		).Scan(&status, &code)
		if errors.Is(err, pgx.ErrNoRows) {
			return TenantLicenceState{}, fmt.Errorf("%w: unknown licence_id %s", ErrInvalidInput, p.LicenceID)
		}
		if err != nil {
			return TenantLicenceState{}, fmt.Errorf("jurisdiction: read licence for assignment: %w", err)
		}
		if status != "active" {
			return TenantLicenceState{}, fmt.Errorf("%w: licence %s is not active (status=%s)", ErrInvalidInput, p.LicenceID, status)
		}
		licenceJurisdictionCode = &code
	}

	// The assignment itself is ONE atomic statement (rather than a
	// separate SELECT ... FOR UPDATE followed by an UPDATE) so the
	// before-state it reports can never go stale under concurrent
	// assignment of the same tenant.
	var beforeLicenceID *uuid.UUID
	err := tx.QueryRow(ctx, `
		WITH before AS (
			SELECT id, licence_id FROM tenants WHERE id = $2 FOR UPDATE
		)
		UPDATE tenants SET licence_id = $1, updated_at = now()
		FROM before
		WHERE tenants.id = before.id
		RETURNING before.licence_id`,
		p.LicenceID, p.TenantID,
	).Scan(&beforeLicenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantLicenceState{}, fmt.Errorf("%w: unknown tenant_id %s", ErrNotFound, p.TenantID)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			if pgErr.ConstraintName == "tenants_licence_matches_model" {
				return TenantLicenceState{}, fmt.Errorf("%w: licence licensee does not match this tenant's licensing_model", ErrInvalidInput)
			}
			// Defensive only: step one above already validated the licence
			// exists, so the plain licence_id -> licences(id) FK should be
			// unreachable here.
			return TenantLicenceState{}, fmt.Errorf("%w: unknown licence_id %s", ErrInvalidInput, p.LicenceID)
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "uq_tenants_exclusive_own_licence" {
				// Migration 0077's partial unique index: a licensee='tenant'
				// (BYOL) licence may be bound to exactly one tenant at a time.
				// This is a diagnosable client input error, not a server
				// fault - mirrors the 23503 arm above exactly.
				return TenantLicenceState{}, fmt.Errorf("%w: licence is already exclusively bound to another tenant", ErrInvalidInput)
			}
			// Defensive only: no other unique constraint on tenants should be
			// reachable from this statement.
			return TenantLicenceState{}, fmt.Errorf("jurisdiction: assign tenant licence: %w", err)
		}
		return TenantLicenceState{}, fmt.Errorf("jurisdiction: assign tenant licence: %w", err)
	}

	if err := recordRegistryAudit(ctx, tx, uuid.Nil, p.Actor, "jurisdiction_registry.tenant_licence_assigned", "tenant", p.TenantID.String(), map[string]any{
		"before":                    map[string]any{"licence_id": uuidOrNil(beforeLicenceID)},
		"after":                     map[string]any{"licence_id": uuidOrNil(p.LicenceID)},
		"licence_jurisdiction_code": licenceJurisdictionCode,
	}); err != nil {
		return TenantLicenceState{}, err
	}

	return TenantLicenceState{TenantID: p.TenantID, LicenceID: p.LicenceID}, nil
}

func uuidOrNil(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}
