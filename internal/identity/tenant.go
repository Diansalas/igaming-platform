package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// ErrPlatformTransactionScope is returned when tx is not a genuinely
// platform-scoped transaction (app.platform_admin_principal_id set AND
// app.tenant_id / app.player_account_id both unset). Deliberately a NEW
// sentinel, NOT a reuse of the existing identity.ErrTransactionScope
// (player_account.go:42), whose message and meaning are bound to
// SetPlayerAccountDeclaredResidence's tenant-scope contract.
var ErrPlatformTransactionScope = errors.New("identity: requires a platform-admin-scoped transaction (use db.Pool.WithPlatformAdmin)")

// assertPlatformScope mirrors jurisdiction.assertPlatformScope
// (internal/jurisdiction/evaluation_policy_admin.go) byte for byte: it
// reads the three relevant GUCs in ONE query and requires
// app.platform_admin_principal_id to be set AND both app.tenant_id and
// app.player_account_id to be unset. Migration 0077's RLS policies on
// `tenants` enforce the identical predicate independently at the
// database, so a caller that bypassed this check would still fail at the
// INSERT, but with a much less diagnosable error - this is a REAL
// control, not defence-in-depth commentary.
func assertPlatformScope(ctx context.Context, tx pgx.Tx) error {
	var platformAdmin *uuid.UUID
	var scopedTenant *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
		        NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&platformAdmin, &scopedTenant, &scopedPlayer); err != nil {
		return fmt.Errorf("identity: read platform admin scope: %w", err)
	}
	if platformAdmin == nil || scopedTenant != nil || scopedPlayer != nil {
		return ErrPlatformTransactionScope
	}
	return nil
}

// Tenant is the commercial/legal/licensing relationship - see
// docs/decisions/0012-brand-distinct-from-tenant.md. This package only
// exposes what identity/auth flows need (creation and slug lookup for
// staff-login resolution); the fuller tenant/licence/jurisdiction
// management surface belongs to a future partner-console-facing service.
type Tenant struct {
	ID             uuid.UUID
	Name           string
	Slug           string
	LicensingModel string
	Status         string
}

