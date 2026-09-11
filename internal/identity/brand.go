// Package identity is the domain layer for Person, PlayerAccount,
// StaffUser, Brand, and login-attempt tracking - the Stage 2 identity
// model. See docs/architecture/05-identity-architecture.md and
// docs/decisions/0012-brand-distinct-from-tenant.md for why these are
// kept as distinct types rather than collapsed for convenience.
package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

var ErrNotFound = errors.New("identity: not found")

// Brand is the consumer-facing product a player registers/logs into.
// Distinct from Tenant - see package doc.
type Brand struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Name     string
	Slug     string
	Status   string
}

// GetBrandBySlug resolves a brand by its public slug. This is the
// pre-authentication tenant-resolution step: brand identity is public
// data (see migration 0008's RLS policy), so this reads via
// db.WithoutTenant - there is no tenant context yet at this point in a
// registration or login flow, and there cannot be, since which tenant is
// involved is exactly what this call determines. See
// docs/security/security-architecture.md's "Pre-authentication tenant
// resolution" section for why this is not a violation of "never trust a
// client-supplied tenant id": the caller supplies a public brand
// identifier, not an assertion of authorization.
func GetBrandBySlug(ctx context.Context, pool *db.Pool, slug string) (Brand, error) {
	var b Brand
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, slug, status FROM brands WHERE slug = $1`,
			slug,
		).Scan(&b.ID, &b.TenantID, &b.Name, &b.Slug, &b.Status)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Brand{}, ErrNotFound
	}
	if err != nil {
		return Brand{}, fmt.Errorf("identity: get brand by slug: %w", err)
	}
	return b, nil
}

// CreateBrand creates a brand under tenantID. Called from an already
// tenant-scoped transaction (the caller - an admin endpoint gated by
// PermBrandWrite - resolves and authorizes tenantID first).
func CreateBrand(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name, slug string) (Brand, error) {
	b := Brand{ID: uuid.New(), TenantID: tenantID, Name: name, Slug: slug, Status: "active"}
	_, err := tx.Exec(ctx,
		`INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, $5)`,
		b.ID, b.TenantID, b.Name, b.Slug, b.Status,
	)
	if err != nil {
		return Brand{}, fmt.Errorf("identity: create brand: %w", err)
	}
	return b, nil
}
