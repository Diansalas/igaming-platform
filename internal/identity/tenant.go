package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

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
// tenant resolution. tenants has no RLS (it is platform-registry-like,
// consistent with jurisdictions/assets - see docs/architecture/03-
// database-architecture.md), so this is a plain WithoutTenant read.
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
// licensing mode itself, the caller does" contract). tenants has no RLS
// (see GetTenantBySlug's doc comment), so this resolves identically
// regardless of the tx's own tenant scope - id MUST therefore always be a
// server-derived value from the caller's own already-authenticated
// context (e.g. tenant.FromContext, or a foreign key already scoped to
// the right tenant), never a client-supplied id, exactly like every
// other cross-tenant-capable lookup in this codebase. PostgreSQL/RLS
// review finding: this table has no RLS backstop, so this discipline is
// enforced entirely by caller convention, not by the database.
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

// CreateTenant provisions a new tenant. Platform-admin-only (see
// PermTenantWrite) - tenant provisioning is inherently a platform-level
// action, never tenant-scoped. Takes a pgx.Tx (rather than opening its
// own transaction) so the caller can write the insert and its audit
// record atomically in one WithoutTenant transaction - see
// CreateBrand/CreateStaffUser for the same pattern, and the Stage 2
// backend review finding this fixes: a tenant could previously be
// created with its audit-record write silently failing in a separate
// transaction.
func CreateTenant(ctx context.Context, tx pgx.Tx, name, slug, licensingModel string) (Tenant, error) {
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
