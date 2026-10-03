//go:build integration

package adjustment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// schemaSnapshot renders every policy, trigger, constraint, table and
// function in the public schema as sorted text - the whole-schema
// equality ADR 0100 §10.9 needs ("the existing ledger policies are left
// byte-identical"; "restore the previous kind CHECK exactly").
func schemaSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || pg_get_triggerdef(t.oid)
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc)
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace) s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func diffLines(a, b string) string {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		as[l] = true
	}
	for _, l := range strings.Split(b, "\n") {
		bs[l] = true
	}
	var out []string
	for l := range as {
		if !bs[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range bs {
		if !as[l] {
			out = append(out, "+ "+l)
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return strings.Join(out, "\n")
}

// B-19 (MIG): 0113 up/down/up on a scratch DB migrated only through 0113;
// the down restores the pre-0113 schema EXACTLY (whole-schema snapshot,
// incl. the reconciliation kind CHECK and every pre-existing ledger
// policy); down refuses (MA099) while governed rows exist, and while a
// ledger_unlinked_manual_adjustment mismatch exists.
func TestB19_Migration0113UpDownUp(t *testing.T) {
	pre, _ := scratchPoolThroughDir(t, "k2m0112_", 112)
	preSnap := schemaSnapshot(t, pre)

	pool, dir := scratchPoolThroughDir(t, "k2m0113_", 113)
	upSnap := schemaSnapshot(t, pool)
	if upSnap == preSnap {
		t.Fatal("0113 changed nothing?")
	}
	ctx := context.Background()
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down on an empty 0113: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != preSnap {
		t.Fatalf("0113 down did not restore the pre-0113 schema exactly:\n%s", diffLines(preSnap, got))
	}
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != upSnap {
		t.Fatalf("0113 re-up differs from the first up:\n%s", diffLines(upSnap, got))
	}

	// The unlinked-mismatch refusal alone (no governed rows yet).
	w := newWorldOn(t, pool, worldOpts{})
	if err := pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: w.Asset})
		if err != nil {
			return err
		}
		reason := "fixture"
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fixture:" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: 3}, {LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: 3}}}); err != nil {
			return err
		}
		_, _, err = reconciliation.RunLedgerVsProjection(ctx, tx, w.Tenant, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil {
		t.Fatalf("seed unlinked mismatch: %v", err)
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); pgCode(err) != "MA099" {
		t.Fatalf("down with a ledger_unlinked_manual_adjustment mismatch: expected MA099, got %v", err)
	}

	// Governed rows (a policy change) also refuse.
	pool2, dir2 := scratchPoolThroughDir(t, "k2m0113b_", 113)
	w2 := newWorldOn(t, pool2, worldOpts{})
	if _, err := w2.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		AssetCode: strPtr(w2.Asset), BaseRequiredApprovals: intPtr(1)}, w2.AdminA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool2.MigrateDown(ctx, dir2, 1); pgCode(err) != "MA099" {
		t.Fatalf("down with governed rows: expected MA099, got %v", err)
	}
}
