//go:build integration

// ADR 0113 slice 2, migration 0133 (the LA025 ledger closure arm) on THROWAWAY scratch databases: a
// whole-schema up/down/up snapshot (the down restores 0130's launch_subject_status_guard body byte for
// byte and drops the index), the index shape (a partial index on ledger_accounts only; no index of
// wallet_balance_projection is added), the LA025 read placed inside the RLS-bound window and rebuilt
// from ledger_entries (never the projection), and the read plan. The behaviour (stranded hold refused, brand scoping, races, Go arm) is in
// internal/launchgov/closure_hold_balance_integration_test.go. Requires TEST_DATABASE_URL and
// TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package tenant

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const migration0133Version = 133

func TestMigration0133_UpDownUp_WholeSchema_RestoresThe0130GuardVerbatim(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0133s_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0133Version-1)); err != nil {
		t.Fatal(err)
	}
	snap132 := lgSnapshot(t, pool)
	full := lgMigDir(t, migration0133Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	snap133 := lgSnapshot(t, pool)
	if snap132 == snap133 {
		t.Fatal("0133 changed nothing in the schema snapshot (vacuous)")
	}
	present := func() (idx int, la025 bool) {
		var src string
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_ledger_accounts_withdrawal_hold'`).Scan(&idx); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'launch_subject_status_guard'`).Scan(&src)
		}); err != nil {
			t.Fatal(err)
		}
		return idx, strings.Contains(src, "'LA025'")
	}
	if idx, la := present(); idx != 1 || !la {
		t.Fatalf("0133 objects: index=%d LA025 arm=%v", idx, la)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := lgSnapshot(t, pool); got != snap132 {
		t.Fatalf("down did not restore the 0132 schema byte for byte:\n%s", lgSnapDiff(snap132, got))
	}
	if idx, la := present(); idx != 0 || la {
		t.Fatalf("down left index=%d LA025 arm=%v", idx, la)
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := lgSnapshot(t, pool); got != snap133 {
		t.Fatalf("up after down differs from the first up:\n%s", lgSnapDiff(snap133, got))
	}
}

// C3 (ledger-finance): the partial index is keyed on tenant_id with a predicate on account_type ONLY,
// and NO index of wallet_balance_projection (key, expression or predicate) names credit_total,
// debit_total or updated_at - the three columns the projection trigger's ON CONFLICT DO UPDATE
// changes - so every projection update stays HOT-eligible. The LA025 read sits inside the guard's
// RLS-bound window (between the set_config of the subject tenant and the restore).

// Index shape and read source (security C1): the only index 0133 adds is the partial
// idx_ledger_accounts_withdrawal_hold (tenant_id) WHERE account_type = 'player_withdrawal_hold';
// wallet_balance_projection gains no index and the guard never reads it; the LA025 read sums
// ledger_entries and sits inside the guard's RLS-bound window (between the set_config of the subject
// tenant and the restore).
func TestMigration0133_IndexShape_LedgerSource_ReadInsideTheRLSWindow(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0133i_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0133Version-1)); err != nil {
		t.Fatal(err)
	}
	indexes := func() map[string]string {
		out := map[string]string{}
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = 'public'`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var n, d string
				if err := rows.Scan(&n, &d); err != nil {
					return err
				}
				out[n] = d
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := indexes()
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0133Version)); err != nil {
		t.Fatal(err)
	}
	after := indexes()
	var added []string
	for n := range after {
		if _, ok := before[n]; !ok {
			added = append(added, n)
		}
	}
	want := "CREATE INDEX idx_ledger_accounts_withdrawal_hold ON public.ledger_accounts USING btree (tenant_id) WHERE (account_type = 'player_withdrawal_hold'::text)"
	if len(added) != 1 || after["idx_ledger_accounts_withdrawal_hold"] != want {
		t.Fatalf("0133 must add exactly the partial ledger_accounts index:\n added %v\n got %s\nwant %s", added, after["idx_ledger_accounts_withdrawal_hold"], want)
	}
	var src string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'launch_subject_status_guard'`).Scan(&src)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, "wallet_balance_projection") {
		t.Fatal("the guard must never read wallet_balance_projection (runtime-writable, security C1)")
	}
	bind := strings.Index(src, "PERFORM set_config('app.tenant_id', v_tenant::text, true);")
	read := strings.Index(src, "FROM public.ledger_accounts la")
	join := strings.Index(src, "FROM public.wallets wl")
	sum := strings.Index(src, "FROM public.ledger_entries e")
	restore := strings.Index(src, "PERFORM set_config('app.tenant_id', COALESCE(v_prev_t, ''), true);")
	if bind < 0 || read < 0 || join < 0 || sum < 0 || restore < 0 || bind >= read || read >= join || join >= sum || sum >= restore {
		t.Fatalf("the LA025 read must sit inside the RLS-bound window (bind=%d read=%d join=%d sum=%d restore=%d)", bind, read, join, sum, restore)
	}
	if !strings.Contains(src, "la.account_type = 'player_withdrawal_hold'") || !strings.Contains(src, "wl.brand_id = v_brand") {
		t.Fatal("the LA025 read must filter the hold account type literally (partial index match) and join wallets.brand_id")
	}
}

