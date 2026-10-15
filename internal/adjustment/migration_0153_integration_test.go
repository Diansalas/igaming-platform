//go:build integration

// MANUAL-ADJ-LINK-1, migration 0153 on a scratch database: up adds EXACTLY the two link constraint triggers and
// their two functions and changes nothing else; down restores the previous schema exactly and touches no row (a
// governed adjustment posted under the fence stays on the ledger, balanced); up after down equals the first up and
// the fence is enforced again; a second down/up round trip is idempotent.
package adjustment

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const m153Version = 153

// m153Dir copies every migration with version <= through into a temp dir. "Below 0153" is every earlier file that
// exists (0152 belongs to parallel work and may or may not be present on this branch).
func m153Dir(t *testing.T, through int64) string {
	t.Helper()
	src := "../../migrations"
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := strconv.ParseInt(name[:4], 10, 64)
		if err != nil || v > through {
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

func m153Snapshot(t *testing.T, pool *db.Pool) string {
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
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || contype::text || ':' || condeferrable || ':' || coalesce(pg_get_constraintdef(oid), '')
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || pt.tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity || ':acl=' || coalesce(c.relacl::text, '')
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef || ':' || p.provolatile::text || ':acl=' || coalesce(p.proacl::text, '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func m153Diff(a, b string) (removed, added []string) {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		as[l] = true
	}
	for _, l := range strings.Split(b, "\n") {
		bs[l] = true
	}
	for l := range as {
		if !bs[l] {
			removed = append(removed, l)
		}
	}
	for l := range bs {
		if !as[l] {
			added = append(added, l)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	return
}

func TestMigration0153_UpDownUp_WholeSchemaAndData(t *testing.T) {
	ctx := context.Background()
	url := scratchdb.New(t, "m0153_")
	pool, err := db.Connect(ctx, url, 20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, m153Dir(t, m153Version-1)); err != nil {
		t.Fatalf("migrate scratch below 0153: %v", err)
	}
	before := m153Snapshot(t, pool)
	full := m153Dir(t, m153Version)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != m153Version {
		t.Fatalf("up: %v %v", applied, err)
	}
	after := m153Snapshot(t, pool)
	removed, added := m153Diff(before, after)
	if len(removed) != 0 {
		t.Fatalf("0153 must not change or remove any object; removed: %v", removed)
	}
	want := []string{
		"constraint:ledger_entries:ledger_entries_manual_adjustment_link:t:false:TRIGGER",
		"constraint:ledger_transactions:ledger_transactions_manual_adjustment_link:t:false:TRIGGER",
		"function:ledger_manual_adjustment_entry_check():",
		"function:ledger_manual_adjustment_link_check():",
		"trigger:ledger_entries:ledger_entries_manual_adjustment_link:O:CREATE CONSTRAINT TRIGGER ledger_entries_manual_adjustment_link AFTER INSERT ON public.ledger_entries NOT DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION ledger_manual_adjustment_entry_check()",
		"trigger:ledger_transactions:ledger_transactions_manual_adjustment_link:O:CREATE CONSTRAINT TRIGGER ledger_transactions_manual_adjustment_link AFTER INSERT ON public.ledger_transactions NOT DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW WHEN ((new.transaction_type = 'manual_adjustment'::text)) EXECUTE FUNCTION ledger_manual_adjustment_link_check()",
	}
	if len(added) != len(want) {
		t.Fatalf("0153 must add exactly %d objects, added %d:\n%s", len(want), len(added), strings.Join(added, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(added[i], w) {
			t.Fatalf("0153 added object %d = %q, want prefix %q", i, added[i], w)
		}
		if strings.HasPrefix(w, "function:") && !strings.Contains(added[i], ":search_path=pg_catalog, public, pg_temp:false:v:") {
			t.Fatalf("0153 function must pin search_path, be invoker-rights and volatile: %s", added[i])
		}
	}

	// Data: a governed adjustment posted under the fence; then down must leave every row exactly as it is.
	prooftest.InstallVia(t, pool.Raw(), url)
	w := newWorldOn(t, pool, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(123, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("governed execution on the scratch DB: %v %+v", err, out)
	}
	rows := func() string {
		var s string
		if err := pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1)::text || '/' ||
				(SELECT count(*) FROM ledger_entries WHERE tenant_id = $1)::text || '/' ||
				(SELECT COALESCE(sum(amount) FILTER (WHERE direction = 'debit'), 0) - COALESCE(sum(amount) FILTER (WHERE direction = 'credit'), 0) FROM ledger_entries WHERE tenant_id = $1)::text`,
				w.Tenant).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// A non-manual_adjustment type on the house manual_adjustment account (MA052, security A4 F1).
	otherType := func() error {
		pc, house := w.accounts(t)
		reason := "m0153-ma052"
		return pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxBonusForfeiture,
				IdempotencyKey: "m0153-ma052-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
				Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 1}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: 1}}})
			if err == nil {
				return errLF153Rollback // never commit a probe posting
			}
			return err
		})
	}
	unlinked := func() error {
		pc, house := w.accounts(t)
		reason := "m0153"
		return pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			id := uuid.New()
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
				IdempotencyKey: "manual_adjustment:" + id.String(), CorrelationID: id, ReasonCode: &reason,
				Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 1}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: 1}}})
			if err == nil {
				return errLF153Rollback // never commit a probe posting
			}
			return err
		})
	}
	if err := unlinked(); pgCode(err) != "MA050" {
		t.Fatalf("after up the fence must refuse an unlinked posting, got %v", err)
	}
	if err := otherType(); pgCode(err) != "MA052" {
		t.Fatalf("after up a bonus_forfeiture on the manual_adjustment account must be refused (MA052), got %v", err)
	}
	dataBefore := rows()
	if !strings.HasSuffix(dataBefore, "/0") {
		t.Fatalf("ledger not balanced before down: %s", dataBefore)
	}

	for round := 1; round <= 2; round++ {
		if rolled, err := pool.MigrateDown(ctx, full, 1); err != nil || len(rolled) != 1 || rolled[0] != m153Version {
			t.Fatalf("round %d down: %v %v", round, rolled, err)
		}
		if got := m153Snapshot(t, pool); got != before {
			r, a := m153Diff(before, got)
			t.Fatalf("round %d: down did not restore the previous schema exactly:\n- %v\n+ %v", round, r, a)
		}
		if got := rows(); got != dataBefore {
			t.Fatalf("round %d: down changed ledger rows: %s -> %s", round, dataBefore, got)
		}
		// Without the fence the unlinked posting is admitted again (proof the down really removed it); the probe
		// itself is rolled back.
		if err := unlinked(); err != errLF153Rollback {
			t.Fatalf("round %d: after down an unlinked posting should be admitted (and rolled back by the probe), got %v", round, err)
		}
		if err := otherType(); err != errLF153Rollback {
			t.Fatalf("round %d: after down MA052 must be gone (the probe posting admitted, then rolled back), got %v", round, err)
		}
		if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != m153Version {
			t.Fatalf("round %d up: %v %v", round, applied, err)
		}
		if got := m153Snapshot(t, pool); got != after {
			r, a := m153Diff(after, got)
			t.Fatalf("round %d: up after down differs from the first up:\n- %v\n+ %v", round, r, a)
		}
		if err := unlinked(); pgCode(err) != "MA050" {
			t.Fatalf("round %d: after re-up the fence must refuse again, got %v", round, err)
		}
		if err := otherType(); pgCode(err) != "MA052" {
			t.Fatalf("round %d: after re-up MA052 must refuse again, got %v", round, err)
		}
	}
	w.assertInvariants()
}
