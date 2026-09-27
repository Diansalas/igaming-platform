//go:build integration

// Migration 0104 (PRH-I5 security C2, database half): charset CHECKs on
// payment_statement_lines.merchant_reference and asset_code, with a
// pre-flight that counts violating rows and changes nothing. Scratch
// databases only; the version is derived from the filename, so the (possibly
// still open) 0103 gap does not matter.
package reconciliation

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func migration0104Version(t *testing.T) int64 {
	t.Helper()
	entries, err := os.ReadDir(payMigrationsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_payment_statement_line_charset.up.sql") {
			v, err := strconv.ParseInt(e.Name()[:4], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatal("migration payment_statement_line_charset not found")
	return 0
}

// payMigrationsCopyThrough copies every migration file with version <=
// through into a fresh directory.
func payMigrationsCopyThrough(t *testing.T, through int64) string {
	t.Helper()
	src := payMigrationsDir(t)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".sql") || len(n) < 4 {
			continue
		}
		v, err := strconv.ParseInt(n[:4], 10, 64)
		if err != nil || v > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// insertStatementLine stores one import with one line carrying merchant /
// asset directly through SQL (never through the Go validation).
func insertStatementLine(pool *db.Pool, tenantID uuid.UUID, merchantSQL, assetSQL string, args ...any) error {
	return pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			WITH i AS (INSERT INTO payment_statement_imports (id, tenant_id, provider_id, source_label, is_mock, coverage_start, coverage_end, line_count, content_digest, fetched_at)
			           VALUES ($1::uuid, $2::uuid, 'p', 'MOCK x', true, now() - interval '1 hour', now(), 1, decode(md5($1::uuid::text) || md5($1::uuid::text), 'hex'), now())
			           RETURNING id, tenant_id)
			INSERT INTO payment_statement_lines (tenant_id, import_id, line_no, provider_id, kind, provider_reference, merchant_reference, status, amount, asset_code, occurred_at)
			SELECT tenant_id, id, 0, 'p', 'deposit', 'r', `+merchantSQL+`, 'pending', 1, `+assetSQL+`, now() FROM i`,
			append([]any{uuid.New(), tenantID}, args...)...)
		return err
	})
}

var charsetCases = []struct {
	name, merchantSQL, assetSQL string
	args                        []any
	acceptedBy0102              bool
}{
	{"merchant_nul", "'m' || chr(0)", "'EUR'", nil, false}, // text cannot hold NUL at all
	{"merchant_newline", "$3::text", "'EUR'", []any{"m\nINJECT"}, true},
	{"merchant_escape", "$3::text", "'EUR'", []any{string(rune(0x1b)) + "[31m"}, true},
	{"merchant_c1", "$3::text", "'EUR'", []any{"m" + string(rune(0x85))}, true},
	{"merchant_del", "$3::text", "'EUR'", []any{"m" + string(rune(0x7f))}, true},
	{"asset_lowercase", "'m1'", "$3::text", []any{"eur"}, true},
	{"asset_formula", "'m1'", "$3::text", []any{"=EUR"}, true},
	{"asset_space", "'m1'", "$3::text", []any{"EU R"}, true},
}

func TestMigration0104_ChecksRefuseBadCharsetDirectly(t *testing.T) {
	v := migration0104Version(t)
	pool, _ := payScratchThrough(t, v)
	f := seedFixture(t, pool)
	if err := insertStatementLine(pool, f.tenantID, "'pa:0f1e-merchant'", "'USDT'"); err != nil {
		t.Fatalf("a valid line must be accepted: %v", err)
	}
	if err := insertStatementLine(pool, f.tenantID, "NULL", "'BTC'"); err != nil {
		t.Fatalf("a NULL merchant reference must be accepted: %v", err)
	}
	for _, c := range charsetCases {
		err := insertStatementLine(pool, f.tenantID, c.merchantSQL, c.assetSQL, c.args...)
		if err == nil {
			t.Errorf("%s: expected the database to refuse", c.name)
			continue
		}
		if c.acceptedBy0102 && !strings.Contains(err.Error(), "payment_statement_lines_merchant_reference_charset") &&
			!strings.Contains(err.Error(), "payment_statement_lines_asset_code_shape") {
			t.Errorf("%s: refused, but not by a 0104 CHECK: %v", c.name, err)
		}
	}
}

// Kill control: without 0104 the same rows are accepted, so the refusals
// above are 0104's doing.
func TestMigration0104_KillControl_0102AcceptsThem(t *testing.T) {
	pool, _ := payScratchThrough(t, migration0102Version(t))
	f := seedFixture(t, pool)
	for _, c := range charsetCases {
		if !c.acceptedBy0102 {
			continue
		}
		if err := insertStatementLine(pool, f.tenantID, c.merchantSQL, c.assetSQL, c.args...); err != nil {
			t.Errorf("%s: 0102 alone was expected to accept this row, got %v", c.name, err)
		}
	}
}

func TestMigration0104_UpDownUpRoundTrip(t *testing.T) {
	v := migration0104Version(t)
	pool, dir := payScratchThrough(t, v)
	count := func() int {
		var n int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname IN
				('payment_statement_lines_merchant_reference_charset', 'payment_statement_lines_asset_code_shape')`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 2 {
		t.Fatal("up must add both CHECKs")
	}
	if rolled, err := pool.MigrateDown(context.Background(), dir, 1); err != nil || len(rolled) != 1 || rolled[0] != v {
		t.Fatalf("down: %v %v", rolled, err)
	}
	if count() != 0 {
		t.Fatal("down must drop both CHECKs")
	}
	if applied, err := pool.MigrateUp(context.Background(), dir); err != nil || len(applied) != 1 || applied[0] != v {
		t.Fatalf("up again: %v %v", applied, err)
	}
	if count() != 2 {
		t.Fatal("up again must re-add both CHECKs")
	}
}

// The pre-flight counts violating rows (through RLS, per tenant) and aborts
// before adding anything; the rows are left exactly as they were.
func TestMigration0104_PreflightCountsAndChangesNothing(t *testing.T) {
	pool, _ := payScratchThrough(t, migration0102Version(t))
	f := seedFixture(t, pool)
	if err := insertStatementLine(pool, f.tenantID, "$3::text", "$4::text", "m\nx", "eur"); err != nil {
		t.Fatal(err)
	}
	if err := insertStatementLine(pool, f.tenantID, "'ok'", "$3::text", "=EUR"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateUp(context.Background(), payMigrationsCopyThrough(t, migration0104Version(t)))
	if err == nil || !strings.Contains(err.Error(), "merchant_reference=1 asset_code=2") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("expected the pre-flight to refuse with merchant_reference=1 asset_code=2, got %v", err)
	}
	var n int
	var constraints int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_statement_lines WHERE tenant_id = $1
			AND (merchant_reference = E'm\nx' OR asset_code IN ('eur', '=EUR'))`, f.tenantID).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname LIKE 'payment_statement_lines_%_charset' OR conname = 'payment_statement_lines_asset_code_shape'`).Scan(&constraints)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 || constraints != 0 {
		t.Fatalf("pre-flight must change nothing: violating rows=%d (want 2), constraints=%d (want 0)", n, constraints)
	}
}
