//go:build integration

// ADR 0113 slice 2, migration 0133 (the LA025 ledger closure arm) on THROWAWAY scratch databases: a
// whole-schema up/down/up snapshot (the down restores 0130's launch_subject_status_guard body byte for
// byte and drops the index), the C3 index shape (HOT safety: no index of wallet_balance_projection
// names a column the projection trigger updates), the LA025 read placed inside the RLS-bound window,
// and the index read plan. The behaviour (stranded hold refused, brand scoping, races, Go arm) is in
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
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_wallet_balance_projection_withdrawal_hold'`).Scan(&idx); err != nil {
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
func TestMigration0133_IndexShape_HOTSafe_ReadInsideTheRLSWindow(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0133i_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0133Version)); err != nil {
		t.Fatal(err)
	}
	var def, src string
	var offenders []string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_wallet_balance_projection_withdrawal_hold'`).Scan(&def); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT c.relname
			  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			 WHERE i.indrelid = 'wallet_balance_projection'::regclass
			   AND (EXISTS (SELECT 1 FROM unnest(i.indkey) k JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k
			                 WHERE a.attname IN ('credit_total', 'debit_total', 'updated_at'))
			        OR coalesce(pg_get_expr(i.indpred, i.indrelid), '') ~ '(credit_total|debit_total|updated_at)'
			        OR coalesce(pg_get_expr(i.indexprs, i.indrelid), '') ~ '(credit_total|debit_total|updated_at)')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			offenders = append(offenders, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'launch_subject_status_guard'`).Scan(&src)
	}); err != nil {
		t.Fatal(err)
	}
	want := "CREATE INDEX idx_wallet_balance_projection_withdrawal_hold ON public.wallet_balance_projection USING btree (tenant_id) WHERE (account_type = 'player_withdrawal_hold'::text)"
	if def != want {
		t.Fatalf("index shape:\n got %s\nwant %s", def, want)
	}
	if len(offenders) != 0 {
		t.Fatalf("an index of wallet_balance_projection names a column the projection trigger updates (defeats HOT): %v", offenders)
	}
	bind := strings.Index(src, "PERFORM set_config('app.tenant_id', v_tenant::text, true);")
	read := strings.Index(src, "FROM public.wallet_balance_projection p")
	join := strings.Index(src, "FROM public.wallets wl")
	restore := strings.Index(src, "PERFORM set_config('app.tenant_id', COALESCE(v_prev_t, ''), true);")
	if bind < 0 || read < 0 || join < 0 || restore < 0 || !(bind < read && read < join && join < restore) {
		t.Fatalf("the LA025 projection read and wallets join must sit inside the RLS-bound window (bind=%d read=%d join=%d restore=%d)", bind, read, join, restore)
	}
	if !strings.Contains(src, "p.account_type = 'player_withdrawal_hold'") || !strings.Contains(src, "wl.brand_id = v_brand") {
		t.Fatal("the LA025 read must filter the hold account type literally (partial index match) and join wallets.brand_id")
	}
}

// Performance of the index read: the LA025 read (the trigger's exact predicate, the tenant bound as a
// parameter like the plpgsql generic plan) can be answered from the partial index; on a populated
// tenant set the planner chooses it without hints. The populated case seeds 2000 house
// accounts (one tenant each) plus 400 hold rows (three of them in the probed tenant) directly into the projection on
// the throwaway database (owner, FORCE RLS / triggers / two FKs lifted, no ledger rows: this database
// is dropped after the test and the transaction rolls back anyway).
func TestMigration0133_LA025ReadUsesThePartialIndex(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0133p_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0133Version)); err != nil {
		t.Fatal(err)
	}
	probe := uuid.New()
	q := `EXPLAIN (FORMAT TEXT) SELECT count(*) FROM public.wallet_balance_projection p
		WHERE p.tenant_id = $1 AND p.account_type = 'player_withdrawal_hold' AND p.credit_total <> p.debit_total`
	plan := func(tx pgx.Tx) string {
		rows, err := tx.Query(ctx, q, probe)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				t.Fatal(err)
			}
			b.WriteString(l + "\n")
		}
		return b.String()
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// FORCE RLS binds the owner: lift it for the synthetic population on this throwaway database.
		for _, s := range []string{
			`ALTER TABLE wallet_balance_projection NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE ledger_accounts NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE wallet_balance_projection DISABLE TRIGGER USER`,
			`ALTER TABLE ledger_accounts DISABLE TRIGGER USER`,
			`ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_asset_code_fkey`,
			`ALTER TABLE wallet_balance_projection DROP CONSTRAINT wallet_balance_projection_ledger_account_id_fkey`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			WITH a AS (
			  INSERT INTO ledger_accounts (id, tenant_id, account_type, asset_code, status)
			  SELECT gen_random_uuid(), gen_random_uuid(), t, 'EUR', 'active'
			    FROM generate_series(1, 400) g, unnest(ARRAY['house_gaming', 'psp_clearing', 'provider_payable', 'promo_liability', 'bonus_expense']) t
			  RETURNING id, tenant_id, account_type, asset_code)
			INSERT INTO wallet_balance_projection (ledger_account_id, tenant_id, asset_code, account_type, debit_total, credit_total)
			SELECT id, tenant_id, asset_code, account_type, 5, 7 FROM a`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wallet_balance_projection (ledger_account_id, tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total)
			SELECT gen_random_uuid(), CASE WHEN g <= 3 THEN $1 ELSE gen_random_uuid() END, gen_random_uuid(), gen_random_uuid(), 'EUR',
			       'player_withdrawal_hold', 0, CASE WHEN g % 2 = 0 THEN 10 ELSE 0 END
			  FROM generate_series(1, 400) g`, probe); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ANALYZE wallet_balance_projection`); err != nil {
			return err
		}
		p := plan(tx)
		if !strings.Contains(p, "idx_wallet_balance_projection_withdrawal_hold") {
			t.Fatalf("on a populated projection the LA025 read must use the partial index:\n%s", p)
		}
		t.Logf("LA025 read plan (2400 projection rows, about 2400 tenants):\n%s", p)
		return errRollback
	}); err != nil && err != errRollback {
		t.Fatal(err)
	}
}

type rollbackErr struct{}

func (rollbackErr) Error() string { return "rollback" }

var errRollback error = rollbackErr{}
