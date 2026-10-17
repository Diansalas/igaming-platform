//go:build integration

// F2 fix round, migration 0168 (the payment_statement run seal) on a THROWAWAY scratch database: a whole-schema
// up/down/up snapshot, the column CHECKs and the same-tenant foreign key, and the fail-loud down while a sealed run
// exists.
package reconciliation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const migration0168Version = 168

func TestMigration0168_UpDownUp_Constraints_DownRefusesSealedRuns(t *testing.T) {
	ctx := context.Background()
	pool, dir := payScratchThrough(t, migration0168Version)
	snapUp := m0134Snapshot(t, pool)
	if rolled, err := pool.MigrateDown(ctx, dir, 1); err != nil || len(rolled) != 1 || rolled[0] != migration0168Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	if strings.Contains(m0134Snapshot(t, pool), "run_seal") {
		t.Fatal("down must remove the run seal columns")
	}
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got := m0134Snapshot(t, pool); got != snapUp {
		t.Fatal("up after down differs from the first up")
	}

	f := seedFixture(t, pool)
	other := seedFixture(t, pool)
	importIn := func(tenant uuid.UUID) uuid.UUID {
		id := uuid.New()
		if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_statement_imports
				(id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
				VALUES ($1, $2, 'p-0168', 'MOCK 0168', true, now() - interval '1 hour', now(), 0, $3, now())`, id, tenant, make([]byte, 32))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mine, theirs := importIn(f.tenantID), importIn(other.tenantID)
	insert := func(importID any, seal, kid any) error {
		return pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end, provider_id, window_truncated,
				run_import_id, run_seal, run_seal_kid)
				VALUES ($1, 'payment_statement', now() - interval '1 hour', now(), 'clean', now(), 'p-0168', false, $2, $3, $4)`, f.tenantID, importID, seal, kid)
			return err
		})
	}
	hex64 := strings.Repeat("0a", 32)
	for name, c := range map[string]struct {
		imp, seal, kid any
		code           string
	}{
		"seal without kid":        {mine, hex64, nil, "23514"},
		"seal without import":     {nil, hex64, "k1", "23514"},
		"malformed seal":          {mine, "XYZ", "k1", "23514"},
		"malformed kid":           {mine, hex64, "bad kid!", "23514"},
		"another tenant's import": {theirs, hex64, "k1", "23503"},
	} {
		var pe *pgconn.PgError
		if err := insert(c.imp, c.seal, c.kid); !errors.As(err, &pe) || pe.Code != c.code {
			t.Fatalf("%s: want %s, got %v", name, c.code, err)
		}
	}
	if err := insert(mine, hex64, "k1"); err != nil {
		t.Fatalf("a well-shaped sealed run is storable (its seal is verified in Go, never by the database): %v", err)
	}
	if err := insert(nil, nil, nil); err != nil {
		t.Fatalf("an unsealed run stays storable: %v", err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "0168 down refused") {
		t.Fatalf("down with a sealed run must refuse, got %v", err)
	}
	if got := m0134Snapshot(t, pool); got != snapUp {
		t.Fatal("a refused down must change nothing")
	}
	_ = time.Now
}
