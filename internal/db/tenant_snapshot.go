package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WithTenantSnapshot is WithTenant with the transaction opened at
// REPEATABLE READ, so every statement fn issues reads ONE snapshot (taken
// at the first statement, the tenant set_config below). The tenant scoping
// is identical to WithTenant's: app.tenant_id is set for the lifetime of
// the transaction via set_config(..., true), and nothing else is set.
//
// It exists for read-mostly jobs that must compare the results of several
// statements with each other and therefore cannot tolerate a commit landing
// between two of them - the casino_statement reconciliation stream (Stage
// 10.3 W3a, CAS-RECON-STMT-1) reads the statement source and the ledger in
// separate statements, and under READ COMMITTED a casino posting committed
// between the two would surface as a false P1 mismatch. Such a job should
// only INSERT (never UPDATE/DELETE a row another transaction may touch), so
// it cannot hit a serialization failure.
func (p *Pool) WithTenantSnapshot(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("db: WithTenantSnapshot called with nil tenant id")
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return fmt.Errorf("db: begin repeatable-read tx: %w", err)
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
