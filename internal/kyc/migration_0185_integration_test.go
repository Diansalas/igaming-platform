//go:build integration

package kyc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func fnBody(t *testing.T, pool *db.Pool, name string) string {
	t.Helper()
	var src string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = $1 AND pronamespace = 'public'::regnamespace`, name).Scan(&src)
	}); err != nil {
		t.Fatal(err)
	}
	return src
}

func fileBody(t *testing.T, file, head string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", file))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, head)
	if i < 0 {
		t.Fatalf("%s: %q not found", file, head)
	}
	a := strings.Index(s[i:], "$$")
	if a < 0 {
		t.Fatal("no body")
	}
	rest := s[i+a+2:]
	return rest[:strings.Index(rest, "$$")]
}

// Migration 0185: up, down (the 0182 verifier and the 0103 lifecycle function are restored byte for byte in the catalogue) and up
// again.
func TestMigration0185_UpDownUp_RestoresTheOlderBodiesByteForByte(t *testing.T) {
	src := filepath.Join("..", "..", "migrations")
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n, perr := strconv.ParseInt(e.Name()[:4], 10, 64)
		if perr != nil || n > 185 {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, "m185"), 10, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	upVerifier, upLifecycle := fnBody(t, pool, "actor_proof_require"), fnBody(t, pool, "kyc_enforcement_policies_enforce_lifecycle")
	if !strings.Contains(upVerifier, "kyc_enforcement_policy:create") || !strings.Contains(upLifecycle, "kyc_enforcement_policy:activate") {
		t.Fatal("0185 must define the kyc_enforcement_policy family in both functions")
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got, want := fnBody(t, pool, "actor_proof_require"), fileBody(t, "0182_withdrawal_restriction_policy.up.sql", "CREATE OR REPLACE FUNCTION actor_proof_require("); got != want {
		t.Fatal("the down must restore the 0182 verifier body byte for byte in the catalogue")
	}
	if got, want := fnBody(t, pool, "kyc_enforcement_policies_enforce_lifecycle"), fileBody(t, "0103_kyc_enforcement_policy_supersession.up.sql", "CREATE OR REPLACE FUNCTION kyc_enforcement_policies_enforce_lifecycle()"); got != want {
		t.Fatal("the down must restore the 0103 lifecycle function byte for byte in the catalogue")
	}
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname = 'kyc_enforcement_policies_require_create_proof'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("the down must drop the creation trigger: %d %v", n, err)
	}
	if applied, err := pool.MigrateUp(context.Background(), dir); err != nil || len(applied) != 1 || applied[0] != 185 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	if fnBody(t, pool, "actor_proof_require") != upVerifier || fnBody(t, pool, "kyc_enforcement_policies_enforce_lifecycle") != upLifecycle {
		t.Fatal("re-up must reproduce the 0185 bodies")
	}
}