// GetTenantBySlug resolves a tenant by its public slug, for staff-login
// tenant resolution. Migration 0077 gives `tenants` a read-open RLS
// policy (`tenants_read ... USING (true)`, the same posture migration
// 0044 gives `assets`) deliberately BECAUSE this call site has no tenant
// context to offer by construction (staff login happens before any
// tenant is known), so this remains a plain WithoutTenant read - RLS now
// backs that read rather than the table simply carrying none.
func GetTenantBySlug(ctx context.Context, pool *db.Pool, slug string) (Tenant, error) {
	var t Tenant
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, name, slug, licensing_model, status FROM tenants WHERE slug = $1`,
			slug,
		).Scan(&t.ID, &t.Name, &t.Slug, &t.LicensingModel, &t.Status)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("identity: get tenant by slug: %w", err)
	}
	return t, nil
}

// GetTenantByID resolves a tenant within an already-open transaction -
// unlike GetTenantBySlug (used before any tenant context exists, e.g.
// staff-login resolution), this is for a caller that already holds a
// tx and needs the tenant's own canonical fields (LicensingMode, in
// particular - Stage 4G-FINAL Part D's "risk.Evaluate never resolves
// licensing mode itself, the caller does" contract). Since migration 0077,
// `tenants_read` is `USING (true)` (see GetTenantBySlug's doc comment for
// why), so this still resolves identically regardless of the tx's own
// tenant scope - id MUST therefore always be a server-derived value from
// the caller's own already-authenticated context (e.g. tenant.FromContext,
// or a foreign key already scoped to the right tenant), never a
// client-supplied id, exactly like every other cross-tenant-capable
// lookup in this codebase. RLS's read-open posture here is a deliberate
// choice (multiple scopeless/cross-tenant readers legitimately need it),
// not a gap - the discipline that this function is never called with a
// client-supplied id is still enforced entirely by caller convention, not
// by the database, exactly as before this migration.
func GetTenantByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Tenant, error) {
	var t Tenant
	err := tx.QueryRow(ctx,
		`SELECT id, name, slug, licensing_model, status FROM tenants WHERE id = $1`,
		id,
	).Scan(&t.ID, &t.Name, &t.Slug, &t.LicensingModel, &t.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("identity: get tenant by id: %w", err)
	}
	return t, nil
}

// ListTenants returns a page of tenants for the platform back office's
// tenant list, ordered by name for stable, deterministic pagination. q (when
// non-empty) is matched case-insensitively as a substring against name OR
// slug; status (when non-empty) is an exact match against the status
// column. Both filters are passed as bound parameters - never string-
// concatenated into the query text - mirroring every other dynamic-filter
// query in this codebase (e.g. internal/risk's rule queries). total is the
// count of ALL rows matching the filters (not just this page), computed via
// count(*) OVER() in the same query so it can never drift from what was
// actually read; it is 0 (not queried separately) when the page itself is
// empty, which is the correct total in that case regardless of why the page
// is empty (no matches at all, or an offset past the end).
//
// Callers determine tx's scope - this function issues one read against
// `tenants`, and (per migration 0077) tenants_read admits any transaction
// that is not player-scoped, so this resolves identically whether tx came
// from db.Pool.WithoutTenant (the platform-wide list) or db.Pool.WithTenant
// (a tenant-scoped caller reading only itself, filtered by the caller via a
// WHERE id = $n the caller adds - see ListTenants' own callers).
func ListTenants(ctx context.Context, tx pgx.Tx, q, status string, limit, offset int) ([]Tenant, int, error) {
	query := `SELECT id, name, slug, licensing_model, status, count(*) OVER() FROM tenants WHERE true`
	args := []any{}
	if q != "" {
		args = append(args, "%"+q+"%")
		query += fmt.Sprintf(" AND (name ILIKE $%d OR slug ILIKE $%d)", len(args), len(args))
	}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY name ASC, id ASC LIMIT $%d", len(args))
	args = append(args, offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("identity: list tenants: %w", err)
	}
	defer rows.Close()

	var tenants []Tenant
	var total int
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.LicensingModel, &t.Status, &total); err != nil {
			return nil, 0, fmt.Errorf("identity: scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	return tenants, total, rows.Err()
}

// CreateTenant provisions a new tenant. Platform-admin-only (see
// PermTenantWrite) - tenant provisioning is inherently a platform-level
// action, never tenant-scoped. Takes a pgx.Tx (rather than opening its
// own transaction) so the caller can write the insert and its audit
// record atomically - see CreateBrand/CreateStaffUser for the same
// pattern, and the Stage 2 backend review finding this fixes: a tenant
// could previously be created with its audit-record write silently
// failing in a separate transaction. tx MUST now be a
// db.Pool.WithPlatformAdmin transaction (Stage 4I Phase E-SECURITY,
// migration 0077): `tenants` gained RLS with NO tenant-scoped write
// policy of any kind - the ordinary tenant-scoped INSERT this function
// used to accept under a WithoutTenant/WithTenant transaction is exactly
// the write surface migration 0077 closes, so assertPlatformScope below
// is checked FIRST, before anything else, and migration 0077's own
// tenants_platform_admin_insert policy enforces the identical predicate
// independently at the database.
func CreateTenant(ctx context.Context, tx pgx.Tx, name, slug, licensingModel string) (Tenant, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return Tenant{}, err
	}
	t := Tenant{ID: uuid.New(), Name: name, Slug: slug, LicensingModel: licensingModel, Status: "active"}
	_, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, $4)`,
		t.ID, t.Name, t.Slug, t.LicensingModel,
	)
	if err != nil {
		return Tenant{}, errSlugTaken("create tenant", err)
	}
	return t, nil
}
