//go:build integration

// ADR 0119 S2, migration 0137 on THROWAWAY scratch databases:
//
//   - up / down / up: the down restores the 0136 schema exactly (whole-schema snapshot, including trigger
//     enablement, SECURITY DEFINER flags, table AND column ACLs) and up again equals the first up; the up only
//     ADDS objects (no existing object changes);
//   - pre-flight (SF098): every drift of the staff_users RLS posture the new tables replicate refuses the up,
//     with names / counts only, and creates nothing;
//   - down refusal (SF097): any row in any of the four tables refuses the down, counts only, with nothing
//     dropped and FORCE ROW LEVEL SECURITY still on.
//
// Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0137Version = 137

const m137UpFile = "0137_staff_mfa.up.sql"
const m137DownFile = "0137_staff_mfa.down.sql"

func migDir(t *testing.T, through int64) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "..", "migrations"))
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

func scratchPool(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), u, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func readMig(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func snapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid) || ':' || coalesce(obj_description(t.oid, 'pg_trigger'), '')
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
			SELECT 'colacl:' || c.relname || ':' || a.attname || ':' || a.attacl::text
			  FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
			 WHERE c.relnamespace = 'public'::regnamespace AND a.attacl IS NOT NULL AND NOT a.attisdropped
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

func snapDiff(a, b string) string { return snapDiffN(a, b, 20) }

func snapDiffN(a, b string, limit int) string {
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
	if limit > 0 && len(out) > limit {
		out = append(out[:limit], "... ("+strconv.Itoa(len(out))+" differences)")
	}
	return strings.Join(out, "\n")
}

