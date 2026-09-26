//go:build integration

// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091) C7 statement-capture harness -
// modeled byte-for-byte on internal/payments/webhook_no_write_before_
// verification_integration_test.go's own recordingTx (see that file's doc
// comment for the full N4 rationale on wrapping Begin/SendBatch/CopyFrom
// too) and internal/kyc/recording_tx_integration_test.go's identical
// casino-package-free copy. Kept as its own file, one per package, per
// this repo's existing per-package test-helper convention (recordingTx
// already exists independently in internal/payments and internal/kyc).
package casino

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type sqlRecorder struct {
	mu         sync.Mutex
	statements []string
}

func (rec *sqlRecorder) record(sql string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.statements = append(rec.statements, sql)
}

func (rec *sqlRecorder) Statements() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.statements))
	copy(out, rec.statements)
	return out
}

type recordingTx struct {
	pgx.Tx
	rec *sqlRecorder
}

func newRecordingTx(tx pgx.Tx) *recordingTx {
	return &recordingTx{Tx: tx, rec: &sqlRecorder{}}
}

func (r *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.rec.record(sql)
	return r.Tx.Exec(ctx, sql, args...)
}

func (r *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.rec.record(sql)
	return r.Tx.Query(ctx, sql, args...)
}

func (r *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.rec.record(sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

func (r *recordingTx) Begin(ctx context.Context) (pgx.Tx, error) {
	r.rec.record("-- recordingTx.Begin: savepoint opened")
	child, err := r.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingTx{Tx: child, rec: r.rec}, nil
}

const sendBatchOrCopyMarker = "-- recordingTx: "

func (r *recordingTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	r.rec.record(sendBatchOrCopyMarker + "SendBatch called (opaque batch of statements, not individually recorded)")
	return r.Tx.SendBatch(ctx, b)
}

func (r *recordingTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	r.rec.record(sendBatchOrCopyMarker + "CopyFrom called: " + tableName.Sanitize())
	return r.Tx.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

func (r *recordingTx) Statements() []string {
	return r.rec.Statements()
}
