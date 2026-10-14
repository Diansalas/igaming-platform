//go:build integration

// Package staffsv (integration builds ONLY) lets domain test fixtures that build a verified-token context by hand
// (tenant.Context) carry the actor's CURRENT staff_users.staff_version as the token's `sv` claim (ADR 0119 S5 + S6,
// migration 0139). A fixture registers the pool a staff row lives in when it creates the row; Version reads the row
// at the moment the context is built, exactly like a token minted at that moment.
package staffsv

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type entry struct {
	pool   *db.Pool
	tenant uuid.UUID
}

var registry sync.Map // uuid.UUID -> entry

// Register records that staffID lives in pool, in tenantID's scope (uuid.Nil: platform).
func Register(pool *db.Pool, staffID, tenantID uuid.UUID) {
	registry.Store(staffID, entry{pool: pool, tenant: tenantID})
}

// Version returns staffID's current staff_version; 1 (the initial version) for an unregistered or unreadable row.
func Version(staffID uuid.UUID) int64 {
	v, ok := registry.Load(staffID)
	if !ok {
		return 1
	}
	e := v.(entry)
	var sv int64 = 1
	fn := func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE((SELECT staff_version FROM staff_users WHERE id = $1), 1)`, staffID).Scan(&sv)
	}
	var err error
	if e.tenant != uuid.Nil {
		err = e.pool.WithTenant(context.Background(), e.tenant, fn)
	} else {
		err = e.pool.WithoutTenant(context.Background(), fn)
	}
	if err != nil {
		return 1
	}
	return sv
}

// personRequiredRoles mirrors migration 0139's SF014 role set (ADR 0119 section 9.4).
var personRequiredRoles = map[string]bool{"platform_admin": true, "tenant_admin": true, "finance": true, "compliance": true}

// PersonIfRequired creates a fresh Person inside tx and returns its id when role is one SF014 requires a Person for,
// and nil otherwise: the fixture form of "an approve-capable staff member is linked to a Person".
func PersonIfRequired(ctx context.Context, tx pgx.Tx, role string) (*uuid.UUID, error) {
	if !personRequiredRoles[role] {
		return nil, nil
	}
	id := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, id); err != nil {
		return nil, err
	}
	return &id, nil
}
