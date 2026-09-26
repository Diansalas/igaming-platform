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
	return p.withTenantTx(ctx, tenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, fn)
}

// WithTenantReadOnly is WithTenant with the transaction opened READ ONLY
// (ADR 0094 §4.1). PostgreSQL refuses every write, nextval and
// SELECT ... FOR UPDATE/SHARE in it; it does NOT refuse advisory locks or
// read-only function calls, so it is defence in depth, never a substitute
// for statement-level review. The tenant scoping is identical to
// WithTenant's (set_config is permitted in a read-only transaction).
//
// It exists for the webhook pre-verification reads (the payments
// capability EXISTS and the provider-credential handle read), which run in
// their own short transaction that COMMITS before any secret-store fetch,
// so no pooled connection is ever held while waiting on the store
// (INV-POOL).
func (p *Pool) WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("db: WithTenantReadOnly called with nil tenant id")
	}
	return p.withTenantTx(ctx, tenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}
