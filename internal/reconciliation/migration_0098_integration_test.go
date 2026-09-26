//go:build integration

// Stage 10.3 W3a (CAS-RECON-STMT-1), migration 0098 - on scratch databases
// only (DDL never touches the shared test database):
//   - up applies on a clean chain and admits cas_mock_statement_mismatch;
//   - down succeeds on a clean database, restores 0097's kind list (the
//     statement kind is refused again, the casino_consistency kinds are
//     still accepted) and round-trips;
//   - down REFUSES, with the "roll forward" message, once a
//     cas_mock_statement_mismatch row exists, and leaves the evidence and
//     the widened CHECK untouched.
package reconciliation

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0098Version = int64(98)

// migration0098DirThroughSelf copies the migrations up to and including
// 0098 (nothing later), so "roll back 1 step" always means 0098 itself.
func migration0098DirThroughSelf(t *testing.T) string {
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
		if err != nil || int64(v) > migration0098Version {
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

func migration0098Scratch(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	dir := migration0098DirThroughSelf(t)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through 0098: %v", err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != migration0098Version {
		t.Fatalf("expected 0098 to be the last applied migration, got %v", applied)
	}
	return pool, dir
}

func persistOneMismatch(t *testing.T, pool *db.Pool, tenantID uuid.UUID, stream Stream, kind MismatchKind) error {
	t.Helper()
	return pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return persistRun(ctx, tx, Run{ID: uuid.New(), TenantID: tenantID, Stream: stream,
			PeriodStart: time.Now(), PeriodEnd: time.Now(), RunAt: time.Now(), Status: StatusMismatchesFound},
			[]Mismatch{{ID: uuid.New(), TenantID: tenantID, ReconciliationKey: "k", ExpectedValue: "e", ActualValue: "[MOCK] a",
				MismatchKind: kind, InvestigationStatus: "open"}})
	})
}

func TestMigration0098_CleanDatabaseDownThenUpRoundTrip(t *testing.T) {
	pool, dir := migration0098Scratch(t, "m0098rt_")
	f := seedFixture(t, pool)
	// Up admits the statement kind (rolled back: a test-only transaction
	// that must not leave evidence behind, or down would refuse).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := persistRun(ctx, tx, Run{ID: uuid.New(), TenantID: f.tenantID, Stream: StreamCasinoStatement,
			PeriodStart: time.Now(), PeriodEnd: time.Now(), RunAt: time.Now(), Status: StatusMismatchesFound},
			[]Mismatch{{ID: uuid.New(), TenantID: f.tenantID, ReconciliationKey: "k", ExpectedValue: "e", ActualValue: "a",
				MismatchKind: MismatchKindCasMockStatement, InvestigationStatus: "open"}}); err != nil {
			return err
		}
		return errRollbackProbe
	})
	if err != errRollbackProbe {
		t.Fatalf("after up the statement kind must be accepted, got %v", err)
	}

	down, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil || len(down) != 1 || down[0] != migration0098Version {
		t.Fatalf("down on a clean database must roll back exactly 0098: %v %v", down, err)
	}
	if err := persistOneMismatch(t, pool, f.tenantID, StreamCasinoStatement, MismatchKindCasMockStatement); err == nil ||
		!strings.Contains(err.Error(), "mismatch_kind_check") {
		t.Fatalf("after down the statement kind must be refused by the restored CHECK, got %v", err)
	}
	// 0097's kinds survive the down (it restores 0097's list, not 0091's).
	if err := persistOneMismatch(t, pool, f.tenantID, StreamCasinoConsistency, MismatchKindCasOrphanWin); err != nil {
		t.Fatalf("after 0098 down the casino_consistency kinds must still be accepted: %v", err)
	}
	up, err := pool.MigrateUp(context.Background(), dir)
	if err != nil || len(up) != 1 || up[0] != migration0098Version {
		t.Fatalf("re-applying 0098: %v %v", up, err)
	}
	if err := persistOneMismatch(t, pool, f.tenantID, StreamCasinoStatement, MismatchKindCasMockStatement); err != nil {
		t.Fatalf("after the round trip the statement kind must be accepted: %v", err)
	}
}

func TestMigration0098_DownRefusesOnceAStatementMismatchExists(t *testing.T) {
	pool, dir := migration0098Scratch(t, "m0098mm_")
	f := seedFixture(t, pool)
	if err := persistOneMismatch(t, pool, f.tenantID, StreamCasinoStatement, MismatchKindCasMockStatement); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	if err == nil || !strings.Contains(err.Error(), "roll forward") || !strings.Contains(err.Error(), "cas_mock_statement_mismatch") {
		t.Fatalf("down must refuse with the roll-forward message, got %v", err)
	}
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE mismatch_kind = 'cas_mock_statement_mismatch'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("evidence must survive the refused down: n=%d err=%v", n, err)
	}
	// The refused down rolled back as a whole: 0098 is still applied and
	// its CHECK still admits the kind.
	if err := persistOneMismatch(t, pool, f.tenantID, StreamCasinoStatement, MismatchKindCasMockStatement); err != nil {
		t.Fatalf("after a refused down the widened CHECK must still be in place: %v", err)
	}
}

type rollbackProbe struct{}

func (rollbackProbe) Error() string { return "rollback probe" }

var errRollbackProbe error = rollbackProbe{}
