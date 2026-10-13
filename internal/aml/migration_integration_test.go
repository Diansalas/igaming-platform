//go:build integration

// ADR 0116 migrations 0140 / 0141 / 0142 on THROWAWAY scratch databases:
//
//   - up / down / up whole-schema snapshots, one migration at a time: each down restores the previous schema EXACTLY (policies,
//     triggers, constraints, columns, function bodies, ACLs, indexes) and the second up equals the first;
//   - the up pre-flight is not blind: each class of pre-existing object or altered dependency refuses AM098 and changes nothing;
//   - the downs REFUSE (AM099) while AML history exists and delete nothing.
//
// Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package aml_test

import (
	"context"
	"fmt"
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
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func migDir(t *testing.T, through int64) string {
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
			t.Fatalf("migration %q has no numeric prefix", name)
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

func scratch(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), u, 8, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, u
}

func snapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || roles::text || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
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
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public'
			UNION ALL
			SELECT 'alertkind:' || kind || ':' || severity || ':' || allowed_keys::text FROM alert_kinds
			UNION ALL
			SELECT 'catalogue:' || capability || ':' || operation_kind || ':' || action || ':' || eligible_tenant_roles::text FROM financial_capability_catalogue
			UNION ALL
			SELECT 'classification:' || operation_kind || ':' || class FROM financial_control_classifications) s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func snapDiff(a, b string) string {
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
	sort.Strings(out)
	if len(out) > 25 {
		out = append(out[:25], fmt.Sprintf("... (%d differences)", len(out)))
	}
	return strings.Join(out, "\n")
}

func wantSame(t *testing.T, what, a, b string) {
	t.Helper()
	if a != b {
		t.Fatalf("%s: schemas differ:\n%s", what, snapDiff(a, b))
	}
}

func TestMigrations0140to0142_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0140s_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 139)); err != nil {
		t.Fatal(err)
	}
	base := snapshot(t, pool)

	full := migDir(t, 142)
	var snaps [3]string
	for i, v := range []int64{140, 141, 142} {
		dir := migDir(t, v)
		if _, err := pool.MigrateUp(ctx, dir); err != nil {
			t.Fatalf("up %d: %v", v, err)
		}
		snaps[i] = snapshot(t, pool)
		if i > 0 && snaps[i] == snaps[i-1] {
			t.Fatalf("migration %d changed nothing", v)
		}
	}
	if snaps[0] == base {
		t.Fatal("0140 changed nothing")
	}
	// Down one at a time, each restoring the previous schema exactly.
	for i := 2; i >= 0; i-- {
		if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
			t.Fatalf("down %d: %v", 140+i, err)
		}
		want := base
		if i > 0 {
			want = snaps[i-1]
		}
		wantSame(t, fmt.Sprintf("after down %d", 140+i), want, snapshot(t, pool))
	}
	// Up again equals the first up.
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("second up: %v", err)
	}
	wantSame(t, "second up vs first up", snaps[2], snapshot(t, pool))
}

// ---- pre-flight (AM098) ----

// preflightRefuses runs `plant` and then the up script of file in ONE transaction as the owner and expects AM098 naming want;
// the transaction rolls back, so nothing is changed.
func preflightRefuses(t *testing.T, pool *db.Pool, file, want string, plant func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", file))
	if err != nil {
		t.Fatal(err)
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if plant != nil {
			if err := plant(ctx, tx); err != nil {
				return fmt.Errorf("plant: %w", err)
			}
		}
		_, err := tx.Exec(ctx, string(up))
		return err
	})
	if pgCode(err) != "AM098" || !strings.Contains(fmt.Sprint(err), want) {
		t.Fatalf("%s must refuse AM098 mentioning %q, got %v", file, want, err)
	}
}

func TestMigration0140_Preflight(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0140p_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 136)); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	f := "0140_aml_access_foundation.up.sql"
	preflightRefuses(t, pool, f, "aml_* function", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION aml_planted() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
		return err
	})
	preflightRefuses(t, pool, f, "aml_* relation", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TABLE aml_planted_table (id int)`)
		return err
	})
	preflightRefuses(t, pool, f, "already exists", func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE financial_control_classifications NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO financial_control_classifications (operation_kind, class, governed_since) VALUES ('aml_officer', 'outside_mandatory_class', now())`); err != nil {
			return err
		}
		return nil
	})
	preflightRefuses(t, pool, f, "financial_capability_catalogue_action_check", func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_action_check`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_action_check CHECK (action IN ('initiate', 'request', 'approve', 'other'))`)
		return err
	})
	preflightRefuses(t, pool, f, "financial_capability_catalogue_operation_kind_check", func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_operation_kind_check`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_operation_kind_check CHECK (operation_kind IN ('ledger_adjustment', 'payment_force_resolve', 'withdrawal_hold_resolution', 'planted'))`)
		return err
	})
	wantSame(t, "a refused pre-flight changes nothing", before, snapshot(t, pool))
	// A clean database applies.
	if _, err := pool.MigrateUp(ctx, migDir(t, 140)); err != nil {
		t.Fatalf("clean apply: %v", err)
	}
}

