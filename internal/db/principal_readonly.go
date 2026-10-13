package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// QueryRowPrincipalScopeReadOnly runs exactly ONE query in a READ ONLY
// transaction scoped like WithPrincipalScope (app.tenant_id when tenantID is
// not uuid.Nil, platform scope otherwise; app.principal_id = principalID),
// with statement_timeout set for the transaction only. The timeout is set
// FIRST, in its own round trip: a pgx batch sends the query's Parse before the
// batch's set_config statements execute, and Parse already takes the table
// locks, so a timeout set inside the same batch would not bound a query whose
// statement is not yet cached on the connection (found by the S4a fault test).
// The scope GUCs and the query then go as ONE pgx batch, the row is scanned,
// and the transaction COMMITS and releases its connection before this function
// returns: the caller never holds a pooled connection afterwards, so a later
// acquisition by the same request cannot deadlock the pool (ADR 0119 section
// 7.3, review N1: the per-request staff liveness check).
//
// scan receives the query's row; "no row" is returned as pgx.ErrNoRows, so
// callers can tell it from a fault with errors.Is. Any other error
// (connection, timeout, cancellation, scan, a failed batch) is a fault the
// caller must treat as fail-closed, and never matches pgx.ErrNoRows.
// statementTimeout must be between 1 ms and 10 s.
func (p *Pool) QueryRowPrincipalScopeReadOnly(ctx context.Context, tenantID, principalID uuid.UUID, statementTimeout time.Duration,
	query string, args []any, scan func(pgx.Row) error) error {
	if principalID == uuid.Nil {
		return fmt.Errorf("db: QueryRowPrincipalScopeReadOnly called with nil principal id")
	}
	if statementTimeout < time.Millisecond || statementTimeout > 10*time.Second {
		return fmt.Errorf("db: QueryRowPrincipalScopeReadOnly statement timeout %s out of range", statementTimeout)
	}
	if scan == nil {
		return fmt.Errorf("db: QueryRowPrincipalScopeReadOnly called without a scan function")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("db: begin read-only tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, strconv.FormatInt(statementTimeout.Milliseconds(), 10)+"ms"); err != nil {
		return fmt.Errorf("db: set read-only statement timeout: %w", err)
	}
	b := &pgx.Batch{}
	if tenantID != uuid.Nil {
		b.Queue(`SELECT set_config('app.tenant_id', $1, true)`, tenantID.String())
	}
	b.Queue(`SELECT set_config('app.principal_id', $1, true)`, principalID.String())
	b.Queue(query, args...)
	setters := b.Len() - 1

	br := tx.SendBatch(ctx, b)
	for i := 0; i < setters; i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("db: set read-only principal scope: %w", err)
		}
	}
	scanErr := scan(br.QueryRow())
	closeErr := br.Close()
	if scanErr != nil {
		if closeErr != nil {
			// A fault: never reported as "no row" (scanErr is not wrapped, so errors.Is(err, pgx.ErrNoRows) is false).
			return fmt.Errorf("db: read-only principal-scope batch: %v; %w", scanErr, closeErr)
		}
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return pgx.ErrNoRows
		}
		return fmt.Errorf("db: read-only principal-scope query: %w", scanErr)
	}
	if closeErr != nil {
		return fmt.Errorf("db: read-only principal-scope batch: %w", closeErr)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit read-only tx: %w", err)
	}
	return nil
}
