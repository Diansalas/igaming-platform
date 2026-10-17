//go:build integration

package payments

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// f1Snapshot is the whole-schema snapshot of the migration tests (the m132Snapshot query of internal/withdrawal).
func f1Snapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid)
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity || ':acl=' || coalesce(c.relacl::text, '')
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef || ':acl=' || coalesce(p.proacl::text, '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

// 0175 up / down / up on a throwaway database: the down restores the previous schema byte for byte (0171 verifier, 0172
// payment_ref_cleared, no column / index / constraint / trigger left), the re-up equals the first up, and the down is
// refused (DT095) while a proof-bound deposit tombstone exists, changing nothing.
func TestMigration0175_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0175_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, migration0101Dir(t, 174)); err != nil {
		t.Fatalf("migrate to 0174: %v", err)
	}
	before := f1Snapshot(t, pool)
	full := migration0101Dir(t, 175)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 175 {
		t.Fatalf("0175 up: %v %v", applied, err)
	}
	up := f1Snapshot(t, pool)
	for _, want := range []string{"ledger_transactions_zz_deposit_tombstone_proof_guard", "column:ledger_transactions:actor_proof_nonce",
		"ledger_transactions_actor_proof_nonce_key", "ledger_transactions_actor_proof_nonce_shape"} {
		if !strings.Contains(up, want) {
			t.Fatalf("0175 up must create %q", want)
		}
	}
	if up == before {
		t.Fatal("0175 changed nothing (non-vacuity)")
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0175 down: %v", err)
	}
	if got := f1Snapshot(t, pool); got != before {
		t.Fatal("0175 down must restore the previous schema byte for byte")
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0175 up again: %v", err)
	}
	if got := f1Snapshot(t, pool); got != up {
		t.Fatal("0175 up again must equal the first up")
	}

	// Down refusal: one proof-bound deposit tombstone (synthetic tenant id; the ledger and receipt tables carry no tenant FK).
	prooftest.InstallVia(t, pool.Raw(), u)
	tenant := uuid.New()
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := PlantVerifiedDepositReversalTombstone(ctx, tx, tenant, "m0175-psp", "m0175-ref-"+uuid.NewString())
		return err
	}); err != nil {
		t.Fatalf("plant a proof-bound tombstone: %v", err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "DT095") {
		t.Fatalf("0175 down must refuse DT095 while a proof-bound tombstone exists, got %v", err)
	}
	if got := f1Snapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}
