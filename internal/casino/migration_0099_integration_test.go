//go:build integration

// PRH-REF (PROVIDER-REF-BOUND-1), migration 0099 - on scratch databases
// only (DDL never touches the shared test database):
//   - up adds every *_ref_bound CHECK; the bound is enforced by the
//     database itself (256 bytes / a control character refused with
//     SQLSTATE 23514, 255 bytes accepted);
//   - down drops them all and up re-applies them (round trip);
//   - the pre-flight FAILS LOUDLY with a per-column count when existing
//     rows violate the bound - including a row in a FORCE RLS tenant table
//     (so the count is not RLS-blind) and one in a platform catalogue
//     table - and never modifies or deletes the offending rows.
package casino

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0099Version = int64(99)

// migration0099Dir copies the migrations up to and including through
// (nothing later), so a scratch database can stop at 0098 or 0099.
func migration0099Dir(t *testing.T, through int64) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || len(name) < 4 {
			continue
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil {
			t.Fatalf("migration filename %q has no numeric version prefix", name)
		}
		if int64(v) > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func migration0099Scratch(t *testing.T, prefix string, through int64) (*db.Pool, string) {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	dir := migration0099Dir(t, through)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", through, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != through {
		t.Fatalf("expected %d to be the last applied migration, got %v", through, applied)
	}
	return pool, dir
}

