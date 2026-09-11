package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TxFunc is application code that runs inside a tenant-scoped
// transaction. It receives the transaction, not the pool, so it cannot
// accidentally issue a query outside the tenant-scoped connection.
type TxFunc func(ctx context.Context, tx pgx.Tx) error

// WithTenant is the ONLY sanctioned way to run a tenant-scoped query.
// It opens a transaction, sets the Postgres session variable
// "app.tenant_id" for the lifetime of that transaction via
// set_config(..., true) (the "true" argument scopes it to the current
// transaction, equivalent to SET LOCAL, and - unlike SET LOCAL itself -
// set_config is a normal function call that accepts a bound parameter,
// so the tenant id is never interpolated into SQL text), runs fn, and
// commits or rolls back.
//
// Every tenant-owned table's row-level security policy is expected to
// check current_setting('app.tenant_id', true)::uuid = tenant_id. This
// function is the single place that sets that value - no other code path
// in the platform may set app.tenant_id, and callers never pass a
// tenant id that didn't come from the verified JWT (see internal/tenant).
func (p *Pool) WithTenant(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("db: WithTenant called with nil tenant id")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op if already committed

	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("db: set tenant context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err // deferred Rollback cleans up
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

// WithoutTenant runs fn in a plain transaction with no tenant context
// set. Row-level-security policies on tenant-owned tables will therefore
// see current_setting('app.tenant_id', true) as empty and deny access -
// this is intentional. It exists only for platform-level operations that
// are genuinely not tenant-scoped (e.g. reading the jurisdiction/asset
// registries, which are platform-wide reference data, not per-tenant).
func (p *Pool) WithoutTenant(ctx context.Context, fn TxFunc) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
