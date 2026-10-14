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