// Read plan: the LA025 read (the guard's exact shape, the tenant bound as a parameter like the
// plpgsql generic plan) finds the tenant's hold accounts through idx_ledger_accounts_withdrawal_hold
// and sums each through idx_ledger_entries_account. Population on the throwaway database: 2000
// house accounts of 2000 tenants plus 400 hold accounts (three in the probed tenant) with two entries
// each (owner; FORCE RLS, triggers and the account/asset FKs lifted inside a rolled-back transaction).
func TestMigration0133_LA025ReadUsesTheIndexes(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0133p_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0133Version)); err != nil {
		t.Fatal(err)
	}
	probe := uuid.New()
	q := `EXPLAIN (FORMAT TEXT) SELECT count(*) FROM public.ledger_accounts la
		WHERE la.tenant_id = $1 AND la.account_type = 'player_withdrawal_hold'
		  AND (SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)
		         FROM public.ledger_entries e WHERE e.ledger_account_id = la.id AND e.tenant_id = $1) <> 0`
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{
			`ALTER TABLE ledger_accounts NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE ledger_entries NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE ledger_accounts DISABLE TRIGGER USER`,
			`ALTER TABLE ledger_entries DISABLE TRIGGER USER`,
			`ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_asset_code_fkey`,
			`ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_wallet_id_fkey`,
			`ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_ledger_transaction_id_tenant_id_fkey`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_accounts (id, tenant_id, account_type, asset_code, status)
			SELECT gen_random_uuid(), gen_random_uuid(), t, 'EUR', 'active'
			  FROM generate_series(1, 400) g, unnest(ARRAY['house_gaming', 'psp_clearing', 'provider_payable', 'promo_liability', 'bonus_expense']) t`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			WITH a AS (
			  INSERT INTO ledger_accounts (id, tenant_id, wallet_id, player_account_id, account_type, asset_code)
			  SELECT gen_random_uuid(), CASE WHEN g <= 3 THEN $1 ELSE gen_random_uuid() END, gen_random_uuid(), gen_random_uuid(),
			         'player_withdrawal_hold', 'EUR'
			    FROM generate_series(1, 400) g
			  RETURNING id, tenant_id)
			INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
			SELECT gen_random_uuid(), a.id, a.tenant_id, 'EUR', d, 10 FROM a, unnest(ARRAY['credit', 'debit']) d`, probe); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ANALYZE ledger_accounts; ANALYZE ledger_entries`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, q, probe)
		if err != nil {
			return err
		}
		var b strings.Builder
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				rows.Close()
				return err
			}
			b.WriteString(l + "\n")
		}
		rows.Close()
		p := b.String()
		if !strings.Contains(p, "idx_ledger_accounts_withdrawal_hold") || !strings.Contains(p, "idx_ledger_entries_account") {
			t.Fatalf("the LA025 read must use the partial account index and the per-account entry index:\n%s", p)
		}
		t.Logf("LA025 read plan (2400 accounts, 800 hold entries):\n%s", p)
		return errRollback
	}); err != nil && err != errRollback {
		t.Fatal(err)
	}
}

type rollbackErr struct{}

func (rollbackErr) Error() string { return "rollback" }

var errRollback error = rollbackErr{}
