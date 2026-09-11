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

// CreateTenant provisions a new tenant. Platform-admin-only (see
// PermTenantWrite) - tenant provisioning is inherently a platform-level
// action, never tenant-scoped, so this runs via WithoutTenant.
func CreateTenant(ctx context.Context, pool *db.Pool, name, slug, licensingModel string) (Tenant, error) {
	t := Tenant{ID: uuid.New(), Name: name, Slug: slug, LicensingModel: licensingModel, Status: "active"}
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, $4)`,
			t.ID, t.Name, t.Slug, t.LicensingModel,
		)
		return err
	})
	if err != nil {
		return Tenant{}, fmt.Errorf("identity: create tenant: %w", err)
	}
	return t, nil
}