func TestMigration0137_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool := scratchPool(t, "m0137s_")
	if _, err := pool.MigrateUp(ctx, migDir(t, migration0137Version-1)); err != nil {
		t.Fatal(err)
	}
	snap136 := snapshot(t, pool)
	full := migDir(t, migration0137Version)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != migration0137Version {
		t.Fatalf("up: %v %v", applied, err)
	}
	snap137 := snapshot(t, pool)
	if snap136 == snap137 {
		t.Fatal("0137 changed nothing in the schema snapshot (vacuous)")
	}
	added := snapDiffN(snap136, snap137, 0)
	for _, must := range []string{
		"table:staff_mfa_factors:rls=true:force=true", "table:staff_mfa_recovery_codes:rls=true:force=true",
		"table:staff_auth_challenges:rls=true:force=true", "table:staff_enrolment_tokens:rls=true:force=true",
		"sf_factor_guard", "sf_recovery_guard", "sf_challenge_guard", "sf_enrolment_token_guard",
		"sf_force_tenant_from_staff()", "sf_deny_delete()", "sf_deny_truncate()",
		"staff_mfa_factors_hard_lock_consistent", "staff_challenge_select_by_mac", "staff_enrolment_select_by_mac",
		"staff_mfa_factors_one_active", "staff_mfa_factors_one_pending", "staff_enrolment_tokens_one_live",
	} {
		if !strings.Contains(added, must) {
			t.Fatalf("0137 snapshot diff lacks %q:\n%s", must, added)
		}
	}
	for _, l := range strings.Split(added, "\n") {
		if strings.HasPrefix(l, "- ") {
			t.Fatalf("0137 must not change or remove an existing object: %s", l)
		}
		// Only the four new tables, their objects and the seven new functions are added.
		if !strings.Contains(l, "staff_mfa_") && !strings.Contains(l, "staff_auth_challenges") && !strings.Contains(l, "staff_enrolment_") &&
			!strings.Contains(l, "sf_") && !strings.HasPrefix(l, "... ") {
			t.Fatalf("0137 added an object outside the staff MFA tables: %s", l)
		}
	}
	if rolled, err := pool.MigrateDown(ctx, full, 1); err != nil || len(rolled) != 1 || rolled[0] != migration0137Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	if got := snapshot(t, pool); got != snap136 {
		t.Fatalf("down did not restore the 0136 schema exactly:\n%s", snapDiff(snap136, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, pool); got != snap137 {
		t.Fatalf("up after down differs from the first up:\n%s", snapDiff(snap137, got))
	}
}

// objects counts what 0137 creates, so a refused up/down can be shown to leave the schema as it was.
func m137Objects(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (tables, functions int, err error) {
	err = q.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname IN ('staff_mfa_factors', 'staff_mfa_recovery_codes', 'staff_auth_challenges', 'staff_enrolment_tokens')),
		(SELECT count(*) FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'sf\_%')`).Scan(&tables, &functions)
	return
}

func TestMigration0137_PreFlight_RefusesFenceDrift(t *testing.T) {
	ctx := context.Background()
	pool := scratchPool(t, "m0137p_")
	if _, err := pool.MigrateUp(ctx, migDir(t, migration0137Version-1)); err != nil {
		t.Fatal(err)
	}
	up := readMig(t, m137UpFile)
	for name, c := range map[string]struct {
		drift string
		want  []string // fragments the refusal must carry
	}{
		"missing KYC worker fence (select)": {`DROP POLICY kyc_worker_fence_select ON staff_users`, []string{"missing_restrictive_fences={kyc_worker_fence_select}"}},
		"missing acting fence (update)":     {`DROP POLICY acting_fence_update ON staff_users`, []string{"missing_restrictive_fences={acting_fence_update}"}},
		"a NEW restrictive fence on staff":  {`CREATE POLICY zz_new_fence ON staff_users AS RESTRICTIVE FOR SELECT USING (true)`, []string{"unexpected_restrictive_fences={zz_new_fence}"}},
		"FORCE ROW LEVEL SECURITY lifted":   {`ALTER TABLE staff_users NO FORCE ROW LEVEL SECURITY`, []string{"force_rls=f"}},
		"dual scope policy gone":            {`DROP POLICY dual_scope_isolation ON staff_users`, []string{"dual_scope_isolation=0"}},
		"acting predicate function removed": {`DROP FUNCTION financial_acting_gucs_present() CASCADE`, []string{"financial_acting_gucs_present=0"}},
	} {
		func() {
			tx, err := pool.Raw().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, c.drift); err != nil {
				t.Fatalf("%s: drift: %v", name, err)
			}
			if _, err := tx.Exec(ctx, `SAVEPOINT up`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, up)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "SF098" {
				t.Fatalf("%s: want SF098, got %v", name, err)
			}
			for _, w := range c.want {
				if !strings.Contains(pe.Message, w) {
					t.Fatalf("%s: the refusal must carry %q: %q", name, w, pe.Message)
				}
			}
			if !strings.Contains(pe.Message, "Nothing was changed") {
				t.Fatalf("%s: the refusal must say nothing was changed: %q", name, pe.Message)
			}
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT up`); err != nil {
				t.Fatal(err)
			}
			if tables, fns, err := m137Objects(ctx, tx); err != nil || tables != 0 || fns != 0 {
				t.Fatalf("%s: a refused up creates nothing: tables=%d functions=%d err=%v", name, tables, fns, err)
			}
		}()
	}
	// Control: the undisturbed schema passes the pre-flight (the up applies).
	if applied, err := pool.MigrateUp(ctx, migDir(t, migration0137Version)); err != nil || len(applied) != 1 {
		t.Fatalf("control up: %v %v", applied, err)
	}
}

