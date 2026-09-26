//go:build integration

// Package phasecapture is TEST SUPPORT for ADR 0094 (secret-resolution
// resource isolation): it records what each phase of a two-phase webhook
// callback runs against the database.
//
//   - Reader is the webhookauth.TenantReader handed to phase 1
//     (VerifyCallback). It wraps a *db.Pool: every pre-verification
//     transaction is recorded - its statements, whether PostgreSQL
//     reports it READ ONLY, and how long fn held it.
//   - Tx wraps phase 2's domain transaction and records its statements.
//   - Pool10 connects the ADR 0094 §9.3 shared fixture pool at the
//     production default size (10), asserting that default so a change
//     to it forces the ADR to be revisited (QA §11 items 1 and 8).
package phasecapture

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// PoolSize is ADR 0094's production pool size.
const PoolSize = 10

// Pool10 connects to the URL in env (skipping when unset) with exactly
// PoolSize connections, after asserting config.DefaultDatabaseMaxConns ==
// PoolSize.
func Pool10(t testing.TB, env string) *db.Pool {
	t.Helper()
	pool, _ := Pool10Named(t, env)
	return pool
}

// Pool10Named is Pool10 with a unique PostgreSQL application_name on every
// connection of the pool, returned so XactAgeSampler can watch exactly
// this pool's transactions in pg_stat_activity.
func Pool10Named(t testing.TB, env string) (*db.Pool, string) {
	t.Helper()
	if config.DefaultDatabaseMaxConns != PoolSize {
		t.Fatalf("config.DefaultDatabaseMaxConns = %d: ADR 0094's resource-allocation rationale assumes %d - revisit the ADR",
			config.DefaultDatabaseMaxConns, PoolSize)
	}
	url := os.Getenv(env)
	if url == "" {
		t.Skipf("%s not set; skipping integration test", env)
	}
	name := "adr0094-" + uuid.NewString()[:8]
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := db.Connect(context.Background(), url+sep+"application_name="+name, PoolSize, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, name
}

// XactAgeSampler watches pg_stat_activity, on its own dedicated connection
// outside the pool under test, for the oldest open transaction of the
// sessions named appName (Pool10Named). A pooled connection held across a
// secret-store wait shows up as a long-lived (idle in) transaction - the
// F-POOL-1 symptom - so MaxAge is ADR 0094's "no transaction held longer
// than longSlack" criterion measured on the real handler path.
type XactAgeSampler struct {
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	maxAge time.Duration
	err    error
}

// StartXactAgeSampler starts sampling every 5 ms until Stop.
func StartXactAgeSampler(t testing.TB, env, appName string) *XactAgeSampler {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv(env))
	if err != nil {
		t.Fatalf("sampler connect: %v", err)
	}
	s := &XactAgeSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer func() { _ = conn.Close(context.Background()) }()
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-tk.C:
			}
			var ms float64
			err := conn.QueryRow(context.Background(), `SELECT COALESCE(EXTRACT(EPOCH FROM max(clock_timestamp() - xact_start)) * 1000, 0)::float8
				FROM pg_stat_activity WHERE application_name = $1 AND xact_start IS NOT NULL`, appName).Scan(&ms)
			s.mu.Lock()
			if err != nil && s.err == nil {
				s.err = err
			}
			if d := time.Duration(ms * float64(time.Millisecond)); d > s.maxAge {
				s.maxAge = d
			}
			s.mu.Unlock()
		}
	}()
	t.Cleanup(s.Stop)
	return s
}

// Stop ends sampling (idempotent).
func (s *XactAgeSampler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

// MaxAge is the oldest open transaction age observed, and any sampling
// error.
func (s *XactAgeSampler) MaxAge() (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxAge, s.err
}

// Transaction is one recorded pre-verification transaction.
type Transaction struct {
	Statements []string
	ReadOnly   bool
	Held       time.Duration
}

// Reader records every pre-verification transaction phase 1 runs.
type Reader struct {
	Pool *db.Pool

	mu  sync.Mutex
	txs []Transaction
}

// NewReader wraps pool.
func NewReader(pool *db.Pool) *Reader { return &Reader{Pool: pool} }

// WithTenantReadOnly implements webhookauth.TenantReader.
func (r *Reader) WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error {
	return r.Pool.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var ro string
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_read_only')`).Scan(&ro); err != nil {
			return err
		}
		rec := NewTx(tx)
		start := time.Now()
		err := fn(ctx, rec)
		held := time.Since(start)
		r.mu.Lock()
		r.txs = append(r.txs, Transaction{Statements: rec.Statements(), ReadOnly: ro == "on", Held: held})
		r.mu.Unlock()
		return err
	})
}

// Transactions returns every recorded transaction, in order.
func (r *Reader) Transactions() []Transaction {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Transaction(nil), r.txs...)
}

// Tx records every statement run through a transaction.
type Tx struct {
	pgx.Tx
	mu    sync.Mutex
	stmts []string
}

// NewTx wraps tx.
func NewTx(tx pgx.Tx) *Tx { return &Tx{Tx: tx} }

func (r *Tx) record(sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, sql)
}

// Exec records and runs sql.
func (r *Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.record(sql)
	return r.Tx.Exec(ctx, sql, args...)
}

// Query records and runs sql.
func (r *Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.record(sql)
	return r.Tx.Query(ctx, sql, args...)
}

// QueryRow records and runs sql.
func (r *Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.record(sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

// SendBatch records every queued statement and runs the batch.
func (r *Tx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	for _, q := range b.QueuedQueries {
		r.record(q.SQL)
	}
	return r.Tx.SendBatch(ctx, b)
}

// Statements returns every recorded statement, in order.
func (r *Tx) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}
