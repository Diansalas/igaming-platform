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

// errSlugTaken wraps a unique-violation error from a slug-conflicting
// insert into ErrSlugTaken, or passes through a wrapped generic error
// otherwise. Shared by CreateTenant and CreateBrand.
func errSlugTaken(op string, err error) error {
	if db.IsUniqueViolation(err) {
		return ErrSlugTaken
	}
	return fmt.Errorf("identity: %s: %w", op, err)
}

var ErrNotFound = errors.New("identity: not found")

// StatusPendingLaunch is the status every new tenant and brand starts in (ADR 0112,
// migration 0128). It is refused by every runtime gate that tests status = 'active'.
const StatusPendingLaunch = "pending_launch"

// ErrNotAcceptingRegistrations is returned by the player registration entry points when
// the brand or its tenant is not active (ADR 0112 section 7.3). HTTP answers 409
// BRAND_NOT_ACCEPTING_REGISTRATIONS.
// Unlaunched reports whether the brand or its tenant is still pending_launch (only meaningful
// for a Brand returned by GetBrandBySlug). An unlaunched brand is hidden: registration and login
// answer exactly as for an unknown brand slug (ADR 0112 7.3, security S-5/S-6).
func (b Brand) Unlaunched() bool {
	return b.Status == StatusPendingLaunch || b.TenantStatus == StatusPendingLaunch
}

var ErrNotAcceptingRegistrations = errors.New("identity: the brand is not accepting registrations")

// ErrSlugTaken is returned when a tenant or brand slug collides with an
// existing row's UNIQUE constraint - a plain client input conflict, not
// a server fault (see db.IsUniqueViolation).
var ErrSlugTaken = errors.New("identity: this slug is already in use")

// Brand is the consumer-facing product a player registers/logs into.
// Distinct from Tenant - see package doc.
type Brand struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Name     string
	Slug     string
	Status   string
	// TenantStatus is the owning tenant's status. Populated ONLY by GetBrandBySlug (the
	// pre-authentication lookup) so the register / login handlers can hide an unlaunched
	// (pending_launch) brand or tenant exactly like an unknown brand (ADR 0112 7.3).
	TenantStatus string
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
			`SELECT b.id, b.tenant_id, b.name, b.slug, b.status, t.status
			   FROM brands b JOIN tenants t ON t.id = b.tenant_id WHERE b.slug = $1`,
			slug,
		).Scan(&b.ID, &b.TenantID, &b.Name, &b.Slug, &b.Status, &b.TenantStatus)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Brand{}, ErrNotFound
	}
	if err != nil {
		return Brand{}, fmt.Errorf("identity: get brand by slug: %w", err)
	}
	return b, nil
}

// GetBrandByID resolves a brand by id within an already-open transaction -
// the admin single-brand-detail counterpart to GetBrandBySlug (used
// pre-authentication, with no tenant context). Since brand_public_read is
// USING (true) (migration 0008), this resolves regardless of tx's own
// tenant scope - callers that must confine a caller to one specific tenant
// (e.g. the admin GET /v1/admin/tenants/{tenantID}/brands/{brandID} route)
// verify the returned TenantID against the server-derived target tenant
// themselves, exactly like GetTenantByID's own "id must be a server-derived
// value" discipline; id must never be trusted to already belong to the
// right tenant just because it parsed as a UUID.
func GetBrandByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Brand, error) {
	var b Brand
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, name, slug, status FROM brands WHERE id = $1`,
		id,
	).Scan(&b.ID, &b.TenantID, &b.Name, &b.Slug, &b.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Brand{}, ErrNotFound
	}
	if err != nil {
		return Brand{}, fmt.Errorf("identity: get brand by id: %w", err)
	}
	return b, nil
}

// ListBrandsForTenant returns a page of brands belonging to tenantID, for
// the admin back-office brand list, ordered by name for stable pagination.
// tenantID must already be a server-derived value the caller has authorized
// (see canActOnTenant in internal/httpserver) - this function does not
// re-check authorization, only tenant membership of the returned rows.
// total is the count of ALL of this tenant's brands matching no filter
// (this endpoint takes none per this stage's scope), computed via
// count(*) OVER() in the same query so it can never drift from what was
// actually read.
func ListBrandsForTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit, offset int) ([]Brand, int, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, name, slug, status, count(*) OVER()
		 FROM brands WHERE tenant_id = $1 ORDER BY name ASC, id ASC LIMIT $2 OFFSET $3`,
		tenantID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("identity: list brands for tenant: %w", err)
	}
	defer rows.Close()

	var brands []Brand
	var total int
	for rows.Next() {
		var b Brand
		if err := rows.Scan(&b.ID, &b.TenantID, &b.Name, &b.Slug, &b.Status, &total); err != nil {
			return nil, 0, fmt.Errorf("identity: scan brand: %w", err)
		}
		brands = append(brands, b)
	}
	return brands, total, rows.Err()
}

// CreateBrand creates a brand under tenantID. Called from an already
// tenant-scoped transaction (the caller - an admin endpoint gated by
// PermBrandWrite - resolves and authorizes tenantID first).
//
// ADR 0112 (migration 0128): creation is not launch. The brand is created
// StatusPendingLaunch and only a governed, audited launch decision can make it
// active; the database refuses any other status from the runtime role (LA021).
func CreateBrand(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name, slug string) (Brand, error) {
	b := Brand{ID: uuid.New(), TenantID: tenantID, Name: name, Slug: slug, Status: StatusPendingLaunch}
	_, err := tx.Exec(ctx,
		`INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, $5)`,
		b.ID, b.TenantID, b.Name, b.Slug, b.Status,
	)
	if err != nil {
		return Brand{}, errSlugTaken("create brand", err)
	}
	return b, nil
}