func TestMigration0137_Down_RefusesWhileDataExists(t *testing.T) {
	ctx := context.Background()
	scratchURL := scratchdb.New(t, "m0137d_")
	pool, err := db.Connect(ctx, scratchURL, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, migDir(t, migration0137Version)); err != nil {
		t.Fatal(err)
	}
	// One tenant, so rows can be planted in the TENANT arm too: a count that is blind to FORCE ROW LEVEL SECURITY
	// (the migration session carries no tenant) would miss them.
	launchfix.SeedOperatorsAt(t, scratchURL)
	var tenant uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0137", "m0137-"+uuid.NewString()[:12], "under_platform_licence")
		tenant = tn.ID
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	down := readMig(t, m137DownFile)
	plants := []struct {
		table, counts, insert string
	}{
		{"staff_mfa_factors", "factors=1, recovery_codes=0, challenges=0, enrolment_tokens=0", insertSQL["staff_mfa_factors"]},
		{"staff_mfa_recovery_codes", "factors=0, recovery_codes=1, challenges=0, enrolment_tokens=0", insertSQL["staff_mfa_recovery_codes"]},
		{"staff_auth_challenges", "factors=0, recovery_codes=0, challenges=1, enrolment_tokens=0", insertSQL["staff_auth_challenges"]},
		{"staff_enrolment_tokens", "factors=0, recovery_codes=0, challenges=0, enrolment_tokens=1", insertSQL["staff_enrolment_tokens"]},
	}
	for _, p := range plants {
		for _, arm := range []string{"platform", "tenant"} {
			func() {
				tx, err := pool.Raw().Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }() // nothing planted here ever commits
				staff := uuid.New()
				if arm == "tenant" {
					if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.Exec(ctx, `WITH s56p AS (SELECT gen_random_uuid() AS id), s56i AS (INSERT INTO persons (id) SELECT id FROM s56p) INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, $2, $3, '!x', 'support', (SELECT id FROM s56p))`,
						staff, tenant, staff.String()+"@fixture.invalid"); err != nil {
						t.Fatalf("%s/%s: plant staff: %v", p.table, arm, err)
					}
				} else if _, err := tx.Exec(ctx, `WITH s56p AS (SELECT gen_random_uuid() AS id), s56i AS (INSERT INTO persons (id) SELECT id FROM s56p) INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, NULL, $2, '!x', 'platform_admin', (SELECT id FROM s56p))`,
					staff, staff.String()+"@fixture.invalid"); err != nil {
					t.Fatalf("%s/%s: plant staff: %v", p.table, arm, err)
				}
				if _, err := tx.Exec(ctx, p.insert, nil, staff, randMAC()); err != nil {
					t.Fatalf("%s/%s: plant row: %v", p.table, arm, err)
				}
				// The migration session carries no tenant: FORCE RLS would hide a tenant row from a plain count.
				if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `SAVEPOINT down`); err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(ctx, down)
				var pe *pgconn.PgError
				if !errors.As(err, &pe) || pe.Code != "SF097" {
					t.Fatalf("%s/%s: want SF097, got %v", p.table, arm, err)
				}
				if !strings.Contains(pe.Message, p.counts) {
					t.Fatalf("%s/%s: the refusal must carry the counts %q: %q", p.table, arm, p.counts, pe.Message)
				}
				if strings.Contains(pe.Message, staff.String()) || strings.Contains(pe.Message, tenant.String()) {
					t.Fatalf("%s/%s: the refusal must carry counts only, not ids: %q", p.table, arm, pe.Message)
				}
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT down`); err != nil {
					t.Fatal(err)
				}
				tables, fns, err := m137Objects(ctx, tx)
				var forced int
				if e := tx.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('staff_mfa_factors', 'staff_mfa_recovery_codes', 'staff_auth_challenges', 'staff_enrolment_tokens') AND relforcerowsecurity`).Scan(&forced); e != nil {
					t.Fatal(e)
				}
				if err != nil || tables != 4 || fns != 7 || forced != 4 {
					t.Fatalf("%s/%s: a refused down drops nothing and leaves FORCE RLS on: tables=%d functions=%d forced=%d err=%v", p.table, arm, tables, fns, forced, err)
				}
				if arm == "tenant" {
					if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
						t.Fatal(err)
					}
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+p.table+` WHERE staff_user_id = $1`, staff).Scan(&n); err != nil || n != 1 {
					t.Fatalf("%s/%s: the planted row must survive the refused down: %d %v", p.table, arm, n, err)
				}
			}()
		}
	}
	// Data that was COMMITTED refuses the real runner too (and the migration stays applied).
	staff := uuid.New()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `WITH s56p AS (SELECT gen_random_uuid() AS id), s56i AS (INSERT INTO persons (id) SELECT id FROM s56p) INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, NULL, $2, '!x', 'platform_admin', (SELECT id FROM s56p))`,
			staff, staff.String()+"@fixture.invalid"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, insertSQL["staff_auth_challenges"], nil, staff, randMAC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateDown(ctx, migDir(t, migration0137Version), 1); err == nil || !strings.Contains(err.Error(), "SF097") && !strings.Contains(err.Error(), "down refused") {
		t.Fatalf("the migrate runner must refuse the down while data exists: %v", err)
	}
	var applied bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 137)`).Scan(&applied)
	}); err != nil || !applied {
		t.Fatalf("0137 must stay applied after a refused down: %v %v", applied, err)
	}
}