func refBoundConstraintCount(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'c' AND conname LIKE '%\_ref\_bound'`).Scan(&n)
	}); err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	return n
}

// wantRefBoundConstraints is the number of CHECKs 0099 adds (one per
// bounded column; see the migration's pre-flight column lists).
const wantRefBoundConstraints = 27

func insertRejectionRow(t *testing.T, pool *db.Pool, f casinoFixture, providerTxID string) error {
	t.Helper()
	return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordCallbackRejection(ctx, tx, f.tenantID, "mock-casino",
			CallbackRejection{Class: RejectionBetNotFound, EventType: CallbackEventWin, ProviderTxID: providerTxID, AssetCode: "EUR", Amount: 1}, "")
		return err
	})
}

func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == constraint
}

func TestMigration0099_UpEnforcesBoundDownDropsItRoundTrip(t *testing.T) {
	pool, dir := migration0099Scratch(t, "m0099rt_", migration0099Version)
	if got := refBoundConstraintCount(t, pool); got != wantRefBoundConstraints {
		t.Fatalf("after up: %d *_ref_bound constraints, want %d", got, wantRefBoundConstraints)
	}
	f := seedCasinoFixture(t, pool)

	// The database enforces the bound on its own (defense in depth behind
	// internal/providerref): 256 bytes and a control character refused.
	if err := insertRejectionRow(t, pool, f, strings.Repeat("a", 256)); !isCheckViolation(err, "casino_callback_rejections_provider_tx_id_ref_bound") {
		t.Fatalf("256-byte provider_tx_id must violate the CHECK, got %v", err)
	}
	if err := insertRejectionRow(t, pool, f, strings.Repeat("é", 128)); !isCheckViolation(err, "casino_callback_rejections_provider_tx_id_ref_bound") {
		t.Fatalf("256-byte (128-rune) provider_tx_id must violate the CHECK, got %v", err)
	}
	if err := insertRejectionRow(t, pool, f, "ref\u0085c1"); !isCheckViolation(err, "casino_callback_rejections_provider_tx_id_ref_bound") {
		t.Fatalf("a C1 control character must violate the CHECK, got %v", err)
	}
	if err := insertRejectionRow(t, pool, f, strings.Repeat("a", 255)); err != nil {
		t.Fatalf("255-byte provider_tx_id must be accepted: %v", err)
	}
	// Code review F3: pin both edges of the SQL control-character range and
	// the minimum length, so a changed SQL literal (e.g. [\x80-\x9F] or
	// [\x7F-\xA0], or BETWEEN 0 AND 255) fails a test. The Go rule and the
	// SQL rule are independent literals; this is what proves their parity.
	if err := insertRejectionRow(t, pool, f, "ref\u007fdel"); !isCheckViolation(err, "casino_callback_rejections_provider_tx_id_ref_bound") {
		t.Fatalf("DEL (U+007F, lower edge of the DEL/C1 range) must violate the CHECK, got %v", err)
	}
	if err := insertRejectionRow(t, pool, f, "ref\u009fc1"); !isCheckViolation(err, "casino_callback_rejections_provider_tx_id_ref_bound") {
		t.Fatalf("U+009F (upper edge of the C1 range) must violate the CHECK, got %v", err)
	}
	if err := insertRejectionRow(t, pool, f, "ref nbsp"); err != nil {
		t.Fatalf("U+00A0 (first code point after the C1 range) must be accepted: %v", err)
	}
	if err := insertRejectionRow(t, pool, f, "ref\x1fus"); !isCheckViolation(err, "casino_callback_rejections_provider_tx_id_ref_bound") {
		t.Fatalf("U+001F (upper edge of the C0 range) must violate the CHECK, got %v", err)
	}
	if err := insertRejectionRow(t, pool, f, "ref space~tilde"); err != nil {
		t.Fatalf("U+0020 and U+007E (just outside the control ranges) must be accepted: %v", err)
	}
	// The ledger's own column is bounded too.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
			VALUES ($1, 'deposit', 'm0099-ledger', 'mock-psp', $2, gen_random_uuid())`, f.tenantID, strings.Repeat("l", 256))
		return err
	})
	if !isCheckViolation(err, "ledger_transactions_provider_tx_id_ref_bound") {
		t.Fatalf("256-byte ledger provider_tx_id must violate the CHECK, got %v", err)
	}
	// F3: the minimum length. ledger_transactions has no other non-empty
	// check on provider_tx_id, so only the 0099 bound can refuse "" here
	// (casino_callback_rejections' own 0097 "<> ''" check would mask it).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
			VALUES ($1, 'deposit', 'm0099-ledger-empty', 'mock-psp', '', gen_random_uuid())`, f.tenantID)
		return err
	})
	if !isCheckViolation(err, "ledger_transactions_provider_tx_id_ref_bound") {
		t.Fatalf("an empty ledger provider_tx_id must violate the 0099 CHECK, got %v", err)
	}

	down, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil || len(down) != 1 || down[0] != migration0099Version {
		t.Fatalf("down must roll back exactly 0099: %v %v", down, err)
	}
	if got := refBoundConstraintCount(t, pool); got != 0 {
		t.Fatalf("after down: %d *_ref_bound constraints remain", got)
	}
	// Without the CHECK the (application-bypassing) insert is possible
	// again - the pre-0099 state.
	if err := insertRejectionRow(t, pool, f, strings.Repeat("b", 300)); err != nil {
		t.Fatalf("after down a 300-byte value must insert: %v", err)
	}
	// ...which the next up's pre-flight now refuses (the row stays).
	if _, err := pool.MigrateUp(context.Background(), dir); err == nil ||
		!strings.Contains(err.Error(), "casino_callback_rejections.provider_tx_id=1") {
		t.Fatalf("re-up with an over-bound row must fail its pre-flight, got %v", err)
	}
}

func TestMigration0099_PreflightFailsLoudlyWithCountsAndChangesNothing(t *testing.T) {
	pool, _ := migration0099Scratch(t, "m0099pf_", migration0099Version-1)
	dir := migration0099Dir(t, migration0099Version)
	f := seedCasinoFixture(t, pool)
	longTx := strings.Repeat("x", 300)
	if err := insertRejectionRow(t, pool, f, longTx); err != nil {
		t.Fatalf("seed over-bound rejection row: %v", err)
	}
	if err := insertRejectionRow(t, pool, f, "ctl\x01ref"); err != nil {
		t.Fatalf("seed control-char rejection row: %v", err)
	}
	longRef := "m0099pf-" + strings.Repeat("s", 300)
	if err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sb_sports (id, external_ref, code, name) VALUES (gen_random_uuid(), $1, 'm0099pf', 'x')`, longRef)
		return err
	}); err != nil {
		t.Fatalf("seed over-bound sb_sports row: %v", err)
	}

	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil {
		t.Fatal("0099 must refuse to apply over existing over-bound rows")
	}
	for _, want := range []string{"migration 0099 pre-flight", "3 existing row(s)", "casino_callback_rejections.provider_tx_id=2", "sb_sports.external_ref=1", "never truncated or deleted"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("pre-flight error must contain %q, got: %v", want, err)
		}
	}
	// Nothing changed: 0099 not recorded, no constraint added, rows intact.
	if got := refBoundConstraintCount(t, pool); got != 0 {
		t.Fatalf("a failed pre-flight left %d constraints behind", got)
	}
	var applied bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 99)`).Scan(&applied)
	}); err != nil || applied {
		t.Fatalf("0099 must not be recorded as applied (applied=%v err=%v)", applied, err)
	}
	var n, maxLen int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), max(octet_length(provider_tx_id)) FROM casino_callback_rejections`).Scan(&n, &maxLen)
	}); err != nil || n != 2 || maxLen != 300 {
		t.Fatalf("offending rows must be untouched: n=%d maxLen=%d err=%v", n, maxLen, err)
	}
	var sportLen int
	if err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT octet_length(external_ref) FROM sb_sports WHERE code = 'm0099pf'`).Scan(&sportLen)
	}); err != nil || sportLen != len(longRef) {
		t.Fatalf("offending sb_sports row must be untouched: len=%d err=%v", sportLen, err)
	}
}