func TestMigration0141_Preflight(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0141p_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 140)); err != nil {
		t.Fatal(err)
	}
	f := "0141_aml_holds_and_enforcement.up.sql"
	before := snapshot(t, pool)
	preflightRefuses(t, pool, f, "AML hold table", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TABLE aml_holds (id int)`)
		return err
	})
	preflightRefuses(t, pool, f, "kyc_enforcement_decisions_outcome_check", func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE kyc_enforcement_decisions DROP CONSTRAINT kyc_enforcement_decisions_outcome_check`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE kyc_enforcement_decisions ADD CONSTRAINT kyc_enforcement_decisions_outcome_check CHECK (outcome IN ('passed'))`)
		return err
	})
	preflightRefuses(t, pool, f, "alert kind", func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE POLICY plant ON alert_kinds FOR INSERT TO CURRENT_USER WITH CHECK (true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode)
			VALUES ('aml.hold_eval_failed', 'p2', 'platform', false, true, true, ARRAY['operation'], 'in_tx')`)
		return err
	})
	wantSame(t, "a refused pre-flight changes nothing", before, snapshot(t, pool))
	// 0141 without 0140 is refused.
	pool2, _ := scratch(t, "m0141q_")
	if _, err := pool2.MigrateUp(ctx, migDir(t, 136)); err != nil {
		t.Fatal(err)
	}
	preflightRefuses(t, pool2, f, "0140", nil)
}

func TestMigration0142_Preflight(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0142p_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 141)); err != nil {
		t.Fatal(err)
	}
	f := "0142_aml_cases_and_four_eyes.up.sql"
	before := snapshot(t, pool)
	preflightRefuses(t, pool, f, "AML case table", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TABLE aml_cases (id int)`)
		return err
	})
	preflightRefuses(t, pool, f, "AML case function", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION aml_case_open() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
		return err
	})
	wantSame(t, "a refused pre-flight changes nothing", before, snapshot(t, pool))
}

// ---- the downs refuse while history exists (AM099) and delete nothing ----

func TestMigrationDowns_RefuseWhileAMLHistoryExists(t *testing.T) {
	ctx := context.Background()
	pool, url := scratch(t, "m0142d_")
	full := migDir(t, 142)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)

	// Build a tenant, two officers (K1 grants), a hold, a case and a decision row on the scratch database (owner drives).
	var tenant uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0142d", "m0142d-"+uuid.NewString(), "under_platform_licence")
		tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var person, account uuid.UUID
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenant, "b", "m0142d-b-"+uuid.NewString())
		if err != nil {
			return err
		}
		a, err := registerFixturePlayer(ctx, tx, b)
		person, account = a.PersonID, a.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	q := amlfix.Bootstrap(t, pool, tenant, 2)
	svc := amlfix.Service(t, pool)
	q.PlaceHold(t, svc, person, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	if _, err := svc.OpenCase(ctx, q.Officers[0].Actor, person, aml.CaseKindManual, "t", nil); err != nil {
		t.Fatal(err)
	}
	_ = account
	populated := snapshot(t, pool)

	down := func(want string) {
		t.Helper()
		_, err := pool.MigrateDown(ctx, full, 1)
		if err == nil || !strings.Contains(fmt.Sprint(err), "AM099") && pgCode(err) != "AM099" {
			t.Fatalf("down must refuse AM099 (%s): %v", want, err)
		}
		wantSame(t, "a refused down changes nothing", populated, snapshot(t, pool))
	}
	down("0142 with case history")
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_holds`).Scan(&n)
	}); err != nil {
		t.Fatalf("history intact: %v", err)
	}
	// 0141 / 0140 cannot be reached while 0142 is applied (one step at a time, newest first); their refusals are exercised in
	// the dedicated, smaller worlds below.
}

func TestMigration0141Down_RefusesWhileHoldsOrReviewDecisionsExist(t *testing.T) {
	ctx := context.Background()
	pool, url := scratch(t, "m0141d_")
	if _, err := pool.MigrateUp(ctx, migDir(t, 141)); err != nil {
		t.Fatal(err)
	}
	full := migDir(t, 141)
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)
	var tenant uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0141d", "m0141d-"+uuid.NewString(), "under_platform_licence")
		tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var person, account uuid.UUID
	var brand uuid.UUID
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenant, "b", "m0141d-b-"+uuid.NewString())
		if err != nil {
			return err
		}
		brand = b.ID
		a, err := registerFixturePlayer(ctx, tx, b)
		person, account = a.PersonID, a.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// (a) A `review` decision row alone refuses the down (the CHECK would otherwise be violated).
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_enforcement_decisions (tenant_id, brand_id, player_account_id, operation, outcome, allowed, policy_version, correlation_id)
			VALUES ($1, $2, $3, 'deposit', 'review', false, 'v', $4)`, tenant, brand, account, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	if _, err := pool.MigrateDown(ctx, full, 1); err == nil || !strings.Contains(fmt.Sprint(err), "AM099") {
		t.Fatalf("0141 down must refuse while a review decision exists: %v", err)
	}
	wantSame(t, "refused down changes nothing", before, snapshot(t, pool))

	// (b) A hold refuses it too.
	q := amlfix.Bootstrap(t, pool, tenant, 2)
	q.PlaceHold(t, amlfix.Service(t, pool), person, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	if _, err := pool.MigrateDown(ctx, full, 1); err == nil || !strings.Contains(fmt.Sprint(err), "AM099") {
		t.Fatalf("0141 down must refuse while a hold exists: %v", err)
	}
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM aml_holds`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("the hold must survive the refused down, found %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMigration0140Down_RefusesWhileOfficerGrantsExist(t *testing.T) {
	ctx := context.Background()
	pool, url := scratch(t, "m0140d_")
	full := migDir(t, 140)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)
	var tenant uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0140d", "m0140d-"+uuid.NewString(), "under_platform_licence")
		tenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	amlfix.Bootstrap(t, pool, tenant, 1)
	before := snapshot(t, pool)
	if _, err := pool.MigrateDown(ctx, full, 1); err == nil || !strings.Contains(fmt.Sprint(err), "AM099") {
		t.Fatalf("0140 down must refuse while aml_officer grants exist: %v", err)
	}
	wantSame(t, "refused down changes nothing", before, snapshot(t, pool))
}
